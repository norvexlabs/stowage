package install

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

// setHost is nft over named sets of one table: it lists them as `nft -j -n list set` does, adds
// and deletes elements, and runs scripts (given on stdin or in a file) of flush set and add
// element lines; anything else succeeds and does nothing.
type setHost struct {
	mu    sync.Mutex
	types map[string][]string        // by set
	elems map[string]map[string]bool // by set
	ran   []string
	// refuse fails a script holding it, which then changes nothing.
	refuse string
	// changed runs after a change; a listing whose context is done fails.
	changed func()
}

func (h *setHost) Run(ctx context.Context, c Cmd) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ran = append(h.ran, strings.Join(c.Args, " "))
	a := c.Args
	switch {
	case len(a) == 8 && a[1] == "-j" && a[3] == "list" && a[4] == "set":
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return h.list(a[7])
	case len(a) == 7 && a[2] == "element":
		if h.refuse != "" && strings.Contains(strings.Join(a, " "), h.refuse) {
			return nil, errors.New("Error: Could not process rule: No such file or directory")
		}
		h.change(a[1], a[5], a[6])
		if h.changed != nil {
			h.changed()
		}
		return nil, nil
	case len(a) == 3 && a[1] == "-f":
		var script []byte
		if c.Stdin != nil {
			script, _ = io.ReadAll(c.Stdin)
		} else {
			script, _ = os.ReadFile(a[2])
		}
		if h.refuse != "" && strings.Contains(string(script), h.refuse) {
			return nil, errors.New("Error: Could not process rule: No such file or directory")
		}
		for line := range strings.Lines(string(script)) {
			f := strings.Fields(line)
			switch {
			case len(f) == 5 && f[0] == "flush" && f[1] == "set":
				h.elems[f[4]] = map[string]bool{}
			case len(f) > 6 && (f[0] == "add" || f[0] == "delete") && f[1] == "element":
				h.change(f[0], f[4], strings.Join(f[6:len(f)-1], " "))
			}
		}
	}
	return nil, nil
}

func (h *setHost) change(verb, set, list string) {
	if h.elems[set] == nil {
		h.elems[set] = map[string]bool{}
	}
	for e := range strings.SplitSeq(strings.Trim(list, "{ }"), ",") {
		e = strings.Join(strings.Fields(e), " ")
		if verb == "add" {
			h.elems[set][e] = true
		} else {
			delete(h.elems[set], e)
		}
	}
}

func (h *setHost) list(set string) ([]byte, error) {
	types, ok := h.types[set]
	if !ok {
		return nil, errors.New("No such file or directory")
	}
	render := func(p string) any {
		if a, l, ok := strings.Cut(p, "/"); ok {
			return map[string]any{"prefix": map[string]any{"addr": a, "len": json.Number(l)}}
		}
		if lo, hi, ok := strings.Cut(p, "-"); ok {
			return map[string]any{"range": []any{json.Number(lo), json.Number(hi)}}
		}
		if strings.Trim(p, "0123456789") == "" {
			return json.Number(p)
		}
		return p
	}
	var elems []any
	for _, e := range slices.Sorted(func(yield func(string) bool) {
		for e := range h.elems[set] {
			if !yield(e) {
				return
			}
		}
	}) {
		parts := strings.Split(e, " . ")
		if len(parts) == 1 {
			elems = append(elems, map[string]any{"elem": map[string]any{"val": render(e), "expires": 1}})
			continue
		}
		var cs []any
		for _, p := range parts {
			cs = append(cs, render(p))
		}
		elems = append(elems, map[string]any{"concat": cs})
	}
	var typ any = types[0]
	if len(types) > 1 {
		typ = types
	}
	return json.Marshal(map[string]any{"nftables": []any{map[string]any{"metainfo": map[string]any{}}, map[string]any{"set": map[string]any{"name": set, "type": typ, "elem": elems}}}})
}

