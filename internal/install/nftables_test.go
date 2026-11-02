package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/tune"
)

const ingestTable = "table inet ingest {\n\tset probes {\n\t\ttype ipv4_addr\n\t}\n}\n"

// Every start on the VM loads each service's table that is absent or changed, whole, and gives
// it a fresh load token (its service's label); one that holds is left; a table is recorded as soon
// as it is loaded, so one of a release that failed halfway is deleted once a release drops it; a
// host whose firewall is its operator's loads none.
func TestEveryStartLoadsTheServicesTables(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &nftHost{}
	r := testRelease(t, "1.0.0")
	r.Nftables = map[string][]byte{"ingest": []byte(ingestTable)}
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: t.TempDir(), Host: host, Log: io.Discard}}
	if err := in.loadTables(ctx, none); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(in.O.Dir, TablesDir, "ingest.nft")
	if !slices.EqualFunc(host.ran, [][]string{{"nft", "-f", f + ".next"}}, slices.Equal) {
		t.Errorf("ran %v", host.ran)
	}
	if b, _ := os.ReadFile(f); !strings.Contains(string(b), "\ntable inet ingest\ndelete table inet ingest\n"+ingestTable) {
		t.Errorf("loaded:\n%s", b)
	}
	if b, _ := os.ReadFile(in.path(TablesFile)); string(b) != "ingest\n" {
		t.Errorf("record %q", b)
	}
	first := in.tableLoads()["ingest"]
	if !strings.HasPrefix(first, "load ") || len(first) != len("load ")+16 {
		t.Errorf("load token %q", first)
	}
	// The same table, still loaded: left as it is, its sets as the service fed them, its token too.
	host.ran = nil
	if err := in.loadTables(ctx, none); err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(host.ran, [][]string{{"nft", "list", "table", "inet", "ingest"}}, slices.Equal) || in.tableLoads()["ingest"] != first {
		t.Errorf("an unchanged table: ran %v", host.ran)
	}
	// Absent (a boot, or a table deleted by hand): loaded again, a new token recreating its service.
	host.ran, host.listErr = nil, errors.New("no such table")
	if err := in.loadTables(ctx, none); err != nil || len(host.ran) != 2 || in.tableLoads()["ingest"] == first {
		t.Errorf("an absent table: %v, ran %v", err, host.ran)
	}
	host.listErr = nil
	// A changed one is loaded again.
	host.ran = nil
	in.R.Nftables = map[string][]byte{"ingest": []byte(strings.Replace(ingestTable, "ipv4_addr", "ipv6_addr", 1))}
	if err := in.loadTables(ctx, none); err != nil || len(host.ran) != 1 || host.ran[0][1] != "-f" {
		t.Errorf("a changed table: %v, ran %v", err, host.ran)
	}
	host.ran = nil
	in.R.Nftables = nil
	if err := in.loadTables(ctx, none); err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(host.ran, [][]string{{"nft", "list", "table", "inet", "ingest"}, {"nft", "delete", "table", "inet", "ingest"}}, slices.Equal) {
		t.Errorf("ran %v", host.ran)
	}
	for _, gone := range []string{f, strings.TrimSuffix(f, ".nft") + ".load", in.path(TablesFile)} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stays: %v", gone, err)
		}
	}
	// Two tables, the second refused: the first is recorded, and deleted once a release drops it.
	host.ran, host.failOn = nil, "b.nft.next"
	in.R.Nftables = map[string][]byte{"a": []byte(ingestTable), "b": []byte(ingestTable)}
	if err := in.loadTables(ctx, none); err == nil || !strings.Contains(err.Error(), "services.b.nftables: nft refuses its table") {
		t.Errorf("a refused table: %v, ran %v", err, host.ran)
	}
	if b, _ := os.ReadFile(in.path(TablesFile)); string(b) != "a\n" {
		t.Errorf("record after a failure halfway %q", b)
	}
	if _, err := os.Stat(filepath.Join(in.O.Dir, TablesDir, "b.nft")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused table is kept as loaded: %v", err)
	}
	host.ran, host.failOn = nil, ""
	in.R.Nftables = nil
	// After a boot the kernel holds no table a: it leaves the record without a delete.
	host.listErr = errors.New("no such table")
	if err := in.loadTables(ctx, none); err != nil || !slices.EqualFunc(host.ran, [][]string{{"nft", "list", "table", "inet", "a"}}, slices.Equal) {
		t.Errorf("rolled back: %v, ran %v", err, host.ran)
	}
	if _, err := os.Stat(in.path(TablesFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the record stays: %v", err)
	}
	host.listErr = nil
	r.Nftables = map[string][]byte{"ingest": []byte(ingestTable)}
	var log bytes.Buffer
	docker := &Installer{R: r, O: Options{Dir: t.TempDir(), Host: host, Log: &log}}
	host.ran = nil
	if err := docker.loadTables(ctx, none); err != nil || len(host.ran) != 0 || docker.gatewayStart(ctx, none) != nil || !strings.Contains(log.String(), "the services' nftables tables (ingest) are not in force on this host") || docker.tableLoads() != nil {
		t.Errorf("Docker host: %v, ran %v, log %q", err, host.ran, log.String())
	}
}

