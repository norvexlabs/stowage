package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// raise is an Alpine package a Dockerfile's stage raises over its base, to version or past it; a
// raise of no package is an `apk upgrade` of every one.
type raise struct {
	file, base, pkg, version string
}

var (
	argDecl    = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?:=(.*))?$`)
	variable   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)
	apkPackage = regexp.MustCompile(`^[a-z0-9][a-z0-9+._-]*$`)
	// An apk version: digits and dots, a letter, suffixes, a release.
	apkVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*[a-z]?(_(alpha|beta|pre|rc|cvs|svn|git|hg|p)[0-9]*)*(-r[0-9]+)?$`)
	shellSep   = regexp.MustCompile(`&&|\|\||[;|]`)
	redirect   = regexp.MustCompile(`^[0-9]*[<>]`)
)

// raises reads them from a Dockerfile, each with the base of its stage: the image its FROM names,
// its global ARGs resolved, or the base of the earlier stage it names. A package raised in another
// form than `pkg>=version` is refused: a pin or a ceiling is no raise this check can judge.
func raises(path string, file []byte) ([]raise, error) {
	args := map[string]string{}
	stages := map[string]string{}
	base := ""
	var out []raise
	for _, line := range strings.Split(instructions(file), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "ARG":
			if base != "" {
				continue // a stage's ARG never reaches a FROM
			}
			for _, a := range fields[1:] {
				m := argDecl.FindStringSubmatch(a)
				if m == nil {
					return nil, fmt.Errorf("%s: ARG %q is not a declaration this check reads", path, a)
				}
				args[m[1]] = strings.Trim(m[2], `"'`)
			}
		case "FROM":
			var image, as string
			for i := 1; i < len(fields); i++ {
				switch {
				case strings.HasPrefix(fields[i], "--"):
				case strings.EqualFold(fields[i], "AS") && i+1 < len(fields):
					as = strings.ToLower(fields[i+1])
					i++
				case image == "":
					image = fields[i]
				}
			}
			var unset []string
			image = variable.ReplaceAllStringFunc(image, func(v string) string {
				m := variable.FindStringSubmatch(v)
				name := m[1] + m[2]
				value, ok := args[name]
				if !ok || value == "" {
					unset = append(unset, name)
				}
				return value
			})
			if image == "" || len(unset) > 0 {
				return nil, fmt.Errorf("%s: FROM %s: its image is not resolved by the file's global ARGs (%s)", path, strings.Join(fields[1:], " "), strings.Join(unset, ", "))
			}
			if earlier, ok := stages[strings.ToLower(image)]; ok {
				image = earlier
			}
			base = image
			if as != "" {
				stages[as] = base
			}
		case "RUN":
			rs, err := runRaises(path, base, line)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		}
	}
	return out, nil
}

