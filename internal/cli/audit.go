package cli

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/sbom"
)

func auditCmd(o *options) *cobra.Command {
	var channel, failOn, db string
	var revision int
	c := &cobra.Command{
		Use:   "audit [<version>]",
		Short: "Check a release's SBOMs are attested by a trusted key and scan them for known vulnerabilities",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			min, on, err := sbom.FailOn(failOn)
			if err != nil {
				return err
			}
			_, pub, err := o.openPublisher(&channel, readAccess)
			if err != nil {
				return err
			}
			v := ""
			if len(args) == 1 {
				v = args[0]
			}
			cache, err := os.UserCacheDir()
			if err != nil {
				return err
			}
			s := sbom.Scanner{Docker: o.docker(), Image: cmp.Or(o.scannerImage, sbom.Grype), Cache: filepath.Join(cache, "stowage", "grype"),
				User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), DB: db}
			rep, err := pub.Audit(cmd.Context(), channel, v, revision, s)
			if err != nil {
				return err
			}
			if o.json {
				err = rep.JSON(o.out)
			} else {
				err = rep.Print(o.out)
			}
			if err != nil {
				return err
			}
			return rep.Verdict(min, on)
		},
	}
	c.Flags().StringVar(&channel, "channel", "", "channel (default: the one that moves latest, else the first)")
	c.Flags().IntVar(&revision, "revision", 1, "package revision (with <version>)")
	c.Flags().StringVar(&failOn, "fail-on", "none", "exit non-zero on a vulnerability at or above this severity: none, negligible, low, medium, high, critical")
	c.Flags().StringVar(&db, "db", "", "a grype database archive to scan with, for a host that does not reach grype's servers")
	c.Flags().BoolVar(&o.json, "json", false, "machine-readable output")
	return c
}

func (o *options) docker() sbom.Docker {
	if o.dockerRun != nil {
		return o.dockerRun
	}
	return sbom.DockerCLI
}

func sbomCmd(o *options) *cobra.Command {
	root := &cobra.Command{Use: "sbom", Short: "A release's SBOMs (SPDX), attested by the release key"}
	var channel string
	var revision int
	export := &cobra.Command{
		Use:   "export [<version>] DIR",
		Short: "Write each artifact's SBOM, verified, into DIR, with index.json naming them",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, pub, err := o.openPublisher(&channel, readAccess)
			if err != nil {
				return err
			}
			v, dir := "", args[len(args)-1]
			if len(args) == 2 {
				v = args[0]
			}
			idx, err := pub.ExportSBOMs(cmd.Context(), channel, v, revision, dir)
			for _, a := range idx.Artifacts {
				if a.File != "" {
					fmt.Fprintf(o.out, "%-12s %s  %s\n", a.Name, a.Digest, filepath.Join(dir, a.File))
				}
			}
			return err
		},
	}
	export.Flags().StringVar(&channel, "channel", "", "channel (default: the one that moves latest, else the first)")
	export.Flags().IntVar(&revision, "revision", 1, "package revision (with <version>)")
	root.AddCommand(export)
	return root
}
