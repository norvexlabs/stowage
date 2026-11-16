// Package installerbin holds the installer executable (cmd/stowage-install), built for every
// platform of Platforms, that `ship` makes each release's installer image of. The release build
// embeds each one gzip-compressed with its sha256 and its size (build tag stowage_installer, see the
// Dockerfile); any other build carries none and cannot ship.
package installerbin

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/norvexlabs/stowage/internal/payload"
)

// Target is the platform an install runs on: the installer image's entry a target pulls, and the
// only platform the installer installs on.
const Target = "linux/amd64"

// Platforms are the platforms (os/arch) the installer is built for, Target first: the entries of
// every installer image, in its index's order. The Dockerfile's build stage builds this list, which
// cmd/platforms prints: it is the one list.
var Platforms = []string{Target, "linux/arm64", "darwin/amd64", "darwin/arm64"}

// MaxExecutable bounds one decompressed installer.
const MaxExecutable = 256 << 20

// MaxFile bounds an installer image's file: an executable with a payload appended (payload.Write),
// which an install reads back.
const MaxFile = MaxExecutable + payload.MaxAppended

// Installer is one platform's installer: the sha256 and the size of its executable, known without
// the executable (recorded when it was built), and the executable, streamed on each use and never
// kept: a release's installers decompressed are hundreds of MiB.
type Installer struct {
	// Digest is the executable's sha256, "sha256:<hex>".
	Digest string
	size   int64
	open   func() (io.ReadCloser, error)
	verify func() error
}

// Open streams the installer's executable: an embedded one decompressed as it is read, whose read
// at its end fails unless its sha256 is Digest and its size Size (MaxExecutable bytes or less).
func (i *Installer) Open() (io.ReadCloser, error) { return i.open() }

// Size is the size of the installer's executable as its build recorded it, read without the
// executable; Open refuses an executable of another size.
func (i *Installer) Size() int64 { return i.size }

// Verify reads the executable once (Open) to its end, which checks its sha256 and its size: once
// per run, its answer kept.
func (i *Installer) Verify() error { return i.verify() }

// Executable is the installer's executable read whole (Open), which the caller alone keeps.
func (i *Installer) Executable() ([]byte, error) {
	rc, err := i.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Of is the installer whose executable is exe, in hand.
func Of(exe []byte) *Installer {
	return &Installer{Digest: digestOf(exe), size: int64(len(exe)),
		open:   func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(exe)), nil },
		verify: func() error { return nil }}
}

func digestOf(exe []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(exe)) }

// Set is an installer per platform (os/arch).
type Set map[string]*Installer

// Digest is the sha256 of the executable of platform's installer, as Installer.Digest; that of no
// bytes when s has none (a build without installers).
func (s Set) Digest(platform string) string {
	if i := s[platform]; i != nil {
		return i.Digest
	}
	return digestOf(nil)
}

var digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Complete refuses a set that lacks a platform of Platforms, or holds one it does not name.
func (s Set) Complete() error {
	var missing []string
	for _, p := range Platforms {
		if i := s[p]; i == nil || !digestForm.MatchString(i.Digest) || i.Digest == digestOf(nil) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("no installer for %s", strings.Join(missing, ", "))
	}
	if len(s) != len(Platforms) {
		return fmt.Errorf("installers for %d platforms, not the %d of %s", len(s), len(Platforms), strings.Join(Platforms, ", "))
	}
	return nil
}

// File is the name of a platform's compressed installer in the embedded files.
func File(platform string) string { return base(platform) + ".gz" }

// SumFile is the name of the file holding the sha256 (in hex) of a platform's executable, beside
// File, written when it was built.
func SumFile(platform string) string { return base(platform) + ".sha256" }

// SizeFile is the name of the file holding the size in bytes (in decimal) of a platform's
// executable, beside File, written when it was built.
func SizeFile(platform string) string { return base(platform) + ".size" }

func base(platform string) string { return "stowage-install-" + strings.ReplaceAll(platform, "/", "-") }

// embedded holds the compressed installers; nil in a build without them.
var embedded fs.FS

// Executables are the embedded installers (Read), their recorded sha256 read once; nil in a build
// without them.
var Executables = sync.OnceValues(func() (Set, error) { return Read(embedded) })

// Read is the installer of each platform of Platforms in files (File, SumFile and SizeFile name
// each): its recorded sha256 and size read now, its executable decompressed on each use (Open) and
// checked against them; nil when files is nil.
func Read(files fs.FS) (Set, error) {
	if files == nil {
		return nil, nil
	}
	s := Set{}
	for _, p := range Platforms {
		raw, err := fs.ReadFile(files, SumFile(p))
		if err != nil {
			return nil, fmt.Errorf("the %s installer's sha256: %w", p, err)
		}
		digest := "sha256:" + strings.TrimSpace(string(raw))
		if !digestForm.MatchString(digest) {
			return nil, fmt.Errorf("the %s installer's sha256: %q is no sha256 in hex", p, strings.TrimSpace(string(raw)))
		}
		raw, err = fs.ReadFile(files, SizeFile(p))
		if err != nil {
			return nil, fmt.Errorf("the %s installer's size: %w", p, err)
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || size <= 0 || size > MaxExecutable {
			return nil, fmt.Errorf("the %s installer's size: %q is no size from 1 to %d bytes", p, strings.TrimSpace(string(raw)), int64(MaxExecutable))
		}
		open := func() (io.ReadCloser, error) {
			f, err := files.Open(File(p))
			if err != nil {
				return nil, fmt.Errorf("the %s installer: %w", p, err)
			}
			rc, err := decompress(f, "the "+p+" installer", digest, size, MaxExecutable)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("the %s installer: %w", p, err)
			}
			return rc, nil
		}
		s[p] = &Installer{Digest: digest, size: size, open: open, verify: sync.OnceValue(func() error {
			rc, err := open()
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.Copy(io.Discard, rc)
			return err
		})}
	}
	return s, s.Complete()
}

// decompress streams the gzip stream gz, the executable what names, decompressed: a read at its
// end fails past limit bytes, or unless they are size bytes whose sha256 is digest. Closing it
// closes gz.
func decompress(gz io.ReadCloser, what, digest string, size, limit int64) (io.ReadCloser, error) {
	zr, err := gzip.NewReader(gz)
	if err != nil {
		return nil, err
	}
	return &checked{r: io.LimitReader(zr, limit+1), c: gz, h: sha256.New(), what: what, digest: digest, size: size, limit: limit}, nil
}

// checked is an executable read as it is decompressed (decompress), checked at its end.
type checked struct {
	r      io.Reader
	c      io.Closer
	h      hash.Hash
	n      int64
	what   string
	digest string
	size   int64
	limit  int64
}

func (c *checked) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.h.Write(b[:n])
	c.n += int64(n)
	if err != io.EOF {
		return n, err
	}
	if c.n > c.limit {
		return n, fmt.Errorf("%s: more than %d bytes", c.what, c.limit)
	}
	if c.n != c.size {
		return n, fmt.Errorf("%s: its executable is %d bytes, and the build recorded %d: rebuild stowage (make build)", c.what, c.n, c.size)
	}
	if got := fmt.Sprintf("sha256:%x", c.h.Sum(nil)); got != c.digest {
		return n, fmt.Errorf("%s: its executable's sha256 is %s, and the build recorded %s: rebuild stowage (make build)", c.what, got, c.digest)
	}
	return n, io.EOF
}

func (c *checked) Close() error { return c.c.Close() }