// runRaises reads one RUN instruction's command. The exec form (a JSON array of strings) is its
// words, or, when it runs a shell's -c in the form shellScript reads (behind busybox or env, too),
// that script; a JSON array that does not decode runs as shell form, as Docker runs it. An exec
// form naming apk otherwise than as a word of its own in a command that is not a shell, or in a
// script shellScript reads, is refused: its raises would go unjudged.
func runRaises(path, base, line string) ([]raise, error) {
	rest := strings.TrimSpace(line)
	rest = strings.TrimSpace(rest[len(strings.Fields(rest)[0]):])
	for strings.HasPrefix(rest, "--") {
		end := strings.IndexFunc(rest, unicode.IsSpace)
		if end < 0 {
			return nil, nil
		}
		rest = strings.TrimSpace(rest[end:])
	}
	script := rest
	var argv []string
	if strings.HasPrefix(rest, "[") && json.Unmarshal([]byte(rest), &argv) == nil {
		for len(argv) > 1 && slices.Contains([]string{"busybox", "env"}, filepath.Base(argv[0])) {
			argv = argv[1:]
		}
		shell := len(argv) > 0 && slices.Contains([]string{"sh", "ash", "bash", "dash"}, filepath.Base(argv[0]))
		isApk := func(w string) bool { return filepath.Base(w) == "apk" }
		s := ""
		if shell {
			s = shellScript(argv)
		}
		switch {
		case s != "":
			script = s
		case !shell && slices.ContainsFunc(argv, isApk):
			return apkRaises(path, base, argv)
		case slices.ContainsFunc(strings.Fields(strings.Join(argv, " ")), isApk):
			return nil, fmt.Errorf("%s: RUN %s: an exec form is read only as [\"apk\", …] or [\"sh\", \"-c\", \"<script>\"] (option words -e, -u, -x before -c): write it so", path, rest)
		default:
			return nil, nil
		}
	}
	var out []raise
	for _, command := range shellSep.Split(script, -1) {
		rs, err := apkRaises(path, base, strings.Fields(command))
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// shellScript is the script of a shell's exec form (argv, the shell first) in the one form it is
// read in: option words each a cluster of e, u and x (-e, -eu), then -c or such a cluster ending
// in c (-ec, -euxc), then the script, which is not an option; the words after it are its $0 and
// arguments. "" for any other form: sh's own option grammar is not emulated.
func shellScript(argv []string) string {
	i := 1
	for i < len(argv) && shellFlags.MatchString(argv[i]) {
		i++
	}
	if i+1 >= len(argv) || !shellC.MatchString(argv[i]) || strings.HasPrefix(argv[i+1], "-") || strings.HasPrefix(argv[i+1], "+") {
		return ""
	}
	return argv[i+1]
}

// shellFlags is a shell's option word shellScript reads before -c: a cluster of e, u and x.
var shellFlags = regexp.MustCompile(`^-[eux]+$`)

// shellC is the option word shellScript reads as -c: -c, or a cluster of e, u and x ending in c.
var shellC = regexp.MustCompile(`^-[eux]*c$`)

// apkRaises reads one shell command: an `apk add`'s constrained packages, an `apk upgrade`.
func apkRaises(path, base string, words []string) ([]raise, error) {
	for len(words) > 0 && filepath.Base(words[0]) != "apk" {
		words = words[1:]
	}
	verb := -1
	for i := 1; i < len(words); i++ {
		if !strings.HasPrefix(words[i], "-") {
			verb = i
			break
		}
	}
	if verb < 0 {
		return nil, nil
	}
	if base == "" {
		return nil, fmt.Errorf("%s: an apk command before any FROM", path)
	}
	switch words[verb] {
	case "upgrade":
		return []raise{{path, base, "", ""}}, nil
	case "add":
	default:
		return nil, nil
	}
	var out []raise
	for _, w := range words[verb+1:] {
		if redirect.MatchString(w) {
			break // the command's words end at a redirection
		}
		w = strings.Trim(w, `"'`)
		if strings.HasPrefix(w, "-") || !strings.ContainsAny(w, "<>=~") {
			continue
		}
		pkg, version, ok := strings.Cut(w, ">=")
		if !ok || !apkPackage.MatchString(pkg) || !apkVersion.MatchString(version) {
			return nil, fmt.Errorf("%s: apk add %q: a package is raised as 'pkg>=version', the only constraint self-audit judges", path, w)
		}
		out = append(out, raise{path, base, pkg, version})
	}
	return out, nil
}

// probeBase returns the version of a package a base image carries ("" when it carries none) and
// how apk orders it against a version: "<", "=" or ">".
type probeBase func(ctx context.Context, image, pkg, version string) (installed, order string, err error)

// probeScript reads the installed database (apk 2 and 3 alike) and orders with apk's own comparer.
const probeScript = `v=$(awk -v p="$1" '$0 == "P:" p {f = 1; next} f && /^V:/ {print substr($0, 3); exit} /^$/ {f = 0}' /lib/apk/db/installed) || exit 3
[ -n "$v" ] || { echo absent; exit 0; }
apk version -c "$2" >/dev/null || { echo "apk refuses the version $2" >&2; exit 3; }
echo "$v $(apk version -t "$v" "$2")"`

// dockerProbe runs the base, with no network, to ask its own apk.
func dockerProbe(ctx context.Context, image, pkg, version string) (string, string, error) {
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--entrypoint", "sh", image, "-c", probeScript, "sh", pkg, version)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("docker run %s: %w: %s", image, err, strings.TrimSpace(stderr.String()))
	}
	return parseProbe(out.String())
}

// parseProbe reads probeScript's answer.
func parseProbe(out string) (string, string, error) {
	f := strings.Fields(out)
	switch {
	case len(f) == 1 && f[0] == "absent":
		return "", "", nil
	case len(f) == 2 && apkVersion.MatchString(f[0]) && (f[1] == "<" || f[1] == "=" || f[1] == ">"):
		return f[0], f[1], nil
	}
	return "", "", fmt.Errorf("unexpected answer from the base's apk: %q", out)
}

// dockerfiles is every Dockerfile under root, named by its path from there.
func dockerfiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		n := d.Name()
		if !d.IsDir() && (n == "Dockerfile" || strings.HasPrefix(n, "Dockerfile.") || strings.HasSuffix(n, ".Dockerfile")) {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// checkRaises prints every raise live or left to judge, and returns one error per stale one: a
// raise its base already satisfies holds nothing up, and is dropped.
func checkRaises(ctx context.Context, w io.Writer, rs []recipe, examples string, probe probeBase) error {
	type source struct {
		path string
		file []byte
	}
	var sources []source
	for _, r := range rs {
		sources = append(sources, source{r.path, r.file})
	}
	paths, err := dockerfiles(examples)
	if err != nil {
		return err
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sources = append(sources, source{p, data})
	}
	// One probe per base, package and version: the same raise in several files runs the base once.
	type question struct{ base, pkg, version string }
	type answer struct {
		installed, order string
		err              error
	}
	answers := map[question]answer{}
	var errs []error
	some := false
	for _, s := range sources {
		found, err := raises(s.path, s.file)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, r := range found {
			short, _, _ := strings.Cut(r.base, "@")
			if r.pkg == "" {
				fmt.Fprintf(w, "raise: %s runs apk upgrade over its base %s: judge it by hand\n", r.file, short)
				continue
			}
			q := question{r.base, r.pkg, r.version}
			a, ok := answers[q]
			if !ok {
				a.installed, a.order, a.err = probe(ctx, r.base, r.pkg, r.version)
				answers[q] = a
			}
			installed, order, err := a.installed, a.order, a.err
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("%s: %s in its base %s: %w", r.file, r.pkg, short, err))
			case installed == "":
				fmt.Fprintf(w, "raise: %s raises %s to >=%s over its base %s, which lacks it\n", r.file, r.pkg, r.version, short)
				some = true
			case order == "<":
				fmt.Fprintf(w, "raise: %s raises %s to >=%s over its base's %s (%s)\n", r.file, r.pkg, r.version, installed, short)
				some = true
			default:
				errs = append(errs, fmt.Errorf("raise: %s raises %s to >=%s, and its base %s already carries %s: the raise is stale, drop '%s>=%s' from its apk add (the base's pin stays)", r.file, r.pkg, r.version, short, installed, r.pkg, r.version))
			}
		}
	}
	if some {
		fmt.Fprintln(w, "reminder: each raise above is a stopgap, not the base's fix. When a release of the base carries the version, repin the base to it and drop the raise.")
	}
	return errors.Join(errs...)
}

// stopgaps judges every override and raise; a stale one fails the audit.
func stopgaps(ctx context.Context, w io.Writer, rs []recipe, examples string, fetch fetchMod, probe probeBase) error {
	return errors.Join(checkOverrides(ctx, w, rs, fetch), checkRaises(ctx, w, rs, examples, probe))
}
