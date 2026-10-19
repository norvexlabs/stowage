//go:build !linux

package install

import "errors"

// noEcho is Linux-only: the installer runs where Docker Engine does.
func noEcho(int) (func(), error) {
	return nil, errors.New("a secret cannot be read without echo on this system: use --set or --answers")
}
