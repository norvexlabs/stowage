package install

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
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
//	POST /v1/services/{service}/exec     {args}: a command run in the service's running container,
//	                                     without a terminal or input: {stdout, stderr, exit}, each
//	                                     output cut at maxExecOutput (operator only)
//	POST /v1/provision                   {set}: the provision hook again, set recorded first
//	GET  /v1/updates                     ?channel=C&version=V: the release an upgrade would install
//	POST /v1/upgrade                     {channel, version, confirm, reboot, skip_backup, dry_run,
//	                                     unordered}: an upgrade job (202, JobStarted), a backup first
//	                                     when one is configured; dry_run: every check before the job,
//	                                     no job (200, UpgradePlan); unordered: version under a `command`
//	                                     scheme, refused without it (unorderedTo)
//	GET  /v1/rollback                    {from, to}: what going back to the passive system would do
//	POST /v1/rollback                    {confirm, dry_run}: a rollback job, choosing the passive
//	                                     system, then a reboot (202, JobStarted); dry_run: every
//	                                     check before the job, no job (200, RollbackPlan)
//	GET  /v1/job                         ?since=N: the last job and its log from line N; log_total is
//	                                     the line to ask from next
//	GET  /v1/events                      ?after=N&wait=S: the event journal's entries after N, waiting
//	                                     up to S seconds for one (hostdevents.go)
//	POST /v1/reboot, /v1/shutdown        {drain_secs}: after the drain (202)
//	GET, POST /v1/network, POST /v1/network/{confirm,revert,reset}: the network override (hostdnet.go)
//	/v1/backup, /v1/backup/{config,status,list,verify}: the backups (hostdbackup.go)
//	/v1/secrets/{export,rotate,set,show}, /v1/restore: the install's secrets and a backup's restore (hostdsecrets.go)
//
// An error is {"error": "..."} with a 4xx or 5xx status. One job runs at a time, and none starts
// once a reboot is scheduled; every request that starts one answers 202 with JobStarted.

// AdminGroup is the group whose members may use the host daemon: Kairos's administrators.
const AdminGroup = "admin"

// StowageVersion is the Stowage release this installer was built with, which its command sets
// (stowage-install's build flags); empty, or "dev", is one of no release.
var StowageVersion string

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
	// RunNetworkd is where the secondary NICs' overrides are rendered; empty is
	// /run/systemd/network.
	RunNetworkd string
	// InstallNet is the file the cloud-config writes the install's network to; empty is
	// InstallNetworkFile.
	InstallNet string
	// SysNet is the kernel's NIC directory, which the VM's NICs are read from (deviceNICs); empty
	// is sysNetDir.
	SysNet string
	// UdevData is udev's records of the NICs, which their drivers are read from (nicDriver);
	// empty is udevDataDir.
	UdevData string
	// Systemd is where the daemon writes runtime unit drop-ins (the backup timer's schedule);
	// empty is /run/systemd/system.
	Systemd string
	// Statfs reads the space of a directory's filesystem; nil is the system's.
	Statfs func(dir string) *Disk
	// Solicit sends a router solicitation on a link; nil is sendRouterSolicitation. Tests set it.
	Solicit func(iface string) error
	// Docker: the daemon of a Docker install, run in the installer's process (dockerhost.go): no
	// boot, no health gate, no banner; its units the installer's own; DiskDir the Docker engine's
	// data, whose filesystem the disk-pressure watch reads.
	Docker  bool
	DiskDir string
	// Stowage is the Stowage release this daemon was built with (StowageVersion): an upgrade's
	// values are judged here only for a release that Stowage published (valuesCheck).
	Stowage string

	// held is the install's lock a rollback keeps to its reboot: the daemon holds it so the
	// collector never closes its file, which would release it, before the reboot.
	held      func()
	mu        sync.Mutex
	job       *job
	rebooting bool
	// rebootGen, under mu, moves at every reboot or power-off scheduled or cancelled: the service
	// watch tells a look one cut.
	rebootGen uint64
	// jobEnd, under mu, is closed when the running job ends (startJobWhenFree waits on it).
	jobEnd chan struct{}
	// slotGen, under mu, moves at every claim of the job slot and every job's end: a run that
	// read what it runs with at one generation starts only at that same one (startJobWhenFree).
	slotGen uint64
	// resetToken is the token the last GET reset-db made, which one POST may use, for the plan
	// that GET showed: resetShown (a crossing or not, and its target).
	resetToken string
	resetShown shownReset
	// checkedPlan is the crossing a GET's resetPlan just checked, which the GET turns into
	// crossPlan with its token.
	checkedPlan *crossPlanned
	// crossPlan is the last crossing a reset-db plan checked (resetPlan), which the next plan of
	// the same target installer for the same install takes as it is.
	crossPlan *crossPlanned
	ctx       context.Context // the daemon's: a job outlives its request

	// netMu orders the network changes and their reverts; netReverts is each NIC's armed revert
	// by role (empty: the primary): one NIC's arming or disarming never touches another's.
	netMu      sync.Mutex
	netReverts map[string]*netRevert
	// healthMu guards healthRun, GET /v1/health's check running or last done: its verdict answers
	// every request until healthReuseSecs after it ended.
	healthMu  sync.Mutex
	healthRun *healthCheck
	// stateGen counts the changes of what the install runs (changed): a health verdict of an
	// earlier one is not reused, nor the services the stack declares (declared).
	stateGen atomic.Uint64
	// declaredMu guards declaredKey and declaredNames: the services the stack declares, as read
	// for that key (declared).
	declaredMu    sync.Mutex
	declaredKey   string
	declaredNames []string
	// afterFunc arms a revert's timer: time.AfterFunc, or a test's, which fires it itself.
	afterFunc func(d time.Duration, f func()) stopper
	// onWait, a test's, is told what a request waits on ("slot": a running job's end; "events":
	// the journal's next event) as it starts waiting: the test acts then, never before.
	onWait func(what string)
	// installNetLogged is the last bad install network record's error logged: the network is
	// polled, and the record is rewritten the same at every boot, so each error is logged once.
	installNetLogged atomic.Value // a string
	// unnamedLogged is the last error that left the NICs no role names unlisted, logged once.
	unnamedLogged atomic.Value // a string
	// backupMu orders the backup settings' reads and changes; backingUp is set while an upgrade
	// takes its backup first.
	backupMu  sync.Mutex
	backingUp atomic.Bool
	// backups counts the backups running: Serve waits for them to clean up as it stops.
	backups sync.WaitGroup
	// rollbacks counts the rollbacks running, each counted under mu as its job is claimed: Serve
	// waits for one stopped in its grace to journal that its reboot does not come. stopping, under
	// mu, refuses every job once Serve stops, so no rollback is counted once it waits.
	rollbacks sync.WaitGroup
	stopping  bool
	// pressMu guards lastPost, the disk-pressure watch's last POST.
	pressMu  sync.Mutex
	lastPost *PressurePost
	// bannerOwed: the login screen's reload after the disk banner's last change failed. The
	// pressure watch alone, one goroutine, reads and writes it.
	bannerOwed bool
	// evMu guards evCh, closed at the daemon's next append to the event journal.
	evMu sync.Mutex
	evCh chan struct{}
	// onEvent, when set (tests), is given the entries of each append event makes.
	onEvent func(evs []Event)
	// bootRec holds this boot's boot.started once read (boot).
	bootRec bootCache
	// carriedMu guards carriedKey and carriedSpec: the spec of the release installed as
	// carriedRelease told it, for the release and the kept installer's file (its size and time)
	// it was read from, so a status reads and logs it once per change (installedSpec).
	carriedMu   sync.Mutex
	carriedKey  string
	carriedSpec *spec.Spec
	// route6Mu guards route6Warn, the warning of a primary link without an IPv6 default route
	// (route6Watch).
	route6Mu   sync.Mutex
	route6Warn string
	// tokenMu guards token, the app socket's token as .env held it when tokenFile described it
	// (appToken).
	tokenMu   sync.Mutex
	token     string
	tokenFile os.FileInfo
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

