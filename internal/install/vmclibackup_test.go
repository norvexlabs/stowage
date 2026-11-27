package install

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// `backup list` marks a backup of a profile the release dropped, which retention never prunes.
func TestBackupListMarksADroppedProfile(t *testing.T) {
	t.Parallel()
	e, d, f, key := backupVM(t)
	sock := served(t, d)
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte(rcloneConf), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := ctl(e, sock, "backup", "config", "--destination", "store:backups/notes", "--rclone-config", conf, "--recipient", key.Recipient().String()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	f.objects["store:backups/notes/notes-full-20261009T030000Z-1.0.0-r1.pgdump.age"] = []byte("abc")
	f.objects["store:backups/notes/notes-lite-20261008T030000Z-0.9.0-r1.pgdump.age"] = []byte("a profile gone")
	out, err := ctl(e, sock, "backup", "list")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		dropped := strings.Contains(line, "a profile the release dropped: never pruned")
		switch {
		case strings.HasPrefix(line, "notes-full-") && dropped:
			t.Errorf("the release's profile marked: %q", line)
		case strings.HasPrefix(line, "notes-lite-") && !dropped:
			t.Errorf("the dropped profile not marked: %q", line)
		}
	}
}

// runnerCut is a Docker whose run of the runner lasts until it is cut off, as exec runs it: its
// Cancel, then the CLI killed; it records those and the calls after (rmErr answering removals).
// With readStdin, the run reads its stdin first, and a failure of it ends the run.
type runnerCut struct {
	mu        sync.Mutex
	after     []string
	readStdin bool
	rmErr     error
	stopErr   error
}

func (d *runnerCut) note(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.after = append(d.after, s)
}

func (d *runnerCut) Run(ctx context.Context, c Cmd) ([]byte, error) {
	if c.Args[0] == "run" {
		if d.readStdin && c.Stdin != nil {
			if _, err := io.ReadAll(c.Stdin); err != nil {
				d.note("stdin failed")
				return nil, &exec.ExitError{ProcessState: &os.ProcessState{}}
			}
		}
		<-ctx.Done()
		if c.Cancel != nil {
			c.Cancel()
		}
		d.note("cli killed")
		return nil, ctx.Err()
	}
	d.note(strings.Join(c.Args, " "))
	if c.Args[0] == "rm" {
		return nil, d.rmErr
	}
	if c.Args[0] == "stop" {
		return nil, d.stopErr
	}
	return nil, nil
}

// An upload's runner cut off is stopped gracefully first, SIGTERM and a grace for rclone to abort
// its multipart upload, and removed by force only after; a runner uploading nothing (an rclone
// call without a stream, the shell) is removed at once.
func TestACutOffRunnerStopsBeforeItsRemoval(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		run  func(h *Hostd, ctx context.Context, in *Installer) error
		want []string
	}{
		{"an upload", func(h *Hostd, ctx context.Context, in *Installer) error {
			_, err := h.rcloneTo(ctx, in, t.TempDir(), "notes-backup-x", strings.NewReader("dump"), nil, "rcat", "store:x")
			return err
		}, []string{"stop --time 20 notes-backup-x", "cli killed", "rm --force notes-backup-x"}},
		{"a listing", func(h *Hostd, ctx context.Context, in *Installer) error {
			_, err := h.rcloneTo(ctx, in, t.TempDir(), "notes-backup-x", nil, nil, "lsjson", "store:x")
			return err
		}, []string{"cli killed", "rm --force notes-backup-x"}},
		{"the shell", func(h *Hostd, ctx context.Context, in *Installer) error {
			_, err := h.runnerShell(ctx, in, "img", "vol", true, nil, nil, "true")
			return err
		}, nil},
	} {
		d := &runnerCut{}
		h := &Hostd{R: testRelease(t, "1.0.0"), O: Options{Log: io.Discard}}
		in := &Installer{R: h.R, O: Options{Dir: t.TempDir(), Docker: d, Log: io.Discard}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.run(h, ctx, in); err == nil {
			t.Errorf("%s: a run cut off succeeded", c.name)
		}
		if c.want == nil {
			if len(d.after) != 2 || d.after[0] != "cli killed" || !strings.HasPrefix(d.after[1], "rm --force ") {
				t.Errorf("%s: after the cut: %q", c.name, d.after)
			}
		} else if !slices.Equal(d.after, c.want) {
			t.Errorf("%s: after the cut: %q, want %q", c.name, d.after, c.want)
		}
	}
}

