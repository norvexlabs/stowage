package install

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// sendRouterSolicitation sends one ICMPv6 router solicitation (RFC 4861 § 4.1) to the all-routers
// address on iface, with a hop limit of 255 as routers require. The kernel picks the link-local
// source and computes the checksum; the solicitation carries no source link-layer option, which
// routers need not have (they resolve the address, or answer to all nodes). While the link-local
// address is still tentative the send fails (no source to use), and the caller's backoff sends
// again. A router answers with an advertisement, which networkd's own listener takes.
func sendRouterSolicitation(iface string) error {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
	if err != nil {
		return fmt.Errorf("an ICMPv6 socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, 255); err != nil {
		return fmt.Errorf("the hop limit: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, ifi.Index); err != nil {
		return fmt.Errorf("the interface: %w", err)
	}
	to := &unix.SockaddrInet6{ZoneId: uint32(ifi.Index)}
	copy(to.Addr[:], net.ParseIP("ff02::2").To16())
	// Type 133, code 0, the checksum (the kernel's), 4 reserved bytes.
	return retryEINTR(func() error { return unix.Sendto(fd, []byte{133, 0, 0, 0, 0, 0, 0, 0}, 0, to) })
}
