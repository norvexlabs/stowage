//go:build linux

package install

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

// ctnetlink's delete message (linux/netfilter/nfnetlink_conntrack.h): sent without a tuple, it
// deletes every entry of the family.
const (
	nfnlSubsysCTNetlink = 1
	ipctnlMsgCTNew      = 0 // a dump's entry
	ipctnlMsgCTGet      = 1
	ipctnlMsgCTDelete   = 2
	conntrackWait       = 10 * time.Second // the longest the kernel's ack is waited for
)

// conntrackCount is the count of the connection-tracking table's entries, there while the kernel
// tracks connections (nf_conntrack).
const conntrackCount = "/proc/sys/net/netfilter/nf_conntrack_count"

// flushConntrack deletes every entry of the host's connection-tracking table, IPv4 and IPv6
// (`conntrack -F`, through netlink: no tool needed). The packets of a connection open before then
// meet the firewall as new. A kernel that tracks no connection has nothing to flush: no error.
func flushConntrack() error {
	if err := flushTable(); err != nil && !untracked(err, conntrackCount) {
		return err
	}
	return nil
}

// untracked reports whether err, a flush's, is a kernel's that tracks no connection: netlink has
// no conntrack subsystem, and the table's count (count) is absent. A kernel tracking connections
// whose ctnetlink is missing has a table the flush did not empty: not untracked.
func untracked(err error, count string) bool {
	if !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.EPROTONOSUPPORT) && !errors.Is(err, unix.ENOENT) {
		return false
	}
	_, serr := os.Stat(count)
	return errors.Is(serr, os.ErrNotExist)
}

// flushTable is flushConntrack's netlink exchange.
func flushTable() error {
	fd, err := ctSocket()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	for seq, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := ctSend(fd, ipctnlMsgCTDelete, unix.NLM_F_ACK, uint32(seq+1), family, nil); err != nil {
			return fmt.Errorf("flushing the connection-tracking table: %w", err)
		}
		if err := netlinkAck(fd, uint32(seq+1)); err != nil {
			return fmt.Errorf("flushing the connection-tracking table: %w", err)
		}
	}
	return nil
}

// deleteConntrack deletes the entries of the host's connection-tracking table, IPv4 and IPv6, that
// match: the table dumped, then each entry matched deleted by its original tuple and zone (as
// `conntrack -D` does), one gone since counted as deleted all the same. A kernel that tracks no
// connection has none to delete.
func deleteConntrack(match func(ctEntry) bool) (int, error) {
	n, err := deleteEntries(match)
	if err != nil && untracked(err, conntrackCount) {
		return 0, nil
	}
	return n, err
}

// deleteEntries is deleteConntrack's netlink exchange.
func deleteEntries(match func(ctEntry) bool) (int, error) {
	fd, err := ctSocket()
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	var seq uint32
	var found []ctEntry
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		var got []ctEntry
		if err := redoInterrupted(func() error {
			got = got[:0]
			seq++
			if err := ctSend(fd, ipctnlMsgCTGet, unix.NLM_F_DUMP, seq, family, nil); err != nil {
				return err
			}
			return ctDump(fd, seq, func(c ctEntry) {
				if match(c) {
					got = append(got, c)
				}
			}, family)
		}); err != nil {
			return 0, fmt.Errorf("reading the connection-tracking table: %w", err)
		}
		found = append(found, got...)
	}
	buf := make([]byte, 1<<16)
	return deleteBatches(found, seq, ctDeleteBound, func(msg []byte, first, last uint32, deadline time.Time) (int, error) {
		if err := netlinkSend(fd, msg); err != nil {
			return int(last - first + 1), err
		}
		return ctBatchAck(fd, buf, first, last, deadline)
	})
}

// ctDeleteBound caps a whole deletion, however many batches it takes.
const ctDeleteBound = 60 * time.Second

