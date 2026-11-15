package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

func TestOverridesReadEveryFormAndSkipTheBuiltProgram(t *testing.T) {
	t.Parallel()
	file := []byte(`# go get a comment/module@v9.9.9 --replace not/read=not/read@v9.9.9
RUN xcaddy build v2.0.0 --with example.com/plugin@v0.1.0 \
      --replace example.com/a=example.com/a@v1.0.0 --replace example.com/b/v2=example.com/fork/b/v2@${B_VERSION}
RUN go mod init build \
 && go get -u example.com/program@v3.0.0 example.com/c@v1.1.0 \
      example.com/d@v1.2.0 \
 && go build example.com/program && go install example.com/tool@v4.0.0
`)
	want := []override{
		{"example.com/a", "v1.0.0", "--replace"}, {"example.com/b/v2", "${B_VERSION}", "--replace"},
		{"example.com/c", "v1.1.0", "go get"}, {"example.com/d", "v1.2.0", "go get"},
	}
	if got := overrides(file); !reflect.DeepEqual(got, want) {
		t.Fatalf("overrides = %v, want %v", got, want)
	}
	for _, nothing := range []string{"FROM busybox:1@sha256:abc\n", "RUN go get  \\\n"} {
		if got := overrides([]byte(nothing)); got != nil {
			t.Fatalf("%q forces nothing: %v", nothing, got)
		}
	}
}

// Every module version in a real recipe is a plugin, the program it builds or an override: a
// recipe written in a form overrides does not read would silently drop out of the check. Each
// recipe is the file its path names, and each that forces a module names the release it builds.
func TestEveryRealRecipesPinIsAccountedFor(t *testing.T) {
	t.Parallel()
	for _, r := range recipes {
		text := instructions(r.file)
		want := strings.Count(text, "@v") - strings.Count(text, "--with ") - strings.Count(text, "go get ")
		got := overrides(r.file)
		if len(got) != want {
			t.Errorf("%s: %d overrides read, want %d: %v", r.key, len(got), want, got)
		}
		if disk, err := os.ReadFile(filepath.Join("..", "..", r.path)); err != nil || !bytes.Equal(disk, r.file) {
			t.Errorf("%s: %s is not the recipe it embeds (%v)", r.key, r.path, err)
		}
		if (r.upstream == "") != (r.module == "") {
			t.Errorf("%s: an upstream (%q) names its module (%q)", r.key, r.upstream, r.module)
		}
		if r.module == "" {
			if len(got) > 0 {
				t.Errorf("%s forces modules, and names no upstream", r.key)
			}
			continue
		}
		if _, err := release(r); err != nil {
			t.Errorf("%s: %v", r.key, err)
		}
		for _, o := range got {
			if !semver.IsValid(o.version) {
				t.Errorf("%s: %s forced at %q", r.key, o.module, o.version)
			}
		}
	}
}

func TestReleaseIsTheProgramsOrXcaddys(t *testing.T) {
	t.Parallel()
	r := func(file string) recipe { return recipe{"k", "k/Dockerfile", []byte(file), "Up", "example.com/up"} }
	for file, want := range map[string]string{
		"RUN go get -u example.com/up/cmd/up@v1.2.3 example.com/x@v0.1.0\n": "v1.2.3",
		"RUN go get example.com/up@v1.2.4\n":                                "v1.2.4",
		"RUN xcaddy build v2.11.9 --replace a=a@v1.0.0\n":                   "v2.11.9",
	} {
		if got, err := release(r(file)); err != nil || got != want {
			t.Errorf("release(%q) = %q, %v; want %q", file, got, err, want)
		}
	}
	for file, wantErr := range map[string]string{
		"RUN go get example.com/upper@v1.0.0 a@v1.0.0\n": "k/Dockerfile: no release of Up (example.com/up)",
		"FROM scratch\n":                   "k/Dockerfile: no release of Up",
		"RUN go get example.com/up@${V}\n": `k/Dockerfile: Up's release "${V}" is not a module version`,
	} {
		if _, err := release(r(file)); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("release(%q) = %v, want %q", file, err, wantErr)
		}
	}
}

func TestModURLCaseEncodes(t *testing.T) {
	t.Parallel()
	got, err := modURL("github.com/BurntSushi/toml", "v1.4.0-RC1")
	if want := "https://proxy.golang.org/github.com/!burnt!sushi/toml/@v/v1.4.0-!r!c1.mod"; err != nil || got != want {
		t.Fatalf("modURL = %q, %v; want %q", got, err, want)
	}
}

