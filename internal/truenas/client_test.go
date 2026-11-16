package truenas

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func init() {
	poll, settleTimeout, lostGrace = time.Millisecond, 50*time.Millisecond, 20*time.Millisecond
}

// fake is a TrueNAS API: its methods answer from handlers, every call is recorded, and an upload
// lands in files.
type fake struct {
	mu      sync.Mutex
	key     string
	calls   []string
	methods map[string]func(params []json.RawMessage) (any, *rpcErr)
	files   map[string][]byte
	jobs    map[int64]string // job id → its state, final unless a test changes it
	jobArgs map[int64]string // job id → its method and arguments as core.get_jobs gives them, JSON
	polled  func(id int64)   // run as a job's state is read, f.mu held
	drop    string           // a method whose next call is run, then its connection closed unanswered
	noEmpty bool             // an upload of an empty file is refused
	next    int64
	notify  bool             // send a notification before each answer
	running []map[string]any // the RUNNING or WAITING imports' jobs core.get_jobs gives
	upload  func()           // run as an upload lands, f.mu held
	early   bool             // an upload is answered with its job before its file is read
	release chan struct{}    // closed when an early upload's connection may close
	lookErr bool             // core.get_jobs of every convert, or every put, fails
	looked  func()           // run as convertJob's query is served, f.mu held
	refuse  int              // an upload is answered with this status, unread, when set
	readAll bool             // a refused upload is read whole before its answer
	login   func()           // run as auth.login_with_api_key is served, f.mu held
	cut     string           // the next upload's connection is closed unanswered: "early", unread; "late", its file landed and a filesystem.put job of it made
}

type rpcErr struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{key: "k3y", methods: map[string]func([]json.RawMessage) (any, *rpcErr){}, files: map[string][]byte{}, jobs: map[int64]string{}, jobArgs: map[int64]string{}}
	f.methods["auth.login_with_api_key"] = func(p []json.RawMessage) (any, *rpcErr) {
		var k string
		_ = json.Unmarshal(p[0], &k)
		if f.login != nil {
			f.login()
		}
		return k == f.key, nil
	}
	f.methods["core.get_jobs"] = func(p []json.RawMessage) (any, *rpcErr) {
		// Every read selects the fields jobRow decodes, as TrueNAS 25.10 takes the option.
		if len(p) != 2 || string(p[1]) != `{"select":["id","method","state","arguments","error"]}` {
			return nil, &rpcErr{Code: 22, Message: "bad options " + strings.Join(raw(p), " ")}
		}
		// busy's query.
		if string(p[0]) == `[["method","in",["vm.device.convert","filesystem.put"]],["state","in",["RUNNING","WAITING"]]]` {
			return f.running, nil
		}
		// convertJob's and putJob's: every job of a method, any state.
		if m := strings.TrimSuffix(strings.TrimPrefix(string(p[0]), `[["method","=","`), `"]]`); m != string(p[0]) {
			if m == "vm.device.convert" && f.looked != nil {
				f.looked()
			}
			if f.lookErr {
				return nil, &rpcErr{Code: 22, Message: "jobs unread"}
			}
			var out []map[string]any
			for id, a := range f.jobArgs {
				j := map[string]any{"id": id, "state": f.jobs[id]}
				_ = json.Unmarshal([]byte(a), &j)
				if j["method"] == m {
					out = append(out, j)
				}
			}
			return out, nil
		}
		var filter [][]any
		_ = json.Unmarshal(p[0], &filter)
		id := int64(filter[0][2].(float64))
		if f.polled != nil {
			f.polled(id)
		}
		st, ok := f.jobs[id]
		if !ok {
			return []any{}, nil
		}
		j := map[string]any{"state": st, "error": "boom"}
		if a := f.jobArgs[id]; a != "" {
			_ = json.Unmarshal([]byte(a), &j)
		}
		return []map[string]any{j}, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/current", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID     int64             `json:"id"`
				Method string            `json:"method"`
				Params []json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				return
			}
			f.mu.Lock()
			f.calls = append(f.calls, req.Method+" "+strings.Join(raw(req.Params), " "))
			h := f.methods[req.Method]
			var res any
			var rerr *rpcErr
			if h == nil {
				rerr = &rpcErr{Code: -32601, Message: "Method does not exist"}
			} else {
				res, rerr = h(req.Params)
			}
			notify, drop := f.notify, f.drop == req.Method
			if drop {
				f.drop = ""
			}
			f.mu.Unlock()
			if drop {
				return
			}
			if notify {
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","method":"collection_update","params":{}}`))
				// An answer to a call this client did not make.
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":-1,"result":"stale"}`))
			}
			out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
			if rerr != nil {
				out["error"] = rerr
			} else {
				out["result"] = res
			}
			b, _ := json.Marshal(out)
			if err := c.Write(ctx, websocket.MessageText, b); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/_upload/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.key {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		early, refuse, readAll, cut := f.early, f.refuse, f.readAll, f.cut
		f.cut = ""
		f.mu.Unlock()
		hangUp := func() {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				conn.Close()
			}
		}
		if cut == "early" {
			hangUp()
			return
		}
		if refuse != 0 {
			if readAll {
				_, _ = io.Copy(io.Discard, r.Body)
			}
			http.Error(w, "refused", refuse)
			return
		}
		if early { // answered at once, the file left unread until the client is done
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 12\r\nConnection: close\r\n\r\n{\"job_id\":0}")
			_ = rw.Flush()
			<-f.release
			return
		}
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var dest string
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			b, err := io.ReadAll(p)
			if err != nil { // a body cut short: no file written
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			switch p.FormName() {
			case "data":
				var d struct {
					Method string `json:"method"`
					Params []any  `json:"params"`
				}
				_ = json.Unmarshal(b, &d)
				if d.Method != "filesystem.put" || d.Params[1].(map[string]any)["mode"].(float64) != 0o600 {
					http.Error(w, "bad data", http.StatusBadRequest)
					return
				}
				dest = d.Params[0].(string)
			case "file":
				f.mu.Lock()
				if len(b) == 0 && f.noEmpty {
					f.mu.Unlock()
					http.Error(w, "no space left", http.StatusInternalServerError)
					return
				}
				f.files[dest] = b
				f.calls = append(f.calls, "upload "+dest)
				if f.upload != nil {
					f.upload()
				}
				f.mu.Unlock()
			}
		}
		if cut == "late" {
			f.mu.Lock()
			p, _ := json.Marshal(dest)
			f.start("filesystem.put", []json.RawMessage{p}, "FAILED")
			f.mu.Unlock()
			hangUp()
			return
		}
		f.mu.Lock()
		f.next++
		id := f.next
		f.jobs[id] = "SUCCESS"
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": id})
	})
	s := httptest.NewTLSServer(mux)
	t.Cleanup(s.Close)
	return f, s
}

func raw(ps []json.RawMessage) []string {
	var out []string
	for _, p := range ps {
		out = append(out, string(p))
	}
	return out
}

// job registers method as a job ending in state.
func (f *fake) job(method, state string) {
	f.methods[method] = func(p []json.RawMessage) (any, *rpcErr) {
		return f.start(method, p, state), nil
	}
}

// start makes a job of method called with p, in state, f.mu held, and gives its id.
func (f *fake) start(method string, p []json.RawMessage, state string) int64 {
	f.next++
	f.jobs[f.next] = state
	b, _ := json.Marshal(map[string]any{"method": method, "arguments": p})
	f.jobArgs[f.next] = string(b)
	return f.next
}

