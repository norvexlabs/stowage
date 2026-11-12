# The installer on the Kairos VM (`<short>-stow` in the OS bundle)

[README](../README.md) · [Documentation index](../README.md#documentation)

The OS bundle carries `/usr/sbin/<short>-stow`: the installer with the release as the bundle carries
it — the manifest without its publication (no channel, tag, number, date or artifact beyond the
images), the compose file, the runtime spec, the trusted keys and the pull account — so one
bundle serves every channel it is promoted to. On the VM it runs as root or for the `admin`
group, installs into `/usr/local/<name>` (the persistent partition, which an A/B upgrade keeps)
and never pulls: it runs the images the bundle preloads, by their digest tag
(`<prefix>/<name>-<key>:sha256-<hex>`), each checked against the IDs the release records.

```bash
sudo <short>-stow install [--channel C] [--set KEY=VALUE]… [--set-file KEY=PATH]… [--answers FILE] [--no-input] [--accessible]
          [--ports route=port,…] [--address 127.x.x.x] [--tls MODE] [--domains route=host,…]
          [--cert F --key F [--ca F]] [--http-port N] [--https-port N] [--no-provision] [--dry-run]
          [--tune-profile default|network|release]   # on an installed VM, a higher profile applies at once, a lower one at the next reboot
          [--secrets FILE|- | --new-secrets]   (both: refused)
          [--given FILE|- | --kit FILE|- [--kit-code-file FILE]]   (with --secrets: refused)
          [--json]   # the run's result as one object, for a program (see "Exit status and --json on the VM")
<short>-stow --version               # (-v) the release this OS carries and the Stowage that built it, as the Docker installer's
<short>-stow describe [--json]        # what the release this OS carries offers, read with no install: the Docker installer's describe
<short>-stow status [--json]          # this OS's release, the install's (and `not finished` while a first install's stack comes up or its provision hook runs, or after one failed — with why, and the `sudo <short>-stow install` that runs it again; `"installing": true` and `"install_failed": "<why>"` in JSON; `not provisioned` while an install given `--secrets` awaits the backup `restore` loads, `"restore_awaited": true`; `not running` while a restore left the volumes half restored, `"volumes_split": true`), the channel, the stack's tuning profile and whose it is (`tune       network (operator)`, `"tune"` in JSON), the system Kairos booted (active, passive or recovery; the fallback), the last failed boot, the disk, a byo install's certificate (`cert`: its names, end and days left, with a warning from 30 days before it; `"cert"` in JSON), a network warning (`network    warning: …`, `"network_warning"` in JSON: IPv6 on and no IPv6 default route for a minute or more), the services
<short>-stow health [--json]          # ready, or fails saying why: the health gate's check, once
<short>-stow events [--after N] [--follow|-f] [--json]   # the event journal: jobs, reboots, boots and the health gate's verdicts, the first install; --follow waits for more until interrupted, through daemon restarts
<short>-stow ps [--json]              # the stack's services, state and health, then `down: <service> <why>` for each service down (as on Docker's `status`; the VM's `status` ends with the same); `--json`: each with `exit_code`, and when set (each absent when false, 0 or empty) `id`, `oom_killed`, `mem_limit_bytes` and `restarts` (these three from docker inspect)
<short>-stow logs SERVICE [--tail N]  # 1 to 5000 lines, default 200
<short>-stow exec [--stdin] SERVICE -- COMMAND [ARG…]   # in the running container, no terminal; --stdin (-i) passes this command's input (at most 4 MiB), else none; its outputs once it ends, its exit status
<short>-stow restart|stop|start SERVICE
<short>-stow provision [--set KEY=VALUE]…   # the provision hook again; --set as on Docker (an input only it reads, or a value it returned or declares it returns; empty forgets it)
<short>-stow updates [--channel C] [--version V] [--json]
<short>-stow upgrade [--channel C] [--version V] [--yes] [--no-reboot] [--skip-backup] [--accessible] [--json]   # ends at the reboot it schedules: status and events after it tell how it went
<short>-stow rollback [--yes] [--accessible] [--json]   # reboot into the passive system: back to the release the last upgrade replaced; ends at that reboot
<short>-stow reset-db [--yes] [--no-provision] [--accessible]   # wipe the data, start again as first installed
          [--new-baseline [--channel C] [--version V]]   # instead: a later release at another schema baseline, its OS written and booted, that boot wiping the data
<short>-stow restore OBJECT [--identity FILE|-] [--yes] [--accessible]   # load a backup into the database and its volumes (the volumes alone without one)
<short>-stow secrets export --recipient age1…… [-o FILE|-]   # the install's secrets, encrypted
<short>-stow secrets rotate KEY [--stage | --finish]   # a secret with rotation: true
<short>-stow secrets set KEY [--accessible] < value   # a secret with settable: true
<short>-stow secrets show KEY   # a settable secret's value, on a terminal only (the operator's socket)
<short>-stow host-key [--json]   # as root: this VM's own SSH host key, the seed's one-time key retired
<short>-stow network [--json]          # the address, gateway and DNS servers, what sets them, a change awaiting confirmation
<short>-stow tune [--check]           # how the VM's network settings differ from what its boot tuning wants (the stack's profile); --check exits non-zero
<short>-stow network set --mode dhcp|static [--address A/P] [--gateway G] [--ipv6 [--address6 A/P] [--gateway6 G]] [--dns D,…] [--revert-secs N | --no-revert]
<short>-stow network confirm|revert   # keep the change awaiting confirmation, or undo it now
<short>-stow network reset [--revert-secs N | --no-revert]   # drop the override: back to the cloud-config's network
<short>-stow network set --role ROLE --mode dhcp|static|none [--address A/P] [--revert-secs N | --no-revert]   # a secondary NIC's override
<short>-stow network reset --role ROLE [--revert-secs N | --no-revert]   # drop it
sudo <short>-stow network attach --role ROLE --mac MAC [--mode dhcp|static|none] [--address A/P]   # a NIC the release names, from the next boot
<short>-stow backup config [--json] [--destination REMOTE:PATH] [--rclone-config FILE|-] [--recipient age1…]…
          [--retention-days N] [--schedule CALENDAR] [--profile P] [--remove]   # show, change or drop the backup settings
          [--lock | --unlock]   # the app socket may (not) change the settings
<short>-stow backup run [--profile P] [--if-configured]   # back up now, and wait for it
<short>-stow backup status|list [--json]   # the settings, the next and the last backup; the destination's backups
<short>-stow backup verify [--destination REMOTE:PATH] [--rclone-config FILE|-] [--recipient age1…]…   # write a probe file, as a job it waits for; the flags try settings unsaved
<short>-stow reboot|shutdown [--drain-secs N]   # 0 to 600, default 30
```

- The VM audits nothing itself, and holds no scanner: an operator audits its release, or
  exports its SBOMs, from their own machine (`<short>-stow audit --version V`, see the Docker
  shape), by the version `status` names.
- `install` is the Docker shape's first install (the same flags, form, checks and refusals) from
  the preloaded images, asking the inputs of the `kairos` shape (`inputs[].shapes`); its re-run
  is the Docker shape's too, moving between TLS modes and taking a new certificate (`--tls`,
  `--cert`, `--key`, `--ca` for a project with `front.publish_ca`, and `--client-ca` for one
  with a `client_auth` route), and `--secrets` makes a
  first install with exported secrets (see `secrets export` under the Docker shape). `--kit FILE|-` and `--kit-code-file FILE` take a handoff kit, as on the Docker shape, and go as `--given` does; on either flag an input declared for other shapes alone is left out and logged (`… not an input of the kairos shape`). `--given FILE|-`
  takes what `vm create` gives over SSH, at most 1 MiB, strict YAML (`project`, `inputs` of KEY: value with a file input as `stowage-file:<base64>`, `tls` of `cert`, `key`, `ca`, `client_ca` as PEM; a certificate picks `byo`): a first install or a re-run, refused with `--secrets`, on Kairos's recovery system, when encrypted, for another project, holding nothing, with a `cert` or `key` alone, or a `ca` without a `cert`. Flags and `--answers` win: `--cert`, `--key` or `--tls` with another mode leave out the file's `cert`, `key` and `ca`, `--ca` and `--client-ca` their own, and the run logs what it left out. A first
  install takes the settings file `/etc/<name>/install.yaml` (written by the cloud-config `vm
  create` makes; YAML: `channel`, `tls`, `http_port`, `https_port`, `inputs` of KEY: value,
  `await_secrets` (the first install waits for what `vm create` gives over SSH), `ipv6`, `release` (see `vm create`: the release whose OS a first boot takes first), and `registry` and `registry_ca` — a mirror and the file of its CA, taken as `--registry` and
  `--registry-ca`, so the install records them and its updates and upgrades read the mirror,
  with the login `registry-login` recorded for the mirror's host (below), else root's Docker login on the VM, anonymously when it has none —; any other key refused) for what its flags and `--answers` do not give (its `tls` is not taken when
  `--cert` or `--key` is given, nor when its `tls` is `none` and a domain is given, by
  `--domains`, `--set` or `--answers`; with `--tls none`, its domains are not taken); `tls: byo` and a secret
  input in it are refused (they are given over SSH). `--channel` (a channel name, as `lint` takes them) is recorded as
  `STOWAGE_CHANNEL`, the channel `updates` and `upgrade` follow, at the first install only (a
  re-run given it is refused, as with the other first-install flags; `--channel` on `updates` and
  `upgrade` names another for one run). A second run is a re-run, as on Docker: over SSH,
  `sudo <short>-stow install --set KEY=VALUE` (or `--answers`, `--domains`, `--ports`, `--http-port`,
  `--https-port`) changes what the install recorded and brings the stack up again; the settings
  file is read by the first install only, and every boot keeps what the install records.
- At every boot `<name>.service` runs `<short>-stow boot` (after Docker, `<name>-preload.service`
  and the cloud-config's boot and network stages, `cos-setup-boot.service` and
  `cos-setup-network.service`). With no install, it makes the first one from the settings file,
  with nothing asked; without that file, while a secret input the install requires has no
  value, or when the file says `await_secrets: true` (`vm create --secrets`, or material given over SSH), it does nothing,
  saying that `sudo <short>-stow install` (`--secrets`; `--given` when `install.yaml.given` marks that wait) makes it — and an install without
  it is then refused (`--given` does not meet the wait for `--secrets`, nor `--secrets` the wait for `--given`), as it would generate secrets the backups do not match, unless
  `--new-secrets` (the file lost) goes on, generating them, or with the flags that give the material again; a mark of `byo` keeps the mode `byo` unless `--tls` says otherwise, so that run asks for the certificate rather than moving to another mode (that run takes the
  settings file and asks the rest). An upgrade or a rollback a reboot stopped is finished first by
  the install's own `<short>-stow` (`.stowage-prev/` recording the release the install runs gives
  it away). Then an install of the OS's release comes up again; an install of
  the release the last upgrade replaced (Kairos booted the passive slot) goes back to it with the
  install's own `<short>-stow` — the newer release's — which first brings the database up, so the
  check reads the schema the newer release left, and refuses as `rollback` refuses (a rollback
  barrier, a release that does not read the newer schema) or when the database does not come up; an install that went back from the
  OS's release stays as it is, brought up again (no upgrade loop: upgrade to a later release, or
  remove `.stowage-failed/` to try that one again); any other install is upgraded to the
  OS's release as `upgrade` does (its own `<short>-stow` brings its stack up first, then the database
  is dumped to `snapshots/`, the replaced release is kept in `.stowage-prev/`, and the install
  keeps the OS's `<short>-stow`). The install's own `<short>-stow` is run only when it carries the
  release the install runs; otherwise the boot fails, naming it. It waits up to 30 minutes.
  `boot` and `boot-rollback` (the install's own `<short>-stow` going back, or finishing a stopped
  run without bringing the database up) are the boot's verbs, hidden from the help, and so is
  `health-gate`. Hidden flags:
  `--dir` (default `/usr/local/<name>`) on `install`, `boot`, `boot-rollback`, `health-gate`, `hostd` and `registry-login`;
  `--settings` (default `/etc/<name>/install.yaml`) on `install`, `boot` and `registry-login`;
  `--socket` (default `/run/<name>/host.sock`) on every verb — the socket a verb asks, and the one
  `hostd` serves on.
