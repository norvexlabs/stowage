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

// Download writes the one layer of the artifact at d — of artifactType, its layer of type layer —
// into dir, under the layer's title (a plain file name), else fallback, and returns its path and
// the layer's digest (what the file's bytes hash to). The
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
	if m.ArtifactType != artifactType || len(m.Layers) != 1 || m.Layers[0].MediaType != layer {
		return "", v1.Hash{}, fmt.Errorf("%s is not a %s artifact with one %s layer", d, artifactType, layer)
	}
	l := m.Layers[0]
	file := fallback
	if t := l.Annotations[TitleAnnotation]; plainName(t) {
		file = t
	}
	path := filepath.Join(dir, file)
	switch ok, err := holds(path, l); {
	case err != nil:
		return "", v1.Hash{}, err
	case ok:
		return path, l.Digest, nil
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
	_, err = io.Copy(tmp, r)
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
	return path, l.Digest, nil
}

// plainName reports whether t is a file name with no directory in it.
func plainName(t string) bool {
	return t != "" && t != "." && t != ".." && !strings.ContainsAny(t, `/\`) && filepath.Base(t) == t
}

// holds reports whether the file at path is the layer l (its size and digest): false when there is
// none, an error when it is something else.
func holds(path string, l v1.Descriptor) (bool, error) {
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
	if st.Size() == l.Size {
		if _, err := io.Copy(h, f); err != nil {
			return false, err
		}
		if "sha256:"+hex.EncodeToString(h.Sum(nil)) == l.Digest.String() {
			return true, nil
		}
	}
	return false, fmt.Errorf("%s exists and is not %s: move it away", path, l.Digest)
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
