#!/usr/bin/env bash
# End-to-end test of examples/stateless (lookup) on the Docker shape: a project without a
# database or a front, no image of its own built (one pulled image), served by its L4 route alone, the real
# stowage and <short>-stow binaries, a real registry (zot) and the local Docker engine. Needs
# Docker and network access.
#
#   make e2e-stateless     (builds ./bin/stowage first)
#
# Walks: ship 1.0.0 → the bootstrap one-liner installs it, with no TLS mode asked → the health path
# and the L4 route serve, the volume was given the server's uid (the server writes into it), no
# front runs → a re-run changes an input its page is rendered from (an inline config: the server is
# made again) → status lists the L4 route → --tls is refused → a backup of its volume alone
# (volumes-age-rclone: the archive and a manifest, no dump) to WebDAV, and a restore that brings the
# volume back → ship 1.0.1, `upgrade --version
# 1.0.1` (nothing dumped) → rollback (no schema check) → reset-db is refused: the release has no
# database → an install of 0.9.0, which the last release's stowage (built from its tag) shipped
# before 1.0.0, upgrades to 1.0.1 with this one's, and a rollback goes back to it with its own
# installer → 1.0.2 adds a front terminating the L4 route's TLS alone (no HTTP route, no portal),
# installed with --tls internal (no ALPN protocol selected), then re-run with --tls byo, its CA
# published into a volume.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

D=$WORK/stowage-e2e-install
# The installer runs as an operator's would (OPERATOR).
CTL=("${OPERATOR[@]}" "$D/lookup-stow")
CLEANUP+=('remove_install')
page() { curl -sf --max-time 10 "http://127.0.0.1:$PORT/${1:-}"; }

step "start zot"
start_zot

# private: the example in $1 published to zot's notes/** repositories, read with the pull account
# (its public images become private ones there), its L4 route on $PORT.
private() {
	python3 - "$1/stowage.yaml" "$REG" "$PORT" <<'PY'
import sys
path, reg, port = sys.argv[1:]
s = open(path).read()
for old, new in [
    ("prefix: registry.example.com/acme", f"prefix: {reg}/notes"),
    ("pull: { provider: none }", "pull: { provider: token, user: pull, token_env: NOTES_PULL_TOKEN }"),
    ("port: 8090,", f"port: {port},"),
]:
    assert s.count(old) == 1, old
    s = s.replace(old, new)
open(path, "w").write(s)
PY
}
PORT=$(free_port)

