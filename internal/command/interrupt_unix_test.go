//go:build unix

package command

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An interrupt to stowage reaches the command although it runs in its own process group: the
// group is killed and stowage dies of the same signal. The test re-runs itself as the "stowage"
// process (STOWAGE_TEST_HELPER) and interrupts it.
func TestInterruptKillsTheCommandGroup(t *testing.T) {
	if dir := os.Getenv("STOWAGE_TEST_HELPER"); dir != "" {
		r := Runner{Root: dir, Config: Config{Cmd: []string{"./c.sh"}, TimeoutS: 60}}
		_ = r.Call(context.Background(), map[string]any{"op": "x"}, nil)
		os.Exit(0) // reached only if the interrupt did not end the process
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleep.pid")
	body := "#!/bin/sh\nsleep 60 & echo $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "c.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestInterruptKillsTheCommandGroup$")
	helper.Env = append(os.Environ(), "STOWAGE_TEST_HELPER="+dir)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	var sleepPid int
	for deadline := time.Now().Add(10 * time.Second); sleepPid == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = helper.Process.Kill()
			t.Fatal("the command never started")
		}
		if b, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(b), "\n") {
			sleepPid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	if err := helper.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := helper.Wait()
	ws, ok := helper.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Errorf("stowage did not die of SIGINT: %v", err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		st, err := os.ReadFile("/proc/" + strconv.Itoa(sleepPid) + "/stat")
		if err != nil || strings.Contains(string(st), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(sleepPid, syscall.SIGKILL)
			t.Fatalf("the command's child %d outlived the interrupt", sleepPid)
		}
	}
}

// A signal stowage was started ignoring (nohup) stays ignored: a hangup neither kills the command
// nor ends stowage, and the call returns the command's answer.
func TestIgnoredHangupStaysIgnored(t *testing.T) {
	if dir := os.Getenv("STOWAGE_TEST_NOHUP"); dir != "" {
		r := Runner{Root: dir, Config: Config{Cmd: []string{"./c.sh"}, TimeoutS: 30}}
		var out struct{ OK bool }
		if err := r.Call(context.Background(), map[string]any{"op": "x"}, &out); err != nil || !out.OK {
			os.Exit(3)
		}
		os.Exit(0)
	}
	if _, err := exec.LookPath("nohup"); err != nil {
		t.Skip("nohup not installed")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	// The command answers only once the hangup was sent: it always lands during the run.
	body := "#!/bin/sh\ntouch " + marker + "\nwhile [ ! -e " + release + " ]; do sleep 0.02; done\necho '{\"ok\":true}'\n"
	if err := os.WriteFile(filepath.Join(dir, "c.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command("nohup", os.Args[0], "-test.run=^TestIgnoredHangupStaysIgnored$")
	helper.Env = append(os.Environ(), "STOWAGE_TEST_NOHUP="+dir)
	helper.Dir = dir
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = helper.Process.Kill()
			t.Fatal("the command never started")
		}
	}
	if err := helper.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // for the hangup to be handled, were it watched
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := helper.Wait(); err != nil {
		t.Fatalf("a hangup under nohup ended the run: %v", err)
	}
}

// procState returns the state letter of a process from /proc ("" once it is gone).
func procState(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return ""
	}
	return s[i+2 : i+3]
}

func waitState(t *testing.T, pid int, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); procState(pid) != want; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is in state %q, want %q", pid, procState(pid), want)
		}
	}
}

