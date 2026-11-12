package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/norvexlabs/stowage/internal/command"
	"github.com/norvexlabs/stowage/internal/providers"
)

// The VM's health gate (`<short>-stow health-gate`, <name>-health.service after the boot) and the
// readiness check behind it and GET /v1/health.
//
// The gate is the judge of Kairos's boot assessment (GRUB images). An upgrade sets
// enable_boot_assessment=yes in COS_STATE's boot_assessment environment; GRUB, booting the active
// system with it set, sets boot_assessment_tentative=yes, and a boot that finds both set boots the
// fallback entry — the passive system — with upgrade_failure on its command line. The bundle's
// 08_grub.yaml leaves them set at boot: the gate clears them once the install is healthy. A boot is on trial when it is the active system and both are set. A gate that finds the
// install unhealthy by the deadline keeps diagnostics and, on trial, reboots: the passive system's
// boot goes back to the release the upgrade replaced. Off trial it reboots nothing (no loop).

// The boot assessment's environment and the variables the gate reads and writes.
const (
	cosState        = "/run/initramfs/cos-state" // COS_STATE, mounted read-only
	assessFile      = "boot_assessment"
	assessEnable    = "enable_boot_assessment"
	assessAlways    = "enable_boot_assessment_always"
	assessTentative = "boot_assessment_tentative"
	passiveImage    = "cOS/passive.img"
	upgradeFailure  = "upgrade_failure" // /run/cos/upgrade_failure: Kairos booted the fallback
)

// FailedBootsDir, in the install directory, keeps the diagnostics of the last keepFailedBoots
// boots the gate found unhealthy; failedBootFile describes one.
const (
	FailedBootsDir  = "failed-boots"
	failedBootFile  = "failed-boot.json"
	keepFailedBoots = 5
)

// HealthService is the service the `command` health provider runs as.
const HealthService = "stowage-health"

// Bounds on a probe and on what the diagnostics keep.
const (
	probeTimeout = 10 * time.Second
	// staleHealthFile is past the longest a health command may run (timeout_s is unbounded only
	// in principle).
	staleHealthFile = time.Hour
	maxProbeAnswer  = 64 << 10
	diagLogLines    = 500
	diagBootLines   = 2000
)

// assessment is Kairos's boot assessment environment in the COS_STATE partition mounted at dir.
type assessment struct {
	dir  string
	host Tool
}

func (a assessment) file() string { return filepath.Join(a.dir, assessFile) }

// read returns the environment's variables; none when it has no file.
func (a assessment) read() (map[string]string, error) {
	raw, err := os.ReadFile(a.file())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	vars := map[string]string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			vars[k] = v
		}
	}
	return vars, nil
}

// trial reports whether GRUB booted this system on trial: assessment enabled, and the attempt
// marked. Enabled but unmarked means GRUB's side of the assessment did not run: a reboot would not
// fall back, so the boot is not on trial.
func trial(vars map[string]string) bool {
	return (vars[assessEnable] == "yes" || vars[assessAlways] == "yes") && vars[assessTentative] == "yes"
}

// set writes vars (KEY=VALUE; an empty value clears) with grub2-editenv, the partition remounted
// read-write meanwhile and read-only after.
func (a assessment) set(ctx context.Context, vars ...string) error {
	if _, err := a.host.Run(ctx, Cmd{Args: []string{"mount", "-o", "remount,rw", a.dir}}); err != nil {
		return fmt.Errorf("remounting %s read-write: %w", a.dir, err)
	}
	_, err := a.host.Run(ctx, Cmd{Args: append([]string{"grub2-editenv", a.file(), "set"}, vars...)})
	if err != nil {
		err = fmt.Errorf("grub2-editenv %s: %w", a.file(), err)
	}
	if _, rerr := a.host.Run(context.WithoutCancel(ctx), Cmd{Args: []string{"mount", "-o", "remount,ro", a.dir}}); rerr != nil && err == nil {
		err = fmt.Errorf("remounting %s read-only: %w", a.dir, rerr)
	}
	return err
}

// unitState is a systemd unit's ActiveState, or noBootUnit when systemd has no such unit.
func unitState(ctx context.Context, host Tool, unit string) (string, error) {
	st, _, err := unitStatus(ctx, host, unit)
	return st, err
}

