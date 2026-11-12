package install

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// The VM's login to its mirror (`registry-login`): a Docker config holding one `auths` entry, for
// the mirror's host alone, given over SSH after the boot and never in the cloud-config. The VM's
// own reads of that host (its updates, an upgrade's OS bundle, a first boot's switch to another
// release's OS) use it; the release's pull account is never sent there, nor the login elsewhere.

// maxRegistryLogin bounds a login file, as readFileOrStdin reads it: one entry is a few hundred
// bytes.
const maxRegistryLogin = maxRcloneConfig

// registryLoginFile records, on the persistent partition beside the install directory, the VM's
// login to its mirror, root's alone (0600). Beside the directory, not in it: a login given before
// the first install is kept by it, and upgrades and rollbacks, which move the install's state,
// never touch it.
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

// heldLogin is the login the VM holds for its mirror; nil when it holds none. Off the VM
// (O.Preloaded unset) there is none.
func (in *Installer) heldLogin() (*registryLogin, error) {
	if !in.O.Preloaded || in.O.Dir == "" {
		return nil, nil
	}
	f := registryLoginFile(in.O.Dir)
	raw, err := os.ReadFile(f)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w (`sudo %s registry-login --remove` removes it, then `registry-login FILE` gives one)", err, in.R.Ctl())
	}
	l, err := parseRegistryLogin(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w (`sudo %s registry-login FILE` replaces it, `--remove` removes it)", f, err, in.R.Ctl())
	}
	return &l, nil
}

// heldLoginHost is the host of the login the VM holds, empty for none; or why the file held
// cannot be read (its reads of the mirror fail on it).
func (in *Installer) heldLoginHost() (host, problem string) {
	l, err := in.heldLogin()
	if err != nil {
		return "", err.Error()
	}
	if l == nil {
		return "", ""
	}
	return l.Host, ""
}

// mirrorLogin is the login the VM's reads of the mirror at prefix take: the one it holds, when it
// is for prefix's host; none otherwise.
func (in *Installer) mirrorLogin(prefix string) (user, password string, err error) {
	l, err := in.heldLogin()
	if err != nil || l == nil || !strings.EqualFold(l.Host, registryHost(prefix)) {
		return "", "", err
	}
	return l.User, l.Password, nil
}

// vmMirror is the mirror the VM reads its releases from: the install's recorded registry, before
// the first install the settings', when it is not the release's own registry; empty for none.
func (in *Installer) vmMirror() (string, error) {
	prefix := ""
	if env, err := ReadEnvFile(in.path(envName)); err == nil {
		prefix, _ = env.Get("STOWAGE_REGISTRY")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
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

// recordRegistryLogin checks the login file raw and records it, replacing the one held: its host
// must be the VM's mirror's. It returns that host.
func (in *Installer) recordRegistryLogin(raw []byte) (string, error) {
	if len(raw) > maxRegistryLogin {
		return "", fmt.Errorf("the login file is over %d KiB: one entry is a few hundred bytes", maxRegistryLogin>>10)
	}
	l, err := parseRegistryLogin(raw)
	if err != nil {
		return "", err
	}
	mirror, err := in.vmMirror()
	if err != nil {
		return "", err
	}
	if mirror == "" {
		return "", fmt.Errorf("this VM reads %s, the release's own registry, with its pull account: a login is for a mirror (`vm create --registry`), and none is used here", in.R.Spec.Registry.Prefix)
	}
	if h := registryHost(mirror); !strings.EqualFold(l.Host, h) {
		return "", fmt.Errorf("the login is for %s, but this VM reads its mirror %s at %s: give an auths entry for %s", l.Host, mirror, h, h)
	}
	data, err := l.marshal()
	if err != nil {
		return "", err
	}
	if err := writeAtomic(registryLoginFile(in.O.Dir), bytes.NewReader(data), 0o600, nil); err != nil {
		return "", err
	}
	return l.Host, nil
}

// registryLoginCommand is `registry-login`, run as root on the VM: it records the VM's login to
// its mirror, or removes it.
func registryLoginCommand(in *Installer, o *Options, stdout io.Writer, euid func() int) *cobra.Command {
	var remove bool
	cmd := &cobra.Command{
		Use:   "registry-login [FILE]",
		Short: "Record the login this VM reads its mirror with — a Docker config holding one auths entry, for the mirror's host — from FILE or standard input (-), replacing the one held; --remove removes it (as root)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.O = *o
			if euid() != 0 {
				return fmt.Errorf("the login is root's: `sudo %s registry-login`", in.R.Ctl())
			}
			f := registryLoginFile(in.O.Dir)
			if remove {
				if len(args) > 0 {
					return refuse("leave out FILE, or --remove", "--remove removes the login held, FILE gives one: give one")
				}
				switch err := os.Remove(f); {
				case errors.Is(err, os.ErrNotExist):
					fmt.Fprintln(stdout, "This VM holds no login to its mirror: nothing to remove.")
					return nil
				case err != nil:
					return err
				}
				fmt.Fprintln(stdout, "Removed the login to the mirror: its reads go without one.")
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
			fmt.Fprintf(stdout, "Recorded the login to %s in %s (root's alone): this VM's reads of its mirror use it.\n", host, f)
			return nil
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "remove the login held")
	return cmd
}

// loginSource names a login file argument in an error: - is standard input.
func loginSource(arg string) string {
	if arg == "-" {
		return "standard input"
	}
	return arg
}
