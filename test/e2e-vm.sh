#!/usr/bin/env bash
# End-to-end test of a real Kairos VM on a site without internet: the release goes to a TLS mirror
# of its own CA (`mirror export`, `mirror import`), `vm create --create opentofu` on a TrueNAS host
# boots the disk it reads from the mirror, whose first boot installs from the cloud-config, IPv6 on
# (`--ipv6`: the LAN's IPv6 address, SSH and the front reached over it);
# its firewall lets SSH in from this host alone (`--ssh-allow`) and the front's ports, and an L4
# route's port from the sources its allowlist holds (this host over IPv4: refused over IPv6, and
# from every source once a re-run changes the allowlist, through the upgrade's reboot), its service
# (the database) on the host's network and reached by name from the stack's, in the time
# zone given (`--timezone`); kits cross between it and a Docker install and a kind cluster beside
# the suite, both ways, and a second VM's first install comes from `vm create --kit` of a kit Kubernetes
# issued, with a `byo` certificate and a secret input over the pinned SSH, in no seed; `reset-db` wipes the data and provisions again without a reboot; a
# backup goes, encrypted, to a WebDAV server on this host — the database and the uploads volume —
# and `restore` loads both back over newer data; `secrets export` writes the install's secrets encrypted to the operator's key; a newer release crosses the same way and the VM upgrades from the
# mirror (a backup first), its health gate marking the boot
# good; `rollback` takes the VM back to the passive system; a release that never becomes ready
# fails its health gate, and Kairos falls back to the passive system, whose boot goes back; one that
# fails it past a rollback barrier is kept, as going back would leave nothing serving;
# `vm attach` gives the VM a NIC for the role 1.0.1 adds, which `network set --role` re-addresses;
# `network set` keeps the lease as a static address, a unique local IPv6 address beside it, and a
# change not confirmed comes undone at its window's end and at a reboot; then `vm destroy`. Along
# the way, the machine interface: the VM's `describe --json` and `status --json`, `--json` on its
# upgrade and rollback, a refusal exiting 3; `vm seed --json` against the mirror, and
# `--known-hosts-line` on `vm kit check`. Needs Docker
# (privileged containers, loop devices), network access, about 15 GB on disk under
# XDG_CACHE_HOME, OpenTofu's `tofu`, ssh, and a TrueNAS host the VM's DHCP lease is registered on.
#
#   STOWAGE_VM_ENV=<file> make e2e-vm        (builds ./bin/stowage first)
#
# The file is read by test/vm.sh's vm_env, never copied or printed.
set -euo pipefail

PG=postgres:18-alpine@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395
. "$(dirname "$0")/vm.sh"
vm_env
. "$(dirname "$0")/lib.sh"

# healthy_postgres <when>: ps shows postgres healthy within 2 minutes (its health check runs after
# the stack is up).
healthy_postgres() {
	local deadline=$((SECONDS + 120))
	until on_vm notes-stow ps >"$WORK/ps.txt" 2>&1 && grep -q '^postgres .*running (healthy)' "$WORK/ps.txt"; do
		((SECONDS < deadline)) || { cat "$WORK/ps.txt"; fail "ps does not show a healthy postgres $1"; }
		sleep 5
	done
}
# vm_result <file> <done|nothing> <version>: the file, a verb's standard output on the VM under
# --json, is one Result: format 1, that result, that release installed as the run ended (before
# the reboot its job schedules).
vm_result() {
	jq -se --arg r "$2" --arg v "$3" 'length == 1 and (.[0] | .format == 1 and .result == $r and .release == {version: $v, revision: 1})' "$1" >/dev/null ||
		{ cat "$1"; fail "$1 is not one Result: $2, release $3"; }
}
# vm_status_json <file> <installed> <previous|null|->: the VM's status --json over SSH, into the
# file: format 1, the release installed, the one the last upgrade replaced (-: not checked), the
# inputs as keys alone, and no value of a recorded input or secret anywhere in it.
vm_status_json() {
	on_vm sudo notes-stow status --json >"$1" 2>"$1.log" || { cat "$1.log" "$1"; fail "status --json on the VM"; }
	jq -se --arg v "$2" --arg p "$3" 'length == 1 and (.[0] | .format == 1 and .name == "notes" and .installed == {version: $v, revision: 1} and
		(if $p == "-" then true elif $p == "null" then .previous == null else .previous.version == $p and .previous.revision == 1 end) and
		(.inputs | type == "array" and index("ADMIN_EMAIL") != null and index("DB_SOURCES") != null and all(.[]; test("^[A-Z][A-Z0-9_]*$"))))' "$1" >/dev/null ||
		{ cat "$1"; fail "the VM's status --json does not show $2 installed, $3 replaced, the inputs by key"; }
	local k v
	on_vm sudo cat /usr/local/notes/.env >"$WORK/vm-status.env" || fail "reading the VM's .env"
	for k in SESSION_SECRET POSTGRES_PASSWORD STOWAGE_HOST_TOKEN ADMIN_EMAIL DB_SOURCES; do
		v=$(sed -n "s/^$k='\{0,1\}\([^']*\)'\{0,1\}\$/\1/p" "$WORK/vm-status.env")
		[[ -n $v ]] || { rm -f "$WORK/vm-status.env"; fail "the VM's .env records no $k: the search below would find nothing"; }
		! grep -qF -- "$v" "$1" "$1.log" || { rm -f "$WORK/vm-status.env"; fail "the VM's status --json prints the value of $k"; }
	done
	rm -f "$WORK/vm-status.env"
}
# release <version> [ready] [migration] [barriers]: ship it on beta (ready=false: its api never
# becomes ready; migration 1 and no barrier by default, barriers a JSON object), carry it through
# the mirror without disks.
release() {
	local barriers=${4:-}
	[[ -n $barriers ]] || barriers='{}'
	printf '%s\n' "$1" >"$P/VERSION"
	printf '### Changed\n- Release %s.\n' "$1" >"$P/changes/$1.md"
	printf '{"version": %s, "barriers": %s, "reads_newer": true, "ready": %s}\n' "${3:-1}" "$barriers" "${2:-true}" >"$P/api/schema.json"
	"${S[@]}" ship --channel beta 2>"$WORK/ship-$1.log" || { tail -50 "$WORK/ship-$1.log"; fail "ship $1"; }
	mirror "$WORK/$1.oci.tar" --disks none
}

step "start zot"
start_zot

step "project, with the kairos shape and its qcow2 and raw disks"
KAIROS=1 new_project "$PG"
# Two formats: the site's mirror carries the qcow2 one alone, which vm create boots TrueNAS from.
sed -i 's/^    disk_formats: \[qcow2\]$/    disk_formats: [qcow2, raw]/' "$P/stowage.yaml"
grep -qx '    disk_formats: \[qcow2, raw\]' "$P/stowage.yaml" || fail "the disk formats were not edited"
# A short health gate: the release that never becomes ready falls back sooner.
sed -i 's/^    host_api: \[api\]$/&\n    health_gate: { deadline_s: 120, deadline_after_reset_s: 600, poll_s: 10 }/' "$P/stowage.yaml"
grep -q '^    health_gate: ' "$P/stowage.yaml" || fail "no health_gate in the project"
# An L4 route to the database, which the VM's firewall opens to the sources DB_SOURCES holds: on
# the host's network, the database listens on the route's port itself. A kernel argument the
# active system boots with and the recovery system without (a module's parameter, which nothing
# reads), and a package of the project's. Its NIC named by a role, the primary (1.0.1 adds a
# secondary, which `vm attach` attaches).
python3 - "$P/stowage.yaml" <<'PY'
import sys
path = sys.argv[1]
s = open(path).read()
for old, new in [
    ("\nhealth:\n", "\nl4:\n  routes:\n    - { name: db, protocol: tcp, port: 5432, upstream: postgres:5432, allow_input: DB_SOURCES, bind_input: DB_BIND }\n\nhealth:\n"),
    ("  - { key: ADMIN_EMAIL, prompt: \"Administrator email\", required: true }\n",
     "  - { key: ADMIN_EMAIL, prompt: \"Administrator email\", required: true }\n  - { key: DEPLOY_TOKEN, prompt: \"Token of the second site\", secret: true, optional: true }\n  - { key: DB_SOURCES, prompt: \"Sources the firewall lets reach the database\", shapes: [docker, kairos] }\n  - { key: DB_BIND, prompt: \"Host address the database is published on\", advanced: true, shapes: [docker, kairos] }\n"),
    ("  loopback_ports: { app: 8080, api: 8000 }\n", "  loopback_ports: { app: 8080, api: 8000 }\n  publish_ca: { volume: ca }\n"),
    ("    uploads:  { uid: 1000 }\n", "    uploads:  { uid: 1000 }\n    ca:       { uid: 1000 }\n"),
    ("\n\nbackup:\n",
     "\n  - name: vm-site\n    for: notes\n    ask:\n      - { key: SITE_ADMIN, prompt: a }\n      - { key: GIVE_DEPLOY_TOKEN, prompt: t, secret: true }\n      - { key: GIVE_KIT_TLS_CERT, prompt: c }\n      - { key: GIVE_KIT_TLS_KEY, prompt: k, secret: true }\n      - { key: GIVE_KIT_TLS_CA, prompt: i }\n    run: { image: api, cmd: [notes-api, kit-issue], env: [] }\n    gives: [ADMIN_EMAIL, DEPLOY_TOKEN, KIT_TLS_CERT, KIT_TLS_KEY, KIT_TLS_CA]\n\nbackup:\n"),
]:
    assert s.count(old) == 1, old
    s = s.replace(old, new)
s += "\nhost:\n  interfaces:\n    uplink: { primary: true, reason: \"the default route's\" }\n  kernel_args:\n    stowage.e2e: { value: \"1\", reason: \"the recovery system boots without it\" }\n  packages:\n    tcpdump: { reason: \"the bundle installs a package of the project's\" }\n"
open(path, "w").write(s)
# The volume the front publishes its CA in, which the api reads.
path = path.replace("stowage.yaml", "deploy/compose.yaml")
s = open(path).read()
for old, new in [
    ("      - uploads:/data/uploads\n", "      - uploads:/data/uploads\n      - ca:/etc/notes/ca:ro\n"),
    ("  uploads: {}\n", "  uploads: {}\n  ca: {}\n"),
]:
    assert s.count(old) == 1, old
    s = s.replace(old, new)
open(path, "w").write(s)
PY
"${S[@]}" lint
vm_names