func (f *fake) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func dial(t *testing.T, s *httptest.Server, key string) (*Client, error) {
	c, err := Dial(context.Background(), s.URL, key, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the fake's own certificate
	if err == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

func TestDialLogsIn(t *testing.T) {
	t.Parallel()
	_, s := newFake(t)
	if _, err := dial(t, s, "k3y"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseURL(s.URL + "/"); err != nil { // a trailing / is the root too
		t.Fatal(err)
	}
	if _, err := dial(t, s, "wrong"); err == nil || !strings.Contains(err.Error(), "refused the API key") {
		t.Fatalf("a wrong key: %v", err)
	}
	// Its own certificate, unchecked only when asked.
	if _, err := Dial(context.Background(), s.URL, "k3y", &tls.Config{}); err == nil {
		t.Fatal("an unknown certificate was taken")
	}
	// HTTPS only: the key never travels in clear.
	for _, u := range []string{"http://" + strings.TrimPrefix(s.URL, "https://"), "ftp://h", "https://", "host:443",
		"https://:443", "https://nas/truenas", "https://u:pw@nas", "https://nas/?a=b", "https://nas/#x"} {
		if _, err := Dial(context.Background(), u, "k", &tls.Config{}); err == nil || !strings.Contains(err.Error(), URLVar) || strings.Contains(err.Error(), ":pw@") {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestFromEnv(t *testing.T) {
	t.Parallel()
	_, s := newFake(t)
	ep := "wss://" + strings.TrimPrefix(s.URL, "https://") + "/api/current"
	for _, env := range []map[string]string{
		{URLVar: s.URL, KeyVar: "k3y", InsecureVar: "true"},
		{EndpointVar: ep, KeyVar: "k3y", InsecureVar: "true"},
		{EndpointVar: ep, URLVar: s.URL, KeyVar: "k3y", InsecureVar: "true"}, // the same host
	} {
		c, err := FromEnv(context.Background(), func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("%v: %v", env, err)
		}
		c.Close()
	}
	for k, want := range map[string]string{URLVar: URLVar, KeyVar: KeyVar} {
		e := map[string]string{URLVar: s.URL, KeyVar: "k3y", InsecureVar: "true"}
		delete(e, k)
		if _, err := FromEnv(context.Background(), func(k string) string { return e[k] }); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("without %s: %v", k, err)
		}
	}
	// One host, its default port written or not, in either variable: taken.
	for ep, u := range map[string]string{"wss://nas.example.org/api/current": "https://NAS.example.org:443", "wss://nas.example.org:443/websocket": "https://nas.example.org/"} {
		e := map[string]string{EndpointVar: ep, URLVar: u}
		if b, err := BaseFromEnv(func(k string) string { return e[k] }); err != nil || b.Host != strings.TrimPrefix(strings.TrimSuffix(strings.TrimSuffix(ep, "/api/current"), "/websocket"), "wss://") {
			t.Errorf("%s and %s: %v %v", ep, u, b, err)
		}
	}
	for ep, u := range map[string]string{"wss://nas.example.org/api/current": "https://nas.example.org:8443", "wss://nas.example.org:8443/api/current": "https://nas.example.org"} {
		e := map[string]string{EndpointVar: ep, URLVar: u}
		if _, err := BaseFromEnv(func(k string) string { return e[k] }); err == nil || !strings.Contains(err.Error(), "set one, or both to the same host") {
			t.Errorf("%s and %s, another port: %v", ep, u, err)
		}
	}
	// Two hosts: neither is taken.
	e := map[string]string{EndpointVar: "wss://nas.example.org/api/current", URLVar: s.URL, KeyVar: "k3y"}
	if _, err := FromEnv(context.Background(), func(k string) string { return e[k] }); err == nil || !strings.Contains(err.Error(), "set one, or both to the same host") {
		t.Errorf("two hosts: %v", err)
	}
}

func TestParseEndpoint(t *testing.T) {
	t.Parallel()
	for ep, want := range map[string]string{
		"wss://nas.example.org/api/current":      "https://nas.example.org",
		"wss://nas.example.org:8443/api/current": "https://nas.example.org:8443",
		"wss://NAS.example.org/websocket":        "https://NAS.example.org",
	} {
		if u, err := ParseEndpoint(ep); err != nil || u.String() != want {
			t.Errorf("%s: %v %v", ep, u, err)
		}
	}
	// wss only (the key never travels in clear), the API's path alone.
	for _, ep := range []string{"ws://nas/api/current", "https://nas/api/current", "wss://nas", "wss://nas/", "wss://nas/api/v25.04",
		"wss://u:pw@nas/api/current", "wss:///api/current", "wss://nas/api/current?a=b", "wss://nas/api/current#x"} {
		if _, err := ParseEndpoint(ep); err == nil || !strings.Contains(err.Error(), EndpointVar) || strings.Contains(err.Error(), ":pw@") {
			t.Errorf("%s: %v", ep, err)
		}
	}
}

func TestTLSFromEnv(t *testing.T) {
	t.Parallel()
	_, s := newFake(t)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The CA's file: the fake's certificate checked, and taken.
	env := map[string]string{URLVar: s.URL, KeyVar: "k3y", CACertVar: ca}
	c, err := FromEnv(context.Background(), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	for _, e := range []map[string]string{
		{CACertVar: ca, InsecureVar: "true"},
		{CACertVar: junk},
		{CACertVar: filepath.Join(dir, "gone.pem")},
	} {
		if _, err := TLSFromEnv(func(k string) string { return e[k] }); err == nil || !strings.Contains(err.Error(), CACertVar) {
			t.Errorf("%v: %v", e, err)
		}
	}
	if tc, err := TLSFromEnv(func(string) string { return "" }); err != nil || tc.InsecureSkipVerify || tc.RootCAs != nil {
		t.Errorf("nothing set: %+v %v", tc, err)
	}
}

func TestInsecure(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"1": true, "true": true, "TRUE": true, "yes": true, "": false, "0": false, "false": false, "no": false} {
		if Insecure(v) != want {
			t.Errorf("Insecure(%q) = %v", v, !want)
		}
	}
}

func TestCallSkipsNotificationsAndReportsErrors(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	f.notify = true
	f.methods["x.fail"] = func([]json.RawMessage) (any, *rpcErr) {
		return nil, &rpcErr{Code: -32001, Message: "Method call error", Data: map[string]any{"reason": "the pool is gone"}}
	}
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	err = c.Call(context.Background(), "x.fail", nil)
	if err == nil || !strings.Contains(err.Error(), "the pool is gone") || !strings.Contains(err.Error(), "x.fail") {
		t.Fatalf("got %v", err)
	}
	f.methods["x.ok"] = func([]json.RawMessage) (any, *rpcErr) { return "mine", nil }
	var got string
	if err := c.Call(context.Background(), "x.ok", &got); err != nil || got != "mine" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestJob(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	f.job("ok.job", "SUCCESS")
	f.job("bad.job", "FAILED")
	f.methods["gone.job"] = func([]json.RawMessage) (any, *rpcErr) { return 999, nil }
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Job(ctx, "ok.job"); err != nil {
		t.Fatal(err)
	}
	if err := c.Job(ctx, "bad.job"); err == nil || !strings.Contains(err.Error(), "FAILED: boom") {
		t.Fatalf("a failed job: %v", err)
	}
	if err := c.Job(ctx, "gone.job"); err == nil || !strings.Contains(err.Error(), "vanished") {
		t.Fatalf("a vanished job: %v", err)
	}
	f.methods["run.job"] = func([]json.RawMessage) (any, *rpcErr) {
		f.next++
		f.jobs[f.next] = "RUNNING"
		return f.next, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := c.Job(cctx, "run.job"); err == nil {
		t.Fatal("a running job waited out a cancelled context")
	}
}

func image(t *testing.T, body string) (string, string) {
	p := filepath.Join(t.TempDir(), "notes-1.0.0.qcow2")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(body))
	return p, hex.EncodeToString(h[:])
}

// zvolHash is the part of a staged file's name the zvol gives: its SHA-256's 8 first hex digits.
func zvolHash(zvol string) string {
	h := sha256.Sum256([]byte(zvol))
	return hex.EncodeToString(h[:4])
}

// uploads is the paths uploaded to since call n, in order.
func (f *fake) uploads(n int) []string {
	var out []string
	for _, l := range f.called()[n:] {
		if p, ok := strings.CutPrefix(l, "upload "); ok {
			out = append(out, p)
		}
	}
	return out
}

func TestImportImage(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	z := f.zvols()
	// The zvol's mark and the staged file as each convert starts.
	during, source := map[string]string{}, map[string]string{}
	f.methods["vm.device.convert"] = func(p []json.RawMessage) (any, *rpcErr) {
		var a struct{ Source, Destination string }
		_ = json.Unmarshal(p[0], &a)
		during[a.Destination], source[a.Destination] = z.marks[a.Destination], a.Source+" "+string(f.files[a.Source])
		return f.start("vm.device.convert", p, "SUCCESS"), nil
	}
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	local, sum := image(t, "QFI\xfbdisk")
	if held, err := c.ImportImage(ctx, local, sum, "/mnt/tank/vms/x-staging", "tank/vms/x", io.Discard); err != nil || held {
		t.Fatal(held, err)
	}
	// Staged under this run's name (its lock's nonce), converted from there, then emptied.
	run := z.lastRun("tank/vms/x-stowage-ref")
	remote := "/mnt/tank/vms/x-staging/" + sum[:16] + "-" + zvolHash("tank/vms/x") + "-" + run + "-notes-1.0.0.qcow2"
	if !sha256Hex(run+run+run+run) || source["tank/vms/x"] != remote+" QFI\xfbdisk" {
		t.Fatalf("run %q converted %q", run, source["tank/vms/x"])
	}
	if got, ok := f.files[remote]; !ok || len(got) != 0 {
		t.Fatalf("the staged file %q, not emptied", got)
	}
	if got := z.mark("tank/vms/x"); got != "sha256:"+sum || during["tank/vms/x"] != "importing:sha256:"+sum {
		t.Fatalf("the zvol's mark %q, %q while written", got, during["tank/vms/x"])
	}
	if z.has("tank/vms/x-stowage-ref") {
		t.Fatal("the lock left")
	}
	// Each upload's job is waited for on a new connection (the image's, the empty file's), and
	// each clean-up step (the emptying, the unlock) runs on one of its own: logged in again.
	logins := 0
	for _, l := range f.called() {
		if strings.HasPrefix(l, "auth.login_with_api_key") {
			logins++
		}
	}
	if logins != 5 {
		t.Errorf("%d logins, want 5", logins)
	}
	converts := func() []string {
		var out []string
		for _, l := range f.called() {
			if strings.HasPrefix(l, "vm.device.convert") {
				out = append(out, l)
			}
		}
		return out
	}
	if cv := converts(); len(cv) != 1 || !strings.Contains(cv[0], `"destination":"tank/vms/x"`) {
		t.Fatalf("converts %q", cv)
	}
	// Run again (a provisioner tainted, a device made since): the zvol holds the image, left as it
	// is, no lock taken.
	z.device("tank/vms/x")
	n := len(f.called())
	if held, err := c.ImportImage(ctx, local, sum, "/mnt/tank/vms/x-staging", "tank/vms/x", io.Discard); err != nil || !held {
		t.Fatal(held, err)
	}
	if len(converts()) != 1 || len(f.called()) != n+1 { // its query alone
		t.Fatalf("a held image was written again: %q", f.called()[n:])
	}
	// Held, the image is not read: gone from here, the zvol is still left as it is.
	if held, err := c.ImportImage(ctx, filepath.Join(t.TempDir(), "gone.qcow2"), sum, "/mnt/tank/vms/x-staging", "tank/vms/x", io.Discard); err != nil || !held {
		t.Fatal("a held image read again:", held, err)
	}
	// Not held, the image missing: refused before the lock is made, nothing staged.
	z.set("tank/vms/ni", freshLogical, "")
	n = len(f.called())
	creates := len(z.creates)
	gone := filepath.Join(t.TempDir(), "gone.qcow2")
	if _, err := c.ImportImage(ctx, gone, sum, "/s", "tank/vms/ni", io.Discard); err == nil || err.Error() != "open "+gone+": no such file or directory" {
		t.Errorf("a missing image: %v", err)
	}
	if len(z.creates) != creates || len(f.uploads(n)) != 0 || z.has("tank/vms/ni-stowage-ref") {
		t.Errorf("a missing image: a lock made or a file staged: %q", f.called()[n:])
	}
	// An image that fails its first read (a directory): the upload sent no byte of it, so nothing
	// is emptied; the lock deleted.
	n = len(f.called())
	if _, err := c.ImportImage(ctx, t.TempDir(), sum, "/s", "tank/vms/ni", io.Discard); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("an image unread: %v", err)
	}
	if len(f.uploads(n)) != 0 || z.has("tank/vms/ni-stowage-ref") {
		t.Errorf("an image unread: a file staged or emptied, or the lock left: %q", f.called()[n:])
	}
	// An earlier run cut off while writing it: uploaded and written again, under a name of this
	// run's own, never the earlier run's.
	z.set("tank/vms/y", 40<<20, "importing:sha256:"+sum)
	if held, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/y", io.Discard); err != nil || held {
		t.Fatal(held, err)
	}
	again := z.lastRun("tank/vms/y-stowage-ref")
	if len(converts()) != 2 || z.mark("tank/vms/y") != "sha256:"+sum || again == run || source["tank/vms/y"] != "/s/"+sum[:16]+"-"+zvolHash("tank/vms/y")+"-"+again+"-notes-1.0.0.qcow2 QFI\xfbdisk" {
		t.Fatalf("not written again under its own name: %q, mark %q, %q", converts(), z.mark("tank/vms/y"), source["tank/vms/y"])
	}
	// Refused, nothing written: another image, data of its own (one byte past a zvol never
	// written; an empty ext4 on 1 GiB as TrueNAS measured it, 387376 bytes referenced once
	// compressed), a VM's disk, no zvol, a dataset, a zvol read without its logicalreferenced, its
	// volblocksize or its user properties.
	z.set("tank/vms/other", freshLogical, "sha256:"+strings.Repeat("a", 64))
	z.set("tank/vms/other-partial", freshLogical, "importing:sha256:"+strings.Repeat("a", 64))
	z.set("tank/vms/data", freshLogical+1, "")
	z.set("tank/vms/ext4", 1028096, "")
	z.refs["tank/vms/ext4"] = 387376
	z.set("tank/vms/empty", freshLogical, "")
	z.set("tank/vms/used", freshLogical, "")
	z.device("tank/vms/used")
	z.kind("tank/vms/fs", "FILESYSTEM")
	z.set("tank/vms/noref", freshLogical, "")
	z.noRef["tank/vms/noref"] = true
	z.set("tank/vms/noprops", freshLogical, "sha256:"+strings.Repeat("a", 64))
	z.noProps["tank/vms/noprops"] = true
	z.set("tank/vms/novbs", freshLogical, "")
	z.vbs["tank/vms/novbs"] = ""
	for zvol, want := range map[string]string{
		"tank/vms/novbs":         "no volblocksize",
		"tank/vms/noref":         "no logicalreferenced",
		"tank/vms/noprops":       "no user_properties",
		"tank/vms/other":         "holds another image",
		"tank/vms/other-partial": "holds another image",
		"tank/vms/data":          "holds data (logicalreferenced 28673 bytes, a zvol never written 28672)",
		"tank/vms/ext4":          "holds data (logicalreferenced 1028096 bytes",
		"tank/vms/used":          "is the disk of VM 7",
		"tank/vms/gone":          "does not exist",
		"tank/vms/fs":            "not a zvol",
	} {
		n := len(f.called())
		if _, err := c.ImportImage(ctx, local, sum, "/s", zvol, io.Discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", zvol, err)
		}
		if len(f.uploads(n)) != 0 || z.mark(zvol) != z.initial[zvol] {
			t.Errorf("%s: uploaded or written", zvol)
		}
		if z.has(zvol + "-stowage-ref") {
			t.Errorf("%s: its lock left", zvol)
		}
	}
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/empty", io.Discard); err != nil {
		t.Errorf("an empty zvol at the bound: %v", err)
	}
	// A file that is not the image named: refused once uploaded (the one read of it, hashed as it
	// is sent), the staged file emptied, the lock deleted, nothing marked or converted; a digest
	// that is none, before the host is asked.
	z.set("tank/vms/z", freshLogical, "")
	n = len(f.called())
	zeros := strings.Repeat("0", 64)
	if _, err := c.ImportImage(ctx, local, zeros, "/s", "tank/vms/z", io.Discard); err == nil || !strings.Contains(err.Error(), local+": sha256 "+sum+", expected "+zeros+": refusing to build a disk from it") {
		t.Fatalf("a wrong digest: %v", err)
	}
	if up := f.uploads(n); len(up) != 2 || up[0] != up[1] || f.staged(up[0]) != "" || z.has("tank/vms/z-stowage-ref") || z.mark("tank/vms/z") != "" || len(converts()) != 3 {
		t.Errorf("a wrong digest: staged %q, or locked, marked or converted", up)
	}
	n = len(f.called())
	for _, bad := range []string{strings.ToUpper(sum), sum[:16], ""} {
		if _, err := c.ImportImage(ctx, local, bad, "/s", "tank/vms/z", io.Discard); err == nil || !strings.Contains(err.Error(), "64 lower-case hex digits") {
			t.Errorf("digest %q: %v", bad, err)
		}
	}
	if len(f.called()) != n {
		t.Error("a digest that is none reached the host")
	}
}

// The bound of an empty zvol is measured on the host each run, on the lock (a reference zvol made
// beside the zvol and deleted), never taken as a constant.
func TestImportImageEmptyBound(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	z := f.zvols()
	f.job("vm.device.convert", "SUCCESS")
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	local, sum := image(t, "QFI\xfbdisk")
	// This host's zvol never written reads 45056: past 28672 is empty still, past 45056 not.
	z.empty = 45056
	z.set("tank/vms/at", 45056, "")
	z.vbs["tank/vms/at"] = "64K"
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/at", io.Discard); err != nil {
		t.Fatalf("a zvol at the host's bound: %v", err)
	}
	want := `{"name":"tank/vms/at-stowage-ref","sparse":true,"type":"VOLUME","user_properties":[{"key":"org.stowage:ref","value":"tank/vms/at"},{"key":"org.stowage:run","value":"` + z.lastRun("tank/vms/at-stowage-ref") + `"}],"volblocksize":"64K","volsize":1048576}`
	if len(z.creates) != 1 || z.creates[0] != want {
		t.Errorf("the lock made %q", z.creates)
	}
	z.set("tank/vms/past", 45057, "")
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/past", io.Discard); err == nil || !strings.Contains(err.Error(), "holds data (logicalreferenced 45057 bytes, a zvol never written 45056)") {
		t.Errorf("a zvol past the host's bound: %v", err)
	}
	for _, zvol := range []string{"tank/vms/at", "tank/vms/past"} {
		if z.has(zvol + "-stowage-ref") {
			t.Errorf("%s: its lock left", zvol)
		}
	}
	if z.mark("tank/vms/past") != "" {
		t.Error("a zvol past the bound marked")
	}
	z.empty = freshLogical

	// Cancelled while the lock is read, which closes the connection: deleted all the same, on a
	// connection of its own.
	z.set("tank/vms/cut", freshLogical, "")
	cctx, cancel := context.WithCancel(ctx)
	z.queried = func(name string) {
		if _, made := z.types[name]; made && name == "tank/vms/cut-stowage-ref" {
			cancel()
			time.Sleep(10 * time.Millisecond) // the answer comes after the call gave up
		}
	}
	if _, err := c.ImportImage(cctx, local, sum, "/s", "tank/vms/cut", io.Discard); err == nil {
		t.Error("a cancelled run went on")
	}
	z.queried = nil
	if z.has("tank/vms/cut-stowage-ref") || z.mark("tank/vms/cut") != "" {
		t.Error("a cancelled run left its lock or marked the zvol")
	}
	// The lock not deleted once the zvol is marked written: a warning naming the lock to delete,
	// the step a success (on a client of its own: a cancelled call closes the connection).
	if c, err = dial(t, s, "k3y"); err != nil {
		t.Fatal(err)
	}
	fix := `delete the zvol tank/vms/nd-stowage-ref on TrueNAS (Datasets, or pool.dataset.delete with {"recursive": true}), then run this again`
	z.set("tank/vms/nd", freshLogical, "")
	z.delErr = true
	var warn strings.Builder
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/nd", &warn); err != nil || !strings.HasPrefix(warn.String(), "warning: the zvol tank/vms/nd holds the image sha256:"+sum+"; the lock tank/vms/nd-stowage-ref not deleted (") || !strings.Contains(warn.String(), fix) {
		t.Errorf("a lock not deleted after the mark: %v, warned %q", err, warn.String())
	}
	if !z.has("tank/vms/nd-stowage-ref") || z.mark("tank/vms/nd") != "sha256:"+sum {
		t.Error("after the mark: the lock deleted, or the zvol not marked")
	}
	// Before the mark (the convert failed): an error, both named.
	f.job("vm.device.convert", "FAILED")
	z.set("tank/vms/nb", freshLogical, "")
	warn.Reset()
	var lockLeft *LockLeftError
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/nb", &warn); err == nil || !strings.Contains(err.Error(), "FAILED") || !strings.Contains(err.Error(), "the lock tank/vms/nb-stowage-ref not deleted (") || !errors.As(err, &lockLeft) || warn.Len() != 0 {
		t.Errorf("a lock not deleted before the mark: %v, warned %q", err, warn.String())
	}
	z.delErr = false
}

// One run at a time writes into a zvol: the lock, a dataset TrueNAS makes once, refuses every other
// run at any age and is deleted by its own run alone.
func TestImportImageLock(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	z := f.zvols()
	f.job("vm.device.convert", "SUCCESS")
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	local, sum := image(t, "QFI\xfbdisk")
	now := time.Now().Unix()
	// lock leaves a lock of zvol held by run feed, made age ago, with a snapshot (a periodic
	// task's).
	lock := func(zvol string, age time.Duration) string {
		ref := zvol + "-stowage-ref"
		z.set(zvol, freshLogical, "")
		z.set(ref, freshLogical, "")
		z.f.mu.Lock()
		z.refOf[ref], z.runOf[ref], z.made[ref], z.snaps[ref] = zvol, "feed", now-int64(age/time.Second), true
		z.f.mu.Unlock()
		return ref
	}
	at := func(age time.Duration) string {
		return time.Unix(now-int64(age/time.Second), 0).UTC().Format(time.RFC3339)
	}
	// Another run's, young or old, or of a creation TrueNAS does not give: refused, naming the
	// fix, the lock kept, nothing uploaded or written.
	for zvol, age := range map[string]time.Duration{"tank/vms/h": time.Minute, "tank/vms/o": 30 * 24 * time.Hour, "tank/vms/nt": 0} {
		ref := lock(zvol, age)
		when := at(age)
		if age == 0 {
			z.noMade[ref] = true
			when = "at a time TrueNAS does not give"
		}
		want := "another import into " + zvol + " holds its lock " + ref + " (run feed, made " + when + "): once no `tofu apply` or `vm truenas import-image` of " + zvol +
			" runs on any machine, and TrueNAS runs no job of it (Jobs: a vm.device.convert into " + zvol +
			", or a filesystem.put or vm.device.convert of a file /s/" + sum[:16] + "-" + zvolHash(zvol) + "-<nonce>-…), delete the zvol " + ref +
			` on TrueNAS (Datasets, or pool.dataset.delete with {"recursive": true}), then run this again; while either runs, let it end`
		n := len(f.called())
		var lockLeft *LockLeftError
		if _, err := c.ImportImage(ctx, local, sum, "/s", zvol, io.Discard); err == nil || err.Error() != want || !errors.As(err, &lockLeft) {
			t.Errorf("%s: a lock held:\n%v\nwant\n%s", zvol, err, want)
		}
		if !z.has(ref) || len(f.uploads(n)) != 0 || z.mark(zvol) != "" || len(z.deletes) != 0 {
			t.Errorf("%s: a lock held: deleted, or the zvol uploaded or written", zvol)
		}
	}
	// Another run's, a job of the import running (an upload of its staged file): busy's refusal,
	// naming the job.
	ref := lock("tank/vms/j", time.Hour)
	staged := "/s/" + sum[:16] + "-" + zvolHash("tank/vms/j") + "-feed-notes-1.0.0.qcow2"
	f.mu.Lock()
	f.running = []map[string]any{{"id": 90, "method": "filesystem.put", "arguments": []any{staged, map[string]any{"mode": 384}}}}
	f.mu.Unlock()
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/j", io.Discard); err == nil || err.Error() != "another import into tank/vms/j is running: job 90 (filesystem.put of "+staged+"); let it end, then run this again" {
		t.Errorf("a lock held, a job running: %v", err)
	}
	f.mu.Lock()
	f.running = nil
	f.mu.Unlock()
	if !z.has(ref) || z.mark("tank/vms/j") != "" {
		t.Error("a lock held, a job running: deleted, or the zvol written")
	}
	// The create made the lock, its answer lost (the connection closed): read on a new connection,
	// this run's own, held; the zvol written, the lock deleted at the end.
	z.set("tank/vms/own", freshLogical, "")
	f.mu.Lock()
	f.drop = "pool.dataset.create"
	f.mu.Unlock()
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/own", io.Discard); err != nil || z.mark("tank/vms/own") != "sha256:"+sum || z.has("tank/vms/own-stowage-ref") {
		t.Errorf("a lock of this run's own: %v, mark %q", err, z.mark("tank/vms/own"))
	}
	// Its answer lost and the read after failing: the lock, this run's, deleted.
	z.set("tank/vms/rf", freshLogical, "")
	f.mu.Lock()
	f.drop, z.failQuery = "pool.dataset.create", "tank/vms/rf-stowage-ref"
	f.mu.Unlock()
	n := len(f.called())
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/rf", io.Discard); err == nil || !strings.Contains(err.Error(), "the lock tank/vms/rf-stowage-ref of imports into tank/vms/rf: ") || !strings.Contains(err.Error(), "query failed") {
		t.Errorf("a lock not read: %v", err)
	}
	if z.has("tank/vms/rf-stowage-ref") || len(f.uploads(n)) != 0 || z.mark("tank/vms/rf") != "" {
		t.Error("a lock not read: left, or the zvol uploaded or written")
	}
	// A dataset of the lock's name that is not one (no property, another zvol's, not a zvol):
	// refused, never deleted, the zvol not written.
	z.set("tank/vms/fa", freshLogical, "")
	z.set("tank/vms/fa-stowage-ref", 1<<20, "")
	z.set("tank/vms/fb", freshLogical, "")
	z.set("tank/vms/fb-stowage-ref", 1<<20, "")
	z.refOf["tank/vms/fb-stowage-ref"] = "tank/vms/other"
	z.set("tank/vms/fc", freshLogical, "")
	z.kind("tank/vms/fc-stowage-ref", "FILESYSTEM")
	z.refOf["tank/vms/fc-stowage-ref"] = "tank/vms/fc"
	for _, zvol := range []string{"tank/vms/fa", "tank/vms/fb", "tank/vms/fc"} {
		want := zvol + "-stowage-ref, the name of the lock of imports into " + zvol + ", is a dataset without org.stowage:ref=" + zvol + ": not Stowage's, refusing to delete it"
		if _, err := c.ImportImage(ctx, local, sum, "/s", zvol, io.Discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", zvol, err)
		}
		if !z.has(zvol+"-stowage-ref") || z.mark(zvol) != "" {
			t.Errorf("%s: its namesake deleted or the zvol written", zvol)
		}
	}
	// A create refused with no dataset of that name: its error.
	z.set("tank/vms/ce", freshLogical, "")
	z.createErr = &rpcErr{Code: -32001, Message: "Method call error", Data: map[string]any{"reason": "[EFAULT] out of space"}}
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/ce", io.Discard); err == nil || !strings.Contains(err.Error(), "the lock tank/vms/ce-stowage-ref of imports into tank/vms/ce: ") || !strings.Contains(err.Error(), "out of space") {
		t.Errorf("a create refused: %v", err)
	}
	z.createErr = nil
	// Another run wrote the image between the first read and the lock: held, nothing uploaded.
	z.set("tank/vms/w", freshLogical, "")
	z.created = func(name string) {
		if name == "tank/vms/w-stowage-ref" {
			z.marks["tank/vms/w"] = "sha256:" + sum
		}
	}
	n = len(f.called())
	if held, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/w", io.Discard); err != nil || !held || len(f.uploads(n)) != 0 || z.has("tank/vms/w-stowage-ref") {
		t.Errorf("written meanwhile: %v %v", held, err)
	}
	z.created = nil
}

