package install

import (
	"cmp"
	"fmt"
	"strings"
)

// The least VM the seed's size allows, whatever the release states: Kairos and the stack's engine
// need as much memory (spec.MinVMMemory), and the app as much space (spec.MinVMDisk) besides what
// Kairos takes of the disk, which each disk's disk_gib adds.
const (
	minVMMiB = 1024
	minVMGiB = 4
)

// vmCount is a VM's size in the seed's units (CPUs, MiB, GiB of the app's); 0 is unstated.
type vmCount struct{ cpus, mib, gib int }

// vmSizes is the size of a VM of the release's Kairos shape, as vm seed gives it (size) and vm
// create's steps state it: the least (the floor, raised by shapes.kairos.size.min), the default
// (size.recommended, else 2 CPUs, 4 GiB and 20 GiB of the app's, raised to the least), and the
// size's min and rec as stated.
// A disk's size is the app's space: Kairos's is added to it (bootDisk.diskGiB).
type vmSizes struct{ least, def, min, rec vmCount }

func (in *Installer) vmSize() vmSizes {
	var lo, rec vmCount
	if k := in.R.Spec.Shapes.Kairos; k != nil && k.Size != nil {
		if m := k.Size.Min; m != nil {
			lo = vmCount{m.CPUs, int(m.Memory.MiB()), int(m.Disk >> 30)}
		}
		if m := k.Size.Recommended; m != nil {
			rec = vmCount{m.CPUs, int(m.Memory.MiB()), int(m.Disk >> 30)}
		}
	}
	z := vmSizes{least: vmCount{max(lo.cpus, 1), max(lo.mib, minVMMiB), max(lo.gib, minVMGiB)}, min: lo, rec: rec}
	z.def = vmCount{max(cmp.Or(rec.cpus, 2), z.least.cpus), max(cmp.Or(rec.mib, 4096), z.least.mib), max(cmp.Or(rec.gib, 20), z.least.gib)}
	return z
}

// stated renders the sizes c states, "" when it states none.
func (c vmCount) stated() string {
	var out []string
	if c.cpus != 0 {
		out = append(out, fmt.Sprintf("%d CPUs", c.cpus))
	}
	if c.mib != 0 {
		out = append(out, fmt.Sprintf("%d MiB", c.mib))
	}
	if c.gib != 0 {
		out = append(out, fmt.Sprintf("%d GiB for the app", c.gib))
	}
	return strings.Join(out, ", ")
}

// note is what the release states of its size, for an operator who sizes the VM by hand (vm
// create's steps): "" when it states none.
func (z vmSizes) note(name string) string {
	var out []string
	if lo := z.min.stated(); lo != "" {
		out = append(out, "at least "+lo)
	}
	if rec := z.rec.stated(); rec != "" {
		out = append(out, rec+" recommended")
	}
	if len(out) == 0 {
		return ""
	}
	return fmt.Sprintf("%s runs on %s", name, strings.Join(out, "; "))
}
