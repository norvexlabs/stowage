package install

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/norvexlabs/stowage/internal/providers"
)

// The host daemon's side of the install's secrets:
//
//	POST /v1/secrets/export  {recipients}: {file}, the install's SecretsFile encrypted to those age
//	                         public keys, ASCII-armored
//	POST /v1/secrets/rotate  {key, pass}: one pass (stage, swap, finish) of a rotated secret's
//	                         rotation, as a job (202, {job, target}): the install's .env changes,
//	                         then the stack comes up again with it
//	POST /v1/restore         {object, identity, confirm}: a backup of the release the install runs
//	                         loaded into its database, as a job (202, {job, target})

// SecretsExportRequest is POST /v1/secrets/export's body, and SecretsExport its answer.
type SecretsExportRequest struct {
	Recipients []string `json:"recipients"`
}

type SecretsExport struct {
	File string `json:"file"`
}

func (h *Hostd) exportSecrets(w http.ResponseWriter, r *http.Request) {
	var req SecretsExportRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	in, _ := h.run()
	env, err := h.installed(in)
	if err != nil {
		replyErr(w, err)
		return
	}
	if _, err := parseRecipients(req.Recipients, maxRecipients); err != nil {
		replyErr(w, fail(http.StatusBadRequest, "%v", err))
		return
	}
	// The secrets are the ones the install's release names: those of another release (after a
	// rollback the OS carries a later one) would leave some out.
	if err := h.osRelease(env, "secrets export"); err != nil {
		replyErr(w, err)
		return
	}
	f, err := in.exportSecrets(env)
	if err != nil {
		replyErr(w, err)
		return
	}
	raw, err := encryptSecrets(f, in.R.Ctl(), req.Recipients)
	if err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, SecretsExport{File: string(raw)})
}

// osRelease refuses (409) an install running another release than the OS's, whose spec the
// daemon reads: what runs names the verb.
func (h *Hostd) osRelease(env *EnvFile, verb string) error {
	m := h.R.Manifest
	if name, v, rev := runs(env); name != m.Name || v != m.ProjectVersion || rev != strconv.Itoa(m.Revision) {
		return fail(http.StatusConflict, "the install runs %s %s (revision %s), not this OS's %s: %s runs on the release the OS carries", name, v, rev, Version{m.ProjectVersion, m.Revision}, verb)
	}
	return nil
}

// SecretRotateRequest is POST /v1/secrets/rotate's body.
type SecretRotateRequest struct {
	Key  string `json:"key"`
	Pass string `json:"pass"` // RotateStage, RotateSwap or RotateFinish
}

func (h *Hostd) rotateSecret(w http.ResponseWriter, r *http.Request) {
	var req SecretRotateRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	if !slices.Contains([]string{RotateStage, RotateSwap, RotateFinish}, req.Pass) {
		replyErr(w, fail(http.StatusBadRequest, "pass %q: %s, %s or %s", req.Pass, RotateStage, RotateSwap, RotateFinish))
		return
	}
	in, _ := h.run()
	if _, err := h.installed(in); err != nil {
		replyErr(w, err)
		return
	}
	if _, err := in.rotated(req.Key); err != nil {
		replyErr(w, fail(http.StatusBadRequest, "%v", err))
		return
	}
	// Not while the health gate judges the boot: the restarts could fail its deadline.
	if err := h.judged(r.Context()); err != nil {
		replyErr(w, err)
		return
	}
	j, err := h.startJob("rotate", req.Key, nil)
	if err != nil {
		replyErr(w, err)
		return
	}
	go func() {
		jw := &jobWriter{h: h, j: j}
		in.O.Out, in.O.Log = jw, jw
		err := in.RotateSecret(h.ctx, req.Key, req.Pass)
		jw.flush()
		h.finish(j, err)
	}()
	accepted(w, j)
}

