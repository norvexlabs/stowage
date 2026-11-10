package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/crypto/ssh"

	"github.com/norvexlabs/stowage/internal/kit"
)

// vmLink reaches a VM over SSH: as user with the key next to pub, at addr, checked strictly
// against the one host key the file known holds under alias — the one vm create recorded for that
// address in the VM's state directory, or the line --known-hosts-line gave. ssh refuses another
// key, and an address it holds none for.
type vmLink struct {
	ssh             Tool
	user, pub, addr string
	known, alias    string
	// line is the known_hosts line --known-hosts-line gave, as givenKnownHosts wrote it; empty
	// without one.
	line string
}

// VMReach names a VM and how SSH reaches it: vm create's defaults, the host name. KnownHostsLine,
// when set, is the VM's host key as a known_hosts line, checked in place of vm create's record.
type VMReach struct {
	Host                  string
	User, SSHKey, Address string
	KnownHostsLine        string
}

// link is the SSH link to the VM r names, refused before any connection when its state holds no
// host key: a VM whose install was finished by hand has none.
func (in *Installer) link(r VMReach) (*vmLink, error) {
	o := &in.O
	if err := checkHostname(r.Host); err != nil {
		return nil, err
	}
	dir, err := o.stateDir(in.R.Spec.Name, r.Host)
	if err != nil {
		return nil, err
	}
	if o.SSH == nil {
		return nil, errors.New("ssh is not available to this run")
	}
	l := &vmLink{ssh: o.SSH, user: cmp.Or(r.User, defaultUser(o.getenv)), pub: cmp.Or(r.SSHKey, defaultKey(o.getenv("HOME"))), addr: cmp.Or(r.Address, r.Host), known: filepath.Join(dir, "known_hosts")}
	l.alias = hostAlias(l.addr)
	if err := checkUser(l.user); err != nil {
		return nil, fmt.Errorf("--user %s: %w", l.user, err)
	}
	if strings.HasPrefix(l.addr, "-") || strings.ContainsAny(l.addr, " \t\r\n%@\"'\\") {
		return nil, fmt.Errorf("--host %q: a host name or an address", l.addr)
	}
	if r.KnownHostsLine != "" {
		if l.known, l.alias, l.line, err = o.givenKnownHosts(in.R.Spec.Name, r.KnownHostsLine); err != nil {
			return nil, err
		}
		return l, nil
	}
	st, err := os.Stat(l.known)
	if err == nil && st.Size() == 0 {
		err = errors.New("it is empty")
	}
	if err != nil {
		return nil, fmt.Errorf("%s: the VM's host key is not known (vm create records it once the VM answers, for the address it waited at: --host gives that one; a VM whose install was finished by hand: put its host key, checked on its console, in %s; or give it with --known-hosts-line): %w", r.Host, l.known, err)
	}
	return l, nil
}

// knownHostName is the host field --known-hosts-line takes: one plain name or address, never a
// pattern, a hashed name, a list or a [host]:port.
var knownHostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)

