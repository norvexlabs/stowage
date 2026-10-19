#!/usr/bin/env bash
# Records docs/img/install.gif: examples/minimal shipped to a throwaway zot (as test/e2e-docker.sh
# does), then its installer run in a terminal by VHS (install.tape) against the Docker engine of
# /var/run/docker.sock. Needs Docker, ./bin/stowage (make build) and free loopback ports 8080 and
# 8000 (the example's defaults). The operator's home is /var/tmp/operator, so the paths the
# installer prints name no one's home; the terminal sees it at the same path as the engine, which
# mounts the install's files from it. The GIF is copied only once the install it shows is running.
#
#   bash docs/demo/record.sh
set -euo pipefail
PG=postgres:17-alpine@sha256:aa90e97ee862e558111d34cfb8b2c4bec768c2b039fb791341686928560263b3
VHS=stowage-demo-vhs
DEMO=$(cd "$(dirname "$0")" && pwd)
. "$DEMO/../../test/lib.sh"

for port in 8080 8000; do
	python3 -c 'import socket,sys; socket.socket().bind(("127.0.0.1", int(sys.argv[1])))' "$port" 2>/dev/null ||
		fail "127.0.0.1:$port is in use: the example's loopback ports must be free"
done
OP=/var/tmp/operator
mkdir -m 0700 "$OP" 2>/dev/null || fail "$OP exists: remove it first"
mkdir "$OP/out"
D=$OP/notes
CLEANUP+=('remove_install' "rm -rf '$OP'" "docker rmi $VHS >/dev/null 2>&1")

step "zot, ship examples/minimal 1.0.0"
start_zot
new_project "$PG"
"${S[@]}" ship --channel stable 2>"$WORK/ship.log" || { cat "$WORK/ship.log"; fail "ship"; }
login_pull # the installer image's pull, in the tape's hidden setup only
unset DOCKER_CONFIG

step "record"
docker build -q -t "$VHS" "$DEMO" >/dev/null
sed "s|@REG@|$REG|g; s|@PULL_CONFIG@|$WORK/docker|g" "$DEMO/install.tape" >"$WORK/demo.tape"
docker run --rm --network host --user "$(id -u):$(id -g)" --group-add "$(stat -c %g /var/run/docker.sock)" \
	-v /var/run/docker.sock:/var/run/docker.sock -v "$WORK:$WORK" -v "$OP:$OP" -w "$OP" -e HOME="$OP" \
	"$VHS" "$WORK/demo.tape"

step "check the install the recording shows"
[[ ! -e $D/.stowage-installing ]] || fail "the install did not finish: $(cat "$D/.stowage-installing")"
"${OPERATOR[@]}" "$D/notesctl" status --dir "$D" >"$WORK/status.log" 2>&1 || { cat "$WORK/status.log"; fail "status"; }
expect_code 200 http://127.0.0.1:8080/
cp "$OP/out/install.gif" "$REPO/docs/img/install.gif"
echo "PASS: docs/img/install.gif"