// RestoreRequest is POST /v1/restore's body.
type RestoreRequest struct {
	// Object is a backup of this project in the destination (`backup list`), taken from the
	// release the install runs.
	Object string `json:"object"`
	// Identity is the age identity that decrypts it (AGE-SECRET-KEY-…): used for this restore,
	// never kept.
	Identity string `json:"identity"`
	// Confirm is the release the operator confirmed restoring over: required, and another is
	// refused.
	Confirm *Version `json:"confirm"`
}

// restoreBackup loads a backup into the install's database as a job: the services but the database
// stop, the backup is downloaded, decrypted as it streams, and restored by pg_restore in one
// transaction into a new database swapped in once it loaded whole, then the stack comes up
// again and the provision hook runs on the restored data.
func (h *Hostd) restoreBackup(w http.ResponseWriter, r *http.Request) {
	var req RestoreRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	if err := (&Installer{R: h.R}).hasDatabase("there is no backup to restore"); err != nil {
		replyErr(w, fail(http.StatusConflict, "%v", err))
		return
	}
	if req.Confirm == nil {
		replyErr(w, fail(http.StatusBadRequest, "restore replaces the install's data: confirm the release it runs (\"confirm\": {\"version\", \"revision\"})"))
		return
	}
	in, _, err := h.runnerCall(r) // refuses before the job: no settings, no install
	if err != nil {
		replyErr(w, err)
		return
	}
	env, err := in.open()
	if err != nil {
		replyErr(w, err)
		return
	}
	m := h.R.Manifest
	target := Version{m.ProjectVersion, m.Revision}
	if err := h.osRelease(env, "restore"); err != nil {
		replyErr(w, err)
		return
	}
	if *req.Confirm != target {
		replyErr(w, fail(http.StatusConflict, "the install runs %s %s, not %s, which was confirmed: look again", m.Name, target, *req.Confirm))
		return
	}
	pg, err := h.pgBackup()
	if err != nil {
		replyErr(w, err)
		return
	}
	if err := ownBackup(h.R.Spec.Name, req.Object, target, pg); err != nil {
		replyErr(w, err)
		return
	}
	ids, err := age.ParseIdentities(strings.NewReader(req.Identity))
	if err != nil {
		replyErr(w, fail(http.StatusBadRequest, "the identity is not an age identity (AGE-SECRET-KEY-…)"))
		return
	}
	if err := h.judged(r.Context()); err != nil {
		replyErr(w, err)
		return
	}
	// Not while an upgrade waits in the passive slot: a restore that left the volumes half
	// restored would strand it, its boot refusing them.
	j, err := h.startJob("restore", req.Object, func() error {
		if err := h.osWritten(); len(pg.Volumes) > 0 && err != nil {
			return fail(http.StatusConflict, "%v: reboot first, then restore", err)
		}
		return nil
	})
	if err != nil {
		replyErr(w, err)
		return
	}
	h.backups.Add(1)
	go func() {
		defer h.backups.Done()
		jw := &jobWriter{h: h, j: j}
		in.O.Out, in.O.Log = jw, jw
		err := h.runRestore(in, req.Object, ids, func(format string, a ...any) { h.logf(j, format, a...) })
		jw.flush()
		h.finish(j, err)
	}()
	accepted(w, j)
}

// ownBackup refuses an object that is not a backup of name taken from the release v (a backup
// restores onto the release whose schema it holds), or not of a profile keeping every table's
// rows: the restore replaces the whole database, and a partial profile's dump holds every
// table's structure, so the rows it left out would be lost.
func ownBackup(name, object string, v Version, pg *providers.PgAgeRclone) error {
	if !matchesOwnBackup(name, object) {
		return fail(http.StatusBadRequest, "%q is not a backup of %s (`backup list` shows them)", object, name)
	}
	profile, _, _ := strings.Cut(strings.TrimPrefix(object, name+"-"), "-")
	if tables, ok := pg.Profiles[profile]; !ok || tables != nil || len(pg.Exclude[profile]) > 0 {
		return fail(http.StatusConflict, "%s is a backup of the profile %s, which does not keep every table's rows: restoring it would empty the others (restore a backup of a profile `all`: %s)", object, profile, strings.Join(fullProfiles(pg), ", "))
	}
	if want := fmt.Sprintf("-%s-r%d.pgdump.age", v.Version, v.Revision); !strings.HasSuffix(object, want) {
		return fail(http.StatusConflict, "%s was not taken from %s %s, which the install runs: a backup restores onto the release it was taken from", object, name, v)
	}
	return nil
}