// givenKnownHosts checks line, the one known_hosts line --known-hosts-line gives — a host name and
// a public key, no marker — and writes it, the name lower-cased as ssh matches it, into a file of
// its own: $XDG_STATE_HOME/<name>/known-hosts/<its digest>, written whole, so a run giving the
// same line meets the same file. alias is the line's host name, which ssh looks the key up by;
// written the line written, without its line end.
func (o *Options) givenKnownHosts(name, line string) (file, alias, written string, err error) {
	// One line end is taken off, a Windows one (CRLF) too.
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if strings.ContainsAny(line, "\r\n") {
		return "", "", "", errors.New("--known-hosts-line: one line")
	}
	marker, hosts, key, _, _, err := ssh.ParseKnownHosts([]byte(line))
	switch {
	case err != nil:
		return "", "", "", fmt.Errorf("--known-hosts-line: not a known_hosts line (HOST KEYTYPE BASE64): %w", err)
	case marker != "":
		return "", "", "", fmt.Errorf("--known-hosts-line: a host key, not a @%s line", marker)
	case len(hosts) != 1 || !knownHostName.MatchString(hosts[0]):
		return "", "", "", fmt.Errorf("--known-hosts-line: its host field %q is not one host name or address (no pattern, list, hashed name or [host]:port)", strings.Join(hosts, ","))
	}
	alias = strings.ToLower(hosts[0])
	text := alias + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n"
	base, err := o.xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state"), "the VM's host key")
	if err != nil {
		return "", "", "", err
	}
	dir := filepath.Join(base, name, "known-hosts")
	if err := sshPathOK(dir); err != nil {
		return "", "", "", fmt.Errorf("%w (set XDG_STATE_HOME elsewhere)", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", "", err
	}
	sum := sha256.Sum256([]byte(text))
	file = filepath.Join(dir, hex.EncodeToString(sum[:16]))
	if err := writeAtomic(file, strings.NewReader(text), 0o600, nil); err != nil {
		return "", "", "", err
	}
	return file, alias, strings.TrimSuffix(text, "\n"), nil
}

// args are ssh's arguments running command on the VM; ssh joins command's words for the VM's
// shell, so each word is a constant or quoted.
func (l *vmLink) args(command ...string) []string {
	return sshArgsChecking("yes", l.alias, l.known, l.user, l.addr, l.pub, command...)
}

// run runs command on the VM, its output returned.
func (l *vmLink) run(ctx context.Context, command ...string) ([]byte, error) {
	return l.ssh.Run(ctx, Cmd{Args: l.args(command...)})
}

// try runs `sudo -n <ctl> --help` on the VM: the link as strict as a run's, and the very sudo
// each run's command takes (a sudoers rule naming the ctl alone included), before anything is
// asked.
func (l *vmLink) try(ctx context.Context, ctl string) error {
	if _, err := l.run(ctx, "sudo", "-n", shellQuote(ctl), "--help"); err != nil {
		return fmt.Errorf("%s does not answer over SSH, or does not run sudo without a password: %w (--host, --user and --ssh-key reach it as vm create did; its key is the one in %s; or its sudoers does not let the user run `%s` without a password)", l.at(), err, l.known, ctl)
	}
	return nil
}

// command is the ssh command line running command over this link, as a shell takes it: what a
// message tells the operator to run.
func (l *vmLink) command(command ...string) string { return sshCommandLine(l.args(command...)) }

// at names the VM as an ssh destination, in messages.
func (l *vmLink) at() string { return l.user + "@" + l.addr }

// reachFlags are --user, --ssh-key, --host and --known-hosts-line: how SSH reaches a VM, as every
// verb that does takes them.
func reachFlags(f *pflag.FlagSet, user, key, host, known *string) {
	f.StringVar(user, "user", "", "the VM's administrator, who logs in over SSH (default: your user name, else sysadmin)")
	f.StringVar(key, "ssh-key", "", "the administrator's SSH public key file (default: ~/.ssh/id_ed25519.pub, id_ecdsa.pub or id_rsa.pub)")
	f.StringVar(host, "host", "", "the name or address the VM answers at (default: HOSTNAME)")
	f.StringVar(known, "known-hosts-line", "", "the VM's SSH host key as one known_hosts line, HOST KEYTYPE BASE64 (`vm seed --json`'s known_hosts_line): that key alone is accepted, in place of the one vm create recorded")
}

// exitCoder is an error carrying the exit status of the program that failed (exec's ExitError).
type exitCoder interface{ ExitCode() int }

// sshKitVerbs run a handoff on the VM reach names, over SSH: create through the VM's
// `kit create --json`, the request on standard input (its answers never on a command line, which
// the VM's process list and journal show), the kit coming back to this machine alone; check
// through the VM's `kit check`, its status read as it exits there.
func (in *Installer) sshKitVerbs(reach *VMReach, stderr io.Writer, prepare func() error) kitVerbs {
	ctl := in.R.Ctl()
	// One link a run, made at its first use: prepare runs once.
	var link *vmLink
	connect := func() (*vmLink, error) {
		if link != nil {
			return link, nil
		}
		if err := prepare(); err != nil {
			return nil, err
		}
		l, err := in.link(*reach)
		link = l
		return l, err
	}
	return kitVerbs{
		host: &reach.Host,
		flags: func(c *cobra.Command) {
			reachFlags(c.Flags(), &reach.User, &reach.SSHKey, &reach.Address, &reach.KnownHostsLine)
		},
		// The link, tried before anything is asked: ssh itself, as strict as the run's, so a VM it
		// cannot reach, or whose recorded key it refuses, costs no answer.
		precheck: func(cmd *cobra.Command) error {
			l, err := connect()
			if err == nil {
				err = l.try(cmd.Context(), ctl)
			}
			return err
		},
		create: func(cmd *cobra.Command, req KitRequest) (*IssuedKit, error) {
			l, err := connect()
			if err != nil {
				return nil, err
			}
			body, err := json.Marshal(req)
			if err != nil {
				return nil, err
			}
			var out bytes.Buffer
			in.O.logf("running the handoff %s on %s", req.Handoff, l.at())
			_, err = l.ssh.Run(cmd.Context(), Cmd{Args: l.args("sudo", shellQuote(ctl), "kit", "create", shellQuote(req.Handoff), "--json"), Stdin: bytes.NewReader(body), Stdout: &limitedWriter{w: &out, n: maxKitAnswer + 1}, Stream: stderr})
			if err != nil {
				return nil, fmt.Errorf("%s: %s kit create: %w; if its run went that far, what it issued is lost: the project's script decides whether it can be issued again", l.at(), ctl, err)
			}
			// Not strict: the run already happened, and a newer VM's answer may carry a field this
			// release does not know, which must not cost the kit.
			var issued IssuedKit
			if out.Len() > maxKitAnswer {
				err = fmt.Errorf("more than %d bytes", maxKitAnswer)
			}
			dec := json.NewDecoder(&out)
			if err == nil {
				err = dec.Decode(&issued)
			}
			if err != nil || issued.Kit == "" || issued.Code == "" {
				if err == nil {
					err = errors.New("no kit or no code")
				}
				return nil, fmt.Errorf("%s: what %s kit create --json answered does not read (%v); what the handoff issued is lost: the project's script decides whether it can be issued again", l.at(), ctl, err)
			}
			return &issued, nil
		},
		check: func(cmd *cobra.Command, handoff, instance string) (int, error) {
			l, err := connect()
			if err != nil {
				return 0, err
			}
			// Its verdict line is this run's to print: the VM's stdout is read, not shown, its stderr
			// shown. A status the VM's check gives counts only with the verdict it prints: sudo or
			// ssh failing exit 1 or 255 too, and that is no answer, never not proven.
			var out bytes.Buffer
			_, err = l.ssh.Run(cmd.Context(), Cmd{Args: l.args("sudo", shellQuote(ctl), "kit", "check", shellQuote(handoff), shellQuote(instance)), Stdout: &limitedWriter{w: &out, n: maxKitVerdict}, Stream: stderr})
			said := func(verdict string) bool {
				return slices.Contains(strings.Split(out.String(), "\n"), handoff+" "+instance+": "+verdict)
			}
			var ec exitCoder
			switch {
			case err == nil && said("proven"):
				return KitProven, nil
			case errors.As(err, &ec) && ec.ExitCode() == 1:
				if code, ok := notProven(out.String(), handoff, instance); ok {
					return code, nil
				}
			case errors.As(err, &ec) && ec.ExitCode() == KitUnknown && said("unknown"):
				return KitUnknown, nil
			case err == nil:
				err = errors.New("it exited 0 without saying proven")
			}
			return 0, fmt.Errorf("%s: %s kit check: %w", l.at(), ctl, err)
		},
	}
}

// notProven reads the VM's `kit check` verdict of not proven for handoff and instance in out: the
// status its check exited with, as it says it (1 when it names none it reads). ok is false
// without that verdict.
func notProven(out, handoff, instance string) (int, bool) {
	prefix := handoff + " " + instance + ": not proven"
	for l := range strings.Lines(out) {
		rest, found := strings.CutPrefix(strings.TrimSuffix(l, "\n"), prefix)
		if !found {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rest, " (its check, or starting it, exited "), ")"))
		if err != nil || n <= 0 || n == KitUnknown {
			return 1, true
		}
		return n, true
	}
	return 0, false
}

