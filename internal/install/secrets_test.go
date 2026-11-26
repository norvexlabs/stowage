package install

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"
	"filippo.io/age/armor"
	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

// withSecretInput adds a secret input to the minimal example.
func withSecretInput(s *spec.Spec) {
	s.Inputs = append(s.Inputs, spec.Input{Key: "SMTP_PASSWORD", Prompt: "SMTP password", Secret: true, Required: true})
}

// decrypt reads an exported file with key.
func decrypt(t *testing.T, raw []byte, key *age.X25519Identity) []byte {
	t.Helper()
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(raw)), key)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func TestSecretsExportedComeBackOnAMachineMadeAgain(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", withSecretInput)
	e.vars["SMTP_PASSWORD"] = "smtp-pw"
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if err := os.WriteFile(filepath.Join(e.dir, ProvisionEnv), []byte("ADMIN_PASSWORD='hook-pw'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	was := e.readEnv()
	key, _ := age.GenerateX25519Identity()
	raw, err := e.installer(nil).ExportSecrets([]string{key.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(armor.Header)) || bytes.Contains(raw, []byte(get(was, "SESSION_SECRET"))) {
		t.Fatalf("not an armored age file:\n%s", raw)
	}
	plain := decrypt(t, raw, key)
	var f SecretsFile
	if err := yaml.Unmarshal(plain, &f); err != nil {
		t.Fatal(err)
	}
	// The generated secrets, the secret input, the hook's record; nothing that is not a secret.
	if f.Project != "notes" || !f.Export || f.Secrets["SESSION_SECRET"] != get(was, "SESSION_SECRET") || f.Secrets["POSTGRES_PASSWORD"] != get(was, "POSTGRES_PASSWORD") ||
		f.Secrets["SMTP_PASSWORD"] != "smtp-pw" || f.Provision["ADMIN_PASSWORD"] != "hook-pw" || len(f.Secrets) != 3 {
		t.Fatalf("exported %+v", f)
	}
	// The machine made again: a new directory, the secrets given, the environment empty of them.
	e.dir = filepath.Join(t.TempDir(), "notes")
	delete(e.vars, "SMTP_PASSWORD")
	if err := e.install(func(o *Options) { o.Secrets = plain }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	now := e.readEnv()
	for _, k := range []string{"SESSION_SECRET", "POSTGRES_PASSWORD", "SMTP_PASSWORD"} {
		if get(now, k) != get(was, k) {
			t.Errorf("%s = %q, want %q", k, get(now, k), get(was, k))
		}
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, ProvisionEnv)); string(b) != "ADMIN_PASSWORD='hook-pw'\n" {
		t.Errorf("provision.env %q", b)
	}
	// A re-run keeps the install's own.
	if err := e.install(func(o *Options) { o.Secrets = plain }); err == nil || !strings.Contains(err.Error(), "--secrets is for a first install") {
		t.Errorf("re-run: %v", err)
	}
}

func TestASecretsFileLackingAKeyOrCarryingAnotherIsTold(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// ADMIN_EMAIL is an input, but no secret: the file never gives it.
	plain := []byte("project: notes\nsecrets:\n  SESSION_SECRET: kept\n  OLD_KEY: x\n  ADMIN_EMAIL: evil@example.org\n")
	if err := e.install(func(o *Options) { o.Secrets = plain }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	f := e.readEnv()
	if get(f, "SESSION_SECRET") != "kept" || len(get(f, "POSTGRES_PASSWORD")) != 18 || get(f, "OLD_KEY") != "" || get(f, "ADMIN_EMAIL") != "admin@example.org" {
		t.Errorf("env %v", f.Keys())
	}
	for _, want := range []string{"--secrets: OLD_KEY is no secret of notes 1.0.0: left out", "--secrets: the file has no POSTGRES_PASSWORD: the install generates a new one"} {
		if !strings.Contains(e.log.String(), want) {
			t.Errorf("no %q in\n%s", want, e.log)
		}
	}
}

func TestASecretsFileIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	t.Parallel()
	key, _ := age.GenerateX25519Identity()
	enc, err := encryptSecrets(&SecretsFile{Project: "notes", Secrets: map[string]string{"SESSION_SECRET": "x"}}, "notes-stow", []string{key.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		file []byte
		want string
	}{
		"another project's": {[]byte("project: crm\nsecrets: {}\n"), `the file holds "crm"'s secrets, not notes's`},
		"still encrypted":   {enc, "the file is still encrypted"},
		"an unknown field":  {[]byte("project: notes\nsecret: {}\n"), "field secret not found"},
		"too big":           {bytes.Repeat([]byte("#"), maxSecretsFile+1), "more than"},
	} {
		e := newEnv(t)
		if err := e.install(func(o *Options) { o.Secrets = c.file }); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
		e.nothingWritten()
	}
}

func TestSecretsExportRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := e.installer(nil).ExportSecrets([]string{"age1x"}); err == nil || !strings.Contains(err.Error(), "holds no install") {
		t.Errorf("no install: %v", err)
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		list []string
		want string
	}{
		{nil, "recipients: 1 to 16"},
		{[]string{"age1nope"}, `recipient "age1nope" is not an age public key`},
		{[]string{"a\nb"}, "one age public key"},
		{strings.Fields(strings.Repeat("x ", 17)), "recipients: 1 to 16"},
	} {
		if _, err := e.installer(nil).ExportSecrets(c.list); err == nil || !strings.Contains(err.Error(), c.want) || !isRefused(err) {
			t.Errorf("%q: %v", c.list, err)
		}
	}
}

func TestTheSecretsVerbsWriteAFileOfTheOperatorsOwn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	key, _ := age.GenerateX25519Identity()
	out := filepath.Join(t.TempDir(), "s.age")
	run := func(args ...string) (string, error) {
		var o, l bytes.Buffer
		cmd := newCommand(e.r, e.self, &o, &l, K8sOptions{}, func(bool) Prompter { return nil })
		cmd.SetArgs(append(args, "--dir", e.dir))
		err := cmd.ExecuteContext(context.Background())
		return o.String() + l.String(), err
	}
	if s, err := run("secrets", "export", "--recipient", key.Recipient().String(), "-o", out); err != nil || !strings.Contains(s, "keep it off this machine") {
		t.Fatalf("%v\n%s", err, s)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", err, fi)
	}
	raw, _ := os.ReadFile(out)
	if !strings.Contains(string(decrypt(t, raw, key)), "SESSION_SECRET:") {
		t.Error("no secret in the file")
	}
	if _, err := run("secrets", "export", "--recipient", key.Recipient().String(), "-o", out); err == nil || !errors.Is(err, os.ErrExist) {
		t.Errorf("over a file: %v", err)
	}
	if _, err := run("secrets", "export"); err == nil || !strings.Contains(err.Error(), "--recipient is required") || !isRefused(err) {
		t.Errorf("no recipient: %v", err)
	}
}

func TestASecretsInstallWaitsForItsDataBeforeTheHook(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	plain := []byte("project: notes\nexport: true\nsecrets:\n  SESSION_SECRET: s3cr3t\nprovision:\n  ADMIN_KEY: key-of-the-old-data\n")
	if err := e.install(func(o *Options) { o.Secrets = plain }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	// The restored values belong to another install's data: the hook waits for it.
	if n := len(e.hookCalls()); n != 0 {
		t.Fatalf("the hook ran %d times on the empty database", n)
	}
	if _, err := os.Stat(filepath.Join(e.dir, restoreAwaitedFile)); err != nil {
		t.Fatalf("no restore-awaited marker: %v", err)
	}
	// Docker's restore, with the install's directory: the records of the digests went with the
	// old install.
	if log := e.log.String(); !strings.Contains(log, "notes-stow backup config --dir "+e.dir+"` with the old install's destination, then `") ||
		!strings.Contains(log, "notes-stow restore OBJECT --unrecorded --dir "+e.dir+"` loads a backup of it") {
		t.Errorf("log: %s", log)
	}
	// The install is finished: its stack is up, the hook waits by design.
	if _, err := os.Stat(filepath.Join(e.dir, installingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the install is marked as not finished")
	}
	var out bytes.Buffer
	if err := e.installer(func(o *Options) { o.Out = &out }).Status(context.Background()); err != nil || !strings.Contains(out.String(), "awaiting its data: made again by --secrets: `") {
		t.Errorf("status: %v\n%s", err, out.String())
	}
	// A re-run waits too.
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if n := len(e.hookCalls()); n != 0 {
		t.Fatalf("a re-run ran the hook %d times", n)
	}
	// `provision`, the data loaded by hand: the hook runs with the restored values, the marker goes.
	if err := e.installer(nil).Provision(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if n := len(e.hookCalls()); n != 1 || !slices.Contains(e.docker.hookEnv, "ADMIN_KEY='key-of-the-old-data'") {
		t.Fatalf("%d hook runs, given %v", n, e.docker.hookEnv)
	}
	if _, err := os.Stat(filepath.Join(e.dir, restoreAwaitedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the marker is left after provision")
	}
	// From now on the hook runs as on any install.
	if err := e.install(nil); err != nil || len(e.hookCalls()) != 2 {
		t.Fatalf("%v: %d hook runs", err, len(e.hookCalls()))
	}
}

func TestASecretsInstallWithoutTheHookStillWaitsAndResetDBEndsTheWait(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	plain := []byte("project: notes\nexport: true\nsecrets:\n  SESSION_SECRET: s3cr3t\nprovision:\n  ADMIN_KEY: key-of-the-old-data\n")
	if err := e.install(func(o *Options) { o.Secrets = plain; o.NoProvision = true }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	// --no-provision says how the data comes, never to provision the empty database.
	if log := e.log.String(); !strings.Contains(log, "awaits its data: `") || strings.Contains(log, "not provisioned (--no-provision)") {
		t.Fatalf("--no-provision's log: %s", log)
	}
	if err := e.install(nil); err != nil || len(e.hookCalls()) != 0 {
		t.Fatalf("%v: a re-run after --no-provision ran the hook %d times on the empty database", err, len(e.hookCalls()))
	}
	// reset-db forgets the restored values: the reset database is provisioned afresh.
	e.docker.answer = volumes(get(e.readEnv(), "COMPOSE_PROJECT_NAME"), nil)
	if err := e.reset(func(o *Options) { o.Yes = true }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if n := len(e.hookCalls()); n != 1 || slices.Contains(e.docker.hookEnv, "ADMIN_KEY='key-of-the-old-data'") {
		t.Fatalf("%d hook runs, given %v", n, e.docker.hookEnv)
	}
	if _, err := os.Stat(filepath.Join(e.dir, restoreAwaitedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the marker is left after reset-db")
	}
}

func TestASecretsInstallWhoseStackFailsStillWaitsAndOneWithoutValuesToo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	plain := []byte("project: notes\nexport: true\nsecrets:\n  SESSION_SECRET: s3cr3t\nprovision:\n  ADMIN_KEY: key-of-the-old-data\n")
	e.docker.upErr = errors.New("a port is taken")
	if err := e.install(func(o *Options) { o.Secrets = plain }); err == nil {
		t.Fatal("the install succeeded with its stack failing")
	}
	// The re-run that finishes it is given no --secrets: the marker, written before the stack
	// started, is what keeps the hook off the empty database.
	e.docker.upErr = nil
	if err := e.install(nil); err != nil || len(e.hookCalls()) != 0 {
		t.Fatalf("%v: the re-run ran the hook %d times on the empty database", err, len(e.hookCalls()))
	}
	// A file holding no provision values is a machine made again too: it awaits its data.
	e2 := newEnv(t)
	if err := e2.install(func(o *Options) {
		o.Secrets = []byte("project: notes\nexport: true\nsecrets:\n  SESSION_SECRET: s3cr3t\n")
	}); err != nil {
		t.Fatalf("%v\n%s", err, e2.log)
	}
	if _, err := os.Stat(filepath.Join(e2.dir, restoreAwaitedFile)); err != nil || len(e2.hookCalls()) != 0 {
		t.Fatalf("marker %v, %d hook runs", err, len(e2.hookCalls()))
	}
	// provision, the operator's word that the data is the install's, ends the wait and runs it.
	if err := e2.installer(nil).Provision(context.Background()); err != nil || len(e2.hookCalls()) != 1 {
		t.Fatalf("provision: %v, %d hook runs", err, len(e2.hookCalls()))
	}
	if _, err := os.Stat(filepath.Join(e2.dir, restoreAwaitedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("still awaiting after provision: %v", err)
	}
}

// A release without a provision hook that runs backups: --secrets marks the install awaiting its
// data, provision (the operator's word) ends the wait, refusing --set as it has no hook. One that
// runs no backups, the file holding no provision values: nothing a restore could load, no wait.
func TestASecretsInstallWithoutAHook(t *testing.T) {
	t.Parallel()
	secretsOnly := []byte("project: notes\nexport: true\nsecrets:\n  SESSION_SECRET: s3cr3t\n")
	e := newEnv(t)
	e.r.Spec.Hooks.Provision = nil
	if !providers.RunsBackups(e.r.Spec) {
		t.Fatal("the test release runs no backups")
	}
	if err := e.install(func(o *Options) { o.Secrets = secretsOnly }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	marker := filepath.Join(e.dir, restoreAwaitedFile)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("no wait: %v", err)
	}
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"K": "v"} }).Provision(context.Background()); err == nil || !strings.Contains(err.Error(), "has no provision hook") {
		t.Fatalf("provision --set without a hook: %v", err)
	}
	if err := e.installer(nil).Provision(context.Background()); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) || !strings.Contains(e.log.String(), "ends the wait --secrets began") {
		t.Fatalf("still waiting: %v\n%s", err, e.log)
	}
	if err := e.installer(nil).Provision(context.Background()); err == nil || !strings.Contains(err.Error(), "has no provision hook") {
		t.Fatalf("provision with nothing awaited: %v", err)
	}
	e2 := newEnv(t)
	e2.r.Spec.Backup = nil
	if err := e2.install(func(o *Options) { o.Secrets = secretsOnly }); err != nil {
		t.Fatalf("%v\n%s", err, e2.log)
	}
	if _, err := os.Stat(filepath.Join(e2.dir, restoreAwaitedFile)); !errors.Is(err, os.ErrNotExist) || len(e2.hookCalls()) != 1 {
		t.Fatalf("a release without backups waits: %v, %d hook runs", err, len(e2.hookCalls()))
	}
	// Neither a hook nor backups: provision values the file holds are left out, nothing awaited.
	withValues := []byte("project: notes\nexport: true\nsecrets:\n  SESSION_SECRET: s3cr3t\nprovision:\n  ADMIN_KEY: old\n")
	e3 := newEnv(t)
	e3.r.Spec.Backup, e3.r.Spec.Hooks.Provision = nil, nil
	if err := e3.install(func(o *Options) { o.Secrets = withValues }); err != nil {
		t.Fatalf("%v\n%s", err, e3.log)
	}
	if _, err := os.Stat(filepath.Join(e3.dir, restoreAwaitedFile)); !errors.Is(err, os.ErrNotExist) || !strings.Contains(e3.log.String(), "are left out") {
		t.Fatalf("no hook, no backups: %v\n%s", err, e3.log)
	}
	// A hook given the values, no backups: it waits, the way on loading the data by hand.
	e4 := newEnv(t)
	e4.r.Spec.Backup = nil
	if err := e4.install(func(o *Options) { o.Secrets = withValues }); err != nil {
		t.Fatalf("%v\n%s", err, e4.log)
	}
	if _, err := os.Stat(filepath.Join(e4.dir, restoreAwaitedFile)); err != nil || !strings.Contains(e4.log.String(), "load a backup of it by hand, then `") || strings.Contains(e4.log.String(), "restore OBJECT") {
		t.Fatalf("no backups: %v\n%s", err, e4.log)
	}
	// A release without a hook that runs backups says it waits, at the install.
	if !strings.Contains(e.log.String(), "an install made again by --secrets awaits its data: `") {
		t.Fatalf("the wait unsaid:\n%s", e.log)
	}
}

// A secrets file without export: true is a new install's secrets a deployment seeds: the install
// takes them and is a plain one — not made again, awaiting nothing, on a release that runs backups
// and has a hook. Provision values in such a file are refused before anything is written.
func TestASeededSecretsFileMakesAPlainInstall(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if !providers.RunsBackups(e.r.Spec) || e.r.Spec.Hooks.Provision == nil {
		t.Fatal("the test release runs no backups or has no hook")
	}
	if err := e.install(func(o *Options) { o.Secrets = []byte("project: notes\nsecrets:\n  SESSION_SECRET: seeded\n") }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := get(e.readEnv(), "SESSION_SECRET"); got != "seeded" {
		t.Fatalf("SESSION_SECRET = %q", got)
	}
	for _, f := range []string{madeAgainFile, restoreAwaitedFile} {
		if _, err := os.Stat(filepath.Join(e.dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s written for a seeded file: %v", f, err)
		}
	}
	// Said, with the way on for an export written before the mark.
	if !strings.Contains(e.log.String(), "--secrets: a new install's secrets (no export: true): a plain install, not made again") ||
		!strings.Contains(e.log.String(), "give it no backup settings, add the line `export: true` to the file, and make the machine again from it") {
		t.Fatalf("not said:\n%s", e.log)
	}
	cert, key := testRoot(t, true)
	ca, _ := yaml.Marshal(SecretsFile{Project: "notes", Secrets: map[string]string{}, FrontCA: &FrontCA{Cert: string(cert), Key: string(key)}})
	for name, file := range map[string][]byte{
		"provision":              []byte("project: notes\nsecrets:\n  SESSION_SECRET: seeded\nprovision:\n  ADMIN_KEY: x\n"),
		"front_ca":               ca,
		"provision and front_ca": append(append([]byte{}, ca...), []byte("provision:\n  ADMIN_KEY: x\n")...),
	} {
		e2 := newEnv(t)
		err := e2.install(func(o *Options) { o.Secrets = file })
		if err == nil || !strings.Contains(err.Error(), "--secrets: "+name+": only a secrets export (export: true) gives "+name+", with its data") || !strings.Contains(err.Error(), "add the line `export: true`; for a new install's file, remove "+name) {
			t.Fatalf("%s in a seeded file: %v", name, err)
		}
		for _, f := range []string{".env", madeAgainFile, restoreAwaitedFile, ProvisionEnv} {
			if _, err := os.Stat(filepath.Join(e2.dir, f)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s: a refused file wrote %s: %v", name, f, err)
			}
		}
	}
}

// Records an earlier attempt left (its .env never written) do not make the next first install,
// given no export, one made again or awaiting its data: its hook runs.
func TestAStaleMadeAgainIsNotKept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{madeAgainFile, restoreAwaitedFile} {
		if err := os.WriteFile(filepath.Join(e.dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if st, out := statusOf(t, e, nil); st.MadeAgain || st.RestoreAwaited || len(e.hookCalls()) != 1 {
		t.Fatalf("made again, or awaiting (%d hook runs): %s", len(e.hookCalls()), out)
	}
}

// A first install given --secrets whose .env is not written removes the records it wrote: no
// install owns them.
func TestAFailedFirstWriteRemovesItsRecords(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "CA", Prompt: "CA file", Optional: true, Type: spec.InputFile})
	// The file inputs' directory a plain file: writing them, before .env, fails.
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, InputFilesDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := e.install(func(o *Options) {
		o.Secrets = []byte("project: notes\nexport: true\nsecrets:\n  SESSION_SECRET: s3cr3t\n")
	})
	if err == nil || fileExists(filepath.Join(e.dir, envName)) {
		t.Fatalf("the first write did not fail: %v", err)
	}
	for _, f := range []string{madeAgainFile, restoreAwaitedFile} {
		if fileExists(filepath.Join(e.dir, f)) {
			t.Errorf("%v: %s is left", err, f)
		}
	}
}
