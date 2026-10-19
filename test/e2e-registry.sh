#!/usr/bin/env bash
# End-to-end test of the publisher against a real registry: zot with htpasswd accounts and
# per-repository access control (the reference setup of docs/DESIGN.md § 5.2), and real image
# builds with the local Docker engine. Needs Docker and network access (base images).
#
#   make e2e        (builds ./bin/stowage first)
#
# Walks examples/minimal, signed with a cosign key, through: ship a mutable channel twice (the
# second run changes nothing), verify (and refuse a key the release was not signed with), promote
# to the immutable channel, the bootstrap one-liner on the installer image :latest, the access
# rules (anonymous refused, the pull account reads but cannot write), a refused rewrite under an
# immutable tag, yank, releases, prune. The Helm chart is pulled with the pull account and rendered
# by helm, its images by digest. The front is built from Stowage's recipe and published with
# the images. It also checks zot's API keys work with htpasswd logins
# alone.
set -euo pipefail

# A small multi-platform image stands in for the example's postgres: mirroring copies every platform.
PINNED=busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
. "$(dirname "$0")/lib.sh"

step "start zot"
start_zot

step "project"
new_project "$PINNED"

step "ship beta (builds)"
"${S[@]}" ship --channel beta 2>"$WORK/ship1.log" || { cat "$WORK/ship1.log"; fail "ship"; }
grep -q 'image api: build' "$WORK/ship1.log" || fail "api was not built"
grep -q 'image postgres: mirror' "$WORK/ship1.log" || fail "postgres was not mirrored"
grep -q 'image front: build' "$WORK/ship1.log" || fail "the front was not built"
grep -q 'installer: push' "$WORK/ship1.log" || fail "the installer was not pushed"
grep -q 'chart: tag .* → charts/notes-beta:0.1.0' "$WORK/ship1.log" || fail "the chart was not pushed"
[[ $(grep -c '^signed ' "$WORK/ship1.log") == 7 ]] || { cat "$WORK/ship1.log"; fail "want 7 signatures (3 images, the front, the chart, the installer, the release manifest)"; }

step "ship beta again (nothing changes)"
"${S[@]}" ship --channel beta 2>"$WORK/ship2.log" || { cat "$WORK/ship2.log"; fail "ship"; }
cat "$WORK/ship2.log"
! grep -qE 'image [a-z]+: (build|mirror|tag )|installer[: ]|chart: |published \(seq|^signed ' "$WORK/ship2.log" || fail "a second ship wrote something"
grep -q 'release beta-1.0.0 unchanged' "$WORK/ship2.log" || fail "release not reported unchanged"

step "registry.pull none, and the registry refuses anonymous reads: ship stops before it builds"
cp "$P/stowage.yaml" "$WORK/stowage.yaml.saved"
sed -i 's/pull: { provider: token, user: pull, token_env: NOTES_PULL_TOKEN }/pull: { provider: none }/' "$P/stowage.yaml"
grep -q 'pull: { provider: none }' "$P/stowage.yaml" || fail "registry.pull was not set to none"
if "${S[@]}" ship --channel beta 2>"$WORK/private.log"; then fail "ship published what an install cannot read"; fi
grep -q "every install reads $REG/notes anonymously, and .*$REG/notes/notes-api.* refuse an anonymous read" "$WORK/private.log" ||
	{ cat "$WORK/private.log"; fail "the refusal does not name the private repositories"; }
! grep -qE 'image [a-z]+: (build|mirror)' "$WORK/private.log" || fail "ship built before it refused"
cp "$WORK/stowage.yaml.saved" "$P/stowage.yaml"

step "verify, promote, verify"
# Read-only: with the pull account only (zot grants it read, nothing else).
env -u STOWAGE_PUSH_TOKEN "${S[@]}" verify --channel beta
# Negative control: the same release against a key it was not signed with.
cp "$P/keys/release.pub" "$WORK/release.pub.saved"
cp "$WORK/other/other.pub" "$P/keys/release.pub"
if env -u STOWAGE_PUSH_TOKEN "${S[@]}" verify --channel beta 2>"$WORK/badkey.log"; then fail "verify accepted a release signed with another key"; fi
grep -q 'no signature by a trusted key' "$WORK/badkey.log" || { cat "$WORK/badkey.log"; fail "wrong-key refusal"; }
cp "$WORK/release.pub.saved" "$P/keys/release.pub"
"${S[@]}" promote 1.0.0 --from beta --to stable 2>"$WORK/promote.log" || { cat "$WORK/promote.log"; fail "promote"; }
! grep -qE 'image [a-z]+: (build|mirror)' "$WORK/promote.log" || fail "promote rebuilt or re-mirrored"
"${S[@]}" verify --channel stable
grep -q 'installer :latest → 1.0.0' "$WORK/promote.log" || { cat "$WORK/promote.log"; fail "the installer :latest did not move with stable"; }

step "bootstrap one-liner (the installer image prints the script; the script runs the installer)"
login_pull
docker run --rm --pull always "$REG/notes/notes-installer:latest" >"$WORK/notes.sh" || fail "docker run of the installer image"
head -1 "$WORK/notes.sh" | grep -q '^#!/bin/sh' || fail "the installer image did not print a script"
(cd "$WORK" && sh notes.sh --help) >"$WORK/help.txt" || { cat "$WORK/help.txt"; fail "the script did not run the installer"; }
grep -q 'Install and manage notes 1.0.0 with Docker' "$WORK/help.txt" || { cat "$WORK/help.txt"; fail "installer help"; }
compgen -G "$WORK/.notes-stow.*" >/dev/null && fail "the script left its extracted installer behind"
# The front shipped is the pinned Caddy, with the Cloudflare DNS plugin.
docker run --rm --pull always "$REG/notes/notes-front:1.0.0" sh -c 'caddy version; caddy list-modules' >"$WORK/front.txt" 2>&1 || { cat "$WORK/front.txt"; fail "docker run of the front"; }
grep -q '^v2\.11\.7 ' "$WORK/front.txt" && grep -qx 'dns.providers.cloudflare' "$WORK/front.txt" || { cat "$WORK/front.txt"; fail "the front is not Caddy 2.11.7 with the Cloudflare plugin"; }

