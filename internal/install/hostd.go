package install

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
)

// The host daemon (`<short>-stow hostd`, a systemd service on the VM) runs the VM verbs as root:
// <short>-stow reaches it on a unix socket that root and the admin group alone open. Its API is
// JSON over HTTP, under /v1:
//
//	GET  /v1/status                      the OS's release, the install's, its channel, the disk and
//	                                     its pressure, the boot (active, passive, fallback, the last
//	                                     failed boot), the job
//	GET  /v1/health                      {ready, reason}: the health gate's check, once
//	GET  /v1/services                    the stack's services
//	GET  /v1/services/{service}/logs     ?tail=N (1..5000, default 200): the service's last lines
//	POST /v1/services/{service}/{action} restart, stop or start one service
//	POST /v1/provision                   {set}: the provision hook again, set recorded first
//	GET  /v1/updates                     ?channel=C&version=V: the release an upgrade would install
//	POST /v1/upgrade                     {channel, version, confirm, reboot, skip_backup}: an upgrade job
//	                                     (202, JobStarted), a backup first when one is configured
//	GET  /v1/rollback                    {from, to}: what going back to the passive system would do
//	POST /v1/rollback                    {confirm}: a rollback job, choosing the passive system, then
//	                                     a reboot (202, JobStarted)
//	GET  /v1/job                         ?since=N: the last job and its log from line N; log_total is
//	                                     the line to ask from next
//	GET  /v1/events                      ?after=N&wait=S: the event journal's entries after N, waiting
//	                                     up to S seconds for one (hostdevents.go)
//	POST /v1/reboot, /v1/shutdown        {drain_secs}: after the drain (202)
//	GET, POST /v1/network, POST /v1/network/{confirm,revert,reset}: the network override (hostdnet.go)
//	/v1/backup, /v1/backup/{config,status,list,verify}: the backups (hostdbackup.go)
//	/v1/secrets/{export,rotate}, /v1/restore: the install's secrets and a backup's restore (hostdsecrets.go)
//
// An error is {"error": "..."} with a 4xx or 5xx status. One job runs at a time, and none starts
// once a reboot is scheduled; every request that starts one answers 202 with JobStarted.

// AdminGroup is the group whose members may use the host daemon: Kairos's administrators.
const AdminGroup = "admin"

// Hostd is the host daemon of one VM install.
type Hostd struct {
	// R is the release this OS carries; O the options every request's run starts from, O.Dir the
	// VM's install.
	R *Release
	O Options
	// Host runs a host program: Args[0] is the program.
	Host Tool
	// Group is the group the socket opens to besides root (AdminGroup on the VM); empty, or a
	// group the host lacks, leaves it root's alone.
	Group string
	// Unit scales the waits (the reboot drain); 0 is a second. Tests shorten it.
	Unit time.Duration
	// Run is the daemon's private directory, root's alone: the socket's, where an upgrade's pull
	// config is written. Serve sets it.
	Run string
	// State is COS_STATE's mount, which holds the boot assessment; empty is
	// /run/initramfs/cos-state. Where Kairos marks the system it booted and the boot id are O's
	// (Cos, BootIDFile).
	State string
	// Probe sends the http health providers' requests and the disk-pressure POSTs; nil is a
	// client of its own.
	Probe *http.Client
	// Networkd is networkd's directory, where the network override is rendered; empty is
	// /etc/systemd/network. Issue is the console banner's directory; empty is /etc/issue.d.
	Networkd, Issue string
	// InstallNet is the file the cloud-config writes the install's network to; empty is
	// InstallNetworkFile.
	InstallNet string
	// Systemd is where the daemon writes runtime unit drop-ins (the backup timer's schedule);
	// empty is /run/systemd/system.
	Systemd string
	// Statfs reads the space of a directory's filesystem; nil is the system's.
	Statfs func(dir string) *Disk
	// Docker: the daemon of a Docker install, run in the installer's process (dockerhost.go): no
	// boot, no health gate, no banner; its units the installer's own; DiskDir the Docker engine's
	// data, whose filesystem the disk-pressure watch reads.
	Docker  bool
	DiskDir string

	// held is the install's lock a rollback keeps to its reboot.
	held      func()
	mu        sync.Mutex
	job       *job
	rebooting bool
	// jobEnd, under mu, is closed when the running job ends (startJobWhenFree waits on it).
	jobEnd chan struct{}
	// slotGen, under mu, moves at every claim of the job slot and every job's end: a run that
	// read what it runs with at one generation starts only at that same one (startJobWhenFree).
	slotGen uint64
	// resetToken is the token the last GET reset-db made, which one POST may use.
	resetToken string
	ctx        context.Context // the daemon's: a job outlives its request

	// netMu orders the network changes and their revert; netTimer is the armed revert, netGen
	// counts disarms (a revert that fired after one does nothing).
	netMu    sync.Mutex
	netTimer *time.Timer
	netGen   int
	// netFired is the netGen of the last revert timer that fired.
	netFired atomic.Int64
	// installNetLogged is the last bad install network record's error logged: the network is
	// polled, and the record is rewritten the same at every boot, so each error is logged once.
	installNetLogged atomic.Value // a string
	// backupMu orders the backup settings' reads and changes; backingUp is set while an upgrade
	// takes its backup first.
	backupMu  sync.Mutex
	backingUp atomic.Bool
	// backups counts the backups running: Serve waits for them to clean up as it stops.
	backups sync.WaitGroup
	// pressMu guards lastPost, the disk-pressure watch's last POST.
	pressMu  sync.Mutex
	lastPost *PressurePost
	// evMu guards evCh, closed at the daemon's next append to the event journal.
	evMu sync.Mutex
	evCh chan struct{}
	// onEvent, when set (tests), is given the entries of each append event makes.
	onEvent func(evs []Event)
	// bootRec holds this boot's boot.started once read (boot).
	bootRec bootCache
}

// job is a long run the daemon does on its own: an upgrade.
type job struct {
	// Started identifies the job: two jobs of one kind and target are told apart by it.
	Started time.Time
	Kind    string
	State   string // running, done, failed
	Target  string
	Error   string
	// Result is what a job that is done found (backup-verify: the destination it reached).
	Result string
	// Reboot is the reboot or power-off a job that is done scheduled as its last step.
	Reboot *EventReboot
	// events is the event journal's last number once the job's start was journaled; nil when
	// it could not be, eventsErr then saying why.
	events    *int64
	eventsErr string
	// Log holds the job's last maxLog lines; dropped counts the lines before them.
	Log     []string
	dropped int
}

// maxLog bounds a job's log in memory: at least its last maxLog lines are kept, at most twice
// that; maxLine bounds a line.
const (
	maxLog  = 2000
	maxLine = 4 << 10
)

// JobStatus is the last job, with its log from the line a client asked for (lines are numbered
// from the job's start; those dropped from memory are skipped).
type JobStatus struct {
	// Started, the job's start, identifies it; absent when there was none.
	Started *time.Time `json:"started,omitempty"`
	Kind    string     `json:"kind,omitempty"`
	State   string     `json:"state"` // idle when there was none
	Target  string     `json:"target,omitempty"`
	Error   string     `json:"error,omitempty"`
	// Result is what a job that is done found; absent for most kinds.
	Result string `json:"result,omitempty"`
	// Reboot is the reboot or power-off a job that is done scheduled as its last step: it runs
	// after its drain, or the journal's reboot.cancelled by the job tells why not.
	Reboot   *EventReboot `json:"reboot,omitempty"`
	Log      []string     `json:"log"`
	LogTotal int          `json:"log_total"`
}