step "ship beta with its disk (--disks): images, the OS bundle, its disk"
"${S[@]}" ship --channel beta --disks 2>"$WORK/ship.log" || { tail -50 "$WORK/ship.log"; fail "ship"; }
grep -q 'disk qcow2: tag .* → beta-1.0.0-qcow2' "$WORK/ship.log" && grep -q 'disk raw: tag .* → beta-1.0.0-raw' "$WORK/ship.log" || fail "the disks were not tagged"

step "the published notes-stow; the site's mirror, TLS of its own CA, on the address the VM reaches this host at"
vm_ctl beta-1.0.0
vm_mirror
mirror "$WORK/r1.oci.tar" --disks qcow2
grep -q '^Exported notes 1.0.0' "$WORK/r1.oci.tar.txt" || { cat "$WORK/r1.oci.tar.txt"; fail "export of 1.0.0"; }

step "this release on Docker and on a kind cluster, beside the VMs: each issues a kit before any VM exists"
DOCKER_OPERATOR=("${OPERATOR[@]}")
k8s_tools
NS=notes
start_kind
installer v1.sh beta-1.0.0
ctl v1.sh k8s install --no-input --set ADMIN_EMAIL=admin@notes.test --timeout 5m >"$WORK/k8s-install.log" 2>&1 || { cat "$WORK/k8s-install.log"; cluster_logs >&2; fail "k8s install beside the VMs"; }
# Its L4 route on the address the VM reaches this host at alone (an address of this host, as a bind
# input's must be): this host's 127.0.0.1:5432 may be another stack's.
docker_beside v1.sh --set "DB_BIND=$MIRROR_IP"
# A CA and a certificate of the routes' domains, which the second VM's kit carries.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$WORK/site-ca.key" -out "$WORK/site-ca.pem" -days 2 -subj "/CN=second site CA" \
	-addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign 2>/dev/null &&
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$WORK/site.key" -out "$WORK/site.csr" -subj /CN=app.notes.test 2>/dev/null &&
	openssl x509 -req -in "$WORK/site.csr" -CA "$WORK/site-ca.pem" -CAkey "$WORK/site-ca.key" -CAcreateserial -out "$WORK/site.pem" -days 2 \
		-extfile <(printf 'subjectAltName=DNS:app.notes.test,DNS:api.notes.test\nextendedKeyUsage=serverAuth\n') 2>/dev/null || fail "openssl: the second site's certificate"
SITE_TOKEN=$(token)
pem_given() { printf 'stowage-file:%s' "$(base64 -w0 "$1")"; }
{
	printf 'SITE_ADMIN: from-k8s@notes.test\nGIVE_DEPLOY_TOKEN: %s\n' "$SITE_TOKEN"
	printf 'GIVE_KIT_TLS_CERT: %s\nGIVE_KIT_TLS_KEY: %s\nGIVE_KIT_TLS_CA: %s\n' "$(pem_given "$WORK/site.pem")" "$(pem_given "$WORK/site.key")" "$(pem_given "$WORK/site-ca.pem")"
} >"$OP/vm-site.yaml"
chmod 600 "$OP/vm-site.yaml"
ctl v1.sh k8s kit create vm-site --name site-vm --answers vm-site.yaml -o vm-site.kit >"$WORK/kit-vm-site.log" 2>&1 || { cat "$WORK/kit-vm-site.log"; cluster_logs >&2; fail "k8s kit create of the second VM's kit"; }
kit_code "$WORK/kit-vm-site.log" "$OP/vm-site.code"
! grep -qF "$SITE_TOKEN" "$WORK/kit-vm-site.log" "$OP/vm-site.kit" || fail "the token in the clear"
"${DOCKER_CTL[@]}" kit create second-site --dir "$D" --name site-vm --set SITE_ADMIN=from-docker@notes.test -o "$WORK/docker.kit" >"$WORK/kit-docker.log" 2>&1 || { cat "$WORK/kit-docker.log"; stack_logs >&2; fail "kit create on Docker"; }
kit_code "$WORK/kit-docker.log" "$WORK/docker.code"

step "vm create --kit, of Kubernetes's kit: the first install takes the byo certificate and the secret over the pinned SSH, and neither is in the seed"
H_MAIN=$H ADDR_MAIN=$ADDR STATE_MAIN=$STATE
H=$H-kit ADDR=$H.$DOMAIN STATE=$XDG_STATE_HOME/$NAME/vms/$H
vm_cleanup
"$CTL" vm create --no-input --platform truenas --create opentofu --hostname "$H" --user ops --ssh-key "$KEY.pub" \
	--domains app=app.notes.test,api=api.notes.test --kit "$OP/vm-site.kit" --kit-code-file "$OP/vm-site.code" \
	--pool "$POOL" --dataset "$DATASET" --bridge "$BRIDGE" --cpus 2 --memory 4096 --disk 16 --host "$ADDR" --wait "${E2E_VM_WAIT:-30m}" \
	--registry "$SITE" --registry-ca "$MIRROR_CA" \
	>"$WORK/vm-kit.txt" 2>"$WORK/vm-kit.log" || { tail -80 "$WORK/vm-kit.log"; fail "vm create --kit"; }
grep -q "^The VM $H runs notes 1.0.0" "$WORK/vm-kit.txt" || { cat "$WORK/vm-kit.txt"; fail "vm create --kit did not report the install"; }
grep -q 'installing over SSH with ' "$WORK/vm-kit.log" || { tail -30 "$WORK/vm-kit.log"; fail "the kit's material did not go over SSH"; }
[[ ! -e $STATE/given.yaml ]] || fail "what the install took is left on this machine"
# The kit's plain input is in the seed and in what the VM's first boot kept, so the search below reads
# them; its secret, its certificate and its key are in neither, nor anywhere in the VM's state.
# Each as a line of its PEM and whole in the form the kit's answers carried it.
key_line=$(sed -n 2p "$WORK/site.key")
cert_line=$(sed -n 2p "$WORK/site.pem")
key_given=$(base64 -w0 "$WORK/site.key")
cert_given=$(base64 -w0 "$WORK/site.pem")
grep -aqF from-k8s@notes.test "$STATE/seed.iso" || fail "the seed does not read in the clear: the search below would see nothing"
on_vm sudo grep -rqF from-k8s@notes.test /oem || fail "/oem does not read in the clear: the search below would see nothing"
for v in "$SITE_TOKEN" "$key_line" "$cert_line" "$key_given" "$cert_given"; do
	! grep -rqaF -- "$v" "$STATE" || fail "what vm create gives over SSH is in the VM's state"
	! grep -qaF -- "$v" "$WORK/vm-kit.log" "$WORK/vm-kit.txt" || fail "what vm create gives over SSH reached its output"
	not_on_vm "$v" /oem
done
on_vm sudo cat /usr/local/notes/.env >"$WORK/vm-kit.env"
grep -qx "DEPLOY_TOKEN='\{0,1\}$SITE_TOKEN'\{0,1\}" "$WORK/vm-kit.env" || fail "the kit's secret input was not recorded"
grep -qx "ADMIN_EMAIL='\{0,1\}from-k8s@notes.test'\{0,1\}" "$WORK/vm-kit.env" || fail "the kit's input was not recorded"
# The kit's CA reaches the api, and the front serves its certificate: a client trusting that CA alone
# reaches both routes.
want=$(openssl x509 -in "$WORK/site-ca.pem" -noout -fingerprint -sha256)
deadline=$((SECONDS + 120))
until on_vm sudo docker exec "$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=api)" cat /etc/notes/ca/ca.crt >"$WORK/vm-kit-ca.crt" 2>/dev/null &&
	[[ $(openssl x509 -in "$WORK/vm-kit-ca.crt" -noout -fingerprint -sha256 2>/dev/null) == "$want" ]]; do
	((SECONDS < deadline)) || { cat "$WORK/vm-kit-ca.crt"; fail "the CA the api reads is not the kit's"; }
	sleep 5
done
# At the address SSH reaches the VM at: the name may give others.
# shellcheck disable=SC2016 # expanded on the VM
ip=$(on_vm 'set -- $SSH_CONNECTION; echo "$3"') && [[ -n $ip ]] || fail "the kit's VM's address"
[[ $ip != *:* ]] || ip="[$ip]"
deadline=$((SECONDS + 120))
until curl -fsS -o /dev/null --max-time 10 --cacert "$WORK/site-ca.pem" --resolve "app.notes.test:443:$ip" https://app.notes.test/ 2>"$WORK/vm-kit-curl.log" &&
	curl -fsS -o /dev/null --max-time 10 --cacert "$WORK/site-ca.pem" --resolve "api.notes.test:443:$ip" https://api.notes.test/healthz 2>"$WORK/vm-kit-curl.log"; do
	((SECONDS < deadline)) || {
		echo "--- at $ip; the name $ADDR gives:"
		getent ahosts "$ADDR" || true
		cat "$WORK/vm-kit-curl.log"
		# shellcheck disable=SC2016 # expanded on the VM
		on_vm 'sudo docker ps -a --format "{{.Names}} {{.Status}}"; sudo ss -tln; sudo nft list ruleset | grep -E "dport|policy"; sudo docker logs --tail 30 "$(sudo docker ps -aq --filter label=com.docker.compose.service=front)"' 2>&1 | cut -c1-300 || true
		fail "the front does not serve the kit's certificate"
	}
	sleep 5
done
"$CTL" vm destroy "$H" --yes >"$WORK/vm-kit-destroy.txt" 2>"$WORK/vm-kit-destroy.log" || { tail -50 "$WORK/vm-kit-destroy.log"; fail "vm destroy of the kit's VM"; }
[[ ! -e $STATE ]] || fail "the kit's VM's state is left"
H=$H_MAIN ADDR=$ADDR_MAIN STATE=$STATE_MAIN

step "vm create with OpenTofu on TrueNAS, from the mirror, IPv6 on: the disk, the seed, the VM; it boots and installs"
vm_cleanup
# SSH lets in this host alone: its address the VM reaches it at, and its global IPv6 addresses.
mapfile -t HOST6 < <(ip -6 -o addr show scope global | awk '$2 !~ /^(docker|br-|veth)/ && $0 !~ / (tentative|dadfailed|deprecated) / {sub(/\/.*/, "", $4); print $4}')
allow=$MIRROR_IP/32
for a in "${HOST6[@]}"; do allow+=",$a/128"; done
"$CTL" vm create --no-input --platform truenas --create opentofu --hostname "$H" --user ops --ssh-key "$KEY.pub" \
	--tls internal --domains app=app.notes.test,api=api.notes.test --set ADMIN_EMAIL=admin@notes.test --set "DB_SOURCES=$MIRROR_IP/32" \
	--pool "$POOL" --dataset "$DATASET" --bridge "$BRIDGE" --cpus 2 --memory 4096 --disk 16 --host "$ADDR" --wait "${E2E_VM_WAIT:-30m}" \
	--registry "$SITE" --registry-ca "$MIRROR_CA" --ssh-allow "$allow" --ipv6 --timezone Europe/Paris \
	>"$WORK/vm-create.txt" 2>"$WORK/vm-create.log" || { tail -80 "$WORK/vm-create.log"; fail "vm create"; }
