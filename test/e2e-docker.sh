#!/usr/bin/env bash
# End-to-end test of the Docker shape: the real stowage and <short>-stow binaries, a real registry
# (zot, as in e2e-registry.sh) and the local Docker engine. Needs Docker and network access.
#
#   make e2e-docker        (builds ./bin/stowage first)
#
# Walks examples/minimal, with a real Postgres, through: ship 1.0.0 → the bootstrap one-liner
# installs it → a re-run changes nothing → `--given` replaces an input, a kit issued and taken by --kit too → backups to a WebDAV server through the release's
# runner (the dump and the uploads volume's archive), and a restore that brings both back → ship 2.0.0 (a migration) → `notes-stow upgrade --version
# 2.0.0` fetches its installer and upgrades → rollback is allowed (1.0.0 reads the newer schema) →
# ship 3.0.0 (a migration that is a rollback barrier) → upgrade → going back to 1.0.0 by upgrade
# is refused, and rollback is refused naming the snapshot → `reset-db` wipes the data volumes and
# provisions again as first installed. The stand-in API keeps its migration
# number in its uploads volume (examples/minimal/api/notes-api). Then a site without internet:
# `notes-stow mirror export` writes 3.0.0 to one file, `mirror import` pushes it into a TLS mirror of
# its own CA, a second install pulls from the mirror (--registry, --registry-ca) and `upgrade
# --version` reads it, trusting the CA the install recorded. Along the way, the machine interface:
# `describe --json`; `status --json` before the install and after each change; `--json` on the
# install, upgrade, rollback and the mirror install's uninstall; the refusals exiting 3.
set -euo pipefail

# The amd64 image of the example's postgres: mirroring copies every platform of an index.
PG=postgres:18-alpine@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395
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
# result <file> <done|nothing> [version]: the file, a run's standard output under --json, is one
# Result: format 1, that result, the release installed after it that version (none: absent).
result() {
	jq -se --arg r "$2" --arg v "${3:-}" 'length == 1 and (.[0] | .format == 1 and .result == $r and
		(if $v == "" then has("release") | not else .release.version == $v and .release.revision == 1 end))' "$1" >/dev/null ||
		{ cat "$1"; fail "$1 is not one Result: $2, release ${3:-none}"; }
}
# refused <file> <reason ERE>: the Result in the file is a refusal with that reason and a fix.
refused() {
	jq -e --arg re "$2" '.result == "nothing" and (.refused.reason | test($re)) and (.refused.fix | length > 0)' "$1" >/dev/null ||
		{ cat "$1"; fail "$1 is not a refusal matching $2, with a fix"; }
}
# status_json <file> <installed> <previous|null>: status --json of the install in $D, into the
# file: format 1, the releases installed and kept (each with its installer by digest), the inputs
# as keys alone, and no value of a recorded input or secret anywhere in it.
status_json() {
	"${CTL[@]}" status --dir "$D" --json >"$1" 2>"$1.log" || { cat "$1.log" "$1"; fail "status --json"; }
	local installer="^$REG/notes/notes-installer@sha256:[0-9a-f]{64}\$" k v
	jq -se --arg v "$2" --arg p "$3" --arg i "$installer" 'length == 1 and (.[0] | .format == 1 and .name == "notes" and
		.installed.version == $v and .installed.revision == 1 and (.installed.installer | test($i)) and
		(if $p == "null" then .previous == null else .previous.version == $p and .previous.revision == 1 and (.previous.installer | test($i)) end) and
		(.inputs | type == "array" and index("ADMIN_EMAIL") != null and index("E2E_TOKEN") != null and all(.[]; test("^[A-Z][A-Z0-9_]*$"))))' "$1" >/dev/null ||
		{ cat "$1"; fail "status --json does not show $2 installed, $3 kept, the inputs by key"; }
	for k in E2E_TOKEN SESSION_SECRET POSTGRES_PASSWORD ADMIN_EMAIL; do
		v=$(sed -n "s/^$k='\{0,1\}\([^']*\)'\{0,1\}\$/\1/p" "$D/.env")
		[[ -n $v ]] || fail "$D/.env records no $k: the search below would find nothing"
		! grep -qF -- "$v" "$1" "$1.log" || fail "status --json prints the value of $k"
	done
}

step "start zot"
start_zot

