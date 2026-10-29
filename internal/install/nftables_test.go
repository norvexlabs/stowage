package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: t.TempDir(), Host: host}}
	if err := in.loadTables(ctx); err != nil {
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
	if err := in.loadTables(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(host.ran, [][]string{{"nft", "list", "table", "inet", "ingest"}}, slices.Equal) || in.tableLoads()["ingest"] != first {
		t.Errorf("an unchanged table: ran %v", host.ran)
	}
	// Absent (a boot, or a table deleted by hand): loaded again, a new token recreating its service.
	host.ran, host.listErr = nil, errors.New("no such table")
	if err := in.loadTables(ctx); err != nil || len(host.ran) != 2 || in.tableLoads()["ingest"] == first {
		t.Errorf("an absent table: %v, ran %v", err, host.ran)
	}
	host.listErr = nil
	// A changed one is loaded again.
	host.ran = nil
	in.R.Nftables = map[string][]byte{"ingest": []byte(strings.Replace(ingestTable, "ipv4_addr", "ipv6_addr", 1))}
	if err := in.loadTables(ctx); err != nil || len(host.ran) != 1 || host.ran[0][1] != "-f" {
		t.Errorf("a changed table: %v, ran %v", err, host.ran)
	}
	host.ran = nil
	in.R.Nftables = nil
	if err := in.loadTables(ctx); err != nil {
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
	if err := in.loadTables(ctx); err == nil || !strings.Contains(err.Error(), "services.b.nftables: nft refuses its table") {
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
	if err := in.loadTables(ctx); err != nil || !slices.EqualFunc(host.ran, [][]string{{"nft", "list", "table", "inet", "a"}}, slices.Equal) {
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
	if err := docker.loadTables(ctx); err != nil || len(host.ran) != 0 || docker.gatewayStart(ctx, none) != nil || !strings.Contains(log.String(), "the services' nftables tables (ingest) are not in force on this host") || docker.tableLoads() != nil {
		t.Errorf("Docker host: %v, ran %v, log %q", err, host.ran, log.String())
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
		view["services"] = map[string]any{name: map[string]any{"nftables": name + ".nft"}}
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
	view["services"] = map[string]any{"ingest": map[string]any{"nftables": "ingest.nft"}}
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
	for path, body := range map[string]string{
		"/v1/nftables/other/sets/probes":  `{"add": ["192.0.2.1"]}`,
		"/v1/nftables/ingest/sets/nope":   `{"add": ["192.0.2.1"]}`,
		"/v1/nftables/ingest/sets/probes": `{"add": ["192.0.2.1 }; flush ruleset; {"]}`,
	} {
		if w := post(path, body); w.Code < 400 {
			t.Errorf("%s %s: %d", path, body, w.Code)
		}
	}
	if w := post("/v1/nftables/ingest/sets/probes", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("no element: %d", w.Code)
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
