//go:build !linux

package install

import "errors"

// sendRouterSolicitation has no ICMPv6 raw socket to send through off Linux.
func sendRouterSolicitation(string) error {
	return errors.New("router solicitations are sent on Linux")
}