step "project, ship 1.0.0"
new_project "$PG"
# A secret input, given at the install: describe prints no default of it, status --json no value.
python3 - "$P/stowage.yaml" <<'PY'
import sys
path = sys.argv[1]
s = open(path).read()
old = '  - { key: ADMIN_EMAIL, prompt: "Administrator email", required: true }\n'
assert s.count(old) == 1, old
s = s.replace(old, old + '  - { key: E2E_TOKEN, prompt: "A token", secret: true, optional: true }\n')
open(path, "w").write(s)
PY
"${S[@]}" lint
"${S[@]}" ship --channel stable 2>"$WORK/ship-1.0.0.log" || { cat "$WORK/ship-1.0.0.log"; fail "ship 1.0.0"; }

step "install 1.0.0 with the bootstrap one-liner"
# Two free ports for the stack, picked just before they are bound.
APP_PORT=$(free_port)
API_PORT=$(free_port)
while [[ $API_PORT == "$APP_PORT" ]]; do API_PORT=$(free_port); done
login_pull
docker run --rm --pull always "$REG/notes/notes-installer:latest" >"$WORK/notes.sh" || fail "docker run of the installer image"
unset DOCKER_CONFIG # for the next ships; the installer's environment never has it (OPERATOR)
# The machine interface before any install: describe reads the release the installer carries;
# status refuses a directory that holds no install (exit 3), its refusal one object on standard
# output, absent (no install there).
(cd "$WORK" && "${OPERATOR[@]}" sh notes.sh describe --json) >"$WORK/describe.json" 2>"$WORK/describe.log" || { cat "$WORK/describe.log"; fail "describe --json"; }
jq -se 'length == 1 and (.[0] | .format == 1 and .name == "notes" and .short_name == "notes" and .release == {version: "1.0.0", revision: 1} and
	.channel == "stable" and .first_install_only == ["address", "registry", "registry_ca", "channel"] and
	(.inputs | map(.key) | index("ADMIN_EMAIL") != null and index("E2E_TOKEN") != null) and
	(.inputs[] | select(.key == "ADMIN_EMAIL") | .required and (.secret | not) and .default == "") and
	(.inputs[] | select(.key == "E2E_TOKEN") | .secret and .optional) and
	all(.inputs[]; if .secret then has("default") | not else has("default") end))' "$WORK/describe.json" >/dev/null ||
	{ cat "$WORK/describe.json"; fail "describe --json does not describe 1.0.0, its inputs (no secret's default) and the first-install-only settings"; }
rc=0; (cd "$WORK" && "${OPERATOR[@]}" sh notes.sh status --dir "$D" --json) >"$WORK/status0.json" 2>"$WORK/status0.log" || rc=$?
[[ $rc == 3 ]] && grep -q 'holds no install' "$WORK/status0.log" && jq -se 'length == 1 and .[0].format == 1 and .[0].refused.absent == true and ((.[0].refused.reason + " " + (.[0].refused.fix // "")) | contains("holds no install"))' "$WORK/status0.json" >/dev/null ||
	{ cat "$WORK/status0.log" "$WORK/status0.json"; fail "status --json before the install exited $rc, not 3 with its refusal ('holds no install', absent) one object on standard output"; }
# The first install, under --json: its Result alone on standard output, its text on standard error.
TOKEN=$(token)
(cd "$WORK" && "${OPERATOR[@]}" sh notes.sh --no-input --dir "$D" --ports "app=$APP_PORT,api=$API_PORT" --set ADMIN_EMAIL=admin@notes.test --set "E2E_TOKEN=$TOKEN" --json) \
	>"$WORK/install.json" 2>"$WORK/install.log" || { cat "$WORK/install.log" "$WORK/install.json"; fail "install --json"; }
result "$WORK/install.json" "done" 1.0.0
grep -q 'notes 1.0.0' "$WORK/install.log" || { cat "$WORK/install.log"; fail "the install's text did not go to standard error"; }
running 1.0.0 1
status_json "$WORK/status1.json" 1.0.0 null
kept 1.0.0
grep -q '^ADMIN_PASSWORD=' "$D/provision.env" || fail "the provision hook recorded no ADMIN_PASSWORD"

step "re-run changes nothing"
sum=$(sha256sum "$D/.env" "$D/compose.yaml")
"${CTL[@]}" --dir "$D" --no-input >"$WORK/rerun.log" 2>&1 || { cat "$WORK/rerun.log"; fail "re-run"; }
[[ $(sha256sum "$D/.env" "$D/compose.yaml") == "$sum" ]] || fail "a re-run changed .env or compose.yaml"
running 1.0.0 1

