// Command stowage-install is the installer and lifecycle command of one release (`<short>-stow`):
// the release it installs is carried by the executable itself (internal/payload).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/norvexlabs/stowage/internal/install"
	"github.com/norvexlabs/stowage/internal/payload"
)

func main() {
	os.Exit(exit(run(), os.Stderr))
}

// exit is the exit status of a run that ended with err, whose error it prints on stderr: an
// installer's it ran passed on (that installer printed its own), 3 on a refusal, 1 on a failure.
func exit(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var code install.ExitCode
	if errors.As(err, &code) {
		return int(code) // the installer it ran printed its error
	}
	fmt.Fprintln(stderr, "error:", err)
	if _, ok := install.Refusal(err); ok {
		return install.ExitRefused
	}
	return 1
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
