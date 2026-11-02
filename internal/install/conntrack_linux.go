//go:build linux

package install

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// ctnetlink's delete message (linux/netfilter/nfnetlink_conntrack.h): sent without a tuple, it
// deletes every entry of the family.
const (
	nfnlSubsysCTNetlink = 1
	ipctnlMsgCTDelete   = 2
	conntrackWait       = 10 * time.Second // the longest the kernel's ack is waited for
)

// flushConntrack deletes every entry of the host's connection-tracking table, IPv4 and IPv6
// (`conntrack -F`, through netlink: no tool needed). The packets of a connection open before then
// meet the firewall as new.
func flushConntrack() error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_NETFILTER)
	if err != nil {
		return fmt.Errorf("the conntrack netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("the conntrack netlink socket: %w", err)
	}
	for seq, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		msg := make([]byte, unix.NLMSG_HDRLEN+4)
		binary.NativeEndian.PutUint32(msg[0:], uint32(len(msg)))
		binary.NativeEndian.PutUint16(msg[4:], nfnlSubsysCTNetlink<<8|ipctnlMsgCTDelete)
		binary.NativeEndian.PutUint16(msg[6:], unix.NLM_F_REQUEST|unix.NLM_F_ACK)
		binary.NativeEndian.PutUint32(msg[8:], uint32(seq+1))
		msg[unix.NLMSG_HDRLEN] = family // nfgenmsg: family, version 0, resource 0
		if err := retryEINTR(func() error { return unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}) }); err != nil {
			return fmt.Errorf("flushing the connection-tracking table: %w", err)
		}
		if err := netlinkAck(fd, uint32(seq+1)); err != nil {
			return fmt.Errorf("flushing the connection-tracking table: %w", err)
		}
	}
	return nil
}

// netlinkAck reads the kernel's answer to the message seq: nil for its ack, else its error, within
// conntrackWait however often a signal interrupts the read.
func netlinkAck(fd int, seq uint32) error {
	buf := make([]byte, unix.Getpagesize())
	deadline := time.Now().Add(conntrackWait)
	for {
		var n int
		err := retryEINTR(func() (err error) {
			// Each read waits the time left at most: the kernel acks each message.
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