// stdinHost records each command with what it read on its stdin.
// refuse and alsoRefuse fail a command whose stdin (or file loaded) holds them.
type stdinHost struct {
	ran                []string
	refuse, alsoRefuse string
}

func (h *stdinHost) Run(_ context.Context, c Cmd) ([]byte, error) {
	line := strings.Join(c.Args, " ")
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		line += "\n" + string(b)
	} else if len(c.Args) == 3 && c.Args[1] == "-f" {
		b, _ := os.ReadFile(c.Args[2])
		line += "\n" + string(b)
	}
	h.ran = append(h.ran, line)
	if h.refuse != "" && strings.Contains(line, h.refuse) || h.alsoRefuse != "" && strings.Contains(line, h.alsoRefuse) {
		return nil, errors.New("Error: interval overlaps with an existing one")
	}
	return nil, nil
}

// A service feeding another's table (feeds) carries that table's load token beside its own, so
// the start that loads it, emptying its sets, recreates the feeder too; a service feeding none
// carries its own token alone.
func TestAFeederCarriesTheTokensOfTheTablesItFeeds(t *testing.T) {
	t.Parallel()
	r := testRelease(t, "1.0.0")
	r.Nftables = map[string][]byte{"proxy": []byte(ingestTable), "vpn": []byte(ingestTable)}
	r.Spec.Services = map[string]spec.Service{"proxy": {}, "vpn": {}, "ctl": {Feeds: []string{"proxy"}}, "both": {Feeds: []string{"vpn", "proxy"}}}
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: t.TempDir(), Host: &nftHost{}}}
	if err := in.loadTables(context.Background(), none); err != nil {
		t.Fatal(err)
	}
	l := in.tableLoads()
	if !strings.HasPrefix(l["proxy"], "load ") || l["ctl"] != "proxy "+l["proxy"] {
		t.Errorf("proxy %q, ctl %q", l["proxy"], l["ctl"])
	}
	if l["both"] != "proxy "+l["proxy"]+"; vpn "+l["vpn"] {
		t.Errorf("both %q", l["both"])
	}
	before := l["ctl"]
	if err := os.WriteFile(filepath.Join(in.O.Dir, TablesDir, "proxy.load"), []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	if after := in.tableLoads(); after["ctl"] == before || after["vpn"] != l["vpn"] {
		t.Errorf("after a load of proxy: %v, before %v", after, l)
	}
}

// Each start writes a set named in services.<name>.sets its input's elements, the set emptied
// first: in the table's load's transaction when it loads, in one of its own when the table holds
// (never an empty set between two commands), the script kept for the boot; a blank input leaves it
// empty; an element nft does not read fails the start before any table loads, naming the input and
// the key; a set nft refuses names the sets' key; a host whose firewall is its operator's is
// given none.
func TestEveryStartFillsTheSetsFromTheirInputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &stdinHost{}
	r := testRelease(t, "1.0.0")
	r.Nftables = map[string][]byte{"ingest": []byte(ingestTable)}
	r.Spec.Services = map[string]spec.Service{"ingest": {Sets: map[string]string{"probes": "PROBES", "peers": "PEERS"}}}
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: t.TempDir(), Host: host, Log: io.Discard}}
	vals := map[string]string{"PROBES": "192.0.2.7, 198.51.100.0/24 ,", "PEERS": " "}
	get := func(k string) string { return vals[k] }
	script := "flush set inet ingest peers\nflush set inet ingest probes\nadd element inet ingest probes { 192.0.2.7, 198.51.100.0/24 }\n"
	f := filepath.Join(in.O.Dir, TablesDir, "ingest.nft")
	if err := in.loadTables(ctx, get); err != nil {
		t.Fatal(err)
	}
	if len(host.ran) != 1 || !strings.HasPrefix(host.ran[0], "nft -f "+f+".next\n") || !strings.HasSuffix(host.ran[0], ingestTable+script) {
		t.Errorf("load: ran %q", host.ran)
	}
	if b, _ := os.ReadFile(strings.TrimSuffix(f, ".nft") + SetsSuffix); string(b) != script {
		t.Errorf("kept %q", b)
	}
	if b, _ := os.ReadFile(f); strings.Contains(string(b), "add element") {
		t.Errorf("the table's file holds the sets:\n%s", b)
	}
	// The table holds: the sets alone, in one transaction.
	host.ran, vals["PEERS"] = nil, "192.0.2.1"
	if err := in.loadTables(ctx, get); err != nil {
		t.Fatal(err)
	}
	held := "flush set inet ingest peers\nadd element inet ingest peers { 192.0.2.1 }\nflush set inet ingest probes\nadd element inet ingest probes { 192.0.2.7, 198.51.100.0/24 }\n"
	if !slices.Equal(host.ran, []string{"nft list table inet ingest", "nft -f -\n" + held}) {
		t.Errorf("held: ran %q", host.ran)
	}
	if b, _ := os.ReadFile(strings.TrimSuffix(f, ".nft") + SetsSuffix); string(b) != held {
		t.Errorf("kept %q", b)
	}
	// Unchanged since the last start: nothing written.
	host.ran = nil
	if err := in.loadTables(ctx, get); err != nil || !slices.Equal(host.ran, []string{"nft list table inet ingest"}) {
		t.Errorf("unchanged: %v, ran %q", err, host.ran)
	}
	host.ran, host.refuse, vals["PEERS"] = nil, "ingest peers", "10.0.0.0/8, 10.1.0.0/16"
	if err := in.loadTables(ctx, get); err == nil || !strings.Contains(err.Error(), "services.ingest.sets (PEERS, PROBES): nft refuses their elements: Error: interval overlaps") {
		t.Errorf("refused: %v", err)
	}
	if b, _ := os.ReadFile(strings.TrimSuffix(f, ".nft") + SetsSuffix); string(b) != held {
		t.Errorf("a refused script kept: %q", b)
	}
	host.refuse = ""
	host.ran, host.refuse, vals["PEERS"] = nil, "", "192.0.2.1; drop"
	if err := in.loadTables(ctx, get); err == nil || !strings.Contains(err.Error(), "PEERS (services.ingest.sets.peers): element \"192.0.2.1; drop\"") || len(host.ran) != 0 {
		t.Errorf("%v, ran %q", err, host.ran)
	}
	// A release that fills no set any more loads the table again, which empties the last input's
	// elements, and keeps no script.
	host.ran, vals["PEERS"] = nil, "192.0.2.1"
	r.Spec.Services = map[string]spec.Service{"ingest": {}}
	if err := in.loadTables(ctx, get); err != nil || len(host.ran) != 2 || !strings.HasPrefix(host.ran[1], "nft -f "+f+".next\n") || strings.Contains(host.ran[1], "add element") {
		t.Errorf("no set filled: %v, ran %q", err, host.ran)
	}
	if fileExists(strings.TrimSuffix(f, ".nft") + SetsSuffix) {
		t.Error("the script stays")
	}
	host.ran = nil
	in.O.Firewall = ""
	if err := in.loadTables(ctx, get); err != nil || len(host.ran) != 0 {
		t.Errorf("an operator's firewall: %v %q", err, host.ran)
	}
}