grep -q "^The VM $H runs notes 1.0.0" "$WORK/vm-create.txt" || { cat "$WORK/vm-create.txt"; fail "vm create did not report the install"; }
for f in main.tf variables.tf outputs.tf versions.tf terraform.tfvars.json terraform.tfstate seed.iso; do
	[[ -f $STATE/$f ]] || fail "no $f in the VM's state"
done
! grep -rqF -- "$TRUENAS_API_KEY" "$STATE" || fail "the API key is in the VM's state"

step "on the VM: the install, its status, its services"
on_vm notes-stow status >"$WORK/status.txt" || fail "status over SSH"
grep -q '^installed  notes 1.0.0 (revision 1)' "$WORK/status.txt" && grep -q '^channel    beta' "$WORK/status.txt" ||
	{ cat "$WORK/status.txt"; fail "status does not show the install"; }
# The machine interface over SSH: describe reads the release the OS carries (a secret input's
# default never printed; no channel, as the OS bundle serves every channel it is tagged on);
# status --json the install, no release replaced yet.
on_vm sudo notes-stow describe --json >"$WORK/vm-describe.json" 2>"$WORK/vm-describe.log" || { cat "$WORK/vm-describe.log"; fail "describe --json on the VM"; }
jq -se 'length == 1 and (.[0] | .format == 1 and .name == "notes" and .short_name == "notes" and .release == {version: "1.0.0", revision: 1} and .channel == "" and
	(.shapes | index("kairos") != null) and (.inputs | map(.key) | index("ADMIN_EMAIL") != null and index("DB_SOURCES") != null) and
	(.inputs[] | select(.key == "DEPLOY_TOKEN") | .secret and (has("default") | not)) and
	.first_install_only == ["address", "registry", "registry_ca", "channel"])' "$WORK/vm-describe.json" >/dev/null ||
	{ cat "$WORK/vm-describe.json"; fail "the VM's describe --json does not describe 1.0.0, its inputs (no secret's default) and the first-install-only settings"; }
vm_status_json "$WORK/vm-status1.json" 1.0.0 null
healthy_postgres "after the install"
# A healthy boot logs no error: the journal at priority err holds nothing of this boot.
# Its standard output alone: SSH's banner comes on its standard error.
on_vm sudo journalctl -b -p err --no-pager -q >"$WORK/journal-err.txt" 2>"$WORK/journal-err.log" || { cat "$WORK/journal-err.log"; fail "journalctl on the VM"; }
# The test's own SSH logins are not the boot's: systemd's user manager, ending one, may find
# logind released the session already (pam_systemd's ReleaseSession, NoSuchSession).
grep -vE '\(sd-pam\)\[[0-9]+\]: pam_systemd\(systemd-user:session\): Varlink call io\.systemd\.Login\.ReleaseSession failed: io\.systemd\.Login\.NoSuchSession$' "$WORK/journal-err.txt" >"$WORK/journal-err-boot.txt" || true
[[ ! -s $WORK/journal-err-boot.txt ]] || { cat "$WORK/journal-err-boot.txt"; fail "the VM's boot logged errors"; }
on_vm sudo -n true || fail "the administrator has no passwordless sudo"
on_vm id -nG | grep -qw admin || fail "the administrator is not in the admin group"

step "on the VM: the firewall lets SSH in from this host alone, and the front's ports; the time zone"
on_vm sudo nft list table inet notes >"$WORK/nft.txt" 2>&1 || { cat "$WORK/nft.txt"; fail "the firewall has no table"; }
grep -q 'hook input .*policy drop;' "$WORK/nft.txt" && grep -q "ip saddr $MIRROR_IP tcp dport 22 accept" "$WORK/nft.txt" &&
	grep -q 'tcp dport { 80, 443 } accept' "$WORK/nft.txt" && ! grep -qx '[[:space:]]*tcp dport 22 accept' "$WORK/nft.txt" ||
	{ cat "$WORK/nft.txt"; fail "the firewall is not the bundle's, with SSH from $MIRROR_IP and the front's ports"; }
for a in "${HOST6[@]}"; do
	grep -q "ip6 saddr $a tcp dport 22 accept" "$WORK/nft.txt" || { cat "$WORK/nft.txt"; fail "the firewall does not let SSH in from $a"; }
done
on_vm cat /proc/cmdline | grep -qw 'stowage.e2e=1' || fail "the VM booted without the project's kernel argument (host.kernel_args)"
on_vm tcpdump --version >/dev/null 2>&1 || fail "tcpdump (host.packages) is not on the VM"
[[ $(on_vm readlink /etc/localtime) == */Europe/Paris && $(on_vm date +%Z) =~ ^CES?T$ ]] || fail "the VM is not in Europe/Paris's time zone"

step "on the VM: nothing listens beyond loopback but SSH, DHCP and the stack's ports, no account logs in with a password, root logs in by no password, no console autologin"
# beyond_loopback <ss -tulpn output> <allowed proto/port>…: the sockets listening on an address
# other than loopback's, as proto/port, that the list leaves out (none: success).
beyond_loopback() {
	local ss=$1
	shift
	awk 'NR > 1 { a = $5; p = a; sub(/.*:/, "", p); sub(/:[^:]*$/, "", a); gsub(/[][]/, "", a); i = ""
		if (a ~ /%/) { i = a; sub(/.*%/, "", i); sub(/%.*/, "", a) }
		if (a ~ /^127\./ || a == "::1" || i == "lo") next; print $1 "/" p }' "$ss" | sort -u |
		grep -vxF -f <(printf '%s\n' "$@") && return 1
	return 0
}
on_vm sudo ss -tulpn >"$WORK/ss.txt" || fail "ss on the VM"
# SSH, DHCP's clients, the front's ports (HTTP/3 among them) and the L4 route's: the purged
# packages' services (rpcbind, snmpd, lldpd, iSCSI) and Kairos's agents listen nowhere.
beyond_loopback "$WORK/ss.txt" tcp/22 tcp/80 tcp/443 udp/443 tcp/5432 udp/68 udp/546 ||
	{ cat "$WORK/ss.txt"; fail "the VM listens beyond loopback on more than SSH, DHCP and the stack's ports"; }
! grep -E 'kairos-agent|edgevpn' "$WORK/ss.txt" || { cat "$WORK/ss.txt"; fail "a Kairos agent listens on the VM"; }
on_vm sudo passwd -S -a >"$WORK/passwd.txt" || fail "passwd -S on the VM"
! awk '$2 == "P" {print; found = 1} END {exit !found}' "$WORK/passwd.txt" || fail "an account of the VM has a password"
on_vm sudo sshd -T >"$WORK/sshd.txt" || fail "sshd -T on the VM"
grep -qxE 'permitrootlogin (no|prohibit-password|without-password)' "$WORK/sshd.txt" && grep -qx 'passwordauthentication no' "$WORK/sshd.txt" ||
	{ grep -E '^(permitrootlogin|passwordauthentication) ' "$WORK/sshd.txt"; fail "sshd lets root or a password in"; }
! on_vm "systemctl cat serial-getty@ttyS0.service getty@tty1.service 2>/dev/null" | grep -qi autologin || fail "a console logs in by itself"
# Docker's forward policy drops what no rule of its admits, for IPv4 and (--ipv6) IPv6.
for t in iptables ip6tables; do
	[[ $(on_vm sudo $t -S FORWARD | head -1) == '-P FORWARD DROP' ]] || { on_vm sudo $t -S FORWARD; fail "$t's FORWARD policy is not DROP"; }
done
# /oem holds the boot's state alone: the user-data kept, GRUB's environment, the seed host key's
# retirement and the file system's lost+found; no cloud-config lands there beside them.
on_vm sudo ls -A /oem >"$WORK/oem.txt" || fail "ls /oem on the VM"
! grep -vxE '95_userdata|grubenv|lost\+found|notes-seed-host-key\.retired' "$WORK/oem.txt" ||
	{ on_vm sudo ls -la /oem; fail "/oem holds more than the user-data, grubenv and the host key's retirement"; }
# The first boot keeps the cloud-config it pulled as /oem/95_userdata, which no later boot pulls
# again (the upgrade's boot is judged against it below): a changed user-data is never applied.
userdata=$(on_vm sudo stat -c "'%Y %s'" /oem/95_userdata) || fail "the first boot kept no user-data (/oem/95_userdata)"

step "on the VM: the boot tuned Docker's path (the release's default profile), and the containers as on the host"
on_vm systemctl is-active notes-tune.service >/dev/null || { on_vm sudo journalctl -b -u notes-tune.service --no-pager | tail -30; fail "the tuning unit did not run"; }
grep -q '^tune       default (release)$' "$WORK/status.txt" || { cat "$WORK/status.txt"; fail "status does not show the release's default profile"; }
on_vm notes-stow tune --check >"$WORK/tune.txt" 2>&1 || { cat "$WORK/tune.txt"; fail "the VM's tune --check finds settings that differ"; }
grep -q '^this host: .* the default profile$' "$WORK/tune.txt" && ! grep -q 'rmem_max' "$WORK/tune.txt" || { cat "$WORK/tune.txt"; fail "the VM's tune does not report the default profile alone"; }
(($(on_vm cat /proc/sys/net/netfilter/nf_conntrack_max) > 65536)) || fail "the boot did not size the connection table"
[[ $(on_vm cat /proc/sys/net/ipv4/tcp_congestion_control) != bbr ]] || fail "the default profile set BBR"
on_vm test ! -e /etc/sysctl.d/60-stowage-net.conf || fail "the boot wrote a sysctl file"
tuned=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=api)
[[ $(on_vm sudo docker exec "$tuned" cat /proc/sys/net/core/somaxconn) == "$(on_vm cat /proc/sys/net/core/somaxconn)" ]] || fail "the api container's somaxconn is not the VM's"
on_vm sudo docker inspect --format "'{{range .HostConfig.Ulimits}}{{.Name}} {{.Soft}} {{.Hard}}{{println}}{{end}}'" "$tuned" | grep -qE '^nofile 65536 [0-9]{6,}$' || fail "the api container is not given the VM's open files"
on_vm sudo docker inspect --format "'{{json .HostConfig.LogConfig}}'" "$tuned" | grep -q '"max-size":"20m"' || fail "the api container's logs do not rotate on the default profile"