// JobStarted is what a request that starts a job answers (202): the job's kind, target and
// start, which tells it from any other (GET /v1/job's started), and the event journal's last
// number once its start was journaled — its end, and its reboot's, come after it. Events is null
// when the start could not be journaled, and EventsError then says why: a follower never takes a
// cursor the journal did not give.
type JobStarted struct {
	Job         string    `json:"job"`
	Target      string    `json:"target"`
	Started     time.Time `json:"started"`
	Events      *int64    `json:"events"`
	EventsError string    `json:"events_error,omitempty"`
}

// accepted answers 202 for the job j started.
func accepted(w http.ResponseWriter, j *job) {
	reply(w, http.StatusAccepted, JobStarted{Job: j.Kind, Target: j.Target, Started: j.Started, Events: j.events, EventsError: j.eventsErr})
}

// Version is a release: a project version at a package revision.
type Version struct {
	Version  string `json:"version"`
	Revision int    `json:"revision"`
}

func (v Version) String() string { return fmt.Sprintf("%s (revision %d)", v.Version, v.Revision) }

// Status is what GET /v1/status answers.
type Status struct {
	Name string  `json:"name"`
	OS   Version `json:"os"` // the release this OS carries
	// Installed is the release the install runs; nil before the first install.
	Installed *Version `json:"installed"`
	// Installing: the first install has not finished — its stack is coming up or the provision
	// hook runs (or a reboot cut it: the boot runs it again). InstallFailed: why its last run
	// failed; a re-run of the install finishes it.
	Installing    bool   `json:"installing,omitempty"`
	InstallFailed string `json:"install_failed,omitempty"`
	// RestoreAwaited: --secrets gave the install another's provision values; the hook waits for a
	// backup of that install's data (`restore` loads it, then provisions).
	RestoreAwaited bool `json:"restore_awaited,omitempty"`
	// VolumesSplit: a restore left the volumes half restored; the stack stays down until a
	// restore again finishes.
	VolumesSplit bool   `json:"volumes_split,omitempty"`
	Channel      string `json:"channel,omitempty"`
	// Tune is the stack's tuning profile, and whose it is: "network (operator)".
	Tune string `json:"tune,omitempty"`
	Disk *Disk  `json:"disk,omitempty"`
	// Pressure is the disk-pressure watch's reading; nil without hooks.maintenance.disk_pressure.
	Pressure *DiskPressure `json:"disk_pressure,omitempty"`
	// Cert is a byo install's certificate: its end, and a warning from 30 days before it.
	Cert *CertStatus `json:"cert,omitempty"`
	// Boot is the system Kairos booted: active, passive (the fallback) or recovery; empty off
	// Kairos.
	Boot string `json:"boot,omitempty"`
	// Fallback: Kairos booted the passive system because the last upgrade's boot failed its health
	// gate, or `rollback` chose it.
	Fallback bool `json:"fallback,omitempty"`
	// OnTrial: the boot runs an upgrade's new system, which the health gate has not judged yet —
	// healthy, it is kept; not, the gate goes back (or keeps it, as failed_boot then says).
	OnTrial bool `json:"on_trial,omitempty"`
	// FailedBoot is the last boot the health gate found unhealthy, its diagnostics kept.
	FailedBoot    *FailedBoot `json:"failed_boot,omitempty"`
	RebootPending bool        `json:"reboot_pending"`
	// BootID is the kernel's id of this boot: a client that saw a reboot scheduled knows the
	// appliance rebooted when it changes. Empty when it cannot be read.
	BootID string `json:"boot_id,omitempty"`
	Job    string `json:"job"` // the last job's state, idle when none ran
	// Events is the event journal's last entry number (0 before the first): GET
	// /v1/events?after= it follows from there. Null when the journal cannot be read, and
	// EventsError then says why: a reader never takes a cursor the journal did not give.
	Events      *int64 `json:"events"`
	EventsError string `json:"events_error,omitempty"`
}

// Disk is the persistent partition's space.
type Disk struct {
	Path      string `json:"path"`
	Total     uint64 `json:"total_bytes"`
	Available uint64 `json:"available_bytes"`
	// Used excludes the blocks reserved for root, which Available excludes too.
	Used uint64 `json:"used_bytes"`
}

// Service is one service of the stack.
type Service struct {
	Service string `json:"service"`
	State   string `json:"state"`
	Health  string `json:"health,omitempty"`
	Image   string `json:"image"`
	// ExitCode is an exited service's exit status.
	ExitCode int `json:"exit_code"`
}

// Update is what GET /v1/updates answers: the release an upgrade would install.
type Update struct {
	Channel   string  `json:"channel"`
	Current   Version `json:"current"`
	Target    Version `json:"target"`
	Available bool    `json:"available"` // the target comes after the current release, and may be installed
	// LeftByRollback: the install went back from the target, and the boot does not upgrade to it
	// again.
	LeftByRollback bool   `json:"left_by_rollback,omitempty"`
	Changelog      string `json:"changelog,omitempty"`
}

// UpgradeRequest is POST /v1/upgrade's body.
type UpgradeRequest struct {
	Channel string `json:"channel,omitempty"` // default: the install's
	Version string `json:"version,omitempty"` // default: the channel's latest release
	// Confirm, when set, is the release the operator confirmed: another is refused.
	Confirm *Version `json:"confirm,omitempty"`
	// Reboot after the new OS is written, to run it; default true.
	Reboot *bool `json:"reboot,omitempty"`
	// SkipBackup: no backup first, though one is configured.
	SkipBackup bool `json:"skip_backup,omitempty"`
}

// PowerRequest is POST /v1/reboot's and /v1/shutdown's body.
type PowerRequest struct {
	DrainSecs *int `json:"drain_secs,omitempty"` // default 30, at most 600
}

// serviceName is a compose service name.
var serviceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// Serve serves the API on the unix socket path until ctx ends. The socket and its directory are
// root's and Group's; without that group, root's alone. When the stack's services reach the daemon
// (shapes.kairos.host_api), the API is served too on the app socket, in the directory app beside
// path, which those services mount: any process may connect to it, and every request carries the
// install's token (AppHandler).
func (h *Hostd) Serve(ctx context.Context, path string) error {
	h.ctx = ctx
	h.startJournal()
	gid := -1
	if h.Group != "" {
		if g, err := user.LookupGroup(h.Group); err == nil {
			gid, _ = strconv.Atoi(g.Gid)
		} else {
			h.O.logf("no %s group: only root can use the host daemon", h.Group)
		}
	}
	dir := filepath.Dir(path)
	h.Run = dir
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := own(dir, gid, 0o750); err != nil {
		return err
	}
	l, err := listen(path, func(p string) error { return own(p, gid, 0o660) })
	if err != nil {
		return err
	}
	servers := []*http.Server{{Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}}
	listeners := []net.Listener{l}
	if k := h.R.Spec.Shapes.Kairos; k != nil && len(k.HostAPI) > 0 {
		app := filepath.Join(dir, filepath.Base(AppDir(h.R.Spec.Name)))
		if app != AppDir(h.R.Spec.Name) {
			h.O.logf("the services %s names mount %s; this daemon serves %s", "shapes.kairos.host_api", AppDir(h.R.Spec.Name), app)
		}
		// The daemon's own, as it runs as root: open to every process that can reach it.
		err := os.MkdirAll(app, 0o755)
		if err == nil {
			err = os.Chmod(app, 0o755)
		}
		var al net.Listener
		if err == nil {
			al, err = listen(filepath.Join(app, appSocketName), func(p string) error { return os.Chmod(p, 0o666) })
		}
		if err != nil {
			l.Close()
			return err
		}
		servers = append(servers, &http.Server{Handler: h.AppHandler(), ReadHeaderTimeout: 10 * time.Second})
		listeners = append(listeners, al)
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(shut)
		}
	}()
	h.netMu.Lock()
	errs := make(chan error, len(servers))
	for i, srv := range servers {
		h.O.logf("serving on %s", listeners[i].Addr())
		go func() { errs <- srv.Serve(listeners[i]) }()
	}
	// Undoing a change still awaiting confirmation may take networkd's reload and its check: the
	// listeners serve meanwhile, but no network request runs before it.
	go func() {
		defer h.netMu.Unlock()
		h.netResume()
	}()
	go h.backupResume()
	wctx, stopWatch := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		h.pressureWatch(wctx)
	}()
	go h.certWatch(wctx)
	// One server failing stops the others: the daemon is restarted whole.
	var first error
	for range servers {
		if err := <-errs; !errors.Is(err, http.ErrServerClosed) && first == nil {
			first = err
			for _, srv := range servers {
				_ = srv.Close()
			}
		}
	}
	stopWatch() // a check under way ends with it: no banner or log after Serve returns
	<-watched
	// A backup cut off by the stop removes its runner and the object it left (each within
	// rcloneTimeout) and records its failure: the daemon waits for it, up to backupStopWait.
	done := make(chan struct{})
	go func() {
		h.backups.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(backupStopWait):
		h.O.logf("a backup did not finish its cleanup within %s", backupStopWait)
	}
	return first
}