// A release whose table is of the project's name (the VM's own firewall's) is refused when its
// payload is read: no start, no sets route reaches that table.
func TestAReleaseWithAProjectNamedTableIsRefused(t *testing.T) {
	t.Parallel()
	_, exe := testInstaller(t, "1.0.0")
	files, err := payload.Read(exe)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(files[payload.Spec], &view); err != nil {
		t.Fatal(err)
	}
	table := func(name string) string {
		return strings.Replace(ingestTable, "table inet ingest", "table inet "+name, 1)
	}
	for name, want := range map[string]string{"notes": "no table of the project's name", "queue": "nor of a name nft does not take", "ingest": ""} {
		view["services"] = map[string]any{name: map[string]any{"nftables": map[string]any{"File": name + ".nft"}}}
		files[payload.Spec], _ = json.Marshal(view)
		f := maps.Clone(files)
		f[payload.NftablesDir+name+".nft"] = []byte(table(name))
		_, err := Load(f)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
	// A table no service declares (the project's own name among them), one without its .nft, and a
	// body that reaches past its table are refused: the VM loads each one as root.
	view["services"] = map[string]any{"ingest": map[string]any{"nftables": map[string]any{"File": "ingest.nft"}}}
	files[payload.Spec], _ = json.Marshal(view)
	for path, body := range map[string]string{
		payload.NftablesDir + "notes.nft":  table("notes"),
		payload.NftablesDir + "ingest":     ingestTable,
		payload.NftablesDir + "ingest.nft": ingestTable + "flush ruleset\n",
	} {
		f := maps.Clone(files)
		if path != payload.NftablesDir+"ingest.nft" {
			f[payload.NftablesDir+"ingest.nft"] = []byte(ingestTable)
		}
		f[path] = []byte(body)
		if _, err := Load(f); err == nil {
			t.Errorf("%s accepted", path)
		}
	}
}

// The app socket feeds a set the service's table declares, each element an address, a prefix, a
// port or their concatenation, in one nft command a verb; anything else is refused before nft runs.
func TestTheSetsRouteFeedsTheServicesOwnSets(t *testing.T) {
	t.Parallel()
	host := &nftHost{}
	r := testRelease(t, "1.0.0")
	r.Nftables = map[string][]byte{"ingest": []byte(ingestTable)}
	h := &Hostd{R: r, O: Options{Dir: t.TempDir(), Host: host}}
	api := h.routes(true)
	post := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		api.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return w
	}
	if w := post("/v1/nftables/ingest/sets/probes", `{"add": ["192.0.2.1", "198.51.100.0/24"], "delete": ["192.0.2.9"]}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	want := [][]string{{"nft", "add", "element", "inet", "ingest", "probes", "{ 192.0.2.1, 198.51.100.0/24 }"}, {"nft", "delete", "element", "inet", "ingest", "probes", "{ 192.0.2.9 }"}}
	if !slices.EqualFunc(host.ran, want, slices.Equal) {
		t.Errorf("ran %v", host.ran)
	}
	host.ran = nil
	for _, c := range [][2]string{
		{"/v1/nftables/other/sets/probes", `{"add": ["192.0.2.1"]}`},
		{"/v1/nftables/ingest/sets/nope", `{"add": ["192.0.2.1"]}`},
		{"/v1/nftables/ingest/sets/probes", `{"add": ["192.0.2.1 }; flush ruleset; {"]}`},
		{"/v1/nftables/ingest/sets/probes", `{"add": ["probe.example.net"]}`}, // nft would resolve it
	} {
		if w := post(c[0], c[1]); w.Code < 400 {
			t.Errorf("%s %s: %d", c[0], c[1], w.Code)
		}
	}
	if w := post("/v1/nftables/ingest/sets/probes", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("no element: %d", w.Code)
	}
	// A set an input fills is the input's alone: the next start would empty what the socket added.
	r.Spec.Services = map[string]spec.Service{"ingest": {Sets: map[string]string{"probes": "PROBES"}}}
	if w := post("/v1/nftables/ingest/sets/probes", `{"add": ["192.0.2.1"]}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "is filled from the input PROBES at each start") {
		t.Errorf("an input's set: %d %s", w.Code, w.Body)
	}
	if len(host.ran) != 0 {
		t.Errorf("ran %v", host.ran)
	}
}