step "--given on a re-run: an input replaced and logged, then given back"
given() {
	printf 'project: notes\ninputs: {ADMIN_EMAIL: %s}\n' "$1" | "${CTL[@]}" --dir "$D" --no-input --given - >"$WORK/given.log" 2>&1 || { cat "$WORK/given.log"; fail "--given $1"; }
}
given other@notes.test
grep -q "^ADMIN_EMAIL='other@notes.test'\$" "$D/.env" || fail "--given did not replace the recorded input"
grep -qF 'changing ADMIN_EMAIL: admin@notes.test → other@notes.test' "$WORK/given.log" || { cat "$WORK/given.log"; fail "the change was not logged"; }
given admin@notes.test
grep -q "^ADMIN_EMAIL='admin@notes.test'\$" "$D/.env" || fail "--given did not give the input back"
running 1.0.0 1

step "kit create: a kit for another install, taken by this one with --kit; kit check"
"${CTL[@]}" kit create second-site --dir "$D" --name site-2 --set SITE_ADMIN=second@notes.test -o "$WORK/site.kit" >"$WORK/kit.log" 2>&1 || { cat "$WORK/kit.log"; fail "kit create"; }
sed -n 's/^code  //p' "$WORK/kit.log" >"$WORK/site.code"
[[ -s $WORK/site.code ]] || { cat "$WORK/kit.log"; fail "kit create printed no code"; }
[[ $(stat -c %a "$WORK/site.kit") == 600 ]] || fail "the kit is not 0600"
head -1 "$WORK/site.kit" | grep -q 'BEGIN AGE ENCRYPTED FILE' || fail "the kit is not armored age"
! grep -qF second@notes.test "$WORK/site.kit" "$WORK/kit.log" || fail "a value in the clear"
"${CTL[@]}" kit create second-site --dir "$D" --name site-2 --set SITE_ADMIN=x -o "$WORK/site.kit" >"$WORK/kit2.log" 2>&1 && fail "kit create replaced a file"
grep -q 'never replaces a file' "$WORK/kit2.log" || { cat "$WORK/kit2.log"; fail "the refusal does not say why"; }
"${CTL[@]}" --dir "$D" --no-input --kit "$WORK/site.kit" >"$WORK/kit3.log" 2>&1 && fail "--kit with --no-input and no code"
grep -q 'nothing gives its code' "$WORK/kit3.log" || { cat "$WORK/kit3.log"; fail "the refusal does not name the code's sources"; }
"${CTL[@]}" --dir "$D" --no-input --kit "$WORK/site.kit" --kit-code-file "$WORK/site.code" >"$WORK/kit4.log" 2>&1 || { cat "$WORK/kit4.log"; fail "--kit on a re-run"; }
grep -q "^ADMIN_EMAIL='second@notes.test'\$" "$D/.env" || fail "--kit did not replace the recorded input"
grep -qF 'changing ADMIN_EMAIL: admin@notes.test → second@notes.test' "$WORK/kit4.log" || { cat "$WORK/kit4.log"; fail "the change was not logged"; }
! grep -qF "$(cat "$WORK/site.code")" "$WORK/kit4.log" || fail "the code reached the log"
"${CTL[@]}" upgrade --dir "$D" --kit "$WORK/site.kit" >"$WORK/kit5.log" 2>&1 && fail "upgrade took --kit"
grep -q -- '--kit: an upgrade takes none of it' "$WORK/kit5.log" || { cat "$WORK/kit5.log"; fail "the refusal does not name the re-run"; }
given admin@notes.test
"${CTL[@]}" kit check second-site site-2 --dir "$D" >"$WORK/kit-check.log" 2>&1 && grep -q 'proven$' "$WORK/kit-check.log" || { cat "$WORK/kit-check.log"; fail "kit check of a site"; }
rc=0; "${CTL[@]}" kit check second-site site-unknown --dir "$D" >"$WORK/kit-check.log" 2>&1 || rc=$?
[[ $rc == 2 ]] || { cat "$WORK/kit-check.log"; fail "kit check of an unknown site exited $rc, not 2"; }
rc=0; "${CTL[@]}" kit check second-site site-refused --dir "$D" >"$WORK/kit-check.log" 2>&1 || rc=$?
[[ $rc == 1 ]] || { cat "$WORK/kit-check.log"; fail "kit check of a refused site exited $rc, not 1"; }
running 1.0.0 1