// deleteBatches deletes found in batches of ctDeleteBatch, numbered after seq, one message each,
// the last alone acked: the kernel answers a failure of any, so exchange sends a batch and reads
// its answers once, up to its ack, by its deadline (conntrackWait, within bound of the start), and
// says how many of it failed. A batch that fails does not stop the next; past bound, the rest is
// not sent. It answers the entries deleted (one gone already counts) and the first failure.
func deleteBatches(found []ctEntry, seq uint32, bound time.Duration, exchange func(msg []byte, first, last uint32, deadline time.Time) (int, error)) (int, error) {
	end := time.Now().Add(bound)
	deleted := 0
	var first error
	for batch := range slices.Chunk(found, ctDeleteBatch) {
		if !time.Now().Before(end) {
			return deleted, errors.Join(first, fmt.Errorf("deleting connection-tracking entries: not done within %s (%d of %d deleted)", bound, deleted, len(found)))
		}
		lo := seq + 1
		seq += uint32(len(batch))
		deadline := time.Now().Add(conntrackWait)
		if end.Before(deadline) {
			deadline = end
		}
		failed, err := exchange(ctDeletes(batch, lo), lo, seq, deadline)
		deleted += len(batch) - min(failed, len(batch))
		if err != nil && first == nil {
			first = fmt.Errorf("deleting connection-tracking entries: %w", err)
		}
	}
	return deleted, first
}

// ctDeleteBatch is how many deletions one send carries: their failures, each an answer, stay
// well within the socket's receive buffer.
const ctDeleteBatch = 64

// ctDeletes is one buffer of the deletions of entries, numbered from first, the last alone asking
// an ack.
func ctDeletes(entries []ctEntry, first uint32) []byte {
	var b []byte
	for i, c := range entries {
		flags := uint16(0)
		if i == len(entries)-1 {
			flags = unix.NLM_F_ACK
		}
		b = append(b, ctMessage(ipctnlMsgCTDelete, flags, first+uint32(i), c.family, c.del)...)
	}
	return b
}

// ctBatchAck reads into buf the answers to the messages first to last until last's ack, by
// deadline: how many failed (all, when the answers do not read) and the first failure.
func ctBatchAck(fd int, buf []byte, first, last uint32, deadline time.Time) (int, error) {
	acks := ctAcks{first: first, last: last}
	for {
		n, err := netlinkRecv(fd, buf, deadline)
		if err != nil {
			// What the batch deleted is not known: none counted.
			return int(last - first + 1), err
		}
		msgs, err := parseNetlink(buf[:n])
		if err != nil {
			return int(last - first + 1), err
		}
		if acks.read(msgs) {
			return acks.failed, acks.err
		}
	}
}

// ctAcks reads the answers to the batch first to last: the deletions that failed (an entry gone
// already is no failure) and the first failure, until last's ack.
type ctAcks struct {
	first, last uint32
	failed      int
	err         error
}

// read takes one read's messages, and reports whether last's ack was among them.
func (a *ctAcks) read(msgs []netlinkMsg) bool {
	for _, m := range msgs {
		if m.typ != unix.NLMSG_ERROR || m.seq < a.first || m.seq > a.last {
			continue
		}
		code := int32(0)
		if len(m.data) >= 4 {
			code = int32(binary.NativeEndian.Uint32(m.data))
		} else {
			code = -int32(unix.EBADMSG)
		}
		if code != 0 && !errors.Is(unix.Errno(-code), unix.ENOENT) {
			a.failed++
			if a.err == nil {
				a.err = unix.Errno(-code)
			}
		}
		if m.seq == a.last {
			return true
		}
	}
	return false
}

// ctSocket opens a netfilter netlink socket.
func ctSocket() (int, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_NETFILTER)
	if err != nil {
		return 0, fmt.Errorf("the conntrack netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		unix.Close(fd)
		return 0, fmt.Errorf("the conntrack netlink socket: %w", err)
	}
	return fd, nil
}

// ctSend sends the ctnetlink message typ of the family, with its flags beside NLM_F_REQUEST and
// its attributes.
func ctSend(fd int, typ, flags uint16, seq uint32, family uint8, attrs []byte) error {
	return netlinkSend(fd, ctMessage(typ, flags, seq, family, attrs))
}

// netlinkSend sends a built buffer of messages to the kernel, again while a signal interrupts it.
func netlinkSend(fd int, msg []byte) error {
	return retryEINTR(func() error { return unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}) })
}

// ctMessage is the ctnetlink message typ of the family, with its flags beside NLM_F_REQUEST and
// its attributes, padded to 4 bytes.
func ctMessage(typ, flags uint16, seq uint32, family uint8, attrs []byte) []byte {
	msg := make([]byte, unix.NLMSG_HDRLEN+4, unix.NLMSG_HDRLEN+4+len(attrs)+3)
	msg = append(msg, attrs...)
	for len(msg)%4 != 0 {
		msg = append(msg, 0)
	}
	binary.NativeEndian.PutUint32(msg[0:], uint32(len(msg)))
	binary.NativeEndian.PutUint16(msg[4:], nfnlSubsysCTNetlink<<8|typ)
	binary.NativeEndian.PutUint16(msg[6:], unix.NLM_F_REQUEST|flags)
	binary.NativeEndian.PutUint32(msg[8:], seq)
	msg[unix.NLMSG_HDRLEN] = family // nfgenmsg: family, version 0, resource 0
	return msg
}