// Status is what GET /v1/status answers, and `status --json` prints: the core of InstallStatus's
// names, and the VM's own.
type Status struct {
	Format int     `json:"format"` // MachineFormat
	Name   string  `json:"name"`
	OS     Version `json:"os"` // the release this OS carries
	// Installed is the release the install runs; nil before the first install.
	Installed *Version `json:"installed"`
	// Previous is the release the last upgrade replaced, which `rollback` goes back to (the
	// passive system's); nil when the install keeps none. It names no installer: a VM's install
	// records none, as its OS carries the release.
	Previous *StatusRelease `json:"previous"`
	// Inputs are the keys of the inputs the install records, sorted: never a value. Nil (left out)
	// before the first install, or when the OS's installer carries another release than the one
	// installed and the install keeps no installer of it this one reads.
	Inputs *[]string `json:"inputs,omitempty"`
	// Values: asked with ?values=true (status --json --values), on the operator's socket alone, what
	// the install records that a configuration gives again (RecordedValues); left out otherwise,
	// before the first install, or when the installed release's spec cannot be read (what is
	// secret cannot be told), Inputs then listed from the install's record alone.
	Values *RecordedValues `json:"values,omitempty"`
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
	// Registry and FirstInstall are what the install's first run recorded, as Docker's status
	// --json gives them; left out before the first install.
	Registry string `json:"registry,omitempty"`
	FirstInstall
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
	// ResetFailed: on the recovery system, Kairos's automatic reset (kairos-reset.service, which
	// lays the system out at a first boot) failed, so the VM stays there; it holds the unit's last
	// journal lines. Empty otherwise.
	ResetFailed string `json:"reset_failed,omitempty"`
	// Fallback: Kairos booted the passive system because the last upgrade's boot failed its health
	// gate, or `rollback` chose it.
	Fallback bool `json:"fallback,omitempty"`
	// OnTrial: the boot runs an upgrade's new system, which the health gate has not judged yet —
	// healthy, it is kept; not, the gate goes back (or keeps it, as failed_boot then says).
	OnTrial bool `json:"on_trial,omitempty"`
	// FailedBoot is the last boot the health gate found unhealthy, its diagnostics kept, until a
	// later boot is judged healthy.
	FailedBoot    *FailedBoot `json:"failed_boot,omitempty"`
	RebootPending bool        `json:"reboot_pending"`
	// BootID is the kernel's id of this boot: a client that saw a reboot scheduled knows the
	// appliance rebooted when it changes. Empty when it cannot be read.
	BootID string `json:"boot_id,omitempty"`
	// BootSettled: this boot's units ran to an end — <name>.service (the boot's install, upgrade
	// or going back) and <name>-health.service (the health gate) are active or failed, or the
	// host has none (a Docker install): what the status says of this boot changes no more but by
	// a job. False while either runs or is queued, or when their state cannot be read.
	BootSettled bool `json:"boot_settled"`
	// BootJudged is the health gate's last verdict in this boot (its boot.judged entry); nil
	// before it, or when it cannot be told, BootJudgedError then saying why. Settled with none and
	// no error, the gate ended without a verdict.
	BootJudged      *BootJudged `json:"boot_judged,omitempty"`
	BootJudgedError string      `json:"boot_judged_error,omitempty"`
	// RegistryLogin is the host the VM holds a login for (`registry-login`), the release's own
	// registry's or its mirror's: never the login itself. Empty when it holds none.
	RegistryLogin string `json:"registry_login,omitempty"`
	// RegistryLoginError: the login file held cannot be read (the reads of its host fail on it),
	// naming the file and the fix; never a value.
	RegistryLoginError string `json:"registry_login_error,omitempty"`
	Job                string `json:"job"` // the last job's state, idle when none ran
	// Events is the event journal's last entry number (0 before the first): GET
	// /v1/events?after= it follows from there. Null when the journal cannot be read, and
	// EventsError then says why: a reader never takes a cursor the journal did not give.
	Events      *int64 `json:"events"`
	EventsError string `json:"events_error,omitempty"`
	// NetworkWarning: IPv6 is on and the primary link has had no IPv6 default route for a minute
	// or more; the daemon solicits the routers until one answers.
	NetworkWarning string `json:"network_warning,omitempty"`
}

// BootJudged is the health gate's verdict on a boot, as its boot.judged entry records it.
type BootJudged struct {
	Seq     int64  `json:"seq"` // the entry's number in the event journal
	Verdict string `json:"verdict"`
	OnTrial bool   `json:"on_trial"`
	// Reboots: the gate reboots now, into the passive system; false, no reboot comes from it.
	Reboots bool   `json:"reboots"`
	Reason  string `json:"reason,omitempty"`
	Kept    string `json:"kept,omitempty"`
	Error   string `json:"error,omitempty"`
}

// judgedOf is e's verdict; nil for none.
func judgedOf(e *Event) *BootJudged {
	if e == nil {
		return nil
	}
	return &BootJudged{Seq: e.Seq, Verdict: e.Verdict, OnTrial: e.OnTrial != nil && *e.OnTrial, Reboots: e.Reboots != nil && *e.Reboots, Reason: e.Reason, Kept: e.Kept, Error: e.Error}
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
	// ID is its container's, as compose ps prints it.
	ID string `json:"id,omitempty"`
	// OOMKilled: its memory cgroup killed its container's last run; MemLimit is the memory limit
	// its container runs with (0: none); Restarts, how many times its restart policy restarted it.
	OOMKilled bool  `json:"oom_killed,omitempty"`
	MemLimit  int64 `json:"mem_limit_bytes,omitempty"`
	Restarts  int   `json:"restarts,omitempty"`
}

// Update is what GET /v1/updates answers: the release an upgrade would install.
type Update struct {
	Channel   string  `json:"channel"`
	Current   Version `json:"current"`
	Target    Version `json:"target"`
	Available bool    `json:"available"` // the target comes after the current release, and may be installed
	// LeftByRollback: the install went back from the target, and the boot does not upgrade to it
	// again.
	LeftByRollback bool `json:"left_by_rollback,omitempty"`
	// Rewrite: the VM runs the passive system (a boot that failed went back to it), and the
	// target is the release it runs, whose OS an upgrade writes to the active slot again.
	Rewrite bool `json:"rewrite,omitempty"`
	// Installed is the release the install runs (none before the first install): after a crossing
	// whose boot fell back, a later one than the OS's.
	Installed *Version `json:"installed,omitempty"`
	// Passive: the VM runs the passive system, whose release `upgrade --version` finds to rewrite.
	Passive bool `json:"passive,omitempty"`
	// Follows is the channel the install follows, set on Docker and Kubernetes when --channel
	// named another and the target there comes after the current release: `upgrade --version`
	// reads the install's channel alone, so the target is not available.
	Follows string `json:"follows,omitempty"`
	// NoInstaller: on Docker and Kubernetes, the target comes after the current release on the
	// install's channel but was published without the installer `upgrade --version` fetches.
	NoInstaller bool   `json:"no_installer,omitempty"`
	Changelog   string `json:"changelog,omitempty"`
	// NeedsUnordered: on the VM, the target --version names is under a `command` scheme and is not
	// the release the install runs (needsUnordered): upgrade takes it only with --unordered.
	NeedsUnordered bool `json:"needs_unordered,omitempty"`
	// ChecksInstaller: an upgrade to the target downloads its installer and checks it against the
	// install before it writes the OS (checksTarget).
	ChecksInstaller bool `json:"checks_installer,omitempty"`
	// TakesInputs: an upgrade to the target gives the boot into it the inputs it adds (`upgrade
	// --set`, UpgradeRequest.Inputs): it moves the install to another release, and that release's
	// boot reads what an upgrade stages (takesStaged).
	TakesInputs bool `json:"takes_inputs,omitempty"`
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
	// DryRun runs every check before the job and answers the UpgradePlan: no job, nothing written.
	DryRun bool `json:"dry_run,omitempty"`
	// Unordered, with Version, takes a version a `command` scheme would order, which the daemon
	// cannot run (unorderedTo): the operator checked the order.
	Unordered bool `json:"unordered,omitempty"`
	// Inputs are values of inputs the release adds, KEY: value (a file input's as --set-file
	// encodes it), staged root-only for the boot into it, which takes them as given and removes
	// them: the operator's alone, and only for an upgrade that moves the install to another
	// release.
	Inputs map[string]string `json:"inputs,omitempty"`
}

// UpgradePlan is POST /v1/upgrade's answer to a dry run: every check passed, nothing started.
type UpgradePlan struct {
	To   Version `json:"to"`
	Slot string  `json:"slot"` // the slot its OS is written to: passive, or active for a rewrite
	Does string  `json:"does"` // what the boot into it does
	// Backup: the job takes a backup first (settings configured, no skip_backup, not a first
	// install nor a rewrite).
	Backup bool `json:"backup"`
	// Unchecked, when set, says why the daemon left the install's values to the boot into the
	// release (published by another Stowage, or a spec this one does not read): the job's log says
	// it too.
	Unchecked string `json:"unchecked,omitempty"`
}

// PowerRequest is POST /v1/reboot's and /v1/shutdown's body.
type PowerRequest struct {
	DrainSecs *int `json:"drain_secs,omitempty"` // default 30, at most 600
}

