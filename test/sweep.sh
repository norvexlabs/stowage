#!/usr/bin/env bash
# Removes what earlier end-to-end runs left on this host, never what a live run holds: a run is
# live while its process is (the PID its registry, cluster and work directory are named by or
# record). Run at the end of every suite (lib.sh's cleanup) and after make smoke-install.
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

# Registries of dead runs, and their backups' WebDAV server: stowage-e2e-zot-<pid>[-mirror|-dav],
# stowage-smoke-zot-<pid>.
for n in $(docker ps -a --format '{{.Names}}' | grep -E '^stowage-(e2e|smoke)-zot-[0-9]+(-mirror|-dav)?$'); do
	pid=${n#stowage-*-zot-}
	pid=${pid%-mirror}
	alive "${pid%-dav}" || docker rm -f "$n" >/dev/null 2>&1
done

# kind clusters of dead runs, stowage-e2e-<pid>: their node containers (and anonymous volumes),
# then their network.
for c in $(docker ps -a --filter label=io.x-k8s.kind.cluster --format '{{.Label "io.x-k8s.kind.cluster"}}' | sort -u | grep -E '^stowage-e2e-[0-9]+$'); do
	alive "${c#stowage-e2e-}" || docker ps -aq --filter "label=io.x-k8s.kind.cluster=$c" | xargs -r docker rm -f -v >/dev/null 2>&1
done
for n in $(docker network ls --format '{{.Name}}' | grep -E '^stowage-e2e-[0-9]+$'); do
	alive "${n#stowage-e2e-}" || docker network rm "$n" >/dev/null 2>&1
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

# Work directories of dead runs: lib.sh records its PID in .pid; one without (older than an hour)
# is dead unless a running test registry mounts from it. Files a privileged container wrote there
# are removed by a container.
mounts=$(docker ps --filter name=stowage- --format '{{.ID}}' | xargs -r docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' 2>/dev/null)
for d in "$CACHE"/stowage-e2e.*; do
	[[ -d $d ]] || continue
	if [[ -f $d/.pid ]]; then
		alive "$(cat "$d/.pid")" && continue
	else
		[[ -n $(find "$d" -maxdepth 0 -mmin +60) ]] || continue
		[[ $mounts == *"$d/"* ]] && continue
	fi
	rm -rf "$d" 2>/dev/null || docker run --rm --network none -v "$d:/w" --entrypoint sh "$HTPASSWD" -c 'rm -rf /w/* /w/.[!.]*' >/dev/null 2>&1
	rm -rf "$d" 2>/dev/null
done

# Stacks of installs whose directory is gone (a run's work directory, a Go test's temporary one):
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
		[[ -f $d ]] && alive "$(cat "$d")" && return 0
	done
	docker ps --format '{{.Names}}' | grep -qE '^stowage-smoke-zot-[0-9]+$'
}
docker ps -a --format '{{.ID}} {{.Label "com.docker.compose.project"}} {{.Label "com.docker.compose.project.working_dir"}}' |
	while read -r id project dir; do
		test_project "$project" && [[ -n $dir && ! -d $dir ]] && docker rm -f -v "$id" >/dev/null 2>&1
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