// unitStatus is unitState's, and whether a job is queued for the unit (its Job property, which
// systemctl leaves out, or empty, when none is).
func unitStatus(ctx context.Context, host Tool, unit string) (string, bool, error) {
	out, err := host.Run(ctx, Cmd{Args: []string{"systemctl", "show", "--property", "LoadState", "--property", "ActiveState", "--property", "Job", unit}})
	if err != nil {
		return "", false, fmt.Errorf("the state of %s: %w", unit, err)
	}
	props := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			props[k] = v
		}
	}
	if props["LoadState"] == "not-found" {
		return noBootUnit, false, nil
	}
	if props["ActiveState"] == "" {
		return "", false, fmt.Errorf("the state of %s: systemctl answered %q", unit, strings.TrimSpace(string(out)))
	}
	return props["ActiveState"], props["Job"] != "", nil
}

// Gate is the VM's health gate over the install of In, which carries the release this OS carries.
type Gate struct {
	In   *Installer
	Host Tool // runs host programs: Args[0] is the program
	// Probe sends the http health providers' requests; nil is a client of its own.
	Probe *http.Client
	// State is COS_STATE's mount; empty is /run/initramfs/cos-state. Where Kairos marks the boot
	// and the boot id are In's Options (Cos, BootIDFile).
	State string
	// Unit is what shapes.kairos.health_gate's seconds count; 0 is a second. Tests shorten it.
	Unit time.Duration
	// Now is the clock the diagnostics are named by; nil is time.Now.
	Now func() time.Time

	// boot holds this boot's boot.started once read (judged).
	boot bootCache
}

// judged journals the gate's verdict on this boot: healthy, went_back (the next boot runs the
// passive system) or kept_unhealthy (why in kept); reason is why the install was not healthy,
// reboots whether the gate reboots now (Event.Reboots). The boot's record is read until a read
// succeeds, then kept for the gate's run.
func (g *Gate) judged(onTrial bool, verdict, reason, kept string, reboots bool) {
	g.journalJudged(Event{OnTrial: &onTrial, Verdict: verdict, Reason: reason, Kept: kept, Reboots: &reboots})
}

// journalJudged journals e as this boot's boot.judged, its boot and release filled in.
func (g *Gate) journalJudged(e Event) {
	m := g.In.R.Manifest
	rel := Version{m.ProjectVersion, m.Revision}
	boot := g.boot.get(time.Now(), bootRetry*cmp.Or(g.Unit, time.Second), func() *Event { return bootRecord(&g.In.O, rel) })
	e.Kind, e.BootID, e.Release = evBootJudged, bootIDOf(boot), &rel
	if _, err := (&journal{dir: g.In.O.Dir, boot: boot}).append(e); err != nil {
		g.In.O.logf("the event journal: %v", err)
	}
}

// errBootFailed is a boot unit that failed: waiting changes nothing.
var errBootFailed = errors.New("the boot failed")

