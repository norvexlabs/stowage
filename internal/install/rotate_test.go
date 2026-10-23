package install

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

// rotating makes SESSION_SECRET a rotated secret.
func rotating(s *spec.Spec) {
	for i := range s.Secrets {
		if s.Secrets[i].Key == "SESSION_SECRET" {
			s.Secrets[i].Rotation = true
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
		if err := rotate(pass); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", pass, err)
		}
		if e.file(".env") != before {
			t.Fatalf("a refused %s changed .env", pass)
		}
	}
	ups := func() int {
		n := 0
		for _, c := range e.docker.calls {
			if slices.Contains(c.Args, "up") {
				n++
			}
		}
		return n
	}
	refused(RotateSwap, "no new SESSION_SECRET is staged")
	if err := e.installer(nil).RotateSecret(context.Background(), "POSTGRES_PASSWORD", RotateStage); err == nil || !strings.Contains(err.Error(), "POSTGRES_PASSWORD is no secret with rotation: true (rotated: SESSION_SECRET)") {
		t.Fatalf("a secret without rotation: %v", err)
	}
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
	if err := e.installer(nil).K8sRotateSecret(context.Background(), "SESSION_SECRET", RotateFinish); err == nil || !strings.Contains(err.Error(), "is staged, not in use") {
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
