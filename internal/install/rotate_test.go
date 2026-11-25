package install

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// The verbs that need the release refuse a namespace without it, as `k8s status` does
// (noRelease): exit 3, the install to run named.
func TestK8sVerbsRefuseNoRelease(t *testing.T) {
	t.Parallel()
	const want = "no release notes in notes: `notes-stow k8s install --namespace notes --release notes --no-provision` installs 1.0.0" // the fixture gives --no-provision
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, rotating)
	e.f.listed = nil
	if err := e.installer(nil).K8sRotateSecret(context.Background(), "SESSION_SECRET", RotateStage); !isRefused(err) || err.Error() != want {
		t.Errorf("secrets rotate: %v", err)
	}
	e = k8sIssuing(t)
	e.f.listed = nil
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); !isRefused(err) || err.Error() != want {
		t.Errorf("kit create: %v", err)
	}
	if _, err := e.installer(nil).K8sCheckKit(context.Background(), "client", "site-1"); !isRefused(err) || err.Error() != want {
		t.Errorf("kit check: %v", err)
	}
}

// rotating makes SESSION_SECRET a rotated secret.
func rotating(s *spec.Spec) {
	for i := range s.Secrets {
		if s.Secrets[i].Key == "SESSION_SECRET" {
			s.Secrets[i].Rotation = true
		}
	}
}

// replacing makes SESSION_SECRET a secret the one pass replaces.
func replacing(s *spec.Spec) {
	for i := range s.Secrets {
		if s.Secrets[i].Key == "SESSION_SECRET" {
			s.Secrets[i].Replace = true
		}
	}
}

func TestASecretRotatesInThreePassesOnDocker(t *testing.T) {
	e := newEnv(t)
	rotating(e.r.Spec)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	f := e.readEnv()
	old := get(f, "SESSION_SECRET")
	for _, k := range []string{"SESSION_SECRET_NEXT", "SESSION_SECRET_PREVIOUS", "SESSION_SECRET_ACCEPTED"} {
		if v, ok := f.Get(k); !ok || v != "" {
			t.Fatalf("%s = %q, %v: recorded empty from the first install", k, v, ok)
		}
	}
	rotate := func(pass string) error {
		e.docker.calls = nil
		return e.installer(nil).RotateSecret(context.Background(), "SESSION_SECRET", pass)
	}
	refused := func(pass, want string) {
		t.Helper()
		before := e.file(".env")
		err := rotate(pass)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", pass, err)
		}
		// A pass out of order, a key or a way it does not take: a refusal, fixed by the command to run.
		if r, ok := Refusal(err); !ok || !strings.Contains(r.Fix, " secrets rotate SESSION_SECRET") || !strings.HasSuffix(r.Fix, " --dir "+e.dir) {
			t.Fatalf("%s: %v is no refusal fixed by the install's command: %+v", pass, err, r)
		}
		if e.file(".env") != before {
			t.Fatalf("a refused %s changed .env", pass)
		}
	}
	ups := func() int {
		n := 0
		for _, c := range e.docker.calls {
			if slices.Contains(c.Args, "--detach") {
				n++
			}
		}
		return n
	}
	refused(RotateSwap, "no new SESSION_SECRET is staged")
	if err := e.installer(nil).RotateSecret(context.Background(), "POSTGRES_PASSWORD", RotateStage); err == nil ||
		err.Error() != "POSTGRES_PASSWORD has neither rotation: true nor replace: true: its release lets `secrets rotate` change it in no way (rotated or replaced: SESSION_SECRET)" {
		t.Fatalf("a secret with neither: %v", err)
	}
	// The fix, as the operator types it: the install's own installer, its directory named.
	ctl := filepath.Join(e.dir, "notes-stow")
	refused(RotateReplace, "SESSION_SECRET has rotation: true: it is replaced in three passes, `"+ctl+" secrets rotate SESSION_SECRET --stage --dir "+e.dir+"` first")
	if err := rotate(RotateStage); err != nil {
		t.Fatal(err)
	}
	f = e.readEnv()
	next := get(f, "SESSION_SECRET_NEXT")
	if next == "" || next == old || get(f, "SESSION_SECRET") != old || get(f, "SESSION_SECRET_ACCEPTED") != next || ups() != 1 {
		t.Fatalf("after --stage: %q %q, %d ups", next, get(f, "SESSION_SECRET"), ups())
	}
	refused(RotateFinish, "a new SESSION_SECRET is staged, not in use")
	// A pass run again (its restart failed) brings the stack up again, changing nothing.
	if err := rotate(RotateStage); err != nil || get(e.readEnv(), "SESSION_SECRET_NEXT") != next || ups() != 1 {
		t.Fatalf("--stage again: %v, %d ups", err, ups())
	}
	if err := rotate(RotateSwap); err != nil {
		t.Fatal(err)
	}
	f = e.readEnv()
	if get(f, "SESSION_SECRET") != next || get(f, "SESSION_SECRET_PREVIOUS") != old || get(f, "SESSION_SECRET_NEXT") != "" || get(f, "SESSION_SECRET_ACCEPTED") != old || ups() != 1 {
		t.Fatalf("after the swap:\n%s", e.file(".env"))
	}
	refused(RotateStage, "the last rotation of SESSION_SECRET is not finished")
	if err := rotate(RotateSwap); err != nil || get(e.readEnv(), "SESSION_SECRET") != next || get(e.readEnv(), "SESSION_SECRET_PREVIOUS") != old {
		t.Fatalf("the swap again: %v\n%s", err, e.file(".env"))
	}
	if err := rotate(RotateFinish); err != nil {
		t.Fatal(err)
	}
	if f = e.readEnv(); get(f, "SESSION_SECRET") != next || get(f, "SESSION_SECRET_PREVIOUS") != "" || get(f, "SESSION_SECRET_ACCEPTED") != "" || ups() != 1 {
		t.Fatalf("after --finish:\n%s", e.file(".env"))
	}
	if !strings.Contains(e.log.String(), "dropped the previous SESSION_SECRET: the rotation is done") {
		t.Fatalf("log:\n%s", e.log)
	}
}

