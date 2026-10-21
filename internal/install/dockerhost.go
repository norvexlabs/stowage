package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/providers"
)

// The Docker shape's host daemon: the VM's backup, restore and disk-pressure code, run in the
// installer's process for one command (no daemon stays), its verbs reaching it as the VM's reach
// the socket. The schedules the VM's OS bundle carries as units are written on the Docker host by
// the installer, as systemd units of the install: the scheduled backup, and the disk-pressure
// watch every pressureEvery. A host without systemd, or a run not as root, gets neither, and is
// told so; `backup run` and `disk-check` still run when asked.

// systemdDir is where an install's units are written on a Docker host.
const systemdDir = "/etc/systemd/system"

// pressureFile records, in the install directory, the watch's last POST on Docker (the VM's
// daemon keeps it in memory).
const pressureFile = "disk-pressure.json"

// installUnit is the name of an install's unit of kind on a Docker host: the project's name, the
// kind, and the first hex digits of the install directory's digest, so two installs on one host
// never share one.
func installUnit(name, kind, dir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	return name + "-" + kind + "-" + hex.EncodeToString(sum[:])[:8]
}

// hostLock is the lock in the install directory a Docker install's daemon holds while it runs
// jobs: one process at a time takes them, as the VM's one daemon does.
const hostLock = ".stowage-host.lock"

// settingsLock is the lock file a Docker install's backup settings change holds.
const settingsLock = ".stowage-settings.lock"

// hostBusy reports whether another process holds the install's hostLock: it runs a job.
func hostBusy(dir string) bool {
	unlock, err := lockFile(filepath.Join(dir, hostLock))
	if err != nil {
		return errors.Is(err, errLocked)
	}
	unlock()
	return false
}

// dockerHostd is the host daemon of the Docker install in.O.Dir that runs jobs (backups,
// restores), run in this process until done is called; ctx bounds its jobs. It holds hostLock,
// waiting for another process holding it (said in the log), so its job slot and its event
// journal are the install's alone meanwhile.
func (in *Installer) dockerHostd(ctx context.Context) (*Hostd, func(), error) {
	h, err := in.dockerWatchd(ctx)
	if err != nil {
		return nil, nil, err
	}
	lock := filepath.Join(h.O.Dir, hostLock)
	unlock, err := lockFile(lock)
	if errors.Is(err, errLocked) {
		in.O.logf("another backup or restore of this install runs: waiting for it")
	}
	for errors.Is(err, errLocked) {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(time.Second):
		}
		unlock, err = lockFile(lock)
	}
	if err != nil {
		return nil, nil, err
	}
	// Under the lock no other daemon of the install runs: what a killed one left (its rclone
	// config among it) goes.
	if left, err := filepath.Glob(filepath.Join(h.O.Dir, ".host-*")); err == nil {
		for _, d := range left {
			_ = os.RemoveAll(d)
		}
	}
	run, err := os.MkdirTemp(h.O.Dir, ".host-")
	if err != nil {
		unlock()
		return nil, nil, err
	}
	h.Run = run
	h.ctx = ctx
	h.startJournal()
	return h, func() {
		h.backups.Wait()
		_ = os.RemoveAll(run)
		unlock()
	}, nil
}

// dockerWatchd is the host daemon of the Docker install in.O.Dir for what runs no job and reads
// no journal: the reads, the disk-pressure check and its units. Nothing is written by making it;
// a read writing a file (the rclone config of a list) writes it under the system's temporary
// directory, removed once read.
func (in *Installer) dockerWatchd(ctx context.Context) (*Hostd, error) {
	dir, err := filepath.Abs(in.O.Dir)
	if err != nil {
		return nil, err
	}
	in.O.Dir = dir
	o := in.O
	if o.Host == nil {
		o.Host = HostTool{}
	}
	h := &Hostd{R: in.R, O: o, Host: o.Host, Docker: true, Systemd: cmp.Or(o.Systemd, systemdDir), DiskDir: in.dockerRoot(ctx)}
	h.ctx = ctx
	return h, nil
}

// dockerRoot is the Docker engine's data directory, which holds the install's volumes; empty
// (the install directory's filesystem is read) when the engine does not say.
func (in *Installer) dockerRoot(ctx context.Context) string {
	if in.O.Docker == nil {
		return ""
	}
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"info", "--format", "{{.DockerRootDir}}"}})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// handlerTransport answers an HTTP request with a handler in this process.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	// As a server gives it: a request without a body reads as empty.
	if r.Body == nil {
		r.Body = http.NoBody
	} else {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// systemdHost reports whether this host runs systemd and the run may write its units.
func (h *Hostd) systemdHost() bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil && h.O.Systemd == "" {
		return false
	}
	return os.Geteuid() == 0 || h.O.Systemd != ""
}