- `status --json` is the host daemon's status (`GET status`, below), the core of the machine
  interface every shape's `status --json` shares: `format` (`1`, as the Docker installer's),
  `name`, `os` (the release this OS carries), `installed` (`{"version", "revision"}`, `null`
  before the first install), `previous` — the release the last upgrade replaced, which
  `rollback` goes back to (`{"version", "revision"}`; no `installer`, as the OS, not an
  installer, carries a VM's release), `null` when the install keeps none (before an upgrade, and
  after a rollback) — and `inputs`, the keys of the inputs the install records, sorted, never a
  value, so the output can be logged, whichever release declared them (the install's
  `STOWAGE_INPUTS`, as on Docker; absent before the first install, and when the install records
  no such list and the OS's `<short>-stow` carries another release than the one installed, a
  crossing whose boot fell back, and the install keeps no installer of it this one reads); then `channel`, `installing`,
  `install_failed`, what the first install recorded as Docker's `status --json` gives it —
  `registry`, `short_name`, `address`, `registry_ca` (`sha256:` of the registry CA the install
  keeps; left out when it keeps none, or when it cannot be read: the status always answers, and
  a poll logs nothing) — and the VM's own fields.
- `registry-login FILE|-` (as root) records the login the VM reads its mirror with: a Docker
  config holding one `auths` entry for the mirror's host, `{"auths": {"HOST": {"auth":
  "BASE64(USER:PASSWORD)"}}}` (or `username` and `password` for `auth`), from FILE or standard
  input, at most 32 KiB. It is refused when not run as root, for another host than the
  mirror's (the install's recorded `registry`, before the first install the settings file's),
  on a VM reading the release's own registry (which it reads with the pull account), and for a
  file that is not one such entry (two entries, a credential store's `credsStore` or
  `credHelpers`, an `identitytoken`, no user or password; the error names the field, never a
  value). It is recorded in `/usr/local/<name>.registry-login.json`, root's alone (0600), beside
  the install directory: kept by the first install, upgrades, rollbacks and reboots, replaced by
  the next `registry-login`, removed by `registry-login --remove`. The VM's reads of that host
  alone take it — its updates, an upgrade's OS bundle (`kairos-agent` runs with a Docker config
  of it alone), a first boot's switch to another release's OS — and the release's pull account
  is never sent there. `status` says `login      held for the mirror HOST`, `status --json`
  `registry_login` (the host), never the login. A recorded file that no longer reads fails the
  reads of the mirror, naming the file and the fix, and `status --json` says so
  (`registry_login_error`). `vm create --registry-login-file` gives it
  over SSH once the VM answers (see [vm-create](vm-create.md)); a program that made the VM from
  `vm seed` runs `sudo <short>-stow registry-login -` over SSH itself. The cloud-config never
  carries it.
- `describe [--json]` prints what the release this OS carries offers, with no install: the
  Docker installer's `describe` (see "The installer (Docker shape)" ([installer](installer.md))), the same object.
- **Exit status and `--json` on the VM.** `install`, `upgrade` and `rollback` take `--json`, as
  the Docker installer's verbs do (see "`--json` on the verbs that change an install" ([machine-interface](machine-interface.md))): one
  object on standard output at the run's end, `{"format", "result", "release", "refused":
  {"reason", "fix"}}` (`fix` absent when no advice holds on the VM), everything the run prints for people (`upgrade`'s release and changelog,
  `rollback`'s plan, the job's log) going to standard error, and a failure printing no object,
  only its error. `release` is the release the install runs as the run ends: `upgrade` and
  `rollback` end as their job schedules the reboot, so theirs is the release installed before
  that reboot, read from the daemon's `status` (absent when none is installed, or when the
  daemon does not answer then, said on standard error); after the reboot, a new `boot_id`,
  `boot_settled` and `boot_judged` and `installed` in `status --json` tell how it went (see
  `GET status` below). `upgrade` with nothing to upgrade to is
  `nothing`. `refused.fix` is advice for a person — what to choose or change —, never a command
  line to run, and holds on the VM: a refusal whose Docker fix names `--dir`, the installer the
  install keeps run by hand, or a flag of the upgrade (which on the VM takes none) carries no
  fix here, and is still a refusal. Each `<short>-stow` run exits `0` done or nothing to do, `3`
  refused (a rule said no before anything changed, the error on standard error naming what is
  wrong), `1` failed.
  The refusals: on `install` (a first install or its re-run, which the VM runs itself as the
  Docker shape does): `--secrets` with `--given` or `--kit`; a directory holding another
  project's `compose.yaml`, or that became an install meanwhile; a re-run given `--secrets`, or
  a first-install-only setting (`--address`, `--registry`, `--registry-ca`, `--channel`); a
  re-run's value its input refuses, a required input left
  empty, or a new certificate that does not cover a route's domain; another lifecycle run holding the lock, or a hook's or
  check's container an earlier run left; volumes a restore left half restored; a required input
  missing or a value its input refuses; ports in use, a front port that is an L4 route's, an L4
  route bound to an address the VM does not have, an existing `tls/`, `--ports`/`--address` or
  `--http-port`/`--https-port` in the wrong TLS mode (the fix of an existing `tls/` is to move it
  away). Refused without a fix on the VM — no step of its verbs is sure to clear them, so the
  reason names what is wrong alone: a re-run by the installer of another release than the one
  installed (`<dir> runs <name> <v> (revision <r>), and this OS's installer carries …`), an
  upgrade by an OS installer whose release comes before the one installed (`<dir> runs <name> <v>
  (revision <r>), after this installer's …`), the boot's upgrade across a schema baseline when the
  install runs another release than the one `reset-db --new-baseline` confirmed (`… which was
  confirmed: nothing was wiped`), another project's install or one kept by another installer
  name, and a directory holding another project's `compose.yaml`. A verb run before the first
  install (`<dir> holds no install`) has the fix `sudo <short>-stow install`. On `upgrade`: `--given` or `--kit`;
  `--version V` refused here (`upgrade --version V: refused, the install went back from …`, or
  `… is not one this VM, which runs …, can be upgraded to …`); no terminal to confirm on without
  `--yes`; and what the host daemon refuses, below. On `rollback`: no terminal to confirm on
  without `--yes`, and what the daemon refuses. An operator's "no" at a prompt (`aborted`:
  `install`'s form, `upgrade`, `rollback`, `restore`, and on the operator's machine `vm
  create`'s confirmation and `vm destroy`'s) is a refusal too. Every verb that asks the host
  daemon exits `3` on an error answer marked `refused` (a refusal, the daemon having changed
  nothing; its `fix` when it has one) and `1` on any other: the daemon refuses an upgrade to a yanked release (the
  channel's, or `--version`'s), to a version or revision not published on the channel, to one
  no key the OS's `<short>-stow` trusts signed, to one that does not come after the OS's
  release, to one published without an OS bundle, to one the install went back from, to one at
  another schema baseline, to one the boot into it would refuse (the boot's refusal, `…\nthe
  boot into <tag> would refuse this upgrade (it is given no input), so its OS is not written`,
  with the fix it has on the VM: a re-run of the install for what the install records —
  `sudo <short>-stow install --tls MODE`, `--set <input>=<domain>`, or `--cert FILE --key FILE`
  for a certificate that covers a route the release adds —, none where the Docker fix is a flag
  of the upgrade, such as a port or an input the release adds), and while a restore left the
  volumes half restored; an upgrade of an install whose kept `<short>-stow` is not the installer
  of the release it runs (another release's or revision's, none it reads, or none at all),
  whatever the order of that release and the OS's (`<dir>/<short>-stow is not the installer of
  <name> <v> (revision <r>), which the install runs: …`, no fix: an `upgrade --version` finds a
  version's latest revision, not the one the install runs); a rollback when the VM runs the
  passive system already, when there is no passive system or no previous release kept, when the
  install runs another release than the OS's (no fix), when the install is another project's or
  is kept by another installer name (no fix), while another
  lifecycle run holds the install's lock or a hook's or check's container an earlier run left is
  there, while the volumes are half restored, when the kept previous release is not one release
  (its installer, its record and its `.env` name others: look into the kept directory) or its
  installer does not read its own release, and when the rollback check says no (a rollback
  barrier since the upgrade, or a newer schema the previous release does not read); and on any
  verb a refusal of the Docker shape's that its handler runs (another run holding the install's
  lock), with the fix it has on the VM or none. A job running, a reboot scheduled, a boot not judged yet, a
  daemon that does not answer: failures (`1`). Once
  `upgrade`'s or `rollback`'s job started, anything that stops the verb is a failure, a refusal
  answered while it follows the job included. `boot-rollback` starts the database first, its
  first change, so the rollback's own check can read it: a directory holding no install exits
  3, and every refusal after the start (no previous release, a kept release that is not one
  release, a kept installer that carries another release or none, another schema baseline,
  volumes half restored, a rollback barrier since or a newer schema the previous release does
  not read) exits 1; with no database service, or when it finishes an upgrade or a rollback a
  reboot stopped (it starts nothing first), the rollback's refusals exit 3 as `rollback`'s.
  `boot` exits 1 on any refusal it raises, a boot changing the install from its first step (an
  install whose kept `<short>-stow` is not the installer of the release it runs says so, naming
  no way out), and
  on whatever an installer it runs exits with — but `boot-rollback`'s 3 when the OS fell back
  to the release the last upgrade replaced: the rollback check said no before anything changed,
  so the boot exits 3 too, claiming no change, unless the boot itself removed a mark before it
  (an upgrade's, or a new baseline's naming another release): it then exits 1. A refusal raised after a run's first change is a
  failure (1). A rollback (`boot-rollback`) whose start fails after the files went back exits
  1, whatever the kept installer's boot exited with, and names the directory the release left
  is kept in.
- The health gate: after the boot, `<name>-health.service` runs `<short>-stow health-gate` (as
  root, hidden from the help), which judges the boot by Kairos's boot assessment (GRUB images).
  An upgrade enables the assessment; GRUB, booting the active system with it enabled, marks the
  attempt (`enable_boot_assessment` and `boot_assessment_tentative`, both `yes`, in
  `/run/initramfs/cos-state/boot_assessment`), and a boot that finds the attempt marked boots the
  fallback entry — the passive system — with `upgrade_failure` on its command line
  (`/run/cos/upgrade_failure`). A boot of the active system with both set is on trial. The gate
  polls every `poll_s` until `deadline_s` (`shapes.kairos.health_gate`): the boot is healthy when
  `<name>.service` did not fail, the install runs the release this OS carries, and it is ready (as
  `health` judges). Healthy, a boot on trial is marked good (both cleared, the partition
  remounted read-write meanwhile); a failure to mark it fails the gate, as the next boot would
  fall back. Not healthy by the deadline (at once when `<name>.service` failed), the gate keeps
  diagnostics in `/usr/local/<name>/failed-boots/<UTC time>-<version>-r<revision>/`
  (`failed-boot.json` — the release, the time, why —, `boot.log`, the boot unit's journal of this
  boot, `ps.txt` and `logs-<service>.txt`, each service's last 500 lines; the last 5 are kept)
  and fails. On trial, it first runs the rollback the passive system's boot would run, dry: when
  the database refuses it (a rollback barrier since the upgrade, or a previous release that does
  not read the newer schema), falling back would leave a release that cannot start, so the gate
  keeps this system — marks the boot good, records why in `failed-boot.json` (`kept`), and
  `status` shows it under the failed boot (`kept on this system, as going back is refused: …`)
  — and does not reboot. Otherwise a boot on trial reboots, unless there is no passive system
  (`/run/initramfs/cos-state/cOS/passive.img`), and Kairos boots the passive system, whose boot
  goes back to the release the upgrade replaced. Off trial (a boot of the passive or recovery
  system, a boot after no upgrade, or an attempt GRUB did not mark) nothing reboots, so a failure
  never loops. A reboot before the gate marks a boot on trial good is a failed attempt too: the
  next boot falls back. The fallback sticks: every later boot boots the passive system, until an
  upgrade.
  With no install yet, the gate judges nothing, except on the OS a first boot switched to (its
  settings naming a newer release): its boot on trial is marked good, whatever `<name>.service`
  did, as the passive system holds no install to go back to and its boot would stop at the
  switch's mark (`<dir>.os-switch`). A first install waiting for what `vm create` gives over SSH,
  or failed, is then run again on this OS. Its first install unhealthy while no previous release
  is kept, it is kept as when going back is refused (with the mark removed, it falls back as any
  other). Each judgement is a `boot.judged` entry of the
  event journal (below).
- Every other verb asks the host daemon, `<name>-hostd.service` (`<short>-stow hostd`, root, hidden
  from the help; its hidden `--group`, default `admin`, names the socket's group, empty for root
  alone), on the unix socket `/run/<name>/host.sock`: the socket is `root:admin` 0660 in a 0750
  directory (`root` alone, 0600, on a host without that group, which the bundle creates). Each
  socket the daemon serves (this one and the app socket) is made as `.<socket>.<pid>` beside it,
  given its owner and mode, then renamed into place: it appears listening, already restricted, and
  a daemon stopping removes it unless a restarted one replaced it. Until
  `<name>.service` ran (its state is `active` or `failed`: before, the boot is queued, or is
  installing, upgrading or going back; a host without that unit has no boot to wait for), `upgrade`, `rollback`, `provision` and `restart`/`stop`/`start` are
  refused; `upgrade` and `rollback` also until `<name>-health.service` judged the boot (its state
  is `active` or `failed`). `restart`/`stop`/`start`, `reboot` and `shutdown` are refused while an install,
  upgrade, rollback or provision holds the install's lock — but `reboot` and `shutdown` are not
  while the boot runs, so a hung boot can be power-cycled (the next boot finishes what it
  stopped). The boot waits up to 10 minutes for an operator's run to release the lock. A verb
  needing the install answers that `sudo <short>-stow install` makes one until it exists — but
  `updates` and `upgrade`, below, which also serve a VM whose first install was refused.
- `updates` reads the channel's pointer in the install's registry (`--version V`: V's latest
  revision on the channel) with the pull account, refuses it unless it is signed by a key the
  OS's `<short>-stow` carries (when it carries any), and says whether it comes after the OS's
  release (by the scheme the release records; revisions order one version), with its changelog;
  a release the install went back from (`.stowage-failed/`) is not offered (`no upgrade on
  <channel>: the install went back from <release>, and the boot does not upgrade to it again: a
  new revision of <version> is the way forward`). On the passive
  system (a boot that failed went back to it), the release it runs is offered too (`rewrite` in
  JSON; `<release> on <channel>: the VM runs it from the passive system (a boot that failed went
  back to it); an upgrade writes it to the active slot again`): `upgrade` then writes its OS to
  the active slot, which held the release that failed, without a backup or the checks below, and
  its boot `runs it from the active slot again`. When the channel offers nothing to upgrade to
  (most often the release that failed, not offered again), `updates` adds `the VM runs the passive system (a
  boot that failed went back to it): `upgrade --version <v>` writes <v>'s latest revision (<v>
  (revision <r>) again when it is that one) to the active slot` (`passive` in JSON); when the
  install runs another release than the OS (a crossing whose boot fell back), it names that one
  instead (`the install runs <v> (revision <r>), which this OS (<v'> (revision <r'>)) does not
  carry: `upgrade --version <v>` writes its OS`; `installed` in JSON), and the passive system's
  own release is not offered. Before the first
  install (none made, or one refused), it reads the channel and registry of the first install's
  settings (`/etc/<name>/install.yaml`: `channel`, `registry` and `registry_ca`, else the
  release's registry), `--channel` naming one when they name none (`the install follows no
  channel: name one (--channel)`).
- `upgrade` shows the same, asks to confirm on a terminal (without one, `--yes` is required),
  and exits 0 without writing anything when there is nothing to upgrade to. With `--version V`,
  a release it refuses to write exits 3, refused (`upgrade --version V: refused, the install went
  back from <release>; publish a new revision of V to upgrade to it`, or `refused, <release> is
  not one this VM, which runs <release>, can be upgraded to: yanked, published without an OS
  bundle, or not after it`), the release the OS carries already aside
  (exit 0), so a script that asked for V tells "nothing to do" from "refused". To upgrade,
  the daemon runs `kairos-agent upgrade --source oci:<prefix>/<name>-os@<digest>` — the
  release's bundle by digest, pulled with the pull account given in a private Docker config, root's
  alone, removed afterwards — writing it to the passive slot, and reboots after 30 s
  (`--no-reboot`: the next boot runs it); the answer names the job's target. The job ends, `done`,
  as it schedules the reboot; a reboot that then fails leaves the job `done`, adds the failure to
  its log, and is journaled as a `reboot.cancelled` naming the job. The boot then upgrades
  the install. The job writes `/run/<name>/os-written` (the target's tag; a reboot removes it)
  before `kairos-agent` runs, and keeps it when the job fails (`kairos-agent` may have moved the
  running system to the passive slot already): until a reboot, another `upgrade` and `rollback`
  are refused (another upgrade would replace the running system's copy in the passive slot), and
  so are they when the mark cannot be read. When the boot assessment still marks an attempt (after a fallback or a
  `rollback`), the job clears the mark once the new system is written (`boot_assessment_tentative`
  emptied), so the next boot tries it on trial; a failure to clear it fails the job, without a
  reboot. It is refused for a
  yanked release, one that does not come after the OS's, one published without an OS bundle, one
  the install went back from, and one other than the release confirmed (published meanwhile).
  Before the job starts it fetches the target's installer (`<prefix>/<name>-installer@<digest>`,
  by the digest of its signed manifest, the payload checked to carry that release; a copy under
  `<dir>/.stowage-next-*`, removed after), reads its spec without running it, and checks the
  upgrade's front and inputs as the boot would, given no input: a route it could not decide, or an
  input the release adds that is required with no default, is refused then (the L4 routes' ports
  are still the boot's to check) (409: `<why>\nthe boot into <tag>
  would refuse this upgrade (it is given no input), so its OS is not written`), and so is a release
  at another schema baseline, read from that installer whichever Stowage built it (409: `<name> <v>
  starts schema baseline <n>, and the install runs <name> <v'>, at baseline <m>: its data cannot
  carry over, so its OS is not written; `<short>-stow reset-db --new-baseline` wipes the data and
  installs <v>`), rather than by a
  boot that would fall back; an installer that cannot be fetched is a 502 (`the installer of
  <tag>: …`); a spec this daemon's Stowage does not read is logged and left to the boot (`the
  installer of <tag> carries a spec this Stowage does not read (…): the boot into it checks the
  upgrade`), its baseline checked all the same. An upgrade removes the mark a `reset-db
  --new-baseline` left whose OS never booted (`/usr/local/<name>.new-baseline`, below).
  Before the first install, it reads the release as `updates` does, takes no backup, records the
  release's tag in `/usr/local/<name>.os-upgrade` before `kairos-agent` runs (removed once the
  first install is made: any boot finding an install removes it), refuses a `--channel` other
  than the settings' one (`the first install follows <channel> (its settings)`) and an operator's
  first install still running (`an install is running: wait for it to finish`); the boot into
  it (`the boot makes the first install`) makes the first install of that release from the
  settings: a boot on the OS of the release that file names does not write the OS of the
  release the settings' `release` names (it logs that `upgrade` wrote this OS's release since).
  So a release fixing what a first install was refused for (a revision of the same version
  included) reaches the VM without making it again; a Docker host's daemon answers `not
  installed yet`.
  `<short>-stow` prints the job's log as it runs (a carriage return ends a line too, a line is cut
  every 4 KiB, a blank line is kept; the daemon keeps at least a job's last 2000 lines), and fails
  when the job fails or when the daemon's job is another (`status` then tells how it went). Every
  verb that starts a job (`upgrade`, `rollback`, `reset-db`, `backup run`, `backup verify`,
  `restore`, `secrets rotate`, `secrets set`) follows it this way, telling it from any other by the `started`
  its 202 carries — another job of the same kind and target is another job. Between two reads of
  the log it waits on the event journal from the 202's `events`, so a job's end is seen at once,
  and read from the journal when reading the job again then fails in any way (the daemon gone,
  an error answer, an answer that does not decode — the daemon stopping for the job's reboot), or
  when the next job (the timer's backup, woken by that end) took the slot before it: a job done
  answers as done (with its `result`), a job failed fails with its error, and the verb says `the
  <kind>'s last log lines were not read: its end is the journal's (<short>-stow events)`. A
  `job.lost` read is likewise the answer, whatever reading the job again then meets. A job the daemon
  no longer holds and whose end the journal does not hold fails the verb: `<kind> <target> lost: <reason>` when the journal's `job.lost`
  names it (the daemon restarted), else naming the job the daemon holds, or none. A job that ends
  `done` having scheduled its reboot or power-off (an upgrade's, a rollback's: `GET job`'s, or its
  `job.ended`'s, `reboot`) ends the verb there — the verb runs on the VM, which that reboot
  stops, so it never follows the reboot. It prints `<kind> done: the VM reboots in <drain> s to
  run it — after it, <short>-stow status and <short>-stow events tell how it went`, exits 0 and asks the
  daemon nothing more. The drain is counted down only when `GET job` told the end and the drain
  is not 0 (a rollback's is): otherwise the line says `the VM reboots to run it`, whatever the
  next request met (the daemon's absence never tells a reboot under way). A power-off says
  `the VM powers off in <drain> s, and its next start runs it — after that start, …` (`powers
  off` likewise). The reboot's outcome is the event journal's: another boot's
  `boot.started`, a `reboot.cancelled` naming the job (`systemctl reboot` failed, or the daemon
  restarted in the drain), the health gate's `boot.judged` — on the VM, read after the boot with
  `<short>-stow status` and `<short>-stow events` (the journal keeps its entries across the reboot). The
  host API is served only on the VM (its operator socket, and the app socket mounted into the
  stack's services `host_api` names): such a service, and what it serves to clients off the VM,
  reads `GET /v1/events` again once the VM is back, from the number it held. Each request the follow sends is given its wait and 2 minutes to be
  answered, then counts as the daemon not answering, so a daemon that takes a request and never
  answers cannot hold a verb. A verb stopped by a signal answers from the job's end when it read
  it: a job failed fails the verb with its error, a job done exits 0 (with the line above when it
  scheduled a reboot); a job lost it read fails with its reason. Otherwise, interrupted (Ctrl-C,
  SIGTERM, SIGHUP, SIGQUIT), a verb fails (`interrupted before …`) and never reports the job as
  done: `status` and `events` tell how it went. So a job-following verb exits 0 when its job ends
  `done` (a reboot it scheduled included), and non-zero when the job failed or was lost; when
  the daemon holds another job or none and the journal does not hold the job's end; when, before
  the job's end or loss was read, the daemon cannot be reached, a request's bound runs out, or
  `GET job` answers an error or an answer that does not decode; when `GET job` answers a state
  other than `running`, `done` or `failed` (`the job is <state>`); or when the follow was
  interrupted. A 202 whose `events` is `null` (its start could not be journaled) is followed by
  the job's log alone, never the journal, which the verb says first (`the event journal does not
  hold this <kind>'s start (<events_error>): …`): a job's end is then only `GET job`'s, and a
  daemon gone during the job fails the verb.
