package installerbin

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) []byte { return fmt.Appendf(nil, "%x\n", sha256.Sum256(b)) }

// files is the embedded files of a build whose installer for each platform is "exe <platform>".
func files(t *testing.T) fstest.MapFS {
	files := fstest.MapFS{}
	for _, p := range Platforms {
		exe := []byte("exe " + p)
		files[File(p)] = &fstest.MapFile{Data: gz(t, exe)}
		files[SumFile(p)] = &fstest.MapFile{Data: sum(exe)}
		files[SizeFile(p)] = &fstest.MapFile{Data: fmt.Appendf(nil, "%d\n", len(exe))}
	}
	return files
}

func TestReadDecompressesEachPlatformOnUse(t *testing.T) {
	t.Parallel()
	fs := files(t)
	s, err := Read(fs)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range Platforms {
		if want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("exe "+p))); s[p].Digest != want || s.Digest(p) != want {
			t.Fatalf("%s: digest %s, want %s", p, s[p].Digest, want)
		}
	}
	// The digests are the recorded ones, read without a decompression: a platform's compressed
	// file is read on each use, and its executable never kept.
	fs[File("darwin/arm64")] = &fstest.MapFile{Data: []byte("not gzip")}
	if exe, err := s[Target].Executable(); err != nil || string(exe) != "exe "+Target {
		t.Fatalf("%s: %q %v", Target, exe, err)
	}
	delete(fs, File(Target))
	if _, err := s[Target].Executable(); err == nil {
		t.Fatalf("%s read again without its file: its executable was kept", Target)
	}
	// The size is the recorded one, read without the executable.
	if n := s[Target].Size(); n != int64(len("exe "+Target)) {
		t.Fatalf("%s: size %d", Target, n)
	}
	if err := s[Target].Verify(); err == nil {
		t.Fatalf("%s verified without its file", Target)
	}
	if _, err := s["darwin/arm64"].Executable(); err == nil || !strings.Contains(err.Error(), "darwin/arm64") {
		t.Fatalf("not gzip: %v", err)
	}
	if s, err := Read(nil); s != nil || err != nil {
		t.Fatalf("a build without installers: %v %v", s, err)
	}
	if Set(nil).Digest(Target) != fmt.Sprintf("sha256:%x", sha256.Sum256(nil)) {
		t.Error("no installer's digest is not that of no bytes")
	}
}

