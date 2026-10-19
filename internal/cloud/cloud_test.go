package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestVars(t *testing.T) {
	t.Parallel()
	// AWS_REGION alone, set for other tools, uploads nothing.
	if v, err := Vars("aws", env(map[string]string{"AWS_REGION": "eu-west-1"})); v != nil || err != nil {
		t.Errorf("region alone: %v, %v", v, err)
	}
	v, err := Vars("aws", env(map[string]string{"AWS_REGION": "eu-west-1", "STOWAGE_AWS_BUCKET": "b"}))
	if err != nil || v["bucket"] != "b" || v["region"] != "eu-west-1" || v["vmimport_role"] != "vmimport" {
		t.Errorf("aws: %v, %v", v, err)
	}
	_, err = Vars("aws", env(map[string]string{"STOWAGE_AWS_BUCKET": "b"}))
	if err == nil || err.Error() != "cloud aws: STOWAGE_AWS_BUCKET set, so its image is uploaded: set AWS_REGION too" {
		t.Errorf("aws without region: %v", err)
	}
	_, err = Vars("azure", env(map[string]string{"STOWAGE_AZURE_LOCATION": "westeurope", "STOWAGE_AZURE_CONTAINER": "c"}))
	if err == nil || !strings.Contains(err.Error(), "STOWAGE_AZURE_CONTAINER, STOWAGE_AZURE_LOCATION set") || !strings.HasSuffix(err.Error(), "set STOWAGE_AZURE_RESOURCE_GROUP, STOWAGE_AZURE_STORAGE_ACCOUNT too") {
		t.Errorf("azure partial: %v", err)
	}
	if v, err := Vars("gcp", env(nil)); v != nil || err != nil {
		t.Errorf("gcp unset: %v, %v", v, err)
	}
	if _, err := Vars("ovh", env(nil)); err == nil {
		t.Error("unknown cloud taken")
	}
}

// fakeTofu records tofu's runs and what their working directory held; an apply leaves a state, and
// output gives upload in an upload directory, id in an image's, existing and unready in a probe's.
type fakeTofu struct {
	runs                          [][]string // the directory's base name, then the arguments
	env                           []string
	id, existing, unready, upload string
	fail                          string // the subcommand that fails
	files                         map[string]map[string]bool
}

func (f *fakeTofu) run(_ context.Context, args, env []string, _ io.Writer) ([]byte, error) {
	dir := strings.TrimPrefix(args[0], "-chdir=")
	base := filepath.Base(dir)
	f.runs, f.env = append(f.runs, append([]string{base}, args[1:]...)), env
	entries, _ := os.ReadDir(dir)
	if f.files == nil {
		f.files = map[string]map[string]bool{}
	}
	f.files[base] = map[string]bool{}
	for _, e := range entries {
		f.files[base][e.Name()] = true
	}
	if args[1] == f.fail {
		return nil, errors.New("tofu " + f.fail + ": exit status 1")
	}
	switch args[1] {
	case "apply":
		return nil, os.WriteFile(filepath.Join(dir, "terraform.tfstate"), []byte("{}"), 0o600)
	case "output":
		switch {
		case args[3] == "unready":
			return []byte(f.unready), nil
		case args[3] == "upload":
			return []byte(f.upload), nil
		case base == "probe":
			return []byte(f.existing), nil
		}
		return []byte(f.id), nil
	}
	return nil, nil
}

func readVars(t *testing.T, dir string) map[string]string {
	t.Helper()
	var vars map[string]string
	b, err := os.ReadFile(filepath.Join(dir, "terraform.tfvars.json"))
	if err == nil {
		err = json.Unmarshal(b, &vars)
	}
	if err != nil {
		t.Fatal(err)
	}
	return vars
}

func applied(dir, output string) [][]string {
	return [][]string{{dir, "init", "-input=false", "-no-color"}, {dir, "apply", "-auto-approve", "-input=false", "-no-color"}, {dir, "output", "-raw", output}}
}