// listen listens on the unix socket path with the owner and mode perm gives it. The socket is made
// and given them under a name of its own beside path (.<base>.<pid>), then renamed onto path
// (replacing a stale one): a client never finds path before it listens, nor while any process may
// still connect. Closing the listener removes path while it is still that socket.
func listen(path string, perm func(string) error) (net.Listener, error) {
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d", filepath.Base(path), os.Getpid()))
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", tmp)
	if err != nil {
		return nil, err
	}
	// The listener would remove tmp, a name it no longer has once renamed: unixSocket removes path.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	fi, err := os.Lstat(tmp)
	if err == nil {
		err = perm(tmp)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		l.Close()
		_ = os.Remove(tmp)
		return nil, err
	}
	return &unixSocket{Listener: l, path: path, file: fi}, nil
}

// unixSocket is a listener on the unix socket file renamed onto path. Its address is path, and
// closing it removes path while path is still that socket: never one a daemon started since put
// there.
type unixSocket struct {
	net.Listener
	path string
	file os.FileInfo
}

func (u *unixSocket) Addr() net.Addr { return &net.UnixAddr{Name: u.path, Net: "unix"} }

func (u *unixSocket) Close() error {
	if fi, err := os.Lstat(u.path); err == nil && os.SameFile(fi, u.file) {
		_ = os.Remove(u.path)
	}
	return u.Listener.Close()
}

// own gives p to root and gid (none when -1, mode then dropping the group's bits).
func own(p string, gid int, mode os.FileMode) error {
	if gid < 0 {
		return os.Chmod(p, mode&^0o070)
	}
	if err := os.Chown(p, 0, gid); err != nil {
		return err
	}
	return os.Chmod(p, mode)
}

// appCaller marks a request's context as the app socket's.
type appCaller struct{}

// fromApp is whether r came through the app socket.
func fromApp(r *http.Request) bool { b, _ := r.Context().Value(appCaller{}).(bool); return b }

// Handler is the API, as the operator's socket serves it.
func (h *Hostd) Handler() http.Handler { return h.routes(false) }

// routes is the API; for the app socket (app), what each route says the app may reach, and a
// 403 for the others: every route states it where it is registered.
func (h *Hostd) routes(app bool) http.Handler {
	if h.ctx == nil {
		h.ctx = context.Background()
	}
	mux := http.NewServeMux()
	handle := func(pattern string, f http.HandlerFunc, appToo bool) {
		switch serve := f; {
		case app && !appToo:
			f = func(w http.ResponseWriter, r *http.Request) {
				reply(w, http.StatusForbidden, map[string]string{"error": "the host daemon's app socket does not serve " + pattern + ": run it as the operator"})
			}
		case app:
			f = func(w http.ResponseWriter, r *http.Request) {
				serve(w, r.WithContext(context.WithValue(r.Context(), appCaller{}, true)))
			}
		}
		mux.HandleFunc(pattern, f)
	}
	// The app reaches what operates the install; the operator's alone are the routes that hand
	// out or replace the secrets, erase or overwrite the data, re-run the hook (whose outputs
	// include credentials), or stop the backups.
	both := func(pattern string, f http.HandlerFunc) { handle(pattern, f, true) }
	operator := func(pattern string, f http.HandlerFunc) { handle(pattern, f, false) }
	both("GET /v1/status", h.status)
	both("GET /v1/services", h.services)
	both("GET /v1/services/{service}/logs", h.logs)
	both("POST /v1/services/{service}/{action}", h.serviceAction)
	operator("POST /v1/provision", h.provision)
	both("GET /v1/reset-db", h.resetDBPlan)
	operator("POST /v1/reset-db", h.resetDB)
	both("GET /v1/health", h.health)
	both("GET /v1/updates", h.updates)
	both("POST /v1/upgrade", h.upgrade)
	both("GET /v1/rollback", h.rollbackPlan)
	both("POST /v1/rollback", h.rollback)
	both("GET /v1/job", h.jobStatus)
	both("GET /v1/events", h.events)
	both("POST /v1/reboot", func(w http.ResponseWriter, r *http.Request) { h.power(w, r, "reboot") })
	both("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) { h.power(w, r, "poweroff") })
	both("GET /v1/network", h.network)
	both("POST /v1/network", h.setNetwork)
	both("POST /v1/network/confirm", h.confirmNetwork)
	both("POST /v1/network/revert", h.revertNetwork)
	both("POST /v1/network/reset", h.resetNetwork)
	both("POST /v1/nftables/{table}/sets/{set}", h.setElements)
	both("GET /v1/backup/config", h.backupConfig)
	both("POST /v1/backup/config", h.setBackupConfig) // the lock's checks are its own
	operator("DELETE /v1/backup/config", h.removeBackupConfig)
	both("POST /v1/backup", h.startBackup)
	both("GET /v1/backup/status", h.backupStatus)
	both("GET /v1/backup/list", h.listBackups)
	both("POST /v1/backup/verify", h.verifyBackup)
	operator("POST /v1/secrets/export", h.exportSecrets)
	operator("POST /v1/secrets/rotate", h.rotateSecret)
	operator("POST /v1/restore", h.restoreBackup)
	return mux
}

// AppHandler is the API as the app socket serves it: each request must carry the install's token
// (HostTokenVar), read from the install at every request, as "Authorization: Bearer <token>".
// Before the install records one, every request is refused. The operator's routes answer 403
// (routes).
func (h *Hostd) AppHandler() http.Handler {
	api := h.routes(true)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in, _ := h.run()
		want := ""
		if env, err := in.open(); err == nil {
			want, _ = env.Get(HostTokenVar)
		}
		got, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if want == "" || !bearer || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			reply(w, http.StatusUnauthorized, map[string]string{"error": "the host daemon's app socket needs the install's token: Authorization: Bearer $" + HostTokenVar})
			return
		}
		api.ServeHTTP(w, r)
	})
}

