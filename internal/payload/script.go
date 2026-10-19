package payload

import (
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
)

// ScriptCommand is the installer's command that prints this script: the installer image's
// default command, so `docker run --rm <installer image>` prints it.
const ScriptCommand = "script"

// scriptName is what a project name may be to name the extracted binary.
var scriptName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Script writes the bootstrap script an installer image prints: a POSIX sh stub with the installer
// binary inside it, base64 in a here-document, so the script works the same saved to a file and
// piped (`| bash -s -- <flags>`) — nothing is read back from $0. It extracts the binary into the
// first private directory where a program may run — under the current directory, the user's
// cache, then the temporary directory (a hardened host often mounts /tmp noexec) — runs it with
// the script's arguments and the operator's umask, and removes it, interrupted or not.
func Script(w io.Writer, name, ctl string, installer io.Reader) error {
	for _, n := range []string{name, ctl} {
		if !scriptName.MatchString(n) {
			return fmt.Errorf("script: %q cannot name the installer", n)
		}
	}
	if _, err := fmt.Fprintf(w, `#!/bin/sh
# %[1]s installer: extracts the %[2]s binary carried below and runs it.
# Usage: sh %[1]s.sh [flags]   (or: docker run --rm <installer image> | sh -s -- [flags])
set -eu
# The first private directory where a program may run: the current directory, the user's cache,
# then the temporary one (a hardened host often mounts /tmp noexec).
dir=
for base in . "${XDG_CACHE_HOME:-${HOME:-/nonexistent}/.cache}" "${TMPDIR:-/tmp}"; do
	d=$( (umask 077; mktemp -d "$base/.%[2]s.XXXXXX") 2>/dev/null ) || continue
	printf '#!/bin/sh\nexit 0\n' > "$d/probe" && chmod 700 "$d/probe" && "$d/probe" 2>/dev/null && { dir=$d; break; }
	rm -rf "$d"
done
[ -n "$dir" ] || { echo "no writable directory lets the installer run (read-only or noexec): run it from another directory" >&2; exit 1; }
trap 'rm -rf "$dir"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
base64 -d > "$dir/%[2]s" <<'STOWAGE_INSTALLER_END'
`, name, ctl); err != nil {
		return err
	}
	enc := base64.NewEncoder(base64.StdEncoding, &lineWriter{w: w})
	if _, err := io.Copy(enc, installer); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, `
STOWAGE_INSTALLER_END
chmod 700 "$dir/%[2]s"
# Piped, stdin is this script: prompts read the terminal when there is one.
if [ ! -t 0 ] && (exec < /dev/tty) 2>/dev/null; then
	"$dir/%[2]s" "$@" < /dev/tty
else
	"$dir/%[2]s" "$@"
fi
`, name, ctl)
	return err
}

// lineWriter breaks base64 into 76-character lines, so no shell or editor meets one huge line.
type lineWriter struct {
	w   io.Writer
	col int
}

func (l *lineWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := min(76-l.col, len(p))
		if _, err := l.w.Write(p[:k]); err != nil {
			return n, err
		}
		n += k
		l.col += k
		p = p[k:]
		if l.col == 76 {
			if _, err := io.WriteString(l.w, "\n"); err != nil {
				return n, err
			}
			l.col = 0
		}
	}
	return n, nil
}