// probedNone are the runs of a probe that finds no image of the name.
func probedNone() [][]string {
	return append(applied("probe", "id"), []string{"probe", "output", "-raw", "unready"})
}

// uploadRemoved are the runs that destroy the upload.
var uploadRemoved = [][]string{{"upload", "init", "-input=false", "-no-color"}, {"upload", "destroy", "-auto-approve", "-input=false", "-no-color"}}

func TestUploadRegistersOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := t.TempDir()
	tf := &fakeTofu{id: "projects/p/global/images/notes-0123456789ab", upload: "https://storage.googleapis.com/b/stowage/notes-0123456789ab.tar.gz"}
	u := &Uploader{WorkDir: work, Env: []string{"PATH=/bin"}, Tofu: tf.run}
	fetched := 0
	img := Image{Cloud: "gcp", Name: "notes-0123456789ab", Description: "notes disk", Vars: map[string]string{"project": "p", "bucket": "b"},
		Fetch: func(_ context.Context, dir string) (string, string, error) {
			fetched++
			p := filepath.Join(dir, "disk.tar.gz")
			return p, strings.Repeat("a", 64), os.WriteFile(p, []byte("disk"), 0o600)
		}}
	// Not in the account: the probe finds none.
	if id, err := u.Registered(ctx, img); id != "" || err != nil {
		t.Fatalf("registered %q, %v", id, err)
	}
	if !slices.EqualFunc(tf.runs, probedNone(), slices.Equal) || !tf.files["probe"]["main.tf"] || !tf.files["probe"][".terraform.lock.hcl"] {
		t.Errorf("probe runs %v, files %v", tf.runs, tf.files["probe"])
	}
	dir, err := u.dir(img)
	if err != nil {
		t.Fatal(err)
	}
	if got := readVars(t, filepath.Join(dir, "probe")); len(got) != 4 || got["name"] != img.Name || got["description"] != "notes disk" || got["project"] != "p" || got["bucket"] != "b" {
		t.Errorf("probe vars %v", got)
	}
	tf.runs = nil
	id, err := u.Upload(ctx, img)
	if err != nil || id != tf.id {
		t.Fatalf("upload: %q, %v", id, err)
	}
	// The disk uploaded, the image made of the upload, then the upload destroyed.
	want := slices.Concat(applied("upload", "upload"), applied(img.Name, "id"), uploadRemoved)
	if !slices.EqualFunc(tf.runs, want, slices.Equal) {
		t.Errorf("runs %v", tf.runs)
	}
	if f := tf.files["upload"]; !f["main.tf"] || !f[".terraform.lock.hcl"] || !f["terraform.tfvars.json"] {
		t.Errorf("upload directory %v", f)
	}
	if f := tf.files[img.Name]; !f["main.tf"] || !f[".terraform.lock.hcl"] || !f["terraform.tfvars.json"] || !f["disk.tar.gz"] {
		t.Errorf("working directory %v", f)
	}
	if !slices.Equal(tf.env, []string{"PATH=/bin", "TF_IN_AUTOMATION=1", "TF_INPUT=0", "TF_PLUGIN_CACHE_DIR=" + filepath.Join(work, "plugins"), "TF_CLI_CONFIG_FILE=" + filepath.Join(work, "tofurc")}) {
		t.Errorf("env %v", tf.env)
	}
	if b, err := os.ReadFile(filepath.Join(work, "tofurc")); err != nil || len(b) != 0 {
		t.Errorf("tofurc %q, %v", b, err)
	}
	if v := readVars(t, filepath.Join(dir, "upload")); v["file"] != filepath.Join(dir, "disk.tar.gz") || v["sha256"] != strings.Repeat("a", 64) || v["name"] != img.Name || v["bucket"] != "b" || v["description"] != "" {
		t.Errorf("upload vars %v", v)
	}
	if v := readVars(t, dir); v["upload"] != tf.upload || v["description"] != "notes disk" || v["name"] != img.Name || v["file"] != "" {
		t.Errorf("image vars %v", v)
	}
	if _, err := os.Stat(filepath.Join(dir, "disk.tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("disk kept: %v", err)
	}
	tf.runs = nil
	if got, err := u.Registered(ctx, img); got != tf.id || err != nil || tf.runs != nil || fetched != 1 {
		t.Errorf("registered %q, %v, runs %v, fetched %d", got, err, tf.runs, fetched)
	}
	// Another project is another account: its image is looked for there.
	other := img
	other.Vars = map[string]string{"project": "q", "bucket": "b"}
	if got, err := u.Registered(ctx, other); got != "" || err != nil || len(tf.runs) != 4 {
		t.Errorf("another project: %q, %v, runs %v", got, err, tf.runs)
	}
}

