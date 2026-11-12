#!/usr/bin/env bash
# End-to-end test of the Kubernetes shape: the real stowage and <short>-stow binaries, a real registry
# (zot, as in e2e-registry.sh), a kind cluster and pinned helm and kubectl. Needs a linux/amd64
# host with Docker and network access; kind, helm and kubectl are downloaded once, checked against
# their digests, into ~/.cache/stowage-tools.
#
#   make e2e-k8s           (builds ./bin/stowage first)
#
# The cluster pulls the images from zot itself: its containerd maps the registry's loopback address
# to zot on the cluster's network (a plain-HTTP mirror), and zot serves no anonymous pull, so every
# running pod proves the pull Secret the installer wrote.
#
# Walks examples/minimal through: ship 1.0.0 → the bootstrap script's `k8s install` (pods ready,
# the Secret's values in the pod's environment, the provision hook's Job run and what it returned
# kept in its Secret, not in the log) → `k8s upgrade --values` (2 replicas) → `k8s upgrade` alone
# keeps them, and hands the hook what it returned → `k8s kit create`, `k8s upgrade --kit`, `k8s kit
# check` → a Docker install beside it: a kit Docker issued taken by `k8s upgrade --kit`, one Kubernetes
# issued taken by the Docker install's `--kit` → ship 2.0.0 (a new web image) → `k8s upgrade --version 2.0.0` → the
# same again is a note → 1.0.0's `k8s upgrade` is refused → an upgrade that never becomes ready is
# rolled back by helm → `k8s rollback` goes back one upgrade, to 1.0.0 → `k8s status` → `k8s
# reset-db` deletes the release's claim, forgets what the hook returned and installs it again with
# the same values and Secret → `k8s uninstall` is refused without --data (helm would delete the
# chart's own claim), then `--data` removes the release, its Secrets, the hook's account and the
# claim, and keeps the namespace → the previous Stowage (the latest release tag, built once from it)
# ships 3.0.0 at schema baseline 1 and installs it; this one ships 4.0.0 at baseline 2, whose
# `k8s upgrade` is refused naming the previous release's baseline, `k8s reset-db --new-baseline`
# crosses, and that release issues and takes a kit, which its own spec holds. Along the way, the
# machine interface: `describe --json`; `k8s status --json` before the install and after each
# change; `--json` on install, upgrade, rollback and uninstall; the refusals exiting 3.
set -euo pipefail

# The amd64 image of the example's postgres: mirroring copies every platform of an index.
PG=postgres:18-alpine@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395
. "$(dirname "$0")/lib.sh"

DOCKER_OPERATOR=("${OPERATOR[@]}")
k8s_tools
NS=notes

