#!/usr/bin/env bash
# The commit gate (CLAUDE.md, gate 1), with the end-to-end suites chosen from what changed: a suite
# runs only when a changed path can affect it, so the long ones (e2e-kairos ~25 min, e2e-vm ~105 min)
# run only for the code they exercise (e2e-full-vm only for its own scripts and examples/full's kairos
# shape: the VM machinery is e2e-vm's).
#
#   make gate                  check, build, lint, then the suites the change needs
#   make gate-plan             print the suites and why, run nothing
#   GATE_BASE=<rev> make gate  compare with rev (default HEAD: the uncommitted change)
#   bash test/gate.sh --plan --paths P…   the plan for these paths, as if they changed
#
# The change is every path that differs from GATE_BASE, untracked files included. Markdown alone
# runs nothing (a docs-only commit). A test file (_test.go) runs no suite: make check runs it. The
# parallel suites, e2e-kairos among them, run with make -j -k (disk builds wait for one another on
# auroraboot's one container name, as its loop devices are the host's); beside them e2e-vm and
# e2e-full-vm run as one chain, one TrueNAS VM at a time (a failure ends the chain), and need
# STOWAGE_VM_ENV=<file>. A change to the cloud
# images' registration (the uploader, the publisher's step, the CLI's settings) runs e2e-kairos with STOWAGE_CLOUD_ENV=<file> (the clouds' settings and credentials), so
# its images are registered for real. Every suite runs the binaries the gate builds once, before
# them: a snapshot of the tree as the gate started.
set -euo pipefail
cd "$(dirname "$0")/.."

plan=false
[[ ${1:-} == --plan ]] && plan=true && shift
base=${GATE_BASE:-HEAD}
if [[ ${1:-} == --paths ]]; then
	shift
	changed=("$@")
else
	mapfile -t changed < <({ git diff --name-only "$base" --; git ls-files --others --exclude-standard; } | sort -u)
fi

declare -A why=()
unrouted=()
need() { # need <suite> <path>: the path runs the suite (the first path is kept as the reason)
	[[ -n ${why[$1]:-} ]] || why[$1]=$2
}
# installer and publish paths that the Docker and Kubernetes lifecycles (both examples) exercise
lifecycle() { need e2e-docker "$1"; need e2e-full "$1"; need e2e-stateless "$1"; need e2e-k8s "$1"; need e2e-full-k8s "$1"; }
# what the VM test exercises: a real VM's creation, boot, host daemon, upgrade and fallback
vm_side() { need e2e-kairos "$1"; need e2e-vm "$1"; }

