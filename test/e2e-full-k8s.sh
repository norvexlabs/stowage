#!/usr/bin/env bash
# End-to-end test of examples/full (atlas) on the Kubernetes shape: the real stowage and <short>-stow
# binaries, a real registry (zot) and a kind cluster pulling from it (as in e2e-k8s.sh, whose
# cluster setup lib.sh shares). Needs a linux/amd64 host with Docker and network access.
#
#   make e2e-full-k8s      (builds ./bin/stowage first)
#
# The example as it stands — CalVer releases, token pushes, signed — with its chart: an api that
# migrates at start under a lock (two replicas), ingest, the console, the search extension's
# input, the provision hook (a Job the installer runs once the release is settled, its outputs
# kept in a Secret, never in a log) and the database external or embedded. Walks: ship this
# month's .1 → `k8s install` against an external Postgres (its two URLs and the administrator's
# password are secret inputs, given in the operator's environment; the search extension's URL
# given at the install), the hook's API key recorded, the endpoint printed → `k8s upgrade` of the
# same release, the key handed back to the hook → the app's users revoke
# the key, ship .2 (a migration) → `k8s upgrade --version`: the release runs .2, its hook fails
# naming `k8s provision --set`, which mints another key; `k8s provision` again keeps it → its
# record lost, `k8s provision --set ADMIN_API_KEY=<key>` gives the key the hook never returned (the
# spec declares it in hooks.provision.returns) and no other is minted → the data kept → `k8s rollback` (the database read in a pod of the release: no barrier since the schema the upgrade recorded, and the previous api reads the newer schema) → a second release in another
# namespace with the embedded Postgres → `k8s reset-db` deletes its database's claim, forgets what
# the hook returned, and the hook registers the administrator again → `k8s secrets rotate` takes
# the internal secret through its three passes, the release restarted at each, each value
# accepted while it must be → `k8s uninstall --yes` of the embedded release keeps its database's
# claim and its Secrets, and `k8s install` takes them back, the events kept.
set -euo pipefail

# The amd64 image of the example's postgres: mirroring copies every platform of an index.
PG=postgres:17-alpine@sha256:aa90e97ee862e558111d34cfb8b2c4bec768c2b039fb791341686928560263b3
. "$(dirname "$0")/lib.sh"

k8s_tools
NS=atlas