- `rollback` goes back to the release the last upgrade replaced by booting the passive system,
  which carries it: the daemon checks that the VM runs the active system (refused on the passive
  one: there is no earlier one, and on recovery), that the install runs the release this OS
  carries, that a passive system and a previous release (`.stowage-prev/`) are kept, that no
  upgrade began to write a system no boot ran yet (`--no-reboot`, or an upgrade that failed:
  the running system may be the passive one now; see `os-written` under `upgrade`), and runs the
  install's rollback dry (`rollback --dry-run`'s checks: refused when the database carries a
  rollback barrier since the upgrade, or moved past a release that does not read a newer schema,
  naming the snapshot to restore instead). `<short>-stow` shows the release the passive system
  carries, asks to confirm on a terminal (without one, `--yes` is required) and follows the job:
  after 30 s it marks the boot assessment as after a failed attempt (both variables `yes`), so
  GRUB boots the fallback entry from then on, and runs `systemctl reboot`; the boot of the
  passive system takes the install back. When the marks cannot be written, they are put back as
  they were and the job fails. Once they are written the job ends, `done`, before `systemctl
  reboot` runs; when the reboot then fails, the job stays `done` and the marks are put back as
  they were. The journal's `reboot.cancelled` names the job, and its `reason`, like the job's log,
  says whether putting them back worked: `systemctl reboot failed: <error>: the boot assessment is
  put back as it was, nothing changed`, or `systemctl reboot failed: <error>; putting the boot
  assessment back failed too, so the next boot may choose the passive system (<grubenv>):
  <error>`. `rollback` itself answers from the job's end (exit 0 once it read it), so this is read
  with `<short>-stow events`. After the 30 s, the checks run again
  and the install's lock is taken and held to the reboot: a check that no longer passes, or an
  install, upgrade, rollback, provision, reset-db or service action holding the lock, fails the job
  before anything is written. It is refused for a release other than the one confirmed.
- `health` answers whether the install is ready, as the health gate judges it (every service up,
  the health provider's answer); not ready, it fails with the reason. A probe of a service (the
  `http-json` and `http-status` health providers, the disk-pressure POST) reaches it at its
  address — IPv4, else IPv6 (a network without IPv4) — on the first network by name that gives
  it one and is not `internal: true` (the host
  has no route into an internal network); a service on internal networks alone is not ready.
