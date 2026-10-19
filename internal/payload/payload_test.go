package payload

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, exe []byte, files Files) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, bytes.NewReader(exe), files); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "installer")
	if err := os.WriteFile(p, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRoundTrip(t *testing.T) {
	exe := bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 1000)
	in := Files{Release: []byte(`{"a":1}`), Compose: []byte("services: {}\n"), KeysDir + "release.pub": []byte("k"), "empty": nil}
	p := write(t, exe, in)
	out, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d files", len(out))
	}
	for n, b := range in {
		if !bytes.Equal(out[n], b) {
			t.Errorf("%s: %q", n, out[n])
		}
	}
	raw, _ := os.ReadFile(p)
	if !bytes.HasPrefix(raw, exe) {
		t.Fatal("the executable is not kept intact")
	}
}

func TestNoPayload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plain")
	_ = os.WriteFile(p, []byte("just a binary, long enough to hold a trailer"), 0o755)
	if _, err := Read(p); !errors.Is(err, ErrNone) {
		t.Fatalf("got %v", err)
	}
	_ = os.WriteFile(p, []byte("x"), 0o755)
	if _, err := Read(p); !errors.Is(err, ErrNone) {
		t.Fatalf("short file: %v", err)
	}
}

func TestRefusesUnsafeNames(t *testing.T) {
	for _, n := range []string{"/etc/passwd", "../x", "a/../../x", "a//b", `a\b`, ""} {
		if err := Write(&bytes.Buffer{}, bytes.NewReader(nil), Files{n: nil}); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestRefusesACorruptLength(t *testing.T) {
	p := write(t, []byte("exe"), Files{Release: []byte("{}")})
	raw, _ := os.ReadFile(p)
	raw[int64(len(raw))-trailer] = 0xff // a length far beyond the file
	_ = os.WriteFile(p, raw, 0o755)
	if _, err := Read(p); err == nil || errors.Is(err, ErrNone) {
		t.Fatalf("got %v", err)
	}
}
