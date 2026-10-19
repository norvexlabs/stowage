package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"
)

// ZstdSuffix ends the media type of a layer that is its file compressed with zstd, titled with the
// file's name and ".zst": Download writes the file decompressed, under its own name.
const ZstdSuffix = "+zstd"

// UncompressedAnnotation is a compressed layer's file's own digest, decompressed: what Download
// checks the file it writes against. The signed manifest covers it, with the compressed digest.
const UncompressedAnnotation = "dev.stowage.uncompressed.digest"

// zstdMaxWindow bounds the memory a compressed layer's decoder may be asked for: zstd's own limit
// without --long (128 MiB); ZstdFileBlob's frames ask for 8 MiB.
const zstdMaxWindow = 1 << 27

// ZstdFileBlob compresses the file at path into path+".zst" (zstd at its default level, 3) and
// returns that file as a blob of media type mt+ZstdSuffix, titled with its name and annotated with
// the file's own digest, both hashed as they are written. The compressed file must not change
// until the blob is pushed.
func ZstdFileBlob(mt types.MediaType, path string) (Blob, error) {
	in, err := os.Open(path)
	if err != nil {
		return Blob{}, err
	}
	defer in.Close()
	zpath := path + ".zst"
	out, err := os.Create(zpath)
	if err != nil {
		return Blob{}, err
	}
	defer out.Close()
	zh := sha256.New()
	zn := &byteCount{}
	enc, err := zstd.NewWriter(io.MultiWriter(out, zh, zn), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return Blob{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(enc, io.TeeReader(in, h)); err != nil {
		enc.Close()
		return Blob{}, fmt.Errorf("compress %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return Blob{}, fmt.Errorf("compress %s: %w", path, err)
	}
	if err := out.Close(); err != nil {
		return Blob{}, fmt.Errorf("compress %s: %w", path, err)
	}
	return Blob{
		MediaType:   mt + ZstdSuffix,
		Annotations: map[string]string{TitleAnnotation: filepath.Base(zpath), UncompressedAnnotation: "sha256:" + hex.EncodeToString(h.Sum(nil))},
		path:        zpath,
		digest:      v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(zh.Sum(nil))},
		size:        zn.n,
	}, nil
}

// byteCount counts the bytes written to it.
type byteCount struct{ n int64 }

func (c *byteCount) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// unzstd is r decompressed, with the frames' window bounded.
func unzstd(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r, zstd.WithDecoderMaxWindow(zstdMaxWindow), zstd.WithDecoderConcurrency(1))
}

// uncompressedDigest is the digest l's annotation gives its file, decompressed.
func uncompressedDigest(l v1.Descriptor) (v1.Hash, error) {
	h, err := v1.NewHash(l.Annotations[UncompressedAnnotation])
	if err != nil || h.Algorithm != "sha256" {
		return v1.Hash{}, fmt.Errorf("a compressed layer without its file's sha256 digest (%s)", UncompressedAnnotation)
	}
	return h, nil
}
