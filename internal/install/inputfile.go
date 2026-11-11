package install

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
)

// InputFilesDir holds, under the install directory, the file of each file input and of each secret
// delivered as a file. It is 0700, and each file 0644: a bind mount gives a container the file
// alone, whatever uid it runs as, and the directory keeps the host's other users out.
const InputFilesDir = ".stowage-inputs"

// HookInputsDir holds, inside the provision hook's container, the file of each file input its env
// names, under the input's key.
const HookInputsDir = "/run/stowage/inputs"

// fileValuePrefix starts the value recorded for a file: base64 alone could not tell a file from a
// text that happens to read as base64, whether mistyped or left by an earlier release.
const fileValuePrefix = "stowage-file:"

// encodeFileInput is the value recorded for a file's content: one line, as a recorded value is.
func encodeFileInput(content []byte) string {
	return fileValuePrefix + base64.StdEncoding.EncodeToString(content)
}

// isFileValue is whether v is what is recorded for a file.
func isFileValue(v string) bool { return strings.HasPrefix(v, fileValuePrefix) }

// decodeFileInput is the content of a recorded value; empty for an input left empty.
func decodeFileInput(v string) ([]byte, error) {
	if v == "" {
		return nil, nil
	}
	encoded, ok := strings.CutPrefix(v, fileValuePrefix)
	content, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if !ok || err != nil {
		return nil, errors.New("not a file's content: give the file with --set-file KEY=PATH, or KEY: {file: PATH} in --answers")
	}
	return content, nil
}

// inputForm is what the input i holds, for a message.
func inputForm(i spec.Input) string {
	if i.File() {
		return "a file"
	}
	return "text"
}

// wrongForm is whether the input i, as a release declares it, cannot read the value v.
func wrongForm(i spec.Input, v string) bool {
	if i.File() {
		return fileInputCheck(i)(v) != nil
	}
	return isFileValue(v)
}

// textInputCheck refuses a file for a text input, which would read its recorded form.
func textInputCheck(v string) error {
	if isFileValue(v) {
		return errors.New("is no file input of this release")
	}
	return nil
}

// recordedForms refuses an upgrade to a release declaring as a file what is recorded as text, or
// the other way, in any mode: the value cannot be carried over, and an input a release changes
// so is a new key to it, which the upgrade asks as any it adds.
func (in *Installer) recordedForms(env *EnvFile) error {
	for _, i := range in.R.Spec.Inputs {
		// A file input is written on every start, a text one read on its shapes alone.
		if len(i.Shapes) > 0 && !i.File() && !slices.Contains(i.Shapes, in.shape()) {
			continue
		}
		if v, ok := env.Get(i.Key); ok && wrongForm(i, v) {
			return in.unreadable(i)
		}
	}
	return nil
}

// unreadable refuses a release that cannot read what is recorded for the input i.
func (in *Installer) unreadable(i spec.Input) error {
	return refuse("stay on the installed release: the project declares a changed input under a new key", "%s: %s %s declares it as %s, which cannot read the value recorded: the project must declare a changed input under a new key", i.Key, in.R.Spec.Name, in.R.Manifest.ProjectVersion, inputForm(i))
}

// loadFileInput is the value recorded for the file at path, at most spec.MaxInputFile bytes.
func loadFileInput(path string) (string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, spec.MaxInputFile+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if len(content) > spec.MaxInputFile {
		return "", fmt.Errorf("%s: a file input is at most %d bytes", path, spec.MaxInputFile)
	}
	if len(content) == 0 {
		return "", fmt.Errorf("%s: an empty file (--set KEY= leaves an optional input empty)", path)
	}
	return encodeFileInput(content), nil
}

// fileInputCheck accepts what the input i records: nothing, or the recorded form of a file it takes.
func fileInputCheck(i spec.Input) func(string) error {
	return func(v string) error {
		if v == "" {
			return nil
		}
		content, err := decodeFileInput(v)
		if err != nil {
			return err
		}
		if len(content) == 0 {
			return errors.New("an empty file (--set KEY= leaves an optional input empty)")
		}
		if len(content) > spec.MaxInputFile {
			return fmt.Errorf("a file input is at most %d bytes", spec.MaxInputFile)
		}
		if i.Type == spec.InputPEM {
			return checkPEM(content)
		}
		return nil
	}
}