step "audit on the Docker host: the release's SBOMs, scanned with its own scanner (the database given)"
grype_db
scanner_fresh "$REG/notes"
"${CTL[@]}" audit --dir "$D" --db "$GRYPE_DB" --json >"$WORK/audit.json" 2>"$WORK/audit.log" || { cat "$WORK/audit.log" "$WORK/audit.json"; fail "audit"; }
[[ $(jq -r .version "$WORK/audit.json") == 1.0.0 && $(jq '[.subjects[] | select(.error)] | length' "$WORK/audit.json") == 0 && $(jq '.subjects | length' "$WORK/audit.json") -ge 7 ]] ||
	{ cat "$WORK/audit.json"; fail "not every artifact of 1.0.0 was audited"; }
"${CTL[@]}" audit --dir "$D" --db "$GRYPE_DB" >"$WORK/audit.txt" 2>&1 || { cat "$WORK/audit.txt"; fail "audit, printed"; }
grep -q '^notes 1.0.0 (revision 1, stable, ' "$WORK/audit.txt" && grep -q ' artifacts, vulnerabilities: ' "$WORK/audit.txt" || { cat "$WORK/audit.txt"; fail "the audit's report"; }
[[ -d $D/.stowage-grype/cache ]] || fail "the scanner's database is not kept in the install"
"${CTL[@]}" sbom export "$WORK/sbom" --dir "$D" >"$WORK/sbom.txt" 2>&1 || { cat "$WORK/sbom.txt"; fail "sbom export"; }
jq -e '.version == "1.0.0" and (.artifacts | length) >= 7 and all(.artifacts[]; .error == null)' "$WORK/sbom/index.json" >/dev/null &&
	jq -e '.spdxVersion' "$WORK/sbom/api.spdx.json" >/dev/null || { cat "$WORK/sbom/index.json"; fail "the exported SBOMs"; }

step "the release's default profile: each container takes the host's own settings and open files"
"${CTL[@]}" status --dir "$D" >"$WORK/status.txt" 2>&1 && grep -qx 'tune      default (release)' "$WORK/status.txt" ||
	{ cat "$WORK/status.txt"; fail "status does not show the release's default profile"; }
api=$(docker ps -q --filter "label=com.docker.compose.project=$(project)" --filter label=com.docker.compose.service=api)
[[ $(docker inspect --format '{{index .HostConfig.Sysctls "net.ipv4.ip_local_port_range"}}' "$api") == "$(awk '{print $1, $2}' /proc/sys/net/ipv4/ip_local_port_range)" &&
	$(docker inspect --format '{{index .HostConfig.Sysctls "net.core.somaxconn"}}' "$api") == "$(cat /proc/sys/net/core/somaxconn)" ]] ||
	fail "the api container's settings are not the host's: $(docker inspect --format '{{json .HostConfig.Sysctls}}' "$api")"