// httpError is an error with the status it is answered with.
type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

func fail(code int, format string, a ...any) error {
	return httpError{code, fmt.Sprintf(format, a...)}
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func replyErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var he httpError
	if errors.As(err, &he) {
		code = he.code
	}
	reply(w, code, map[string]string{"error": err.Error()})
}

// decodeBody reads a JSON body strictly; an empty one leaves v as it is.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fail(http.StatusBadRequest, "the request body: %v", err)
	}
	return nil
}

// run is one request's run of the installer: its output is collected, not the daemon's.
func (h *Hostd) run() (*Installer, *bytes.Buffer) {
	var out bytes.Buffer
	o := h.O
	o.Out, o.Log, o.Prompt = &out, &out, nil
	return &Installer{R: h.R, O: o}, &out
}

// installed opens the install; a 409 before the first install.
func (h *Hostd) installed(in *Installer) (*EnvFile, error) {
	if notInstalled(in) {
		return nil, fail(http.StatusConflict, "%s is not installed yet: `sudo %s install` installs it", h.R.Spec.Name, filepath.Base(BundledCtl(h.R.Spec.Short())))
	}
	return in.open()
}

// notInstalled reports whether in has no install yet.
func notInstalled(in *Installer) bool {
	_, err := os.Stat(filepath.Join(in.O.Dir, envName))
	return errors.Is(err, os.ErrNotExist)
}

func (h *Hostd) status(w http.ResponseWriter, r *http.Request) {
	in, _ := h.run()
	m := h.R.Manifest
	st := Status{Name: m.Name, OS: Version{m.ProjectVersion, m.Revision}, Job: "idle"}
	if env, err := in.open(); err == nil {
		_, v, rev := runs(env)
		n, _ := strconv.Atoi(rev)
		st.Installed = &Version{v, n}
		st.Installing, st.InstallFailed, _ = in.firstInstall()
		st.RestoreAwaited = in.restoreAwaited()
		st.Channel, _ = env.Get(ChannelVar)
		p, by := in.tuneProfile(func(k string) string { v, _ := env.Get(k); return v })
		st.Tune = fmt.Sprintf("%s (%s)", p, by)
		if mode, _ := env.Get("STOWAGE_TLS_MODE"); mode == "byo" {
			st.Cert, _ = certStatus(in.O.Dir, time.Now(), h.renewHint())
		}
	} else if !errors.Is(err, os.ErrNotExist) && fileExists(filepath.Join(in.O.Dir, envName)) {
		replyErr(w, err)
		return
	}
	// The boot unit runs the first install: while it runs, a failure recorded before is being
	// retried; failed with no install written, it failed before writing one.
	if boot, err := h.bootState(r.Context()); err == nil {
		switch {
		case boot != "active" && boot != "failed" && boot != noBootUnit:
			st.InstallFailed = ""
		case boot == "failed" && st.Installed == nil:
			st.InstallFailed = fmt.Sprintf("the boot's install failed before it wrote the install (`journalctl -u %s` tells why)", m.Name)
		}
	}
	st.Disk = h.disk()
	st.Pressure = h.pressureStatus(st.Disk)
	st.Boot = bootMode(h.O.cos())
	st.Fallback = fileExists(filepath.Join(h.O.cos(), upgradeFailure))
	if st.Boot == "active" {
		if vars, err := h.assessment().read(); err == nil {
			st.OnTrial = trial(vars)
		}
	}
	st.FailedBoot = lastFailedBoot(in.O.Dir)
	st.BootID = h.bootID()
	if a, err := h.journal().since(-1, 0); err != nil {
		st.EventsError = fmt.Sprintf("the event journal cannot be read: %v", err)
	} else {
		st.Events = &a.Last
	}
	h.mu.Lock()
	st.RebootPending = h.rebooting
	// Read with the job: a restore running holds the mark from its database's swap to its last
	// volume's.
	restoring := h.job != nil && h.job.Kind == "restore" && h.job.State == "running"
	st.VolumesSplit = st.Installed != nil && !restoring && in.volumesSplit() != nil
	if h.job != nil {
		st.Job = h.job.State
	}
	h.mu.Unlock()
	reply(w, http.StatusOK, st)
}

// readBootID is the boot id the kernel publishes in file; empty when it cannot be read.
func readBootID(file string) string {
	raw, err := os.ReadFile(file)
	if err != nil || len(raw) > 64 {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// bootMode is the system Kairos booted — active, passive or recovery — as it marks it in dir;
// empty when it marks none (not a Kairos boot).
func bootMode(dir string) string {
	for _, m := range []string{"active", "passive", "recovery"} {
		if fileExists(filepath.Join(dir, m+"_mode")) {
			return m
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (h *Hostd) services(w http.ResponseWriter, r *http.Request) {
	in, _ := h.run()
	env, err := h.installed(in)
	if err != nil {
		replyErr(w, err)
		return
	}
	out, err := in.Compose(r.Context(), env, nil, "ps", "--all", "--format", "json")
	if err != nil {
		replyErr(w, err)
		return
	}
	list, err := parseServices(out)
	if err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, list)
}

// parseServices reads `compose ps --format json`: one object per line, or (older Compose) one
// array.
func parseServices(out []byte) ([]Service, error) {
	type row struct {
		Service, State, Health, Image string
		ExitCode                      int
	}
	var rows []row
	trimmed := bytes.TrimSpace(out)
	if bytes.HasPrefix(trimmed, []byte("[")) {
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return nil, fmt.Errorf("compose ps: %w", err)
		}
	} else {
		sc := bufio.NewScanner(bytes.NewReader(trimmed))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			if len(bytes.TrimSpace(sc.Bytes())) == 0 {
				continue
			}
			var r row
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				return nil, fmt.Errorf("compose ps: %w", err)
			}
			rows = append(rows, r)
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	list := []Service{}
	for _, r := range rows {
		list = append(list, Service(r))
	}
	return list, nil
}

// knownService refuses a name the stack does not define.
func (h *Hostd) knownService(ctx context.Context, in *Installer, env *EnvFile, svc string) error {
	if !serviceName.MatchString(svc) {
		return fail(http.StatusBadRequest, "%q is not a service name", svc)
	}
	out, err := in.Compose(ctx, env, nil, "config", "--services")
	if err != nil {
		return err
	}
	for _, s := range strings.Fields(string(out)) {
		if s == svc {
			return nil
		}
	}
	return fail(http.StatusNotFound, "the stack has no service %s", svc)
}

func (h *Hostd) logs(w http.ResponseWriter, r *http.Request) {
	in, _ := h.run()
	env, err := h.installed(in)
	if err != nil {
		replyErr(w, err)
		return
	}
	tail := 200
	if t := r.URL.Query().Get("tail"); t != "" {
		if tail, err = strconv.Atoi(t); err != nil || tail < 1 || tail > 5000 {
			replyErr(w, fail(http.StatusBadRequest, "tail %q is not a number from 1 to 5000", t))
			return
		}
	}
	svc := r.PathValue("service")
	if err := h.knownService(r.Context(), in, env, svc); err != nil {
		replyErr(w, err)
		return
	}
	out, err := in.Compose(r.Context(), env, nil, "logs", "--no-color", "--tail", strconv.Itoa(tail), svc)
	if err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]string{"logs": string(out)})
}

func (h *Hostd) serviceAction(w http.ResponseWriter, r *http.Request) {
	in, _ := h.run()
	if _, err := h.installed(in); err != nil {
		replyErr(w, err)
		return
	}
	var args []string
	switch a := r.PathValue("action"); a {
	case "restart", "stop":
		args = []string{a, "--timeout", "30"}
	case "start":
		args = []string{a}
	default:
		replyErr(w, fail(http.StatusNotFound, "no action %q: restart, stop or start", a))
		return
	}
	if err := h.booting(r.Context()); err != nil {
		replyErr(w, err)
		return
	}
	// Under the install's lifecycle lock — never during an install, upgrade, rollback, provision or reset-db —
	// and with the install as it is under it.
	unlock, err := lockFile(in.path(lifecycleLock))
	if errors.Is(err, errLocked) {
		replyErr(w, fail(http.StatusConflict, "an install, upgrade, rollback, provision or reset-db is running: wait for it to finish"))
		return
	}
	if err != nil {
		replyErr(w, err)
		return
	}
	defer unlock()
	env, err := in.open()
	if err != nil {
		replyErr(w, err)
		return
	}
	// Under the lock, which a restore holds while it marks them.
	if err := in.volumesSplit(); args[0] != "stop" && err != nil {
		replyErr(w, fail(http.StatusConflict, "%v", err))
		return
	}
	svc := r.PathValue("service")
	if err := h.knownService(r.Context(), in, env, svc); err != nil {
		replyErr(w, err)
		return
	}
	if _, err := in.Compose(r.Context(), env, nil, append(args, svc)...); err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]string{"service": svc, "action": r.PathValue("action")})
}

