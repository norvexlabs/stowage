#!/usr/bin/env bash
# End-to-end test of a real Kairos VM on a site without internet: the release goes to a TLS mirror
# of its own CA (`mirror export`, `mirror import`), `vm create --create opentofu` on a TrueNAS host
# boots the disk it reads from the mirror, whose first boot installs from the cloud-config, IPv6 on
# (`--ipv6`: the LAN's IPv6 address, SSH and the front reached over it);
# its firewall lets SSH in from this host alone (`--ssh-allow`) and the front's ports, and an L4
# route's port from the sources its allowlist holds (this host over IPv4: refused over IPv6, and
# from every source once a re-run changes the allowlist, through the upgrade's reboot), its service
# (the database) on the host's network and reached by name from the stack's, in the time
# zone given (`--timezone`); `reset-db` wipes the data and provisions again without a reboot; a
# backup goes, encrypted, to a WebDAV server on this host — the database and the uploads volume —
# and `restore` loads both back over newer data; `secrets export` writes the install's secrets encrypted to the operator's key; a newer release crosses the same way and the VM upgrades from the
# mirror (a backup first), its health gate marking the boot
# good; `rollback` takes the VM back to the passive system; a release that never becomes ready
# fails its health gate, and Kairos falls back to the passive system, whose boot goes back; one that
# fails it past a rollback barrier is kept, as going back would leave nothing serving;
# `network set` keeps the lease as a static address, a unique local IPv6 address beside it, and a
# change not confirmed comes undone at its window's end and at a reboot; then `vm destroy`. Needs Docker
# (privileged containers, loop devices), network access, about 15 GB on disk under
# XDG_CACHE_HOME, OpenTofu's `tofu`, ssh, and a TrueNAS host the VM's DHCP lease is registered on.
#
#   STOWAGE_VM_ENV=<file> make e2e-vm        (builds ./bin/stowage first)
#
# The file is read by test/vm.sh's vm_env, never copied or printed.
set -euo pipefail

PG=postgres:17-alpine@sha256:aa90e97ee862e558111d34cfb8b2c4bec768c2b039fb791341686928560263b3
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
# reads), and a package of the project's.
python3 - "$P/stowage.yaml" <<'PY'
import sys
path = sys.argv[1]
s = open(path).read()
for old, new in [
    ("\nhealth:\n", "\nl4:\n  routes:\n    - { name: db, protocol: tcp, port: 5432, upstream: postgres:5432, allow_input: DB_SOURCES }\n\nhealth:\n"),
    ("  - { key: ADMIN_EMAIL, prompt: \"Administrator email\", required: true }\n",
     "  - { key: ADMIN_EMAIL, prompt: \"Administrator email\", required: true }\n  - { key: DB_SOURCES, prompt: \"Sources the VM lets reach the database\", shapes: [kairos] }\n"),
]:
    assert s.count(old) == 1, old
    s = s.replace(old, new)
s += "\nhost:\n  kernel_args:\n    stowage.e2e: { value: \"1\", reason: \"the recovery system boots without it\" }\n  packages:\n    tcpdump: { reason: \"the bundle installs a package of the project's\" }\n"
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
healthy_postgres "after the install"
# A healthy boot logs no error: the journal at priority err holds nothing of this boot.
# Its standard output alone: SSH's banner comes on its standard error.
on_vm sudo journalctl -b -p err --no-pager -q >"$WORK/journal-err.txt" 2>"$WORK/journal-err.log" || { cat "$WORK/journal-err.log"; fail "journalctl on the VM"; }
[[ ! -s $WORK/journal-err.txt ]] || { cat "$WORK/journal-err.txt"; fail "the VM's boot logged errors"; }
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