# kairos_lines <spec>: the change touches the spec's kairos shape (one of its own keys) or its
# host block, which the OS bundle is built with. An input's or a route's `shapes:` does not: the VM
# runs the Docker shape's install, which the short suites prove.
# The shape's block (`  kairos:` under `shapes:`, to the next key of its level) and the top-level
# `host:` block are compared as a whole, before and after: a key in any form, a module's or a
# package's entry included.
kairos_block() {
	awk '/^host:/ { h = 1; print; next } /^[^ #]/ { h = 0 } h { print; next }
		/^shapes:/ { s = 1; next } s && /^[^ #]/ { s = 0; k = 0 } s && /^  kairos:/ { k = 1; print; next } k && /^  [^ #]/ { k = 0 } k { print }'
}
kairos_lines() {
	[[ "$(git show "$base:$1" 2>/dev/null | kairos_block)" != "$(kairos_block <"$1")" ]]
}

# os_allow_lines <audit-allow.yaml>: an entry whose artifact is os or "*" differs, before and after
# (an entry is its `- ` line and the lines under it, to the next entry; block or flow style, the
# value bare or quoted).
os_allow_block() {
	awk '/^ *- / { if (e != "" && os) printf "%s", e; e = ""; os = 0 } { e = e $0 "\n" }
		/(^|[ {,])artifact: *(os|"os"|\x27os\x27|"\*"|\x27\*\x27) *([,}#]|$)/ { os = 1 }
		END { if (e != "" && os) printf "%s", e }'
}
os_allow_lines() {
	[[ "$(git show "$base:$1" 2>/dev/null | os_allow_block)" != "$(os_allow_block <"$1")" ]]
}

code=false hooks=false selfaudit=false release=false
for f in "${changed[@]}"; do
	case $f in
	*.md | LICENSE) continue ;;
	esac
	code=true
	case $f in
	internal/install/smoke_test.go) need smoke-install "$f" ;;
	*_test.go) ;; # make check runs it
	# Before internal/publish/* and internal/cli/*: the first arm that matches is taken.
	internal/cloud/* | internal/publish/cloud.go | internal/cli/cloud.go)
		need e2e-kairos "$f"
		cloud_why=$f
		# Every ship of the examples' kairos shape runs the publisher's and the CLI's cloud step.
		if [[ $f != internal/cloud/* ]]; then
			need e2e "$f"
			lifecycle "$f"
		fi
		;;
	# The publisher: its commands, the project it reads, its lock, its build graph's inputs, the hook
	# that ships on a push.
	internal/registry/* | internal/release/* | internal/publish/* | internal/cli/* | cmd/stowage/* | \
		internal/hook/* | internal/lock/* | internal/project/* | internal/dockerfile/* | internal/digest/* | \
		internal/changelog/* | internal/gitcmd/*)
		need e2e "$f"
		lifecycle "$f"
		[[ $f == internal/publish/os.go ]] && need e2e-kairos "$f"
		;;
	internal/install/tofu/* | internal/install/vmtofu*.go | internal/install/seed*.go | internal/install/vm.go | \
		internal/install/vmhealth*.go | internal/install/vmcreate*.go | internal/install/vmcli*.go | internal/install/hostd*.go)
		need smoke-install "$f"
		lifecycle "$f"
		vm_side "$f"
		# A machine's creation and its seed: bare metal's is examples/full's (its installer).
		[[ $f == internal/install/vmcreate*.go || $f == internal/install/seed*.go ]] && need e2e-full-vm "$f"
		;;
	internal/install/audit.go) # the Docker host's audit, and the VM's scanner (preloaded, or pulled)
		need smoke-install "$f"
		lifecycle "$f"
		vm_side "$f"
		;;
	internal/install/vm*.go)
		need smoke-install "$f"
		lifecycle "$f"
		need e2e-kairos "$f"
		;;
	# A service's nftables table, its address of its own and its cores: examples/full's on its machine.
	internal/install/nftables*.go | internal/install/ipvlan*.go | internal/install/cpus*.go)
		need smoke-install "$f"
		lifecycle "$f"
		need e2e-full-vm "$f"
		;;
	internal/install/* | internal/kit/* | internal/payload/* | cmd/stowage-install/*)
		need smoke-install "$f"
		lifecycle "$f"
		;;
	internal/installerbin/* | internal/version/*) lifecycle "$f" ;;
	internal/sigverify/*) # the publisher's verify and every install's upgrade read signatures with it
		need e2e "$f"
		need smoke-install "$f"
		lifecycle "$f"
		vm_side "$f"
		;;
	internal/helm/*)
		need e2e-k8s "$f"
		need e2e-full-k8s "$f"
		;;
	internal/kairos/files/* | internal/kairos/Dockerfile.tmpl | internal/kairos/kairos.go | internal/backup/*) vm_side "$f" ;;
	# The release gate's policy and allowances: ship's alone, which e2e's and the lifecycle suites'
	# ships pass; e2e-full-vm's is the one that keeps examples/full's kairos shape, so audits its OS
	# bundle under the allowances narrowed to its upstream binaries. Before internal/sbom/*.
	internal/sbom/policy.go)
		need e2e "$f"
		lifecycle "$f"
		need e2e-full-vm "$f"
		;;
	# Every lock Stowage takes: the installs' (smoke and the lifecycles), the host daemon's (the
	# VM's), the scanner cache's (every ship's audit).
	internal/flock/*)
		need smoke-install "$f"
		need e2e "$f"
		lifecycle "$f"
		vm_side "$f"
		;;
	# The SBOMs ship makes, the scanner a release carries, the audits that read them.
	internal/sbom/*) # every signed ship of every example makes SBOMs; the audits read them
		need e2e "$f"
		lifecycle "$f"
		vm_side "$f"
		;;
	# The host's tuning: the Docker host's report and apply, the VM's boot (e2e-vm's project declares
	# a kernel argument and a package); the project's modules on examples/full's VM, and its
	# settings, power posture (host.go), firmware (packages.go), interfaces and their ethtool
	# settings, and the services' cores there too.
	internal/tune/modules.go) need e2e-full-vm "$f" ;;
	internal/tune/host.go | internal/tune/packages.go | internal/tune/interfaces.go | internal/tune/ethtool.go | internal/tune/cores.go)
		lifecycle "$f"
		need e2e-vm "$f"
		need e2e-full-vm "$f"
		;;
	internal/tune/*)
		lifecycle "$f"
		need e2e-vm "$f"
		;;
	# The installer ISO is made with the disks, and boots examples/full's bare-metal machine.
	internal/kairos/disks.go)
		need e2e-kairos "$f"
		need e2e-full-vm "$f"
		;;
	# The build graph: every ship's skip and re-tag decision, the OS keys' bundle.
	internal/graph/*)
		need e2e "$f"
		lifecycle "$f"
		need e2e-kairos "$f"
		;;
	internal/kairos/*) need e2e-kairos "$f" ;;
	# The front every Docker install runs: every ship builds it, every lifecycle serves through it.
	internal/front/*)
		need e2e "$f"
		lifecycle "$f"
		;;
	# Read by the installer and the publisher alike: the spec's decoding, the providers' settings,
	# the `command` contract, the volumes' ownership.
	internal/spec/* | internal/providers/* | internal/command/* | internal/chown/*)
		need smoke-install "$f"
		need e2e "$f"
		lifecycle "$f"
		;;
	# make check and the examples' lint prove them; a test helper runs no suite.
	internal/lint/* | internal/testutil/*) ;;
	# Stowage's own release gate: make self-audit (images built, scanned; needs the network).
	audit-allow.yaml | test/selfaudit/*) selfaudit=true ;;
	internal/truenas/*) need e2e-vm "$f" ;;
	examples/minimal/*)
		need e2e "$f"
		need e2e-docker "$f"
		need e2e-k8s "$f"
		# The kairos shape of the example: its OS bundle and disks.
		if [[ $f == examples/minimal/stowage.yaml ]] &&
			kairos_lines "$f"; then
			need e2e-kairos "$f"
		fi
		;;
	examples/full/deploy/chart/*) need e2e-full-k8s "$f" ;;
	examples/full/stowage.yaml)
		need e2e-full "$f"
		need e2e-full-k8s "$f"
		# The kairos shape of the example: its OS bundle, disks and VM.
		if kairos_lines "$f"; then
			need e2e-full-vm "$f"
		fi
		;;
	examples/full/deploy/compose.yaml) need e2e-full "$f" ;;
	# The allowances: an entry of the OS bundle's (artifact os or "*") is audited only by the ship
	# that keeps the kairos shape, e2e-full-vm's.
	examples/full/audit-allow.yaml)
		need e2e-full "$f"
		need e2e-full-k8s "$f"
		if os_allow_lines "$f"; then
			need e2e-full-vm "$f"
		fi
		;;
	examples/stateless/*) need e2e-stateless "$f" ;; # its chart is linted only: no suite installs it on Kubernetes
	examples/full/*) # the images, the spec, the migrations: both shapes run them
		need e2e-full "$f"
		need e2e-full-k8s "$f"
		;;
	go.mod | go.sum) # a dependency reaches anything
		need e2e "$f"
		need smoke-install "$f"
		lifecycle "$f"
		vm_side "$f"
		;;
	Dockerfile | Makefile | .dockerignore) # the binaries' build, its context, the targets
		need e2e "$f"
		need smoke-install "$f"
		lifecycle "$f"
		;;
	test/e2e-registry.sh) need e2e "$f" ;;
	test/e2e-docker.sh) need e2e-docker "$f" ;;
	test/e2e-full.sh) need e2e-full "$f" ;;
	test/e2e-stateless.sh) need e2e-stateless "$f" ;;
	test/e2e-k8s.sh) need e2e-k8s "$f" ;;
	test/e2e-full-k8s.sh) need e2e-full-k8s "$f" ;;
	test/e2e-kairos.sh | test/libvirtd/*) need e2e-kairos "$f" ;;
	test/e2e-vm.sh) need e2e-vm "$f" ;;
	test/e2e-full-vm.sh | test/vm.sh | test/metal-truenas/*) need e2e-full-vm "$f" ;;
	.githooks/* | test/hooks.sh) hooks=true ;; # the git hooks' branches, test/hooks.sh
	test/lib.sh | test/pins.sh | test/sweep.sh) # shared by every suite; the short ones exercise it
		need e2e "$f"
		lifecycle "$f"
		;;
	# The release workflow's reading of CHANGELOG.md: test/release.sh after lint. CI's other steps
	# run on a push.
	.github/workflows/* | test/release.sh | test/release-notes.sh) release=true ;;
	# No suite: the gate's plan prints itself, the docs' pictures, git's ignores.
	test/gate.sh | docs/* | .gitignore) ;;
	*) unrouted+=("$f") ;;
	esac
done

# A path no arm names would run no suite unseen: each gets its rule, here and in CLAUDE.md gate 1.
if ((${#unrouted[@]} > 0)); then
	printf 'gate: no rule for %s: add its suites (or none) to test/gate.sh and CLAUDE.md gate 1\n' "${unrouted[@]}" >&2
	exit 1
fi
if ! $code; then
	echo "gate: nothing but Markdown changed against $base: no gate (a self-review against the code is enough)"
	exit 0
fi
order=(smoke-install e2e e2e-docker e2e-full e2e-stateless e2e-k8s e2e-full-k8s e2e-kairos e2e-vm e2e-full-vm)
parallel=() chain=()
echo "gate: against $base, ${#changed[@]} paths changed; make check, make build, lint$($hooks && echo ", test/hooks.sh")$($selfaudit && echo ", make self-audit")$($release && echo ", test/release.sh"), then:"
for s in "${order[@]}"; do
	if [[ -n ${why[$s]:-} ]]; then
		echo "  $s  (${why[$s]})"
		case $s in
		e2e-vm | e2e-full-vm) chain+=("$s") ;;
		*) parallel+=("$s") ;;
		esac
	else
		echo "  ($s not needed)"
	fi
done
$plan && exit 0

if [[ -n ${cloud_why:-} && -z ${STOWAGE_CLOUD_ENV:-} ]]; then
	echo "gate: e2e-kairos registers the clouds' images ($cloud_why): give STOWAGE_CLOUD_ENV=<file>" >&2
	exit 1
fi
for s in e2e-vm e2e-full-vm; do
	if [[ -n ${why[$s]:-} && -z ${STOWAGE_VM_ENV:-} ]]; then
		echo "gate: $s is needed (${why[$s]}): give STOWAGE_VM_ENV=<file>" >&2
		exit 1
	fi
done
make check
make build
./bin/stowage -C examples/minimal lint
./bin/stowage -C examples/full lint
./bin/stowage -C examples/stateless lint
if $hooks; then test/hooks.sh; fi
if $selfaudit; then make -o build self-audit; fi
if $release; then test/release.sh; fi
# The VM chain runs one suite at a time, beside the parallel suites. Both run the binaries the
# build above made (-o build): a suite's own build would rewrite ./bin while another one runs a
# binary from it, which exec then refuses (text file busy).
chain_rc=0 pid=""
if ((${#chain[@]} > 0)); then
	(for s in "${chain[@]}"; do make -o build --output-sync=target "$s" || exit 1; done) &
	pid=$!
fi
rc=0
if ((${#parallel[@]} > 0)); then
	make -o build -k -j --output-sync=target "${parallel[@]}" || rc=1
fi
if [[ -n $pid ]]; then
	wait "$pid" || chain_rc=1
fi
((rc == 0 && chain_rc == 0)) || { echo "gate: FAIL" >&2; exit 1; }
echo "gate: PASS"
