package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestOverridesReadEveryFormAndSkipTheBuiltProgram(t *testing.T) {
	file := []byte(`# go get a comment/module@v9.9.9 --replace not/read=not/read@v9.9.9
RUN xcaddy build v2.0.0 --with example.com/plugin@v0.1.0 \
      --replace example.com/a=example.com/a@v1.0.0 --replace example.com/b/v2=example.com/fork/b/v2@${B_VERSION}
RUN go mod init build \
 && go get -u example.com/program@v3.0.0 example.com/c@v1.1.0 \
      example.com/d@v1.2.0 \
 && go build example.com/program && go install example.com/tool@v4.0.0
`)
	want := []override{
		{"example.com/a", "v1.0.0"}, {"example.com/b/v2", "${B_VERSION}"},
		{"example.com/c", "v1.1.0"}, {"example.com/d", "v1.2.0"},
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
// recipe written in a form overrides does not read would silently drop out of the reminder.
func TestEveryRealRecipesPinIsAccountedFor(t *testing.T) {
	for _, r := range recipes {
		text := instructions(r.file)
		want := strings.Count(text, "@v") - strings.Count(text, "--with ") - strings.Count(text, "go get ")
		got := overrides(r.file)
		if len(got) != want {
			t.Errorf("%s: %d overrides read, want %d: %v", r.key, len(got), want, got)
		}
		if len(got) > 0 && r.upstream == "" {
			t.Errorf("%s forces modules, and names no upstream for the reminder", r.key)
		}
	}
}

func TestRemindNamesEachOverrideAndWhatIsOwed(t *testing.T) {
	var b bytes.Buffer
	remind(&b, []recipe{
		{"none", []byte("FROM scratch\n"), ""},
		{"front", []byte("RUN xcaddy build v2 --replace example.com/a=example.com/a@v1.0.0\n"), "Caddy"},
	})
	out := b.String()
	for _, want := range []string{"override: front forces example.com/a v1.0.0 over what Caddy's release ships\n", "reminder: ", "drop the override"} {
		if !strings.Contains(out, want) {
			t.Errorf("reminder lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "none") {
		t.Errorf("a recipe that forces nothing is named:\n%s", out)
	}
	b.Reset()
	remind(&b, []recipe{{"none", []byte("FROM scratch\n"), ""}})
	if b.Len() != 0 {
		t.Errorf("nothing forced, and still a reminder: %s", b.String())
	}
}