// checkPEM accepts PEM blocks and nothing after the last; text before or between them is taken, as a
// bundle's comments are by the tools reading it. Every "-----BEGIN " must open a block that decodes,
// as a decoder skips one that does not. An error names a block by its position and type, never its
// content.
func checkPEM(content []byte) error {
	opened := bytes.Count(content, []byte("-----BEGIN "))
	rest := content
	for n := 1; ; n++ {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			switch {
			case n == 1:
				return errors.New("no PEM block")
			case len(bytes.TrimSpace(rest)) > 0:
				return fmt.Errorf("text after PEM block %d", n-1)
			case opened != n-1:
				return fmt.Errorf("%d PEM blocks open, and %d of them decode", opened, n-1)
			}
			return nil
		}
		if err := checkPEMBlock(block); err != nil {
			return fmt.Errorf("PEM block %d (%s): %w", n, block.Type, err)
		}
	}
}

// checkPEMBlock refuses an encrypted key, as nothing could give a service its passphrase, and a
// certificate or key that is not well-formed DER. It parses no further: the service's library may
// well read a curve or an algorithm this one does not.
func checkPEMBlock(b *pem.Block) error {
	errEncrypted := errors.New("an encrypted key: give it without its passphrase")
	if strings.Contains(b.Type, "ENCRYPTED") || strings.Contains(b.Headers["Proc-Type"], "ENCRYPTED") {
		return errEncrypted
	}
	switch b.Type {
	case "CERTIFICATE", "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY":
		var der asn1.RawValue
		if rest, err := asn1.Unmarshal(b.Bytes, &der); err != nil || len(rest) > 0 {
			return errors.New("does not parse")
		}
	case "OPENSSH PRIVATE KEY":
		// openssh-key-v1: the magic, then the cipher's name, a length-prefixed string.
		const magic = "openssh-key-v1\x00"
		body, ok := bytes.CutPrefix(b.Bytes, []byte(magic))
		if !ok || len(body) < 4 {
			return errors.New("does not parse")
		}
		n := binary.BigEndian.Uint32(body)
		if uint64(n) > uint64(len(body)-4) {
			return errors.New("does not parse")
		}
		if string(body[4:4+n]) != "none" {
			return errEncrypted
		}
	}
	return nil
}

// fileInputDigest shows a file input's value in a log: its value is the file.
func fileInputDigest(v string) string {
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return "sha256:" + hex.EncodeToString(sum[:6])
}

// fileInputName is the file of the input key holding v. The value's digest is in the name, so a
// new value is a new path: compose recreates the services mounting it.
func fileInputName(key, v string) string {
	sum := sha256.Sum256([]byte(v))
	return key + "-" + hex.EncodeToString(sum[:6])
}

// mountedFile is a file the installer writes under InputFilesDir for a service to mount: a file
// input's content, or the value of a secret delivered as a file (as_file).
type mountedFile struct {
	Key string
	Var string // the variable holding its path
	// secret: the value is written as it is recorded; else it is a file input's recorded form.
	secret bool
}

// mountedFiles are the files this release's services mount: each file input's, then each secret
// delivered as a file.
func (in *Installer) mountedFiles() []mountedFile {
	var files []mountedFile
	for _, i := range in.R.Spec.Inputs {
		if i.File() {
			files = append(files, mountedFile{Key: i.Key, Var: spec.FileVar(i.Key)})
		}
	}
	for _, k := range in.R.Spec.AsFileKeys() {
		files = append(files, mountedFile{Key: k, Var: spec.SecretFileVar(k), secret: true})
	}
	return files
}

// FileSaltVar is the install's random key the name of a secret's file is made with (secretFileName):
// recorded in .env, never logged or shown.
const FileSaltVar = "STOWAGE_FILE_SALT"