// An executable whose sha256 is not the one its build recorded is refused when it is decompressed.
func TestARecordedSumOfOtherBytesIsRefused(t *testing.T) {
	t.Parallel()
	fs := files(t)
	fs[SumFile("linux/arm64")] = &fstest.MapFile{Data: sum([]byte("other"))}
	s, err := Read(fs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s["linux/arm64"].Executable(); err == nil || !strings.Contains(err.Error(), "the linux/arm64 installer: its executable's sha256") {
		t.Fatalf("a mismatching sum: %v", err)
	}
	if err := s["linux/arm64"].Verify(); err == nil || !strings.Contains(err.Error(), "the build recorded") {
		t.Fatalf("a mismatching sum, verified: %v", err)
	}
	if _, err := s[Target].Executable(); err != nil {
		t.Fatalf("another platform's: %v", err)
	}
}

// An executable of another size than its build recorded is refused when it is decompressed, its
// sha256 right or not.
func TestARecordedSizeOfOtherBytesIsRefused(t *testing.T) {
	t.Parallel()
	fs := files(t)
	fs[SizeFile("linux/arm64")] = &fstest.MapFile{Data: fmt.Appendf(nil, "%d\n", len("exe linux/arm64")+1)}
	s, err := Read(fs)
	if err != nil {
		t.Fatal(err)
	}
	if err := s["linux/arm64"].Verify(); err == nil || !strings.Contains(err.Error(), "the linux/arm64 installer: its executable is 15 bytes, and the build recorded 16") {
		t.Fatalf("a mismatching size: %v", err)
	}
	if err := s[Target].Verify(); err != nil {
		t.Fatalf("another platform's: %v", err)
	}
}

func TestReadRefusesAMissingOrMalformedPlatform(t *testing.T) {
	t.Parallel()
	for what, mod := range map[string]func(fstest.MapFS){
		"no sum":        func(fs fstest.MapFS) { delete(fs, SumFile("darwin/arm64")) },
		"a sum not hex": func(fs fstest.MapFS) { fs[SumFile("darwin/arm64")] = &fstest.MapFile{Data: []byte("xyz\n")} },
		"an empty sum":  func(fs fstest.MapFS) { fs[SumFile("darwin/arm64")] = &fstest.MapFile{} },
		"no size":       func(fs fstest.MapFS) { delete(fs, SizeFile("darwin/arm64")) },
		"a size not decimal": func(fs fstest.MapFS) {
			fs[SizeFile("darwin/arm64")] = &fstest.MapFile{Data: []byte("0x10\n")}
		},
		"a zero size": func(fs fstest.MapFS) { fs[SizeFile("darwin/arm64")] = &fstest.MapFile{Data: []byte("0\n")} },
		"a size past the cap": func(fs fstest.MapFS) {
			fs[SizeFile("darwin/arm64")] = &fstest.MapFile{Data: fmt.Appendf(nil, "%d\n", MaxExecutable+1)}
		},
	} {
		fs := files(t)
		mod(fs)
		if _, err := Read(fs); err == nil || !strings.Contains(err.Error(), "darwin/arm64") {
			t.Errorf("%s: %v", what, err)
		}
	}
}

// The cap on a decompressed executable takes one of its size and refuses one byte more.
func TestDecompressCapsTheExecutable(t *testing.T) {
	t.Parallel()
	const limit = 1 << 10
	read := func(exe []byte) ([]byte, error) {
		rc, err := decompress(io.NopCloser(bytes.NewReader(gz(t, exe))), "x", fmt.Sprintf("sha256:%x", sha256.Sum256(exe)), int64(len(exe)), limit)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	exe := bytes.Repeat([]byte{'x'}, limit)
	if got, err := read(exe); err != nil || len(got) != limit {
		t.Fatalf("at the cap: %d %v", len(got), err)
	}
	if _, err := read(append(exe, 'x')); err == nil || !strings.Contains(err.Error(), "x: more than 1024 bytes") {
		t.Fatalf("past the cap: %v", err)
	}
}

func TestComplete(t *testing.T) {
	t.Parallel()
	s := Set{}
	for _, p := range Platforms {
		s[p] = Of([]byte("x"))
	}
	if err := s.Complete(); err != nil {
		t.Fatal(err)
	}
	s["windows/amd64"] = Of([]byte("x"))
	if err := s.Complete(); err == nil {
		t.Fatal("a platform Platforms does not name was taken")
	}
	delete(s, "windows/amd64")
	s[Target] = Of(nil)
	if err := s.Complete(); err == nil || !strings.Contains(err.Error(), Target) {
		t.Fatalf("an empty target installer: %v", err)
	}
	s[Target] = nil
	if err := s.Complete(); err == nil || !strings.Contains(err.Error(), Target) {
		t.Fatalf("no target installer: %v", err)
	}
}

// The Dockerfile's build stage builds Platforms, as cmd/platforms prints them, and names none.
func TestTheDockerfileBuildsEveryPlatform(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(raw)
	for _, want := range []string{"platforms=$(go run ./internal/installerbin/cmd/platforms); ", "for platform in $platforms; do "} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("the Dockerfile's build stage does not read its platforms from Go: no %q", want)
		}
	}
	// Platforms is the one list: the Dockerfile names none of them but the target it copies.
	for _, p := range Platforms[1:] {
		if strings.Contains(dockerfile, p) || strings.Contains(dockerfile, strings.ReplaceAll(p, "/", "-")) {
			t.Errorf("the Dockerfile names %s: Platforms is the one list", p)
		}
	}
	// Each beside its executable's sha256 and size (File, SumFile, SizeFile).
	for _, f := range []string{File("$os/$arch"), SumFile("$os/$arch"), SizeFile("$os/$arch")} {
		if !strings.Contains(dockerfile, "> internal/installerbin/"+f+"; ") {
			t.Errorf("the Dockerfile's build stage writes no %s", f)
		}
	}
}
