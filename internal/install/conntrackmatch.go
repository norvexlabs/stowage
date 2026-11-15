package install

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
)

// A set's change under flush_conntrack ends the flows its changed elements may have judged: the
// connection-tracking entries matching an element one listing of the set holds and the other does
// not (added or deleted, as nft lists them before and after), the others kept. An element matches
// an entry by each part of its key: an address (ipv4_addr, ipv6_addr: one, a prefix or a range)
// any address of the entry's two tuples, a port (inet_service) any of their ports, a protocol
// (inet_proto) the entry's; a concatenation every part. Matching any end of the flow, not only
// the one the rules read, ends a few flows more than the rules judged by the element, never
// fewer: each meets the firewall again as a new one. A set of another key type is not matched:
// errUnmatched, and the caller flushes the whole table instead.

// errUnmatched is a set whose key type an element cannot be matched to a flow by.
var errUnmatched = errors.New("its key type is not matched to flows")

// ctEntry is one connection-tracking entry as a change of a set reads it: its protocol, the
// addresses and ports of its original and reply tuples (src, dst, src, dst; ports zero without),
// and the attributes that delete it (its original tuple and zone, as the kernel gave them).
type ctEntry struct {
	family   uint8
	proto    uint8
	addrs    [4]netip.Addr
	ports    [4]uint16
	hasPorts bool
	del      []byte
}

// nftSet is a set as `nft -j list set` gives it: its key's types (one each part) and its
// elements, each by its JSON value (without its timeout or expiry), keyed by that value's text.
type nftSet struct {
	types []string
	elems map[string]json.RawMessage
}

// listSet lists the set of the table inet table.
func listSet(ctx context.Context, host Tool, table, set string) (nftSet, error) {
	out, err := host.Run(ctx, Cmd{Args: []string{"nft", "-j", "-n", "list", "set", "inet", table, set}})
	if err != nil {
		return nftSet{}, fmt.Errorf("nft list set inet %s %s: %w", table, set, err)
	}
	return parseNftSet(out)
}

