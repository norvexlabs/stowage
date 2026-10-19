package release

import (
	"strings"
	"testing"
	"time"
)

func valid() *Manifest {
	return &Manifest{SchemaVersion: SchemaVersion, Name: "notes", ShortName: "notes", ProjectVersion: "1.0.0", Revision: 1, ReleaseSeq: 1,
		Channel: "stable", Tag: "1.0.0", Date: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), Inputs: "sha256:00",
		Scheme: Scheme{Provider: "semver"}, Artifacts: Artifacts{Images: map[string]string{"api": "sha256:11"}, Configs: map[string]string{"api": "sha256:22"}, Manifests: map[string]string{"api": "sha256:33"},
			Chart: &Chart{Repository: "charts/notes-stable", Version: "0.1.0", Digest: "sha256:" + strings.Repeat("c", 64)},
			OS:    "sha256:" + strings.Repeat("d", 64), Disks: map[string]string{"qcow2": "sha256:" + strings.Repeat("e", 64)}}}
}

func TestRoundTrip(t *testing.T) {
	raw, err := valid().Encode()
	if err != nil {
		t.Fatal(err)
	}
	m, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	m.Artifacts.Installer = "sha256:" + strings.Repeat("a", 64)
	if raw, err = m.Encode(); err != nil {
		t.Fatal(err)
	}
	if m, err = Decode(raw); err != nil || m.Artifacts.Installer != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("installer digest: %v", err)
	}
	if m.Artifacts.OS != valid().Artifacts.OS || m.Artifacts.Disks["qcow2"] != valid().Artifacts.Disks["qcow2"] {
		t.Fatalf("os and disks: %+v", m.Artifacts)
	}
	if m.Tag != "1.0.0" || m.Artifacts.Images["api"] != "sha256:11" || *m.Artifacts.Chart != *valid().Artifacts.Chart || !m.Date.Equal(valid().Date) || m.Yanked != nil {
		t.Fatalf("round trip lost data: %+v", m)
	}
}

