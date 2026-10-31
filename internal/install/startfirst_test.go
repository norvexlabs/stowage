package install

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/tune"
)

// engineState is what an overlap reads of the engine: each service's containers, as inspect
// renders them (id|name|state|exit|health|hash|image|volumes), and its configuration's digest. Every image
// is sha256:img. The overlap's `up` fails with upErr, and adds api's new1.
type engineState struct {
	containers map[string][]string
	hash       map[string]string
	upErr      error
	answer     func(a string) ([]byte, bool, error) // answers a call first, when it does
	// joins are the engine's running containers as the join check reads them
	// (id|name|network|pid|ipc|volumes_from).
	joins []string
	// news are new1's states as an `up` without --wait leaves it, one more each time it is read
	// (the last stays).
	news []string
}

func (e *engineState) docker(cfg string) *fakeDocker {
	f := &fakeDocker{}
	byID := map[string]string{}
	for _, cs := range e.containers {
		for _, c := range cs {
			id, _, _ := strings.Cut(c, "|")
			byID[id] = c
		}
	}
	f.answer = func(a string) ([]byte, bool, error) {
		args := strings.Fields(a)
		if e.answer != nil {
			if out, ok, err := e.answer(a); ok {
				return out, ok, err
			}
		}
		last := args[len(args)-1]
		switch {
		case strings.HasSuffix(a, " config --format json"):
			return []byte(cfg), true, nil
		case strings.Contains(a, " ps --all --quiet "):
			var ids []string
			for _, c := range e.containers[last] {
				id, _, _ := strings.Cut(c, "|")
				ids = append(ids, id)
			}
			return []byte(strings.Join(ids, "\n")), true, nil
		case strings.Contains(a, " config --hash "):
			return []byte(last + " " + e.hash[last] + "\n"), true, nil
		case strings.HasPrefix(a, "image inspect --format {{.Id}} "):
			return []byte("sha256:img\n"), true, nil
		case a == "ps --all --quiet --no-trunc":
			var ids []string
			for _, j := range e.joins {
				id, _, _ := strings.Cut(j, "|")
				ids = append(ids, id)
			}
			return []byte(strings.Join(ids, "\n")), true, nil
		case strings.HasPrefix(a, "inspect --type container --format ") && strings.Contains(a, "HostConfig.NetworkMode"):
			var out []string
			for _, j := range e.joins {
				id, _, _ := strings.Cut(j, "|")
				if slices.Contains(args, id) {
					if strings.HasPrefix(j, "gone|") {
						return nil, true, errors.New("Error: No such object: gone")
					}
					if strings.HasPrefix(j, "busy|") {
						return nil, true, errors.New("Cannot connect to the Docker daemon")
					}
					out = append(out, j)
				}
			}
			return []byte(strings.Join(out, "\n") + "\n"), true, nil
		case strings.HasPrefix(a, "inspect --type container --format "):
			var out []string
			for _, id := range args {
				if c, ok := byID[id]; ok {
					out = append(out, c)
				}
			}
			if slices.Contains(args, "new1") && len(e.news) > 1 {
				e.news = e.news[1:]
				byID["new1"] = e.news[0]
			}
			return []byte(strings.Join(out, "\n") + "\n"), true, nil
		case strings.Contains(a, " up --detach --no-deps --no-recreate --scale "):
			e.containers["api"] = append(e.containers["api"], "new1")
			byID["new1"] = e.news[0]
			return nil, true, e.upErr
		case strings.HasPrefix(a, "kill "):
			return nil, true, nil
		case strings.Contains(a, " up --detach --wait --no-deps --no-recreate --scale "):
			e.containers["api"] = append(e.containers["api"], "new1|/p-new1|running|0|healthy|h2|sha256:img|")
			byID["new1"] = "new1|/p-new1|running|0|healthy|h2|sha256:img|"
			return nil, true, e.upErr
		case strings.HasPrefix(a, "stop "), strings.HasPrefix(a, "rm "):
			return nil, true, nil
		}
		return nil, false, nil
	}
	return f
}

// engine is the calls an overlap made of the engine, but its reads.
func (f *fakeDocker) engine() []string {
	var out []string
	for _, c := range f.calls {
		a := strings.Join(c.Args, " ")
		if i := strings.Index(a, " up "); i >= 0 {
			a = a[i+1:]
		}
		if strings.HasPrefix(a, "up ") || strings.HasPrefix(a, "stop ") || strings.HasPrefix(a, "rm ") || strings.HasPrefix(a, "kill ") {
			out = append(out, a)
		}
	}
	return out
}

