package sbom

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
)

// evidentBy opens the comment syft gives the relationship from a package to the file it found it
// in (a Go module to its binary, a jar to its archive).
const evidentBy = "evident-by:"

// Evidence maps a package of an SPDX document, by its SPDX id less the "SPDXRef-" prefix (grype's
// artifact id for a package it read from the document), to the files the document says it was
// found in: absolute, cleaned, sorted. A package with no such relationship has none; a document
// whose files or relationships do not decode is an error, never a report without its files.
func Evidence(doc []byte) (map[string][]string, error) {
	var d struct {
		Files []struct {
			ID   string `json:"SPDXID"`
			Name string `json:"fileName"`
		} `json:"files"`
		Relationships []struct {
			From    string `json:"spdxElementId"`
			To      string `json:"relatedSpdxElement"`
			Type    string `json:"relationshipType"`
			Comment string `json:"comment"`
		} `json:"relationships"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("the SBOM's files and relationships: %w", err)
	}
	files := make(map[string]string, len(d.Files))
	for _, f := range d.Files {
		if f.ID != "" && f.Name != "" {
			files[f.ID] = path.Clean("/" + f.Name)
		}
	}
	out := map[string][]string{}
	for _, r := range d.Relationships {
		if r.Type != "OTHER" || !strings.HasPrefix(r.Comment, evidentBy) {
			continue
		}
		p, ok := files[r.To]
		if !ok {
			continue
		}
		id := strings.TrimPrefix(r.From, "SPDXRef-")
		if id == "" {
			continue // ParseGrype looks up "" for a match with no artifact id: "" never holds files
		}
		if !slices.Contains(out[id], p) {
			out[id] = append(out[id], p)
		}
	}
	for _, ps := range out {
		slices.Sort(ps)
	}
	return out, nil
}
