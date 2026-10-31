package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/spec"
)

// A Docker host's firewall and routing are its operator's: the stack's network declarations (the
// services' nftables tables, the forwarding between host.interfaces roles, the networkd file of
// each tun device a service opens) are applied there only once the operator consented, with `tune
// --apply`, which records it (GatewayFile). From then on each start as root applies them as the
// VM's does, and a boot unit runs the install's start at every boot (the kernel keeps no table or
// chain across one); `tune --revert` removes them all. What stays the operator's: a NIC's MTU and
// policy routing (its own NIC, configured by its own files: `tune` gives networkd's settings) and
// a transparent role's input accept (an accept in a table of the install's cannot override a drop
// in the host's own input chain).

// GatewayFile records, in a Docker install's directory, the operator's consent (`tune --apply`) to
// the stack's network declarations, and the networkd files they wrote.
const GatewayFile = ".stowage-gateway"

// gatewayRecord is GatewayFile's content.
type gatewayRecord struct {
	// Digest names the install's chain, unit and files (dirDigest of the directory --apply took),
	// so they are found again from the record wherever the directory is now.
	Digest string `json:"digest"`
	// Dir is the install directory the record is of (absolute). At another one (the directory
	// moved or copied) it applies nothing: its consent is that directory's.
	Dir string `json:"dir,omitempty"`
	// Networkd are the networkd files the install wrote, by path.
	Networkd map[string]networkdFile `json:"networkd,omitempty"`
	// Revoked: a revert withdrew the consent, but did not remove all it applied: no start applies
	// anything, and a revert again removes the rest.
	Revoked bool `json:"revoked,omitempty"`
}

// networkdFile is a tun role's networkd file the install wrote.
type networkdFile struct {
	Role string `json:"role"`
	// Pending: recorded before it was written, not yet seen loaded by networkd.
	Pending bool `json:"pending,omitempty"`
}

// gatewayDeclared reports whether the release declares what a Docker host applies on consent: a
// service's nftables table, forwarding between roles, or a tun device.
func (in *Installer) gatewayDeclared() bool {
	return len(in.R.Nftables) > 0 || len(in.gatewayRoles()) > 0 || forwards(in.R.Spec.Host)
}

// gatewayRoles are the tun roles, whose whole networkd file a Docker host is given.
func (in *Installer) gatewayRoles() []string {
	h := in.R.Spec.Host
	if h == nil {
		return nil
	}
	var roles []string
	for _, role := range sortedKeys(h.Interfaces) {
		if h.Interfaces[role].Kind == spec.InterfaceTUN {
			roles = append(roles, role)
		}
	}
	return roles
}

// nicSettings are the NIC roles that set an MTU or policy routing: the operator's on a Docker host.
func (in *Installer) nicSettings() []string {
	h := in.R.Spec.Host
	if h == nil {
		return nil
	}
	var roles []string
	for _, role := range sortedKeys(h.Interfaces) {
		i := h.Interfaces[role]
		if i.Kind != spec.InterfaceTUN && (i.MTU != 0 || len(i.RoutingPolicyRules)+len(i.Routes) > 0) {
			roles = append(roles, role)
		}
	}
	return roles
}

// gatewayConsented reports whether the operator of this Docker host consented (GatewayFile) and
// did not revoke it since. A record that does not read is no consent: nothing is applied from it
// (a start and status say so; a revert refuses until it is fixed or removed).
func (in *Installer) gatewayConsented() bool {
	if !in.gatewayRecorded() {
		return false
	}
	rec, err := in.readGateway()
	return err == nil && !rec.Revoked && in.gatewayHere(rec)
}

// gatewayHere reports whether rec is this directory's: recorded for it, by file identity (a link
// to it, or another spelling of it, is it). A record naming no directory is no directory's.
func (in *Installer) gatewayHere(rec gatewayRecord) bool {
	if rec.Dir == "" {
		return false
	}
	a, err := os.Stat(rec.Dir)
	if err != nil {
		return false
	}
	b, err := os.Stat(in.gatewayDir())
	return err == nil && os.SameFile(a, b)
}