// serviceName is a compose service name.
var serviceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// Serve serves the API on the unix socket path until ctx ends. The socket and its directory are
// root's and Group's; without that group, root's alone. When the stack's services reach the daemon
// (shapes.kairos.host_api, the OS's release's or the installed one's: appServed), the API is
// served too on the app socket, in the directory app beside
// path, which those services mount: any process may connect to it, and every request carries the
// install's token (AppHandler).
func (h *Hostd) Serve(ctx context.Context, path string) error {
	// The daemon's own: whatever ends Serve (the stop, a server failing) stops what runs on.
	ctx, stopAll := context.WithCancel(ctx)
	defer stopAll()
	h.ctx = ctx
	h.mu.Lock()
	h.stopping = false
	h.mu.Unlock()
	// The journal tells boots apart by their ids (a reboot scheduled, then run or not): with none
	// read, such an entry would stay open for good, so the daemon does not start.
	if h.O.bootID() == "" {
		return fmt.Errorf("the kernel's boot id (%s) does not read: the host daemon tells boots apart by it", cmp.Or(h.O.BootIDFile, kernelBootID))
	}
	h.bootRec.again() // read now, whatever a read before Serve found
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
	// No job runs before the daemon serves: every rclone config a killed run left goes.
	sweepRclone(dir, 0)
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
	if h.appServed() {
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
	watched, servicesWatched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		h.pressureWatch(wctx)
	}()
	go h.certWatch(wctx)
	go func() {
		defer close(servicesWatched)
		h.serviceWatch(wctx)
	}()
	if !h.Docker {
		go h.route6Watch(wctx)
	}
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
	h.mu.Lock()
	h.stopping = true
	h.mu.Unlock()
	stopAll()   // a rollback in its grace ends with it, its cancel journaled
	stopWatch() // a check under way ends with it: no banner or log after Serve returns
	<-watched
	<-servicesWatched
	// A backup cut off by the stop removes its runner and the object it left (each within
	// rcloneTimeout) and records its failure, and a rollback stopped in its grace journals its
	// reboot's cancel: the daemon waits for them, up to backupStopWait.
	done := make(chan struct{})
	go func() {
		h.backups.Wait()
		h.rollbacks.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(backupStopWait):
		h.O.logf("a backup or a rollback did not finish within %s", backupStopWait)
	}
	return first
}

// appServed reports whether the daemon serves the app socket: the OS's release names services
// that reach it (shapes.kairos.host_api), or the release the install runs does, read from its
// kept installer. An upgrade's boot brings the installed release up before upgrading, and its
// services must reach the daemon then, whatever the OS's release names.
func (h *Hostd) appServed() bool {
	if k := h.R.Spec.Shapes.Kairos; k != nil && len(k.HostAPI) > 0 {
		return true
	}
	in, _ := h.run()
	files, err := payload.Read(in.ctl())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			h.O.logf("no app socket: the installed release's host_api cannot be read from %s: %v", in.ctl(), err)
		}
		return false
	}
	return len(payloadHostAPI(files[payload.Spec])) > 0
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
	// include credentials), issue a kit, stop the backups, or run a command in a container.
	both := func(pattern string, f http.HandlerFunc) { handle(pattern, f, true) }
	operator := func(pattern string, f http.HandlerFunc) { handle(pattern, f, false) }
	both("GET /v1/status", h.status)
	both("GET /v1/services", h.services)
	both("GET /v1/services/{service}/logs", h.logs)
	both("POST /v1/services/{service}/{action}", h.serviceAction)
	operator("POST /v1/services/{service}/exec", h.execService)
	operator("POST /v1/provision", h.provision)
	both("GET /v1/reset-db", h.resetDBPlan)
	operator("POST /v1/reset-db/plan", h.resetDBPlanWith)
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
	operator("POST /v1/kits", h.createKit)
	operator("POST /v1/kits/check", h.checkKit)
	operator("POST /v1/secrets/rotate", h.rotateSecret)
	operator("POST /v1/secrets/set", h.setSecret)
	operator("POST /v1/secrets/show", h.showSecret)
	operator("POST /v1/restore", h.restoreBackup)
	return mux
}

// AppHandler is the API as the app socket serves it: each request must carry the install's token
// (HostTokenVar), as the install holds it now (appToken), as "Authorization: Bearer <token>".
// Before the install records one, every request is refused. The operator's routes answer 403
// (routes).
func (h *Hostd) AppHandler() http.Handler {
	api := h.routes(true)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := h.appToken()
		got, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if want == "" || !bearer || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			reply(w, http.StatusUnauthorized, map[string]string{"error": "the host daemon's app socket needs the install's token: Authorization: Bearer $" + HostTokenVar})
			return
		}
		api.ServeHTTP(w, r)
	})
}