// ProvisionRequest is POST /v1/provision's body, which may be empty.
type ProvisionRequest struct {
	// Set corrects, before the hook runs, an input only the hook reads, or sets a value it
	// returned before or declares it returns (empty forgets that value), as `provision --set` does.
	Set map[string]string `json:"set,omitempty"`
}

func (h *Hostd) provision(w http.ResponseWriter, r *http.Request) {
	var req ProvisionRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	in, out := h.run()
	in.O.Set = req.Set
	if _, err := h.installed(in); err != nil {
		replyErr(w, err)
		return
	}
	if err := h.booting(r.Context()); err != nil {
		replyErr(w, err)
		return
	}
	if err := in.Provision(r.Context()); err != nil {
		replyErr(w, fmt.Errorf("%w\n%s", err, out.String()))
		return
	}
	reply(w, http.StatusOK, map[string]string{"output": out.String()})
}

// ResetRequest is POST /v1/reset-db's body.
type ResetRequest struct {
	// Confirm is the release the operator confirmed wiping (GET /v1/reset-db's target): required,
	// and another is refused.
	Confirm     *Version `json:"confirm"`
	Token       string   `json:"token"`                  // the token of the GET that showed the plan
	NoProvision bool     `json:"no_provision,omitempty"` // start the stack without running the provision hook
}

// ResetPlan is what GET /v1/reset-db answers: the release the job resets and what it does.
type ResetPlan struct {
	Target Version `json:"target"`
	Plan   string  `json:"plan"`
	// Token is POST's to send back: each GET makes a new one, a POST uses it up.
	Token string `json:"token"`
}

// resetCheck refuses a reset the daemon would refuse: before the install, until the health gate
// judged the boot (it would read the stopped stack as a failed boot), and when the install runs
// a release other than the OS's, whose installer the job runs.
func (h *Hostd) resetCheck(ctx context.Context, in *Installer) (ResetPlan, error) {
	if err := in.hasDatabase("reset-db has nothing to reset"); err != nil {
		return ResetPlan{}, fail(http.StatusConflict, "%v", err)
	}
	env, err := h.installed(in)
	if err != nil {
		return ResetPlan{}, err
	}
	if err := h.judged(ctx); err != nil {
		return ResetPlan{}, err
	}
	m := h.R.Manifest
	target := Version{m.ProjectVersion, m.Revision}
	if name, v, rev := runs(env); name != m.Name || v != m.ProjectVersion || rev != strconv.Itoa(m.Revision) {
		return ResetPlan{}, fail(http.StatusConflict, "the install runs %s %s (revision %s), not this OS's %s %s: reset-db resets the release the OS carries", name, v, rev, m.Name, target)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.rebooting:
		return ResetPlan{}, fail(http.StatusConflict, "a reboot is scheduled")
	case h.job != nil && h.job.State == "running":
		return ResetPlan{}, fail(http.StatusConflict, "a job (%s) is running", h.job.Kind)
	}
	return ResetPlan{Target: target, Plan: in.resetPlan()}, nil
}

func (h *Hostd) resetDBPlan(w http.ResponseWriter, r *http.Request) {
	in, _ := h.run()
	p, err := h.resetCheck(r.Context(), in)
	if err != nil {
		replyErr(w, err)
		return
	}
	p.Token = rand.Text()
	h.mu.Lock()
	h.resetToken = p.Token
	h.mu.Unlock()
	reply(w, http.StatusOK, p)
}

// resetDB runs reset-db as a job: the install back to its first setup, without a reboot.
func (h *Hostd) resetDB(w http.ResponseWriter, r *http.Request) {
	var req ResetRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	if req.Confirm == nil {
		replyErr(w, fail(http.StatusBadRequest, "reset-db wipes the install's data: confirm the release it resets (\"confirm\": {\"version\", \"revision\"}, GET reset-db's target)"))
		return
	}
	in, _ := h.run()
	p, err := h.resetCheck(r.Context(), in)
	if err != nil {
		replyErr(w, err)
		return
	}
	if *req.Confirm != p.Target {
		replyErr(w, fail(http.StatusConflict, "the install runs %s %s, not %s, which was confirmed: look again", h.R.Manifest.Name, p.Target, *req.Confirm))
		return
	}
	// The token a GET made, used once: a POST sent again, or one no GET preceded, is refused.
	j, err := h.startJob("reset-db", p.Target.String(), func() error {
		if req.Token == "" || subtle.ConstantTimeCompare([]byte(req.Token), []byte(h.resetToken)) != 1 {
			return fail(http.StatusConflict, "reset-db needs the token of the GET that showed its plan, once: look again")
		}
		h.resetToken = ""
		return nil
	})
	if err != nil {
		replyErr(w, err)
		return
	}
	go func() {
		jw := &jobWriter{h: h, j: j}
		in.O.Out, in.O.Log, in.O.Yes, in.O.NoProvision = jw, jw, true, req.NoProvision
		err := in.Reset(h.ctx)
		jw.flush()
		h.finish(j, err)
	}()
	accepted(w, j)
}

