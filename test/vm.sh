# shellcheck shell=bash
# VM helpers for the suites that run a real Kairos VM on TrueNAS (sourced; vm_env runs before
# lib.sh, the others once start_zot and new_project or new_full set REG, P, NAME and S). Each VM is
# made by a test OpenTofu root of the suite's own (test/vm-truenas, test/metal-truenas, on
# truenas/truenas), from `vm seed --json`: applied and destroyed with tofu, one state directory per
# VM.
#
# vm_env: STOWAGE_VM_ENV=<file> is read with `set -a` and never copied or printed: TRUENAS_URL,
# TRUENAS_API_KEY, TRUENAS_INSECURE_SKIP_VERIFY (the API), TF_VAR_pool, TF_VAR_parent_dataset,
# TF_VAR_nic_attach (where the VM goes), TF_VAR_domain (the domain its DHCP server registers host
# names under) and DEV_VM_SSH_KEY (a private key without a passphrase, its public key next to it
# as <key>.pub). Sets POOL, DATASET, BRIDGE, DOMAIN, KEY, and TRUENAS_ENDPOINT (the provider's,
# TRUENAS_URL's JSON-RPC API); the TF_VAR_ variables are unset (a root's variables come from the
# file vm_vars writes alone). Needs tofu, genisoimage (the seed ISO) and Python's websocket-client
# (the API's checks). Call it before lib.sh.
vm_env() {
	[[ -n ${STOWAGE_VM_ENV:-} && -r $STOWAGE_VM_ENV ]] || { echo "FAIL: STOWAGE_VM_ENV=<file> is required (see the head of $0)" >&2; exit 1; }
	set -a
	# shellcheck disable=SC1090
	. "$STOWAGE_VM_ENV"
	set +a
	local v
	for v in TRUENAS_URL TRUENAS_API_KEY TF_VAR_pool TF_VAR_parent_dataset TF_VAR_nic_attach TF_VAR_domain DEV_VM_SSH_KEY; do
		[[ -n ${!v:-} ]] || { echo "FAIL: $v is not set by STOWAGE_VM_ENV" >&2; exit 1; }
	done
	POOL=$TF_VAR_pool DATASET=$TF_VAR_parent_dataset BRIDGE=$TF_VAR_nic_attach DOMAIN=$TF_VAR_domain KEY=$DEV_VM_SSH_KEY
	unset "${!TF_VAR_@}"
	case $TRUENAS_URL in
	https://*) TRUENAS_ENDPOINT=wss://${TRUENAS_URL#https://} ;;
	http://*) TRUENAS_ENDPOINT=ws://${TRUENAS_URL#http://} ;;
	*) echo "FAIL: TRUENAS_URL is not an http(s) URL" >&2; exit 1 ;;
	esac
	export TRUENAS_ENDPOINT=${TRUENAS_ENDPOINT%/}/api/current
	case ${TRUENAS_INSECURE_SKIP_VERIFY:-false} in
	1 | true | TRUE | True) TN_INSECURE=true ;;
	*) TN_INSECURE=false ;;
	esac
	command -v tofu >/dev/null || PATH=$HOME/.local/bin:$PATH
	command -v tofu >/dev/null || { echo "FAIL: no tofu on PATH" >&2; exit 1; }
	command -v genisoimage >/dev/null || { echo "FAIL: no genisoimage on PATH (the seed ISO)" >&2; exit 1; }
	python3 -c 'import websocket' 2>/dev/null || { echo "FAIL: Python's websocket-client is missing (python3-websocket): the suite reads TrueNAS's API" >&2; exit 1; }
}

# vm_names (after lib.sh): H, the VM's host name (stowage-e2e-<run>, lib.sh's RUN), and vm_host's.
vm_names() {
	export XDG_STATE_HOME=$WORK/state
	vm_host "stowage-e2e-$RUN"
}