// parseNftSet reads `nft -j list set`'s answer.
func parseNftSet(out []byte) (nftSet, error) {
	var doc struct {
		Nftables []struct {
			Set *struct {
				Type json.RawMessage   `json:"type"`
				Elem []json.RawMessage `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nftSet{}, fmt.Errorf("nft's listing of the set: %w", err)
	}
	for _, o := range doc.Nftables {
		if o.Set == nil {
			continue
		}
		s := nftSet{elems: map[string]json.RawMessage{}}
		var one string
		if err := json.Unmarshal(o.Set.Type, &one); err == nil {
			s.types = []string{one}
		} else if err := json.Unmarshal(o.Set.Type, &s.types); err != nil || len(s.types) == 0 {
			return nftSet{}, fmt.Errorf("nft's listing of the set: its type %s", o.Set.Type)
		}
		for _, e := range o.Set.Elem {
			var wrapped struct {
				Elem *struct {
					Val json.RawMessage `json:"val"`
				} `json:"elem"`
			}
			if json.Unmarshal(e, &wrapped) == nil && wrapped.Elem != nil {
				e = wrapped.Elem.Val
			}
			var c bytes.Buffer
			if err := json.Compact(&c, e); err != nil {
				return nftSet{}, fmt.Errorf("nft's listing of the set: %w", err)
			}
			s.elems[c.String()] = e
		}
		return s, nil
	}
	return nftSet{}, errors.New("nft's listing holds no set")
}

// changedFlows is the match of the entries the change of a set from before to after may have
// judged (one element held by one listing and not the other), and how many elements changed;
// errUnmatched when the set's key type is not matched to flows.
func changedFlows(before, after nftSet) (func(ctEntry) bool, int, error) {
	if !slices.Equal(before.types, after.types) {
		return nil, 0, fmt.Errorf("the set's type changed from %v to %v", before.types, after.types)
	}
	if len(after.types) == 1 {
		if m, n, ok, err := changedSpans(after.types[0], before, after); ok || err != nil {
			return m, n, err
		}
	}
	var changed []json.RawMessage
	for _, k := range slices.Sorted(maps.Keys(before.elems)) {
		if _, ok := after.elems[k]; !ok {
			changed = append(changed, before.elems[k])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(after.elems)) {
		if _, ok := before.elems[k]; !ok {
			changed = append(changed, after.elems[k])
		}
	}
	var matchers []func(ctEntry) bool
	for _, e := range changed {
		m, err := elementMatch(after.types, e)
		if err != nil {
			return nil, 0, err
		}
		matchers = append(matchers, m)
	}
	return func(c ctEntry) bool {
		return slices.ContainsFunc(matchers, func(m func(ctEntry) bool) bool { return m(c) })
	}, len(changed), nil
}

// span is a range of values, both ends in it.
type span[T any] struct{ lo, hi T }

// subtract is what of a's spans no span of b covers, by cmp; next and prev are the values beside
// one.
func subtract[T any](a, b []span[T], cmp func(x, y T) int, next, prev func(T) T) []span[T] {
	out := slices.Clone(a)
	for _, cut := range b {
		var kept []span[T]
		for _, s := range out {
			if cmp(cut.hi, s.lo) < 0 || cmp(cut.lo, s.hi) > 0 {
				kept = append(kept, s)
				continue
			}
			if cmp(cut.lo, s.lo) > 0 {
				kept = append(kept, span[T]{s.lo, prev(cut.lo)})
			}
			if cmp(cut.hi, s.hi) < 0 {
				kept = append(kept, span[T]{next(cut.hi), s.hi})
			}
		}
		out = kept
	}
	return out
}

// changedSpans is changedFlows for a set of one key type of addresses, ports or protocols: what
// the listings cover, compared as ranges, so an element nft merged with its neighbours (an
// interval set's auto-merge) counts only for what it adds or loses: the values one listing
// covers and the other not. ok is false for another type.
func changedSpans(typ string, before, after nftSet) (func(ctEntry) bool, int, bool, error) {
	switch typ {
	case "ipv4_addr", "ipv6_addr":
		spans := func(s nftSet) ([]span[netip.Addr], error) {
			var out []span[netip.Addr]
			for _, e := range s.elems {
				lo, hi, err := addrBounds(e)
				if err != nil {
					return nil, err
				}
				if lo.Is4() != (typ == "ipv4_addr") {
					return nil, fmt.Errorf("%s is no %s", e, typ)
				}
				out = append(out, span[netip.Addr]{lo, hi})
			}
			return out, nil
		}
		b, err := spans(before)
		if err != nil {
			return nil, 0, true, err
		}
		a, err := spans(after)
		if err != nil {
			return nil, 0, true, err
		}
		cmp := func(x, y netip.Addr) int { return x.Compare(y) }
		next, prev := func(x netip.Addr) netip.Addr { return x.Next() }, func(x netip.Addr) netip.Addr { return x.Prev() }
		diff := append(subtract(b, a, cmp, next, prev), subtract(a, b, cmp, next, prev)...)
		return func(c ctEntry) bool {
			return slices.ContainsFunc(diff, func(d span[netip.Addr]) bool { return addrIn(c, d.lo, d.hi) })
		}, len(diff), true, nil
	case "inet_service", "inet_proto":
		max, names := uint64(0xffff), map[string]uint64(nil)
		if typ == "inet_proto" {
			max, names = 0xff, protoNumbers
		}
		spans := func(s nftSet) ([]span[uint64], error) {
			var out []span[uint64]
			for _, e := range s.elems {
				lo, hi, err := numBounds(e, max, names)
				if err != nil {
					return nil, err
				}
				out = append(out, span[uint64]{lo, hi})
			}
			return out, nil
		}
		b, err := spans(before)
		if err != nil {
			return nil, 0, true, err
		}
		a, err := spans(after)
		if err != nil {
			return nil, 0, true, err
		}
		cmp := func(x, y uint64) int { return cmpUint(x, y) }
		next, prev := func(x uint64) uint64 { return x + 1 }, func(x uint64) uint64 { return x - 1 }
		diff := append(subtract(b, a, cmp, next, prev), subtract(a, b, cmp, next, prev)...)
		return func(c ctEntry) bool {
			return slices.ContainsFunc(diff, func(d span[uint64]) bool {
				if typ == "inet_proto" {
					return uint64(c.proto) >= d.lo && uint64(c.proto) <= d.hi
				}
				return portIn(c, d.lo, d.hi)
			})
		}, len(diff), true, nil
	}
	return nil, 0, false, nil
}

func cmpUint(x, y uint64) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// addrIn reports whether an address of c's tuples lies from lo to hi.
func addrIn(c ctEntry, lo, hi netip.Addr) bool {
	return slices.ContainsFunc(c.addrs[:], func(a netip.Addr) bool {
		a = a.Unmap()
		return a.IsValid() && a.BitLen() == lo.BitLen() && lo.Compare(a) <= 0 && a.Compare(hi) <= 0
	})
}

// portIn reports whether a port of c's tuples lies from lo to hi.
func portIn(c ctEntry, lo, hi uint64) bool {
	return c.hasPorts && slices.ContainsFunc(c.ports[:], func(port uint16) bool { return uint64(port) >= lo && uint64(port) <= hi })
}

// elementMatch is the match of an element of a set of key types (one each part).
func elementMatch(types []string, e json.RawMessage) (func(ctEntry) bool, error) {
	parts := []json.RawMessage{e}
	var concat struct {
		Concat []json.RawMessage `json:"concat"`
	}
	if json.Unmarshal(e, &concat) == nil && concat.Concat != nil {
		if len(types) < 2 {
			return nil, fmt.Errorf("element %s: a concatenation in a set of the type %v", e, types)
		}
		parts = concat.Concat
	}
	if len(parts) != len(types) {
		return nil, fmt.Errorf("element %s: %d parts for the type %v", e, len(parts), types)
	}
	var ms []func(ctEntry) bool
	for i, p := range parts {
		m, err := partMatch(types[i], p)
		if err != nil {
			return nil, fmt.Errorf("element %s: %w", e, err)
		}
		ms = append(ms, m)
	}
	return func(c ctEntry) bool {
		for _, m := range ms {
			if !m(c) {
				return false
			}
		}
		return true
	}, nil
}

// partMatch is the match of one part of an element by its type: a single value, a prefix or a
// range of it.
func partMatch(typ string, p json.RawMessage) (func(ctEntry) bool, error) {
	switch typ {
	case "ipv4_addr", "ipv6_addr":
		lo, hi, err := addrBounds(p)
		if err != nil {
			return nil, err
		}
		if lo.Is4() != (typ == "ipv4_addr") {
			return nil, fmt.Errorf("%s is no %s", p, typ)
		}
		return func(c ctEntry) bool { return addrIn(c, lo, hi) }, nil
	case "inet_service":
		lo, hi, err := numBounds(p, 0xffff, nil)
		if err != nil {
			return nil, err
		}
		return func(c ctEntry) bool { return portIn(c, lo, hi) }, nil
	case "inet_proto":
		lo, hi, err := numBounds(p, 0xff, protoNumbers)
		if err != nil {
			return nil, err
		}
		return func(c ctEntry) bool { return uint64(c.proto) >= lo && uint64(c.proto) <= hi }, nil
	}
	return nil, fmt.Errorf("type %s: %w", typ, errUnmatched)
}

// addrBounds are the first and last address of an address, a prefix or a range.
func addrBounds(p json.RawMessage) (netip.Addr, netip.Addr, error) {
	var s string
	if json.Unmarshal(p, &s) == nil {
		a, err := netip.ParseAddr(s)
		return a.Unmap(), a.Unmap(), err
	}
	var v struct {
		Prefix *struct {
			Addr string `json:"addr"`
			Len  int    `json:"len"`
		} `json:"prefix"`
		Range []string `json:"range"`
	}
	if err := json.Unmarshal(p, &v); err != nil {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("address %s: %w", p, err)
	}
	switch {
	case v.Prefix != nil:
		a, err := netip.ParseAddr(v.Prefix.Addr)
		if err != nil {
			return netip.Addr{}, netip.Addr{}, err
		}
		pfx, err := a.Unmap().Prefix(v.Prefix.Len)
		if err != nil {
			return netip.Addr{}, netip.Addr{}, err
		}
		return pfx.Addr(), lastAddr(pfx), nil
	case len(v.Range) == 2:
		lo, err := netip.ParseAddr(v.Range[0])
		if err != nil {
			return netip.Addr{}, netip.Addr{}, err
		}
		hi, err := netip.ParseAddr(v.Range[1])
		if err != nil {
			return netip.Addr{}, netip.Addr{}, err
		}
		if lo, hi = lo.Unmap(), hi.Unmap(); lo.BitLen() != hi.BitLen() || hi.Less(lo) {
			return netip.Addr{}, netip.Addr{}, fmt.Errorf("address range %s", p)
		}
		return lo, hi, nil
	}
	return netip.Addr{}, netip.Addr{}, fmt.Errorf("address %s: %w", p, errUnmatched)
}

// lastAddr is the last address of a prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// protoNumbers are the protocols nft prints by name in an inet_proto set (its proto_tbl, from
// /etc/protocols' names), by number.
var protoNumbers = map[string]uint64{
	"icmp": 1, "igmp": 2, "ggp": 3, "ipencap": 4, "st": 5, "tcp": 6, "egp": 8, "igp": 9, "pup": 12,
	"udp": 17, "hmp": 20, "xns-idp": 22, "rdp": 27, "iso-tp4": 29, "dccp": 33, "xtp": 36,
	"ddp": 37, "idpr-cmtp": 38, "ipv6": 41, "ipv6-route": 43, "ipv6-frag": 44, "idrp": 45,
	"rsvp": 46, "gre": 47, "esp": 50, "ah": 51, "skip": 57, "ipv6-icmp": 58, "icmpv6": 58,
	"ipv6-nonxt": 59, "ipv6-opts": 60, "rspf": 73, "vmtp": 81, "eigrp": 88, "ospf": 89,
	"ax.25": 93, "ipip": 94, "etherip": 97, "encap": 98, "pim": 103, "ipcomp": 108, "vrrp": 112,
	"l2tp": 115, "isis": 124, "sctp": 132, "fc": 133, "mobility-header": 135, "udplite": 136,
	"mpls-in-ip": 137, "manet": 138, "hip": 139, "shim6": 140, "wesp": 141, "rohc": 142,
	"ethernet": 143,
}

// numBounds are the first and last value of a number or a range of numbers, at most max; a name
// is looked up in names, and one it does not hold (a service's that nft did not print
// numerically) is not matched.
func numBounds(p json.RawMessage, max uint64, names map[string]uint64) (uint64, uint64, error) {
	one := func(r json.RawMessage) (uint64, error) {
		var s string
		if json.Unmarshal(r, &s) == nil {
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				named, ok := names[s]
				if !ok {
					return 0, fmt.Errorf("%q: %w", s, errUnmatched)
				}
				n = named
			}
			r = json.RawMessage(strconv.FormatUint(n, 10))
		}
		var n uint64
		if err := json.Unmarshal(r, &n); err != nil || n > max {
			return 0, fmt.Errorf("%s is no value of 0 to %d", r, max)
		}
		return n, nil
	}
	var v struct {
		Range []json.RawMessage `json:"range"`
	}
	if json.Unmarshal(p, &v) == nil && len(v.Range) == 2 {
		lo, err := one(v.Range[0])
		if err != nil {
			return 0, 0, err
		}
		hi, err := one(v.Range[1])
		if err != nil {
			return 0, 0, err
		}
		if hi < lo {
			return 0, 0, fmt.Errorf("range %s", p)
		}
		return lo, hi, nil
	}
	n, err := one(p)
	return n, n, err
}

// ctnetlink's attributes (linux/netfilter/nfnetlink_conntrack.h) an entry is read and deleted by.
const (
	ctaTupleOrig  = 1
	ctaTupleReply = 2
	ctaZone       = 18
	ctaTupleIP    = 1
	ctaTupleProto = 2
	ctaIPv4Src    = 1
	ctaIPv4Dst    = 2
	ctaIPv6Src    = 3
	ctaIPv6Dst    = 4
	ctaProtoNum   = 1
	ctaProtoSrc   = 2
	ctaProtoDst   = 3
	nlaTypeMask   = 0x3fff // a type without its nested and byte-order flags
)

// nlAttr is one netlink attribute: its type and payload, and its whole bytes (header and padding
// included), which another message carries as they are.
type nlAttr struct {
	typ  uint16
	data []byte
	raw  []byte
}

// parseAttrs splits a netlink attribute stream, each attribute 4-byte aligned.
func parseAttrs(b []byte) ([]nlAttr, error) {
	var out []nlAttr
	for len(b) >= 4 {
		l := int(binary.NativeEndian.Uint16(b))
		if l < 4 || l > len(b) {
			return nil, fmt.Errorf("a netlink attribute of %d bytes in %d", l, len(b))
		}
		next := min((l+3)&^3, len(b))
		out = append(out, nlAttr{typ: binary.NativeEndian.Uint16(b[2:]) & nlaTypeMask, data: b[4:l], raw: b[:next]})
		b = b[next:]
	}
	return out, nil
}

// parseCTEntry reads one entry of a ctnetlink dump (the message's payload after its nfgenmsg) of
// the family: false when it has no original tuple.
func parseCTEntry(family uint8, b []byte) (ctEntry, bool, error) {
	attrs, err := parseAttrs(b)
	if err != nil {
		return ctEntry{}, false, err
	}
	c := ctEntry{family: family}
	var orig, zone []byte
	for _, a := range attrs {
		switch a.typ {
		case ctaTupleOrig, ctaTupleReply:
			at := 0
			if a.typ == ctaTupleReply {
				at = 2
			} else {
				orig = a.raw
			}
			if err := c.readTuple(a.data, at); err != nil {
				return ctEntry{}, false, err
			}
		case ctaZone:
			zone = a.raw
		}
	}
	if orig == nil {
		return ctEntry{}, false, nil
	}
	c.del = append(slices.Clone(orig), zone...)
	return c, true, nil
}

// readTuple reads a tuple's addresses and ports into c at at (0: the original's, 2: the reply's).
func (c *ctEntry) readTuple(b []byte, at int) error {
	attrs, err := parseAttrs(b)
	if err != nil {
		return err
	}
	for _, a := range attrs {
		inner, err := parseAttrs(a.data)
		if err != nil {
			return err
		}
		for _, f := range inner {
			switch {
			case a.typ == ctaTupleIP && (f.typ == ctaIPv4Src || f.typ == ctaIPv4Dst || f.typ == ctaIPv6Src || f.typ == ctaIPv6Dst):
				addr, ok := netip.AddrFromSlice(f.data)
				if !ok {
					return fmt.Errorf("an address of %d bytes", len(f.data))
				}
				c.addrs[at+int((f.typ-1)%2)] = addr.Unmap()
			case a.typ == ctaTupleProto && f.typ == ctaProtoNum && len(f.data) == 1:
				c.proto = f.data[0]
			case a.typ == ctaTupleProto && (f.typ == ctaProtoSrc || f.typ == ctaProtoDst) && len(f.data) == 2:
				c.ports[at+int(f.typ-ctaProtoSrc)] = binary.BigEndian.Uint16(f.data)
				c.hasPorts = true
			}
		}
	}
	return nil
}