// A secret with replace: true is replaced in one pass: a new value by its gen, the stack up once
// with it; refused, changing nothing, for a key the spec does not hold and for one with neither
// replace nor rotation (lint keeps replace off what the database reads).
func TestASecretWithoutRotationIsReplacedOnDocker(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	replacing(e.r.Spec)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	old, pw := get(e.readEnv(), "SESSION_SECRET"), get(e.readEnv(), "POSTGRES_PASSWORD")
	for key, want := range map[string]string{
		"POSTGRES_PASSWORD": "POSTGRES_PASSWORD has neither rotation: true nor replace: true: its release lets `secrets rotate` change it in no way (rotated or replaced: SESSION_SECRET)",
		"NO_SUCH_SECRET":    "NO_SUCH_SECRET is no secret of notes (secrets: SESSION_SECRET, POSTGRES_PASSWORD)",
	} {
		before := e.file(".env")
		if err := e.installer(nil).RotateSecret(context.Background(), key, RotateReplace); err == nil || err.Error() != want {
			t.Errorf("%s: %v", key, err)
		}
		if e.file(".env") != before {
			t.Errorf("a refused replace of %s changed .env", key)
		}
	}
	for _, pass := range []string{RotateStage, RotateSwap, RotateFinish} {
		if err := e.installer(nil).RotateSecret(context.Background(), "SESSION_SECRET", pass); err == nil ||
			err.Error() != "SESSION_SECRET has replace: true: `"+filepath.Join(e.dir, "notes-stow")+" secrets rotate SESSION_SECRET --dir "+e.dir+"` replaces it in one pass, without --stage or --finish" {
			t.Errorf("%s: %v", pass, err)
		}
	}
	e.docker.calls = nil
	if err := e.installer(nil).RotateSecret(context.Background(), "SESSION_SECRET", RotateReplace); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	f := e.readEnv()
	ups := 0
	for _, c := range e.docker.calls {
		if slices.Contains(c.Args, "--detach") {
			ups++
		}
	}
	if v := get(f, "SESSION_SECRET"); len(v) != 64 || v == old || get(f, "POSTGRES_PASSWORD") != pw || ups != 1 {
		t.Fatalf("after the replace: %q (was %q), %d ups", v, old, ups)
	}
	for _, k := range []string{"SESSION_SECRET_NEXT", "SESSION_SECRET_PREVIOUS"} {
		if _, ok := f.Get(k); ok {
			t.Errorf("%s recorded for a secret without rotation", k)
		}
	}
	if !strings.Contains(e.log.String(), "recorded a new SESSION_SECRET: the services reading it are recreated") || strings.Contains(e.log.String(), get(f, "SESSION_SECRET")) {
		t.Fatalf("log:\n%s", e.log)
	}
}

