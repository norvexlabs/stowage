#!/usr/bin/env bash
# End-to-end test of examples/full (atlas) on the Docker shape: the real stowage and <short>-stow
# binaries, a real registry (zot, as in e2e-registry.sh) and the local Docker engine. Needs Docker
# and network access.
#
#   make e2e-full          (builds ./bin/stowage first)
#
# The example as it stands — CalVer releases, token pushes, signed with a key its environment
# holds, a private registry the installer pulls with its baked account — with a real Postgres its
# api migrates through two roles, and a provision hook that registers the administrator through
# the api. Walks: ship this month's .1 → the bootstrap one-liner installs it in loopback mode
# (every route serves but ops, which asks for a client certificate plain HTTP never carries; a
# one-shot service no other one waits for exits 0; the hook's credentials file; the endpoint printed, the tip of the search
# extension, not published; the ui's SBOM, exported, takes a package from the document its image ships) → re-runs set the search extension's input, then clear it, the hook
# handed back the key it minted → the app's users revoke that key: `provision` fails
# naming `--set`, `provision --set ADMIN_API_KEY=` mints and registers another, the next run keeps
# it → its record lost, `provision --set ADMIN_API_KEY=<key>` gives the key the hook never returned
# (the spec declares it in hooks.provision.returns) and no other is minted → re-runs turn the api route's forward auth on and off, narrow the ingest route's allowlist,
# set an advanced input and give it back to its default → `secrets rotate` takes the internal
# secret through its three passes, each value accepted while it must be → ship .2 (a migration) → upgrade --version → the data kept → rollback is allowed (the
# previous api reads the newer schema, by its label) → ship .3 (a migration that is a rollback
# barrier) → upgrade → rollback is refused, naming the barrier and the snapshot.
set -euo pipefail

# The amd64 image of the example's postgres: the one e2e-docker mirrors.
PG=postgres:17-alpine@sha256:aa90e97ee862e558111d34cfb8b2c4bec768c2b039fb791341686928560263b3
. "$(dirname "$0")/lib.sh"

D=$WORK/stowage-e2e-install
# The installer runs as an operator's would (OPERATOR).
CTL=("${OPERATOR[@]}" "$D/at-stow")
CLEANUP+=('remove_install')

# status <field>…: those fields of one answer of the api's /api/v1/status (through the front's
# api route), joined by /.
status() {
	curl -sf --max-time 10 "http://127.0.0.1:$API_PORT/api/v1/status" |
		python3 -c 'import json,sys; s=json.load(sys.stdin); print("/".join(str(s[k]) for k in sys.argv[1:]))' "$@"
}
# atlas_running <version> <schema>: the api answers as that release on that migration, ready; the
# console and ingest routes serve.
atlas_running() {
	local got="" end=$((SECONDS + 60))
	while ((SECONDS < end)); do
		got=$(status version schema 2>/dev/null) || true
		[[ $got == "$1/$2" ]] && break
		sleep 0.2
	done
	[[ $got == "$1/$2" ]] || { stack_logs >&2; fail "the api answers $got (version/migration), want $1/$2"; }
	expect_code 200 "http://127.0.0.1:$API_PORT/ready"
	[[ $(curl -sf --max-time 10 "http://127.0.0.1:$CONSOLE_PORT/") == *'<title>Atlas'* ]] || fail "the console route does not serve the ui"
	expect_code 200 "http://127.0.0.1:$INGEST_PORT/health"
}

step "start zot"
start_zot

step "project, ship this month's first release"
new_full "$PG"
V1=$CALVER.1 V2=$CALVER.2 V3=$CALVER.3
"${S[@]}" ship --channel stable 2>"$WORK/ship-$V1.log" || { cat "$WORK/ship-$V1.log"; fail "ship $V1"; }
grep -q 'not in the current month' "$WORK/ship-$V1.log" && fail "this month's release is out of its month"

