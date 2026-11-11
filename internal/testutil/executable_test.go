package testutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// executableWrite is a test writing a file executable with os.WriteFile: a program it runs, which a
// child another goroutine forks may hold open for writing as it is run (ETXTBSY).
var executableWrite = regexp.MustCompile(`os\.WriteFile\([^\n]*, 0o7[0-7][0-7]\)`)

// No test writes a program it runs with os.WriteFile: WriteExecutable writes it.
func TestNoTestWritesAProgramWithWriteFile(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	var found []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "bin") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, "executable_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if executableWrite.MatchString(line) {
				found = append(found, p+":"+strconv.Itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("a program written with os.WriteFile (write it with testutil.WriteExecutable): %v", found)
	}
}

// WriteExecutable writes the program, mode 0755, runnable, the PATH of the test aside.
func TestWriteExecutable(t *testing.T) {
	t.Setenv("PATH", "")
	p := filepath.Join(t.TempDir(), "prog")
	if err := WriteExecutable(p, []byte("#!/bin/sh\necho ok\n")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("%v %v", err, st)
	}
	if b, _ := os.ReadFile(p); string(b) != "#!/bin/sh\necho ok\n" {
		t.Errorf("%q", b)
	}
	if err := WriteExecutable(filepath.Join(p, "under-a-file"), nil); err == nil {
		t.Error("a write that cannot be made succeeded")
	}
}