# api_get <path>: the body the api in NS answers on <path>, read inside its pod.
api_get() {
	kubectl exec -n "$NS" deploy/atlas-api -c api -- python3 -c \
		'import sys, urllib.request; print(urllib.request.urlopen(sys.argv[1], timeout=10).read().decode())' "$1"
}
# status <field>…: those fields of one answer of the api's /api/v1/status, joined by /.
status() {
	api_get http://127.0.0.1:9090/api/v1/status |
		python3 -c 'import json,sys; s=json.load(sys.stdin); print("/".join(str(s[k]) for k in sys.argv[1:]))' "$@"
}
# atlas_runs <version> <schema>: the release in NS runs that version's images, every pod ready
# (the api's two replicas), and its api answers as that release on that migration.
atlas_runs() {
	local d want img got
	for d in api ingest ui; do
		kubectl rollout status -n "$NS" "deploy/atlas-$d" --timeout 180s >/dev/null || { cluster_logs >&2; fail "atlas-$d is not ready"; }
	done
	want=$REG/atlas/atlas-api@$(digest_of "atlas/atlas-api:v$1")
	img=$(kubectl get -n "$NS" deploy/atlas-api -o jsonpath='{.spec.template.spec.containers[0].image}')
	[[ $img == "$want" ]] || fail "atlas-api runs $img, want $want ($1)"
	got=$(kubectl get -n "$NS" deploy/atlas-api -o jsonpath='{.status.readyReplicas}')
	[[ $got == 2 ]] || { cluster_logs >&2; fail "$got api replicas ready, want 2"; }
	got=$(status version schema) || { cluster_logs >&2; fail "the api does not answer"; }
	[[ $got == "$1/$2" ]] || fail "the api answers $got (version/migration), want $1/$2"
	got=$(helm list -n "$NS" --filter '^atlas$' --output json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["app_version"])')
	[[ $got == "$1" ]] || fail "helm lists $got, want $1"
}
# ingest_event: one event posted to the ingest service in NS, inside its pod.
ingest_event() {
	kubectl exec -n "$NS" deploy/atlas-ingest -- python3 -c 'import urllib.request
r = urllib.request.Request("http://127.0.0.1:8080/events", data=b"{\"kind\": \"probe\"}", method="POST", headers={"Content-Type": "application/json"})
print(urllib.request.urlopen(r, timeout=10).status)' | grep -qx 201 || fail "the ingest service did not take the event"
}
# admin_hash <psql args…>: the administrator's password hash, read with psql in the database's pod.
admin_hash() { "$@" psql -U atlas -d atlas -tAc "SELECT password_sha256 FROM users WHERE email = 'admin@atlas.test'"; }
sha256() { printf '%s' "$1" | sha256sum | cut -d' ' -f1; }
# keys_of <kubectl exec target>: the names of the target's environment, sorted (the exec must work).
keys_of() { local e; e=$(kubectl exec -n "$NS" "$1" -- env) || fail "kubectl exec $1 env"; cut -d= -f1 <<<"$e" | sort; }
# k8s_get <secret> <key>: the value a Secret of the release in NS holds.
k8s_get() { kubectl get secret -n "$NS" "$1" -o "jsonpath={.data.$2}" | base64 -d; }
# key_status <key>: the status the api in NS answers its key check with, inside its pod.
key_status() {
	kubectl exec -n "$NS" deploy/atlas-api -c api -- python3 -c 'import sys, urllib.request, urllib.error
r = urllib.request.Request("http://127.0.0.1:9090/api/v1/key", headers={"Authorization": "Bearer " + sys.argv[1]})
try:
    print(urllib.request.urlopen(r, timeout=10).status)
except urllib.error.HTTPError as e:
    print(e.code)' "$1"
}
# hook_ran <log> <key>: the provision Job ran and is gone, its stderr in the installer's log, and the
# key it returned in no log (its stdout went to the pod's volume, then the outputs Secret).
hook_ran() {
	grep -q 'the Job atlas-stowage runs the hook' "$1" && grep -q 'registering admin@atlas.test' "$1" || { cat "$1"; fail "the hook's Job did not run, or its stderr is not in the log"; }
	! kubectl get job -n "$NS" atlas-stowage >/dev/null 2>&1 || fail "the provision Job was not deleted"
	! grep -qF "$2" "$1" || fail "what the hook returned reached a log"
}
# lacks <target> <key>…: none of the keys is in the target's environment.
lacks() {
	local t=$1 k ks
	shift
	ks=$(keys_of "$t")
	for k in "$@"; do grep -qx "$k" <<<"$ks" && fail "$t sees $k"; done
	return 0
}

step "start zot, a kind cluster pulling from it"
start_zot
start_kind

step "project, ship this month's first release"
new_full "$PG"
V1=$CALVER.1 V2=$CALVER.2
"${S[@]}" ship --channel stable 2>"$WORK/ship-$V1.log" || { cat "$WORK/ship-$V1.log"; fail "ship $V1"; }

step "an external Postgres in the cluster (namespace atlas-db)"
kubectl create namespace atlas-db >/dev/null
login_pull
kubectl create secret generic db-pull -n atlas-db --type=kubernetes.io/dockerconfigjson \
	--from-file=.dockerconfigjson="$DOCKER_CONFIG/config.json" >/dev/null
unset DOCKER_CONFIG
DB_PW=$(token) APP_PW=$(token) ADMIN_PW=$(token)
kubectl apply -n atlas-db -f - >/dev/null <<YAML
apiVersion: v1
kind: Secret
metadata: { name: db }
stringData: { POSTGRES_PASSWORD: "$DB_PW" }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: db }
spec:
  selector: { matchLabels: { app: db } }
  template:
    metadata: { labels: { app: db } }
    spec:
      imagePullSecrets: [{ name: db-pull }]
      containers:
        - name: postgres
          image: $REG/atlas/atlas-postgres@$(digest_of "atlas/atlas-postgres:v$V1")
          env:
            - { name: POSTGRES_USER, value: atlas }
            - { name: POSTGRES_DB, value: atlas }
            - name: POSTGRES_PASSWORD
              valueFrom: { secretKeyRef: { name: db, key: POSTGRES_PASSWORD } }
          readinessProbe:
            exec: { command: [pg_isready, -U, atlas, -d, atlas] }
            periodSeconds: 2
---
apiVersion: v1
kind: Service
metadata: { name: db }
spec:
  selector: { app: db }
  ports: [{ port: 5432 }]
