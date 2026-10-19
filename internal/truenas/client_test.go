package truenas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func init() { poll = time.Millisecond }

// fake is a TrueNAS API: its methods answer from handlers, every call is recorded, and an upload
// lands in files.
type fake struct {
	mu      sync.Mutex
	key     string
	calls   []string
	methods map[string]func(params []json.RawMessage) (any, *rpcErr)
	files   map[string][]byte
	jobs    map[int64]string // job id → final state
	next    int64
	notify  bool // send a notification before each answer
}

type rpcErr struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{key: "k3y", methods: map[string]func([]json.RawMessage) (any, *rpcErr){}, files: map[string][]byte{}, jobs: map[int64]string{}}
	f.methods["auth.login_with_api_key"] = func(p []json.RawMessage) (any, *rpcErr) {
		var k string
		_ = json.Unmarshal(p[0], &k)
		return k == f.key, nil
	}
	f.methods["core.get_jobs"] = func(p []json.RawMessage) (any, *rpcErr) {
		var filter [][]any
		_ = json.Unmarshal(p[0], &filter)
		id := int64(filter[0][2].(float64))
		st, ok := f.jobs[id]
		if !ok {
			return []any{}, nil
		}
		return []map[string]any{{"state": st, "error": "boom"}}, nil
	}
	f.methods["filesystem.stat"] = func(p []json.RawMessage) (any, *rpcErr) {
		var path string
		_ = json.Unmarshal(p[0], &path)
		b, ok := f.files[path]
		if !ok {
			return nil, &rpcErr{Code: -32001, Message: "Method call error", Data: map[string]any{"errname": "ENOENT", "reason": path + " does not exist"}}
		}
		return map[string]any{"size": len(b)}, nil
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
			notify := f.notify
			f.mu.Unlock()
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
			b, _ := io.ReadAll(p)
			switch p.FormName() {
			case "data":
				var d struct {
					Method string `json:"method"`
					Params []any  `json:"params"`
				}
				_ = json.Unmarshal(b, &d)
				if d.Method != "filesystem.put" || d.Params[1].(map[string]any)["mode"].(float64) != 0o644 {
					http.Error(w, "bad data", http.StatusBadRequest)
					return
				}
				dest = d.Params[0].(string)
			case "file":
				f.mu.Lock()
				f.files[dest] = b
				f.calls = append(f.calls, "upload "+dest)
				f.mu.Unlock()
			}
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
	f.methods[method] = func([]json.RawMessage) (any, *rpcErr) {
		f.next++
		f.jobs[f.next] = state
		return f.next, nil
	}
}

