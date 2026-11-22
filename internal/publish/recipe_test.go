package publish

import (
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/chown"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/graph"
	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/sbom"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// Every recipe Stowage supplies runs a package manager in its final stage alone,
// named RecipeStage, which the build never takes from the cache.
func TestRecipesInstallInTheirFreshStage(t *testing.T) {
	t.Parallel()
	bundle, err := kairos.Dockerfile(kairos.Params{Name: "notes", Ctl: "notes-stow", Base: "ubuntu:26.04@sha256:" + strings.Repeat("a", 64),
		KairosInit: "quay.io/kairos/kairos-init:v4.6.0@sha256:" + strings.Repeat("b", 64), OSVersion: "1.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	recipes := map[string][]byte{graph.FrontID: front.Dockerfile, graph.BackupID: backup.Dockerfile, graph.AuditID: sbom.Dockerfile, graph.ChownID: chown.Dockerfile, "os": bundle}
	for name, recipe := range recipes {
		if err := testutil.FreshStage(recipe, RecipeStage); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Every node of a release that Stowage builds from its own recipe is one of those.
	p, err := project.Open(testutil.CopyExample(t, "full"), "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := graph.Build(p, graph.Release{Version: "1.0.0", Revision: 1, Channel: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range g.Nodes {
		if r, ok := recipes[n.ID]; n.Recipe != nil && (!ok || string(r) != string(n.Recipe)) {
			t.Errorf("node %s is built from a recipe the test does not check", n.ID)
		}
	}
	// The check refuses a recipe whose final stage is unnamed or another, or which installs before it.
	for _, bad := range []string{"FROM a\nRUN true\n", "FROM a AS other\n", "FROM a AS b\nRUN apk add x\nFROM c AS " + RecipeStage + "\n", "RUN true\n",
		"FROM a AS b\nRUN apk --no-cache add x\nFROM c AS " + RecipeStage + "\n",
		"FROM a AS b\nRUN true && apt-get \\\n    -y install x\nFROM c AS " + RecipeStage + "\n",
		"FROM a AS b\nRUN /kairos-init -s install -v 0.0.0\nFROM c AS " + RecipeStage + "\n",
		"FROM a AS b\nRUN /usr/bin/apt-get install -y x\nFROM c AS " + RecipeStage + "\n",
		"FROM a AS b\nRUN sh -c \"apk add x\"\nFROM c AS " + RecipeStage + "\n",
		"FROM a AS b\nRUN zypper -n in x;true\nFROM c AS " + RecipeStage + "\n",
		"FROM a AS b\nRUN apk>/dev/null add x\nFROM c AS " + RecipeStage + "\n",
		"FROM a AS b\nRUN apk\\\n  add x\nFROM c AS " + RecipeStage + "\n",
		"FROM a AS b\nRUN tdnf install -y x && apk.static add y\nFROM c AS " + RecipeStage + "\n",
		"FROM a AS b\r\nRUN true \\ \r\n# pinned\r\n  && apk upgrade\r\nFROM c AS " + RecipeStage + "\r\n"} {
		if testutil.FreshStage([]byte(bad), RecipeStage) == nil {
			t.Errorf("%q passes", bad)
		}
	}
	// A stage before the final one that names a package manager only in a comment, or as a stage, passes.
	if err := testutil.FreshStage([]byte("FROM a AS apk\n# apk add x\nRUN go build\nFROM c AS "+RecipeStage+"\nRUN apk add x\n"), RecipeStage); err != nil {
		t.Error(err)
	}
}
