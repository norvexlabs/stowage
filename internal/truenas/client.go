// Package truenas drives what OpenTofu's TrueNAS provider cannot declare about a VM — the bytes
// of its disk and seed, its power, the snapshots that block a delete — through TrueNAS's own
// interfaces: the JSON-RPC 2.0 API over a WebSocket (/api/current) and the /_upload/ endpoint
// that feeds filesystem.put.
package truenas

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// The environment the provider reads too: the API's address, its key, and whether its
// certificate (a TrueNAS host's own, self-signed) goes unchecked.
const (
	URLVar      = "TRUENAS_URL"
	KeyVar      = "TRUENAS_API_KEY"
	InsecureVar = "TRUENAS_INSECURE_SKIP_VERIFY"
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

// FromEnv dials the host getenv names (URLVar, KeyVar, InsecureVar).
func FromEnv(ctx context.Context, getenv func(string) string) (*Client, error) {
	for _, k := range []string{URLVar, KeyVar} {
		if getenv(k) == "" {
			return nil, fmt.Errorf("%s is not set", k)
		}
	}
	return Dial(ctx, getenv(URLVar), getenv(KeyVar), Insecure(getenv(InsecureVar)))
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

// Dial connects to the API at base (https://host[:port]) and logs in with the API key.
func Dial(ctx context.Context, base, key string, insecure bool) (*Client, error) {
	u, err := ParseURL(base)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // the operator's choice, as the provider's
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
	for {
		var jobs []struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		if err := c.Call(ctx, "core.get_jobs", &jobs, [][]any{{"id", "=", id}}); err != nil {
			return err
		}
		if len(jobs) == 0 {
			return fmt.Errorf("%s: job %d vanished", what, id)
		}
		switch jobs[0].State {
		case "SUCCESS":
			return nil
		case "FAILED", "ABORTED":
			return fmt.Errorf("%s: job %d %s: %s", what, id, jobs[0].State, strings.TrimSpace(jobs[0].Error))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Upload writes the local file to remote on the host (filesystem.put through /_upload/, 0644),
// streamed: a disk image does not fit in memory.
func (c *Client) Upload(ctx context.Context, local, remote string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			data, err := json.Marshal(map[string]any{"method": "filesystem.put", "params": []any{remote, map[string]any{"mode": 0o644}}})
			if err != nil {
				return err
			}
			if err := mw.WriteField("data", string(data)); err != nil {
				return err
			}
			w, err := mw.CreateFormFile("file", filepath.Base(local))
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	u := *c.base
	u.Path = "/_upload/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), pr)
	if err != nil {
		pr.Close()
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("upload of %s: %w", filepath.Base(local), err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload of %s refused: HTTP %d %s", filepath.Base(local), resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)])))
	}
	var job struct {
		ID *int64 `json:"job_id"`
	}
	if err := json.Unmarshal(body, &job); err != nil || job.ID == nil {
		return fmt.Errorf("upload of %s: no job in the answer %q", filepath.Base(local), body[:min(len(body), 300)])
	}
	// The WebSocket sat unread through the upload (minutes for a disk), which the server may have
	// closed: the job is waited for on a new one.
	if err := c.connect(ctx); err != nil {
		return err
	}
	return c.Wait(ctx, *job.ID, "upload "+remote)
}

// size is the size of the file at remote on the host; -1 when there is none.
func (c *Client) size(ctx context.Context, remote string) (int64, error) {
	var st struct {
		Size int64 `json:"size"`
	}
	err := c.Call(ctx, "filesystem.stat", &st, remote)
	var e *Error
	if errors.As(err, &e) && e.Errname == "ENOENT" {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return st.Size, nil
}

// ImportImage writes the disk image at local — checked against its SHA-256 want — into the zvol
// (a dataset path, pool/…): uploaded into staging (a dataset's mount point), unless a file of its
// size already is there under its digest's name, then converted (vm.device.convert, which reads
// the format off the file's extension).
func (c *Client) ImportImage(ctx context.Context, local, want, staging, zvol string) error {
	got, err := SHA256File(local)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s: sha256 %s, expected %s: refusing to build a disk from it", local, got, want)
	}
	st, err := os.Stat(local)
	if err != nil {
		return err
	}
	remote := path.Join(staging, want[:16]+"-"+filepath.Base(local))
	have, err := c.size(ctx, remote)
	if err != nil {
		return err
	}
	if have != st.Size() {
		if err := c.Upload(ctx, local, remote); err != nil {
			return err
		}
	}
	return c.Job(ctx, "vm.device.convert", map[string]any{"source": remote, "destination": zvol})
}

// SHA256File is the hex SHA-256 of the file at p.
func SHA256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

// Start starts the VM unless it runs.
func (c *Client) Start(ctx context.Context, id int64) error {
	s, err := c.state(ctx, id)
	if err != nil {
		return err
	}
	switch s {
	case "":
		return fmt.Errorf("VM %d does not exist", id)
	case "RUNNING":
		return nil
	}
	return c.Call(ctx, "vm.start", nil, id)
}

// Stop powers the VM off when it runs — a clean shutdown, forced after the VM's timeout — as
// TrueNAS refuses to delete a running VM's device.
func (c *Client) Stop(ctx context.Context, id int64) error {
	s, err := c.state(ctx, id)
	if err != nil || s != "RUNNING" {
		return err
	}
	return c.Job(ctx, "vm.stop", id, map[string]any{"force_after_timeout": true})
}

// DropSnapshots deletes every snapshot of dataset, which TrueNAS's non-recursive delete of the
// dataset (the provider's) refuses otherwise.
func (c *Client) DropSnapshots(ctx context.Context, dataset string) error {
	var snaps []struct {
		ID string `json:"id"`
	}
	if err := c.Call(ctx, "pool.snapshot.query", &snaps, [][]any{{"dataset", "=", dataset}}, map[string]any{"select": []string{"id"}}); err != nil {
		return err
	}
	for _, s := range snaps {
		if err := c.Call(ctx, "pool.snapshot.delete", nil, s.ID); err != nil {
			return err
		}
	}
	return nil
}