// fullProfiles are the profiles keeping every table's rows.
func fullProfiles(pg *providers.PgAgeRclone) []string {
	var out []string
	for _, p := range sortedKeys(pg.Profiles) {
		if pg.Profiles[p] == nil && len(pg.Exclude[p]) == 0 {
			out = append(out, p)
		}
	}
	return out
}

// matchesOwnBackup reports whether object is named as backupObject names name's backups: the
// project, a profile, a UTC time, then the release.
func matchesOwnBackup(name, object string) bool {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `-[a-z0-9]+-\d{8}T\d{6}Z-[^/\\]+-r\d+\.pgdump\.age$`).MatchString(object)
}

// The restore's two scripts, run in the database's container as role $1 for database $2, through
// the maintenance database $3 (postgres, or template1 for a database named postgres): a database
// cannot be renamed by a session connected to it.
//
// loadScript loads a pg_dump archive (stdin) beside $2, never into it: a new database,
// stowage_restore (a fixed name, so no cut to 63 bytes can make it $2's), made as the stack's own
// Postgres made $2 — from template0, with its owner, encoding and libc locale; nothing set at the
// database's level is carried, as a stack keeps its configuration in its schemas — takes the
// archive in one transaction, owners and privileges kept (every role they name must exist). A
// load that fails drops it. $2 is never touched, whatever ends the script: the swap is
// swapScript's, which the job runs only once the load succeeded and while it still waits — so no
// load the job stopped waiting for is ever swapped in under a running stack. A database named as
// one of the two the restore keeps for itself, or a template, is refused.
const loadScript = `u=$1 db=$2 maint=$3
case $db in stowage_restore|stowage_replaced|template0|template1) echo "the database is named $db, which a restore cannot replace: nothing restored" >&2; exit 3 ;; esac
sql() { psql -X -q -o /dev/null -v ON_ERROR_STOP=1 -U "$u" -d "$maint" -v db="$db"; }
sql <<'SQL' || exit 3
SET client_min_messages = warning;
SELECT EXISTS (SELECT FROM pg_database WHERE datname = 'stowage_replaced') AS replaced_left,
  EXISTS (SELECT FROM pg_database WHERE datname = 'stowage_restore') AS load_left \gset
\if :replaced_left
\echo 'dropping stowage_replaced, the database an earlier restore replaced and could not drop'
\endif
\if :load_left
\echo 'dropping stowage_restore, the database an earlier restore loaded and did not swap in'
\endif
DROP DATABASE IF EXISTS stowage_restore WITH (FORCE);
DROP DATABASE IF EXISTS stowage_replaced WITH (FORCE);
SELECT format('CREATE DATABASE stowage_restore OWNER %I TEMPLATE template0 ENCODING %L LC_COLLATE %L LC_CTYPE %L', pg_get_userbyid(datdba), pg_encoding_to_char(encoding), datcollate, datctype) FROM pg_database WHERE datname = :'db' \gexec
SQL
if ! pg_restore -U "$u" -d stowage_restore --single-transaction --exit-on-error; then
  echo 'DROP DATABASE IF EXISTS stowage_restore WITH (FORCE);' | sql
  echo "the backup was not loaded" >&2; exit 3
fi
`