step "install $V1 with the bootstrap one-liner (loopback)"
# Three distinct free ports for the stack, picked just before they are bound.
CONSOLE_PORT=$(free_port) API_PORT=$(free_port) INGEST_PORT=$(free_port) OPS_PORT=$(free_port) STATUS_PORT=$(free_port)
while [[ $API_PORT == "$CONSOLE_PORT" ]]; do API_PORT=$(free_port); done
while [[ $INGEST_PORT == "$CONSOLE_PORT" || $INGEST_PORT == "$API_PORT" ]]; do INGEST_PORT=$(free_port); done
while [[ $OPS_PORT == "$CONSOLE_PORT" || $OPS_PORT == "$API_PORT" || $OPS_PORT == "$INGEST_PORT" ]]; do OPS_PORT=$(free_port); done
while [[ " $CONSOLE_PORT $API_PORT $INGEST_PORT $OPS_PORT " == *" $STATUS_PORT "* ]]; do STATUS_PORT=$(free_port); done
login_pull
docker run --rm --pull always "$REG/atlas/atlas-installer:latest" >"$WORK/atlas.sh" || fail "docker run of the installer image"
unset DOCKER_CONFIG # for the next ships; the installer's environment never has it (OPERATOR)
(cd "$WORK" && "${OPERATOR[@]}" sh atlas.sh --no-input --dir "$D" --ports "console=$CONSOLE_PORT,api=$API_PORT,ingest=$INGEST_PORT,ops=$OPS_PORT,status=$STATUS_PORT" \
	--set ADMIN_EMAIL=admin@atlas.test --set ORG_NAME=Atlas) >"$WORK/install.log" 2>&1 || { cat "$WORK/install.log"; fail "install"; }
atlas_running "$V1" 1
# The one-shot echo-check, which no service waits for, exited 0: the install came up all the same.
[[ $(docker ps -a --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=echo-check --format '{{.State}} {{.Status}}') == "exited Exited (0)"* ]] ||
	{ stack_logs >&2; fail "echo-check did not exit 0"; }
# The vpn service runs on the host's network (services.vpn.host_network), where echo-check
# reached it by its name, at the host's gateway.
vpn_id=$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=vpn)
[[ -n $vpn_id && $(docker inspect --format '{{.HostConfig.NetworkMode}}' "$vpn_id") == host ]] ||
	{ stack_logs >&2; fail "the vpn service is not on the host's network"; }
# The ops route asks for a client certificate, which plain HTTP never carries: it refuses everyone.
expect_code 403 --max-time 10 "http://127.0.0.1:$OPS_PORT/health"
# The status route serves its path on its own port, the path kept for the api; nothing else there.
[[ $(curl -sf --max-time 10 "http://127.0.0.1:$STATUS_PORT/api/v1/status") == *'"version"'* ]] || fail "the status route does not serve the api's status"
expect_code 404 --max-time 10 "http://127.0.0.1:$STATUS_PORT/health"
grep -qx "STOWAGE_URL_STATUS='\{0,1\}http://[^:]*:$STATUS_PORT/api/v1/status'\{0,1\}" "$D/.env" || fail "the status route's URL does not carry its path"
grep -qF 'ops refuse(s) everyone in loopback mode' "$WORK/install.log" || { cat "$WORK/install.log"; fail "the install does not say the ops route refuses everyone"; }
kept "$V1"
[[ $(status organisation) == Atlas ]] || fail "the api did not record ORG_NAME"
[[ $(status admins) == 1 ]] || fail "the provision hook did not register the administrator"
grep -q '^ADMIN_LOGIN=admin@atlas.test$' "$D/credentials.txt" && grep -qE '^ADMIN_PASSWORD=[A-Za-z0-9]{20}$' "$D/credentials.txt" ||
	fail "the hook's outputs file holds no credentials"
