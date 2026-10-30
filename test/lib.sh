# Shared by the end-to-end scripts (sourced): a work directory, zot with htpasswd accounts and
# per-repository access control (the reference setup of docs/DESIGN.md § 5.2), and a copy of
# examples/minimal that publishes to it, signed with a cosign key.
#
# After sourcing: REPO, STOWAGE, WORK (on disk, never under /tmp: image exports can be large),
# step / fail / expect_code. start_zot sets REG and PULL_TOKEN (the account pull's) and exports
# STOWAGE_PUSH_TOKEN, NOTES_PULL_TOKEN (pull's) and STOWAGE_PULL_TOKEN (atlas-pull's, a token of its
# own); new_project <postgres ref> (notes) or new_full <postgres ref> (atlas) sets P (the project),
# S (stowage -C P), NAME, and the account login_pull and digest_of read with: PULL_USER and
# PULL_TOKEN (new_full switches them to atlas-pull's).
# k8s_tools and start_kind (after start_zot) set up the Kubernetes suites: pinned kind, helm and
# kubectl, a cluster pulling from zot, OP and OPERATOR, ctl, installer and cluster_logs.
# E2E_LOGS=<dir> keeps the run's logs there. A script adds its own cleanup to CLEANUP (commands run in order at exit, before WORK goes);
# then test/sweep.sh removes what earlier runs left.

. "$(dirname "${BASH_SOURCE[0]}")/pins.sh"

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
STOWAGE=${STOWAGE:-$REPO/bin/stowage}
CACHE=${XDG_CACHE_HOME:-$HOME/.cache}
mkdir -p "$CACHE" # a fresh machine (a CI runner) has none
WORK=$(mktemp -d "$CACHE/stowage-e2e.XXXXXX")
echo $$ >"$WORK/.pid" # this run's: test/sweep.sh leaves a live run's work directory alone
ZOT_NAME=stowage-e2e-zot-$$
export XDG_CACHE_HOME=$WORK/cache
CLEANUP=()