// `secrets rotate` is offered on Docker only when a secret takes a way of it: the three passes'
// flags with a secret of rotation: true, the one pass with one it refuses nothing of; and the
// secrets help names only the verbs there.
func TestDockersRotateIsOfferedWhereASecretTakesIt(t *testing.T) {
	t.Parallel()
	help := func(t *testing.T, edit func(*spec.Spec), args ...string) string {
		e := newEnv(t)
		edit(e.r.Spec)
		var out bytes.Buffer
		cmd := newCommand(e.r, e.self, &out, &out, K8sOptions{}, func(bool) Prompter { return nil })
		cmd.SetArgs(append(args, "--help"))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	settable := func(s *spec.Spec) { s.Secrets[0].Settable = true } // SESSION_SECRET; POSTGRES_PASSWORD the database reads
	if out := help(t, settable, "secrets"); strings.Contains(out, "rotate") || !strings.Contains(out, "The install's secrets: set one, show a settable one or export them all") {
		t.Errorf("nothing to rotate:\n%s", out)
	}
	if out := help(t, func(*spec.Spec) {}, "secrets"); strings.Contains(out, "rotate") {
		t.Errorf("neither rotation nor replace:\n%s", out)
	}
	if out := help(t, replacing, "secrets", "rotate"); strings.Contains(out, "--stage") || !strings.Contains(out, "Replace a generated secret with replace: true in one pass, confirmed first") {
		t.Errorf("the one pass alone:\n%s", out)
	}
	if out := help(t, rotating, "secrets", "rotate"); !strings.Contains(out, "--stage") || strings.Contains(out, "--yes") {
		t.Errorf("the three passes alone:\n%s", out)
	}
}

// On Docker the one pass refuses before it asks: no install in --dir, and --yes on a secret of
// three passes; the question comes once every check of its own passed.
func TestDockersReplaceRefusesBeforeItAsks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rotating(e.r.Spec)
	e.r.Spec.Secrets = append(e.r.Spec.Secrets, spec.Secret{Key: "OTHER_TOKEN", Gen: "hex32", Replace: true})
	run := func(args ...string) error {
		cmd := newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { t.Fatal("asked"); return nil })
		cmd.SetArgs(append(args, "--dir", e.dir))
		return cmd.Execute()
	}
	if err := run("secrets", "rotate", "OTHER_TOKEN"); err == nil || !strings.Contains(err.Error(), "holds no install") {
		t.Errorf("no install: %v", err)
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	for args, want := range map[string]string{
		"SESSION_SECRET --yes":            "--yes, --no-input and --accessible confirm the one pass of a secret with replace: true, which SESSION_SECRET is not: leave them out",
		"SESION_SECRET --yes":             "SESION_SECRET is no secret of notes (secrets: SESSION_SECRET, POSTGRES_PASSWORD, OTHER_TOKEN)",
		"OTHER_TOKEN --no-input":          "secrets rotate OTHER_TOKEN replaces it at once, and --no-input asks nothing: give --yes",
		"SESSION_SECRET --stage --finish": "--stage and --finish are two passes: give one",
	} {
		// Each a refusal: exit 3, nothing changed.
		if err := run(append([]string{"secrets", "rotate"}, strings.Fields(args)...)...); err == nil || err.Error() != want || !isRefused(err) {
			t.Errorf("%s: %v", args, err)
		}
	}
	// The fix of an unconfirmed one pass runs as given: the install's installer, its directory named.
	err := run("secrets", "rotate", "OTHER_TOKEN", "--no-input")
	if r, _ := Refusal(err); r.Fix != filepath.Join(e.dir, "notes-stow")+" secrets rotate OTHER_TOKEN --yes --dir "+e.dir {
		t.Errorf("--no-input's fix: %q", r.Fix)
	}
}