// What changes while the lock is held, the upload under way, is refused before the zvol is marked,
// and the staged file emptied whatever the end.
func TestImportImageUnderLock(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	z := f.zvols()
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	local, sum := image(t, "QFI\xfbdisk")
	// emptied says the file the run staged exists and is empty.
	emptied := func(zvol string, n int) bool {
		up := f.uploads(n)
		return len(up) == 2 && up[0] == up[1] && strings.HasPrefix(up[0], "/s/"+sum[:16]+"-"+zvolHash(zvol)+"-"+z.lastRun(zvol+"-stowage-ref")+"-") && f.staged(up[0]) == "" && f.has(up[0])
	}
	for _, tc := range []struct {
		zvol, want string
		during     func(zvol string) // run as the upload lands, f.mu held
	}{
		// The file changed since it was hashed.
		{"tank/vms/t", "sha256 ", nil},
		// Another image's mark: refused as such.
		{"tank/vms/m", "the zvol tank/vms/m holds another image (org.stowage:image importing:sha256:" + strings.Repeat("b", 64) + ")", func(zvol string) {
			z.marks[zvol] = "importing:sha256:" + strings.Repeat("b", 64)
		}},
		{"tank/vms/m2", "changed during the upload (\"\", now \"importing:sha256:", func(zvol string) { z.marks[zvol] = "importing:sha256:" + sum }},
		{"tank/vms/dv", "is the disk of VM 7", func(zvol string) { z.used[zvol] = true }},
		{"tank/vms/dt", "holds data (logicalreferenced 28673 bytes", func(zvol string) { z.logical[zvol]++ }},
		{"tank/vms/tk", "no longer holds the lock tank/vms/tk-stowage-ref", func(zvol string) { z.runOf[zvol+"-stowage-ref"] = "beef" }},
	} {
		f.job("vm.device.convert", "SUCCESS")
		z.set(tc.zvol, freshLogical, "")
		if tc.during == nil {
			z.created = func(string) { _ = os.WriteFile(local, []byte("QFI\xfbdiXk"), 0o644) }
		}
		f.mu.Lock()
		f.upload = func() {
			if tc.during != nil {
				tc.during(tc.zvol)
			}
		}
		f.mu.Unlock()
		n := len(f.called())
		if _, err := c.ImportImage(ctx, local, sum, "/s", tc.zvol, io.Discard); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.zvol, err)
		}
		z.created = nil
		_ = os.WriteFile(local, []byte("QFI\xfbdisk"), 0o644)
		for _, l := range f.called()[n:] {
			if strings.HasPrefix(l, "vm.device.convert") || strings.HasPrefix(l, "pool.dataset.update") {
				t.Errorf("%s: marked or converted: %s", tc.zvol, l)
			}
		}
		if !emptied(tc.zvol, n) {
			t.Errorf("%s: the staged file not emptied: %q", tc.zvol, f.uploads(n))
		}
		// The lock deleted, unless taken over: another run's is left.
		if z.has(tc.zvol+"-stowage-ref") != (tc.zvol == "tank/vms/tk") {
			t.Errorf("%s: the lock left or another's deleted", tc.zvol)
		}
	}
	// A convert that fails: the staged file emptied all the same, the zvol left marked as begun.
	f.mu.Lock()
	f.upload = nil
	f.mu.Unlock()
	f.job("vm.device.convert", "FAILED")
	z.set("tank/vms/cf", freshLogical, "")
	n := len(f.called())
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/cf", io.Discard); err == nil || !strings.Contains(err.Error(), "FAILED") {
		t.Errorf("a convert failed: %v", err)
	}
	if !emptied("tank/vms/cf", n) || z.mark("tank/vms/cf") != "importing:sha256:"+sum || z.has("tank/vms/cf-stowage-ref") {
		t.Errorf("a convert failed: staged %q, mark %q", f.uploads(n), z.mark("tank/vms/cf"))
	}
}