cleanup() {
	local c
	for c in "${CLEANUP[@]}"; do eval "$c" || true; done
	if [[ -n ${E2E_LOGS:-} ]]; then mkdir -p "$E2E_LOGS" && cp "$WORK"/*.log "$E2E_LOGS"/ 2>/dev/null || true; fi
	docker rm -f "$ZOT_NAME" >/dev/null 2>&1 || true
	rm -rf "$WORK"
	# What earlier runs left: nothing accumulates from run to run.
	bash "$REPO/test/sweep.sh" "$CACHE"
}
trap cleanup EXIT

step() { printf '\n== %s  [%dm%02ds]\n' "$*" $((SECONDS / 60)) $((SECONDS % 60)); } # the time since the suite began
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
expect_code() { # <want> <curl args…>
	local want=$1 got
	shift
	got=$(curl -s -o /dev/null -w '%{http_code}' "$@")
	[[ $got == "$want" ]] || fail "curl $* → $got, want $want"
}
token() { head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }

# tool <name> <url> <sha256> [tar member]: links $BIN/<name> to $TOOLS/<sha256>/<name>, downloaded
# once and moved into place by a rename (another member of the same archive may be there).
TOOLS=$CACHE/stowage-tools
BIN=$WORK/bin
mkdir -p "$BIN"
tool() {
	local d=$TOOLS/$3 tmp
	if [[ ! -x $d/$1 ]]; then
		mkdir -p "$TOOLS"
		tmp=$(mktemp -d "$TOOLS/.dl.XXXXXX")
		CLEANUP+=("rm -rf '$tmp'") # a failed download goes too
		curl -fsSL "$2" -o "$tmp/download" || fail "download $2"
		echo "$3  $tmp/download" | sha256sum -c --quiet - || fail "$2: the digest is not $3"
		mkdir "$tmp/out"
		if [[ -n ${4:-} ]]; then
			tar -xzf "$tmp/download" -C "$tmp/out" --strip-components 1 "$4" || fail "unpack $2"
		else
			mv "$tmp/download" "$tmp/out/$1"
		fi
		chmod +x "$tmp/out/$1"
		mkdir -p "$d" && mv -f "$tmp/out/$1" "$d/$1" || fail "install $1 into $d"
		rm -rf "$tmp"
	fi
	ln -sf "$d/$1" "$BIN/$1"
}

# start_dav [ip]: a WebDAV server (rclone's, pinned) the backups go to, its files in $WORK/dav;
# published on ip at a free port, else reached at its container's address on Docker's bridge.
# DAV_CONF is an rclone config whose remote dav reaches it.
start_dav() {
	local url
	mkdir -p "$WORK/dav"
	if [[ -n ${1:-} ]]; then
		local port
		port=$(free_port)
		docker run -d --name "$ZOT_NAME-dav" --user "$(id -u):$(id -g)" -p "$1:$port:8080" -v "$WORK/dav:/data" "$RCLONE" serve webdav /data --addr :8080 >/dev/null
		url=http://$1:$port
	else
		docker run -d --name "$ZOT_NAME-dav" --user "$(id -u):$(id -g)" -v "$WORK/dav:/data" "$RCLONE" serve webdav /data --addr :8080 >/dev/null
		url=http://$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$ZOT_NAME-dav"):8080
	fi
	CLEANUP+=("docker rm -f $ZOT_NAME-dav >/dev/null 2>&1")
	DAV_CONF=$WORK/dav.conf
	printf '[dav]\ntype = webdav\nurl = %s\nvendor = other\n' "$url" >"$DAV_CONF"
	for _ in $(seq 1 50); do docker logs "$ZOT_NAME-dav" 2>&1 | grep -q 'WebDav Server started' && return; sleep 0.2; done
	docker logs "$ZOT_NAME-dav"
	fail "the WebDAV server did not start"
}

# age_key: the pinned age, and a key pair: AGE_KEY its identity file, AGE_RECIPIENT its public key.
AGE_URL=https://github.com/FiloSottile/age/releases/download/v1.3.2/age-v1.3.2-linux-amd64.tar.gz
AGE_SHA=cbe24006683f8eb669266162894b9a522a1af52f2665fbc63a4bb032ed26ac10
age_key() {
	tool age "$AGE_URL" "$AGE_SHA" age/age
	tool age-keygen "$AGE_URL" "$AGE_SHA" age/age-keygen
	AGE_KEY=$WORK/backup.key
	"$BIN/age-keygen" -o "$AGE_KEY" 2>/dev/null || fail "age-keygen"
	AGE_RECIPIENT=$("$BIN/age-keygen" -y "$AGE_KEY")
}

# check_backup <file> <pg image> <table>: the backup decrypts with AGE_KEY into a pg_dump archive
# that holds the table's data.
check_backup() {
	"$BIN/age" -d -i "$AGE_KEY" "$1" >"$WORK/backup.dump" || fail "$1 does not decrypt with the key"
	docker run --rm -i --network none --entrypoint pg_restore "$2" --list <"$WORK/backup.dump" >"$WORK/backup.list" 2>&1 ||
		{ cat "$WORK/backup.list"; fail "pg_restore does not read $1"; }
	grep -q "TABLE DATA public $3 " "$WORK/backup.list" || { cat "$WORK/backup.list"; fail "$1 holds no data of $3"; }
	rm -f "$WORK/backup.dump"
}

# scanner_fresh <prefix> [<project>]: the scanner an audit of the registry prefix loads (the local
# name the installer gives it: the prefix's hash), untagged now (none an earlier run of the same
# address left is taken for a pull) and at the end, and recorded in the work directory for
# test/sweep.sh. By tag alone: another suite's scanner is the same image under another name.
scanner_fresh() {
	local repo
	repo="stowage.local/$(printf %s "$1" | sha256sum | cut -c1-8)/${2:-notes}-audit"
	echo "$repo" >>"$WORK/.scanners"
	untag_repo "$repo"
	CLEANUP+=("untag_repo '$repo'")
}
untag_repo() { docker images --format '{{.Repository}}:{{.Tag}}' "$1" | xargs -r docker rmi >/dev/null 2>&1 || true; }
# grype_db: GRYPE_DB, grype's latest vulnerability database archive, fetched once into the tools
# cache (older ones removed) and checked against the listing's digest; the audits scan with it
# (--db), as a site without access to grype's servers does, so no suite fetches its own.
grype_db() {
	local dir=$TOOLS/grype-db latest path sum tmp
	mkdir -p "$dir"
	latest=$(curl -fsSL https://grype.anchore.io/databases/v6/latest.json) || fail "grype's database listing"
	path=$(jq -r .path <<<"$latest") sum=$(jq -r .checksum <<<"$latest")
	[[ $path == vulnerability-db_*.tar.zst && $sum == sha256:* ]] || fail "grype's listing: $latest"
	GRYPE_DB=$dir/$path
	if [[ ! -s $GRYPE_DB ]]; then
		tmp=$(mktemp "$dir/.dl.XXXXXX")
		CLEANUP+=("rm -f '$tmp'")
		curl -fsSL "https://grype.anchore.io/databases/v6/$path" -o "$tmp" || fail "download $path"
		echo "${sum#sha256:}  $tmp" | sha256sum -c --quiet - || fail "$path: the digest is not $sum"
		mv -f "$tmp" "$GRYPE_DB"
	fi
	find "$dir" -maxdepth 1 -name 'vulnerability-db_*.tar.zst' ! -name "$path" -mmin +60 -delete
}

[[ -x $STOWAGE ]] || fail "$STOWAGE is missing: run make build"

# start_zot runs the project's registry. It takes Docker v2 manifests (a project's pinned image may
# be one) unless ZOT_COMPAT says otherwise: ZOT_COMPAT='' takes OCI manifests alone.
start_zot() {
	local port code
	port=$(free_port)
	REG=127.0.0.1:$port
	PULL_TOKEN=$(token)
	export STOWAGE_PUSH_TOKEN NOTES_PULL_TOKEN=$PULL_TOKEN STOWAGE_PULL_TOKEN
	STOWAGE_PUSH_TOKEN=$(token) STOWAGE_PULL_TOKEN=$(token)
	mkdir -p "$WORK/zot/data"
	# The passwords reach htpasswd on its standard input, one per account, never on a command line.
	printf '%s\n' "$PULL_TOKEN" "$STOWAGE_PULL_TOKEN" "$STOWAGE_PUSH_TOKEN" |
		docker run --rm -i --entrypoint sh "$HTPASSWD" -ec \
			'read -r a; read -r b; read -r c; printf "%s\n" "$a" | htpasswd -niB pull; printf "%s\n" "$b" | htpasswd -niB atlas-pull; printf "%s\n" "$c" | htpasswd -niB publisher' |
		grep . >"$WORK/zot/htpasswd"
	cat >"$WORK/zot/config.json" <<JSON
{
  "storage": { "rootDirectory": "/etc/zot/data" },
  "http": {
    "address": "0.0.0.0", "port": "5000", "compat": [${ZOT_COMPAT-\"docker2s2\"}],
    "auth": { "htpasswd": { "path": "/etc/zot/htpasswd" }, "apikey": true },
    "accessControl": {
      "repositories": {
        "notes/**": {
          "policies": [
            { "users": ["pull"], "actions": ["read"] },
            { "users": ["publisher"], "actions": ["read", "create", "update", "delete"] }
          ],
          "defaultPolicy": []
        },
        "atlas/**": {
          "policies": [
            { "users": ["atlas-pull"], "actions": ["read"] },
            { "users": ["publisher"], "actions": ["read", "create", "update", "delete"] }
          ],
          "defaultPolicy": []
        }
      }
    }
  },
  "extensions": { "search": { "enable": true }, "ui": { "enable": false } },
  "log": { "level": "warn" }
}
JSON
	docker run -d --name "$ZOT_NAME" --user "$(id -u):$(id -g)" -p "$REG:5000" -v "$WORK/zot:/etc/zot" "$ZOT" serve /etc/zot/config.json >/dev/null
	for _ in $(seq 1 50); do
		code=$(curl -s -o /dev/null -w '%{http_code}' "http://$REG/v2/" || true)
		[[ $code == 401 ]] && break
		sleep 0.2
	done
	[[ $code == 401 ]] || { docker logs "$ZOT_NAME" >&2; fail "zot did not come up (last status $code)"; }
}

# start_mirror [address]: a second zot, a site's mirror without internet: HTTPS with a certificate
# its own test CA ($MIRROR_CA) signs for 127.0.0.1 and the address, published on the address
# (default the loopback); anonymous reads, writes by the account site. Sets MIRROR (host:port) and
# MIRROR_CONFIG (a Docker config directory holding the site's login, as `docker login` writes).
start_mirror() {
	local ip=${1:-127.0.0.1} port code m=$WORK/mirror token
	port=$(free_port)
	MIRROR=$ip:$port MIRROR_CA=$m/ca.pem MIRROR_CONFIG=$WORK/mirror-docker
	token=$(token)
	mkdir -p "$m/data" "$MIRROR_CONFIG"
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj /CN=stowage-e2e-mirror-ca \
		-keyout "$m/ca.key" -out "$m/ca.pem" 2>/dev/null
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=mirror -keyout "$m/tls.key" -out "$m/tls.csr" 2>/dev/null
	printf 'subjectAltName=IP:127.0.0.1,IP:%s\nextendedKeyUsage=serverAuth\n' "$ip" >"$m/ext"
	openssl x509 -req -in "$m/tls.csr" -CA "$m/ca.pem" -CAkey "$m/ca.key" -CAcreateserial -days 2 -extfile "$m/ext" -out "$m/tls.crt" 2>/dev/null
	rm -f "$m/ca.key"
	docker run --rm "$HTPASSWD" htpasswd -nbB site "$token" | grep . >"$m/htpasswd"
	cat >"$m/config.json" <<JSON
{
  "storage": { "rootDirectory": "/etc/zot/data" },
  "http": {
    "address": "0.0.0.0", "port": "5000", "compat": ["docker2s2"],
    "tls": { "cert": "/etc/zot/tls.crt", "key": "/etc/zot/tls.key" },
    "auth": { "htpasswd": { "path": "/etc/zot/htpasswd" } },
    "accessControl": {
      "repositories": {
        "**": {
          "anonymousPolicy": ["read"],
          "policies": [{ "users": ["site"], "actions": ["read", "create", "update", "delete"] }],
          "defaultPolicy": []
        }
      }
    }
  },
  "log": { "level": "warn" }
}
JSON
	docker run -d --name "$ZOT_NAME-mirror" --user "$(id -u):$(id -g)" -p "$MIRROR:5000" -v "$m:/etc/zot" "$ZOT" serve /etc/zot/config.json >/dev/null
	CLEANUP+=("docker rm -f $ZOT_NAME-mirror >/dev/null 2>&1")
	for _ in $(seq 1 50); do
		code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$MIRROR_CA" "https://$MIRROR/v2/" || true)
		[[ $code == 200 || $code == 401 ]] && break
		sleep 0.2
	done
	[[ $code == 200 || $code == 401 ]] || { docker logs "$ZOT_NAME-mirror" >&2; fail "the mirror did not come up (last status $code)"; }
	printf '{"auths":{"%s":{"auth":"%s"}}}' "$MIRROR" "$(printf 'site:%s' "$token" | base64 -w0)" >"$MIRROR_CONFIG/config.json"
}

# mirror_referrers <repo> <tag>: how many referrers (signatures) the mirror holds for the manifest a tag
# names, read anonymously.
mirror_referrers() {
	local d
	d=$(curl -sfI --cacert "$MIRROR_CA" -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json' \
		"https://$MIRROR/v2/$1/manifests/$2" | tr -d '\r' | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: //p')
	[[ -n $d ]] || { echo 0; return; }
	curl -sf --cacert "$MIRROR_CA" "https://$MIRROR/v2/$1/referrers/$d" | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("manifests") or []))'
}

# new_project <postgres ref>: examples/minimal pinned to $STOWAGE, publishing to zot with the token accounts, its
# postgres replaced by the ref given, signed with a fresh cosign key (keys/release.*); a second key
# pair, never trusted, is $WORK/other/other.*. S runs stowage on it as a test release (zot's
# prefix and token accounts, the fresh key, without the kairos shape, whose OS bundle is a long
# build); with KAIROS=1 the spec itself is rewritten so, the kairos shape kept.
new_project() {
	P=$WORK/notes NAME=notes SHORT=notes
	cp -r "$REPO/examples/minimal" "$P"
	cosign_keys "$P/keys/release"
	python3 - "$P/stowage.yaml" "$REG" "$1" "${KAIROS:-0}" <<'PY'
import sys
path, reg, pinned, kairos = sys.argv[1:]
s = open(path).read()
edits = []
if kairos == "1":
    edits = [
        ("prefix: ghcr.io/acme/notes", f"prefix: {reg}/notes"),
        ("pull: { provider: none }", "pull: { provider: token, user: pull, token_env: NOTES_PULL_TOKEN }"),
        ("push: { provider: docker-config }", "push: { provider: token, user: publisher, token_env: STOWAGE_PUSH_TOKEN }"),
    ]
for old, new in edits:
    assert s.count(old) == 1, old
    s = s.replace(old, new)
lines = [l for l in s.splitlines() if not l.lstrip().startswith("postgres: { ref:")]
assert len(lines) == len(s.splitlines()) - 1
i = lines.index("images:")
lines.insert(i + 1, f'  postgres: {{ ref: "{pinned}" }}')
if kairos == "1":
    lines.insert(i, "signing: { provider: cosign-key, key: keys/release.key, public_keys: [keys/release.pub] }")
open(path, "w").write("\n".join(lines) + "\n")
PY
	S=("$STOWAGE" -C "$P")
	if [[ ${KAIROS:-0} != 1 ]]; then
		S+=(--test-registry "$REG/notes" --test-push publisher:STOWAGE_PUSH_TOKEN --test-pull pull:NOTES_PULL_TOKEN
			--test-key "$P/keys/release.key" --test-public-key keys/release.pub --shapes docker,kubernetes)
	fi
	"${S[@]}" lock --update >/dev/null # the example pins a stowage; publish with the one built
	"${S[@]}" lint
}

# cosign_keys <prefix>: STOWAGE_COSIGN, a cosign run in its container on the host network (the
# registry is on loopback), and a fresh key pair <prefix>.key / <prefix>.pub; a second pair, never
# trusted, is $WORK/other/other.*. Every path cosign is handed — the key, the public keys, the
# per-call Docker config under XDG_CACHE_HOME — is under $WORK, mounted at the same path; a key
# given as env://ATLAS_SIGNING_KEY reaches it through its environment.
cosign_keys() {
	export STOWAGE_COSIGN=$WORK/cosign COSIGN_PASSWORD
	COSIGN_PASSWORD=$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')
	cat >"$STOWAGE_COSIGN" <<SH
#!/bin/sh
exec docker run --rm --network host --user $(id -u):$(id -g) -e HOME=/tmp -e DOCKER_CONFIG -e COSIGN_PASSWORD -e ATLAS_SIGNING_KEY -v "$WORK:$WORK" -w "$WORK" $COSIGN "\$@"
SH
	chmod +x "$STOWAGE_COSIGN"
	mkdir -p "$(dirname "$1")" "$WORK/other"
	"$STOWAGE_COSIGN" generate-key-pair --output-key-prefix "$1" >/dev/null 2>&1
	"$STOWAGE_COSIGN" generate-key-pair --output-key-prefix "$WORK/other/other" >/dev/null 2>&1
	[[ -s $1.pub && -s $WORK/other/other.pub ]] || fail "cosign generate-key-pair"
}

# new_full <postgres ref>: examples/full (atlas) as it stands — token pushes and pulls (the pull
# account atlas-pull), signed with the key its environment holds (env://ATLAS_SIGNING_KEY) —
# publishing to zot, its postgres replaced by the ref given and its first release this month's
# (CalVer with month_guard: utc), CALVER being the month (YYYY.M). The kairos shape is dropped
# unless KAIROS=1.
new_full() {
	P=$WORK/atlas NAME=atlas SHORT=at PULL_USER=atlas-pull PULL_TOKEN=$STOWAGE_PULL_TOKEN
	cp -r "$REPO/examples/full" "$P"
	cosign_keys "$WORK/atlas-key/release"
	export ATLAS_SIGNING_KEY
	ATLAS_SIGNING_KEY=$(cat "$WORK/atlas-key/release.key")
	cp "$WORK/atlas-key/release.pub" "$P/keys/release-2026.pub"
	CALVER=$(date -u +%Y.%-m)
	printf '# Changelog\n\n## [%s.1]\n\n### Added\n- The first release.\n' "$CALVER" >"$P/CHANGELOG.md"
	python3 - "$P/stowage.yaml" "$REG" "$1" "${KAIROS:-0}" <<'PY'
import sys
path, reg, pinned, kairos = sys.argv[1:]
s = open(path).read()
old = "prefix: registry.example.com/atlas"
assert s.count(old) == 1, old
s = s.replace(old, f"prefix: {reg}/atlas")
lines = s.splitlines()
pg = [i for i, l in enumerate(lines) if l.startswith("  postgres:  { ref:")]
assert len(pg) == 1, pg
lines[pg[0]] = f'  postgres:  {{ ref: "{pinned}" }}'
if kairos != "1":
    k = lines.index("  kairos:")
    end = k + 1
    while end < len(lines) and lines[end].startswith("    "):
        end += 1
    del lines[k:end]
open(path, "w").write("\n".join(lines) + "\n")
PY
	S=("$STOWAGE" -C "$P")
	"${S[@]}" lock --update >/dev/null
	"${S[@]}" lint
}

# full_release <micro> <migration file> <sql>: this month's release <micro>, with a migration, shipped
# on stable (after new_full).
full_release() {
	local v=$CALVER.$1
	printf '%s\n' "$3" >"$P/migrations/$2"
	python3 - "$P/CHANGELOG.md" "$v" <<'PY'
import sys
path, v = sys.argv[1:]
s = open(path).read()
head = "# Changelog\n\n"
assert s.startswith(head)
open(path, "w").write(head + f"## [{v}]\n\n### Changed\n- Release {v}.\n\n" + s[len(head):])
PY
	"${S[@]}" ship --channel stable 2>"$WORK/ship-$v.log" || { cat "$WORK/ship-$v.log"; fail "ship $v"; }
}

# helm <args…>: the pinned helm, on the host network, with $WORK mounted at the same path.
helm() {
	docker run --rm --network host --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$WORK:$WORK" -w "$WORK" "$HELM" "$@"
}

# digest_of <repo:tag>: the digest a tag of the registry holds, read with the project's pull account.
digest_of() {
	curl -sfI -u "${PULL_USER:-pull}:$PULL_TOKEN" \
		-H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
		"http://$REG/v2/${1%:*}/manifests/${1##*:}" | tr -d '\r' | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: //p'
}

# login_pull: a private Docker config holding the project's pull account (PULL_USER), as an
# operator's login would.
login_pull() {
	export DOCKER_CONFIG=$WORK/docker
	mkdir -p "$DOCKER_CONFIG"
	printf '{"auths":{"%s":{"auth":"%s"}}}' "$REG" "$(printf '%s:%s' "${PULL_USER:-pull}" "$PULL_TOKEN" | base64 -w0)" >"$DOCKER_CONFIG/config.json"
}

# An operator's environment: none of the publisher's or the test's credentials (an allowlist), so
# an installer's pulls and fetches read the pull account baked into it. The engine selection
# passes, so the installer reaches the engine the test's own docker calls do.
OPERATOR=(env -i "PATH=$PATH" "HOME=$HOME")
for v in DOCKER_HOST DOCKER_CONTEXT DOCKER_TLS_VERIFY DOCKER_CERT_PATH; do
	if [[ -n ${!v:-} ]]; then OPERATOR+=("$v=${!v}"); fi
done

# The stack of an install in $D, reached on $APP_PORT and $API_PORT (e2e-docker, e2e-kairos).
# project: the install's compose project, empty before its .env exists.
project() { [[ -f $D/.env ]] && sed -n "s/^COMPOSE_PROJECT_NAME=['\"]\{0,1\}\([^'\"]*\).*/\1/p" "$D/.env"; }
stack_logs() { # the stack's containers and their last lines
	local c p
	p=$(project) || return 0
	for c in $(docker ps -aq --filter "label=com.docker.compose.project=$p"); do
		docker ps -a --filter "id=$c" --format '--- {{.Names}} ({{.Status}})'
		docker logs --tail 50 "$c" 2>&1 || true
	done
}
# remove_install, at exit (CLEANUP): with E2E_LOGS, the stack's logs into $WORK/stack.log; then the stack, its volumes and
# network, and the images pulled from this run's registry, by name: an image ID would also take
# the host's own tags of the same image (the mirrored postgres).
remove_install() {
	local p l
	if p=$(project) && [[ -n $p ]]; then
		[[ -z ${E2E_LOGS:-} ]] || stack_logs >"$WORK/stack.log" 2>&1
		l=label=com.docker.compose.project=$p
		docker ps -aq --filter "$l" | xargs -r docker rm -f >/dev/null
		docker volume ls -q --filter "$l" | xargs -r docker volume rm >/dev/null
		docker network ls -q --filter "$l" | xargs -r docker network rm >/dev/null
	fi
	[[ -n ${REG:-} ]] || return 0
	docker images -q --filter "reference=$REG/${NAME:-notes}/*" | sort -u |
		xargs -r docker image inspect --format '{{range .RepoTags}}{{println .}}{{end}}{{range .RepoDigests}}{{println .}}{{end}}' |
		grep -F "$REG/${NAME:-notes}/" | sort -u | xargs -r docker rmi >/dev/null 2>&1 || true
}

kept() { # <version>: the installer the install in $D keeps (CTL) is that release's
	local h
	h=$("${CTL[@]}" --help 2>&1) || true
	grep -qF "$NAME $1 with Docker" <<<"$h" || { printf '%s\n' "$h" | head -5 >&2; fail "the kept ${SHORT}-stow is not $1's"; }
}

running() { # <version> <schema>: the API answers as that release, on that migration, and the app route serves
	# The example's API and web have no healthcheck: the stack is up before they serve.
	python3 - "$API_PORT" "$APP_PORT" "$1/$2" <<'PY' || { stack_logs >&2; fail "the stack does not run $1 on migration $2"; }
import json, sys, time, urllib.request
api, app, want = sys.argv[1:]
def get(port, path):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=2) as r:
        return r.read()
deadline = time.monotonic() + 15
while True:
    try:
        got = f'{json.loads(get(api, "/healthz"))["version"]}/{json.loads(get(api, "/schema"))["version"]}'
        last = f"the API answers {got} (version/migration), want {want}"
        if got == want:
            last = "the app route serves nothing"
            if get(app, "/"):
                sys.exit(0)
    except Exception as e:
        last = repr(e)
    if time.monotonic() > deadline:
        sys.exit(last)
    time.sleep(0.2)
PY
}

