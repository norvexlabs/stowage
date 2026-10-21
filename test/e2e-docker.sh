#!/usr/bin/env bash
# End-to-end test of the Docker shape: the real stowage and <short>-stow binaries, a real registry
# (zot, as in e2e-registry.sh) and the local Docker engine. Needs Docker and network access.
#
#   make e2e-docker        (builds ./bin/stowage first)
#
# Walks examples/minimal, with a real Postgres, through: ship 1.0.0 → the bootstrap one-liner
# installs it → a re-run changes nothing → backups to a WebDAV server through the release's
# runner (the dump and the uploads volume's archive), and a restore that brings both back → ship 2.0.0 (a migration) → `notes-stow upgrade --version
# 2.0.0` fetches its installer and upgrades → rollback is allowed (1.0.0 reads the newer schema) →
# ship 3.0.0 (a migration that is a rollback barrier) → upgrade → going back to 1.0.0 by upgrade
# is refused, and rollback is refused naming the snapshot → `reset-db` wipes the data volumes and
# provisions again as first installed. The stand-in API keeps its migration
# number in its uploads volume (examples/minimal/api/notes-api). Then a site without internet:
# `notes-stow mirror export` writes 3.0.0 to one file, `mirror import` pushes it into a TLS mirror of
# its own CA, a second install pulls from the mirror (--registry, --registry-ca) and `upgrade
# --version` reads it, trusting the CA the install recorded.
set -euo pipefail

# The amd64 image of the example's postgres: mirroring copies every platform of an index.
PG=postgres:17-alpine@sha256:aa90e97ee862e558111d34cfb8b2c4bec768c2b039fb791341686928560263b3
. "$(dirname "$0")/lib.sh"

D=$WORK/stowage-e2e-install
# The installer runs as an operator's would (OPERATOR).
CTL=("${OPERATOR[@]}" "$D/notes-stow")
CLEANUP+=('remove_install')

# release <version> <schema.json>: bump the example, ship it on stable.
release() {
	printf '%s\n' "$1" >"$P/VERSION"
	printf '### Changed\n- Release %s.\n' "$1" >"$P/changes/$1.md"
	printf '%s\n' "$2" >"$P/api/schema.json"
	"${S[@]}" ship --channel stable 2>"$WORK/ship-$1.log" || { cat "$WORK/ship-$1.log"; fail "ship $1"; }
}
no_next() { ! compgen -G "$D/.stowage-next-*" >/dev/null || fail "a fetched installer was left in $D"; }

step "start zot"
start_zot

step "project, ship 1.0.0"
new_project "$PG"
"${S[@]}" ship --channel stable 2>"$WORK/ship-1.0.0.log" || { cat "$WORK/ship-1.0.0.log"; fail "ship 1.0.0"; }

step "install 1.0.0 with the bootstrap one-liner"
# Two free ports for the stack, picked just before they are bound.
APP_PORT=$(free_port)
API_PORT=$(free_port)
while [[ $API_PORT == "$APP_PORT" ]]; do API_PORT=$(free_port); done
login_pull
docker run --rm --pull always "$REG/notes/notes-installer:latest" >"$WORK/notes.sh" || fail "docker run of the installer image"
unset DOCKER_CONFIG # for the next ships; the installer's environment never has it (OPERATOR)
(cd "$WORK" && "${OPERATOR[@]}" sh notes.sh --no-input --dir "$D" --ports "app=$APP_PORT,api=$API_PORT" --set ADMIN_EMAIL=admin@notes.test) \
	>"$WORK/install.log" 2>&1 || { cat "$WORK/install.log"; fail "install"; }
running 1.0.0 1
kept 1.0.0
grep -q '^ADMIN_PASSWORD=' "$D/provision.env" || fail "the provision hook recorded no ADMIN_PASSWORD"