// Run judges this boot. Healthy within the deadline, a boot on trial is marked good. Not, the
// diagnostics are kept in FailedBootsDir and a boot on trial reboots into the passive system.
func (g *Gate) Run(ctx context.Context) error {
	in := g.In
	o, m := &in.O, in.R.Manifest
	if !fileExists(in.path(envName)) {
		return g.noInstall(ctx)
	}
	// The compose files of health commands a reboot or a kill interrupted: older than any call
	// the daemon may be running.
	if left, err := filepath.Glob(in.path(".stowage-health-*.yaml")); err == nil {
		for _, f := range left {
			if fi, err := os.Stat(f); err == nil && time.Since(fi.ModTime()) > staleHealthFile {
				_ = os.Remove(f)
			}
		}
	}
	a, onTrial, err := g.trial()
	if err != nil {
		return err
	}
	cfg := in.R.Spec.Shapes.Kairos.Gate()
	unit := cmp.Or(g.Unit, time.Second)
	limit := time.Duration(cfg.DeadlineS) * unit
	this := fmt.Sprintf("%s %s (revision %d)", m.Name, m.ProjectVersion, m.Revision)
	if onTrial {
		o.logf("this boot runs %s on trial after an upgrade: the install must be healthy within %s", this, limit)
	}
	deadline := time.Now().Add(limit)
	var why error
	for {
		if why = g.check(ctx); why == nil || errors.Is(why, errBootFailed) || !time.Now().Before(deadline) {
			break
		}
		o.logf("not healthy yet: %v", why)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(cfg.PollS) * unit):
		}
	}
	if why == nil {
		if !onTrial {
			g.judged(false, verdictHealthy, "", "", false)
			o.logf("%s is healthy", this)
			return nil
		}
		if err := a.set(ctx, assessEnable+"=", assessTentative+"="); err != nil {
			g.judged(true, verdictWentBack, "marking the boot good failed: "+reason(err), "", false)
			return fmt.Errorf("%s is healthy, but marking the boot good failed (the next boot would fall back): %w", this, err)
		}
		g.judged(true, verdictHealthy, "", "", false)
		o.logf("%s is healthy: the upgrade is kept", this)
		return nil
	}
	// On trial, falling back would run the release the upgrade replaced: its own boot refuses when
	// the database moved past what it reads, and then nothing serves. The new system is kept.
	var kept string
	if onTrial {
		if in.switchedHere() && !fileExists(filepath.Join(in.path(PrevDir), prevState)) {
			kept = "the passive system is the OS this install's first boot switched from, and no previous release is kept: it holds no release to go back to"
		} else {
			kept = g.refusedRollback(ctx)
		}
	}
	dir, err := g.keep(ctx, why, kept)
	if err != nil {
		o.logf("keeping the diagnostics: %v", err)
	}
	failed := fmt.Errorf("%s is not healthy: %w (diagnostics: %s)", this, why, cmp.Or(dir, "none"))
	unhealthy := reason(why)
	if !onTrial {
		g.judged(false, verdictKeptUnhealthy, unhealthy, "the boot is not on trial: there is nothing to go back to", false)
		return failed
	}
	if kept != "" {
		if err := a.set(ctx, assessEnable+"=", assessTentative+"="); err != nil {
			g.judged(true, verdictWentBack, unhealthy, "", false)
			return fmt.Errorf("%w; going back is refused (%s), but marking the boot kept failed (the next boot would fall back): %v", failed, kept, err)
		}
		g.judged(true, verdictKeptUnhealthy, unhealthy, kept, false)
		return fmt.Errorf("%w; this system is kept, as going back is refused: %s", failed, kept)
	}
	if !fileExists(filepath.Join(a.dir, passiveImage)) {
		g.judged(true, verdictKeptUnhealthy, unhealthy, "there is no passive system to go back to", false)
		return fmt.Errorf("%w; there is no passive system to go back to", failed)
	}
	// Journaled before the reboot, which may end this process: a reboot that fails leaves the
	// marks set, so the next boot goes back all the same.
	g.judged(true, verdictWentBack, unhealthy, "", true)
	o.logf("%v", failed)
	o.logf("rebooting: Kairos boots the passive system, and its boot goes back to the release the upgrade replaced")
	if _, err := g.Host.Run(ctx, Cmd{Args: []string{"systemctl", "reboot"}}); err != nil {
		// A run the shutdown stopped is the reboot coming: no second verdict. Its signal may reach
		// systemctl before the gate's context is cancelled.
		if ctx.Err() != nil || killedBySignal(err) {
			return fmt.Errorf("%w; stopped as the system goes down: %v", failed, cmp.Or[error](ctx.Err(), err))
		}
		// The marks stay set: the next boot, whenever it comes, goes back all the same.
		f, onTrial := false, true
		g.journalJudged(Event{OnTrial: &onTrial, Verdict: verdictWentBack, Reason: unhealthy, Reboots: &f, Error: "the reboot failed: " + reason(err)})
		return fmt.Errorf("%w; the reboot failed: %v", failed, err)
	}
	return failed
}

// killedBySignal reports whether err is a program's that the shutdown's signal ended (SIGTERM or
// SIGKILL), not one that exited or crashed.
func killedBySignal(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ProcessState == nil {
		return false
	}
	ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && (ws.Signal() == syscall.SIGTERM || ws.Signal() == syscall.SIGKILL)
}

// trial reads the boot assessment: whether GRUB booted the active system on trial.
func (g *Gate) trial() (assessment, bool, error) {
	a := assessment{cmp.Or(g.State, cosState), g.Host}
	vars, err := a.read()
	if err != nil {
		return a, false, fmt.Errorf("the boot assessment: %w", err)
	}
	return a, bootMode(g.In.O.cos()) == "active" && trial(vars), nil
}