// An import refuses while another writes into its zvol or a staged file of its image, before it
// uploads and again before it marks the zvol.
func TestImportImageBusy(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	z := f.zvols()
	f.job("vm.device.convert", "SUCCESS")
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	local, sum := image(t, "QFI\xfbdisk")
	staged := "/s/" + sum[:16] + "-" + zvolHash("tank/vms/c") + "-0123456789abcdef-notes-1.0.0.qcow2"
	convert := func(id int64, src, dest string) map[string]any {
		return map[string]any{"id": id, "method": "vm.device.convert", "arguments": []any{map[string]any{"source": src, "destination": dest}}}
	}
	put := func(id int64, path string) map[string]any {
		return map[string]any{"id": id, "method": "filesystem.put", "arguments": []any{path, map[string]any{"mode": 384}}}
	}
	// Jobs on another zvol from another image's file, on another file, or without arguments, and
	// an import of this image into another zvol (its upload, its convert): not this import's.
	other := "/s/" + sum[:16] + "-" + zvolHash("tank/vms/other") + "-0123456789abcdef-notes-1.0.0.qcow2"
	f.mu.Lock()
	f.running = []map[string]any{convert(70, "/s/"+strings.Repeat("a", 16)+"-"+zvolHash("tank/vms/b")+"-0123456789abcdef-notes-1.0.0.qcow2", "tank/vms/other"), put(71, "/s/other.qcow2"),
		put(73, "/t/"+sum[:16]+"-"+zvolHash("tank/vms/b")+"-0123456789abcdef-notes-1.0.0.qcow2"), {"id": 72, "method": "filesystem.put", "arguments": []any{}},
		put(81, other), convert(82, other, "tank/vms/other")}
	f.mu.Unlock()
	z.set("tank/vms/b", freshLogical, "importing:sha256:"+sum) // a resume, no job running: taken
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/b", io.Discard); err != nil {
		t.Fatalf("other jobs: %v", err)
	}
	for _, tc := range []struct {
		job     map[string]any
		staging string
		want    string
	}{
		{convert(77, "/s/a.qcow2", "tank/vms/c"), "/s", "another import into tank/vms/c is running: job 77 (vm.device.convert of /s/a.qcow2 into tank/vms/c)"},
		// The destination as a device path, or not clean.
		{convert(74, "/s/a.qcow2", "/dev/zvol/tank/vms/c"), "/s", "job 74 (vm.device.convert"},
		{convert(75, "/s/a.qcow2", "tank/vms//c/"), "/s", "job 75 (vm.device.convert"},
		// From a staged file of this image, into another zvol (as a device path).
		{convert(76, staged, "/dev/zvol/tank/vms/elsewhere"), "/s", "job 76 (vm.device.convert of " + staged + " into /dev/zvol/tank/vms/elsewhere)"},
		// An upload of a staged file of this image into this zvol, any run's, the staging directory or
		// the path not clean.
		{put(78, staged), "/s", "another import into tank/vms/c is running: job 78 (filesystem.put of " + staged + ")"},
		{put(79, "/s//"+sum[:16]+"-"+zvolHash("tank/vms/c")+"-feed-x.qcow2"), "/s/", "job 79 (filesystem.put of /s//" + sum[:16] + "-" + zvolHash("tank/vms/c") + "-feed-x.qcow2)"},
	} {
		z.set("tank/vms/c", freshLogical, "importing:sha256:"+sum)
		f.mu.Lock()
		f.running = []map[string]any{tc.job}
		f.mu.Unlock()
		n := len(f.called())
		if _, err := c.ImportImage(ctx, local, sum, tc.staging, "tank/vms/c", io.Discard); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v", tc.job, err)
		}
		if len(f.uploads(n)) != 0 || z.mark("tank/vms/c") != "importing:sha256:"+sum {
			t.Errorf("%v: uploaded or marked", tc.job)
		}
	}
	// One that starts converting during the upload: refused before the zvol is marked.
	z.set("tank/vms/d", freshLogical, "")
	f.mu.Lock()
	f.running = nil
	f.upload = func() { f.running = []map[string]any{convert(80, "/s/a.qcow2", "tank/vms/d")} }
	f.mu.Unlock()
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/d", io.Discard); err == nil || !strings.Contains(err.Error(), "another import into tank/vms/d is running: job 80") {
		t.Errorf("a convert begun during the upload: %v", err)
	}
	if z.mark("tank/vms/d") != "" {
		t.Error("marked under a running convert")
	}
}