// swapScript puts the loaded stowage_restore in $2's place: $2's sessions the role may end are
// ended (Postgres's own rule: a role it has the privileges of, a superuser's only by a superuser),
// then the two renamed in one transaction; if that fails, the load is dropped and it exits 4.
// Postgres bounds every psql it runs (sql: a 5 s connect, 15 s for a lock, 20 s a statement; about
// 90 s across both legs) inside the job's three minutes for it: when the job reads the outcome,
// nothing of the swap still runs. A failure may follow a commit the client did not see, so the
// job reads any end but 0 by swappedCheck. Dropping the replaced database is dropScript's.
const swapScript = `u=$1 db=$2 maint=$3
sql() { PGCONNECT_TIMEOUT=5 PGOPTIONS='-c lock_timeout=15s -c statement_timeout=20s' psql -X -q -o /dev/null -v ON_ERROR_STOP=1 -U "$u" -d "$maint" -v db="$db"; }
sql <<'SQL' && exit 0
SET client_min_messages = warning;
SELECT pg_terminate_backend(a.pid) FROM pg_stat_activity a JOIN pg_roles r ON r.oid = a.usesysid
  WHERE a.datname = :'db' AND a.pid <> pg_backend_pid()
    AND ((SELECT rolsuper FROM pg_roles WHERE rolname = current_user) OR (NOT r.rolsuper AND pg_has_role(current_user, r.oid, 'USAGE')));
BEGIN;
ALTER DATABASE :"db" RENAME TO stowage_replaced;
ALTER DATABASE stowage_restore RENAME TO :"db";
COMMIT;
SQL
echo 'DROP DATABASE IF EXISTS stowage_restore WITH (FORCE);' | sql
echo "the backup loaded but was not swapped in" >&2; exit 4
`

// dropScript drops the database a swap replaced, bounded by Postgres inside the job's ten minutes
// for it; one it cannot drop is told by the job, and the next restore drops it.
const dropScript = `echo 'DROP DATABASE IF EXISTS stowage_replaced WITH (FORCE);' |
  PGCONNECT_TIMEOUT=5 PGOPTIONS='-c lock_timeout=30s -c statement_timeout=8min' psql -X -q -o /dev/null -v ON_ERROR_STOP=1 -U "$1" -d "$3"
`

// swappedCheck prints how many databases are named stowage_replaced: 1 once a swap committed —
// only its rename makes one, the load having dropped any an earlier restore left.
const swappedCheck = `PGCONNECT_TIMEOUT=5 PGOPTIONS='-c statement_timeout=10s' psql -X -tA -v ON_ERROR_STOP=1 -U "$1" -d "$3" -c "SELECT count(*) FROM pg_database WHERE datname = 'stowage_replaced'"`

// maintenanceDB is the database the restore's scripts connect through: postgres, or template1
// for a database named postgres.
func maintenanceDB(name string) string {
	if name == "postgres" {
		return "template1"
	}
	return "postgres"
}

// provisionRestored runs the provision hook on the data a restore loaded. Without a hook it does
// nothing.
func (h *Hostd) provisionRestored(ctx context.Context, in *Installer, env *EnvFile, object string, logf func(string, ...any)) error {
	hook, err := in.hook()
	if err != nil {
		return fmt.Errorf("restored %s, but the provision hook cannot be read: %w", object, err)
	}
	if hook == nil {
		return nil
	}
	logf("provisioning %s on the restored data", in.R.Spec.Name)
	if err := in.provisionNow(ctx, env, hook); err != nil {
		return fmt.Errorf("restored %s, but the provision hook failed: %w (`sudo %s provision` runs it again)", object, err, in.R.Ctl())
	}
	return nil
}

