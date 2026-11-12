#!/usr/bin/env bash
# Removes what earlier end-to-end runs left on this host, never what a live run holds. A run's
# work directory, registries, cluster, network and ports are named by its random RUN (lib.sh's);
# a run is live while its process is (the PID its work directory records, trusted in its own PID
# namespace alone), and a run this one cannot judge (another container's) for six hours after its
# thing was made. Run at the end of every suite (lib.sh's cleanup) and after make
# smoke-install.
#
#   bash test/sweep.sh [cache dir]     (default: XDG_CACHE_HOME, else ~/.cache)
#
# Kept on purpose, one copy each, which the next run reuses: the pinned images every run pulls
# (zot, httpd, cosign, helm, rclone, kind's node, auroraboot, postgres, the front's base) and the newest
# disk-tools image. A VM kept with E2E_KEEP_VM and its state are the operator's to destroy.
set -u

CACHE=${1:-${XDG_CACHE_HOME:-$HOME/.cache}}
. "$(dirname "${BASH_SOURCE[0]}")/pins.sh"
alive() { [[ $1 =~ ^[0-9]+$ ]] && kill -0 "$1" 2>/dev/null; }
ns=$(readlink /proc/self/ns/pid)
now=$(date +%s)

# pid_live <work dir>: 0 its run is live, 1 dead, 2 unknown here (its PID is another namespace's,
# or a namespace does not read). A directory without a PID is being made: live.
pid_live() {
	[[ -f $1/.pid ]] || return 0
	[[ -n $ns && $(cat "$1/.pidns" 2>/dev/null) == "$ns" ]] || return 2
	alive "$(cat "$1/.pid")" && return 0
	return 1
}
not_dead() {
	pid_live "$1"
	(($? != 1))
}
# young <created, as docker inspect or date -Iseconds prints it>: made in the last six hours, the
# longest suite's time with room to spare. Only the date, time and offset are read (a network's
# ends with a zone's name date may not read); a time that does not read is young.
young() {
	local f created
	read -ra f <<<"$1"
	((${#f[@]})) || return 0
	created=$(date -d "${f[*]:0:3}" +%s 2>/dev/null) || return 0
	((created > now - 6 * 3600))
}
# run_live <run> <created>: whether the run a thing is named by is live: by its work directory
# here, else (a run in another container on the same Docker) by the thing's age.
run_live() {
	local d=$CACHE/stowage-e2e.$1
	if [[ -d $d ]]; then
		pid_live "$d"
		case $? in 0) return 0 ;; 1) return 1 ;; esac
	fi
	young "$2"
}
created() { docker inspect -f '{{.Created}}' "$@" 2>/dev/null | head -n1; }

# Registries of dead runs, and their backups' WebDAV server: stowage-e2e-zot-<run>[-mirror|-dav]
# (the smoke test's too: it records its run as lib.sh does).
for n in $(docker ps -a --format '{{.Names}}' | grep -E '^stowage-e2e-zot-r[0-9a-f]+(-mirror|-dav)?$'); do
	r=${n#stowage-e2e-zot-}
	r=${r%-mirror}
	run_live "${r%-dav}" "$(created "$n")" || docker rm -f "$n" >/dev/null 2>&1
done

# e2e-kairos's libvirt daemon of a dead run, stowage-e2e-libvirtd-<run> (with its domains).
for n in $(docker ps -a --format '{{.Names}}' | grep -E '^stowage-e2e-libvirtd-r[0-9a-f]+$'); do
	run_live "${n#stowage-e2e-libvirtd-}" "$(created "$n")" || docker rm -f "$n" >/dev/null 2>&1
done
# kind clusters of dead runs, stowage-e2e-<run>: their node containers (and anonymous volumes),
# then their network.
for c in $(docker ps -a --filter label=io.x-k8s.kind.cluster --format '{{.Label "io.x-k8s.kind.cluster"}}' | sort -u | grep -E '^stowage-e2e-r[0-9a-f]+$'); do
	nodes=$(docker ps -aq --filter "label=io.x-k8s.kind.cluster=$c")
	# shellcheck disable=SC2086 # one ID per word
	run_live "${c#stowage-e2e-}" "$(created $nodes)" || xargs -r docker rm -f -v <<<"$nodes" >/dev/null 2>&1
done
for n in $(docker network ls --format '{{.Name}}' | grep -E '^stowage-e2e-r[0-9a-f]+$'); do
	run_live "${n#stowage-e2e-}" "$(docker network inspect -f '{{.Created}}' "$n" 2>/dev/null)" || docker network rm "$n" >/dev/null 2>&1
done

# Ports reserved by runs no longer live (lib.sh's free_port): stowage-e2e-ports/<port>, whose run
# file names the run; one without a run file a minute old (its run died while reserving it).
for r in "$CACHE"/stowage-e2e-ports/*; do
	[[ -d $r ]] || continue
	if [[ -f $r/run ]]; then
		run_live "$(cat "$r/run")" "$(date -r "$r" -Iseconds)" && continue
	else
		[[ -n $(find "$r" -maxdepth 0 -mmin +1) ]] || continue
	fi
	rm -rf "$r"
done

# Images of a test registry that no longer runs: each reference <ipv4>:<port>/{notes,atlas,smoke,site,t}/…
# (a tag, or a digest it was pulled by) whose port no running container publishes, removed by
# that reference alone: the image goes with its last one (docker rmi refuses the last reference of
# an image a container uses). The references are listed before the live ports are read: an image a
# run pulls after the listing is not in it, and a run that started before it publishes its port
# when the ports are read. A failed read of the ports removes nothing.
refs=$(docker images -q | sort -u | xargs -r docker image inspect -f '{{range .RepoTags}}{{println .}}{{end}}{{range .RepoDigests}}{{println .}}{{end}}' 2>/dev/null |
	grep -E '^[0-9]+(\.[0-9]+){3}:[0-9]+/(notes|atlas|smoke|site|t)/' | sort -u)
if ports=$(docker ps --format '{{.Ports}}'); then
	live=" $(grep -oE ':[0-9]+->' <<<"$ports" | grep -oE '[0-9]+' | tr '\n' ' ') "
	while read -r ref; do
		[[ -n $ref ]] || continue
		port=${ref#*:}
		port=${port%%/*}
		[[ $live == *" $port "* ]] || docker rmi "$ref" >/dev/null 2>&1
	done <<<"$refs"
else
	echo "sweep: docker ps failed; the test registries' images are kept" >&2
fi

# Work directories of dead runs: lib.sh records its PID in .pid; one whose PID this sweep cannot
# judge is dead after six hours; one without a PID (older than an hour) is dead unless a running
# test registry mounts from it. Files a privileged container wrote there
# are removed by a container.
mounts=$(docker ps --filter name=stowage- --format '{{.ID}}' | xargs -r docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' 2>/dev/null)
for d in "$CACHE"/stowage-e2e.*; do
	[[ -d $d ]] || continue
	if [[ -f $d/.pid ]]; then
		pid_live "$d"
		case $? in
		0) continue ;;
		2) [[ -n $(find "$d" -maxdepth 0 -mmin +360) ]] || continue ;;
		esac
	else
		[[ -n $(find "$d" -maxdepth 0 -mmin +60) ]] || continue
		[[ $mounts == *"$d/"* ]] && continue
	fi
	# The scanners its audits loaded (lib.sh's scanner_fresh), by tag, but a name a live run
	# records too (its registry on the same address again).
	[[ -f $d/.scanners ]] && sort -u "$d/.scanners" | while read -r repo; do
		for l in "$CACHE"/stowage-e2e.*/.pid; do
			[[ -f $l && ${l%/.pid} != "$d" ]] && not_dead "${l%/.pid}" && grep -qxF "$repo" "${l%/.pid}/.scanners" 2>/dev/null && continue 2
		done
		docker images --format '{{.Repository}}:{{.Tag}}' "$repo" | xargs -r docker rmi >/dev/null 2>&1 || true
	done
	rm -rf "$d" 2>/dev/null || docker run --rm --network none -v "$d:/w" --entrypoint sh "$HTPASSWD" -c 'rm -rf /w/* /w/.[!.]*' >/dev/null 2>&1
	rm -rf "$d" 2>/dev/null
done

# Stacks of installs whose directory is gone (a run's work directory, a Go test's temporary one;
# one in another container's work directory once its run is no longer live):
# their containers, then the volumes and networks of every test project left with no container for
# an hour, while no other run is live (a live run's stack is down between a reset-db's or a
# rollback's down and up). A test project is one whose install directory the suites or the smoke
# test named (stowage-e2e-… or stowage-smoke-…, so its compose project is
# <name>-stowage-e2e-…-<crc>), never a name an operator's install takes by default: a volume
# carries no directory to check.
test_project() { [[ $1 =~ ^[a-z0-9-]+-stowage-(e2e|smoke)-[a-z0-9-]+-[0-9a-f]{8}$ ]]; }
live_run() {
	local d
	for d in "$CACHE"/stowage-e2e.*/.pid; do
		[[ -f $d ]] && not_dead "${d%/.pid}" && return 0
	done
	return 1
}
docker ps -a --format '{{.ID}} {{.Label "com.docker.compose.project"}} {{.Label "com.docker.compose.project.working_dir"}}' |
	while read -r id project dir; do
		if ! test_project "$project" || [[ -z $dir || -d $dir ]]; then continue; fi
		# A run's install directory in a work directory outside this cache (another container's on
		# the same Docker): its run judges it. One in this cache is gone with its run.
		[[ $dir != "$CACHE"/* && $dir =~ /stowage-e2e\.(r[0-9a-f]+)/ ]] &&
			run_live "${BASH_REMATCH[1]}" "$(created "$id")" && continue
		docker rm -f -v "$id" >/dev/null 2>&1
	done
projects=" $(docker ps -a --format '{{.Label "com.docker.compose.project"}}' | sort -u | tr '\n' ' ') "
old=$(date -d '-1 hour' +%s)
live_run && kinds=() || kinds=(volume network)
for kind in "${kinds[@]}"; do
	docker "$kind" ls --format '{{.Name}} {{.Label "com.docker.compose.project"}}' | while read -r n project; do
		test_project "$project" && [[ $projects != *" $project "* ]] || continue
		created=$(date -d "$(docker "$kind" inspect -f '{{.CreatedAt}}' "$n" 2>/dev/null | sed 's/ [A-Z]*$//')" +%s 2>/dev/null) || continue
		((created < old)) && docker "$kind" rm "$n" >/dev/null 2>&1
	done
done

# Containers of the pinned helper images an interrupted `docker run --rm` left created, never
# started, over ten minutes ago.
old=$(date -d '-10 minutes' +%s)
for img in "$HTPASSWD" "$COSIGN" "$HELM"; do
	docker ps -a --filter status=created --format '{{.ID}} {{.Image}} {{.Label "com.docker.compose.project"}}' | while read -r id image project; do
		[[ -z $project && $image == "${img%%[:@]*}"* ]] || continue
		created=$(date -d "$(docker inspect -f '{{.Created}}' "$id")" +%s 2>/dev/null) || continue
		((created < old)) && docker rm -f "$id" >/dev/null 2>&1
	done
done

# Disk tools: the newest image only.
docker images stowage-disktools --format '{{.CreatedAt}}|{{.Repository}}:{{.Tag}}' | sort -r | tail -n +2 | cut -d'|' -f2 |
	xargs -r docker rmi >/dev/null 2>&1

exit 0
