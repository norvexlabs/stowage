package install

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

// maxEnvLine bounds a line ReadEnvFile reads, above any line the installer writes.
const maxEnvLine = 1 << 20

// EnvFile is an install's `.env`: KEY='value' lines in a stable order. Compose's dotenv reads a
// single-quoted value literally — no $ expansion, no # comment, no escapes — so a value is written
// as given; one holding a quote, a line break or a NUL cannot be written that way and is refused.
type EnvFile struct {
	keys []string
	vals map[string]string
}

// NewEnvFile returns an empty file.
func NewEnvFile() *EnvFile { return &EnvFile{vals: map[string]string{}} }

// Set sets key, keeping its first position.
func (e *EnvFile) Set(key, value string) {
	if _, ok := e.vals[key]; !ok {
		e.keys = append(e.keys, key)
	}
	e.vals[key] = value
}

// Delete removes key.
func (e *EnvFile) Delete(key string) {
	if _, ok := e.vals[key]; ok {
		delete(e.vals, key)
		e.keys = slices.DeleteFunc(e.keys, func(k string) bool { return k == key })
	}
}

// Get returns key's value.
func (e *EnvFile) Get(key string) (string, bool) {
	v, ok := e.vals[key]
	return v, ok
}

// Keys returns the keys in file order.
func (e *EnvFile) Keys() []string { return append([]string(nil), e.keys...) }

// ReadEnvFile reads a file written by WriteFile.
func ReadEnvFile(path string) (*EnvFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	e := NewEnvFile()
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(nil, maxEnvLine)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !providers.EnvName.MatchString(k) || len(v) < 2 || v[0] != '\'' || v[len(v)-1] != '\'' {
			return nil, fmt.Errorf("%s:%d: not KEY='value'", path, n)
		}
		e.Set(k, v[1:len(v)-1])
	}
	return e, sc.Err()
}

// WriteFile writes the file with mode 0600, replacing it atomically.
func (e *EnvFile) WriteFile(path string) error {
	if err := e.Check(); err != nil {
		return err
	}
	var b bytes.Buffer
	for _, k := range e.keys {
		fmt.Fprintf(&b, "%s='%s'\n", k, e.vals[k])
	}
	return writePrivate(path, b.Bytes())
}

// Check refuses a value the file cannot hold as written (spec.CheckEnvValue).
func (e *EnvFile) Check() error {
	for _, k := range e.keys {
		if err := spec.CheckEnvValue(e.vals[k]); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// Environ returns env without the keys the file records, so a shell export never overrides what
// the install recorded, then the file's own values.
func (e *EnvFile) Environ(env []string) []string {
	out := make([]string, 0, len(env)+len(e.keys))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, recorded := e.vals[k]; !recorded {
			out = append(out, kv)
		}
	}
	for _, k := range e.keys {
		out = append(out, k+"="+e.vals[k])
	}
	return out
}