// Ctrl-Z stops the command's group with stowage, and SIGCONT resumes both: job control behaves as
// if the command were still in stowage's process group.
func TestJobControlReachesTheCommandGroup(t *testing.T) {
	if dir := os.Getenv("STOWAGE_TEST_TSTP"); dir != "" {
		forceForeground = os.Getenv("STOWAGE_TEST_BACKGROUND") == "" // the helper has no terminal
		timeout := 60
		if v := os.Getenv("STOWAGE_TEST_TSTP_TIMEOUT"); v != "" {
			timeout, _ = strconv.Atoi(v)
		}
		r := Runner{Root: dir, Config: Config{Cmd: []string{"./c.sh"}, TimeoutS: timeout}, unit: testSecond}
		var out struct{ OK bool }
		if err := r.Call(context.Background(), map[string]any{"op": "x"}, &out); err != nil || !out.OK {
			os.Exit(3)
		}
		os.Exit(0)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleep.pid")
	release := filepath.Join(dir, "release")
	body := "#!/bin/sh\nsleep 60 & echo $! > " + pidFile + "\nwhile [ ! -e " + release + " ]; do sleep 0.05; done\nkill $!\necho '{\"ok\":true}'\n"
	if err := os.WriteFile(filepath.Join(dir, "c.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestJobControlReachesTheCommandGroup$")
	helper.Env = append(os.Environ(), "STOWAGE_TEST_TSTP="+dir)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = helper.Process.Kill() }()
	var sleepPid int
	for deadline := time.Now().Add(10 * time.Second); sleepPid == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
		if b, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(b), "\n") {
			sleepPid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	if err := helper.Process.Signal(syscall.SIGTSTP); err != nil {
		t.Fatal(err)
	}
	waitState(t, sleepPid, "T")
	waitState(t, helper.Process.Pid, "T")
	if err := helper.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	waitState(t, sleepPid, "S")
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := helper.Wait(); err != nil {
		t.Fatalf("the call did not complete after resuming: %v", err)
	}
}

// Time spent suspended does not count against the command's timeout: a stop longer than the
// timeout, then a resume, still lets the command answer.
func TestSuspendDoesNotConsumeTheTimeout(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleep.pid")
	release := filepath.Join(dir, "release")
	body := "#!/bin/sh\nsleep 60 & echo $! > " + pidFile + "\nwhile [ ! -e " + release + " ]; do sleep 0.05; done\nkill $!\necho '{\"ok\":true}'\n"
	if err := os.WriteFile(filepath.Join(dir, "c.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestJobControlReachesTheCommandGroup$")
	helper.Env = append(os.Environ(), "STOWAGE_TEST_TSTP="+dir, "STOWAGE_TEST_TSTP_TIMEOUT=5")
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = helper.Process.Kill() }()
	var sleepPid int
	for deadline := time.Now().Add(10 * time.Second); sleepPid == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
		if b, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(b), "\n") {
			sleepPid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	if err := helper.Process.Signal(syscall.SIGTSTP); err != nil {
		t.Fatal(err)
	}
	waitState(t, helper.Process.Pid, "T")
	time.Sleep(8 * testSecond) // longer than the helper's 5-unit timeout
	if err := helper.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	waitState(t, sleepPid, "S")
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := helper.Wait(); err != nil {
		t.Fatalf("the suspend counted against the timeout: %v", err)
	}
}

// Off the terminal's foreground a SIGTSTP is dropped: stowage and the command keep running, so a
// stray stop can never leave the run stopped with nobody to continue it.
func TestStopOffTheForegroundIsDropped(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	body := "#!/bin/sh\ntouch " + marker + "\nwhile [ ! -e " + release + " ]; do sleep 0.05; done\necho '{\"ok\":true}'\n"
	if err := os.WriteFile(filepath.Join(dir, "c.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestJobControlReachesTheCommandGroup$")
	helper.Env = append(os.Environ(), "STOWAGE_TEST_TSTP="+dir, "STOWAGE_TEST_BACKGROUND=1")
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = helper.Process.Kill() }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
	}
	if err := helper.Process.Signal(syscall.SIGTSTP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if st := procState(helper.Process.Pid); st == "T" {
		t.Fatal("stowage stopped on a SIGTSTP off the terminal's foreground")
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := helper.Wait(); err != nil {
		t.Fatalf("the call did not complete: %v", err)
	}
}
