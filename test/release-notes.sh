#!/usr/bin/env bash
# The release workflow's reading of CHANGELOG.md, apart so test/release.sh proves it:
#   release-notes.sh dated <version> [changelog]   exit 0 when the entry `## [<version>] - YYYY-MM-DD` is there
#   release-notes.sh notes <version> [changelog]   print that entry's text, up to the next entry or a link
#                                                  definition; refuse when there is none
set -euo pipefail
cmd=${1:?dated|notes} v=${2:?version} f=${3:-CHANGELOG.md}
case $cmd in
dated)
	awk -v h="## [$v] - " 'index($0, h) == 1 && substr($0, length(h) + 1) ~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]$/ {found = 1} END {exit !found}' "$f" ||
		{ echo "$f's $v entry has no date" >&2; exit 1; }
	;;
notes)
	notes="$(awk -v h="## [$v] - " 'index($0, h) == 1 {on = 1; next} on && (/^## \[/ || /^\[[^]]+\]: /) {exit} on' "$f")"
	grep -q . <<<"$notes" || { echo "no notes for $v in $f" >&2; exit 1; }
	printf '%s\n' "$notes"
	;;
*) echo "release-notes.sh: dated or notes, not $cmd" >&2; exit 2 ;;
esac