// A service's address of its own: the NIC's subnet now, the address in it, the service on its
// networks and the ipvlan's; on a Docker host the NIC is the one --interface names; no address
// given, none.
func TestServicesAddressesOfTheirOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := testRelease(t, "1.0.0")
	r.Spec.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}, "edge": {}}}
	r.Spec.Services = map[string]spec.Service{"relay": {Interface: "edge", AddressInput: "RELAY_ADDRESS"}}
	host := &addrHost{out: "3: edge    inet 192.0.2.7/24 brd 192.0.2.255 scope global edge\n"}
	in := &Installer{R: r, O: Options{Firewall: t.TempDir(), Host: host}}
	vals := map[string]string{"RELAY_ADDRESS": "192.0.2.50"}
	get := func(k string) string { return vals[k] }
	got, err := in.ipvlans(ctx, get)
	if err != nil {
		t.Fatal(err)
	}
	if v := got["relay"]; v.Network != "stowage-edge" || v.Parent != "edge" || v.Subnet.String() != "192.0.2.0/24" || v.Address.String() != "192.0.2.50" {
		t.Errorf("%+v", v)
	}
	extra := map[string]any{}
	giveIPVlan(extra, got["relay"], map[string]any{"default": nil, "back": nil})
	nets := extra["networks"].(map[string]any)
	if len(nets) != 3 || nets["stowage-edge"].(map[string]any)["ipv4_address"] != "192.0.2.50" {
		t.Errorf("networks %v", nets)
	}
	def := ipvlanNetworks(got)["stowage-edge"].(map[string]any)
	if def["driver"] != "ipvlan" || def["driver_opts"].(map[string]string)["parent"] != "edge" {
		t.Errorf("network %v", def)
	}
	vals["RELAY_ADDRESS"] = "198.51.100.1"
	if _, err := in.ipvlans(ctx, get); err == nil || !strings.Contains(err.Error(), "is not an address of 192.0.2.0/24, the subnet of edge") {
		t.Errorf("outside: %v", err)
	}
	for _, bad := range []string{"192.0.2.7", "192.0.2.0", "192.0.2.255"} {
		vals["RELAY_ADDRESS"] = bad
		if _, err := in.ipvlans(ctx, get); err == nil || !strings.Contains(err.Error(), "edge's own address, or 192.0.2.0/24's network or broadcast address") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	vals["RELAY_ADDRESS"] = ""
	if got, err := in.ipvlans(ctx, get); err != nil || len(got) != 0 {
		t.Errorf("none given: %v %v", got, err)
	}
	vals["RELAY_ADDRESS"] = "192.0.2.50"
	docker := &Installer{R: r, O: Options{Host: host}}
	if _, err := docker.ipvlans(ctx, get); err == nil || !strings.Contains(err.Error(), "which NIC of this host plays edge?") {
		t.Errorf("Docker host, no NIC named: %v", err)
	}
	vals[InterfaceVar("edge")] = "eno2"
	if got, err := docker.ipvlans(ctx, get); err != nil || got["relay"].Parent != "eno2" || !slices.Contains(host.devs, "eno2") {
		t.Errorf("Docker host: %+v %v", got, err)
	}
	env := NewEnvFile()
	docker.O.Interfaces = map[string]string{"edge": "eno3"}
	if err := docker.interfaceVars(env); err != nil {
		t.Fatal(err)
	}
	if v, _ := env.Get("STOWAGE_INTERFACE_EDGE"); v != "eno3" {
		t.Errorf("recorded %q", v)
	}
	for role, nic := range map[string]string{"wan": "eno1", "edge": "en o1"} {
		docker.O.Interfaces = map[string]string{role: nic}
		if err := docker.interfaceVars(env); err == nil {
			t.Errorf("--interface %s=%s taken", role, nic)
		}
	}
	in.O.Interfaces = map[string]string{"edge": "eno3"}
	if err := in.interfaceVars(env); err == nil || !strings.Contains(err.Error(), "the VM names its NICs by their roles") {
		t.Errorf("on the VM: %v", err)
	}
}

