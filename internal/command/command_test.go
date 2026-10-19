package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func script(t *testing.T, body string) Runner {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c.sh"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return Runner{Root: dir, Config: Config{Cmd: []string{"./c.sh"}, TimeoutS: 5}}
}

func TestCallRoundTrip(t *testing.T) {
	// The request arrives on stdin as JSON; the answer is decoded from stdout.
	r := script(t, `read -r req; case "$req" in *'"op":"ping"'*) echo '{"pong":true}';; *) exit 1;; esac`)
	var out struct{ Pong bool }
	if err := r.Call(context.Background(), map[string]any{"op": "ping"}, &out); err != nil || !out.Pong {
		t.Fatalf("Call = %+v, %v", out, err)
	}
}

func TestExitTwoIsNoAnswer(t *testing.T) {
	r := script(t, "exit 2")
	if err := r.Call(context.Background(), map[string]any{"op": "x"}, nil); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err = %v, want ErrNoAnswer", err)
	}
}

func TestOtherExitIsAnErrorWithStderr(t *testing.T) {
	r := script(t, "echo boom >&2; exit 3")
	err := r.Call(context.Background(), map[string]any{"op": "x"}, nil)
	if err == nil || errors.Is(err, ErrNoAnswer) || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "exit 3") {
		t.Fatalf("err = %v", err)
	}
}

func TestNonJSONAnswer(t *testing.T) {
	r := script(t, "echo not json")
	var out map[string]any
	if err := r.Call(context.Background(), map[string]any{"op": "x"}, &out); err == nil {
		t.Fatal("non-JSON answer accepted")
	}
}

func TestOversizedAnswer(t *testing.T) {
	r := script(t, `head -c 1100000 /dev/zero | tr '\0' a`)
	if err := r.Call(context.Background(), map[string]any{"op": "x"}, nil); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v", err)
	}
}

// testSecond is what one timeout_s counts in the tests that wait one out.
const testSecond = 200 * time.Millisecond

func TestTimeoutSCountsSeconds(t *testing.T) {
	if (Runner{}).second() != time.Second {
		t.Fatalf("timeout_s counts %s", (Runner{}).second())
	}
}

func TestTimeout(t *testing.T) {
	// Without exec, sleep is a child holding stdout after the shell is killed: the timeout must
	// still hold.
	for _, body := range []string{"exec sleep 30", "sleep 30; echo '{}'", "sleep 30 & wait"} {
		r := script(t, body)
		r.Config.TimeoutS, r.unit = 1, testSecond
		start := time.Now()
		err := r.Call(context.Background(), map[string]any{"op": "x"}, nil)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Errorf("%q: err = %v", body, err)
		}
		// Below waitDelay: the group kill ends the call, not the wait for the pipes giving up.
		if d := time.Since(start); d > testSecond+waitDelay/2 {
			t.Errorf("%q: Call took %s, timeout %s", body, d, testSecond)
		}
	}
}

// A command that answers and leaves a child holding stdout succeeds, and the child is killed.
func TestAnswerWithLingeringChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	r := script(t, "sleep 30 & echo $! > "+pidFile+"; echo '{\"ok\":true}'")
	var out struct{ OK bool }
	if err := r.Call(context.Background(), map[string]any{"op": "x"}, &out); err != nil || !out.OK {
		t.Fatalf("Call = %+v, %v", out, err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(b))
	deadline := time.Now().Add(2 * time.Second)
	for {
		// kill -0 fails once the process is gone (a zombie is reaped by init in the container).
		if _, err := os.Stat("/proc/" + pid + "/cmdline"); err != nil {
			return
		}
		st, _ := os.ReadFile("/proc/" + pid + "/stat")
		if strings.Contains(string(st), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %s still running after the call", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRelativeRoot(t *testing.T) {
	r := script(t, `echo '{"ok":true}'`)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, r.Root)
	if err != nil {
		t.Fatal(err)
	}
	r.Root = rel
	var out struct{ OK bool }
	if err := r.Call(context.Background(), map[string]any{"op": "x"}, &out); err != nil || !out.OK {
		t.Fatalf("relative root: %+v, %v", out, err)
	}
}

func TestValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Error("empty cmd accepted")
	}
	if err := (Config{Cmd: []string{"x"}, TimeoutS: -1}).Validate(); err == nil {
		t.Error("negative timeout accepted")
	}
}
