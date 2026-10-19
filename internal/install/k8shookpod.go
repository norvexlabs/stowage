package install

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/providers"
)

// The provision Job's pod runs four steps of this installer (the `__k8s-hook` command), each in
// its own container: `copy` (first) puts the installer on the pod's shared volume; `check` (then)
// proves the way to the outputs Secret with an empty patch; then, side by side, `run`, in the hook's own image, runs the hook with its stdout to a file on that volume
// (never the pod's log, which a cluster's collectors ship), and `record` follows that file and
// writes the KEY=VALUE lines into the release's outputs Secret as they arrive, through a
// ServiceAccount that may only patch that one Secret. The hook's container never holds that
// account's token.
const (
	k8sHookCommand = "__k8s-hook"
	hookOutFile    = "stdout"      // the hook's stdout, on the shared volume
	hookStatusFile = "status.json" // how the hook ended (hookStatus)
)

// hookStatus is how the hook ended, as `run` records it for `record`.
type hookStatus struct {
	Exit    int    `json:"exit"`              // its exit status; -1 when it could not start
	Stopped string `json:"stopped,omitempty"` // why `run` stopped it, when it did
}

// k8sHookPodCommand is the hidden `__k8s-hook` command the provision Job's containers run.
func k8sHookPodCommand(stderr io.Writer) *cobra.Command {
	c := &cobra.Command{Use: k8sHookCommand, Hidden: true, Args: cobra.NoArgs}
	c.AddCommand(&cobra.Command{
		Use:  "copy DEST",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			return copyFile(exe, args[0], 0o755)
		},
	})
	var dir string
	var timeout time.Duration
	run := &cobra.Command{
		Use:  "run -- CMD…",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return hookRun(cmd.Context(), dir, timeout, args, stderr)
		},
	}
	run.Flags().StringVar(&dir, "dir", "", "the shared volume")
	run.Flags().DurationVar(&timeout, "timeout", provisionTimeout, "how long the hook may run")
	c.AddCommand(run)
	var r hookRecorder
	var reserved string
	rec := &cobra.Command{
		Use:  "record",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r.reserved = map[string]bool{}
			for _, k := range strings.Split(reserved, ",") {
				if k != "" {
					r.reserved[k] = true
				}
			}
			r.log = stderr
			var err error
			if r.check {
				if err = r.patch(cmd.Context(), map[string]string{}); err != nil {
					err = fmt.Errorf("the outputs Secret %s cannot be written, so the hook does not run: %w", r.secret, err)
				}
			} else {
				err = r.record(cmd.Context())
			}
			if err != nil {
				// The pod's status carries it to the installer; it names keys, never a value.
				_ = os.WriteFile("/dev/termination-log", []byte(err.Error()), 0o644)
			}
			return err
		},
	}
	f := rec.Flags()
	f.BoolVar(&r.check, "check", false, "only check that the outputs Secret can be patched (an empty patch), before the hook starts")
	f.StringVar(&r.dir, "dir", "", "the shared volume")
	f.StringVar(&r.prev, "prev", "", "the outputs Secret's keys as files: what the hook returned before")
	f.StringVar(&r.token, "token", "", "the directory holding the ServiceAccount's token and the cluster's CA")
	f.StringVar(&r.namespace, "namespace", "", "the release's namespace")
	f.StringVar(&r.secret, "secret", "", "the outputs Secret")
	f.StringVar(&reserved, "reserved", "", "the keys the install records, comma-separated: never a hook's")
	c.AddCommand(rec)
	return c
}

// hookRun runs the hook argv with its stdout to dir's hookOutFile (at most maxProvisionOut bytes and
// one more, which tells `record` it wrote too much; past that it is stopped), its stderr to the
// pod's log, for at most timeout, then records how it ended in hookStatusFile. It fails only when
// it cannot record that: `record` reports the hook's own failure, after keeping what it returned.
func hookRun(ctx context.Context, dir string, timeout time.Duration, argv []string, stderr io.Writer) error {
	out, err := os.OpenFile(filepath.Join(dir, hookOutFile), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	st := hookStatus{Exit: -1}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	capped := &capWriter{w: out, left: int64(maxProvisionOut) + 1, over: cancel}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		st.Stopped = fmt.Sprintf("%s: %v", argv[0], err)
	} else {
		c := exec.CommandContext(runCtx, path, argv[1:]...)
		c.Args[0] = argv[0]
		c.Stdout, c.Stderr, c.Env = capped, stderr, os.Environ()
		c.Cancel = func() error { return c.Process.Signal(syscall.SIGTERM) }
		c.WaitDelay = 10 * time.Second
		err := c.Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			st.Exit = 0
		case errors.As(err, &exit):
			st.Exit = exit.ExitCode()
		default:
			st.Stopped = err.Error()
		}
		switch {
		case capped.exceeded:
			st.Stopped = fmt.Sprintf("it wrote more than %d bytes on stdout", maxProvisionOut)
		case capped.long:
			st.Stopped = fmt.Sprintf("it wrote a stdout line longer than %d bytes", maxProvisionLine)
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			st.Stopped = fmt.Sprintf("stopped after %s", timeout)
		case ctx.Err() != nil:
			st.Stopped = "stopped: the pod is ending"
		}
	}
	raw, _ := json.Marshal(st)
	return errors.Join(out.Close(), writeAtomic(filepath.Join(dir, hookStatusFile), bytes.NewReader(raw), 0o644, nil))
}

