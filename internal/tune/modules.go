package tune

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// Module is a kernel module the project declares (shapes.kairos.modules): loaded at boot,
// blocked, or given options, and why.
type Module struct {
	Name        string
	Load, Block bool
	Options     map[string]string
	Reason      string
}

// inspectModules judges each module the project declares on the running kernel. On a host Stowage
// owns (the VM, whose image carries the modules' files) what differs is drift; on another, whose
// kernel is its operator's, advice: nothing is set there.
func (s System) inspectModules() []Item {
	var items []Item
	for _, m := range s.Modules {
		loaded := s.exists(filepath.Join("sys/module", m.Name))
		add := func(ok bool, name, have, want string) {
			if !ok && !s.Owned {
				items = append(items, Item{Advice, "module", name, fmt.Sprintf("%s, the project asks for %s: this host's kernel is its operator's", have, want), ""})
				return
			}
			items = append(items, Item{verdict(ok), "module", name, have, want})
		}
		state := map[bool]string{true: "loaded", false: "not loaded"}[loaded]
		why := " (" + m.Reason + ")"
		switch {
		case m.Load:
			add(loaded, m.Name, state, "loaded"+why)
		case m.Block:
			add(!loaded, m.Name, state, "blocked"+why)
		}
		for _, o := range slices.Sorted(maps.Keys(m.Options)) {
			name, want := m.Name+" "+o, m.Options[o]
			if !loaded {
				items = append(items, Item{NA, "module", name, m.Name + " is not loaded:", "its options apply when it loads"})
				continue
			}
			have, ok := s.read(filepath.Join("sys/module", m.Name, "parameters", o))
			if !ok {
				items = append(items, Item{NA, "module", name, "not readable:", "the module does not show it"})
				continue
			}
			add(sameParam(have, want), name, have, want+why)
		}
	}
	return items
}

// sameParam: a module parameter the kernel shows as have holds want. A boolean shows as Y or N,
// and takes what the kernel's kstrtobool reads: y, t, 1 or on; n, f, 0 or off.
func sameParam(have, want string) bool {
	if have != "Y" && have != "N" {
		return have == want
	}
	w := strings.ToLower(want)
	switch {
	case w == "":
		return false
	case strings.ContainsRune("yt1", rune(w[0])) || strings.HasPrefix(w, "on"):
		return have == "Y"
	case strings.ContainsRune("nf0", rune(w[0])) || strings.HasPrefix(w, "of"):
		return have == "N"
	}
	return false
}