// A start-first service whose running container differs from what the start runs gets its new
// container beside the old one, which stops (its grace period, Docker's own) and goes once the new
// is healthy; one that differs in nothing is left; a new container that fails is removed and the
// old one serves on; one that none runs is the stack's start's; and the stack's start replaces it
// stop-first when the overlap could not keep it serving or would start it against what the start
// changes.
func TestAStartFirstServiceOverlaps(t *testing.T) {
	t.Parallel()
	const model = `{"services": {"api": {"image": "reg/api:2", "depends_on": {"db": {}}, "volumes": [{"type": "volume", "source": "data"}]},
		"db": {"image": "reg/db:1", "depends_on": {"init": {"condition": "service_completed_successfully"}}}, "init": {"image": "reg/init:1"}}, "volumes": {"data": {"name": "p_data"}}}`
	var m composeModel
	if err := json.Unmarshal([]byte(model), &m); err != nil {
		t.Fatal(err)
	}
	const cfg = `{"services": {"api": {}}}`
	const oldAPI = "old1|/p-old1|running|0|healthy|h1|sha256:img|p_data,"
	deps := func() map[string][]string {
		return map[string][]string{"api": {oldAPI}, "db": {"db1|/p-db1|running|0||d1|sha256:img|"}, "init": {"init1|/p-init1|exited|0||i1|sha256:img|"}}
	}
	hashes := map[string]string{"api": "h2", "db": "d1", "init": "i1"}
	// The last start ran the networks of a configuration without any.
	none := sha256.Sum256([]byte("null"))
	run := func(e *engineState, cfg string, fresh ...string) (*fakeDocker, string, error) {
		r := testRelease(t, "1.0.0")
		r.Spec.Services = map[string]spec.Service{"api": {Update: spec.UpdateStartFirst}, "gone": {Update: spec.UpdateStartFirst}}
		var log bytes.Buffer
		f := e.docker(cfg)
		in := &Installer{R: r, O: Options{Dir: t.TempDir(), Docker: f, Log: &log}}
		if err := os.WriteFile(in.path(NetworksFile), []byte(hex.EncodeToString(none[:])+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := in.takeNetworks(); err != nil {
			t.Fatal(err)
		}
		err := in.startFirst(context.Background(), nil, &m, fresh)
		return f, log.String(), err
	}
	up := "up --detach --wait --no-deps --no-recreate --scale api=2 api"
	f, log, err := run(&engineState{containers: deps(), hash: hashes}, cfg)
	if err != nil || !slices.Equal(f.engine(), []string{up, "stop old1", "rm old1"}) || !strings.Contains(log, "the new container is healthy, the old one stopped") {
		t.Errorf("a changed configuration: %v, ran %q, logged %s", err, f.engine(), log)
	}
	c := deps()
	c["api"] = []string{"old1|/p-old1|running|0|healthy|h2|sha256:other|"}
	if f, _, err := run(&engineState{containers: c, hash: hashes}, cfg); err != nil || !slices.Equal(f.engine(), []string{up, "stop old1", "rm old1"}) {
		t.Errorf("a changed image: %v, ran %q", err, f.engine())
	}
	same := maps.Clone(hashes)
	same["api"] = "h1"
	if f, _, err := run(&engineState{containers: deps(), hash: same}, cfg); err != nil || len(f.engine()) != 0 {
		t.Errorf("unchanged: %v, ran %q", err, f.engine())
	}
	c = deps()
	delete(c, "api")
	if f, _, err := run(&engineState{containers: c, hash: hashes}, cfg); err != nil || len(f.engine()) != 0 {
		t.Errorf("none: %v, ran %q", err, f.engine())
	}
	f, _, err = run(&engineState{containers: deps(), hash: hashes, upErr: errors.New("unhealthy")}, cfg)
	if err == nil || !strings.Contains(err.Error(), "services.api: its new container did not become healthy beside the old one, which serves on: unhealthy") ||
		!slices.Equal(f.engine(), []string{up, "rm --force new1"}) {
		t.Errorf("a new container that fails: %v, ran %q", err, f.engine())
	}
	with := func(service string, cs ...string) map[string][]string {
		c := deps()
		c[service] = cs
		return c
	}
	for _, c := range []struct {
		cfg, fresh string
		containers map[string][]string
		why        string
	}{
		{`{"services": {"api": {"ports": [{"target": 80, "published": "8080"}]}}}`, "", deps(), "its ports are published from its own network"},
		{`{"services": {"api": {"container_name": "api"}}}`, "", deps(), "its container has a fixed name"},
		{`{"services": {"api": {"networks": {"default": {"ipv4_address": "172.30.0.5"}}}}}`, "", deps(), "its container has a fixed address"},
		{`{"services": {"api": {"mac_address": "02:42:ac:11:00:02"}}}`, "", deps(), "its container has a fixed address"},
		{`{"services": {"api": {"network_mode": "service:vpn"}}}`, "", deps(), "it runs in another's network namespace"},
		{`{"services": {"api": {}, "side": {"pid": "service:api"}}}`, "", deps(), "side joins its namespaces, mounts its volumes or links to it"},
		{`{"services": {"api": {}, "side": {"volumes_from": ["api:ro"]}}}`, "", deps(), "side joins its namespaces, mounts its volumes or links to it"},
		{cfg, "p_data", deps(), "a volume it mounts awaits its uid"},
		{cfg, "", with("api", oldAPI, "old2|/p-old2|exited|0||h1|sha256:img|"), "it has 2 containers"},
		{cfg, "", with("api", "old1|/p-old1|restarting|0||h1|sha256:img|"), "its container is not running"},
		{cfg, "", with("api", "old1|/p-old1|running|0|unhealthy|h1|sha256:img|"), "its container is not healthy"},
		{cfg, "", with("api", "old1|/p-old1|running|0|healthy|h1|sha256:img|p_data,"+strings.Repeat("ab", 32)+","), "it has an anonymous volume, which only a recreate keeps"},
		{cfg, "", with("init"), "this start makes or changes its dependency init, or finds it not serving"},
		{cfg, "", with("db", "db1|/p-db1|running|0||d0|sha256:img|"), "this start makes or changes its dependency db, or finds it not serving"},
		{cfg, "", with("init", "init1|/p-init1|exited|1||i1|sha256:img|"), "this start makes or changes its dependency init, or finds it not serving"},
		{cfg, "", with("init", "init1|/p-init1|running|0||i1|sha256:img|"), "this start makes or changes its dependency init, or finds it not serving"},
		{cfg, "", with("db", "db1|/p-db1|exited|0||d1|sha256:img|"), "this start makes or changes its dependency db, or finds it not serving"},
		{cfg, "", with("db", "db1|/p-db1|running|0|starting|d1|sha256:img|"), "this start makes or changes its dependency db, or finds it not serving"},
		{`{"services": {"api": {}}, "networks": {"default": {"driver_opts": {"com.docker.network.driver.mtu": "1400"}}}}`, "", deps(), "this start changes the stack's networks"},
	} {
		f, log, err := run(&engineState{containers: c.containers, hash: hashes}, c.cfg, c.fresh)
		if err != nil || len(f.engine()) != 0 || !strings.Contains(log, "services.api.update: "+c.why+", so this start replaces it stop-first") {
			t.Errorf("%s: %v, ran %q, logged %s", c.why, err, f.engine(), log)
		}
	}
	if f, _, err := run(&engineState{containers: deps(), hash: hashes}, `{"services": {"api": {"network_mode": "host", "ports": [{"target": 80}]}}}`); err != nil || len(f.engine()) != 3 {
		t.Errorf("on the host's network: %v, ran %q", err, f.engine())
	}
	// A running container the engine records in the old one's namespace replaces it stop-first.
	if f, log, err := run(&engineState{containers: deps(), hash: hashes, joins: []string{"s1|/side|container:old1||||"}}, cfg); err != nil || len(f.engine()) != 0 ||
		!strings.Contains(log, "services.api.update: the container side joins its container's namespaces, mounts its volumes or links to it, so this start replaces it stop-first") {
		t.Errorf("a container joining the old one: %v, ran %q, logged %s", err, f.engine(), log)
	}
	// A one-shot dependency (db waits for init to complete) that exited 0, and one of several
	// containers, serve.
	c = deps()
	c["init"] = []string{"init1|/p-init1|exited|0||i1|sha256:img|"}
	c["db"] = []string{"db1|/p-db1|running|0||d1|sha256:img|", "db2|/p-db2|running|0||d1|sha256:img|"}
	if f, _, err := run(&engineState{containers: c, hash: hashes}, cfg); err != nil || len(f.engine()) != 3 {
		t.Errorf("dependencies that serve: %v, ran %q", err, f.engine())
	}
	// An unchanged service is left whatever its state, and nothing is logged.
	c = deps()
	c["api"] = []string{"old1|/p-old1|running|0|starting|h2|sha256:img|"}
	if f, log, err := run(&engineState{containers: c, hash: hashes}, `{"services": {"api": {"container_name": "api"}}}`); err != nil || len(f.engine()) != 0 || log != "" {
		t.Errorf("unchanged and starting: %v, ran %q, logged %s", err, f.engine(), log)
	}
	// An image it cannot read, and a container it cannot read, fail the start.
	failImage := func(a string) ([]byte, bool, error) {
		return nil, strings.HasPrefix(a, "image inspect "), errors.New("no such image")
	}
	if _, _, err := run(&engineState{containers: deps(), hash: hashes, answer: failImage}, cfg); err == nil || !strings.Contains(err.Error(), "services.api: its image reg/api:2: no such image") {
		t.Errorf("an image it cannot read: %v", err)
	}
	short := func(a string) ([]byte, bool, error) {
		return []byte("old1|running\n"), strings.HasPrefix(a, "inspect "), nil
	}
	if _, _, err := run(&engineState{containers: deps(), hash: hashes, answer: short}, cfg); err == nil || !strings.Contains(err.Error(), `services.api: reading its containers: "old1|running\n"`) {
		t.Errorf("a container it cannot read: %v", err)
	}
	// Start-first services are overlapped dependencies first: auth, which api depends on, then api.
	var both composeModel
	if err := json.Unmarshal([]byte(`{"services": {"api": {"depends_on": {"auth": {}}}, "auth": {}, "zz": {}}}`), &both); err != nil {
		t.Fatal(err)
	}
	if got := dependencyOrder(&both, []string{"api", "auth", "zz"}); !slices.Equal(got, []string{"auth", "api", "zz"}) {
		t.Errorf("order %v", got)
	}
	// A dependency is a one-shot only when every service of the closure depending on it waits for
	// it to complete.
	var mixed composeModel
	if err := json.Unmarshal([]byte(`{"services": {"api": {"depends_on": {"x": {"condition": "service_healthy"}, "y": {}}},
		"y": {"depends_on": {"x": {"condition": "service_completed_successfully"}, "z": {"condition": "service_completed_successfully"}}}, "x": {}, "z": {}}}`), &mixed); err != nil {
		t.Fatal(err)
	}
	if got := dependencies(&mixed, "api"); !maps.Equal(got, map[string]bool{"x": false, "y": false, "z": true}) {
		t.Errorf("dependencies %v", got)
	}
}

// With update_signal, the new container starts without waiting, the old one is sent the signal
// once the new one runs, and the old one stops once the new one is healthy; a new one that ends
// or turns unhealthy is removed, the old one sent the signal or not as far as the new one ran.
func TestAnUpdateSignalTellsTheOldContainerToGo(t *testing.T) {
	overlapPoll = time.Millisecond
	t.Cleanup(func() { overlapPoll = 500 * time.Millisecond })
	const model = `{"services": {"api": {"image": "reg/api:2"}}}`
	var m composeModel
	if err := json.Unmarshal([]byte(model), &m); err != nil {
		t.Fatal(err)
	}
	none := sha256.Sum256([]byte("null"))
	run := func(news ...string) (*fakeDocker, string, error) {
		r := testRelease(t, "1.0.0")
		r.Spec.Services = map[string]spec.Service{"api": {Update: spec.UpdateStartFirst, UpdateSignal: "SIGUSR2"}}
		var log bytes.Buffer
		e := &engineState{containers: map[string][]string{"api": {"old1|/p-old1|running|0|healthy|h1|sha256:img|"}}, hash: map[string]string{"api": "h2"}, news: news}
		f := e.docker(`{"services": {"api": {}}}`)
		in := &Installer{R: r, O: Options{Dir: t.TempDir(), Docker: f, Log: &log}}
		if err := os.WriteFile(in.path(NetworksFile), []byte(hex.EncodeToString(none[:])+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := in.takeNetworks(); err != nil {
			t.Fatal(err)
		}
		err := in.startFirst(context.Background(), nil, &m, nil)
		return f, log.String(), err
	}
	const (
		up      = "up --detach --no-deps --no-recreate --scale api=2 api"
		kill    = "kill --signal SIGUSR2 old1"
		created = "new1|/p-new1|created|0||h2|sha256:img|"
		start   = "new1|/p-new1|running|0|starting|h2|sha256:img|"
		healthy = "new1|/p-new1|running|0|healthy|h2|sha256:img|"
	)
	f, log, err := run(created, start, start, healthy)
	if err != nil || !slices.Equal(f.engine(), []string{up, kill, "stop old1", "rm old1"}) ||
		!strings.Contains(log, "services.api: the new container runs: SIGUSR2 sent to the old one (update_signal)") {
		t.Errorf("a new container that becomes healthy: %v, ran %q, logged %s", err, f.engine(), log)
	}
	if f, _, err := run("new1|/p-new1|running|0||h2|sha256:img|"); err != nil || !slices.Equal(f.engine(), []string{up, kill, "stop old1", "rm old1"}) {
		t.Errorf("a new container without a healthcheck: %v, ran %q", err, f.engine())
	}
	f, _, err = run(start, "new1|/p-new1|running|0|unhealthy|h2|sha256:img|")
	if err == nil || !strings.Contains(err.Error(), "services.api: its new container did not become healthy beside the old one, which was sent SIGUSR2 (update_signal) and serves on as far as the app keeps serving after it: its new container /p-new1 is unhealthy") ||
		!slices.Equal(f.engine(), []string{up, kill, "rm --force new1"}) {
		t.Errorf("a new container that turns unhealthy: %v, ran %q", err, f.engine())
	}
	f, _, err = run(created, "new1|/p-new1|exited|1||h2|sha256:img|")
	if err == nil || !strings.Contains(err.Error(), "beside the old one, which serves on: its new container /p-new1 is exited (exit code 1)") || !slices.Equal(f.engine(), []string{up, "rm --force new1"}) {
		t.Errorf("a new container that ends before it runs: %v, ran %q", err, f.engine())
	}
}

// overlapRun runs startFirst on api, start-first with svc's update keys, its old container old1
// running healthy and new1 taking the states news; answer answers a call first, when it does.
func overlapRun(t *testing.T, svc spec.Service, answer func(a string) ([]byte, bool, error), news ...string) (*fakeDocker, string, error) {
	t.Helper()
	var m composeModel
	if err := json.Unmarshal([]byte(`{"name": "p", "services": {"api": {"image": "reg/api:2"}}}`), &m); err != nil {
		t.Fatal(err)
	}
	none := sha256.Sum256([]byte("null"))
	r := testRelease(t, "1.0.0")
	svc.Update = spec.UpdateStartFirst
	r.Spec.Services = map[string]spec.Service{"api": svc}
	var log bytes.Buffer
	e := &engineState{containers: map[string][]string{"api": {"old1|/p-old1|running|0|healthy|h1|sha256:img|"}}, hash: map[string]string{"api": "h2"}, news: news, answer: answer}
	f := e.docker(`{"services": {"api": {}}}`)
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Docker: f, Log: &log}}
	if err := os.WriteFile(in.path(NetworksFile), []byte(hex.EncodeToString(none[:])+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := in.takeNetworks(); err != nil {
		t.Fatal(err)
	}
	// A wait the change under test leaves unbounded ends the test, not the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := in.startFirst(ctx, nil, &m, nil)
	return f, log.String(), err
}

// tarOf is the archive `docker cp … -` writes of one empty file modified at mod.
func tarOf(t *testing.T, name string, mod time.Time) []byte {
	t.Helper()
	return tarOfContent(t, name, mod, "")
}

// tarOfContent is the archive of one file of content modified at mod.
func tarOfContent(t *testing.T, name string, mod time.Time, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, ModTime: mod, Typeflag: tar.TypeReg, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// With update_ready, the old container is sent update_signal only once the new one holds that file
// in update_socket as it was not before the new one started (none, or another than an earlier
// overlap's, read through the old container); a new one that never writes it in readyWait, or ends
// first, is removed and the old one is never signalled.
func TestAnUpdateReadyFileHoldsTheSignal(t *testing.T) {
	overlapPoll = time.Millisecond
	t.Cleanup(func() { overlapPoll = 500 * time.Millisecond; readyWait = 10 * time.Minute })
	svc := spec.Service{UpdateSocket: "/run/hand", UpdateSignal: "SIGUSR2", UpdateReady: "ready"}
	const (
		up      = "up --detach --no-deps --no-recreate --scale api=2 api"
		kill    = "kill --signal SIGUSR2 old1"
		start   = "new1|/p-new1|running|0|starting|h2|sha256:img|"
		healthy = "new1|/p-new1|running|0|healthy|h2|sha256:img|"
	)
	missing := errors.New("Error: No such container:path: new1:/run/hand/ready")
	// mounts is where old1 mounts the update volume ("" none).
	mounts := "/run/hand"
	// answer answers the old container's read of the file with old, and the new one's with news in
	// turn, the last again; nil is a file not there.
	answer := func(old []byte, oldErr error, news ...[]byte) func(a string) ([]byte, bool, error) {
		return func(a string) ([]byte, bool, error) {
			switch a {
			case "inspect --type container --format {{json .Mounts}} old1":
				if mounts == "" {
					return []byte(`[{"Type":"volume","Name":"p_other","Destination":"/data"}]` + "\n"), true, nil
				}
				return []byte(`[{"Type":"bind","Name":"","Destination":"/etc/x"},{"Type":"volume","Name":"p_stowage-update-api","Destination":"` + mounts + `"}]` + "\n"), true, nil
			case "cp old1:" + mounts + "/ready -":
				if old == nil {
					return nil, true, cmp.Or(oldErr, missing)
				}
				return old, true, nil
			case "cp new1:/run/hand/ready -":
				out := news[0]
				if len(news) > 1 {
					news = news[1:]
				}
				if out == nil {
					return nil, true, missing
				}
				return out, true, nil
			}
			return nil, false, nil
		}
	}
	// signalledAfter is the new container's reads of the file before the old one was signalled; -1
	// when it never was.
	signalledAfter := func(f *fakeDocker) int {
		reads := 0
		for _, c := range f.calls {
			switch a := strings.Join(c.Args, " "); {
			case a == "cp new1:/run/hand/ready -":
				reads++
			case a == kill:
				return reads
			}
		}
		return -1
	}
	t0 := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	stale, fresh := tarOf(t, "ready", t0), tarOf(t, "ready", t0.Add(time.Second))
	// An earlier overlap's file: the signal waits for one that differs.
	f, log, err := overlapRun(t, svc, answer(stale, nil, nil, stale, fresh), start, healthy)
	if err != nil || !slices.Equal(f.engine(), []string{up, kill, "stop old1", "rm old1"}) || signalledAfter(f) != 3 ||
		!strings.Contains(log, "services.api: the new container wrote /run/hand/ready (update_ready)") {
		t.Errorf("a new container that writes the file: %v, ran %q, signalled after %d reads, logged %s", err, f.engine(), signalledAfter(f), log)
	}
	// None before: any file the new one holds is its own, whatever its time (an earlier one's
	// included: the clocks are not compared).
	f, _, err = overlapRun(t, svc, answer(nil, nil, nil, stale), start, healthy)
	if err != nil || signalledAfter(f) != 2 {
		t.Errorf("none before: %v, signalled after %d reads", err, signalledAfter(f))
	}
	// The same time (whole seconds, or a copy keeping it), another content: written.
	same := tarOfContent(t, "ready", t0, "2")
	f, _, err = overlapRun(t, svc, answer(tarOfContent(t, "ready", t0, "1"), nil, tarOfContent(t, "ready", t0, "1"), same), start, healthy)
	if err != nil || signalledAfter(f) != 2 {
		t.Errorf("another content at the same time: %v, signalled after %d reads", err, signalledAfter(f))
	}
	// An earlier release's update_socket elsewhere: the old container's mount is read.
	mounts = "/run/old"
	f, _, err = overlapRun(t, svc, answer(stale, nil, stale, fresh), start, healthy)
	if err != nil || signalledAfter(f) != 2 {
		t.Errorf("the volume at another directory: %v, signalled after %d reads", err, signalledAfter(f))
	}
	// An old container without the volume: no handoff to make, stop-first.
	mounts = ""
	f, log, err = overlapRun(t, svc, answer(nil, nil, fresh), start, healthy)
	if err != nil || len(f.engine()) != 0 || !strings.Contains(log, "services.api.update: its old container does not mount the update_socket volume (no socket to hand over), so this start replaces it stop-first") {
		t.Errorf("no volume: %v, ran %q, logged %s", err, f.engine(), log)
	}
	// So without update_ready too: the signal would ask for a handoff it cannot make.
	noReady := svc
	noReady.UpdateReady = ""
	if f, log, err := overlapRun(t, noReady, answer(nil, nil), start, healthy); err != nil || len(f.engine()) != 0 || !strings.Contains(log, "does not mount the update_socket volume (no socket to hand over), so this start replaces it stop-first") {
		t.Errorf("no volume, no update_ready: %v, ran %q, logged %s", err, f.engine(), log)
	}
	// A volume of the name's suffix, another project's: not the service's.
	mounts = "/run/hand"
	if f, log, err := overlapRun(t, svc, func(a string) ([]byte, bool, error) {
		if a == "inspect --type container --format {{json .Mounts}} old1" {
			return []byte(`[{"Type":"volume","Name":"q_stowage-update-api","Destination":"/run/hand"}]`), true, nil
		}
		return answer(nil, nil, fresh)(a)
	}, start, healthy); err != nil || len(f.engine()) != 0 || !strings.Contains(log, "stop-first") {
		t.Errorf("another project's volume: %v, ran %q, logged %s", err, f.engine(), log)
	}
	// The old container's read failing otherwise (a connection's error naming a missing socket
	// included): the overlap does not start.
	f, log, err = overlapRun(t, svc, answer(nil, errors.New("error during connect: dial unix /run/docker.sock: connect: no such file or directory")), start)
	if err == nil || err.Error() != "services.api: before its new container starts, which it does not: reading /run/hand/ready (update_ready) through its old container: error during connect: dial unix /run/docker.sock: connect: no such file or directory" || len(f.engine()) != 0 || strings.Contains(log, "stop-first") {
		t.Errorf("an old read failing: %v, ran %q, logged %s", err, f.engine(), log)
	}
	readyWait = 20 * time.Millisecond
	f, _, err = overlapRun(t, svc, answer(stale, nil, stale), start)
	if err == nil || !strings.Contains(err.Error(), "which serves on: its new container /p-new1 did not write /run/hand/ready (update_ready) within 20ms") ||
		!slices.Equal(f.engine(), []string{up, "rm --force new1"}) {
		t.Errorf("a new container that never writes it: %v, ran %q", err, f.engine())
	}
	readyWait = 10 * time.Minute
	f, _, err = overlapRun(t, svc, answer(nil, nil, nil), start, start, "new1|/p-new1|exited|1||h2|sha256:img|")
	if err == nil || !strings.Contains(err.Error(), "which serves on: its new container /p-new1 is exited (exit code 1)") ||
		!slices.Equal(f.engine(), []string{up, "rm --force new1"}) {
		t.Errorf("a new container that ends during its setup: %v, ran %q", err, f.engine())
	}
}

// A new container that ends with update_decline_status before the old one is signalled declines
// the overlap: it is removed and the stack's start replaces the service stop-first, with or
// without update_signal; another status, or one after the signal, fails the start.
func TestANewContainerDeclinesTheOverlap(t *testing.T) {
	overlapPoll = time.Millisecond
	t.Cleanup(func() { overlapPoll = 500 * time.Millisecond })
	const (
		up       = "up --detach --no-deps --no-recreate --scale api=2 api"
		created  = "new1|/p-new1|created|0||h2|sha256:img|"
		start    = "new1|/p-new1|running|0|starting|h2|sha256:img|"
		declines = "new1|/p-new1|exited|75||h2|sha256:img|"
		says     = "services.api.update: its new container declined the overlap (exit status 75, update_decline_status), so this start replaces it stop-first"
	)
	decline75 := 75
	signal := spec.Service{UpdateSignal: "SIGUSR2", UpdateDeclineStatus: &decline75}
	f, log, err := overlapRun(t, signal, nil, created, declines)
	if err != nil || !slices.Equal(f.engine(), []string{up, "rm --force new1"}) || !strings.Contains(log, says) {
		t.Errorf("a decline before the signal: %v, ran %q, logged %s", err, f.engine(), log)
	}
	// Its restart policy restarting it: the engine keeps the exit code meanwhile.
	f, log, err = overlapRun(t, signal, nil, created, "new1|/p-new1|restarting|75||h2|sha256:img|")
	if err != nil || !slices.Equal(f.engine(), []string{up, "rm --force new1"}) || !strings.Contains(log, says) {
		t.Errorf("a decline the restart policy restarts: %v, ran %q, logged %s", err, f.engine(), log)
	}
	f, _, err = overlapRun(t, signal, nil, created, "new1|/p-new1|exited|1||h2|sha256:img|")
	if err == nil || !strings.Contains(err.Error(), "exited (exit code 1)") || !slices.Equal(f.engine(), []string{up, "rm --force new1"}) {
		t.Errorf("another exit status: %v, ran %q", err, f.engine())
	}
	f, _, err = overlapRun(t, signal, nil, start, declines)
	if err == nil || !strings.Contains(err.Error(), "which was sent SIGUSR2 (update_signal)") || !strings.Contains(err.Error(), "exited (exit code 75)") ||
		!slices.Equal(f.engine(), []string{up, "kill --signal SIGUSR2 old1", "rm --force new1"}) {
		t.Errorf("a decline after the signal: %v, ran %q", err, f.engine())
	}
	// Without update_signal, up --wait fails on the new container's end, which is then read.
	wait := func(exit string) func(a string) ([]byte, bool, error) {
		upped := false
		return func(a string) ([]byte, bool, error) {
			switch {
			case strings.Contains(a, " up --detach --wait "):
				upped = true
				return nil, true, errors.New("container p-new1 exited (" + exit + ")")
			case upped && strings.Contains(a, " ps --all --quiet api"):
				return []byte("old1\nnew1\n"), true, nil
			case upped && a == "inspect --type container --format "+containerFormat+" old1 new1":
				return []byte("old1|/p-old1|running|0|healthy|h1|sha256:img|\nnew1|/p-new1|exited|" + exit + "||h2|sha256:img|\n"), true, nil
			}
			return nil, false, nil
		}
	}
	waitUp := "up --detach --wait --no-deps --no-recreate --scale api=2 api"
	f, log, err = overlapRun(t, spec.Service{UpdateDeclineStatus: &decline75}, wait("75"))
	if err != nil || !slices.Equal(f.engine(), []string{waitUp, "rm --force new1"}) || !strings.Contains(log, says) {
		t.Errorf("a decline under up --wait: %v, ran %q, logged %s", err, f.engine(), log)
	}
	f, _, err = overlapRun(t, spec.Service{UpdateDeclineStatus: &decline75}, wait("1"))
	if err == nil || !strings.Contains(err.Error(), "did not become healthy beside the old one, which serves on: container p-new1 exited (1)") ||
		!slices.Equal(f.engine(), []string{waitUp, "rm --force new1"}) {
		t.Errorf("another status under up --wait: %v, ran %q", err, f.engine())
	}
}

// The engine renders an inspect template over the container's JSON, failing on a key it lacks: a
// container without a healthcheck has no State.Health, and reads as one with no health.
func TestTheContainerFormatReadsAContainerWithoutAHealthcheck(t *testing.T) {
	t.Parallel()
	tpl := template.Must(template.New("").Option("missingkey=error").Parse(containerFormat))
	c := func(state map[string]any) map[string]any {
		return map[string]any{"Id": "c1", "Name": "/p-api-1", "State": state, "Image": "sha256:img",
			"Config": map[string]any{"Labels": map[string]any{hashLabel: "h1"}},
			"Mounts": []any{map[string]any{"Type": "volume", "Name": "p_data"}, map[string]any{"Type": "bind", "Name": ""}}}
	}
	for _, tc := range []struct {
		state map[string]any
		want  string
	}{
		{map[string]any{"Status": "running", "ExitCode": 0}, "c1|/p-api-1|running|0||h1|sha256:img|p_data,"},
		{map[string]any{"Status": "running", "ExitCode": 0, "Health": nil}, "c1|/p-api-1|running|0||h1|sha256:img|p_data,"},
		{map[string]any{"Status": "running", "ExitCode": 0, "Health": map[string]any{"Status": "healthy"}}, "c1|/p-api-1|running|0|healthy|h1|sha256:img|p_data,"},
	} {
		var b bytes.Buffer
		if err := tpl.Execute(&b, c(tc.state)); err != nil {
			t.Fatalf("%v: %v", tc.state, err)
		}
		if b.String() != tc.want {
			t.Errorf("%v: %q, want %q", tc.state, b.String(), tc.want)
		}
	}
}

// A start-first service's update_socket is a volume of its own mounted at the directory, which
// both containers of an overlap mount.
func TestTheUpdateSocketIsAVolume(t *testing.T) {
	t.Parallel()
	var m composeModel
	if err := json.Unmarshal([]byte(`{"services": {"api": {}, "ui": {}}}`), &m); err != nil {
		t.Fatal(err)
	}
	raw, err := contentCompose(&m, nil, []byte("key"), tune.Container{}, hostPlan{Sockets: map[string]string{"api": "/run/handoff"}}, hostNet{})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Volumes []string `yaml:"volumes"`
		} `yaml:"services"`
		Volumes map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if v := doc.Services["api"].Volumes; !slices.Equal(v, []string{"stowage-update-api:/run/handoff"}) || len(doc.Services["ui"].Volumes) != 0 {
		t.Errorf("volumes %v:\n%s", v, raw)
	}
	if _, ok := doc.Volumes["stowage-update-api"]; !ok || len(doc.Volumes) != 1 {
		t.Errorf("top-level volumes %v", doc.Volumes)
	}
}

// The networks' record: none, an unreadable one, or another start's replaces a changed start-first
// service stop-first; the start's own overlaps it, laid out otherwise too; takeNetworks removes it
// as the start begins, so only a start that comes up leaves one, and one it cannot remove fails
// the start.
func TestTheNetworksRecord(t *testing.T) {
	t.Parallel()
	var m composeModel
	if err := json.Unmarshal([]byte(`{"services": {"api": {"image": "reg/api:2"}}}`), &m); err != nil {
		t.Fatal(err)
	}
	r := testRelease(t, "1.0.0")
	r.Spec.Services = map[string]spec.Service{"api": {Update: spec.UpdateStartFirst}}
	dir := t.TempDir()
	record := filepath.Join(dir, NetworksFile)
	start := func(cfg string) (*fakeDocker, string, error) {
		e := &engineState{containers: map[string][]string{"api": {"old1|/p-old1|running|0||h1|sha256:img|"}}, hash: map[string]string{"api": "h2"}}
		var log bytes.Buffer
		f := e.docker(cfg)
		in := &Installer{R: r, O: Options{Dir: dir, Docker: f, Log: &log}}
		if err := in.takeNetworks(); err != nil {
			return f, log.String(), err
		}
		if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("takeNetworks left the record: %v", err)
		}
		if err := in.startFirst(context.Background(), nil, &m, nil); err != nil {
			in.keepNetworks()
			return f, log.String(), err
		}
		return f, log.String(), in.recordNetworks()
	}
	const cfg = `{"services": {"api": {}}, "networks": {"default": {}}}`
	for _, c := range []struct {
		name, seed, why string
		dir             bool
	}{
		{"none", "", "no start that came up recorded the stack's networks", false},
		{"another start's", "other\n", "this start changes the stack's networks", false},
		{"unreadable", "", "the record of the stack's networks cannot be read", true},
	} {
		if c.seed != "" {
			if err := os.WriteFile(record, []byte(c.seed), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if c.dir {
			if err := os.Mkdir(record, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if f, log, err := start(cfg); err != nil || len(f.engine()) != 0 || !strings.Contains(log, c.why) || !strings.Contains(log, "so this start replaces it stop-first") {
			t.Fatalf("%s: %v, ran %q, logged %s", c.name, err, f.engine(), log)
		}
		if err := os.Remove(record); err != nil { // the start came up: its own record
			t.Fatal(err)
		}
	}
	// Its own record, written by a start that came up, laid out otherwise: the overlap runs.
	if _, _, err := start(cfg); err != nil {
		t.Fatal(err)
	}
	if f, _, err := start(`{"networks":{"default":{}},"services":{"api":{}}}`); err != nil || !slices.Equal(f.engine(), []string{"up --detach --wait --no-deps --no-recreate --scale api=2 api", "stop old1", "rm old1"}) {
		t.Fatalf("its own record: %v, ran %q", err, f.engine())
	}
	// A record it cannot remove fails the start, naming it.
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(record, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := start(cfg); err == nil || !strings.Contains(err.Error(), record+", the record of the stack's networks, cannot be removed") {
		t.Fatalf("a record it cannot remove: %v", err)
	}
}

// up records the networks once the stack came up, and not when it did not; a start without a
// start-first service removes the record.
func TestUpRecordsTheNetworksOnceUp(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Spec.Services = map[string]spec.Service{"web": {Update: spec.UpdateStartFirst}}
	e.docker.model = `{"services": {"web": {"image": "reg/web:1"}}, "networks": {"default": {}}}`
	e.docker.upErr = errors.New("unhealthy")
	if err := e.install(nil); err == nil {
		t.Fatal("the start came up")
	}
	record := filepath.Join(e.dir, NetworksFile)
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a start that failed recorded the networks: %v", err)
	}
	e.docker.upErr = nil
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(`{"default":{}}`))
	if b, err := os.ReadFile(record); err != nil || string(b) != hex.EncodeToString(sum[:])+"\n" {
		t.Fatalf("record %q %v", b, err)
	}
	// Through up: the record read as the start begins lets the overlap run; an overlap that fails
	// keeps it, as no network changed.
	overlapped := func(upErr error) (bool, error) {
		st := &engineState{containers: map[string][]string{"web": {"old1|/p-old1|running|0||h1|sha256:img|"}}, hash: map[string]string{"web": "h2"}, upErr: upErr}
		e.docker.answer = st.docker(e.docker.model).answer
		defer func() { e.docker.answer = nil }()
		n := len(e.docker.calls)
		err := e.install(nil)
		for _, c := range e.docker.calls[n:] {
			if strings.Contains(strings.Join(c.Args, " "), " --scale web=2 web") {
				return true, err
			}
		}
		return false, err
	}
	if ran, err := overlapped(errors.New("unhealthy")); !ran || err == nil {
		t.Fatalf("an overlap that fails: ran %v, %v", ran, err)
	}
	if b, err := os.ReadFile(record); err != nil || string(b) != hex.EncodeToString(sum[:])+"\n" {
		t.Fatalf("an overlap that failed dropped the record: %q %v", b, err)
	}
	if ran, err := overlapped(nil); !ran || err != nil {
		t.Fatalf("an overlap through up: ran %v, %v\n%s", ran, err, e.log)
	}
	// A start that fails before it moves a network (here reading the stack's model) puts it back.
	e.docker.answer = func(a string) ([]byte, bool, error) {
		return nil, strings.HasSuffix(a, " config --format json"), errors.New("compose refuses the file")
	}
	if err := e.install(nil); err == nil {
		t.Fatal("the start came up without its model")
	}
	e.docker.answer = nil
	if b, err := os.ReadFile(record); err != nil || string(b) != hex.EncodeToString(sum[:])+"\n" {
		t.Fatalf("an early failure dropped the record: %q %v", b, err)
	}
	// A start whose stack does not come up leaves none: takeNetworks removed it.
	e.docker.upErr = errors.New("unhealthy")
	if err := e.install(nil); err == nil {
		t.Fatal("the start came up")
	}
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a start that failed left the record: %v", err)
	}
	e.docker.upErr = nil
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.r.Spec.Services = nil
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a start without a start-first service kept the record: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(record, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), record+", the record of the stack's networks, cannot be removed (directory not empty)") {
		t.Fatalf("a start without a start-first service and a record it cannot remove: %v", err)
	}
}

// A teardown that makes the stack's networks again (a changed MTU, IPv6 turned on or off) marks
// the start as having moved them: a failure after it leaves no record.
func TestTeardownsMoveTheNetworks(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{answer: func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasPrefix(a, "network ls --quiet "):
			return []byte("n1\n"), true, nil
		case strings.HasPrefix(a, "network inspect "):
			return []byte("default 1500\n"), true, nil
		case strings.HasPrefix(a, "network ls --filter "):
			return []byte("n1 \n"), true, nil
		}
		return nil, false, nil
	}}
	in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Docker: f, Log: &bytes.Buffer{}}}
	m := &composeModel{Networks: map[string]composeNetwork{"default": {}}}
	if err := in.networkMTU(context.Background(), []string{"COMPOSE_PROJECT_NAME=p"}, m, 1450); err != nil || !in.networksMoved {
		t.Fatalf("an MTU teardown: %v, moved %v", err, in.networksMoved)
	}
	// No teardown when the MTU holds, and a teardown that fails has moved them all the same.
	in.networksMoved = false
	if err := in.networkMTU(context.Background(), []string{"COMPOSE_PROJECT_NAME=p"}, m, 1500); err != nil || in.networksMoved {
		t.Fatalf("an MTU that holds: %v, moved %v", err, in.networksMoved)
	}
	answer := f.answer
	f.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, " down --remove-orphans") {
			return nil, true, errors.New("down refused")
		}
		return answer(a)
	}
	if err := in.networkMTU(context.Background(), []string{"COMPOSE_PROJECT_NAME=p"}, m, 1450); err == nil || !in.networksMoved {
		t.Fatalf("a teardown that fails: %v, moved %v", err, in.networksMoved)
	}
	f.answer = answer
	in.networksMoved = false
	env := NewEnvFile()
	env.Set("COMPOSE_PROJECT_NAME", "p")
	env.Set(IPv6Var, "true")
	if err := in.ipv6Network(context.Background(), env); err != nil || !in.networksMoved {
		t.Fatalf("an IPv6 teardown: %v, moved %v", err, in.networksMoved)
	}
	in.networksMoved = false
	f.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, " down --remove-orphans") {
			return nil, true, errors.New("down refused")
		}
		return answer(a)
	}
	if err := in.ipv6Network(context.Background(), env); err == nil || !in.networksMoved {
		t.Fatalf("an IPv6 teardown that fails: %v, moved %v", err, in.networksMoved)
	}
}

