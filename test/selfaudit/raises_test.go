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
)

func TestRaisesReadEachStageBase(t *testing.T) {
	t.Parallel()
	file := []byte(`# RUN apk add 'commented>=9'
ARG BASE=alpine:3.24@sha256:aaa TAG="1.2"
ARG NOVALUE
FROM --platform=$BUILDPLATFORM golang:${TAG}-alpine@sha256:bbb AS builder
RUN apk --no-cache add ca-certificates 'zlib>=1.4' openssl>=3.5.1-r0 \
      "musl>=1.2.5_git20240512-r3" >/dev/null && go build
FROM $BASE AS final
ARG BASE=not:read
RUN setcap -r /x 2>/dev/null || true; apk del curl && apk add --no-cache -X https://repo 'zlib>=1.4'
FROM final
RUN apk upgrade --no-cache && apk add --no-cache su-exec
RUN apk info -v zlib
FROM ${BASE}
RUN apk add 'zlib>=1.4'
`)
	got, err := raises("x/Dockerfile", file)
	if err != nil {
		t.Fatal(err)
	}
	want := []raise{
		{"x/Dockerfile", "golang:1.2-alpine@sha256:bbb", "zlib", "1.4"},
		{"x/Dockerfile", "golang:1.2-alpine@sha256:bbb", "openssl", "3.5.1-r0"},
		{"x/Dockerfile", "golang:1.2-alpine@sha256:bbb", "musl", "1.2.5_git20240512-r3"},
		{"x/Dockerfile", "alpine:3.24@sha256:aaa", "zlib", "1.4"},
		{"x/Dockerfile", "alpine:3.24@sha256:aaa", "", ""},
		{"x/Dockerfile", "alpine:3.24@sha256:aaa", "zlib", "1.4"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raises =\n%v\nwant\n%v", got, want)
	}
}

func TestRaisesRefuseWhatTheyCannotJudge(t *testing.T) {
	t.Parallel()
	for file, want := range map[string]string{
		"FROM a\nRUN apk add 'zlib>1.4'\n":       `x/Dockerfile: apk add "zlib>1.4": a package is raised as 'pkg>=version'`,
		"FROM a\nRUN apk add zlib=1.4\n":         `apk add "zlib=1.4"`,
		"FROM a\nRUN apk add 'zlib<=1.4'\n":      `apk add "zlib<=1.4"`,
		"FROM a\nRUN apk add 'zlib~1.4'\n":       `apk add "zlib~1.4"`,
		"FROM a\nRUN apk add 'zlib>='\n":         `apk add "zlib>="`,
		"FROM a\nRUN apk add '>=1.4'\n":          `apk add ">=1.4"`,
		"FROM a\nRUN apk add 'zlib>=1.x'\n":      `apk add "zlib>=1.x"`,
		"FROM $IMAGE\nRUN apk add 'zlib>=1.4'\n": "x/Dockerfile: FROM $IMAGE: its image is not resolved by the file's global ARGs (IMAGE)",
		"FROM a:${T}@sha256:1\n":                 "(T)",
		"ARG I\nFROM ${I}\n":                     "(I)",
		"RUN apk add 'zlib>=1.4'\n":              "x/Dockerfile: an apk command before any FROM",
		"ARG -x\nFROM a\n":                       `x/Dockerfile: ARG "-x"`,
	} {
		if _, err := raises("x/Dockerfile", []byte(file)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("raises(%q) = %v, want %q", file, err, want)
		}
	}
}

func TestParseProbe(t *testing.T) {
	t.Parallel()
	for out, want := range map[string][2]string{
		"absent\n":     {"", ""},
		"1.3.2-r1 <\n": {"1.3.2-r1", "<"},
		"1.4-r0 =\n":   {"1.4-r0", "="},
		"1.4.1-r0 >\n": {"1.4.1-r0", ">"},
	} {
		if v, o, err := parseProbe(out); err != nil || v != want[0] || o != want[1] {
			t.Errorf("parseProbe(%q) = %q %q %v, want %v", out, v, o, err, want)
		}
	}
	for _, out := range []string{"", "1.4 ?", "1.4", "x <", "1.4 < extra"} {
		if _, _, err := parseProbe(out); err == nil {
			t.Errorf("parseProbe(%q) accepted", out)
		}
	}
}

// fakeBase answers for zlib at 1.4-r0 in image "base@sha256:1"; any other package is absent.
func fakeBase(t *testing.T) probeBase {
	return func(_ context.Context, image, pkg, version string) (string, string, error) {
		if image != "base@sha256:1" {
			t.Errorf("probed %q, want the pinned base", image)
		}
		switch {
		case pkg == "broken":
			return "", "", errors.New("docker run: exit 125")
		case pkg != "zlib":
			return "", "", nil
		case version == "1.4-r0":
			return "1.4-r0", "=", nil
		case version == "1.3":
			return "1.4-r0", ">", nil
		}
		return "1.4-r0", "<", nil
	}
}

func TestCheckRaisesFailsOnAStaleOneAndPrintsTheOthers(t *testing.T) {
	t.Parallel()
	examples := t.TempDir()
	for p, body := range map[string]string{
		"a/Dockerfile":          "FROM base@sha256:1\nRUN apk add 'zlib>=1.4-r0'\n",
		"b/build/Dockerfile.ci": "FROM base@sha256:1\nRUN apk add zlib>=1.3\n",
		"c/Dockerfile":          "FROM base@sha256:1\nRUN apk add 'zlib>=1.4-r1' 'libpq>=17'\n",
		"d/app.Dockerfile":      "FROM base@sha256:1\nRUN apk upgrade\n",
		"e/notes.txt":           "FROM base@sha256:1\nRUN apk add 'zlib>=1.4-r0'\n",
	} {
		p = filepath.Join(examples, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rs := []recipe{{"r", "internal/r/Dockerfile", []byte("FROM base@sha256:1\nRUN apk add 'zlib>=1.4-r0'\n"), "", ""}}
	var b bytes.Buffer
	err := checkRaises(context.Background(), &b, rs, examples, fakeBase(t))
	if err == nil {
		t.Fatalf("stale raises pass:\n%s", b.String())
	}
	msg := err.Error()
	for _, want := range []string{
		"raise: internal/r/Dockerfile raises zlib to >=1.4-r0, and its base base already carries 1.4-r0: the raise is stale, drop 'zlib>=1.4-r0' from its apk add",
		"raise: " + filepath.Join(examples, "a/Dockerfile") + " raises zlib to >=1.4-r0, and its base base already carries 1.4-r0",
		"raise: " + filepath.Join(examples, "b/build/Dockerfile.ci") + " raises zlib to >=1.3, and its base base already carries 1.4-r0",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	if n := strings.Count(msg, "stale"); n != 3 {
		t.Errorf("%d stale raises, want 3:\n%s", n, msg)
	}
	out := b.String()
	for _, want := range []string{
		"raise: " + filepath.Join(examples, "c/Dockerfile") + " raises zlib to >=1.4-r1 over its base's 1.4-r0 (base)\n",
		"raise: " + filepath.Join(examples, "c/Dockerfile") + " raises libpq to >=17 over its base base, which lacks it\n",
		"raise: " + filepath.Join(examples, "d/app.Dockerfile") + " runs apk upgrade over its base base: judge it by hand\n",
		"reminder: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out+msg, "notes.txt") {
		t.Errorf("a file that is no Dockerfile is read:\n%s%s", out, msg)
	}
}

func TestCheckRaisesRefusesWhatItCannotJudge(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ file, want string }{
		{"FROM base@sha256:1\nRUN apk add 'broken>=1'\n", "internal/r/Dockerfile: broken in its base base: docker run: exit 125"},
		{"FROM base@sha256:1\nRUN apk add 'zlib=1'\n", `internal/r/Dockerfile: apk add "zlib=1"`},
	} {
		rs := []recipe{{"r", "internal/r/Dockerfile", []byte(c.file), "", ""}}
		var b bytes.Buffer
		if err := checkRaises(context.Background(), &b, rs, t.TempDir(), fakeBase(t)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want %q", c.file, err, c.want)
		}
	}
	var b bytes.Buffer
	rs := []recipe{{"r", "internal/r/Dockerfile", []byte("FROM base@sha256:1\nRUN apk add 'zlib>=1.4-r1'\n"), "", ""}}
	if err := checkRaises(context.Background(), &b, rs, filepath.Join(t.TempDir(), "missing"), fakeBase(t)); err == nil {
		t.Error("a missing examples directory passes")
	}
}

// Every constraint in a real Dockerfile, the recipes' and the examples', is read as a raise, and
// every apk upgrade: one written in a form raises does not read would silently drop out.
func TestEveryRealRaiseIsAccountedFor(t *testing.T) {
	t.Parallel()
	examples := filepath.Join("..", "..", "examples")
	paths, err := dockerfiles(examples)
	if err != nil || len(paths) == 0 {
		t.Fatalf("no example Dockerfile found under %s: %v", examples, err)
	}
	files := map[string][]byte{}
	for _, r := range recipes {
		files[r.path] = r.file
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		files[p] = data
	}
	total := 0
	for p, data := range files {
		got, err := raises(p, data)
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		text := instructions(data)
		if want := strings.Count(text, ">=") + strings.Count(text, "apk upgrade"); len(got) != want {
			t.Errorf("%s: %d raises read, want %d: %v", p, len(got), want, got)
		}
		for _, r := range got {
			if !strings.Contains(r.base, "@sha256:") {
				t.Errorf("%s: %s raised over %q, a base not pinned by digest", p, r.pkg, r.base)
			}
		}
		total += len(got)
	}
	if total == 0 {
		t.Error("no raise read in any real Dockerfile: the check would judge nothing")
	}
}

func TestRaisesReadTheExecForm(t *testing.T) {
	t.Parallel()
	file := []byte(`FROM base@sha256:1
RUN ["apk", "add", "--no-cache", "zlib>=1.4"]
RUN --mount=type=cache,target=/var/cache/apk ["/sbin/apk", "add", "openssl>=3.5"]
RUN ["/bin/sh", "-ec", "apk add 'musl>=1.2' && apk upgrade"]
RUN [ -f /x ] && apk add 'libpq>=17'
RUN ["echo", "apk", "list"]
RUN ["go", "build"]
RUN ["/bin/busybox", "sh", "-c", "apk add 'curl>=8.1'"]
RUN ["env", "ash", "-c", "apk add 'expat>=2.7'"]
RUN ["bash", "-e", "-c", "apk add 'pcre2>=10.4'"]
RUN ["sh", "-euxc", "apk add 'xz>=5.6'", "sh", "arg"]
`)
	got, err := raises("x/Dockerfile", file)
	if err != nil {
		t.Fatal(err)
	}
	want := []raise{
		{"x/Dockerfile", "base@sha256:1", "zlib", "1.4"},
		{"x/Dockerfile", "base@sha256:1", "openssl", "3.5"},
		{"x/Dockerfile", "base@sha256:1", "musl", "1.2"},
		{"x/Dockerfile", "base@sha256:1", "", ""},
		{"x/Dockerfile", "base@sha256:1", "libpq", "17"},
		{"x/Dockerfile", "base@sha256:1", "curl", "8.1"},
		{"x/Dockerfile", "base@sha256:1", "expat", "2.7"},
		{"x/Dockerfile", "base@sha256:1", "pcre2", "10.4"},
		{"x/Dockerfile", "base@sha256:1", "xz", "5.6"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raises =\n%v\nwant\n%v", got, want)
	}
	for _, f := range []string{
		"FROM a\nRUN [\"env\", \"A=1\", \"sh\", \"-c\", \"apk add 'zlib>=1.4'\"]\n",
		"FROM a\nRUN [\"nice\", \"apk add 'zlib>=1.4'\"]\n",
		"FROM a\nRUN [\"sh\", \"-oc\", \"pipefail\", \"apk add 'zlib>=1.4'\"]\n",
		"FROM a\nRUN [\"bash\", \"-O\", \"extglob\", \"-c\", \"apk add 'zlib>=1.4'\"]\n",
		"FROM a\nRUN [\"sh\", \"-c\", \"-\", \"apk add 'zlib>=1.4'\"]\n",
		"FROM a\nRUN [\"bash\", \"--norc\", \"-c\", \"apk add 'zlib>=1.4'\"]\n",
		"FROM a\nRUN [\"bash\", \"-o\", \"pipefail\", \"-c\", \"apk add 'zlib>=1.4'\"]\n",
		"FROM a\nRUN [\"bash\", \"-c\", \"-e\", \"apk add 'zlib>=1.4'\"]\n",
		"FROM a\nRUN [\"bash\", \"--norc\", \"apk\", \"add\", \"zlib>=1.4\"]\n",
		"FROM a\nRUN [\"sh\", \"-o\", \"pipefail\", \"-c\", \"\\u0061pk add 'zlib>=1.4'\"]\n",
	} {
		if _, err := raises("x/Dockerfile", []byte(f)); err == nil || !strings.Contains(err.Error(), `x/Dockerfile: RUN [`) {
			t.Errorf("raises(%q) = %v, want a refusal naming the file", f, err)
		}
	}
	if got, err := raises("x/Dockerfile", []byte("FROM a\nRUN [\"/usr/local/bin/mkapkindex\", \"SNAPKIT=1\"]\n")); err != nil || len(got) != 0 {
		t.Errorf("an exec form naming apk only inside a word = %v, %v; want nothing", got, err)
	}
	if got, err := raises("x/Dockerfile", []byte("FROM a\nRUN [\"sh\", \"build.sh\"]\n")); err != nil || len(got) != 0 {
		t.Errorf("a shell exec form with no apk = %v, %v; want nothing", got, err)
	}
}

// shellScript reads one form only: option words each a cluster of e, u and x, then -c or such a
// cluster ending in c, then a script that is not an option; any other form is none.
func TestShellScriptReadsOnlyTheCForm(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"sh", "-c", "apk add x"}, "apk add x"},
		{[]string{"bash", "-e", "-c", "apk add x"}, "apk add x"},
		{[]string{"sh", "-euxc", "apk add x"}, "apk add x"},
		{[]string{"sh", "-eu", "-x", "-ec", "apk add x", "sh", "a"}, "apk add x"},
		{[]string{"sh", "-oc", "pipefail", "apk add x"}, ""},
		{[]string{"bash", "-O", "extglob", "-c", "apk add x"}, ""},
		{[]string{"sh", "-c", "-", "apk add x"}, ""},
		{[]string{"sh", "-c", "+x", "apk add x"}, ""},
		{[]string{"bash", "--norc", "-c", "apk add x"}, ""},
		{[]string{"bash", "-o", "pipefail", "-c", "apk add x"}, ""},
		{[]string{"bash", "-c", "-e", "apk add x"}, ""},
		{[]string{"sh", "+e", "-c", "apk add x"}, ""},
		{[]string{"sh", "-ce", "apk add x"}, ""},
		{[]string{"sh", "-e", "script.sh"}, ""},
		{[]string{"sh", "-c"}, ""},
		{[]string{"sh"}, ""},
	} {
		if got := shellScript(c.argv); got != c.want {
			t.Errorf("shellScript(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestCheckRaisesProbesEachQuestionOnce(t *testing.T) {
	t.Parallel()
	examples := t.TempDir()
	for _, p := range []string{"a/Dockerfile", "b/Dockerfile", "c/Dockerfile"} {
		p = filepath.Join(examples, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("FROM base@sha256:1\nRUN apk add 'zlib>=1.4-r1'\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rs := []recipe{{"r", "internal/r/Dockerfile", []byte("FROM base@sha256:1\nRUN apk add 'zlib>=1.4-r1' 'zlib>=1.4-r2' 'broken>=1' 'broken>=1'\n"), "", ""}}
	calls := map[string]int{}
	inner := fakeBase(t)
	probe := func(ctx context.Context, image, pkg, version string) (string, string, error) {
		calls[pkg+">="+version]++
		return inner(ctx, image, pkg, version)
	}
	var b bytes.Buffer
	err := checkRaises(context.Background(), &b, rs, examples, probe)
	if want := map[string]int{"zlib>=1.4-r1": 1, "zlib>=1.4-r2": 1, "broken>=1": 1}; !reflect.DeepEqual(calls, want) {
		t.Errorf("probes = %v, want %v", calls, want)
	}
	if n := strings.Count(b.String(), "raises zlib to >=1.4-r1"); n != 4 {
		t.Errorf("%d raises of zlib>=1.4-r1 printed, want 4:\n%s", n, b.String())
	}
	if err == nil || strings.Count(err.Error(), "broken in its base") != 2 {
		t.Errorf("err = %v, want the cached failure for each of the two raises", err)
	}
}
