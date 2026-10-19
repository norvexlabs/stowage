package version

import (
	"os"
	"testing"

	"github.com/norvexlabs/stowage/internal/testutil"
)

func TestMain(m *testing.M) {
	cleanup := testutil.IsolateGit()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
