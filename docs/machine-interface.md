# The machine interface

[README](../README.md) · [Documentation index](../README.md#documentation)

What a program (CI, the OpenTofu provider, a script) reads and relies on: one JSON object on
standard output per run with `--json`, a `format` number that moves only on a breaking change, and
an exit status that tells a refusal from a failure. The text output for people is unchanged
without `--json`. This page holds the Docker shape's full description, which the other shapes
share; each shape's own fields and verbs are on its page:

| What | Where |
|---|---|
| `describe --json` — what a release offers, read from the installer alone | [below](#describe) |
| `status --json` on Docker | [below](#status---json) |
| `--json` on the verbs that change an install; the Result object; `absent` | [below](#--json-on-the-verbs-that-change-an-install) |
| `updates --json` on Docker and Kubernetes (the VM's object, `follows` theirs alone) | [below](#updates---json) |
| Exit status: `0` done, `1` failed, `2` unknown (`kit check`), `3` refused | [below](#exit-status) |
| `k8s status --json` and the `k8s` verbs' `--json` | [kubernetes.md](kubernetes.md) |
| `vm seed --json`, `vm host-key --json`, `<short>-stow host-key --json` | [vm-create.md](vm-create.md) |
| The VM's `status --json`, its verbs' `--json`, exit status on the VM, the host daemon's API | [vm.md](vm.md) |
| The `command` provider contract (programs Stowage calls, not this interface) | [contracts.md](contracts.md) |

## status --json

- `status --json` prints the install's status as one JSON object for a program, the VM's
  `status --json` names (see "The installer on the Kairos VM" ([vm](vm.md))) where they mean the same, and
  never a recorded value, an input's or a secret's, so it can be logged; `status`'s text
  ([installer](installer.md)) is unchanged without it, and nothing else goes to standard output (a warning goes to standard
  error):
  ```json
  {
    "format": 1,
    "name": "notes",
    "installed": { "version": "2.0.0", "revision": 1, "installer": "ghcr.io/acme/notes/notes-installer@sha256:…" },
    "channel": "stable",
    "previous": { "version": "1.0.0", "revision": 1, "installer": "ghcr.io/acme/notes/notes-installer@sha256:…" },
    "inputs": ["ADMIN_EMAIL"],
    "registry": "ghcr.io/acme/notes",
    "short_name": "notes",
    "address": "127.0.0.1",
    "registry_ca": "sha256:…",
    "services": [{ "service": "api", "state": "running", "health": "healthy", "image": "…", "exit_code": 0, "id": "…" }]
  }
  ```
  `format` is the machine interface's version, `1`, moved only on a breaking change (a reader
  refuses one it does not know); `name` is the project; `installed` the release the install runs (`version`, `revision`) and
  `installer`, its installer image by the digest its release manifest records; `channel` the
  channel the install follows: the first install's `--channel`, else the channel the installed
  release's manifest records, as the installer that carries it tells it (the one run, else the one
  the install keeps; absent when neither carries the installed release);
  `installing` (`true`, absent otherwise) while the first install has not finished, and
  `install_failed` the first line of the error its last run failed with; `previous` the release
  `rollback` goes back to, with its `installer`, `null` when the install keeps none (before an
  upgrade, after a rollback) or keeps the release installed (an upgrade or a rollback that
  stopped, which `rollback` finishes on it) — never the release `installed` names; `inputs` the keys of the inputs the install
  records, sorted (`[]` when none), whichever release declared them: the install lists them in
  `.env` as `STOWAGE_INPUTS` (comma-separated, not a value compose or a hook reads) at the first
  install, each re-run, upgrade, rollback and provision, keeping the keys its record already
  listed — so after a rollback an input only the newer release declares, whose value the install
  keeps, is still listed —, together with those the installed release's spec declares when the
  installer that carries that release reads it. Absent (unknown) when neither tells: an install
  made before the list was recorded, whose release no installer read carries — none kept, or the
  kept one's spec does not load in this Stowage (logged once: `the installer the install keeps,
  <path>: …`, as any kept installer that does not read; none kept is said by the field alone); `registry` the image prefix the install pulls from,
  as its first install recorded it (`--registry`, else the release's: `STOWAGE_REGISTRY` in
  `.env`); `short_name` the `<short>` of the installer the install keeps, `<dir>/<short>-stow`
  (`STOWAGE_SHORT_NAME`); `address` the loopback address the first install recorded (`--address`,
  else `127.0.0.1`; `STOWAGE_LOOPBACK_ADDRESS`); `registry_ca` `sha256:<hex>` of the registry CA
  the install keeps (`registry-ca.pem`, the `--registry-ca` file as given), absent when it keeps
  none — so a program compares what it would give with what the install holds; `restore_awaited` (`true`, absent otherwise) while
  an install given `--secrets` awaits its data, and `volumes_split` while a restore left the
  volumes half restored — not while a restore runs (it holds the install's host lock), as on the
  VM; `services` the stack's services as the VM's `ps --json` gives them
  (`service`, `state`, `health`, `image`, `exit_code`, `id`, `oom_killed`, `mem_limit_bytes`,
  `restarts`), `null` with `services_error` saying why when Compose cannot list them. The
  installer image is recorded in `.env` as `STOWAGE_INSTALLER`, with `STOWAGE_INSTALLER_RELEASE`
  the release it was recorded for (`<version>/<revision>`), by the install and each upgrade,
  read from the release manifest of its release under its tag in the registry the install pulls
  from (`<prefix>/<name>-release`, this installer's version and revision, signed by a key the
  installer carries when it carries any); the upgrade keeps the replaced release's with it for
  `rollback`, which takes it back. It is absent — never guessed — where it was not read: an
  `--offline` install, a registry that holds no release manifest (logged: `the release's
  installer image is not recorded (status --json leaves it out): …`), an install made before it
  was recorded, or one recorded for another release than the one reported (`installed`: the
  release `.env` records; `previous`: the one `.stowage-prev/state.json` records), as an
  installer that does not record it leaves the replaced release's. Refused (exit 3): a directory
  with no install, which prints one object on standard output: `{"format": 1, "refused":
  {"reason": "… holds no install", "fix": "…", "absent": true}}` — `absent` tells "nothing
  installed here" from any other refusal. Failed (exit 1, nothing on standard output): a recorded
  revision that is no number, a kept record (`.stowage-prev/state.json`) that does not read,
  naming the file, and an `.env` or a `registry-ca.pem` that cannot be read (a permission). An install of another project is
  not refused: `name` says whose it is.
## describe

- `describe` prints what the release this installer carries offers, read from the installer alone,
  with no install and no `--dir`: the release, its channel (`""` from the `<short>-stow` inside a
  VM, whose OS bundle serves every channel it is tagged on: the VM's channel is `status`'s), its shapes, each input and the settings
  only a first install takes. It applies to every shape: the installer that runs `k8s` and `vm create`
  is the same binary, so it describes the Kubernetes and VM releases too (the `<short>-stow` inside
  a VM, which the OS bundle carries, has its own `describe` of the release that OS carries, see
  "The installer on the Kairos VM"). Without `--json` it prints a
  short summary for people (`<name> <version> (revision N), channel C`, `shapes`, an `input` line
  per input, `first install only: …`). `--json` prints the machine interface, one object, which
  programs read instead of the text:
  - `format`: the interface's version, an integer, 1; it moves only on a breaking change (a field
    removed, renamed or given another meaning), never when a field is added, so a reader refuses a
    format it does not know.
  - `name`, `release` (`{version, revision}`), `channel`: the release's, from its manifest.
  - `stowage_version`: the Stowage release that built the installer, as its release manifest
    records it (`""` when it records none), as `--version` prints it: a program tells what an
    installer offers by it without running another verb.
  - `short_name`: the `<short>` of `<short>-stow`, the installer's name, so a program finds the
    installer an install keeps, `<dir>/<short>-stow`.
  - `shapes`: the shapes the spec declares, in the order `docker`, `kubernetes`, `kairos`.
  - `inputs`: the spec's inputs in the spec's order (the form's), each `{key, type, secret,
    required, optional, shapes, when, advanced, as_file, profile, prompt, default}` as `inputs`
    declares it, every field present: `type` is `text`, `file` or `pem`; `as_file` is true for a
    secret input delivered as a file; `shapes` is `[]` for an input of every
    shape; `when`, `profile`, `prompt` and `default` are `""` when the spec gives none. A secret
    input's `default` is never printed: the key is left out.
  - `first_install_only`: the settings an install records at its first run and a re-run refuses,
    `["address", "registry", "registry_ca", "channel"]` (`--address`, `--registry`,
    `--registry-ca`, and the VM's `install --channel`).
  - `registry`: the prefix the release's images are pulled from when no `--registry` (or
    `--image-registry`) gives a mirror: the spec's `registry.prefix`.
  - `routes`: the front's routes in the spec's order, each `{name, loopback_port}` — the names
    `--ports route=port` takes, and the port each binds in loopback mode when `--ports` gives
    none (`front.loopback_ports`; left out when it gives none); `[]` without a front.
    `l4_routes`: the `l4` routes, each `{name, protocol, port}` — host ports no flag moves; `[]`
    when none. A program compares a release's routes with the installed one's to tell a route an
    upgrade adds from one the install holds.
  Without `--json`, a `route` line per route (`route     NAME (loopback port N)`), an `l4 route`
  line per L4 route (`l4 route  NAME (PROTOCOL/PORT)`), `registry  PREFIX` and `stowage   VERSION`
  (the Stowage that built the installer; left out when its manifest records none) come before
  `first install only: …`.

## updates --json

- `updates --json` (Docker) and `k8s updates --json` print the release an upgrade would move to
  as one object, the VM's `updates --json` object ([vm](vm.md)) without its VM-only fields and
  with `follows`, and
  nothing else on standard output:
  ```json
  {
    "channel": "stable",
    "current": { "version": "1.0.0", "revision": 1 },
    "target": { "version": "1.1.0", "revision": 1 },
    "available": true,
    "changelog": "…"
  }
  ```
  `current` is the release the install runs, `target` the one found (the latest revision of the
  channel's pointer's version, or of `--version`'s), `available` whether an upgrade moves to it (after `current`,
  with an installer, on the channel the install follows); `follows` (Docker and Kubernetes
  only: the VM's `upgrade` takes `--channel`), present only when `--channel` named another
  channel and the release found there comes after `current`, is the one the install follows, which `upgrade --version`
  reads alone (`available` is then `false`); `changelog` is left out when the release has none. A run that
  fails or is refused prints no object, only its error, and exits as any run does (below).
  The object has no `format` number, as the VM's, which its host daemon answers and the VM prints
  as is.

## `--json` on the verbs that change an install

- **`--json` on the verbs that change an install**, for a program: the install and its re-run,
  `upgrade` (`--version` too), `rollback` and `uninstall`, and on Kubernetes `k8s install`,
  `k8s upgrade` (`--version` too), `k8s rollback` and `k8s uninstall`. The run prints one object
  on standard output at its end, and nothing else there: what it prints for people otherwise (the
  running line, the endpoint and the tips, `--dry-run`'s values and helm command, "already runs",
  `release R in NS runs …`, `kept: …`) goes to standard error with its log. Asking still works on a
  terminal; give `--no-input` (and `--yes` where it confirms) for none.
  ```json
  {
    "format": 1,
    "result": "done",
    "release": { "version": "2.0.0", "revision": 1 },
    "refused": { "reason": "…", "fix": "…", "absent": true }
  }
  ```
  `result` is `done` when the run changed the install, `nothing` when it had nothing to do (the
  install already runs that release), was a `--dry-run`, or was refused; `release` the release
  installed once the run ended (`.env`'s in the directory `--dir` names on Docker; on Kubernetes
  the one the helm release's values record while helm lists it deployed), absent when none is (a
  refused first install, an install of another project, after an uninstall, a release failed
  or pending); `refused` only on a refusal:
  `reason` the message the error line prints, naming the specifics; `fix` advice for the
  operator, what to run or change — a program shows it, never runs it (where another installer
  answers, it names that installer, run in this one's place with the same arguments). `fix` is
  chosen for the shape that refused, and is absent when no advice holds there (on the VM, see
  "Exit status and `--json` on the VM" in [vm.md](vm.md)): the run is still refused, exit 3. `absent` (`true`, left
  out otherwise) says the refusal is that no install is there: no `.env` in the `--dir` on
  Docker, no helm release in the namespace on Kubernetes (`k8s upgrade`, `--version` too, and `k8s
  rollback`; never `k8s uninstall`, whose "nothing to uninstall" does not tell what helm keeps of a
  release — read `k8s status --json`) — never an install of another project or a release not from
  Stowage's chart. On the VM, a verb that needs the install, run before the first install (`ps`,
  `logs`, `restart`, `backup run`, `restore`, `rollback`, …), is refused, `absent` true (`<name>
  is not installed yet: \`sudo <short>-stow install\` installs it`, its fix `sudo <short>-stow
  install`, exit 3): the host daemon answers it 409 with `absent` and the verb's client carries
  it; `upgrade` and `updates` still serve a VM whose first install did not run or was refused
  ([vm](vm.md)), and `status --json` answers before the first install, `installed` `null`. A Docker
  host's `backup`, `backup config` and `restore` verbs answer the same refusal with Docker's
  words and fix (`<dir> holds no install`, the directory absolute, `install first, or give the
  --dir of the install`) for a `--dir` that does not exist, is no directory or lies under a file,
  before the daemon of their process is made (nothing is made there); whatever fails as that
  daemon is made is reported as itself (an interruption, an exit status kept), never as an
  unreachable daemon. A `--dir` that is a directory is the verb's to judge (its daemon's lock
  files are made there first): the backup timer's `backup run` still skips one whose install is
  not made. The k8s refusal of a missing release is one on every verb: its reason and its fix
  name the install to run, with what this run was given that the install takes
  (`--image-registry`, `--registry-ca`, `--image-pull-login`, `--values`, `--unsafe-sysctls`,
  `--no-provision`; never `--dry-run`, as the install named is the one to run, nor `--timeout`,
  each verb's own), the fix adding `, given the inputs this run was given (…)` when it was given any
  (never a value); `k8s upgrade --version V` says `… to upgrade to V`.
  A run that
  fails prints no object, only its error. `upgrade --version --json` passes `--json` to the
  installer it fetches, which prints the object; the run that fetched it prints none and exits
  with that installer's status (an installer from before 0.34.0 refuses the flag).
## Exit status

- **Exit status** of every `<short>-stow` run: `0` done, or nothing to do; `3` refused — a rule
  said no before anything changed, the error on standard error naming what to do; `1` failed;
  `2` stays `kit check`'s "unknown"; an installer this one runs for it (`upgrade --version`, a
  later revision's `upgrade`) passes its own status on. A check that says no once the run
  changed the install (a restore splitting the volumes as an upgrade starts the new release) is
  a failure (`1`), as is a rollback whose start fails after the files went back, and `tune
  --apply`'s start of the stack after its consent: the install changed. A kept installer that cannot be read, whose manifest this installer cannot decode, or whose
  probe gives no answer (it timed out, did not start, or exited otherwise than 1) fails (`1`);
  one that carries another release, or carries none, or exits 1 on reading its own, is refused. The refusals (each exits 3): the
  directory holds no install (`upgrade`, `rollback`, `uninstall`, `status`, `secrets export`), or another project's install, or one kept by another installer name; another
  lifecycle run holds the install's lock, or a hook's or a check's container an earlier run left
  is there (a refused first install may leave the directory it made, empty but for its lock
  file, `.stowage.lock`, which the next install takes); a restore left the volumes
  half restored; a directory with another project's `compose.yaml`, or that became an install
  meanwhile; a re-run given `--secrets`, or a first-install-only setting (`--address`,
  `--registry`, `--registry-ca`); `--secrets` with `--given` or `--kit`; a re-run by the
  installer of another release than the one installed; a required input missing, or a value its
  input refuses; ports in use, a front port that is an L4 route's, an L4 route bound to an
  address the host does not have, an existing `tls/`, and `--ports`/`--address` or
  `--http-port`/`--https-port` in the wrong TLS mode; an operator's "no" at a prompt (`aborted`);
  on `upgrade`: an older release, another front, TLS mode or a route the install cannot keep or
  serve, `--ports` or a `--set`/`--answers` key it does not take, a recorded value set again, a
  recorded value the release cannot read, `--offline` on an install that pulls, `--given`/`--kit`,
  a stopped upgrade or rollback, a kept installer that is not the release installed, a schema
  baseline crossed (or `reset-db --new-baseline` confirmed for another release, or with nothing
  to wipe), a host without the cores the services ask, and an `--answers` file's `tls` section;
  on `upgrade --version`: `--offline`, an install made `--offline`, keys the install trusts that
  cannot be told, a yanked release, one no key the install trusts signed, a version or revision
  not published on the install's channel, and an older release; on `rollback`: no previous
  release, a kept release that is not one release, a run by another installer than the one kept,
  another schema baseline, a rollback barrier since or a newer schema the previous release does
  not read; on `uninstall`: a gateway record that does not read, no `--yes` without a terminal,
  and a timer or the network declarations that take root to remove. On Kubernetes: a release
  that exists (install) or does not (upgrade, rollback, `--version`), one helm is working on
  (`pending-*`), one not installed from this project's chart, an older release, another schema
  baseline, an unknown or recorded input, the pull account's password missing, Pod Security
  refusing `--unsafe-sysctls`, `--given` with `--version`, keys the release trusts this
  installer does not carry, a release without a chart, `--revision` under 1, no revision to roll
  back to, the schema refusing a rollback, and on `uninstall` a release not from this project's
  chart, nothing to uninstall (or only the Secrets kept for the data), claims `--data` must
  name, and no `--yes` without a terminal.