// name is f's file for the recorded value v: a new value a new name, so compose recreates what
// mounts it. A file input's is named by the value's digest (fileInputName); a secret's by an HMAC
// keyed with the install's salt (env's FileSaltVar), so its name, which docker inspect and the
// container's mounts show, tells nothing of the value.
func (f mountedFile) name(env *EnvFile, v string) string {
	if !f.secret {
		return fileInputName(f.Key, v)
	}
	salt, _ := env.Get(FileSaltVar)
	m := hmac.New(sha256.New, []byte(salt))
	m.Write([]byte(v))
	return f.Key + "-" + hex.EncodeToString(m.Sum(nil)[:6])
}

// content is what f's file holds for the recorded value v.
func (f mountedFile) content(v string) ([]byte, error) {
	if f.secret {
		return []byte(v), nil
	}
	return decodeFileInput(v)
}

// fileVars records each mounted file's path, relative to the install directory compose resolves it
// against, so a moved directory keeps working; and forgets the path of a file this release no
// longer declares. Every one has one, in any mode: a service mounts it whatever was asked.
func (in *Installer) fileVars(env *EnvFile) {
	files := in.mountedFiles()
	if _, ok := env.Get(FileSaltVar); !ok && slices.ContainsFunc(files, func(f mountedFile) bool { return f.secret }) {
		b := make([]byte, 16)
		_, _ = io.ReadFull(in.O.rand(), b) // crypto/rand never fails on Linux
		env.Set(FileSaltVar, hex.EncodeToString(b))
	}
	for _, k := range env.Keys() {
		if strings.HasPrefix(k, spec.FileVarPrefix) || strings.HasPrefix(k, spec.SecretFileVarPrefix) {
			if !slices.ContainsFunc(files, func(f mountedFile) bool { return f.Var == k }) {
				env.Delete(k)
			}
		}
	}
	for _, f := range files {
		v, _ := env.Get(f.Key)
		env.Set(f.Var, "./"+InputFilesDir+"/"+f.name(env, v))
	}
}

// writeInputFiles writes each mounted file for the start to mount; an empty value writes an
// empty file, as the engine would make a directory of a path it does not find.
func (in *Installer) writeInputFiles(env *EnvFile) error {
	dir := in.path(InputFilesDir)
	files := in.mountedFiles()
	if len(files) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for _, f := range files {
		v, _ := env.Get(f.Key)
		content, err := f.content(v)
		if err != nil {
			return fmt.Errorf("input %s: its recorded value: %w", f.Key, err)
		}
		if err := writeAtomic(filepath.Join(dir, f.name(env, v)), bytes.NewReader(content), 0o644, nil); err != nil {
			return fmt.Errorf("%s: %w", f.Key, err)
		}
	}
	return nil
}

// sweepInputFiles removes the files no input holds any more, once the stack started on the ones it
// does: until then a container may need the path it was made with to restart.
func (in *Installer) sweepInputFiles(env *EnvFile) error {
	dir := in.path(InputFilesDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var keep []string
	for _, f := range in.mountedFiles() {
		v, _ := env.Get(f.Key)
		keep = append(keep, f.name(env, v))
	}
	var errs []error
	for _, e := range entries {
		if !slices.Contains(keep, e.Name()) {
			errs = append(errs, os.Remove(filepath.Join(dir, e.Name())))
		}
	}
	if len(keep) == 0 {
		errs = append(errs, os.Remove(dir))
	}
	return errors.Join(errs...)
}

// addSetFiles gives set what --set-file gives, KEY=PATH items; one input given by both is refused.
// Whether the input takes a file is the check of the installer that runs, as a fetched release's
// judges its own inputs.
func addSetFiles(set map[string]string, items []string) error {
	for _, it := range items {
		k, path, ok := strings.Cut(it, "=")
		if !ok || k == "" || path == "" {
			return fmt.Errorf("--set-file: %q is not KEY=PATH", it)
		}
		if _, ok := set[k]; ok {
			return fmt.Errorf("--set and --set-file both give %s, or --set-file twice", k)
		}
		v, err := loadFileInput(path)
		if err != nil {
			return fmt.Errorf("--set-file %s: %w", k, err)
		}
		set[k] = v
	}
	return nil
}

// readAnswers reads an --answers file: KEY: value, or KEY: {file: PATH} for a file input, the path
// absolute or beside the file; and its reserved tls section apart from the inputs, nil without one.
func readAnswers(path string) (map[string]string, *answersTLS, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, nil, err
	}
	var nodes map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &nodes); err != nil {
		return nil, nil, fmt.Errorf("--answers %s: a map of KEY: value is expected: %w", path, err)
	}
	m := make(map[string]string, len(nodes))
	var section *answersTLS
	for _, k := range sortedKeys(nodes) {
		n := nodes[k]
		if k == answersTLSKey {
			if section, err = readAnswersTLS(path, &n); err != nil {
				return nil, nil, err
			}
			continue
		}
		if n.Kind != yaml.MappingNode {
			var v string
			if err := n.Decode(&v); err != nil {
				return nil, nil, fmt.Errorf("--answers %s: %s: a value is expected: %w", path, k, err)
			}
			m[k] = v
			continue
		}
		var form struct {
			File string `yaml:"file"`
		}
		if err := n.Decode(&form); err != nil || form.File == "" || len(n.Content) != 2 {
			return nil, nil, fmt.Errorf("--answers %s: %s: a value or {file: PATH} is expected", path, k)
		}
		file, err := answersPath(path, form.File)
		if err == nil {
			m[k], err = loadFileInput(file)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("--answers %s: %s: %w", path, k, err)
		}
	}
	return m, section, nil
}

