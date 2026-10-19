// Package lock reads stowage.lock, the committed pin of the exact Stowage a project ships with.
package lock

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/version"
)

// FileName is the lock file's name, next to stowage.yaml.
const FileName = "stowage.lock"

// Lock pins the Stowage version: the installer, the front's and backup runner's recipes and
// every other runtime piece are embedded in that version's binary.
type Lock struct {
	Stowage string `yaml:"stowage"`
}

// Load reads a lock file. A missing file returns (nil, nil): the project has not pinned a
// Stowage yet.
func Load(path string) (*Lock, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var l Lock
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	if l.Stowage == "" {
		return nil, fmt.Errorf("%s: stowage version is missing", FileName)
	}
	if err := (version.SemVer{}).Validate(l.Stowage); err != nil {
		return nil, fmt.Errorf("%s: stowage: %w", FileName, err)
	}
	return &l, nil
}

// Compare orders the release version v against the pin, as SemVer: < 0 when v is older, 0 when
// v is the pinned version. A v that is not a SemVer release is an error.
func (l *Lock) Compare(v string) (int, error) {
	if err := (version.SemVer{}).Validate(v); err != nil {
		return 0, err
	}
	return version.SemVer{}.Compare(v, l.Stowage)
}

// Write stores the lock at path, replacing the file atomically: a header, then the keys in a
// stable order.
func Write(path string, l *Lock) error {
	if err := write(path, l); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func write(path string, l *Lock) error {
	body, err := yaml.Marshal(l)
	if err != nil {
		return err
	}
	raw := append([]byte("# Written by stowage lock: the Stowage this project publishes with. Move it with stowage lock --update.\n"), body...)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".stowage.lock-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// The rename is durable once the directory is synced; a filesystem that cannot sync a
	// directory still holds the new file, so that is not a failure.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}