step "the chart: pulled with the pull account, rendered by helm with the release's images by digest"
mkdir -p "$WORK/chart"
helm pull "oci://$REG/notes/charts/notes-stable" --version 0.1.0 --plain-http --registry-config "$DOCKER_CONFIG/config.json" -d "$WORK/chart" >/dev/null || fail "helm pull of the stable chart"
tgz=$(compgen -G "$WORK/chart/*.tgz") || fail "helm pull wrote no archive"
helm show chart "$tgz" >"$WORK/chart.txt"
grep -qx 'name: notes' "$WORK/chart.txt" && grep -qx 'appVersion: 1.0.0' "$WORK/chart.txt" && grep -qx 'version: 0.1.0' "$WORK/chart.txt" || { cat "$WORK/chart.txt"; fail "chart metadata"; }
helm template notes "$tgz" >"$WORK/rendered.yaml" || fail "helm template"
web=$(digest_of notes/notes-web:1.0.0)
[[ -n $web ]] || fail "no digest for notes-web:1.0.0"
grep -q "image: $REG/notes/notes-web@$web\$" "$WORK/rendered.yaml" || { cat "$WORK/rendered.yaml"; fail "the rendered chart does not run notes-web@$web"; }
unset DOCKER_CONFIG

step "access rules"
ACCEPT='Accept: application/vnd.oci.image.manifest.v1+json'
expect_code 401 -H "$ACCEPT" "http://$REG/v2/notes/notes-api/manifests/1.0.0"
expect_code 200 -u "pull:$PULL_TOKEN" -H "$ACCEPT" "http://$REG/v2/notes/notes-api/manifests/1.0.0"
expect_code 401 -H "$ACCEPT" "http://$REG/v2/notes/charts/notes-stable/manifests/0.1.0"
expect_code 403 -u "pull:$PULL_TOKEN" -X POST "http://$REG/v2/notes/notes-api/blobs/uploads/"
expect_code 403 -u "pull:$PULL_TOKEN" -X DELETE "http://$REG/v2/notes/notes-api/manifests/1.0.0"

step "an immutable tag refuses new inputs"
echo '<!-- changed -->' >>"$P/web/index.html"
if "${S[@]}" ship --channel stable 2>"$WORK/refused.log"; then fail "stable accepted new inputs under 1.0.0"; fi
grep -q -- '--revision 2' "$WORK/refused.log" || { cat "$WORK/refused.log"; fail "refusal does not name --revision 2"; }

step "yank"
"${S[@]}" yank 1.0.0 --channel stable --reason "e2e" 2>"$WORK/yank.log" || { cat "$WORK/yank.log"; fail "yank"; }
grep -q '^signed .*notes-release@' "$WORK/yank.log" || { cat "$WORK/yank.log"; fail "the yanked manifest was not signed"; }
env -u STOWAGE_PUSH_TOKEN "${S[@]}" verify 1.0.0 --channel stable >/dev/null
if "${S[@]}" verify --channel stable 2>/dev/null; then fail "stable still has a pointer after yanking its only release"; fi
expect_code 404 -u "pull:$PULL_TOKEN" -H "$ACCEPT" "http://$REG/v2/notes/notes-installer/manifests/latest"
"${S[@]}" releases | tee "$WORK/releases.txt"
grep -qE '^stable +1\.0\.0 .* e2e$' "$WORK/releases.txt" || fail "yank not listed"

step "prune beta (tag deletion keeps the stable tag on the same digest)"
[[ $("${S[@]}" prune --channel beta 1.0.0) == beta-1.0.0 ]] || fail "prune output"
expect_code 404 -u "pull:$PULL_TOKEN" -H "$ACCEPT" "http://$REG/v2/notes/notes-api/manifests/beta-1.0.0"
expect_code 404 -u "pull:$PULL_TOKEN" -H "$ACCEPT" "http://$REG/v2/notes/notes-installer/manifests/beta-1.0.0"
expect_code 404 -u "pull:$PULL_TOKEN" -H "$ACCEPT" "http://$REG/v2/notes/notes-front/manifests/beta-1.0.0"
expect_code 404 -u "pull:$PULL_TOKEN" -H "$ACCEPT" "http://$REG/v2/notes/charts/notes-beta/manifests/0.1.0"
expect_code 200 -u "pull:$PULL_TOKEN" -H "$ACCEPT" "http://$REG/v2/notes/notes-api/manifests/1.0.0"

step "zot API keys with htpasswd logins (docs/DESIGN.md § 5.2 relies on it)"
resp=$(curl -s -u "publisher:$STOWAGE_PUSH_TOKEN" -H 'Content-Type: application/json' \
	-d '{"label":"e2e","scopes":["notes/**"]}' "http://$REG/zot/auth/apikey" || true)
key=$(printf '%s' "$resp" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("apiKey",""))
except Exception: print("")')
if [[ -n $key ]] && [[ $(curl -s -o /dev/null -w '%{http_code}' -u "publisher:$key" "http://$REG/v2/notes/notes-api/tags/list") == 200 ]]; then
	echo "zot API keys: work with htpasswd logins (no OpenID needed)"
else
	fail "zot API keys: not usable with htpasswd alone ($resp)"
fi

printf '\nPASS\n'