// gatewayOtherDir reports whether this install keeps a record that is another directory's (or
// names none): nothing was applied from it here.
func (in *Installer) gatewayOtherDir() (gatewayRecord, bool) {
	if !in.gatewayRecorded() {
		return gatewayRecord{}, false
	}
	rec, err := in.readGateway()
	return rec, err == nil && !in.gatewayHere(rec)
}

// gatewayMoved reports whether rec is of a directory that is gone: this install moved from it,
// the one left to revert what it applied.
func gatewayMoved(rec gatewayRecord) bool {
	if rec.Dir == "" {
		return false
	}
	_, err := os.Stat(rec.Dir)
	return errors.Is(err, os.ErrNotExist)
}

// gatewayRevertsHere reports whether a revert here removes what rec applied: rec is this
// directory's, or of one gone (this install moved).
func (in *Installer) gatewayRevertsHere(rec gatewayRecord) bool {
	return in.gatewayHere(rec) || gatewayMoved(rec)
}

// gatewayElsewhere is what a record of another directory says: applied nothing here.
func (in *Installer) gatewayElsewhere(rec gatewayRecord) string {
	ctl := in.R.Ctl()
	switch {
	case rec.Dir == "":
		return fmt.Sprintf("this install's %s names no directory: nothing of the stack's network declarations is applied from it; remove it, then `%s tune --apply` here", GatewayFile, ctl)
	case gatewayMoved(rec):
		return fmt.Sprintf("this install's %s is %s's, which is gone (the directory moved): nothing of the stack's network declarations is applied here until `%s tune --revert` here removes what it applied, then `%s tune --apply` here", GatewayFile, rec.Dir, ctl, ctl)
	}
	return fmt.Sprintf("this install's %s is %s's (the directory moved or copied): nothing of the stack's network declarations is applied here; `%s tune --revert --dir %s` removes what it applied, from there, or remove the file and `%s tune --apply` here", GatewayFile, rec.Dir, in.R.Ctl(), rec.Dir, in.R.Ctl())
}

// gatewayUnitName is the boot unit of the install whose names digest gives.
func (in *Installer) gatewayUnitName(digest string) string {
	return in.R.Spec.Name + "-gateway-" + digest + ".service"
}

// gatewayTablesUnitName is the tables' boot unit of the install whose names digest gives.
func (in *Installer) gatewayTablesUnitName(digest string) string {
	return in.R.Spec.Name + "-gateway-" + digest + "-tables.service"
}

// gatewayRecorded reports whether this Docker install keeps a gateway record: consented, or revoked
// with something left to remove (a revert takes it from there).
func (in *Installer) gatewayRecorded() bool {
	return in.O.Firewall == "" && fileExists(in.path(GatewayFile))
}

