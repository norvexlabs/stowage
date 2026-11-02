package install

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
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
// that holds already leaves its sets as the service fed them, but those its inputs fill (below).
// Each load empties its sets, so it
// writes a token of its own (<name>.load), which is its service's label (TableLabel): the start
// that loaded it recreates the service, which feeds them again. A table is recorded (TablesFile)
// as soon as it is in the firewall, and a table recorded whose service the release no longer
// declares is deleted, so none outlives its release, a load that fails halfway included. The
// host's own table stays Stowage's: a file holds its service's table alone, never of the
// project's name (lint, the release's decoding). The sets inputs fill (services.<name>.sets, get
// reading the install's values) are written in the table's own transaction (setScripts), so a set
// of its rules is never empty a moment, at a load as at a start that finds the table held (when
// they changed since the last start's script); a set no longer filled loads the table again,
// which empties it of its last input's elements.
func (in *Installer) loadTables(ctx context.Context, get func(string) string) error {
	if !in.gatewayApplies() {
		return nil // gatewayStart says why
	}
	in.warnSocketFed()
	want, err := in.R.TablesNow(get)
	if err != nil {
		rerun := fmt.Sprintf("%s --dir %s", in.ctl(), in.O.Dir)
		if in.O.Preloaded {
			rerun = "sudo " + in.R.Ctl() + " install"
		}
		return fmt.Errorf("%w: a re-run of the install, `%s --set <input>=<value>`, picks one", err, rerun)
	}
	sets, err := in.setScripts(get)
	if err != nil {
		return err
	}
	return in.syncTables(ctx, want, sets)
}

// SetsSuffix names, beside a table's file under TablesDir, the nft script that filled its sets
// from their inputs at the last start, which the boot loads with the table.
const SetsSuffix = ".sets"

// setScripts are, by table, the nft script giving each set an input fills its elements: the set
// emptied, then the input's elements (spec.SetElements), every set of the table in one script. An
// input that holds no element nft reads is an error naming it and its key, before any table loads.
func (in *Installer) setScripts(get func(string) string) (map[string]string, error) {
	out := map[string]string{}
	var errs []error
	for _, name := range sortedKeys(in.R.Spec.Services) {
		sets := in.R.Spec.Services[name].Sets
		if !in.R.hasTable(name) || len(sets) == 0 {
			continue
		}
		var b strings.Builder
		for _, set := range sortedKeys(sets) {
			elems, err := spec.SetElements(get(sets[set]))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s (services.%s.sets.%s): %w", sets[set], name, set, err))
				continue
			}
			fmt.Fprintf(&b, "flush set inet %s %s\n", name, set)
			if len(elems) > 0 {
				fmt.Fprintf(&b, "add element inet %s %s { %s }\n", name, set, strings.Join(elems, ", "))
			}
		}
		out[name] = b.String()
	}
	return out, errors.Join(errs...)
}

// setsDropped reports whether the script next leaves as it is a set the script prev (setScripts'
// form, as the last start kept it) filled: a set prev flushes and next does not, or a prev that
// names no set though it holds text (not of setScripts' form: what it filled is unknown). Only a
// load of the table empties such a set of prev's elements.
func setsDropped(prev, next string) bool {
	was, now := setsFlushed(prev), setsFlushed(next)
	if strings.TrimSpace(prev) != "" && len(was) == 0 {
		return true
	}
	for set := range was {
		if !now[set] {
			return true
		}
	}
	return false
}

// setsFlushed is the sets ("table set") a script of setScripts' form flushes: every set it fills.
func setsFlushed(script string) map[string]bool {
	out := map[string]bool{}
	for line := range strings.Lines(script) {
		if f := strings.Fields(line); len(f) == 5 && f[0] == "flush" && f[1] == "set" && f[2] == "inet" {
			out[f[3]+" "+f[4]] = true
		}
	}
	return out
}

// setsKeys names a table's input-filled sets for an error: services.<name>.sets (their inputs).
func (in *Installer) setsKeys(name string) string {
	sets := in.R.Spec.Services[name].Sets
	var keys []string
	for _, set := range sortedKeys(sets) {
		keys = append(keys, sets[set])
	}
	if len(keys) == 0 {
		return fmt.Sprintf("services.%s.sets", name)
	}
	return fmt.Sprintf("services.%s.sets (%s)", name, strings.Join(keys, ", "))
}