// A convert this run cannot see end is waited for, never aborted (TrueNAS marks an aborted convert
// ABORTED while its qemu-img writes on): only SUCCESS and FAILED end it. Ended, the staged file is
// emptied and the lock deleted, and SUCCESS marks the zvol written; in any other state the lock
// and the staged file are left, named. A job TrueNAS answers that is not this convert is refused.
func TestImportImageConvertCut(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	z := f.zvols()
	ctx := context.Background()
	local, sum := image(t, "QFI\xfbdisk")
	// convertCut answers vm.device.convert with a RUNNING job, and cancels the run's context as
	// its wait first reads the job (the second read of it, after the check of its arguments);
	// then(n) runs (f.mu held) at each read n of the job.
	var cancel context.CancelFunc
	var job int64
	convertCut := func(then func(n int)) {
		reads := 0
		f.mu.Lock()
		defer f.mu.Unlock()
		f.methods["vm.device.convert"] = func(p []json.RawMessage) (any, *rpcErr) {
			job = f.start("vm.device.convert", p, "RUNNING")
			return job, nil
		}
		f.polled = func(id int64) {
			if id != job {
				return
			}
			if reads++; reads == 2 {
				cancel()
			}
			if then != nil {
				then(reads)
			}
		}
	}
	var warn strings.Builder
	run := func(zvol string) (string, error) {
		c, err := dial(t, s, "k3y")
		if err != nil {
			t.Fatal(err)
		}
		cctx, cc := context.WithCancel(ctx)
		defer cc()
		cancel = cc
		z.set(zvol, freshLogical, "")
		warn.Reset()
		_, err = c.ImportImage(cctx, local, sum, "/s", zvol, &warn)
		return "/s/" + sum[:16] + "-" + zvolHash(zvol) + "-" + z.lastRun(zvol+"-stowage-ref") + "-notes-1.0.0.qcow2", err
	}
	// ended checks the run ended its job: the staged file emptied and the lock deleted, the zvol's
	// mark mark.
	ended := func(zvol, remote, mark string) {
		t.Helper()
		if f.staged(remote) != "" || !f.has(remote) || z.has(zvol+"-stowage-ref") || z.mark(zvol) != mark {
			t.Errorf("%s: staged %q, lock left %v, mark %q", zvol, f.staged(remote), z.has(zvol+"-stowage-ref"), z.mark(zvol))
		}
	}
	// Cut off, the job ending SUCCESS on the fourth read of it (the second after the cut): the
	// zvol written, so marked written, the step a success with the cut-off a warning; the staged
	// file emptied and the lock deleted only after the job ended.
	reads, early := 0, false
	convertCut(func(n int) {
		if reads = n; n == 4 {
			f.jobs[job] = "SUCCESS"
		}
	})
	f.mu.Lock()
	f.upload = func() { early = early || (job != 0 && f.jobs[job] == "RUNNING") }
	f.mu.Unlock()
	remote, err := run("tank/vms/ce")
	// The cut-off's error is the wait's read cut off, which the cancellation words as it races it.
	if want := fmt.Sprintf("warning: the zvol tank/vms/ce holds the image sha256:%s; the wait for the convert job %d was cut off (TrueNAS core.get_jobs: ", sum, job); err != nil || !strings.HasPrefix(warn.String(), want) || !strings.HasSuffix(warn.String(), "), and the job then ended SUCCESS\n") {
		t.Errorf("a convert cut off, then ended SUCCESS: %v, warned\n%q\nwant\n%q…", err, warn.String(), want)
	}
	if early || reads < 4 {
		t.Errorf("a convert cut off, then ended SUCCESS: emptied while it ran %v, %d reads", early, reads)
	}
	ended("tank/vms/ce", remote, "sha256:"+sum)
	// Cut off, the job ending FAILED (its qemu-img exited): the cut-off's error, the zvol left
	// marked as begun.
	convertCut(func(n int) {
		if n == 4 {
			f.jobs[job] = "FAILED"
		}
	})
	remote, err = run("tank/vms/cf")
	// The job's own error named, a *FailedError (no run again with more time fixes it).
	var lockLeft *LockLeftError
	var failed *FailedError
	if err == nil || !strings.HasPrefix(err.Error(), "TrueNAS core.get_jobs: ") || !strings.HasSuffix(err.Error(), fmt.Sprintf("; the convert job %d then ended FAILED: boom", job)) ||
		strings.Contains(err.Error(), "left") || errors.As(err, &lockLeft) || !errors.As(err, &failed) || warn.Len() != 0 {
		t.Errorf("a convert cut off, then FAILED: %v", err)
	}
	ended("tank/vms/cf", remote, "importing:sha256:"+sum)
	f.mu.Lock()
	f.polled, f.upload = nil, nil
	f.mu.Unlock()
	// left checks the lock and the staged file are left, named in err with the fix.
	left := func(zvol, remote string, err error, why string) {
		t.Helper()
		ref := zvol + "-stowage-ref"
		fix := ": the lock " + ref + " and the staged file " + remote + " are left; once TrueNAS's Jobs shows no vm.device.convert of " + remote + `, delete the zvol ` + ref +
			` on TrueNAS (Datasets, or pool.dataset.delete with {"recursive": true}), then run this again (TrueNAS's API deletes no file: delete ` + remote + " on the host)"
		var lockLeft *LockLeftError
		if err == nil || !strings.HasSuffix(err.Error(), why+fix) || !errors.As(err, &lockLeft) {
			t.Errorf("%s: %v\nwant the end\n%s", zvol, err, why+fix)
		}
		if !z.has(ref) || f.staged(remote) != "QFI\xfbdisk" || z.mark(zvol) != "importing:sha256:"+sum {
			t.Errorf("%s: the lock or the staged file not left, or the mark changed", zvol)
		}
	}
	// Cut off, the job still running past the wait.
	convertCut(nil)
	remote, err = run("tank/vms/cr")
	left("tank/vms/cr", remote, err, fmt.Sprintf("; the convert job %d is still RUNNING on TrueNAS after %s", job, stillFor(t, err, settleTimeout)))
	// Cut off, the job then ABORTED (its qemu-img writes on), or gone from TrueNAS's jobs: left at
	// once, never waited out.
	convertCut(func(n int) {
		if n == 3 {
			f.jobs[job] = "ABORTED"
		}
	})
	remote, err = run("tank/vms/ca")
	left("tank/vms/ca", remote, err, fmt.Sprintf("; the convert job %d is ABORTED, which TrueNAS marks an aborted convert at once while its qemu-img writes on: it may still run", job))
	convertCut(func(n int) {
		if n == 3 {
			delete(f.jobs, job)
		}
	})
	remote, err = run("tank/vms/cg")
	left("tank/vms/cg", remote, err, fmt.Sprintf("; the convert job %d is gone from TrueNAS's jobs: whether its qemu-img still writes cannot be told", job))
	// Cut off, the job not read after (the connection breaks).
	convertCut(func(n int) {
		if n == 3 {
			f.drop = "core.get_jobs"
		}
	})
	remote, err = run("tank/vms/cn")
	left("tank/vms/cn", remote, err, fmt.Sprintf("the convert job %d could not be read (", job)+"TrueNAS core.get_jobs: failed to get reader: failed to read frame header: EOF): it may still run")
	f.mu.Lock()
	f.polled = nil
	f.mu.Unlock()

	// The convert's answer lost (its connection closed): its job looked for on a new connection.
	// lostCall makes the call's job in state, or none when state is "".
	lostCall := func(state string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.methods["vm.device.convert"] = func(p []json.RawMessage) (any, *rpcErr) {
			f.drop = "vm.device.convert"
			if state == "" {
				return 0, nil
			}
			job = f.start("vm.device.convert", p, state)
			return job, nil
		}
	}
	lostErr := func(remote, zvol string) string {
		return "vm.device.convert of " + remote + " into " + zvol + ": TrueNAS vm.device.convert: failed to get reader: failed to read frame header: EOF"
	}
	// Found RUNNING: settled, still running, left.
	lostCall("RUNNING")
	remote, err = run("tank/vms/cl")
	left("tank/vms/cl", remote, err, lostErr(remote, "tank/vms/cl")+fmt.Sprintf(" (its answer lost); the convert job %d is still RUNNING on TrueNAS after %s", job, stillFor(t, err, settleTimeout)))
	// Found SUCCESS: the zvol written, marked, a warning.
	lostCall("SUCCESS")
	remote, err = run("tank/vms/cs")
	if want := fmt.Sprintf("warning: the zvol tank/vms/cs holds the image sha256:%s; the wait for the convert job %d was cut off (%s (its answer lost)), and the job then ended SUCCESS\n", sum, job, lostErr(remote, "tank/vms/cs")); err != nil || warn.String() != want {
		t.Errorf("a lost convert that ended SUCCESS: %v, warned\n%q\nwant\n%q", err, warn.String(), want)
	}
	ended("tank/vms/cs", remote, "sha256:"+sum)
	// No job made: nothing ran, released as any failure is.
	lostCall("")
	remote, err = run("tank/vms/c0")
	if err == nil || err.Error() != lostErr(remote, "tank/vms/c0")+" (its answer lost; TrueNAS has no job of it, looked for 3 times over 20ms: nothing ran)" {
		t.Errorf("a lost convert that made no job: %v", err)
	}
	ended("tank/vms/c0", remote, "importing:sha256:"+sum)
	// The jobs not read: left.
	lostCall("RUNNING")
	f.mu.Lock()
	f.lookErr = true
	f.mu.Unlock()
	remote, err = run("tank/vms/cu")
	left("tank/vms/cu", remote, err, lostErr(remote, "tank/vms/cu")+" (its answer lost, its job not read: TrueNAS core.get_jobs: jobs unread; a convert may run)")
	f.mu.Lock()
	f.lookErr = false
	f.mu.Unlock()

	// Its job's arguments as TrueNAS may give them, the zvol a device path and the file's path not
	// clean: this convert's, waited for, the zvol written.
	f.mu.Lock()
	f.methods["vm.device.convert"] = func(p []json.RawMessage) (any, *rpcErr) {
		var a struct{ Source, Destination string }
		_ = json.Unmarshal(p[0], &a)
		q, _ := json.Marshal(map[string]string{"source": strings.Replace(a.Source, "/s/", "/s//", 1), "destination": "/dev/zvol/" + a.Destination})
		return f.start("vm.device.convert", []json.RawMessage{q}, "SUCCESS"), nil
	}
	f.mu.Unlock()
	remote, err = run("tank/vms/cz")
	if err != nil {
		t.Errorf("a convert's arguments normalised: %v", err)
	}
	ended("tank/vms/cz", remote, "sha256:"+sum)
	// Answered with another import's queued job: refused, never waited for, the staged file
	// emptied and the lock deleted.
	f.mu.Lock()
	f.methods["vm.device.convert"] = func([]json.RawMessage) (any, *rpcErr) {
		p, _ := json.Marshal(map[string]string{"source": "/s/q.qcow2", "destination": "tank/vms/q"})
		job = f.start("vm.device.convert", []json.RawMessage{p}, "WAITING")
		return job, nil
	}
	f.mu.Unlock()
	remote, err = run("tank/vms/cq")
	if err == nil || err.Error() != fmt.Sprintf("TrueNAS answered vm.device.convert of %s into tank/vms/cq with job %d, another convert's (of /s/q.qcow2 into tank/vms/q): TrueNAS runs one convert at a time and queues one more, handing a further call the queued job; let the other converts end, then run this again", remote, job) {
		t.Errorf("another convert's job: %v", err)
	}
	ended("tank/vms/cq", remote, "importing:sha256:"+sum)
}

// imported is what importRun saw of a run.
type imported struct {
	remote, warned string
	logins         int // the API logins the run made: one per connection
	err            error
}

// importRun imports local (sum) into zvol, made a zvol never written, on a client of its own.
func importRun(t *testing.T, f *fake, s *httptest.Server, z *zvolFake, local, sum, zvol string) imported {
	t.Helper()
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	z.set(zvol, freshLogical, "")
	n := len(f.called())
	var w strings.Builder
	_, err = c.ImportImage(context.Background(), local, sum, "/s", zvol, &w)
	r := imported{remote: "/s/" + sum[:16] + "-" + zvolHash(zvol) + "-" + z.lastRun(zvol+"-stowage-ref") + "-notes-1.0.0.qcow2", warned: w.String(), err: err}
	for _, l := range f.called()[n:] {
		if strings.HasPrefix(l, "auth.login_with_api_key ") {
			r.logins++
		}
	}
	return r
}

// stillFor is the time a "still <state> on TrueNAS after <d>" in err names, failing unless it is
// at least least (and below a second).
func stillFor(t *testing.T, err error, least time.Duration) string {
	t.Helper()
	m := regexp.MustCompile(`still \w+ on TrueNAS after ([^:]+):`).FindStringSubmatch(fmt.Sprint(err))
	if m == nil {
		t.Errorf("no time watched in %v", err)
		return "?"
	}
	d, perr := time.ParseDuration(m[1])
	if perr != nil || d < least || d >= time.Second {
		t.Errorf("watched %s, want at least %s: %v", m[1], least, err)
	}
	return m[1]
}

