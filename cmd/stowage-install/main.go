// Command stowage-install is the installer and lifecycle command of one release (`<short>-stow`):
// the release it installs is carried by the executable itself (internal/payload).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/norvexlabs/stowage/internal/install"
	"github.com/norvexlabs/stowage/internal/payload"
)

func main() {
	if err := run(); err != nil {
		var code install.ExitCode
		if errors.As(err, &code) {
			os.Exit(int(code)) // the installer it ran printed its error
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	files, err := payload.Read(exe)
	if err != nil {
		return err
	}
	r, err := install.Load(files)
	if err != nil {
		return err
	}
	// A closed terminal (SIGHUP) stops the run like Ctrl-C, so the private pull config is removed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()
	return install.Command(r, exe, os.Stdout, os.Stderr).ExecuteContext(ctx)
}