step "the last release's stowage, built from its tag, ships 0.9.0"
# The nearest release tag in this branch's history that is not this build's version (by history,
# never by sorting tags); its checks ran when it was released.
THIS=$("$STOWAGE" --version | awk '{print $NF}')
OLDV=$(git -C "$REPO" describe --tags --abbrev=0 --match 'v*' --exclude "v$THIS" HEAD) || fail "no release before $THIS to install with"
OLDV=${OLDV#v}
OLD=$WORK/stowage-$OLDV
mkdir -p "$OLD" "$CACHE/stowage-go"
git -C "$REPO" archive "v$OLDV" | tar -x -C "$OLD"
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e CGO_ENABLED=0 -e GOFLAGS=-trimpath \
	-v "$CACHE/stowage-go:/cache" -v "$OLD:/src" -w /src "golang:$(sed -n 's/^GO_VERSION ?= //p' "$OLD/Makefile")-bookworm" sh -ec "
	GOOS=linux GOARCH=amd64 go build -ldflags '-s -w -buildid= -X main.version=$OLDV' -o out/stowage-install ./cmd/stowage-install
	cp out/stowage-install internal/installerbin/stowage-install
	go build -tags stowage_installer -ldflags '-s -w -buildid= -X main.version=$OLDV' -o out/stowage ./cmd/stowage" >"$WORK/old-build.log" 2>&1 ||
	{ cat "$WORK/old-build.log"; fail "building stowage $OLDV"; }
P0=$WORK/lookup-$OLDV
cp -r "$OLD/examples/stateless" "$P0"
private "$P0"
python3 - "$P0/CHANGELOG.md" <<'PY'
import re, sys
p = sys.argv[1]
s, n = re.subn(r"^## \[[^]]+\].*$", "## [0.9.0]", open(p).read(), count=1, flags=re.M)
assert n == 1, "no top entry in " + p
open(p, "w").write(s)
PY
S0=("$OLD/out/stowage" -C "$P0")
"${S0[@]}" lock --update >/dev/null
"${S0[@]}" ship --channel stable >"$WORK/ship-0.9.0.out" 2>"$WORK/ship-0.9.0.log" || { cat "$WORK/ship-0.9.0.log"; fail "ship 0.9.0 with stowage $OLDV"; }
# The installer image as the ship names it, by digest.
INSTALLER0=$(sed -n 's/^Then: docker run --rm \([^ ]*\) > .*/\1/p' "$WORK/ship-0.9.0.out")
[[ -n $INSTALLER0 ]] || { cat "$WORK/ship-0.9.0.out"; fail "ship 0.9.0 names no installer image"; }

step "project, ship 1.0.0"
P=$WORK/lookup
cp -r "$REPO/examples/stateless" "$P"
private "$P"
S=("$STOWAGE" -C "$P")
"${S[@]}" lock --update >/dev/null # the example pins a stowage; publish with the one built
"${S[@]}" lint
"${S[@]}" ship --channel stable 2>"$WORK/ship-1.0.0.log" || { cat "$WORK/ship-1.0.0.log"; fail "ship 1.0.0"; }

step "install 1.0.0 with the bootstrap one-liner (no front)"
login_pull
docker run --rm --pull always "$REG/notes/lookup-installer:latest" >"$WORK/lookup.sh" || fail "docker run of the installer image"
unset DOCKER_CONFIG # the installer's environment never has it (OPERATOR)
(cd "$WORK" && "${OPERATOR[@]}" sh lookup.sh --no-input --dir "$D" --set GREETING=bonjour) \
	>"$WORK/install.log" 2>&1 || { cat "$WORK/install.log"; fail "install"; }
[[ $(page health) == ok ]] || { stack_logs >&2; fail "the health path does not answer"; }
[[ $(page) == bonjour ]] || fail "the route does not serve the input GREETING"
grep -q 'volume .*_state given to its uid' "$WORK/install.log" || { cat "$WORK/install.log"; fail "the install did not give the state volume its uid"; }
! grep -q 'TLS\|https://\|portal\|served' "$WORK/install.log" || { cat "$WORK/install.log"; fail "the install of a project without a front speaks of one"; }
[[ -z $(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=stowage-front) ]] ||
	fail "a front runs"
! grep -q '^STOWAGE_TLS_MODE=\|^STOWAGE_URL_' "$D/.env" || fail "the install recorded a TLS mode or a route URL"
! grep -qi 'database\|snapshot' "$WORK/install.log" || { cat "$WORK/install.log"; fail "the install of a release without a database names one"; }

step "a re-run changes an input its page is rendered from; status lists the L4 route; --tls is refused"
"${CTL[@]}" --dir "$D" --no-input --set GREETING=salut >"$WORK/rerun.log" 2>&1 || { cat "$WORK/rerun.log"; fail "re-run"; }
grep -q 'changing GREETING: bonjour → salut' "$WORK/rerun.log" || { cat "$WORK/rerun.log"; fail "the re-run did not log the change"; }
! grep -q 'given to its uid' "$WORK/rerun.log" || { cat "$WORK/rerun.log"; fail "the re-run gave a volume it did not make a uid"; }
[[ $(page) == salut ]] || fail "the route does not serve the changed input: its server was not made again"
"${CTL[@]}" status --dir "$D" >"$WORK/status.txt" 2>&1 || { cat "$WORK/status.txt"; fail "status"; }
grep -q "lookup.*tcp.*$PORT" "$WORK/status.txt" || { cat "$WORK/status.txt"; fail "status does not list the L4 route"; }
if "${CTL[@]}" --dir "$D" --no-input --tls acme >"$WORK/tls.log" 2>&1; then cat "$WORK/tls.log"; fail "--tls was taken"; fi
grep -qF 'lookup has no front: --tls' "$WORK/tls.log" || { cat "$WORK/tls.log"; fail "the refusal of --tls does not say there is no front"; }

step "a backup of the volume alone (no database): config, run to WebDAV, list, restore"
start_dav
age_key
server() { docker ps --quiet --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=server; }
docker exec "$(server)" sh -c 'echo at-the-backup >/state/e2e.txt' || fail "a file to back up in the state volume"
"${CTL[@]}" backup config --dir "$D" --destination dav:backups --rclone-config "$DAV_CONF" --recipient "$AGE_RECIPIENT" >"$WORK/backup-config.txt" 2>&1 ||
	{ cat "$WORK/backup-config.txt"; fail "backup config"; }
"${CTL[@]}" backup run --dir "$D" >"$WORK/backup-run.txt" 2>&1 || { cat "$WORK/backup-run.txt"; fail "backup run"; }
! grep -qi 'dump\|database' "$WORK/backup-run.txt" || { cat "$WORK/backup-run.txt"; fail "the backup of a release without a database names one"; }
manifests=("$WORK"/dav/backups/lookup-volumes-*-1.0.0-r1.manifest.age)
[[ ${#manifests[@]} == 1 && -s ${manifests[0]} ]] || { ls -la "$WORK/dav/backups"; fail "no backup's manifest in the destination"; }
[[ -s ${manifests[0]%.manifest.age}.state.tar.age ]] || { ls -la "$WORK/dav/backups"; fail "no archive of the state volume beside the manifest"; }
"$BIN/age" -d -i "$AGE_KEY" "${manifests[0]}" | grep -qF '"project":"lookup","release":{"version":"1.0.0","revision":1},"volumes":["state"]' ||
	fail "the manifest does not decrypt with the key into the release and its volume"
"${CTL[@]}" backup list --dir "$D" >"$WORK/backup-list.txt" 2>&1 || { cat "$WORK/backup-list.txt"; fail "backup list"; }
grep -q "^${manifests[0]##*/} " "$WORK/backup-list.txt" && ! grep -q 'tar\.age' "$WORK/backup-list.txt" ||
	{ cat "$WORK/backup-list.txt"; fail "backup list does not show the backup by its manifest alone"; }
docker exec "$(server)" sh -c 'echo since >/state/e2e.txt && echo later >/state/later.txt' || fail "files after the backup"
"${CTL[@]}" restore "${manifests[0]##*/}" --dir "$D" --identity "$AGE_KEY" --yes >"$WORK/restore.txt" 2>&1 || { cat "$WORK/restore.txt"; fail "restore"; }
[[ $(docker exec "$(server)" sh -c 'cat /state/e2e.txt; stat -c %u /state/e2e.txt; test -e /state/later.txt || echo gone') == $'at-the-backup\n1000\ngone' ]] ||
	{ docker exec "$(server)" ls -lan /state; fail "the restore did not bring the state volume back as it was"; }
[[ $(page) == salut ]] || { stack_logs >&2; fail "the stack does not serve after the restore"; }
"${CTL[@]}" backup config --dir "$D" --remove >/dev/null 2>&1 || fail "backup config --remove"

step "ship 1.0.1, upgrade --version 1.0.1, rollback: no database to dump or check"
python3 - "$P/CHANGELOG.md" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
s = s.replace("## [1.0.0]", "## [1.0.1]\n\n### Changed\n- Release 1.0.1.\n\n## [1.0.0]", 1)
open(p, "w").write(s)
PY
"${S[@]}" ship --channel stable 2>"$WORK/ship-1.0.1.log" || { cat "$WORK/ship-1.0.1.log"; fail "ship 1.0.1"; }
[[ $(page version) == 1.0.0 ]] || fail "the install does not run 1.0.0"
"${CTL[@]}" upgrade --version 1.0.1 --dir "$D" --no-input >"$WORK/upgrade.log" 2>&1 || { cat "$WORK/upgrade.log"; fail "upgrade --version 1.0.1"; }
[[ $(page version) == 1.0.1 && $(page) == salut ]] || { stack_logs >&2; fail "the upgrade does not run 1.0.1 with the recorded input"; }
! grep -qi 'database\|snapshot\|schema' "$WORK/upgrade.log" || { cat "$WORK/upgrade.log"; fail "the upgrade of a release without a database names one"; }
[[ ! -d $D/snapshots ]] || fail "the upgrade dumped something"
"${CTL[@]}" rollback --dir "$D" >"$WORK/rollback.log" 2>&1 || { cat "$WORK/rollback.log"; fail "rollback"; }
[[ $(page version) == 1.0.0 && $(page) == salut ]] || { stack_logs >&2; fail "the rollback does not run 1.0.0 with the recorded input"; }
! grep -qi 'schema\|unchecked' "$WORK/rollback.log" || { cat "$WORK/rollback.log"; fail "the rollback of a release without a database checked a schema"; }

step "reset-db is refused: the release has no database"
if "${CTL[@]}" reset-db --dir "$D" --yes >"$WORK/reset.log" 2>&1; then cat "$WORK/reset.log"; fail "reset-db passed"; fi
grep -qF 'lookup 1.0.0 has no database: reset-db has nothing to reset' "$WORK/reset.log" || { cat "$WORK/reset.log"; fail "reset-db's refusal does not say the release has no database"; }
[[ $(page) == salut ]] || fail "the refused reset-db changed the stack"

step "an install by stowage $OLDV upgrades with this one's, and goes back by its own installer"
remove_install
D=$WORK/stowage-e2e-install-$OLDV # removed at exit in its turn
CTL=("${OPERATOR[@]}" "$D/lookup-stow")
login_pull
docker run --rm --pull always "$INSTALLER0" >"$WORK/lookup-0.9.0.sh" || fail "docker run of the 0.9.0 installer image"
unset DOCKER_CONFIG # the installer's environment never has it (OPERATOR)
(cd "$WORK" && "${OPERATOR[@]}" sh lookup-0.9.0.sh --no-input --dir "$D" --set GREETING=hola) \
	>"$WORK/install-0.9.0.log" 2>&1 || { cat "$WORK/install-0.9.0.log"; fail "install 0.9.0"; }
[[ $(page version) == 0.9.0 && $(page) == hola ]] || { stack_logs >&2; fail "the install does not run 0.9.0"; }
cp "$D/lookup-stow" "$WORK/lookup-stow-0.9.0"
"${CTL[@]}" upgrade --version 1.0.1 --dir "$D" --no-input >"$WORK/upgrade-0.9.0.log" 2>&1 || { cat "$WORK/upgrade-0.9.0.log"; fail "upgrade from 0.9.0 to 1.0.1"; }
[[ $(page version) == 1.0.1 && $(page) == hola ]] || { stack_logs >&2; fail "the upgrade from 0.9.0 does not run 1.0.1 with the recorded input"; }
"${CTL[@]}" rollback --dir "$D" >"$WORK/rollback-0.9.0.log" 2>&1 || { cat "$WORK/rollback-0.9.0.log"; fail "rollback to 0.9.0"; }
[[ $(page version) == 0.9.0 && $(page) == hola ]] || { stack_logs >&2; fail "the rollback does not run 0.9.0 with the recorded input"; }
grep -qF "starting lookup 0.9.0 (revision 1) with its own installer: $D/lookup-stow --dir $D --no-input --no-provision" "$WORK/rollback-0.9.0.log" ||
	{ cat "$WORK/rollback-0.9.0.log"; fail "the rollback did not start 0.9.0 with its own installer"; }
cmp -s "$D/lookup-stow" "$WORK/lookup-stow-0.9.0" || fail "the install does not keep 0.9.0's installer, which stowage $OLDV built"


step "a front terminating TLS for the L4 route alone (no HTTP route, no portal): internal, then byo, its CA published"
remove_install
D=$WORK/stowage-e2e-install-tls # removed at exit in its turn
CTL=("${OPERATOR[@]}" "$D/lookup-stow")
python3 - "$P/stowage.yaml" "$P/deploy/compose.yaml" "$P/CHANGELOG.md" <<'PY2'
import sys
spec, compose, changelog = sys.argv[1:]
for path, edits in [
    (spec, [
        ("upstream: server:8080 }", "upstream: server:8080, tls: { domain_input: LOOKUP_DOMAIN } }"),
        ("\nhealth:\n", "\nfront:\n  tls_modes: [internal, byo, acme, dns-cloudflare]\n  publish_ca: { volume: ca }\n\nhealth:\n"),
    ]),
    (compose, [
        ("      - state:/state\n", "      - state:/state\n      - ca:/ca:ro\n"),
        ("  state: {}\n", "  state: {}\n  ca: {}\n"),
    ]),
    (changelog, [("## [1.0.1]", "## [1.0.2]\n\n### Changed\n- The front terminates the route's TLS.\n\n## [1.0.1]")]),
]:
    s = open(path).read()
    for old, new in edits:
        assert s.count(old) == 1, old
        s = s.replace(old, new)
    open(path, "w").write(s)
PY2
"${S[@]}" lint || fail "an L4 route's TLS as the front's only work is refused"
"${S[@]}" ship --channel stable 2>"$WORK/ship-1.0.2.log" || { cat "$WORK/ship-1.0.2.log"; fail "ship 1.0.2"; }
login_pull
docker run --rm --pull always "$REG/notes/lookup-installer:latest" >"$WORK/lookup-tls.sh" || fail "docker run of the 1.0.2 installer image"
unset DOCKER_CONFIG # the installer's environment never has it (OPERATOR)
# Three distinct ports: free_port releases the one it found, and the L4 route's is free again.
HTTP_PORT=$(free_port)
while [[ $HTTP_PORT == "$PORT" ]]; do HTTP_PORT=$(free_port); done
HTTPS_PORT=$(free_port)
while [[ $HTTPS_PORT == "$PORT" || $HTTPS_PORT == "$HTTP_PORT" ]]; do HTTPS_PORT=$(free_port); done
(cd "$WORK" && "${OPERATOR[@]}" sh lookup-tls.sh --no-input --dir "$D" --set GREETING=secure --tls internal \
	--domains lookup=lookup.test --http-port "$HTTP_PORT" --https-port "$HTTPS_PORT") >"$WORK/install-tls.log" 2>&1 ||
	{ cat "$WORK/install-tls.log"; fail "install 1.0.2 with --tls internal"; }
# Hints (tuning, timers) may follow the line.
grep -qx "lookup 1.0.2 is running" "$WORK/install-tls.log" || { cat "$WORK/install-tls.log"; fail "the install without a portal does not end as a project without a front does"; }
grep -q "L4 route lookup: tcp/$PORT .*lookup.test" "$WORK/install-tls.log" || { cat "$WORK/install-tls.log"; fail "the install does not name the L4 route and its domain"; }
# tls_get CA: the page through the front's TLS on the L4 port, trusting CA alone. curl offers h2:
# the front selects no ALPN protocol, so it speaks HTTP/1.1, as the upstream does.
tls_get() { curl -sf --max-time 10 --cacert "$1" --resolve "lookup.test:$PORT:127.0.0.1" "https://lookup.test:$PORT/"; }
# tls_why CA: what curl saw, for a failure.
tls_why() { curl -sv --max-time 10 --cacert "$1" --resolve "lookup.test:$PORT:127.0.0.1" "https://lookup.test:$PORT/" 2>&1 | tail -20 >&2 || true; }
published_ca() { docker exec "$(server)" cat /ca/ca.crt; }
for _ in $(seq 30); do published_ca >"$WORK/internal-ca.crt" 2>/dev/null && break; sleep 1; done
[[ -s $WORK/internal-ca.crt ]] || { stack_logs >&2; fail "the front did not publish its local CA into the ca volume"; }
[[ $(tls_get "$WORK/internal-ca.crt") == secure ]] || { stack_logs >&2; tls_why "$WORK/internal-ca.crt"; fail "the L4 route does not serve over the front's TLS, chained to the published local CA"; }
! curl -sf --max-time 5 "http://127.0.0.1:$PORT/" >/dev/null || fail "the L4 route answers in the clear"
# A handshake completed and verified, with no protocol selected (a refused one prints no verify code).
alpn=$(timeout 10 openssl s_client -connect "127.0.0.1:$PORT" -servername lookup.test -CAfile "$WORK/internal-ca.crt" -alpn h2,http/1.1 </dev/null 2>&1) || true
grep -q '^No ALPN negotiated' <<<"$alpn" && grep -q 'Verify return code: 0 (ok)' <<<"$alpn" ||
	{ echo "$alpn" >&2; fail "the front's handshake on the L4 route failed, or selected an ALPN protocol for a route declaring none"; }
# byo: a CA of the test's own and its certificate for the domain.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj /CN=lookup-e2e-ca \
	-keyout "$WORK/byo-ca.key" -out "$WORK/byo-ca.crt" 2>/dev/null || fail "a byo CA"
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=lookup.test -keyout "$WORK/byo.key" -out "$WORK/byo.csr" 2>/dev/null &&
	openssl x509 -req -in "$WORK/byo.csr" -CA "$WORK/byo-ca.crt" -CAkey "$WORK/byo-ca.key" -days 2 \
		-extfile <(printf 'subjectAltName=DNS:lookup.test') -out "$WORK/byo.crt" 2>/dev/null || fail "a byo certificate"
"${CTL[@]}" --dir "$D" --no-input --tls byo --cert "$WORK/byo.crt" --key "$WORK/byo.key" --ca "$WORK/byo-ca.crt" >"$WORK/rerun-byo.log" 2>&1 ||
	{ cat "$WORK/rerun-byo.log"; fail "re-run with --tls byo"; }
grep -qx "lookup 1.0.2 is running" "$WORK/rerun-byo.log" || { cat "$WORK/rerun-byo.log"; fail "the byo re-run does not end as a project without a front does"; }
[[ $(tls_get "$WORK/byo-ca.crt") == secure ]] || { stack_logs >&2; tls_why "$WORK/byo-ca.crt"; fail "the L4 route does not serve the byo certificate"; }
# The front copies its CA in the background once Caddy runs: the internal root may still be there.
for _ in $(seq 30); do cmp -s <(published_ca 2>/dev/null) "$WORK/byo-ca.crt" && break; sleep 1; done
cmp -s <(published_ca) "$WORK/byo-ca.crt" || fail "the front did not publish the byo CA"

echo PASS
