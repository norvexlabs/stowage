#!/usr/bin/env bash
# End-to-end test of the Kairos VM shape's publication: the OS bundle and its disk images, built for
# real (kairos-init, auroraboot, qemu-img) and published to zot. Needs Docker (privileged
# containers, loop devices), zstd, network access and about 30 GB on disk under XDG_CACHE_HOME.
#
#   make e2e-kairos        (builds ./bin/stowage first)
#   STOWAGE_CLOUD_ENV=<file> make e2e-kairos    (also registers the clouds' images: the file sets
#                          a cloud's STOWAGE_<CLOUD>_* settings and credentials; needs tofu)
#
# Walks examples/minimal, with a real Postgres, two disk formats and the three clouds' (raw, vhd,
# gce), through: ship a mutable
# channel (the bundle preloads every image, named by digest, and `docker load` takes each archive;
# each disk is compressed, decompressed with zstd -d and checked in its format, the clouds' as each cloud imports it), the bundle's own notes-stow run on this host's Docker as on
# the VM (install from the preloaded images with the cloud-config's settings, boot, the host daemon
# and its app socket, a backup through the preloaded runner to a WebDAV server, decrypted with the
# key), a second ship that builds nothing, promote to the immutable channel
# (re-tagged, with latest-<format> following it), `vm create` from the published notes-stow (the
# disk by digest, the seed ISO), verify, prune (a channel's disk set stays); then ship 2.0.0 (a migration): booting its notes-stow
# upgrades the install, and booting 1.0.0's again (the A/B fallback) goes back.
set -euo pipefail

# The amd64 image of the example's postgres, as e2e-docker.
PG=postgres:17-alpine@sha256:aa90e97ee862e558111d34cfb8b2c4bec768c2b039fb791341686928560263b3
FORMATS=(raw qcow2 ova vhd gce)
. "$(dirname "$0")/lib.sh"
command -v zstd >/dev/null || fail "zstd is needed (the disks are checked decompressed)"

D=$WORK/stowage-e2e-vm
CLEANUP+=('remove_install')
# The api service mounts the host daemon's app directory, /run/notes/app, which Docker creates on
# this host: removed at exit (empty directories only), as root, by a container.
CLEANUP+=("docker run --rm --network none -v /run:/r --entrypoint rmdir $PG /r/notes/app /r/notes >/dev/null 2>&1")

# The clouds the publisher has settings for (STOWAGE_CLOUD_ENV, the user's file, never in the
# tree): ship registers their images, which are destroyed at exit; the others are left out.
CLOUDS=(aws azure gcp)
declare -A CLOUD_KEY=([aws]=STOWAGE_AWS_BUCKET [azure]=STOWAGE_AZURE_STORAGE_ACCOUNT [gcp]=STOWAGE_GCP_BUCKET)
if [[ -n ${STOWAGE_CLOUD_ENV:-} ]]; then
	set -a
	# shellcheck disable=SC1090
	. "$STOWAGE_CLOUD_ENV"
	set +a
fi
# destroy_images: every image ship registered, with its upload, through its own OpenTofu state.
destroy_images() {
	local d f v
	# As ship runs it: an empty OpenTofu configuration, no Stowage or registry secret.
	: >"$WORK/tofurc"
	# env takes its -u options before the assignment.
	local -a env=(env -u PULL_TOKEN -u NOTES_PULL_TOKEN)
	for v in $(compgen -e | grep -E '^(STOWAGE_|COSIGN_|TF_)'); do env+=(-u "$v"); done
	env+=(TF_CLI_CONFIG_FILE="$WORK/tofurc")
	# cloud/<cloud>/<settings digest>/<image>/, and those set aside to prove the probe.
	# Their uploads too, which a run stopped part way leaves.
	for d in "$XDG_CACHE_HOME"/stowage/cloud{,-aside}/*/*/*/ "$XDG_CACHE_HOME"/stowage/cloud{,-aside}/*/*/*/upload/; do
		[[ -f $d/terraform.tfstate ]] || continue
		# An upload's disk is gone (and moved, for a directory set aside): destroy reads only its
		# path. The providers come again from the module's lock file.
		# A run stopped mid-apply leaves its lock: nothing else runs here now.
		f=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("file", ""))' "$d/terraform.tfvars.json")
		[[ -z $f ]] || { mkdir -p "$(dirname "$f")" && touch "$f"; }
		"${env[@]}" tofu -chdir="$d" init -input=false -no-color >"$WORK/destroy.log" 2>&1 &&
			"${env[@]}" tofu -chdir="$d" destroy -lock=false -auto-approve -input=false -no-color >"$WORK/destroy.log" 2>&1 || { tail -20 "$WORK/destroy.log"; echo "WARNING: $d not destroyed: remove its image by hand" >&2; }
	done
}
CLEANUP+=('destroy_images')