// gatewayDir is the install directory's real path: absolute, its links resolved, so the record
// and the boot unit name it as it is, whatever spelling of it a run was given.
func (in *Installer) gatewayDir() string {
	dir, err := filepath.Abs(in.O.Dir)
	if err != nil {
		return in.O.Dir
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

// digestOf names the install's chain, unit and networkd files: rec's digest, else the directory's.
func (in *Installer) digestOf(rec gatewayRecord) string {
	return cmp.Or(rec.Digest, dirDigest(in.gatewayDir()))
}

// gatewayDigest names the install's chain, unit and networkd files: the record's, else the
// directory's (dirDigest).
func (in *Installer) gatewayDigest() string {
	rec, _ := in.readGateway()
	return in.digestOf(rec)
}

// gatewayApplies reports whether this run applies the network declarations: always on the VM; on
// a Docker host once consented, as root.
func (in *Installer) gatewayApplies() bool {
	return in.O.Firewall != "" || in.gatewayConsented() && in.O.euid() == 0
}

// forwardChain is the iptables chain DOCKER-USER jumps to for the install's forwarding: the VM's
// one name (it holds one install), a Docker host's of the install directory's digest.
func (in *Installer) forwardChain() string {
	if in.O.Firewall != "" {
		return ForwardChain
	}
	return "STOWAGE-FWD-" + in.gatewayDigest()
}

// forwardRecord is where forwardRoles records the rules it gave the chain: under /run, which a
// boot empties as it does iptables's rules.
func (in *Installer) forwardRecord() string {
	if in.O.Firewall != "" {
		return filepath.Join(in.O.Firewall, in.R.Spec.Name+"-forward.rules")
	}
	return filepath.Join(in.O.gatewayRun(), in.R.Spec.Name+"-gateway-"+in.gatewayDigest()+"-forward.rules")
}

// gatewayStart applies, at a Docker host's start, what is not the VM's own firewall's: the
// forwarding's chain and the tun roles' networkd files (the tables are loadTables', at every start
// that applies them). Not consented, the release's declarations are said not to be in force;
// consented but not run as root, they are said not to be applied (or, the release declaring none,
// not removed). A NIC role's MTU and policy routing are said to be the operator's.
func (in *Installer) gatewayStart(ctx context.Context, get func(string) string) error {
	if in.O.Firewall != "" {
		return nil
	}
	ctl := in.R.Ctl()
	if roles := in.nicSettings(); len(roles) > 0 {
		in.O.logf("warning: the MTU and policy routing of %s are this host's NICs', its operator's: `%s tune` gives networkd's settings for them", strings.Join(roles, ", "), ctl)
	}
	declared, consented := in.gatewayDeclared(), in.gatewayConsented()
	rec, err := in.readGateway()
	switch {
	case in.gatewayRecorded() && err != nil:
		in.O.logf("warning: %v: nothing of the stack's network declarations is applied (fix it, or remove it and `%s tune --apply` again)", err, ctl)
		return nil
	case in.gatewayRecorded() && !in.gatewayHere(rec):
		in.O.logf("warning: %s", in.gatewayElsewhere(rec))
		return nil
	case rec.Revoked:
		in.O.logf("warning: the consent to the stack's network declarations is withdrawn, but not all it applied is removed: `%s tune --revert` again removes the rest", ctl)
		return nil
	case !declared && !consented:
		return nil
	case !consented:
		in.O.logf("warning: %s are not in force on this host, whose firewall and routing are its operator's: `%s tune` lists them, `%s tune --apply` (as root) applies them, and each start then again", in.gatewayWhat(), ctl, ctl)
		return nil
	case in.O.euid() != 0 && declared:
		in.O.logf("warning: %s are applied by a start as root alone (`%s tune --apply` consented): this start applies nothing of them", in.gatewayWhat(), ctl)
		return nil
	case in.O.euid() != 0:
		if len(rec.Networkd) > 0 || fileExists(in.forwardRecord()) || fileExists(in.path(TablesFile)) {
			in.O.logf("warning: what an earlier release applied of the stack's network declarations is removed by a start as root alone: this start removes nothing")
		}
		return nil
	}
	// The boot units as this release writes them: a consent an earlier release recorded gets the
	// tables' unit too.
	if _, err := in.writeUnits(ctx, false); err != nil {
		in.O.logf("warning: the boot units of %s are not written (%v): `%s tune --apply` writes them again", in.gatewayWhat(), err, ctl)
	}
	if err := in.forwardRoles(ctx, get); err != nil {
		return err
	}
	return in.gatewayRouting(ctx)
}

// gatewayWhat names the release's network declarations, for a warning.
func (in *Installer) gatewayWhat() string {
	var what []string
	if len(in.R.Nftables) > 0 {
		what = append(what, "the services' nftables tables ("+strings.Join(sortedKeys(in.R.Nftables), ", ")+")")
	}
	if forwards(in.R.Spec.Host) {
		what = append(what, "the forwarding between host.interfaces")
	}
	if roles := in.gatewayRoles(); len(roles) > 0 {
		what = append(what, "the networkd files of "+strings.Join(roles, ", "))
	}
	if len(what) == 0 {
		return "the stack's network declarations"
	}
	return "the stack's network declarations, " + strings.Join(what, ", ")
}

// readGateway reads GatewayFile; an empty record when there is none. A record naming another path
// than the install's own networkd files (ownRecord) is refused: root removes what it lists.
func (in *Installer) readGateway() (gatewayRecord, error) {
	var r gatewayRecord
	raw, err := os.ReadFile(in.path(GatewayFile))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return gatewayRecord{}, fmt.Errorf("%s: %w", in.path(GatewayFile), err)
	}
	if r.Digest != "" && !hexDigest.MatchString(r.Digest) {
		return gatewayRecord{}, fmt.Errorf("%s: digest %q is not 8 hex digits", in.path(GatewayFile), r.Digest)
	}
	for _, p := range slices.Sorted(maps.Keys(r.Networkd)) {
		role := r.Networkd[p].Role
		if r.Digest == "" || !spec.InterfaceName.MatchString(role) || p != filepath.Join(in.O.networkdDir(), tunFile(in.R.Spec.Name, r.Digest, role)) {
			return gatewayRecord{}, fmt.Errorf("%s records %s, which is not a networkd file of the install's: refused", in.path(GatewayFile), p)
		}
	}
	return r, nil
}

func (in *Installer) writeGateway(r gatewayRecord) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	// Readable by all, as nothing in it is secret: a start not as root reads the consent too.
	return writeAtomic(in.path(GatewayFile), bytes.NewReader(append(raw, '\n')), 0o644, nil)
}

