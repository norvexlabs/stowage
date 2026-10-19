// Command stowage is the publisher CLI: it lints a project's stowage.yaml, computes its build
// graph and publishes releases to the project's registry.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/norvexlabs/stowage/internal/cli"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	// An interrupt cancels registry calls and builds in flight. Ship builds and pushes by digest
	// before it moves any tag, and moves the channel pointer only after the release verified.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cli.New(version, os.Stdout, os.Stderr).ExecuteContext(ctx)
	stop()
	if err == nil {
		return
	}
	var exit cli.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.Code)
	}
	fmt.Fprintln(os.Stderr, "stowage:", err)
	os.Exit(1)
}