// capWriter passes at most left bytes to w, then calls over once and drops the rest.
// A line longer than maxProvisionLine calls over too: past either bound the hook is stopped, as
// on Docker, since nothing it prints after is recorded.
type capWriter struct {
	w        io.Writer
	left     int64
	over     func()
	exceeded bool // past left
	long     bool // a line past maxProvisionLine
	line     int  // bytes of the current line, but for the '\r's it ends with so far
	cr       int  // those '\r's
}

func (c *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	for _, b := range p {
		switch b {
		case '\n':
			c.line, c.cr = 0, 0 // a '\r' right before it is trimmed by the parse: never counted
		case '\r':
			c.cr++
		default:
			c.line += c.cr + 1
			c.cr = 0
		}
		if (c.line > maxProvisionLine || c.line+c.cr > maxProvisionLine+1) && !c.long {
			c.long = true
			c.over()
		}
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
		if !c.exceeded {
			c.exceeded = true
			c.over()
		}
	}
	c.left -= int64(len(p))
	if _, err := c.w.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}

// hookRecorder is `record`: what the hook returned, added to what it returned before, into the
// outputs Secret.
type hookRecorder struct {
	dir, prev, token, namespace, secret string
	check                               bool // only the empty patch
	reserved                            map[string]bool
	log                                 io.Writer
	client                              *http.Client // nil: one trusting the cluster's CA
	api                                 string       // the API server's base URL; empty: the in-cluster one
}

// hookFollow is how often `record` reads what the hook has printed since.
var hookFollow = time.Second

// patchBackoff is the wait between two tries of one patch the API server could not answer.
var patchBackoff = 2 * time.Second

// hookFinalFlush is how long `record` keeps trying to record the hook's last values once it ended.
var hookFinalFlush = 2 * time.Minute

// record follows the hook's stdout as the Docker shape reads it while the hook runs (outputWriter:
// the same grammar, bounds and refusals) and patches the outputs Secret with each value that
// changed, as it arrives: a value the hook printed survives a pod killed later (an eviction, the
// Job's deadline, the installer's interrupt), as on Docker. It ends once the hook has (its status
// written), or when the pod ends, after a last read; it fails when the hook did, when a line was
// refused, or when what it read could not be recorded. A last line without a line break counts only
// from a hook that exited 0.
func (r *hookRecorder) record(ctx context.Context) error {
	outs, err := readPrev(r.prev)
	if err != nil {
		return err
	}
	saved := map[string]string{} // what the Secret holds
	for _, k := range outs.Keys() {
		saved[k], _ = outs.Get(k)
	}
	w := &outputWriter{outs: outs, reserved: r.reserved, save: func() error { return nil }}
	var off int64
	var readErr error
	read := func() {
		if w.refused != nil {
			return // past a bound: nothing more is recorded
		}
		f, err := os.Open(filepath.Join(r.dir, hookOutFile))
		if errors.Is(err, os.ErrNotExist) {
			return // the hook has not started
		}
		if err != nil {
			readErr = err
			return
		}
		defer f.Close()
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			readErr = err
			return
		}
		n, err := io.Copy(w, io.LimitReader(f, int64(maxProvisionOut)+1-off))
		off += n
		if err != nil && w.refused == nil {
			readErr = err
		}
	}
	var patchErr error
	flush := func(ctx context.Context) {
		changed := map[string]string{}
		for _, k := range outs.Keys() {
			if v, _ := outs.Get(k); saved[k] != v {
				changed[k] = v
			}
		}
		if len(changed) == 0 {
			return
		}
		if patchErr = r.patch(ctx, changed); patchErr != nil {
			return // tried again at the next read
		}
		maps.Copy(saved, changed)
		fmt.Fprintf(r.log, "recorded %s in the Secret %s\n", strings.Join(sortedKeys(changed), ", "), r.secret)
	}
	var st *hookStatus
	for st == nil {
		// The status first: once it is there, the read after it holds everything the hook wrote.
		var err error
		if st, err = readStatus(r.dir); err != nil {
			return err
		}
		read()
		if st == nil {
			flush(ctx)
			select {
			case <-ctx.Done():
				// The pod ends: a last read, and what it holds is recorded, within its grace period.
				read()
				lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
				flush(lctx)
				cancel()
				return errors.Join(errors.New("the hook did not finish: the pod was stopped"), w.close(false), readErr, wrapPatch(r.secret, patchErr))
			case <-time.After(hookFollow):
			}
		}
	}
	errs := []error{w.close(st.Exit == 0 && st.Stopped == ""), readErr}
	// The last values are the ones the hook acted on last: tried again for a while (an API server
	// that is away a moment), within the Job's deadline, or until the pod is stopped.
	flush(ctx)
	var final refusedPatch
	for end := time.Now().Add(hookFinalFlush); patchErr != nil && !errors.As(patchErr, &final) && time.Now().Before(end); {
		select {
		case <-ctx.Done():
			// The pod ends: one last try within its grace period, as before the hook ended.
			lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			flush(lctx)
			cancel()
			end = time.Time{}
		case <-time.After(hookFollow):
			flush(ctx)
		}
	}
	errs = append(errs, wrapPatch(r.secret, patchErr))
	switch {
	case st.Stopped != "":
		errs = append([]error{fmt.Errorf("the hook did not finish: %s", st.Stopped)}, errs...)
	case st.Exit != 0:
		errs = append([]error{fmt.Errorf("the hook exited %d", st.Exit)}, errs...)
	}
	return errors.Join(errs...)
}