// joiner names a service whose configuration joins a start-first service's namespaces or mounts
// its volumes, and no other: a Docker keyword equal to its name is none.
func TestJoinerMatchesItsOwnServiceAlone(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		services map[string]runService
		service  string
		want     string
	}{
		{map[string]runService{"api": {}, "side": {Ipc: "service:api"}}, "api", "side"},
		{map[string]runService{"api": {}, "side": {Pid: "service:api"}}, "api", "side"},
		{map[string]runService{"api": {}, "side": {VolumesFrom: []string{"service:api:rw"}}}, "api", "side"},
		{map[string]runService{"api": {}, "side": {VolumesFrom: []string{"api"}}}, "api", "side"},
		{map[string]runService{"api": {}, "side": {VolumesFrom: []string{"api:ro"}}}, "api", "side"},
		{map[string]runService{"api": {}, "side": {VolumesFrom: []string{"api-worker"}}}, "api", ""},
		{map[string]runService{"api": {}, "side": {Links: []string{"api:www"}}}, "api", "side"},
		{map[string]runService{"api": {}, "side": {Links: []string{"api-worker"}}}, "api", ""},
		{map[string]runService{"api": {}, "side": {ExternalLinks: []string{"p-api-1:api"}}}, "api", "side"},
		{map[string]runService{"api": {}, "side": {ExternalLinks: []string{"p-db-1"}}}, "api", ""},
		{map[string]runService{"api": {}, "side": {ExternalLinks: []string{"p_api_1"}}}, "api", "side"},
		{map[string]runService{"api": {}, "side": {ExternalLinks: []string{"/p-api-1:api"}}}, "api", "side"},
		{map[string]runService{"host": {}, "lan": {NetworkMode: "host", Pid: "host"}}, "host", ""},
		{map[string]runService{"api": {}, "side": {NetworkMode: "container:p-api-1"}}, "api", "side"},
		{map[string]runService{"api": {}, "side": {VolumesFrom: []string{"container:p-api-2:ro"}}}, "api", "side"},
		{map[string]runService{"api": {}, "side": {NetworkMode: "container:p-api-worker-1"}}, "api", ""},
		{map[string]runService{"api": {}, "side": {NetworkMode: "container:q-api-1"}}, "api", ""},
	} {
		if got := joiner(c.services, c.service, "p"); got != c.want {
			t.Errorf("%+v: joiner %q, want %q", c.services, got, c.want)
		}
	}
	if got := joiner(map[string]runService{"api": {}, "side": {NetworkMode: "container:-api-1"}}, "api", ""); got != "" {
		t.Errorf("without a project: joiner %q", got)
	}
}

