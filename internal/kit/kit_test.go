package kit

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// fast is scrypt's work factor in these tests, which a real seal's would make slow.
const fast = 4

var (
	t0      = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	expires = t0.Add(7 * 24 * time.Hour)
)

func sample() *Kit {
	return &Kit{For: "notes", Name: "search", Instance: "site-1", Issued: t0, Expires: expires,
		Inputs: map[string]string{"TOKEN": "s3cret", "CA": "stowage-file:AAAA"},
		TLS:    &TLS{Cert: "c", Key: "k", CA: "a", ClientCA: "b"}}
}

// sealRaw seals plain as a kit is sealed, whatever it holds.
func sealRaw(t *testing.T, plain, code string, factor int) []byte {
	t.Helper()
	r, err := age.NewScryptRecipient(code)
	if err != nil {
		t.Fatal(err)
	}
	r.SetWorkFactor(factor)
	var out bytes.Buffer
	a := armor.NewWriter(&out)
	w, err := age.Encrypt(a, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(plain)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// askingMoreWork is raw with its header asking scrypt for one more than Open pays. The header's MAC
// no longer matches, which only the work would show.
func askingMoreWork(t *testing.T, raw []byte) []byte {
	t.Helper()
	plain, err := io.ReadAll(armor.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	stanza := regexp.MustCompile(`(-> scrypt \S+) \d+\n`)
	if !stanza.Match(plain) {
		t.Fatalf("no scrypt stanza in %q", plain)
	}
	plain = stanza.ReplaceAll(plain, []byte(fmt.Sprintf("$1 %d\n", WorkFactor+1)))
	var out bytes.Buffer
	a := armor.NewWriter(&out)
	if _, err := a.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestSealThenOpen(t *testing.T) {
	t.Parallel()
	raw, err := Seal(sample(), " the-code\n", fast)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(armor.Header)) || bytes.Contains(raw, []byte("s3cret")) {
		t.Fatalf("not armored, or holds a value in the clear:\n%s", raw)
	}
	k, err := Open(raw, "the-code")
	if err != nil {
		t.Fatal(err)
	}
	if k.Format != Format || k.For != "notes" || k.Name != "search" || k.Instance != "site-1" || !k.Issued.Equal(t0) || !k.Expires.Equal(expires) {
		t.Fatalf("%+v", k)
	}
	if k.Inputs["TOKEN"] != "s3cret" || k.Inputs["CA"] != "stowage-file:AAAA" || *k.TLS != *sample().TLS {
		t.Fatalf("%+v %+v", k.Inputs, k.TLS)
	}
}

// A kit of this format, as written once: a change that stops it opening breaks every kit issued.
func TestAKitOfThisFormatStaysReadable(t *testing.T) {
	t.Parallel()
	const golden = `-----BEGIN AGE ENCRYPTED FILE-----
YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IHNjcnlwdCA3VGFLSFJLbkIvOFZsVlRN
UStxNVhRIDQKdTdvaEE1RGJ6MVh1eHRWWEdSbzgrT2xuVXhOMVRybFY3dkpuNDFU
Z09aSQotLS0gamxZaFBHcmM0K09RWmZGUkpzbitIVVAwWWZIVkxkOUNDUzE4aVJu
THZtSQrRJHthhoKiEi+PDVnG6V68tLr0Hxatw/RIt/83oRiel4ghJAg80v2C0cq8
DleGlRKRDpMD6OxwP4TYUKmzvj7rCbK350ZCjAhoFSGGjiiFXMvwdwdRCvZmw4kS
2GfWa7KFzqodqiigxRIDpPeCvaTZiblZlelri08zRa+BRQ/KtNizZTdgluopcisH
nW4MYCfesT1Ye4bqe4ZL4JP0gRMfoItFLcXM2A5Z0TZZE8po2Fz5lhmXofkXtklY
suGBZZ9i2yvm3czNbX2NENMvwafVzR1paXPC4Bs3JVJxj/RJdAijL4jk4osIWDxG
uL4P8372dOETKx0XJ7WrrniIrcfOVYIehIbS
-----END AGE ENCRYPTED FILE-----
`
	k, err := Open([]byte(golden), "golden-code")
	if err != nil {
		t.Fatal(err)
	}
	if k.For != "notes" || k.Instance != "site-1" || k.Inputs["TOKEN"] != "s3cret" || k.TLS.Cert != "c" {
		t.Fatalf("%+v", k)
	}
}

func TestOpenRefuses(t *testing.T) {
	t.Parallel()
	good, err := Seal(sample(), "code", fast)
	if err != nil {
		t.Fatal(err)
	}
	// One byte of the armored body changed, past the header, and one of the header.
	flip := func(at int) []byte {
		lines := bytes.Split(bytes.Clone(good), []byte("\n"))
		l := lines[at]
		if l[0] == 'A' {
			l[0] = 'B'
		} else {
			l[0] = 'A'
		}
		return bytes.Join(lines, []byte("\n"))
	}
	bodyLine := len(bytes.Split(good, []byte("\n"))) - 3

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var forged bytes.Buffer
	a := armor.NewWriter(&forged)
	w, err := age.Encrypt(a, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("format: stowage-kit/v1\nfor: notes\nexpires: 2030-01-01T00:00:00Z\n"))
	_ = w.Close()
	_ = a.Close()

	for name, c := range map[string]struct {
		raw        []byte
		code, want string
	}{
		"wrong code":      {good, "other", "does not open it"},
		"no code":         {good, "  ", "no code"},
		"clear text":      {[]byte("project: notes\n"), "code", "not a kit"},
		"binary age":      {[]byte("age-encryption.org/v1\n"), "code", "not a kit"},
		"body changed":    {flip(bodyLine), "code", "does not open"},
		"header changed":  {flip(1), "code", "does not open"},
		"cut short":       {good[:len(good)-60], "code", "open"},
		"trailing":        {append(bytes.Clone(good), "x\n"...), "code", "open"},
		"to a public key": {forged.Bytes(), "code", "does not open it"},
		"too large":       {append(bytes.Clone(good), make([]byte, MaxSize)...), "code", "more than"},
		"work factor":     {askingMoreWork(t, good), "code", "work factor"},
		"content over":    {sealRaw(t, strings.Repeat("#", MaxContent+1), "code", fast), "code", "more than"},
	} {
		if _, err := Open(c.raw, c.code); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// The largest content a kit holds fits the largest file, armored; one byte past it is refused.
func TestTheSizeBounds(t *testing.T) {
	t.Parallel()
	k := &Kit{For: "notes", Expires: expires, Inputs: map[string]string{"A": ""}}
	base, err := Seal(k, "c", fast)
	if err != nil || len(base) == 0 {
		t.Fatal(err)
	}
	// The value's bytes alone count: plain letters are written as they are, in one line.
	over := MaxContent
	k.Inputs["A"] = strings.Repeat("x", over)
	if _, err := Seal(k, "c", fast); err == nil || !strings.Contains(err.Error(), "a kit holds") {
		t.Fatalf("over: %v", err)
	}
	for n := MaxContent - 200; ; n-- {
		k.Inputs["A"] = strings.Repeat("x", n)
		raw, err := Seal(k, "c", fast)
		if err != nil {
			continue
		}
		if len(raw) > MaxSize {
			t.Fatalf("%d bytes of armor over the bound", len(raw))
		}
		if _, err := Open(raw, "c"); err != nil {
			t.Fatal(err)
		}
		return
	}
}

func TestSealRefuses(t *testing.T) {
	t.Parallel()
	if _, err := Seal(sample(), " ", fast); err == nil || !strings.Contains(err.Error(), "no code") {
		t.Fatal(err)
	}
	if _, err := Seal(&Kit{Expires: expires}, "c", fast); err == nil || !strings.Contains(err.Error(), "no project") {
		t.Fatal(err)
	}
	if _, err := Seal(&Kit{For: "notes"}, "c", fast); err == nil || !strings.Contains(err.Error(), "no expiry") {
		t.Fatal(err)
	}
}

func TestParseIsStrict(t *testing.T) {
	t.Parallel()
	const ok = "format: stowage-kit/v1\nfor: notes\nexpires: 2030-01-01T00:00:00Z\n"
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ raw, want string }{
		"no format":      {"for: notes\nexpires: 2030-01-01T00:00:00Z\n", "its format"},
		"newer format":   {"format: stowage-kit/v2\nfor: notes\nexpires: 2030-01-01T00:00:00Z\nnew: x\n", "stowage-kit/v2"},
		"unknown field":  {ok + "nope: x\n", "nope"},
		"unknown in tls": {ok + "tls: {nope: x}\n", "nope"},
		"no project":     {"format: stowage-kit/v1\nexpires: 2030-01-01T00:00:00Z\n", "no project"},
		"no expiry":      {"format: stowage-kit/v1\nfor: notes\n", "no expiry"},
		"bad expiry":     {"format: stowage-kit/v1\nfor: notes\nexpires: soon\n", "parsing time"},
		"two documents":  {ok + "---\n" + ok, "more than one document"},
		"key twice":      {ok + "inputs: {A: x, A: y}\n", "already"},
		"not yaml":       {"{", "its content"},
	} {
		if _, err := Parse([]byte(c.raw)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()
	k := sample()
	if err := k.Check("notes", expires.Add(-time.Nanosecond)); err != nil {
		t.Fatalf("just before its expiry: %v", err)
	}
	for _, at := range []time.Time{expires, expires.Add(time.Nanosecond)} {
		if err := k.Check("notes", at); err == nil || !strings.Contains(err.Error(), "expired at 2026-11-13T12:00:00Z") {
			t.Fatalf("at %v: %v", at, err)
		}
	}
	if err := k.Check("crm", t0); err == nil || !strings.Contains(err.Error(), `for "notes", not crm`) {
		t.Fatal(err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte("format: stowage-kit/v1\nfor: notes\nexpires: 2030-01-01T00:00:00Z\ninputs: {A: b}\ntls: {cert: c}\n"))
	f.Add([]byte("{"))
	f.Add([]byte("format: stowage-kit/v1\n---\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		k, err := Parse(raw)
		if err != nil {
			return
		}
		if k.Format != Format || k.For == "" || k.Expires.IsZero() {
			t.Fatalf("accepted %+v", k)
		}
	})
}
