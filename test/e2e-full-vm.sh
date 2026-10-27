#!/usr/bin/env bash
# End-to-end test of examples/full (atlas) on the Kairos shape, a real VM on TrueNAS: this month's
# signed CalVer release .1 is shipped with its installer ISO, .2 without one (it records .1's), and
# .2 crosses to a TLS mirror of its own CA; `vm create --platform metal` of .2 on a bare-metal
# machine that a TrueNAS VM plays (test/metal-truenas: an empty disk, the installer and the seed as
# two USB sticks, left in) installs .1's OS on the disk, powers off and is started again; the
# installed system's first boot writes .2's OS bundle and reboots into it, which installs it from the cloud-config's settings with the search
# extension's input set (to the api itself, standing in for a search service); the provision hook registers the administrator through the api; the api reaches the host
# daemon's app socket and reads the front's local CA; an event goes through ingest; the ops route
# refuses everyone until a re-run gives `--client-ca`, then takes a certificate that CA issued
# alone, and its login (basic_auth, the generated OPS_PASSWORD); then `vm destroy`. Upgrade, rollback and the health gate's fallback are the VM machinery's: e2e-vm. Needs
# what e2e-vm needs: Docker (privileged containers, loop devices), network access, about 15 GB
# on disk under XDG_CACHE_HOME, OpenTofu's `tofu`, ssh, and a TrueNAS host.
#
#   STOWAGE_VM_ENV=<file> make e2e-full-vm        (builds ./bin/stowage first)
#
# The file is the one e2e-vm reads (test/vm.sh, vm_env), never copied or printed.
set -euo pipefail

# The amd64 image of the example's postgres: the one e2e-docker mirrors.
PG=postgres:17-alpine@sha256:aa90e97ee862e558111d34cfb8b2c4bec768c2b039fb791341686928560263b3
. "$(dirname "$0")/vm.sh"
vm_env
. "$(dirname "$0")/lib.sh"

