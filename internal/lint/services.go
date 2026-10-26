package lint

import (
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"

	"github.com/norvexlabs/stowage/internal/spec"
)

// MaxServiceCPUs is the most whole cores a service is given (services.<name>.cpus).
const MaxServiceCPUs = 64

// serviceHost checks what a service asks of the host beyond its memory: its whole cores (an IRQ
// posture needs them), its nftables table (a file of the project holding its table alone), and
// its address of its own on a NIC the release names, from an input.
func (l *linter) serviceHost(at, name string, svc spec.Service) {
	if svc.CPUs != 0 && (svc.CPUs < 1 || svc.CPUs > MaxServiceCPUs) {
		l.errf(at+".cpus", "%d: whole cores, 1 to %d", svc.CPUs, MaxServiceCPUs)
	}
	switch svc.IRQ {
	case "":
	case spec.IRQOff, spec.IRQOn:
		if svc.CPUs == 0 {
			l.errf(at+".irq", "is about the service's cores: give it cpus")
		}
	default:
		l.errf(at+".irq", "%q: %s (the default: the NICs' interrupts off its cores) or %s", svc.IRQ, spec.IRQOff, spec.IRQOn)
	}
	if f := svc.Nftables; f != "" {
		l.nftTable(at+".nftables", name, f)
	}
	switch {
	case (svc.Interface == "") != (svc.AddressInput == ""):
		l.errf(at, "interface and address_input go together: the NIC its address is on, and the input giving it")
	case svc.Interface != "":
		var nic bool
		if l.s.Host != nil {
			i, ok := l.s.Host.Interfaces[svc.Interface]
			nic = ok && (i.Kind == "" || i.Kind == spec.InterfaceNIC)
		}
		if !nic {
			l.errf(at+".interface", "%q is not a NIC of host.interfaces", svc.Interface)
		}
		in, ok := l.s.Input(svc.AddressInput)
		switch {
		case !ok:
			l.errf(at+".address_input", "%s is not an input", svc.AddressInput)
		case in.Secret:
			l.errf(at+".address_input", "%s is secret: an address is written into the compose file", svc.AddressInput)
		case in.Default != "" && !strings.Contains(in.Default, "${"):
			if _, err := netip.ParseAddr(in.Default); err != nil {
				l.errf(at+".address_input", "%s's default %q is not an address", svc.AddressInput, in.Default)
			}
		}
		if l.compose != nil {
			if s, ok := l.compose.Services[name]; ok && s.NetworkMode != "" {
				l.errf(at+".interface", "%s runs with network_mode %s: its address of its own is a network beside the stack's", name, s.NetworkMode)
			}
		}
	}
}

// nftTable checks a service's nftables file: inside the project, holding one table, `table inet
// <service>`, and nothing beside it (no include, no flush of the ruleset or of another table): the
// host's firewall stays Stowage's. The OS bundle's build and the VM check its syntax with nft.
func (l *linter) nftTable(at, service, rel string) {
	path, err := spec.ProjectPath(l.root, rel)
	if err != nil {
		l.errf(at, "%v", err)
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		l.errf(at, "%v", err)
		return
	}
	if service == l.s.Name {
		l.errf(at, "the service %s has the project's name: its table inet %s would be the VM's own firewall's", service, service)
		return
	}
	if spec.NftKeyword(service) {
		l.errf(at, "the service's name %q is a word of nft's own: its table needs another name", service)
		return
	}
	if !spec.NftTableName.MatchString(service) {
		l.errf(at, "the service's name %q is no nftables table's: a letter, then letters, digits and _ - . /", service)
		return
	}
	if err := CheckNftTable(string(raw), service); err != nil {
		l.errf(at, "%s: %v", rel, err)
	} else if nftDropsInput(string(raw)) {
		l.warnf(at, "%s: a chain hooks the input with a drop policy: a base chain of any table judges every packet, so it drops what the VM's own firewall accepts (SSH, the stack's ports) unless it accepts them itself", rel)
	}
}

// nftTokens are an nftables file's tokens as nft lexes them where it matters here: a quoted
// string one token (its braces and # its own), a comment (# outside a string, to the line's end)
// none, a backslash before a newline a continuation, a brace one token, the rest words split at
// spaces, tabs, newlines, ; and braces. An unterminated string, and a character nft takes for junk
// (a carriage return, a form feed, a vertical tab), are errors.
func nftTokens(text string) ([]string, error) {
	var out []string
	word := strings.Builder{}
	flush := func() {
		if word.Len() > 0 {
			out = append(out, word.String())
			word.Reset()
		}
	}
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '"':
			flush()
			j := strings.IndexByte(text[i+1:], '"')
			if j < 0 {
				return nil, fmt.Errorf("a string is not closed")
			}
			out = append(out, text[i:i+j+2])
			i += j + 1
		case c == '#':
			flush()
			if j := strings.IndexByte(text[i:], '\n'); j >= 0 {
				i += j
			} else {
				i = len(text)
			}
		case c == '\\' && i+1 < len(text) && text[i+1] == '\n':
			flush()
			i++
		case c == '\r' || c == '\f' || c == '\v':
			return nil, fmt.Errorf("holds a carriage return, a form feed or a vertical tab, which nft refuses (save it with Unix line endings)")
		case c == '{' || c == '}':
			flush()
			out = append(out, string(c))
		case c == ';' || c == ' ' || c == '\t' || c == '\n':
			flush()
		default:
			word.WriteByte(c)
		}
	}
	flush()
	return out, nil
}

// CheckNftTable checks that an nftables file is one `table inet <name> { … }` alone, as nft lexes
// it (strings and comments aside): nothing before or after it, its braces balanced, no include nor
// a word naming the ruleset.
func CheckNftTable(text, name string) error {
	toks, err := nftTokens(text)
	if err != nil {
		return err
	}
	head := fmt.Sprintf("table inet %s {", name)
	if len(toks) < 4 || toks[0] != "table" || toks[1] != "inet" || toks[2] != name || toks[3] != "{" {
		return fmt.Errorf("does not start with %q", head)
	}
	depth := 0
	for i, t := range toks {
		switch t {
		case "include", "ruleset":
			return fmt.Errorf("holds an include or names the ruleset: the file is its table alone")
		case "{":
			depth++
		case "}":
			if depth--; depth == 0 && i != len(toks)-1 {
				return fmt.Errorf("holds something after its table: the file is %q alone", head+" … }")
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("its braces do not balance")
	}
	return nil
}

// nftDropsInput reports whether a table's chain hooks the input, or a hook before it (prerouting,
// ingress), with a drop policy: a base chain of any table judges every packet, so it drops what
// the host's own firewall accepts (SSH, the stack's ports) unless the chain accepts them itself.
func nftDropsInput(text string) bool {
	toks, err := nftTokens(text)
	if err != nil {
		return false
	}
	depth, hook, drop := 0, false, false
	for i, t := range toks {
		switch {
		case t == "{":
			if depth++; depth == 2 {
				hook, drop = false, false
			}
		case t == "}":
			if depth == 2 && hook && drop {
				return true
			}
			depth--
		case depth == 2 && t == "hook" && i+1 < len(toks) && slices.Contains([]string{"input", "prerouting", "ingress"}, toks[i+1]):
			hook = true
		case depth == 2 && t == "policy" && i+1 < len(toks) && toks[i+1] == "drop":
			drop = true
		}
	}
	return false
}