// entry is a TCP flow from src:sport to dst:dport.
func entry(src, dst string, sport, dport uint16) ctEntry {
	c := ctEntry{proto: 6, hasPorts: true}
	c.addrs = [4]netip.Addr{netip.MustParseAddr(src), netip.MustParseAddr(dst), netip.MustParseAddr(dst), netip.MustParseAddr(src)}
	c.ports = [4]uint16{sport, dport, dport, sport}
	return c
}

// An element matches the flows any end of which its key's parts hold: an address, a prefix or a
// range of them, a port or a range of ports, a protocol; a concatenation by every part. A set of
// another key type is not matched.
func TestAChangedElementMatchesItsFlows(t *testing.T) {
	t.Parallel()
	flow := entry("192.0.2.7", "198.51.100.1", 40000, 443)
	for _, c := range []struct {
		types []string
		elem  string
		want  bool
	}{
		{[]string{"ipv4_addr"}, `"192.0.2.7"`, true},
		{[]string{"ipv4_addr"}, `"198.51.100.1"`, true},
		{[]string{"ipv4_addr"}, `"192.0.2.8"`, false},
		{[]string{"ipv4_addr"}, `{"prefix":{"addr":"192.0.2.0","len":24}}`, true},
		{[]string{"ipv4_addr"}, `{"prefix":{"addr":"192.0.3.0","len":24}}`, false},
		{[]string{"ipv4_addr"}, `{"range":["192.0.2.1","192.0.2.7"]}`, true},
		{[]string{"ipv4_addr"}, `{"range":["192.0.2.8","192.0.2.9"]}`, false},
		{[]string{"ipv6_addr"}, `"2001:db8::7"`, false},
		{[]string{"inet_service"}, `443`, true},
		{[]string{"inet_service"}, `"443"`, true},
		{[]string{"inet_service"}, `444`, false},
		{[]string{"inet_service"}, `{"range":[400,450]}`, true},
		{[]string{"inet_service"}, `{"range":[444,450]}`, false},
		{[]string{"inet_proto"}, `6`, true},
		{[]string{"inet_proto"}, `17`, false},
		{[]string{"inet_proto"}, `"tcp"`, true},
		{[]string{"inet_proto"}, `"udp"`, false},
		{[]string{"inet_proto"}, `{"range":["icmp","tcp"]}`, true},
		{[]string{"ipv4_addr", "inet_proto"}, `{"concat":["192.0.2.7","tcp"]}`, true},
		{[]string{"ipv4_addr", "inet_service"}, `{"concat":["192.0.2.7",443]}`, true},
		{[]string{"ipv4_addr", "inet_service"}, `{"concat":["192.0.2.7",80]}`, false},
		{[]string{"ipv4_addr", "inet_service"}, `{"concat":["192.0.2.9",443]}`, false},
	} {
		m, err := elementMatch(c.types, json.RawMessage(c.elem))
		if err != nil {
			t.Errorf("%v %s: %v", c.types, c.elem, err)
			continue
		}
		if got := m(flow); got != c.want {
			t.Errorf("%v %s: %v, want %v", c.types, c.elem, got, c.want)
		}
	}
	v6 := entry("2001:db8::7", "2001:db8::1", 40000, 443)
	if m, err := elementMatch([]string{"ipv6_addr"}, json.RawMessage(`{"prefix":{"addr":"2001:db8::","len":64}}`)); err != nil || !m(v6) || m(flow) {
		t.Errorf("an IPv6 prefix: %v", err)
	}
	// Each name nft prints is a protocol's number.
	for name, n := range map[string]uint8{"udp": 17, "icmp": 1, "icmpv6": 58, "sctp": 132, "udplite": 136, "esp": 50, "ah": 51, "gre": 47} {
		m, err := elementMatch([]string{"inet_proto"}, json.RawMessage(`"`+name+`"`))
		if err != nil || !m(ctEntry{proto: n}) || m(ctEntry{proto: n + 1}) {
			t.Errorf("protocol %s: %v", name, err)
		}
	}
	// Without ports (ICMP), a port matches nothing.
	icmp := ctEntry{proto: 1, addrs: flow.addrs}
	if m, _ := elementMatch([]string{"inet_service"}, json.RawMessage(`{"range":[0,65535]}`)); m(icmp) {
		t.Error("a port matches a flow without ports")
	}
	for _, c := range []struct {
		types []string
		elem  string
	}{
		{[]string{"ether_addr"}, `"02:00:00:00:00:01"`},
		{[]string{"inet_service"}, `"https"`},
		{[]string{"mark"}, `1`},
		{[]string{"inet_proto"}, `"no-such-protocol"`},
	} {
		if _, err := elementMatch(c.types, json.RawMessage(c.elem)); !errors.Is(err, errUnmatched) {
			t.Errorf("%v %s: %v", c.types, c.elem, err)
		}
	}
	for _, bad := range []string{`"300.0.0.1"`, `{"range":["192.0.2.9","192.0.2.1"]}`, `{"prefix":{"addr":"192.0.2.0","len":33}}`, `{"concat":["192.0.2.1"]}`} {
		if _, err := elementMatch([]string{"ipv4_addr"}, json.RawMessage(bad)); err == nil {
			t.Errorf("%s matched", bad)
		}
	}
	if _, err := elementMatch([]string{"inet_service"}, json.RawMessage(`65536`)); err == nil {
		t.Error("port 65536 matched")
	}
}