// refusedPatch is an answer of the API server that trying again cannot change (a 4xx).
type refusedPatch struct{ error }

func (e refusedPatch) Unwrap() error { return e.error }

func wrapPatch(secret string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("the Secret %s: %w", secret, err)
}

// readStatus is how the hook ended, nil while it runs.
func readStatus(dir string) (*hookStatus, error) {
	raw, err := os.ReadFile(filepath.Join(dir, hookStatusFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st hookStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("the hook's status: %w", err)
	}
	return &st, nil
}

// readPrev reads what the hook returned before: the outputs Secret mounted as a directory, one file
// per key (none when the Secret holds none).
func readPrev(dir string) (*EnvFile, error) {
	outs := NewEnvFile()
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || dir == "" {
		return outs, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range ents {
		// A Secret volume holds its keys as links into a hidden directory it swaps.
		if providers.EnvName.MatchString(e.Name()) {
			keys = append(keys, e.Name())
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, err := os.ReadFile(filepath.Join(dir, k))
		if err != nil {
			return nil, err
		}
		outs.Set(k, string(v))
	}
	return outs, nil
}

// patch merges data into the outputs Secret through the API server, as the pod's ServiceAccount.
func (r *hookRecorder) patch(ctx context.Context, data map[string]string) error {
	enc := map[string]string{}
	for k, v := range data {
		enc[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	body, err := json.Marshal(map[string]any{"data": enc})
	if err != nil {
		return err
	}
	c, base, err := r.apiClient()
	if err != nil {
		return err
	}
	token, err := os.ReadFile(filepath.Join(r.token, "token"))
	if err != nil {
		return err
	}
	u := base + "/api/v1/namespaces/" + url.PathEscape(r.namespace) + "/secrets/" + url.PathEscape(r.secret) + "?fieldManager=" + managedBy
	var last error
	for try := 0; try < 3; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return errors.Join(last, ctx.Err())
			case <-time.After(patchBackoff):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/merge-patch+json")
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		resp, err := c.Do(req)
		if err != nil {
			last = err
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			return nil
		}
		// The API server's Status names the object and the verb, never the data sent.
		var s struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(msg, &s)
		last = fmt.Errorf("the API server answered %s: %s", resp.Status, s.Message)
		if resp.StatusCode < 500 {
			return refusedPatch{last} // the same patch again would be refused again
		}
	}
	return last
}

// apiClient is the client and base URL of the API server: the in-cluster one, trusting the
// cluster's CA, unless the recorder was given its own.
func (r *hookRecorder) apiClient() (*http.Client, string, error) {
	if r.client != nil {
		return r.client, r.api, nil
	}
	ca, err := os.ReadFile(filepath.Join(r.token, "ca.crt"))
	if err != nil {
		return nil, "", err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, "", errors.New("the cluster's CA holds no certificate")
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		host, port = "kubernetes.default.svc", "443"
	}
	c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
	return c, "https://" + net.JoinHostPort(host, port), nil
}