// Kubernetes takes the three passes alone: a secret with replace: true is refused, saying where
// the one pass runs.
func TestKubernetesRefusesTheOnePass(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) {
		rotating(s)
		s.Secrets = append(s.Secrets, spec.Secret{Key: "OTHER_TOKEN", Gen: "hex32", Replace: true})
	})
	if err := e.installer(nil).K8sRotateSecret(context.Background(), "OTHER_TOKEN", RotateSwap); err == nil ||
		err.Error() != "OTHER_TOKEN has replace: true, which `secrets rotate` replaces in one pass on Docker and the VM: `k8s secrets rotate` takes a secret with rotation: true alone" {
		t.Errorf("%v", err)
	}
}

func TestASecretRotatesInThreePassesOnKubernetes(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, rotating)
	if err := e.installer(nil).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	rec := e.secretData("notes-stowage")
	for _, k := range []string{"SESSION_SECRET_NEXT", "SESSION_SECRET_ACCEPTED"} {
		if v, ok := rec[k]; !ok || v != "" {
			t.Fatalf("%s = %q, %v", k, v, ok)
		}
	}
	old := rec["SESSION_SECRET"]
	e.runs("1.0.0", 1)
	e.stored(rec, ours)
	reads := func(how string) string {
		return "spec: {template: {spec: {containers: [{name: c, " + how + "}]}}}\n"
	}
	e.f.manifest = "kind: Service\nmetadata: {name: notes-api}\n" + reads("env: [{name: X, valueFrom: {secretKeyRef: {name: notes-stowage, key: SESSION_SECRET}}}]") +
		"---\nkind: Deployment\nmetadata: {name: notes-api}\n" + reads("env: [{name: X, valueFrom: {secretKeyRef: {name: notes-stowage, key: SESSION_SECRET_PREVIOUS}}}]") +
		"---\nkind: Deployment\nmetadata: {name: notes-web}\n" + reads("env: [{name: X, valueFrom: {secretKeyRef: {name: notes-stowage, key: POSTGRES_PASSWORD}}}]") +
		"---\nkind: StatefulSet\nmetadata: {name: notes-postgres}\n" + reads("envFrom: [{secretRef: {name: notes-stowage}}]") +
		"---\nkind: Deployment\nmetadata: {name: notes-files}\n" + reads("volumeMounts: []}], volumes: [{name: v, projected: {sources: [{secret: {name: notes-stowage}}]}}], x: [{y: 1") +
		"---\nkind: Deployment\nmetadata: {name: notes-accepted}\n" + reads("env: [{name: X, valueFrom: {secretKeyRef: {name: notes-stowage, key: SESSION_SECRET_ACCEPTED}}}]") +
		"---\nkind: DaemonSet\nmetadata: {name: notes-other}\n" + reads("env: [{name: X, valueFrom: {secretKeyRef: {name: other, key: SESSION_SECRET}}}]") +
		"---\nkind: Deployment\nmetadata: {name: elsewhere, namespace: other}\n" + reads("envFrom: [{secretRef: {name: notes-stowage}}]")
	e.f.applied, e.f.calls = nil, nil
	if err := e.installer(nil).K8sRotateSecret(context.Background(), "SESSION_SECRET", RotateStage); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	got := e.secretData("notes-stowage")
	if got["SESSION_SECRET_NEXT"] == "" || got["SESSION_SECRET"] != old || got["SESSION_SECRET_ACCEPTED"] != got["SESSION_SECRET_NEXT"] || got["POSTGRES_PASSWORD"] != rec["POSTGRES_PASSWORD"] {
		t.Fatalf("after --stage: %v", got)
	}
	var restarted []string
	for _, c := range e.f.ran("kubectl rollout") {
		restarted = append(restarted, strings.Join(c[2:], " "))
	}
	want := []string{"restart deployment/notes-api --namespace notes", "status deployment/notes-api --timeout 10m0s --namespace notes",
		"restart statefulset/notes-postgres --namespace notes", "status statefulset/notes-postgres --timeout 10m0s --namespace notes",
		"restart deployment/notes-files --namespace notes", "status deployment/notes-files --timeout 10m0s --namespace notes",
		"restart deployment/notes-accepted --namespace notes", "status deployment/notes-accepted --timeout 10m0s --namespace notes"}
	if !slices.Equal(restarted, want) {
		t.Fatalf("rollouts:\n%q\nwant\n%q", restarted, want)
	}
	e.stored(got, ours)
	if err := e.installer(nil).K8sRotateSecret(context.Background(), "SESSION_SECRET", RotateFinish); err == nil || !strings.Contains(err.Error(), "is staged, not in use: `notes-stow k8s secrets rotate SESSION_SECRET --namespace notes --release notes` puts it in use first") || !isRefused(err) {
		t.Fatalf("--finish while staged: %v", err)
	}
	e.runs("0.9.0", 1)
	if err := e.installer(nil).K8sRotateSecret(context.Background(), "SESSION_SECRET", RotateSwap); err == nil || !strings.Contains(err.Error(), "rotate with the installer of 0.9.0") {
		t.Fatalf("another release: %v", err)
	}
}

