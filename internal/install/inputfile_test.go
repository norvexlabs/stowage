package install

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/spec"
)

func pemOf(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func opensshKey(cipher string) []byte {
	return append(append([]byte("openssh-key-v1\x00"), 0, 0, 0, byte(len(cipher))), cipher...)
}

func testCert(t *testing.T) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return pemOf("CERTIFICATE", der)
}

func TestCheckPEM(t *testing.T) {
	t.Parallel()
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(ed)
	if err != nil {
		t.Fatal(err)
	}
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalECPrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	cert := testCert(t)
	encrypted := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00"}, Bytes: []byte{1}})
	for _, c := range []struct {
		name    string
		content []byte
		want    string
	}{
		{"a certificate", cert, ""},
		{"a bundle", append(slices.Clone(cert), cert...), ""},
		{"trailing newlines", append(slices.Clone(cert), "\n\n"...), ""},
		{"pkcs8", pemOf("PRIVATE KEY", pkcs8), ""},
		{"rsa", pemOf("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rk)), ""},
		{"ec", pemOf("EC PRIVATE KEY", ecDER), ""},
		{"another type", pemOf("X509 CRL", []byte{1}), ""},
		{"nothing", nil, "no PEM block"},
		{"text", []byte("not PEM"), "no PEM block"},
		{"after the last", append(slices.Clone(cert), "garbage"...), "text after PEM block 1"},
		{"comments before and between", append(append([]byte("# one\n"), cert...), append([]byte("# two\n"), cert...)...), ""},
		{"a block between that does not decode", append(append(slices.Clone(cert), "-----BEGIN CERTIFICATE-----\n!!!\n-----END CERTIFICATE-----\n"...), cert...), "2 of them decode"},
		{"a truncated last block", append(slices.Clone(cert), "-----BEGIN CERTIFICATE-----\nAAAA\n"...), "text after PEM block 1"},
		{"bad certificate", pemOf("CERTIFICATE", []byte{1}), "PEM block 1 (CERTIFICATE): does not parse"},
		{"bad second", append(slices.Clone(cert), pemOf("CERTIFICATE", []byte{1})...), "PEM block 2 (CERTIFICATE)"},
		{"a curve the Go library does not read", pemOf("EC PRIVATE KEY", []byte{0x30, 0x03, 0x02, 0x01, 0x01}), ""},
		{"DER with a tail", pemOf("CERTIFICATE", []byte{0x30, 0x00, 0x00}), "does not parse"},
		{"indented block between", append(append(slices.Clone(cert), "  -----BEGIN CERTIFICATE-----\nAAAA\n  -----END CERTIFICATE-----\n"...), cert...), "2 of them decode"},
		{"a mark before the first", append([]byte("\xef\xbb\xbf"), append(slices.Clone(cert), cert...)...), "1 of them decode"},
		{"openssh", pemOf("OPENSSH PRIVATE KEY", opensshKey("none")), ""},
		{"encrypted openssh", pemOf("OPENSSH PRIVATE KEY", opensshKey("aes256-ctr")), "an encrypted key"},
		{"openssh without its magic", pemOf("OPENSSH PRIVATE KEY", []byte("junk")), "does not parse"},
		{"openssh cut short", pemOf("OPENSSH PRIVATE KEY", []byte("openssh-key-v1\x00\x00\x00\x00\xff")), "does not parse"},
		{"bad pkcs8", pemOf("PRIVATE KEY", []byte{1}), "does not parse"},
		{"bad rsa", pemOf("RSA PRIVATE KEY", []byte{1}), "does not parse"},
		{"bad ec", pemOf("EC PRIVATE KEY", []byte{1}), "does not parse"},
		{"encrypted pkcs8", pemOf("ENCRYPTED PRIVATE KEY", []byte{1}), "an encrypted key"},
		{"encrypted legacy", encrypted, "an encrypted key"},
	} {
		err := checkPEM(c.content)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// checkPEM reads what an operator hands over: it never panics, and refuses what ends in text.
func FuzzCheckPEM(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"))
	f.Add([]byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\ntext"))
	f.Add([]byte("-----BEGIN X-----\nProc-Type: 4,ENCRYPTED\n\nAAAA\n-----END X-----\n"))
	f.Fuzz(func(t *testing.T, content []byte) {
		if err := checkPEM(content); err == nil && !strings.Contains(string(content), "-----BEGIN ") {
			t.Errorf("accepted %q, which holds no block", content)
		}
	})
}

func TestFileInputCheckBounds(t *testing.T) {
	t.Parallel()
	check := fileInputCheck(spec.Input{Key: "F", Type: spec.InputFile})
	exact := encodeFileInput(make([]byte, spec.MaxInputFile))
	if err := check(exact); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if err := spec.CheckEnvValue(exact); err != nil {
		t.Errorf("at the limit, the record refuses it: %v", err)
	}
	if err := check(encodeFileInput(make([]byte, spec.MaxInputFile+1))); err == nil {
		t.Error("over the limit passed")
	}
	if err := check(""); err != nil {
		t.Errorf("empty: %v", err)
	}
	if err := check(fileValuePrefix); err == nil {
		t.Error("the prefix alone, an empty file, passed")
	}
	if err := check("admin123"); err == nil {
		t.Error("a text that reads as base64 passed")
	}
	if err := check(fileValuePrefix + "not base64!"); err == nil {
		t.Error("the prefix alone passed")
	}
	if err := check("not base64!"); err == nil || !strings.Contains(err.Error(), "--set-file KEY=PATH") {
		t.Errorf("text: %v", err)
	}
	if err := fileInputCheck(spec.Input{Type: spec.InputPEM})(encodeFileInput([]byte("text"))); err == nil {
		t.Error("a pem input took text")
	}

	dir := t.TempDir()
	at := filepath.Join(dir, "at")
	over := filepath.Join(dir, "over")
	if err := os.WriteFile(at, make([]byte, spec.MaxInputFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(over, make([]byte, spec.MaxInputFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := loadFileInput(at); err != nil || v != exact {
		t.Errorf("a file at the limit: %v", err)
	}
	if _, err := loadFileInput(over); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("a file over the limit: %v", err)
	}
	if _, err := loadFileInput(dir); err == nil {
		t.Error("a directory passed")
	}
}

func TestAddSetFiles(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"F", "=" + file, "F=", "F=" + file + ".missing", "F=" + empty} {
		if err := addSetFiles(map[string]string{}, []string{bad}); err == nil {
			t.Errorf("--set-file %s passed", bad)
		}
	}
	set := map[string]string{"T": "x"}
	if err := addSetFiles(set, []string{"F=" + file}); err != nil || set["F"] != encodeFileInput([]byte("hi")) || set["T"] != "x" {
		t.Errorf("%v, %v", err, set)
	}
	if err := addSetFiles(map[string]string{"F": "x"}, []string{"F=" + file}); err == nil || !strings.Contains(err.Error(), "both give F") {
		t.Errorf("both: %v", err)
	}
	if err := addSetFiles(map[string]string{}, []string{"F=" + file, "F=" + file}); err == nil {
		t.Error("twice passed")
	}
}

// A file given for a text input is refused as it is given, on Docker and on Kubernetes.
func TestAFileForATextInputIsRefused(t *testing.T) {
	e := newEnv(t)
	err := e.install(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": encodeFileInput([]byte("x"))} })
	if err == nil || !strings.Contains(err.Error(), "ADMIN_EMAIL: is no file input of this release") {
		t.Errorf("install: %v", err)
	}
	k := newKubeEnv(t, spec.Provider{Name: "none"})
	err = k.installer(func(o *Options) { o.DryRun, o.Set = true, map[string]string{"ADMIN_EMAIL": fileValuePrefix} }).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is no file input of this release") {
		t.Errorf("kubernetes: %v", err)
	}
}

// `upgrade --version` hands --set-file to the fetched installer as it was given, which judges it.
func TestSetFileIsPassedOn(t *testing.T) {
	t.Parallel()
	var items []string
	cmd := &cobra.Command{Use: "x", Run: func(*cobra.Command, []string) {}}
	cmd.Flags().StringArrayVar(&items, "set-file", nil, "")
	cmd.SetArgs([]string{"--set-file", "A=/p/a", "--set-file", "B=b"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := passed(cmd, ""); !slices.Equal(got, []string{"--set-file=A=/p/a", "--set-file=B=b"}) {
		t.Errorf("%v", got)
	}
}

func TestFileInputsFit(t *testing.T) {
	t.Parallel()
	s := &spec.Spec{Inputs: []spec.Input{{Key: "CA", Type: spec.InputFile}, {Key: "T"}}}
	p := Platform{Label: "A cloud", UserDataMax: 100}
	st := &Settings{Inputs: map[string]string{"CA": "x", "T": "y"}}
	if err := fileInputsFit(p, s, st, 100); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if err := fileInputsFit(p, s, st, 101); err == nil || !strings.Contains(err.Error(), "give smaller files for CA") || strings.Contains(err.Error(), "T,") {
		t.Errorf("over it: %v", err)
	}
	if err := fileInputsFit(p, s, &Settings{Inputs: map[string]string{"T": "y"}}, 101); err != nil {
		t.Errorf("no file input: %v", err)
	}
	if err := fileInputsFit(Platform{}, s, st, 1<<20); err != nil {
		t.Errorf("no limit: %v", err)
	}
}

func TestReadAnswers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "beside"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(body string) string {
		p := filepath.Join(dir, "answers.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	got, err := readAnswers(write("F: {file: beside}\nT: text\nN: 5\n"))
	if err != nil || got["F"] != encodeFileInput([]byte("hi")) || got["T"] != "text" || got["N"] != "5" {
		t.Errorf("%v, %v", err, got)
	}
	if got, err := readAnswers(write("F: {file: " + filepath.Join(dir, "beside") + "}\n")); err != nil || got["F"] == "" {
		t.Errorf("an absolute path: %v, %v", err, got)
	}
	for body, want := range map[string]string{
		"F: {file: missing}\n":         "F:",
		"F: {path: beside}\n":          "{file: PATH}",
		"F: {file: beside, more: x}\n": "{file: PATH}",
		"T: [a, b]\n":                  "a value is expected",
		"- a\n":                        "a map of KEY: value",
	} {
		if _, err := readAnswers(write(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
}

// inputFiles lists the install's input files, by name.
func inputFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, InputFilesDir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A file input's life on Docker: an empty file left unasked, its path recorded relative to the
// install, a new value a new path, the old file gone once the stack started on the new one, and
// the value never in the log.
func TestAFileInputIsWrittenForTheStack(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "CA", Prompt: "CA file", Optional: true, Type: spec.InputFile})
	e.vars["CA"] = encodeFileInput([]byte("from the environment"))
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	env := e.readEnv()
	path := func() string { return get(e.readEnv(), spec.FileVar("CA")) }
	first := path()
	if get(env, "CA") != "" || first != "./"+InputFilesDir+"/"+fileInputName("CA", "") {
		t.Fatalf("recorded %q at %q: the environment gives no file input", get(env, "CA"), first)
	}
	info, err := os.Stat(filepath.Join(e.dir, first))
	if err != nil || info.Size() != 0 || !info.Mode().IsRegular() {
		t.Fatalf("the empty file: %v, %v", info, err)
	}
	dirInfo, err := os.Stat(filepath.Join(e.dir, InputFilesDir))
	if err != nil || dirInfo.Mode().Perm() != 0o700 || info.Mode().Perm() != 0o644 {
		t.Errorf("modes: %v, %v, %v", dirInfo, info, err)
	}

	// Given: the file and a new path. What the start ran with is what is recorded.
	v := encodeFileInput([]byte("the content"))
	e.docker.calls = nil
	var during []string
	e.docker.onUp = func() error { during = inputFiles(t, e.dir); return nil }
	if err := e.install(func(o *Options) { o.Set = map[string]string{"CA": v} }); err != nil {
		t.Fatal(err)
	}
	second := path()
	if second == first {
		t.Fatal("a new value kept its path: compose would not recreate the services mounting it")
	}
	got, err := os.ReadFile(filepath.Join(e.dir, second))
	if err != nil || string(got) != "the content" {
		t.Fatalf("%q, %v", got, err)
	}
	up := slices.IndexFunc(e.docker.calls, func(c Cmd) bool { return slices.Contains(c.Args, "up") })
	if up < 0 || !slices.Contains(e.docker.calls[up].Env, spec.FileVar("CA")+"="+second) {
		t.Error("the start was not given the file's path")
	}
	if len(during) != 2 {
		t.Errorf("during the start: %v: the old file goes after it", during)
	}
	if names := inputFiles(t, e.dir); len(names) != 1 || "./"+InputFilesDir+"/"+names[0] != second {
		t.Errorf("after the start: %v", names)
	}
	log := e.log.String()
	if strings.Contains(log, v) || !strings.Contains(log, `changing CA: "" → `+fileInputDigest(v)) || strings.Contains(log, "changing "+spec.FileVar("CA")) {
		t.Errorf("the log: %s", log)
	}

	// A start that fails keeps the file the running containers were made with.
	v2 := encodeFileInput([]byte("newer"))
	e.docker.onUp = func() error { return errors.New("no") }
	if err := e.install(func(o *Options) { o.Set = map[string]string{"CA": v2} }); err == nil {
		t.Fatal("the failed start passed")
	}
	if names := inputFiles(t, e.dir); len(names) != 2 {
		t.Errorf("after the failed start: %v", names)
	}
	e.docker.onUp = nil

	// A value that is no base64 is refused, naming the input and the flag.
	err = e.install(func(o *Options) { o.Set = map[string]string{"CA": "plain text"} })
	if err == nil || !strings.Contains(err.Error(), "CA: not a file's content") || !strings.Contains(err.Error(), "--set-file") {
		t.Errorf("text: %v", err)
	}
}

func TestAPEMInputIsCheckedAndAskedAsAPath(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "CA", Prompt: "CA file", Required: true, Type: spec.InputPEM, Secret: true})
	cert := testCert(t)
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, cert, 0o600); err != nil {
		t.Fatal(err)
	}

	err := e.install(func(o *Options) { o.Set = map[string]string{"CA": encodeFileInput([]byte("text"))} })
	if err == nil || !strings.Contains(err.Error(), "CA: no PEM block") {
		t.Fatalf("not PEM: %v", err)
	}
	err = e.install(nil)
	if err == nil || !strings.Contains(err.Error(), "CA (CA file: a file, --set-file CA=PATH)") {
		t.Fatalf("missing: %v", err)
	}

	p := &fakePrompter{answers: map[string]string{"CA": path}}
	if err := e.install(func(o *Options) { o.Prompt = p }); err != nil {
		t.Fatal(err)
	}
	if got := get(e.readEnv(), "CA"); got != encodeFileInput(cert) {
		t.Errorf("recorded %q", got)
	}
	var q Question
	for _, f := range p.forms {
		for _, one := range f.Questions {
			if one.Key == "CA" {
				q = one
			}
		}
	}
	if q.Key == "" {
		t.Fatal("not asked")
	}
	if q.Secret {
		t.Error("its path was asked as a secret")
	}
	if err := q.Check(path); err != nil {
		t.Errorf("the path: %v", err)
	}
	if err := q.Check(path + ".missing"); err == nil {
		t.Error("a missing path passed")
	}
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(bad, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := q.Check(bad); err == nil || !strings.Contains(err.Error(), "no PEM block") {
		t.Errorf("a file that is not PEM: %v", err)
	}
	if err := q.Check(""); err != nil {
		t.Errorf("empty, which Required handles: %v", err)
	}
}

// A release that no longer declares a file input forgets its path and removes its file.
func TestARemovedFileInputGoes(t *testing.T) {
	e := newEnv(t)
	inputs := e.r.Spec.Inputs
	e.r.Spec.Inputs = append(slices.Clone(inputs), spec.Input{Key: "CA", Prompt: "CA file", Optional: true, Type: spec.InputFile})
	if err := e.install(func(o *Options) { o.Set = map[string]string{"CA": encodeFileInput([]byte("x"))} }); err != nil {
		t.Fatal(err)
	}
	e.r.Spec.Inputs = inputs
	in := e.installer(nil)
	env := e.readEnv()
	in.fileVars(env)
	if _, ok := env.Get(spec.FileVar("CA")); ok {
		t.Error("the path of a removed input stayed")
	}
	if err := in.sweepInputFiles(env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, InputFilesDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory stayed: %v", err)
	}
}

// A secret file input is carried by `secrets export` as any secret input, base64 unchanged, and
// its content is never in the log.
func TestASecretFileInputIsExported(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "KEY_FILE", Prompt: "Key", Optional: true, Secret: true, Type: spec.InputFile})
	v := encodeFileInput([]byte("private"))
	if err := e.install(func(o *Options) { o.Set = map[string]string{"KEY_FILE": v} }); err != nil {
		t.Fatal(err)
	}
	e.out.Reset()
	dir := e.dir
	e.dir = filepath.Join(t.TempDir(), "fresh")
	if err := e.install(func(o *Options) { o.DryRun, o.Set = true, map[string]string{"KEY_FILE": v} }); err != nil {
		t.Fatal(err)
	}
	if out := e.out.String(); !strings.Contains(out, spec.FileVar("KEY_FILE")+"=<redacted>\n") || !strings.Contains(out, "KEY_FILE=<redacted>\n") {
		t.Errorf("the dry run:\n%s", out)
	}
	e.dir = dir
	f, err := e.installer(nil).exportSecrets(context.Background(), e.readEnv())
	if err != nil || f.Secrets["KEY_FILE"] != v {
		t.Errorf("exported %v, %v", f, err)
	}
	if strings.Contains(e.log.String(), v) {
		t.Error("the secret file's value is in the log")
	}
}

// What --dry-run prints shows a file input by its digest, a file being no line to read.
func TestADryRunShowsAFileInputsDigest(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "CA", Prompt: "CA file", Optional: true, Type: spec.InputFile})
	v := encodeFileInput([]byte("the content"))
	if err := e.install(func(o *Options) { o.DryRun, o.Set = true, map[string]string{"CA": v} }); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	if strings.Contains(out, v) || !strings.Contains(out, "CA="+fileInputDigest(v)+"\n") {
		t.Errorf("%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.dir, InputFilesDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dry run wrote the files: %v", err)
	}
}

// vm create puts a non-secret file input in the seed, showing its digest, and refuses a secret one.
func TestVMCreateCarriesAFileInput(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "CA", Prompt: "CA file", Optional: true, Type: spec.InputFile})
	value := encodeFileInput([]byte("the content"))
	if err := v.create(VMCreate{Platform: "proxmox", SSHAllow: []string{"192.0.2.0/24"}}, func(o *Options) { o.Set = map[string]string{"CA": value} }); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if log := v.log.String(); strings.Contains(log, value) || !strings.Contains(log, "CA="+fileInputDigest(value)) {
		t.Errorf("%s", log)
	}
	ud, _, _ := readSeed(t, filepath.Join(v.out, "notes-seed.iso"))
	if st, _ := cloudSettings(t, ud); st.Inputs["CA"] != value {
		t.Errorf("the seed holds %q", st.Inputs["CA"])
	}

	v = newVMCreate(t, "qcow2")
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "CA", Prompt: "CA file", Optional: true, Secret: true, Type: spec.InputFile})
	err := v.create(VMCreate{Platform: "proxmox", SSHAllow: []string{"192.0.2.0/24"}}, func(o *Options) { o.Set = map[string]string{"CA": value} })
	if err == nil || !strings.Contains(err.Error(), "CA") {
		t.Errorf("a secret file input in the seed: %v", err)
	}
}

// A cloud's limit on user data refuses a file input carrying the cloud-config past it.
func TestVMCreateRefusesAFileInputPastTheUserDataLimit(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "raw")
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "CA", Prompt: "CA file", Optional: true, Type: spec.InputFile})
	big := encodeFileInput(bytes.Repeat([]byte("x"), 20<<10))
	err := v.create(VMCreate{Platform: "aws", Hostname: "edge"}, func(o *Options) { o.Set = map[string]string{"CA": big} })
	if err == nil || !strings.Contains(err.Error(), "give smaller files for CA") {
		t.Fatalf("%v", err)
	}
}