// gatewayTunFile is the name of a tun role's networkd file, holding the install's digest, so two
// installs never share one.
func (in *Installer) gatewayTunFile(role string) string {
	return tunFile(in.R.Spec.Name, in.gatewayDigest(), role)
}

func tunFile(name, digest, role string) string {
	return "10-" + name + "-" + digest + "-" + role + ".network"
}

// hexDigest is a dirDigest.
var hexDigest = regexp.MustCompile(`^[0-9a-f]{8}$`)

// gatewayRouting writes each tun role's whole networkd file (kairos.InterfaceNetwork, the VM's
// own), which matches the device by name once its service opens it, and removes a recorded one
// whose role the release no longer declares. Every file is in the record, pending, before it is
// written; networkd reloads (which applies a changed file to its link), and the record then holds
// the files wanted, none pending: a start cut off in between is applied again by the next. A host
// where networkd does not run is warned and changed in nothing.
func (in *Installer) gatewayRouting(ctx context.Context) error {
	rec, err := in.readGateway()
	if err != nil {
		in.O.logf("warning: %v: the networkd files are left as they are (fix or remove it)", err)
		return nil
	}
	roles := in.gatewayRoles()
	digest := in.digestOf(rec)
	want, content := map[string]networkdFile{}, map[string]string{}
	for _, role := range roles {
		p := filepath.Join(in.O.networkdDir(), tunFile(in.R.Spec.Name, digest, role))
		want[p], content[p] = networkdFile{Role: role}, kairos.InterfaceNetwork(in.R.Spec.Name, role, in.R.Spec.Host.Interfaces[role])
	}
	var changed []string
	for _, p := range slices.Sorted(maps.Keys(content)) {
		if cur, err := os.ReadFile(p); err != nil || string(cur) != content[p] {
			changed = append(changed, p)
		}
	}
	// A pending entry differs from the one wanted: it is applied again.
	if len(changed) == 0 && maps.Equal(want, rec.Networkd) {
		return nil
	}
	host := in.O.host()
	if !networkdRuns(ctx, host) {
		if len(roles) > 0 {
			in.O.logf("warning: systemd-networkd does not run on this host: the networkd files of %s are not written or reloaded (`%s tune` gives their settings)", strings.Join(roles, ", "), in.R.Ctl())
		} else {
			in.O.logf("warning: systemd-networkd does not run on this host: the networkd files an earlier release wrote (%s) stay until a start as root finds it running", strings.Join(slices.Sorted(maps.Keys(rec.Networkd)), ", "))
		}
		return nil
	}
	held := maps.Clone(rec.Networkd)
	if held == nil {
		held = map[string]networkdFile{}
	}
	for _, p := range changed {
		held[p] = networkdFile{Role: want[p].Role, Pending: true}
	}
	if err := in.writeGateway(gatewayRecord{Digest: digest, Dir: rec.Dir, Networkd: held}); err != nil {
		return err
	}
	for _, p := range slices.Sorted(maps.Keys(held)) {
		if _, ok := want[p]; !ok {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	for _, p := range changed {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(p, strings.NewReader(content[p]), 0o644, nil); err != nil {
			return err
		}
	}
	if _, err := host.Run(ctx, Cmd{Args: []string{"networkctl", "reload"}}); err != nil {
		return fmt.Errorf("networkctl reload: %w", err)
	}
	return in.writeGateway(gatewayRecord{Digest: digest, Dir: rec.Dir, Networkd: want})
}

// networkdRuns reports whether systemd-networkd runs on the host.
func networkdRuns(ctx context.Context, host Tool) bool {
	_, err := host.Run(ctx, Cmd{Args: []string{"systemctl", "is-active", "--quiet", "systemd-networkd"}})
	return err == nil
}

// gatewayUnit is the boot unit of a consented Docker install: the install's start at every boot,
// after Docker and the network, which loads its tables and forwarding again and recreates the
// services whose table it loaded (they feed its sets again).
func (in *Installer) gatewayUnit() (name, content string) {
	dir := in.gatewayDir()
	name = in.gatewayUnitName(in.gatewayDigest())
	ctl := filepath.Join(dir, in.R.Ctl())
	return name, fmt.Sprintf("[Unit]\nDescription=%s (%s): the stack's network declarations, applied by its start (tune --apply)\nAfter=docker.service network-online.target\nWants=docker.service network-online.target\n\n[Service]\nType=oneshot\nExecStart=%s --dir %s --no-input --no-provision\nTimeoutStartSec=30min\n\n[Install]\nWantedBy=multi-user.target\n",
		in.R.Spec.Name, systemdText(dir), systemdArg(ctl), systemdArg(dir))
}

// gatewayTablesUnit is the boot unit of a consented Docker install that loads the services' tables
// the last start loaded (tables-boot) before Docker restores their containers, once the network
// is up (a table may name a device by its index).
func (in *Installer) gatewayTablesUnit() (name, content string) {
	dir := in.gatewayDir()
	name = in.gatewayTablesUnitName(in.gatewayDigest())
	ctl := filepath.Join(dir, in.R.Ctl())
	return name, fmt.Sprintf("[Unit]\nDescription=%s (%s): the services' nftables tables, before Docker restores their containers (tune --apply)\nAfter=network-online.target\nWants=network-online.target\nBefore=docker.service\n\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=%s %s --dir %s\n\n[Install]\nWantedBy=multi-user.target\n",
		in.R.Spec.Name, systemdText(dir), systemdArg(ctl), tablesBootCmd, systemdArg(dir))
}

// systemdArg is s as one argument of a unit's ExecStart: double-quoted, its backslashes and quotes
// escaped, its % (systemd's specifiers) and $ (its variables, expanded in quotes too) doubled, so
// a path with a space, a % or a $ stays the one argument it is.
func systemdArg(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s)
	// A control character (a newline would end the line and start a directive) as a C escape,
	// which systemd reads in a quoted argument.
	// So is a byte outside UTF-8, kept as the byte it is.
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r < 0x20 || r == 0x7f || r == utf8.RuneError && n == 1 {
			fmt.Fprintf(&b, `\x%02x`, s[i])
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return `"` + b.String() + `"`
}

// systemdText is s as a unit's free text (Description=): its % doubled, a control character a ?
// (no escape is read there, and a newline would end the line).
func systemdText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, strings.ReplaceAll(s, "%", "%%"))
}

