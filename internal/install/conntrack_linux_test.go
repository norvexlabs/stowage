//go:build linux

package install

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A flush failing for want of a conntrack subsystem is nothing to flush only on a kernel that
// tracks no connection: with the table there, it stays an error, as does any other errno.
func TestAKernelWithoutConnectionTrackingHasNothingToFlush(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	absent, present := filepath.Join(dir, "absent"), filepath.Join(dir, "nf_conntrack_count")
	if err := os.WriteFile(present, []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		err   error
		count string
		want  bool
	}{
		{fmt.Errorf("flushing: %w", unix.EOPNOTSUPP), absent, true},
		{fmt.Errorf("the conntrack netlink socket: %w", unix.EPROTONOSUPPORT), absent, true},
		{unix.ENOENT, absent, true},
		{unix.EOPNOTSUPP, present, false},
		{unix.EPERM, absent, false},
	} {
		if got := untracked(c.err, c.count); got != c.want {
			t.Errorf("%v, count %s: %v", c.err, filepath.Base(c.count), got)
		}
	}
}