step "start zot"
start_zot

step "project, with the kairos shape, two disk formats and three clouds"
KAIROS=1 new_project "$PG"
sed -i 's/disk_formats: \[qcow2\]/disk_formats: [qcow2, ova]\n    cloud_publish: [aws, azure, gcp]/' "$P/stowage.yaml"
grep -q 'cloud_publish: \[aws, azure, gcp\]' "$P/stowage.yaml" || fail "disk formats not set"
# The disk-pressure hook at 1%: the host daemon posts as it starts (the api answers 501, and logs it).
sed -i 's|^    env: \[ADMIN_EMAIL\]$|&\n  maintenance:\n    disk_pressure: { soft_pct: 1, hard_pct: 99, post: "http://api:8000/maintenance/disk-pressure", auth_secret: SESSION_SECRET }|' "$P/stowage.yaml"
grep -q 'disk_pressure: { soft_pct: 1,' "$P/stowage.yaml" || fail "disk pressure not set"
"${S[@]}" lint

step "ship beta: images, the OS bundle, its disks"
"${S[@]}" ship --channel beta --disks 2>"$WORK/ship1.log" || { tail -50 "$WORK/ship1.log"; fail "ship"; }
grep -q 'os bundle: build 1.0.0' "$WORK/ship1.log" || fail "the bundle was not built"
grep -q 'disks: make raw, qcow2, ova, vhd, gce' "$WORK/ship1.log" || fail "the disks were not made"
for f in "${FORMATS[@]}"; do
	grep -q "disk $f: tag .* → beta-1.0.0-$f" "$WORK/ship1.log" || fail "disk $f was not tagged"
done
[[ $(grep -c '^signed ' "$WORK/ship1.log") == 16 ]] || { grep '^signed ' "$WORK/ship1.log"; fail "want 16 signatures (9, the bundle, 5 disks, the channel's disk set)"; }
for c in "${CLOUDS[@]}"; do
	if [[ -n ${!CLOUD_KEY[$c]:-} ]]; then
		grep -q "cloud $c: register notes-[0-9a-f]\{12\}$" "$WORK/ship1.log" && grep -q "cloud $c: image ." "$WORK/ship1.log" || { tail -30 "$WORK/ship1.log"; fail "the $c image was not registered"; }
		grep "cloud $c: image " "$WORK/ship1.log"
		# The upload is gone once the image is made: its state holds nothing.
		for d in "$XDG_CACHE_HOME"/stowage/cloud/"$c"/*/*/upload; do
			[[ -z $(tofu -chdir="$d" state list 2>&1) ]] || { tofu -chdir="$d" state list; fail "the $c upload was not deleted"; }
		done
	else
		grep -q "cloud $c: not uploaded (set " "$WORK/ship1.log" || fail "the $c image is not reported left out"
	fi
done

pull_auth=(-u "pull:$PULL_TOKEN")
ACCEPT='Accept: application/vnd.oci.image.manifest.v1+json'
manifest() { curl -sf "${pull_auth[@]}" -H "$ACCEPT" "http://$REG/v2/notes/$1/manifests/$2"; }
release=$(manifest notes-release beta-1.0.0)
blob=$(printf '%s' "$release" | python3 -c 'import json,sys; print(json.load(sys.stdin)["layers"][0]["digest"])')
curl -sf "${pull_auth[@]}" "http://$REG/v2/notes/notes-release/blobs/$blob" >"$WORK/release.json"
art() { python3 -c 'import json,sys; a=json.load(open(sys.argv[1]))["artifacts"]; print(eval(sys.argv[2]))' "$WORK/release.json" "$1"; }
os_digest=$(art 'a["os"]')
[[ $os_digest == sha256:* ]] || fail "the release records no os bundle"
[[ $(digest_of "notes/notes-os:beta-1.0.0") == "$os_digest" ]] || fail "notes-os:beta-1.0.0 is not the release's bundle"