# Kubernetes (e2e-k8s, e2e-full-k8s): a kind cluster that pulls from this run's zot, and the k8s
# installer run as an operator would.
KIND_URL=https://github.com/kubernetes-sigs/kind/releases/download/v0.33.0/kind-linux-amd64
KIND_SHA=aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d
NODE=kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
HELM_URL=https://get.helm.sh/helm-v4.0.4-linux-amd64.tar.gz
HELM_SHA=29454bc351f4433e66c00f5d37841627cbbcc02e4c70a6d796529d355237671c
KUBECTL_URL=https://dl.k8s.io/release/v1.37.1/bin/linux/amd64/kubectl
KUBECTL_SHA=65691ff77eb6fa44c908b77a1082c9f092c3b9733b5cefabec0d1104890e21a8

# k8s_tools: the pinned kind, helm (the binary, not the containerised helm above) and kubectl
# first on PATH, KUBECONFIG in $WORK, and the operator: OP, its working directory (where `upgrade
# --version` fetches an installer), and OPERATOR, an environment that holds none of the
# publisher's or the test's credentials (an allowlist), with its own home for helm's caches.
k8s_tools() {
	tool kind "$KIND_URL" "$KIND_SHA"
	tool helm "$HELM_URL" "$HELM_SHA" linux-amd64/helm
	tool kubectl "$KUBECTL_URL" "$KUBECTL_SHA"
	export PATH=$BIN:$PATH KUBECONFIG=$WORK/kubeconfig
	unset -f helm
	[[ $(helm version --template '{{.Version}}') == v4.0.4 ]] || fail "helm is not the pinned one"
	OP=$WORK/operator
	mkdir -p "$OP/home"
	OPERATOR=(env -i "PATH=$PATH" "HOME=$OP/home" "KUBECONFIG=$KUBECONFIG")
}

