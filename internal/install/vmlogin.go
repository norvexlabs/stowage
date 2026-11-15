package install

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// An install's login to the registry it reads (`registry-login`, `install --registry-login-file`):
// a Docker config holding one `auths` entry, for the host of the release's own registry or of its
// mirror. The operator mints one per install, so revoking it leaves every other install pulling.
// On the VM it is given over SSH after the boot, never in the cloud-config. The install's reads of
// that host use it: on the release's own registry it comes after the password TOKEN_ENV gives and
// before the account baked into the installer; the release's pull account is never sent to a
// mirror, nor the login to another host.

// maxRegistryLogin bounds a login file, as readFileOrStdin reads it: one entry is a few hundred
// bytes.
const maxRegistryLogin = maxRcloneConfig

// registryLoginFile records, beside the install directory (on the VM's persistent partition), the
// install's login, root's or the install owner's alone (0600). Beside the directory, not in it:
// a login given before the first install is kept by it, and upgrades and rollbacks, which move the
// install's state, never touch it.
func registryLoginFile(dir string) string {
	return strings.TrimSuffix(dir, "/") + ".registry-login.json"
}

// registryLogin is a login to a registry host.
type registryLogin struct {
	Host, User, Password string
}

// loginEntry is a Docker config's `auths` entry: `auth` (base64 of user:password), or `username`
// and `password`.
type loginEntry struct {
	Auth     string `json:"auth,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// loginHost is a Docker config's `auths` key as a registry host: `https://host/v1/` is host.
func loginHost(key string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	h, _, _ = strings.Cut(h, "/")
	return strings.ToLower(h)
}

// parseRegistryLogin strictly reads a login file: a Docker config with one `auths` entry and
// nothing else, its user and password in `auth` or in `username` and `password`. An error never
// quotes the file.
func parseRegistryLogin(raw []byte) (registryLogin, error) {
	const form = `a login file is a Docker config holding one "auths" entry, {"auths": {"HOST": {"auth": "BASE64(USER:PASSWORD)"}}} (or "username" and "password" for "auth")`
	var doc struct {
		Auths map[string]loginEntry `json:"auths"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return registryLogin{}, fmt.Errorf("not JSON: %s", form)
		}
		// A field's name is the config's, never a value: a credential store (credsStore,
		// credHelpers) keeps the login out of the file.
		return registryLogin{}, fmt.Errorf("%s: %s; a credential store's login is not in the file", strings.TrimPrefix(err.Error(), "json: "), form)
	}
	if dec.More() {
		return registryLogin{}, fmt.Errorf("more than one JSON value: %s", form)
	}
	if len(doc.Auths) != 1 {
		return registryLogin{}, fmt.Errorf("%d entries in auths: %s", len(doc.Auths), form)
	}
	var l registryLogin
	for k, e := range doc.Auths {
		l.Host = loginHost(k)
		switch {
		case e.Auth != "" && (e.Username != "" || e.Password != ""):
			return registryLogin{}, fmt.Errorf("auths %q has auth and username or password: give one: %s", l.Host, form)
		case e.Auth != "":
			dec, err := base64.StdEncoding.DecodeString(e.Auth)
			if err != nil {
				return registryLogin{}, fmt.Errorf("auths %q: auth is not base64: %s", l.Host, form)
			}
			var ok bool
			if l.User, l.Password, ok = strings.Cut(string(dec), ":"); !ok {
				return registryLogin{}, fmt.Errorf("auths %q: auth is not USER:PASSWORD in base64: %s", l.Host, form)
			}
		default:
			l.User, l.Password = e.Username, e.Password
		}
	}
	if l.Host == "" {
		return registryLogin{}, fmt.Errorf("auths has an entry with no host: %s", form)
	}
	if l.User == "" || l.Password == "" {
		return registryLogin{}, fmt.Errorf("auths %q has no user or no password: %s", l.Host, form)
	}
	return l, nil
}

// marshal is l as the file records it: one `auths` entry, in `auth`.
func (l registryLogin) marshal() ([]byte, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(l.User + ":" + l.Password))
	return json.Marshal(map[string]any{"auths": map[string]loginEntry{l.Host: {Auth: auth}}})
}

// heldLogin is the login the install holds (`registry-login`, `install --registry-login-file`)
// for its reads of prefix; nil when it holds none. On Kubernetes no file is read: it is the one
// this run's --registry-login-file gave (k8sLogin), else the one the release's pull Secret records
// from a run before, as k8sLogins took it for prefix (k8sPulls), and a read of a prefix whose
// logins were not taken is refused: none is made without the login it may hold.
func (in *Installer) heldLogin(prefix string) (*registryLogin, error) {
	if in.k8sRun {
		held, ok := in.k8sPulls[prefix]
		if !ok {
			return nil, fmt.Errorf("internal error: a Kubernetes run reads %s before it takes the pull Secret's login (k8sLogins)", prefix)
		}
		if in.k8sLogin != nil {
			return in.k8sLogin, nil
		}
		return held, nil
	}
	if in.noLoginFile || in.O.Dir == "" {
		return nil, nil
	}
	f := registryLoginFile(in.O.Dir)
	raw, err := os.ReadFile(f)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w (`%s` removes it, then `registry-login FILE` gives one)", err, in.loginCmd("--remove"))
	}
	l, err := parseRegistryLogin(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w (`%s` replaces it, `--remove` removes it)", f, err, in.loginCmd("FILE"))
	}
	return &l, nil
}

// loginCmd is the `registry-login` command line giving args to this install: as root on the VM,
// with the install's directory on Docker.
func (in *Installer) loginCmd(args string) string {
	if in.O.Preloaded {
		return fmt.Sprintf("sudo %s registry-login %s", in.R.Ctl(), args)
	}
	return fmt.Sprintf("%s registry-login --dir %s %s", in.R.Ctl(), shellQuote(in.O.Dir), args)
}

// heldLoginHost is the host of the login the install holds, empty for none; or why the file held
// cannot be read (its reads of that registry fail on it).
func (in *Installer) heldLoginHost() (host, problem string) {
	l, err := in.heldLogin("")
	if err != nil {
		return "", err.Error()
	}
	if l == nil {
		return "", ""
	}
	return l.Host, ""
}

// loginFor is the login the install's reads of the registry at prefix take: the one it holds,
// when it is for prefix's host; none otherwise. A login is never sent to another host.
func (in *Installer) loginFor(prefix string) (user, password string, err error) {
	l, err := in.heldLogin(prefix)
	if err != nil || l == nil || !strings.EqualFold(l.Host, registryHost(prefix)) {
		return "", "", err
	}
	return l.User, l.Password, nil
}

// installMirror is the mirror the install reads its releases from: the install's recorded
// registry; before the first install the one it is given (Docker's --registry, the VM's
// settings'), when it is not the release's own registry; empty for none.
func (in *Installer) installMirror() (string, error) {
	prefix := ""
	if env, err := ReadEnvFile(in.path(envName)); err == nil {
		prefix, _ = env.Get("STOWAGE_REGISTRY")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	} else if !in.O.Preloaded {
		prefix = strings.TrimSuffix(in.O.Registry, "/")
	} else {
		path := in.O.Settings
		if path == "" {
			path = SettingsFile(in.R.Spec.Name)
		}
		st, err := ReadSettings(path)
		if err != nil {
			return "", err
		}
		if st != nil {
			prefix = st.Registry
		}
	}
	if prefix == "" || prefix == in.R.Spec.Registry.Prefix {
		return "", nil
	}
	return prefix, nil
}

// checkLogin checks the login file raw for this install: one auths entry, for the host of a
// registry the install reads — the release's own, or its mirror.
func (in *Installer) checkLogin(raw []byte) (registryLogin, error) {
	if len(raw) > maxRegistryLogin {
		return registryLogin{}, fmt.Errorf("the login file is over %d KiB: one entry is a few hundred bytes", maxRegistryLogin>>10)
	}
	l, err := parseRegistryLogin(raw)
	if err != nil {
		return registryLogin{}, err
	}
	mirror, err := in.installMirror()
	if err != nil {
		return registryLogin{}, err
	}
	return l, loginReads(l.Host, in.R.Spec.Registry.Prefix, mirror, "this install")
}

// loginReads refuses a login for host unless host is the release's own registry's (own) or the
// mirror's (empty: none) that who reads.
func loginReads(host, own, mirror, who string) error {
	hosts := []string{registryHost(own)}
	reads := fmt.Sprintf("the release's own registry %s at %s", own, hosts[0])
	if mirror != "" {
		hosts = append(hosts, registryHost(mirror))
		reads += fmt.Sprintf(" and its mirror %s at %s", mirror, hosts[1])
	}
	if slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, host) }) {
		return nil
	}
	return fmt.Errorf("the login is for %s, but %s reads %s: give an auths entry for %s", host, who, reads, strings.Join(hosts, " or "))
}

// writeLogin records l as the install's login, replacing the one held: root's or the install
// owner's alone (0600).
func (in *Installer) writeLogin(l registryLogin) error {
	data, err := l.marshal()
	if err != nil {
		return err
	}
	return writeAtomic(registryLoginFile(in.O.Dir), bytes.NewReader(data), 0o600, nil)
}

// recordRegistryLogin checks the login file raw and records it, replacing the one held. It
// returns its host.
func (in *Installer) recordRegistryLogin(raw []byte) (string, error) {
	l, err := in.checkLogin(raw)
	if err != nil {
		return "", err
	}
	if err := in.writeLogin(l); err != nil {
		return "", err
	}
	return l.Host, nil
}

// givenLogin is the login Docker's --registry-login-file gave (O.RegistryLogin), checked for the
// install in O.Dir; nil for none.
func (in *Installer) givenLogin() (*registryLogin, error) {
	if in.O.RegistryLogin == nil {
		return nil, nil
	}
	l, err := in.checkLogin(in.O.RegistryLogin)
	if err != nil {
		return nil, fmt.Errorf("--registry-login-file: %w", err)
	}
	return &l, nil
}

// recordLogin records l as the install's login, before anything is pulled, and says where. undo,
// called when the run fails before the images are fetched with l (undoLoginOn), puts back the
// login held before (0600), or removes l when none was.
func (in *Installer) recordLogin(l registryLogin) (undo func(), err error) {
	file := registryLoginFile(in.O.Dir)
	prev, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("--registry-login-file: the login held: %w", err)
	}
	held := err == nil
	if err := in.writeLogin(l); err != nil {
		return nil, fmt.Errorf("--registry-login-file: %w", err)
	}
	in.O.logf("recorded the login to %s in %s (0600): the install's reads of %s take it", l.Host, file, l.Host)
	return func() {
		if !held {
			if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
				in.O.logf("the login this run recorded could not be removed: %v (`%s` removes it)", err, in.loginCmd("--remove"))
			}
			return
		}
		if err := writeAtomic(file, bytes.NewReader(prev), 0o600, nil); err != nil {
			in.O.logf("the login held before this run could not be restored: %v (`%s` gives it again)", err, in.loginCmd("FILE"))
			return
		}
		in.O.logf("the run failed: the login held before it is restored in %s", file)
	}, nil
}

// undoLoginOn is a run's end for the login it recorded (recordLogin): when the run failed, undo
// puts back the login held before; a nil undo is a login the run fetched the images with, which
// stays, said so on a failure.
func undoLoginOn(err error, undo func(), o *Options) {
	switch {
	case err == nil:
	case undo != nil:
		undo()
	default:
		o.logf("the run failed after the images were fetched with the login it recorded: the login stays")
	}
}

// loginDryRun says what a run would record of the login l (nil: none).
func (in *Installer) loginDryRun(l *registryLogin) {
	if l != nil {
		in.O.logf("a run would record the login to %s in %s (0600), replacing the one held", l.Host, registryLoginFile(in.O.Dir))
	}
}

// registryLoginCommand is `registry-login`: it records the login the install reads its registry
// with, or removes it. On the VM (euid set) it runs as root; on Docker (euid nil) as the install's
// owner, on an install that exists (a first install takes --registry-login-file).
func registryLoginCommand(in *Installer, o *Options, stdout io.Writer, euid func() int) *cobra.Command {
	var remove bool
	short := "Record the login this install reads its registry with — a Docker config holding one auths entry, for the host of the release's own registry or of its mirror — from FILE or standard input (-), replacing the one held; --remove removes it"
	owner := "the install owner's alone"
	if euid != nil {
		short += " (as root)"
		owner = "root's alone"
	}
	cmd := &cobra.Command{
		Use:   "registry-login [FILE]",
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.O = *o
			if euid != nil && euid() != 0 {
				return fmt.Errorf("the login is root's: `sudo %s registry-login`", in.R.Ctl())
			}
			if euid == nil {
				if err := in.loginInstalled(); err != nil {
					return err
				}
			}
			f := registryLoginFile(in.O.Dir)
			if remove {
				if len(args) > 0 {
					return refuse("leave out FILE, or --remove", "--remove removes the login held, FILE gives one: give one")
				}
				switch err := os.Remove(f); {
				case errors.Is(err, os.ErrNotExist):
					fmt.Fprintln(stdout, "This install holds no login to a registry: nothing to remove.")
					return nil
				case err != nil:
					return err
				}
				fmt.Fprintln(stdout, "Removed the login held: the reads of the release's own registry take the installer's pull account again, a mirror's go without one.")
				return nil
			}
			if len(args) == 0 {
				return refuse("give FILE, or - for standard input", "registry-login records a login file: give it")
			}
			raw, err := readFileOrStdin(args[0], cmd.InOrStdin())
			if err != nil {
				return err
			}
			host, err := in.recordRegistryLogin(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", loginSource(args[0]), err)
			}
			fmt.Fprintf(stdout, "Recorded the login to %s in %s (%s): this install's reads of %s use it.\n", host, f, owner, host)
			return nil
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "remove the login held")
	return cmd
}

// loginInstalled refuses `registry-login` on a Docker directory holding no install: the first
// install takes the login with --registry-login-file. O.Dir is made absolute.
func (in *Installer) loginInstalled() error {
	dir, err := filepath.Abs(in.O.Dir)
	if err != nil {
		return err
	}
	in.O.Dir = dir
	if _, err := os.Stat(in.path(envName)); errors.Is(err, os.ErrNotExist) {
		return refuse(fmt.Sprintf("give the first install its login: %s --dir %s --registry-login-file FILE", in.R.Ctl(), shellQuote(dir)), "%s holds no install: the first install takes its login with --registry-login-file", dir)
	} else if err != nil {
		return err
	}
	return nil
}

// loginSource names a login file argument in an error: - is standard input.
func loginSource(arg string) string {
	if arg == "-" {
		return "standard input"
	}
	return arg
}