step "the bundle: Kairos, the OS version, every image preloaded and loadable"
login_pull
docker pull -q "$REG/notes/notes-os:beta-1.0.0" >/dev/null
ctr=$(docker create "$REG/notes/notes-os:beta-1.0.0" /bin/true)
CLEANUP+=("docker rm -f $ctr >/dev/null 2>&1")
docker cp "$ctr:/etc/notes/os-version" "$WORK/os-version"
[[ $(cat "$WORK/os-version") == 1.0.0 ]] || fail "os-version is $(cat "$WORK/os-version")"
docker cp "$ctr:/etc/kairos-release" "$WORK/kairos-release"
grep -q '^KAIROS_FLAVOR="ubuntu"' "$WORK/kairos-release" || fail "not a Kairos image"
docker cp "$ctr:/etc/systemd/system/notes-tune.service" "$WORK/tune.service" && grep -q '^ExecStart=/usr/sbin/notes-stow tune-boot$' "$WORK/tune.service" ||
	fail "the bundle does not tune the network at boot"
[[ $(docker cp "$ctr:/etc/systemd/system/multi-user.target.wants/notes-tune.service" - | tar -tvf - 2>/dev/null) == *"-> /etc/systemd/system/notes-tune.service"* ]] ||
	fail "the tuning unit is not enabled"
docker cp "$ctr:/usr/sbin/ethtool" "$WORK/ethtool" || fail "the bundle has no ethtool"
docker cp "$ctr:/usr/share/notes-preload" "$WORK/preload"
for img in api web postgres front backup chown; do
	tar="$WORK/preload/$img.tar"
	[[ -s $tar ]] || fail "no archive for $img"
	digest=$(art "a['images']['$img']")
	ref="$REG/notes/notes-$img:${digest/:/-}"
	docker load --input "$tar" >"$WORK/load-$img.log" || { cat "$WORK/load-$img.log"; fail "docker load of $img"; }
	CLEANUP+=("docker image rm -f $ref >/dev/null")
	id=$(docker image inspect --format '{{.Id}}' "$ref") || fail "$ref is not loaded"
	want_m=$(art "a['manifests']['$img']")
	want_c=$(art "a['configs']['$img']")
	[[ $id == "$want_m" || $id == "$want_c" || $id == "$digest" ]] || fail "$img loaded as $id, the release has $want_m"
done
docker cp "$ctr:/usr/sbin/notes-stow" "$WORK/os-1.0.0-ctl"
docker rm -f "$ctr" >/dev/null
docker image rm -f "$REG/notes/notes-os:beta-1.0.0" >/dev/null

step "the bundle's notes-stow installs from the preloaded images, as on the VM; boot brings it up again"
APP_PORT=$(free_port)
API_PORT=$(free_port)
while [[ $API_PORT == "$APP_PORT" ]]; do API_PORT=$(free_port); done
VM1=("${OPERATOR[@]}" "$WORK/os-1.0.0-ctl")
# The settings a VM's cloud-config writes give the channel and the input; the flags the rest.
printf 'channel: beta\ninputs:\n  ADMIN_EMAIL: admin@notes.test\n' >"$WORK/install.yaml"
"${VM1[@]}" install --dir "$D" --settings "$WORK/install.yaml" --no-input --tls none --ports "app=$APP_PORT,api=$API_PORT" \
	>"$WORK/vm-install.log" 2>&1 || { cat "$WORK/vm-install.log"; fail "install from the bundle"; }
running 1.0.0 1
grep -q "^STOWAGE_IMAGE_API=.*/notes-api:sha256-" "$D/.env" || { cat "$D/.env"; fail "the install does not run the preloaded api"; }
grep -qx "STOWAGE_CHANNEL='beta'" "$D/.env" || fail "the channel is not recorded"
grep -qx "ADMIN_EMAIL='admin@notes.test'" "$D/.env" || fail "the settings' input is not recorded"
"${VM1[@]}" boot --dir "$D" >"$WORK/vm-boot.log" 2>&1 || { cat "$WORK/vm-boot.log"; fail "boot"; }
running 1.0.0 1

