package install

import (
	"bytes"
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

// InputFilesDir holds, under the install directory, the file of each file input. It is 0700, and
// each file 0644: a bind mount gives a container the file alone, whatever uid it runs as, and the
// directory keeps the host's other users out.
const InputFilesDir = ".stowage-inputs"

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
			return fmt.Errorf("%s: %s %s declares it as %s, which cannot read the value recorded: the project must declare a changed input under a new key", i.Key, in.R.Spec.Name, in.R.Manifest.ProjectVersion, inputForm(i))
		}
	}
	return nil
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

// fileVars records each file input's path, relative to the install directory compose resolves it
// against, so a moved directory keeps working; and forgets the path of an input this release no
// longer declares. Every file input has one, in any mode: a service mounts it whatever was asked.
func (in *Installer) fileVars(env *EnvFile) {
	for _, k := range env.Keys() {
		if key, ok := strings.CutPrefix(k, spec.FileVarPrefix); ok {
			if i, found := in.R.Spec.Input(key); !found || !i.File() {
				env.Delete(k)
			}
		}
	}
	for _, i := range in.R.Spec.Inputs {
		if i.File() {
			v, _ := env.Get(i.Key)
			env.Set(spec.FileVar(i.Key), "./"+InputFilesDir+"/"+fileInputName(i.Key, v))
		}
	}
}

// writeInputFiles writes each file input's file for the start to mount; an empty value writes an
// empty file, as the engine would make a directory of a path it does not find.
func (in *Installer) writeInputFiles(env *EnvFile) error {
	dir := in.path(InputFilesDir)
	if !slices.ContainsFunc(in.R.Spec.Inputs, spec.Input.File) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for _, i := range in.R.Spec.Inputs {
		if !i.File() {
			continue
		}
		v, _ := env.Get(i.Key)
		content, err := decodeFileInput(v)
		if err != nil {
			return fmt.Errorf("input %s: its recorded value: %w", i.Key, err)
		}
		if err := writeAtomic(filepath.Join(dir, fileInputName(i.Key, v)), bytes.NewReader(content), 0o644, nil); err != nil {
			return fmt.Errorf("input %s: %w", i.Key, err)
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
	for _, i := range in.R.Spec.Inputs {
		if i.File() {
			v, _ := env.Get(i.Key)
			keep = append(keep, fileInputName(i.Key, v))
		}
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
// relative to the answers file.
func readAnswers(path string) (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var nodes map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &nodes); err != nil {
		return nil, fmt.Errorf("--answers %s: a map of KEY: value is expected: %w", path, err)
	}
	m := make(map[string]string, len(nodes))
	for _, k := range sortedKeys(nodes) {
		n := nodes[k]
		if n.Kind != yaml.MappingNode {
			var v string
			if err := n.Decode(&v); err != nil {
				return nil, fmt.Errorf("--answers %s: %s: a value is expected: %w", path, k, err)
			}
			m[k] = v
			continue
		}
		var form struct {
			File string `yaml:"file"`
		}
		if err := n.Decode(&form); err != nil || form.File == "" || len(n.Content) != 2 {
			return nil, fmt.Errorf("--answers %s: %s: a value or {file: PATH} is expected", path, k)
		}
		file := form.File
		if !filepath.IsAbs(file) {
			file = filepath.Join(filepath.Dir(path), file)
		}
		if m[k], err = loadFileInput(file); err != nil {
			return nil, fmt.Errorf("--answers %s: %s: %w", path, k, err)
		}
	}
	return m, nil
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
	if i, ok := in.R.Spec.Input(k); ok && i.File() {
		return fileInputDigest(v)
	}
	if key, ok := strings.CutPrefix(k, spec.FileVarPrefix); ok && secrets[key] {
		return "<redacted>"
	}
	return v
}
