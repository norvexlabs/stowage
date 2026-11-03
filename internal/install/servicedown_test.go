package install

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestServiceDown(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		s    Service
		down bool
		why  string
	}{
		{Service{State: "running"}, false, ""},
		{Service{State: "exited"}, false, ""}, // a one-shot that finished
		{Service{State: "exited", ExitCode: 1}, true, "exited 1"},
		{Service{State: "exited", ExitCode: 137, OOMKilled: true, MemLimit: 96 << 20}, true, "exited 137 (SIGKILL), OOM-killed (memory limit 96 MiB)"},
		{Service{State: "exited", ExitCode: 143}, true, "exited 143 (SIGTERM)"}, // a stop
		{Service{State: "exited", ExitCode: 130}, true, "exited 130 (SIGINT)"},
		{Service{State: "exited", ExitCode: 131}, true, "exited 131 (SIGQUIT)"},
		{Service{State: "exited", ExitCode: 129}, true, "exited 129 (SIGHUP)"},
		{Service{State: "exited", ExitCode: 133}, true, "exited 133 (SIGTRAP)"},
		{Service{State: "exited", ExitCode: 128}, true, "exited 128"},
		{Service{State: "exited", ExitCode: 164}, true, "exited 164 (signal 36)"},
		{Service{State: "exited", ExitCode: 192}, true, "exited 192 (signal 64)"},
		{Service{State: "exited", ExitCode: 193}, true, "exited 193"},
		{Service{State: "restarting", ExitCode: 137}, true, "restarting (exited 137 (SIGKILL))"},
		{Service{State: "removing"}, true, "removing"},
		{Service{State: "created"}, true, "created, never started"},
		{Service{State: "dead"}, true, "dead"},
		{Service{State: "paused"}, false, ""}, // paused by someone: not down
	} {
		if got := c.s.Down(); got != c.down {
			t.Errorf("%+v: down %v", c.s, got)
		}
		if c.down && c.s.Why() != c.why {
			t.Errorf("%+v: %q, want %q", c.s, c.s.Why(), c.why)
		}
	}
}

// stackOf answers compose ps with ps and docker inspect with inspect (or fails it with inspectErr).
type stackOf struct {
	mu         sync.Mutex
	ps         string
	inspect    string
	inspectErr error
	psErr      error
	looks      int
	// during, when set, runs as compose ps answers: a run taking the lock in the middle of a look.
	during func()
}

func (s *stackOf) answer(a string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.HasSuffix(a, "ps --all --format json"):
		s.looks++
		if s.during != nil {
			s.during()
		}
		return []byte(s.ps), true, s.psErr
	case strings.HasPrefix(a, "inspect --type container --format "):
		return []byte(s.inspect), true, s.inspectErr
	}
	return nil, false, nil
}

func (s *stackOf) set(ps, inspect string) {
	s.mu.Lock()
	s.ps, s.inspect = ps, inspect
	s.mu.Unlock()
}

// The facts come from docker inspect, matched by the ID compose ps prints (shortened or whole);
// inspect failing keeps the list compose ps gave.
func TestServiceFacts(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	st := &stackOf{}
	e.docker.answer = st.answer
	in := e.installer(vm)
	list := []Service{{Service: "api", ID: "abc123", State: "exited", ExitCode: 137}, {Service: "web", ID: "def456", State: "running"}, {Service: "gone"}}
	st.set("", "abc123ffff true 100663296 0\ndef456ffff false 0 3\n")
	in.serviceFacts(context.Background(), list)
	if a, w := list[0], list[1]; !a.OOMKilled || a.MemLimit != 96<<20 || w.OOMKilled || w.Restarts != 3 || list[2].MemLimit != 0 {
		t.Errorf("%+v", list)
	}
	st.inspectErr = errors.New("no such object")
	st.set("", "")
	fresh := []Service{{Service: "api", ID: "abc123", State: "exited", ExitCode: 137}}
	in.serviceFacts(context.Background(), fresh)
	if fresh[0].OOMKilled || fresh[0].State != "exited" {
		t.Errorf("inspect failing: %+v", fresh)
	}
}