- `secrets rotate` is the Docker shape's three passes, run by the daemon as a job (`rotate`)
  once the health gate judged the boot (its restarts could fail the gate's deadline); `<short>-stow`
  follows it. `secrets set` is the Docker shape's too: `<short>-stow` reads the value as Docker's
  does and sends it to the daemon (`POST /v1/secrets/set`, the operator's socket alone), which
  refuses (400) a key that is not settable and a value Docker's refuses, then records it in a job
  (`secret`) once the health gate judged the boot, followed the same way. `secrets export` is
  the Docker shape's, the daemon writing the encrypted file it answers (and what the export said,
  a front CA it could not read, which `<short>-stow` prints on standard error). `secrets export` and
  `secrets set` refuse (409) an install running another release than the OS's, whose spec
  names the secrets.
- `exec [--stdin] SERVICE -- COMMAND [ARG…]` runs a command in the service's running container through
  the daemon (`POST /v1/services/<service>/exec`, `{"args": […], "stdin": "<base64>"}`, the
  operator's socket alone; it answers `{"stdout", "stderr", "exit"}`, each output base64 as JSON
  carries bytes, and `"stdout_cut"` / `"stderr_cut"` true when one was cut),
  as `docker compose exec -T`: no terminal; with `--stdin` (`-i`) the command reads
  `<short>-stow`'s standard input (`… | sudo <short>-stow exec -i api -- tar -x -C /data`), read
  whole before the command starts and at most 4 MiB (`--stdin: the input is over the 4194304
  bytes an exec takes: …` above, and the daemon answers 400 to a larger `stdin`, `stdin: <n> bytes, over the 4194304 an exec takes`, and 413 to a body past
  its base64, `the request body is over the <n> bytes it may be`), else none (an
  empty input); its standard output and error
  are printed once it ends, each cut at 4 MiB (said on standard error), and its exit status is
  `<short>-stow`'s. An unknown service is 404, no command 400. As on Docker it takes no lock: it
  runs beside a job (an upgrade, a restore) as `docker exec` would. Interrupting `<short>-stow`
  stops the `docker compose exec`, not always the command in the container.
- `restore OBJECT` loads a backup into the install's database (the VM's destination and rclone
  config, so backups must be configured): `<short>-stow` reads the age identity that decrypts it
  from standard input (`--identity FILE` instead), asks to confirm on a terminal (without one,
  `--yes` is required) naming the release the install runs, which the request carries, and
  follows the job. For a backup with `volumes` it is refused (409, `reboot first, then restore`)
  while an upgrade's system waits in the passive slot for its boot. The daemon refuses (400, 409) a request without that release or with
  another, an install running another release than the OS's, an object not named as this
  project's backups are (`<name>-<profile>-<UTC time>-<version>-r<revision>.pgdump.age`, or
  `….manifest.age` for a `volumes-age-rclone` backup, the suffix the release's provider gives; no
  path), a backup of another release (a backup restores onto the release whose schema it
  holds), a backup of a profile that does not keep every table's rows (a table list, or an
  `exclude`; one a later release dropped too: the restore replaces the whole database, and
  such a dump holds every table's structure, so the rows it left out would be lost —
  the refusal names the profiles to restore from), an identity that is not one (never echoed), and every verb's refusals until the health
  gate judged the boot. The job takes the install's lock, reads the install again (refusing
  another release, and, before anything is stopped, a provision or schema container an earlier
  run left), refuses, before anything is stopped, a backup missing one of its volumes' archives
  (`has no archive of the volume V to restore with it`), stops every service but the database
  (none to stop: nothing), extracts each volume's archive into a directory of its own at the
  volume's root (`.stowage-restore`, made afresh with the root's owner and mode: it and
  `.stowage-replaced` are names the stack's own data must not use, never extracted from an
  archive; a volume not there is made as the compose file declares it —
  driver, `driver_opts`, labels, and Compose's labels — refused when those hold a `$`),
  streams the object out of the destination (the backup runner's `rclone cat`), decrypts it as
  it streams, and loads it beside the database, never into it: as the migration role — which
  owns the database, may create one (`CREATEDB`) and `SET ROLE` to every owner the dump names, as
  a stack's own Postgres superuser does — through the maintenance database (`postgres`, or
  `template1` for a database named `postgres`), a new database `stowage_restore` (a fixed name)
  made as the stack's Postgres made the database (from `template0`, its owner, encoding and libc
  locale; nothing set at the database's level — `ALTER DATABASE … SET`, its grants — is carried,
  as a stack keeps its configuration in its schemas) takes the archive by `pg_restore
  --single-transaction --exit-on-error`, owners and privileges kept (every role the dump names
  must exist: a role made by hand on the old database is made again first, or the restore fails
  naming it). The load never touches the install's database: whatever ends it — a failure, the
  job stopped — the database is as it was (a load that fails drops its database; one left behind
  is dropped by the next restore, which says so). Only once it loaded, and while the job still
  waits, does a second step end the database's sessions (those the migration role may end) and
  swap the two by renaming them in one transaction (under a short timeout of its own, which
  neither the job's deadline nor the daemon's stop cuts, and bounded inside the database's
  container by Postgres — connect, lock and statement timeouts on every step, both legs together
  well within it — so nothing of it still runs when the job reads how it ended; any end but success is judged by whether
  `stowage_replaced`, which only its rename makes, exists: there, it committed and the restore
  goes on; if that cannot be read, the job removes the volumes' staging and says that a
  `stowage_replaced` means it committed, that it holds the data from before the restore, and to
  restore again, which loads and swaps afresh whether it committed or not, dropping it), then, once the stack is up again and provisioned, drop the replaced one (its own
  ten minutes, bounded the same way)
  (`stowage_replaced`; if that drop fails the restore is still done, says so, and the next
  restore drops it); a swap that fails drops the loaded copy (one the job could not see fail is left for the next
  restore to drop). A restore that ends before the swap removes the volumes' staging directories:
  their data is as it was. Once the database is swapped in, each volume's data is replaced by
  what its staging directory holds, by renames alone: the volume's entries move into
  `.stowage-replaced` (made afresh), then the staging's into the root, which takes the archive's
  owner and mode — each volume within ten minutes, every one tried. The volumes that could not
  be are the job's error, naming them (`restore again`): each holds its files split between its
  root, `.stowage-restore` (the backup's) and `.stowage-replaced` (its own), the services stay
  stopped (they would run, and be backed up, on data of two times) and the install still awaits
  a restore. From the database's swap until every volume is in, `<dir>/.stowage-volumes-split`
  marks the install (a swap whose outcome cannot be read keeps it too, with the staging): while
  it is there nothing brings the stack up, writes the data or moves the release — the boot, a
  re-run, `secrets rotate`, `secrets set`, `start` and `restart` (409), `provision`, `reset-db`, `upgrade` and
  `rollback` refuse before changing anything, saying so; the stopped services' containers are
  removed (`compose rm --stop --force`), so no restart policy starts them at a reboot —, no backup is taken (409, an upgrade's first one
  failing), and `status` says `not running` (on Docker `… (or a restore is running)`; the VM's
  daemon leaves it out while its restore job runs). Only a restore again, onto the release the
  install runs, removes it once it finishes: it brings the database's service alone up first
  (`up --detach --wait --no-deps`), and any end short of finishing keeps the mark. Once the stack is up and provisioned, the `.stowage-replaced` directories are
  deleted, each within ten minutes, cut by the daemon's stop (one that is not is logged; the next
  restore's staging deletes it, and what an earlier restore left of `.stowage-restore`). An install that awaited a restore stops
  awaiting it only once the database and every volume are in. If the stack does not come up
  after a swap, the job says so and names `provision`. A database named `stowage_restore`, `stowage_replaced` or a template is
  refused. Nothing is dropped object by object, so partitioned tables, large objects and
  extensions come back as the dump holds them, and nothing the replaced database held stays. Then, whatever happened, the stack comes up again; after a
  restore that swapped the backup and every volume in, the install no longer awaits one (`.stowage-restore-awaited` goes) and
  the provision hook runs on the restored data (a failure there is the job's, naming `sudo
  <short>-stow provision`). The identity is kept nowhere.
- `reset-db` is the Docker shape's `reset-db` on the install, run by the daemon as a job without
  a reboot: the stack stops, the `data.reset` volumes are deleted (`keep_on_reset` ones and the
  front's certificates stay), the provision hook's record is forgotten, then the stack starts and
  is provisioned as on a first install (`--no-provision` skips the hook). `<short>-stow` asks to
  confirm on a terminal, as the Docker shape asks (without one, `--yes` is required), once the
  daemon's checks passed, and follows the job. The daemon refuses it (409) until the health gate judged the boot, when the install
  runs a release other than the OS's, during another job, and while a reboot is scheduled; the
  job fails on the Docker shape's refusals. The question names the release; the request names
  it, and another is refused, and carries the plan's single-use token. Nothing is dumped first. The app socket serves it too,
  as the whole API.
- `reset-db --new-baseline` crosses a schema baseline on the VM: it finds the release as
  `upgrade` does (`--channel`, `--version`, refused without `--new-baseline`; `--no-provision`
  is refused with it, as the boot that crosses is given no flag: `provision` runs the hook
  afterwards), which must come after the OS's, carry an OS bundle,
  not be yanked, start another baseline than the install's — both told, read from the two
  installers (`… cannot be told (the daemon's log says why): --new-baseline never wipes on a
  guess`; the same baseline: `… --new-baseline only crosses a baseline; `<short>-stow upgrade`
  moves it`) — and pass `upgrade`'s checks given no input. The question names that release and
  the plan (`write <tag>'s OS and reboot into it; that boot stops <name>, deletes the volumes
  data.reset names, forgets what its provision hook returned, then installs <v>, at schema
  baseline <n> (from <m>), with the same values`, naming the volumes of both releases it deletes
  and those it keeps). A release the install went back from is refused (`remove
  <dir>/.stowage-failed to cross to it again`). The POST takes the check of the GET that showed the plan when the
  target's installer and the install's `.env` are unchanged, rather than fetching the installer
  again (the check of that GET alone: its token). The job, as `upgrade`'s, records the tag in
  `/usr/local/<name>.new-baseline`, writes the OS and reboots; the boot on that OS runs the
  Docker shape's `reset-db --new-baseline` (no dump, no previous release kept),
  logging `… which reset-db --new-baseline asked: wiping the data and installing it`. A mark
  naming another release is removed by the next boot (its OS was not written, or the health gate
  went back from it); one naming the OS's release stays until the crossing is made, so a boot
  whose crossing failed tries it again (nothing else can ask it from that OS). Should the new
  release fail its health gate, the VM falls back to the passive system with the data already
  wiped: its boot, finding `.stowage-crossed` naming a crossing from its OS's release to the
  install's, refuses (`the install runs <name> <v> (revision <r>), after this OS's <v'>
  (revision <r'>), and keeps no release to go back to (a reset-db --new-baseline crossed to it):
  `<short>-stow upgrade --version <v>` writes its OS again`), and that `upgrade` checks no
  baseline when it writes the release the install runs; to another release it checks the
  baseline of the release the install runs (its own installer's), not the OS's, and across one
  names `upgrade --version <v>` (the install's release back first: `reset-db` resets the OS's
  release), then `reset-db --new-baseline`. A later OS
  boots as an upgrade like any. The plan resolves what that boot would wipe (both releases' `data.reset`) before the OS
  is written, and refuses one it cannot tell.
- `network` shows the interface (that of the default route, else the first `en*`/`eth*`), its
  MAC, its global IPv4 and IPv6 addresses (static, leased or from router advertisements; an IPv6
  one once duplicate address detection passed), the default gateways (IPv4's, then each IPv6
  default route's on that interface, static or advertised) and the DNS servers in use
  (`resolvectl`), and what sets them: `network set`'s override (`set by     \`<short>-stow
  network set\`:` and its settings — `DHCP` or the address via its gateway, then the IPv6
  address via its gateway, `automatic IPv6` or `no IPv6`, then the DNS servers), the network
  `vm create` gave (its cloud-config's file; with its settings, which the cloud-config also
  records in `/etc/<name>/install-network.yaml`, rewritten at every boot — a record that does not
  read is logged, once per error, and left out), or DHCP (a VM whose cloud-config wrote none).
  Then a `role` line for each of the release's secondary NICs (`role       <role>: <MAC>
  <addresses, or no address>; set by …` — `no NIC is named so` when the VM names none — set by
  `` `<short>-stow network set --role <role>`: `` and the override's settings, or by `the
  cloud-config or \`network attach\``), and the change awaiting confirmation, a secondary's
  named with its role (`roles` and `pending.role` in `--json`).
- `network attach` (as root; `vm attach` runs it) names and addresses a NIC the VM's
  cloud-config does not name, for a secondary role of the release's `host.interfaces` (`kind:
  nic`), from the next boot: by its `--mac` (a unicast MAC), with `--mode` `dhcp`, `static` with
  `--address` (IPv4 with its prefix) or `none` (the default: the link up, no address), as `vm
  create --interface` addresses a secondary. It records the NIC in `<dir>/interfaces.yaml` and
  writes `/oem/96_<name>-interfaces.yaml`, a cloud-config every boot runs after the seed's: its
  `/etc/systemd/network/09-<name>-<role>.link` (by MAC; 09- sorts before the seed's 10- files) in
  the initramfs stage, before udev names the NICs, and its `10-<name>-<role>.network` at boot
  (the release's MTU and routing drop-ins apply to it). A NIC the seed named by its original name
  alone (a VM made by a release with one NIC) is named by its MAC too, the MAC of the NIC its role
  names now (`<role>: its NIC (<MAC>) is named by its MAC from the next boot, as another NIC joins
  it`), so the new NIC never takes its name. Given again for a role, it replaces the role's MAC
  and addressing (applied at the next boot, with no window: `network set --role` re-addresses
  with one, and its override outranks this addressing). Refused: a role that is not one of the release's
  NICs, the primary (`network set` addresses it), a role the cloud-config names, a MAC another
  recorded NIC has, that the cloud-config names another NIC by, or that a NIC named as another
  role has, `--address` without `static` or `static` without an IPv4 one, a mode other
  than the three, a seed's NIC to pin that no NIC is named for now, and a run not as root. The
  install's warning about a role no NIC is named for counts these NICs once a boot named them (a
  NIC recorded and never attached names none: the warning stays).
- `network set` sets an override that outranks the cloud-config's network, as a whole (a setting
  left out is not kept from before): `--mode static` with
  `--address` (IPv4 with its prefix, 1 to 32) and an optional `--gateway`, or `--mode dhcp`
  (DHCPv4); `--ipv6` turns IPv6 on, in either mode — without it the link has no IPv6
  (`LinkLocalAddressing=no`, `IPv6AcceptRA=no`, and `DHCP=ipv4` with dhcp). With it, networkd
  takes the router advertisements (SLAAC and the IPv6 default route) and, with dhcp
  (`DHCP=yes`), DHCPv6, its client started at once rather than on a router advertisement's
  managed flag (`[DHCPv6] WithoutRA=solicit`): the clouds (AWS, Azure, Google Cloud) give the
  address by DHCPv6. With IPv6 on (here or from `vm create --ipv6`), Stowage keeps the primary
  link soliciting the routers while it has no IPv6 default route — networkd stops its router
  solicitations at the first advertisement of any router, a router lifetime of 0 (a router that is
  not a default one) included, so one lost answer of the default router would leave the link
  without a default route until that router's next unsolicited advertisement:
  `<name>-route6.service` (`<short>-stow route6-boot`), once the network is online and before
  Docker starts, waits up to 60 s for the IPv6 default route, sending a router solicitation on the
  primary link at 1 s, then 2, 4, … s apart, and past the 60 s warns in its journal (`warning: no
  IPv6 default route on <link> after 1m0s: the services start without one; …`, or `warning: the
  routes did not read for 1m0s (…)` when `ip` failed throughout) and lets the boot
  go on; the host daemon then keeps soliciting, every 1 s doubling to an hour (RFC 7559), while
  the route is missing (the route checked at each solicitation and every 30 s, whether it is there or not; a failed read of the routes is logged
  once and neither starts nor ends a stretch; IPv6 turned off ends one, its warning cleared), and a minute without one is a warning in
  `status` (`network    warning: no IPv6 default route on <link> since <time>: …`,
  `"network_warning"` in JSON) and one `network.no_ipv6_route` event for the stretch; the
  warning clears once a route is back (logged). Also with it,
  `--address6` (a static IPv6 address with its prefix, 1 to 128, beside the advertised ones)
  and an optional `--gateway6` (link-local, such as `fe80::1`, or in the address's subnet; /127
  and /128 reach any gateway on the link); `--dns` (at most 4, IPv4 or IPv6, duplicates dropped) in either mode, and with `dhcp` in place of
  the lease's; given in either mode, they replace DHCPv6's and the router advertisements' too. Refused before anything changes: another mode, an address or gateway with `dhcp`,
  no address with `static`, `--address6`, `--gateway6` or an IPv6 `--dns` server without
  `--ipv6`, an address with a zone, a loopback, link-local, multicast,
  unspecified or 240.0.0.0/4 address, an IPv6 `--address` or `--gateway`, an `--address6` that is
  not IPv6 (an IPv4-mapped one included), loopback, link-local or multicast, or up to /126 its
  subnet's anycast address (all zeros after the prefix), a `--gateway6` without `--address6`, not
  IPv6, with a zone, multicast, outside the subnet up to /126 (unless link-local) or equal to the
  address, up to /30 the subnet's network
  or broadcast address or a gateway outside the subnet (/31 and /32 reach their gateway on the
  link: networkd is given an on-link route to it), a gateway equal to the address, and a change while another awaits confirmation or once
  a reboot is scheduled. The override is recorded in `/usr/local/<name>/network.yaml` (0600, on
  the persistent partition, strictly read), rendered as
  `/etc/systemd/network/01-<name>-override.network` (`Name=en* eth*`; sorted first, so networkd
  takes it), then `networkctl reload` and `networkctl reconfigure` of the interface; with static
  addresses, each address (an IPv6 one past duplicate address detection: one another host has
  never does) and the default routes via the gateways must reach the interface within 10 s. A change that fails is undone at once and reported (the settings before it rendered and
  applied again).
- An addressing change (mode, `--ipv6`, an address or a gateway, IPv4 or IPv6; any change from no override) awaits
  confirmation: unless `network confirm` keeps it within `--revert-secs` (default 90, 30 to
  3600; a confirmation that comes once the window's timer fired is refused, 409, and the change
  undone; `network confirm` with nothing awaiting confirmation fails, saying that a change not
  confirmed in time is undone), the settings before it come back — at the window's end, when the host daemon restarts
  inside it (no network request runs before that; a record the daemon stopped before applying is
  applied then), and at a reboot inside it (`<name>-network.service` runs `<short>-stow network-boot`
  before networkd starts, rendering the override and undoing a change never confirmed). A
  DNS change alone, or `--no-revert`, applies with no window. `network revert` undoes the change
  awaiting confirmation now (refused when none does). A state file that does not read (unknown
  key, invalid setting) sets nothing at boot, fails `network` and `network set` naming the file,
  and `network reset` drops it, with no window. The file carries `version: 2` when IPv6 is off or a static IPv6
  address is set — in the override, or in the settings a change awaiting confirmation would bring
  back; without a version (IPv6 on everywhere, no static IPv6 address — the form a release older
  than `--ipv6` writes and reads) it is read with IPv6 on, and one holding an `ipv6`, `address6`
  or `gateway6` key, or a version other than 2, does not read. A release older than `--ipv6` reads an
  override with the version as such a file: after `rollback` to it, the
  whole override (its IPv4 addressing too) is dropped at boot and the cloud-config's network
  comes back — before rolling back to it, `network set --ipv6` without `--address6`, then `network confirm`
  (the settings before a change awaiting confirmation count too), whose record it reads. A revert that fails is tried again every
  15 s, 5 times in all; a later change stops it.
- `network reset` drops the override, back to the cloud-config's static address or DHCP, on the
  same window (the override comes back unless confirmed); with no override it changes nothing.
- `network set --role ROLE` sets a secondary NIC's override, on the release's secondary NIC
  (`host.interfaces`, `kind: nic`, not `primary`) of that role, which the VM names (by its
  cloud-config, `vm create --interface`, or `network attach` once a boot named it), whatever set
  its addressing before: `--mode dhcp` (DHCPv4, never its gateway, routes, DNS servers or the
  host name), `static` with `--address` (IPv4 with its prefix, checked as the primary's) or
  `none` (the link up, no address) — as a whole, never a gateway, IPv6 or DNS servers, which are
  the primary's. It is recorded in `/usr/local/<name>/network-<role>.yaml` (0600, strictly read,
  `version: 2`) and rendered as `/run/systemd/network/09-<name>-<role>-override.network`
  (`Name=<role>`; it sorts before the cloud-config's and `network attach`'s `10-` file, and the
  release's MTU and routing drop-ins apply to it; under `/run`, a release older than `--role`
  renders none: after `rollback` to one, the cloud-config's or `network attach`'s settings
  apply, and the record is kept for a later release), then `networkctl reload` and `networkctl
  reconfigure <role>`; a static address must reach the link within 10 s. It follows the
  primary's rules otherwise: the window (`--revert-secs`, `--no-revert`, `network confirm`,
  `network revert`, undone at the window's end, at the daemon's start and at a reboot inside
  it), a change that fails undone at once, and `network reset --role ROLE` back to the settings
  the cloud-config or `network attach` gave the role. One change awaits confirmation at a time,
  across the primary and the secondaries. Refused before anything changes: the primary's role
  (`network set` without `--role`), a role that is not one of the release's secondary NICs, a
  role no NIC is named for on the VM (409: `vm attach` attaches one), a gateway, IPv6 or DNS
  server, an address without `static`, `static` without one, another mode, and any NIC's change
  while a revert is armed for another NIC's (at its window's end, or tried again while it
  fails: `<NIC>'s network change has a revert armed`); a change awaiting confirmation names
  its NIC (`<role>'s network change awaits confirmation`). A record that does not read, the
  primary's or a secondary's, sets nothing at boot and stops every change, `network confirm`
  and `network revert` (it may hold the change awaiting confirmation), each naming the `network
  reset [--role ROLE]` that drops it, which goes ahead whatever another NIC does (with no window,
  another NIC's revert kept); `network` shows a secondary's on its role's line (`as its
  override's record does not read: <why>`; `error` in `--json`). A record of a role the
  release no longer names is kept and not read.
- The console's login screen (`/etc/issue.d`, after the host name and `IPv4: <address>`) tells
  what sets the network (`network: <the override's settings, as \`network\` prints them>; set
  with \`<short>-stow network set\``; else "the VM's cloud-config (DHCP, or its static
  address)"), each secondary NIC's override (`<role>: <its settings>; set with \`<short>-stow
  network set --role <role>\``) and, while a change awaits confirmation, when it goes back
  (`06-<name>-network.issue`, written at boot and at each change, then `agetty --reload`); the
  cloud-config adds the connect line.
- Disk pressure (`hooks.maintenance.disk_pressure`; the VM only): the daemon reads the use of the
  persistent partition (the filesystem of `/usr/local`, which holds the install and Docker's
  volumes: used of used and available, as `df` counts it, the blocks reserved for root in
  neither) as it starts and every 10 minutes — 1 minute after a check that could not read the
  disk, or whose POST was not sent for want of the install or the service's address (at boot the
  daemon starts before the stack is up), got no answer, or was answered 5xx. At
  `soft_pct` or past it — `soft`, or `hard` at `hard_pct` or past it — it POSTs
  `{"level": "soft", "used_pct": 80.3, "soft_pct": 75, "hard_pct": 90}` (`application/json`;
  `used_pct` rounded down to a tenth) to `post`, at the service's address on the stack's network
  with `<service>:<port>` as the `Host` (no proxy, no redirect followed; 30 s for the lookup and
  the request), with `Authorization: Bearer <value>` when
  `auth_secret` names a secret; below `soft_pct` it sends nothing. It sends nothing either before
  the first install, while the service has no container (the stack down, an upgrade), or when the
  install has no value for `auth_secret`. Each POST is logged to the daemon's journal (`journalctl -u <name>-hostd`), never the token. What to
  do about it (purge, shorter retention) is the service's. `status` prints `pressure   <level>:
  <used>% used (soft <soft_pct>%, hard <hard_pct>%)`, the use read now (`unknown: the disk cannot
  be read` when it cannot), and the last POST (its time, level and use, and the answer's status —
  with the first 512 bytes of a non-2xx answer, unprintable characters as spaces, when no
  `auth_secret` is set: with one, the status alone, as an answer may echo the token — or why none
  was sent, the token shown as `<auth_secret>` should an error quote it); `status --json` gives them as `disk_pressure`, beside `disk` (`total_bytes`,
  `available_bytes`, `used_bytes`). Past `hard_pct`, the
  console's login screen says so (`08-<name>-disk.issue`, then `agetty --reload`), until a check
  finds the use below it; a release without the hook removes that banner as the daemon starts.
- A `volumes-age-rclone` backup (a project without a database) is a `pg-age-rclone` one without
  the dump: each of its `volumes` is archived as below, then a manifest — `{"project", "release":
  {"version", "revision"}, "volumes"}`, JSON, encrypted to the same recipients — is uploaded as
  the backup's last object, in the dump's place (the volumes are read live, as below: the whole
  backup is then that live read, which suits what the stack writes once or rarely — a CA's key,
  a VPN node's identity, an operator's edits — and a file changing as it is read fails the
  backup; files written together, such as a CA's index and serial, may be archived between two
  writes):
  `<destination>/<name>-volumes-<UTC time>-<version>-r<revision>.manifest.age`, each archive
  `<the same name, .manifest.age left out>.<volume>.tar.age`. `backup list` lists the manifests
  (and a `pg-age-rclone` project's dumps), retention and the archives' pruning go by them as by
  the dumps, and its settings' one profile is `volumes`. `restore` of one decrypts the manifest
  first, before anything stops, and refuses (409, before the job: an object ending in
  `.pgdump.age`, `… is not a volumes-age-rclone backup, which <name> takes: its backups end in
  .manifest.age`, or of a profile other than `volumes`, `… a volumes-age-rclone backup's profile is
  volumes`; then, in the job: `… is not a backup's manifest`, `… is the manifest of
  the volumes V of P R, not of the volumes …`, or the identity that does not decrypt it: `nothing
  restored`) a manifest that is not exactly one JSON object of this project, this release and the
  release's volumes in their order (64 KiB at most); it then stops every service (there is no
  database to keep up), stages and swaps the volumes in as below — no database is loaded or
  swapped, the volumes are the whole backup, so `.stowage-volumes-split` marks the install from
  the first volume's swap — and brings the stack up and provisions it. A machine made again with
  `--secrets` awaits it as it awaits a dump.
- Backups (a `pg-age-rclone` or `volumes-age-rclone` `backup`): the daemon runs `pg_dump --format=custom --compress=9`
  (owners and privileges kept: the stack's roles exist on every install) as `database.roles.migrate` inside the database service (`docker
  compose exec`: the server's own version), encrypts the dump with age as it streams, to every
  recipient of the settings (any one's private key decrypts it; the VM holds none), and pipes it to
  the backup runner — rclone 1.76.0, preloaded, run with `docker run --rm --pull never --network host
  --cap-drop ALL --cap-add DAC_OVERRIDE --cap-add DAC_READ_SEARCH --security-opt
  no-new-privileges:true` (those alone: it reads and writes its configuration, whoever owns it —
  root on the VM, the operator running a Docker install's backup; the runner's shell, which archives and restores volumes, keeps
  `CHOWN`, `DAC_OVERRIDE`, `DAC_READ_SEARCH`, `FOWNER`, `FSETID` and `MKNOD` alone, for files of
  every owner, their modes, set-id bits and device nodes)
  (it reaches the destination, and names it, as the VM does — over IPv6 too), the settings' rclone config mounted from a directory under `/run/<name>/`
  (root's, removed afterwards) — which uploads it with `rclone rcat` to
  `<destination>/<name>-<profile>-<UTC time, 20261010T030000Z>-<version>-r<revision>.pgdump.age`;
  nothing is written to the VM's disk. Before the dump, each of the backup's `volumes` is
  archived the same way beside it, as `<the same name>.<volume>.tar.age`: a tar of the volume's
  files (owners and modes kept), read while the stack runs by the runner's own `tar`, the volume
  mounted read-only and the container on no network, a restore's `.stowage-restore` and
  `.stowage-replaced` left out
  (Docker's volume of the compose project, found by Compose's labels, else by the name Compose
  gives it when it carries no other project's labels — another project's is refused; one never
  created archives as empty). A volume's files are read live, unlike the dump's snapshot (a file changing as it is
  read fails the backup, which says so): the volumes suit what a
  stack writes once or rarely (minted keys and certificates, a service's own state between
  writes). The dump is the backup's last object: a backup that fails deletes every object it
  sent, and `backup list` lists the dumps alone. Bytes are the whole backup's. A profile listing tables keeps the data of those tables
  only (every table's schema is kept: the other tables' data is excluded with
  `--exclude-table-data`, quoted, so names are taken literally); a listed name no table has is
  logged. A profile `{ exclude: [pattern, …] }` keeps every table, leaving out the rows of the
  tables its patterns match (`--exclude-table-data=<pattern>`, pg_dump's own matching: `*` and
  `?` wildcards, `schema.table` or a table in any schema; their structure kept). The backup then fails unless the destination holds an object of exactly the bytes
  sent. A backup that fails — the size check included — deletes the object its upload may have
  left (a deletion that fails, other than for an object not there, is logged), and says what
  failed: the dump, the upload, the hour's limit, or the daemon stopping. The daemon's stop waits
  up to 2 min 15 s for a running backup to clean up and record its failure
  (`<name>-hostd.service`'s `TimeoutStopSec` is 3 min); each runner's container is named
  `<name>-backup-<random>` and removed when its run is cut off. After a backup,
  the profile's backups (its file names) older than `retention_days` are deleted (`rclone delete
  --min-age`), then — whatever the retention — the profile's volume archives whose dump is gone (retention goes by the
  dump's age; one younger than two hours may be a backup's still running, another install's in a
  shared destination, and stays); a pruning that fails is logged and recorded, the backup stands. A backup takes at
  most an hour. `backup run` is a job (`<short>-stow` prints its log as it runs; refused until
  `<name>.service` ran, during another job, and once a reboot is scheduled); its last run —
  object, profile, release, start and end, bytes, error, pruning error — is kept in
  `/usr/local/<name>/backup-last.json`, which `backup status` shows with the timer's next run.
- The backup settings are the operator's, in `/usr/local/<name>/backup.yaml` (0600, on the
  persistent partition, strictly read; none: no backup runs), set over SSH with `backup config`
  (the cloud-config carries none): `destination` (`remote:path`, the remote a section of the
  rclone config — a remote name is letters, digits, `_`, `.`, `+`, `@`, `-` and spaces, at most
  64; at most 1024 bytes, no control character), `rclone_config` (the rclone config file's text,
  at most 32 KiB: the destination's credentials; `-` reads it from standard input; never shown —
  `backup config` and its JSON say only that it is set), `recipients` (1 to 16 age public keys,
  `age1…`, X25519 or post-quantum, one per flag; given, they replace the list), `retention_days`
  (default 30; 0 keeps every backup; at most 3650), `schedule` (a systemd calendar event, checked
  with `systemd-analyze calendar`, without `%` or `\`; default `*-*-* 03:00:00 UTC`) and `profile`
  (one of the release's, default its `default_profile`). A change gives the flags given and keeps
  the rest; the first needs a destination, the rclone config and a recipient; `--remove` drops the
  settings. `<name>-backup.timer` (`RandomizedDelaySec` 15 min, `Persistent`: a run missed while
  the VM was off runs once the timer starts again) is never enabled: the daemon starts it once
  settings exist, at each change and as it starts, on their schedule — one other than the default
  is the runtime drop-in `/run/systemd/system/<name>-backup.timer.d/schedule.conf`, then
  `systemctl daemon-reload` and a restart of the timer —, and stops it when they are removed (on a
  host without that unit, nothing); no boot runs it on a schedule not the operator's. Settings this release cannot read still start it,
  so its runs fail and say why; on the default schedule when the file does not decode, or when
  its schedule is one the unit cannot hold or `systemd-analyze` refuses (the daemon's log says so;
  `backup config` still shows the stored one). As the
  timer is `Persistent`, a restart whose schedule had a time pass since the last scheduled run (a
  new schedule, settings set again) runs a backup at once. The timer starts `<name>-backup.service`, which
  runs `<short>-stow backup run --if-configured` (nothing, saying why, without settings, before the
  install, or for a release without a `pg-age-rclone` backup or an OS without the runner; while
  another job runs — a `backup verify`, a restore, an upgrade — it waits for that job to end, at
  most 10 minutes, then runs at once, reading its settings, the install and its secrets then (a
  change made during the wait counts; settings removed skip it), or fails as refused (409) when the job still runs; a reboot
  scheduled, and any other refusal, fails it at once; at most 75 minutes). `backup run` asked by
  hand is refused at once (409) while a job runs. `backup list` lists the destination's backups of
  the project — `<name>-<letters and digits>-<UTC time>-…`, a profile a later release dropped
  included, another project's in a shared destination not — (name, size, time); `backup verify` writes `<destination>/<name>-verify.txt` (again each time)
  and reads its size back. Given `--destination`, `--rclone-config` or `--recipient`, it tries
  those over the saved settings (or, with none saved, as the whole settings; checked as `backup
  config` checks a change) and saves nothing; the answer says the settings were proposed. It is a
  job (`backup-verify`, its target the destination, one at a time with the others), which the
  verb follows and waits for: done, it prints that the destination is reachable and writable;
  failed, it fails with the destination's reason. A release whose spec has no `pg-age-rclone` backup refuses every
  `backup` verb (409), but the timer's run. `backup status` shows a backup running, the one an
  `upgrade` takes first included.
- `upgrade` takes a backup first, in its job, when settings exist (`--skip-backup`: none) with the
  settings' profile; a backup that fails is logged and the upgrade goes on (the boot still dumps
  the database to `snapshots/` before it migrates).
- One job runs at a time; none starts once a reboot or a shutdown is scheduled, and neither is
  scheduled during a job.
- The event journal: the host daemon and the health gate append what happened to
  `/usr/local/<name>/events.jsonl` (root's, 0600; one JSON object per line), which survives the
  daemon's restarts and reboots; `/usr/local/<name>/.stowage-events` is the writers' lock and holds
  the last number given, the last one a rotation dropped and the last boot recorded. Every entry is `{"seq", "at", "kind", …}`: `seq` is one past the last
  entry written in the journal's life (never reused; a crash may skip one), `at` the time
  (RFC 3339, nanoseconds, UTC). Past 2000 entries the journal keeps its last 1000. The kinds and
  their fields (a field without a value is left out):
  - `job.started` — `job` (the job's `started`, as `GET job` answers it: its identity),
    `job_kind` (`upgrade`, `rollback`, `reset-db`, `backup`, `backup-verify`, `restore`,
    `rotate`), `target`, `boot_id` (the boot it started in; absent when its id was not read).
  - `job.ended` — `job`, `job_kind`, `target`, `state` (`done` or `failed`), `error` (failed),
    `result` (done: what the job found, `GET job`'s `result` — `backup-verify`'s message),
    `reboot` (`{"action", "drain_secs"}`: the reboot the job scheduled as its last step — an
    upgrade's, `{"action": "reboot", "drain_secs": 30}`, a rollback's, `{"action": "reboot",
    "drain_secs": 0}`; absent when it scheduled none). A job that reboots at its end journals its
    `reboot.scheduled` and its `job.ended` (done) in one append, in that order, so a long poll
    answers both or neither; both are written before the job's `state` reads `done` in `GET job`,
    and before the reboot runs, so a shutdown that stops the daemon never leaves that job without
    an end. When the reboot then fails, the end stands (the job stays `done`) and a
    `reboot.cancelled` with the job's `by` tells it.
  - `job.lost` — `job`, `job_kind`, `target`, `reason`: `the daemon restarted` (its
    `job.started` was written in this boot), `the system rebooted` (in another), or `the daemon
    restarted, or the system rebooted (a boot id was not read)` (its `boot_id` or this boot's was
    not read: only two ids read tell two boots apart); written by a starting daemon for each
    `job.started` the journal holds without an end.
  - `reboot.scheduled` — `action` (`reboot` or `poweroff`), `drain_secs`, `by` (the `job` that
    scheduled it: an upgrade's reboot, a rollback's; absent when asked for), `boot_id` (absent
    when its id was not read). A `job.started` and a `reboot.scheduled` carry the id of the very
    `boot.started` record their append writes with, never one read apart from it.
  - `reboot.cancelled` — `action`, `reason`: `systemctl <action> failed: …` (with `by`, the
    `job` whose reboot it was, when a job scheduled it; a rollback's goes on to say whether the
    boot assessment was put back, see `rollback`), or `the daemon restarted before it`
    (written by a starting daemon that finds the last `reboot.scheduled` without a cancel,
    scheduled in this same boot — both ids read and equal; a `reboot.scheduled` without one is
    left open).
  - `boot.started` — `boot_id`, `release` (`{"version", "revision"}`: the release of the
    writer, the OS's), `slot` (`active`, `passive` or `recovery`; absent off Kairos),
    `fallback`; once per boot id, before any other entry of that boot: whichever writer appends
    first in a boot (the daemon as it starts, the health gate's verdict, the first install's
    end) writes it before its own entries — a starting daemon's `job.lost` and
    `reboot.cancelled` for what the boot before left open come after it, whichever process wrote
    first — when the last boot recorded — kept in
    `.stowage-events`, so through a rotation — is another. A boot whose id cannot be read is not
    recorded; a writer that could not read it (the daemon, the health gate) reads it again at its
    first entry a minute or more after the failed read (never at every entry), and keeps it once
    read.
  - `boot.judged` — the health gate's verdict, at each judgement: `boot_id`, `release`,
    `on_trial`, `verdict` and `reason` (why the install was not healthy, or why marking a boot
    good failed). `verdict` is `healthy`; `went_back` (on trial, unhealthy: the gate reboots and
    Kairos boots the passive system — also when marking the boot good or kept failed, as the next
    boot falls back); or `kept_unhealthy`, with `kept` telling why the system keeps running:
    going back is refused (the dry rollback's refusal), there is no passive system, or the boot
    is not on trial; or `not_installed`, on a boot that held no install when the gate ran (a
    first install given over SSH after it): nothing was judged (`went_back`, `on_trial`, when
    the gate then failed to mark the OS a first boot switched to good). `reboots` says whether the gate reboots now: `true` only on a `went_back`
    that reboots into the passive system; `false` on every other verdict, and on a `went_back`
    left to the next boot (marking the boot good or kept failed: nothing reboots now). A
    reboot that fails is a second `boot.judged` of the same boot, `went_back`, `reboots`
    `false` and `error` (`the reboot failed: …`): no reboot comes from the gate. A reboot the
    shutdown itself stops is the reboot coming, and writes none.
  - `install.finished`, `install.failed` — `release`, and `reason` (failed): a run of the VM's
    first install that brought it to its end, or that failed (a run stopped by a reboot or a stop
    writes nothing; the boot runs it again). A Docker install's journal holds the jobs its
    backup and restore verbs run (no verb reads it there).
  - `cert.expiring` — `reason`, the warning `status` gives: a byo install's certificate within 30
    days of its end, or past it, journaled as the daemon starts and once a day.
  - `network.no_ipv6_route` — `reason`, the warning `status` gives: IPv6 on and the primary link
    a minute or more without an IPv6 default route, once for each such stretch.
  - `service.down` — `target` (the service), `state`, `reason` (as `ps` says it: `exited 137
    (SIGKILL), OOM-killed (memory limit 96 MiB)`; an operator's `stop` that exits non-zero is one
    too, `exited 143 (SIGTERM)`): a service that becomes down — down, as `ps` judges it, at a look
    after a whole one where it was not — once a stretch while the looks are whole (a crash loop
    between `restarting` and `exited` is one). A look that is not whole forgets the stretch, so a
    service still down after it is journaled again: the daemon's start, a look skipped while one
    of its jobs runs, a reboot or power-off it scheduled drains, the install's lifecycle lock is
    held (an install, upgrade, rollback, provision, reset-db or a service verb; read from the
    kernel's lock table, never by taking the lock) or the boot unit brings the stack up again, and
    a look whose gate, install or `compose ps` does not read, or whose read outlives 30 seconds
    (logged once a stretch: `the service watch skips its looks: …`, the gate's refusal or the read's
    error, then `the service watch looks again`). An
    event the journal refuses is journaled at the next look. The VM's daemon looks every minute
    once the health gate judged the boot, and not before anything is installed. A restart its restart
    policy made is no event: `ps --json` gives each container's `restarts` and `oom_killed`. A
    Docker install has no such watch: its `status` names a service down. `GET health` and
    `health` answer the services as they are at once (`not ready: api exited with code 137`).
  - `backup.config` — `reason`: `changed through the app socket: <the settings changed>`
    (`destination`, `rclone_config`, `recipients`, `retention_days`, `schedule`, `profile`; the
    values never), each time a service `host_api` names saves the backup settings.
  - `<short>-stow events` prints the entries after `--after` (default 0), one per line (`--json`:
    the daemon's JSON), and says when entries asked for are no longer kept. `--follow` then waits
    for more until interrupted; a daemon it cannot reach (a restart) is asked again from the same
    number, after a wait that doubles from 1 s to 30 s. Each request is given its wait (60 s
    following) and 2 minutes to be answered, then counts as the daemon not reached, so a daemon
    that takes one and never answers is asked again. It runs on the VM, so a reboot ends it with
    the VM: run it again after the boot.
- The app socket: when `shapes.kairos.host_api` names services — in the OS's release or in the
  release the install runs when the daemon starts (an upgrade's boot brings that one up first, so
  its services reach the socket though the new release names none; it stays served for that boot;
  an installed release its kept installer cannot tell is logged, `no app socket: …`) — the daemon also serves its API on `/run/<name>/app/app.sock` (0666, in a 0755 directory beside its socket), and every
  request there must carry `Authorization: Bearer <token>`, the install's `STOWAGE_HOST_TOKEN`
  (64 hex digits generated at the first install, or by the upgrade to a release that names
  services first); before an install records one, or with another token, the answer is 401.
  `POST /v1/nftables/<table>/sets/<set>` (`{"add": […], "delete": […]}`, 1 to 1024 elements)
  feeds a named set of a service's nftables table (`services.<name>.nftables`, the table the
  service's name): a table no service has, or a set its file does not declare, is 404, a set an
  input fills (`services.<name>.sets`) 409; any service given the socket feeds any table's sets
  (`services.<name>.feeds` makes a load of the table recreate it); each element
  is an IP address, a prefix, a port (0 to 65535) or a range of two of one kind (low first, one
  address family), or their concatenation (`a . b`), written as such — a host
  or service name, which nft would resolve, and anything else 400 before nft runs; it answers `{"added": N, "deleted": M}`.
  It never flushes the host's connection-tracking table, `flush_conntrack` or not (the app's calls
  come often, and a flush ends every flow of the host's): a flow an element removed admitted stays
  open until it ends.
  The operator's routes answer 403 there, token or not: `provision`, `reset-db` (its plan
  excepted), `secrets export`, `secrets rotate`, `secrets set`, `secrets show`, `exec`, `restore` and `backup config --remove`
  (`DELETE backup/config`), run over SSH alone, and so does an `upgrade` with `skip_backup` (the
  backup a failed migration is restored from). `network` stays the app's, `revert_secs` 0
  included: a service holding the token can move the VM to an address nothing reaches, which
  the console undoes. The backup settings stay the app's to change —
  destination (a remote of the operator's rclone config, and a path), recipients, retention,
  schedule and profile, as a console sets them, so a service whose token leaked can send the
  backups to another of those remotes, to its own keys, or stop them —
  unless the operator locks them (`backup config --lock`, kept in `<dir>/backup-settings.lock`,
  which removing the settings keeps, and a lock file that cannot be read counts as one): the app
  then saves them unchanged alone (its whole form, recipients in any order), and every change is
  the operator's, over SSH. A lock is written before the settings and an unlock after them, so a
  refused or interrupted request leaves the settings locked; while they are locked the app's
  `backup verify` tries the saved settings alone (proposed ones answer 403: `the backup settings
  are locked: verify the saved ones …`). The rclone config — the remotes and their credentials —
  is the operator's, locked or not: an `rclone_config` from the app, saved or verified, is
  refused, the saved one too (GET never shows it, and an answer to whether a config is it would
  let the app test guesses of its secrets) (403, `rclone_config is the operator's: rclone runs the
  programs its options name, as root on the host; backup config over SSH`), so before the
  operator's first `backup config` the app sets nothing. Each operator's save of an rclone config
  keeps its SHA-256 in `<dir>/backup-rclone.sha256` (written before the settings, removed with
  them); on the VM, a saved config whose digest is not that one — saved before the digest was
  kept (a service may have saved it through the app socket under an earlier release) or edited
  on disk — is never run: a backup (scheduled, before an upgrade, or now), `verify`, `list` and
  `restore` fail with `the saved rclone config is not known to be the operator's … it is not run
  until the operator saves one over SSH` (409), and `backup status` and the app's settings say so
  (`rclone_config_from`: `unknown`) until the operator saves one again
  (`backup config --rclone-config FILE`; saving other settings keeps the doubt). A config the
  operator's `verify` proposes unsaved runs. On Docker, which has no app socket, every saved
  config is the operator's. The journal named an app's change of it (`backup.config`, `changed
  through the app socket: …rclone_config…`).
  The daemon
  runs rclone as root on the host's network, and its options name programs it runs (sftp's
  `ssh`, a `*_command`, S3's `credential_process` through a credentials file). Each change the app makes is
  journaled (`backup.config`; a save changing nothing, none).
  A daemon serving elsewhere (the hidden `--socket`) says so, as the services still mount
  `/run/<name>/app`. The services named, and only they, mount `/run/<name>/app` read-only at `/run/stowage-host`
  and are given `STOWAGE_HOST_SOCKET=/run/stowage-host/app.sock` and `STOWAGE_HOST_TOKEN`
  (through the front's compose file, `stowage-front.yaml`). A Docker install records no token and
  mounts nothing.
- The daemon's API is JSON over HTTP on the socket, under `/v1`: `GET status` (`format`,
  `previous` and `inputs` as `status --json` gives them, above; `boot`;
  `fallback`: Kairos booted the passive system; `on_trial`: the boot runs an upgrade's new system, which the gate has not judged yet; `boot_id`: the kernel's id of this boot, which changes with every reboot; `boot_settled`: this boot is over — `<name>-health.service` (the health gate, ordered after the boot unit) is active or failed, or absent, and `<name>.service` (the boot's install, upgrade or going back) is not running (active, failed, or never started, as when a dependency of it failed) —, so what the status says of this boot changes no more but by a job (`false` while either runs or is queued, or when their state cannot be read; `true` on Docker, which has no boot units); `boot_judged`: the health gate's last verdict in this boot, its `boot.judged` entry (`seq`, `verdict`, `on_trial`, `reboots`, and when set `reason`, `kept`, `error`; kept through the journal's rotations), absent before the verdict, and when it cannot be told, `boot_judged_error` then saying why (this boot's id or the journal cannot be read) — settled with an install, no `boot_judged` and no `boot_judged_error`, the gate ended without a verdict, which `status` says (`judged     no verdict: …`; `judged     unknown: …` for an error); a `went_back` whose `reboots` is `false` gets no reboot from the gate; `status` prints `judged` with the verdict, and `settling` while the boot is not settled (or the units' state cannot be read); `registry_login`: the host of the mirror the VM holds a login for (`registry-login`), never the login, absent when it holds none, and `registry_login_error` when the file held cannot be read (naming it and the fix; `status` prints `login      unreadable: …`); `events`: the event journal's last `seq`, 0 before the first, which a client follows with `GET events?after=` — `null` when the journal cannot be read, `events_error` then saying why (a client never takes a cursor from such a status); `failed_boot`: the last boot the gate found
  unhealthy — `release`, `at`, `reason`, `kept` when the gate kept it, `dir`), `GET health` (`{"ready", "reason"}`), `GET services` (each with its `exit_code`, and when set — each absent when false, 0 or empty — `id`, `oom_killed`, `mem_limit_bytes`, `restarts`),
  `GET services/{service}/logs?tail=N`, `POST services/{service}/{restart|stop|start}`,
  `POST provision` (an optional `{"set": {"KEY": "VALUE"}}`, as `provision --set`), `GET reset-db` (`{"target": {"version", "revision"}, "plan", "token"}`: the release a reset
resets, what it does, and a new token, which replaces the last; refusals as `reset-db`'s, a
running job and a scheduled reboot included), `POST reset-db` (`{"confirm": {"version",
"revision"}, "token", "no_provision"}`, 202, a job; refused without `confirm`, with a release
other than the one the install runs, and without the last GET's token, which it uses up; `GET
reset-db?new_baseline=true&channel=&version=` and `"new_baseline": true, "channel", "version"` in
the POST, `confirm` then naming the release found, cross a baseline; a channel or version without
it, or `no_provision` with it, is a 400; a token is good for the plan its GET showed alone — a
crossing or not, and its target — and another is refused: `reset-db's token is the GET's that
showed another plan: look again`), `GET updates?channel=&version=` (`left_by_rollback` marks a release the
  install went back from; `rewrite` the running release offered on the passive system), `POST upgrade` (`{"channel", "version", "confirm": {"version", "revision"}, "reboot", "skip_backup"}`, 202), `GET job?since=N` (`log_total` is the line to ask from next; the last job's `started` (its identity), kind, target, state — `idle`, `running`, `done`, `failed`
  — and its log from line N; `result`, what a job that is done found: `backup-verify`'s message;
  `reboot`, `{"action", "drain_secs"}`, the reboot a job that is done scheduled as its last step), `GET events?after=N&wait=S` (`{"events": [...],
  "last", "dropped", "truncated"}`: the journal's entries after N (default 0), at most 200,
  oldest first; `last` the last entry's `seq`; `dropped` the journal's floor, every entry up to
  it gone (0 when none is); `truncated` when the journal no longer holds N's successor — N is
  below `dropped` (the entries answered are then those after `dropped`) — or N is past `last` (a
  journal started again). A truncated answer comes at once, whatever `wait`: a client goes on
  after `dropped`, or, past `last`, from 0 (then after `dropped`), and never asks the same N
  again, which would be answered the same. With
  nothing after N and `wait` S from 1 to 60 (default 0, more refused with a 400, as is a bad
  `after`), the answer waits until an entry is appended — the daemon's own at once, the health
  gate's within half a second — or S seconds pass (then empty), or the daemon stops), `GET rollback` (`{"from", "to"}`: the release the install runs, the
  one the passive system carries; refusals as `rollback`'s), `POST rollback` (`{"confirm":
  {"version", "revision"}}`, 202, a job), `POST reboot` and `POST shutdown` (`{"drain_secs"}`, 202), `GET network` (`{"live":
  {"interface", "mac", "addresses", "gateway", "gateways6", "dns"}, "source": "override|install|dhcp",
  "override", "install", "pending": {"previous", "deadline", "secs_left"}}`; `install` is the
  network `vm create` gave, DHCP or static, which `network reset` returns to while its networkd
  file applies it (`source` `install`) — absent when the VM's
  cloud-config predates its record (`source` `install` then means a static network the answer does
  not detail) or when the record does not read (logged); `source` is `override`, `install` (that
  network) or `dhcp` (the OS's default); `previous` null is the cloud-config's network), `POST network` (`{"mode", "address", "gateway", "ipv6", "address6", "gateway6",
  "dns", "revert_secs"}`; `ipv6` absent is off), `POST network/confirm`, `POST network/revert`, `POST network/reset`
  (`{"revert_secs"}`), each answering `{"message", "override", "pending"}`, `GET backup/config`
  (`{"configured", "destination", "rclone_config_set", "rclone_config_from" (`operator`, or `unknown`: not run), "recipients", "retention_days", "schedule",
  "profile", "profiles", "locked"}`), `POST backup/config` (`{"destination", "rclone_config", "recipients",
  "retention_days", "schedule", "profile", "lock"}`: a field left out, or an empty `recipients`, keeps its
  value; answers the settings; `lock`, which `backup config --lock` / `--unlock` set, alone or
  with other settings, is the operator's: on the app socket it is refused (403), and while it is
  on, so is any change of the settings there; `POST backup/verify` refuses it, 400), `DELETE backup/config`, `POST backup` (`{"profile",
  "if_configured"}`: 202, a job, the target the backup's file name; with
  `if_configured`, 200 `{"skipped"}` without settings, before the install, or when the release
  has no `pg-age-rclone` backup or the OS no runner), `GET backup/status` (`{"settings", "next",
  "running", "last"}`), `GET backup/list` (`[{"name", "size", "time"}]`), `POST backup/verify`
  (no body, or `POST backup/config`'s fields to try unsaved, checked at once — 400 when invalid
  —; 202, a `backup-verify` job, the target the destination: done, its
  `result` is the message; failed, its `error` the destination's reason), `POST
  secrets/export` (`{"recipients"}`: `{"file"}`, the armored file), `POST secrets/rotate`
  (`{"key", "pass": "stage|swap|finish"}`, 202, a job), `POST restore` (`{"object", "identity",
  "confirm": {"version", "revision"}}`, 202, a job). Every 202 that starts a job answers
  `{"job", "target", "started", "events"}`: the job's kind and target, its `started` (its
  identity, as `GET job` and the journal's `job` give it) and the journal's last `seq` once its
  `job.started` was written, which its `job.ended` (and its reboot's entries) come after — `null`
  when the start could not be journaled, with `{"events_error": "the job's start could not be
  journaled: …"}` beside it (absent otherwise): a client then takes no cursor from it. An error is `{"error": "…"}` with a 4xx or 5xx status, and a refusal (a rule said no, nothing changed) `{"error": "…", "refused": true, "fix": "…"}`, the fix advice for a person that holds on the VM, absent when none does, which the verbs exit 3 on (an answer without `refused` is a failure, fix or not); a body with an unknown field is refused.
- A disaster recovery: `secrets export --recipient age1…` while the VM lives (the file kept off
  it), backups to a destination off it; on a new VM, `vm create --secrets FILE` (the file
  decrypted) installs with those secrets over SSH, after `systemctl start <name>.service` (a
  start the boot cancels, `Job for <name>.service canceled.`, is tried once more once
  `systemctl is-system-running --wait` returns, 10 min at most, unless the unit failed; a failure
  then fails the create, the VM kept), then `backup config` with the same destination, `backup
  list` and `restore OBJECT` load the data back. The front's local CA (`--tls internal`) goes
  with the secrets: the new VM's front issues from the same root.
- Not yet: `upgrade --reset-db`, secrets sealed in an encrypted answers file.
- The firewall: the bundle's `/etc/nftables.conf` holds a table of the project's own (`inet
  <name>`, flushed by itself on each load, never the whole ruleset: Docker's rules stay) whose
  input chain drops by default and accepts the loopback, replies (`ct state
  established,related`), ICMP and ICMPv6 (behind extension headers too), DHCP's answers (UDP 67→68, 547→546), SSH per
  `/etc/nftables.d/ssh.nft` (the bundle's is open to every source; `vm create --ssh-allow`
  replaces it) and the install's ports per `/run/nftables.d/<name>-ports.nft`, which each start
  of the install writes: the front's that something serves on (`tcp dport { <http port>, <https port> }
  accept` with a front route in a TLS mode; `tcp dport { <http port> } accept` for a front of L4
  routes alone in `acme` mode, the HTTP-01 challenge of their certificates; none otherwise, nor in
  loopback mode); from the stack's bridges, the ports the stack reaches the L4
  routes' services on, on the host's network (`iifname "br-*" <protocol> dport { … } accept`,
  see "L4 routes on the VM" ([installer](installer.md))); a route with `tls` (`tcp dport <port> accept`, in a TLS mode alone, from
  every source: the front filters them, whatever its upstream); and in every mode each other `l4`
route's whose service runs (none for a service whose profiles are all off) (`<protocol> dport <port>
  accept`, or, with an `allow_input`, `… ip saddr { <its IPv4 sources> } accept` and `… ip6
  saddr { <its IPv6 sources> } accept`, widest first: an address as its /32 or /128, an
  IPv4-mapped IPv6 one as IPv4, a CIDR's host bits cleared, one another holds left out; none for
  a family the list holds no source of, nothing while the list is empty; a value that is not an
  allowlist fails the start). A route with a bind address is accepted on it alone (`ip daddr
  <address> …`, or `ip6 daddr`, its family's rules alone); the front, and a route, on a
  `host.interfaces` role (`front.interfaces`, a route's `interfaces`) on those interfaces alone (`iifname "<role>" …`, `iifname { … }` for more than one); and on each
  role a service intercepts on (`services.<name>.transparent`), what the kernel hands a
  transparent socket (`iifname "<role>" meta l4proto { tcp, udp } socket transparent 1 accept`):
  a packet the service's table steered to its own socket (TPROXY), its destination a remote
  host's still, which the drop would otherwise take. Every input not accepted is dropped,
  so a boot drops the routes' ports until the install's start writes the file. Its forward chain
  (`priority filter - 1`, ahead of Docker's; policy accept) judges what the VM routes for others,
  IPv4 and IPv6 alike (one `inet` table), whatever Docker's own FORWARD policies are: it includes
  `/usr/local/<name>/nftables.d/*.nft`, on the persistent partition, where each start writes
  `<name>-bridges.nft`, the bridges the stack's networks name otherwise than `docker0` or `br-…`
  (`com.docker.network.bridge.name` of its own bridge networks, and of the external ones it joins
  as the engine reports them, `docker network inspect`: `iifname "<bridge>" accept`, `oifname
  "<bridge>" accept`), so a boot reaches the containers Docker restarts on them before the
  install's start (no file when none is named), and removes `<name>-l4.nft`, where earlier
  releases kept their L4 allowlists. Ahead of it (`priority filter - 2`), a chain of its own,
  `hold`, includes `/usr/local/<name>/nftables.d/*.hold`, where each start writes
  `<name>-front.hold` while the front answers on some roles alone (`front.interfaces`, an L4
  route it terminates TLS on with `interfaces`) outside loopback mode: Docker translates the
  front's published ports (those something serves on, as above) to its container, so the input
  chain never sees them, and these rules drop every packet, in its original direction, of a
  connection to one of them (`ct direction original ct status dnat ct original proto-dst {
  <ports> }`, an established one's too once the holds change) forwarded to a container bridge
  (`oifname "br-*"`, a rule alike for `oifname "docker0"` and for each named bridge: another
  translation to such a port, a cluster's service address, is not judged) and arriving on any
  interface but those roles' (`iifname != "<role>" drop`, or `!= { "<role>", … }`) — a
  container on a bridge included, the stack's own reaching the front by its routes' names on
  their network, an external network's container through the VM's address refused; a drop in
  that chain is final whatever the forward chain accepts after (an earlier release's bundle
  includes no `*.hold`: a rollback never loads it). The file goes while nothing is held
  (values that do not read hold the spec's ports, `http_port` and `https_port`, the front's
  unless the install moved them). After its includes, the forward chain accepts
  replies (`ct state established,related`) and what enters or leaves Docker's bridges by their
  names (`iifname`/`oifname` `"docker0"` and `"br-*"`), which Docker's own chains judge after it;
  includes `/run/nftables.d/forward/<name>-forward.nft`, which each start of the install writes,
  one rule per role and the role it forwards to (`iifname "<role>" oifname "<out>" accept`, a
  `tun` role by its `device`); and drops every other packet forwarded between the host's own
  interfaces (`counter drop`), so a boot forwards nothing between them until the install's start
  writes the file, and a release that forwards nothing leaves no file. The ruleset is loaded again
  (`nft -f /etc/nftables.conf`) when any file changed; a load that fails fails the start, leaves
  no ports nor forwarding file (the next start loads it again) and puts the persistent partition's
  files back as they were. Outbound traffic is Docker's and the OS's to judge; the input rules
  serve what reaches the host itself: the L4 routes' services, the front's ports Docker's userland
  proxy takes (an IPv6 client of a bridge without IPv6). Docker's FORWARD policy is DROP for IPv4
  (and for IPv6 where its `ip6tables` is on), so a stack routing for others (a VPN exit node, a
  gateway between two NICs) declares the roles it forwards between
  (`host.interfaces.<role>.forward`): besides the forward chain's rules, each start of the install
  gives the chain `STOWAGE-FORWARD`, which Docker's `DOCKER-USER` jumps to (inserted
  when missing), for `iptables` and `ip6tables` (Docker's iptables backend, the VM's; IPv4's
  alone on a kernel running without IPv6, `ipv6.disable=1`), one rule
  per role and the role it forwards to (`-i <role> -o <out> -j ACCEPT`) and one for the replies
  (`-i <out> -o <role> -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT`), nothing new into
  the role, in one `iptables-restore --noflush` each that replaces the chain's rules alone. This
  accept leaves the judging to the stack's own table (a service's `nftables`, whose forward chain
  decides what is forwarded and masquerades it): it does not make the role forward anything by
  itself. Each start gives the rules again (a chain flushed or edited meanwhile holds again);
  `/run/nftables.d/<name>-forward.rules` records what the chain may hold, written before the rules
  are given, and `/run/nftables.d/<name>-forward.rules.applied` that every table took them (a boot
  empties both); a release that forwards nothing removes the chain and `DOCKER-USER`'s jump
  to it, and with no record left iptables is not touched. A failed `iptables` fails the start (`the
  forwarding between the host's interfaces (ip6tables): …`), and the next start gives the rules
  again. A Docker host's chains are recorded the same way, under `/run/<name>-gateway-<8
  hex>-forward.rules` and `…-allow.rules`, each with its `.applied` mark (and the allowlists'
  `…-allow.held`, once a start as root saw them hold).