# The resolver file is the link to resolved's list of the lease's servers, at every boot, and a
# container on the host's network reads them (Docker copies the host's file into it).
resolv_ok() {
	[[ $(on_vm readlink /etc/resolv.conf) == ../run/systemd/resolve/resolv.conf ]] || { on_vm ls -l /etc/resolv.conf; fail "/etc/resolv.conf is not the link to resolved's servers ($1)"; }
	on_vm grep -q "'^nameserver '" /etc/resolv.conf || { on_vm resolvectl dns; fail "/etc/resolv.conf lists no server ($1)"; }
	local img
	img=$(on_vm sudo docker inspect --format "'{{.Config.Image}}'" "$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=api)")
	on_vm sudo docker run --rm --network host --entrypoint cat "$img" /etc/resolv.conf | grep -q '^nameserver ' || fail "a container on the host's network lists no DNS server ($1)"
}
resolv_ok "first boot"

step "the operator raises the VM to the network profile: a re-run tunes it at once, and the containers with it"
on_vm sudo notes-stow install --no-input --tune-profile network >"$WORK/retune.txt" 2>&1 || { cat "$WORK/retune.txt"; fail "the re-run with --tune-profile network"; }
grep -q 'tuning: the network profile, the operator' "$WORK/retune.txt" && grep -q 'tuned by the network profile' "$WORK/retune.txt" || { cat "$WORK/retune.txt"; fail "the re-run did not tune the VM"; }
[[ $(on_vm cat /proc/sys/net/ipv4/tcp_congestion_control) == bbr && $(on_vm cat /proc/sys/net/core/default_qdisc) == fq ]] || fail "the re-run did not set BBR and fq"
(($(on_vm cat /proc/sys/net/core/rmem_max) >= 16777216 && $(on_vm cat /proc/sys/net/core/netdev_budget) >= 600)) || {
	on_vm sudo sysctl net.core.rmem_max net.core.netdev_budget
	fail "the re-run did not give the network profile's ceilings"
}
[[ $(on_vm cat /proc/sys/net/ipv4/ip_local_port_range | awk '{print $1}') == 10000 ]] || fail "the network profile's local ports do not start at 10000"
on_vm notes-stow status | grep -q '^tune       network (operator)$' || fail "status does not show the operator's network profile"
tuned=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=api)
[[ $(on_vm sudo docker exec "$tuned" cat /proc/sys/net/ipv4/tcp_rmem | awk '{print $3}') -ge 16777216 && $(on_vm sudo docker exec "$tuned" cat /proc/sys/net/ipv4/tcp_tw_reuse) == 1 ]] ||
	fail "the api container's settings are not the network profile's"

step "on the VM: the L4 route's port, open to its allowlist's sources alone (this host, over IPv4)"
# reach <address> <port>: a TCP connection opens within 8 s.
reach() { python3 -c 'import socket, sys; socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=8).close()' "$@" 2>/dev/null; }
vm4=$(getent ahostsv4 "$ADDR" | awk '{print $1; exit}') # `getent hosts` gives IPv6 first
[[ -n $vm4 ]] || fail "$ADDR has no IPv4 address"
grep -q "tcp dport 5432 ip saddr $MIRROR_IP accept" "$WORK/nft.txt" && grep -qE 'iifname "br-\*" tcp dport (\{ )?5432( \})? accept' "$WORK/nft.txt" &&
	! grep -q 'ct original proto-dst' "$WORK/nft.txt" ||
	{ cat "$WORK/nft.txt"; fail "the firewall does not open the L4 route's port to $MIRROR_IP and the stack's bridges alone"; }
on_vm test ! -e /usr/local/notes/nftables.d/notes-l4.nft || fail "the forward chain's allowlists are written"
[[ $(on_vm sudo docker inspect --format "'{{.HostConfig.NetworkMode}}'" "$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=postgres)") == host ]] ||
	fail "the L4 route's service (postgres) is not on the host's network"
reach "$vm4" 5432 || { on_vm sudo nft list chain inet notes input; on_vm sudo ss -ltn; fail "the L4 route's port ($vm4:5432) is not reachable from this host ($MIRROR_IP), which its allowlist holds"; }