// noInstall is the gate of a boot with no install yet: there is nothing to judge. The OS a first
// boot switched to (switchOS) runs on trial, and its first install may wait for what `vm create`
// gives over SSH, after this gate, or have failed: its marks are cleared, whatever its boot unit
// did. The passive system, the OS it switched from, holds no install, and its boot stops at the
// same mark; left set, the next boot would fall back to it, whose installer refuses the newer
// install made meanwhile. Here, a retried `install` runs this OS's release.
func (g *Gate) noInstall(ctx context.Context) error {
	in := g.In
	o := &in.O
	onTrial := false
	var a assessment
	if in.switchedHere() {
		var err error
		if a, onTrial, err = g.trial(); err != nil {
			return err
		}
	}
	if !onTrial {
		g.judged(false, verdictNotInstalled, "", "", false)
		o.logf("%s holds no install yet: there is nothing to judge", o.Dir)
		return nil
	}
	if err := a.set(ctx, assessEnable+"=", assessTentative+"="); err != nil {
		g.judged(true, verdictWentBack, "the boot held no install, and marking the OS its first boot switched to good failed: "+reason(err), "", false)
		return fmt.Errorf("%s holds no install yet, but marking the OS its first boot switched to good failed (the next boot would fall back to the older OS): %w", o.Dir, err)
	}
	g.judged(true, verdictNotInstalled, "", "", false)
	o.logf("%s holds no install yet: the OS its first boot switched to is marked good, as the passive system holds no install to go back to", o.Dir)
	return nil
}

// switchedHere reports whether this OS is the one a first boot switched to: the settings file
// names this OS's release (by version and revision, as firstBoot compares them: an OS bundle's
// installer carries no tag), and switchOS's mark names that release's tag. The mark stays after
// the install, and the health gate reads it: removed, an unhealthy first install on trial falls
// back as any other.
func (in *Installer) switchedHere() bool {
	raw, err := os.ReadFile(osSwitchFile(in.O.Dir))
	if err != nil {
		return false
	}
	st, err := ReadSettings(cmp.Or(in.O.Settings, SettingsFile(in.R.Spec.Name)))
	if err != nil {
		in.O.logf("warning: %v: the OS a first boot switched to is not recognised", err)
		return false
	}
	if st == nil || st.Release == nil {
		return false
	}
	t, m := st.Release, in.R.Manifest
	return t.Tag != "" && strings.TrimSpace(string(raw)) == t.Tag && t.Version == m.ProjectVersion && t.Revision == m.Revision
}

// refusedRollback runs the rollback the passive system's boot would run, dry: why the database
// refuses it, or empty when it would go back (or cannot be checked: the boot then decides).
func (g *Gate) refusedRollback(ctx context.Context) string {
	var log bytes.Buffer
	dry := &Installer{R: g.In.R, O: g.In.O}
	dry.O.DryRun, dry.O.Out, dry.O.Log = true, &log, &log
	var refused rollbackRefused
	if err := dry.Rollback(ctx); errors.As(err, &refused) {
		return string(refused)
	}
	return ""
}

// check is one judgement: the boot brought the install up, the install runs this OS's release,
// and it is ready.
func (g *Gate) check(ctx context.Context) error {
	in := g.In
	m := in.R.Manifest
	unit := in.R.Spec.Name + ".service"
	st, err := unitState(ctx, g.Host, unit)
	if err != nil {
		return err
	}
	if st == "failed" {
		return fmt.Errorf("%w: %s failed (journalctl -u %s -b tells why)", errBootFailed, unit, unit)
	}
	env, err := in.open()
	if err != nil {
		return err
	}
	if _, v, rev := runs(env); v != m.ProjectVersion || rev != strconv.Itoa(m.Revision) {
		return fmt.Errorf("the install runs %s (revision %s), not %s (revision %d), which this OS carries", v, rev, m.ProjectVersion, m.Revision)
	}
	return in.ready(ctx, env, g.Probe)
}

// ready reports why the install is not ready, nil when it is: every service of the stack runs —
// healthy when it has a healthcheck — or exited 0, and the health provider answers ready.
func (in *Installer) ready(ctx context.Context, env *EnvFile, probe *http.Client) error {
	out, err := in.Compose(ctx, env, nil, "ps", "--all", "--format", "json")
	if err != nil {
		return fmt.Errorf("compose ps: %w", err)
	}
	list, err := parseServices(out)
	if err != nil {
		return err
	}
	// Judged as an install's start judges them (settled): a healthcheck still starting is not
	// ready yet, which the gate's next poll asks again.
	switch starting, why := settled(list); {
	case why != "":
		return errors.New(why)
	case len(starting) > 0:
		return fmt.Errorf("%s is starting", strings.Join(starting, ", "))
	}
	switch p := in.R.Spec.Health; p.Name {
	case "http-json", "http-status":
		return in.probeHTTP(ctx, env, probe)
	case "command":
		return in.probeCommand(ctx, env)
	}
	return nil
}