// An upgrade to a release that cannot read a recorded value, a text as a file or a file as text, in
// any mode, is refused before it writes anything, and a recorded empty value passes.
func TestAnUpgradeRefusesARecordedValueOfAnotherForm(t *testing.T) {
	t.Parallel()
	in := &Installer{R: testRelease(t, "2.0.0")}
	in.R.Spec.Inputs = append(in.R.Spec.Inputs,
		spec.Input{Key: "CA", Prompt: "CA file", Type: spec.InputPEM, When: "tls"},
		spec.Input{Key: "NOTE", Prompt: "A note"},
		spec.Input{Key: "CHART_NOTE", Prompt: "A note", Shapes: []string{"kubernetes"}})
	for _, c := range []struct {
		key, value, want string
	}{
		{"CA", "/etc/ssl/ca.pem", "CA: notes 2.0.0 declares it as a file"},
		{"CA", "admin123", "CA: notes 2.0.0 declares it as a file"},
		{"CA", encodeFileInput([]byte("not PEM")), "CA: notes 2.0.0 declares it as a file"},
		{"NOTE", encodeFileInput([]byte("x")), "NOTE: notes 2.0.0 declares it as text"},
		{"CA", "", ""},
		{"CA", encodeFileInput(testCert(t)), ""},
		{"NOTE", "hello", ""},
		{"CHART_NOTE", encodeFileInput([]byte("x")), ""},
	} {
		env := NewEnvFile()
		env.Set(c.key, c.value)
		err := in.recordedForms(env)
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s=%s: %v, want %q", c.key, c.value, err, c.want)
		}
		if c.want == "" && err != nil {
			t.Errorf("%s=%s: %v", c.key, c.value, err)
		}
	}
}