step "ship beta 1.0.1, carry it through the mirror; the VM upgrades from it, and its health gate keeps it"
release 1.0.1
on_vm notes-stow updates >"$WORK/updates.txt" 2>&1 || { cat "$WORK/updates.txt"; fail "updates"; }
grep -q '1.0.1 (revision 1) on beta' "$WORK/updates.txt" || { cat "$WORK/updates.txt"; fail "updates does not offer 1.0.1 from the mirror"; }
boot=$(boot_id)
on_vm sudo notes-stow upgrade --yes >"$WORK/upgrade.txt" 2>&1 || { cat "$WORK/upgrade.txt"; fail "upgrade"; }
grep -q 'a backup first: notes-full-' "$WORK/upgrade.txt" && grep -q 'backed up: dav:backups/notes-full-' "$WORK/upgrade.txt" ||
	{ cat "$WORK/upgrade.txt"; fail "the upgrade took no backup first"; }
# The VM reboots into 1.0.1, whose boot upgrades the install; the gate marks the boot good.
back_on 1.0.1 active "after the upgrade" "$boot"
healthy_postgres "after the upgrade"
[[ $(on_vm systemctl is-active notes-health.service) == active ]] || fail "the health gate did not pass 1.0.1"
# The boot keeps the operator's profile, and its Docker file, written before Docker starts, rotates
# the logs.
on_vm notes-stow tune --check >"$WORK/tune.txt" 2>&1 && grep -q '^this host: .* the network profile$' "$WORK/tune.txt" ||
	{ cat "$WORK/tune.txt"; fail "the upgraded boot's tuning differs, or is not the network profile"; }
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

step "rollback: the VM reboots into the passive system, whose boot goes back to 1.0.0"
# The job reboots the VM itself: the reboot may close the session following it (ssh: 255).
boot=$(boot_id)
on_vm sudo notes-stow rollback --yes >"$WORK/rollback.txt" 2>&1 || { [[ $? == 255 ]] && grep -q 'rebooting into the passive system' "$WORK/rollback.txt"; } ||
	{ cat "$WORK/rollback.txt"; fail "rollback"; }
grep -q 'the passive system carries notes 1.0.0 (revision 1)' "$WORK/rollback.txt" || { cat "$WORK/rollback.txt"; fail "rollback did not name 1.0.0"; }
back_on 1.0.0 passive "after rollback" "$boot"
healthy_postgres "after rollback"
if on_vm sudo notes-stow rollback --yes >"$WORK/rollback2.txt" 2>&1; then fail "a rollback from the passive system ran"; fi
grep -q 'runs the passive system already' "$WORK/rollback2.txt" || { cat "$WORK/rollback2.txt"; fail "the refusal does not say why"; }

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
on_recovery sudo nft list table inet notes >"$WORK/nft.recovery.txt" 2>&1 && grep -q 'hook input .*policy drop;' "$WORK/nft.recovery.txt" ||
	{ cat "$WORK/nft.recovery.txt"; fail "the recovery system runs without the bundle's firewall"; }
on_recovery sudo ss -tulpn >"$WORK/ss.recovery.txt" || fail "ss on the recovery system"
beyond_loopback "$WORK/ss.recovery.txt" tcp/22 udp/68 udp/546 ||
	{ cat "$WORK/ss.recovery.txt"; fail "the recovery system listens beyond loopback on more than SSH and DHCP"; }
on_recovery sudo passwd -S -a >"$WORK/passwd.recovery.txt" || fail "passwd -S on the recovery system"
cmdline=$(on_recovery cat /proc/cmdline) || fail "the recovery system's command line does not read"
[[ $cmdline != *stowage.e2e* ]] || fail "the recovery system booted with the project's kernel argument: $cmdline"
! awk '$2 == "P" {print; found = 1} END {exit !found}' "$WORK/passwd.recovery.txt" || fail "an account of the recovery system has a password"

step "vm destroy: the VM, its disk and its uploads go, and its state"
"$CTL" vm destroy "$H" --yes >"$WORK/vm-destroy.txt" 2>"$WORK/vm-destroy.log" || { tail -50 "$WORK/vm-destroy.log"; fail "vm destroy"; }
grep -q "^Destroyed $H" "$WORK/vm-destroy.txt" || fail "vm destroy did not report"
[[ ! -e $STATE ]] || fail "the VM's state is left"

printf '\nPASS\n'