# POD_SYSCTLS: what `k8s install --unsafe-sysctls` gives the pods, which the kind node's kubelet
# allows (`k8s tune` must list each).
POD_SYSCTLS=(net.core.somaxconn net.ipv4.tcp_mtu_probing net.ipv4.tcp_notsent_lowat net.ipv4.tcp_slow_start_after_idle)

# start_kind (after start_zot): a kind cluster on a network of its own, which zot joins; the
# node's containerd maps the registry's loopback address to zot there (a plain-HTTP mirror). zot
# serves no anonymous pull, so every running pod proves the pull Secret the installer wrote. Its
# kubelet allows POD_SYSCTLS.
start_kind() {
	local pid
	CLUSTER=stowage-e2e-$$ NET=stowage-e2e-$$
	CLEANUP+=('remove_cluster')
	docker network create "$NET" >/dev/null
	docker network connect "$NET" "$ZOT_NAME"
	mkdir -p "$WORK/certs.d/$REG"
	cat >"$WORK/certs.d/$REG/hosts.toml" <<TOML
server = "http://$REG"
[host."http://$ZOT_NAME:5000"]
  capabilities = ["pull", "resolve"]
TOML
	cat >"$WORK/kind.yaml" <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
  - |-
    [plugins."io.containerd.cri.v1.images".registry]
      config_path = "/etc/containerd/certs.d"
kubeadmConfigPatches:
  - |
    kind: KubeletConfiguration
    allowedUnsafeSysctls: [$(IFS=,; echo "${POD_SYSCTLS[*]}")]
nodes:
  - role: control-plane
    image: $NODE
    extraMounts:
      - { hostPath: "$WORK/certs.d", containerPath: /etc/containerd/certs.d, readOnly: true }
YAML
	# A host without /dev/kmsg (the kubelet needs it) gets a link to the console in the node as
	# soon as the node exists; a no-op elsewhere.
	KIND_EXPERIMENTAL_PROVIDER=docker KIND_EXPERIMENTAL_DOCKER_NETWORK=$NET \
		kind create cluster --name "$CLUSTER" --config "$WORK/kind.yaml" --wait 150s >"$WORK/kind.log" 2>&1 &
	pid=$!
	while kill -0 "$pid" 2>/dev/null; do
		docker exec "$CLUSTER-control-plane" sh -c 'test -e /dev/kmsg || ln -sf /dev/console /dev/kmsg' 2>/dev/null || true
		sleep 2
	done
	wait "$pid" || { cat "$WORK/kind.log"; fail "kind create cluster"; }
}
# cluster_logs: what runs in the namespace NS, and the history of the release NAME there (notes
# before a project is made).
cluster_logs() {
	kubectl get all,secrets -n "$NS" -o wide 2>&1 || true
	kubectl describe pods -n "$NS" 2>&1 | tail -60 || true
	helm history "${NAME:-notes}" -n "$NS" 2>&1 || true
}
# remove_cluster, at exit (CLEANUP): with E2E_LOGS, the cluster's state into $WORK/cluster.log;
# then the cluster, its network (zot leaves it first) and the installer images pulled from this
# run's registry.
remove_cluster() {
	[[ -z ${E2E_LOGS:-} ]] || cluster_logs >"$WORK/cluster.log" 2>&1
	kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	docker network disconnect -f "$NET" "$ZOT_NAME" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	[[ -n ${REG:-} ]] || return 0
	docker images --format '{{.Repository}}:{{.Tag}}' --filter "reference=$REG/${NAME:-notes}/*" | xargs -r docker rmi >/dev/null 2>&1 || true
}
# prev_stowage: the path of the previous release's stowage — the latest v* tag but this tree's
# own version — built from that tag with its own `make build` (in Docker) once, then kept in
# ~/.cache/stowage-prev/<tag>; its git worktree is removed once built.
prev_stowage() (
	# A subshell: its EXIT trap cleans the build up however it ends, and leaves the suite's own.
	local cur tag dir out
	cur=$(sed -n 's/^## \[\([^]]*\)\].*/\1/p' "$REPO/CHANGELOG.md" | head -n1)
	tag=$(git -C "$REPO" tag --list 'v*' --sort=-v:refname | { grep -vxF "v$cur" || true; } | head -n1)
	[[ -n $tag ]] || fail "no previous release tag"
	out=$HOME/.cache/stowage-prev/$tag/stowage
	if [[ ! -x $out ]]; then
		mkdir -p "$HOME/.cache" || fail "no $HOME/.cache"
		dir=$(mktemp -d "$HOME/.cache/stowage-prev-tree.XXXXXX") || fail "mktemp in $HOME/.cache"
		# The tree, its log and a half-kept binary go whichever way the build ends.
		trap 'git -C "$REPO" worktree remove --force "$dir" >/dev/null 2>&1; rm -rf "$dir" "$dir.log" "$out.tmp"' EXIT
		git -C "$REPO" worktree add --detach "$dir" "$tag" >&2 || fail "worktree of $tag"
		# Its binary alone: the tag's checks ran when it was released, and a vulnerability
		# published since says nothing of this tree.
		sed -i '/^COPY --from=checks /d' "$dir/Dockerfile"
		# Its output goes to stderr: the caller takes the path from stdout.
		make -C "$dir" build >"$dir.log" 2>&1 || { cat "$dir.log" >&2; fail "build of $tag"; }
		mkdir -p "$(dirname "$out")"
		{ cp "$dir/bin/stowage" "$out.tmp" && mv "$out.tmp" "$out"; } || fail "keeping the build of $tag in $out"
	fi
	printf '%s\n' "$out"
)
# ctl <installer script> <args…>: the bootstrap script, run as the operator in $OP.
ctl() {
	local s=$1
	shift
	(cd "$OP" && "${OPERATOR[@]}" sh "$s" "$@")
}
# installer <file>: the latest installer of the registry, as the bootstrap one-liner fetches it.
installer() {
	login_pull
	docker run --rm --pull always "$REG/$NAME/$NAME-installer:latest" >"$OP/$1" || fail "docker run of the installer image"
	unset DOCKER_CONFIG # for the next ships; the installer's environment never has it (OPERATOR)
}