// answersTLSKey is the reserved section of an answers file holding the front's TLS settings. An
// input's key is upper case (providers.EnvName), so no input can take this name.
const answersTLSKey = "tls"

// answersTLS is an answers file's tls section: what --tls, --cert, --key, --ca and --client-ca
// give, its paths absolute, so a resolved path can never read as the word `none`.
type answersTLS struct {
	Mode, Cert, Key, CA, ClientCA string
}

// answersPath resolves a path an answers file names: absolute as it is, else beside the file.
func answersPath(answers, file string) (string, error) {
	if filepath.IsAbs(file) {
		return file, nil
	}
	abs, err := filepath.Abs(answers)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(abs), file), nil
}

// readAnswersTLS decodes the section strictly: a field it does not know, given twice or holding no
// value is an error, as is a mode no project can offer, each named by the file and field to fix.
// `none` for ca or client_ca stays the word those flags take.
func readAnswersTLS(path string, n *yaml.Node) (*answersTLS, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("--answers %s: %s: a map of mode, cert, key, ca and client_ca is expected", path, answersTLSKey)
	}
	var t answersTLS
	fields := map[string]*string{"mode": &t.Mode, "cert": &t.Cert, "key": &t.Key, "ca": &t.CA, "client_ca": &t.ClientCA}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		name := n.Content[i].Value
		field, known := fields[name]
		if !known {
			return nil, fmt.Errorf("--answers %s: %s: %s is no field of it (mode, cert, key, ca, client_ca)", path, answersTLSKey, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("--answers %s: %s: %s is given twice", path, answersTLSKey, name)
		}
		seen[name] = true
		var v string
		if err := n.Content[i+1].Decode(&v); err != nil || v == "" {
			return nil, fmt.Errorf("--answers %s: %s: %s: a value is expected", path, answersTLSKey, name)
		}
		if name == "mode" {
			if _, ok := modeLabels[v]; !ok {
				return nil, fmt.Errorf("--answers %s: %s: mode: %s is no TLS mode (%s)", path, answersTLSKey, v, strings.Join(sortedKeys(modeLabels), ", "))
			}
			*field = v
			continue
		}
		if (name == "ca" || name == "client_ca") && v == noCA {
			*field = v
			continue
		}
		file, err := answersPath(path, v)
		if err != nil {
			return nil, fmt.Errorf("--answers %s: %s: %s: %w", path, answersTLSKey, name, err)
		}
		*field = file
	}
	return &t, nil
}