[[ $(stat -c %a "$D/credentials.txt") == 600 ]] || fail "the credentials file is not 0600"
grep -qx "COOKIE_SECURE='\{0,1\}false'\{0,1\}" "$D/.env" || fail "loopback mode did not relax COOKIE_SECURE"
expect_code 201 -X POST -H 'Content-Type: application/json' -d '{"kind": "probe"}' "http://127.0.0.1:$INGEST_PORT/events"
[[ $(status events) == 1 ]] || fail "the ingested event did not reach the api"
expect_code 401 -X POST "http://127.0.0.1:$API_PORT/api/v1/maintenance/purge"
[[ $(status search) == False ]] || fail "the api reaches a search service while SEARCH_URL is empty"
# services.postgres.memory: its limit is 25 % of this host's RAM (in 64 MiB steps), at most its 16 GiB
# maximum, never under its own 256 MiB, and it sizes its cache from the variable it is given.
pg=$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=postgres)
host_mb=$(awk '/^MemTotal:/ { print int($2 / 65536) * 64 }' /proc/meminfo)
want_mb=$((host_mb * 25 / 100)); ((want_mb < 16384)) || want_mb=16384; ((want_mb > 256)) || want_mb=256
[[ $(docker inspect --format '{{.HostConfig.Memory}}' "$pg") == $((want_mb << 20)) ]] ||
	fail "postgres's memory limit is $(docker inspect --format '{{.HostConfig.Memory}}' "$pg"), want $want_mb MiB of $host_mb"
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$pg" | grep -qxF "STOWAGE_HOST_MEM_MB=$host_mb" ||
	fail "postgres is not given the host's RAM"
[[ $(docker exec "$pg" psql -U atlas -d atlas -tAc "SELECT setting::bigint * 8 / 1024 FROM pg_settings WHERE name = 'shared_buffers'") == $((want_mb / 4)) ]] ||
	fail "postgres did not size shared_buffers from STOWAGE_MEM_LIMIT_MB"
# The release's network profile: each container gets the profile's settings and the host's open
# files, the stack's network the host route's MTU.
api=$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=api)
read -r reuse somax < <(docker exec "$api" python3 -c 'print(*(open("/proc/sys/net/" + k).read().strip() for k in ("ipv4/tcp_tw_reuse", "core/somaxconn")))') ||
	fail "the api's namespace does not read"
[[ $reuse == 1 ]] && ((somax >= 8192)) || fail "the api's namespace holds tcp_tw_reuse $reuse, somaxconn $somax: not the network profile's"
# Its open files: fs.nr_open, bounded by the engine's own limit; the soft limit at most 65536.
read -r _ soft hard < <(docker inspect --format '{{range .HostConfig.Ulimits}}{{.Name}} {{.Soft}} {{.Hard}}{{println}}{{end}}' "$api" | grep '^nofile ') || true
[[ -n $hard ]] && ((hard > 1024 && hard <= $(cat /proc/sys/fs/nr_open) && soft == (hard < 65536 ? hard : 65536))) ||
	fail "the api is given ${soft:-no} open files (hard ${hard:-none})"