step "re-run changes nothing"
sum=$(sha256sum "$D/.env" "$D/compose.yaml")
"${CTL[@]}" --dir "$D" --no-input >"$WORK/rerun.log" 2>&1 || { cat "$WORK/rerun.log"; fail "re-run"; }
[[ $(sha256sum "$D/.env" "$D/compose.yaml") == "$sum" ]] || fail "a re-run changed .env or compose.yaml"
running 1.0.0 1

step "backups on Docker: config, verify, run to WebDAV through the release's runner, list, status, restore"
start_dav
age_key
project=$(project)
pg() { docker ps --quiet --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=postgres; }
docker exec "$(pg)" psql -U notes -d notes -c 'CREATE TABLE e2e_backup (n int); INSERT INTO e2e_backup VALUES (1)' >/dev/null || fail "a table to back up"
api() { docker ps --quiet --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=api; }
docker exec "$(api)" sh -c 'echo at-the-backup >/data/uploads/e2e.txt' || fail "a file to back up in the uploads volume"
# The schedule is not tested here: the host's systemd is not this suite's (the units are unit-tested).
"${CTL[@]}" backup config --dir "$D" --destination dav:backups --rclone-config "$DAV_CONF" --recipient "$AGE_RECIPIENT" >"$WORK/dbackup-config.txt" 2>&1 ||
	{ cat "$WORK/dbackup-config.txt"; fail "backup config"; }