// An install made before KEY_ACCEPTED, its rotation under way, is given it by the upgrade: the
// staged value, as the services accept it.
func TestUpgradeRecordsTheAcceptedValueOfARotation(t *testing.T) {
	e := newEnv(t)
	rotating(e.r.Spec)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	f := e.readEnv()
	f.Set("SESSION_SECRET_NEXT", "staged")
	f.Delete("SESSION_SECRET_ACCEPTED")
	if err := f.WriteFile(filepath.Join(e.dir, ".env")); err != nil {
		t.Fatal(err)
	}
	e.to("2.0.0", rotating)
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if v := get(e.readEnv(), "SESSION_SECRET_ACCEPTED"); v != "staged" {
		t.Fatalf("SESSION_SECRET_ACCEPTED = %q", v)
	}
}

// KEY_ACCEPTED is a secret, as KEY_NEXT and KEY_PREVIOUS are, and stays recorded while its
// secret rotates.
func TestTheAcceptedValueIsASecret(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rotating(e.r.Spec)
	if !e.installer(nil).secretKey("SESSION_SECRET_ACCEPTED") || e.installer(nil).secretKey("POSTGRES_PASSWORD_ACCEPTED") {
		t.Error("secretKey")
	}
	from := &Release{Spec: &spec.Spec{Inputs: []spec.Input{{Key: "SESSION_SECRET_ACCEPTED"}}}}
	if r := removedBy(from, e.r); slices.Contains(r.inputs, "SESSION_SECRET_ACCEPTED") {
		t.Errorf("an input now the rotated secret's accepted value is dropped: %v", r.inputs)
	}
}

// A Kubernetes upgrade during a rotation records KEY_ACCEPTED from the Secret's staged value.
func TestK8sUpgradeRecordsTheAcceptedValueOfARotation(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, rotating)
	e.runs("0.9.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p",
		"SESSION_SECRET_NEXT": "staged", "SESSION_SECRET_PREVIOUS": ""}, ours)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := e.secretData("notes-stowage"); got["SESSION_SECRET_ACCEPTED"] != "staged" || got["SESSION_SECRET_NEXT"] != "staged" {
		t.Fatalf("%v", got)
	}
}

// The command a hint names runs as given: through a shell, Docker's directory with a space and
// Kubernetes's namespace, release and context reach the installer as one argument each; the VM's
// runs as root.
func TestSecretsCmdRunsAsGiven(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "my app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testutil.WriteExecutable(filepath.Join(dir, "notes-stow"), []byte("#!/bin/sh\nfor a; do printf '%s\\n' \"$a\"; done\n")); err != nil {
		t.Fatal(err)
	}
	r := &Release{Spec: &spec.Spec{Name: "notes"}}
	in := &Installer{R: r, O: Options{Dir: dir, K8s: K8sOptions{Namespace: "my ns", Release: "notes prod", Context: "a ctx"}}}
	for shape, want := range map[string][]string{
		onDocker: {"secrets", "rotate", "K", "--yes", "--dir", dir},
		onK8s:    {"k8s", "secrets", "rotate", "K", "--yes", "--namespace", "my ns", "--release", "notes prod", "--kube-context", "a ctx"},
	} {
		cmd := exec.Command("sh", "-c", in.secretsCmd(shape, "rotate K --yes"))
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
		out, err := cmd.Output()
		if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %q run by a shell gave %q (%v), want %q", shape, in.secretsCmd(shape, "rotate K --yes"), got, err, want)
		}
	}
	if got := in.secretsCmd(onVM, "rotate K --yes"); got != "sudo notes-stow secrets rotate K --yes" {
		t.Errorf("the VM's: %s", got)
	}
}
