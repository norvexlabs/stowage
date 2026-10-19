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
# keeps them, and hands the hook what it returned → ship 2.0.0 (a new web image) → `k8s upgrade --version 2.0.0` → the
# same again is a note → 1.0.0's `k8s upgrade` is refused → an upgrade that never becomes ready is
# rolled back by helm → `k8s rollback` goes back one upgrade, to 1.0.0 → `k8s status` → `k8s
# reset-db` deletes the release's claim, forgets what the hook returned and installs it again with
# the same values and Secret.
set -euo pipefail

# The amd64 image of the example's postgres: mirroring copies every platform of an index.
PG=postgres:17-alpine@sha256:aa90e97ee862e558111d34cfb8b2c4bec768c2b039fb791341686928560263b3
. "$(dirname "$0")/lib.sh"

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
ctl v1.sh k8s install --no-input --set ADMIN_EMAIL=admin@notes.test --timeout 3m >"$WORK/install.log" 2>&1 ||
	{ cat "$WORK/install.log"; cluster_logs >&2; fail "k8s install"; }
runs 1.0.0 1
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

step "k8s upgrade --values (2 replicas), then k8s upgrade alone keeps them"
printf 'web:\n  replicas: 2\n' >"$OP/replicas.yaml"
ctl v1.sh k8s upgrade --values replicas.yaml --no-input --timeout 3m >"$WORK/values.log" 2>&1 || { cat "$WORK/values.log"; fail "k8s upgrade --values"; }
runs 1.0.0 2
ctl v1.sh k8s upgrade --no-input --timeout 3m >"$WORK/reapply.log" 2>&1 || { cat "$WORK/reapply.log"; fail "k8s upgrade"; }
runs 1.0.0 2
[[ $(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.data}') == "$secret" ]] || fail "an upgrade changed the Secret"
[[ $(returned) == "$pw" ]] || fail "the hook minted another password: what it returned was not handed back"

step "ship 2.0.0, k8s upgrade --version 2.0.0"
release 2.0.0
[[ $(digest_of notes/notes-web:2.0.0) != "$(digest_of notes/notes-web:1.0.0)" ]] || fail "2.0.0 has 1.0.0's web image"
ctl v1.sh k8s upgrade --version 2.0.0 --no-input --timeout 3m >"$WORK/up2.log" 2>&1 || { cat "$WORK/up2.log"; fail "k8s upgrade --version 2.0.0"; }
runs 2.0.0 2
! compgen -G "$OP/.stowage-next-*" >/dev/null || fail "a fetched installer was left in $OP"
ctl v1.sh k8s upgrade --version 2.0.0 >"$WORK/again.log" 2>&1 || { cat "$WORK/again.log"; fail "upgrade to the running release"; }
grep -q 'already runs notes 2.0.0' "$WORK/again.log" || { cat "$WORK/again.log"; fail "no note that 2.0.0 already runs"; }

step "1.0.0's k8s upgrade is refused"
if ctl v1.sh k8s upgrade --no-input >"$WORK/down.log" 2>&1; then fail "1.0.0's installer upgraded 2.0.0"; fi
grep -q "runs notes 2.0.0 (revision 1), after this installer's 1.0.0 (revision 1): .*rollback" "$WORK/down.log" ||
	{ cat "$WORK/down.log"; fail "the refusal does not name rollback"; }
runs 2.0.0 2

step "an upgrade that never becomes ready is rolled back by helm"
installer v2.sh
printf 'stowage:\n  images:\n    web:\n      digest: sha256:%064d\n' 0 >"$OP/broken.yaml"
if ctl v2.sh k8s upgrade --values broken.yaml --no-input --timeout 30s >"$WORK/broken.log" 2>&1; then fail "a broken upgrade succeeded"; fi
grep -q 'helm rolled the release back' "$WORK/broken.log" || { cat "$WORK/broken.log"; fail "the failure does not say helm rolled back"; }
runs 2.0.0 2

step "k8s rollback goes back one upgrade, to 1.0.0"
ctl v2.sh k8s rollback --timeout 3m >"$WORK/rollback.log" 2>&1 || { cat "$WORK/rollback.log"; fail "k8s rollback"; }
grep -q 'release notes in notes runs notes 1.0.0 (revision 1)' "$WORK/rollback.log" || { cat "$WORK/rollback.log"; fail "the rollback does not say 1.0.0 runs"; }
runs 1.0.0 2

step "k8s status"
ctl v2.sh k8s status >"$WORK/status.log" 2>&1 || { cat "$WORK/status.log"; fail "k8s status"; }
grep -q "^notes 1.0.0 (revision 1, stable)$" "$WORK/status.log" && grep -q "^registry  $REG/notes$" "$WORK/status.log" && grep -q 'STATUS: deployed' "$WORK/status.log" ||
	{ cat "$WORK/status.log"; fail "k8s status"; }

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

printf '\nPASS\n'