// Each container down is named, a crashed replica beside one that runs and one the operator
// stopped (its signal) included; a one-shot that finished is not.
func TestPrintDown(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	printDown(&b, []Service{
		{Service: "api", State: "exited", ExitCode: 137, OOMKilled: true, MemLimit: 96 << 20},
		{Service: "seed", State: "exited"},
		{Service: "web", State: "running"},
		{Service: "web", State: "exited", ExitCode: 1},
		{Service: "ui", State: "exited", ExitCode: 143},
	})
	if got := b.String(); got != "down: api exited 137 (SIGKILL), OOM-killed (memory limit 96 MiB)\ndown: web exited 1\ndown: ui exited 143 (SIGTERM)\n" {
		t.Errorf("%q", got)
	}
}

// The facts go by the exact ID first, else the one whole ID a short one begins; two is none.
func TestServiceFactsMatchOneContainer(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	st := &stackOf{}
	e.docker.answer = st.answer
	in := e.installer(vm)
	st.set("", "abc true 100663296 0\nabcdef false 268435456 2\nabd123 true 1 1\nabd456 true 1 1\n")
	list := []Service{{Service: "a", ID: "abc"}, {Service: "b", ID: "abcdef"}, {Service: "c", ID: "abd"}}
	in.serviceFacts(context.Background(), list)
	if !list[0].OOMKilled || list[1].MemLimit != 256<<20 || list[1].OOMKilled || list[2].OOMKilled {
		t.Errorf("%+v", list)
	}
}

// The VM's `status` and `ps` print a line for a service down, and not for a one-shot that finished.
func TestTheVMTellsAServiceDown(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	inner := e.docker.answer
	st := &stackOf{}
	st.set(`{"ID":"c1","Service":"api","State":"exited","ExitCode":137}`+"\n"+`{"ID":"c2","Service":"seed","State":"exited","ExitCode":0}`+"\n", "c1ffff true 100663296 0\nc2ffff false 0 0\n")
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if out, ok, err := st.answer(a); ok {
			return out, ok, err
		}
		return inner(a)
	}
	h := daemon(e, &fakeHost{})
	sock := served(t, h)
	for _, verb := range []string{"ps", "status"} {
		out, err := ctl(e, sock, verb)
		if err != nil || !strings.Contains(out, "down: api exited 137 (SIGKILL), OOM-killed (memory limit 96 MiB)\n") || strings.Contains(out, "down: seed") {
			t.Errorf("%s: %v\n%s", verb, err, out)
		}
	}
}

// Docker's status warns when the services' state does not read, after Compose's table.
func TestDockerStatusWarnsWhenTheStateDoesNotRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, "ps --all --format json") {
			return nil, true, errors.New("engine restarting")
		}
		return nil, false, nil
	}
	e.out.Reset()
	if err := e.installer(nil).Status(context.Background()); err != nil || !strings.Contains(e.out.String(), "warning: which services are down could not be read: compose ps: engine restarting\n") {
		t.Errorf("%v\n%s", err, e.out)
	}
}

// Docker's status prints Compose's table, then a line for each service down.
func TestDockerStatusTellsAServiceDown(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasSuffix(a, "ps --all --format json"):
			return []byte(`{"ID":"c1","Service":"api","State":"exited","ExitCode":1}` + "\n"), true, nil
		case strings.HasPrefix(a, "inspect --type container --format "):
			return []byte("c1ffff false 268435456 0\n"), true, nil
		}
		return nil, false, nil
	}
	e.out.Reset()
	if err := e.installer(nil).Status(context.Background()); err != nil || !strings.Contains(e.out.String(), "down: api exited 1 (memory limit 256 MiB)\n") {
		t.Errorf("%v\n%s", err, e.out)
	}
}