// upstreamMod is the go.mod the fake proxy serves for example.com/up v2.0.0.
const upstreamMod = `module example.com/up

go 1.25

require (
	example.com/equal v1.0.0
	example.com/past v1.2.1
	example.com/live v1.2.9 // indirect
)

require example.com/pre v1.3.0-rc.1
`

func fakeProxy(t *testing.T) fetchMod {
	return func(_ context.Context, mod, version string) ([]byte, error) {
		if mod != "example.com/up" || version != "v2.0.0" {
			t.Errorf("fetched %s %s, want example.com/up v2.0.0", mod, version)
		}
		return []byte(upstreamMod), nil
	}
}

func TestCheckOverridesFailsOnAStaleOneAndPrintsTheOthers(t *testing.T) {
	t.Parallel()
	r := recipe{"front", "internal/front/Dockerfile", []byte(`RUN xcaddy build v2.0.0 --replace example.com/equal=example.com/equal@v1.0.0 \
  --replace example.com/past=example.com/past@v1.2.0 --replace example.com/live=example.com/live@v1.3.0 \
  --replace example.com/pre=example.com/pre@v1.3.0 --replace example.com/absent=example.com/absent@v0.1.0
`), "Up", "example.com/up"}
	var b bytes.Buffer
	err := checkOverrides(context.Background(), &b, []recipe{r}, fakeProxy(t))
	if err == nil {
		t.Fatalf("stale overrides pass:\n%s", b.String())
	}
	msg := err.Error()
	for _, want := range []string{
		"override: front forces example.com/equal v1.0.0, and Up v2.0.0 already requires v1.0.0: the override is stale, drop the --replace of example.com/equal from internal/front/Dockerfile",
		"override: front forces example.com/past v1.2.0, and Up v2.0.0 already requires v1.2.1: the override is stale",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	if n := strings.Count(msg, "stale"); n != 2 {
		t.Errorf("%d stale overrides, want 2:\n%s", n, msg)
	}
	out := b.String()
	for _, want := range []string{
		"override: front forces example.com/live v1.3.0 over the v1.2.9 Up v2.0.0 requires\n",
		"override: front forces example.com/pre v1.3.0 over the v1.3.0-rc.1 Up v2.0.0 requires\n",
		"override: front forces example.com/absent v0.1.0 over what Up's release ships (not in Up v2.0.0's go.mod: judge it by hand)\n",
		"reminder: ", "drop the override",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "equal") || strings.Contains(out, "past") {
		t.Errorf("a stale override is printed as live:\n%s", out)
	}
}

func TestCheckOverridesRefusesWhatItCannotJudge(t *testing.T) {
	t.Parallel()
	failing := func(context.Context, string, string) ([]byte, error) { return nil, errors.New("GET: 404 Not Found") }
	none := recipe{"none", "n/Dockerfile", []byte("FROM scratch\n"), "", ""}
	for _, c := range []struct {
		r     recipe
		fetch fetchMod
		want  string
	}{
		{recipe{"g", "g/Dockerfile", []byte("RUN go get example.com/up@v2.0.0 example.com/live@${V}\n"), "Up", "example.com/up"}, fakeProxy(t),
			`g/Dockerfile: the go get of example.com/live forces "${V}", not a module version`},
		{recipe{"g", "g/Dockerfile", []byte("RUN go get example.com/up@v2.0.0 example.com/live@v1.3.0\n"), "Up", "example.com/up"}, failing,
			"g/Dockerfile: Up v2.0.0's go.mod: GET: 404 Not Found"},
		{recipe{"g", "g/Dockerfile", []byte("RUN go get example.com/live@v1.3.0 example.com/x@v1.0.0\n"), "Up", "example.com/up"}, fakeProxy(t),
			"g/Dockerfile: no release of Up"},
	} {
		var b bytes.Buffer
		if err := checkOverrides(context.Background(), &b, []recipe{none, c.r}, c.fetch); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.r.file, err, c.want)
		}
	}
	var b bytes.Buffer
	if err := checkOverrides(context.Background(), &b, []recipe{none}, failing); err != nil || b.Len() != 0 {
		t.Errorf("nothing forced: err %v, output %q", err, b.String())
	}
}
