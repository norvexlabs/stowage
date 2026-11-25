package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/version"
)

// stagedInputsFile holds, on the persistent partition beside the install directory and root-only,
// the inputs an `upgrade` gave for the boot into the release whose OS it wrote (stageInputs).
func stagedInputsFile(dir string) string { return filepath.Clean(dir) + ".os-upgrade-inputs" }

// staged is stagedInputsFile's content: the release the inputs are for (releaseMark), the channel
// the upgrade was asked on ("": the install's), and the inputs, KEY: value.
type staged struct {
	Release string            `json:"release"`
	Channel string            `json:"channel,omitempty"`
	Inputs  map[string]string `json:"inputs,omitempty"`
}

// stageInputs writes, for the boot into release m, the inputs the upgrade gives and the channel it
// was asked on (which that boot's refusal names again), replacing what an earlier upgrade staged;
// neither removes it, so a boot never takes what an upgrade that gave nothing did not give.
func stageInputs(dir string, m *release.Manifest, channel string, inputs map[string]string) error {
	path := stagedInputsFile(dir)
	if len(inputs) == 0 && channel == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	raw, err := json.Marshal(staged{Release: releaseMark(m.ProjectVersion, m.Revision), Channel: channel, Inputs: inputs})
	if err != nil {
		return err
	}
	return writeAtomic(path, bytes.NewReader(raw), 0o600, nil)
}

// readStaged reads what an upgrade staged, nil when nothing is.
func (in *Installer) readStaged() (*staged, error) {
	path := stagedInputsFile(in.O.Dir)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st staged
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &st, nil
}

// thisRelease is this installer's release as a mark names it.
func (in *Installer) thisRelease() string {
	return releaseMark(in.R.Manifest.ProjectVersion, in.R.Manifest.Revision)
}

// stagedInputs are the inputs an upgrade staged for this OS's release, nil when none, and the
// channel it was asked on. What was staged for another release is left out, logged: this boot is
// not the one into it.
func (in *Installer) stagedInputs() (map[string]string, string, error) {
	st, err := in.readStaged()
	if err != nil || st == nil {
		return nil, "", err
	}
	if this := in.thisRelease(); st.Release != this {
		in.O.logf("%s holds what an upgrade gave release %s, not this OS's %s: left out", stagedInputsFile(in.O.Dir), st.Release, this)
		return nil, "", nil
	}
	return st.Inputs, st.Channel, nil
}

// dropStagedInputs removes what an upgrade staged for this OS's release, whichever way its boot
// went: never read again, a value given once stays in the install's records alone. What was
// staged for another release is that release's boot's to take — the boot into it first runs the
// installer of the release the install runs, whose boot must leave it — and the next upgrade
// replaces or removes it; so is a file that cannot be read, which the boot into its release
// reports.
func (in *Installer) dropStagedInputs() {
	if st, err := in.readStaged(); err != nil || st == nil || st.Release != in.thisRelease() {
		return
	}
	if err := os.Remove(stagedInputsFile(in.O.Dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		in.O.logf("warning: removing %s: %v", stagedInputsFile(in.O.Dir), err)
	}
}

// upgradeInputs are the inputs the VM's `upgrade` (or `reset-db --new-baseline`) gives the boot
// into the release: --answers's, then --set's and --set-file's over them, as Docker's upgrade
// orders them; nil when none. An answers file's tls section is refused, as the upgrade keeps what
// the install records.
func upgradeInputs(sets, setFiles []string, answers string) (map[string]string, error) {
	set, err := parseSets(sets)
	if err != nil {
		return nil, err
	}
	if err := addSetFiles(set, setFiles); err != nil {
		return nil, err
	}
	out := map[string]string{}
	if answers != "" {
		var o Options
		if err := o.readAnswersFile(answers, false); err != nil {
			return nil, err
		}
		maps.Copy(out, o.Answers)
	}
	maps.Copy(out, set)
	if len(out) == 0 {
		return nil, nil
	}
	// The daemon reads them in one request of up to maxSecretsFile and 64 KiB: measured as sent.
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if n := len(raw); n > maxSecretsFile {
		return nil, refuse("give the inputs the release requires, and the rest by a re-run of `install` once it runs", "the inputs given are %d bytes, over the %d an upgrade or a reset-db carries", n, maxSecretsFile)
	}
	return out, nil
}

// inputsNotTaken says why an upgrade that does not move the install to another release takes no
// input: before the first install (first), that install takes its own; else the install runs the
// release already.
func inputsNotTaken(first bool) string {
	if first {
		return "the first install takes its inputs from its settings (`vm create --set`), and a re-run of `install` changes them once it runs"
	}
	return "the install runs it already, and a re-run of `install` changes a recorded input"
}

// stagedInputsSince is the first Stowage whose boot takes the inputs an upgrade staged for it, and
// crossedInputsSince the first whose crossing's boot takes those a reset-db --new-baseline staged.
const (
	stagedInputsSince  = "0.43.0"
	crossedInputsSince = "0.45.0"
)

// takesStaged reports whether the boot into release m takes the inputs an upgrade stages.
func (h *Hostd) takesStaged(m *release.Manifest) bool {
	return stagedInputsBy(m.StowageVersion, h.Stowage, stagedInputsSince)
}

// stagedInputsBy reports whether what Stowage v built — a release's boot, an OS's `upgrade` or
// `reset-db` — takes the inputs a command stages, which Stowage since first did: v is own (the
// Stowage running this), or since or a later one.
func stagedInputsBy(v, own, since string) bool {
	if sameStowage(v, own) {
		return true
	}
	c, err := version.SemVer{}.Compare(v, since)
	return err == nil && c >= 0
}

// upgradeArgs are the arguments of the VM's `upgrade` that reaches release version, as a fix
// names them: the channel it was asked on (none: the install's), --unordered when it was given.
func upgradeArgs(channel, version string, unordered bool) string {
	args := ""
	if channel != "" {
		args += " --channel " + channel
	}
	args += " --version " + version
	if unordered {
		args += " --unordered"
	}
	return args
}