// The daemon's stop waits for the worst a backup cut off takes to clean up, and the hostd unit's
// TimeoutStopSec leaves it a margin.
func TestTheDaemonsStopCoversABackupsCleanup(t *testing.T) {
	t.Parallel()
	if worst := runnerCutWait(runnerGrace) + 2*(rcloneTimeout+runnerCutWait(0)) + rollbackStopWait; backupStopWait < worst {
		t.Errorf("backupStopWait %s, a cleanup takes up to %s", backupStopWait, worst)
	}
	unit, err := os.ReadFile("../kairos/files/usr/lib/systemd/system/NAME-hostd.service")
	if err != nil {
		t.Fatal(err)
	}
	var stop time.Duration
	for line := range strings.Lines(string(unit)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "TimeoutStopSec="); ok {
			if stop, err = time.ParseDuration(strings.Replace(v, "min", "m", 1)); err != nil {
				t.Fatalf("TimeoutStopSec=%s: %v", v, err)
			}
		}
	}
	if stop < backupStopWait+30*time.Second {
		t.Errorf("TimeoutStopSec %s, backupStopWait %s: no 30 s margin", stop, backupStopWait)
	}
}

// failingStream is an upload's stream whose producer fails.
type failingStream struct{}

func (failingStream) Read([]byte) (int, error) { return 0, errors.New("the dump failed") }

// An upload whose stream fails stops its runner before the runner sees its input end, so rclone
// does not complete a truncated upload, then removes it, a stop that fails joined to the error; a removal Docker answers is under way (409, the stopped
// container's --rm) is no error.
func TestAFailedStreamStopsTheUploadFirst(t *testing.T) {
	t.Parallel()
	d := &runnerCut{readStdin: true}
	h := &Hostd{R: testRelease(t, "1.0.0"), O: Options{Log: io.Discard}}
	in := &Installer{R: h.R, O: Options{Dir: t.TempDir(), Docker: d, Log: io.Discard}}
	if _, err := h.rcloneTo(context.Background(), in, t.TempDir(), "notes-backup-x", failingStream{}, nil, "rcat", "store:x"); err == nil {
		t.Error("a failed stream's upload succeeded")
	}
	if want := []string{"stop --time 20 notes-backup-x", "stdin failed", "rm --force notes-backup-x"}; !slices.Equal(d.after, want) {
		t.Errorf("%q, want %q", d.after, want)
	}
	// A stop that fails there is the run's error too.
	d = &runnerCut{readStdin: true, stopErr: errors.New("Cannot connect to the Docker daemon")}
	in.O.Docker = d
	if _, err := h.rcloneTo(context.Background(), in, t.TempDir(), "notes-backup-x", failingStream{}, nil, "rcat", "store:x"); err == nil || !strings.Contains(err.Error(), "did not stop: Cannot connect") {
		t.Errorf("a stop that fails: %v", err)
	}
	d = &runnerCut{rmErr: errors.New("Error response from daemon: removal of container notes-backup-x is already in progress")}
	in.O.Docker = d
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h.rcloneTo(ctx, in, t.TempDir(), "notes-backup-x", strings.NewReader("dump"), nil, "rcat", "store:x")
	if err == nil || strings.Contains(err.Error(), "may still be running") {
		t.Errorf("a removal under way: %v", err)
	}
}

// The host's tool passes a run's Cancel on: a cancelled run calls it before the program is killed.
func TestTheHostToolCallsTheRunsCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var called atomic.Bool
	if _, err := (HostTool{}).Run(ctx, Cmd{Args: []string{"sleep", "5"}, Cancel: func() { called.Store(true) }}); err == nil {
		t.Error("a cancelled run succeeded")
	}
	if !called.Load() {
		t.Error("the run's Cancel was not called")
	}
}

// A tool with a grace runs a run's Cancel and then its own: the group interrupted, then killed
// once the grace has passed, its children with it.
func TestAGracefulToolComposesTheRunsCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var called atomic.Bool
	start := time.Now()
	// A child holding the output: only the group's kill ends it before the pipes' delay.
	if _, err := (ExecTool{Bin: "sh", Grace: 100 * time.Millisecond}).Run(ctx, Cmd{Args: []string{"-c", "sleep 5 & wait"}, Cancel: func() { called.Store(true) }}); err == nil {
		t.Error("a cancelled run succeeded")
	}
	if !called.Load() || time.Since(start) > 3*time.Second {
		t.Errorf("Cancel called %v, ended after %s", called.Load(), time.Since(start))
	}
}

// A stop's grace is whole seconds, rounded up, at least 1.
func TestAStopsGraceIsWholeSeconds(t *testing.T) {
	t.Parallel()
	for grace, want := range map[time.Duration]string{20 * time.Second: "20", 1500 * time.Millisecond: "2", 100 * time.Millisecond: "1", time.Second: "1"} {
		if got := stopSeconds(grace); got != want {
			t.Errorf("%s: %s, want %s", grace, got, want)
		}
	}
}