// The elements changed are those one listing holds and the other not, whatever their timeout
// and expiry.
func TestTheChangedElementsAreTheListingsDifference(t *testing.T) {
	t.Parallel()
	list := func(elems ...string) nftSet {
		h := &setHost{types: map[string][]string{"s": {"ipv4_addr"}}, elems: map[string]map[string]bool{"s": {}}}
		for _, e := range elems {
			h.elems["s"][e] = true
		}
		out, _ := h.list("s")
		s, err := parseNftSet(out)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	m, n, err := changedFlows(list("192.0.2.1", "192.0.2.2"), list("192.0.2.2", "192.0.2.3"))
	if err != nil || n != 2 {
		t.Fatalf("%d changed: %v", n, err)
	}
	for addr, want := range map[string]bool{"192.0.2.1": true, "192.0.2.2": false, "192.0.2.3": true} {
		if got := m(entry(addr, "198.51.100.1", 40000, 443)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
	if _, n, err := changedFlows(list("192.0.2.1"), list("192.0.2.1")); err != nil || n != 0 {
		t.Errorf("unchanged: %d %v", n, err)
	}
}

// ctAttr is a netlink attribute of typ holding data.
func ctAttr(typ uint16, data ...[]byte) []byte {
	body := bytes.Join(data, nil)
	b := make([]byte, 4, 4+len(body)+3)
	binary.NativeEndian.PutUint16(b, uint16(4+len(body)))
	binary.NativeEndian.PutUint16(b[2:], typ)
	b = append(b, body...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// A dump's entry gives its tuples' addresses, ports and protocol, and deletes by its original
// tuple and zone as the kernel gave them.
func TestAConntrackEntryReads(t *testing.T) {
	t.Parallel()
	port := func(p uint16) []byte { return binary.BigEndian.AppendUint16(nil, p) }
	tuple := func(typ uint16, src, dst string, sport, dport uint16) []byte {
		return ctAttr(typ|0x8000,
			ctAttr(ctaTupleIP|0x8000, ctAttr(ctaIPv4Src, netip.MustParseAddr(src).AsSlice()), ctAttr(ctaIPv4Dst, netip.MustParseAddr(dst).AsSlice())),
			ctAttr(ctaTupleProto|0x8000, ctAttr(ctaProtoNum, []byte{6}), ctAttr(ctaProtoSrc, port(sport)), ctAttr(ctaProtoDst, port(dport))))
	}
	orig := tuple(ctaTupleOrig, "192.0.2.7", "198.51.100.1", 40000, 443)
	zone := ctAttr(ctaZone, port(0))
	msg := slices.Concat(orig, tuple(ctaTupleReply, "172.17.0.2", "192.0.2.7", 8443, 40000), ctAttr(3, []byte{1, 2, 3, 4}), zone)
	c, ok, err := parseCTEntry(2, msg)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	want := [4]netip.Addr{netip.MustParseAddr("192.0.2.7"), netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("172.17.0.2"), netip.MustParseAddr("192.0.2.7")}
	if c.addrs != want || c.ports != [4]uint16{40000, 443, 8443, 40000} || c.proto != 6 || !c.hasPorts || c.family != 2 {
		t.Errorf("%+v", c)
	}
	if !bytes.Equal(c.del, slices.Concat(orig, zone)) {
		t.Errorf("deleted by %x", c.del)
	}
	// The reply's address after Docker's DNAT matches an element of the container's address.
	if m, _ := elementMatch([]string{"ipv4_addr"}, json.RawMessage(`"172.17.0.2"`)); !m(c) {
		t.Error("the reply tuple is not matched")
	}
	if _, ok, err := parseCTEntry(2, zone); ok || err != nil {
		t.Errorf("an entry without its tuple: %v %v", ok, err)
	}
	if _, _, err := parseCTEntry(2, []byte{200, 0, 1, 0}); err == nil {
		t.Error("a truncated attribute read")
	}
}

// The app socket's change of a set under flush_conntrack ends the flows of the elements nft
// changed, those of the elements it did not change kept, and reads no table when none changed; a
// set whose flows cannot be told keeps them, with a warning, and never flushes the whole table;
// without flush_conntrack nothing is ended.
func TestTheSetsRouteEndsTheFlowsOfTheChangedElements(t *testing.T) {
	t.Parallel()
	flows := []ctEntry{entry("192.0.2.1", "198.51.100.1", 40000, 443), entry("192.0.2.2", "198.51.100.1", 40001, 443), entry("192.0.2.3", "198.51.100.1", 40002, 443)}
	for _, c := range []struct {
		name    string
		flushes bool
		types   []string
		body    string
		ended   []int // the flows deleted, by index
		kept    bool  // the flows kept, with a warning
	}{
		{"an element added", true, []string{"ipv4_addr"}, `{"add": ["192.0.2.2"]}`, []int{1}, false},
		{"an element there already", true, []string{"ipv4_addr"}, `{"add": ["192.0.2.1"]}`, nil, false},
		{"an element deleted", true, []string{"ipv4_addr"}, `{"delete": ["192.0.2.1"]}`, []int{0}, false},
		{"a prefix added", true, []string{"ipv4_addr"}, `{"add": ["192.0.2.2/31"]}`, []int{1, 2}, false},
		{"another key type", true, []string{"ether_addr"}, `{"add": ["192.0.2.2"]}`, nil, true},
		{"a set that does not list", true, nil, `{"add": ["192.0.2.2"]}`, nil, true},
		{"without flush_conntrack", false, []string{"ipv4_addr"}, `{"add": ["192.0.2.2"]}`, nil, false},
	} {
		r := testRelease(t, "1.0.0")
		r.Nftables = map[string][]byte{"ingest": []byte(ingestTable)}
		r.Spec.Services = map[string]spec.Service{"ingest": {FlushConntrack: c.flushes}}
		host := &setHost{types: map[string][]string{"probes": c.types}, elems: map[string]map[string]bool{"probes": {"192.0.2.1": true}}}
		if c.types == nil {
			delete(host.types, "probes")
		}
		var ended []int
		deletes, flushes := 0, 0
		h := &Hostd{R: r, O: Options{Dir: t.TempDir(), Host: host, Log: io.Discard,
			FlushConntrack: func() error { flushes++; return nil },
			DeleteConntrack: func(match func(ctEntry) bool) (int, error) {
				deletes++
				for i, f := range flows {
					if match(f) {
						ended = append(ended, i)
					}
				}
				return len(ended), nil
			}}}
		w := httptest.NewRecorder()
		h.routes(true).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/nftables/ingest/sets/probes", strings.NewReader(c.body)))
		var done SetElementsDone
		if err := json.Unmarshal(w.Body.Bytes(), &done); w.Code != http.StatusOK || err != nil {
			t.Fatalf("%s: %d %s", c.name, w.Code, w.Body)
		}
		// Never the whole table; the table read only when an element changed.
		if !slices.Equal(ended, c.ended) || flushes != 0 || (deletes == 0) != (len(c.ended) == 0) {
			t.Errorf("%s: ended %v, %d flushes, %d deletions: %s", c.name, ended, flushes, deletes, w.Body)
		}
		if c.kept != strings.Contains(done.Warning, "are kept, as they judged them before") {
			t.Errorf("%s: %s", c.name, w.Body)
		}
		if c.flushes && !c.kept && (done.FlowsEnded == nil || *done.FlowsEnded != len(c.ended)) || !c.flushes && (done.FlowsEnded != nil || deletes != 0) {
			t.Errorf("%s: %s, %d deletions", c.name, w.Body, deletes)
		}
	}
}

// A start's change of a set an input fills, under flush_conntrack, ends the flows of the elements
// changed alone and leaves no flush due; a set whose flows cannot be told flushes the whole table.
func TestAStartsSetChangeEndsTheFlowsOfItsElements(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &setHost{types: map[string][]string{"probes": {"ipv4_addr"}}, elems: map[string]map[string]bool{}}
	r := testRelease(t, "1.0.0")
	r.Nftables = map[string][]byte{"ingest": []byte(ingestTable)}
	r.Spec.Services = map[string]spec.Service{"ingest": {FlushConntrack: true, Sets: map[string]string{"probes": "PROBES"}}}
	flows := []ctEntry{entry("192.0.2.1", "198.51.100.1", 40000, 443), entry("192.0.2.2", "198.51.100.1", 40001, 443)}
	var ended []int
	flushes := 0
	var log bytes.Buffer
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: t.TempDir(), Host: host, Log: &log,
		FlushConntrack: func() error { flushes++; return nil },
		DeleteConntrack: func(match func(ctEntry) bool) (int, error) {
			n := 0
			for i, f := range flows {
				if match(f) {
					ended = append(ended, i)
					n++
				}
			}
			return n, nil
		}}}
	vals := map[string]string{"PROBES": "192.0.2.1"}
	get := func(k string) string { return vals[k] }
	if err := in.loadTables(ctx, get); err != nil || flushes != 1 {
		t.Fatalf("the first load: %v, %d flushes", err, flushes)
	}
	mark := filepath.Join(in.O.Dir, TablesDir, "ingest.flush")
	flushes = 0
	vals["PROBES"] = "192.0.2.2"
	if err := in.loadTables(ctx, get); err != nil || flushes != 0 || !slices.Equal(ended, []int{0, 1}) || fileExists(mark) {
		t.Errorf("an element replaced: %v, %d flushes, ended %v, mark %v\n%s", err, flushes, ended, fileExists(mark), log.String())
	}
	if !strings.Contains(log.String(), "ended the flows of the 2 elements changed in services.ingest.sets (PROBES) (services.ingest.flush_conntrack): 2 connection-tracking entries deleted") {
		t.Errorf("not logged:\n%s", log.String())
	}
	// A change of the input nft finds the set holding already: nothing ended, nothing said.
	host.elems["probes"]["192.0.2.5"] = true
	vals["PROBES"], ended = "192.0.2.2, 192.0.2.5", nil
	log.Reset()
	if err := in.loadTables(ctx, get); err != nil || flushes != 0 || ended != nil || strings.Contains(log.String(), "ended the flows") || fileExists(mark) {
		t.Errorf("no element changed: %v, %d flushes, ended %v:\n%s", err, flushes, ended, log.String())
	}
	// Its type not matched to flows: the whole table flushes.
	host.types["probes"] = []string{"ether_addr"}
	vals["PROBES"], ended = "192.0.2.3", nil
	if err := in.loadTables(ctx, get); err != nil || flushes != 1 || ended != nil || fileExists(mark) {
		t.Errorf("another key type: %v, %d flushes, ended %v, mark %v", err, flushes, ended, fileExists(mark))
	}
	// A deletion that fails flushes the whole table.
	host.types["probes"] = []string{"ipv4_addr"}
	in.O.DeleteConntrack = func(func(ctEntry) bool) (int, error) { return 0, errors.New("operation not permitted") }
	flushes, vals["PROBES"] = 0, "192.0.2.4"
	if err := in.loadTables(ctx, get); err != nil || flushes != 1 || fileExists(mark) {
		t.Errorf("a deletion that fails: %v, %d flushes, mark %v", err, flushes, fileExists(mark))
	}
}

// A dump's entry of any bytes reads or is refused, never panics; one that reads deletes by
// attributes it holds.
func FuzzParseCTEntry(f *testing.F) {
	port := binary.BigEndian.AppendUint16(nil, 443)
	f.Add(slices.Concat(ctAttr(ctaTupleOrig|0x8000, ctAttr(ctaTupleIP|0x8000, ctAttr(ctaIPv4Src, []byte{192, 0, 2, 1})), ctAttr(ctaTupleProto|0x8000, ctAttr(ctaProtoDst, port))), ctAttr(ctaZone, port)))
	f.Add([]byte{4, 0, 1, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		c, ok, err := parseCTEntry(2, b)
		if ok && (err != nil || len(c.del) == 0 || !bytes.Contains(b, c.del[:4])) {
			t.Errorf("%x: %+v %v", b, c, err)
		}
	})
}

// A deletion nft refuses (an element gone already) leaves the additions made: their flows end,
// and the answer's 400 carries nft's refusal with them; a deletion that fails after some entries
// went says some flows may have ended, never that they are kept.
func TestARefusedDeletionEndsTheAdditionsFlows(t *testing.T) {
	t.Parallel()
	r := testRelease(t, "1.0.0")
	r.Nftables = map[string][]byte{"ingest": []byte(ingestTable)}
	r.Spec.Services = map[string]spec.Service{"ingest": {FlushConntrack: true}}
	host := &setHost{types: map[string][]string{"probes": {"ipv4_addr"}}, elems: map[string]map[string]bool{"probes": {"192.0.2.1": true}}, refuse: "delete"}
	flows := []ctEntry{entry("192.0.2.1", "198.51.100.1", 40000, 443), entry("192.0.2.2", "198.51.100.1", 40001, 443)}
	var ended []int
	deleteErr := error(nil)
	h := &Hostd{R: r, O: Options{Dir: t.TempDir(), Host: host, Log: io.Discard,
		DeleteConntrack: func(match func(ctEntry) bool) (int, error) {
			for i, f := range flows {
				if match(f) {
					ended = append(ended, i)
				}
			}
			return len(ended), deleteErr
		}}}
	post := func(body string) (int, SetElementsDone) {
		w := httptest.NewRecorder()
		h.routes(true).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/nftables/ingest/sets/probes", strings.NewReader(body)))
		var done SetElementsDone
		if err := json.Unmarshal(w.Body.Bytes(), &done); err != nil {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return w.Code, done
	}
	code, done := post(`{"add": ["192.0.2.2"], "delete": ["192.0.2.9"]}`)
	if code != http.StatusBadRequest || !strings.Contains(done.Error, "nft delete element inet ingest probes") || done.Added != 1 || done.Deleted != 0 || done.FlowsEnded == nil || *done.FlowsEnded != 1 {
		t.Errorf("%d %+v", code, done)
	}
	if !host.elems["probes"]["192.0.2.2"] || !slices.Equal(ended, []int{1}) {
		t.Errorf("the set holds %v, ended %v", host.elems["probes"], ended)
	}
	host.refuse, ended, deleteErr = "", nil, errors.New("no buffer space available")
	code, done = post(`{"delete": ["192.0.2.2"]}`)
	if code != http.StatusOK || !strings.Contains(done.Warning, "some may have ended (1 connection-tracking entries deleted)") || strings.Contains(done.Warning, "are kept, as they judged") || done.FlowsEnded == nil || *done.FlowsEnded != 1 {
		t.Errorf("a deletion failing midway: %d %+v", code, done)
	}
}

// A client gone once nft changed the set does not keep the flows of its change: they end.
func TestAClientGoneAfterTheChangeStillEndsItsFlows(t *testing.T) {
	t.Parallel()
	r := testRelease(t, "1.0.0")
	r.Nftables = map[string][]byte{"ingest": []byte(ingestTable)}
	r.Spec.Services = map[string]spec.Service{"ingest": {FlushConntrack: true}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := &setHost{types: map[string][]string{"probes": {"ipv4_addr"}}, elems: map[string]map[string]bool{"probes": {}}, changed: cancel}
	deletes := 0
	h := &Hostd{R: r, O: Options{Dir: t.TempDir(), Host: host, Log: io.Discard,
		DeleteConntrack: func(match func(ctEntry) bool) (int, error) {
			if match(entry("192.0.2.2", "198.51.100.1", 40000, 443)) {
				deletes++
			}
			return deletes, nil
		}}}
	w := httptest.NewRecorder()
	h.routes(true).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/nftables/ingest/sets/probes", strings.NewReader(`{"add": ["192.0.2.2"]}`)).WithContext(ctx))
	if deletes != 1 || strings.Contains(w.Body.String(), "warning") {
		t.Errorf("%d deletions: %s", deletes, w.Body)
	}
}

// An interval set's element nft merged with its neighbours counts only for what it adds or
// loses: 10.0.0.5 added beside 10.0.0.0-10.0.0.4, listed merged, ends .5's flows alone; a port
// range shrunk ends the flows of the ports it lost alone.
func TestAMergedRangeCountsOnlyForWhatChanged(t *testing.T) {
	t.Parallel()
	set := func(typ string, elems ...string) nftSet {
		s := nftSet{types: []string{typ}, elems: map[string]json.RawMessage{}}
		for _, e := range elems {
			s.elems[e] = json.RawMessage(e)
		}
		return s
	}
	m, n, err := changedFlows(set("ipv4_addr", `{"range":["10.0.0.0","10.0.0.4"]}`), set("ipv4_addr", `{"range":["10.0.0.0","10.0.0.5"]}`))
	if err != nil || n != 1 {
		t.Fatalf("%d changed: %v", n, err)
	}
	for addr, want := range map[string]bool{"10.0.0.3": false, "10.0.0.0": false, "10.0.0.5": true, "10.0.0.6": false} {
		if got := m(entry(addr, "198.51.100.1", 40000, 443)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
	m, n, err = changedFlows(set("inet_service", `{"range":[1000,2000]}`), set("inet_service", `{"range":[1000,1500]}`, `1800`))
	if err != nil || n != 2 {
		t.Fatalf("ports: %d changed: %v", n, err)
	}
	for port, want := range map[uint16]bool{1200: false, 1501: true, 1800: false, 2000: true, 2001: false} {
		if got := m(entry("192.0.2.1", "198.51.100.1", 40000, port)); got != want {
			t.Errorf("port %d: %v, want %v", port, got, want)
		}
	}
}
