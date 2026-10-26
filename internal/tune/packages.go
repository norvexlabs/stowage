package tune

import (
	"bufio"
	"os"
	"strings"
)

// Package is a distribution package the project declares (host.packages), and why.
type Package struct {
	Name   string
	Reason string
}

// dpkgStatus is dpkg's database of the installed packages, relative to the root.
const dpkgStatus = "var/lib/dpkg/status"

// inspectPackages judges each package the project declares against dpkg's database. On a host
// Stowage owns (the VM, whose image installs them) one missing is drift; on another, advice: its
// packages are its operator's. A host without dpkg names the package to install with its own
// manager.
func (s System) inspectPackages() []Item {
	if len(s.Packages) == 0 {
		return nil
	}
	installed, ok := s.dpkgInstalled()
	var items []Item
	for _, p := range s.Packages {
		why := " (" + p.Reason + ")"
		switch {
		case !ok:
			items = append(items, Item{NA, "package", p.Name, "no dpkg database:", "the project asks for Debian's " + p.Name + why + ": its counterpart, with this host's package manager"})
		case installed[p.Name]:
			items = append(items, Item{OK, "package", p.Name, "installed", "installed" + why})
		case s.Owned:
			items = append(items, Item{Differs, "package", p.Name, "not installed", "installed" + why})
		default:
			items = append(items, Item{Advice, "package", p.Name, "not installed, the project asks for it" + why + ":", "apt-get install " + p.Name})
		}
	}
	return items
}

// dpkgInstalled are the packages dpkg's database holds as installed; false without one.
func (s System) dpkgInstalled() (map[string]bool, bool) {
	f, err := os.Open(s.path(dpkgStatus))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	out := map[string]bool{}
	var name string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			name = ""
		case strings.HasPrefix(line, "Package: "):
			name = strings.TrimSpace(strings.TrimPrefix(line, "Package: "))
		case strings.HasPrefix(line, "Status: ") && name != "":
			// want flag, error flag, status: "install ok installed".
			if f := strings.Fields(strings.TrimPrefix(line, "Status: ")); len(f) == 3 && f[2] == "installed" {
				out[name] = true
			}
		}
	}
	return out, sc.Err() == nil
}
