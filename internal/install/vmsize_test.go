package install

import (
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

// A size the release states alone raises the built-in defaults only to its least.
func TestVMSizeDefaults(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		lo, rec    *spec.Machine
		least, def vmCount
	}{
		{nil, nil, vmCount{1, 1024, 4}, vmCount{2, 4096, 20}},
		{&spec.Machine{CPUs: 3}, nil, vmCount{3, 1024, 4}, vmCount{3, 4096, 20}},
		{&spec.Machine{Memory: 6 << 30, Disk: 50 << 30}, nil, vmCount{1, 6144, 50}, vmCount{2, 6144, 50}},
		{nil, &spec.Machine{CPUs: 1, Memory: 2 << 30}, vmCount{1, 1024, 4}, vmCount{1, 2048, 20}},
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

// By hand, the steps say the size.
func TestVMCreateByHandSaysTheSize(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.sized(leastNotes, recNotes)
	if err := v.create(VMCreate{Platform: "proxmox"}, nil); err != nil {
		t.Fatal(err)
	}
	if want := "  Size the VM: notes runs on at least 2 CPUs, 2048 MiB, 20 GiB for the app; 4 CPUs, 8192 MiB, 60 GiB for the app recommended.\n"; !strings.Contains(v.out2(), want) {
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
