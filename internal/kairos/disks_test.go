package kairos

import (
	"archive/tar"
	"compress/gzip"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vhd writes a sparse file of size data bytes ending in a VHD footer, its checksum off by skew.
func vhd(t *testing.T, data int64, cookie string, diskType uint32, size uint64, skew uint32) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "d.vhd")
	var foot [512]byte
	copy(foot[:], cookie)
	binary.BigEndian.PutUint64(foot[48:], size)
	binary.BigEndian.PutUint32(foot[60:], diskType)
	var sum uint32
	for _, c := range foot {
		sum += uint32(c)
	}
	binary.BigEndian.PutUint32(foot[64:], ^sum+skew)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(foot[:], data); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckVHD(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	for _, c := range []struct {
		name     string
		data     int64
		cookie   string
		diskType uint32
		size     uint64
		skew     uint32
		want     string
	}{
		{"fixed, whole MiB", 3 * mib, "conectix", 2, 3 * mib, 0, ""},
		{"not a VHD", 3 * mib, "notavhd!", 2, 3 * mib, 0, "no VHD footer"},
		{"checksum off by one", 3 * mib, "conectix", 2, 3 * mib, 1, "checksum"},
		{"dynamic", 3 * mib, "conectix", 3, 3 * mib, 0, "not fixed"},
		{"a MiB and a sector", mib + 512, "conectix", 2, mib + 512, 0, "whole number of MiB"},
		{"empty", 0, "conectix", 2, 0, 0, "whole number of MiB"},
		{"size not the file's", 3 * mib, "conectix", 2, 2 * mib, 0, "bytes for a fixed disk"},
	} {
		err := checkVHD(vhd(t, c.data, c.cookie, c.diskType, c.size, c.skew))
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	short := filepath.Join(t.TempDir(), "short.vhd")
	if err := os.WriteFile(short, []byte("conectix"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkVHD(short); err == nil || !strings.Contains(err.Error(), "short.vhd: 8 bytes, no VHD footer") {
		t.Errorf("short: err = %v", err)
	}
}

// gce writes a tar.gz of the entries, each of its size in zeros.
func gce(t *testing.T, entries ...tar.Header) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "d.gce.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		h.Mode, h.Format = 0o644, tar.FormatGNU
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(make([]byte, h.Size)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckGCE(t *testing.T) {
	saved := gceUnit
	gceUnit = 1024
	t.Cleanup(func() { gceUnit = saved })
	for _, c := range []struct {
		entries []tar.Header
		want    string
	}{
		{[]tar.Header{{Name: "disk.raw", Size: 2048}}, ""},
		{[]tar.Header{{Name: "disk.raw", Size: 2048 + 512}}, "whole number of GiB"},
		{[]tar.Header{{Name: "disk.raw"}}, "whole number of GiB"},
		{[]tar.Header{{Name: "disk.img", Size: 2048}}, "first entry disk.img"},
		{[]tar.Header{{Name: "disk.raw", Size: 2048}, {Name: "notes", Size: 1}}, "notes after disk.raw"},
	} {
		err := checkGCE(gce(t, c.entries...))
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%v: err = %v, want %q", c.entries, err, c.want)
		}
	}
	notGz := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(notGz, []byte("disk.raw"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkGCE(notGz); err == nil {
		t.Error("a file that is no gzip passed")
	}
}