func (f *fake) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func dial(t *testing.T, s *httptest.Server, key string) (*Client, error) {
	c, err := Dial(context.Background(), s.URL, key, true)
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
	if _, err := Dial(context.Background(), s.URL, "k3y", false); err == nil {
		t.Fatal("an unknown certificate was taken")
	}
	// HTTPS only: the key never travels in clear.
	for _, u := range []string{"http://" + strings.TrimPrefix(s.URL, "https://"), "ftp://h", "https://", "host:443",
		"https://:443", "https://nas/truenas", "https://u:pw@nas", "https://nas/?a=b", "https://nas/#x"} {
		if _, err := Dial(context.Background(), u, "k", false); err == nil || !strings.Contains(err.Error(), URLVar) || strings.Contains(err.Error(), ":pw@") {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestFromEnv(t *testing.T) {
	t.Parallel()
	_, s := newFake(t)
	env := map[string]string{URLVar: s.URL, KeyVar: "k3y", InsecureVar: "true"}
	c, err := FromEnv(context.Background(), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	for _, k := range []string{URLVar, KeyVar} {
		e := map[string]string{URLVar: s.URL, KeyVar: "k3y", InsecureVar: "true"}
		delete(e, k)
		if _, err := FromEnv(context.Background(), func(k string) string { return e[k] }); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("without %s: %v", k, err)
		}
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

func TestImportImage(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	f.job("vm.device.convert", "SUCCESS")
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	local, sum := image(t, "QFI\xfbdisk")
	remote := "/mnt/tank/vms/x-staging/" + sum[:16] + "-notes-1.0.0.qcow2"
	if err := c.ImportImage(ctx, local, sum, "/mnt/tank/vms/x-staging", "tank/vms/x"); err != nil {
		t.Fatal(err)
	}
	if string(f.files[remote]) != "QFI\xfbdisk" {
		t.Fatalf("uploaded %v", f.files)
	}
	// The upload's job is waited for on a new connection, logged in again.
	logins := 0
	for _, l := range f.called() {
		if strings.HasPrefix(l, "auth.login_with_api_key") {
			logins++
		}
	}
	if logins != 2 {
		t.Errorf("%d logins, want 2", logins)
	}
	calls := f.called()
	if last := calls[len(calls)-2]; !strings.HasPrefix(last, "vm.device.convert") || !strings.Contains(last, `"source":"`+remote+`"`) || !strings.Contains(last, `"destination":"tank/vms/x"`) {
		t.Fatalf("calls %q", calls)
	}
	// Staged already, at its size: converted again, not uploaded.
	if err := c.ImportImage(ctx, local, sum, "/mnt/tank/vms/x-staging", "tank/vms/x"); err != nil {
		t.Fatal(err)
	}
	uploads := 0
	for _, l := range f.called() {
		if strings.HasPrefix(l, "upload ") {
			uploads++
		}
	}
	if uploads != 1 {
		t.Fatalf("%d uploads, want 1", uploads)
	}
	// A file that is not the image named is refused before anything is sent.
	n := len(f.called())
	if err := c.ImportImage(ctx, local, strings.Repeat("0", 64), "/s", "tank/vms/x"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("a wrong digest: %v", err)
	}
	if len(f.called()) != n {
		t.Fatal("a wrong digest reached the host")
	}
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

func TestPower(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	state := map[int64]string{1: "RUNNING", 2: "STOPPED"}
	f.methods["vm.query"] = func(p []json.RawMessage) (any, *rpcErr) {
		var filter [][]any
		_ = json.Unmarshal(p[0], &filter)
		st, ok := state[int64(filter[0][2].(float64))]
		if !ok {
			return []any{}, nil
		}
		return []map[string]any{{"status": map[string]any{"state": st}}}, nil
	}
	f.methods["vm.start"] = func([]json.RawMessage) (any, *rpcErr) { return true, nil }
	f.job("vm.stop", "SUCCESS")
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []int64{1, 2} {
		if err := c.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := c.Stop(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Start(ctx, 3); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a missing VM: %v", err)
	}
	if err := c.Stop(ctx, 3); err != nil {
		t.Fatalf("stopping a missing VM: %v", err)
	}
	var starts, stops []string
	for _, l := range f.called() {
		if strings.HasPrefix(l, "vm.start") {
			starts = append(starts, l)
		}
		if strings.HasPrefix(l, "vm.stop") {
			stops = append(stops, l)
		}
	}
	if len(starts) != 1 || starts[0] != "vm.start 2" {
		t.Errorf("starts %q, want only the stopped VM", starts)
	}
	if len(stops) != 1 || stops[0] != `vm.stop 1 {"force_after_timeout":true}` {
		t.Errorf("stops %q, want only the running VM, forced after its timeout", stops)
	}
}

func TestDropSnapshots(t *testing.T) {
	t.Parallel()
	f, s := newFake(t)
	f.methods["pool.snapshot.query"] = func([]json.RawMessage) (any, *rpcErr) {
		return []map[string]any{{"id": "tank/x@a"}, {"id": "tank/x@b"}}, nil
	}
	f.methods["pool.snapshot.delete"] = func([]json.RawMessage) (any, *rpcErr) { return true, nil }
	c, err := dial(t, s, "k3y")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DropSnapshots(context.Background(), "tank/x"); err != nil {
		t.Fatal(err)
	}
	var del []string
	for _, l := range f.called() {
		if strings.HasPrefix(l, "pool.snapshot") {
			del = append(del, l)
		}
	}
	want := []string{`pool.snapshot.query [["dataset","=","tank/x"]] {"select":["id"]}`, `pool.snapshot.delete "tank/x@a"`, `pool.snapshot.delete "tank/x@b"`}
	if strings.Join(del, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls\n%s\nwant\n%s", strings.Join(del, "\n"), strings.Join(want, "\n"))
	}
}