# runs <version> <replicas>: the release, its pods ready, runs that version's web image, that many times.
runs() {
	local want img got
	want=$REG/notes/notes-web@$(digest_of "notes/notes-web:$1")
	kubectl rollout status -n "$NS" deploy/notes-web --timeout 120s >/dev/null || { cluster_logs >&2; fail "notes-web is not ready"; }
	img=$(kubectl get -n "$NS" deploy/notes-web -o jsonpath='{.spec.template.spec.containers[0].image}')
	[[ $img == "$want" ]] || fail "notes-web runs $img, want $want ($1)"
	got=$(kubectl get -n "$NS" deploy/notes-web -o jsonpath='{.status.readyReplicas}')
	[[ $got == "$2" ]] || { cluster_logs >&2; fail "$got web replicas ready, want $2"; }
	got=$(helm list -n "$NS" --filter '^notes$' --output json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["app_version"])')
	[[ $got == "$1" ]] || fail "helm lists $got, want $1"
}
# result <file> <done|nothing> [version]: the file, a run's standard output under --json, is one
# Result: format 1, that result, the release installed after it that version (none: absent).
result() {
	jq -se --arg r "$2" --arg v "${3:-}" 'length == 1 and (.[0] | .format == 1 and .result == $r and
		(if $v == "" then has("release") | not else .release.version == $v and .release.revision == 1 end))' "$1" >/dev/null ||
		{ cat "$1"; fail "$1 is not one Result: $2, release ${3:-none}"; }
}
# refused <file> <code> <reason ERE>: the run that wrote the file exited <code> (3, refused), and
# the Result in it is a refusal with that reason and a fix.
refused() {
	[[ $2 == 3 ]] || { cat "$1"; fail "$1: the run exited $2, not 3 (refused)"; }
	jq -e --arg re "$3" '.result == "nothing" and (.refused.reason | test($re)) and (.refused.fix | length > 0)' "$1" >/dev/null ||
		{ cat "$1"; fail "$1 is not a refusal matching $3, with a fix"; }
}
# status_json <script> <file> <installed> <previous|null>: k8s status --json of the release, run
# by the bootstrap script of the release installed (another tells no inputs), into the file:
# format 1, the releases installed and the one rollback goes back to (each with its installer by
# digest), the inputs as keys alone, helm's latest revision deployed, and no value of the
# release's Secret anywhere in it.
status_json() {
	local s=$1 installer="^$REG/notes/notes-installer@sha256:[0-9a-f]{64}\$" rev v n=0
	shift
	ctl "$s" k8s status --json >"$1" 2>"$1.log" || { cat "$1.log" "$1"; fail "k8s status --json"; }
	rev=$(helm history notes -n "$NS" --output json | jq 'map(.revision) | max')
	jq -se --arg v "$2" --arg p "$3" --arg i "$installer" --argjson rev "$rev" 'length == 1 and (.[0] | .format == 1 and .name == "notes" and
		.installed.version == $v and .installed.revision == 1 and (.installed.installer | test($i)) and
		(if $p == "null" then .previous == null else .previous.version == $p and .previous.revision == 1 and (.previous.installer | test($i)) end) and
		(.inputs | type == "array" and index("ADMIN_EMAIL") != null and all(.[]; test("^[A-Z][A-Z0-9_]*$"))) and
		.helm == {revision: $rev, status: "deployed"})' "$1" >/dev/null ||
		{ cat "$1"; fail "k8s status --json does not show $2 installed, $3 to go back to, the inputs by key, helm revision $rev deployed"; }
	while IFS= read -r v; do
		((${#v} >= 8)) || continue
		n=$((n + 1))
		! grep -qF -- "$v" "$1" "$1.log" || fail "k8s status --json prints a value of the release's Secret"
	done < <(kubectl get -n "$NS" secret notes-stowage -o json | jq -r '.data[] | @base64d')
	((n >= 2)) || fail "the release's Secret holds fewer than 2 values to search for"
}
# release <version>: bump the example, with a web page of its own (a new image), ship it on stable.
release() {
	printf '%s\n' "$1" >"$P/VERSION"
	printf '### Changed\n- Release %s.\n' "$1" >"$P/changes/$1.md"
	printf '<!doctype html><title>notes %s</title>\n' "$1" >"$P/web/index.html"
	"${S[@]}" ship --channel stable 2>"$WORK/ship-$1.log" || { cat "$WORK/ship-$1.log"; fail "ship $1"; }
}

step "start zot, a kind cluster pulling from it"
start_zot
start_kind

step "project, ship 1.0.0"
new_project "$PG"
"${S[@]}" ship --channel stable 2>"$WORK/ship-1.0.0.log" || { cat "$WORK/ship-1.0.0.log"; fail "ship 1.0.0"; }

step "k8s install 1.0.0 with the bootstrap script"
installer v1.sh
# The machine interface before any install: describe reads the release the installer carries;
# k8s status finds no release: its refusal one object on standard output, absent.
ctl v1.sh describe --json >"$WORK/describe.json" 2>"$WORK/describe.log" || { cat "$WORK/describe.log"; fail "describe --json"; }
jq -se 'length == 1 and (.[0] | .format == 1 and .name == "notes" and .release == {version: "1.0.0", revision: 1} and (.shapes | index("kubernetes") != null) and
	(.inputs | map(.key) | index("ADMIN_EMAIL") != null) and all(.inputs[]; if .secret then has("default") | not else has("default") end))' "$WORK/describe.json" >/dev/null ||
	{ cat "$WORK/describe.json"; fail "describe --json does not describe 1.0.0 for Kubernetes"; }
rc=0; ctl v1.sh k8s status --json >"$WORK/status0.json" 2>"$WORK/status0.log" || rc=$?
[[ $rc == 3 ]] && grep -qF 'no release notes in notes' "$WORK/status0.log" && jq -se 'length == 1 and .[0].format == 1 and .[0].refused.absent == true and ((.[0].refused.reason + " " + (.[0].refused.fix // "")) | contains("no release notes in notes") and contains("notes-stow k8s install --namespace notes --release notes"))' "$WORK/status0.json" >/dev/null ||
	{ cat "$WORK/status0.log" "$WORK/status0.json"; fail "k8s status --json before the install exited $rc, not 3 with its refusal ('no release notes in notes', absent, the k8s install as its fix) one object on standard output"; }
# The install, under --json: its Result alone on standard output, its text on standard error.
ctl v1.sh k8s install --no-input --set ADMIN_EMAIL=admin@notes.test --timeout 3m --json >"$WORK/install.json" 2>"$WORK/install.log" ||
	{ cat "$WORK/install.log" "$WORK/install.json"; cluster_logs >&2; fail "k8s install --json"; }
result "$WORK/install.json" "done" 1.0.0
grep -q 'runs notes 1.0.0' "$WORK/install.log" || { cat "$WORK/install.log"; fail "the install's text did not go to standard error"; }
runs 1.0.0 1
status_json v1.sh "$WORK/status1.json" 1.0.0 null
[[ $(kubectl get -n "$NS" deploy/notes-web -o jsonpath='{.spec.template.spec.imagePullSecrets[0].name}') == notes-stowage-pull ]] ||
	fail "the pods do not pull with notes-stowage-pull"
[[ $(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}') == stowage ]] ||
	fail "notes-stowage is not labelled as Stowage's"
[[ $(kubectl exec -n "$NS" deploy/notes-web -- printenv ADMIN_EMAIL) == admin@notes.test ]] || fail "the pod's ADMIN_EMAIL is not the input"
[[ $(kubectl exec -n "$NS" deploy/notes-web -- printenv SESSION_SECRET) =~ ^[0-9a-f]{64}$ ]] || fail "the pod's SESSION_SECRET is not a generated hex32"
secret=$(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.data}')
# returned: the ADMIN_PASSWORD the provision hook returned, from its outputs Secret.
returned() { kubectl get -n "$NS" secret notes-stowage-provision -o jsonpath='{.data.ADMIN_PASSWORD}' | base64 -d; }
pw=$(returned)
[[ -n $pw ]] || fail "the provision hook's outputs Secret holds no ADMIN_PASSWORD"
! grep -qF "$pw" "$WORK/install.log" || fail "what the hook returned reached the installer's log"
! kubectl get -n "$NS" job notes-stowage >/dev/null 2>&1 || fail "the provision Job was not deleted"
# somaxconn: the net.core.somaxconn of each web pod not being deleted, its namespace's own, one
# line each, sorted unique (a failed exec fails).
somaxconn() {
	local p
	for p in $(kubectl get pods -n "$NS" -l app.kubernetes.io/name=web -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}} {{end}}{{end}}'); do
		kubectl exec -n "$NS" "$p" -- cat /proc/sys/net/core/somaxconn || fail "kubectl exec $p"
	done | sort -u
}
n=$(somaxconn)
[[ $n =~ ^[0-9]+$ && $n != 8192 ]] || fail "an install without --unsafe-sysctls gave the pods their settings"
ctl v1.sh k8s tune >"$WORK/tune.log" 2>&1 || { cat "$WORK/tune.log"; fail "k8s tune"; }
for k in "${POD_SYSCTLS[@]}"; do grep -qx "    - $k" "$WORK/tune.log" || { cat "$WORK/tune.log"; fail "k8s tune does not list $k for the kubelet"; }; done

step "k8s upgrade --values (2 replicas) --unsafe-sysctls, then k8s upgrade alone keeps them"
printf 'web:\n  replicas: 2\n' >"$OP/replicas.yaml"
ctl v1.sh k8s upgrade --values replicas.yaml --unsafe-sysctls --no-input --timeout 3m >"$WORK/values.log" 2>&1 || { cat "$WORK/values.log"; cluster_logs >&2; fail "k8s upgrade --values --unsafe-sysctls"; }
runs 1.0.0 2
[[ $(somaxconn) == 8192 ]] || { cluster_logs >&2; fail "--unsafe-sysctls did not give the pods their settings"; }
ctl v1.sh k8s upgrade --no-input --timeout 3m >"$WORK/reapply.log" 2>&1 || { cat "$WORK/reapply.log"; fail "k8s upgrade"; }
runs 1.0.0 2
[[ $(somaxconn) == 8192 ]] || { cluster_logs >&2; fail "an upgrade without --unsafe-sysctls dropped the pods' settings"; }
[[ $(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.data}') == "$secret" ]] || fail "an upgrade changed the Secret"
[[ $(returned) == "$pw" ]] || fail "the hook minted another password: what it returned was not handed back"

step "k8s kit create: a Job whose values reach no log, its Secret deleted; k8s upgrade --kit; k8s kit check"
ctl v1.sh k8s kit create second-site --name site-2 --set SITE_ADMIN=second@notes.test -o site.kit >"$WORK/kit.log" 2>&1 || { cat "$WORK/kit.log"; cluster_logs >&2; fail "k8s kit create"; }
sed -n 's/^code  //p' "$WORK/kit.log" >"$OP/site.code"
[[ -s $OP/site.code ]] || { cat "$WORK/kit.log"; fail "k8s kit create printed no code"; }
! grep -qF second@notes.test "$WORK/kit.log" "$OP/site.kit" || fail "a value in the clear"
! kubectl get -n "$NS" secret notes-stowage-kit-second-site >/dev/null 2>&1 || fail "the kit's Secret is still there"
! kubectl get -n "$NS" job notes-stowage-kit-second-site >/dev/null 2>&1 || fail "the kit's Job is still there"
ctl v1.sh k8s upgrade --kit site.kit --kit-code-file site.code --no-input --timeout 3m >"$WORK/kit-up.log" 2>&1 || { cat "$WORK/kit-up.log"; cluster_logs >&2; fail "k8s upgrade --kit"; }
[[ $(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.data.ADMIN_EMAIL}' | base64 -d) == second@notes.test ]] || fail "--kit did not replace the recorded input"
grep -qF 'changing ADMIN_EMAIL: ' "$WORK/kit-up.log" || { cat "$WORK/kit-up.log"; fail "the change was not logged"; }
ctl v1.sh k8s kit check second-site site-2 >"$WORK/kit-check.log" 2>&1 && grep -q 'proven$' "$WORK/kit-check.log" || { cat "$WORK/kit-check.log"; fail "k8s kit check of a site"; }
rc=0; ctl v1.sh k8s kit check second-site site-unknown >"$WORK/kit-check.log" 2>&1 || rc=$?
[[ $rc == 2 ]] || { cat "$WORK/kit-check.log"; fail "k8s kit check of an unknown site exited $rc, not 2"; }
rc=0; ctl v1.sh k8s kit check second-site site-refused >"$WORK/kit-check.log" 2>&1 || rc=$?
[[ $rc == 1 ]] || { cat "$WORK/kit-check.log"; fail "k8s kit check of a refused site exited $rc, not 1"; }
printf 'project: notes\ninputs: {ADMIN_EMAIL: admin@notes.test}\n' >"$OP/back.yaml"
ctl v1.sh k8s upgrade --given back.yaml --no-input --timeout 3m >"$WORK/kit-back.log" 2>&1 || { cat "$WORK/kit-back.log"; fail "k8s upgrade --given back"; }
[[ $(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.data}') == "$secret" ]] || fail "the Secret is not back as it was"

step "between shapes: a kit Docker issued taken by k8s upgrade --kit, one Kubernetes issued taken by a Docker install's --kit"
docker_beside v1.sh
running 1.0.0 1
"${DOCKER_CTL[@]}" kit create second-site --dir "$D" --name site-k8s --set SITE_ADMIN=from-docker@notes.test -o "$OP/docker.kit" >"$WORK/kit-docker.log" 2>&1 || { cat "$WORK/kit-docker.log"; stack_logs >&2; fail "kit create on Docker"; }
kit_code "$WORK/kit-docker.log" "$OP/docker.code"
ctl v1.sh k8s upgrade --kit docker.kit --kit-code-file docker.code --no-input --timeout 3m >"$WORK/kit-docker-up.log" 2>&1 || { cat "$WORK/kit-docker-up.log"; cluster_logs >&2; fail "k8s upgrade --kit of Docker's kit"; }
[[ $(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.data.ADMIN_EMAIL}' | base64 -d) == from-docker@notes.test ]] || fail "Docker's kit did not replace the recorded input"
grep -qF 'kubectl: restarting deployment/notes-web' "$WORK/kit-docker-up.log" || { cat "$WORK/kit-docker-up.log"; fail "what reads Docker's kit's value was not restarted"; }
# Of the pods not being deleted: an old one may still be ending.
for p in $(kubectl get pods -n "$NS" -l app.kubernetes.io/name=web -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}} {{end}}{{end}}'); do
	[[ $(kubectl exec -n "$NS" "$p" -- printenv ADMIN_EMAIL) == from-docker@notes.test ]] || fail "the pod $p does not read Docker's kit's value"
done
ctl v1.sh k8s kit create second-site --name site-docker --set SITE_ADMIN=from-k8s@notes.test -o k8s.kit >"$WORK/kit-k8s.log" 2>&1 || { cat "$WORK/kit-k8s.log"; cluster_logs >&2; fail "k8s kit create for Docker"; }
kit_code "$WORK/kit-k8s.log" "$OP/k8s.code"
"${DOCKER_CTL[@]}" --dir "$D" --no-input --kit "$OP/k8s.kit" --kit-code-file "$OP/k8s.code" >"$WORK/kit-k8s-take.log" 2>&1 || { cat "$WORK/kit-k8s-take.log"; stack_logs >&2; fail "--kit of Kubernetes's kit on Docker"; }
grep -q "^ADMIN_EMAIL='from-k8s@notes.test'\$" "$D/.env" || fail "Kubernetes's kit did not replace the recorded input"
grep -qF 'changing ADMIN_EMAIL: admin@notes.test → from-k8s@notes.test' "$WORK/kit-k8s-take.log" || { cat "$WORK/kit-k8s-take.log"; fail "the change was not logged"; }
running 1.0.0 1
ctl v1.sh k8s upgrade --given back.yaml --no-input --timeout 3m >"$WORK/kit-back.log" 2>&1 || { cat "$WORK/kit-back.log"; fail "k8s upgrade --given back"; }
[[ $(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.data}') == "$secret" ]] || fail "the Secret is not back as it was"
"${DOCKER_CTL[@]}" uninstall --dir "$D" --data --yes >"$WORK/docker-site-gone.log" 2>&1 || { cat "$WORK/docker-site-gone.log"; fail "uninstall of the Docker install beside"; }

step "ship 2.0.0, k8s upgrade --version 2.0.0"
release 2.0.0
[[ $(digest_of notes/notes-web:2.0.0) != "$(digest_of notes/notes-web:1.0.0)" ]] || fail "2.0.0 has 1.0.0's web image"
ctl v1.sh k8s upgrade --version 2.0.0 --no-input --timeout 3m --json >"$WORK/up2.json" 2>"$WORK/up2.log" || { cat "$WORK/up2.log" "$WORK/up2.json"; fail "k8s upgrade --version 2.0.0 --json"; }
result "$WORK/up2.json" "done" 2.0.0
runs 2.0.0 2
installer v2.sh
status_json v2.sh "$WORK/status2.json" 2.0.0 1.0.0
[[ $(somaxconn) == 8192 ]] || { cluster_logs >&2; fail "2.0.0's pods lost the settings the release was given"; }
! compgen -G "$OP/.stowage-next-*" >/dev/null || fail "a fetched installer was left in $OP"
ctl v1.sh k8s upgrade --version 2.0.0 --json >"$WORK/again.json" 2>"$WORK/again.log" || { cat "$WORK/again.log"; fail "upgrade to the running release"; }
grep -q 'already runs notes 2.0.0' "$WORK/again.log" || { cat "$WORK/again.log"; fail "no note that 2.0.0 already runs"; }
result "$WORK/again.json" nothing 2.0.0

step "1.0.0's k8s upgrade is refused (exit 3)"
rc=0; ctl v1.sh k8s upgrade --no-input --json >"$WORK/down.json" 2>"$WORK/down.log" || rc=$?
grep -q "runs notes 2.0.0 (revision 1), after this installer's 1.0.0 (revision 1): .*rollback" "$WORK/down.log" ||
	{ cat "$WORK/down.log"; fail "the refusal does not name rollback"; }
refused "$WORK/down.json" "$rc" "runs notes 2[.]0[.]0 [(]revision 1[)], after this installer's 1[.]0[.]0 [(]revision 1[)]"
result "$WORK/down.json" nothing 2.0.0
runs 2.0.0 2

step "an upgrade that never becomes ready is rolled back by helm"
installer v2.sh
printf 'stowage:\n  images:\n    web:\n      digest: sha256:%064d\n' 0 >"$OP/broken.yaml"
if ctl v2.sh k8s upgrade --values broken.yaml --no-input --timeout 30s >"$WORK/broken.log" 2>&1; then fail "a broken upgrade succeeded"; fi
grep -q 'helm rolled the release back' "$WORK/broken.log" || { cat "$WORK/broken.log"; fail "the failure does not say helm rolled back"; }
runs 2.0.0 2

step "k8s rollback goes back one upgrade, to 1.0.0"
ctl v2.sh k8s rollback --timeout 3m --json >"$WORK/rollback.json" 2>"$WORK/rollback.log" || { cat "$WORK/rollback.log" "$WORK/rollback.json"; fail "k8s rollback --json"; }
result "$WORK/rollback.json" "done" 1.0.0
grep -q 'release notes in notes runs notes 1.0.0 (revision 1)' "$WORK/rollback.log" || { cat "$WORK/rollback.log"; fail "the rollback does not say 1.0.0 runs"; }
runs 1.0.0 2
# Nothing to go back to: every revision helm keeps before the one rolled back to ran the same
# values (the first install's, with one replica, is past helm's history limit).
status_json v1.sh "$WORK/status3.json" 1.0.0 null

step "k8s status"
ctl v2.sh k8s status >"$WORK/status.log" 2>&1 || { cat "$WORK/status.log"; fail "k8s status"; }
grep -q "^notes 1.0.0 (revision 1, stable)$" "$WORK/status.log" && grep -q "^registry  $REG/notes$" "$WORK/status.log" && grep -q 'STATUS: deployed' "$WORK/status.log" ||
	{ cat "$WORK/status.log"; fail "k8s status"; }

step "audit and sbom export from the operator's machine: the cluster's release, by its version alone"
grype_db
scanner_fresh "$REG/notes"
ctl v1.sh audit --version 1.0.0 --db "$GRYPE_DB" --json >"$WORK/audit.json" 2>"$WORK/audit.log" || { cat "$WORK/audit.log" "$WORK/audit.json"; fail "audit of 1.0.0"; }
[[ $(jq -r .version "$WORK/audit.json") == 1.0.0 && $(jq '[.subjects[] | select(.error)] | length' "$WORK/audit.json") == 0 && $(jq '[.subjects[] | select(.name == "installer")] | length' "$WORK/audit.json") == 1 ]] ||
	{ cat "$WORK/audit.json"; fail "not every artifact of 1.0.0 was audited"; }
ctl v1.sh sbom export --version 1.0.0 "$WORK/sbom" >"$WORK/sbom.txt" 2>&1 || { cat "$WORK/sbom.txt"; fail "sbom export"; }
jq -e '(.artifacts | length) > 0 and all(.artifacts[]; .error == null and .file != null)' "$WORK/sbom/index.json" >/dev/null &&
	jq -e '.spdxVersion' "$WORK/sbom/installer.spdx.json" >/dev/null || { cat "$WORK/sbom/index.json"; fail "the exported SBOMs"; }

step "k8s reset-db: uninstalled, its claim deleted, installed again with the same values"
kubectl exec -n "$NS" deploy/notes-web -- touch /data/marker || fail "no marker written in the uploads claim"
claim=$(kubectl get -n "$NS" pvc notes-uploads -o jsonpath='{.metadata.uid}')
if ctl v1.sh k8s reset-db </dev/null >"$WORK/reset0.log" 2>&1; then fail "k8s reset-db wiped without --yes"; fi
grep -q 'give --yes' "$WORK/reset0.log" || { cat "$WORK/reset0.log"; fail "the refusal does not name --yes"; }
ctl v1.sh k8s reset-db --yes --timeout 3m >"$WORK/reset.log" 2>&1 || { cat "$WORK/reset.log"; cluster_logs >&2; fail "k8s reset-db"; }
grep -q 'runs notes 1.0.0 (revision 1), as first installed' "$WORK/reset.log" || { cat "$WORK/reset.log"; fail "reset-db does not say 1.0.0 runs"; }
runs 1.0.0 2
[[ $(kubectl get -n "$NS" pvc notes-uploads -o jsonpath='{.metadata.uid}') != "$claim" ]] || fail "the uploads claim was not deleted"
if kubectl exec -n "$NS" deploy/notes-web -- test -e /data/marker; then fail "the uploads claim kept its data"; fi
[[ $(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.data}') == "$secret" ]] || fail "reset-db changed the Secret"
[[ -n $(returned) && $(returned) != "$pw" ]] || fail "reset-db did not forget what the hook returned"

step "k8s uninstall: refused without --data (helm deletes the chart's claim), then removes all but the namespace"
rc=0; ctl v1.sh k8s uninstall --yes --json >"$WORK/uninstall0.json" 2>"$WORK/uninstall0.log" || rc=$?
grep -q 'notes-uploads, which `helm uninstall` deletes with it' "$WORK/uninstall0.log" || { cat "$WORK/uninstall0.log"; fail "the refusal does not name the claim"; }
refused "$WORK/uninstall0.json" "$rc" 'notes-uploads, which `helm uninstall` deletes with it'
result "$WORK/uninstall0.json" nothing 1.0.0
helm status -n "$NS" notes >/dev/null 2>&1 || fail "the refused uninstall removed the release"
ctl v1.sh k8s uninstall --data --yes --timeout 3m --json >"$WORK/uninstall.json" 2>"$WORK/uninstall.log" || { cat "$WORK/uninstall.log" "$WORK/uninstall.json"; cluster_logs >&2; fail "k8s uninstall --data --json"; }
result "$WORK/uninstall.json" "done"
grep -q 'kept: the namespace notes' "$WORK/uninstall.log" || { cat "$WORK/uninstall.log"; fail "uninstall does not say what it kept"; }
! helm status -n "$NS" notes >/dev/null 2>&1 || fail "the release is still installed"
for o in secret/notes-stowage secret/notes-stowage-provision serviceaccount/notes-stowage-provision role/notes-stowage-provision rolebinding/notes-stowage-provision pvc/notes-uploads; do
	[[ -z $(kubectl get -n "$NS" "$o" --ignore-not-found -o name) ]] || fail "$o is left"
done
kubectl get namespace "$NS" >/dev/null || fail "the namespace was deleted"

# baseline <n>: the project's schema compat becomes sql at schema baseline n.
baseline() {
	python3 - "$P/stowage.yaml" "$1" <<'PY'
import re, sys
path, n = sys.argv[1:]
s = open(path).read()
s, k = re.subn(r"  schema_compat:\n(    .*\n)+", f"  schema_compat:\n    provider: sql\n    current: \"SELECT 1\"\n    baseline: {n}\n", s, count=1)
assert k == 1, "schema_compat"
open(path, "w").write(s)
PY
}
step "across Stowage versions: the previous Stowage's release, at baseline 1, crosses to this one's at baseline 2"
prev=$(prev_stowage)
S_NOW=("${S[@]}")
S=("$prev" "${S[@]:1}")
# That Stowage reads its spec strictly: it ships the example as its own tag has it, which holds no
# key newer than it.
example_at "$(basename "$(dirname "$prev")")"
"${S[@]}" lock --update >/dev/null # a project publishes with the stowage its lock pins
baseline 1
release 3.0.0
installer v3.sh
ctl v3.sh k8s install --no-input --set ADMIN_EMAIL=admin@notes.test --timeout 3m >"$WORK/install3.log" 2>&1 ||
	{ cat "$WORK/install3.log"; cluster_logs >&2; fail "k8s install of the previous Stowage's 3.0.0"; }
runs 3.0.0 1
S=("${S_NOW[@]}")
example_at
"${S[@]}" lock --update >/dev/null
baseline 2
release 4.0.0
installer v4.sh
if ctl v4.sh k8s upgrade --no-input --timeout 3m >"$WORK/up4.log" 2>&1; then fail "an upgrade crossed a schema baseline"; fi
grep -q 'notes 4.0.0 starts schema baseline 2, and release notes in notes runs notes 3.0.0, at baseline 1' "$WORK/up4.log" && grep -q -- '--new-baseline` wipes it' "$WORK/up4.log" ||
	{ cat "$WORK/up4.log"; fail "the upgrade's refusal does not tell the previous Stowage's baseline"; }
ctl v4.sh k8s reset-db --new-baseline --yes --timeout 3m >"$WORK/cross.log" 2>&1 || { cat "$WORK/cross.log"; cluster_logs >&2; fail "k8s reset-db --new-baseline"; }
runs 4.0.0 1
ctl v4.sh k8s kit create second-site --name site-after --set SITE_ADMIN=after@notes.test -o after.kit >"$WORK/kit-after.log" 2>&1 || { cat "$WORK/kit-after.log"; cluster_logs >&2; fail "k8s kit create after the crossing"; }
kit_code "$WORK/kit-after.log" "$OP/after.code"
ctl v4.sh k8s upgrade --kit after.kit --kit-code-file after.code --no-input --timeout 3m >"$WORK/kit-after-up.log" 2>&1 || { cat "$WORK/kit-after-up.log"; cluster_logs >&2; fail "k8s upgrade --kit after the crossing"; }
[[ $(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.data.ADMIN_EMAIL}' | base64 -d) == after@notes.test ]] || fail "the kit did not replace the recorded input after the crossing"
ctl v4.sh k8s uninstall --data --yes --timeout 3m >"$WORK/uninstall4.log" 2>&1 || { cat "$WORK/uninstall4.log"; fail "k8s uninstall --data after the crossing"; }

printf '\nPASS\n'