# vm_host <host name>: the VM the next steps work on: H, ADDR its name under DOMAIN, VMNAME its name
# on TrueNAS (H's letters and digits), and STATE, the directory of its root's copy, state, seed and
# known_hosts (E2E_KEEP_VM=1: beside the work directory, past the run).
vm_host() {
	H=$1
	ADDR=$H.$DOMAIN
	VMNAME=${H//-/}
	STATE=$WORK/vms/$H
	if [[ -n ${E2E_KEEP_VM:-} ]]; then STATE=$(dirname "$WORK")/stowage-e2e-vm-state-$RUN/$H; fi
}

# vm_ctl <tag>: CTL, the <short>-stow the installer image of that tag carries.
vm_ctl() {
	CTL=$WORK/${SHORT}-stow
	login_pull
	docker pull -q "$REG/$NAME/$NAME-installer:$1" >/dev/null
	local ctr
	ctr=$(docker create "$REG/$NAME/$NAME-installer:$1")
	CLEANUP+=("docker rm -f $ctr >/dev/null 2>&1")
	docker cp "$ctr:/${SHORT}-stow" "$CTL"
	docker rm -f "$ctr" >/dev/null
	docker image rm -f "$REG/$NAME/$NAME-installer:$1" >/dev/null
	unset DOCKER_CONFIG # the next ships push with the publisher's token alone
}

# vm_mirror: the site's mirror, TLS of its own CA, on the address the VM reaches this host at
# (MIRROR_IP); SITE is the project's repository in it.
vm_mirror() {
	local truenas tn_ip
	truenas=${TRUENAS_URL#*://}
	truenas=${truenas%%[:/]*}
	tn_ip=$(getent ahostsv4 "$truenas" | awk 'NR == 1 {print $1}')
	MIRROR_IP=$(ip -4 route get "$tn_ip" | sed -n 's/.* src \([0-9.]*\).*/\1/p')
	[[ -n $MIRROR_IP ]] || fail "no address of this host toward $truenas"
	start_mirror "$MIRROR_IP"
	SITE=$MIRROR/site/$NAME
}

# mirror <archive> <export flags…>: the channel's latest release into the mirror.
mirror() {
	local a=$1
	shift
	"$CTL" mirror export --output "$a" "$@" >"$a.txt" 2>"$a.log" || { cat "$a.log"; fail "mirror export"; }
	DOCKER_CONFIG=$MIRROR_CONFIG "$CTL" mirror import "$a" --to "$SITE" --registry-ca "$MIRROR_CA" >>"$a.txt" 2>>"$a.log" || { cat "$a.log"; fail "mirror import"; }
	rm -f "$a"
}

# truenas_api <method> [params, a JSON array]: the result of one call of TrueNAS's JSON-RPC API,
# as JSON, signed in with TRUENAS_API_KEY; an error fails the call (exit 1, said on standard error).
truenas_api() {
	python3 - "$@" <<'PY'
import json, os, ssl, sys
import websocket
method = sys.argv[1]
params = json.loads(sys.argv[2]) if len(sys.argv) > 2 else []
insecure = os.environ.get("TRUENAS_INSECURE_SKIP_VERIFY", "").lower() in ("1", "true", "yes")
sslopt = {"cert_reqs": ssl.CERT_NONE, "check_hostname": False} if insecure else {}
ws = websocket.create_connection(os.environ["TRUENAS_ENDPOINT"], timeout=120, sslopt=sslopt)
def call(i, m, p):
    ws.send(json.dumps({"jsonrpc": "2.0", "id": i, "method": m, "params": p}))
    while True:
        r = json.loads(ws.recv())
        if r.get("id") == i:
            break
    if "error" in r:
        sys.exit(f"TrueNAS {m}: {json.dumps(r['error'])[:2000]}")
    return r["result"]
if call(1, "auth.login_with_api_key", [os.environ["TRUENAS_API_KEY"]]) is not True:
    sys.exit("TrueNAS refused the API key")
print(json.dumps(call(2, method, params)))
PY
}

# vm_tofu <tofu command and flags…>: tofu on STATE's root.
vm_tofu() { tofu -chdir="$STATE" "$@"; }

# vm_root <root>: STATE made from that root of test/ (its files, its lock file) and CTL, the
# program its steps run, copied in; tofu initialized from the lock file alone. Whatever happens
# next, the VM goes (vm_cleanup).
vm_root() {
	[[ ! -e $STATE ]] || fail "$STATE exists: another VM of this name"
	mkdir -p "$(dirname "$STATE")" && mkdir -m 700 "$STATE" || fail "$STATE"
	cp "$(dirname "${BASH_SOURCE[0]}")/$1"/*.tf "$(dirname "${BASH_SOURCE[0]}")/$1/.terraform.lock.hcl" "$STATE/"
	cp "$CTL" "$STATE/ctl"
	vm_cleanup
	vm_tofu init -input=false -lockfile=readonly -no-color >"$STATE/init.log" 2>&1 || { cat "$STATE/init.log"; fail "tofu init of $1"; }
}

# vm_seed <vm seed flags…>: `vm seed --json` of H for this VM, the disk fetched into the run's
# disk directory, into STATE/seed.json (0600: it holds the one-time host key's private key); its
# NoCloud seed ISO, STATE/seed.iso; STATE/known_hosts the one-time key under ADDR, which on_vm and
# every VM verb (--known-hosts-line: VMLINE) check the VM by until vm_retire.
vm_seed() {
	mkdir -p "$WORK/disks" "$STATE/seed"
	(umask 077 && "$CTL" vm seed --json --hostname "$H" --user ops --ssh-key "$KEY.pub" --disk-dir "$WORK/disks" "$@" >"$STATE/seed.json" 2>"$STATE/seed.log") ||
		{ cat "$STATE/seed.log" "$STATE/seed.json"; fail "vm seed --json of $H"; }
	jq -e '.format == 1 and (.disk_file.path | startswith("/")) and (.disk_file.sha256 | test("^[0-9a-f]{64}$")) and (.known_hosts_line | startswith("vm-seed-host-key ssh-ed25519 "))' "$STATE/seed.json" >/dev/null ||
		{ jq 'del(.host_key_private, .user_data)' "$STATE/seed.json"; fail "vm seed --json of $H: no disk file or one-time key"; }
	jq -j .user_data "$STATE/seed.json" >"$STATE/seed/user-data"
	jq -j .meta_data "$STATE/seed.json" >"$STATE/seed/meta-data"
	genisoimage -quiet -output "$STATE/seed.iso" -volid cidata -joliet -rock "$STATE/seed/user-data" "$STATE/seed/meta-data" || fail "the seed ISO of $H"
	VMLINE="$ADDR $(jq -r .known_hosts_line "$STATE/seed.json" | cut -d' ' -f2-)"
	printf '%s\n' "$VMLINE" >"$STATE/known_hosts"
}

# vm_vars <cpus> <memory MiB> <app GiB> [<NICs, a JSON array of {role, mac, attach}>]: the root's
# variables, STATE/terraform.tfvars.json, from the seed: the disk the seed's disk_file, the zvol its
# platform's os_gib and the app's space, the NICs given, else the seed's interfaces (the primary
# first, each of the MAC the seed printed, which the root requires; a bridge the seed leaves out
# left to the root; none, the root's one NIC).
vm_vars() {
	local nics=${4:-}
	[[ -n $nics ]] || nics=$(jq -c '.interfaces // [] | sort_by(.primary != true) | map({role, mac: (.mac // ""), attach: (.attach // "")})' "$STATE/seed.json")
	jq -n --slurpfile s "$STATE/seed.json" --argjson nics "$nics" --arg name "$VMNAME" --arg pool "$POOL" --arg dataset "$DATASET" --arg bridge "$BRIDGE" \
		--argjson cpus "$1" --argjson mem "$2" --argjson app "$3" --arg seed "$STATE/seed.iso" --arg seed_sha "$(sha256sum "$STATE/seed.iso" | cut -d' ' -f1)" \
		--arg ctl "$STATE/ctl" --argjson insecure "$TN_INSECURE" \
		'$s[0] as $s | {insecure: $insecure, name: $name, pool: $pool, parent_dataset: $dataset, nic_attach: $bridge, cpus: $cpus, memory_mib: $mem,
		disk_gib: (($s.disk[$s.platform].os_gib // ($s.disk_file.disk_gib - $s.app_gib)) + $app), image: $s.disk_file.path, image_sha256: $s.disk_file.sha256, seed: $seed, seed_sha256: $seed_sha,
		nics: $nics, ctl: $ctl}' >"$STATE/terraform.tfvars.json" || fail "the root's variables of $H"
}

# vm_apply <what>: the root applied, then a plan that must change nothing (-detailed-exitcode 0).
vm_apply() {
	vm_tofu apply -input=false -auto-approve -no-color >>"$STATE/apply.log" 2>&1 || { tail -60 "$STATE/apply.log"; fail "tofu apply of $H ($1)"; }
	local rc=0
	vm_tofu plan -detailed-exitcode -input=false -no-color >"$STATE/plan.log" 2>&1 || rc=$?
	[[ $rc == 0 ]] || { tail -60 "$STATE/plan.log"; fail "the plan after the apply of $H ($1) exited $rc: the root drifts"; }
	VM_ID=$(vm_tofu output -raw id) && [[ $VM_ID =~ ^[0-9]+$ ]] || fail "the root of $H gives no VM id"
}

# vm_state: the VM's state on TrueNAS (RUNNING, STOPPED…).
vm_state() { truenas_api vm.query "[[[\"id\", \"=\", $VM_ID]]]" | jq -r '.[0].status.state'; }

# secs <duration>: a duration (Ns, Nm, Nh) in seconds.
secs() {
	case $1 in
	*h) echo $((${1%h} * 3600)) ;; *m) echo $((${1%m} * 60)) ;; *s) echo "${1%s}" ;; *) fail "$1: not a duration (Ns, Nm, Nh)" ;;
	esac
}

# vm_await <wait> <install: 0|1> [<OS version>] [power]: within wait, `<short>-stow status --json`
# over SSH answers from a system that is not Kairos's recovery, carrying that OS version when one is
# given, and — with install 1 — reports the install finished; a reset or an install that failed
# fails at once. With power, a machine found stopped is started again by an apply (the power
# button: the installer powers it off).
vm_await() {
	local deadline=$((SECONDS + $(secs "$1"))) why
	while :; do
		why="no answer over SSH"
		if on_vm "${SHORT}-stow" status --json >"$STATE/await.json" 2>"$STATE/await.log"; then
			why=$(jq -r --argjson inst "$2" --arg os "${3:-}" '
				if .boot == "recovery" and (.reset_failed // "") != "" then "FAIL the first boot gave up in the recovery system: \(.reset_failed)"
				elif .boot == "recovery" then "the recovery system answers"
				elif $os != "" and .os.version != $os then "the OS of \(.os.version) answers"
				elif $inst == 1 and (.install_failed // "") != "" then "FAIL the first install failed: \(.install_failed)"
				elif $inst == 1 and .installed == null then "not installed yet"
				elif $inst == 1 and (.installing // false) then "the first install has not finished"
				else "" end' "$STATE/await.json") || why="status --json does not read"
		fi
		[[ -n $why ]] || return 0
		[[ $why != FAIL* ]] || { on_vm sudo journalctl -b -u "$NAME.service" --no-pager 2>&1 | tail -40; fail "$H: ${why#FAIL }"; }
		((SECONDS < deadline)) || { cat "$STATE/await.log"; fail "$H: not ready after $1 ($why)"; }
		if [[ ${4:-} == power && $(vm_state) == STOPPED ]]; then
			echo "$H is off: the power button (an apply)"
			vm_tofu apply -input=false -auto-approve -no-color >>"$STATE/apply.log" 2>&1 || { tail -40 "$STATE/apply.log"; fail "the apply starting $H again"; }
		fi
		sleep 15
	done
}

# vm_login: the mirror's login for the VM, given over SSH (`registry-login -`), as the seed says.
vm_login() {
	on_vm sudo "${SHORT}-stow" registry-login - <"$MIRROR_VM_LOGIN" >"$STATE/login.log" 2>&1 || { cat "$STATE/login.log"; fail "registry-login over SSH on $H"; }
}

# vm_retire: `vm host-key` reads the VM's own host key through the one-time key and retires that
# one; STATE/known_hosts and VMLINE hold the VM's own from then on.
vm_retire() {
	"$CTL" vm host-key "$H" --json --user ops --ssh-key "$KEY.pub" --host "$ADDR" --known-hosts-line "$VMLINE" >"$STATE/host-key.json" 2>"$STATE/host-key.log" ||
		{ cat "$STATE/host-key.log" "$STATE/host-key.json"; fail "vm host-key of $H"; }
	jq -e --arg a "$ADDR" '.format == 1 and .retired == true and (.known_hosts_line | startswith($a + " ssh-ed25519 "))' "$STATE/host-key.json" >/dev/null ||
		{ cat "$STATE/host-key.json"; fail "vm host-key of $H did not retire the one-time key"; }
	VMLINE=$(jq -r .known_hosts_line "$STATE/host-key.json")
	[[ $VMLINE != "$(cat "$STATE/known_hosts")" ]] || fail "vm host-key of $H gave the one-time key back"
	printf '%s\n' "$VMLINE" >"$STATE/known_hosts"
}

# vm_given <file>: a re-run of the install over SSH, given what the file holds (`install --given
# -`): secret inputs, in no seed.
vm_given() {
	on_vm sudo "${SHORT}-stow" install --given - --no-input <"$1" >"$STATE/given.log" 2>&1 || { tail -60 "$STATE/given.log"; fail "install --given over SSH on $H"; }
}

# vm_destroy: a ZFS snapshot of the VM's zvol and of its staging dataset taken through TrueNAS's
# API (force_destroy deletes them with their dataset), then tofu destroy; the VM, its zvol and its
# staging dataset are gone from TrueNAS, and STATE with them.
vm_destroy() {
	local zvol staging ds
	VM_ID=$(vm_tofu output -raw id) && zvol=$(vm_tofu output -raw zvol) && staging=$(vm_tofu output -raw staging) || fail "the root of $H gives no VM, zvol or staging dataset"
	for ds in "$zvol" "$staging"; do
		truenas_api pool.snapshot.create "$(jq -cn --arg d "$ds" '[{dataset: $d, name: "stowage-e2e"}]')" >/dev/null || fail "a snapshot of $ds"
	done
	[[ $(truenas_api pool.snapshot.query "$(jq -cn --arg z "$zvol@stowage-e2e" '[[["id", "=", $z]]]')" | jq length) == 1 ]] || fail "no snapshot of $zvol: the destroy below would prove nothing"
	vm_tofu destroy -input=false -auto-approve -no-color >"$STATE/destroy.log" 2>&1 || { tail -60 "$STATE/destroy.log"; fail "tofu destroy of $H"; }
	[[ $(truenas_api vm.query "[[[\"id\", \"=\", $VM_ID]]]") == '[]' ]] || fail "tofu destroy left the VM $VMNAME ($VM_ID)"
	[[ $(truenas_api pool.dataset.query "$(jq -cn --arg z "$zvol" --arg s "$staging" '[[["id", "in", [$z, $s]]]]')") == '[]' ]] ||
		fail "tofu destroy left $zvol or $staging"
	rm -rf "$STATE"
}

# vm_cleanup: whatever happens next, the VM goes, destroyed while its state is there, whether an
# apply finished or not (E2E_KEEP_VM=1: kept, and how to remove it printed).
vm_cleanup() {
	if [[ -z ${E2E_KEEP_VM:-} ]]; then
		CLEANUP=("[[ ! -e $STATE/terraform.tfstate ]] || tofu -chdir=$STATE destroy -input=false -auto-approve -no-color >>$STATE/destroy.log 2>&1 || { tail -30 $STATE/destroy.log; echo 'the VM $VMNAME is left on TrueNAS: tofu destroy failed'; } >&2" "${CLEANUP[@]}")
	else
		CLEANUP=("[[ ! -e $STATE/terraform.tfstate ]] || echo 'keeping the VM $VMNAME: TRUENAS_ENDPOINT=$TRUENAS_ENDPOINT TRUENAS_API_KEY=<TRUENAS_API_KEY of $STOWAGE_VM_ENV> tofu -chdir=$STATE destroy'" "${CLEANUP[@]}")
	fi
}

# VM_IP, once set, is the address ssh reaches the VM at, its host key still known by its name.
on_vm() { ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$STATE/known_hosts" -o GlobalKnownHostsFile=/dev/null -o HostKeyAlias="$ADDR" -i "$KEY" -o IdentitiesOnly=yes "ops@${VM_IP:-$ADDR}" -- "$@"; }
# not_on_vm <text> <path>: a search of that path on the VM ran and did not find that text (no single
# quote in it). A search that could not run fails, as it found nothing out.
not_on_vm() {
	local rc=0
	on_vm sudo grep -rqF -- "'$1'" "$2" || rc=$?
	[[ $rc == 1 ]] || fail "the search of $2 on the VM exited $rc (0: what goes over SSH is there)"
}
# drained <grep args…>: grep's answer on its input, read to its end. `<command> | grep -q` under
# pipefail fails when grep, done at its first match, closes the pipe while the command (ssh among
# them) still writes: the command dies of SIGPIPE, whatever grep found.
drained() {
	local rc=0
	grep "$@" || rc=$?
	cat >/dev/null
	return "$rc"
}
# boot_id: the VM's current boot; a reboot changes it.
boot_id() { on_vm cat /proc/sys/kernel/random/boot_id; }
# assessment: the boot assessment's environment on the VM.
assessment() { on_vm sudo grub2-editenv /run/initramfs/cos-state/boot_assessment list; }
# back_on <version> <boot> <what> <boot id>: the VM comes back from a reboot (a boot other than the
# one given) running the install of version, booted from the boot system (active, or passive: the
# fallback), its health gate done; within 25 minutes.
back_on() {
	local deadline=$((SECONDS + 1500))
	until [[ $(boot_id 2>/dev/null) != "$4" ]] && on_vm "${SHORT}-stow" status >"$WORK/status.txt" 2>/dev/null && grep -q "^installed  $NAME $1 (revision 1)" "$WORK/status.txt" &&
		grep -q "^boot       $2" "$WORK/status.txt" && [[ $(on_vm systemctl is-active "$NAME-health.service" 2>/dev/null) =~ ^(active|failed)$ ]]; do
		((SECONDS < deadline)) || { cat "$WORK/status.txt"; on_vm sudo journalctl -b -u "$NAME.service" -u "$NAME-health.service" --no-pager 2>&1 | tail -60; fail "the VM did not come back on $1 ($2) $3"; }
		sleep 15
	done
	if [[ $2 == passive ]]; then
		grep -q '^boot       passive (the fallback' "$WORK/status.txt" || { cat "$WORK/status.txt"; fail "status does not tell the fallback $3"; }
	fi
}
