// Package truenas drives what OpenTofu's TrueNAS provider cannot declare about a VM — the bytes
// of its disk and seed, its power before its devices are deleted — through TrueNAS's own
// interfaces: the JSON-RPC 2.0 API over a WebSocket (/api/current) and the /_upload/ endpoint
// that feeds filesystem.put.
package truenas

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// The environment the client reads: the API's address — TRUENAS_ENDPOINT, the WebSocket URL the
// truenas/truenas provider reads, or TRUENAS_URL, the host's HTTPS root — its key, and how the
// host's certificate (a TrueNAS host's own, often self-signed) is checked: against the CA in the
// PEM file TRUENAS_CA_CERT, or not at all (TRUENAS_INSECURE_SKIP_VERIFY).
const (
	EndpointVar = "TRUENAS_ENDPOINT"
	URLVar      = "TRUENAS_URL"
	KeyVar      = "TRUENAS_API_KEY"
	InsecureVar = "TRUENAS_INSECURE_SKIP_VERIFY"
	CACertVar   = "TRUENAS_CA_CERT"
)

// poll is how often a job's state is read.
var poll = time.Second

// Client is a logged-in connection to a TrueNAS host's API.
type Client struct {
	conn *websocket.Conn
	base *url.URL
	key  string
	http *http.Client
	id   int64
}

// Insecure reports whether v, TRUENAS_INSECURE_SKIP_VERIFY's value, turns the certificate check
// off: 1, true or yes, as the provider reads it.
func Insecure(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// FromEnv dials the host getenv names (EndpointVar or URLVar, KeyVar, InsecureVar or CACertVar).
func FromEnv(ctx context.Context, getenv func(string) string) (*Client, error) {
	base, err := BaseFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	if getenv(KeyVar) == "" {
		return nil, fmt.Errorf("%s is not set", KeyVar)
	}
	tc, err := TLSFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	return Dial(ctx, base.String(), getenv(KeyVar), tc)
}

// BaseFromEnv is the API's HTTPS root from EndpointVar or URLVar: either, or both naming the same
// host and port.
func BaseFromEnv(getenv func(string) string) (*url.URL, error) {
	ep, root := getenv(EndpointVar), getenv(URLVar)
	var fromEP, fromURL *url.URL
	var err error
	if ep != "" {
		if fromEP, err = ParseEndpoint(ep); err != nil {
			return nil, err
		}
	}
	if root != "" {
		if fromURL, err = ParseURL(root); err != nil {
			return nil, err
		}
	}
	switch {
	case fromEP == nil && fromURL == nil:
		return nil, fmt.Errorf("%s (wss://<host>[:<port>]/api/current) or %s (https://<host>[:<port>]) is not set", EndpointVar, URLVar)
	case fromEP == nil:
		return fromURL, nil
	case fromURL != nil && hostPort(fromEP) != hostPort(fromURL):
		return nil, fmt.Errorf("%s names %s and %s %s: set one, or both to the same host", EndpointVar, fromEP.Host, URLVar, fromURL.Host)
	}
	return fromEP, nil
}

// hostPort is u's host, lower-cased, and its port, the scheme's default when none is given (443
// for https and wss, 80 for http and ws): the same host however each variable writes it.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "wss": "443", "http": "80", "ws": "80"}[u.Scheme]
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// ParseEndpoint reads EndpointVar's WebSocket URL — wss://<host>[:<port>]/api/current, or the
// legacy /websocket path, rewritten to /api/current as the truenas/truenas provider does — as the
// API's HTTPS root (the client dials /api/current). wss only: the API key travels in every login
// and upload, and TrueNAS revokes a key used over plain HTTP.
func ParseEndpoint(ep string) (*url.URL, error) {
	u, err := url.Parse(ep)
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "/api/current" && u.Path != "/websocket") {
		return nil, fmt.Errorf("%s %q: wss://<host>[:<port>]/api/current, nothing more", EndpointVar, redact(ep))
	}
	return &url.URL{Scheme: "https", Host: u.Host}, nil
}

// TLSFromEnv is how the host's certificate is checked: against the system's CAs, the CA in
// CACertVar's PEM file alone, or not at all (InsecureVar); both set is refused.
func TLSFromEnv(getenv func(string) string) (*tls.Config, error) {
	insecure, ca := Insecure(getenv(InsecureVar)), getenv(CACertVar)
	switch {
	case insecure && ca != "":
		return nil, fmt.Errorf("%s and %s: set one (the CA checks the host's certificate, the other checks none)", CACertVar, InsecureVar)
	case insecure:
		return &tls.Config{InsecureSkipVerify: true}, nil //nolint:gosec // the operator's choice, as the provider's
	case ca == "":
		return &tls.Config{}, nil
	}
	pem, err := os.ReadFile(ca)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", CACertVar, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s %s: no PEM certificate in it", CACertVar, ca)
	}
	return &tls.Config{RootCAs: pool}, nil
}

// ParseURL checks the API's address, base: HTTPS only, as the API key travels in every login and
// upload (and TrueNAS revokes a key used over plain HTTP).
func ParseURL(base string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s %q: https://<host>[:<port>], nothing more", URLVar, redact(base))
	}
	return u, nil
}

// redact is a URL without the password it may carry.
func redact(s string) string {
	if u, err := url.Parse(s); err == nil && u.User != nil {
		return u.Redacted()
	}
	return s
}

// Dial connects to the API at base (https://host[:port]), checking its certificate as tc says, and
// logs in with the API key.
func Dial(ctx context.Context, base, key string, tc *tls.Config) (*Client, error) {
	u, err := ParseURL(base)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: tc,
	}}
	c := &Client{base: u, key: key, http: hc}
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// connect opens the API's WebSocket and logs in, replacing the connection there was.
func (c *Client) connect(ctx context.Context) error {
	if c.conn != nil {
		c.conn.CloseNow() // a connection left idle may be half dead: no close handshake
		c.conn = nil
	}
	ws := *c.base
	ws.Scheme, ws.Path = "wss", "/api/current"
	conn, _, err := websocket.Dial(ctx, ws.String(), &websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		return fmt.Errorf("TrueNAS API at %s: %w", c.base.Host, err)
	}
	conn.SetReadLimit(64 << 20)
	c.conn = conn
	var ok bool
	if err := c.Call(ctx, "auth.login_with_api_key", &ok, c.key); err != nil {
		c.Close()
		return err
	}
	if !ok {
		c.Close()
		return errors.New("TrueNAS refused the API key")
	}
	return nil
}

// Close ends the connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close(websocket.StatusNormalClosure, "")
	c.conn = nil
	return err
}