// gatewayApply records the operator's consent, enables the boot unit, and runs the install's
// start, which applies the declarations (tune --apply on a Docker host).
func (in *Installer) gatewayApply(ctx context.Context) error {
	// Under the install's lock: a start running meanwhile would write the record it read. The
	// start this runs takes the lock again, waiting for one that took it meanwhile.
	unlock, err := lockFile(in.path(lifecycleLock))
	if errors.Is(err, errLocked) {
		return errors.New("a start, upgrade, rollback, provision or reset-db of this install runs: wait for it to finish, then tune --apply again")
	}
	if err != nil {
		return err
	}
	err = in.consent(ctx)
	unlock()
	if err != nil {
		return err
	}
	in.O.logf("applying %s: the install's start", in.gatewayWhat())
	return in.O.exec(ctx, in.ctl(), []string{"--dir", in.gatewayDir(), "--no-input", "--no-provision"})
}

// consent enables the boot unit, then records the consent. A unit that fails leaves the record
// and the unit as they were (a unit this run wrote is removed again). A record of another
// directory is refused: its consent is that directory's.
func (in *Installer) consent(ctx context.Context) error {
	prev, err := in.readGateway()
	if err != nil {
		return fmt.Errorf("%w: fix or remove it, then tune --apply again", err)
	}
	if in.gatewayRecorded() && !in.gatewayHere(prev) {
		return errors.New(in.gatewayElsewhere(prev))
	}
	recorded := in.gatewayRecorded()
	rec := prev
	rec.Digest = in.digestOf(prev)
	rec.Revoked, rec.Dir = false, cmp.Or(prev.Dir, in.gatewayDir())
	undo, err := in.writeUnits(ctx, true)
	if err != nil {
		return err
	}
	if err := in.writeGateway(rec); err != nil {
		if recorded {
			return err // the record as it was, the units enabled for it: a revert reaches both
		}
		undo()
		return err
	}
	return nil
}

