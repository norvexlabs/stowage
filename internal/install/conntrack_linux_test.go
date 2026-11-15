//go:build linux

package install

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// The deletions go as one buffer of messages numbered in turn, the last alone asking an ack.
func TestConntrackDeletionsGoInOneBufferAckedOnce(t *testing.T) {
	t.Parallel()
	entries := []ctEntry{{family: unix.AF_INET, del: []byte{8, 0, 1, 0, 1, 2, 3, 4}}, {family: unix.AF_INET6, del: []byte{6, 0, 18, 0, 0, 0, 0, 0}}, {family: unix.AF_INET, del: []byte{8, 0, 1, 0, 5, 6, 7, 8}}}
	msgs, err := parseNetlink(ctDeletes(entries, 7))
	if err != nil || len(msgs) != 3 {
		t.Fatalf("%d messages: %v", len(msgs), err)
	}
	raw := ctDeletes(entries, 7)
	off := 0
	for i, m := range msgs {
		flags := binary.NativeEndian.Uint16(raw[off+6:])
		if m.seq != uint32(7+i) || m.typ != nfnlSubsysCTNetlink<<8|ipctnlMsgCTDelete || (flags&unix.NLM_F_ACK != 0) != (i == 2) || m.data[0] != entries[i].family || !bytes.Equal(m.data[4:], entries[i].del) {
			t.Errorf("message %d: seq %d, type %#x, flags %#x, data %x", i, m.seq, m.typ, flags, m.data)
		}
		off += int(binary.NativeEndian.Uint32(raw[off:]))
	}
}

// A dump the kernel marks interrupted ends in errDumpInterrupted, which is dumped again once and
// is an error when interrupted again; one not interrupted ends clean.
func TestAnInterruptedDumpIsRedoneOnce(t *testing.T) {
	t.Parallel()
	done := netlinkMsg{typ: unix.NLMSG_DONE, seq: 3}
	part := netlinkMsg{typ: nfnlSubsysCTNetlink<<8 | ipctnlMsgCTNew, seq: 3, data: []byte{2, 0, 0, 0}}
	for _, c := range []struct {
		msgs []netlinkMsg
		want error
	}{
		{[]netlinkMsg{part, done}, nil},
		{[]netlinkMsg{{typ: part.typ, flags: unix.NLM_F_DUMP_INTR, seq: 3, data: part.data}, done}, errDumpInterrupted},
		{[]netlinkMsg{part, {typ: unix.NLMSG_DONE, flags: unix.NLM_F_DUMP_INTR, seq: 3}}, errDumpInterrupted},
	} {
		interrupted := false
		end, err := ctDumpPart(c.msgs, 3, 2, func(ctEntry) {}, &interrupted)
		if !end || !errors.Is(err, c.want) || (err == nil) != (c.want == nil) {
			t.Errorf("%+v: %v %v", c.msgs, end, err)
		}
	}
	for _, c := range []struct {
		results []error
		want    error
		runs    int
	}{
		{[]error{nil}, nil, 1},
		{[]error{errDumpInterrupted, nil}, nil, 2},
		{[]error{errDumpInterrupted, errDumpInterrupted}, errDumpInterrupted, 2},
		{[]error{unix.EPERM}, unix.EPERM, 1},
	} {
		runs := 0
		err := redoInterrupted(func() error { runs++; return c.results[runs-1] })
		if !errors.Is(err, c.want) || (err == nil) != (c.want == nil) || runs != c.runs {
			t.Errorf("%v: %v after %d runs", c.results, err, runs)
		}
	}
}

// A batch that fails does not stop the next: the entries deleted are counted, the failed ones
// not, and the first failure is answered at the end; past the whole deletion's bound, the rest
// is not sent.
func TestADeletionGoesOnPastAFailedBatch(t *testing.T) {
	t.Parallel()
	found := make([]ctEntry, ctDeleteBatch+10)
	for i := range found {
		found[i] = ctEntry{family: unix.AF_INET, del: []byte{4, 0, 1, 0}}
	}
	var batches [][2]uint32
	n, err := deleteBatches(found, 4, time.Minute, func(msg []byte, first, last uint32, _ time.Time) (int, error) {
		batches = append(batches, [2]uint32{first, last})
		if first == 5 {
			return 3, unix.EPERM
		}
		return 0, nil
	})
	if n != len(found)-3 || !errors.Is(err, unix.EPERM) || len(batches) != 2 || batches[1] != [2]uint32{5 + ctDeleteBatch, 4 + uint32(len(found))} {
		t.Errorf("%d deleted, %v, batches %v", n, err, batches)
	}
	n, err = deleteBatches(found, 0, 0, func([]byte, uint32, uint32, time.Time) (int, error) { t.Error("sent past the bound"); return 0, nil })
	if n != 0 || err == nil || !strings.Contains(err.Error(), "not done within") {
		t.Errorf("past the bound: %d, %v", n, err)
	}
	// The answers of a batch: a failure counted, an entry gone already not, up to the ack.
	errMsg := func(seq uint32, code int32) netlinkMsg {
		return netlinkMsg{typ: unix.NLMSG_ERROR, seq: seq, data: binary.NativeEndian.AppendUint32(nil, uint32(code))}
	}
	a := ctAcks{first: 1, last: 4}
	if a.read([]netlinkMsg{errMsg(1, -int32(unix.ENOENT)), errMsg(2, -int32(unix.EPERM))}) || !a.read([]netlinkMsg{errMsg(9, -1), errMsg(4, 0)}) || a.failed != 1 || !errors.Is(a.err, unix.EPERM) {
		t.Errorf("%+v", a)
	}
}