// Error is an error the API answered.
type Error struct {
	Method  string
	Code    int
	Message string
	Reason  string
	Errname string // the errno's name a call error carries (ENOENT, …)
}

func (e *Error) Error() string {
	if e.Reason != "" && e.Reason != e.Message {
		return fmt.Sprintf("TrueNAS %s: %s (%s)", e.Method, e.Message, strings.TrimSpace(e.Reason))
	}
	return fmt.Sprintf("TrueNAS %s: %s", e.Method, e.Message)
}

// Call calls method with params and decodes its result into out (nil discards it). Messages that
// answer no call of this client (the API's notifications) are skipped.
func (c *Client) Call(ctx context.Context, method string, out any, params ...any) error {
	c.id++
	id := c.id
	if params == nil {
		params = []any{}
	}
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, req); err != nil {
		return fmt.Errorf("TrueNAS %s: %w", method, err)
	}
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("TrueNAS %s: %w", method, err)
		}
		var resp struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Data    struct {
					Reason  string `json:"reason"`
					Errname string `json:"errname"`
				} `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return fmt.Errorf("TrueNAS %s: %w", method, err)
		}
		if resp.ID == nil || *resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return &Error{Method: method, Code: resp.Error.Code, Message: resp.Error.Message, Reason: resp.Error.Data.Reason, Errname: resp.Error.Data.Errname}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("TrueNAS %s: %w", method, err)
		}
		return nil
	}
}

// Job calls a method that runs as a job, and waits for the job to end.
func (c *Client) Job(ctx context.Context, method string, params ...any) error {
	var id int64
	if err := c.Call(ctx, method, &id, params...); err != nil {
		return err
	}
	return c.Wait(ctx, id, method)
}

// Wait waits for job id to succeed; a job that fails, is aborted or vanishes is an error naming
// what.
func (c *Client) Wait(ctx context.Context, id int64, what string) error {
	j, done, err := c.follow(ctx, id, nil, time.Time{}, ended)
	switch {
	case err != nil:
		return err
	case j == nil:
		return fmt.Errorf("%s: job %d vanished", what, id)
	case !done:
		return ctx.Err()
	case j.State == "SUCCESS":
		return nil
	}
	return jobErr(what, id, j)
}

// jobErr is the error of job id, what, ended in j's state, in TrueNAS's words.
func jobErr(what string, id int64, j *jobRow) error {
	return fmt.Errorf("%s: job %d %s: %s", what, id, j.State, strings.TrimSpace(j.Error))
}

// jobRow is a job as core.get_jobs gives it: the fields jobFields selects.
type jobRow struct {
	ID        int64             `json:"id"`
	Method    string            `json:"method"`
	State     string            `json:"state"`
	Arguments []json.RawMessage `json:"arguments"`
	Error     string            `json:"error"`
}

// jobFields is what every read of TrueNAS's jobs selects: jobRow's fields.
var jobFields = []string{"id", "method", "state", "arguments", "error"}

// jobs reads the jobs filter matches (core.get_jobs), in any state unless filter says.
func (c *Client) jobs(ctx context.Context, filter [][]any) ([]jobRow, error) {
	var out []jobRow
	if err := c.Call(ctx, "core.get_jobs", &out, filter, map[string]any{"select": jobFields}); err != nil {
		return nil, err
	}
	return out, nil
}

// job reads job id: nil when TrueNAS has no such job.
func (c *Client) job(ctx context.Context, id int64) (*jobRow, error) {
	js, err := c.jobs(ctx, [][]any{{"id", "=", id}})
	if err != nil || len(js) == 0 {
		return nil, err
	}
	return &js[0], nil
}

// ended says whether a job in state has ended: SUCCESS, FAILED or ABORTED.
func ended(state string) bool { return state == "SUCCESS" || state == "FAILED" || state == "ABORTED" }

// follow reads job id every poll until done(its state), giving the row then read and true; until
// the job is gone, giving nil; or until ctx is done or end (unless zero) passes, giving the last
// row read and false. A read that fails is err. j, unless nil, is the job's row the caller just
// read: taken as the first read.
func (c *Client) follow(ctx context.Context, id int64, j *jobRow, end time.Time, done func(string) bool) (*jobRow, bool, error) {
	for {
		if j == nil {
			var err error
			if j, err = c.job(ctx, id); err != nil {
				return nil, false, err
			}
			if j == nil {
				return nil, false, nil
			}
		}
		if done(j.State) {
			return j, true, nil
		}
		if !end.IsZero() && time.Now().After(end) {
			return j, false, nil
		}
		select {
		case <-ctx.Done():
			return j, false, nil
		case <-time.After(poll):
		}
		j = nil
	}
}

// Upload writes the local file to remote on the host (filesystem.put through /_upload/, 0600: a
// seed holds the one-time SSH host key, a disk the release's pull account; the middleware reads
// it as root, and libvirt hands a VM's disk or CD-ROM to the VM's process as it starts it),
// streamed: a disk image does not fit in memory.
func (c *Client) Upload(ctx context.Context, local, remote string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = c.send(ctx, f, filepath.Base(local), remote, io.Discard)
	return err
}

// send is Upload of the bytes f gives, the file's name name, every byte it reads of f to send also
// written to sent: TrueNAS took them all only when send succeeds; sent is written no more once
// send returns. It gives what the upload may have left at remote, failed or not (upload).
func (c *Client) send(ctx context.Context, f io.Reader, name, remote string, sent io.Writer) (upload, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	wrote := make(chan error, 1)
	go func() {
		err := func() error {
			data, err := json.Marshal(map[string]any{"method": "filesystem.put", "params": []any{remote, map[string]any{"mode": 0o600}}})
			if err != nil {
				return err
			}
			if err := mw.WriteField("data", string(data)); err != nil {
				return err
			}
			w, err := mw.CreateFormFile("file", name)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, io.TeeReader(f, sent)); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
		wrote <- err
	}()
	u := *c.base
	u.Path = "/_upload/"
	reqBody := &tracked{r: pr}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), reqBody)
	if err != nil {
		pr.Close()
		<-wrote
		return putNothing, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	// The transport is done with the body: a part of the file it did not take fails the copy, which
	// has ended once wrote answers (sent is complete, and no longer written).
	pr.Close()
	werr := <-wrote
	if err != nil {
		put := putMaybe
		if !reqBody.read.Load() {
			put = putNothing
		}
		return put, fmt.Errorf("upload of %s: %w", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		put := putMaybe
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			if !reqBody.drained.Load() {
				put = putNothing
			}
		}
		return put, fmt.Errorf("upload of %s refused: HTTP %d %s", name, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)])))
	}
	if werr != nil {
		return putFile, fmt.Errorf("upload of %s: %w", name, werr)
	}
	var job struct {
		ID *int64 `json:"job_id"`
	}
	if err := json.Unmarshal(body, &job); err != nil || job.ID == nil {
		return putFile, fmt.Errorf("upload of %s: no job in the answer %q", name, body[:min(len(body), 300)])
	}
	// The WebSocket sat unread through the upload (minutes for a disk), which the server may have
	// closed: the job is waited for on a new one.
	if err := c.connect(ctx); err != nil {
		return putFile, err
	}
	return putFile, c.Wait(ctx, *job.ID, "upload "+remote)
}

// tracked is an upload's request body: read, once the transport took a byte of it; drained, once
// it took the whole.
type tracked struct {
	r             io.ReadCloser
	read, drained atomic.Bool
}

func (t *tracked) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.read.Store(true)
	}
	if errors.Is(err, io.EOF) {
		t.drained.Store(true)
	}
	return n, err
}

func (t *tracked) Close() error { return t.r.Close() }

// upload is what an upload may have left at its path.
type upload int

const (
	// putNothing: no byte of it can have reached TrueNAS's filesystem.put: the request was not
	// made, or failed before its body was read (the dial, TLS), or HTTP 401 or 403 (answered on
	// the headers) before its body was read whole.
	putNothing upload = iota
	// putMaybe: no answer once its body was read, or another refusal (a 413 included): a
	// filesystem.put job of the path, if TrueNAS made one, may have written a part of it.
	putMaybe
	// putFile: TrueNAS took the body (HTTP 200).
	putFile
)

// ImportImage writes the disk image at local — want its SHA-256 — into the zvol (a dataset path,
// pool/…), converted by vm.device.convert (which reads the format off the file's extension) from a
// file uploaded into staging (a dataset's mount point). The zvol records the image in
// ImageProperty, so a re-run is safe: a zvol holding this image already is left as it is (true),
// read alone (the file is not read, no lock taken); one an earlier run began writing it into is
// written again; one holding another image, or data of its own (without the property, a
// logicalreferenced past an empty zvol's), is refused, as is one a VM's device uses, and one an
// upload or a convert of this import writes into (busy).
//
// One run at a time writes into a zvol: the run makes the lock, the zvol ref (lock), holds it
// until it ends, its RunProperty this run's nonce, and deletes it then (unlock); a run finding
// another run's lock refuses, naming the lock to delete once neither a run nor a job of that import
// runs. The file is opened before the lock is taken, then uploaded under a name of this run's,
// stagedPrefix then <nonce>-<file name>, hashed as it is sent — the one read of it, so the bytes
// converted are the bytes checked: a file that is not want is refused before the zvol is marked —
// and emptied when the run ends (TrueNAS's API deletes no file) unless the upload left nothing (send's
// putNothing) or nothing but an upload that made no job of it (putMaybe, emptyStaged). The zvol,
// its devices and the lock are read again before the zvol is marked: any change since the upload
// began is refused. A convert this run cannot see end, or cannot tell whose it is (cut off, its answer lost,
// its job unread or unidentified), leaves the lock and the staged file, named in a *LockLeftError;
// one of this run's TrueNAS gives as SUCCESS after the wait was cut off wrote the zvol, which is
// marked written, the cut-off a warning; one that ended FAILED is a *FailedError. Once the zvol is marked
// written, a failure to delete the lock or empty the file is a warning on warn.
func (c *Client) ImportImage(ctx context.Context, local, want, staging, zvol string, warn io.Writer) (held bool, err error) {
	if !sha256Hex(want) {
		return false, fmt.Errorf("the image's sha256 %q: 64 lower-case hex digits", want)
	}
	done, partial := "sha256:"+want, importingMark+"sha256:"+want
	z, err := c.zvol(ctx, zvol)
	if err != nil {
		return false, err
	}
	if z.mark() == done {
		return true, nil
	}
	if err := otherImage(zvol, z.mark(), want); err != nil {
		return false, err
	}
	// Opened before the lock is taken: a file that cannot be read fails with no lock made and
	// nothing staged.
	f, err := os.Open(local)
	if err != nil {
		return false, err
	}
	defer f.Close()
	run, err := nonce()
	if err != nil {
		return false, err
	}
	ref, prefix := zvol+"-stowage-ref", stagedPrefix(staging, want, zvol)
	empty, err := c.lock(ctx, ref, zvol, z.Volblocksize.Value, run, prefix)
	if err != nil {
		return false, err
	}
	// marked: the zvol holds the image, marked done, and a failure to clean up after is a warning;
	// keep: a convert may still run, so the staged file and the lock are left, named in err.
	marked, keep := false, false
	finish := func(cerr error) {
		switch {
		case cerr == nil:
		case marked:
			fmt.Fprintf(warn, "warning: the zvol %s holds the image %s; %v\n", zvol, done, cerr)
		default:
			err = errors.Join(err, cerr)
		}
	}
	defer func() {
		if !keep {
			finish(c.unlock(ctx, ref, run))
		}
	}()
	mark, err := c.admit(ctx, zvol, want, empty, prefix)
	if err != nil || mark == done {
		marked = err == nil
		return marked, err
	}
	remote := prefix + run + "-" + filepath.Base(local)
	// put is what the upload may have left at remote: emptied as the run ends unless it is nothing
	// (an upload refused before TrueNAS took a byte wrote nothing to empty).
	var put upload
	defer func() {
		if put != putNothing && !keep {
			finish(c.emptyStaged(ctx, remote, put))
		}
	}()
	h := sha256.New()
	if put, err = c.send(ctx, f, filepath.Base(local), remote, h); err != nil {
		return false, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return false, fmt.Errorf("%s: sha256 %s, expected %s: refusing to build a disk from it", local, got, want)
	}
	// Read again under the lock, as the upload may have taken an hour.
	if err := c.holds(ctx, ref, zvol, run); err != nil {
		return false, err
	}
	again, err := c.admit(ctx, zvol, want, empty, prefix)
	if err != nil {
		return false, err
	}
	if again != mark {
		return false, fmt.Errorf("the zvol %s's %s changed during the upload (%q, now %q), which no import holding its lock writes: refusing to write it; find what wrote it, then run this again", zvol, ImageProperty, mark, again)
	}
	if err := c.setMark(ctx, zvol, partial); err != nil {
		return false, err
	}
	mctx := ctx
	var cut *cutOff
	if err := c.convert(ctx, remote, zvol); err != nil {
		var left *ConvertLeftError
		if errors.As(err, &left) {
			keep = true
			return false, &LockLeftError{fmt.Errorf("%w: the lock %s and the staged file %s are left; once TrueNAS's Jobs shows no vm.device.convert of %s, %s (TrueNAS's API deletes no file: delete %s on the host)", err, ref, remote, remote, deleteLock(ref), remote)}
		}
		if !errors.As(err, &cut) {
			return false, err
		}
		// The convert wrote the zvol, its wait cut off: marked on the connection settle read the
		// job's end on, which it left on c, past ctx's end, as ctx may be done.
		dctx, cancel := detach(ctx, refTimeout)
		defer cancel()
		mctx = dctx
	}
	if err := c.setMark(mctx, zvol, done); err != nil {
		if cut != nil {
			return false, fmt.Errorf("%v; the zvol %s not marked written: %w", cut, zvol, err)
		}
		return false, err
	}
	marked = true
	if cut != nil {
		finish(cut)
	}
	return false, nil
}

// otherImage refuses a zvol whose ImageProperty, mark, names another image than want, written or
// being written.
func otherImage(zvol, mark, want string) error {
	if mark != "" && mark != "sha256:"+want && mark != importingMark+"sha256:"+want {
		return fmt.Errorf("the zvol %s holds another image (%s %s): an image is written once into a new zvol; give this one a zvol of its own", zvol, ImageProperty, mark)
	}
	return nil
}

// LockLeftError is an error after which the zvol's lock stays: a run again is refused until the
// fix its message names is done.
type LockLeftError struct{ Err error }

func (e *LockLeftError) Error() string { return e.Err.Error() }

func (e *LockLeftError) Unwrap() error { return e.Err }

// nonce is a run's random name.
func nonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// admit reads the zvol under the lock and gives its ImageProperty: the image itself (held), the
// image being written, or none; it refuses another image, data of its own without the property
// (a logicalreferenced past empty), a VM's device using it, and a job of this import running.
func (c *Client) admit(ctx context.Context, zvol, want string, empty int64, prefix string) (string, error) {
	z, err := c.zvol(ctx, zvol)
	if err != nil {
		return "", err
	}
	mark := z.mark()
	if mark == "sha256:"+want {
		return mark, nil
	}
	if err := otherImage(zvol, mark, want); err != nil {
		return "", err
	}
	if mark == "" && *z.Logical.Parsed > empty {
		return "", fmt.Errorf("the zvol %s holds data (logicalreferenced %d bytes, a zvol never written %d) and no %s: refusing to write over it; give the image a new zvol, never written", zvol, *z.Logical.Parsed, empty, ImageProperty)
	}
	var devs []struct {
		ID int64 `json:"id"`
		VM int64 `json:"vm"`
	}
	if err := c.Call(ctx, "vm.device.query", &devs, [][]any{{"attributes.path", "=", "/dev/zvol/" + zvol}}); err != nil {
		return "", err
	}
	if len(devs) > 0 {
		return "", fmt.Errorf("the zvol %s is the disk of VM %d (device %d): refusing to write a disk a VM may run on; import the image before the VM's disk device is made (order the device after the import)", zvol, devs[0].VM, devs[0].ID)
	}
	if err := c.busy(ctx, zvol, prefix); err != nil {
		return "", err
	}
	return mark, nil
}

// ImageProperty is the ZFS user property ImportImage records a zvol's image in: importingMark and
// the image's sha256:<hex> while it writes it, sha256:<hex> once written.
const ImageProperty = "org.stowage:image"

// importingMark prefixes ImageProperty's value while the image is being written.
const importingMark = "importing:"

// RefProperty is the ZFS user property on the lock ImportImage makes, a reference zvol: the zvol it
// locks. A dataset of the lock's name without it is not Stowage's, never deleted.
const RefProperty = "org.stowage:ref"

// RunProperty is the ZFS user property naming the run holding the lock: its nonce.
const RunProperty = "org.stowage:run"

// refTimeout bounds each step that outlives a cancelled run: the lock's release, the staged file's
// emptying, the zvol's mark after a convert cut off, the look for a convert's lost job (with
// lostGrace), and the connection and the last read of settle's wait.
var refTimeout = 30 * time.Second

// settleTimeout bounds settle's wait for a convert cut off to end.
var settleTimeout = 20 * time.Second

// lostGrace is how long lost looks for the job of a convert call whose answer was lost, lostLooks
// times, the first at once.
var lostGrace = 5 * time.Second

const lostLooks = 3

// sha256Hex says whether s is a SHA-256 written as hex.EncodeToString does: 64 lower-case hex digits.
func sha256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// zvolInfo is what ImportImage reads of a zvol.
type zvolInfo struct {
	Type    string `json:"type"`
	Logical *struct {
		Parsed *int64 `json:"parsed"`
	} `json:"logicalreferenced"`
	Volblocksize *struct {
		Value string `json:"value"`
	} `json:"volblocksize"`
	Creation *struct {
		Rawvalue string `json:"rawvalue"`
	} `json:"creation"`
	UserProperties map[string]struct {
		Value string `json:"value"`
	} `json:"user_properties"`
}

// mark is the zvol's ImageProperty, "" without one.
func (z zvolInfo) mark() string { return z.UserProperties[ImageProperty].Value }

// prop is the user property key, "" without one.
func (z zvolInfo) prop(key string) string { return z.UserProperties[key].Value }

// made is the dataset's creation time, false when TrueNAS gave none (its rawvalue is in seconds
// since the epoch).
func (z zvolInfo) made() (time.Time, bool) {
	if z.Creation == nil {
		return time.Time{}, false
	}
	s, err := strconv.ParseInt(z.Creation.Rawvalue, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(s, 0), true
}

// datasets reads the dataset name with the properties named (TrueNAS gives null for any other)
// and its user properties: none when there is no such dataset.
func (c *Client) datasets(ctx context.Context, name string, props ...string) ([]zvolInfo, error) {
	var zs []zvolInfo
	extra := map[string]any{"retrieve_children": false, "properties": props, "user_properties": true}
	if err := c.Call(ctx, "pool.dataset.query", &zs, [][]any{{"id", "=", name}}, map[string]any{"extra": extra}); err != nil {
		return nil, err
	}
	return zs, nil
}

// zvol reads the zvol at name (pool/…) with its logicalreferenced and volblocksize and its user
// properties, refusing a dataset that is none, does not exist, or comes without any of them.
func (c *Client) zvol(ctx context.Context, name string) (zvolInfo, error) {
	zs, err := c.datasets(ctx, name, "logicalreferenced", "volblocksize")
	if err != nil {
		return zvolInfo{}, err
	}
	switch {
	case len(zs) == 0:
		return zvolInfo{}, fmt.Errorf("the zvol %s does not exist: create it first (a pool/… path, not /dev/zvol/…)", name)
	case zs[0].Type != "VOLUME":
		return zvolInfo{}, fmt.Errorf("%s is a %s, not a zvol", name, strings.ToLower(zs[0].Type))
	case zs[0].Logical == nil || zs[0].Logical.Parsed == nil:
		return zvolInfo{}, fmt.Errorf("pool.dataset.query gave no logicalreferenced for the zvol %s: cannot tell whether it holds data; refusing to write over it", name)
	case zs[0].Volblocksize == nil || zs[0].Volblocksize.Value == "":
		return zvolInfo{}, fmt.Errorf("pool.dataset.query gave no volblocksize for the zvol %s: cannot measure a zvol never written beside it; refusing to write over it", name)
	case zs[0].UserProperties == nil:
		return zvolInfo{}, fmt.Errorf("pool.dataset.query gave no user_properties for the zvol %s: cannot read its %s; refusing to write over it", name, ImageProperty)
	}
	return zs[0], nil
}

// lock makes ref, the lock of imports into zvol held by run: a zvol in zvol's parent, of zvol's
// volblocksize, sparse, 1 MiB, its RefProperty zvol and its RunProperty run. TrueNAS refuses a
// dataset of a name that exists, so one run makes it. A create that failed reads the name: a lock
// of this run's (the create made it, its answer lost) is held; another run's is refused at any age
// (busy's refusal while a job of the import runs), never deleted — only its run, or the operator
// the refusal names it to, deletes it; a dataset of ref's name without RefProperty zvol, or not a
// zvol, is refused, never deleted. A read that fails deletes the lock if it is this run's
// (unlock). It gives the lock's logicalreferenced, read once made: a zvol never written's,
// measured on this host. A zvol without ImageProperty is empty while its logicalreferenced is at
// most this: it counts the bytes written before compression, so a block of data adds at least
// its size, and an all-zero block, a hole once compressed, holds nothing to lose.
func (c *Client) lock(ctx context.Context, ref, zvol, volblocksize, run, prefix string) (int64, error) {
	cerr := c.Call(ctx, "pool.dataset.create", nil, map[string]any{"name": ref, "type": "VOLUME", "volsize": 1 << 20, "volblocksize": volblocksize, "sparse": true,
		"user_properties": []map[string]any{{"key": RefProperty, "value": zvol}, {"key": RunProperty, "value": run}}})
	if cerr != nil {
		ds, err := c.lockRead(ctx, ref, cerr)
		if err != nil {
			return 0, errors.Join(fmt.Errorf("the lock %s of imports into %s: %w", ref, zvol, cerr), err, c.unlock(ctx, ref, run))
		}
		if len(ds) == 0 {
			return 0, fmt.Errorf("the lock %s of imports into %s: %w", ref, zvol, cerr)
		}
		if ds[0].Type != "VOLUME" || ds[0].prop(RefProperty) != zvol {
			return 0, fmt.Errorf("%s, the name of the lock of imports into %s, is a dataset without %s=%s: not Stowage's, refusing to delete it; rename or delete it", ref, zvol, RefProperty, zvol)
		}
		if other := ds[0].prop(RunProperty); other != run {
			if err := c.busy(ctx, zvol, prefix); err != nil {
				return 0, err
			}
			when := "at a time TrueNAS does not give"
			if made, ok := ds[0].made(); ok {
				when = made.UTC().Format(time.RFC3339)
			}
			return 0, &LockLeftError{fmt.Errorf("another import into %s holds its lock %s (run %s, made %s): once no `tofu apply` or `vm truenas import-image` of %s runs on any machine, and TrueNAS runs no job of it (Jobs: a vm.device.convert into %s, or a filesystem.put or vm.device.convert of a file %s<nonce>-…), %s; while either runs, let it end", zvol, ref, other, when, zvol, zvol, prefix, deleteLock(ref))}
		}
		// This run's own: the create made it, its answer lost.
	}
	r, err := c.zvol(ctx, ref)
	if err != nil {
		return 0, errors.Join(err, c.unlock(ctx, ref, run))
	}
	return *r.Logical.Parsed, nil
}

// holds refuses when run no longer holds the lock ref of imports into zvol.
func (c *Client) holds(ctx context.Context, ref, zvol, run string) error {
	ds, err := c.datasets(ctx, ref)
	if err != nil {
		return err
	}
	if len(ds) == 0 || ds[0].prop(RunProperty) != run {
		return fmt.Errorf("this run (%s) no longer holds the lock %s of imports into %s (deleted or taken over meanwhile): refusing to write the zvol; run this again", run, ref, zvol)
	}
	return nil
}

// lockRead reads the dataset ref after its create failed with cerr: on a new connection when
// cerr is no answer of the API's (the connection broke, the create's outcome unknown).
func (c *Client) lockRead(ctx context.Context, ref string, cerr error) ([]zvolInfo, error) {
	var api *Error
	if !errors.As(cerr, &api) {
		if err := c.connect(ctx); err != nil {
			return nil, err
		}
	}
	return c.datasets(ctx, ref, "creation")
}

// deleteLock is the fix for a lock left: delete it, then run again.
func deleteLock(ref string) string {
	return fmt.Sprintf(`delete the zvol %s on TrueNAS (Datasets, or pool.dataset.delete with {"recursive": true}), then run this again`, ref)
}

// stagedPrefix is the start of the name an import of the image want into zvol stages its file
// under in staging: <want's 16 first hex digits>-<zvol's SHA-256's 8 first hex digits>-, a run's
// nonce and the file's name after it. Imports of one image into two zvols share no prefix, so
// neither's jobs make the other busy.
func stagedPrefix(staging, want, zvol string) string {
	h := sha256.Sum256([]byte(zvol))
	return path.Join(staging, want[:16]+"-"+hex.EncodeToString(h[:])[:8]) + "-"
}

// ConvertLeftError is a convert an import cannot see end: it may still write the zvol, so its lock
// and staged file are left, and a run again with more time would not get past them.
type ConvertLeftError struct{ Msg string }

func (e *ConvertLeftError) Error() string { return e.Msg }

// convert converts the staged file remote into zvol and waits for it. TrueNAS runs one
// vm.device.convert at a time and queues one more, handing a further call the queued job, another
// import's: the job is read first, and one whose arguments read as another convert's (whose) is
// refused, the one ending that releases the lock without this run's job seen to end. A job the
// read cannot tell (gone, unread, arguments that are no convert's) is settled. Waited for, a
// job that ends SUCCESS is done; FAILED is a *FailedError with TrueNAS's error; ABORTED or gone
// is settled on the connection the wait read it on, and a wait cut off (ctx done, the connection
// broken) on a new one. A call whose answer was lost looks for its job first (lost). Nothing here
// stops a convert: core.job_abort marks a running one ABORTED while its qemu-img writes on, so the
// zvol is not this run's to give up until its job ends.
func (c *Client) convert(ctx context.Context, remote, zvol string) error {
	var id int64
	if err := c.Call(ctx, "vm.device.convert", &id, map[string]any{"source": remote, "destination": zvol}); err != nil {
		var api *Error
		if errors.As(err, &api) {
			return err
		}
		return c.lost(ctx, remote, zvol, err)
	}
	what := "vm.device.convert of " + remote
	j, err := c.job(ctx, id)
	if err != nil {
		return c.settle(ctx, id, remote, zvol, err, true)
	}
	switch who, of := whose(j, remote, zvol); who {
	case otherJob:
		return foreign(remote, zvol, id, of)
	case unknownJob:
		return c.settle(ctx, id, remote, zvol, fmt.Errorf("TrueNAS answered vm.device.convert of %s into %s with job %d, %s", remote, zvol, id, of), false)
	}
	j, done, err := c.follow(ctx, id, j, time.Time{}, ended)
	switch {
	case err != nil:
		return c.settle(ctx, id, remote, zvol, err, true)
	case j == nil:
		return c.settle(ctx, id, remote, zvol, fmt.Errorf("%s: job %d vanished", what, id), false)
	case !done:
		return c.settle(ctx, id, remote, zvol, ctx.Err(), true)
	case j.State == "SUCCESS":
		return nil
	case j.State == "FAILED":
		return &FailedError{jobErr(what, id, j)}
	}
	return c.settle(ctx, id, remote, zvol, jobErr(what, id, j), false)
}

// foreign is the refusal of job id, another import's convert (of, whose's words), which TrueNAS
// answered this convert's call with.
func foreign(remote, zvol string, id int64, of string) error {
	return fmt.Errorf("TrueNAS answered vm.device.convert of %s into %s with job %d, another convert's (%s): TrueNAS runs one convert at a time and queues one more, handing a further call the queued job; let the other converts end, then run this again", remote, zvol, id, of)
}

// owner is whose convert a job is.
type owner int

const (
	unknownJob owner = iota // gone, or its arguments no convert's: it may be this run's
	ourJob                  // a convert of this run's staged file into its zvol
	otherJob                // a convert of another file, or into another zvol
)

// whose reads job j's arguments as convertJob and running do (paths cleaned, a zvol's /dev/zvol/
// prefix dropped): a convert of remote into zvol, another convert, or unknown — j nil, another
// method, or arguments without a source and a destination. It also words them: "of <source> into
// <destination>", or why they tell nothing.
func whose(j *jobRow, remote, zvol string) (owner, string) {
	if j == nil {
		return unknownJob, "which TrueNAS's jobs do not hold"
	}
	src, dst, ok := convertArgs(j.Arguments)
	switch {
	case j.Method != "vm.device.convert":
		return unknownJob, fmt.Sprintf("which TrueNAS gives as a %s", j.Method)
	case !ok || src == "" || dst == "":
		return unknownJob, "whose arguments TrueNAS gives as no convert's"
	case sameFile(src, remote) && sameZvol(dst, zvol):
		return ourJob, fmt.Sprintf("of %s into %s", src, dst)
	}
	return otherJob, fmt.Sprintf("of %s into %s", src, dst)
}

// FailedError is a job TrueNAS ended FAILED: its cause is the job's own, so running the step again
// with more time fails the same way.
type FailedError struct{ Err error }

func (e *FailedError) Error() string { return e.Err.Error() }

func (e *FailedError) Unwrap() error { return e.Err }

// settle follows the convert job id of remote into zvol, whose watch ended in werr, reading it for
// up to settleTimeout (or till refTimeout before its context's deadline, if sooner) on a new
// connection when reconnect (ctx may be cancelled, the connection broken), else on the one there
// is; either is left on c for the caller's next step. It reads the job's arguments itself
// (whose), from the last row read: another convert's is refused (foreign). Only SUCCESS and FAILED
// end a convert, and only this convert's ending is read: SUCCESS is a *cutOff (the zvol written),
// FAILED a *FailedError with the job's error (the middleware's convert fails once its qemu-img has
// exited, or before it started). Anything else — RUNNING or WAITING past the watch (named with
// the time watched), ABORTED (core.job_abort marks a convert so while its qemu-img writes on), the
// job gone, unread, or ended with arguments that tell nothing — is a *ConvertLeftError naming it: a
// qemu-img of this run's may still write the zvol.
func (c *Client) settle(ctx context.Context, id int64, remote, zvol string, werr error, reconnect bool) error {
	left := func(why string) error {
		return &ConvertLeftError{fmt.Sprintf("%v; the convert job %d %s", werr, id, why)}
	}
	unread := func(err error) error { return left(fmt.Sprintf("could not be read (%v): it may still run", err)) }
	// Long enough for the connection's making, the wait and a last read (refTimeout each but the
	// wait): a job still running is named as such, in still's words, never cut off as unread.
	budget := refTimeout + settleTimeout + refTimeout
	var dctx context.Context
	var cancel context.CancelFunc
	if reconnect {
		var err error
		if dctx, cancel, err = c.cleanup(ctx, budget); err != nil {
			return unread(err)
		}
	} else {
		dctx, cancel = detach(ctx, budget)
	}
	defer cancel()
	start := time.Now()
	still := func(state string) error {
		return left(fmt.Sprintf("is still %s on TrueNAS after %s", state, watched(time.Since(start))))
	}
	j, err := c.job(dctx, id)
	if err != nil {
		return unread(err)
	}
	if j != nil && !ended(j.State) {
		if who, _ := whose(j, remote, zvol); who != otherJob {
			if j, _, err = c.follow(dctx, id, j, until(dctx, settleTimeout), ended); err != nil {
				return unread(err)
			}
		}
	}
	if j == nil {
		return left("is gone from TrueNAS's jobs: whether its qemu-img still writes cannot be told")
	}
	// Told from the last row read: arguments a first row did not give may read later.
	who, of := whose(j, remote, zvol)
	switch {
	case who == otherJob:
		return foreign(remote, zvol, id, of)
	case !ended(j.State):
		return still(j.State)
	case j.State == "ABORTED":
		return left("is ABORTED, which TrueNAS marks an aborted convert at once while its qemu-img writes on: it may still run")
	case who == unknownJob:
		return left(fmt.Sprintf("ended %s, %s: whether a convert of this run's still writes the zvol cannot be told", j.State, of))
	case j.State == "SUCCESS":
		return &cutOff{id: id, err: werr}
	}
	return &FailedError{fmt.Errorf("%v; the convert job %d then ended FAILED: %s", werr, id, strings.TrimSpace(j.Error))}
}

// watched words the time settle watched a job: rounded to a twentieth of settleTimeout.
func watched(d time.Duration) string { return d.Round(settleTimeout / 20).String() }

// until is d from now, or refTimeout before ctx's deadline when that comes sooner: the read that
// ends a watch has its margin.
func until(ctx context.Context, d time.Duration) time.Time {
	end := time.Now().Add(d)
	if dl, ok := ctx.Deadline(); ok && dl.Add(-refTimeout).Before(end) {
		return dl.Add(-refTimeout)
	}
	return end
}

// lost follows a vm.device.convert call of remote into zvol whose answer was lost (cerr): on a
// connection of its own it looks for the job among TrueNAS's (convertJob), lostLooks times over
// lostGrace, as a call the middleware took may make its job after the first look. One found:
// settled, on that connection. None: no convert was made, an error saying so, which releases the
// lock — wrongly only for a call whose job TrueNAS makes past lostGrace after the look began (a
// middleware that slow), a convert that then writes the zvol unlocked. A lookup that fails: a
// *ConvertLeftError.
func (c *Client) lost(ctx context.Context, remote, zvol string, cerr error) error {
	what := fmt.Sprintf("vm.device.convert of %s into %s: %v", remote, zvol, cerr)
	unread := func(err error) error {
		return &ConvertLeftError{fmt.Sprintf("%s (its answer lost, its job not read: %v; a convert may run)", what, err)}
	}
	dctx, cancel, err := c.cleanup(ctx, refTimeout+lostGrace)
	if err != nil {
		return unread(err)
	}
	defer cancel()
	var id int64
	for look := 1; ; look++ {
		if id, err = c.convertJob(dctx, remote, zvol); err != nil {
			return unread(err)
		}
		if id != 0 || look == lostLooks {
			break
		}
		select {
		case <-dctx.Done():
			return unread(dctx.Err())
		case <-time.After(lostGrace / (lostLooks - 1)):
		}
	}
	if id == 0 {
		return fmt.Errorf("%s (its answer lost; TrueNAS has no job of it, looked for %d times over %s: nothing ran)", what, lostLooks, lostGrace)
	}
	return c.settle(dctx, id, remote, zvol, errors.New(what+" (its answer lost)"), false)
}

// convertJob is the id of TrueNAS's vm.device.convert job of remote into zvol in any state, 0 when
// there is none; more than one is an error. remote holds a run's nonce, so such a job is the run's.
func (c *Client) convertJob(ctx context.Context, remote, zvol string) (int64, error) {
	js, err := c.jobs(ctx, [][]any{{"method", "=", "vm.device.convert"}})
	if err != nil {
		return 0, err
	}
	var ids []int64
	for _, j := range js {
		if who, _ := whose(&j, remote, zvol); who == ourJob {
			ids = append(ids, j.ID)
		}
	}
	switch len(ids) {
	case 0:
		return 0, nil
	case 1:
		return ids[0], nil
	}
	return 0, fmt.Errorf("TrueNAS gives jobs %v as vm.device.convert of %s into %s: one call makes one", ids, remote, zvol)
}

// putJob is a filesystem.put job of remote TrueNAS has, in any state: one not ended if there is
// one, nil when there is none.
func (c *Client) putJob(ctx context.Context, remote string) (*jobRow, error) {
	js, err := c.jobs(ctx, [][]any{{"method", "=", "filesystem.put"}})
	if err != nil {
		return nil, err
	}
	var found *jobRow
	for i := range js {
		if p, ok := putPath(js[i]); ok && sameFile(p, remote) && (found == nil || !ended(js[i].State)) {
			found = &js[i]
		}
	}
	return found, nil
}

// putPath is the path a filesystem.put job writes, false when its arguments give none.
func putPath(j jobRow) (string, bool) {
	var p string
	if j.Method != "filesystem.put" || len(j.Arguments) == 0 || json.Unmarshal(j.Arguments[0], &p) != nil {
		return "", false
	}
	return p, true
}

// cutOff is a convert of this run's whose wait was cut off (err), and which TrueNAS then gave as
// SUCCESS: the zvol is written.
type cutOff struct {
	id  int64
	err error
}

func (e *cutOff) Error() string {
	return fmt.Sprintf("the wait for the convert job %d was cut off (%v), and the job then ended SUCCESS", e.id, e.err)
}

// convertArgs is the source and destination of a vm.device.convert job's arguments, false when
// they are not a convert's.
func convertArgs(args []json.RawMessage) (src, dst string, ok bool) {
	var a struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	if len(args) == 0 || json.Unmarshal(args[0], &a) != nil {
		return "", "", false
	}
	return a.Source, a.Destination, true
}

// sameZvol says whether two zvol paths name one zvol: cleaned, a /dev/zvol/ prefix dropped, as the
// middleware's vm.device.convert reads its zvol.
func sameZvol(a, b string) bool {
	norm := func(p string) string { return strings.TrimPrefix(path.Clean(p), "/dev/zvol/") }
	return norm(a) == norm(b)
}

// sameFile says whether two file paths name one file, cleaned.
func sameFile(a, b string) bool { return path.Clean(a) == path.Clean(b) }

// cleanup is a context for a step that outlives ctx's cancellation, d long (the new connection's
// making included), always on a new connection: a call ctx cut off closed the old one, and one
// that failed otherwise may have left it broken.
func (c *Client) cleanup(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc, error) {
	dctx, cancel := detach(ctx, d)
	if err := c.connect(dctx); err != nil {
		cancel()
		return nil, nil, err
	}
	return dctx, cancel, nil
}

// detach is a context for a step that outlives ctx's cancellation, d long, on the connection
// there is: one a call just answered on.
func detach(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}

// unlock deletes the lock ref while run holds it, recursively (a periodic task's snapshots of it
// would block the deletion; RefProperty proved it Stowage's), even when ctx is cancelled. A lock
// gone or another run's is left. It deletes only a lock whose RunProperty it read as run's: no run
// deletes or takes over another's (lock refuses it at any age), so between the read and the
// deletion the lock stays run's — unless an operator deletes it then, against the conditions
// lock's refusal names, and another run makes its own: that voids the exclusion, and this deletes
// the other run's lock. A deletion that fails is a *LockLeftError.
func (c *Client) unlock(ctx context.Context, ref, run string) error {
	failed := func(err error) error {
		return &LockLeftError{fmt.Errorf("the lock %s not deleted (%v): once no job of this import runs, %s", ref, err, deleteLock(ref))}
	}
	dctx, cancel, err := c.cleanup(ctx, refTimeout)
	if err != nil {
		return failed(err)
	}
	defer cancel()
	ds, err := c.datasets(dctx, ref)
	if err != nil {
		return failed(err)
	}
	if len(ds) == 0 || ds[0].prop(RunProperty) != run {
		return nil
	}
	if err := c.Call(dctx, "pool.dataset.delete", nil, ref, map[string]any{"recursive": true}); err != nil {
		return failed(err)
	}
	return nil
}

// emptyStaged truncates the staged file remote by uploading an empty file over it, even when ctx
// is cancelled: TrueNAS's API deletes no file, so an empty file of that name stays. put is what its
// upload may have left (never putNothing): putMaybe empties it only once a filesystem.put job of
// remote is found, as no job wrote nothing. A put job of remote RUNNING or WAITING is followed to
// its end first, for up to settleTimeout: one that does not end is left, the file not emptied.
func (c *Client) emptyStaged(ctx context.Context, remote string, put upload) error {
	failed := func(err error) error {
		return fmt.Errorf("the staged file %s not emptied (TrueNAS's API deletes no file: delete it on the host): %w", remote, err)
	}
	// Long enough for the connection's making, the wait for the upload's job and the emptying.
	dctx, cancel, err := c.cleanup(ctx, refTimeout+settleTimeout+refTimeout)
	if err != nil {
		return failed(err)
	}
	defer cancel()
	j, err := c.putJob(dctx, remote)
	if err != nil {
		return failed(fmt.Errorf("its upload's job not read: %w", err))
	}
	if j == nil && put == putMaybe {
		return nil
	}
	// An upload still running would write on after the emptying: it is waited for to end first.
	if j != nil && !ended(j.State) {
		start := time.Now()
		id := j.ID
		if j, _, err = c.follow(dctx, id, j, until(dctx, settleTimeout), ended); err != nil {
			return failed(fmt.Errorf("its upload's job %d not read: %w", id, err))
		}
		if j != nil && !ended(j.State) {
			return failed(fmt.Errorf("its upload's job %d is still %s on TrueNAS after %s", id, j.State, watched(time.Since(start))))
		}
	}
	if _, err := c.send(dctx, strings.NewReader(""), path.Base(remote), remote, io.Discard); err != nil {
		return failed(err)
	}
	return nil
}

// busy refuses while a job of this import runs (running): the one refusal of it, lock's and
// admit's.
func (c *Client) busy(ctx context.Context, zvol, prefix string) error {
	job, err := c.running(ctx, zvol, prefix)
	if err != nil || job == "" {
		return err
	}
	return fmt.Errorf("another import into %s is running: %s; let it end, then run this again", zvol, job)
}

// running names a job of an import into zvol RUNNING or WAITING, "" when there is none: a
// vm.device.convert into zvol or from a staged file of this import (its path beginning with prefix,
// stagedPrefix's: this image into this zvol), or a filesystem.put (an upload, whose job runs while
// the file streams) of such a file. Paths are compared cleaned, a zvol's /dev/zvol/ prefix dropped
// (sameZvol, as convert compares its job's).
func (c *Client) running(ctx context.Context, zvol, prefix string) (string, error) {
	js, err := c.jobs(ctx, [][]any{{"method", "in", []string{"vm.device.convert", "filesystem.put"}}, {"state", "in", []string{"RUNNING", "WAITING"}}})
	if err != nil {
		return "", err
	}
	staged := func(p string) bool { return strings.HasPrefix(path.Clean(p), prefix) }
	for _, j := range js {
		src, dst, conv := convertArgs(j.Arguments)
		put, isPut := putPath(j)
		switch {
		case j.Method == "vm.device.convert" && conv && (sameZvol(dst, zvol) || staged(src)):
			return fmt.Sprintf("job %d (vm.device.convert of %s into %s)", j.ID, src, dst), nil
		case isPut && staged(put):
			return fmt.Sprintf("job %d (filesystem.put of %s)", j.ID, put), nil
		}
	}
	return "", nil
}

// setMark records value as the zvol's ImageProperty.
func (c *Client) setMark(ctx context.Context, zvol, value string) error {
	return c.Call(ctx, "pool.dataset.update", nil, zvol, map[string]any{"user_properties_update": []map[string]any{{"key": ImageProperty, "value": value}}})
}

// state is the VM's state (RUNNING, STOPPED, …), or "" when there is no such VM.
func (c *Client) state(ctx context.Context, id int64) (string, error) {
	var vms []struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := c.Call(ctx, "vm.query", &vms, [][]any{{"id", "=", id}}); err != nil {
		return "", err
	}
	if len(vms) == 0 {
		return "", nil
	}
	return vms[0].Status.State, nil
}

// Stop powers the VM off in any state but STOPPED, as TrueNAS refuses to delete a running VM's
// device: a RUNNING VM by vm.stop (a clean shutdown, forced after the VM's timeout), any other
// (SUSPENDED, …) by vm.poweroff, as vm.stop shuts down only a RUNNING VM. It then checks the VM is
// STOPPED or gone, failing naming the state it is in otherwise, whether the call failed or not (a VM
// that stopped or went meanwhile fails the call, and is no failure). A VM stopped or gone is left
// as it is.
func (c *Client) Stop(ctx context.Context, id int64) error {
	s, err := c.state(ctx, id)
	if err != nil || s == "" || s == "STOPPED" {
		return err
	}
	method := "vm.poweroff"
	if s == "RUNNING" {
		method = "vm.stop"
		err = c.Job(ctx, method, id, map[string]any{"force_after_timeout": true})
	} else {
		err = c.Call(ctx, method, nil, id)
	}
	switch s, serr := c.state(ctx, id); {
	case serr != nil && err != nil:
		return err
	case serr != nil:
		return serr
	case s == "" || s == "STOPPED":
		return nil // stopped or gone, by this call or meanwhile (a VM that stops itself fails the call)
	case err != nil:
		return fmt.Errorf("VM %d is %s after %s failed (%w): its devices cannot be deleted yet; stop it on TrueNAS, then run this again", id, s, method, err)
	}
	return fmt.Errorf("VM %d is %s after %s, not STOPPED: its devices cannot be deleted yet; stop it on TrueNAS, then run this again", id, s, method)
}
