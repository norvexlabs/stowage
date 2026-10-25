package sbom

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IndexFile is the export's index, beside the documents.
const IndexFile = "index.json"

// Exported is one artifact of an export: its document's file, or why it has none.
type Exported struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	File   string `json:"file,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Index is what an export wrote: the release, and each artifact's document.
type Index struct {
	Project   string     `json:"project"`
	Version   string     `json:"version"`
	Revision  int        `json:"revision"`
	Channel   string     `json:"channel,omitempty"`
	Tag       string     `json:"tag,omitempty"`
	Format    string     `json:"format"` // the documents' format: spdx-2.3+json
	Artifacts []Exported `json:"artifacts"`
}

// Export writes each target's SBOM, read as attested by a trusted key, into dir as
// <name>.spdx.json, and the index (idx, its artifacts filled) as IndexFile. A target without one
// is in the index with its error; Export then fails, once everything else is written.
func Export(ctx context.Context, read Attested, targets []Target, dir string, idx Index) (Index, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return idx, err
	}
	idx.Format, idx.Artifacts = "spdx-2.3+json", nil
	var missing []string
	for _, t := range targets {
		e := Exported{Name: t.Name, Digest: t.Digest.DigestStr()}
		doc, err := read(ctx, t.Digest)
		if err == nil {
			err = Check(doc)
		}
		if err == nil {
			e.File = t.Name + ".spdx.json"
			err = os.WriteFile(filepath.Join(dir, e.File), doc, 0o644)
		}
		if err != nil {
			e.File, e.Error = "", err.Error()
			missing = append(missing, t.Name)
		}
		idx.Artifacts = append(idx.Artifacts, e)
	}
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return idx, err
	}
	if err := os.WriteFile(filepath.Join(dir, IndexFile), append(raw, '\n'), 0o644); err != nil {
		return idx, err
	}
	if len(missing) > 0 {
		return idx, fmt.Errorf("sbom export: no SBOM by a trusted key for %s (the others are in %s)", strings.Join(missing, ", "), dir)
	}
	return idx, nil
}