YAML
kubectl rollout status -n atlas-db deploy/db --timeout 180s >/dev/null || { kubectl describe pods -n atlas-db | tail -30; fail "the external Postgres is not ready"; }
DB_URL=postgres://atlas:$DB_PW@db.atlas-db.svc:5432/atlas
APP_URL=postgres://atlas_app:$APP_PW@db.atlas-db.svc:5432/atlas

step "k8s install $V1 with the bootstrap script, external database"
installer v1.sh
printf 'postgres:\n  mode: external\n' >"$OP/external.yaml"
# The secret inputs come from the operator's environment, never a command line.
(cd "$OP" && "${OPERATOR[@]}" "DATABASE_URL=$DB_URL" "APP_DATABASE_URL=$APP_URL" "ADMIN_PASSWORD=$ADMIN_PW" \
	sh v1.sh k8s install --no-input --values external.yaml --set ADMIN_EMAIL=admin@atlas.test --set ORG_NAME=Atlas \
	--set SEARCH_URL=http://atlas-api:9090 --unsafe-sysctls --timeout 5m) \
	>"$WORK/install.log" 2>&1 || { cat "$WORK/install.log"; cluster_logs >&2; fail "k8s install"; }
atlas_runs "$V1" 1
# --unsafe-sysctls: every pod of the chart is given the settings, which reach the api's namespace.
for d in api echo ingest ui; do
	[[ $(kubectl get -n "$NS" deploy/atlas-$d -o jsonpath='{.spec.template.spec.securityContext.sysctls[*].name}') == "${POD_SYSCTLS[*]}" ]] ||
		fail "atlas-$d's pods are not given ${POD_SYSCTLS[*]}"
done
[[ $(kubectl exec -n "$NS" deploy/atlas-api -c api -- python3 -c 'print(*(open("/proc/sys/net/" + k).read().strip() for k in ("core/somaxconn", "ipv4/tcp_notsent_lowat")))') == "8192 131072" ]] ||
	fail "the api's namespace does not hold the settings --unsafe-sysctls gives"
[[ $(kubectl exec -n "$NS" deploy/atlas-api -- printenv DATABASE_URL) == "$DB_URL" ]] || fail "the api's DATABASE_URL is not the input"
# Each pod reads only its keys of the Secret: ingest has the service token, not the database's;
# the api not the administrator's password; the console none.
keys_of deploy/atlas-ingest | grep -qx INTERNAL_SERVICE_SECRET || fail "ingest has no INTERNAL_SERVICE_SECRET"
lacks deploy/atlas-ingest DATABASE_URL APP_DATABASE_URL ENCRYPTION_KEY ADMIN_PASSWORD
lacks deploy/atlas-api ADMIN_PASSWORD ADMIN_EMAIL
lacks deploy/atlas-ui INTERNAL_SERVICE_SECRET DATABASE_URL ENCRYPTION_KEY
[[ -z $(helm get hooks atlas -n "$NS") ]] || fail "the chart carries a helm hook"
key=$(k8s_get atlas-stowage-provision ADMIN_API_KEY)
[[ -n $key && $(k8s_get atlas-stowage-provision ADMIN_LOGIN) == admin@atlas.test ]] || fail "the outputs Secret holds no API key or login"
hook_ran "$WORK/install.log" "$key"
# The database was up before the api: a replica that tried again lost a race with the other.
logs=$(kubectl logs -n "$NS" -l app.kubernetes.io/name=api --tail=-1 --prefix)
[[ $(grep -c 'migrated to 1:' <<<"$logs") == 1 ]] && ! grep -q 'trying again' <<<"$logs" ||
	{ printf '%s\n' "$logs" >&2; fail "the two api replicas did not migrate once, each on its first try"; }
[[ $(status organisation admins) == Atlas/1 ]] || fail "the api did not record ORG_NAME, or the provision hook no administrator"
[[ $(key_status "$key") == 200 ]] || fail "the api does not know the hook's key"
[[ $(admin_hash kubectl exec -n atlas-db deploy/db --) == "$(sha256 "$ADMIN_PW")" ]] || fail "the administrator's password is not the input"
ingest_event
[[ $(status events) == 1 ]] || fail "the ingested event did not reach the api"
[[ $(api_get http://atlas-ui:8080/) == *'<title>Atlas'* ]] || fail "the console does not serve the ui"
# The api's own liveness stands in for a search service: what is proven is that the input reaches it.
[[ $(status search) == True ]] || fail "the api does not reach SEARCH_URL, given at the install"
grep -qF "endpoint  http://atlas-api.$NS.svc:9090" "$WORK/install.log" || { cat "$WORK/install.log"; fail "k8s install did not print its endpoint"; }
! grep -qF "tip: the extension atlas-search" "$WORK/install.log" || { cat "$WORK/install.log"; fail "a tip for the search extension, whose URL was given"; }
# The chart's Services of the L4 routes (stowage.l4), to the echo pods.
[[ $(kubectl get -n "$NS" svc atlas-l4-echo atlas-l4-echo-udp -o jsonpath='{range .items[*]}{.spec.ports[0].protocol}/{.spec.ports[0].port} {end}') == "TCP/17007 UDP/17007 " ]] ||
	fail "the chart did not render a Service per L4 route from stowage.l4"
[[ -n $(kubectl get -n "$NS" endpoints atlas-l4-echo -o jsonpath='{.subsets[0].addresses[0].ip}') ]] || fail "the L4 route's Service selects no echo pod"

step "k8s upgrade of the same release: the key handed back to the hook"
ctl v1.sh k8s upgrade --no-input --timeout 5m >"$WORK/search.log" 2>&1 || { cat "$WORK/search.log"; fail "k8s upgrade"; }
atlas_runs "$V1" 1
[[ $(status search admins) == True/1 ]] || fail "the upgrade lost SEARCH_URL, or the hook registered a second administrator"
hook_ran "$WORK/search.log" "$key"
[[ $(k8s_get atlas-stowage-provision ADMIN_API_KEY) == "$key" ]] || fail "the upgrade's hook minted another key: what it returned was not handed back"

step "the app's users revoke the key; ship $V2 (a migration), k8s upgrade --version $V2: the hook fails, the release runs $V2"
kubectl exec -n atlas-db deploy/db -- psql -U atlas -d atlas -qc 'DELETE FROM api_keys' || fail "revoking the key"
full_release 2 002_event_source.sql "-- 002: where an event came from
ALTER TABLE events ADD COLUMN source text;"
if ctl v1.sh k8s upgrade --version "$V2" --no-input --timeout 5m >"$WORK/up2.log" 2>&1; then cat "$WORK/up2.log"; fail "k8s upgrade --version $V2 passed with the hook failing"; fi
grep -q 'provisioning did not finish' "$WORK/up2.log" && grep -qF 'provision --set ADMIN_API_KEY=` forgets it' "$WORK/up2.log" &&
	grep -qF '`at-stow k8s provision --namespace atlas --release atlas` runs it again, and with `--set KEY=VALUE` sets a value it returned or declares it returns (' "$WORK/up2.log" ||
	{ cat "$WORK/up2.log"; fail "the hook's failure does not name k8s provision --set"; }
! grep -q 'helm rolled the release back' "$WORK/up2.log" || fail "the hook's failure rolled the upgrade back"
atlas_runs "$V2" 2
[[ $(k8s_get atlas-stowage-provision ADMIN_API_KEY) == "$key" ]] || fail "a failed hook run changed the recorded key"

step "k8s provision --set ADMIN_API_KEY= mints another key; k8s provision again keeps it"
installer v2.sh
ctl v2.sh k8s provision --set ADMIN_API_KEY= >"$WORK/prov.log" 2>&1 || { cat "$WORK/prov.log"; cluster_logs >&2; fail "k8s provision --set"; }
new=$(k8s_get atlas-stowage-provision ADMIN_API_KEY)
[[ -n $new && $new != "$key" && $(key_status "$new") == 200 ]] || fail "k8s provision --set ADMIN_API_KEY= did not mint and register another key"
hook_ran "$WORK/prov.log" "$new"
ctl v2.sh k8s provision >"$WORK/prov2.log" 2>&1 || { cat "$WORK/prov2.log"; fail "k8s provision"; }
hook_ran "$WORK/prov2.log" "$new"
[[ $(k8s_get atlas-stowage-provision ADMIN_API_KEY) == "$new" ]] || fail "k8s provision minted again: what the hook returned was not handed back"
[[ $(status events admins search) == 1/1/True ]] || fail "the upgrade lost data or the search value"

step "a key the hook never returned (its record lost): k8s provision --set gives it, as hooks.provision.returns declares it"
db_keys() { kubectl exec -n atlas-db deploy/db -- psql -U atlas -d atlas -tAc 'SELECT count(*) FROM api_keys'; }
keys=$(db_keys)
kubectl patch secret atlas-stowage-provision -n "$NS" --type=json -p '[{"op": "remove", "path": "/data/ADMIN_API_KEY"}]' >/dev/null ||
	fail "dropping ADMIN_API_KEY from the outputs Secret"
[[ -z $(k8s_get atlas-stowage-provision ADMIN_API_KEY) ]] || fail "the outputs Secret still holds ADMIN_API_KEY"
if ctl v2.sh k8s provision --set NOPE=x >"$WORK/prov-nope.log" 2>&1; then cat "$WORK/prov-nope.log"; fail "k8s provision --set of an undeclared key passed"; fi
grep -qF 'NOPE is no value the provision hook returned or declares it returns (ADMIN_API_KEY, ADMIN_LOGIN)' "$WORK/prov-nope.log" ||
	{ cat "$WORK/prov-nope.log"; fail "the refusal does not name the declared key"; }
ctl v2.sh k8s provision --set "ADMIN_API_KEY=$new" >"$WORK/prov-given.log" 2>&1 || { cat "$WORK/prov-given.log"; cluster_logs >&2; fail "k8s provision --set of a declared key the hook never returned"; }
grep -q 'recorded ADMIN_API_KEY in the Secret atlas-stowage-provision' "$WORK/prov-given.log" || { cat "$WORK/prov-given.log"; fail "k8s provision --set did not record the declared key"; }
hook_ran "$WORK/prov-given.log" "$new"
[[ $(k8s_get atlas-stowage-provision ADMIN_API_KEY) == "$new" && $(db_keys) == "$keys" ]] || fail "the hook did not keep the key it was given, or minted another"

step "k8s rollback to $V1 (its api reads the newer schema)"
ctl v1.sh k8s rollback --timeout 5m >"$WORK/rollback.log" 2>&1 || { cat "$WORK/rollback.log"; fail "k8s rollback"; }
grep -q "release atlas in atlas runs atlas $V1" "$WORK/rollback.log" || { cat "$WORK/rollback.log"; fail "the rollback does not say $V1 runs"; }
grep -q "the database is at 2; atlas $V1 (revision 1) reads past the newer migrations" "$WORK/rollback.log" ||
	{ cat "$WORK/rollback.log"; fail "the rollback did not check the schema in a pod"; }
atlas_runs "$V1" 2
[[ $(status events admins) == 1/1 ]] || fail "the rollback lost data"

step "k8s install $V1 in atlas-eval, embedded database"
NS=atlas-eval
(cd "$OP" && "${OPERATOR[@]}" "ADMIN_PASSWORD=$ADMIN_PW" sh v1.sh k8s install --namespace "$NS" --no-input \
	--set ADMIN_EMAIL=admin@atlas.test --set ORG_NAME=Eval --timeout 5m) >"$WORK/eval.log" 2>&1 ||
	{ cat "$WORK/eval.log"; cluster_logs >&2; fail "k8s install (embedded)"; }
atlas_runs "$V1" 1
[[ $(status organisation admins) == Eval/1 ]] || fail "the embedded release's api did not record its inputs"
evalkey=$(k8s_get atlas-stowage-provision ADMIN_API_KEY)
[[ -n $evalkey && $evalkey != "$new" ]] || fail "the second release's hook returned no key of its own"
[[ $(admin_hash kubectl exec -n "$NS" atlas-postgres-0 --) == "$(sha256 "$ADMIN_PW")" ]] || fail "the embedded administrator's password is not the input"
lacks atlas-postgres-0 INTERNAL_SERVICE_SECRET APP_DB_PASSWORD ADMIN_PASSWORD
lacks deploy/atlas-ingest POSTGRES_PASSWORD APP_DB_PASSWORD
ingest_event
[[ $(status events) == 1 ]] || fail "the embedded release lost the ingested event"

step "k8s reset-db: the embedded database's claim deleted, the administrator registered again"
claim=$(kubectl get -n "$NS" pvc data-atlas-postgres-0 -o jsonpath='{.metadata.uid}')
ctl v1.sh k8s reset-db --namespace "$NS" --yes --timeout 5m >"$WORK/reset.log" 2>&1 || { cat "$WORK/reset.log"; cluster_logs >&2; fail "k8s reset-db"; }
atlas_runs "$V1" 1
[[ $(kubectl get -n "$NS" pvc data-atlas-postgres-0 -o jsonpath='{.metadata.uid}') != "$claim" ]] || fail "the database's claim was not deleted"
[[ $(status events organisation admins) == 0/Eval/1 ]] || fail "reset-db kept the events, or the hook did not register the administrator again"
again=$(k8s_get atlas-stowage-provision ADMIN_API_KEY)
[[ -n $again && $again != "$evalkey" && $(key_status "$again") == 200 ]] || fail "reset-db did not forget what the hook returned"
NS=atlas
[[ $(status events) == 1 ]] || fail "reset-db of atlas-eval touched the external release's data"

step "k8s secrets rotate INTERNAL_SERVICE_SECRET: --stage, the swap, --finish, each restarting the release"
# k8s_secret <key>: the value the release's Secret holds. api_post <token>: the HTTP status the api
# answers an event posted with that bearer token, inside its pod.
k8s_secret() { k8s_get atlas-stowage "$1"; }
api_post() {
	kubectl exec -n "$NS" deploy/atlas-api -c api -- python3 -c 'import sys, urllib.request, urllib.error
r = urllib.request.Request("http://127.0.0.1:9090/api/v1/events", data=b"{}", method="POST", headers={"Content-Type": "application/json", "Authorization": "Bearer " + sys.argv[1]})
try:
    print(urllib.request.urlopen(r, timeout=10).status)
except urllib.error.HTTPError as e:
    print(e.code)' "$1"
}
rotate() { ctl v1.sh k8s secrets rotate INTERNAL_SERVICE_SECRET "$@" --timeout 5m >"$WORK/rotate.log" 2>&1 || { cat "$WORK/rotate.log"; cluster_logs >&2; fail "k8s secrets rotate $*"; }; }
old=$(k8s_secret INTERNAL_SERVICE_SECRET)
rotate --stage
grep -q 'restarting deployment/atlas-api' "$WORK/rotate.log" || { cat "$WORK/rotate.log"; fail "--stage did not restart the api"; }
next=$(k8s_secret INTERNAL_SERVICE_SECRET_NEXT)
[[ -n $next && $(k8s_secret INTERNAL_SERVICE_SECRET) == "$old" ]] || fail "--stage did not record a new value beside the old one"
[[ $(api_post "$next") == 201 ]] || fail "the api refuses the staged value"
ingest_event
rotate
[[ $(k8s_secret INTERNAL_SERVICE_SECRET) == "$next" && $(k8s_secret INTERNAL_SERVICE_SECRET_PREVIOUS) == "$old" ]] || fail "the swap did not make the new value current and keep the old one"
[[ $(api_post "$old") == 201 ]] || fail "the api refuses the previous value before --finish"
ingest_event
rotate --finish
[[ -z $(k8s_secret INTERNAL_SERVICE_SECRET_PREVIOUS) ]] || fail "--finish kept the previous value"
[[ $(api_post "$old") == 401 ]] || fail "the api still takes the old value after --finish"
ingest_event
atlas_runs "$V1" 2

step "k8s uninstall keeps the embedded database's claim and its Secrets; k8s install takes them back"
NS=atlas-eval
ingest_event
events=$(status events)
claim=$(kubectl get -n "$NS" pvc data-atlas-postgres-0 -o jsonpath='{.metadata.uid}')
secret=$(kubectl get -n "$NS" secret atlas-stowage -o jsonpath='{.data}')
ctl v1.sh k8s uninstall --namespace "$NS" --yes --timeout 3m >"$WORK/uninstall.log" 2>&1 || { cat "$WORK/uninstall.log"; cluster_logs >&2; fail "k8s uninstall"; }
grep -q 'kept: its PersistentVolumeClaims data-atlas-postgres-0, which hold its data' "$WORK/uninstall.log" || { cat "$WORK/uninstall.log"; fail "uninstall does not say it kept the claim"; }
! helm status -n "$NS" atlas >/dev/null 2>&1 || fail "the release is still installed"
[[ -z $(kubectl get -n "$NS" serviceaccount atlas-stowage-provision --ignore-not-found -o name) ]] || fail "the hook's account is left"
[[ $(kubectl get -n "$NS" pvc data-atlas-postgres-0 -o jsonpath='{.metadata.uid}') == "$claim" ]] || fail "the database's claim was deleted"
(cd "$OP" && "${OPERATOR[@]}" sh v1.sh k8s install --namespace "$NS" --no-input --timeout 5m) >"$WORK/again.log" 2>&1 ||
	{ cat "$WORK/again.log"; cluster_logs >&2; fail "k8s install over the kept data"; }
atlas_runs "$V1" 1
[[ $(kubectl get -n "$NS" secret atlas-stowage -o jsonpath='{.data}') == "$secret" ]] || fail "the install did not take the kept Secret back"
[[ $(status events) == "$events" ]] || fail "the install over the kept claim lost the events"

printf '\nPASS\n'
