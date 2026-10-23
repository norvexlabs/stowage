package install

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

// sized gives v's release the size shapes.kairos.size states.
func (v *vmCreate) sized(lo, rec *spec.Machine) {
	k := *v.r.Spec.Shapes.Kairos
	k.Size = &spec.VMSize{Min: lo, Recommended: rec}
	v.r.Spec.Shapes.Kairos = &k
}

var (
	leastNotes = &spec.Machine{CPUs: 2, Memory: 2 << 30, Disk: 20 << 30}
	recNotes   = &spec.Machine{CPUs: 4, Memory: 8 << 30, Disk: 60 << 30}
)

// The recommended size is the default, the least the least taken, and under the recommendation
// warned.
func TestVMCreateSizesFromTheRelease(t *testing.T) {
	t.Parallel()
	vars := func(r *tofuRun) tofuVars {
		t.Helper()
		var v tofuVars
		b, err := os.ReadFile(filepath.Join(r.state, "notes", "vms", "notes-1", "terraform.tfvars.json"))
		if err == nil {
			err = json.Unmarshal(b, &v)
		}
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	v := newVMCreate(t, "qcow2")
	v.sized(leastNotes, recNotes)
	r := newTofuRun(t)
	if err := v.create(tofuCreate(), r.mod(nil)); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if got := vars(r); got.CPUs != 4 || got.MemoryMiB != 8192 || got.DiskGiB != 60 {
		t.Errorf("defaults %+v", got)
	}
	if strings.Contains(v.log.String(), "recommends") {
		t.Errorf("warned at the recommendation:\n%s", v.log)
	}

	v = newVMCreate(t, "qcow2")
	v.sized(leastNotes, recNotes)
	r = newTofuRun(t)
	c := tofuCreate()
	c.CPUs, c.MemoryMiB, c.DiskGiB = 3, 2048, 20 // one CPU under the recommendation, the rest the least
	if err := v.create(c, r.mod(nil)); err != nil {
		t.Fatalf("the least: %v\n%s", err, v.log)
	}
	for _, s := range []string{"3 CPUs: under the 4 notes recommends", "2048 MiB of memory: under the 8192 notes recommends", "20 GiB of disk: under the 60 notes recommends"} {
		if !strings.Contains(v.log.String(), s) {
			t.Errorf("no %q in:\n%s", s, v.log)
		}
	}

	for _, under := range []func(*VMCreate){
		func(c *VMCreate) { c.CPUs = 1 },
		func(c *VMCreate) { c.MemoryMiB = 2047 },
		func(c *VMCreate) { c.DiskGiB = 19 },
	} {
		v := newVMCreate(t, "qcow2")
		v.sized(leastNotes, recNotes)
		c := tofuCreate()
		under(&c)
		if err := v.create(c, newTofuRun(t).mod(nil)); err == nil || !strings.Contains(err.Error(), "the least notes runs on") {
			t.Errorf("%+v: %v", c.TofuVM, err)
		}
	}
}

// A size the release states alone raises the built-in defaults only to its least.
func TestVMSizeDefaults(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		lo, rec    *spec.Machine
		least, def vmCount
	}{
		{nil, nil, vmCount{1, 1024, 16}, vmCount{2, 4096, 40}},
		{&spec.Machine{CPUs: 3}, nil, vmCount{3, 1024, 16}, vmCount{3, 4096, 40}},
		{&spec.Machine{Memory: 6 << 30, Disk: 50 << 30}, nil, vmCount{1, 6144, 50}, vmCount{2, 6144, 50}},
		{nil, &spec.Machine{CPUs: 1, Memory: 2 << 30}, vmCount{1, 1024, 16}, vmCount{1, 2048, 40}},
	} {
		in := &Installer{R: &Release{Spec: &spec.Spec{Name: "notes", Shapes: spec.Shapes{Kairos: &spec.KairosShape{Size: &spec.VMSize{Min: c.lo, Recommended: c.rec}}}}}}
		if z := in.vmSize(); z.least != c.least || z.def != c.def {
			t.Errorf("%+v %+v: least %+v default %+v", c.lo, c.rec, z.least, z.def)
		}
	}
	if z := (&Installer{R: &Release{Spec: &spec.Spec{Shapes: spec.Shapes{Kairos: &spec.KairosShape{}}}}}).vmSize(); z.note("notes") != "" {
		t.Errorf("no size stated: %q", z.note("notes"))
	}
}

// A module that sizes the VM by its own variables is told the size it cannot check.
func TestVMSizeNoteForAModuleOfItsOwn(t *testing.T) {
	t.Parallel()
	in := &Installer{R: &Release{Spec: &spec.Spec{Shapes: spec.Shapes{Kairos: &spec.KairosShape{Size: &spec.VMSize{Min: leastNotes, Recommended: recNotes}}}}}}
	var log strings.Builder
	logf := func(f string, a ...any) { fmt.Fprintf(&log, f+"\n", a...) }
	in.vmSize().warn(logf, "notes", TofuVM{DiskGiB: 60}, false, false)
	if want := "the module sizes the VM by its own variables: notes runs on at least 2 CPUs, 2048 MiB, a 20 GiB disk; 4 CPUs, 8192 MiB, a 60 GiB disk recommended\n"; log.String() != want {
		t.Errorf("%q", log.String())
	}
	log.Reset()
	in.vmSize().warn(logf, "notes", TofuVM{CPUs: 4, MemoryMiB: 8192, DiskGiB: 60}, true, true)
	if log.Len() != 0 {
		t.Errorf("%q", log.String())
	}
}

// By hand, the steps say the size.
func TestVMCreateByHandSaysTheSize(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.sized(leastNotes, recNotes)
	if err := v.create(VMCreate{Platform: "proxmox"}, nil); err != nil {
		t.Fatal(err)
	}
	if want := "  Size the VM: notes runs on at least 2 CPUs, 2048 MiB, a 20 GiB disk; 4 CPUs, 8192 MiB, a 60 GiB disk recommended.\n"; !strings.Contains(v.out2(), want) {
		t.Errorf("no %q in:\n%s", want, v.out2())
	}
	v = newVMCreate(t, "qcow2")
	if err := v.create(VMCreate{Platform: "proxmox"}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(v.out2(), "Size the VM") {
		t.Errorf("a size no release states:\n%s", v.out2())
	}
}