// A convert's job is told this run's only by its arguments: a job the run cannot identify leaves
// the lock and the staged file, whatever it then does; one of this run's ending is read on any
// connection; settle states the time it watched.
func TestImportImageConvertWhose(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	z := f.zvols()
	local, sum := image(t, "QFI\xfbdisk")
	converts := func(h func(p []json.RawMessage) (any, *rpcErr)) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.methods["vm.device.convert"] = h
	}
	// kept checks the lock and the staged file are left, err a *LockLeftError holding why.
	kept := func(zvol string, r imported, why string) {
		t.Helper()
		var lockLeft *LockLeftError
		if r.err == nil || !strings.Contains(r.err.Error(), why+": the lock "+zvol+"-stowage-ref and the staged file "+r.remote+" are left;") || !errors.As(r.err, &lockLeft) {
			t.Errorf("%s: %v\nwant\n%s", zvol, r.err, why)
		}
		if !z.has(zvol+"-stowage-ref") || f.staged(r.remote) != "QFI\xfbdisk" || z.mark(zvol) != "importing:sha256:"+sum {
			t.Errorf("%s: the lock or the staged file not left, or the mark changed", zvol)
		}
	}
	// released checks the run ended its job: the lock deleted, the staged file emptied, the zvol's
	// mark mark.
	released := func(zvol string, r imported, mark string) {
		t.Helper()
		if z.has(zvol+"-stowage-ref") || !f.has(r.remote) || f.staged(r.remote) != "" || z.mark(zvol) != mark {
			t.Errorf("%s: lock left %v, staged %q, mark %q", zvol, z.has(zvol+"-stowage-ref"), f.staged(r.remote), z.mark(zvol))
		}
	}
	var job int64
	// Answered with a job TrueNAS's jobs do not hold: not refused as another's, left.
	converts(func([]json.RawMessage) (any, *rpcErr) { return 999, nil })
	r := importRun(t, f, s, z, local, sum, "tank/vms/u0")
	kept("tank/vms/u0", r, "TrueNAS answered vm.device.convert of "+r.remote+" into tank/vms/u0 with job 999, which TrueNAS's jobs do not hold; the convert job 999 is gone from TrueNAS's jobs: whether its qemu-img still writes cannot be told")
	// Answered with a job whose arguments are no convert's, which then ends SUCCESS: left.
	converts(func([]json.RawMessage) (any, *rpcErr) {
		job = f.start("vm.device.convert", []json.RawMessage{json.RawMessage(`"x"`)}, "SUCCESS")
		return job, nil
	})
	r = importRun(t, f, s, z, local, sum, "tank/vms/ua")
	kept("tank/vms/ua", r, fmt.Sprintf("TrueNAS answered vm.device.convert of %s into tank/vms/ua with job %d, whose arguments TrueNAS gives as no convert's; the convert job %d ended SUCCESS, whose arguments TrueNAS gives as no convert's: whether a convert of this run's still writes the zvol cannot be told", r.remote, job, job))
	// The first read of the job fails (its connection breaks), the job then SUCCESS: settle reads
	// its arguments itself, this run's, so the zvol is marked written.
	converts(func(p []json.RawMessage) (any, *rpcErr) {
		f.drop = "core.get_jobs"
		job = f.start("vm.device.convert", p, "SUCCESS")
		return job, nil
	})
	r = importRun(t, f, s, z, local, sum, "tank/vms/ud")
	if want := fmt.Sprintf("warning: the zvol tank/vms/ud holds the image sha256:%s; the wait for the convert job %d was cut off (TrueNAS core.get_jobs: failed to get reader: failed to read frame header: EOF), and the job then ended SUCCESS\n", sum, job); r.err != nil || r.warned != want {
		t.Errorf("the first read failed, the job SUCCESS: %v, warned\n%q\nwant\n%q", r.err, r.warned, want)
	}
	released("tank/vms/ud", r, "sha256:"+sum)
	// Marked on settle's connection: logins for the upload's wait, settle's, the emptying's and its
	// upload's wait, and the lock's release's connections alone.
	if r.logins != 5 {
		t.Errorf("the first read failed, the job SUCCESS: %d logins, want 5", r.logins)
	}
	// FAILED: TrueNAS's error, a *FailedError, the lock released.
	converts(func(p []json.RawMessage) (any, *rpcErr) {
		job = f.start("vm.device.convert", p, "FAILED")
		return job, nil
	})
	r = importRun(t, f, s, z, local, sum, "tank/vms/uf")
	var failed *FailedError
	if want := fmt.Sprintf("vm.device.convert of %s: job %d FAILED: boom", r.remote, job); r.err == nil || r.err.Error() != want || !errors.As(r.err, &failed) {
		t.Errorf("a convert FAILED: %v, want %q", r.err, want)
	}
	released("tank/vms/uf", r, "importing:sha256:"+sum)
	// The job read once: its wait takes the row its check read.
	reads := 0
	for _, l := range f.called() {
		if strings.HasPrefix(l, fmt.Sprintf(`core.get_jobs [["id","=",%d]]`, job)) {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("a convert FAILED: its job read %d times, want 1", reads)
	}
	// A job whose arguments tell nothing, then read as this run's or another's as it ends: told
	// from the row it ends on.
	unknownThen := func(args string) {
		converts(func(p []json.RawMessage) (any, *rpcErr) {
			job = f.start("vm.device.convert", []json.RawMessage{json.RawMessage(`"x"`)}, "RUNNING")
			if args == "" {
				b, _ := json.Marshal(p)
				args = string(b)
			}
			n := 0
			f.polled = func(id int64) {
				if id != job {
					return
				}
				if n++; n == 3 {
					f.jobs[job], f.jobArgs[job] = "SUCCESS", `{"method":"vm.device.convert","arguments":`+args+`}`
				}
			}
			return job, nil
		})
	}
	unknownThen("")
	r = importRun(t, f, s, z, local, sum, "tank/vms/uo")
	if want := fmt.Sprintf("warning: the zvol tank/vms/uo holds the image sha256:%s; the wait for the convert job %d was cut off (TrueNAS answered vm.device.convert of %s into tank/vms/uo with job %d, whose arguments TrueNAS gives as no convert's), and the job then ended SUCCESS\n", sum, job, r.remote, job); r.err != nil || r.warned != want {
		t.Errorf("unknown, then this run's: %v, warned\n%q\nwant\n%q", r.err, r.warned, want)
	}
	released("tank/vms/uo", r, "sha256:"+sum)
	unknownThen(`[{"source":"/s/q.qcow2","destination":"tank/vms/q"}]`)
	r = importRun(t, f, s, z, local, sum, "tank/vms/uv")
	if want := fmt.Sprintf("TrueNAS answered vm.device.convert of %s into tank/vms/uv with job %d, another convert's (of /s/q.qcow2 into tank/vms/q): ", r.remote, job); r.err == nil || !strings.HasPrefix(r.err.Error(), want) {
		t.Errorf("unknown, then another's: %v, want %q…", r.err, want)
	}
	released("tank/vms/uv", r, "importing:sha256:"+sum)
	f.mu.Lock()
	f.polled = nil
	f.mu.Unlock()
	// A wait that ended ABORTED on a healthy connection is settled on it: one login, the upload's.
	converts(func(p []json.RawMessage) (any, *rpcErr) {
		job = f.start("vm.device.convert", p, "ABORTED")
		return job, nil
	})
	r = importRun(t, f, s, z, local, sum, "tank/vms/uh")
	kept("tank/vms/uh", r, fmt.Sprintf("vm.device.convert of %s: job %d ABORTED: boom; the convert job %d is ABORTED, which TrueNAS marks an aborted convert at once while its qemu-img writes on: it may still run", r.remote, job, job))
	if r.logins != 1 {
		t.Errorf("an ABORTED wait settled: %d logins, want 1 (the upload's)", r.logins)
	}
	// A lost answer's job found on the second or the third look: settled, on the look's
	// connection (two logins: the upload's, the look's).
	for _, k := range []int{2, 3} {
		zvol := fmt.Sprintf("tank/vms/lg%d", k)
		var params []json.RawMessage
		looks := 0
		converts(func(p []json.RawMessage) (any, *rpcErr) {
			f.drop, params, looks = "vm.device.convert", p, 0
			return 0, nil
		})
		f.mu.Lock()
		f.looked = func() {
			if looks++; looks == k {
				job = f.start("vm.device.convert", params, "RUNNING")
			}
		}
		f.mu.Unlock()
		r = importRun(t, f, s, z, local, sum, zvol)
		kept(zvol, r, fmt.Sprintf("vm.device.convert of %s into %s: TrueNAS vm.device.convert: failed to get reader: failed to read frame header: EOF (its answer lost); the convert job %d is still RUNNING on TrueNAS after %s", r.remote, zvol, job, stillFor(t, r.err, settleTimeout)))
		if r.logins != 2 {
			t.Errorf("%s: %d logins, want 2", zvol, r.logins)
		}
	}
	f.mu.Lock()
	f.looked = nil
	f.mu.Unlock()
	// Settle names the time it watched, which slow reads stretch past settleTimeout.
	converts(func(p []json.RawMessage) (any, *rpcErr) {
		f.drop = "core.get_jobs"
		job = f.start("vm.device.convert", p, "RUNNING")
		f.polled = func(id int64) {
			if id == job {
				f.mu.Unlock()
				time.Sleep(30 * time.Millisecond)
				f.mu.Lock()
			}
		}
		return job, nil
	})
	r = importRun(t, f, s, z, local, sum, "tank/vms/us")
	f.mu.Lock()
	f.polled = nil
	f.mu.Unlock()
	kept("tank/vms/us", r, fmt.Sprintf("; the convert job %d is still RUNNING on TrueNAS after %s", job, stillFor(t, r.err, 55*time.Millisecond)))
}

// settle's watch ends refTimeout before its context's deadline when that comes first, leaving its
// last read that margin.
func TestUntil(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dl, _ := ctx.Deadline()
	if got := until(ctx, time.Hour); !got.Equal(dl.Add(-refTimeout)) {
		t.Errorf("an hour, the deadline in a minute: %v, want %v", got, dl.Add(-refTimeout))
	}
	if got := until(ctx, time.Second); !got.Before(dl.Add(-refTimeout)) || time.Until(got) > time.Second {
		t.Errorf("a second, the deadline in a minute: %v", got)
	}
	if got := until(context.Background(), time.Second); time.Until(got) > time.Second || time.Until(got) < 0 {
		t.Errorf("a second, no deadline: %v", got)
	}
}

// A watch whose connection took most of settle's budget still ends refTimeout before its deadline:
// its last read is not cut off, and the job is named still running, not unread. It sets package
// variables: not parallel.
func TestSettleSlowConnect(t *testing.T) {
	ref, st := refTimeout, settleTimeout
	t.Cleanup(func() { refTimeout, settleTimeout = ref, st })
	// settle's budget is 150+250+150 ms; its connection takes 310 of them, leaving less than its
	// 250 ms watch; a read of the job takes 40.
	refTimeout, settleTimeout = 150*time.Millisecond, 250*time.Millisecond
	f, s := newFake(t)
	z := f.zvols()
	local, sum := image(t, "QFI\xfbdisk")
	var job int64
	slow := false
	f.mu.Lock()
	f.methods["vm.device.convert"] = func(p []json.RawMessage) (any, *rpcErr) {
		f.drop, slow = "core.get_jobs", true // the run's first read fails: settled on a new connection
		job = f.start("vm.device.convert", p, "RUNNING")
		return job, nil
	}
	f.login = func() {
		if slow {
			slow = false
			f.mu.Unlock()
			time.Sleep(310 * time.Millisecond)
			f.mu.Lock()
		}
	}
	f.polled = func(id int64) {
		if id == job {
			f.mu.Unlock()
			time.Sleep(40 * time.Millisecond)
			f.mu.Lock()
		}
	}
	f.mu.Unlock()
	r := importRun(t, f, s, z, local, sum, "tank/vms/sc")
	if want := fmt.Sprintf("; the convert job %d is still RUNNING on TrueNAS after ", job); r.err == nil || !strings.Contains(r.err.Error(), want) {
		t.Errorf("a slow connection: %v, want %q", r.err, want)
	}
}