// A re-run on the newer release gives a file to what the previous one reads as text, the input
// recorded empty until then: a rollback keeps no value the previous release cannot read.
func TestARollbackTakesBackAValueItCannotRead(t *testing.T) {
	e := newEnv(t)
	e.docker.dump = "DUMP"
	e.schemaAt("5", nil, true)
	mod := func(typ string) func(*spec.Spec) {
		return func(s *spec.Spec) {
			s.Inputs = append(s.Inputs, spec.Input{Key: "CA", Prompt: "CA", Optional: true, Type: typ})
		}
	}
	e.r, e.self = testInstallerWith(t, "1.0.0", mod(""))
	if err := e.install(func(o *Options) { o.Set = map[string]string{"CA": ""} }); err != nil {
		t.Fatal(err)
	}
	e.r, e.self = testInstallerWith(t, "2.0.0", mod(spec.InputFile))
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if err := e.install(func(o *Options) { o.Set = map[string]string{"CA": encodeFileInput([]byte("x"))} }); err != nil {
		t.Fatal(err)
	}
	if err := e.rollback(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	env := e.readEnv()
	if got := get(env, "CA"); got != "" {
		t.Errorf("rolled back to %q", got)
	}
	if _, ok := env.Get(spec.FileVar("CA")); ok {
		t.Error("the release that reads it as text kept a file's path")
	}
}

// A rollback to a release that never recorded the input drops the value it cannot read, and says so.
func TestARollbackDropsAValueThePreviousReleaseNeverRecorded(t *testing.T) {
	e := newEnv(t)
	e.docker.dump = "DUMP"
	e.schemaAt("5", nil, true)
	e.r, e.self = testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "CA", Prompt: "CA", Advanced: true})
	})
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.r, e.self = testInstallerWith(t, "2.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "CA", Prompt: "CA", Optional: true, Type: spec.InputFile})
	})
	if err := e.upgrade(func(o *Options) { o.Set = map[string]string{"CA": encodeFileInput([]byte("x"))} }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if err := e.rollback(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if _, ok := e.readEnv().Get("CA"); ok {
		t.Error("the value stayed")
	}
	if !strings.Contains(e.log.String(), "CA goes back to the value notes 1.0.0 ran with") {
		t.Errorf("not said:\n%s", e.log)
	}
}

// A release requiring a file input recorded empty asks it, rather than start on an empty file.
func TestAnUpgradeAsksARequiredFileRecordedEmpty(t *testing.T) {
	t.Parallel()
	in := &Installer{R: testRelease(t, "2.0.0")}
	in.R.Spec.Inputs = append(in.R.Spec.Inputs, spec.Input{Key: "CA", Prompt: "CA", Required: true, Type: spec.InputFile})
	env := NewEnvFile()
	env.Set("CA", "")
	if added := in.addedInputs(env, Loopback); !slices.ContainsFunc(added, func(i input) bool { return i.Key == "CA" }) {
		t.Errorf("%v", added)
	}
}

func TestShownValue(t *testing.T) {
	t.Parallel()
	in := &Installer{R: &Release{Spec: &spec.Spec{Inputs: []spec.Input{{Key: "CA", Type: spec.InputFile}, {Key: "KEY", Type: spec.InputFile, Secret: true}}}}}
	secrets := map[string]bool{"KEY": true, "TOKEN": true}
	v := encodeFileInput([]byte("x"))
	for _, c := range []struct{ k, v, want string }{
		{"CA", v, fileInputDigest(v)},
		{"KEY", v, "<redacted>"},
		{"TOKEN", "t", "<redacted>"},
		{"TOKEN", "", ""},
		{spec.FileVar("KEY"), "./p", "<redacted>"},
		{spec.FileVar("CA"), "./p", "./p"},
		{"NAME", "n", "n"},
	} {
		if got := in.shownValue(c.k, c.v, secrets); got != c.want {
			t.Errorf("%s: %q, want %q", c.k, got, c.want)
		}
	}
}

// `upgrade --dry-run` shows a file input it adds by its digest; a rollback takes back a required
// file emptied since.
func TestFileInputsInAnUpgradesDryRunAndRollback(t *testing.T) {
	e := newEnv(t)
	e.docker.dump = "DUMP"
	e.schemaAt("5", nil, true)
	e.r, e.self = testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "CA", Prompt: "CA", Required: true, Type: spec.InputFile})
	})
	first := encodeFileInput([]byte("first"))
	if err := e.install(func(o *Options) { o.Set = map[string]string{"CA": first} }); err != nil {
		t.Fatal(err)
	}
	e.r, e.self = testInstallerWith(t, "2.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs,
			spec.Input{Key: "CA", Prompt: "CA", Optional: true, Type: spec.InputFile},
			spec.Input{Key: "ADDED", Prompt: "Added", Optional: true, Type: spec.InputFile})
	})
	added := encodeFileInput([]byte("added"))
	e.out.Reset()
	if err := e.upgrade(func(o *Options) { o.DryRun, o.Set = true, map[string]string{"ADDED": added} }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if out := e.out.String(); strings.Contains(out, added) || !strings.Contains(out, "ADDED="+fileInputDigest(added)+"\n") {
		t.Errorf("the dry run:\n%s", out)
	}
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if err := e.install(func(o *Options) { o.Set = map[string]string{"CA": ""} }); err != nil {
		t.Fatal(err)
	}
	if err := e.rollback(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := get(e.readEnv(), "CA"); got != first {
		t.Errorf("rolled back to %q", got)
	}
}