// writeUnits writes and enables the consented install's boot units (gatewayUnit,
// gatewayTablesUnit), each written only when its content differs, and enabled when one was written
// or enable says so. One that fails leaves every unit as it was (a unit this run wrote is removed
// again); undo puts them back so after a success.
func (in *Installer) writeUnits(ctx context.Context, enable bool) (undo func(), err error) {
	host := in.O.host()
	type was struct {
		name, path string
		old        []byte
		had        bool
	}
	var done []was
	undo = func() {
		for _, w := range done {
			if w.had {
				_ = writeAtomic(w.path, bytes.NewReader(w.old), 0o644, nil)
			} else {
				_, _ = host.Run(ctx, Cmd{Args: []string{"systemctl", "disable", w.name}})
				_ = os.Remove(w.path)
			}
		}
		_, _ = host.Run(ctx, Cmd{Args: []string{"systemctl", "daemon-reload"}})
	}
	fail := func(err error) (func(), error) {
		undo()
		return nil, err
	}
	var names []string
	reload := false
	for _, unit := range []func() (string, string){in.gatewayUnit, in.gatewayTablesUnit} {
		name, content := unit()
		path := filepath.Join(in.O.systemd(), name)
		old, readErr := os.ReadFile(path)
		w := was{name: name, path: path, old: old, had: readErr == nil}
		done = append(done, w)
		names = append(names, name)
		if w.had && string(old) == content {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fail(err)
		}
		if err := writeAtomic(path, strings.NewReader(content), 0o644, nil); err != nil {
			return fail(err)
		}
		reload = true
	}
	if !reload && !enable {
		return undo, nil
	}
	if reload {
		if _, err := host.Run(ctx, Cmd{Args: []string{"systemctl", "daemon-reload"}}); err != nil {
			return fail(err)
		}
	}
	if _, err := host.Run(ctx, Cmd{Args: append([]string{"systemctl", "enable"}, names...)}); err != nil {
		return fail(err)
	}
	return undo, nil
}

