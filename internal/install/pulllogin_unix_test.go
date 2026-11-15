//go:build unix

package install

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// inheritable is a descriptor as a child finds the one its parent handed: not close-on-exec.
func inheritable(t *testing.T, f *os.File) int {
	t.Helper()
	fd := int(f.Fd())
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		t.Fatal(err)
	}
	return fd
}

// keptFromChildren reports whether fd is close-on-exec.
func keptFromChildren(t *testing.T, fd int) bool {
	t.Helper()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil && flags&unix.FD_CLOEXEC != 0
}

// A login read from a path naming a descriptor a parent handed (childLogin), in any of its
// spellings, leaves that descriptor close-on-exec: no process the run starts (helm, kubectl, a
// hook) inherits it. A read that fails leaves it so too.
func TestALoginReadFromADescriptorIsKeptFromChildren(t *testing.T) {
	t.Parallel()
	pid := strconv.Itoa(os.Getpid())
	for _, spell := range []string{"/dev/fd/%d", "/proc/self/fd/%d", "/proc/" + pid + "/fd/%d", "/dev//fd/./%d", "link"} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		fd := inheritable(t, r)
		if _, err := w.WriteString(`{"auths": {}}`); err != nil {
			t.Fatal(err)
		}
		w.Close()
		path := strings.Replace(spell, "%d", strconv.Itoa(fd), 1)
		if spell == "link" { // a symlink to /dev/fd/N, itself named through a symlink
			dir := t.TempDir()
			if err := os.Symlink("/dev/fd/"+strconv.Itoa(fd), filepath.Join(dir, "login")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("login", filepath.Join(dir, "again")); err != nil {
				t.Fatal(err)
			}
			path = filepath.Join(dir, "again")
		}
		raw, err := readLoginFlag(path, strings.NewReader(""))
		if err != nil || string(raw) != `{"auths": {}}` {
			t.Errorf("%s: %q %v", path, raw, err)
		}
		if !keptFromChildren(t, fd) {
			t.Errorf("%s: children inherit it", path)
		}
		r.Close()
	}
	// A descriptor whose read fails: a directory's.
	d, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fd := inheritable(t, d)
	if _, err := readLoginFlag("/dev/fd/"+strconv.Itoa(fd), strings.NewReader("")); err == nil {
		t.Error("a directory read as a login")
	}
	if !keptFromChildren(t, fd) {
		t.Error("a failed read: children inherit it")
	}
}

// Only this process's descriptors past standard error are named.
func TestInheritedFDIsThisProcesssPastStandardError(t *testing.T) {
	t.Parallel()
	pid := os.Getpid()
	tid := ""
	tasks, _ := os.ReadDir("/proc/self/task")
	for _, task := range tasks {
		if task.Name() != strconv.Itoa(pid) {
			tid = task.Name()
		}
	}
	cases := map[string]int{
		"/dev/fd/5": 5, "/proc/self/fd/7": 7, "/proc/thread-self/fd/8": 8, "/proc/" + strconv.Itoa(pid) + "/task/1/fd/9": 9,
		"/dev/fd/2": -1, "/dev/stdin": -1, "/proc/self/fd/0": -1, "/proc/4194305/fd/5": -1, // past pid_max: no process's
		"/dev/fd/x": -1, "dev/fd/5": -1, "/tmp/fd/5": -1, "-": -1,
	}
	if tid != "" { // one of this process's threads
		cases["/proc/"+tid+"/fd/6"] = 6
	} else if _, err := os.Stat("/proc/self/task"); err == nil {
		t.Fatal("no thread but the main one")
	}
	for path, want := range cases {
		fd, ok := inheritedFD(path)
		if (want < 0 && ok) || (want >= 0 && (!ok || fd != want)) {
			t.Errorf("inheritedFD(%q) = %d %v, want %d", path, fd, ok, want)
		}
	}
}
