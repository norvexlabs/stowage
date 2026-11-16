package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/installerbin"
)

// The command prints installerbin.Platforms, in its order, as the Dockerfile's loop reads them.
func TestWritePrintsThePlatforms(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(buf.String()); !slices.Equal(got, installerbin.Platforms) {
		t.Fatalf("printed %v, Platforms is %v", got, installerbin.Platforms)
	}
}
