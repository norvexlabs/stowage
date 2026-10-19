package install

import "golang.org/x/sys/unix"

// noEcho turns a terminal's echo off and returns what puts it back.
func noEcho(fd int) (restore func(), err error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	t := *old
	t.Lflag &^= unix.ECHO
	t.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &t); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }, nil
}
