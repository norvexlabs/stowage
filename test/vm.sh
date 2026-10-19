# shellcheck shell=bash
# VM helpers for the suites that run a real Kairos VM on TrueNAS (sourced; vm_env runs before
# lib.sh, the others once start_zot and new_project or new_full set REG, P, NAME and S).
#
# vm_env: STOWAGE_VM_ENV=<file> is read with `set -a` and never copied or printed: TRUENAS_URL,
# TRUENAS_API_KEY, TRUENAS_INSECURE_SKIP_VERIFY (the API), TF_VAR_pool, TF_VAR_parent_dataset,
# TF_VAR_nic_attach (where the VM goes), TF_VAR_domain (the domain its DHCP server registers host
# names under) and DEV_VM_SSH_KEY (a private key without a passphrase, its public key next to it
# as <key>.pub). Sets POOL, DATASET, BRIDGE, DOMAIN, KEY; the TF_VAR_ variables are unset (the
# module's variables come from the file `vm create` writes alone). Call it before lib.sh.
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
	command -v tofu >/dev/null || PATH=$HOME/.local/bin:$PATH
	command -v tofu >/dev/null || { echo "FAIL: no tofu on PATH" >&2; exit 1; }
}

# vm_names: H, the VM's host name (stowage-e2e-<pid>), ADDR its name under DOMAIN, the state
# directory under $WORK (E2E_KEEP_VM=1: beside the work directory, past the run) and STATE, the
# VM's state in it.
vm_names() {
	H=stowage-e2e-$$
	ADDR=$H.$DOMAIN
	export XDG_STATE_HOME=$WORK/state
	if [[ -n ${E2E_KEEP_VM:-} ]]; then XDG_STATE_HOME=$(dirname "$WORK")/stowage-e2e-vm-state-$$; fi
	STATE=$XDG_STATE_HOME/$NAME/vms/$H
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

# vm_cleanup: whatever happens next, the VM goes, destroyed while its state is there (E2E_KEEP_VM=1:
# kept, and how to remove it printed).
vm_cleanup() {
	if [[ -z ${E2E_KEEP_VM:-} ]]; then
		CLEANUP=("[[ -f $STATE/terraform.tfvars.json ]] && $CTL vm destroy $H --yes >>$WORK/vm-destroy.log 2>&1" "${CLEANUP[@]}")
	else
		cp "$CTL" "$XDG_STATE_HOME.${SHORT}-stow"
		echo "keeping the VM: XDG_STATE_HOME=$XDG_STATE_HOME $XDG_STATE_HOME.${SHORT}-stow vm destroy $H --yes"
	fi
}

# VM_IP, once set, is the address ssh reaches the VM at, its host key still known by its name.
on_vm() { ssh -o BatchMode=yes -o ConnectTimeout=10 -o UserKnownHostsFile="$STATE/known_hosts" -o HostKeyAlias="$ADDR" -i "$KEY" -o IdentitiesOnly=yes "ops@${VM_IP:-$ADDR}" -- "$@"; }
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