// lockedRevert is gatewayRevert under the install's lock (tune --revert).
func (in *Installer) lockedRevert(ctx context.Context) error {
	unlock, err := lockFile(in.path(lifecycleLock))
	if errors.Is(err, errLocked) {
		return errors.New("a start, upgrade, rollback, provision or reset-db of this install runs: wait for it to finish, then tune --revert again")
	}
	if err != nil {
		return err
	}
	defer unlock()
	return in.gatewayRevert(ctx)
}

// gatewayRevert removes what the declarations applied (the boot unit, the services' tables, the
// forwarding's chain, the networkd files) and the consent (tune --revert, uninstall on a Docker
// host; the caller holds the install's lock). The record is read first: one that does not read, or
// names another file, refuses before anything goes. Each step is tried; when one failed, the record
// stays, revoked (no start applies anything), and a revert again removes the rest. A service's
// table goes with the consent: the service runs on without it.
func (in *Installer) gatewayRevert(ctx context.Context) error {
	rec, err := in.readGateway()
	if err != nil {
		return fmt.Errorf("%w: fix or remove it, then `%s tune --revert` again", err, in.R.Ctl())
	}
	if !in.gatewayRevertsHere(rec) {
		return errors.New(in.gatewayElsewhere(rec))
	}
	host := in.O.host()
	var errs []error
	reload := false
	for _, name := range []string{in.gatewayUnitName(in.digestOf(rec)), in.gatewayTablesUnitName(in.digestOf(rec))} {
		if unit := filepath.Join(in.O.systemd(), name); fileExists(unit) {
			_, _ = host.Run(ctx, Cmd{Args: []string{"systemctl", "disable", name}})
			if err := os.Remove(unit); err != nil {
				errs = append(errs, err)
			} else {
				reload = true
			}
		}
	}
	if reload {
		if _, err := host.Run(ctx, Cmd{Args: []string{"systemctl", "daemon-reload"}}); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, in.syncTables(ctx, nil), in.dropForward(ctx))
	if raw, err := os.ReadFile(in.path(TablesFile)); err == nil && len(strings.Fields(string(raw))) > 0 {
		errs = append(errs, fmt.Errorf("the nftables tables %s are still loaded", strings.Join(strings.Fields(string(raw)), ", ")))
	}
	for _, p := range slices.Sorted(maps.Keys(rec.Networkd)) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	// networkd not running reads the files at its start: they are gone.
	if len(rec.Networkd) > 0 && networkdRuns(ctx, host) {
		if _, err := host.Run(ctx, Cmd{Args: []string{"networkctl", "reload"}}); err != nil {
			errs = append(errs, fmt.Errorf("networkctl reload: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		rec.Revoked = true
		if werr := in.writeGateway(rec); werr != nil {
			return errors.Join(err, werr)
		}
		return fmt.Errorf("the consent is withdrawn, but not all it applied is removed (`%s tune --revert` again removes the rest): %w", in.R.Ctl(), err)
	}
	if err := os.Remove(in.path(GatewayFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in.O.logf("removed %s, and the consent to them", in.gatewayWhat())
	return nil
}

// dropForward removes the install's forwarding chain on a Docker host: DOCKER-USER's jump to it,
// its rules, the chain, and its record.
func (in *Installer) dropForward(ctx context.Context) error {
	chain, host := in.forwardChain(), in.O.host()
	tables := []string{"iptables", "ip6tables"}
	if _, err := os.Stat(ipv6Proc); err != nil {
		tables = tables[:1]
	}
	for _, t := range tables {
		jump := []string{"DOCKER-USER", "-j", chain}
		for range 16 { // each jump inserted once a start; a bound on what deleting takes
			if _, err := host.Run(ctx, Cmd{Args: append([]string{t, "-w", "-C"}, jump...)}); err != nil {
				break
			}
			if _, err := host.Run(ctx, Cmd{Args: append([]string{t, "-w", "-D"}, jump...)}); err != nil {
				return fmt.Errorf("%s: DOCKER-USER's jump to %s: %w", t, chain, err)
			}
		}
		if _, err := host.Run(ctx, Cmd{Args: []string{t, "-w", "-n", "-L", chain}}); err != nil {
			continue // no such chain
		}
		for _, op := range []string{"-F", "-X"} {
			if _, err := host.Run(ctx, Cmd{Args: []string{t, "-w", op, chain}}); err != nil {
				return fmt.Errorf("%s %s %s: %w", t, op, chain, err)
			}
		}
	}
	if err := os.Remove(in.forwardRecord()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// gatewayStatus describes, for `status` on a Docker host, whether the declarations are applied:
// empty when the release declares none and no consent stands.
func (in *Installer) gatewayStatus(get func(string) string) string {
	recorded := in.gatewayRecorded()
	if in.O.Firewall != "" || !in.gatewayDeclared() && !recorded {
		return ""
	}
	ctl := in.R.Ctl()
	rec, err := in.readGateway()
	switch {
	case !recorded:
		return fmt.Sprintf("not in force: the host's firewall and routing are its operator's (`%s tune --apply` applies %s)", ctl, strings.TrimPrefix(in.gatewayWhat(), "the stack's network declarations, "))
	case err != nil:
		return fmt.Sprintf("not applied: %v (fix it, or remove it and `%s tune --apply` again)", err, ctl)
	case !in.gatewayHere(rec):
		return "not applied: " + in.gatewayElsewhere(rec)
	case rec.Revoked:
		return fmt.Sprintf("revoked, but not all it applied is removed: `%s tune --revert` again removes the rest", ctl)
	}
	var applied []string
	if loaded := in.loadedTables(context.Background()); len(loaded) > 0 {
		applied = append(applied, "tables "+strings.Join(loaded, ", "))
	}
	if fileExists(in.forwardRecord()) {
		applied = append(applied, "forwarding")
	}
	if _, unmapped := forwardRules(in.R.Spec.Host, ForwardChain, func(r string) string { return in.roleDevice(r, get) }); len(unmapped) > 0 {
		applied = append(applied, "no forwarding through "+strings.Join(unmapped, ", ")+" (no NIC named: --interface)")
	}
	n, pending := 0, 0
	for _, f := range rec.Networkd {
		if f.Pending {
			pending++
		} else {
			n++
		}
	}
	if n > 0 {
		applied = append(applied, fmt.Sprintf("%d networkd file(s)", n))
	}
	if pending > 0 {
		applied = append(applied, fmt.Sprintf("%d networkd file(s) pending (written, not yet seen loaded by networkd: the next start as root reloads it)", pending))
	}
	switch {
	case len(applied) == 0 && !in.gatewayDeclared():
		return "consented (tune --apply): the release declares none (`" + ctl + " tune --revert` withdraws the consent)"
	case len(applied) == 0:
		return "consented (tune --apply): nothing applied yet, the next start as root applies them"
	}
	return "applied at each start (tune --apply): " + strings.Join(applied, "; ")
}

// loadedTables are the services' tables the install recorded loaded (TablesFile); as root, those
// the kernel holds alone (a boot drops them until the next start loads them).
func (in *Installer) loadedTables(ctx context.Context) []string {
	raw, err := os.ReadFile(in.path(TablesFile))
	if err != nil {
		return nil
	}
	names := strings.Fields(string(raw))
	if in.O.euid() != 0 {
		return names
	}
	return slices.DeleteFunc(names, func(n string) bool {
		_, err := in.O.host().Run(ctx, Cmd{Args: []string{"nft", "list", "table", "inet", n}})
		return err != nil
	})
}
