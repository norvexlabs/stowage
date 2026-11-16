package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/truenas"
)

// truenasCommand is `vm truenas`: what OpenTofu's TrueNAS provider (truenas/truenas) cannot do
// about a VM, each step run by a root's local-exec provisioner and reading its arguments and the
// API's address, key and certificate check from the environment. Each is safe to run again: a
// create-time provisioner that failed taints its resource, which runs it again.
func truenasCommand(getenv func(string) string) *cobra.Command {
	t := &cobra.Command{
		Use: "truenas",
		Short: fmt.Sprintf("The TrueNAS steps an OpenTofu root runs where its provider has none: write a disk image into a zvol, upload a seed ISO, stop a VM before its devices go (%s or %s, %s; %s or %s)",
			truenas.EndpointVar, truenas.URLVar, truenas.KeyVar, truenas.CACertVar, truenas.InsecureVar),
		Args: cobra.NoArgs,
	}
	vmID := func() (int64, error) {
		id, err := strconv.ParseInt(getenv("VM_ID"), 10, 64)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("VM_ID %q: the VM's id on TrueNAS, a number", getenv("VM_ID"))
		}
		return id, nil
	}
	// step checks its environment (keys, then check) before it dials the host.
	step := func(use, short string, keys []string, check func() error, timeout time.Duration, f func(ctx context.Context, c *truenas.Client, log io.Writer) error) *cobra.Command {
		var wait time.Duration
		c := &cobra.Command{
			Use: use, Short: short, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				for _, k := range keys {
					if getenv(k) == "" {
						return fmt.Errorf("%s is not set", k)
					}
				}
				if check != nil {
					if err := check(); err != nil {
						return err
					}
				}
				if wait <= 0 {
					return fmt.Errorf("--timeout %s: a duration past 0", wait)
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), wait)
				defer cancel()
				api, err := truenas.FromEnv(ctx, getenv)
				if err != nil {
					return err
				}
				defer api.Close()
				if err := f(ctx, api, cmd.ErrOrStderr()); err != nil {
					if ctx.Err() == context.DeadlineExceeded {
						return timedOut(err, wait)
					}
					return err
				}
				return nil
			},
		}
		c.Flags().DurationVar(&wait, "timeout", timeout, "how long the step may take, the API's answers and its jobs included")
		return c
	}
	t.AddCommand(
		step("import-image", "Write the disk image IMAGE, checked against IMAGE_SHA256, into the zvol ZVOL through STAGING_DIR (a dataset's mount point): a zvol already holding it is left as it is; one holding other data, or a VM's disk, is refused",
			[]string{"IMAGE", "IMAGE_SHA256", "STAGING_DIR", "ZVOL"}, nil, time.Hour, func(ctx context.Context, c *truenas.Client, log io.Writer) error {
				held, err := c.ImportImage(ctx, getenv("IMAGE"), getenv("IMAGE_SHA256"), getenv("STAGING_DIR"), getenv("ZVOL"), log)
				if err == nil && held {
					fmt.Fprintf(log, "the zvol %s holds the image sha256:%s already: left as it is\n", getenv("ZVOL"), getenv("IMAGE_SHA256"))
				}
				return err
			}),
		step("seed", "Upload the seed ISO SEED to SEED_PATH on the host, replacing the file there", []string{"SEED", "SEED_PATH"}, nil, 10*time.Minute,
			func(ctx context.Context, c *truenas.Client, _ io.Writer) error {
				return c.Upload(ctx, getenv("SEED"), getenv("SEED_PATH"))
			}),
		step("stop", "Power the VM VM_ID off in any state but STOPPED, its devices then deletable: a running VM by a clean shutdown forced after its timeout, a suspended one at once; one stopped or gone already is left", []string{"VM_ID"}, func() error { _, err := vmID(); return err }, 10*time.Minute,
			func(ctx context.Context, c *truenas.Client, _ io.Writer) error {
				id, err := vmID()
				if err != nil {
					return err
				}
				return c.Stop(ctx, id)
			}),
	)
	return t
}

// timedOut is err, a step's that --timeout wait cut off: a run again starts the step over — nothing
// resumes, but import-image leaves a zvol marked holding its image as it is — so it is told to run
// again with a longer --timeout (after the fix a lock left by a failed release names, which its
// message holds); unless a convert may still run (a *truenas.ConvertLeftError), which leaves the
// zvol's lock until it ends, or a job FAILED on its own (a *truenas.FailedError), which more time
// does not fix: those are told the timeout ran out, and no other fix than their message's.
func timedOut(err error, wait time.Duration) error {
	var left *truenas.ConvertLeftError
	var failed *truenas.FailedError
	if errors.As(err, &left) || errors.As(err, &failed) {
		return fmt.Errorf("%w (--timeout %s ran out)", err, wait)
	}
	return fmt.Errorf("%w (--timeout %s ran out: the step starts over: run it again with a longer --timeout)", err, wait)
}
