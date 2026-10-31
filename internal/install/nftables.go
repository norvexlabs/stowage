package install

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/spf13/cobra"
)

// TableLabel is the label of a service with an nftables table: its table's last load's token, so
// a start that loads the table (changed, or absent: a boot) recreates the service, which then
// feeds the table's sets again.
const TableLabel = "io.stowage.nftables"

// TablesFile names, in the install's directory, the services' nftables tables its last start
// loaded, one per line; TablesDir holds what each was loaded from.
const (
	TablesFile = ".stowage-nftables"
	TablesDir  = "nftables"
)

// loadTables loads each service's nftables table (services.<name>.nftables) into the VM's
// firewall, or a consented Docker host's (gatewayApplies), before the services start (syncTables): at a start that finds it absent (a boot) or its file
// changed, replaced whole in one transaction (the table made if absent, deleted, then its file's,
// so a rule never doubles nor goes missing in between; nft checks it before it commits). A table
// that holds already leaves its sets as the service fed them. Each load empties its sets, so it
// writes a token of its own (<name>.load), which is its service's label (TableLabel): the start
// that loaded it recreates the service, which feeds them again. A table is recorded (TablesFile)
// as soon as it is in the firewall, and a table recorded whose service the release no longer
// declares is deleted, so none outlives its release, a load that fails halfway included. The
// host's own table stays Stowage's: a file holds its service's table alone, never of the
// project's name (lint, the release's decoding).
func (in *Installer) loadTables(ctx context.Context) error {
	if !in.gatewayApplies() {
		return nil // gatewayStart says why
	}
	return in.syncTables(ctx, in.R.Nftables)
}