// runRestore stops the services but the database, streams the backup into pg_restore, and brings
// the stack up again whatever happened.
func (h *Hostd) runRestore(in *Installer, object string, ids []age.Identity, logf func(string, ...any)) (err error) {
	ctx, cancel := context.WithTimeout(h.ctx, backupTimeout)
	defer cancel()
	unlock, err := lockFile(in.path(lifecycleLock))
	if errors.Is(err, errLocked) {
		return errors.New("an install, upgrade, rollback, provision or reset-db is running: restore again once it is done")
	}
	if err != nil {
		return err
	}
	defer unlock()
	// The install and the backup settings as they are under the lock: a run before it may have
	// changed them.
	env, err := in.open()
	if err != nil {
		return err
	}
	// The hook runs after the data is replaced: a container an earlier run left would fail it then.
	if err := in.leftovers(ctx, env); err != nil {
		return err
	}
	if err := h.osRelease(env, "restore"); err != nil {
		return err
	}
	pg, err := h.pgBackup()
	if err != nil {
		return err
	}
	h.backupMu.Lock()
	c, err := h.readBackup(pg)
	h.backupMu.Unlock()
	switch {
	case err != nil:
		return err
	case c == nil:
		return errors.New("the backup settings were removed meanwhile")
	}
	conf, done, err := h.rcloneConf(*c)
	if err != nil {
		return err
	}
	defer done()
	d := in.R.Spec.DB()
	out, err := in.Compose(ctx, env, nil, "config", "--services")
	if err != nil {
		return err
	}
	var others []string
	for _, s := range strings.Fields(string(out)) {
		if s != d.Service {
			others = append(others, s)
		}
	}
	// Every volume's archive is there before anything stops.
	for _, v := range pg.Volumes {
		src := within(c.Destination, volumeObject(object, v))
		out, err := h.rclone(ctx, in, conf, nil, "lsjson", "--files-only", "--no-mimetype", src)
		var found []struct{ Name string }
		if err == nil {
			err = json.Unmarshal(out, &found)
		}
		if err != nil || len(found) != 1 {
			return fmt.Errorf("%s has no archive of the volume %s to restore with it (%s: %v): nothing restored", object, v, src, cmp.Or(err, errors.New("not found")))
		}
	}
	// Registered first: a stop that fails part-way leaves services stopped too. Once the stack is
	// up on restored data, the provision hook runs on it: its values belong to that data.
	// An earlier restore that left the volumes half restored keeps the stack down until this one
	// finishes, whatever ends it.
	hadSplit := in.volumesSplit() != nil
	swapped, halfSwapped := false, hadSplit
	// asWas says what a failure before the swap left: what was, or volumes an earlier restore
	// left half restored (whose staging this one's replaced).
	asWas := func(what string) string {
		if hadSplit {
			return "(the volumes are still half restored: restore again)"
		}
		return "(the " + what + " is as it was)"
	}
	dropReplaced := func() {}
	defer func() {
		// A volume left half swapped keeps the services stopped: they would run, and be backed
		// up, on data of two times.
		if halfSwapped {
			// Their containers go too: a restart policy would start them at a reboot.
			if len(others) > 0 {
				rctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 2*time.Minute)
				defer cancel()
				if _, rerr := in.Compose(rctx, env, in.O.Log, append([]string{"rm", "--stop", "--force"}, others...)...); rerr != nil {
					logf("the stopped services' containers were not removed (%v): a restart policy may start them at a reboot", rerr)
				}
			}
			return
		}
		// A context of its own: the restore's may be what ran out.
		up, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 10*time.Minute)
		defer cancel()
		if uerr := in.up(up, env); uerr != nil {
			if swapped {
				uerr = fmt.Errorf("restored %s, but %w: once the stack is up, `sudo %s provision` provisions it", object, uerr, in.R.Ctl())
			}
			err = errors.Join(err, uerr)
		}
		if err == nil {
			// Its own context: bringing the stack up may have taken most of up's.
			err = h.provisionRestored(context.WithoutCancel(h.ctx), in, env, object, logf)
		}
		// Only a restore that is done gives up the database it replaced: one that failed after
		// the swap leaves it (the next restore drops it) and its lock free at once.
		if swapped && err == nil {
			dropReplaced()
		}
	}()
	// The runner is there before anything stops: a Docker install pulls it again here, never
	// inside a step bounded after the swap.
	var image string
	if len(pg.Volumes) > 0 {
		if image, err = h.runnerImage(ctx, in); err != nil {
			return err
		}
	}
	// The database alone comes up when an earlier restore left the stack down: the load runs in
	// its service.
	if hadSplit {
		if _, err := in.Compose(ctx, env, in.O.Log, "up", "--detach", "--wait", "--no-deps", d.Service); err != nil {
			return fmt.Errorf("starting the database: %w (the volumes are still half restored: restore again)", err)
		}
	}
	// Without a service to name, `stop` would stop them all, the database included.
	if len(others) > 0 {
		logf("stopping %s but its database: %s", in.R.Spec.Name, strings.Join(others, ", "))
		if _, err := in.Compose(ctx, env, in.O.Log, append([]string{"stop", "--timeout", "30"}, others...)...); err != nil {
			return fmt.Errorf("stopping the services: %w", err)
		}
	}
	// The volumes are staged first, each in a directory of its own beside its data, which a
	// restore that ends before the swap removes.
	staged := map[string]string{}
	step := volumeStepWait * cmp.Or(h.Unit, time.Second)
	unstage := func() {
		for _, v := range sortedKeys(staged) {
			clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), step)
			if _, err := h.runnerShell(clean, in, image, staged[v], false, nil, nil, unstageScript); err != nil {
				logf("the volume %s keeps the restore's staging directory %s (%v): the next restore removes it", v, restoreStage, err)
			}
			cancel()
		}
	}
	for _, v := range pg.Volumes {
		vol, err := in.stackVolume(ctx, env, v, true)
		if err != nil {
			unstage()
			return fmt.Errorf("%w %s", err, asWas("data"))
		}
		staged[v] = vol
		src := within(c.Destination, volumeObject(object, v))
		logf("restoring %s into the volume %s", src, v)
		if err := h.fetchDecrypted(ctx, in, conf, src, ids, func(plain io.Reader) error {
			_, err := h.runnerShell(ctx, in, image, vol, false, plain, nil, stageScript)
			return err
		}); err != nil {
			unstage()
			return fmt.Errorf("the volume %s: %w %s", v, err, asWas("data"))
		}
	}
	src := within(c.Destination, object)
	logf("restoring %s into %s's database", src, in.R.Spec.Name)
	if err := h.fetchDecrypted(ctx, in, conf, src, ids, func(plain io.Reader) error {
		load := []string{"exec", "-T", d.Service, "sh", "-c", loadScript, "restore", d.Roles.Migrate, d.Name, maintenanceDB(d.Name)}
		if _, err := in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), load...), Env: env.Environ(in.O.environ()), Stdin: plain, Stream: in.O.Log}); err != nil {
			return fmt.Errorf("loading %s: %w", object, err)
		}
		return nil
	}); err != nil {
		unstage()
		// The load writes only its own database: whatever ended it, the install's is as it was.
		return fmt.Errorf("%w %s", err, asWas("database"))
	}
	inDB := func(base context.Context, timeout time.Duration, script, verb string, stream bool) ([]byte, error) {
		cctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		cmd := Cmd{Args: append(in.composeBase(), "exec", "-T", d.Service, "sh", "-c", script, verb, d.Roles.Migrate, d.Name, maintenanceDB(d.Name)), Env: env.Environ(in.O.environ())}
		if stream {
			cmd.Stream = in.O.Log
		}
		return in.O.Docker.Run(cctx, cmd)
	}
	// The swap is short and never left half done: a context of its own, which neither the job's
	// deadline nor the daemon's stop cuts while it commits.
	firm := context.WithoutCancel(h.ctx)
	// From the database's swap until every volume is swapped in, the data is of two times: the
	// mark keeps the stack down and backups off, across a reboot too, until a restore finishes.
	split := in.path(volumesSplitFile)
	if len(staged) > 0 {
		if err := writePrivate(split, []byte(object+"\n")); err != nil {
			// A write that failed after its rename leaves it: one that cannot go keeps the
			// stack down, as it says.
			if rerr := os.Remove(split); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				halfSwapped = true
			}
			unstage()
			return fmt.Errorf("%w %s", err, asWas("data"))
		}
	}
	if _, err := inDB(firm, 3*time.Minute, swapScript, "swap", true); err != nil {
		// The swap's one transaction either committed or not, whatever the exit: stowage_replaced,
		// which only its rename makes, tells which.
		out, qerr := inDB(firm, 30*time.Second, swappedCheck, "check", false)
		switch {
		case qerr != nil:
			// Committed or not, a restore again does it all (its load drops what this one
			// left). With volumes, the database may be the backup's beside volumes that are not:
			// the stack stays down, marked, until then.
			halfSwapped = halfSwapped || len(staged) > 0
			if !halfSwapped {
				return fmt.Errorf("swapping %s in: %w; whether it committed could not be read (%v): a database named stowage_replaced in %s's Postgres means it did — then `sudo %s provision` finishes the restore", object, err, qerr, in.R.Spec.Name, in.R.Ctl())
			}
			return fmt.Errorf("swapping %s in: %w; whether it committed could not be read (%v): a database named stowage_replaced in %s's Postgres means it did, and holds the data from before the restore, which a restore again drops — restore again to bring the backup back whole, its volumes included", object, err, qerr, in.R.Spec.Name)
		case strings.TrimSpace(string(out)) != "1":
			unstage()
			if hadSplit {
				// The volumes are still as an earlier restore left them.
				halfSwapped = true
				return fmt.Errorf("swapping %s in: %w (the database is as it was; the volumes still half restored: restore again)", object, err)
			}
			if err := os.Remove(split); err != nil && !errors.Is(err, os.ErrNotExist) {
				halfSwapped = true
				return fmt.Errorf("swapping %s in: the data is as it was, but %w: remove it, then a re-run of the installer brings the stack up", object, err)
			}
			return fmt.Errorf("swapping %s in: %w (the database is as it was)", object, err)
		}
		logf("the swap ended with %v, but committed: the backup is in place", err)
	}
	swapped = true
	dropReplaced = func() {
		// Optional, once the stack is up and provisioned: never holding either back, and with a
		// context of its own, which the load's long hour cannot have used up.
		if _, err := inDB(firm, 10*time.Minute, dropScript, "drop", true); err != nil {
			logf("the replaced database was not dropped (%v): the next restore drops stowage_replaced", err)
		}
		// Bounded, and cut by the daemon's stop: the install's lock is held until it ends. One
		// left is removed by the next restore's staging.
		for _, v := range sortedKeys(staged) {
			dctx, cancel := context.WithTimeout(h.ctx, 6*step)
			if _, err := h.runnerShell(dctx, in, image, staged[v], false, nil, nil, dropVolumeScript); err != nil {
				logf("the volume %s keeps its data from before the restore in %s (%v): the next restore removes it", v, restoreReplaced, err)
			}
			cancel()
		}
	}
	// Each volume is swapped in by renames alone, its data moved aside rather than deleted, each
	// bounded (a hung runner never holds the install's lock), every one tried: one that fails
	// keeps what it holds, split between its root, its staging and its former data, and the
	// services stay stopped until a restore again.
	var failed []error
	for _, v := range sortedKeys(staged) {
		sctx, cancel := context.WithTimeout(firm, step)
		_, err := h.runnerShell(sctx, in, image, staged[v], false, nil, nil, swapVolumeScript)
		cancel()
		if err != nil {
			failed = append(failed, fmt.Errorf("the volume %s: %w", v, err))
		}
	}
	if len(failed) > 0 {
		halfSwapped = true
		return fmt.Errorf("restored %s's database, but not every volume was swapped in (%w; its files are split between its root, %s — the backup's — and %s — its own): the services stay stopped, restore again", object, errors.Join(failed...), restoreStage, restoreReplaced)
	}
	if err := os.Remove(split); err != nil && !errors.Is(err, os.ErrNotExist) {
		halfSwapped = true
		return fmt.Errorf("restored %s whole, but %w: remove it, then restore again", object, err)
	}
	halfSwapped = false
	// The data is loaded whole: what a --secrets install waited for is here, whatever comes next.
	in.restoreDone()
	logf("restored %s", object)
	return nil
}