// appToken is the install's token (HostTokenVar) as its .env holds it; "" before the install
// records one. The file is read again only once it changed (another file, size or time) since the
// read the token was kept from: it is stat'ed before it is read, so a change during the read is
// seen at the next request.
func (h *Hostd) appToken() string {
	in, _ := h.run()
	dir, err := filepath.Abs(in.O.Dir)
	if err != nil {
		return ""
	}
	path := filepath.Join(dir, envName)
	fi, err := os.Stat(path)
	h.tokenMu.Lock()
	defer h.tokenMu.Unlock()
	if err != nil {
		h.tokenFile, h.token = nil, ""
		return ""
	}
	if old := h.tokenFile; old != nil && os.SameFile(old, fi) && old.Size() == fi.Size() && old.ModTime().Equal(fi.ModTime()) {
		return h.token
	}
	env, err := ReadEnvFile(path)
	if err != nil {
		h.tokenFile, h.token = nil, ""
		return ""
	}
	h.token, _ = env.Get(HostTokenVar)
	h.tokenFile = fi
	return h.token
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

// replyErr answers err, with the code its httpError gives (500 without one); a refusal (Refused,
// raised before the request changed anything) is answered as one, refused true, with its fix when
// it has one: the client exits 3 on it (hostClient.do).
func replyErr(w http.ResponseWriter, err error) {
	err = notNow(err)
	code := http.StatusInternalServerError
	var he httpError
	if errors.As(err, &he) {
		code = he.code
	}
	body := map[string]any{"error": err.Error()}
	if r, ok := Refusal(err); ok {
		body["refused"] = true
		if r.Fix != "" {
			body["fix"] = r.Fix
		}
		if r.Absent {
			body["absent"] = true
		}
	}
	reply(w, code, body)
}

// failAs is err answered with code, a refusal kept one, with its fix or without.
func failAs(code int, err error) error {
	he := httpError{code, err.Error()}
	if r, ok := Refusal(err); ok {
		return Refused{Reason: he.msg, Fix: r.Fix, Absent: r.Absent, cause: he}
	}
	return he
}

// refuseAs is a refusal answered with code, its fix fix and its message format's.
func refuseAs(code int, fix, format string, a ...any) error {
	return refusal(fix, fail(code, format, a...))
}

// decodeBody reads a JSON body strictly; an empty one leaves v as it is.
func decodeBody(r *http.Request, v any) error { return decodeBodyUpTo(nil, r, v, 64<<10) }

// decodeBodyUpTo is decodeBody for a body of up to limit bytes; given w, a body over it closes
// the connection rather than drain the rest.
func decodeBodyUpTo(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	var over *http.MaxBytesError
	if err := dec.Decode(v); errors.As(err, &over) {
		return fail(http.StatusRequestEntityTooLarge, "the request body is over the %d bytes it may be", limit)
	} else if err != nil && !errors.Is(err, io.EOF) {
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

// installed opens the install; before the first install, open()'s absent refusal (its fix the
// shape's: the daemon's options say which), answered 409.
func (h *Hostd) installed(in *Installer) (*EnvFile, error) {
	env, err := in.open()
	if isAbsent(err) {
		return nil, failAs(http.StatusConflict, err)
	}
	return env, err
}

// source opens what a release is looked up for: the install, or on the VM before its first install
// (none made, or one refused) the first install's settings — their channel, and their registry
// (a mirror, its CA trusted) or the release's — so a newer release's OS can replace the one whose
// install was refused. An empty settings file names no channel: the request names one.
func (h *Hostd) source(in *Installer) (env *EnvFile, first bool, err error) {
	if h.Docker || !notInstalled(in) {
		env, err = h.installed(in)
		return env, false, err
	}
	st, err := ReadSettings(cmp.Or(in.O.Settings, SettingsFile(h.R.Spec.Name)))
	if err != nil {
		return nil, false, err
	}
	if st == nil {
		st = &Settings{}
	}
	env = NewEnvFile()
	if st.Channel != "" {
		env.Set(ChannelVar, st.Channel)
	}
	env.Set("STOWAGE_REGISTRY", cmp.Or(st.Registry, h.R.Spec.Registry.Prefix))
	in.O.RegistryCA = cmp.Or(in.O.RegistryCA, st.RegistryCA)
	return env, true, nil
}

// installedSpec is the spec carriedRelease tells for the release name version at revision, read
// once per release and kept installer file (none, or its size and modification time), whether the
// read gave a spec or failed: a status does not read the kept installer, nor log it, at every
// request. A file that stats with an error other than its absence gives no key: it is read, and
// what is read not kept, at every request.
func (h *Hostd) installedSpec(in *Installer, name, version string, revision int) *spec.Spec {
	key := fmt.Sprintf("%s\x00%s\x00%d\x00", name, version, revision)
	// A file that does not stat (absent aside) gives no key: what is read is not kept.
	keep := true
	switch fi, err := os.Stat(in.ctl()); {
	case err == nil:
		key += fmt.Sprintf("%d\x00%d", fi.Size(), fi.ModTime().UnixNano())
	case !errors.Is(err, os.ErrNotExist):
		keep = false
	}
	h.carriedMu.Lock()
	if keep && h.carriedKey == key {
		defer h.carriedMu.Unlock()
		return h.carriedSpec
	}
	h.carriedMu.Unlock()
	// Read outside the lock: a status waits on no other's read.
	k := *in
	k.O.Log = h.O.Log // what it logs is the daemon's, once
	_, s, err := k.readCarried(name, version, revision)
	if err != nil {
		h.O.logf("%v", err)
		s = nil // kept as no spec: read and logged again once the key changes
	}
	if keep {
		h.carriedMu.Lock()
		h.carriedKey, h.carriedSpec = key, s
		h.carriedMu.Unlock()
	}
	return s
}

// notInstalled reports whether in has no install yet: open's absent refusal, the one rule of it.
func notInstalled(in *Installer) bool {
	_, err := in.open()
	return isAbsent(err)
}

func (h *Hostd) status(w http.ResponseWriter, r *http.Request) {
	in, _ := h.run()
	m := h.R.Manifest
	q := r.URL.Query()
	if q.Has("values") && fromApp(r) {
		reply(w, http.StatusForbidden, map[string]string{"error": "the host daemon's app socket does not serve the install's values: run it as the operator"})
		return
	}
	values, err := strconv.ParseBool(cmp.Or(q.Get("values"), "false"))
	if err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": "values: want true or false"})
		return
	}
	st := Status{Format: MachineFormat, Name: m.Name, OS: Version{m.ProjectVersion, m.Revision}, Job: "idle"}
	st.RegistryLogin, st.RegistryLoginError = in.heldLoginHost()
	if env, err := in.open(); err == nil {
		name, v, rev := runs(env)
		n, _ := strconv.Atoi(rev)
		st.Installed = &Version{v, n}
		// The release `rollback` goes back to, and the inputs as an installer of the release
		// installed tells them: neither fails the answer (said in the daemon's log).
		if st.Previous, err = in.keptPrevious(v, n); err != nil {
			h.O.logf("status: the previous release is not known: %v", err)
		}
		s := h.installedSpec(in, name, v, n)
		st.Inputs = installInputs(env, s)
		if values {
			st.Values = recordedValues(s, env.Get, s != nil && s.HasFront())
			in.heldValues(st.Values, st.RegistryLogin, st.RegistryLoginError)
		}
		st.Installing, st.InstallFailed, _ = in.firstInstall()
		st.RestoreAwaited = in.restoreAwaited()
		st.Channel, _ = env.Get(ChannelVar)
		// A registry CA that cannot be read is left out: the status always answers, and a poll
		// logs nothing at each request.
		st.Registry, st.FirstInstall, _ = readFirstInstall(env, in.O.Dir)
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
	boot, bootQueued, bootErr := unitStatus(r.Context(), h.Host, h.R.Spec.Name+".service")
	// Both the boot unit and the gate, which runs after it, ended: the boot is over.
	st.BootSettled = h.Docker || (bootErr == nil && unitEnded(boot, bootQueued) && h.gateDone(r.Context()))
	if err := bootErr; err == nil {
		switch {
		case !unitDone(boot):
			st.InstallFailed = ""
		case boot == "failed" && st.Installed == nil:
			st.InstallFailed = fmt.Sprintf("the boot's install failed before it wrote the install (`journalctl -u %s` tells why)", m.Name)
		}
	}
	st.Disk = h.disk()
	st.Pressure = h.pressureStatus(st.Disk)
	st.Boot = bootMode(h.O.cos())
	if st.Boot == "recovery" {
		st.ResetFailed = resetFailed(r.Context(), h.Host)
	}
	st.Fallback = fileExists(filepath.Join(h.O.cos(), upgradeFailure))
	if st.Boot == "active" {
		if vars, err := h.assessment().read(); err == nil {
			st.OnTrial = trial(vars)
		}
	}
	st.BootID = h.bootID()
	// One read of the journal: its cursor, its last verdict and this boot's.
	last, lastJudged, jerr := h.journal().lastOf()
	var judged *Event
	if lastJudged != nil && sameBoot(lastJudged.BootID, st.BootID) {
		judged = lastJudged
	}
	// The last failed boot is told until a later boot on trial (an upgrade's) is judged healthy,
	// after it in the journal: the fallback's boot of the passive system, healthy as it may be,
	// keeps it told.
	if fb := lastFailedBoot(in.O.Dir); fb != nil && !(lastJudged != nil && lastJudged.Verdict == verdictHealthy &&
		lastJudged.OnTrial != nil && *lastJudged.OnTrial && lastJudged.Seq > fb.Seq) {
		st.FailedBoot = fb
	}
	if jerr != nil {
		st.EventsError = fmt.Sprintf("the event journal cannot be read: %v", jerr)
	} else {
		st.Events = &last
	}
	switch {
	case h.Docker:
	case st.BootID == "":
		st.BootJudgedError = "this boot's id cannot be read: its verdict is not told"
	case jerr != nil:
		st.BootJudgedError = st.EventsError
	default:
		st.BootJudged = judgedOf(judged)
	}
	st.NetworkWarning = h.route6Warning()
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

// kairosReset is Kairos's automatic reset unit.
const kairosReset = "kairos-reset.service"

// resetLines bounds the journal lines resetFailed tells.
const resetLines = 15

// resetFailed says, when kairosReset failed in this boot, so with its last journal lines; empty
// when it did not fail, or its state cannot be read (the next status asks again).
func resetFailed(ctx context.Context, host Tool) string {
	if st, _, err := unitStatus(ctx, host, kairosReset); err != nil || st != "failed" {
		return ""
	}
	out, err := host.Run(ctx, Cmd{Args: []string{"journalctl", "--unit", kairosReset, "--boot", "--no-pager", "--output", "cat", "--lines", strconv.Itoa(resetLines)}})
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return kairosReset + " failed (`journalctl -u " + kairosReset + " -b` on the VM tells why)"
	}
	return kairosReset + " failed: " + strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", " | ")), " ")
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

// declared gives the services the stack of in's install (env) declares, read once per change
// (stateGen) and per state of its .env (its size and modification time, which a re-run changing
// the profiles moves): the service watch's looks and GET services fork no `compose config` each.
func (h *Hostd) declared(in *Installer, env *EnvFile) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		key := ""
		if fi, err := os.Stat(in.path(envName)); err == nil {
			key = fmt.Sprintf("%d %d %d", h.stateGen.Load(), fi.Size(), fi.ModTime().UnixNano())
		}
		h.declaredMu.Lock()
		defer h.declaredMu.Unlock()
		if key != "" && key == h.declaredKey {
			return h.declaredNames, nil
		}
		names, err := in.declaredServices(ctx, env)
		if err == nil && key != "" {
			h.declaredKey, h.declaredNames = key, names
		}
		return names, err
	}
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
	list, err := in.stackServices(r.Context(), env, h.declared(in, env))
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
		ID, Service, State, Health, Image, Labels string
		ExitCode                                  int
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
		// A `compose run` container is no service's: one left exited is not its service down.
		if slices.Contains(strings.Split(r.Labels, ","), "com.docker.compose.oneoff=True") {
			continue
		}
		list = append(list, Service{Service: r.Service, State: r.State, Health: r.Health, Image: r.Image, ExitCode: r.ExitCode, ID: r.ID})
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

// maxExecOutput bounds each output of an exec the daemon answers.
const maxExecOutput = 4 << 20

// ExecRequest is POST /v1/services/{service}/exec's body, and ExecResult its answer: the outputs
// (cut, StdoutCut and StderrCut then set, past maxExecOutput) and the command's exit status.
type ExecRequest struct {
	Args  []string `json:"args"`
	Stdin []byte   `json:"stdin,omitempty"` // the command's standard input (base64), at most maxExecInput bytes
}

// maxExecInput bounds an exec's standard input, as maxExecOutput bounds each output.
const maxExecInput = 4 << 20

type ExecResult struct {
	Stdout    []byte `json:"stdout"`
	Stderr    []byte `json:"stderr"`
	StdoutCut bool   `json:"stdout_cut,omitempty"`
	StderrCut bool   `json:"stderr_cut,omitempty"`
	Exit      int    `json:"exit"`
}

func (h *Hostd) execService(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	// The input as base64, and the rest of the request.
	if err := decodeBodyUpTo(w, r, &req, int64(base64.StdEncoding.EncodedLen(maxExecInput))+64<<10); err != nil {
		replyErr(w, err)
		return
	}
	if len(req.Args) == 0 {
		replyErr(w, fail(http.StatusBadRequest, "args: the command to run, as [\"psql\", \"-c\", \"…\"]"))
		return
	}
	if len(req.Stdin) > maxExecInput {
		replyErr(w, fail(http.StatusBadRequest, "stdin: %d bytes, over the %d an exec takes", len(req.Stdin), maxExecInput))
		return
	}
	in, _ := h.run()
	env, err := h.installed(in)
	if err != nil {
		replyErr(w, err)
		return
	}
	svc := r.PathValue("service")
	if err := h.knownService(r.Context(), in, env, svc); err != nil {
		replyErr(w, err)
		return
	}
	stdout, stderr := &capped{limit: maxExecOutput}, &capped{limit: maxExecOutput}
	in.O.Out, in.O.Log = stdout, stderr
	var stdin io.Reader
	if req.Stdin != nil {
		stdin = bytes.NewReader(req.Stdin)
	}
	err = in.exec(r.Context(), env, svc, req.Args, stdin, false)
	var code ExitCode
	switch {
	case errors.As(err, &code):
	case err != nil:
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, ExecResult{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes(), StdoutCut: stdout.over, StderrCut: stderr.over, Exit: int(code)})
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
	unlock, err := in.lockNow()
	if errors.Is(err, errLocked) {
		replyErr(w, notNowErr{fail(http.StatusConflict, "an install, upgrade, rollback, provision or reset-db is running: wait for it to finish"), "wait for it to finish, then run again"})
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
		replyErr(w, failAs(http.StatusConflict, err))
		return
	}
	svc := r.PathValue("service")
	if err := h.knownService(r.Context(), in, env, svc); err != nil {
		replyErr(w, err)
		return
	}
	// Done or not, the service may have changed.
	_, err = in.Compose(r.Context(), env, nil, append(args, svc)...)
	h.changed()
	if err != nil {
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
	// NewBaseline crosses a schema baseline: the release Channel and Version find (as an upgrade
	// finds it) is installed instead, its OS written and booted, the data wiped by that boot.
	// Confirm is then that release.
	NewBaseline bool   `json:"new_baseline,omitempty"`
	Channel     string `json:"channel,omitempty"`
	Version     string `json:"version,omitempty"`
	// Unordered, with Version, takes a version a `command` scheme would order, which the daemon
	// cannot run (unorderedTo): the operator checked the order.
	Unordered bool `json:"unordered,omitempty"`
	// Inputs, with NewBaseline, are values of inputs the release adds, KEY: value (a file input's
	// as --set-file encodes it), staged root-only for the boot that crosses, which takes them as
	// given and removes them: those the plan was asked with (POST /v1/reset-db/plan), or refused.
	Inputs map[string]string `json:"inputs,omitempty"`
}

// ResetPlanRequest is POST /v1/reset-db/plan's body: GET /v1/reset-db's query, with the inputs a
// crossing gives the release it installs (reset-db --new-baseline --set), which no URL carries.
type ResetPlanRequest struct {
	NewBaseline bool              `json:"new_baseline,omitempty"`
	Channel     string            `json:"channel,omitempty"`
	Version     string            `json:"version,omitempty"`
	Unordered   bool              `json:"unordered,omitempty"`
	Inputs      map[string]string `json:"inputs,omitempty"`
}

// inputsDigest is a digest of the inputs a reset-db plan was asked with, which its POST must give
// again: none and empty are one.
func inputsDigest(inputs map[string]string) [32]byte {
	if len(inputs) == 0 {
		return [32]byte{}
	}
	raw, _ := json.Marshal(inputs) // a map of strings, its keys sorted
	return sha256.Sum256(raw)
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
	if err := h.osRelease(env, "reset-db"); err != nil {
		return ResetPlan{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.rebooting:
		return ResetPlan{}, notNowErr{fail(http.StatusConflict, "a reboot is scheduled"), "wait for the reboot, then run again"}
	case h.job != nil && h.job.State == "running":
		return ResetPlan{}, notNowErr{fail(http.StatusConflict, "a job (%s) is running", h.job.Kind), "wait for the job to end, then run again"}
	}
	return ResetPlan{Target: target, Plan: in.resetPlan()}, nil
}

func (h *Hostd) resetDBPlan(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h.showResetPlan(w, r, ResetPlanRequest{NewBaseline: q.Get("new_baseline") == "true", Channel: q.Get("channel"), Version: q.Get("version"), Unordered: q.Get("unordered") == "true"})
}

// resetDBPlanWith is POST /v1/reset-db/plan: GET /v1/reset-db's plan, asked with the inputs the
// crossing gives — the operator's alone, as they may be secrets.
func (h *Hostd) resetDBPlanWith(w http.ResponseWriter, r *http.Request) {
	var req ResetPlanRequest
	// The inputs may hold a file input's content.
	if err := decodeBodyUpTo(w, r, &req, maxSecretsFile+64<<10); err != nil {
		replyErr(w, err)
		return
	}
	h.showResetPlan(w, r, req)
}

// showResetPlan answers the reset-db plan req asks for, with the token its POST uses once.
func (h *Hostd) showResetPlan(w http.ResponseWriter, r *http.Request, req ResetPlanRequest) {
	in, _ := h.run()
	nb := req.NewBaseline
	// The app may look at a plan, not one the operator alone vouches for (POST reset-db is the
	// operator's); unordered without a version is resetPlan's refusal, whoever asks.
	if req.Unordered && req.Version != "" && fromApp(r) {
		replyErr(w, unorderedOperators(in, verbNewBaseline, req.Channel, req.Version))
		return
	}
	// The app looks, and never confirms (POST reset-db is the operator's): its plan carries no token
	// and leaves the operator's as it was.
	p, _, _, err := h.resetPlan(r.Context(), in, req, fromApp(r), "", !fromApp(r))
	if err != nil {
		replyErr(w, err)
		return
	}
	if fromApp(r) {
		reply(w, http.StatusOK, p)
		return
	}
	// The token, the plan it shows and that plan's check, set together: a later GET replaces all.
	p.Token = rand.Text()
	h.mu.Lock()
	h.resetToken, h.resetShown = p.Token, shownReset{nb, p.Target, inputsDigest(req.Inputs)}
	if c := h.checkedPlan; nb && c != nil {
		c.token = p.Token
		h.crossPlan, h.checkedPlan = c, nil
	} else {
		h.crossPlan = nil
	}
	h.mu.Unlock()
	reply(w, http.StatusOK, p)
}

// resetPlan is resetCheck's plan, or with newBaseline the crossing's: the release found on
// channel (version's latest revision, or the channel's latest) as an upgrade finds it, m, which
// must come after this OS's and start another schema baseline, both told, and take the install's
// values as its boot takes them (upgradeCheck); prefix is the registry it comes from. A version
// named under a `command` scheme is taken only with unordered, as an upgrade's (unorderedTo);
// asked by the app (app), its refusal names the operator's command over SSH (unorderedCmd). The
// inputs the request gives are judged with the install's values, given to m's boot, which must
// take what a reset-db stages.
func (h *Hostd) resetPlan(ctx context.Context, in *Installer, req ResetPlanRequest, app bool, token string, shown bool) (p ResetPlan, m *release.Manifest, prefix string, err error) {
	newBaseline, channel, version, unordered := req.NewBaseline, req.Channel, req.Version, req.Unordered
	switch {
	case !newBaseline && (channel != "" || version != ""):
		return ResetPlan{}, nil, "", fail(http.StatusBadRequest, "a channel or a version names the release new_baseline installs: reset-db resets the release the OS carries")
	case !newBaseline && len(req.Inputs) > 0:
		return ResetPlan{}, nil, "", fail(http.StatusBadRequest, "inputs are what the release new_baseline installs adds: reset-db resets the release the OS carries with the values the install records")
	}
	if unordered && version == "" {
		return ResetPlan{}, nil, "", failAs(http.StatusBadRequest, unorderedAlone(verbNewBaseline))
	}
	if p, err = h.resetCheck(ctx, in); err != nil || !newBaseline {
		return p, nil, "", err
	}
	env, err := h.installed(in)
	if err != nil {
		return ResetPlan{}, nil, "", err
	}
	if m, _, err = h.target(ctx, in, env, channel, version); err != nil {
		return ResetPlan{}, nil, "", err
	}
	// after() calls any other version of a `command` scheme later, which it may not be.
	in.O.Unordered = unordered
	if version != "" {
		if err := in.unorderedTo(m, h.runsVersion(env), verbNewBaseline, appRetry(in, app, verbNewBaseline, channel, version)); err != nil {
			return ResetPlan{}, nil, "", failAs(http.StatusConflict, err)
		}
	}
	newer, err := h.after(m)
	switch {
	case err != nil:
		return ResetPlan{}, nil, "", err
	case m.Yanked != nil:
		return ResetPlan{}, nil, "", fail(http.StatusConflict, "%s %s is yanked: %s", m.Name, m.Tag, m.Yanked.Reason)
	case in.leftByRollback(m.ProjectVersion, m.Revision):
		return ResetPlan{}, nil, "", fail(http.StatusConflict, "the install went back from %s %s (revision %d): remove %s to cross to it again", m.Name, m.ProjectVersion, m.Revision, in.path(FailedDir))
	case !newer:
		return ResetPlan{}, nil, "", fail(http.StatusConflict, "%s %s (revision %d) does not come after %s (revision %d), which this OS runs: --new-baseline installs a later release", m.Name, m.ProjectVersion, m.Revision, h.R.Manifest.ProjectVersion, h.R.Manifest.Revision)
	case m.Artifacts.OS == "":
		return ResetPlan{}, nil, "", fail(http.StatusConflict, "%s %s was published without an OS bundle", m.Name, m.Tag)
	}
	// A boot that does not read what a reset-db stages would leave it there, unused.
	if len(req.Inputs) > 0 && !stagedInputsBy(m.StowageVersion, h.Stowage, crossedInputsSince) {
		return ResetPlan{}, nil, "", refuseAs(http.StatusConflict, "cross without the inputs, with a release that requires none it adds: only a release published with Stowage "+crossedInputsSince+" or later is given them",
			"%s was published by Stowage %s, whose crossing's boot takes no input a reset-db gives: nothing is staged", m.Tag, cmp.Or(m.StowageVersion, "of no recorded version"))
	}
	prefix, _ = env.Get("STOWAGE_REGISTRY")
	key := crossPlanned{installer: m.Artifacts.Installer, install: sha256.Sum256([]byte(strings.Join(env.Environ(nil), "\x00")))}
	h.mu.Lock()
	c := h.crossPlan
	h.mu.Unlock()
	if c != nil && !shown && token != "" && c.token == token && c.installer == key.installer && c.install == key.install {
		return c.plan, m, prefix, nil
	}
	k, err := h.targetRelease(ctx, in, env, m)
	if err != nil {
		return ResetPlan{}, nil, "", err
	}
	from, to, ok := baselines(&in.O, in, k)
	switch {
	case !ok:
		return ResetPlan{}, nil, "", fail(http.StatusConflict, "the schema baseline of %s %s or of %s %s cannot be told (the daemon's log says why): --new-baseline never wipes on a guess", m.Name, h.R.Manifest.ProjectVersion, m.Name, m.ProjectVersion)
	case from == to:
		return ResetPlan{}, nil, "", fail(http.StatusConflict, "%s %s starts the schema baseline the install runs: --new-baseline only crosses a baseline; `%s upgrade` moves it", m.Name, m.ProjectVersion, in.vmCtl())
	}
	// What the boot's crossing wipes is told now, before the OS is written.
	t, err := in.wipeOf(k)
	var wipe, keep []string
	if err == nil {
		wipe, keep, _, _, err = t.wipeSet(in)
	}
	if err != nil {
		return ResetPlan{}, nil, "", fail(http.StatusConflict, "%v", err)
	}
	if len(wipe) == 0 {
		return ResetPlan{}, nil, "", fail(http.StatusConflict, "neither %s %s nor %s %s names a volume in data.reset: --new-baseline has nothing to wipe", m.Name, h.R.Manifest.ProjectVersion, m.Name, m.ProjectVersion)
	}
	kept := ""
	if len(keep) > 0 {
		kept = " (keeping " + strings.Join(keep, ", ") + ")"
	}
	gives := &vmGiving{run: verbNewBaseline, cmd: verbNewBaseline + upgradeArgs(channel, m.ProjectVersion, unordered || needsUnordered(m, h.runsVersion(env))), since: crossedInputsSince}
	unchecked, err := h.valuesCheck(ctx, in, env, m, k, "it", gives, req.Inputs)
	if err != nil {
		return ResetPlan{}, nil, "", err
	}
	if unchecked != "" {
		unchecked = " (" + unchecked + ")"
	}
	key.plan = ResetPlan{Target: Version{m.ProjectVersion, m.Revision},
		Plan: fmt.Sprintf("write %s's OS and reboot into it; that boot stops %s, deletes its volumes %s%s, forgets what its provision hook returned, then installs %s, at schema baseline %d (from %d), with the same values%s",
			m.Tag, m.Name, strings.Join(wipe, ", "), kept, m.ProjectVersion, to, from, unchecked)}
	if shown {
		h.mu.Lock()
		h.checkedPlan = &key
		h.mu.Unlock()
	}
	return key.plan, m, prefix, nil
}

// shownReset is the plan a reset-db GET showed: a crossing or not, its target, and the inputs it was
// asked with (inputsDigest).
type shownReset struct {
	newBaseline bool
	target      Version
	inputs      [32]byte
}

// crossPlanned is a crossing resetPlan checked: the target's installer (its digest), the install
// it was checked against (its .env's digest), the plan, and the token of the GET that showed it,
// whose POST alone takes it as it is — with the inputs that GET was asked with (shownReset).
type crossPlanned struct {
	installer string
	install   [32]byte
	plan      ResetPlan
	token     string
}

// resetDB runs reset-db as a job: the install back to its first setup, without a reboot.
func (h *Hostd) resetDB(w http.ResponseWriter, r *http.Request) {
	var req ResetRequest
	// The inputs may hold a file input's content.
	if err := decodeBodyUpTo(w, r, &req, maxSecretsFile+64<<10); err != nil {
		replyErr(w, err)
		return
	}
	if req.Confirm == nil {
		replyErr(w, fail(http.StatusBadRequest, "reset-db wipes the install's data: confirm the release it resets (\"confirm\": {\"version\", \"revision\"}, GET reset-db's target)"))
		return
	}
	if req.NewBaseline && req.NoProvision {
		replyErr(w, fail(http.StatusBadRequest, "no_provision is not given to the boot that crosses the baseline: provision runs the hook afterwards"))
		return
	}
	// A crossing's token for another plan is refused before the crossing is checked again, which
	// fetches the target's installer (the job's start checks it once more, and uses it up).
	if req.NewBaseline {
		h.mu.Lock()
		known := req.Token != "" && subtle.ConstantTimeCompare([]byte(req.Token), []byte(h.resetToken)) == 1
		other := h.resetShown.newBaseline != req.NewBaseline || h.resetShown.inputs != inputsDigest(req.Inputs)
		h.mu.Unlock()
		switch {
		case !known:
			replyErr(w, fail(http.StatusConflict, "reset-db needs the token of the GET that showed its plan, once: look again"))
			return
		case other:
			replyErr(w, fail(http.StatusConflict, "reset-db's token is the GET's that showed another plan: look again"))
			return
		}
	}
	in, _ := h.run()
	p, m, prefix, err := h.resetPlan(r.Context(), in, ResetPlanRequest{NewBaseline: req.NewBaseline, Channel: req.Channel, Version: req.Version, Unordered: req.Unordered, Inputs: req.Inputs}, false, req.Token, false)
	if err != nil {
		replyErr(w, err)
		return
	}
	if *req.Confirm != p.Target {
		replyErr(w, fail(http.StatusConflict, "reset-db would reset to %s %s, not %s, which was confirmed: look again", h.R.Manifest.Name, p.Target, *req.Confirm))
		return
	}
	// The token a GET made, used once: a POST sent again, or one no GET preceded, is refused.
	j, err := h.startJob("reset-db", p.Target.String(), func() error {
		if req.Token == "" || subtle.ConstantTimeCompare([]byte(req.Token), []byte(h.resetToken)) != 1 {
			return fail(http.StatusConflict, "reset-db needs the token of the GET that showed its plan, once: look again")
		}
		shown := h.resetShown
		h.resetToken, h.resetShown = "", shownReset{}
		if shown != (shownReset{req.NewBaseline, p.Target, inputsDigest(req.Inputs)}) {
			return fail(http.StatusConflict, "reset-db's token is the GET's that showed another plan: look again")
		}
		if m == nil {
			return nil
		}
		if err := h.osWritten(); err != nil {
			return sameKind(err, fail(http.StatusConflict, "%v: reboot first, then reset-db", err))
		}
		if err := in.volumesSplit(); err != nil {
			return failAs(http.StatusConflict, err)
		}
		return nil
	})
	if err != nil {
		replyErr(w, err)
		return
	}
	if m != nil {
		src := fmt.Sprintf("%s/%s-os@%s", prefix, m.Name, m.Artifacts.OS)
		go h.osJob(j, in, m.Tag, prefix, src, true, "wipes the data and installs it", func() error {
			// What an upgrade staged was for the OS this one replaces in the passive slot: replaced by
			// what this crossing gives its boot, or removed.
			if err := stageInputs(in.O.Dir, m, req.Channel, req.Inputs); err != nil {
				return err
			}
			return writeAtomic(newBaselineFile(in.O.Dir), strings.NewReader(releaseMark(m.ProjectVersion, m.Revision)+"\n"), 0o600, nil)
		})
		accepted(w, j)
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
	m, err := channelTarget(ctx, c, prefix, h.R.Spec.Name, channel, v, trust, in.O.logf)
	if err != nil {
		return nil, "", failAs(http.StatusBadGateway, err)
	}
	return m, channel, nil
}

// after reports whether m comes after the release the OS carries, by the scheme m records; a
// `command` scheme, which the daemon does not run, orders by revision within one version only.
func (h *Hostd) after(m *release.Manifest) (bool, error) {
	return laterRelease(m, h.R.Manifest.ProjectVersion, h.R.Manifest.Revision)
}

// runsVersion is the version of the release the install runs, which a --version is ordered from
// (unorderedTo); before the first install (env the settings'), the OS's, which that install makes.
func (h *Hostd) runsVersion(env *EnvFile) string {
	if _, v, _ := runs(env); v != "" {
		return v
	}
	return h.R.Manifest.ProjectVersion
}

// unorderedOperators is the app socket's refusal of unordered on verb (`upgrade`, `reset-db
// --new-baseline`) to version on channel: the operator alone vouches for an order the daemon
// cannot check. Its fix is the operator's command over SSH (unorderedCmd).
func unorderedOperators(in *Installer, verb, channel, version string) error {
	return refuseAs(http.StatusForbidden, "run `"+unorderedCmd(in, verb, channel, version)+"` over SSH",
		"unordered is the operator's: the operator alone vouches for an order this daemon cannot check")
}

// unorderedCmd is verb to version (on channel, when given) with --unordered, as the operator types
// it on the VM over SSH (vmCtl).
func unorderedCmd(in *Installer, verb, channel, version string) string {
	c := in.vmCtl() + " " + verb
	if channel != "" {
		c += " --channel " + channel
	}
	return c + " --version " + version + " --unordered"
}

// appRetry is unorderedTo's retry for a request: from the app socket (app), which may not send
// unordered (unorderedOperators), the operator's command over SSH; else none, the same command
// with --unordered added.
func appRetry(in *Installer, app bool, verb, channel, version string) string {
	if !app {
		return ""
	}
	return "run `" + unorderedCmd(in, verb, channel, version) + "` over SSH"
}

func (h *Hostd) updates(w http.ResponseWriter, r *http.Request) {
	in, _ := h.run()
	env, first, err := h.source(in)
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
	rewrite := h.rewrites(m, env)
	var installed *Version
	if _, v, rev := runs(env); v != "" {
		r, _ := strconv.Atoi(rev)
		installed = &Version{v, r}
	}
	reply(w, http.StatusOK, Update{
		Channel: channel, Current: Version{cur.ProjectVersion, cur.Revision}, Target: Version{m.ProjectVersion, m.Revision},
		Available: (newer || rewrite) && m.Yanked == nil && m.Artifacts.OS != "" && !left, LeftByRollback: left, Rewrite: rewrite, Installed: installed, Passive: !h.Docker && bootMode(h.O.cos()) == "passive", Changelog: m.Changelog.Project,
		NeedsUnordered:  q.Get("version") != "" && needsUnordered(m, h.runsVersion(env)),
		ChecksInstaller: checksTarget(first, !newer && rewrite, env, m),
		TakesInputs:     checksTarget(first, !newer && rewrite, env, m) && h.takesStaged(m),
	})
}

// rewrites reports whether an upgrade to m writes the running release's OS to the active slot
// again: the VM runs the passive system, which a boot that failed went back to, and m is that
// system's release. The active slot then holds the release that failed, which no later upgrade
// replaces until one is published.
func (h *Hostd) rewrites(m *release.Manifest, env *EnvFile) bool {
	_, v, rev := runs(env)
	this := h.R.Manifest
	return !h.Docker && bootMode(h.O.cos()) == "passive" && isAt(m.ProjectVersion, m.Revision, this.ProjectVersion, strconv.Itoa(this.Revision)) &&
		isAt(this.ProjectVersion, this.Revision, v, rev)
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
	if !unitDone(st) {
		return notNowErr{fail(http.StatusConflict, "the boot is still bringing the install up (%s.service is %s): wait for it", h.R.Spec.Name, st), "wait for the boot to bring the install up, then run again"}
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
	if !unitDone(st) {
		return notNowErr{fail(http.StatusConflict, "the health gate has not judged this boot yet (%s is %s): wait for it, or `systemctl start %s` if it does not run", unit, st, unit), "wait for the health gate's judgement of this boot, then run again"}
	}
	return nil
}

// unitDone reports whether a unit's state (unitState's) is an end: active or failed (a oneshot
// unit with RemainAfterExit), or no such unit.
func unitDone(st string) bool { return st == "active" || st == "failed" || st == noBootUnit }

// unitEnded reports whether a unit of state st, a job queued for it or not, is done with this
// boot: done (unitDone), or inactive with no job queued — skipped by its condition, stopped,
// or never started as a dependency of it failed.
func unitEnded(st string, queued bool) bool { return unitDone(st) || (st == "inactive" && !queued) }

// gateDone reports whether the health gate's unit ran to an end in this boot.
func (h *Hostd) gateDone(ctx context.Context) bool {
	st, queued, err := unitStatus(ctx, h.Host, h.R.Spec.Name+"-health.service")
	return err == nil && unitEnded(st, queued)
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

// slotRefusal is why the job slot cannot be claimed now, guard (when set) run last; nil when it
// can. The caller holds mu.
func (h *Hostd) slotRefusal(guard func() error) error {
	switch {
	case h.stopping:
		return notNowErr{fail(http.StatusServiceUnavailable, "the host daemon is stopping"), "wait for the host daemon to start again, then run again"}
	case h.rebooting:
		return notNowErr{fail(http.StatusConflict, "a reboot is scheduled"), "wait for the reboot, then run again"}
	case h.job != nil && h.job.State == "running":
		return notNowErr{jobRunning{fail(http.StatusConflict, "a job (%s) is running", h.job.Kind)}, "wait for the job to end, then run again"}
	}
	if guard != nil {
		return guard()
	}
	return nil
}

// notNowErr is a check's answer that the run must wait — a job running, a reboot scheduled,
// the daemon stopping, the boot not over or not judged yet, a system written that no boot ran
// yet — never an error reading what it checks. Every answer of the daemon makes it a refusal
// (replyErr, through notNow): nothing changed.
type notNowErr struct {
	error
	fix string // what to wait for
}

func (e notNowErr) Unwrap() error { return e.error }

// notNow is err as the daemon answers it: a notNowErr is a refusal with its fix, nothing
// changed; any other error, a refusal included, as it is.
func notNow(err error) error {
	var n notNowErr
	if _, ok := Refusal(err); ok || !errors.As(err, &n) {
		return err
	}
	return refusal(n.fix, err)
}

// sameKind is err restated as as: a notNowErr stays one, with its fix.
func sameKind(err, as error) error {
	var n notNowErr
	if errors.As(err, &n) {
		return notNowErr{as, n.fix}
	}
	return as
}

// slotFree refuses as claimJob would, guard included, and claims nothing: a dry run's answer.
func (h *Hostd) slotFree(guard func() error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.slotRefusal(guard)
}

func (h *Hostd) claimJob(kind, target string, guard func() error) (*job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.slotRefusal(guard); err != nil {
		return nil, err
	}
	h.job = &job{Started: time.Now().UTC(), Kind: kind, State: "running", Target: target}
	h.jobEnd = make(chan struct{})
	h.slotGen++
	h.changed()
	return h.job, nil
}

// ended sets j's end under mu, and wakes the runs waiting for the slot. The caller holds mu.
func (h *Hostd) ended(j *job, state, msg string) {
	j.State, j.Error = state, msg
	h.slotGen++
	h.changed()
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
		if h.onWait != nil {
			h.onWait("slot")
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
	// The inputs may hold a file input's content.
	if err := decodeBodyUpTo(w, r, &req, maxSecretsFile+64<<10); err != nil {
		replyErr(w, err)
		return
	}
	// The inputs may be secrets, recorded in the install: the operator's alone to give.
	if len(req.Inputs) > 0 && fromApp(r) {
		replyErr(w, fail(http.StatusForbidden, "inputs are the operator's: upgrade --set, --set-file or --answers over SSH"))
		return
	}
	// The backup before an upgrade is what a failed migration is restored from: the operator's
	// alone to skip.
	if req.SkipBackup && fromApp(r) {
		replyErr(w, fail(http.StatusForbidden, "skip_backup is the operator's: upgrade --skip-backup over SSH"))
		return
	}
	// unordered is the operator vouching that the version named comes after the installed one.
	if req.Unordered && req.Version == "" {
		replyErr(w, failAs(http.StatusBadRequest, unorderedAlone("upgrade")))
		return
	}
	in, _ := h.run()
	if req.Unordered && fromApp(r) {
		replyErr(w, unorderedOperators(in, "upgrade", req.Channel, req.Version))
		return
	}
	env, first, err := h.source(in)
	if err != nil {
		replyErr(w, err)
		return
	}
	// A restore again finishes half restored volumes, onto this release only.
	if err := in.volumesSplit(); err != nil {
		replyErr(w, failAs(http.StatusConflict, err))
		return
	}
	// The checks that say "not now" are refusals: nothing changed, the run may be made again.
	if err := h.judged(r.Context()); err != nil {
		replyErr(w, err)
		return
	}
	// An operator's first install running (it holds the lock before it writes .env): the reboot
	// would cut it.
	if first && lockHeld(in.path(lifecycleLock)) {
		replyErr(w, notNowErr{fail(http.StatusConflict, "an install is running: wait for it to finish"), "wait for the install to finish, then run again"})
		return
	}
	// The first install follows its settings' channel: a release from another would be followed
	// on a channel it is not on.
	if c, _ := env.Get(ChannelVar); first && c != "" && req.Channel != "" && req.Channel != c {
		replyErr(w, refuseAs(http.StatusConflict, "upgrade on the channel the first install's settings name", "the first install follows %s (its settings): an upgrade before it takes that channel's release, not %s's", c, req.Channel))
		return
	}
	m, _, err := h.target(r.Context(), in, env, req.Channel, req.Version)
	if err != nil {
		replyErr(w, err)
		return
	}
	// A version named under a `command` scheme is taken as Docker's `upgrade --version` takes it:
	// after() calls any other version later, which it may not be.
	in.O.Unordered = req.Unordered
	if req.Version != "" {
		if err := in.unorderedTo(m, h.runsVersion(env), "upgrade", appRetry(in, fromApp(r), "upgrade", req.Channel, req.Version)); err != nil {
			replyErr(w, failAs(http.StatusConflict, err))
			return
		}
	}
	newer, err := h.after(m)
	rewrite := !newer && h.rewrites(m, env)
	switch {
	case err != nil:
		replyErr(w, err)
		return
	case req.Confirm != nil && *req.Confirm != (Version{m.ProjectVersion, m.Revision}):
		replyErr(w, refuseAs(http.StatusConflict, "look again with updates, then confirm the release found", "the release found is %s %s (revision %d), not %s, which was confirmed: look again with updates", m.Name, m.ProjectVersion, m.Revision, *req.Confirm))
		return
	case in.leftByRollback(m.ProjectVersion, m.Revision):
		replyErr(w, refuseAs(http.StatusConflict, "upgrade to a later release; or, to try this one again, remove the record of the release the install went back from", "the install went back from %s %s (revision %d): the boot would not upgrade to it again (remove %s to try it again)", m.Name, m.ProjectVersion, m.Revision, in.path(FailedDir)))
		return
	case m.Yanked != nil:
		replyErr(w, refuseAs(http.StatusConflict, "choose another release: this one is yanked", "%s %s is yanked: %s", m.Name, m.Tag, m.Yanked.Reason))
		return
	case !newer && !rewrite:
		replyErr(w, refuseAs(http.StatusConflict, "choose a release after the one this OS runs", "%s %s (revision %d) does not come after %s (revision %d), which this OS runs", m.Name, m.ProjectVersion, m.Revision, h.R.Manifest.ProjectVersion, h.R.Manifest.Revision))
		return
	case m.Artifacts.OS == "":
		replyErr(w, refuseAs(http.StatusConflict, "choose a release published with an OS bundle (its kairos shape)", "%s %s was published without an OS bundle", m.Name, m.Tag))
		return
	}
	// What the boot into it would refuse is refused now, before its OS is written. The release
	// the install runs, rewritten, has nothing to refuse.
	unchecked := ""
	if !checksTarget(first, rewrite, env, m) && len(req.Inputs) > 0 {
		replyErr(w, refuseAs(http.StatusConflict, "leave out the inputs: "+inputsNotTaken(first), "%s %s (revision %d) adds no input to the install: %s", m.Name, m.ProjectVersion, m.Revision, inputsNotTaken(first)))
		return
	}
	// A boot that does not read what an upgrade stages would leave it there, unused.
	if len(req.Inputs) > 0 && !h.takesStaged(m) {
		replyErr(w, refuseAs(http.StatusConflict, "upgrade without the inputs, then give them by a re-run of `install` once it runs: an input it requires with no default fails that boot, which only a release published with Stowage "+stagedInputsSince+" or later is given",
			"%s was published by Stowage %s, whose boot takes no input an upgrade gives: nothing is staged", m.Tag, cmp.Or(m.StowageVersion, "of no recorded version")))
		return
	}
	if checksTarget(first, rewrite, env, m) {
		if unchecked, err = h.checkUpgrade(r.Context(), in, env, m, req.Channel, upgradeArgs(req.Channel, m.ProjectVersion, req.Unordered || needsUnordered(m, h.runsVersion(env))), req.Inputs); err != nil {
			replyErr(w, err)
			return
		}
	}
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	src := fmt.Sprintf("%s/%s-os@%s", prefix, m.Name, m.Artifacts.OS)
	// Checked as the job starts: another upgrade could replace the running system's copy in the
	// passive slot.
	guard := func() error {
		if err := h.osWritten(); err != nil {
			return sameKind(err, fail(http.StatusConflict, "%v: reboot first, then upgrade", err))
		}
		// Again as the job claims the slot: a restore that just ended may have marked them.
		if err := in.volumesSplit(); err != nil {
			return failAs(http.StatusConflict, err)
		}
		return nil
	}
	does := "upgrades the install"
	switch {
	case first:
		does = "makes the first install"
	case rewrite:
		does = "runs it from the active slot again"
	}
	if req.DryRun {
		if err := h.slotFree(guard); err != nil {
			replyErr(w, err)
			return
		}
		slot := "passive"
		if rewrite {
			slot = "active"
		}
		backup := !req.SkipBackup && !first && !rewrite && h.backupFirst()
		reply(w, http.StatusOK, UpgradePlan{To: Version{m.ProjectVersion, m.Revision}, Slot: slot, Does: does, Backup: backup, Unchecked: unchecked})
		return
	}
	j, err := h.startJob("upgrade", m.Tag, guard)
	if err != nil {
		replyErr(w, err)
		return
	}
	reboot := req.Reboot == nil || *req.Reboot
	if unchecked != "" {
		h.logf(j, "%s", unchecked)
	}
	go func() {
		if !req.SkipBackup && !first && !rewrite {
			h.preflightBackup(in, func(format string, a ...any) { h.logf(j, format, a...) })
		}
		h.osJob(j, in, m.Tag, prefix, src, reboot, does, func() error {
			// An upgrade crosses no baseline: a crossing asked before, whose OS never booted, is
			// not this release's to make.
			if err := os.Remove(newBaselineFile(in.O.Dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := stageInputs(in.O.Dir, m, req.Channel, req.Inputs); err != nil {
				return err
			}
			if !first {
				return nil
			}
			// The boot into it makes the first install of that release, whatever release the
			// settings name.
			return writeAtomic(osUpgradeFile(in.O.Dir), strings.NewReader(releaseMark(m.ProjectVersion, m.Revision)+"\n"), 0o600, nil)
		})
	}()
	accepted(w, j)
}

// osJob ends job j: it writes the OS of release tag from src into the passive slot, mark run
// first, and reboots into it (unless reboot is false: the next boot runs it), whose boot does
// what does says. The written mark is set before kairos-agent runs and kept when it fails: it may
// have moved the running system to the passive slot already.
func (h *Hostd) osJob(j *job, in *Installer, tag, prefix, src string, reboot bool, does string, mark func() error) {
	err := os.WriteFile(filepath.Join(h.Run, osWritten), []byte(tag+"\n"), 0o600)
	if err == nil {
		err = mark()
	}
	if err == nil {
		err = h.writeOS(in, j, prefix, src)
	}
	if err == nil {
		err = h.retry(j)
	}
	switch {
	case err == nil && reboot:
		if err = h.schedule("reboot", 30, false, j, fmt.Sprintf("rebooting into %s in 30 s: the boot %s", tag, does)); err == nil {
			return // ended by schedule, with its reboot
		}
	case err == nil:
		h.logf(j, "%s is written: the next boot runs it and %s", tag, does)
	}
	h.finish(j, err)
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
		unlock, err := in.lockNow()
		if errors.Is(err, errLocked) {
			replyErr(w, notNowErr{fail(http.StatusConflict, "an install, upgrade, rollback, provision or reset-db is running: wait for it to finish"), "wait for it to finish, then run again"})
			return
		}
		// Any other answer lets the power action go: it needs no install (none yet, or one gone),
		// only that no lifecycle run holds one.
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
	if err := h.claimReboot(idle, nil); err != nil {
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
	time.AfterFunc(time.Duration(drain)*h.unit(), func() {
		if down, err := powerHost(h.ctx, h.Host, action); down {
			h.O.logf("systemctl %s stopped as the system goes down: %v", action, err)
		} else if err != nil {
			reason := fmt.Sprintf("systemctl %s failed: %v", action, err)
			h.O.logf("%s", reason)
			if by != nil {
				h.logf(by, "%s: the system keeps running", reason)
			}
			h.unclaimReboot()
			h.rebootCancelled(action, reason, by)
		}
	})
	return nil
}

// unclaimReboot gives back a reboot claimed that does not come: jobs start again, and the daemon
// drops its hold on the install's lock (released by its holder before).
func (h *Hostd) unclaimReboot() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rebooting, h.held = false, nil
	h.rebootGen++
}

// claimReboot claims the one reboot (idle: refused while a job runs), the daemon holding hold (a
// rollback's lock on the install; nil for none) with it until unclaimReboot.
func (h *Hostd) claimReboot(idle bool, hold func()) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rebooting {
		return notNowErr{fail(http.StatusConflict, "a reboot is scheduled"), "wait for the reboot, then run again"}
	}
	if idle && h.job != nil && h.job.State == "running" {
		return notNowErr{fail(http.StatusConflict, "a job (%s) is running: wait for it to finish", h.job.Kind), "wait for the job to end, then run again"}
	}
	h.rebooting, h.held = true, hold
	h.rebootGen++
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
	bin := c.Args[0]
	c.Args = c.Args[1:] // the rest of c (its stdin, its Cancel) as given
	return ExecTool{Bin: bin}.Run(ctx, c)
}