// target finds the release an upgrade on channel would install: version's latest revision, or
// the channel's latest release.
func (h *Hostd) target(ctx context.Context, in *Installer, env *EnvFile, channel, v string) (*release.Manifest, string, error) {
	if channel == "" {
		channel, _ = env.Get(ChannelVar)
	}
	if channel == "" {
		return nil, "", fail(http.StatusBadRequest, "the install follows no channel: name one (--channel)")
	}
	if !lint.ChannelName(channel) {
		return nil, "", fail(http.StatusBadRequest, "%q is not a channel name", channel)
	}
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	c, err := in.registryClient(prefix)
	if err != nil {
		return nil, "", err
	}
	trust, err := trusted("the OS's "+h.R.Ctl(), h.R.Keys)
	if err != nil {
		return nil, "", err
	}
	var m *release.Manifest
	if v != "" {
		m, err = findRelease(ctx, c, prefix, h.R.Spec.Name, channel, v, trust, in.O.logf)
	} else {
		repo := prefix + "/" + h.R.Spec.Name + "-release"
		var d v1.Hash
		m, d, err = readRelease(ctx, c, repo, channel)
		if err == nil && m == nil {
			err = fmt.Errorf("the channel %s has no release in %s", channel, prefix)
		}
		if err == nil {
			var r name.Repository
			if r, err = c.Repo(repo); err == nil {
				err = checkSigned(ctx, c, r, d, m, trust)
			}
		}
		// A pointer left on a publication its tag no longer holds (yanked or rebuilt since; the
		// old one stays validly signed) is no pointer, as the publisher reads it.
		if err == nil && m.Yanked != nil {
			err = fmt.Errorf("the channel %s's release %s is yanked: %s", channel, m.Tag, m.Yanked.Reason)
		}
		if err == nil {
			var td v1.Hash
			var tm *release.Manifest
			if tm, td, err = readRelease(ctx, c, repo, m.Tag); err == nil && (tm == nil || td != d) {
				err = fmt.Errorf("the channel %s's pointer names %s, which its tag no longer holds: the channel is being changed, try again later", channel, m.Tag)
			}
		}
	}
	if err != nil {
		return nil, "", fail(http.StatusBadGateway, "%v", err)
	}
	if m.Name != h.R.Spec.Name || m.Channel != channel {
		return nil, "", fail(http.StatusBadGateway, "the release found is %s's on %s, not %s's on %s", m.Name, m.Channel, h.R.Spec.Name, channel)
	}
	return m, channel, nil
}

// after reports whether m comes after the release the OS carries, by the scheme m records; a
// `command` scheme, which the daemon does not run, orders by revision within one version only.
func (h *Hostd) after(m *release.Manifest) (bool, error) {
	cur := h.R.Manifest
	c, err := compareTo(m, cur.ProjectVersion)
	if err != nil {
		return false, err
	}
	if m.Scheme.Provider == "command" && m.ProjectVersion != cur.ProjectVersion {
		return true, nil
	}
	if c == 0 {
		return m.Revision > cur.Revision, nil
	}
	return c < 0, nil
}

func (h *Hostd) updates(w http.ResponseWriter, r *http.Request) {
	in, _ := h.run()
	env, err := h.installed(in)
	if err != nil {
		replyErr(w, err)
		return
	}
	q := r.URL.Query()
	m, channel, err := h.target(r.Context(), in, env, q.Get("channel"), q.Get("version"))
	if err != nil {
		replyErr(w, err)
		return
	}
	newer, err := h.after(m)
	if err != nil {
		replyErr(w, err)
		return
	}
	cur := h.R.Manifest
	left := in.leftByRollback(m.ProjectVersion, m.Revision)
	reply(w, http.StatusOK, Update{
		Channel: channel, Current: Version{cur.ProjectVersion, cur.Revision}, Target: Version{m.ProjectVersion, m.Revision},
		Available: newer && m.Yanked == nil && m.Artifacts.OS != "" && !left, LeftByRollback: left, Changelog: m.Changelog.Project,
	})
}

// booting refuses until <name>.service ran (active, or failed): before, the boot is queued or is
// installing, upgrading or going back. A host without the unit has no boot to wait for.
func (h *Hostd) booting(ctx context.Context) error {
	if h.Docker {
		return nil // no boot unit: the install is brought up by the installer
	}
	st, err := h.bootState(ctx)
	if err != nil {
		return err
	}
	if st != "active" && st != "failed" && st != noBootUnit {
		return fail(http.StatusConflict, "the boot is still bringing the install up (%s.service is %s): wait for it", h.R.Spec.Name, st)
	}
	return nil
}

// judged refuses as booting does, and then until the health gate judged this boot: an upgrade or
// a rollback during the judgement would reboot before the boot assessment is settled.
func (h *Hostd) judged(ctx context.Context) error {
	if h.Docker {
		return nil // no health gate
	}
	if err := h.booting(ctx); err != nil {
		return err
	}
	unit := h.R.Spec.Name + "-health.service"
	st, err := unitState(ctx, h.Host, unit)
	if err != nil {
		return err
	}
	// RemainAfterExit: a judged boot leaves the gate active, or failed.
	if st != "active" && st != "failed" && st != noBootUnit {
		return fail(http.StatusConflict, "the health gate has not judged this boot yet (%s is %s): wait for it, or `systemctl start %s` if it does not run", unit, st, unit)
	}
	return nil
}

// noBootUnit is bootState's answer on a host without <name>.service.
const noBootUnit = "not-found"

// bootState is <name>.service's ActiveState, or noBootUnit when systemd has no such unit.
func (h *Hostd) bootState(ctx context.Context) (string, error) {
	return unitState(ctx, h.Host, h.R.Spec.Name+".service")
}

// startJob claims the one job slot, and journals the job's start.
// guard, when set, refuses under the same lock.
func (h *Hostd) startJob(kind, target string, guard func() error) (*job, error) {
	j, err := h.claimJob(kind, target, guard)
	if err != nil {
		return nil, err
	}
	started := j.Started
	// The number this start was given: set before the job runs, so before its end is journaled.
	// None when it could not be journaled: no number stands in for it.
	if last, err := h.event(Event{Kind: evJobStarted, Job: &started, JobKind: kind, Target: target}); err != nil {
		j.eventsErr = fmt.Sprintf("the job's start could not be journaled: %v", err)
	} else {
		j.events = &last
	}
	return j, nil
}

// jobRunning is a job refused because another one runs.
type jobRunning struct{ err error }

func (e jobRunning) Error() string { return e.err.Error() }
func (e jobRunning) Unwrap() error { return e.err }

func (h *Hostd) claimJob(kind, target string, guard func() error) (*job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.rebooting:
		return nil, fail(http.StatusConflict, "a reboot is scheduled")
	case h.job != nil && h.job.State == "running":
		return nil, jobRunning{fail(http.StatusConflict, "a job (%s) is running", h.job.Kind)}
	}
	if guard != nil {
		if err := guard(); err != nil {
			return nil, err
		}
	}
	h.job = &job{Started: time.Now().UTC(), Kind: kind, State: "running", Target: target}
	h.jobEnd = make(chan struct{})
	h.slotGen++
	return h.job, nil
}

// ended sets j's end under mu, and wakes the runs waiting for the slot. The caller holds mu.
func (h *Hostd) ended(j *job, state, msg string) {
	j.State, j.Error = state, msg
	h.slotGen++
	if h.job == j && h.jobEnd != nil {
		close(h.jobEnd)
		h.jobEnd = nil
	}
}

// startJobWhenFree is startJob without a guard for a run that waits its turn: while another job
// runs, it waits for that one's end (woken by it, at once) and tries again, for at most wait
// (then it is refused as startJob refuses); any other refusal, or ctx or the daemon ending,
// answers at once. prepare is run before each try, outside the lock, and gives the job's target:
// what the run reads (settings, secrets) is read once the slot is free, never before the wait; an
// error from it is the answer. The job starts only when no job claimed the slot or ended since
// prepare began, so it never runs on values read before another job's end: when one did, prepare
// runs again.
func (h *Hostd) startJobWhenFree(ctx context.Context, wait time.Duration, kind string, prepare func() (string, error)) (*job, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		h.mu.Lock()
		gen := h.slotGen
		h.mu.Unlock()
		target, err := prepare()
		if err != nil {
			return nil, err
		}
		j, err := h.startJob(kind, target, func() error {
			if h.slotGen != gen {
				return errSlotMoved
			}
			return nil
		})
		if errors.Is(err, errSlotMoved) {
			continue
		}
		if err == nil || !errors.As(err, new(jobRunning)) || wait <= 0 {
			return j, err
		}
		// The running job's: nil once it ended, as it may have since the refusal.
		h.mu.Lock()
		end := h.jobEnd
		h.mu.Unlock()
		if end == nil {
			continue
		}
		select {
		case <-end:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.ctx.Done():
			return nil, err
		case <-deadline.C:
			return nil, err
		}
	}
}