// restoreStage is the directory, at a volume's root, a restore extracts the volume's archive into;
// restoreReplaced the one its swap moves the volume's former data into, deleted once the restore
// is done. volumeStepWait bounds, in Unit, a volume's swap (renames) and its staging's removal.
const (
	restoreStage    = ".stowage-restore"
	restoreReplaced = ".stowage-replaced"
	volumeStepWait  = 10 * 60
)

// archiveScript writes the volume's archive, a restore's two directories left out; stageScript
// removes what an earlier restore left of them, then extracts one archive (stdin) into the
// staging directory, made with the root's owner and mode (an archive's own root entry then sets
// them), the two names never extracted; unstageScript removes it; swapVolumeScript, renames
// alone, moves the volume's entries into the replaced directory, then the staging's into the
// root, which takes the archive's owner and mode; dropVolumeScript deletes the replaced data.
const (
	archiveScript = "tar -c -f - -C /v --exclude ./" + restoreStage + " --exclude ./" + restoreReplaced + " ."
	stageScript   = "rm -rf /v/" + restoreStage + " /v/" + restoreReplaced + " && mkdir /v/" + restoreStage +
		` && chown "$(stat -c %u:%g /v)" /v/` + restoreStage + ` && chmod "$(stat -c %a /v)" /v/` + restoreStage +
		" && tar -x -f - -C /v/" + restoreStage + " --exclude ./" + restoreStage + " --exclude ./" + restoreReplaced
	unstageScript    = "rm -rf /v/" + restoreStage
	swapVolumeScript = "cd /v && mkdir " + restoreReplaced + " && " +
		`for f in ./* ./.[!.]* ./..?*; do case "$f" in ./` + restoreStage + "|./" + restoreReplaced + `) continue ;; esac; if [ -e "$f" ] || [ -L "$f" ]; then mv -- "$f" ` + restoreReplaced + `/ || exit 1; fi; done && ` +
		"for f in ./" + restoreStage + "/* ./" + restoreStage + "/.[!.]* ./" + restoreStage + `/..?*; do if [ -e "$f" ] || [ -L "$f" ]; then mv -- "$f" . || exit 1; fi; done && ` +
		`chown "$(stat -c %u:%g ` + restoreStage + `)" . && chmod "$(stat -c %a ` + restoreStage + `)" . && rmdir ` + restoreStage
	dropVolumeScript = "rm -rf /v/" + restoreReplaced
)

// fetchDecrypted downloads src, decrypting it as it streams into use; a failure says whether the
// download or the identity was why.
func (h *Hostd) fetchDecrypted(ctx context.Context, in *Installer, conf, src string, ids []age.Identity, use func(io.Reader) error) error {
	pr, pw := io.Pipe()
	fetched := make(chan error, 1)
	go func() {
		_, err := h.rcloneTo(ctx, in, conf, h.runnerName(), nil, pw, "cat", src)
		if err != nil {
			err = fmt.Errorf("downloading %s: %w", src, err)
		}
		pw.CloseWithError(err)
		fetched <- err
	}()
	plain, err := age.Decrypt(pr, ids...)
	if err != nil {
		pr.CloseWithError(err)
		if ferr := <-fetched; ferr != nil {
			return ferr
		}
		return fmt.Errorf("decrypting %s: %w (is the identity one of the backup's recipients?)", src, err)
	}
	uerr := use(plain)
	pr.CloseWithError(errUploadEnded) // the download has nothing more to write to
	if ferr := <-fetched; ferr != nil && uerr != nil {
		return ferr
	}
	return uerr
}