// A run killed once its image was made leaves the disk: it goes with the upload.
func TestRegisteredRemovesALeftDisk(t *testing.T) {
	t.Parallel()
	tf := &fakeTofu{id: "img-1"}
	u := &Uploader{WorkDir: t.TempDir(), Tofu: tf.run}
	img := Image{Cloud: "gcp", Name: "notes-1", Vars: map[string]string{"project": "p", "bucket": "b"}}
	dir, _ := u.dir(img)
	up := filepath.Join(dir, "upload")
	if err := os.MkdirAll(up, 0o700); err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(dir, "disk.tar.gz")
	for f, b := range map[string]string{filepath.Join(dir, "terraform.tfstate"): "{}", filepath.Join(up, "terraform.tfstate"): "{}", left: "disk",
		filepath.Join(up, "terraform.tfvars.json"): `{"file": "` + left + `"}`} {
		if err := os.WriteFile(f, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := u.Registered(context.Background(), img); id != "img-1" || err != nil {
		t.Fatalf("registered %q, %v", id, err)
	}
	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("disk left: %v", err)
	}
}

// An image of its name already in the account (another machine registered it, or the cache was
// cleared) is taken, never uploaded again; one not ready stops.
func TestRegisteredTakesTheAccountsImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tf := &fakeTofu{existing: "ami-0abc"}
	u := &Uploader{WorkDir: t.TempDir(), Tofu: tf.run}
	img := Image{Cloud: "aws", Name: "notes-1", Vars: map[string]string{"bucket": "b", "region": "eu-west-1", "vmimport_role": "vmimport"}}
	if id, err := u.Registered(ctx, img); id != "ami-0abc" || err != nil {
		t.Fatalf("registered %q, %v", id, err)
	}
	// No Env: this process's.
	if !slices.ContainsFunc(tf.env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		t.Errorf("env %v", tf.env)
	}
	tf.runs = nil
	if id, err := u.Registered(ctx, img); id != "ami-0abc" || err != nil || tf.runs != nil {
		t.Errorf("again %q, %v, runs %v", id, err, tf.runs)
	}
	tf3 := &fakeTofu{unready: "not available"}
	u.Tofu, u.WorkDir = tf3.run, t.TempDir()
	if _, err := u.Registered(ctx, img); err == nil || err.Error() != "cloud aws: image notes-1 exists and is not this disk's ready image (not available): wait for it, or delete it, then ship again" {
		t.Errorf("unready: %v", err)
	}
	tf2 := &fakeTofu{fail: "apply"}
	u.Tofu, u.WorkDir = tf2.run, t.TempDir()
	if _, err := u.Registered(ctx, img); err == nil || err.Error() != "cloud aws: look for image notes-1: tofu apply: exit status 1" {
		t.Errorf("failed probe: %v", err)
	}
}