// addrHost answers `ip -4 -o addr show dev <dev>` with out, recording the devices asked.
type addrHost struct {
	out  string
	devs []string
}

func (h *addrHost) Run(_ context.Context, c Cmd) ([]byte, error) {
	h.devs = append(h.devs, c.Args[len(c.Args)-1])
	return []byte(h.out), nil
}

// A service with a table carries the table's digest and the boot as a label: a changed table or
// a boot recreates it.
func TestAServiceCarriesItsTablesLabel(t *testing.T) {
	t.Parallel()
	var model composeModel
	if err := json.Unmarshal([]byte(`{"services": {"ingest": {"image": "x"}, "api": {"image": "y"}}}`), &model); err != nil {
		t.Fatal(err)
	}
	raw, err := contentCompose(&model, nil, []byte("key"), tune.Container{}, hostPlan{Tables: map[string]string{"ingest": "sha256:ab boot 1"}}, hostNet{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "io.stowage.nftables: sha256:ab boot 1") || strings.Count(string(raw), "io.stowage.nftables") != 1 {
		t.Errorf("content:\n%s", raw)
	}
}

// An input filling a set is checked as it is given (a prompt, a flag, --answers), not first at
// the start, after the tables loaded.
func TestASetsInputIsCheckedAsItIsGiven(t *testing.T) {
	t.Parallel()
	r := testRelease(t, "1.0.0")
	r.Spec.Inputs = append(r.Spec.Inputs, spec.Input{Key: "PROBES", Optional: true})
	r.Spec.Services = map[string]spec.Service{"ingest": {Nftables: spec.NftTable{File: "ingest.nft"}, Sets: map[string]string{"probes": "PROBES"}}}
	r.Spec.L4.Routes = []spec.L4Route{{Name: "feed", Protocol: "udp", Port: 17007, Upstream: "ingest:17007", AllowInput: "PROBES"}}
	in := &Installer{R: r}
	for _, i := range in.inputList(Loopback) {
		if i.Key != "PROBES" {
			continue
		}
		// An L4 route's allowlist too: both checks hold (a concatenation is no source).
		if i.check == nil || i.check("192.0.2.1, 198.51.100.0/24") != nil || i.check("192.0.2.1;10.0.0.0/8") == nil || i.check("10.0.0.1 . 443") == nil {
			t.Errorf("PROBES is not checked as a set's elements")
		}
		return
	}
	t.Error("PROBES not listed")
}

// At boot, a table loaded again has the sets its inputs filled at the last start written in the
// same transaction, before Docker restores the containers: never empty a moment.
func TestTheBootFillsTheSetsItEmptied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &stdinHost{}
	r := testRelease(t, "1.0.0")
	r.Nftables = map[string][]byte{"ingest": []byte(ingestTable)}
	r.Spec.Services = map[string]spec.Service{"ingest": {Sets: map[string]string{"probes": "PROBES"}}}
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: t.TempDir(), Host: host, Log: io.Discard}}
	if err := in.loadTables(ctx, func(string) string { return "192.0.2.7" }); err != nil {
		t.Fatal(err)
	}
	host.ran = nil
	if err := in.bootTables(ctx); err != nil {
		t.Fatal(err)
	}
	if len(host.ran) != 1 || !strings.HasPrefix(host.ran[0], "nft -f -\n# ingest's nftables table") ||
		!strings.HasSuffix(host.ran[0], ingestTable+"flush set inet ingest probes\nadd element inet ingest probes { 192.0.2.7 }\n") {
		t.Errorf("ran %q", host.ran)
	}
	// Sets nft refuses now: the table loads alone, said.
	host.ran, host.refuse = nil, "add element"
	err := in.bootTables(ctx)
	if err == nil || !strings.Contains(err.Error(), "services.ingest.sets (PROBES): nft refuses their elements at boot, so the table loads without them") ||
		len(host.ran) != 2 || !strings.HasPrefix(host.ran[1], "nft -f "+filepath.Join(in.O.Dir, TablesDir, "ingest.nft")) {
		t.Errorf("%v, ran %q", err, host.ran)
	}
	// The boot's start finds the table held, its sets empty: it writes them again.
	host.ran, host.refuse = nil, ""
	if err := in.loadTables(ctx, func(string) string { return "192.0.2.7" }); err != nil ||
		!slices.Equal(host.ran, []string{"nft list table inet ingest", "nft -f -\nflush set inet ingest probes\nadd element inet ingest probes { 192.0.2.7 }\n"}) {
		t.Errorf("the boot's start: %v, ran %q", err, host.ran)
	}
	// The table itself refused: said as the table's, not the sets'.
	host.ran, host.refuse = nil, "table inet ingest"
	if err := in.bootTables(ctx); err == nil || strings.Contains(err.Error(), "sets") || !strings.Contains(err.Error(), "services.ingest.nftables: its table does not load at boot") {
		t.Errorf("the table refused: %v", err)
	}
}