"${CTL[@]}" backup verify --dir "$D" >"$WORK/dbackup-verify.txt" 2>&1 || { cat "$WORK/dbackup-verify.txt"; fail "backup verify"; }
grep -q 'reachable and writable' "$WORK/dbackup-verify.txt" || { cat "$WORK/dbackup-verify.txt"; fail "backup verify did not reach the destination"; }
"${CTL[@]}" backup run --dir "$D" >"$WORK/dbackup-run.txt" 2>&1 || { cat "$WORK/dbackup-run.txt"; fail "backup run"; }
backups=("$WORK"/dav/backups/notes-full-*-1.0.0-r1.pgdump.age)
[[ ${#backups[@]} == 1 && -s ${backups[0]} ]] || { ls -la "$WORK/dav/backups"; fail "no backup in the destination"; }
check_backup "${backups[0]}" "$PG" e2e_backup
# The uploads volume's archive beside the dump (backup.volumes), encrypted too; backup list shows the dump alone.
archive=${backups[0]%.pgdump.age}.uploads.tar.age
"$BIN/age" -d -i "$AGE_KEY" "$archive" | tar -t | grep -qx './e2e.txt' || fail "$archive does not hold the uploads volume's file"
"${CTL[@]}" backup list --dir "$D" | grep -q "^${backups[0]##*/} " || fail "backup list does not show the backup"
! "${CTL[@]}" backup list --dir "$D" | grep -q 'tar\.age' || fail "backup list shows a volume's archive as a backup"
"${CTL[@]}" backup status --dir "$D" | grep -q "^last  .*${backups[0]##*/}" || fail "backup status does not show the backup"
docker exec "$(pg)" psql -U notes -d notes -c 'INSERT INTO e2e_backup VALUES (2)' >/dev/null || fail "a row after the backup"
docker exec "$(api)" sh -c 'echo since >/data/uploads/e2e.txt && echo later >/data/uploads/later.txt' || fail "files after the backup"
"${CTL[@]}" restore "${backups[0]##*/}" --dir "$D" --identity "$AGE_KEY" --yes >"$WORK/drestore.txt" 2>&1 || { cat "$WORK/drestore.txt"; fail "restore"; }
[[ $(docker exec "$(pg)" psql -U notes -d notes -tAc 'SELECT count(*) FROM e2e_backup') == 1 ]] || fail "the restore did not bring the backup's data back"
# The volume as it was at the backup, its file's owner kept; what came after is gone.
[[ $(docker exec "$(api)" sh -c 'cat /data/uploads/e2e.txt; stat -c %u /data/uploads/e2e.txt; test -e /data/uploads/later.txt || echo gone') == $'at-the-backup\n1000\ngone' ]] ||
	{ docker exec "$(api)" ls -lan /data/uploads; fail "the restore did not bring the uploads volume back as it was"; }
running 1.0.0 1
"${CTL[@]}" backup config --dir "$D" --remove >/dev/null 2>&1 || fail "backup config --remove"

step "ship 2.0.0, upgrade --version 2.0.0"
release 2.0.0 '{"version": 2, "barriers": {}, "reads_newer": true}'
"${CTL[@]}" upgrade --version 2.0.0 --dir "$D" --no-input >"$WORK/up2.log" 2>&1 || { cat "$WORK/up2.log"; fail "upgrade to 2.0.0"; }
running 2.0.0 2
kept 2.0.0
no_next
ls "$D"/snapshots/*.sql.gz >/dev/null 2>&1 || fail "the upgrade took no snapshot"
gzip -dc "$D"/snapshots/*.sql.gz >"$WORK/snapshot.sql" && grep -q 'PostgreSQL database dump' "$WORK/snapshot.sql" || fail "the snapshot is not a pg_dump"
"${CTL[@]}" upgrade --version 2.0.0 --dir "$D" >"$WORK/again.log" 2>&1 || { cat "$WORK/again.log"; fail "upgrade to the running release"; }
grep -q 'already runs' "$WORK/again.log" || { cat "$WORK/again.log"; fail "no note that 2.0.0 already runs"; }

step "rollback to 1.0.0 (allowed: 1.0.0 reads the newer schema)"
"${CTL[@]}" rollback --dir "$D" >"$WORK/back1.log" 2>&1 || { cat "$WORK/back1.log"; fail "rollback"; }
grep -q 'the database is at 2; 1.0.0 reads past the newer migrations' "$WORK/back1.log" || { cat "$WORK/back1.log"; fail "the rollback did not check the schema"; }
running 1.0.0 2
kept 1.0.0

step "ship 3.0.0 (a rollback barrier), upgrade --version 3.0.0"
release 3.0.0 '{"version": 3, "barriers": {"3": "notes moved to a new table"}, "reads_newer": true}'
"${CTL[@]}" upgrade --version 3.0.0 --dir "$D" --no-input >"$WORK/up3.log" 2>&1 || { cat "$WORK/up3.log"; fail "upgrade to 3.0.0"; }
running 3.0.0 3
kept 3.0.0
no_next

step "going back is refused"
if "${CTL[@]}" upgrade --version 1.0.0 --dir "$D" >"$WORK/down.log" 2>&1; then fail "upgrade --version 1.0.0 went back"; fi
grep -q 'runs notes 3.0.0 (revision 1), after notes 1.0.0 (revision 1): .*rollback' "$WORK/down.log" || { cat "$WORK/down.log"; fail "the refusal does not name rollback"; }
if "${CTL[@]}" rollback --dir "$D" >"$WORK/back3.log" 2>&1; then fail "rollback crossed a barrier"; fi
grep -q 'notes moved to a new table' "$WORK/back3.log" && grep -q 'snapshots/' "$WORK/back3.log" ||
	{ cat "$WORK/back3.log"; fail "the refusal does not name the barrier and the snapshot"; }
running 3.0.0 3
kept 3.0.0

step "reset-db: the data wiped, provisioned again as first installed"
project=$(project)
api() { docker ps --quiet --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=api; }
docker exec "$(api)" touch /data/uploads/marker || fail "no marker written in the uploads volume"
pw=$(grep '^ADMIN_PASSWORD=' "$D/provision.env")
sum=$(sha256sum "$D/.env")
if "${CTL[@]}" reset-db --dir "$D" </dev/null >"$WORK/reset0.log" 2>&1; then fail "reset-db wiped without --yes"; fi
grep -q 'give --yes' "$WORK/reset0.log" || { cat "$WORK/reset0.log"; fail "the refusal does not name --yes"; }
"${CTL[@]}" reset-db --dir "$D" --yes >"$WORK/reset.log" 2>&1 || { cat "$WORK/reset.log"; fail "reset-db"; }
running 3.0.0 3
if docker exec "$(api)" test -e /data/uploads/marker; then fail "the uploads volume was not wiped"; fi
[[ $(grep '^ADMIN_PASSWORD=' "$D/provision.env") != "$pw" ]] || fail "the provision hook was given what it returned before the reset"
[[ $(sha256sum "$D/.env") == "$sum" ]] || fail "reset-db changed .env"

step "mirror: export 3.0.0, import it into a TLS mirror of its own CA"
start_mirror
"${CTL[@]}" mirror export --output "$WORK/notes.oci.tar" >"$WORK/export.txt" 2>"$WORK/export.log" || { cat "$WORK/export.log"; fail "mirror export"; }
grep -q "^Exported notes 3.0.0 (revision 1, .*) to $WORK/notes.oci.tar" "$WORK/export.txt" || { cat "$WORK/export.txt"; fail "export did not report"; }
SITE=$MIRROR/site/notes
if env -i "PATH=$PATH" "HOME=$HOME" "DOCKER_CONFIG=$MIRROR_CONFIG" "$D/notes-stow" mirror import "$WORK/notes.oci.tar" --to "$SITE" >"$WORK/import0.log" 2>&1; then
	fail "imported into a mirror whose CA nothing trusts"
fi
env -i "PATH=$PATH" "HOME=$HOME" "DOCKER_CONFIG=$MIRROR_CONFIG" "$D/notes-stow" mirror import "$WORK/notes.oci.tar" --to "$SITE" --registry-ca "$MIRROR_CA" \
	>"$WORK/import.txt" 2>"$WORK/import.log" || { cat "$WORK/import.log"; fail "mirror import"; }
grep -q "^Imported $WORK/notes.oci.tar into $SITE" "$WORK/import.txt" || { cat "$WORK/import.txt"; fail "import did not report"; }
[[ $(mirror_referrers site/notes/notes-release stable) -ge 1 ]] || fail "the release's signature did not reach the mirror"
tag=$(sed -n 's/^Exported notes 3.0.0 (revision 1, \([^)]*\)).*/\1/p' "$WORK/export.txt")
[[ $(mirror_referrers site/notes/notes-api "$tag") -ge 1 ]] || fail "the api image's signature did not reach the mirror"

step "an install from the mirror; upgrade --version reads it with the recorded CA"
D1=$D D=$WORK/stowage-e2e-site APP1=$APP_PORT API1=$API_PORT
APP_PORT=$(free_port)
API_PORT=$(free_port)
while [[ $API_PORT == "$APP_PORT" ]]; do API_PORT=$(free_port); done
CLEANUP+=('D=$WORK/stowage-e2e-site remove_install' "docker images --format '{{.Repository}}@{{.Digest}}' | grep -F '$SITE/' | xargs -r docker rmi >/dev/null 2>&1")
"${OPERATOR[@]}" "$D1/notes-stow" --no-input --dir "$D" --ports "app=$APP_PORT,api=$API_PORT" --set ADMIN_EMAIL=admin@notes.test \
	--registry "$SITE" --registry-ca "$MIRROR_CA" >"$WORK/install-site.log" 2>&1 || { cat "$WORK/install-site.log"; fail "install from the mirror"; }
running 3.0.0 3
grep -qx "STOWAGE_REGISTRY='\{0,1\}$SITE'\{0,1\}" "$D/.env" || fail "the install does not record the mirror"
cmp -s "$D/registry-ca.pem" "$MIRROR_CA" || fail "the install does not keep the mirror's CA"
"${OPERATOR[@]}" "$D/notes-stow" upgrade --version 3.0.0 --dir "$D" >"$WORK/site-again.log" 2>&1 || { cat "$WORK/site-again.log"; fail "upgrade --version through the mirror"; }
grep -q 'already runs' "$WORK/site-again.log" || { cat "$WORK/site-again.log"; fail "upgrade --version did not read the mirror"; }
D=$D1 APP_PORT=$APP1 API_PORT=$API1

printf '\nPASS\n'