// errSlotMoved is a claim refused because a job claimed the slot or ended since the claimant
// read what it runs with.
var errSlotMoved = errors.New("the job slot moved")

// logf adds a line to j's log. Past twice maxLog lines the first are dropped, keeping the last
// maxLog: the copy happens once every maxLog lines.
func (h *Hostd) logf(j *job, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	h.mu.Lock()
	defer h.mu.Unlock()
	j.Log = append(j.Log, line)
	if over := len(j.Log) - maxLog; len(j.Log) > 2*maxLog {
		j.Log = append(j.Log[:0:0], j.Log[over:]...)
		j.dropped += over
	}
}

// jobWriter adds what a host program prints to a job's log, line by line.
type jobWriter struct {
	h    *Hostd
	j    *job
	part []byte
	cr   bool // the last write ended with a carriage return
}

// Write ends a line at a line feed, a carriage return (a progress line rewritten in place) or
// both, and cuts every line into pieces of at most maxLine bytes; a blank line stays one.
func (w *jobWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.cr && len(p) > 0 && p[0] == '\n' {
		p = p[1:] // the line feed of a CR LF the last write ended inside
	}
	w.cr = false
	w.part = append(w.part, p...)
	for {
		i := bytes.IndexAny(w.part, "\r\n")
		if i < 0 {
			for len(w.part) > maxLine {
				w.h.logf(w.j, "%s", string(w.part[:maxLine]))
				w.part = w.part[maxLine:]
			}
			break
		}
		line := w.part[:i]
		for len(line) > maxLine {
			w.h.logf(w.j, "%s", string(line[:maxLine]))
			line = line[maxLine:]
		}
		w.h.logf(w.j, "%s", string(line))
		end := i + 1
		if w.part[i] == '\r' {
			if end == len(w.part) {
				w.cr = true
			} else if w.part[end] == '\n' {
				end++
			}
		}
		w.part = w.part[end:]
	}
	return n, nil
}

func (w *jobWriter) flush() {
	if len(w.part) > 0 {
		w.h.logf(w.j, "%s", string(w.part))
		w.part = w.part[:0]
	}
}

// finish ends j, failed when err is set. Its end is journaled first: a client that sees the job
// ended finds its job.ended in the journal.
func (h *Hostd) finish(j *job, err error) {
	state, msg := "done", ""
	if err != nil {
		state, msg = "failed", err.Error()
	}
	started := j.Started // set once, at the job's start
	h.mu.Lock()
	result := j.Result
	h.mu.Unlock()
	h.event(Event{Kind: evJobEnded, Job: &started, JobKind: j.Kind, Target: j.Target, State: state, Error: msg, Result: result})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ended(j, state, msg)
}

// upgrade writes the target release's OS into the passive slot with kairos-agent, pulling it by
// digest with the install's pull account, then reboots into it: the boot upgrades the install.
func (h *Hostd) upgrade(w http.ResponseWriter, r *http.Request) {
	var req UpgradeRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	// The backup before an upgrade is what a failed migration is restored from: the operator's
	// alone to skip.
	if req.SkipBackup && fromApp(r) {
		replyErr(w, fail(http.StatusForbidden, "skip_backup is the operator's: upgrade --skip-backup over SSH"))
		return
	}
	in, _ := h.run()
	env, err := h.installed(in)
	if err != nil {
		replyErr(w, err)
		return
	}
	// A restore again finishes half restored volumes, onto this release only.
	if err := in.volumesSplit(); err != nil {
		replyErr(w, fail(http.StatusConflict, "%v", err))
		return
	}
	if err := h.judged(r.Context()); err != nil {
		replyErr(w, err)
		return
	}
	m, _, err := h.target(r.Context(), in, env, req.Channel, req.Version)
	if err != nil {
		replyErr(w, err)
		return
	}
	newer, err := h.after(m)
	switch {
	case err != nil:
		replyErr(w, err)
		return
	case req.Confirm != nil && *req.Confirm != (Version{m.ProjectVersion, m.Revision}):
		replyErr(w, fail(http.StatusConflict, "the release found is %s %s (revision %d), not %s, which was confirmed: look again with updates", m.Name, m.ProjectVersion, m.Revision, *req.Confirm))
		return
	case in.leftByRollback(m.ProjectVersion, m.Revision):
		replyErr(w, fail(http.StatusConflict, "the install went back from %s %s (revision %d): the boot would not upgrade to it again (remove %s to try it again)", m.Name, m.ProjectVersion, m.Revision, in.path(FailedDir)))
		return
	case m.Yanked != nil:
		replyErr(w, fail(http.StatusConflict, "%s %s is yanked: %s", m.Name, m.Tag, m.Yanked.Reason))
		return
	case !newer:
		replyErr(w, fail(http.StatusConflict, "%s %s (revision %d) does not come after %s (revision %d), which this OS runs", m.Name, m.ProjectVersion, m.Revision, h.R.Manifest.ProjectVersion, h.R.Manifest.Revision))
		return
	case m.Artifacts.OS == "":
		replyErr(w, fail(http.StatusConflict, "%s %s was published without an OS bundle", m.Name, m.Tag))
		return
	}
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	src := fmt.Sprintf("%s/%s-os@%s", prefix, m.Name, m.Artifacts.OS)
	// Checked as the job starts: another upgrade could replace the running system's copy in the
	// passive slot.
	j, err := h.startJob("upgrade", m.Tag, func() error {
		if err := h.osWritten(); err != nil {
			return fail(http.StatusConflict, "%v: reboot first, then upgrade", err)
		}
		// Again as the job claims the slot: a restore that just ended may have marked them.
		if err := in.volumesSplit(); err != nil {
			return fail(http.StatusConflict, "%v", err)
		}
		return nil
	})
	if err != nil {
		replyErr(w, err)
		return
	}
	reboot := req.Reboot == nil || *req.Reboot
	go func() {
		if !req.SkipBackup {
			h.preflightBackup(in, func(format string, a ...any) { h.logf(j, format, a...) })
		}
		// Marked before kairos-agent runs, and kept when it fails: it may have moved the running
		// system to the passive slot already.
		err := os.WriteFile(filepath.Join(h.Run, osWritten), []byte(m.Tag+"\n"), 0o600)
		if err == nil {
			err = h.writeOS(in, j, prefix, src)
		}
		if err == nil {
			err = h.retry(j)
		}
		switch {
		case err == nil && reboot:
			if err = h.schedule("reboot", 30, false, j, fmt.Sprintf("rebooting into %s in 30 s: the boot upgrades the install", m.Tag)); err == nil {
				return // ended by schedule, with its reboot
			}
		case err == nil:
			h.logf(j, "%s is written: the next boot runs it and upgrades the install", m.Tag)
		}
		h.finish(j, err)
	}()
	accepted(w, j)
}

// writeOS runs kairos-agent upgrade from src, with a private Docker config holding the pull
// account (none when the registry is public).
func (h *Hostd) writeOS(in *Installer, j *job, prefix, src string) error {
	ctx, cancel := context.WithTimeout(h.ctx, 30*time.Minute)
	defer cancel()
	h.logf(j, "writing %s to the passive slot", src)
	jw := &jobWriter{h: h, j: j}
	err := writeOSBundle(ctx, in, h.Host, h.Run, prefix, src, jw)
	jw.flush()
	return err
}