// probeHTTP asks the http health provider's service, at its address on the stack's network.
func (in *Installer) probeHTTP(ctx context.Context, env *EnvFile, client *http.Client) error {
	c, err := providers.HTTPHealth(in.R.Spec.Health)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	ip, err := in.address(ctx, env, c.Service)
	if err != nil {
		return err
	}
	if client == nil {
		client = probes
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	where := fmt.Sprintf("%s:%d%s", c.Service, c.Port, c.Path)
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, "http://"+net.JoinHostPort(ip, strconv.Itoa(c.Port))+c.Path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s does not answer: %w", where, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeAnswer+1))
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	if c.Fields == nil {
		if resp.StatusCode != c.Status {
			return fmt.Errorf("%s answered %d, not %d", where, resp.StatusCode, c.Status)
		}
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s answered %d", where, resp.StatusCode)
	}
	if len(body) > maxProbeAnswer {
		return fmt.Errorf("%s answered more than %d bytes", where, maxProbeAnswer)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		return fmt.Errorf("%s did not answer a JSON object: %w", where, err)
	}
	// The expected fields as JSON reads them, so a YAML 1 equals a JSON 1.
	raw, err := json.Marshal(c.Fields)
	if err != nil {
		return err
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		return err
	}
	for _, k := range sortedKeys(want) {
		g, ok := got[k]
		if ok && reflect.DeepEqual(g, want[k]) {
			continue
		}
		gs := []byte("nothing")
		if ok {
			gs, _ = json.Marshal(g)
		}
		ws, _ := json.Marshal(want[k])
		return fmt.Errorf("%s answered %s: %s, not %s", where, k, gs, ws)
	}
	return nil
}

// probes sends the probes: a readiness answer is the service's own, so no redirect is followed
// and no proxy is used; no connection is kept between probes.
var probes = &http.Client{
	Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// address is service's container's address on a network the host reaches: the first network by
// name that gives it one, its IPv4 address, else its IPv6 one (a network without IPv4); an
// `internal: true` network skipped (the host has no route to it). A service on the host's
// network is the loopback's.
func (in *Installer) address(ctx context.Context, env *EnvFile, service string) (string, error) {
	out, err := in.Compose(ctx, env, nil, "ps", "--quiet", service)
	if err != nil {
		return "", fmt.Errorf("compose ps %s: %w", service, err)
	}
	id, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if id == "" {
		return "", fmt.Errorf("%s has no container", service)
	}
	raw, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"inspect", "--format", "{{json .NetworkSettings.Networks}}", strings.TrimSpace(id)}})
	if err != nil {
		return "", fmt.Errorf("inspecting %s's container: %w", service, err)
	}
	var nets map[string]struct{ IPAddress, GlobalIPv6Address, NetworkID string }
	if err := json.Unmarshal(bytes.TrimSpace(raw), &nets); err != nil {
		return "", fmt.Errorf("inspecting %s's container: %w", service, err)
	}
	// On the host's network (hostServices), the service listens on the host.
	if _, host := nets["host"]; host {
		return "127.0.0.1", nil
	}
	var names, ids []string
	for _, n := range sortedKeys(nets) {
		if net.ParseIP(cmp.Or(nets[n].IPAddress, nets[n].GlobalIPv6Address)) != nil && nets[n].NetworkID != "" {
			names, ids = append(names, n), append(ids, nets[n].NetworkID)
		}
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("%s has no address on a network", service)
	}
	raw, err = in.O.Docker.Run(ctx, Cmd{Args: append([]string{"network", "inspect", "--format", "{{.Internal}}"}, ids...)})
	if err != nil {
		return "", fmt.Errorf("inspecting %s's networks: %w", service, err)
	}
	internal := strings.Fields(string(raw))
	if len(internal) != len(ids) {
		return "", fmt.Errorf("inspecting %s's networks: %d answers for %d networks", service, len(internal), len(ids))
	}
	for i, n := range names {
		if internal[i] == "false" {
			return cmp.Or(nets[n].IPAddress, nets[n].GlobalIPv6Address), nil
		}
	}
	return "", fmt.Errorf("%s is only on internal networks (%s), which the host does not reach", service, strings.Join(names, ", "))
}