// maxKitVerdict bounds what `vm kit check` reads of the VM's `kit check`: its verdict line.
const maxKitVerdict = 64 << 10

// limitedWriter keeps the first n bytes written to w and drops the rest, never failing a write.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if k := min(len(p), l.n); k > 0 {
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// maxKitAnswer bounds what `kit create --json` answers: the kit (kit.MaxSize), its code and the
// run's log (cut at maxKitLog), each byte JSON-escaped at worst to six (\u00XX, \ufffd).
const maxKitAnswer = 6*(kit.MaxSize+maxKitLog) + 64<<10

// vmKitCommand is `vm kit`, for a project that declares a handoff: create and check run on the VM
// over SSH, take gives the VM's install a kit opened here.
// o is the command's own Options, whose writers are set while the tree is built (in.O is set from
// it by prepare, at a run).
func (in *Installer) vmKitCommand(o *Options, prepare func() error, terminal func(bool) Prompter) *cobra.Command {
	reach := &VMReach{}
	k := kitCommand(in.R, o.Out, o.Log, terminal, in.sshKitVerbs(reach, o.Log, prepare))
	if k == nil {
		return nil
	}
	k.Short = "Issue a handoff kit on a VM `vm create` made, over SSH, or check one issued there, or give its install a kit"
	var take VMKitTake
	var noInput, accessible bool
	c := &cobra.Command{
		Use:   "take HOSTNAME FILE|-",
		Short: "Open a kit here, with its code, and give what it holds to the VM's install over SSH (`install --given -` there)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := prepare(); err != nil {
				return err
			}
			take.Host, take.Kit = args[0], args[1]
			var prompt Prompter
			if !noInput {
				prompt = terminal(accessible)
			}
			return in.TakeKitVM(cmd.Context(), &take, cmd.InOrStdin(), prompt, time.Now())
		},
	}
	f := c.Flags()
	f.Var(pathFlag{&take.CodeFile}, "kit-code-file", "the file holding the kit's code (default: "+KitCodeVar+", else asked on a terminal)")
	reachFlags(f, &take.User, &take.SSHKey, &take.Address, &take.KnownHostsLine)
	f.BoolVar(&noInput, "no-input", false, "never ask: the code comes from --kit-code-file or "+KitCodeVar)
	f.BoolVar(&accessible, "accessible", false, "ask with plain line prompts (screen readers); also when ACCESSIBLE is set or TERM=dumb")
	k.AddCommand(c)
	return k
}