// What an upload may have left: nothing only when no byte of its body was read, or a 401 or 403
// came before it was read whole.
func TestUploadLeaves(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	send := func(body string) (upload, error) {
		return c.send(ctx, strings.NewReader(body), "x", "/s/x", io.Discard)
	}
	// A 413, its body read or not: maybe.
	f.mu.Lock()
	f.refuse = http.StatusRequestEntityTooLarge
	f.mu.Unlock()
	for _, body := range []string{"x", strings.Repeat("x", 32<<20)} {
		if put, err := send(body); put != putMaybe || err == nil {
			t.Errorf("a 413 to %d bytes: %v, %v", len(body), put, err)
		}
	}
	// A 401 or 403 read whole first: maybe; before (a body too large for the sockets' buffers):
	// nothing.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		f.mu.Lock()
		f.refuse, f.readAll = status, true
		f.mu.Unlock()
		if put, err := send("x"); put != putMaybe || err == nil {
			t.Errorf("a %d after the body: %v, %v", status, put, err)
		}
		f.mu.Lock()
		f.readAll = false
		f.mu.Unlock()
		if put, err := send(strings.Repeat("x", 32<<20)); put != putNothing || err == nil {
			t.Errorf("a %d before the body: %v, %v", status, put, err)
		}
	}
	f.mu.Lock()
	f.refuse = 0
	f.cut = "late"
	f.mu.Unlock()
	// No answer once the body was read: maybe.
	if put, err := send("x"); put != putMaybe || err == nil {
		t.Errorf("no answer after the body: %v, %v", put, err)
	}
	// The dial refused: nothing.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := *c.base
	u.Host = l.Addr().String()
	l.Close()
	c.base = &u
	if put, err := send("x"); put != putNothing || err == nil {
		t.Errorf("a dial refused: %v, %v", put, err)
	}
}

// The staged file is emptied only once no upload of it runs: one RUNNING is followed to its end,
// and one still running after the watch leaves the file, named to delete on the host.
func TestEmptyStagedWaitsForItsUpload(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	put := func(remote string) int64 {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.files[remote] = []byte("part")
		p, _ := json.Marshal(remote)
		return f.start("filesystem.put", []json.RawMessage{p}, "RUNNING")
	}
	// Ends SUCCESS on its third read: emptied after.
	job := put("/s/a")
	reads := 0
	f.mu.Lock()
	f.polled = func(id int64) {
		if id == job {
			if reads++; reads == 2 {
				f.jobs[job] = "SUCCESS"
			}
		}
	}
	f.upload = func() {
		if f.jobs[job] != "SUCCESS" {
			t.Error("emptied while its upload ran")
		}
	}
	f.mu.Unlock()
	if err := c.emptyStaged(ctx, "/s/a", putMaybe); err != nil || f.staged("/s/a") != "" {
		t.Errorf("an upload that ended: %v, staged %q", err, f.staged("/s/a"))
	}
	if reads < 2 {
		t.Errorf("an upload that ended: read %d times, not followed", reads)
	}
	f.mu.Lock()
	f.polled, f.upload = nil, nil
	f.mu.Unlock()
	// Still running: left.
	job = put("/s/b")
	err = c.emptyStaged(ctx, "/s/b", putFile)
	if want := fmt.Sprintf("the staged file /s/b not emptied (TrueNAS's API deletes no file: delete it on the host): its upload's job %d is still RUNNING on TrueNAS after ", job); err == nil || !strings.HasPrefix(err.Error(), want) || f.staged("/s/b") != "part" {
		t.Errorf("an upload still running: %v, staged %q", err, f.staged("/s/b"))
	}
}

// The staged file is emptied only where its upload may have left it: TrueNAS took the body, or a
// filesystem.put job of it exists.
func TestImportImageUploadLeft(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	z := f.zvols()
	local, sum := image(t, "QFI\xfbdisk")
	f.job("vm.device.convert", "SUCCESS")
	// Refused by a 413, which may come after a part of the body: its put job looked for, none
	// found, nothing emptied, no fix on the host.
	f.mu.Lock()
	f.refuse = http.StatusRequestEntityTooLarge
	f.mu.Unlock()
	n := len(f.called())
	r := importRun(t, f, s, z, local, sum, "tank/vms/p4")
	f.mu.Lock()
	f.refuse = 0
	f.mu.Unlock()
	if r.err == nil || r.err.Error() != "upload of notes-1.0.0.qcow2 refused: HTTP 413 refused" || z.has("tank/vms/p4-stowage-ref") || f.has(r.remote) ||
		!slices.ContainsFunc(f.called()[n:], func(l string) bool { return strings.HasPrefix(l, `core.get_jobs [["method","=","filesystem.put"]]`) }) {
		t.Errorf("an upload refused by 413: %v, lock left %v, its put job not looked for", r.err, z.has("tank/vms/p4-stowage-ref"))
	}
	// Cut off unanswered, no put job made: nothing emptied (an empty upload would make the file).
	f.mu.Lock()
	f.cut = "early"
	f.mu.Unlock()
	r = importRun(t, f, s, z, local, sum, "tank/vms/pe")
	if r.err == nil || !strings.HasPrefix(r.err.Error(), "upload of notes-1.0.0.qcow2: ") || strings.Contains(r.err.Error(), "emptied") || f.has(r.remote) {
		t.Errorf("an upload cut off, no job: %v, a file made %v", r.err, f.has(r.remote))
	}
	// Cut off unanswered, its file landed by a put job: emptied.
	f.mu.Lock()
	f.cut = "late"
	f.mu.Unlock()
	r = importRun(t, f, s, z, local, sum, "tank/vms/pl")
	if r.err == nil || !strings.HasPrefix(r.err.Error(), "upload of notes-1.0.0.qcow2: ") || strings.Contains(r.err.Error(), "emptied") || !f.has(r.remote) || f.staged(r.remote) != "" {
		t.Errorf("an upload cut off, its job made: %v, staged %q", r.err, f.staged(r.remote))
	}
}

// Cleaning up runs on a connection of its own, whatever broke the run's: the staged file's
// emptying failing is a warning once the zvol is marked, an error before.
func TestImportImageCleanup(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	z := f.zvols()
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	local, sum := image(t, "QFI\xfbdisk")
	// The connection broken under the run (not cancelled), before the upload: the lock deleted on
	// a new one.
	z.set("tank/vms/br", freshLogical, "")
	f.mu.Lock()
	f.drop = "vm.device.query"
	f.mu.Unlock()
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/br", io.Discard); err == nil || !strings.Contains(err.Error(), "TrueNAS vm.device.query") {
		t.Errorf("a connection broken: %v", err)
	}
	if z.has("tank/vms/br-stowage-ref") {
		t.Error("a connection broken: the lock left")
	}
	// The staged file not emptied once the zvol is written: a warning, the step a success.
	f.job("vm.device.convert", "SUCCESS")
	f.mu.Lock()
	f.noEmpty = true
	f.mu.Unlock()
	z.set("tank/vms/ne", freshLogical, "")
	var warn strings.Builder
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/ne", &warn); err != nil || !strings.HasPrefix(warn.String(), "warning: the zvol tank/vms/ne holds the image sha256:"+sum+"; the staged file /s/"+sum[:16]+"-"+zvolHash("tank/vms/ne")+"-") ||
		!strings.Contains(warn.String(), " not emptied (TrueNAS's API deletes no file: delete it on the host): ") {
		t.Errorf("not emptied after the mark: %v, warned %q", err, warn.String())
	}
	if z.mark("tank/vms/ne") != "sha256:"+sum || z.has("tank/vms/ne-stowage-ref") {
		t.Error("not emptied after the mark: the zvol not marked, or the lock left")
	}
	// Before the mark (the convert failed): an error.
	f.job("vm.device.convert", "FAILED")
	z.set("tank/vms/nf", freshLogical, "")
	warn.Reset()
	if _, err := c.ImportImage(ctx, local, sum, "/s", "tank/vms/nf", &warn); err == nil || !strings.Contains(err.Error(), "FAILED") || !strings.Contains(err.Error(), " not emptied (TrueNAS's API deletes no file") || warn.Len() != 0 {
		t.Errorf("not emptied before the mark: %v, warned %q", err, warn.String())
	}
}

// staged is the file an upload left at remote.
func (f *fake) staged(remote string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.files[remote])
}

// has says whether an upload left a file at remote.
func (f *fake) has(remote string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[remote]
	return ok
}

// zvolFake is the fake's zvols and VM devices: pool.dataset.query, create, update and delete, and
// vm.device.query.
type zvolFake struct {
	f         *fake
	types     map[string]string
	logical   map[string]int64 // logicalreferenced; referenced is fresh's unless refs names it
	refs      map[string]int64
	marks     map[string]string
	initial   map[string]string
	used      map[string]bool
	noRef     map[string]bool   // zvols queried without their logicalreferenced
	noProps   map[string]bool   // zvols queried without their user_properties
	refOf     map[string]string // RefProperty
	runOf     map[string]string // RunProperty
	made      map[string]int64  // creation, in seconds since the epoch; now unless set
	noMade    map[string]bool   // datasets queried without their creation
	snaps     map[string]bool   // datasets with a snapshot: deleted only recursively
	vbs       map[string]string // volblocksize, 16K unless set, none when set empty
	empty     int64             // the logicalreferenced of a zvol pool.dataset.create makes
	creates   []string          // pool.dataset.create's parameters
	deletes   []string          // pool.dataset.delete's parameters, of the deletions done
	createErr *rpcErr           // pool.dataset.create fails so, making nothing
	queried   func(name string) // run as a dataset is queried, f.mu held
	created   func(name string) // run as a dataset is made, f.mu held
	deleted   func(name string) // run as a dataset is deleted, f.mu held
	delErr    bool              // pool.dataset.delete fails
	failQuery string            // the next pool.dataset.query of this name fails
}

// fresh is what TrueNAS 25.10 gives for a zvol never written: referenced and logicalreferenced.
const freshRef, freshLogical = 76384, 28672