// A held table whose sets nft refuses leaves the others made all the same: a changed one loads.
func TestARefusedSetLeavesTheOtherTablesMade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &stdinHost{}
	r := testRelease(t, "1.0.0")
	r.Nftables = map[string][]byte{"a": []byte(strings.Replace(ingestTable, "ingest", "a", 1)), "b": []byte(strings.Replace(ingestTable, "ingest", "b", 1))}
	r.Spec.Services = map[string]spec.Service{"a": {Sets: map[string]string{"probes": "A"}}, "b": {}}
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: t.TempDir(), Host: host, Log: io.Discard}}
	vals := map[string]string{"A": "192.0.2.1"}
	get := func(k string) string { return vals[k] }
	if err := in.loadTables(ctx, get); err != nil {
		t.Fatal(err)
	}
	host.ran, host.refuse, vals["A"] = nil, "inet a probes", "192.0.2.2"
	r.Nftables["b"] = []byte(strings.Replace(string(r.Nftables["b"]), "ipv4_addr", "ipv6_addr", 1))
	err := in.loadTables(ctx, get)
	if err == nil || !strings.Contains(err.Error(), "services.a.sets (A): nft refuses their elements") ||
		!slices.ContainsFunc(host.ran, func(l string) bool { return strings.HasPrefix(l, "nft -f "+in.tableFile("b", ".nft.next")) }) {
		t.Errorf("%v, ran %q", err, host.ran)
	}
	// b refused too: a's refusal is said with b's.
	host.ran, host.alsoRefuse, vals["A"] = nil, "ether_addr", "192.0.2.3"
	r.Nftables["b"] = []byte(strings.Replace(string(r.Nftables["b"]), "ipv6_addr", "ether_addr", 1))
	if err := in.loadTables(ctx, get); err == nil || !strings.Contains(err.Error(), "services.a.sets (A): nft refuses their elements") || !strings.Contains(err.Error(), "services.b.nftables: nft refuses its table") {
		t.Errorf("both refused: %v", err)
	}
}

// A held table whose sets' script does not read loads again with its sets: what they hold is
// unknown.
func TestAnUnreadSetsScriptLoadsTheTableAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &stdinHost{}
	r := testRelease(t, "1.0.0")
	r.Nftables = map[string][]byte{"ingest": []byte(ingestTable)}
	r.Spec.Services = map[string]spec.Service{"ingest": {Sets: map[string]string{"probes": "PROBES"}}}
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: t.TempDir(), Host: host, Log: io.Discard}}
	get := func(string) string { return "192.0.2.7" }
	if err := in.loadTables(ctx, get); err != nil {
		t.Fatal(err)
	}
	// A link to itself, which no one reads (root included).
	f := in.tableFile("ingest", SetsSuffix)
	if err := os.Remove(f); err != nil || os.Symlink(f, f) != nil {
		t.Fatal(err)
	}
	host.ran = nil
	if err := in.loadTables(ctx, get); err != nil || len(host.ran) != 2 || !strings.HasPrefix(host.ran[1], "nft -f "+in.tableFile("ingest", ".nft.next")) {
		t.Errorf("%v, ran %q", err, host.ran)
	}
}