// dockerUnits writes (content non-empty) or removes the install's unit pair of kind, then
// enables and starts the timer, or disables it: false when the host takes no unit (said in the
// log).
func (h *Hostd) dockerUnits(ctx context.Context, kind string, service, timer string) error {
	name := installUnit(h.R.Spec.Name, kind, h.O.Dir)
	if !h.systemdHost() {
		if timer != "" {
			h.O.logf("the %s timer is not installed: it needs systemd, and a run as root", kind)
		}
		return nil
	}
	svc, tmr := filepath.Join(h.Systemd, name+".service"), filepath.Join(h.Systemd, name+".timer")
	if timer == "" {
		if _, err := os.Stat(tmr); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		_, _ = h.Host.Run(ctx, Cmd{Args: []string{"systemctl", "disable", "--now", name + ".timer"}})
		for _, f := range []string{tmr, svc} {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		_, err := h.Host.Run(ctx, Cmd{Args: []string{"systemctl", "daemon-reload"}})
		return err
	}
	if err := os.MkdirAll(h.Systemd, 0o755); err != nil {
		return err
	}
	if err := writeAtomic(svc, strings.NewReader(service), 0o644, nil); err != nil {
		return err
	}
	if err := writeAtomic(tmr, strings.NewReader(timer), 0o644, nil); err != nil {
		return err
	}
	if _, err := h.Host.Run(ctx, Cmd{Args: []string{"systemctl", "daemon-reload"}}); err != nil {
		return err
	}
	_, err := h.Host.Run(ctx, Cmd{Args: []string{"systemctl", "enable", "--now", name + ".timer"}})
	if err == nil {
		_, err = h.Host.Run(ctx, Cmd{Args: []string{"systemctl", "restart", name + ".timer"}})
	}
	return err
}

// unitService is a oneshot unit running the install's installer with args.
func (h *Hostd) unitService(what string, timeout time.Duration, args ...string) string {
	ctl := filepath.Join(h.O.Dir, h.R.Ctl())
	return fmt.Sprintf("[Unit]\nDescription=%s (%s): %s\nAfter=docker.service\nWants=docker.service\n\n[Service]\nType=oneshot\nExecStart=%s %s --dir %s\nTimeoutStartSec=%dmin\n",
		h.R.Spec.Name, h.O.Dir, what, ctl, strings.Join(args, " "), h.O.Dir, int(timeout.Minutes()))
}

// dockerSchedule writes the scheduled backup's units for c's schedule (nil: removes them).
func (h *Hostd) dockerSchedule(ctx context.Context, c *BackupConfig) error {
	if c == nil {
		return h.dockerUnits(ctx, "backup", "", "")
	}
	return h.dockerUnits(ctx, "backup", h.unitService("the scheduled backup", 75*time.Minute, "backup", "run", "--if-configured"),
		fmt.Sprintf("[Unit]\nDescription=%s (%s): the scheduled backup's timer\n\n[Timer]\nOnCalendar=%s\nRandomizedDelaySec=15min\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n",
			h.R.Spec.Name, h.O.Dir, c.Schedule))
}

// InstallWatch writes the disk-pressure watch's units of a Docker install: `disk-check` at boot
// and every pressureEvery. Without hooks.maintenance.disk_pressure, it removes them.
func (in *Installer) InstallWatch(ctx context.Context) error {
	h, err := in.dockerWatchd(ctx)
	if err != nil {
		return err
	}
	if h.pressureHook() == nil {
		return h.dockerUnits(ctx, "disk", "", "")
	}
	return h.dockerUnits(ctx, "disk", h.unitService("the disk-pressure check", 5*time.Minute, "disk-check"),
		fmt.Sprintf("[Unit]\nDescription=%s (%s): the disk-pressure watch\n\n[Timer]\nOnBootSec=2min\nOnUnitActiveSec=%ds\n\n[Install]\nWantedBy=timers.target\n",
			in.R.Spec.Name, in.O.Dir, pressureEvery))
}

// DiskCheck is `disk-check` on Docker: the disk-pressure watch's one check, its POST recorded.
func (in *Installer) DiskCheck(ctx context.Context) error {
	h, err := in.dockerWatchd(ctx)
	if err != nil {
		return err
	}
	dp := h.pressureHook()
	if dp == nil {
		fmt.Fprintf(in.O.Out, "%s declares no disk-pressure hook: nothing to check\n", in.R.Spec.Name)
		return nil
	}
	h.loadPressure()
	h.pressureCheck(ctx, dp)
	st := h.pressureStatus(h.disk())
	fmt.Fprintf(in.O.Out, "pressure  %s: %.1f%% used of %s (soft %d%%, hard %d%%)\n", st.Level, st.UsedPct, h.diskPath(), st.SoftPct, st.HardPct)
	return nil
}

// loadPressure reads the last POST a Docker install recorded.
func (h *Hostd) loadPressure() {
	raw, err := os.ReadFile(filepath.Join(h.O.Dir, pressureFile))
	if err != nil {
		return
	}
	var p PressurePost
	if json.Unmarshal(raw, &p) == nil {
		h.pressMu.Lock()
		h.lastPost = &p
		h.pressMu.Unlock()
	}
}

// savePressure records the last POST of a Docker install.
func (h *Hostd) savePressure(p PressurePost) {
	raw, err := json.Marshal(p)
	if err == nil {
		err = writeAtomic(filepath.Join(h.O.Dir, pressureFile), bytes.NewReader(raw), 0o600, nil)
	}
	if err != nil {
		h.O.logf("disk pressure: the POST's record: %v", err)
	}
}

// dockerPressure is the disk-pressure line of a Docker install's status: the disk read now, the
// last POST; nil without the hook.
func (in *Installer) dockerPressure(ctx context.Context) *DiskPressure {
	h, err := in.dockerWatchd(ctx)
	if err != nil {
		return nil
	}
	h.loadPressure()
	return h.pressureStatus(h.disk())
}

// lazyTransport answers a request starting a job (startsJob) — and every one after it — with the
// daemon jobs makes, any other with the one read makes (no job, no lock: a read or a settings
// change never waits for a running backup), each made the first time it is needed.
type lazyTransport struct {
	read, jobs func() (http.Handler, error)
	r, j       http.Handler
	// settings is the lock file a settings change holds; empty: none (tests).
	settings string
}

// startsJob reports a request that starts a job: a backup, a verify, a restore.
func startsJob(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/v1/backup", "/v1/backup/verify", "/v1/restore":
		return true
	}
	return false
}