// zvols serves tank/vms/x, an empty zvol, and whatever set adds.
func (f *fake) zvols() *zvolFake {
	z := &zvolFake{f: f, types: map[string]string{}, logical: map[string]int64{}, refs: map[string]int64{}, marks: map[string]string{}, initial: map[string]string{},
		used: map[string]bool{}, noRef: map[string]bool{}, noProps: map[string]bool{}, refOf: map[string]string{}, runOf: map[string]string{}, made: map[string]int64{},
		noMade: map[string]bool{}, snaps: map[string]bool{}, vbs: map[string]string{}, empty: freshLogical}
	z.set("tank/vms/x", freshLogical, "")
	f.methods["pool.dataset.query"] = func(p []json.RawMessage) (any, *rpcErr) {
		var filter [][]any
		_ = json.Unmarshal(p[0], &filter)
		name := filter[0][2].(string)
		if z.queried != nil {
			z.queried(name)
		}
		if name == z.failQuery {
			z.failQuery = ""
			return nil, &rpcErr{Code: 22, Message: "query failed"}
		}
		typ, ok := z.types[name]
		if !ok {
			return []any{}, nil
		}
		props := map[string]any{"managedby": map[string]any{"value": "192.0.2.1"}}
		if m := z.marks[name]; m != "" {
			props[ImageProperty] = map[string]any{"value": m}
		}
		if r := z.refOf[name]; r != "" {
			props[RefProperty] = map[string]any{"value": r}
		}
		if r := z.runOf[name]; r != "" {
			props[RunProperty] = map[string]any{"value": r}
		}
		// As TrueNAS does, a property is given when extra.properties names it alone, and the user
		// properties whatever extra.user_properties says.
		var opts struct {
			Extra struct {
				Properties []string `json:"properties"`
			} `json:"extra"`
		}
		_ = json.Unmarshal(p[1], &opts)
		ds := map[string]any{"id": name, "type": typ, "user_properties": props}
		if z.noProps[name] {
			delete(ds, "user_properties")
		}
		for _, k := range opts.Extra.Properties {
			switch k {
			case "logicalreferenced":
				if !z.noRef[name] {
					ds[k] = map[string]any{"parsed": z.logical[name]}
				}
			case "referenced":
				ds[k] = map[string]any{"parsed": cmp.Or(z.refs[name], freshRef)}
			case "volblocksize":
				if v, set := z.vbs[name]; !set {
					ds[k] = map[string]any{"value": "16K"}
				} else if v != "" {
					ds[k] = map[string]any{"value": v}
				}
			case "creation":
				// As TrueNAS 25.10 gives it.
				if !z.noMade[name] {
					s := cmp.Or(z.made[name], time.Now().Unix())
					ds[k] = map[string]any{"parsed": map[string]any{"$date": s * 1000}, "rawvalue": strconv.FormatInt(s, 10), "value": time.Unix(s, 0).Format(time.ANSIC), "source": "NONE"}
				}
			}
		}
		return []map[string]any{ds}, nil
	}
	f.methods["pool.dataset.update"] = func(p []json.RawMessage) (any, *rpcErr) {
		var name string
		var upd struct {
			Props []struct{ Key, Value string } `json:"user_properties_update"`
		}
		_ = json.Unmarshal(p[0], &name)
		_ = json.Unmarshal(p[1], &upd)
		for _, u := range upd.Props {
			if u.Key == ImageProperty {
				z.marks[name] = u.Value
			}
		}
		return map[string]any{"id": name}, nil
	}
	// As TrueNAS 25.10 does (pool.dataset.create's answer is the dataset, unread; a name that
	// exists is refused).
	f.methods["pool.dataset.create"] = func(p []json.RawMessage) (any, *rpcErr) {
		var a struct {
			Name  string                        `json:"name"`
			Props []struct{ Key, Value string } `json:"user_properties"`
		}
		_ = json.Unmarshal(p[0], &a)
		z.creates = append(z.creates, string(p[0]))
		if z.createErr != nil {
			return nil, z.createErr
		}
		if _, ok := z.types[a.Name]; ok {
			return nil, &rpcErr{Code: -32001, Message: "Method call error", Data: map[string]any{"error": 14, "errname": "EFAULT",
				"reason": "[EFAULT] Failed to create dataset: cannot create '" + a.Name + "': dataset already exists"}}
		}
		z.types[a.Name], z.logical[a.Name], z.made[a.Name] = "VOLUME", z.empty, time.Now().Unix()
		for _, u := range a.Props {
			switch u.Key {
			case RefProperty:
				z.refOf[a.Name] = u.Value
			case RunProperty:
				z.runOf[a.Name] = u.Value
			}
		}
		if z.created != nil {
			z.created(a.Name)
		}
		return map[string]any{"id": a.Name}, nil
	}
	f.methods["pool.dataset.delete"] = func(p []json.RawMessage) (any, *rpcErr) {
		var name string
		_ = json.Unmarshal(p[0], &name)
		var opts struct{ Recursive bool }
		if len(p) > 1 {
			_ = json.Unmarshal(p[1], &opts)
		}
		if _, ok := z.types[name]; !ok || z.delErr {
			return nil, &rpcErr{Code: 2, Message: name + " cannot be deleted"}
		}
		if z.snaps[name] && !opts.Recursive {
			return nil, &rpcErr{Code: -32001, Message: "Method call error", Data: map[string]any{"reason": "[EFAULT] Failed to delete dataset: cannot destroy '" + name + "': volume has children"}}
		}
		z.deletes = append(z.deletes, strings.Join(raw(p), " "))
		delete(z.types, name)
		delete(z.refOf, name)
		delete(z.runOf, name)
		delete(z.made, name)
		delete(z.snaps, name)
		if z.deleted != nil {
			z.deleted(name)
		}
		return true, nil
	}
	f.methods["vm.device.query"] = func(p []json.RawMessage) (any, *rpcErr) {
		var filter [][]any
		_ = json.Unmarshal(p[0], &filter)
		if filter[0][0] == "attributes.path" && z.used[strings.TrimPrefix(filter[0][2].(string), "/dev/zvol/")] {
			return []map[string]any{{"id": 12, "vm": 7}}, nil
		}
		return []any{}, nil
	}
	return z
}

func (z *zvolFake) set(name string, logical int64, mark string) {
	z.f.mu.Lock()
	defer z.f.mu.Unlock()
	z.types[name], z.logical[name], z.marks[name], z.initial[name] = "VOLUME", logical, mark, mark
}

func (z *zvolFake) kind(name, typ string) {
	z.f.mu.Lock()
	defer z.f.mu.Unlock()
	z.types[name] = typ
}

func (z *zvolFake) device(name string) {
	z.f.mu.Lock()
	defer z.f.mu.Unlock()
	z.used[name] = true
}

func (z *zvolFake) has(name string) bool {
	z.f.mu.Lock()
	defer z.f.mu.Unlock()
	_, ok := z.types[name]
	return ok
}

func (z *zvolFake) mark(name string) string {
	z.f.mu.Lock()
	defer z.f.mu.Unlock()
	return z.marks[name]
}

// lastRun is the RunProperty of the last pool.dataset.create of name.
func (z *zvolFake) lastRun(name string) string {
	z.f.mu.Lock()
	defer z.f.mu.Unlock()
	for i := len(z.creates) - 1; i >= 0; i-- {
		var a struct {
			Name  string                        `json:"name"`
			Props []struct{ Key, Value string } `json:"user_properties"`
		}
		_ = json.Unmarshal([]byte(z.creates[i]), &a)
		if a.Name != name {
			continue
		}
		for _, u := range a.Props {
			if u.Key == RunProperty {
				return u.Value
			}
		}
	}
	return ""
}

func TestUploadRefused(t *testing.T) {
	t.Parallel()
	_, s := newFake(t)
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	c.key = "other" // the upload's bearer is the API key
	local, _ := image(t, "x")
	if err := c.Upload(context.Background(), local, "/mnt/x"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("got %v", err)
	}
}

// An upload answered before its file was sent whole fails, though TrueNAS named a job.
func TestUploadCutShort(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	f.early, f.release = true, make(chan struct{})
	f.jobs[0] = "SUCCESS"
	t.Cleanup(func() { close(f.release) })
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	local, _ := image(t, strings.Repeat("x", 32<<20))
	if err := c.Upload(context.Background(), local, "/mnt/x"); err == nil || !strings.Contains(err.Error(), "upload of notes-1.0.0.qcow2") {
		t.Fatalf("got %v", err)
	}
}

func TestStop(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	state := map[int64]string{1: "RUNNING", 2: "STOPPED", 4: "SUSPENDED", 5: "RUNNING", 6: "SUSPENDED", 7: "RUNNING", 8: "SUSPENDED", 9: "RUNNING", 10: "PAUSED"}
	f.methods["vm.query"] = func(p []json.RawMessage) (any, *rpcErr) {
		var filter [][]any
		_ = json.Unmarshal(p[0], &filter)
		st, ok := state[int64(filter[0][2].(float64))]
		if !ok {
			return []any{}, nil
		}
		return []map[string]any{{"status": map[string]any{"state": st}}}, nil
	}
	// As on TrueNAS: vm.stop shuts down a RUNNING VM alone (but VM 5, which stays RUNNING), its
	// job failing otherwise; vm.poweroff stops a RUNNING or SUSPENDED one (but VM 6).
	f.methods["vm.stop"] = func(p []json.RawMessage) (any, *rpcErr) {
		var id int64
		_ = json.Unmarshal(p[0], &id)
		f.next++
		f.jobs[f.next] = "SUCCESS"
		switch {
		case state[id] != "RUNNING":
			f.jobs[f.next] = "FAILED"
		case id == 7: // it shut down on its own as the call came: the job fails
			state[id] = "STOPPED"
			f.jobs[f.next] = "FAILED"
		case id == 9: // the job fails, the VM still running
			f.jobs[f.next] = "FAILED"
		case id != 5:
			state[id] = "STOPPED"
		}
		return f.next, nil
	}
	f.methods["vm.poweroff"] = func(p []json.RawMessage) (any, *rpcErr) {
		var id int64
		_ = json.Unmarshal(p[0], &id)
		if id == 8 { // deleted as the call came
			delete(state, id)
			return nil, &rpcErr{Code: 2, Message: "no such VM"}
		}
		if st := state[id]; st != "RUNNING" && st != "SUSPENDED" {
			return nil, &rpcErr{Code: 22, Message: "not active"}
		}
		if id != 6 {
			state[id] = "STOPPED"
		}
		return nil, nil
	}
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Running, stopped, gone, suspended: each succeeds, and again.
	for range 2 {
		for _, id := range []int64{1, 2, 3, 4} {
			if err := c.Stop(ctx, id); err != nil {
				t.Fatalf("VM %d: %v", id, err)
			}
		}
	}
	var stops []string
	for _, l := range f.called() {
		if strings.HasPrefix(l, "vm.stop") || strings.HasPrefix(l, "vm.poweroff") {
			stops = append(stops, l)
		}
	}
	if want := []string{`vm.stop 1 {"force_after_timeout":true}`, `vm.poweroff 4`}; !slices.Equal(stops, want) {
		t.Errorf("stops %q, want %q: the VMs not stopped, once, the running one forced after its timeout, the suspended one powered off", stops, want)
	}
	// Not stopped after the stop: refused, naming the state and the call.
	if err := c.Stop(ctx, 5); err == nil || !strings.Contains(err.Error(), "VM 5 is RUNNING after vm.stop, not STOPPED") {
		t.Errorf("a VM the stop left running: %v", err)
	}
	if err := c.Stop(ctx, 6); err == nil || !strings.Contains(err.Error(), "VM 6 is SUSPENDED after vm.poweroff, not STOPPED") {
		t.Errorf("a VM the poweroff left suspended: %v", err)
	}
	// A call failing as the VM stops or goes meanwhile: stopped all the same.
	for _, id := range []int64{7, 8} {
		if err := c.Stop(ctx, id); err != nil {
			t.Errorf("VM %d stopped or gone as the call failed: %v", id, err)
		}
	}
	// A call failing, the VM not stopped: refused, naming its state and the call's error.
	if err := c.Stop(ctx, 9); err == nil || !strings.Contains(err.Error(), "VM 9 is RUNNING after vm.stop failed (vm.stop: job") {
		t.Errorf("a failed stop: %v", err)
	}
	if err := c.Stop(ctx, 10); err == nil || !strings.Contains(err.Error(), "VM 10 is PAUSED after vm.poweroff failed (") || !strings.Contains(err.Error(), "not active") {
		t.Errorf("a failed poweroff: %v", err)
	}
}