// A table an input picks: the release carries a file by each value, each judged as a table file;
// a start loads the one the input's value picks, again when the value changes, and a value without
// a file is refused at the input and at the start, before any table loads.
// postureTables is a release whose ingest table the input POSTURE picks (observe, enforce: the
// enforce file's table is "# enforce\n" before observe's), the payload edited by edit first; view
// and files are its spec and payload without the tables.
func postureTables(t *testing.T) (picked func(edit func(f map[string][]byte)) (*Release, error), enforce string, view map[string]any, files map[string][]byte) {
	t.Helper()
	_, exe := testInstaller(t, "1.0.0")
	files, err := payload.Read(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(files[payload.Spec], &view); err != nil {
		t.Fatal(err)
	}
	view["services"] = map[string]any{"ingest": map[string]any{"nftables": map[string]any{"Input": "POSTURE", "Files": map[string]any{"observe": "a.nft", "enforce": "b.nft"}}}}
	view["Inputs"] = []any{map[string]any{"Key": "POSTURE", "Default": "observe"}, map[string]any{"Key": "HIDDEN", "Secret": true, "Default": "observe"},
		map[string]any{"Key": "MODE", "When": "tls", "Default": "observe"}, map[string]any{"Key": "PROF", "Profile": "p", "Default": "observe"},
		map[string]any{"Key": "ADV", "Advanced": true}, map[string]any{"Key": "K8S", "Shapes": []any{"kubernetes"}, "Default": "observe"}, map[string]any{"Key": "BARE"}}
	files[payload.Spec], _ = json.Marshal(view)
	enforce = "# enforce\n" + ingestTable
	picked = func(edit func(f map[string][]byte)) (*Release, error) {
		f := maps.Clone(files)
		f[payload.NftablesDir+"ingest/observe.nft"] = []byte(ingestTable)
		f[payload.NftablesDir+"ingest/enforce.nft"] = []byte(enforce)
		if edit != nil {
			edit(f)
		}
		return Load(f)
	}
	return picked, enforce, view, files
}

func TestAnInputPicksTheServicesTable(t *testing.T) {
	t.Parallel()
	picked, enforce, view, files := postureTables(t)
	r, err := picked(nil)
	if err != nil || !slices.Equal(r.Tables(), []string{"ingest"}) || !slices.Equal(r.tableSets("ingest"), []string{"probes"}) {
		t.Fatalf("%v %v", r, err)
	}
	for name, edit := range map[string]func(f map[string][]byte){
		"has no nftables/ingest/enforce.nft": func(f map[string][]byte) { delete(f, payload.NftablesDir+"ingest/enforce.nft") },
		"is no table a service declares":     func(f map[string][]byte) { f[payload.NftablesDir+"ingest/count.nft"] = []byte(ingestTable) },
		"nftables/ingest.nft is no table":    func(f map[string][]byte) { f[payload.NftablesDir+"ingest.nft"] = []byte(ingestTable) },
		"services.ingest.nftables.files.enforce": func(f map[string][]byte) {
			f[payload.NftablesDir+"ingest/enforce.nft"] = []byte(ingestTable + "flush ruleset\n")
		},
		"its files declare different sets": func(f map[string][]byte) {
			f[payload.NftablesDir+"ingest/enforce.nft"] = []byte(strings.Replace(ingestTable, "set probes", "set trusted", 1))
		},
	} {
		if _, err := picked(edit); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
	host := &nftHost{}
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: t.TempDir(), Host: host, Log: io.Discard}}
	posture := "observe"
	get := func(k string) string {
		if k == "POSTURE" {
			return posture
		}
		return ""
	}
	loaded := func() string {
		b, _ := os.ReadFile(filepath.Join(in.O.Dir, TablesDir, "ingest.nft"))
		return string(b)
	}
	if err := in.loadTables(context.Background(), get); err != nil || !strings.HasSuffix(loaded(), "delete table inet ingest\n"+ingestTable) {
		t.Errorf("observe: %v\n%s", err, loaded())
	}
	posture = "enforce"
	if err := in.loadTables(context.Background(), get); err != nil || !strings.HasSuffix(loaded(), "delete table inet ingest\n"+enforce) {
		t.Errorf("enforce: %v\n%s", err, loaded())
	}
	posture, host.ran = "count", nil
	const refused = `"count" picks no table of ingest (services.ingest.nftables.files: enforce, observe)`
	if err := in.loadTables(context.Background(), get); err == nil || !strings.HasPrefix(err.Error(), "POSTURE: "+refused+": a re-run of the install, `"+in.ctl()+" --dir "+in.O.Dir+" --set <input>=<value>`, picks one") || len(host.ran) != 0 {
		t.Errorf("a value without a file: %v, ran %v", err, host.ran)
	}
	in.O.Preloaded = true
	if err := in.loadTables(context.Background(), get); err == nil || !strings.HasSuffix(err.Error(), ": a re-run of the install, `sudo notes-stow install --set <input>=<value>`, picks one") {
		t.Errorf("a value without a file on the VM: %v", err)
	}
	in.O.Preloaded = false
	if err := in.tablePick("POSTURE")("count"); err == nil || err.Error() != refused {
		t.Errorf("the input's check: %v", err)
	}
	// Another service's table the same input picks: its values are checked too.
	in.R.Spec.Services["probe"] = spec.Service{Nftables: spec.NftTable{Input: "POSTURE", Files: map[string]string{"observe": "p.nft"}}}
	if err := in.tablePick("POSTURE")("enforce"); err == nil || err.Error() != `"enforce" picks no table of probe (services.probe.nftables.files: observe)` {
		t.Errorf("a value one of two tables has no file for: %v", err)
	}
	delete(in.R.Spec.Services, "probe")
	if err := in.tablePick("POSTURE")("enforce"); err != nil {
		t.Errorf("the input's check of a value with its file: %v", err)
	}
	if in.tablePick("OTHER") != nil {
		t.Error("a check for an input picking no table")
	}
	// An upgrade to this release asks again, as an input it adds, one whose recorded value it has
	// no file for; one whose value it has, it keeps.
	in.R.Spec.Inputs = []spec.Input{{Key: "POSTURE", Default: "observe"}}
	env := NewEnvFile()
	env.Set("POSTURE", "strict")
	if added := in.addedInputs(env, Loopback); len(added) != 1 || added[0].Key != "POSTURE" {
		t.Errorf("a recorded value without a file: %v", added)
	}
	env.Set("POSTURE", "enforce")
	if added := in.addedInputs(env, Loopback); len(added) != 0 {
		t.Errorf("a recorded value with its file: %v", added)
	}
	in.R.Spec.Inputs = nil
	// A picking input not asked on every install, in the clear, with a value, is refused.
	for _, k := range []string{"NONE", "HIDDEN", "MODE", "PROF", "ADV", "K8S", "BARE"} {
		v := maps.Clone(view)
		v["services"] = map[string]any{"ingest": map[string]any{"nftables": map[string]any{"Input": k, "Files": map[string]any{"observe": "a.nft"}}}}
		f := maps.Clone(files)
		f[payload.Spec], _ = json.Marshal(v)
		if _, err := Load(f); err == nil || !strings.Contains(err.Error(), k+" is no input asked on every install, in the clear") {
			t.Errorf("%s: %v", k, err)
		}
	}
	// A table neither a file nor an input with its files, or both, is refused.
	for _, nft := range []map[string]any{{"Input": "POSTURE"}, {"File": "a.nft", "Input": "POSTURE", "Files": map[string]any{"observe": "a.nft"}}} {
		v := maps.Clone(view)
		v["services"] = map[string]any{"ingest": map[string]any{"nftables": nft}}
		f := maps.Clone(files)
		f[payload.Spec], _ = json.Marshal(v)
		if _, err := Load(f); err == nil || !strings.Contains(err.Error(), "a file, or an input and its files, not both nor either alone") {
			t.Errorf("%v: %v", nft, err)
		}
	}
}