func (t *lazyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	h, make := &t.r, t.read
	// A request starting a job, and every one after it (a job is followed where it runs), goes to
	// the jobs' daemon.
	if t.j != nil || startsJob(r) {
		h, make = &t.j, t.jobs
	}
	if *h == nil {
		made, err := make()
		if err != nil {
			return nil, err
		}
		*h = made
	}
	// A settings change reads, merges and writes the settings: one process at a time.
	if t.settings != "" && r.URL.Path == "/v1/backup/config" && r.Method != http.MethodGet {
		unlock, err := flockWait(t.settings, true)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	return handlerTransport{*h}.RoundTrip(r)
}

// dockerHostCommands are the Docker shape's verbs of the host daemon: backup, restore and the
// disk-pressure watch's check, each run with the daemon in this process.
func dockerHostCommands(in *Installer, prepare func() error, dirFlag func(*cobra.Command), stdout, stderr io.Writer, terminal func(bool) Prompter) []*cobra.Command {
	name := in.R.Spec.Name
	var done func()
	// The command's context: a signal stops its jobs, as the VM's daemon stopping stops them.
	jobs := context.Background()
	c := &hostClient{ctl: in.R.Ctl(), out: stdout}
	lt := &lazyTransport{}
	lt.read = func() (http.Handler, error) {
		if err := prepare(); err != nil {
			return nil, err
		}
		c.ctl = in.ctl()
		h, err := in.dockerWatchd(jobs)
		if err != nil {
			return nil, err
		}
		lt.settings = filepath.Join(h.O.Dir, settingsLock)
		return h.Handler(), nil
	}
	lt.jobs = func() (http.Handler, error) {
		if err := prepare(); err != nil {
			return nil, err
		}
		c.ctl = in.ctl()
		h, d, err := in.dockerHostd(jobs)
		if err != nil {
			return nil, err
		}
		done = d
		lt.settings = filepath.Join(h.O.Dir, settingsLock)
		return h.Handler(), nil
	}
	c.http = &http.Client{Transport: lt}
	// The daemon goes when the command ends, whatever it returned.
	ended := func(cmd *cobra.Command) {
		run := cmd.RunE
		if run == nil {
			return
		}
		cmd.RunE = func(c *cobra.Command, args []string) error {
			jobs = c.Context()
			defer func() {
				if done != nil {
					done()
				}
			}()
			return run(c, args)
		}
	}
	var out []*cobra.Command
	if providers.RunsBackups(in.R.Spec) {
		b := backupCommand(c, name, stdout, stderr)
		restore := restoreCommand(c, name, in.R.Ctl()+" --dir DIR", stderr, terminal)
		for _, cmd := range append([]*cobra.Command{restore}, b.Commands()...) {
			dirFlag(cmd)
			ended(cmd)
		}
		out = append(out, b, restore)
	}
	check := &cobra.Command{
		Use:    "disk-check",
		Short:  "Read the data's disk once, as the disk-pressure watch does, and POST its level at soft_pct or past it (the watch's timer runs it)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			return in.DiskCheck(cmd.Context())
		},
	}
	dirFlag(check)
	return append(out, check)
}
