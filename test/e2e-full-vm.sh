#!/usr/bin/env bash
# End-to-end test of examples/full (atlas) on the Kairos shape, a real VM on TrueNAS: this month's
# signed CalVer release .1 is shipped with its disk, .2 without one (it records .1's), and .2
# crosses to a TLS mirror of its own CA; `vm create --create opentofu` of .2 boots .1's disk, whose
# first boot writes .2's OS bundle and reboots into it, which installs it from the cloud-config's settings with the search
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
}

step "start zot"
start_zot

step "project: examples/full with its kairos shape, a qcow2 disk, a short health gate"
KAIROS=1 new_full "$PG"
V1=$CALVER.1
sed -i 's/^    disk_formats: \[raw, qcow2, ova, vhd, gce\]$/    disk_formats: [qcow2]/; s/^    health_gate: { deadline_s: 600, /    health_gate: { deadline_s: 300, /' "$P/stowage.yaml"
grep -qx '    disk_formats: \[qcow2\]' "$P/stowage.yaml" && grep -q '^    health_gate: { deadline_s: 300, ' "$P/stowage.yaml" || fail "the kairos shape was not edited"
"${S[@]}" lint

step "ship $V1 with its disk (--disks): images, the OS bundle, its disk"
"${S[@]}" ship --channel stable --disks 2>"$WORK/ship-$V1.log" || { tail -50 "$WORK/ship-$V1.log"; fail "ship $V1"; }
grep -q "disk qcow2: tag .* → v$V1-qcow2" "$WORK/ship-$V1.log" || { grep -i disk "$WORK/ship-$V1.log"; fail "the qcow2 disk was not tagged"; }

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

step "vm create of $V2 with OpenTofu on TrueNAS, from the mirror, search set: it boots $V1's disk, takes $V2's OS, and installs $V2"
vm_names
vm_cleanup
"$CTL" vm create --no-input --platform truenas --create opentofu --hostname "$H" --user ops --ssh-key "$KEY.pub" \
	--tls internal --domains console=console.atlas.test,api=api.atlas.test,ingest=ingest.atlas.test,ops=ops.atlas.test \
	--set ADMIN_EMAIL=admin@atlas.test --set ORG_NAME=Atlas --set SUPPORT_EMAIL=support@atlas.test --set SEARCH_URL=http://api:9090 \
	--pool "$POOL" --dataset "$DATASET" --bridge "$BRIDGE" --cpus 2 --memory 4096 --disk 32 --host "$ADDR" --wait "${E2E_VM_WAIT:-45m}" \
	--registry "$SITE" --registry-ca "$MIRROR_CA" \
	>"$WORK/vm-create.txt" 2>"$WORK/vm-create.log" || { tail -80 "$WORK/vm-create.log"; fail "vm create"; }
grep -q "fetching atlas's qcow2 disk of v$V1" "$WORK/vm-create.log" || { tail -30 "$WORK/vm-create.log"; fail "vm create did not boot $V1's disk"; }
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

step "on the VM: the echo L4 routes' service runs on the host's network"
echo_id=$(on_vm sudo docker ps -q --filter label=com.docker.compose.service=echo)
[[ -n $echo_id && $(on_vm sudo docker inspect --format "'{{.HostConfig.NetworkMode}}'" "$echo_id") == host ]] ||
	fail "the echo L4 routes' service is not on the host's network"

step "vm destroy: the VM, its disk and its uploads go, and its state"
"$CTL" vm destroy "$H" --yes >"$WORK/vm-destroy.txt" 2>"$WORK/vm-destroy.log" || { tail -50 "$WORK/vm-destroy.log"; fail "vm destroy"; }
grep -q "^Destroyed $H" "$WORK/vm-destroy.txt" || fail "vm destroy did not report"
[[ ! -e $STATE ]] || fail "the VM's state is left"

printf '\nPASS\n'