step "the host daemon answers notes-stow"
SOCK=$WORK/hostd/host.sock
"${VM1[@]}" hostd --dir "$D" --socket "$SOCK" --group "" >"$WORK/hostd.log" 2>&1 &
CLEANUP+=("kill $! 2>/dev/null")
for _ in $(seq 1 50); do [[ -S $SOCK ]] && break; sleep 0.1; done
[[ -S $SOCK ]] || { cat "$WORK/hostd.log"; fail "the host daemon did not start"; }
vmctl() { "${VM1[@]}" "$@" --socket "$SOCK"; }
vmctl status >"$WORK/status.txt" || fail "status"
grep -q '^installed  notes 1.0.0 (revision 1)' "$WORK/status.txt" && grep -q '^channel    beta' "$WORK/status.txt" ||
	{ cat "$WORK/status.txt"; fail "status does not show the install"; }
vmctl ps | grep -q '^postgres .*running (healthy)' || fail "ps does not show a healthy postgres"
vmctl logs api --tail 5 >/dev/null || fail "logs"
vmctl restart web | grep -q 'web: restart done' || fail "restart"
running 1.0.0 1
vmctl updates | grep -q 'no upgrade on beta' || fail "updates found an upgrade"

step "disk pressure: the host daemon posts the level to the api, on the stack's network, with the install's token"
for _ in $(seq 1 100); do grep -q 'disk pressure soft' "$WORK/hostd.log" && break; sleep 0.2; done
grep -q 'disk pressure soft, .* used: POST http://api:8000/maintenance/disk-pressure: answered 501' "$WORK/hostd.log" ||
	{ cat "$WORK/hostd.log"; fail "the host daemon did not post the disk pressure"; }
