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
# made again) → status lists the L4 route → --tls is refused → ship 1.0.1, `upgrade --version
# 1.0.1` (nothing dumped) → rollback (no schema check) → reset-db is refused: the release has no
# database → an install of 0.9.0, which the last release's stowage (built from its tag) shipped
# before 1.0.0, upgrades to 1.0.1 with this one's, and a rollback goes back to it with its own
# installer.
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
[[ -z $(docker ps -q --filter "label=com.docker.compose.project=$(sed -n 's/^COMPOSE_PROJECT_NAME=//p' "$D/.env")" --filter label=com.docker.compose.service=stowage-front) ]] ||
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

echo PASS