mtu=$(cat "/sys/class/net/$(ip -o route show default | awk '{print $5; exit}')/mtu")
net_mtu=$(docker network inspect --format '{{index .Options "com.docker.network.driver.mtu"}}' "$(project)_default")
[[ $net_mtu == "$mtu" ]] || fail "the stack's network has MTU '$net_mtu', the host's route $mtu"
# The loopback URL of the api route, as the install records it (STOWAGE_URL_API).
url=$(sed -n "s/^STOWAGE_URL_API='\{0,1\}\([^']*\)'\{0,1\}$/\1/p" "$D/.env")
[[ $url == http://*:$API_PORT ]] || fail "the install records the api route's URL as $url"
grep -qxF "endpoint  $url (loopback: this machine only; another compose project's containers cannot reach it, a project using atlas as an extension reaches it through a domain: a new install with --tls)" "$WORK/install.log" ||
	{ cat "$WORK/install.log"; fail "the install did not print its endpoint (the api route)"; }
grep -qF "tip: the extension atlas-search (Full-text search over the events) is off: SEARCH_URL is empty" "$WORK/install.log" &&
	grep -qF "$REG/atlas/atlas-search-installer is not published on the stable channel" "$WORK/install.log" &&
	grep -qF "then give its endpoint: $D/at-stow --dir $D --set SEARCH_URL=<its endpoint> --answers <a file: SEARCH_KEY: <its key>>" "$WORK/install.log" ||
	{ cat "$WORK/install.log"; fail "the install did not end with the search extension's tip"; }
"${CTL[@]}" status --dir "$D" >"$WORK/status.txt" 2>&1 || { cat "$WORK/status.txt"; fail "status"; }
grep -qxF "endpoint  $url" "$WORK/status.txt" || { cat "$WORK/status.txt"; fail "status does not show the endpoint"; }
grep -qx "tune      network (release)" "$WORK/status.txt" || { cat "$WORK/status.txt"; fail "status does not show the release's tuning profile"; }
# The stack's network declarations (its vpn table, forwarding, routing) are the Docker host's
# operator's until tune --apply: said by the install's end and by status.
grep -qF "are not in force on this host, whose firewall and routing are its operator's" "$WORK/install.log" &&
	grep -q "^gateway   not in force: " "$WORK/status.txt" ||
	{ cat "$WORK/install.log" "$WORK/status.txt"; fail "the install or status does not say the network declarations are not in force"; }
# The ui image ships a third-party script and the SBOM document declaring it: the image's attested
# SBOM takes the package from that document.
"${CTL[@]}" sbom export "$WORK/sbom" --dir "$D" >"$WORK/sbom.txt" 2>&1 || { cat "$WORK/sbom.txt"; fail "sbom export"; }
jq -e 'any(.packages[]; .name == "escape-html" and .versionInfo == "1.0.3" and .licenseDeclared == "MIT" and (.sourceInfo | endswith("from SBOM: /usr/share/sbom/ui.cdx.json")) and any(.externalRefs[]?; .referenceLocator == "pkg:npm/escape-html@1.0.3"))' "$WORK/sbom/ui.spdx.json" >/dev/null ||
	{ jq -c '[.packages[] | .name]' "$WORK/sbom/ui.spdx.json"; fail "the ui's SBOM lacks the document its image ships"; }

step "a re-run sets the search extension's input, then clears it"
pw=$(grep '^ADMIN_PASSWORD=' "$D/credentials.txt") key=$(sed -n 's/^ADMIN_API_KEY=//p' "$D/credentials.txt")
[[ -n $key ]] || fail "the hook returned no ADMIN_API_KEY"
# The api's own liveness stands in for a search service: what is proven is that the input reaches it.
"${CTL[@]}" --dir "$D" --no-input --set SEARCH_URL=http://api:9090 >"$WORK/search-on.log" 2>&1 || { cat "$WORK/search-on.log"; fail "re-run --set SEARCH_URL=http://api:9090"; }
grep -q 'changing SEARCH_URL: "" → http://api:9090' "$WORK/search-on.log" || { cat "$WORK/search-on.log"; fail "the re-run did not log the change"; }
grep -q 'tip: the extension atlas-search' "$WORK/search-on.log" && { cat "$WORK/search-on.log"; fail "a tip for the search extension while SEARCH_URL is set"; }
[[ $(status search) == True ]] || { stack_logs >&2; fail "the api does not reach SEARCH_URL after it was set"; }
"${CTL[@]}" --dir "$D" --no-input --set SEARCH_URL= >"$WORK/search-off.log" 2>&1 || { cat "$WORK/search-off.log"; fail "re-run --set SEARCH_URL="; }
[[ $(status search) == False ]] || fail "the api still reaches a search service after SEARCH_URL was cleared"
grep -q 'tip: the extension atlas-search' "$WORK/search-off.log" || { cat "$WORK/search-off.log"; fail "no tip once SEARCH_URL is cleared"; }
[[ $(grep '^ADMIN_PASSWORD=' "$D/credentials.txt") == "$pw" ]] || fail "a re-run minted a new administrator password"
[[ $(sed -n 's/^ADMIN_API_KEY=//p' "$D/credentials.txt") == "$key" ]] || fail "a re-run minted a new API key: what the hook returned was not handed back"
atlas_running "$V1" 1

step "the L4 routes: the echo service over TCP and UDP on 17007, published beside the front, then on one address; an optional service its input turns on"
# echo_l4 tcp|udp: what the echo service sends back for "ping" (TCP: prefixed by its endpoint).
echo_l4() {
	python3 -c '
import socket, sys
proto = sys.argv[1]
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM if proto == "tcp" else socket.SOCK_DGRAM)
s.settimeout(10)
s.connect(("127.0.0.1", 17007))
s.sendall(b"ping\n" if proto == "tcp" else b"ping")
print(s.recv(256).decode().strip())' "$1"
}
[[ $(echo_l4 tcp) == "localhost:17007 ping" ]] || { stack_logs >&2; fail "the echo service over TCP (STOWAGE_L4_ECHO handed to it)"; }
[[ $(echo_l4 udp) == "ping" ]] || fail "the echo service over UDP"
# ECHO_BIND publishes the TCP route on that host address alone; empty, on every address again.
echo_ports() { docker port "$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=echo)" 17007/tcp | sort | tr '\n' ' '; }
[[ $(echo_ports) == *0.0.0.0:17007* ]] || fail "the TCP echo is published on $(echo_ports), not every address"
"${CTL[@]}" --dir "$D" --no-input --set ECHO_BIND=127.0.0.1 >"$WORK/bind.log" 2>&1 || { cat "$WORK/bind.log"; fail "re-run --set ECHO_BIND=127.0.0.1"; }
[[ $(echo_ports) == "127.0.0.1:17007 " ]] || fail "the TCP echo is published on $(echo_ports), not 127.0.0.1 alone"
[[ $(echo_l4 tcp) == "localhost:17007 ping" ]] || fail "the echo service bound to 127.0.0.1 does not answer"
"${CTL[@]}" --dir "$D" --no-input --set ECHO_BIND= >"$WORK/unbind.log" 2>&1 || { cat "$WORK/unbind.log"; fail "re-run --set ECHO_BIND="; }
[[ $(echo_ports) == *0.0.0.0:17007* ]] || fail "the TCP echo is published on $(echo_ports) once ECHO_BIND is empty"
grep -qxF "echo      tcp/17007 localhost:17007 · on every address, open to whoever reaches this host (ECHO_BIND gives it one address)" "$WORK/status.txt" &&
	grep -qxF "echo-udp  udp/17007 · on every address, open to whoever reaches this host" "$WORK/status.txt" ||
	{ cat "$WORK/status.txt"; fail "status does not list the L4 routes"; }
relay() { docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=relay; }
[[ -z $(relay) ]] || fail "the relay runs while RELAY_SOURCE is empty"
"${CTL[@]}" --dir "$D" --no-input --set RELAY_SOURCE=sensors >"$WORK/relay-on.log" 2>&1 || { cat "$WORK/relay-on.log"; fail "re-run --set RELAY_SOURCE=sensors"; }
[[ -n $(relay) ]] || fail "the relay does not run once RELAY_SOURCE is set"
"${CTL[@]}" --dir "$D" --no-input --set RELAY_SOURCE= >"$WORK/relay-off.log" 2>&1 || { cat "$WORK/relay-off.log"; fail "re-run --set RELAY_SOURCE="; }
[[ -z $(relay) ]] || fail "the relay still runs once RELAY_SOURCE is cleared"

step "a key the app's users revoked: provision fails naming --set, which replaces it before the hook runs"
# key_status <key>: the status the api answers its key check with, through the api route.
key_status() { curl -s -o /dev/null -w '%{http_code}' --max-time 10 -H "Authorization: Bearer $1" "http://127.0.0.1:$API_PORT/api/v1/key"; }
[[ $(key_status "$key") == 200 ]] || fail "the api does not know the key the hook registered"
docker exec "$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=postgres)" \
	psql -U atlas -d atlas -qc 'DELETE FROM api_keys' || fail "revoking the key"
if "${CTL[@]}" provision --dir "$D" >"$WORK/prov.log" 2>&1; then cat "$WORK/prov.log"; fail "provision passed with a revoked key"; fi
grep -qF 'provision --set ADMIN_API_KEY=` forgets it' "$WORK/prov.log" && grep -qF "at-stow provision --dir $D\` continues it, and with \`--set KEY=VALUE\` sets a value it returned or declares it returns (" "$WORK/prov.log" ||
	{ cat "$WORK/prov.log"; fail "the failure does not name provision --set"; }
"${CTL[@]}" provision --dir "$D" --set ADMIN_API_KEY= >"$WORK/prov-set.log" 2>&1 || { cat "$WORK/prov-set.log"; fail "provision --set ADMIN_API_KEY="; }
new=$(sed -n 's/^ADMIN_API_KEY=//p' "$D/credentials.txt")
[[ -n $new && $new != "$key" && $(key_status "$new") == 200 ]] || fail "provision --set ADMIN_API_KEY= did not mint and register another key"
! grep -qF "$new" "$WORK/prov-set.log" || fail "the hook's output reached the installer's log"
"${CTL[@]}" provision --dir "$D" >"$WORK/prov2.log" 2>&1 || { cat "$WORK/prov2.log"; fail "provision"; }
[[ $(sed -n 's/^ADMIN_API_KEY=//p' "$D/credentials.txt") == "$new" && $(status admins) == 1 ]] || fail "a provision run did not keep the key it was handed back"

step "a key the hook never returned (its record lost): provision --set gives it, as hooks.provision.returns declares it"
# db_keys: the API keys the api holds.
db_keys() { docker exec "$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=postgres)" \
	psql -U atlas -d atlas -tAc 'SELECT count(*) FROM api_keys'; }
keys=$(db_keys)
sed -i '/^ADMIN_API_KEY=/d' "$D/provision.env"
! grep -q '^ADMIN_API_KEY=' "$D/provision.env" || fail "provision.env still holds ADMIN_API_KEY"
if "${CTL[@]}" provision --dir "$D" --set NOPE=x >"$WORK/prov-nope.log" 2>&1; then cat "$WORK/prov-nope.log"; fail "provision --set of an undeclared key passed"; fi
grep -qF 'NOPE is neither an input only the provision hook reads (ADMIN_EMAIL) nor a value it returned or declares it returns (ADMIN_API_KEY, ADMIN_LOGIN, ADMIN_PASSWORD)' "$WORK/prov-nope.log" ||
	{ cat "$WORK/prov-nope.log"; fail "the refusal does not name the declared key"; }
"${CTL[@]}" provision --dir "$D" --set "ADMIN_API_KEY=$new" >"$WORK/prov-given.log" 2>&1 || { cat "$WORK/prov-given.log"; fail "provision --set of a declared key the hook never returned"; }
grep -q "recorded ADMIN_API_KEY in $D/provision.env" "$WORK/prov-given.log" || { cat "$WORK/prov-given.log"; fail "provision --set did not record the declared key"; }
! grep -qF "$new" "$WORK/prov-given.log" || fail "the given key reached the installer's log"
[[ $(sed -n 's/^ADMIN_API_KEY=//p' "$D/credentials.txt") == "$new" && $(db_keys) == "$keys" ]] || fail "the hook did not keep the key it was given, or minted another"
"${CTL[@]}" provision --dir "$D" >"$WORK/prov3.log" 2>&1 || { cat "$WORK/prov3.log"; fail "provision"; }
[[ $(sed -n 's/^ADMIN_API_KEY=//p' "$D/credentials.txt") == "$new" && $(db_keys) == "$keys" ]] || fail "the next run was not handed the key given"

step "the front: forward auth on the api route, the ingest route's allowlist; an advanced input"
rerun() { "${CTL[@]}" --dir "$D" --no-input "$@" >"$WORK/rerun.log" 2>&1 || { cat "$WORK/rerun.log"; fail "re-run $*"; }; }
whoami="http://127.0.0.1:$API_PORT/api/v1/whoami"
[[ $(curl -sf --max-time 10 "$whoami") == '{"user": ""}' ]] || fail "the api route asks an authentication service while AUTH_ADDRESS is empty"
rerun --set AUTH_ADDRESS=api:9090 --set AUTH_URI=/api/v1/auth
expect_code 401 --max-time 10 "$whoami"
expect_code 401 --max-time 10 -H 'X-Auth-Request-User: mallory' "$whoami"
got=$(curl -sf --max-time 10 -H 'X-Atlas-User: alice' -H 'X-Auth-Request-User: mallory' "$whoami") || fail "the authenticated request failed"
[[ $got == '{"user": "alice"}' ]] || fail "the api was given $got: the front must strip the client's identity header, then pass the authentication service's"
rerun --set AUTH_ADDRESS= --set AUTH_URI=
[[ $(curl -sf --max-time 10 "$whoami") == '{"user": ""}' ]] || fail "forward auth stayed on with AUTH_ADDRESS empty"
expect_code 200 --max-time 10 "http://127.0.0.1:$INGEST_PORT/health"
rerun --set INGEST_SOURCES=192.0.2.1/32
expect_code 403 --max-time 10 "http://127.0.0.1:$INGEST_PORT/health"
rerun --set INGEST_SOURCES=
expect_code 403 --max-time 10 "http://127.0.0.1:$INGEST_PORT/health"
rerun --set 'INGEST_SOURCES=0.0.0.0/0, ::/0'
expect_code 200 --max-time 10 "http://127.0.0.1:$INGEST_PORT/health"
log_level() { docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=api)" | sed -n 's/^LOG_LEVEL=//p'; }
tunable() { local out; out=$("${CTL[@]}" status --dir "$D") || fail "status"; grep -qx "tunable   LOG_LEVEL=$1" <<<"$out"; }
tunable '(not set)' || fail "status does not show the advanced input at its default"
[[ $(log_level) == info ]] || fail "LOG_LEVEL is $(log_level), not the compose file's default"
rerun --set LOG_LEVEL=debug
[[ $(log_level) == debug ]] && tunable debug || fail "the advanced input was not set"
rerun --set LOG_LEVEL=
[[ $(log_level) == info ]] && ! grep -q '^LOG_LEVEL=' "$D/.env" || fail "the advanced input did not go back to its default"
atlas_running "$V1" 1

step "secrets rotate INTERNAL_SERVICE_SECRET: --stage, the swap, --finish"
secret() { sed -n "s/^$1='\(.*\)'\$/\1/p" "$D/.env"; }
event() { expect_code 201 --max-time 10 -X POST -H 'Content-Type: application/json' -d '{"kind": "rotation"}' "$@"; }
api_event() { event -H "Authorization: Bearer $1" "http://127.0.0.1:$API_PORT/api/v1/events"; }
rotate() { "${CTL[@]}" secrets rotate INTERNAL_SERVICE_SECRET --dir "$D" "$@" >"$WORK/rotate.log" 2>&1 || { cat "$WORK/rotate.log"; fail "secrets rotate $*"; }; }
old=$(secret INTERNAL_SERVICE_SECRET)
rotate --stage
next=$(secret INTERNAL_SERVICE_SECRET_NEXT)
[[ -n $next && $(secret INTERNAL_SERVICE_SECRET) == "$old" ]] || fail "--stage did not record a new value beside the old one"
api_event "$next"
event "http://127.0.0.1:$INGEST_PORT/events"
rotate
[[ $(secret INTERNAL_SERVICE_SECRET) == "$next" && $(secret INTERNAL_SERVICE_SECRET_PREVIOUS) == "$old" && -z $(secret INTERNAL_SERVICE_SECRET_NEXT) ]] ||
	fail "the swap did not make the new value current and keep the old one"
api_event "$old"
event "http://127.0.0.1:$INGEST_PORT/events"
rotate --finish
[[ -z $(secret INTERNAL_SERVICE_SECRET_PREVIOUS) ]] || fail "--finish kept the previous value"
expect_code 401 --max-time 10 -X POST -H "Authorization: Bearer $old" "http://127.0.0.1:$API_PORT/api/v1/events"
event "http://127.0.0.1:$INGEST_PORT/events"
atlas_running "$V1" 1
EV=$(status events) # the events so far, which the upgrades keep

step "ship $V2 (a migration), upgrade --version $V2: the api start-first, no request lost"
full_release 2 002_event_source.sql "-- 002: where an event came from
ALTER TABLE events ADD COLUMN source text;"
# The api route asked every 0.1 s through the upgrade: its new container serves before the old one
# stops, which drains.
(while [[ ! -e $WORK/up2.done ]]; do
	curl -s -o /dev/null -w '%{http_code}\n' --max-time 5 "http://127.0.0.1:$API_PORT/health" >>"$WORK/poll2.log" || true
	sleep 0.1
done) &
poller=$!
up2=0
"${CTL[@]}" upgrade --version "$V2" --dir "$D" --no-input >"$WORK/up2.log" 2>&1 || up2=$?
touch "$WORK/up2.done"
wait "$poller"
((up2 == 0)) || { cat "$WORK/up2.log"; fail "upgrade to $V2"; }
atlas_running "$V2" 2
grep -q 'services.api: the new container is healthy, the old one stopped' "$WORK/up2.log" || { cat "$WORK/up2.log"; fail "the upgrade did not replace the api start-first"; }
grep -q 'services.api: the new container runs: SIGUSR2 sent to the old one (update_signal)' "$WORK/up2.log" || { cat "$WORK/up2.log"; fail "the upgrade did not send the old api its update_signal"; }
lost=$(grep -cv '^200$' "$WORK/poll2.log" || true)
((lost == 0 && $(wc -l <"$WORK/poll2.log") > 10)) || fail "the api route failed $lost of $(wc -l <"$WORK/poll2.log") requests through the upgrade: $(sort "$WORK/poll2.log" | uniq -c | tr '\n' ' ')"
api=$(docker ps --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=api --format '{{.ID}}')
[[ $(docker inspect --format '{{range .Mounts}}{{.Name}} {{.Destination}};{{end}}' "$api") == *"$(project)_stowage-update-api /run/atlas/handoff;"* ]] ||
	fail "the api does not mount its update_socket volume at /run/atlas/handoff"
# A start that changes nothing leaves it: the configuration's digest matches its container's.
"${CTL[@]}" --dir "$D" --no-input >"$WORK/rerun2.log" 2>&1 || { cat "$WORK/rerun2.log"; fail "re-run after the upgrade"; }
! grep -q 'services.api' "$WORK/rerun2.log" && [[ $(docker ps --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=api --format '{{.ID}}') == "$api" ]] ||
	{ cat "$WORK/rerun2.log"; fail "a re-run that changes nothing replaced the api"; }
kept "$V2"
[[ $(status events) == "$EV" && $(status admins) == 1 ]] || fail "the upgrade lost data"
gzip -dc "$D"/snapshots/*.sql.gz >"$WORK/snapshot.sql" && grep -q 'PostgreSQL database dump' "$WORK/snapshot.sql" || fail "the upgrade took no pg_dump snapshot"

step "rollback to $V1 (allowed: its api reads the newer schema)"
"${CTL[@]}" rollback --dir "$D" >"$WORK/back1.log" 2>&1 || { cat "$WORK/back1.log"; fail "rollback"; }
grep -q "the database is at 2; $V1 reads past the newer migrations" "$WORK/back1.log" || { cat "$WORK/back1.log"; fail "the rollback did not check the schema"; }
atlas_running "$V1" 2
kept "$V1"

step "ship $V3 (a rollback barrier), upgrade, rollback is refused"
full_release 3 003_events_partitioned.sql "-- rollback barrier: events moved to a partitioned table
ALTER TABLE events RENAME TO events_legacy;
CREATE TABLE events (id bigserial, received timestamptz NOT NULL DEFAULT now(), body jsonb NOT NULL, source text) PARTITION BY RANGE (received);
CREATE TABLE events_all PARTITION OF events DEFAULT;
INSERT INTO events (received, body, source) SELECT received, body, source FROM events_legacy;
DROP TABLE events_legacy;"
"${CTL[@]}" upgrade --version "$V3" --dir "$D" --no-input >"$WORK/up3.log" 2>&1 || { cat "$WORK/up3.log"; fail "upgrade to $V3"; }
atlas_running "$V3" 3
[[ $(status events) == "$EV" ]] || fail "the barrier migration lost the events"
if "${CTL[@]}" rollback --dir "$D" >"$WORK/back3.log" 2>&1; then fail "rollback crossed a barrier"; fi
grep -q 'rollback barrier: events moved to a partitioned table' "$WORK/back3.log" && grep -q 'snapshots/' "$WORK/back3.log" ||
	{ cat "$WORK/back3.log"; fail "the refusal does not name the barrier and the snapshot"; }
atlas_running "$V3" 3
kept "$V3"

printf '\nPASS\n'