// probeCommand asks the `command` health provider: {"op": "health.ready"} in, {"ready": bool,
// "reason": "..."} out; no answer (exit 2) is not ready. Each call runs as a container of its own,
// so the gate and the daemon may ask at once.
func (in *Installer) probeCommand(ctx context.Context, env *EnvFile) error {
	var c providers.RuntimeCommand
	if err := in.R.Spec.Health.Decode(&c); err != nil {
		return fmt.Errorf("health: %w", err)
	}
	var b [4]byte
	if _, err := io.ReadFull(in.O.rand(), b[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(b[:])
	var out struct {
		Ready  *bool  `json:"ready"`
		Reason string `json:"reason"`
	}
	err := in.runCommand(ctx, commandRun{what: "the health command", service: HealthService, file: ".stowage-health-" + id + ".yaml",
		name: containerName(env, HealthService) + "-" + id}, c, env, env, map[string]any{"op": "health.ready"}, &out)
	switch {
	case errors.Is(err, command.ErrNoAnswer):
		return errors.New("the health command has no answer")
	case err != nil:
		return fmt.Errorf("the health command: %w", err)
	case out.Ready == nil:
		return errors.New("the health command's answer carries no ready")
	case !*out.Ready:
		return fmt.Errorf("the health command answers not ready: %s", cmp.Or(out.Reason, "no reason given"))
	}
	return nil
}

// FailedBoot describes a boot the gate found unhealthy.
type FailedBoot struct {
	Release Version   `json:"release"`
	At      time.Time `json:"at"`
	Reason  string    `json:"reason"`
	// Kept: the gate kept the failed system, as going back is refused for this reason.
	Kept string `json:"kept,omitempty"`
	Dir  string `json:"dir"`
}

// unsafeName is what a directory name drops from a version.
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// keep writes the diagnostics of a failed judgement into a directory of FailedBootsDir — why, the
// services, their last log lines and the boot unit's journal — and drops all but the last
// keepFailedBoots. What cannot be read is noted in its place.
func (g *Gate) keep(ctx context.Context, why error, kept string) (string, error) {
	in := g.In
	m := in.R.Manifest
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	at := now().UTC()
	root := in.path(FailedBootsDir)
	dir := filepath.Join(root, fmt.Sprintf("%s-%s-r%d", at.Format("20060102T150405Z"), unsafeName.ReplaceAllString(m.ProjectVersion, "_"), m.Revision))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	desc, err := json.MarshalIndent(FailedBoot{Release: Version{m.ProjectVersion, m.Revision}, At: at, Reason: why.Error(), Kept: kept, Dir: dir}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, failedBootFile), append(desc, '\n'), 0o600); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	save := func(name string, out []byte, err error) {
		if err != nil {
			out = append(out, fmt.Sprintf("\n(%v)\n", err)...)
		}
		_ = os.WriteFile(filepath.Join(dir, name), out, 0o600)
	}
	unit := in.R.Spec.Name + ".service"
	out, err := g.Host.Run(ctx, Cmd{Args: []string{"journalctl", "--unit", unit, "--boot", "--no-pager", "--lines", strconv.Itoa(diagBootLines)}})
	save("boot.log", out, err)
	if env, err := in.open(); err == nil {
		out, err := in.Compose(ctx, env, nil, "ps", "--all")
		save("ps.txt", out, err)
		if raw, err := in.Compose(ctx, env, nil, "ps", "--all", "--format", "json"); err == nil {
			if list, err := parseServices(raw); err == nil {
				for _, s := range list {
					if !serviceName.MatchString(s.Service) {
						continue
					}
					out, err := in.Compose(ctx, env, nil, "logs", "--no-color", "--timestamps", "--tail", strconv.Itoa(diagLogLines), s.Service)
					save("logs-"+s.Service+".txt", out, err)
				}
			}
		}
	}
	return dir, prune(root)
}

// prune drops all but the last keepFailedBoots diagnostics, named by their time.
func prune(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	slices.Sort(dirs)
	for len(dirs) > keepFailedBoots {
		if err := os.RemoveAll(filepath.Join(root, dirs[0])); err != nil {
			return err
		}
		dirs = dirs[1:]
	}
	return nil
}

// lastFailedBoot is the last boot the gate found unhealthy; nil when none is kept.
func lastFailedBoot(dir string) *FailedBoot {
	entries, err := os.ReadDir(filepath.Join(dir, FailedBootsDir))
	if err != nil {
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		raw, err := os.ReadFile(filepath.Join(dir, FailedBootsDir, entries[i].Name(), failedBootFile))
		if err != nil || len(raw) > 64<<10 {
			continue
		}
		var fb FailedBoot
		if json.Unmarshal(raw, &fb) == nil {
			return &fb
		}
	}
	return nil
}