api=$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=api)
# Docker copies a container's output to its log on its own time: the line may trail the answer.
# The log is read to a file first: `grep -q` quitting at its match would fail `docker logs`, still
# writing, and so the pipeline (pipefail).
posted() { docker logs "$api" >"$WORK/api.log" 2>&1; grep -q '"POST /maintenance/disk-pressure HTTP/1.1" 501' "$WORK/api.log"; }
for _ in $(seq 1 50); do posted && break; sleep 0.2; done
posted || { tail -20 "$WORK/api.log"; fail "the api was not posted the disk pressure"; }
session=$(sed -n "s/^SESSION_SECRET='\{0,1\}\([0-9a-f]*\).*/\1/p" "$D/.env")
[[ ${#session} == 64 ]] || fail "no session secret recorded"
! grep -qF "$session" "$WORK/hostd.log" || fail "the host daemon logged the token"
vmctl status >"$WORK/status.txt" || fail "status"
grep -q '^pressure   soft: .* used (soft 1%, hard 99%)$' "$WORK/status.txt" && grep -q 'last POST at .*: answered 501' "$WORK/status.txt" ||
	{ cat "$WORK/status.txt"; fail "status does not show the disk pressure"; }

step "the app socket serves the api service the daemon, with the install's token"
APP_SOCK=$WORK/hostd/app/app.sock
[[ -S $APP_SOCK ]] || fail "no app socket"
host_token=$(sed -n "s/^STOWAGE_HOST_TOKEN='\{0,1\}\([0-9a-f]*\).*/\1/p" "$D/.env")
[[ ${#host_token} == 64 ]] || fail "no host token recorded"
app_get() { curl -s -o /dev/null -w '%{http_code}' --unix-socket "$APP_SOCK" "$@" http://host/v1/status; }
[[ $(app_get) == 401 ]] || fail "the app socket answered without the token"
[[ $(app_get -H "Authorization: Bearer x$host_token") == 401 ]] || fail "the app socket took another token"
[[ $(app_get -H "Authorization: Bearer $host_token") == 200 ]] || fail "the app socket refused the install's token"
api=$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=api)
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$api" >"$WORK/api-env.txt"
grep -qx "STOWAGE_HOST_TOKEN=$host_token" "$WORK/api-env.txt" && grep -qx "STOWAGE_HOST_SOCKET=/run/stowage-host/app.sock" "$WORK/api-env.txt" ||
	fail "the api service is not given the token and the socket"
docker inspect --format '{{range .Mounts}}{{.Source}}:{{.Destination}}:{{.RW}}{{println}}{{end}}' "$api" | grep -qx '/run/notes/app:/run/stowage-host:false' ||
	fail "the api service does not mount the app directory read-only"
web=$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=web)
! docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$web" | grep -q STOWAGE_HOST_TOKEN || fail "the web service is given the token"

step "backups: set over notes-stow, taken by the host daemon (the dump encrypted, uploaded by the preloaded runner), decrypted with the key"
age_key
start_dav
pg=$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=postgres)
docker exec "$pg" psql -U notes -d notes -c 'CREATE TABLE e2e_backup (n int); INSERT INTO e2e_backup VALUES (1)' >/dev/null || fail "a table to back up"
vmctl backup config --destination dav:backups --rclone-config "$DAV_CONF" --recipient "$AGE_RECIPIENT" >"$WORK/backup-config.txt" 2>&1 ||
	{ cat "$WORK/backup-config.txt"; fail "backup config"; }
grep -q '^rclone       set (not shown)' "$WORK/backup-config.txt" && ! grep -q 'webdav' "$WORK/backup-config.txt" ||
	{ cat "$WORK/backup-config.txt"; fail "backup config does not mask the rclone config"; }
vmctl backup verify >"$WORK/backup-verify.txt" 2>&1; grep -q 'reachable and writable' "$WORK/backup-verify.txt" || { cat "$WORK/backup-verify.txt"; fail "backup verify"; }
vmctl backup run >"$WORK/backup-run.txt" 2>&1 || { cat "$WORK/backup-run.txt"; fail "backup run"; }
backups=("$WORK"/dav/backups/notes-full-*-1.0.0-r1.pgdump.age)
[[ ${#backups[@]} == 1 && -s ${backups[0]} ]] || { ls -la "$WORK/dav/backups"; fail "no backup in the destination"; }
check_backup "${backups[0]}" "$PG" e2e_backup
[[ -s ${backups[0]%.pgdump.age}.uploads.tar.age ]] || { ls -la "$WORK/dav/backups"; fail "no archive of the uploads volume beside the backup"; }
vmctl backup list | grep -q "^${backups[0]##*/} " || fail "backup list does not show the backup"
vmctl backup status | grep -q "^last  .*${backups[0]##*/}" || fail "backup status does not show the backup"

step "the disks: each pulled with the pull account and checked in its format"
tools=$(docker image ls --format '{{.Repository}}:{{.Tag}}' stowage-disktools | head -1)
[[ -n $tools ]] || fail "no qemu-img image left by the disk builder"
mkdir -p "$WORK/disks"
for f in "${FORMATS[@]}"; do
	d=$(art "a['disks']['$f']")
	[[ $(digest_of "notes/notes-disk:beta-1.0.0-$f") == "$d" ]] || fail "notes-disk:beta-1.0.0-$f is not the release's"
	m=$(manifest notes-disk "$d")
	read -r layer title type mt sum <<<"$(printf '%s' "$m" | python3 -c 'import json,sys; m=json.load(sys.stdin); l=m["layers"][0]
print(l["digest"], l["annotations"]["org.opencontainers.image.title"], m["artifactType"], l["mediaType"], l["annotations"]["dev.stowage.uncompressed.digest"])')"
	[[ $type == application/vnd.stowage.disk.v1 ]] || fail "disk $f artifact type $type"
	[[ $mt == "application/vnd.stowage.disk.$f+zstd" && $title == *.zst ]] || fail "disk $f is not compressed: $mt $title"
	curl -sf "${pull_auth[@]}" -o "$WORK/disks/$title" "http://$REG/v2/notes/notes-disk/blobs/$layer" || fail "download of disk $f"
	# As the docs say: zstd -d gives the disk, of the digest the layer's annotation names.
	zstd -d -q --rm "$WORK/disks/$title" || fail "zstd -d of disk $f"
	[[ "sha256:$(sha256sum "$WORK/disks/${title%.zst}" | cut -d' ' -f1)" == "$sum" ]] || fail "disk $f decompressed is not $sum"
done
run_tools() { docker run --rm --user "$(id -u):$(id -g)" -v "$WORK/disks:/w" -w /w "$tools" "$@"; }
run_tools qemu-img check notes-1.0.0.qcow2 >/dev/null || fail "qemu-img check of the qcow2"
run_tools qemu-img info -f vpc notes-1.0.0.vhd | grep -q 'file format: vpc' || fail "the vhd is not a VHD"
# Azure imports a fixed VHD of a whole number of MiB; Google Cloud a tar.gz whose disk.raw is a
# whole number of GiB.
python3 - "$WORK/disks/notes-1.0.0.vhd" "$WORK/disks/notes-1.0.0.gce.tar.gz" <<'PY' || fail "a cloud's disk is not as it imports it"
import os, struct, sys, tarfile
vhd, gce = sys.argv[1:]
with open(vhd, "rb") as f:
    f.seek(-512, 2)
    foot = f.read(512)
size, = struct.unpack(">Q", foot[48:56])
kind, = struct.unpack(">I", foot[60:64])
assert foot[:8] == b"conectix" and kind == 2, ("vhd footer", foot[:8], kind)
assert size % (1 << 20) == 0 and os.path.getsize(vhd) == size + 512, ("vhd size", size)
with tarfile.open(gce, "r:gz") as t:
    m = t.getmembers()
assert [x.name for x in m] == ["disk.raw"] and m[0].size % (1 << 30) == 0, [(x.name, x.size) for x in m]
assert m[0].size >= size, ("gce smaller than the vhd", m[0].size, size)
PY
run_tools sh -c 'od -An -c -j 512 -N 8 notes-1.0.0.raw' | tr -d ' ' | grep -q 'EFIPART' || fail "the raw disk has no GPT"
tar tf "$WORK/disks/notes-1.0.0.ova" >"$WORK/ova.txt"
[[ $(cat "$WORK/ova.txt") == $'notes-1.0.0.ovf\nnotes-1.0.0-disk1.vmdk' ]] || { cat "$WORK/ova.txt"; fail "the OVA is not the descriptor then the disk"; }
rm -rf "$WORK/disks"

step "ship beta again (nothing is built)"
"${S[@]}" ship --channel beta 2>"$WORK/ship2.log" || { cat "$WORK/ship2.log"; fail "ship"; }
! grep -qE 'os bundle: (build|tag)|disks: make|disk [a-z0-9]+: (push|tag)|^signed ' "$WORK/ship2.log" || { cat "$WORK/ship2.log"; fail "a second ship wrote something"; }
grep -q 'release beta-1.0.0 unchanged' "$WORK/ship2.log" || fail "release not reported unchanged"
! grep -q 'cloud [a-z]*: register' "$WORK/ship2.log" || fail "a second ship registered an image again"
for c in "${CLOUDS[@]}"; do
	[[ -z ${!CLOUD_KEY[$c]:-} ]] || [[ $(grep "cloud $c: image " "$WORK/ship2.log") == $(grep "cloud $c: image " "$WORK/ship1.log") ]] || fail "the $c image differs on the second ship"
done
if [[ -d $XDG_CACHE_HOME/stowage/cloud ]]; then
	step "ship beta from an empty cloud cache: each image is found in its account, not registered again"
	mv "$XDG_CACHE_HOME/stowage/cloud" "$XDG_CACHE_HOME/stowage/cloud-aside"
	"${S[@]}" ship --channel beta 2>"$WORK/ship2b.log" || { tail -30 "$WORK/ship2b.log"; fail "ship"; }
	! grep -q 'cloud [a-z]*: register' "$WORK/ship2b.log" || fail "an image in its account was registered again"
	for c in "${CLOUDS[@]}"; do
		[[ -z ${!CLOUD_KEY[$c]:-} ]] || [[ $(grep "cloud $c: image " "$WORK/ship2b.log") == $(grep "cloud $c: image " "$WORK/ship1.log") ]] || { grep "cloud $c" "$WORK/ship2b.log"; fail "the $c image was not found in its account"; }
	done
fi

step "promote to stable: the bundle and the disks are re-tagged, latest-<format> follows"
"${S[@]}" promote 1.0.0 --from beta --to stable 2>"$WORK/promote.log" || { cat "$WORK/promote.log"; fail "promote"; }
! grep -qE 'os bundle: build|disks: make' "$WORK/promote.log" || fail "promote built"
[[ $(digest_of "notes/notes-os:1.0.0") == "$os_digest" ]] || fail "notes-os:1.0.0 is not the beta bundle"
for f in "${FORMATS[@]}"; do
	d=$(art "a['disks']['$f']")
	[[ $(digest_of "notes/notes-disk:1.0.0-$f") == "$d" ]] || fail "notes-disk:1.0.0-$f is not the beta disk"
	[[ $(digest_of "notes/notes-disk:latest-$f") == "$d" ]] || fail "notes-disk:latest-$f does not follow stable"
done
env -u STOWAGE_PUSH_TOKEN "${S[@]}" verify --channel stable

step "vm create from the published notes-stow: the qcow2 disk by digest, and the seed ISO"
login_pull
docker pull -q "$REG/notes/notes-installer:latest" >/dev/null
ctr=$(docker create "$REG/notes/notes-installer:latest")
CLEANUP+=("docker rm -f $ctr >/dev/null 2>&1")
docker cp "$ctr:/notes-stow" "$WORK/notes-stow-stable"
docker rm -f "$ctr" >/dev/null
docker image rm -f "$REG/notes/notes-installer:latest" >/dev/null
python3 -c 'import base64,os,struct,sys
s=lambda b: struct.pack(">I", len(b)) + b
print("ssh-ed25519 " + base64.b64encode(s(b"ssh-ed25519") + s(os.urandom(32))).decode() + " e2e")' >"$WORK/key.pub"
VMC=$WORK/vm-create
"${OPERATOR[@]}" "$WORK/notes-stow-stable" vm create --no-input --platform truenas --out "$VMC" --hostname notes-e2e --user ops \
	--ssh-key "$WORK/key.pub" --tls internal --domains app=app.notes.test,api=api.notes.test --set ADMIN_EMAIL=admin@notes.test \
	>"$WORK/vm-create.txt" 2>"$WORK/vm-create.log" || { cat "$WORK/vm-create.log"; fail "vm create"; }
grep -q '^On TrueNAS:' "$WORK/vm-create.txt" || { cat "$WORK/vm-create.txt"; fail "vm create printed no steps"; }
d=$(art "a['disks']['qcow2']")
# The disk decompressed: the file of the digest the layer's annotation names.
file=$(manifest notes-disk "$d" | python3 -c 'import json,sys; print(json.load(sys.stdin)["layers"][0]["annotations"]["dev.stowage.uncompressed.digest"])')
[[ sha256:$(sha256sum "$VMC/notes-1.0.0.qcow2" | cut -d' ' -f1) == "$file" ]] || fail "the qcow2 written is not the release's"
python3 - "$VMC/notes-e2e-seed.iso" <<'PY' || fail "the seed ISO"
import sys
iso = open(sys.argv[1], "rb").read()
assert iso[0x8001:0x8006] == b"CD001", "not ISO 9660"
assert iso[0x8028:0x8048] == b"cidata".ljust(32), iso[0x8028:0x8048]
for want in [b"#cloud-config", b"ssh-ed25519 ", b"/etc/notes/install.yaml", b"channel: stable", b"tls: internal", b"APP_DOMAIN: app.notes.test", b"instance-id: notes-e2e", b"user-data", b"meta-data"]:
    assert want in iso, want
PY
rm -rf "$VMC"

step "prune beta: its bundle tag goes, its disks (beta's disk set) and stable's stay"
[[ $("${S[@]}" prune --channel beta 1.0.0) == beta-1.0.0 ]] || fail "prune output"
expect_code 404 "${pull_auth[@]}" -H "$ACCEPT" "http://$REG/v2/notes/notes-os/manifests/beta-1.0.0"
expect_code 200 "${pull_auth[@]}" -H "$ACCEPT" "http://$REG/v2/notes/notes-disk/manifests/beta-1.0.0-qcow2"
[[ $(digest_of "notes/notes-os:1.0.0") == "$os_digest" ]] || fail "prune took stable's bundle"
env -u STOWAGE_PUSH_TOKEN "${S[@]}" verify --channel stable

step "ship 2.0.0 (a migration); booting its notes-stow upgrades the install"
sed -i 's/disk_formats: \[qcow2, ova\]/disk_formats: [qcow2]/; /cloud_publish:/d' "$P/stowage.yaml"
grep -q 'disk_formats: \[qcow2\]$' "$P/stowage.yaml" && ! grep -q cloud_publish "$P/stowage.yaml" || fail "disk formats not reset"
printf '2.0.0\n' >"$P/VERSION"
printf '### Changed\n- Release 2.0.0.\n' >"$P/changes/2.0.0.md"
printf '%s\n' '{"version": 2, "barriers": {}, "reads_newer": true}' >"$P/api/schema.json"
"${S[@]}" ship --channel beta 2>"$WORK/ship3.log" || { tail -50 "$WORK/ship3.log"; fail "ship 2.0.0"; }
# Without --disks: no disk made or moved; 2.0.0's VMs boot 1.0.0's.
! grep -qE 'disks: make|disk [a-z0-9]+: (push|tag)|disk .* removed' "$WORK/ship3.log" || { cat "$WORK/ship3.log"; fail "a ship without --disks touched a disk"; }
grep -q "no disk published: a VM of this release boots" "$WORK/ship3.log" || { cat "$WORK/ship3.log"; fail "2.0.0 does not record the disk set it boots"; }
vmctl updates | grep -q '2.0.0 (revision 1) on beta' || fail "updates does not offer 2.0.0"
login_pull
docker pull -q "$REG/notes/notes-os:beta-2.0.0" >/dev/null
ctr=$(docker create "$REG/notes/notes-os:beta-2.0.0" /bin/true)
CLEANUP+=("docker rm -f $ctr >/dev/null 2>&1")
docker cp "$ctr:/usr/sbin/notes-stow" "$WORK/os-2.0.0-ctl"
rm -rf "$WORK/preload2" && docker cp "$ctr:/usr/share/notes-preload" "$WORK/preload2"
docker rm -f "$ctr" >/dev/null
docker image rm -f "$REG/notes/notes-os:beta-2.0.0" >/dev/null
for tar in "$WORK"/preload2/*.tar; do docker load --input "$tar" >/dev/null || fail "docker load of $tar"; done
"${OPERATOR[@]}" "$WORK/os-2.0.0-ctl" boot --dir "$D" >"$WORK/vm-boot2.log" 2>&1 || { cat "$WORK/vm-boot2.log"; fail "boot on 2.0.0"; }
running 2.0.0 2
ls "$D"/snapshots/*.sql.gz >/dev/null 2>&1 || fail "the upgrade took no snapshot"
cmp -s "$D/notes-stow" "$WORK/os-2.0.0-ctl" || fail "the install does not keep 2.0.0's notes-stow"

step "booting 1.0.0 again (the A/B fallback) goes back to it"
"${VM1[@]}" boot --dir "$D" >"$WORK/vm-boot3.log" 2>&1 || { cat "$WORK/vm-boot3.log"; fail "boot on 1.0.0"; }
grep -q 'which the install.s last upgrade replaced: going back' "$WORK/vm-boot3.log" || { cat "$WORK/vm-boot3.log"; fail "boot did not go back"; }
running 1.0.0 2
cmp -s "$D/notes-stow" "$WORK/os-1.0.0-ctl" || fail "the install does not keep 1.0.0's notes-stow"

step "booting 2.0.0 again does not upgrade again"
"${OPERATOR[@]}" "$WORK/os-2.0.0-ctl" boot --dir "$D" >"$WORK/vm-boot4.log" 2>&1 || { cat "$WORK/vm-boot4.log"; fail "boot on 2.0.0 again"; }
grep -q 'went back from notes 2.0.0' "$WORK/vm-boot4.log" || { cat "$WORK/vm-boot4.log"; fail "boot did not keep the rollback"; }
running 1.0.0 2

printf '\nPASS\n'
