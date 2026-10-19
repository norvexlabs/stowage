// Package payload is what an installer binary carries about one release: the files an install
// needs, appended to the executable itself, so the one file an operator downloads works offline.
//
// Layout: <executable> <tar of the files> <8-byte big-endian tar length> <Magic>.
package payload

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

// Magic ends every installer that carries a payload.
const Magic = "STOWAGE-PAYLOAD1"

// MaxSize bounds a payload: a compose file, a manifest and a few keys, never images.
const MaxSize = 16 << 20

const trailer = int64(8 + len(Magic))

// Well-known files.
const (
	Release = "release.json" // the release manifest, as published
	// Bundled is the release as an OS bundle carries it, in Release's place: the manifest without
	// its publication (release.Manifest.EncodeBundled).
	Bundled = "bundled.json"
	Spec    = "spec.json"    // the spec's runtime view
	Compose = "compose.yaml" // the project's compose file
	KeysDir = "keys/"        // trusted public keys, by their spec path's base name
	Pull    = "pull.json"    // the project's pull account (Account), when registry.pull is a token
)

// Account is a registry login baked into an installer: the project's read-only pull account.
type Account struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// Files maps a payload path (slash-separated, relative) to its content.
type Files map[string][]byte

// Write writes exe followed by files as a payload.
func Write(w io.Writer, exe io.Reader, files Files) error {
	if _, err := io.Copy(w, exe); err != nil {
		return err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		if err := checkName(n); err != nil {
			return err
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if buf.Len() > MaxSize {
		return fmt.Errorf("payload: %d bytes, more than %d", buf.Len(), MaxSize)
	}
	var tail [trailer]byte
	binary.BigEndian.PutUint64(tail[:8], uint64(buf.Len()))
	copy(tail[8:], Magic)
	if _, err := w.Write(buf.Bytes()); err != nil {
		return err
	}
	_, err := w.Write(tail[:])
	return err
}

// ErrNone is returned by Read for an executable that carries no payload.
var ErrNone = errors.New("this installer carries no release: download it from the project's installer image")

// Read reads the payload at the end of the file at p.
func Read(p string) (Files, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() < trailer {
		return nil, ErrNone
	}
	var tail [trailer]byte
	if _, err := f.ReadAt(tail[:], fi.Size()-trailer); err != nil {
		return nil, err
	}
	if string(tail[8:]) != Magic {
		return nil, ErrNone
	}
	n := binary.BigEndian.Uint64(tail[:8])
	if n > MaxSize || int64(n) > fi.Size()-trailer {
		return nil, fmt.Errorf("payload: corrupt length %d", n)
	}
	tr := tar.NewReader(io.NewSectionReader(f, fi.Size()-trailer-int64(n), int64(n)))
	files := Files{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("payload: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("payload: %s is not a regular file", h.Name)
		}
		if err := checkName(h.Name); err != nil {
			return nil, err
		}
		if _, dup := files[h.Name]; dup {
			return nil, fmt.Errorf("payload: %s twice", h.Name)
		}
		b, err := io.ReadAll(io.LimitReader(tr, MaxSize))
		if err != nil {
			return nil, fmt.Errorf("payload: %w", err)
		}
		files[h.Name] = b
	}
}

// checkName refuses a path that is absolute, not clean or leaves the payload.
func checkName(n string) error {
	if n == "" || path.IsAbs(n) || path.Clean(n) != n || n == ".." || strings.HasPrefix(n, "../") || strings.Contains(n, "\\") {
		return fmt.Errorf("payload: %q is not a clean relative path", n)
	}
	return nil
}