// The engine tells a container that joins the old one by its ID or name, or mounts its volumes,
// from one in no namespace of its: the first replaces the service stop-first.
func TestAContainerJoiningTheOldOneIsTheEngines(t *testing.T) {
	t.Parallel()
	old := container{ID: "old1", Name: "/p-api-1"}
	for _, c := range []struct {
		joins []string
		want  string
	}{
		{[]string{"old1|/p-api-1|bridge||||", "s1|/side|container:old1||||"}, "side"},
		{[]string{"s1|/outside|bridge|container:p-api-1|||"}, "outside"},
		{[]string{"s1|/side|bridge||container:old1||"}, "side"},
		{[]string{"s1|/side|bridge|||p-api-1:ro,|"}, "side"},
		{[]string{"s1|/side|bridge|||old1,|"}, "side"},
		{[]string{"s1|/side|container:other|host|private|p-db-1,|"}, ""},
		{[]string{"s1|/side|container:old||||"}, "side"},                                   // a short ID
		{[]string{"old1|/p-api-1|container:old1||||"}, ""},                                 // itself
		{[]string{"gone|/gone|container:old1||||", "s1|/side|container:old1||||"}, "side"}, // one removed meanwhile
		{[]string{"s1|/side|bridge||||/p-api-1:/side/api,"}, "side"},                       // a link
		{[]string{"s1|/side|bridge||||/p-db-1:/side/db,"}, ""},
		{[]string{"s1|/side|bridge||||p-api-1:www,"}, "side"},
		{[]string{"s1|/side|bridge||||old1:www,"}, "side"},                  // a link by ID
		{[]string{"s1|/side|bridge||||old:www,"}, "side"},                   // a link by a prefix of its ID
		{[]string{"s1|/side|bridge||||old:www,", "s2|/old|bridge||||"}, ""}, // a link to a container named old
		{[]string{"s1|/side|container:old||||", "s2|/old|bridge||||"}, ""},  // a container named old
		{nil, ""},
	} {
		e := &engineState{joins: c.joins}
		in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Docker: e.docker("{}")}}
		if got, err := in.joinedContainer(context.Background(), old); err != nil || got != c.want {
			t.Errorf("%q: %q %v, want %q", c.joins, got, err, c.want)
		}
	}
	// One the engine cannot read for another reason fails the start, as does a line that does not
	// read: the check is never skipped.
	busy := &engineState{joins: []string{"busy|/b||||||", "s1|/side|bridge||||"}}
	in0 := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Docker: busy.docker("{}")}}
	if _, err := in0.joinedContainer(context.Background(), old); err == nil || !strings.Contains(err.Error(), "reading the engine's container busy") {
		t.Errorf("an engine error: %v", err)
	}
	e := &engineState{joins: []string{"s1|/side"}}
	in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Docker: e.docker("{}")}}
	if _, err := in.joinedContainer(context.Background(), old); err == nil || !strings.Contains(err.Error(), "reading the engine's containers") {
		t.Errorf("a container that does not read: %v", err)
	}
}