// ctDump reads the answer to the dump seq, giving each entry to each, until its end: each part
// of it waited for conntrackWait at most, however large the table. A dump the kernel marks
// interrupted (the table changed under it: entries may be missing) is errDumpInterrupted once
// read to its end.
func ctDump(fd int, seq uint32, each func(ctEntry), family uint8) error {
	buf := make([]byte, 1<<16)
	interrupted := false
	for {
		n, err := netlinkRecv(fd, buf, time.Now().Add(conntrackWait))
		if err != nil {
			return err
		}
		msgs, err := parseNetlink(buf[:n])
		if err != nil {
			return err
		}
		done, err := ctDumpPart(msgs, seq, family, each, &interrupted)
		if err != nil || done {
			return err
		}
	}
}

// ctDumpPart handles one read of the dump seq: whether it ended it, and its error; a part marked
// interrupted (NLM_F_DUMP_INTR) sets interrupted, and the dump's end is errDumpInterrupted then.
func ctDumpPart(msgs []netlinkMsg, seq uint32, family uint8, each func(ctEntry), interrupted *bool) (bool, error) {
	for _, m := range msgs {
		if m.seq != seq {
			continue
		}
		if m.flags&unix.NLM_F_DUMP_INTR != 0 {
			*interrupted = true
		}
		switch {
		case m.typ == unix.NLMSG_DONE:
			if *interrupted {
				return true, errDumpInterrupted
			}
			return true, nil
		case m.typ == unix.NLMSG_ERROR:
			if len(m.data) < 4 {
				return true, fmt.Errorf("a netlink error of %d bytes", len(m.data))
			}
			if code := int32(binary.NativeEndian.Uint32(m.data)); code != 0 {
				return true, unix.Errno(-code)
			}
			return true, nil
		case m.typ == nfnlSubsysCTNetlink<<8|ipctnlMsgCTNew && len(m.data) >= 4:
			c, ok, err := parseCTEntry(family, m.data[4:])
			if err != nil {
				return true, err
			}
			if ok {
				each(c)
			}
		}
	}
	return false, nil
}

// errDumpInterrupted is a dump the table changed under.
var errDumpInterrupted = errors.New("the table changed while it was read (the dump was interrupted twice)")

// redoInterrupted runs dump, again once when it was interrupted; interrupted again, its error.
func redoInterrupted(dump func() error) error {
	if err := dump(); !errors.Is(err, errDumpInterrupted) {
		return err
	}
	return dump()
}

// netlinkRecv reads one answer into buf, waiting until deadline at most however often a signal
// interrupts the read.
func netlinkRecv(fd int, buf []byte, deadline time.Time) (int, error) {
	var n int
	err := retryEINTR(func() (err error) {
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("no answer from the kernel in %s", conntrackWait)
		}
		tv := unix.NsecToTimeval(left.Nanoseconds())
		if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
			return err
		}
		n, _, err = unix.Recvfrom(fd, buf, 0)
		if errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("no answer from the kernel in %s", conntrackWait)
		}
		return err
	})
	return n, err
}

// netlinkAck reads the kernel's answer to the message seq: nil for its ack, else its error, within
// conntrackWait however often a signal interrupts the read.
func netlinkAck(fd int, seq uint32) error {
	buf := make([]byte, unix.Getpagesize())
	deadline := time.Now().Add(conntrackWait)
	for {
		// Each read waits the time left at most: the kernel acks each message.
		n, err := netlinkRecv(fd, buf, deadline)
		if err != nil {
			return err
		}
		msgs, err := parseNetlink(buf[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.seq != seq || m.typ != unix.NLMSG_ERROR {
				continue
			}
			if len(m.data) < 4 {
				return fmt.Errorf("a netlink error of %d bytes", len(m.data))
			}
			if code := int32(binary.NativeEndian.Uint32(m.data)); code != 0 {
				return unix.Errno(-code)
			}
			return nil
		}
	}
}

// retryEINTR runs call again while a signal interrupts it: the kernel does not restart a call on a
// socket with a timeout (signal(7)).
func retryEINTR(call func() error) error {
	for {
		if err := call(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