func TestATableLoadedAnewFlushesTheConnectionTracking(t *testing.T) {
	t.Parallel()
	picked, _, _, _ := postureTables(t)
	r, err := picked(nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := r.Spec.Services["ingest"]
	svc.FlushConntrack = true
	r.Spec.Services["ingest"] = svc
	var log bytes.Buffer
	flushes, flushErr := 0, error(nil)
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: t.TempDir(), Host: &nftHost{}, Log: &log,
		FlushConntrack: func() error { flushes++; return flushErr }}}
	posture := "observe"
	get := func(k string) string {
		if k == "POSTURE" {
			return posture
		}
		return ""
	}
	start := func(want int, what string) {
		t.Helper()
		flushes = 0
		if err := in.loadTables(context.Background(), get); err != nil || flushes != want {
			t.Errorf("%s: %v, %d flushes, want %d\n%s", what, err, flushes, want, log.String())
		}
	}
	start(1, "the first load")
	start(0, "the same table held")
	posture = "enforce"
	start(1, "another table picked")
	if !strings.Contains(log.String(), "flushed the host's connection-tracking table (services.ingest.flush_conntrack)") {
		t.Errorf("the flush is not logged:\n%s", log.String())
	}
	// A flush that fails is warned, the start going on, and the next start, which finds the table
	// held, flushes it then.
	posture, flushErr = "observe", errors.New("operation not permitted")
	log.Reset()
	start(1, "a flush that fails")
	if !strings.Contains(log.String(), "warning: services.ingest.flush_conntrack: the host's connection-tracking table does not flush: operation not permitted: the connections open before keep the previous table's verdict until they end, or until `sudo conntrack -F` (the next start tries again)") {
		t.Errorf("a failed flush is not warned:\n%s", log.String())
	}
	flushErr = nil
	start(1, "a flush failed before")
	start(0, "flushed since")
	// A table held, loaded again for its sets alone (their script unread), flushes nothing.
	sets := filepath.Join(in.O.Dir, TablesDir, "ingest"+SetsSuffix)
	if err := os.MkdirAll(sets, 0o755); err != nil {
		t.Fatal(err)
	}
	start(0, "a load for the sets alone")
	if err := os.RemoveAll(sets); err != nil {
		t.Fatal(err)
	}
	// A load's file left by a start cut off: what the firewall holds is unknown, so the table
	// loads anew.
	if err := os.WriteFile(filepath.Join(in.O.Dir, TablesDir, "ingest.nft.next"), []byte("table inet ingest {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start(1, "a load cut off before")
	start(0, "loaded since")
	// A start whose sync fails after the load flushes all the same.
	record := filepath.Join(in.O.Dir, TablesFile)
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(record, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	posture, flushes = "enforce", 0
	if err := in.loadTables(context.Background(), get); err == nil || flushes != 1 {
		t.Errorf("a sync failing after the load: %v, %d flushes", err, flushes)
	}
	if err := os.RemoveAll(record); err != nil {
		t.Fatal(err)
	}
	// Without flush_conntrack a change of rules is noted, and nothing flushes.
	svc.FlushConntrack = false
	r.Spec.Services["ingest"] = svc
	posture = "enforce"
	log.Reset()
	start(0, "without flush_conntrack")
	if !strings.Contains(log.String(), "note: services.ingest.nftables: its table changed, and the connections open before keep their conntrack entries") {
		t.Errorf("no note:\n%s", log.String())
	}
	log.Reset()
	start(0, "the same table, without flush_conntrack")
	if strings.Contains(log.String(), "note:") {
		t.Errorf("a note for a table held:\n%s", log.String())
	}
}