// writeOSBundle runs kairos-agent upgrade from src (<prefix>/<name>-os@<digest>) through host,
// its output streamed to w, with a private Docker config under dir holding the install's pull
// account for prefix (none when the registry is public).
func writeOSBundle(ctx context.Context, in *Installer, host Tool, dir, prefix, src string, w io.Writer) error {
	user, token, err := in.pullAccount(prefix)
	if err != nil {
		return err
	}
	env := os.Environ()
	if token != "" {
		repo, err := name.NewRepository(prefix)
		if err != nil {
			return err
		}
		tmp, err := os.MkdirTemp(dir, ".pull-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		if err := registry.WriteDockerConfig(tmp, repo.Registry, &authn.AuthConfig{Username: user, Password: token}, nil); err != nil {
			return err
		}
		env = append(env, "DOCKER_CONFIG="+tmp)
	}
	if _, err := host.Run(ctx, Cmd{Args: []string{"kairos-agent", "upgrade", "--source", "oci:" + src}, Env: env, Stream: w}); err != nil {
		return fmt.Errorf("kairos-agent upgrade: %w", err)
	}
	return nil
}

func (h *Hostd) jobStatus(w http.ResponseWriter, r *http.Request) {
	since := 0
	if s := r.URL.Query().Get("since"); s != "" {
		var err error
		if since, err = strconv.Atoi(s); err != nil || since < 0 {
			replyErr(w, fail(http.StatusBadRequest, "since %q is not a line number", s))
			return
		}
	}
	// Copied under the lock, written after it: a slow reader never holds the job up.
	st := JobStatus{State: "idle", Log: []string{}}
	h.mu.Lock()
	if j := h.job; j != nil {
		started := j.Started
		st = JobStatus{Started: &started, Kind: j.Kind, State: j.State, Target: j.Target, Error: j.Error, Result: j.Result, Reboot: j.Reboot, Log: []string{}, LogTotal: j.dropped + len(j.Log)}
		if from := max(since-j.dropped, 0); from < len(j.Log) {
			st.Log = append(st.Log, j.Log[from:]...)
		}
	}
	h.mu.Unlock()
	reply(w, http.StatusOK, st)
}

func (h *Hostd) power(w http.ResponseWriter, r *http.Request, action string) {
	var req PowerRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	drain := 30
	if req.DrainSecs != nil {
		drain = *req.DrainSecs
	}
	if drain < 0 || drain > 600 {
		replyErr(w, fail(http.StatusBadRequest, "drain_secs %d is not from 0 to 600", drain))
		return
	}
	// Not during an install, upgrade, rollback, provision or reset-db, but for the boot's: a boot that hangs
	// can be power-cycled (the next boot finishes what it stopped).
	if st, err := h.bootState(r.Context()); err != nil {
		replyErr(w, err)
		return
	} else if st != "activating" {
		in, _ := h.run()
		unlock, err := lockFile(in.path(lifecycleLock))
		if errors.Is(err, errLocked) {
			replyErr(w, fail(http.StatusConflict, "an install, upgrade, rollback, provision or reset-db is running: wait for it to finish"))
			return
		}
		if err == nil {
			unlock()
		}
	}
	if err := h.schedule(action, drain, true, nil, ""); err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusAccepted, map[string]any{"action": action, "drain_secs": drain})
}

// schedule runs `systemctl action` after drain units; once scheduled, no job starts and no other
// reboot is scheduled. idle refuses while a job runs (checked under the lock a job starts under);
// the job that reboots at its end passes false, and itself as by: once the reboot is claimed,
// schedule ends the job (endRebooting) before arming it, and the caller does not finish it —
// an error comes before that. note, when set, goes to the job's log once the reboot is claimed,
// before the job ends. Both the scheduling and a systemctl that fails are journaled; the job's
// end stands when its reboot then fails.
func (h *Hostd) schedule(action string, drain int, idle bool, by *job, note string) error {
	if err := h.claimReboot(idle); err != nil {
		return err
	}
	if by != nil {
		if note != "" {
			h.logf(by, "%s", note)
		}
		h.endRebooting(by, action, drain)
	} else {
		h.event(h.scheduledEvent(action, drain, nil))
	}
	time.AfterFunc(time.Duration(drain)*cmp.Or(h.Unit, time.Second), func() {
		if _, err := h.Host.Run(h.ctx, Cmd{Args: []string{"systemctl", action}}); err != nil {
			reason := fmt.Sprintf("systemctl %s failed: %v", action, err)
			h.O.logf("%s", reason)
			if by != nil {
				h.logf(by, "%s: the system keeps running", reason)
			}
			h.mu.Lock()
			h.rebooting = false
			h.mu.Unlock()
			h.rebootCancelled(action, reason, by)
		}
	})
	return nil
}

func (h *Hostd) claimReboot(idle bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rebooting {
		return fail(http.StatusConflict, "a reboot is scheduled")
	}
	if idle && h.job != nil && h.job.State == "running" {
		return fail(http.StatusConflict, "a job (%s) is running: wait for it to finish", h.job.Kind)
	}
	h.rebooting = true
	return nil
}

// scheduledEvent is the entry of a reboot or power-off scheduled in drain seconds, by the job by
// (nil: asked for). Its boot id is the append's (journal.update), never read apart from it.
func (h *Hostd) scheduledEvent(action string, drain int, by *job) Event {
	return Event{Kind: evRebootScheduled, Action: action, DrainSecs: &drain, By: jobID(by)}
}

// rebootCancelled journals a scheduled reboot or power-off that did not happen, and why; by is
// the job that scheduled it (nil: asked for).
func (h *Hostd) rebootCancelled(action, reason string, by *job) {
	h.event(Event{Kind: evRebootCancelled, Action: action, Reason: reason, By: jobID(by)})
}

// jobID is j's identity in the journal (its start); nil for no job.
func jobID(j *job) *time.Time {
	if j == nil {
		return nil
	}
	started := j.Started // set once, at the job's start
	return &started
}

// endRebooting ends j, done, with the reboot or power-off it schedules in drain seconds as its
// last step. The reboot's entry and the job's end are one append, so a long poll never answers
// one without the other, and the job's state changes only once both are journaled. The caller
// has claimed the reboot (rebooting) and runs it only after this returns: a shutdown that stops
// the daemon cannot leave the job unended.
func (h *Hostd) endRebooting(j *job, action string, drain int) {
	h.mu.Lock()
	result := j.Result
	h.mu.Unlock()
	h.event(h.scheduledEvent(action, drain, j),
		Event{Kind: evJobEnded, Job: jobID(j), JobKind: j.Kind, Target: j.Target, State: "done", Result: result, Reboot: &EventReboot{Action: action, DrainSecs: drain}})
	h.mu.Lock()
	defer h.mu.Unlock()
	j.Reboot = &EventReboot{Action: action, DrainSecs: drain}
	h.ended(j, "done", "")
}

// HostTool runs host programs: Args[0] is the program.
type HostTool struct{}

// Run implements Tool.
func (HostTool) Run(ctx context.Context, c Cmd) ([]byte, error) {
	if len(c.Args) == 0 {
		return nil, errors.New("no program")
	}
	return ExecTool{Bin: c.Args[0]}.Run(ctx, Cmd{Args: c.Args[1:], Env: c.Env, Stdin: c.Stdin, Stream: c.Stream, Stdout: c.Stdout})
}