# status <field>…: those fields of one answer of the api's /api/v1/status, through the VM's front,
# joined by /.
status() {
	curl -sfk --max-time 10 --resolve "api.atlas.test:443:$IP" https://api.atlas.test/api/v1/status |
		python3 -c 'import json,sys; s=json.load(sys.stdin); print("/".join(str(s[k]) for k in sys.argv[1:]))' "$@"
}
# atlas_on <version> <schema>: the api answers as that release on that migration, within 5
# minutes; the console and ingest routes serve.
atlas_on() {
	local got="" end=$((SECONDS + 300))
	while ((SECONDS < end)); do
		got=$(status version schema 2>/dev/null) || true
		[[ $got == "$1/$2" ]] && break
		sleep 5
	done
	[[ $got == "$1/$2" ]] || { on_vm at-stow ps >&2 || true; fail "the api answers $got (version/migration), want $1/$2"; }
	[[ $(curl -sfk --max-time 10 --resolve "console.atlas.test:443:$IP" https://console.atlas.test/) == *'<title>Atlas'* ]] ||
		fail "the console route does not serve the ui"
	[[ $(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 --resolve "ingest.atlas.test:443:$IP" https://ingest.atlas.test/health) == 200 ]] ||
		fail "the ingest route does not serve"
	# The status route: the console's domain, under its path, to the api.
	[[ $(curl -sfk --max-time 10 --resolve "console.atlas.test:443:$IP" https://console.atlas.test/api/v1/status) == *'"version"'* ]] ||
		fail "the status route does not serve the api's status on the console's domain"
}

step "start zot"
start_zot

step "project: examples/full with its kairos shape, its installer ISO alone, a short health gate"
KAIROS=1 new_full "$PG"
V1=$CALVER.1
sed -i 's/^    disk_formats: \[raw, qcow2, ova, vhd, gce, iso\]$/    disk_formats: [iso]/; s/^    health_gate: { deadline_s: 600, /    health_gate: { deadline_s: 300, /' "$P/stowage.yaml"
grep -qx '    disk_formats: \[iso\]' "$P/stowage.yaml" && grep -q '^    health_gate: { deadline_s: 300, ' "$P/stowage.yaml" || fail "the kairos shape was not edited"
"${S[@]}" lint

step "ship $V1 with its disk (--disks): images, the OS bundle, its installer ISO"
"${S[@]}" ship --channel stable --disks 2>"$WORK/ship-$V1.log" || { tail -50 "$WORK/ship-$V1.log"; fail "ship $V1"; }
grep -q "disk iso: tag .* → v$V1-iso" "$WORK/ship-$V1.log" || { grep -i disk "$WORK/ship-$V1.log"; fail "the installer ISO was not tagged"; }

V2=$CALVER.2
step "ship $V2 (a migration) without disks: it records $V1's as the disk its VMs boot"
full_release 2 002_event_source.sql "-- 002: where an event came from
ALTER TABLE events ADD COLUMN source text;"
grep -q "no disk published: a VM of this release boots v$V1's disks" "$WORK/ship-$V2.log" || { grep -i disk "$WORK/ship-$V2.log"; fail "$V2 does not record $V1's disk"; }
! grep -q 'disks: make' "$WORK/ship-$V2.log" || fail "$V2 made disks"

step "the published at-stow of $V2, the site's mirror, $V2 in it with $V1's disk"
vm_ctl "v$V2"
vm_mirror
mirror "$WORK/r1.oci.tar"
grep -q "^Exported atlas $V2" "$WORK/r1.oci.tar.txt" || { cat "$WORK/r1.oci.tar.txt"; fail "export of $V2"; }

step "vm create of $V2 on metal (a TrueNAS VM), from the mirror, search set: $V1's installer installs, the machine starts again, takes $V2's OS, and installs $V2"
vm_names
vm_cleanup
# The machine's power button loops until it answers, and stops before any destroy.
CLEANUP=("touch $WORK/metal-up" "${CLEANUP[@]}")
# The server's two NICs (host.interfaces' uplink and edge), both on the bridge, by MACs of this run.
MAC_UPLINK=$(printf '02:5e:%02x:%02x:00:01' $(($$ >> 8 & 255)) $(($$ & 255)))
MAC_EDGE=$(printf '02:5e:%02x:%02x:00:02' $(($$ >> 8 & 255)) $(($$ & 255)))
"$CTL" vm create --no-input --platform metal --device /dev/vda --create opentofu --module "$(dirname "$0")/metal-truenas" --vm-name "${H//-/}" \
	--interface uplink=mac="$MAC_UPLINK" --interface edge=dhcp,mac="$MAC_EDGE" --hostname "$H" --user ops --ssh-key "$KEY.pub" \
	--tls internal --domains console=console.atlas.test,api=api.atlas.test,ingest=ingest.atlas.test,ops=ops.atlas.test \
	--set ADMIN_EMAIL=admin@atlas.test --set ORG_NAME=Atlas --set SUPPORT_EMAIL=support@atlas.test --set SEARCH_URL=http://api:9090 \
	--tofu-var pool="$POOL" --tofu-var parent_dataset="$DATASET" --tofu-var nic_attach="$BRIDGE" --tofu-var stop_file="$WORK/metal-up" \
	--tofu-var insecure_skip_verify="${TRUENAS_INSECURE_SKIP_VERIFY:-false}" --cpus 2 --memory 4096 --disk 20 --host "$ADDR" --wait "${E2E_VM_WAIT:-60m}" \
	--registry "$SITE" --registry-ca "$MIRROR_CA" \
	>"$WORK/vm-create.txt" 2>"$WORK/vm-create.log" || { tail -80 "$WORK/vm-create.log"; fail "vm create"; }
touch "$WORK/metal-up"
grep -q "fetching atlas's iso disk of v$V1" "$WORK/vm-create.log" || { tail -30 "$WORK/vm-create.log"; fail "vm create did not take $V1's installer"; }
grep -q "^The VM $H runs atlas $V2" "$WORK/vm-create.txt" || { cat "$WORK/vm-create.txt"; fail "vm create did not report the install"; }
! grep -rqF -- "$TRUENAS_API_KEY" "$STATE" || fail "the API key is in the VM's state"
IP=$(getent hosts "$ADDR" | awk '{print $1; exit}')
[[ -n $IP ]] || fail "$ADDR does not resolve"

step "on the VM: $V2 runs from the mirror on its own OS, the administrator registered, search set"
on_vm at-stow status >"$WORK/status.txt" || fail "status over SSH"
grep -q "^os         atlas $V2 (revision 1)" "$WORK/status.txt" || { cat "$WORK/status.txt"; fail "the VM does not run $V2's OS"; }
grep -q "^installed  atlas $V2 (revision 1)" "$WORK/status.txt" && grep -q '^channel    stable' "$WORK/status.txt" ||
	{ cat "$WORK/status.txt"; fail "status does not show the install"; }
atlas_on "$V2" 2
[[ $(status organisation admins search) == Atlas/1/True ]] || fail "the api answers $(status organisation admins search) (organisation/admins/search), want Atlas/1/True"
on_vm sudo cat /usr/local/atlas/.env >"$WORK/vm.env"
grep -qx "STOWAGE_REGISTRY='$SITE'" "$WORK/vm.env" || fail "the install does not record the mirror"
grep -qx "SEARCH_URL='\{0,1\}http://api:9090'\{0,1\}" "$WORK/vm.env" || fail "the install does not record SEARCH_URL"
on_vm sudo grep -qE "'^ADMIN_PASSWORD=[A-Za-z0-9]{20}$'" /usr/local/atlas/credentials.txt || fail "the hook's outputs file holds no credentials"
[[ $(on_vm sudo stat -c %a /usr/local/atlas/credentials.txt) == 600 ]] || fail "the credentials file is not 0600"

step "on metal: the installer's copy of the cloud-config kept as the first boot's, never pulled again with the seed left in; the screen owns the console; the state partition the installer's config gave"
on_vm sudo test -e /oem/95_userdata && on_vm sudo test ! -e /oem/90_custom.yaml || { on_vm sudo ls -la /oem; fail "the installed system does not keep the cloud-config as /oem/95_userdata alone"; }
on_vm test ! -e /run/.userdata_load || fail "the installed system pulled the seed again"
[[ $(on_vm cat /proc/cmdline | tr ' ' '\n' | grep '^console=' | tail -n1) == console=tty1 ]] || { on_vm cat /proc/cmdline; fail "console=tty1 is not the last console"; }
state=$(on_vm lsblk -bno SIZE,LABEL | awk '$2 == "COS_STATE" {print int($1 / 1048576)}')
want=$(on_vm sudo cat /oem/95_userdata/userdata.yaml | awk '/state:/ {s = 1} s && /size:/ {print $2; exit}')
[[ -n $want && $state -ge $want ]] || fail "the state partition is ${state:-?} MiB, the installer's config asked $want"

step "on the VM: the api reaches the host daemon's app socket; an event goes through ingest"
api=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=api)
on_vm sudo docker inspect --format "'{{range .Mounts}}{{.Source}}:{{.Destination}}:{{.RW}}{{println}}{{end}}'" "$api" |
	grep -qx '/run/atlas/app:/run/stowage-host:false' || fail "the api service does not mount the app directory read-only"
# The front's local CA reaches the api (front.publish_ca): a client trusting it alone reaches the
# api route.
on_vm sudo docker exec "$api" cat /etc/atlas/ca/ca.crt >"$WORK/vm-ca.crt" && grep -q 'BEGIN CERTIFICATE' "$WORK/vm-ca.crt" || fail "the api reads no CA"
curl -sf --max-time 10 --cacert "$WORK/vm-ca.crt" --resolve "api.atlas.test:443:$IP" https://api.atlas.test/health >/dev/null ||
	fail "the api route's certificate does not chain to the CA the front published"
code=$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 --resolve "ingest.atlas.test:443:$IP" -X POST -H 'Content-Type: application/json' -d '{"kind": "probe"}' https://ingest.atlas.test/events)
[[ $code == 201 ]] || fail "ingest answered $code to an event"
[[ $(status events) == 1 ]] || fail "the ingested event did not reach the api"

step "on the VM: the ops route refuses everyone until --client-ca, then a client certificate its CA issued alone, and its login"
ops() { curl -sk -o /dev/null -w '%{http_code}' --max-time 10 --resolve "ops.atlas.test:443:$IP" "$@" https://ops.atlas.test/health; }
[[ $(ops) == 403 ]] || fail "the ops route serves without a client CA"
for n in client rogue; do
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$WORK/$n-ca.key" -out "$WORK/$n-ca.pem" -days 1 -subj "/CN=$n CA" \
		-addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign 2>/dev/null &&
		openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$WORK/$n.key" -out "$WORK/$n.csr" -subj "/CN=$n" 2>/dev/null &&
		openssl x509 -req -in "$WORK/$n.csr" -CA "$WORK/$n-ca.pem" -CAkey "$WORK/$n-ca.key" -CAcreateserial -out "$WORK/$n.pem" -days 1 \
			-extfile <(printf 'extendedKeyUsage=clientAuth\n') 2>/dev/null || fail "openssl: the $n certificate"
done
on_vm 'cat >/tmp/client-ca.pem' <"$WORK/client-ca.pem" || fail "copying the client CA"
on_vm sudo at-stow install --no-input --client-ca /tmp/client-ca.pem >"$WORK/client-ca.txt" 2>&1 || { cat "$WORK/client-ca.txt"; fail "the re-run with --client-ca"; }
ops_pw=$(sed -n "s/^OPS_PASSWORD='\{0,1\}\([^']*\).*/\1/p" "$WORK/vm.env")
[[ -n $ops_pw ]] || fail "no OPS_PASSWORD recorded on the VM"
[[ $(ops --cert "$WORK/client.pem" --key "$WORK/client.key") == 401 ]] || fail "the ops route serves a certificate without its login"
[[ $(ops --cert "$WORK/client.pem" --key "$WORK/client.key" -u "ops:wrong") == 401 ]] || fail "the ops route takes a wrong password"
[[ $(ops --cert "$WORK/client.pem" --key "$WORK/client.key" -u "ops:$ops_pw") == 200 ]] || fail "the ops route refuses a certificate its client CA issued, with its login"
on_vm sudo test -s /usr/local/atlas/.stowage-front-logins || fail "the login's hash is not kept"
[[ $(ops) == 000 && $(ops --cert "$WORK/rogue.pem" --key "$WORK/rogue.key") == 000 ]] || fail "the ops route takes a client without a certificate its client CA issued"
[[ $(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 --resolve "api.atlas.test:443:$IP" https://api.atlas.test/health) == 200 ]] || fail "the api route asks for a client certificate"

step "on metal: each NIC named by its role from its MAC, the edge's lease neither the default route nor the host's name, its policy routing, the tun device's route once opened; the firewall opens the front on uplink alone and the UDP echo on edge alone"
for r in uplink:$MAC_UPLINK edge:$MAC_EDGE; do
	[[ $(on_vm cat "/sys/class/net/${r%%:*}/address") == "${r#*:}" ]] || { on_vm ip -o link; fail "no NIC ${r%%:*} of MAC ${r#*:}"; }
done
[[ $(on_vm ip -o route show default | awk '{for (n = 1; n < NF; n++) if ($n == "dev") print $(n+1)}' | sort -u) == uplink ]] || { on_vm ip route; fail "the default route is not uplink's alone"; }
on_vm ip -4 -o addr show dev edge | grep -q ' inet ' || { on_vm networkctl status edge --no-pager; fail "edge took no lease"; }
[[ $(on_vm cat /sys/class/net/edge/mtu) == 1500 ]] || fail "edge's MTU is not 1500"
on_vm ip rule | grep -q 'fwmark 0x1 lookup 100' || { on_vm ip rule; fail "no rule of fwmark 0x1 to table 100"; }
on_vm ip route show table 100 | grep -q '^local default dev edge' || { on_vm ip route show table 100; fail "no local route in table 100 on edge"; }
on_vm sudo ip tuntap add vpn0 mode tun && on_vm sudo ip link set vpn0 up || fail "a tun device vpn0 could not be opened"
deadline=$((SECONDS + 30))
until on_vm ip route show 10.99.0.0/24 | grep -q 'dev vpn0'; do
	((SECONDS < deadline)) || { on_vm networkctl status vpn0 --no-pager; fail "networkd gave vpn0 no route to 10.99.0.0/24"; }
	sleep 2
done
on_vm sudo ip tuntap del vpn0 mode tun
on_vm sudo nft list ruleset >"$WORK/ruleset.txt"
grep -q 'iifname "uplink" tcp dport { 80, 443 } accept' "$WORK/ruleset.txt" && grep -q 'iifname "edge" udp dport 17007 ' "$WORK/ruleset.txt" ||
	{ grep -n 'dport' "$WORK/ruleset.txt"; fail "the front's or the UDP echo's ports are not on their interfaces alone"; }
# The vpn service's table steers vpn0's traffic to its sockets, which the input chain accepts on
# vpn0 alone (services.vpn.transparent); vpn0's traffic out of the uplink and its replies pass
# Docker's FORWARD policy (host.interfaces.vpn0.forward), for IPv4 and IPv6.
grep -q 'tproxy to :17008' "$WORK/ruleset.txt" || { grep -n 'tproxy' "$WORK/ruleset.txt"; fail "the vpn service's table is not loaded"; }
grep -q 'iifname "vpn0" meta l4proto { tcp, udp } socket transparent 1 accept' "$WORK/ruleset.txt" ||
	{ grep -n 'socket' "$WORK/ruleset.txt"; fail "the input chain does not accept vpn0's intercepted traffic"; }
for t in iptables ip6tables; do
	on_vm sudo $t -S DOCKER-USER >"$WORK/docker-user.txt" && on_vm sudo $t -S STOWAGE-FORWARD >"$WORK/forward.txt" || fail "$t lists no DOCKER-USER or STOWAGE-FORWARD"
	grep -qxF -e '-A DOCKER-USER -j STOWAGE-FORWARD' "$WORK/docker-user.txt" &&
		grep -qxF -e '-A STOWAGE-FORWARD -i vpn0 -o uplink -j ACCEPT' "$WORK/forward.txt" &&
		grep -qxF -e '-A STOWAGE-FORWARD -i uplink -o vpn0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT' "$WORK/forward.txt" &&
		[[ $(wc -l <"$WORK/forward.txt") == 3 ]] ||
		{ cat "$WORK/docker-user.txt" "$WORK/forward.txt"; fail "$t does not forward vpn0 out of the uplink alone"; }
done
on_vm at-stow tune >"$WORK/tune-ifaces.txt" 2>&1 || true
for r in uplink edge; do
	grep -qE "^ok +interface $r " "$WORK/tune-ifaces.txt" || { cat "$WORK/tune-ifaces.txt"; fail "tune does not report $r ok"; }
done
grep -qE '^ok +firmware intel/ice/ddp/ice.pkg ' "$WORK/tune-ifaces.txt" || { cat "$WORK/tune-ifaces.txt"; fail "tune does not report the E810's firmware ok"; }

step "on the VM: the echo L4 routes' service and the vpn service run on the host's network"
echo_id=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=echo)
[[ -n $echo_id && $(on_vm sudo docker inspect --format "'{{.HostConfig.NetworkMode}}'" "$echo_id") == host ]] ||
	fail "the echo L4 routes' service is not on the host's network"
vpn_id=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=vpn)
[[ -n $vpn_id && $(on_vm sudo docker inspect --format "'{{.HostConfig.NetworkMode}}'" "$vpn_id") == host ]] ||
	fail "the vpn service (host_network) is not on the host's network"

step "on the VM: what the project asks of its host (host): kernel modules, packages, kernel arguments, settings, power"
on_vm test -d /sys/module/tcp_diag || fail "tcp_diag is not loaded at boot"
on_vm '! test -d /sys/module/pcspkr' || fail "pcspkr is loaded, though blocked"
on_vm '! sudo modprobe pcspkr' 2>/dev/null || fail "modprobe loads pcspkr, though blocked"
[[ $(on_vm cat /sys/module/overlay/parameters/redirect_always_follow) == Y ]] || fail "overlay lacks its option"
on_vm at-stow tune >"$WORK/tune.txt" 2>&1 || true
for m in tcp_diag pcspkr "overlay redirect_always_follow"; do
	grep -qE "^ok +module $m " "$WORK/tune.txt" || { cat "$WORK/tune.txt"; fail "tune does not report module $m as asked"; }
done
on_vm tcpdump --version >/dev/null 2>&1 || fail "tcpdump (host.packages) is not on the VM"
grep -qE "^ok +package tcpdump " "$WORK/tune.txt" || { cat "$WORK/tune.txt"; fail "tune does not report package tcpdump as installed"; }
on_vm cat /proc/cmdline | grep -qw 'iommu=pt' || fail "the VM booted without its kernel argument iommu=pt (host.kernel_args)"
[[ $(on_vm cat /proc/sys/vm/swappiness) == 10 ]] || fail "vm.swappiness is not the project's 10 (host.sysctls)"
grep -qE "^ok +kernel iommu=pt " "$WORK/tune.txt" && grep -qE "^ok +sysctl vm.swappiness " "$WORK/tune.txt" ||
	{ cat "$WORK/tune.txt"; fail "tune does not report the kernel argument and the setting as asked"; }
# host.power performance: the VM has no cpufreq (the hypervisor drives the CPUs' frequency).
grep -qE "^n/a +cpu governor " "$WORK/tune.txt" || { cat "$WORK/tune.txt"; fail "tune does not report the governor as the hypervisor's"; }
# shellcheck disable=SC2016 # expanded on the VM
on_vm '! dpkg-query -W -f="\${db:Status-Abbrev}" nfs-common rpcbind snmpd 2>/dev/null | grep -q "^i"' || fail "a package the bundle purges is on the VM"

step "vm destroy: the VM, its disk and its uploads go, and its state"
"$CTL" vm destroy "$H" --yes >"$WORK/vm-destroy.txt" 2>"$WORK/vm-destroy.log" || { tail -50 "$WORK/vm-destroy.log"; fail "vm destroy"; }
grep -q "^Destroyed $H" "$WORK/vm-destroy.txt" || fail "vm destroy did not report"
[[ ! -e $STATE ]] || fail "the VM's state is left"

printf '\nPASS\n'