# Its open files: fs.nr_open, bounded by the engine's own limit; the soft limit at most 65536.
read -r _ soft hard < <(docker inspect --format '{{range .HostConfig.Ulimits}}{{.Name}} {{.Soft}} {{.Hard}}{{println}}{{end}}' "$api" | grep '^nofile ') || true
[[ -n $hard ]] && ((hard > 1024 && hard <= $(cat /proc/sys/fs/nr_open) && soft == (hard < 65536 ? hard : 65536))) ||
	fail "the api container is given ${soft:-no} open files (hard ${hard:-none})"

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
"${CTL[@]}" upgrade --version 2.0.0 --dir "$D" --no-input --json >"$WORK/up2.json" 2>"$WORK/up2.log" || { cat "$WORK/up2.log" "$WORK/up2.json"; fail "upgrade --json to 2.0.0"; }
result "$WORK/up2.json" "done" 2.0.0
running 2.0.0 2
kept 2.0.0
no_next
status_json "$WORK/status2.json" 2.0.0 1.0.0
ls "$D"/snapshots/*.sql.gz >/dev/null 2>&1 || fail "the upgrade took no snapshot"
gzip -dc "$D"/snapshots/*.sql.gz >"$WORK/snapshot.sql" && grep -q 'PostgreSQL database dump' "$WORK/snapshot.sql" || fail "the snapshot is not a pg_dump"
"${CTL[@]}" upgrade --version 2.0.0 --dir "$D" --json >"$WORK/again.json" 2>"$WORK/again.log" || { cat "$WORK/again.log"; fail "upgrade to the running release"; }
grep -q 'already runs' "$WORK/again.log" || { cat "$WORK/again.log"; fail "no note that 2.0.0 already runs"; }
result "$WORK/again.json" nothing 2.0.0

step "rollback to 1.0.0 (allowed: 1.0.0 reads the newer schema)"
"${CTL[@]}" rollback --dir "$D" --json >"$WORK/back1.json" 2>"$WORK/back1.log" || { cat "$WORK/back1.log" "$WORK/back1.json"; fail "rollback --json"; }
result "$WORK/back1.json" "done" 1.0.0
grep -q 'the database is at 2; 1.0.0 reads past the newer migrations' "$WORK/back1.log" || { cat "$WORK/back1.log"; fail "the rollback did not check the schema"; }
running 1.0.0 2
kept 1.0.0
status_json "$WORK/status3.json" 1.0.0 null

step "ship 3.0.0 (a rollback barrier), upgrade --version 3.0.0"
release 3.0.0 '{"version": 3, "barriers": {"3": "notes moved to a new table"}, "reads_newer": true}'
"${CTL[@]}" upgrade --version 3.0.0 --dir "$D" --no-input >"$WORK/up3.log" 2>&1 || { cat "$WORK/up3.log"; fail "upgrade to 3.0.0"; }
running 3.0.0 3
kept 3.0.0
no_next

step "going back is refused"
# Each a refusal: exit 3, and under --json a Result of nothing with its reason.
rc=0; "${CTL[@]}" upgrade --version 1.0.0 --dir "$D" --json >"$WORK/down.json" 2>"$WORK/down.log" || rc=$?
[[ $rc == 3 ]] || { cat "$WORK/down.log"; fail "upgrade --version 1.0.0 exited $rc, not 3 (refused)"; }
grep -q 'runs notes 3.0.0 (revision 1), after notes 1.0.0 (revision 1): .*rollback' "$WORK/down.log" || { cat "$WORK/down.log"; fail "the refusal does not name rollback"; }
result "$WORK/down.json" nothing 3.0.0
refused "$WORK/down.json" 'runs notes 3[.]0[.]0 [(]revision 1[)], after notes 1[.]0[.]0 [(]revision 1[)]'
rc=0; "${CTL[@]}" rollback --dir "$D" --json >"$WORK/back3.json" 2>"$WORK/back3.log" || rc=$?
[[ $rc == 3 ]] || { cat "$WORK/back3.log"; fail "rollback across a barrier exited $rc, not 3 (refused)"; }
grep -q 'notes moved to a new table' "$WORK/back3.log" && grep -q 'snapshots/' "$WORK/back3.log" ||
	{ cat "$WORK/back3.log"; fail "the refusal does not name the barrier and the snapshot"; }
result "$WORK/back3.json" nothing 3.0.0
jq -e '.refused.reason | test("notes moved to a new table")' "$WORK/back3.json" >/dev/null || { cat "$WORK/back3.json"; fail "the Result's reason does not name the barrier"; }
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
# The SBOMs and the scanner came through the mirror: the audit reads only the site (a scanner read
# from another registry is another local image, so it is pulled from the site).
scanner_fresh "$SITE"
"${OPERATOR[@]}" "$D/notes-stow" audit --dir "$D" --db "$GRYPE_DB" --json >"$WORK/audit-site.json" 2>"$WORK/audit-site.log" ||
	{ cat "$WORK/audit-site.log" "$WORK/audit-site.json"; fail "audit through the mirror"; }
[[ $(jq '[.subjects[] | select(.error)] | length' "$WORK/audit-site.json") == 0 ]] || { cat "$WORK/audit-site.json"; fail "an SBOM did not reach the mirror"; }
grep -q "pulling $SITE/notes-audit@" "$WORK/audit-site.log" || { cat "$WORK/audit-site.log"; fail "the scanner was not pulled from the mirror"; }

step "uninstall --json of the mirror's install: done, no release left"
project=$(project)
"${OPERATOR[@]}" "$D/notes-stow" uninstall --dir "$D" --data --yes --json >"$WORK/uninstall.json" 2>"$WORK/uninstall.log" ||
	{ cat "$WORK/uninstall.log" "$WORK/uninstall.json"; fail "uninstall --json"; }
result "$WORK/uninstall.json" "done"
[[ -z $(docker ps -aq --filter "label=com.docker.compose.project=$project") ]] || fail "uninstall left the stack's containers"
D=$D1 APP_PORT=$APP1 API_PORT=$API1

printf '\nPASS\n'
