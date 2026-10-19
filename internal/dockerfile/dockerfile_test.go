package dockerfile

import (
	"reflect"
	"testing"
)

func TestMayDeclareArg(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"FROM x\nARG STOWAGE_PROJECT_VERSION\n", true},
		{"FROM x\narg STOWAGE_PROJECT_VERSION=dev\n", true}, // keywords are case-insensitive
		{"ARG STOWAGE_PROJECT_VERSION\nFROM x\n", true},     // a global ARG, before the first FROM
		{"FROM x\nARG BUILD=1 STOWAGE_PROJECT_VERSION\n", true},
		{"FROM x\nARG \\\n  STOWAGE_PROJECT_VERSION\n", true},
		{"FROM x\nARG A=1 \\\n  # a comment between continuation lines\n  STOWAGE_PROJECT_VERSION=dev\n", true},
		{"# escape=`\nFROM x\nARG A=1 `\n STOWAGE_PROJECT_VERSION\n", true},
		{"FROM x\nARG \"STOWAGE_PROJECT_VERSION\"\n", true}, // the build strips the quotes
		{"FROM x\nARG STOWAGE_PROJECT_${PART}\n", true},     // may resolve to it: counted
		{"FROM x\nARG stowage_project_version\n", false},    // names are case-sensitive
		{"FROM x\n# ARG STOWAGE_PROJECT_VERSION\n", false},
		{"FROM x\nARG STOWAGE_PROJECT_VERSION_X\n", false},
		{"FROM x\nENV STOWAGE_PROJECT_VERSION=1\n", false},
		{"FROM x\nRUN echo ARG STOWAGE_PROJECT_VERSION\n", false},
		// Heredoc bodies are data, whatever their delimiter looks like.
		{"FROM x\nRUN <<EOF\nARG STOWAGE_PROJECT_VERSION\nEOF\n", false},
		{"FROM x\nRUN <<-\"EOF\"\n\tARG STOWAGE_PROJECT_VERSION\n\tEOF\nARG STOWAGE_PROJECT_VERSION\n", true},
		{"FROM x\nCOPY <<A <<B /dst/\nARG X\nA\nARG STOWAGE_PROJECT_VERSION\nB\n", false},
		{"FROM x\nRUN python3 <<END-PY\nARG STOWAGE_PROJECT_VERSION\nEND-PY\n", false},
		{"FROM x\nRUN cat <<EOF.py\nARG STOWAGE_PROJECT_VERSION\nEOF.py\n", false},
		// Not heredocs: a here-string, a shift, an opener inside a quoted string.
		{"FROM x\nRUN tr a-z A-Z <<<foo\nARG STOWAGE_PROJECT_VERSION\n", true},
		{"FROM x\nRUN echo $((1<<4))\nARG STOWAGE_PROJECT_VERSION\n", true},
		{"FROM x\nRUN echo \"usage: cat <<EOF here\"\nARG STOWAGE_PROJECT_VERSION\n", true},
	}
	for _, c := range cases {
		f, err := Parse([]byte(c.src))
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if got, err := f.MayDeclareArg("STOWAGE_PROJECT_VERSION"); err != nil || got != c.want {
			t.Errorf("%q: got %v, %v; want %v", c.src, got, err, c.want)
		}
	}
}

func TestFroms(t *testing.T) {
	src := "# syntax=docker/dockerfile:1\nARG BASE=alpine\nFROM --platform=$BUILDPLATFORM golang:1.27@sha256:abc AS Build\nRUN x\nfrom ${BASE}\nRUN python3 <<EOF\nfrom pathlib import Path\nEOF\nFROM build\n"
	want := []From{
		{Image: "golang:1.27@sha256:abc", Stage: "build", Line: 3},
		{Image: "${BASE}", Line: 5},
		{Image: "build", Line: 9},
	}
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Froms(); !reflect.DeepEqual(got, want) {
		t.Errorf("Froms = %+v\nwant %+v", got, want)
	}
}

// A Dockerfile the build would refuse is refused here, so lint reports it before ship does.
func TestInvalidDockerfiles(t *testing.T) {
	for _, src := range []string{
		"FROM\n",
		"FROM a AS\n",
		"FROM a AS b c\n",
		"FROM a AS 9bad\n",
		"FROM x\nRUN echo\nNOTANINSTRUCTION y\n",
		"ARG X=1\n", // no stage
		"# only a comment\n",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%q accepted", src)
		}
	}
}

func FuzzNeverPanics(f *testing.F) {
	f.Add("FROM x\nARG A \\\n B\n")
	f.Add("# escape=`\nFROM x `\n")
	f.Add("FROM x\nRUN <<EOF\nfrom y\nEOF\n")
	f.Add("FROM x\nARG \"A\" B=${C}\n")
	f.Fuzz(func(t *testing.T, src string) {
		df, err := Parse([]byte(src))
		if err != nil {
			return
		}
		_, _ = df.MayDeclareArg("A")
		df.Froms()
	})
}

// A Dockerfile naming another frontend is not judged by the built-in grammar: it is accepted, its
// stages are unknown, and it counts as reading the version.
func TestCustomFrontend(t *testing.T) {
	src := "# syntax=registry.example.com/my-frontend:1\nFROM x\nFUTUREINSTRUCTION y\n"
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if f.Frontend == "" || len(f.Froms()) != 0 {
		t.Errorf("Frontend = %q, Froms = %v", f.Frontend, f.Froms())
	}
	if reads, _ := f.MayDeclareArg("STOWAGE_PROJECT_VERSION"); !reads {
		t.Error("a custom frontend must count as reading the version")
	}
	// The standard frontend, any version, is validated as usual.
	for _, syntax := range []string{"docker/dockerfile:1", "docker.io/docker/dockerfile:1.7", "docker/dockerfile:1@sha256:abc"} {
		if _, err := Parse([]byte("# syntax=" + syntax + "\nFROM x\nFUTUREINSTRUCTION y\n")); err == nil {
			t.Errorf("%s: an unknown instruction passed", syntax)
		}
	}
}