// A run stopped once its image was made gives the module's id and removes its upload, without the
// disk (a file stands in for it); one stopped before looks at the account, then goes on with the
// next upload.
func TestRegisteredAfterAStoppedRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []struct{ id, existing, want string }{{"img-1", "never", "img-1"}, {"", "img-2", "img-2"}, {"", "", ""}} {
		tf := &fakeTofu{id: c.id, existing: c.existing}
		u := &Uploader{WorkDir: t.TempDir(), Tofu: tf.run}
		img := Image{Cloud: "gcp", Name: "notes-1", Vars: map[string]string{"project": "p", "bucket": "b"}}
		dir, _ := u.dir(img)
		up := filepath.Join(dir, "upload")
		if err := os.MkdirAll(up, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{filepath.Join(dir, "terraform.tfstate"), filepath.Join(up, "terraform.tfstate")} {
			if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		gone := filepath.Join(dir, "disk.tar.gz") // removed when the run stopped
		if err := os.WriteFile(filepath.Join(up, "terraform.tfvars.json"), []byte(`{"file": "`+gone+`", "sha256": "`+strings.Repeat("c", 64)+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		want := [][]string{{"notes-1", "output", "-raw", "id"}}
		switch {
		case c.existing == "":
			want = append(want, probedNone()...)
		case c.id == "":
			want = append(want, applied("probe", "id")...)
		}
		if c.want != "" {
			want = append(want, uploadRemoved...)
		}
		got, err := u.Registered(ctx, img)
		if got != c.want || err != nil || !slices.EqualFunc(tf.runs, want, slices.Equal) {
			t.Errorf("state, output %q, account %q: %q, %v, runs %v", c.id, c.existing, got, err, tf.runs)
		}
		if c.want != "" {
			// The upload destroyed with a file standing in for the disk, removed since.
			stand := filepath.Join(up, "disk.removed")
			if v := readVars(t, up); v["file"] != stand || v["sha256"] != strings.Repeat("c", 64) || v["name"] != "notes-1" {
				t.Errorf("upload vars %v", v)
			}
			if !tf.files["upload"]["disk.removed"] {
				t.Errorf("no stand-in when destroyed: %v", tf.files["upload"])
			}
			if _, err := os.Stat(stand); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("stand-in kept: %v", err)
			}
		}
		// What was found is kept: asked again, nothing runs.
		tf.runs = nil
		if again, _ := u.Registered(ctx, img); c.want != "" && (again != c.want || tf.runs != nil) {
			t.Errorf("kept: %q, runs %v", again, tf.runs)
		}
	}
}

func TestUploadFailures(t *testing.T) {
	t.Parallel()
	fetch := func(_ context.Context, dir string) (string, string, error) {
		p := filepath.Join(dir, "disk.raw")
		return p, strings.Repeat("b", 64), os.WriteFile(p, nil, 0o600)
	}
	vars := map[string]string{"bucket": "b", "region": "r", "vmimport_role": "vmimport"}
	for _, c := range []struct {
		name, fail, id, upload, want string
		fetch                        func(context.Context, string) (string, string, error)
	}{
		{"apply fails", "apply", "ami-1", "k", "cloud aws: upload: tofu apply: exit status 1", fetch},
		{"no upload", "", "ami-1", "", "cloud aws: the upload module gave no upload (<nil>)", fetch},
		{"no id", "", " \n", "k", "cloud aws: the module gave no image id", fetch},
		{"destroy fails too", "destroy", " ", "k", "cloud aws: the module gave no image id (and cloud aws: remove the upload of notes-1: tofu destroy: exit status 1: delete it by hand)", fetch},
		{"upload not removed", "destroy", "ami-1", "k", "cloud aws: remove the upload of notes-1: tofu destroy: exit status 1", fetch},
		{"fetch fails", "", "ami-1", "k", "cloud aws: denied", func(context.Context, string) (string, string, error) { return "", "", errors.New("denied") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tf := &fakeTofu{id: c.id, upload: c.upload, fail: c.fail}
			u := &Uploader{WorkDir: t.TempDir(), Tofu: tf.run}
			img := Image{Cloud: "aws", Name: "notes-1", Vars: vars, Fetch: c.fetch}
			if _, err := u.Upload(context.Background(), img); err == nil || err.Error() != c.want {
				t.Fatalf("got %v, want %s", err, c.want)
			}
			dir, _ := u.dir(img)
			if _, err := os.Stat(filepath.Join(dir, idFile)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("id recorded: %v", err)
			}
			// Once uploaded, the upload is destroyed whatever failed after.
			uploaded := slices.ContainsFunc(tf.runs, func(r []string) bool { return r[0] == "upload" && r[1] == "apply" })
			destroyed := slices.ContainsFunc(tf.runs, func(r []string) bool { return r[0] == "upload" && r[1] == "destroy" })
			if uploaded && c.fail != "apply" && !destroyed {
				t.Errorf("upload left: runs %v", tf.runs)
			}
		})
	}
	u := &Uploader{WorkDir: t.TempDir(), Tofu: (&fakeTofu{}).run}
	for _, img := range []Image{{Cloud: "aws", Name: "../x"}, {Cloud: "aws", Name: "Notes-1"}, {Cloud: "aws", Name: "notes-" + strings.Repeat("a", 58)}, {Cloud: "ovh", Name: "notes-1"}} {
		if _, err := u.Upload(context.Background(), img); err == nil {
			t.Errorf("%s/%s taken", img.Cloud, img.Name)
		}
		if _, err := u.Registered(context.Background(), img); err == nil {
			t.Errorf("%s/%s looked for", img.Cloud, img.Name)
		}
	}
	if _, err := u.Upload(context.Background(), Image{Cloud: "aws", Name: "notes-" + strings.Repeat("a", 57), Fetch: fetch}); err == nil || !strings.Contains(err.Error(), "upload module gave no upload") {
		t.Errorf("63 characters: %v", err)
	}
}

// Every cloud has a module, and the module declares exactly the variables the uploader writes.
func TestModulesDeclareTheirVariables(t *testing.T) {
	t.Parallel()
	for c, ss := range Settings {
		for _, m := range []struct {
			file string
			vars []string
		}{{c + "/main.tf", []string{"description", "name", "upload"}}, {c + "/upload/main.tf", []string{"file", "name", "sha256"}}, {c + "/probe/main.tf", []string{"description", "name"}}} {
			checkModule(t, m.file, m.vars, ss)
		}
		// The probe takes only this disk's image: its description, which names the disk's digest.
		if b, _ := modules.ReadFile("tofu/" + c + "/probe/main.tf"); !strings.Contains(string(b), "\noutput \"unready\" {") || strings.Count(string(b), "var.description") != 1 {
			t.Errorf("%s: the probe has no output unready, or does not match the description once", c)
		}
		if b, _ := modules.ReadFile("tofu/" + c + "/upload/main.tf"); !strings.Contains(string(b), "\noutput \"upload\" {") {
			t.Errorf("%s: the upload module has no output upload", c)
		}
		for _, lock := range []string{c + "/.terraform.lock.hcl", c + "/upload/.terraform.lock.hcl", c + "/probe/.terraform.lock.hcl"} {
			if b, err := modules.ReadFile("tofu/" + lock); err != nil || !strings.Contains(string(b), "h1:") {
				t.Errorf("%s: no checksums (%v)", lock, err)
			}
		}
	}
}

// checkModule checks that the module file declares vars and the cloud's settings, and outputs id.
func checkModule(t *testing.T, file string, vars []string, ss []Setting) {
	t.Helper()
	b, err := modules.ReadFile("tofu/" + file)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	want := slices.Clone(vars)
	for _, s := range ss {
		want = append(want, s.Var)
	}
	var got []string
	for l := range strings.SplitSeq(string(b), "\n") {
		if n, ok := strings.CutPrefix(l, `variable "`); ok {
			got = append(got, strings.TrimSuffix(n, `" {`))
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("%s: variables %v, want %v", file, got, want)
	}
	if !strings.HasSuffix(file, "/upload/main.tf") && !strings.Contains(string(b), "\noutput \"id\" {") {
		t.Errorf("%s: no output id", file)
	}
}
