#!/usr/bin/env bash
# The commit gate (CLAUDE.md, gate 1), with the end-to-end suites chosen from what changed: a suite
# runs only when a changed path can affect it, so the long ones (e2e-kairos ~25 min, e2e-vm ~85 min)
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
# parallel suites run with make -j -k; beside them e2e-kairos, e2e-vm and e2e-full-vm run as one
# chain, one at a time (they make disks with auroraboot, which races for loop devices; a failure
# ends the chain), and e2e-vm and e2e-full-vm need STOWAGE_VM_ENV=<file>.
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
need() { # need <suite> <path>: the path runs the suite (the first path is kept as the reason)
	[[ -n ${why[$1]:-} ]] || why[$1]=$2
}
# installer and publish paths that the Docker and Kubernetes lifecycles (both examples) exercise
lifecycle() { need e2e-docker "$1"; need e2e-full "$1"; need e2e-stateless "$1"; need e2e-k8s "$1"; need e2e-full-k8s "$1"; }
# what the VM test exercises: a real VM's creation, boot, host daemon, upgrade and fallback
vm_side() { need e2e-kairos "$1"; need e2e-vm "$1"; }

# kairos_lines <spec>: the change touches the spec's kairos shape: one of its own keys. An input's
# or a route's `shapes:` does not: the VM runs the Docker shape's install, which the short suites
# prove.
kairos_lines() {
	git diff -U0 "$base" -- "$1" | grep -qE '^[-+][[:space:]]*(kairos|base|kairos_init|disk_formats|cloud_publish|health_gate|host_api):'
}

code=false
for f in "${changed[@]}"; do
	case $f in
	*.md | LICENSE) continue ;;
	esac
	code=true
	case $f in
	internal/install/smoke_test.go) need smoke-install "$f" ;;
	*_test.go) ;; # make check runs it
	internal/registry/* | internal/release/* | internal/publish/* | internal/cli/* | cmd/stowage/*)
		need e2e "$f"
		lifecycle "$f"
		[[ $f == internal/publish/os.go ]] && need e2e-kairos "$f"
		;;
	internal/install/tofu/* | internal/install/vmtofu*.go | internal/install/seed*.go | internal/install/vm.go | \
		internal/install/vmhealth*.go | internal/install/vmcreate*.go | internal/install/vmcli*.go | internal/install/hostd*.go)
		need smoke-install "$f"
		lifecycle "$f"
		vm_side "$f"
		;;
	internal/install/vm*.go)
		need smoke-install "$f"
		lifecycle "$f"
		need e2e-kairos "$f"
		;;
	internal/install/* | internal/payload/* | cmd/stowage-install/*)
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
	internal/kairos/files/* | internal/kairos/Dockerfile.tmpl | internal/backup/*) vm_side "$f" ;;
	internal/kairos/* | internal/graph/*) need e2e-kairos "$f" ;;
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
	Dockerfile | Makefile) # the binaries' build, the targets
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
	test/e2e-kairos.sh) need e2e-kairos "$f" ;;
	test/e2e-vm.sh) need e2e-vm "$f" ;;
	test/e2e-full-vm.sh | test/vm.sh) need e2e-full-vm "$f" ;;
	test/lib.sh | test/pins.sh | test/sweep.sh) # shared by every suite; the short ones exercise it
		need e2e "$f"
		lifecycle "$f"
		;;
	esac
done

if ! $code; then
	echo "gate: nothing but Markdown changed against $base: no gate (a self-review against the code is enough)"
	exit 0
fi
order=(smoke-install e2e e2e-docker e2e-full e2e-stateless e2e-k8s e2e-full-k8s e2e-kairos e2e-vm e2e-full-vm)
parallel=() chain=()
echo "gate: against $base, ${#changed[@]} paths changed; make check, make build, lint, then:"
for s in "${order[@]}"; do
	if [[ -n ${why[$s]:-} ]]; then
		echo "  $s  (${why[$s]})"
		case $s in
		e2e-kairos | e2e-vm | e2e-full-vm) chain+=("$s") ;;
		*) parallel+=("$s") ;;
		esac
	else
		echo "  ($s not needed)"
	fi
done
$plan && exit 0

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
# The kairos chain runs one suite at a time, beside the parallel suites.
chain_rc=0 pid=""
if ((${#chain[@]} > 0)); then
	(for s in "${chain[@]}"; do make --output-sync=target "$s" || exit 1; done) &
	pid=$!
fi
rc=0
if ((${#parallel[@]} > 0)); then
	make -k -j --output-sync=target "${parallel[@]}" || rc=1
fi
if [[ -n $pid ]]; then
	wait "$pid" || chain_rc=1
fi
((rc == 0 && chain_rc == 0)) || { echo "gate: FAIL" >&2; exit 1; }
echo "gate: PASS"