// syncTables makes the tables loaded those of want, by service, each with its sets' script (sets):
// each loaded as loadTables says, the script in the load's transaction or, for a table that holds,
// in one of its own; a table recorded and not wanted deleted.
func (in *Installer) syncTables(ctx context.Context, want map[string][]byte, sets map[string]string) error {
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
	var errs []error // a held table's sets refused: the other tables are made all the same
	// The tables loaded anew (absent, or of other rules) whose service flushes the
	// connection-tracking table then: each marked before its load, so a start cut off before the
	// flush, or whose flush failed, flushes at the next one. It runs however this sync ends, a
	// later table's refusal included.
	var flush []string
	defer func() {
		if len(flush) == 0 {
			return
		}
		keys := make([]string, len(flush))
		for i, name := range flush {
			keys[i] = "services." + name + ".flush_conntrack"
		}
		if err := in.O.flushConntrack(); err != nil {
			in.O.logf("warning: %s: the host's connection-tracking table does not flush: %v: the connections open before keep the previous table's verdict until they end, or until `sudo conntrack -F` (the next start tries again)", strings.Join(keys, ", "), err)
			return
		}
		in.O.logf("flushed the host's connection-tracking table (%s): the connections open before meet the tables and sets now loaded as new ones", strings.Join(keys, ", "))
		for _, name := range flush {
			_ = os.Remove(in.tableFile(name, ".flush"))
		}
	}()
	for _, name := range sortedKeys(want) {
		mark, flushes := in.tableFile(name, ".flush"), in.R.Spec.Services[name].FlushConntrack
		switch {
		case flushes && fileExists(mark):
			flush = append(flush, name)
		case !flushes:
			_ = os.Remove(mark)
		}
		var b bytes.Buffer
		fmt.Fprintf(&b, "# %s's nftables table, as services.%s.nftables gives it, loaded whole.\ntable inet %s\ndelete table inet %s\n", name, name, name, name)
		b.Write(want[name])
		if !bytes.HasSuffix(want[name], []byte("\n")) {
			b.WriteByte('\n') // nft takes a comment on the last line only before a newline
		}
		f, setsFile := in.tableFile(name, ".nft"), in.tableFile(name, SetsSuffix)
		// Loaded from a file of its own, which becomes its file once loaded: a load that fails
		// leaves it as it was, so the next start tries again.
		next := f + ".next"
		// A table held — its rules in the firewall as this start loads them — writes its sets alone,
		// or nothing; else it loads (again). A load's file left (.next) is a start cut off after nft
		// may have loaded it, before f recorded it: what the firewall holds is unknown, and the table
		// loads again. Under flush_conntrack every load flushes, and so does a held table's change
		// of its sets: an element added to a set the rules deny by ends the flows it denies now, one
		// removed from a set they admit by the flows it admitted.
		earlier, earlierErr := os.ReadFile(f)
		if earlierErr == nil && bytes.Equal(earlier, b.Bytes()) && !fileExists(next) {
			if _, err := host.Run(ctx, Cmd{Args: []string{"nft", "list", "table", "inet", name}}); err == nil {
				kept, err := os.ReadFile(setsFile)
				switch script := sets[name]; {
				case err != nil && !errors.Is(err, os.ErrNotExist):
					// What the sets hold is unknown: the table loads again, with them.
				case setsDropped(string(kept), script):
					// A set no longer filled keeps its last input's elements: the table loads again,
					// which gives it back the table's own and empties the sets the app socket fed.
				case script == string(kept):
					continue // as the last start wrote them
				default:
					// Marked before the sets change, as a load is: a start cut off before the
					// flush flushes at the next one. A mark this start wrote goes when nft refuses
					// the elements, which then change nothing.
					marked := false
					if flushes && !fileExists(mark) {
						if err := writeAtomic(mark, strings.NewReader(in.O.bootID()), 0o644, nil); err != nil {
							return errors.Join(append(errs, err)...)
						}
						marked = true
					}
					if _, err := host.Run(ctx, Cmd{Args: []string{"nft", "-f", "-"}, Stdin: strings.NewReader(script)}); err != nil {
						if marked {
							_ = os.Remove(mark)
						}
						errs = append(errs, fmt.Errorf("%s: nft refuses their elements: %w", in.setsKeys(name), err))
						continue
					}
					if flushes && !slices.Contains(flush, name) {
						flush = append(flush, name)
					}
					if err := in.keepSets(setsFile, script); err != nil {
						// A record of the sets before would match a later script that wants them
						// back, which would then not be written: none is left.
						_ = os.Remove(setsFile)
						return errors.Join(append(errs, err)...)
					}
					continue
				}
			}
		}
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			return errors.Join(append(errs, err)...)
		}
		table := b.String()
		b.WriteString(sets[name]) // the sets filled in the load's transaction
		if err := writeAtomic(next, &b, 0o644, nil); err != nil {
			return errors.Join(append(errs, err)...)
		}
		// The token first: a start cut off after the load recreates the service all the same (a
		// token without its file loaded again only makes the next start load it again).
		token := make([]byte, 8)
		if _, err := rand.Read(token); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := writeAtomic(in.tableFile(name, ".load"), strings.NewReader(hex.EncodeToString(token)+"\n"), 0o644, nil); err != nil {
			return errors.Join(append(errs, err)...)
		}
		// Marked before the load; a mark this start wrote goes when nft refuses the load, which
		// then changes nothing.
		marked := false
		if flushes && !fileExists(mark) {
			if err := writeAtomic(mark, strings.NewReader(in.O.bootID()), 0o644, nil); err != nil {
				return errors.Join(append(errs, err)...)
			}
			marked = true
		}
		if _, err := host.Run(ctx, Cmd{Args: []string{"nft", "-f", next}}); err != nil {
			_ = os.Remove(next)
			if marked {
				_ = os.Remove(mark)
			}
			if sets[name] != "" {
				return errors.Join(append(errs, fmt.Errorf("services.%s.nftables: nft refuses its table, or %s their elements: %w", name, in.setsKeys(name), err))...)
			}
			return errors.Join(append(errs, fmt.Errorf("services.%s.nftables: nft refuses its table: %w", name, err))...)
		}
		record[name] = true
		switch {
		case flushes && !slices.Contains(flush, name):
			flush = append(flush, name)
		case !flushes && earlierErr == nil && !bytes.Equal(earlier, []byte(table)):
			in.O.logf("note: services.%s.nftables: its table changed, and the connections open before keep their conntrack entries: a rule for new connections, or a nat rule, leaves them be until they end (services.%s.flush_conntrack: true flushes the host's connection-tracking table at each such load)", name, name)
		}
		if err := save(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := in.keepSets(setsFile, sets[name]); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := writeAtomic(f, strings.NewReader(table), 0o644, nil); err != nil {
			return errors.Join(append(errs, err)...)
		}
		_ = os.Remove(next)
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
		for _, ext := range []string{".nft", ".load", ".flush", SetsSuffix} {
			_ = os.Remove(in.tableFile(name, ext))
		}
	}
	if len(record) == 0 {
		if err := os.Remove(in.path(TablesFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.Join(append(errs, err)...)
		}
		return errors.Join(errs...)
	}
	return errors.Join(append(errs, save())...)
}

// tableFile is the file, under the install's TablesDir, of the table name with the extension
// ext: .nft the table as last loaded, .load its load token, SetsSuffix its sets' script, .flush the
// mark of a load whose flush of the connection-tracking table is still due (flush_conntrack),
// holding the boot id it was written in.
func (in *Installer) tableFile(name, ext string) string {
	return filepath.Join(in.O.Dir, TablesDir, name+ext)
}

// tableLoads are the services' tables' load tokens where they are loaded (TableLabel): a service whose table
// a start loads is recreated by it.
func (in *Installer) tableLoads() map[string]string {
	// Consent alone, not who runs the start: a start not as root keeps the services' labels.
	if in.O.Firewall == "" && !in.gatewayConsented() {
		return nil
	}
	loads := map[string]string{}
	for _, name := range in.R.Tables() {
		if raw, err := os.ReadFile(filepath.Join(in.O.Dir, TablesDir, name+".load")); err == nil {
			loads[name] = "load " + strings.TrimSpace(string(raw))
		}
	}
	// A service carries its own table's token, and a feeder (feeds) its own and those of the
	// tables it feeds, so a load of any of them recreates it as it does the table's own service.
	out := maps.Clone(loads)
	for _, name := range sortedKeys(in.R.Spec.Services) {
		feeds := in.R.Spec.Services[name].Feeds
		if len(feeds) == 0 {
			continue
		}
		var tokens []string
		for _, t := range append([]string{name}, feeds...) {
			if d, ok := loads[t]; ok && !slices.Contains(tokens, t+" "+d) {
				tokens = append(tokens, t+" "+d)
			}
		}
		if len(tokens) > 0 {
			slices.Sort(tokens)
			out[name] = strings.Join(tokens, "; ")
		}
	}
	return out
}

// socketFed reports whether the table of the service name is fed through the VM's app socket: the
// service, or one feeding its table (feeds), is given the socket (shapes.kairos.host_api).
func (in *Installer) socketFed(name string) bool {
	k := in.R.Spec.Shapes.Kairos
	if k == nil {
		return false
	}
	for _, svc := range sortedKeys(in.R.Spec.Services) {
		if (svc == name || slices.Contains(in.R.Spec.Services[svc].Feeds, name)) && slices.Contains(k.HostAPI, svc) {
			return true
		}
	}
	return false
}

// setInput reports whether the input key fills a service's set (services.<name>.sets).
func (in *Installer) setInput(key string) bool {
	for _, svc := range in.R.Spec.Services {
		for _, k := range svc.Sets {
			if k == key {
				return true
			}
		}
	}
	return false
}

// tablePick is the check of an input picking services' tables (services.<name>.nftables.input):
// its value one each such table has a file for; nil for another input.
func (in *Installer) tablePick(key string) func(string) error {
	var check func(string) error
	for _, name := range sortedKeys(in.R.Spec.Services) {
		if t := in.R.Spec.Services[name].Nftables; t.Input == key {
			check = chainChecks(check, func(v string) error {
				if t.Files[v] == "" {
					return pickError(name, t, v)
				}
				return nil
			})
		}
	}
	return check
}

// chainChecks is a check passing what a and b both pass (a nil one passes everything).
func chainChecks(a, b func(string) error) func(string) error {
	if a == nil {
		return b
	}
	return func(v string) error {
		if err := a(v); err != nil {
			return err
		}
		return b(v)
	}
}

// checkSetElements accepts an input's value a set is filled with (spec.SetElements).
func checkSetElements(v string) error {
	_, err := spec.SetElements(v)
	return err
}

// keepSets records script as the sets' script the boot loads with the table at f's side (file),
// or removes it when the table has none.
func (in *Installer) keepSets(file, script string) error {
	if script == "" {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeAtomic(file, strings.NewReader(script), 0o644, nil)
}

// warnSocketFed warns on a Docker host, once a release, of each set of a table fed through the
// VM's app socket (socketFed) that no input fills: nothing but the table's own rules or elements
// feeds it there.
func (in *Installer) warnSocketFed() {
	if in.O.Firewall != "" || in.setsWarned == in.R {
		return
	}
	in.setsWarned = in.R // a start prepares again after a pull: warned once
	for _, name := range in.R.Tables() {
		if !in.socketFed(name) {
			continue
		}
		for _, set := range in.R.tableSets(name) {
			if _, filled := in.R.Spec.Services[name].Sets[set]; !filled {
				in.O.logf("warning: the set %s of the table inet %s is fed through the VM's app socket, which a Docker host does not serve: it stays as the table declares it here unless its own rules fill it (services.%s.sets fills a set from an input)", set, name, name)
			}
		}
	}
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
		f := in.tableFile(name, ".nft")
		if !spec.NftTableName.MatchString(name) || name == in.R.Spec.Name || !fileExists(f) {
			continue
		}
		// A reboot empties the connection-tracking table: a flush still due from an earlier boot (the
		// mark holds the boot it was written in) is moot. One of this boot stays (the unit run again
		// by hand empties nothing), and so does one whose boot is not known.
		if mark := in.tableFile(name, ".flush"); fileExists(mark) {
			if b, _ := os.ReadFile(mark); otherBoot(strings.TrimSpace(string(b)), in.O.bootID()) {
				_ = os.Remove(mark)
			}
		}
		token := make([]byte, 8)
		if _, err := rand.Read(token); err != nil {
			return err
		}
		if err := writeAtomic(in.tableFile(name, ".load"), strings.NewReader(hex.EncodeToString(token)+"\n"), 0o644, nil); err != nil {
			return err
		}
		failed := func(err error) {
			errs = append(errs, fmt.Errorf("services.%s.nftables: its table does not load at boot (the install's start loads it again): %w", name, err))
		}
		// With the sets its last start filled from their inputs, in the load's transaction: a set
		// of its rules is never empty a moment. Sets nft refuses now leave the table loaded alone,
		// said, and their script removed, so the boot's start, which finds the table held, writes
		// them again.
		setsFile := in.tableFile(name, SetsSuffix)
		script, readErr := os.ReadFile(setsFile)
		unread := readErr != nil && !errors.Is(readErr, os.ErrNotExist)
		if unread {
			errs = append(errs, fmt.Errorf("%s: the script that fills them at boot: %w", in.setsKeys(name), readErr))
		}
		var setsErr error
		if len(script) > 0 {
			table, tableErr := os.ReadFile(f)
			if tableErr != nil {
				failed(tableErr)
				continue
			}
			_, setsErr = in.O.host().Run(ctx, Cmd{Args: []string{"nft", "-f", "-"}, Stdin: io.MultiReader(bytes.NewReader(table), bytes.NewReader(script))})
			if setsErr == nil {
				in.O.logf("loaded the nftables table inet %s and the sets its inputs fill, before the containers start", name)
				continue
			}
		}
		// A script refused or unread goes, so the boot's start writes the sets again.
		if setsErr != nil || unread {
			if err := os.Remove(setsFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("%s: the script nft refused stays, so the start may not fill them again (remove %s): %w", in.setsKeys(name), setsFile, err))
			}
		}
		if _, err := in.O.host().Run(ctx, Cmd{Args: []string{"nft", "-f", f}}); err != nil {
			failed(err)
			continue
		}
		if setsErr != nil {
			errs = append(errs, fmt.Errorf("%s: nft refuses their elements at boot, so the table loads without them (the install's start fills them again): %w", in.setsKeys(name), setsErr))
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

// SetElements is a request of the host daemon's sets route: the elements a set gains and loses.
type SetElements struct {
	Add    []string `json:"add,omitempty"`
	Delete []string `json:"delete,omitempty"`
}

// setElements adds and deletes elements of a named set of a service's table: the set one its
// release's table declares and no input fills (services.<name>.sets, whose input is its whole
// content), each element an address, a prefix, a port or a range (or their concatenation), so a
// caller feeds sets and reaches no other rule. The app socket serves it: any service the release
// gives the socket feeds any table's sets (its own, or those it feeds).
func (h *Hostd) setElements(w http.ResponseWriter, r *http.Request) {
	table, set := r.PathValue("table"), r.PathValue("set")
	if !h.R.hasTable(table) {
		replyErr(w, fail(http.StatusNotFound, "no service %q has an nftables table (services.<name>.nftables)", table))
		return
	}
	if !slices.Contains(h.R.tableSets(table), set) {
		replyErr(w, fail(http.StatusNotFound, "the table inet %s declares no set %q", table, set))
		return
	}
	if k, filled := h.R.Spec.Services[table].Sets[set]; filled {
		replyErr(w, fail(http.StatusConflict, "the set %s of the table inet %s is filled from the input %s at each start (services.%s.sets): the input is its whole content", set, table, k, table))
		return
	}
	var req SetElements
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	if len(req.Add)+len(req.Delete) == 0 || len(req.Add)+len(req.Delete) > spec.MaxSetElements {
		replyErr(w, fail(http.StatusBadRequest, "add or delete 1 to %d elements", spec.MaxSetElements))
		return
	}
	for _, e := range append(slices.Clone(req.Add), req.Delete...) {
		if !spec.LiteralElement(e) {
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
	// No flush of the connection-tracking table here, flush_conntrack or not: the app's calls
	// come often, and a flush ends every flow of the host's.
	reply(w, http.StatusOK, map[string]int{"added": len(req.Add), "deleted": len(req.Delete)})
}