step "on the VM: the api service reaches the host daemon's app socket with the install's token"
on_vm sudo cat /usr/local/notes/.env >"$WORK/vm.env"
host_token=$(sed -n "s/^STOWAGE_HOST_TOKEN='\{0,1\}\([0-9a-f]*\).*/\1/p" "$WORK/vm.env")
[[ ${#host_token} == 64 ]] || fail "no host token recorded on the VM"
app_get() { on_vm curl -s -o /dev/null -w '%{http_code}' --unix-socket /run/notes/app/app.sock "$@" http://host/v1/status; }
[[ $(app_get) == 401 ]] || fail "the app socket answered without the token"
[[ $(app_get -H "'Authorization: Bearer $host_token'") == 200 ]] || fail "the app socket refused the install's token"
[[ $(on_vm curl -s -o /dev/null -w '%{http_code}' --unix-socket /run/notes/app/app.sock -X POST -d "'{}'" -H "'Authorization: Bearer $host_token'" http://host/v1/secrets/export) == 403 ]] ||
	fail "the app socket serves secrets export"
api=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=api)
on_vm sudo docker inspect --format "'{{range .Mounts}}{{.Source}}:{{.Destination}}:{{.RW}}{{println}}{{end}}'" "$api" |
	grep -qx '/run/notes/app:/run/stowage-host:false' || fail "the api service does not mount the app directory read-only"

step "on the VM: the install reads the mirror, whose CA the VM trusts"
grep -qx "STOWAGE_REGISTRY='$SITE'" "$WORK/vm.env" || fail "the install does not record the mirror"
on_vm curl -sf -o /dev/null "https://$MIRROR/v2/" || fail "the VM does not trust the mirror's CA"

step "the disk: the app's 16 GiB on the persistent partition, Kairos's partitions beside it"
size=$(on_vm "df -BG --output=size /usr/local | tail -1" | tr -dc 0-9)
((size >= 15)) || { on_vm "df -h; lsblk" || true; fail "/usr/local holds ${size}G, not the app's 16 GiB"; }

step "vm seed --json from the mirror: a new VM's first-boot material, no VM made; a secret input given to it is refused"
# shellcheck disable=SC2054 # --domains takes its routes comma-separated
SEED=(--platform truenas --hostname "$H-seed" --user ops --ssh-key "$KEY.pub" --tls internal --domains app=app.notes.test,api=api.notes.test
	--set ADMIN_EMAIL=seed@notes.test --set "DB_SOURCES=$MIRROR_IP/32" --registry "$SITE" --registry-ca "$MIRROR_CA")
"$CTL" vm seed --json "${SEED[@]}" >"$WORK/seed.json" 2>"$WORK/seed.log" || { cat "$WORK/seed.log"; fail "vm seed --json"; }
# The project's one secret input is optional: no required secret input waits, so waiting is left out.
jq -se --arg site "$SITE" 'length == 1 and (.[0] | .format == 1 and .platform == "truenas" and
	(.user_data | startswith("#cloud-config") and contains("seed@notes.test")) and
	(.known_hosts_line | startswith("vm-seed-host-key ")) and (.fingerprint | startswith("SHA256:")) and
	(.host_key_private | startswith("-----BEGIN OPENSSH PRIVATE KEY-----")) and
	.disk.truenas.format == "qcow2" and (.disk.truenas.image | startswith($site + "/notes-disk@sha256:")) and (has("waiting") | not))' "$WORK/seed.json" >/dev/null ||
	{ jq 'del(.host_key_private, .user_data)' "$WORK/seed.json"; fail "vm seed --json is not the first-boot material of a truenas VM from the mirror"; }
[[ ! -e $XDG_STATE_HOME/$NAME/vms/$H-seed ]] || fail "vm seed wrote a VM's state"
# Its known_hosts line, its fingerprint and its private key are the same key.
jq -r .known_hosts_line "$WORK/seed.json" >"$WORK/seed.known_hosts"
(umask 077; jq -r .host_key_private "$WORK/seed.json" >"$WORK/seed.key")
[[ $(ssh-keygen -lf "$WORK/seed.known_hosts" | awk '{print $2}') == "$(jq -r .fingerprint "$WORK/seed.json")" ]] ||
	{ ssh-keygen -lf "$WORK/seed.known_hosts"; fail "the seed's fingerprint is not its known_hosts line's"; }
[[ $(ssh-keygen -y -f "$WORK/seed.key" | awk '{print $1, $2}') == "$(awk '{print $2, $3}' "$WORK/seed.known_hosts")" ]] || fail "the seed's private key is not its known_hosts line's"
rm -f "$WORK/seed.key"
seed_token=$(token)
rc=0; "$CTL" vm seed --json "${SEED[@]}" --set "DEPLOY_TOKEN=$seed_token" >"$WORK/seed2.json" 2>"$WORK/seed2.log" || rc=$?
[[ $rc == 3 && ! -s $WORK/seed2.json ]] && grep -q 'DEPLOY_TOKEN: a seed carries no secret input' "$WORK/seed2.log" ||
	{ cat "$WORK/seed2.log"; fail "vm seed given a secret input exited $rc, not 3 naming it with nothing on standard output"; }
! grep -qF -- "$seed_token" "$WORK/seed2.json" "$WORK/seed2.log" "$WORK/seed.json" || fail "the secret input given to vm seed reached its output"

step "audit off the VM: the operator's notes-stow reads the VM's release's SBOMs from the mirror, by its version alone, and scans them"
grype_db
scanner_fresh "$SITE"
"$CTL" audit --version 1.0.0 --channel beta --registry "$SITE" --registry-ca "$MIRROR_CA" --db "$GRYPE_DB" --json >"$WORK/audit.json" 2>"$WORK/audit.log" ||
	{ cat "$WORK/audit.log" "$WORK/audit.json"; fail "audit of the VM's release"; }
[[ $(jq '[.subjects[] | select(.error)] | length' "$WORK/audit.json") == 0 && $(jq '[.subjects[] | select(.name == "os" or .name == "disk-qcow2")] | length' "$WORK/audit.json") == 2 ]] ||
	{ cat "$WORK/audit.json"; fail "not every artifact of the VM's release was audited"; }
grep -q "pulling $SITE/notes-audit@" "$WORK/audit.log" || { cat "$WORK/audit.log"; fail "the scanner was not pulled from the mirror"; }
# The mirror carries the qcow2 disk alone: the raw one is left out, not failed.
grep -q "^disk-raw sha256:[0-9a-f]* is not in $SITE (a mirror carries the shapes and the disk formats its export named): not read$" "$WORK/audit.log" ||
	{ cat "$WORK/audit.log"; fail "the raw disk the mirror lacks was not left out"; }
"$CTL" sbom export --version 1.0.0 --channel beta --registry "$SITE" --registry-ca "$MIRROR_CA" "$WORK/sbom" >"$WORK/sbom.txt" 2>&1 || { cat "$WORK/sbom.txt"; fail "sbom export"; }
jq -e '[.artifacts[] | select(.file)] | length > 0 and all(.[]; .error == null)' "$WORK/sbom/index.json" >/dev/null &&
	jq -e '.spdxVersion' "$WORK/sbom/os.spdx.json" >/dev/null || { cat "$WORK/sbom/index.json"; fail "the exported SBOMs"; }
# The kernel is its distribution's packages: no SBOM lists it by upstream's version, nor its
# modules (syft's SPDX IDs carry the package type: linux-kernel, linux-kernel-module).
jq -e '[.packages[] | select(.name == "linux-kernel" or (.SPDXID | test("^SPDXRef-Package-linux-kernel")))] | length == 0' "$WORK/sbom/os.spdx.json" >/dev/null ||
	fail "the OS bundle's SBOM lists the kernel itself"
! on_vm 'sudo test -e /usr/local/notes/.stowage-grype' || fail "the VM holds a scanner's database"

step "the front serves HTTPS from the VM"
ip=$(getent hosts "$ADDR" | awk '{print $1; exit}')
code=$(curl -sk -o /dev/null -w '%{http_code}' --resolve "app.notes.test:443:$ip" https://app.notes.test/)
[[ $code =~ ^[23] ]] || fail "https://app.notes.test/ on the VM answered $code"

step "vm kit on the VM over the pinned SSH, through the daemon: the kit written here, taken by vm kit take, by a Docker install and by a release on Kubernetes; Docker's by the VM; vm kit check"
VMKIT=(--user ops --ssh-key "$KEY.pub" --host "$ADDR")
"$CTL" vm kit create "$H" second-site --name site-2 --set SITE_ADMIN=second@notes.test -o "$OP/vm.kit" --no-input "${VMKIT[@]}" >"$WORK/kit.log" 2>&1 || { cat "$WORK/kit.log"; fail "vm kit create"; }
code=$(sed -n 's/^code  //p' "$WORK/kit.log")
[[ -n $code && -s $OP/vm.kit ]] || { cat "$WORK/kit.log"; fail "vm kit create wrote no kit or printed no code"; }
[[ $(stat -c %a "$OP/vm.kit") == 600 ]] || fail "the kit is not 0600"
(umask 077; printf '%s\n' "$code" >"$OP/vm.code")
not_on_vm "$code" /tmp
not_on_vm "$code" /home/ops
"$CTL" vm kit take "$H" "$OP/vm.kit" --kit-code-file "$OP/vm.code" --no-input "${VMKIT[@]}" >"$WORK/kit-take.log" 2>&1 || { cat "$WORK/kit-take.log"; fail "vm kit take"; }
grep -qF 'changing ADMIN_EMAIL: ' "$WORK/kit-take.log" || { cat "$WORK/kit-take.log"; fail "the change was not logged"; }
"$CTL" vm kit check "$H" second-site site-2 "${VMKIT[@]}" | grep -q 'proven$' || fail "vm kit check of a site"
# --known-hosts-line: the VM's own key (the one vm create recorded) given as one line, in place of
# the record, reaches it; another key's line is refused by ssh before it logs in.
real=$(awk 'NF >= 3 && $1 !~ /^#/ {print $2, $3; exit}' "$STATE/known_hosts")
[[ -n $real ]] || { cat "$STATE/known_hosts"; fail "vm create recorded no host key"; }
"$CTL" vm kit check "$H" second-site site-2 "${VMKIT[@]}" --known-hosts-line "$ADDR $real" >"$WORK/kit-line.txt" 2>"$WORK/kit-line.log" && grep -q 'proven$' "$WORK/kit-line.txt" ||
	{ cat "$WORK/kit-line.txt" "$WORK/kit-line.log"; fail "vm kit check with --known-hosts-line of the VM's key"; }
ssh-keygen -q -t ed25519 -N '' -C '' -f "$WORK/wrong-host-key" || fail "ssh-keygen"
rc=0; "$CTL" vm kit check "$H" second-site site-2 "${VMKIT[@]}" --known-hosts-line "$ADDR $(awk '{print $1, $2}' "$WORK/wrong-host-key.pub")" >"$WORK/kit-wrong.txt" 2>"$WORK/kit-wrong.log" || rc=$?
[[ $rc != 0 ]] && ! grep -q 'proven$' "$WORK/kit-wrong.txt" && grep -q 'Host key verification failed' "$WORK/kit-wrong.log" ||
	{ cat "$WORK/kit-wrong.txt" "$WORK/kit-wrong.log"; fail "vm kit check with another key's --known-hosts-line exited $rc, not refused by ssh's host key check"; }
rc=0; "$CTL" vm kit check "$H" second-site site-unknown "${VMKIT[@]}" >/dev/null 2>&1 || rc=$?
[[ $rc == 2 ]] || fail "vm kit check of an unknown site exited $rc, not 2"
"${DOCKER_CTL[@]}" --dir "$D" --no-input --kit "$OP/vm.kit" --kit-code-file "$OP/vm.code" >"$WORK/kit-vm-docker.log" 2>&1 || { cat "$WORK/kit-vm-docker.log"; stack_logs >&2; fail "--kit of the VM's kit on Docker"; }
grep -q "^ADMIN_EMAIL='second@notes.test'\$" "$D/.env" || fail "the VM's kit did not replace Docker's recorded input"
ctl v1.sh k8s upgrade --kit vm.kit --kit-code-file vm.code --no-input --timeout 5m >"$WORK/kit-vm-k8s.log" 2>&1 || { cat "$WORK/kit-vm-k8s.log"; cluster_logs >&2; fail "k8s upgrade --kit of the VM's kit"; }
[[ $(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.data.ADMIN_EMAIL}' | base64 -d) == second@notes.test ]] || fail "the VM's kit did not replace Kubernetes's recorded input"
"$CTL" vm kit take "$H" "$WORK/docker.kit" --kit-code-file "$WORK/docker.code" --no-input "${VMKIT[@]}" >"$WORK/kit-docker-vm.log" 2>&1 || { cat "$WORK/kit-docker-vm.log"; fail "vm kit take of Docker's kit"; }
on_vm sudo cat /usr/local/notes/.env >"$WORK/vm-docker.env" || fail "reading the VM's .env"
grep -qx "ADMIN_EMAIL='\{0,1\}from-docker@notes.test'\{0,1\}" "$WORK/vm-docker.env" || fail "Docker's kit did not replace the VM's recorded input"
# The cluster and the Docker install have done their part: the long steps left run without them.
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
"${DOCKER_CTL[@]}" uninstall --dir "$D" --data --yes >"$WORK/docker-site-gone.log" 2>&1 || { cat "$WORK/docker-site-gone.log"; fail "uninstall of the Docker install beside"; }

step "reset-db on the VM: the data wiped and provisioned again, without a reboot"
on_vm sudo docker exec "$api" touch /data/uploads/marker || fail "no marker written in the uploads volume"
pw=$(on_vm sudo grep '^ADMIN_PASSWORD=' /usr/local/notes/provision.env) || fail "the VM's install recorded no ADMIN_PASSWORD"
boot=$(boot_id)
on_vm sudo notes-stow reset-db --yes >"$WORK/reset.txt" 2>&1 || { cat "$WORK/reset.txt"; fail "reset-db"; }
grep -q 'deleting the volume ' "$WORK/reset.txt" || { cat "$WORK/reset.txt"; fail "reset-db deleted no volume"; }
[[ $(boot_id) == "$boot" ]] || fail "reset-db rebooted the VM"
healthy_postgres "after reset-db"
api=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=api)
if on_vm sudo docker exec "$api" test -e /data/uploads/marker; then fail "the uploads volume was not wiped"; fi
[[ $(on_vm sudo grep '^ADMIN_PASSWORD=' /usr/local/notes/provision.env) != "$pw" ]] || fail "the provision hook was given what it returned before the reset"

step "backups on the VM: set over SSH, the timer on their schedule; one taken now to a WebDAV server on this host, decrypted with the key"
age_key
start_dav "$MIRROR_IP"
on_vm sudo notes-stow backup config --destination dav:backups --rclone-config - --recipient "$AGE_RECIPIENT" --schedule "'*-*-* 04:30:00 UTC'" \
	<"$DAV_CONF" >"$WORK/backup-config.txt" 2>&1 || { cat "$WORK/backup-config.txt"; fail "backup config"; }
on_vm systemctl show notes-backup.timer --property TimersCalendar --value | grep -q '04:30:00' || fail "the timer does not take the schedule"
pg=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=postgres)
# Partitioned, with a key: the restore brings it back whole, its partition's inherited key with it.
on_vm sudo docker exec "$pg" psql -U notes -d notes -c "'CREATE TABLE e2e_backup (n int PRIMARY KEY) PARTITION BY RANGE (n); CREATE TABLE e2e_backup_all PARTITION OF e2e_backup DEFAULT; INSERT INTO e2e_backup VALUES (1)'" >/dev/null || fail "a table to back up"
on_vm sudo docker exec "$api" sh -c "'echo at-the-backup >/data/uploads/e2e.txt'" || fail "a file to back up in the uploads volume"
on_vm sudo notes-stow backup run >"$WORK/backup-run.txt" 2>&1 || { cat "$WORK/backup-run.txt"; fail "backup run"; }
backups=("$WORK"/dav/backups/notes-full-*-1.0.0-r1.pgdump.age)
[[ ${#backups[@]} == 1 && -s ${backups[0]} ]] || { ls -la "$WORK/dav/backups"; fail "no backup in the destination"; }
check_backup "${backups[0]}" "$PG" e2e_backup_all
"$BIN/age" -d -i "$AGE_KEY" "${backups[0]%.pgdump.age}.uploads.tar.age" | tar -t | grep -qx './e2e.txt' || fail "the backup holds no archive of the uploads volume"
on_vm notes-stow backup status | grep -q "^last  .*${backups[0]##*/}" || fail "backup status does not show the backup"

step "restore on the VM: the backup loaded back over newer data; the secrets exported, encrypted to the operator's key"
on_vm sudo docker exec "$pg" psql -U notes -d notes -c "'INSERT INTO e2e_backup VALUES (2)'" >/dev/null || fail "a row after the backup"
on_vm sudo docker exec "$api" sh -c "'echo since >/data/uploads/e2e.txt && echo later >/data/uploads/later.txt'" || fail "files after the backup"
on_vm sudo notes-stow restore "${backups[0]##*/}" --yes <"$AGE_KEY" >"$WORK/restore.txt" 2>&1 || { cat "$WORK/restore.txt"; fail "restore"; }
grep -q "restored ${backups[0]##*/}" "$WORK/restore.txt" || { cat "$WORK/restore.txt"; fail "restore did not say it restored"; }
healthy_postgres "after restore"
pg=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=postgres)
[[ $(on_vm sudo docker exec "$pg" psql -U notes -d notes -Atc "'SELECT count(*) FROM e2e_backup'") == 1 ]] || fail "the restore did not bring the backup's data back"
api=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=api)
[[ $(on_vm sudo docker exec "$api" sh -c "'cat /data/uploads/e2e.txt; stat -c %u /data/uploads/e2e.txt; test -e /data/uploads/later.txt || echo gone'") == $'at-the-backup\n1000\ngone' ]] ||
	{ on_vm sudo docker exec "$api" ls -lan /data/uploads; fail "the restore did not bring the uploads volume back as it was"; }
# A backup cut short (named as one, a later time): its restore fails and the data stays as it was.
cut=${backups[0]##*/}; cut=${cut/-full-????????T??????Z-/-full-29991231T235959Z-}
head -c $(($(stat -c %s "${backups[0]}") / 2)) "${backups[0]}" >"$WORK/dav/backups/$cut"
# Its archive whole beside it (the dump is what is cut), sent through the server, whose listing a
# file written behind its back reaches only once its directory cache expires.
dav_rclone() { docker run --rm --network host --user "$(id -u):$(id -g)" -v "$WORK:$WORK" "$RCLONE" --config "$DAV_CONF" "$@"; }
dav_rclone copyto "${backups[0]%.pgdump.age}.uploads.tar.age" "dav:backups/${cut%.pgdump.age}.uploads.tar.age" || fail "the cut backup's archive"
on_vm sudo docker exec "$pg" psql -U notes -d notes -c "'INSERT INTO e2e_backup VALUES (3)'" >/dev/null || fail "a row before the restore cut short"
on_vm sudo docker exec "$api" touch /data/uploads/since-cut || fail "a file before the restore cut short"
if on_vm sudo notes-stow restore "$cut" --yes <"$AGE_KEY" >"$WORK/restore-cut.txt" 2>&1; then cat "$WORK/restore-cut.txt"; fail "a backup cut short restored"; fi
grep -q 'the database is as it was' "$WORK/restore-cut.txt" || { cat "$WORK/restore-cut.txt"; fail "the restore cut short did not fail loading the dump"; }
healthy_postgres "after the restore cut short"
pg=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=postgres)
[[ $(on_vm sudo docker exec "$pg" psql -U notes -d notes -Atc "'SELECT count(*) FROM e2e_backup'") == 2 ]] || fail "the restore cut short changed the data"
[[ $(on_vm sudo docker exec "$pg" psql -U notes -d postgres -Atc "\"SELECT count(*) FROM pg_database WHERE datname LIKE 'notes_stowage%'\"") == 0 ]] || fail "the restore cut short left its database"
api=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=api)
[[ $(on_vm sudo docker exec "$api" sh -c "'cat /data/uploads/e2e.txt; ls -A /data/uploads | grep -c -e since-cut -e stowage'") == $'at-the-backup\n1' ]] || fail "the restore cut short changed the uploads volume, or left its staging"
rm -f "$WORK/dav/backups/$cut"
dav_rclone deletefile "dav:backups/${cut%.pgdump.age}.uploads.tar.age" || fail "removing the cut backup's archive"
on_vm notes-stow secrets export --recipient "$AGE_RECIPIENT" -o - >"$WORK/secrets.age" 2>"$WORK/secrets.log" || { cat "$WORK/secrets.log"; fail "secrets export"; }
"$BIN/age" -d -i "$AGE_KEY" "$WORK/secrets.age" >"$WORK/secrets.yaml" || fail "the exported secrets do not decrypt with the key"
grep -qx 'project: notes' "$WORK/secrets.yaml" && grep -q "$host_token" "$WORK/secrets.yaml" || fail "the exported secrets are not the install's"
rm -f "$WORK/secrets.yaml"

step "a re-run on the VM changes the L4 route's allowlist: this host is refused"
on_vm sudo notes-stow install --no-input --set DB_SOURCES=192.0.2.1/32 >"$WORK/allow.txt" 2>&1 || { cat "$WORK/allow.txt"; fail "the re-run changing DB_SOURCES"; }
! reach "$vm4" 5432 || fail "the L4 route's port still answers this host, which its allowlist no longer holds"

step "ship beta 1.0.1, which names a second NIC, carry it through the mirror; the VM upgrades from it, and its health gate keeps it"
sed -i 's/^    uplink: { primary: true, reason: "the default route'"'"'s" }$/&\n    edge: { mtu: 1400, reason: "a NIC vm attach attaches" }/' "$P/stowage.yaml"
grep -q '^    edge: ' "$P/stowage.yaml" || fail "no edge NIC in the project"
release 1.0.1
on_vm notes-stow updates >"$WORK/updates.txt" 2>&1 || { cat "$WORK/updates.txt"; fail "updates"; }
grep -q '1.0.1 (revision 1) on beta' "$WORK/updates.txt" || { cat "$WORK/updates.txt"; fail "updates does not offer 1.0.1 from the mirror"; }
boot=$(boot_id)
# Under --json: its Result alone on standard output, the release installed as its job schedules the
# reboot (1.0.0); its text, the job's log among it, on standard error.
on_vm sudo notes-stow upgrade --yes --json >"$WORK/upgrade.json" 2>"$WORK/upgrade.txt" || { cat "$WORK/upgrade.txt" "$WORK/upgrade.json"; fail "upgrade --json"; }
vm_result "$WORK/upgrade.json" "done" 1.0.0
grep -q 'a backup first: notes-full-' "$WORK/upgrade.txt" && grep -q 'backed up: dav:backups/notes-full-' "$WORK/upgrade.txt" ||
	{ cat "$WORK/upgrade.txt"; fail "the upgrade took no backup first"; }
# The VM reboots into 1.0.1, whose boot upgrades the install; the gate marks the boot good.
back_on 1.0.1 active "after the upgrade" "$boot"
healthy_postgres "after the upgrade"
vm_status_json "$WORK/vm-status2.json" 1.0.1 1.0.0
[[ $(on_vm systemctl is-active notes-health.service) == active ]] || fail "the health gate did not pass 1.0.1"
# The boot keeps the operator's profile, and its Docker file, written before Docker starts, rotates
# the logs. Until vm attach gives it a NIC, edge is the one setting that differs.
on_vm notes-stow tune --check >"$WORK/tune.txt" 2>&1 || true
grep -q '^this host: .* the network profile$' "$WORK/tune.txt" && grep -qE '^differs +interface edge +absent' "$WORK/tune.txt" &&
	[[ $(grep '^differs ' "$WORK/tune.txt" | grep -cvE '^differs +interface edge ') == 0 ]] ||
	{ cat "$WORK/tune.txt"; fail "the upgraded boot's tuning differs beyond edge's missing NIC, or is not the network profile"; }
grep -qE "^ok +package tcpdump " "$WORK/tune.txt" && grep -qE "^ok +kernel stowage.e2e=1 " "$WORK/tune.txt" ||
	{ cat "$WORK/tune.txt"; fail "tune does not report the project's package and kernel argument as asked"; }
tuned=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=api)
on_vm sudo docker inspect --format "'{{json .HostConfig.LogConfig}}'" "$tuned" | grep -q '"max-size":"20m"' || fail "the api container's logs do not rotate"
# The allowlist outlives the reboot: the boot's input chain drops the port until the install's
# start opens it to the allowlist's sources again.
on_vm sudo nft list chain inet notes input | grep -q 'tcp dport 5432 ip saddr 192.0.2.1 accept' || fail "the upgraded boot lost the L4 route's allowlist"
! reach "$vm4" 5432 || fail "the L4 route's port answers this host after the upgrade"
assessment >"$WORK/assess.txt" || { cat "$WORK/assess.txt"; fail "the boot assessment cannot be read"; }
grep -qx 'enable_boot_assessment=' "$WORK/assess.txt" && grep -qx 'boot_assessment_tentative=' "$WORK/assess.txt" ||
	{ cat "$WORK/assess.txt"; fail "the health gate did not mark the upgraded boot good"; }
on_vm notes-stow health >"$WORK/health.txt" 2>&1 && grep -qx ready "$WORK/health.txt" || { cat "$WORK/health.txt"; fail "health"; }
[[ $(on_vm sudo stat -c "'%Y %s'" /oem/95_userdata) == "$userdata" ]] && on_vm test ! -e /run/.userdata_load ||
	fail "the upgrade's boot pulled the user-data again: /oem/95_userdata is not the first boot's"

step "vm attach: a NIC for the role 1.0.1 names, the VM power-cycled on TrueNAS; network set --role re-addresses it, confirmed; a reset not confirmed comes undone"
# The operator's notes-stow of the release naming the role; an address of TEST-NET-1 on the
# primary's bridge, which nothing else on the LAN holds.
vm_ctl beta-1.0.1
boot=$(boot_id)
"$CTL" vm attach "$H" --interface edge=static,address=192.0.2.10/24 --user ops --ssh-key "$KEY.pub" --host "$ADDR" --wait 15m \
	>"$WORK/attach.txt" 2>"$WORK/attach.log" || { tail -80 "$WORK/attach.log"; fail "vm attach"; }
back_on 1.0.1 active "after vm attach" "$boot"
healthy_postgres "after vm attach"
on_vm ip -4 -o addr show dev edge | grep -q ' 192\.0\.2\.10/24 ' || { on_vm ip -o addr || true; fail "the attached NIC is not named edge with its address"; }
on_vm test -e /sys/class/net/uplink || fail "the primary NIC lost its name as edge joined it"
on_vm notes-stow tune --check >"$WORK/tune.txt" 2>&1 || { cat "$WORK/tune.txt"; fail "tune still finds a setting that differs once edge is attached"; }
# role_line: the edge role's line of `notes-stow network`.
role_line() { on_vm notes-stow network >"$WORK/net-role.txt" 2>&1 || { cat "$WORK/net-role.txt"; fail "network"; }; sed -n 's/^role       edge: //p' "$WORK/net-role.txt"; }
[[ $(role_line) == *"192.0.2.10/24; set by the cloud-config or \`network attach\`" ]] || { cat "$WORK/net-role.txt"; fail "network does not show the attached NIC"; }
on_vm notes-stow network set --role edge --mode static --address 192.0.2.11/24 --revert-secs 600 >"$WORK/net-set.txt" 2>&1 ||
	{ cat "$WORK/net-set.txt"; fail "network set --role edge"; }
grep -q 'network confirm` keeps it within 600 s' "$WORK/net-set.txt" || { cat "$WORK/net-set.txt"; fail "the secondary's change does not await confirmation"; }
role_line >/dev/null
grep -q '^pending    not confirmed' "$WORK/net-role.txt" || { cat "$WORK/net-role.txt"; fail "the secondary's change does not await confirmation"; }
[[ $(on_vm notes-stow network confirm) == confirmed ]] || fail "network confirm of edge's change"
[[ $(role_line) == *"192.0.2.11/24; set by \`notes-stow network set --role edge\`: 192.0.2.11/24" ]] && ! grep -q '^pending' "$WORK/net-role.txt" ||
	{ cat "$WORK/net-role.txt"; fail "edge's override is not kept"; }
on_vm ip -4 -o addr show dev edge | grep -q ' 192\.0\.2\.11/24 ' || fail "edge does not have its override's address"
# The override's file under /run, and the release's MTU drop-in applied to it.
on_vm test -e /run/systemd/network/09-notes-edge-override.network || fail "no override file for edge under /run"
on_vm test -e /usr/lib/systemd/network/09-notes-edge-override.network.d/50-notes.conf || fail "the release ships no drop-in for edge's override"
on_vm ip -o link show dev edge | grep -q ' mtu 1400 ' || { on_vm ip -o link show dev edge || true; fail "edge's override runs without the release's MTU"; }
# Reset, not confirmed: the attach's address now, the override back at the window's end.
on_vm notes-stow network reset --role edge --revert-secs 60 >"$WORK/net-reset.txt" 2>&1 || { cat "$WORK/net-reset.txt"; fail "network reset --role edge"; }
[[ $(role_line) == *"set by the cloud-config or \`network attach\`" ]] && grep -q '^pending    not confirmed' "$WORK/net-role.txt" ||
	{ cat "$WORK/net-role.txt"; fail "the reset of edge's override does not await confirmation"; }
deadline=$((SECONDS + 240))
until [[ $(role_line) == *"set by \`notes-stow network set --role edge\`: 192.0.2.11/24" ]] && ! grep -q '^pending' "$WORK/net-role.txt"; do
	((SECONDS < deadline)) || { cat "$WORK/net-role.txt"; fail "edge's reset was not undone at its window's end"; }
	sleep 10
done
on_vm ip -4 -o addr show dev edge | grep -q ' 192\.0\.2\.11/24 ' || fail "edge does not have its override's address back"

step "rollback: the VM reboots into the passive system, whose boot goes back to 1.0.0"
# The job reboots the VM itself: the reboot may close the session following it (ssh: 255), once
# the run printed its Result. Under --json, the Result alone on standard output: the release
# installed as the job schedules the reboot (1.0.1); the text on standard error.
boot=$(boot_id)
rc=0; on_vm sudo notes-stow rollback --yes --json >"$WORK/rollback.json" 2>"$WORK/rollback.txt" || rc=$?
[[ $rc == 0 ]] || { [[ $rc == 255 ]] && grep -q 'rebooting into the passive system' "$WORK/rollback.txt"; } ||
	{ cat "$WORK/rollback.txt" "$WORK/rollback.json"; fail "rollback --json exited $rc"; }
vm_result "$WORK/rollback.json" "done" 1.0.1
grep -q 'the passive system carries notes 1.0.0 (revision 1)' "$WORK/rollback.txt" || { cat "$WORK/rollback.txt"; fail "rollback did not name 1.0.0"; }
back_on 1.0.0 passive "after rollback" "$boot"
healthy_postgres "after rollback"
vm_status_json "$WORK/vm-status3.json" 1.0.0 -
# A refusal: exit 3, and under --json a Result of nothing, its reason and its fix.
rc=0; on_vm sudo notes-stow rollback --yes --json >"$WORK/rollback2.json" 2>"$WORK/rollback2.txt" || rc=$?
[[ $rc == 3 ]] || { cat "$WORK/rollback2.txt"; fail "a rollback from the passive system exited $rc, not 3 (refused)"; }
grep -q 'runs the passive system already' "$WORK/rollback2.txt" || { cat "$WORK/rollback2.txt"; fail "the refusal does not say why"; }
vm_result "$WORK/rollback2.json" nothing 1.0.0
jq -e '(.refused.reason | test("runs the passive system already")) and (.refused.fix | length > 0)' "$WORK/rollback2.json" >/dev/null ||
	{ cat "$WORK/rollback2.json"; fail "the Result does not give the refusal's reason and fix"; }

resolv_ok "after the rollback's reboot"

step "ship 1.0.2, which never becomes ready: its health gate fails, and Kairos falls back to the passive system"
release 1.0.2 false
boot=$(boot_id)
on_vm sudo notes-stow upgrade --yes >"$WORK/upgrade3.txt" 2>&1 || { cat "$WORK/upgrade3.txt"; fail "upgrade to 1.0.2 from the passive system"; }
# The fallback left the last attempt marked; the upgrade clears it (Kairos's after-upgrade, or the
# host daemon's job after it) before the 30 s reboot, so that reboot tries 1.0.2. That 1.0.2 then
# ran and was judged is the failed boot status below.
assessment >"$WORK/assess3.txt" || { cat "$WORK/assess3.txt"; fail "the boot assessment cannot be read"; }
grep -qx 'enable_boot_assessment=yes' "$WORK/assess3.txt" && grep -qx 'boot_assessment_tentative=' "$WORK/assess3.txt" ||
	{ cat "$WORK/upgrade3.txt" "$WORK/assess3.txt"; fail "the upgrade did not give 1.0.2 a trial of its own"; }
back_on 1.0.0 passive "after 1.0.2 failed its health gate" "$boot"
healthy_postgres "after the fallback"
grep -q '^failed     notes 1.0.2 (revision 1) at .*healthz/ready answered 503, not 200' "$WORK/status.txt" ||
	{ cat "$WORK/status.txt"; fail "status does not tell the failed boot"; }
dir=$(sed -n 's/^ *diagnostics: //p' "$WORK/status.txt")
on_vm sudo test -s "$dir/logs-api.txt" -a -s "$dir/boot.log" || fail "the failed boot's diagnostics are not kept in $dir"
on_vm notes-stow updates >"$WORK/updates3.txt" 2>&1 || { cat "$WORK/updates3.txt"; fail "updates after the fallback"; }
grep -q 'went back from 1.0.2' "$WORK/updates3.txt" || { cat "$WORK/updates3.txt"; fail "updates offers 1.0.2 again"; }

step "ship 1.0.3, a rollback barrier that never becomes ready: its health gate fails, and keeps it, as going back is refused"
release 1.0.3 false 2 '{"2": "notes moved to a new table"}'
boot=$(boot_id)
on_vm sudo notes-stow upgrade --yes >"$WORK/upgrade4.txt" 2>&1 || { cat "$WORK/upgrade4.txt"; fail "upgrade to 1.0.3"; }
back_on 1.0.3 active "after 1.0.3 failed its health gate past a barrier" "$boot"
[[ $(on_vm systemctl is-active notes-health.service) == failed ]] || fail "the health gate passed 1.0.3"
grep -q '^failed     notes 1.0.3 (revision 1) at ' "$WORK/status.txt" && grep -q '^           kept on this system, as going back is refused: refusing: the database carries notes moved to a new table' "$WORK/status.txt" ||
	{ cat "$WORK/status.txt"; fail "status does not tell why 1.0.3 is kept"; }
assessment >"$WORK/assess.txt" || { cat "$WORK/assess.txt"; fail "the boot assessment cannot be read"; }
grep -qx 'enable_boot_assessment=' "$WORK/assess.txt" && grep -qx 'boot_assessment_tentative=' "$WORK/assess.txt" ||
	{ cat "$WORK/assess.txt"; fail "the kept boot is not marked: the next boot would fall back"; }

step "network: the lease kept as a static address, a static IPv6 address beside it, confirmed; a change not confirmed comes undone, at its window's end and at a reboot"
# net_field <field>: the field of `notes-stow network`, as the last call printed it.
net() { on_vm notes-stow network >"$WORK/net.txt" 2>&1 || { cat "$WORK/net.txt"; fail "network $1"; }; }
net_field() { sed -n "s/^$1 *//p" "$WORK/net.txt"; }
net "on DHCP"
# edge's override, kept through 1.0.0 (which names no edge), rendered again by 1.0.3's boot.
grep -q "^role       edge: .*192\.0\.2\.11/24; set by \`notes-stow network set --role edge\`: 192\.0\.2\.11/24$" "$WORK/net.txt" ||
	{ cat "$WORK/net.txt"; fail "edge's override did not come back with a release naming edge"; }
# vm create's network, DHCP with IPv6 on: the cloud-config's file, which asks DHCPv6 without
# waiting for a router's advertisement, as a cloud gives IPv6 addresses.
[[ $(net_field 'set by') == "the VM's cloud-config, as \`vm create\` gave it: DHCP, automatic IPv6" ]] || { cat "$WORK/net.txt"; fail "the network is not the cloud-config's"; }
on_vm cat /etc/systemd/network/05-notes-static.network >"$WORK/net-file.txt" || fail "no network file from the cloud-config"
grep -qx 'DHCP=yes' "$WORK/net-file.txt" && grep -qx 'WithoutRA=solicit' "$WORK/net-file.txt" && ! grep -q 'IPv6AcceptRA=no' "$WORK/net-file.txt" ||
	{ cat "$WORK/net-file.txt"; fail "the cloud-config's network is not DHCP with IPv6"; }
# The stack's network has IPv6 (vm create --ipv6 reaches the install).
[[ -n $(on_vm sudo docker network ls -q --filter label=io.stowage.ipv6=true) ]] || fail "the stack's network on the VM has no IPv6"
# On a LAN with IPv6 (this host has a global address), the VM gets a global IPv6 address and an
# IPv6 default route, and this host reaches it over IPv6: SSH (the firewall's ip6 rule) and the front.
if ((${#HOST6[@]})); then
	deadline=$((SECONDS + 300))
	until vm6=$(net_field address | tr ',' '\n' | tr -d ' ' | grep -m1 -E '^[23][0-9a-f]{0,3}:') && net_field gateway | grep -q ':'; do
		((SECONDS < deadline)) || { cat "$WORK/net.txt"; fail "the LAN gave the VM no global IPv6 address and IPv6 default route"; }
		sleep 10
		net "IPv6"
	done
	vm6=${vm6%/*}
	VM_IP=$vm6 on_vm true || fail "no SSH over IPv6 to $vm6"
	code=$(curl -6 -sk --connect-timeout 10 --max-time 30 -o /dev/null -w '%{http_code}' --resolve "app.notes.test:443:[$vm6]" https://app.notes.test/)
	[[ $code =~ ^[23] ]] || fail "https://app.notes.test/ over IPv6 ([$vm6]) answered $code"
	! reach "$vm6" 5432 || fail "the L4 route's port answered over IPv6, which its allowlist leaves out"
	# The boot waited for the IPv6 default route before Docker started. With the advertised route
	# dropped, the wait run again at once sends a router solicitation, and the route comes back
	# from the router's answer (networkd solicits no more once a router answered): its line counts
	# the solicitations, so a route brought back by an unsolicited advertisement alone fails.
	[[ $(on_vm systemctl is-active notes-route6.service) == active ]] || fail "the boot's wait for the IPv6 default route did not run"
	on_vm 'for _ in 1 2 3 4 5 6 7 8; do sudo ip -6 route del default proto ra 2>/dev/null || break; done; sudo notes-stow route6-boot' >"$WORK/route6.txt" 2>&1 || { cat "$WORK/route6.txt"; fail "route6-boot"; }
	grep -qE '^the IPv6 default route on [^ ]+ came after [1-9][0-9]* router solicitations$' "$WORK/route6.txt" ||
		{ cat "$WORK/route6.txt"; fail "the router solicitation did not bring the IPv6 default route back"; }
	[[ -n $(on_vm ip -6 route show default proto ra) ]] || fail "no advertised IPv6 default route after route6-boot"
else
	echo "this host has no global IPv6 address: the VM's IPv6 address, SSH and HTTPS over it are not checked"
fi
lease=$(net_field address) gw=$(net_field gateway) dns=$(net_field dns)
# The first address and gateway are IPv4's, the IPv6 ones follow.
lease=${lease%%,*} gw=${gw%%,*} dns=${dns// /}
# A unique local address: no router needed, and duplicate address detection runs on the LAN.
ula=fd5f:7465:7374::10/64
[[ $lease == */* && -n $gw ]] || { cat "$WORK/net.txt"; fail "no address and gateway on the VM"; }
# Kept static, the VM may lose the DHCP lease its name was registered with: it is reached at its
# address from here on.
VM_IP=${lease%/*}
on_vm notes-stow network set --mode static --address "$lease" --gateway "$gw" --ipv6 --address6 "$ula" ${dns:+--dns "$dns"} --revert-secs 600 >"$WORK/net-set.txt" 2>&1 ||
	{ cat "$WORK/net-set.txt"; fail "network set"; }
grep -q 'network confirm` keeps it within 600 s' "$WORK/net-set.txt" || { cat "$WORK/net-set.txt"; fail "the change does not await confirmation"; }
net "awaiting confirmation"
grep -q '^pending    not confirmed' "$WORK/net.txt" || { cat "$WORK/net.txt"; fail "nothing awaits confirmation"; }
[[ $(on_vm notes-stow network confirm) == confirmed ]] || fail "network confirm"
net "confirmed"
grep -q "^set by     \`notes-stow network set\`: $lease via $gw, $ula" "$WORK/net.txt" && ! grep -q '^pending' "$WORK/net.txt" &&
	grep -q "^address    .*$ula" "$WORK/net.txt" || { cat "$WORK/net.txt"; fail "the override is not kept"; }
on_vm cat /etc/issue.d/06-notes-network.issue | grep -q "^network: $lease via $gw, $ula" || fail "the console banner does not tell the override"
# Back to DHCP (the same lease), not confirmed: the static address comes back at the window's end.
# A change of addressing may cut the session that makes it (ssh: 255); the daemon applies it all
# the same.
on_vm notes-stow network set --mode dhcp --revert-secs 30 >"$WORK/net-set.txt" 2>&1 || [[ $? == 255 ]] || { cat "$WORK/net-set.txt"; fail "network set dhcp"; }
deadline=$((SECONDS + 180))
until on_vm notes-stow network >"$WORK/net.txt" 2>&1 && grep -q "^set by     \`notes-stow network set\`: $lease" "$WORK/net.txt" && ! grep -q '^pending' "$WORK/net.txt"; do
	((SECONDS < deadline)) || { cat "$WORK/net.txt"; fail "the change was not undone at its window's end"; }
	sleep 10
done
# Again, and a reboot inside the window: the boot undoes it before networkd starts. DHCP may hand
# the VM another lease, out of reach from here: the reboot is scheduled on the VM first, and the
# boot's journal tells the change was pending.
boot=$(boot_id)
on_vm sudo systemd-run --on-active=90 --unit=e2e-net-reboot notes-stow reboot --drain-secs 0 >/dev/null 2>&1 || fail "schedule the reboot"
on_vm notes-stow network set --mode dhcp --revert-secs 3600 >"$WORK/net-set.txt" 2>&1 || [[ $? == 255 ]] || { cat "$WORK/net-set.txt"; fail "network set dhcp"; }
deadline=$((SECONDS + 900))
# The IPv6 address is shown once duplicate address detection passed: waited for too.
until [[ $(boot_id 2>/dev/null) != "$boot" ]] && on_vm notes-stow network >"$WORK/net.txt" 2>/dev/null && grep -q "^address    $lease.*$ula" "$WORK/net.txt"; do
	((SECONDS < deadline)) || { cat "$WORK/net.txt"; fail "the VM did not come back from the reboot with its addresses"; }
	sleep 15
done
grep -q "^set by     \`notes-stow network set\`: $lease" "$WORK/net.txt" && ! grep -q '^pending' "$WORK/net.txt" ||
	{ cat "$WORK/net.txt"; fail "the boot did not undo the change not confirmed"; }
on_vm sudo journalctl -b -u notes-network.service --no-pager | grep -q 'not confirmed before the reboot' || fail "the boot does not say it undid the change"

step "the recovery system answers over SSH, behind the bundle's firewall, without the project's kernel arguments; nothing but SSH and DHCP listens beyond loopback, no account has a password"
# The recovery system serves a host key of its own, not the active system's that known_hosts
# holds: each attempt takes the key it is served into a file of its own (one taken from the active
# system before it rebooted is never kept), and the recovery mark tells which system answered. Its
# DHCP lease may not be the active system's: the lease, then the name its DHCP server registers.
on_recovery() {
	rm -f "$WORK/known_hosts.recovery"
	ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$WORK/known_hosts.recovery" \
		-i "$KEY" -o IdentitiesOnly=yes "ops@$RECOVERY" -- "$@"
}
# recovery_lost <what>: a system that answered stopped answering, which fails the step; what this host
# sees, and what the VM's journal says once it answers again within 5 minutes, tell why.
recovery_lost() {
	echo "--- $1: the recovery system stopped answering at $RECOVERY; the name $ADDR gives:"
	getent ahosts "$ADDR" || true
	ip neigh show | grep -F "$RECOVERY" || true
	local until=$((SECONDS + 300)) r
	while ((SECONDS < until)); do
		for r in "$RECOVERY" "$VM_IP" "$ADDR"; do
			# shellcheck disable=SC2016 # expanded on the VM
			if RECOVERY=$r on_recovery 'echo "--- answers at '"$r"', up $(cut -d" " -f1 /proc/uptime) s:"; ip -br addr; sudo journalctl -b -u systemd-networkd --no-pager | tail -40'; then
				fail "the recovery system stopped answering ($1), and answered again"
			fi
		done
		sleep 15
	done
	fail "the recovery system stopped answering ($1)"
}
# recovery_failed <exit status> <what>: ssh's own 255 is a system that stopped answering; any other is
# what ran failing on a system that answers.
recovery_failed() {
	[[ $1 != 255 ]] || recovery_lost "$2"
	fail "$2 on the recovery system exited $1"
}
on_vm 'sudo grub2-editenv /oem/grubenv set next_entry=recovery && sudo systemctl reboot' >/dev/null 2>&1 || true
deadline=$((SECONDS + 600))
until for RECOVERY in "$VM_IP" "$ADDR"; do on_recovery test -e /run/cos/recovery_mode 2>>"$WORK/recovery.log" && break; RECOVERY=; done; [[ -n $RECOVERY ]]; do
	((SECONDS < deadline)) || {
		tail -20 "$WORK/recovery.log"
		# shellcheck disable=SC2016 # expanded on the VM
		on_vm 'echo "the active system answers, boot $(cat /proc/sys/kernel/random/boot_id)"; sudo grub2-editenv /oem/grubenv list' || true
		fail "the recovery system was not reached over SSH within 10 minutes"
	}
	sleep 15
done
echo "recovery: answers at $RECOVERY"
# The name may move between the two NICs' leases: the address that answered is kept from here on.
recovery_name=$RECOVERY
# shellcheck disable=SC2016 # expanded on the VM
RECOVERY=$(on_recovery 'set -- $SSH_CONNECTION; echo "$3"') && [[ -n $RECOVERY ]] || { RECOVERY=$recovery_name; recovery_lost "its address"; }
echo "recovery: its address is $RECOVERY"
rc=0
on_recovery sudo nft list table inet notes >"$WORK/nft.recovery.txt" 2>&1 || rc=$?
[[ $rc != 255 ]] || { cat "$WORK/nft.recovery.txt"; recovery_lost nft; }
[[ $rc == 0 ]] && grep -q 'hook input .*policy drop;' "$WORK/nft.recovery.txt" ||
	{ cat "$WORK/nft.recovery.txt"; fail "the recovery system runs without the bundle's firewall"; }
on_recovery sudo ss -tulpn >"$WORK/ss.recovery.txt" || recovery_failed $? ss
beyond_loopback "$WORK/ss.recovery.txt" tcp/22 udp/68 udp/546 ||
	{ cat "$WORK/ss.recovery.txt"; fail "the recovery system listens beyond loopback on more than SSH and DHCP"; }
on_recovery sudo passwd -S -a >"$WORK/passwd.recovery.txt" || recovery_failed $? "passwd -S"
cmdline=$(on_recovery cat /proc/cmdline) || recovery_failed $? /proc/cmdline
[[ $cmdline != *stowage.e2e* ]] || fail "the recovery system booted with the project's kernel argument: $cmdline"
! awk '$2 == "P" {print; found = 1} END {exit !found}' "$WORK/passwd.recovery.txt" || fail "an account of the recovery system has a password"

step "vm destroy: the VM, its disk and its uploads go, and its state"
"$CTL" vm destroy "$H" --yes >"$WORK/vm-destroy.txt" 2>"$WORK/vm-destroy.log" || { tail -50 "$WORK/vm-destroy.log"; fail "vm destroy"; }
grep -q "^Destroyed $H" "$WORK/vm-destroy.txt" || fail "vm destroy did not report"
[[ ! -e $STATE ]] || fail "the VM's state is left"

printf '\nPASS\n'