// takeAnswersTLS gives the options the section's settings where no flag decides them, and returns
// the flags it stood in for and the fields it left out. A flag wins whole, not field by field: a
// certificate given replaces the section's mode, certificate, key and CA, and a mode other than byo
// leaves out its certificate, key and CA, as they would be refused with it. The client CA stands
// alone, and a domain given replaces the section's loopback mode, which serves none. Only a file the run will read must exist, so a flag replaces a stale path; one that cannot
// be read for another reason is left to the checks a flag's file gets.
func (o *Options) takeAnswersTLS(path string, t answersTLS) (taken, left []string, err error) {
	byFlagCert := o.Cert != "" || o.Key != ""
	byFlagMode := o.TLS != "" && o.TLS != "byo"
	for _, f := range []struct {
		name, flag, value string
		into              *string
		out               bool
		file              bool
	}{
		{"mode", "--tls", t.Mode, &o.TLS, byFlagCert || (t.Mode == Loopback && len(o.Domains) > 0), false},
		{"cert", "--cert", t.Cert, &o.Cert, byFlagCert || byFlagMode, true},
		{"key", "--key", t.Key, &o.Key, byFlagCert || byFlagMode, true},
		{"ca", "--ca", t.CA, &o.CA, byFlagCert || byFlagMode, true},
		{"client_ca", "--client-ca", t.ClientCA, &o.ClientCA, false, true},
	} {
		switch {
		case f.value == "":
			continue
		case f.out || *f.into != "":
			left = append(left, f.name)
			continue
		}
		if f.file && f.value != noCA {
			if _, err := os.Stat(f.value); errors.Is(err, os.ErrNotExist) {
				return nil, nil, fmt.Errorf("--answers %s: %s: %s: %w", path, answersTLSKey, f.name, err)
			}
		}
		*f.into = f.value
		taken = append(taken, f.flag)
	}
	return taken, left, nil
}

// readAnswersFile reads --answers into the options. A command that takes no TLS settings refuses
// the file's tls section, naming the fix, before anything in it is judged by the files it names.
// The run says which flags the section stands in for, as a later refusal names those flags, and
// which of its fields a flag left out.
func (o *Options) readAnswersFile(path string, takesTLS bool) error {
	inputs, section, err := readAnswers(path)
	if err != nil {
		return err
	}
	o.Answers = inputs
	if section == nil {
		return nil
	}
	if !takesTLS {
		return refuse("remove the "+answersTLSKey+" section from "+path, "--answers %s: its %s section gives the front's TLS settings, which only a Docker install, a VM's `install` or `vm create` takes, and their re-runs (`upgrade` keeps what is recorded, and on Kubernetes the chart's ingress serves the routes): remove the section for this command", path, answersTLSKey)
	}
	taken, left, err := o.takeAnswersTLS(path, *section)
	if err != nil {
		return err
	}
	if len(taken) > 0 {
		o.logf("the %s section of %s gives %s", answersTLSKey, path, strings.Join(taken, ", "))
	}
	if len(left) > 0 {
		o.logf("the %s section of %s: %s left out, as the flags given decide", answersTLSKey, path, strings.Join(left, ", "))
	}
	return nil
}

// fileInputsFit refuses a cloud-config a file input carries past what the cloud takes as user data:
// the operator would learn it only as the instance is launched, the image work done.
func fileInputsFit(p Platform, s *spec.Spec, st *Settings, size int) error {
	if p.UserDataMax == 0 || size <= p.UserDataMax {
		return nil
	}
	var files []string
	for _, k := range sortedKeys(st.Inputs) {
		if i, ok := s.Input(k); ok && i.File() {
			files = append(files, k)
		}
	}
	if len(files) == 0 {
		return nil
	}
	return fmt.Errorf("the cloud-config is %d bytes, and %s takes %d as user data: give smaller files for %s; an optional one can be left out and given over SSH once the VM answers, with `install --set-file`", size, p.Label, p.UserDataMax, strings.Join(files, ", "))
}

// shownValue is the recorded value of k as --dry-run prints it: a secret redacted, as is the path of
// a secret file input, which holds its digest; a file input by its digest.
func (in *Installer) shownValue(k, v string, secrets map[string]bool) string {
	if secrets[k] && v != "" {
		return "<redacted>"
	}
	// In the recorded form whatever this release declares: a value it replaces may be one.
	if i, ok := in.R.Spec.Input(k); (ok && i.File()) || isFileValue(v) {
		return fileInputDigest(v)
	}
	if key, ok := strings.CutPrefix(k, spec.FileVarPrefix); ok && secrets[key] {
		return "<redacted>"
	}
	if strings.HasPrefix(k, spec.SecretFileVarPrefix) || k == FileSaltVar {
		return "<redacted>"
	}
	return v
}