// VMKitTake is `vm kit take`: the kit file (or - for standard input) and its code's file.
type VMKitTake struct {
	VMReach
	Kit, CodeFile string
}

// TakeKitVM opens a kit on this machine and gives what it holds to the VM's install over SSH, on
// standard input, as `vm create --kit` gives a first install's: the code never leaves this
// machine, and the VM's install checks each input as `install --given` does (an input of another
// shape left out and said there, one it does not declare refused).
func (in *Installer) TakeKitVM(ctx context.Context, t *VMKitTake, stdin io.Reader, prompt Prompter, now time.Time) error {
	o := &in.O
	l, err := in.link(t.VMReach)
	if err == nil {
		err = l.try(ctx, in.R.Ctl()) // before the code is asked
	}
	if err != nil {
		return err
	}
	g, err := in.openKit(ctx, t.Kit, t.CodeFile, stdin, prompt, now)
	if err != nil {
		return err
	}
	raw, err := marshalGiven(g)
	if err != nil {
		return err
	}
	ctl := in.R.Ctl()
	o.logf("installing on %s over SSH with %s", l.at(), g.names())
	if _, err := l.ssh.Run(ctx, Cmd{Args: l.args("sudo", shellQuote(ctl), "install", "--given", "-", "--no-input"), Stdin: bytes.NewReader(raw), Stream: o.Log}); err != nil {
		return fmt.Errorf("%s: the install over SSH: %w; `%s` tells where it stopped, and the same `vm kit take` command, with the same flags, gives the kit again", l.at(), err, l.command("sudo", shellQuote(ctl), "status"))
	}
	return nil
}
