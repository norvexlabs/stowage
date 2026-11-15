#!/usr/bin/env bash
# Proves test/release-notes.sh, which the release workflow runs on a pushed tag, against fixture
# changelogs: a dated entry passes, an undated or absent one refuses; the notes stop at the next
# entry and at a link definition, and an entry with no text refuses. Run by test/gate.sh after
# lint when the workflows or these scripts change.
set -euo pipefail
cd "$(dirname "$0")/.."
d="$(mktemp -d)"
trap 'rm -rf "$d"' EXIT
fail() { echo "release.sh: $*" >&2; exit 1; }
cat >"$d/dated.md" <<'MD'
# Changelog

## [1.2.0] - 2026-11-15

- One.
- Two.

## [1.1.0] - 2026-11-01

- Earlier.
MD
cat >"$d/link.md" <<'MD'
## [1.2.0] - 2026-11-15

- Only.

[1.2.0]: https://example.com/1.2.0
MD
printf '## [1.2.0]\n\n- Undated.\n' >"$d/undated.md"
printf '## [1.2.0] - 2026-11-15\n## [1.1.0] - 2026-11-01\n\n- Earlier.\n' >"$d/empty.md"
printf '## [1.2.0] - 2026-11-1\n\n- Short date.\n' >"$d/short.md"

n=test/release-notes.sh
$n dated 1.2.0 "$d/dated.md" || fail "a dated entry was refused"
! $n dated 1.2.0 "$d/undated.md" 2>/dev/null || fail "an undated entry passed"
! $n dated 1.2.0 "$d/short.md" 2>/dev/null || fail "a date of 7 digits passed"
! $n dated 1.3.0 "$d/dated.md" 2>/dev/null || fail "another version's entry passed"
[[ "$($n notes 1.2.0 "$d/dated.md")" == $'\n- One.\n- Two.' ]] || fail "the notes did not stop at the next entry: $($n notes 1.2.0 "$d/dated.md")"
[[ "$($n notes 1.2.0 "$d/link.md")" == $'\n- Only.' ]] || fail "the notes did not stop at a link definition"
! $n notes 1.2.0 "$d/empty.md" >/dev/null 2>&1 || fail "an entry with no text gave notes"
! $n notes 1.2.0 "$d/undated.md" >/dev/null 2>&1 || fail "an undated entry gave notes"
echo "release.sh: PASS"