func TestDecodeRefuses(t *testing.T) {
	raw, _ := valid().Encode()
	s := string(raw)
	for name, doc := range map[string]string{
		"unknown field":            strings.Replace(s, `"name"`, `"surprise": 1, "name"`, 1),
		"newer schema":             strings.Replace(s, `"schemaVersion": 2`, `"schemaVersion": 3`, 1),
		"missing schema":           strings.Replace(s, `"schemaVersion": 2,`, ``, 1),
		"previous schema":          strings.Replace(s, `"schemaVersion": 2`, `"schemaVersion": 1`, 1),
		"no short name":            strings.Replace(s, `"short_name": "notes"`, `"short_name": ""`, 1),
		"short name is a path":     strings.Replace(s, `"short_name": "notes"`, `"short_name": "../../usr/bin/x"`, 1),
		"revision 0":               strings.Replace(s, `"revision": 1`, `"revision": 0`, 1),
		"seq 0":                    strings.Replace(s, `"release_seq": 1`, `"release_seq": 0`, 1),
		"no config digest":         strings.Replace(s, `"api": "sha256:22"`, `"web": "sha256:22"`, 1),
		"no manifest digest":       strings.Replace(s, `"api": "sha256:33"`, `"web": "sha256:33"`, 1),
		"no tag":                   strings.Replace(s, `"tag": "1.0.0"`, `"tag": ""`, 1),
		"no scheme":                strings.Replace(s, `"provider": "semver"`, `"provider": ""`, 1),
		"trailing document":        s + `{}`,
		"trailing brace":           s + `}`,
		"trailing bracket":         s + `]`,
		"not json":                 "nope",
		"installer no digest":      strings.Replace(s, `"manifests"`, `"installer": "latest", "manifests"`, 1),
		"installer 63 hex":         strings.Replace(s, `"manifests"`, `"installer": "sha256:`+strings.Repeat("a", 63)+`", "manifests"`, 1),
		"installer 65 hex":         strings.Replace(s, `"manifests"`, `"installer": "sha256:`+strings.Repeat("a", 65)+`", "manifests"`, 1),
		"installer uppercase":      strings.Replace(s, `"manifests"`, `"installer": "sha256:`+strings.Repeat("A", 64)+`", "manifests"`, 1),
		"chart of another channel": strings.Replace(s, `"charts/notes-stable"`, `"charts/notes-beta"`, 1),
		"chart of another name":    strings.Replace(s, `"charts/notes-stable"`, `"charts/atlas-stable"`, 1),
		"chart version not seq":    strings.Replace(s, `"0.1.0"`, `"0.2.0"`, 1),
		"chart version empty":      strings.Replace(s, `"0.1.0"`, `""`, 1),
		"chart no digest":          strings.Replace(s, `"sha256:`+strings.Repeat("c", 64)+`"`, `"latest"`, 1),
		"chart unknown field":      strings.Replace(s, `"repository"`, `"url": "x", "repository"`, 1),
		"os no digest":             strings.Replace(s, `"sha256:`+strings.Repeat("d", 64)+`"`, `"latest"`, 1),
		"disks without os":         strings.Replace(s, `"os": "sha256:`+strings.Repeat("d", 64)+`",`, ``, 1),
		"disk without a format":    strings.Replace(s, `"qcow2"`, `""`, 1),
		"disk format not tag-safe": strings.Replace(s, `"qcow2"`, `"x/y"`, 1),
		"disk format uppercase":    strings.Replace(s, `"qcow2"`, `"QCOW2"`, 1),
		"disk no digest":           strings.Replace(s, `"sha256:`+strings.Repeat("e", 64)+`"`, `"sha256:e"`, 1),
		"installer sha512":         strings.Replace(s, `"manifests"`, `"installer": "sha512:`+strings.Repeat("a", 64)+`", "manifests"`, 1),
	} {
		if doc == s {
			t.Fatalf("%s: mutation did nothing", name)
		}
		if _, err := Decode([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A disk format this build does not know is kept: a later stowage may publish more.
func TestDecodeKeepsAnUnknownDiskFormat(t *testing.T) {
	raw, _ := valid().Encode()
	m, err := Decode([]byte(strings.Replace(string(raw), `"qcow2"`, `"iso"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Artifacts.Disks["iso"] == "" {
		t.Errorf("disks = %v", m.Artifacts.Disks)
	}
}

func TestEncodeRefusesAnIncompleteManifest(t *testing.T) {
	m := valid()
	m.Inputs = ""
	if _, err := m.Encode(); err == nil {
		t.Fatal("encoded a manifest without inputs")
	}
}

// Decode never panics, and whatever it accepts encodes and decodes back to the same manifest.
func FuzzDecode(f *testing.F) {
	raw, _ := valid().Encode()
	f.Add(raw)
	f.Add([]byte(`{"schemaVersion":2}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Decode(b)
		if err != nil {
			return
		}
		again, err := m.Encode()
		if err != nil {
			t.Fatalf("accepted a manifest it cannot encode: %v", err)
		}
		m2, err := Decode(again)
		if err != nil {
			t.Fatalf("re-decode: %v", err)
		}
		a, _ := m.Encode()
		b2, _ := m2.Encode()
		if string(a) != string(b2) {
			t.Fatalf("round trip changed the manifest:\n%s\n%s", a, b2)
		}
	})
}

// bundled is valid() as an OS bundle carries it: without its publication.
func bundled() *Manifest {
	m := valid()
	m.Channel, m.Tag, m.ReleaseSeq, m.Date = "", "", 0, time.Time{}
	m.Artifacts.Chart, m.Artifacts.OS, m.Artifacts.Disks = nil, "", nil
	return m
}

func TestABundledManifestCarriesNoPublication(t *testing.T) {
	raw, err := bundled().EncodeBundled()
	if err != nil {
		t.Fatal(err)
	}
	m, err := DecodeBundled(raw)
	if err != nil || !m.Bundled() || m.ProjectVersion != "1.0.0" || m.Artifacts.Images["api"] != "sha256:11" {
		t.Fatalf("%v %+v", err, m)
	}
	// A published manifest is not a bundled one, and the reverse.
	if _, err := Decode(raw); err == nil {
		t.Error("Decode accepted a bundled manifest")
	}
	pub, _ := valid().Encode()
	if _, err := DecodeBundled(pub); err == nil {
		t.Error("DecodeBundled accepted a published manifest")
	}
	// An artifact beyond the images is refused as such, before the published manifest's rules.
	for name, mod := range map[string]func(*Manifest){
		"installer": func(m *Manifest) { m.Artifacts.Installer = "sha256:" + strings.Repeat("a", 64) },
		"chart":     func(m *Manifest) { m.Artifacts.Chart = valid().Artifacts.Chart },
		"os":        func(m *Manifest) { m.Artifacts.OS = valid().Artifacts.OS },
		"disks":     func(m *Manifest) { m.Artifacts.Disks = valid().Artifacts.Disks },
	} {
		m := bundled()
		mod(m)
		if _, err := m.EncodeBundled(); err == nil || !strings.Contains(err.Error(), "records no installer, chart, os or disks") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, mod := range map[string]func(*Manifest){
		"channel":     func(m *Manifest) { m.Channel = "stable" },
		"tag":         func(m *Manifest) { m.Tag = "1.0.0" },
		"release_seq": func(m *Manifest) { m.ReleaseSeq = 1 },
		"date":        func(m *Manifest) { m.Date = time.Now() },
		"yank":        func(m *Manifest) { m.Yanked = &Yank{Reason: "x"} },
		"installer":   func(m *Manifest) { m.Artifacts.Installer = "sha256:" + strings.Repeat("a", 64) },
		"chart":       func(m *Manifest) { m.Artifacts.Chart = valid().Artifacts.Chart },
		"os":          func(m *Manifest) { m.Artifacts.OS = valid().Artifacts.OS },
		"disks":       func(m *Manifest) { m.Artifacts.Disks = valid().Artifacts.Disks },
		// The rest is checked as a published manifest's.
		"no inputs":  func(m *Manifest) { m.Inputs = "" },
		"revision 0": func(m *Manifest) { m.Revision = 0 },
		"no config":  func(m *Manifest) { m.Artifacts.Configs = map[string]string{} },
	} {
		m := bundled()
		mod(m)
		if _, err := m.EncodeBundled(); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
}

func TestPreloadTagNamesTheDigest(t *testing.T) {
	if got := PreloadTag("sha256:" + strings.Repeat("a", 64)); got != "sha256-"+strings.Repeat("a", 64) {
		t.Errorf("got %s", got)
	}
}