// syncTables makes the tables loaded those of want, by service: each loaded as loadTables says, a
// table recorded and not wanted deleted.
func (in *Installer) syncTables(ctx context.Context, want map[string][]byte) error {
	host := in.O.host()
	record := map[string]bool{}
	if prev, err := os.ReadFile(in.path(TablesFile)); err == nil {
		for _, name := range strings.Fields(string(prev)) {
			if spec.NftTableName.MatchString(name) && name != in.R.Spec.Name {
				record[name] = true
			}
		}
	}
	save := func() error {
		return writeAtomic(in.path(TablesFile), strings.NewReader(strings.Join(slices.Sorted(maps.Keys(record)), "\n")+"\n"), 0o644, nil)
	}
	for _, name := range sortedKeys(want) {
		var b bytes.Buffer
		fmt.Fprintf(&b, "# %s's nftables table, as services.%s.nftables gives it, loaded whole.\ntable inet %s\ndelete table inet %s\n", name, name, name, name)
		b.Write(want[name])
		if !bytes.HasSuffix(want[name], []byte("\n")) {
			b.WriteByte('\n') // nft takes a comment on the last line only before a newline
		}
		f := filepath.Join(in.O.Dir, TablesDir, name+".nft")
		if cur, err := os.ReadFile(f); err == nil && bytes.Equal(cur, b.Bytes()) {
			if _, err := host.Run(ctx, Cmd{Args: []string{"nft", "list", "table", "inet", name}}); err == nil {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			return err
		}
		// Loaded from a file of its own, which becomes its file once loaded: a load that fails
		// leaves it as it was, so the next start tries again.
		next := f + ".next"
		if err := writeAtomic(next, &b, 0o644, nil); err != nil {
			return err
		}
		// The token first: a start cut off after the load recreates the service all the same (a
		// token without its file loaded again only makes the next start load it again).
		token := make([]byte, 8)
		if _, err := rand.Read(token); err != nil {
			return err
		}
		if err := writeAtomic(strings.TrimSuffix(f, ".nft")+".load", strings.NewReader(hex.EncodeToString(token)+"\n"), 0o644, nil); err != nil {
			return err
		}
		if _, err := host.Run(ctx, Cmd{Args: []string{"nft", "-f", next}}); err != nil {
			_ = os.Remove(next)
			return fmt.Errorf("services.%s.nftables: nft refuses its table: %w", name, err)
		}
		record[name] = true
		if err := save(); err != nil {
			return err
		}
		if err := os.Rename(next, f); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(record)) {
		if _, ok := want[name]; ok {
			continue
		}
		// One a boot already dropped (the kernel's ruleset is not kept) is deleted.
		if _, err := host.Run(ctx, Cmd{Args: []string{"nft", "list", "table", "inet", name}}); err == nil {
			if _, err := host.Run(ctx, Cmd{Args: []string{"nft", "delete", "table", "inet", name}}); err != nil {
				in.O.logf("warning: the nftables table inet %s of a service the release no longer declares stays: %v", name, err)
				continue
			}
		}
		delete(record, name)
		for _, ext := range []string{".nft", ".load"} {
			_ = os.Remove(filepath.Join(in.O.Dir, TablesDir, name+ext))
		}
	}
	if len(record) == 0 {
		if err := os.Remove(in.path(TablesFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return save()
}

// tableLoads are the services' tables' load tokens where they are loaded (TableLabel): a service whose table
// a start loads is recreated by it.
func (in *Installer) tableLoads() map[string]string {
	// Consent alone, not who runs the start: a start not as root keeps the services' labels.
	if in.O.Firewall == "" && !in.gatewayConsented() {
		return nil
	}
	out := map[string]string{}
	for name := range in.R.Nftables {
		if raw, err := os.ReadFile(filepath.Join(in.O.Dir, TablesDir, name+".load")); err == nil {
			out[name] = "load " + strings.TrimSpace(string(raw))
		}
	}
	return out
}

// tablesBootCmd loads the services' tables at boot, before Docker restores their containers.
const tablesBootCmd = "tables-boot"

// bootTables loads again, at boot before Docker starts and restores the stack's containers, each
// table the install's last start loaded (TablesFile, from its file under TablesDir), so no service
// runs a moment without its table (the kernel keeps no ruleset over a reboot). Each load writes a
// load token of its own first, as syncTables does: the boot's start, which finds the table loaded,
// recreates its service, which feeds its sets again. A Docker host's without the consent loads
// nothing. A table that does not load is the boot's start's to load again: the others load still,
// and the error names it.
func (in *Installer) bootTables(ctx context.Context) error {
	if in.O.Firewall == "" && !in.gatewayConsented() {
		return nil
	}
	raw, err := os.ReadFile(in.path(TablesFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range strings.Fields(string(raw)) {
		f := filepath.Join(in.O.Dir, TablesDir, name+".nft")
		if !spec.NftTableName.MatchString(name) || name == in.R.Spec.Name || !fileExists(f) {
			continue
		}
		token := make([]byte, 8)
		if _, err := rand.Read(token); err != nil {
			return err
		}
		if err := writeAtomic(strings.TrimSuffix(f, ".nft")+".load", strings.NewReader(hex.EncodeToString(token)+"\n"), 0o644, nil); err != nil {
			return err
		}
		if _, err := in.O.host().Run(ctx, Cmd{Args: []string{"nft", "-f", f}}); err != nil {
			errs = append(errs, fmt.Errorf("services.%s.nftables: its table does not load at boot (the install's start loads it again): %w", name, err))
			continue
		}
		in.O.logf("loaded the nftables table inet %s, before the containers start", name)
	}
	return errors.Join(errs...)
}

// tablesBootCommand is the hidden verb a boot unit runs as root before Docker starts: the
// services' tables the last start loaded (bootTables) into the install of dir's.
func tablesBootCommand(prepare func(cmd *cobra.Command) (*Installer, error)) *cobra.Command {
	return &cobra.Command{
		Use:    tablesBootCmd,
		Short:  "Load the services' nftables tables before Docker restores their containers (run at boot)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in, err := prepare(cmd)
			if err != nil {
				return err
			}
			return in.bootTables(cmd.Context())
		},
	}
}

// nftSet is a named set a table declares, and nftElement one element of a set as nft reads it: an
// address, a prefix, a port or a range, or a concatenation of them (" . " apart).
var (
	nftSet     = regexp.MustCompile(`(?m)^\s*set\s+([A-Za-z][A-Za-z0-9_]*)\s*\{`)
	nftElement = regexp.MustCompile(`^[0-9A-Za-z_:./-]{1,64}( \. [0-9A-Za-z_:./-]{1,64}){0,3}$`)
)

// maxElements is the most elements one request adds or deletes.
const maxElements = 1024

// SetElements is a request of the host daemon's sets route: the elements a set gains and loses.
type SetElements struct {
	Add    []string `json:"add,omitempty"`
	Delete []string `json:"delete,omitempty"`
}

// setElements adds and deletes elements of a named set of a service's table: the set one its
// release's table declares, each element an address, a prefix, a port or a range (or their
// concatenation), so a service feeds its own table's sets and reaches no other rule. The app
// socket serves it: the services the release gives the socket feed their sets.
func (h *Hostd) setElements(w http.ResponseWriter, r *http.Request) {
	table, set := r.PathValue("table"), r.PathValue("set")
	body, ok := h.R.Nftables[table]
	if !ok {
		replyErr(w, fail(http.StatusNotFound, "no service %q has an nftables table (services.<name>.nftables)", table))
		return
	}
	declared := false
	for _, m := range nftSet.FindAllStringSubmatch(string(body), -1) {
		declared = declared || m[1] == set
	}
	if !declared {
		replyErr(w, fail(http.StatusNotFound, "the table inet %s declares no set %q", table, set))
		return
	}
	var req SetElements
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	if len(req.Add)+len(req.Delete) == 0 || len(req.Add)+len(req.Delete) > maxElements {
		replyErr(w, fail(http.StatusBadRequest, "add or delete 1 to %d elements", maxElements))
		return
	}
	for _, e := range append(slices.Clone(req.Add), req.Delete...) {
		if !nftElement.MatchString(e) {
			replyErr(w, fail(http.StatusBadRequest, "element %q: an address, a prefix, a port or a range, or their concatenation (a . b)", e))
			return
		}
	}
	in, _ := h.run()
	for _, op := range []struct {
		verb  string
		elems []string
	}{{"add", req.Add}, {"delete", req.Delete}} {
		if len(op.elems) == 0 {
			continue
		}
		if _, err := in.O.Host.Run(r.Context(), Cmd{Args: []string{"nft", op.verb, "element", "inet", table, set, "{ " + strings.Join(op.elems, ", ") + " }"}}); err != nil {
			replyErr(w, fail(http.StatusBadRequest, "nft %s element inet %s %s: %v", op.verb, table, set, err))
			return
		}
	}
	reply(w, http.StatusOK, map[string]int{"added": len(req.Add), "deleted": len(req.Delete)})
}
