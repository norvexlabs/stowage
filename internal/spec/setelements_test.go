package spec

import (
	"strings"
	"testing"
)

// An input fills a set with MaxSetElements elements at most: the cap tested on both sides.
func TestSetElementsCap(t *testing.T) {
	t.Parallel()
	at := strings.TrimSuffix(strings.Repeat("192.0.2.1,", MaxSetElements), ",")
	if e, err := SetElements(at); err != nil || len(e) != MaxSetElements {
		t.Errorf("%d elements: %v", len(e), err)
	}
	if _, err := SetElements(at + ",192.0.2.2"); err == nil || !strings.Contains(err.Error(), "1025 elements: at most 1024") {
		t.Errorf("one more: %v", err)
	}
	if e, err := SetElements(" 10.0.0.1 . 443 , ,198.51.100.0/24"); err != nil || len(e) != 2 || e[0] != "10.0.0.1 . 443" {
		t.Errorf("%q %v", e, err)
	}
}

// An element is written as nft takes it without resolving a name: an address, a prefix, a port, a
// range of two of one kind; a host or service name (resolved as nft loads it, at boot before the
// network) and a mixed or open range are refused.
func TestSetElementsAreLiterals(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"192.0.2.1", "2001:db8::1", "198.51.100.0/24", "0", "65535", "1000-2000", "192.0.2.1-192.0.2.9", "10.0.0.1 . 443", "fe80::1-fe80::9", "2001:db8:aaaa:bbbb:cccc:dddd:eeee:1-2001:db8:aaaa:bbbb:cccc:dddd:eeee:9", "1000-1000", "1 . 2 . 3 . 4 . 5"} {
		if _, err := SetElements(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"probe.example.net", "dead.beef", "http", "65536", "01", "192.0.2.1-80", "10.0.0.0/8-10.1.0.0/16", "1-", "fe80::1%eth0", "10.0.0.1 . ssh", "2000-1000", "192.0.2.9-192.0.2.1", "192.0.2.1-2001:db8::1"} {
		if _, err := SetElements(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// Whatever an operator types, an element SetElements returns is one nft reads as an element
// alone: no brace, semicolon, quote, line break or other word an nft script would take.
func FuzzSetElements(f *testing.F) {
	for _, s := range []string{"192.0.2.1, 198.51.100.0/24", "10.0.0.1 . 443", "1.2.3.4 }; flush ruleset; {", "a\nb", "fe80::1-fe80::9", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		elems, err := SetElements(v)
		if err != nil {
			return
		}
		for _, e := range elems {
			if e == "" || strings.ContainsAny(e, "{};\"'\n\r\t#$@,") || strings.TrimSpace(e) != e {
				t.Fatalf("%q gave the element %q", v, e)
			}
			for _, part := range strings.Split(e, " . ") {
				if part == "" || strings.Contains(part, " ") {
					t.Fatalf("%q gave the element %q", v, e)
				}
			}
		}
	})
}

// A set is found wherever nft takes its declaration: on a line of its own, after a brace or a
// semicolon on one line; a word ending in "set", a comment and a quoted string hold none.
func TestNftSets(t *testing.T) {
	t.Parallel()
	text := "table inet t { set allow-list.v4 { }; set a { type ipv4_addr; }; set b { type ipv4_addr; }set e { }\n\tset c {\n\t}\n\t# set legacy {\n\tchain x { meta mark set 1; comment \"x; set quoted {\"; } offset d { }\n}\n"
	if got := NftSets(text); strings.Join(got, " ") != "allow-list.v4 a b e c" {
		t.Errorf("%q", got)
	}
}
