package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// TitleAnnotation names the file an artifact's layer is written to.
const TitleAnnotation = "org.opencontainers.image.title"

// Download writes the one layer of the artifact at d — of artifactType, its layer of type layer,
// or layer+ZstdSuffix, then decompressed as it is written and its title's ".zst" dropped — into
// dir, under the layer's title (a plain file name), else fallback, and returns its path and the
// file's digest (what the file's bytes hash to: a compressed layer's UncompressedAnnotation, checked). The
// manifest is checked against d's digest, and the layer against its digest as it is read (both by
// go-containerregistry's reads); the file reaches its name only once read whole. A file of that name already
// holding the layer is kept, not fetched again; any other is refused, never replaced. The file is
// 0644: what it holds is published, and a program running as another user (a hypervisor) reads
// it. progress, when set, is called as bytes arrive.
func (c *Client) Download(ctx context.Context, d name.Digest, artifactType, layer types.MediaType, dir, fallback string, progress func(done, total int64)) (string, v1.Hash, error) {
	desc, err := c.Get(ctx, d)
	if err != nil {
		return "", v1.Hash{}, err
	}
	if desc == nil {
		return "", v1.Hash{}, fmt.Errorf("%s does not exist", d)
	}
	var m struct {
		ArtifactType types.MediaType `json:"artifactType"`
		Layers       []v1.Descriptor `json:"layers"`
	}
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return "", v1.Hash{}, fmt.Errorf("%s: manifest: %w", d, err)
	}
	if m.ArtifactType != artifactType || len(m.Layers) != 1 || (m.Layers[0].MediaType != layer && m.Layers[0].MediaType != layer+ZstdSuffix) {
		return "", v1.Hash{}, fmt.Errorf("%s is not a %s artifact with one %s layer", d, artifactType, layer)
	}
	l := m.Layers[0]
	compressed := l.MediaType == layer+ZstdSuffix
	want, size, title := l.Digest, l.Size, l.Annotations[TitleAnnotation]
	if compressed {
		if want, err = uncompressedDigest(l); err != nil {
			return "", v1.Hash{}, fmt.Errorf("%s: %w", d, err)
		}
		size, title = -1, strings.TrimSuffix(title, ".zst")
	}
	file := fallback
	if plainName(title) {
		file = title
	}
	path := filepath.Join(dir, file)
	switch ok, err := holds(path, want, size); {
	case err != nil:
		return "", v1.Hash{}, err
	case ok:
		return path, want, nil
	}
	rl, err := remote.Layer(d.Context().Digest(l.Digest.String()), c.opts(ctx)...)
	if err != nil {
		return "", v1.Hash{}, fmt.Errorf("%s: %w", d, err)
	}
	rc, err := rl.Compressed()
	if err != nil {
		return "", v1.Hash{}, fmt.Errorf("%s: %w", d, err)
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(dir, "."+file+".part-*")
	if err != nil {
		return "", v1.Hash{}, err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	var r io.Reader = rc
	if progress != nil {
		r = &counter{r: r, total: l.Size, progress: progress}
	}
	err = write(tmp, r, compressed, want)
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", v1.Hash{}, fmt.Errorf("%s: %w", d, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", v1.Hash{}, err
	}
	return path, want, nil
}

// write copies the layer r into w: as it is, or decompressed, its bytes then checked against want.
// The layer is read to its end either way, where its own digest is checked.
func write(w io.Writer, r io.Reader, compressed bool, want v1.Hash) error {
	if !compressed {
		_, err := io.Copy(w, r)
		return err
	}
	dec, err := unzstd(r)
	if err != nil {
		return err
	}
	defer dec.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), dec); err != nil {
		return fmt.Errorf("decompress: %w", err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != want.String() {
		return fmt.Errorf("decompressed, the layer is %s, not %s", got, want)
	}
	return nil
}

// plainName reports whether t is a file name with no directory in it.
func plainName(t string) bool {
	return t != "" && t != "." && t != ".." && !strings.ContainsAny(t, `/\`) && filepath.Base(t) == t
}

// holds reports whether the file at path is the file of digest want (and size, unless it is
// negative): false when there is none, an error when it is something else.
func holds(path string, want v1.Hash, size int64) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf("%s exists and is not a file", path)
	}
	h := sha256.New()
	if size < 0 || st.Size() == size {
		if _, err := io.Copy(h, f); err != nil {
			return false, err
		}
		if "sha256:"+hex.EncodeToString(h.Sum(nil)) == want.String() {
			return true, nil
		}
	}
	return false, fmt.Errorf("%s exists and is not %s: move it away", path, want)
}

// counter reports the bytes read through it.
type counter struct {
	r        io.Reader
	done     int64
	total    int64
	progress func(done, total int64)
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.done += int64(n)
	if n > 0 {
		c.progress(c.done, c.total)
	}
	return n, err
}
