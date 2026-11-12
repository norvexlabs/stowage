# The installer (Docker shape)

[README](../README.md) · [Documentation index](../README.md#documentation)

What an operator runs: `docker run --rm <prefix>/<name>-installer:latest > <name>.sh && sh
<name>.sh` (or `… | sh -s -- <flags>`). The script is POSIX `sh` and needs `base64`. It carries the
`<short>-stow` binary, which carries the release (manifest, compose file, runtime spec, trusted keys,
pull account), so an install needs nothing else. The script unpacks `<short>-stow` into the first
private directory where a program may run: the current directory, then the user's cache, then
`$TMPDIR`. It runs it there and removes it afterwards. `<short>-stow script` prints that script
again, to copy an installer to another host.

```bash
<short>-stow --version   # (-v) `<short>-stow version <version> (revision N), stowage <Stowage version>`: the release it carries and the Stowage that built it (`, stowage …` left out when its manifest records none)
<short>-stow [--dir D] [--registry PREFIX] [--registry-ca FILE] [--offline] [--dry-run] # install / finish setup
          [--set KEY=VALUE]… [--set-file KEY=PATH]… [--answers FILE] [--no-input] [--accessible] [--no-provision]
          [--given FILE|- | --kit FILE|- [--kit-code-file FILE]]           # what another install gives
          [--ports route=port,…] [--address 127.x.x.x|::1]                 # loopback mode
          [--tls acme|dns-cloudflare|byo|internal] [--domains route=host,…] # TLS mode
          [--cert FILE --key FILE [--ca FILE]] [--http-port N] [--https-port N]
          [--client-ca FILE|none]                                          # routes with client_auth
          [--ipv6[=false]]                                                 # IPv6 on the stack's network
          [--tune-profile default|network|release]                         # over the release's tune.profile
          [--interface ROLE=NIC]…                                          # this host's NIC of a host.interfaces role
          [--secrets FILE|-]                                               # a machine made again
          [--json]                                                         # the result as one object, for a program
<short>-stow status [--json] [--dir D]                                    # --json: one object, for a program
<short>-stow describe [--json]                                           # what the release offers, read with no install
<short>-stow ps [--json] [--dir D]                                       # the stack's services, state and health
<short>-stow logs SERVICE [--tail N] [--follow|-f] [--dir D]             # 1 to 5000 lines, default 200
<short>-stow restart|stop|start SERVICE [--dir D]
<short>-stow exec [--stdin] SERVICE [--dir D] -- COMMAND [ARG…]          # in the running container; its exit status
<short>-stow schema [--after N] [--json] [--dir D]                        # the database's migration, the kept release's, the barriers after it
<short>-stow uninstall [--data] [--yes] [--no-input] [--accessible] [--dry-run] [--json] [--dir D]
<short>-stow secrets export --recipient AGE_KEY… [-o FILE|-] [--dir D]   # the secrets, encrypted
<short>-stow secrets rotate KEY [--stage | --finish] [--dir D]           # a secret with rotation: true
<short>-stow secrets set KEY [--accessible] [--dir D] < value            # a secret with settable: true; asked, unechoed, on a terminal
<short>-stow secrets show KEY [--dir D]                                   # a settable secret's value, printed on a terminal only
<short>-stow provision [--dir D] [--set KEY=VALUE]…                      # run the provision hook again; --set an input only it reads, or a value it returned or declares it returns
<short>-stow upgrade [--dir D] [--set KEY=VALUE]… [--set-file KEY=PATH]… [--answers FILE] [--no-input] # the NEW release's installer
          [--ports route=port]… [--offline] [--dry-run] [--no-provision] [--accessible] [--json]
<short>-stow upgrade --version V [--dir D] [--set KEY=VALUE]… [--set-file KEY=PATH]… [--answers FILE] # fetches V's installer
          [--ports route=port]… [--no-input] [--accessible] [--dry-run] [--no-provision] [--json]
<short>-stow rollback [--dir D] [--offline] [--dry-run] [--json]         # the installer the install keeps
<short>-stow reset-db [--dir D] [--yes] [--no-input] [--accessible] [--dry-run] [--no-provision]
          [--new-baseline]                                               # the NEW release's installer, across a baseline
<short>-stow backup config|run|status|list|verify [--dir D] [the VM's flags]   # a pg-age-rclone or volumes-age-rclone backup
<short>-stow restore OBJECT [--dir D] [--identity FILE|-] [--yes] [--accessible]
<short>-stow script                                                      # print the bootstrap script
<short>-stow tune [--check | --print] [--profile default|network]        # the host's network tuning
<short>-stow tune --apply | --revert [--profile default|network] [--yes] [--restart-docker] [--no-input] [--accessible]
<short>-stow mirror export [--channel C] [--version V [--revision N]] [--shapes S,…] [--output FILE] [--disks F,…|none]
<short>-stow mirror import FILE --to PREFIX [--registry-ca FILE] [--older]
<short>-stow audit [--version V [--channel C] [--revision N]] [--registry PREFIX [--registry-ca FILE]] [--db FILE] [--fail-on S] [--offline] [--json] [--dir D]
<short>-stow sbom export DIR [--version V [--channel C] [--revision N]] [--registry PREFIX [--registry-ca FILE]] [--dir D]
```

- `audit` and `sbom export` run where `<short>-stow` runs — a Docker host, or an operator's own
  machine with Docker for an install on a VM or on Kubernetes: the VM runs neither, and an
  operator needs only the version it runs (`status`). They read the release this installer
  carries, or with `--version` version V's latest revision (or `--revision`'s) on `--channel`
  (default: the channel the install follows, else this installer's), found and checked as `upgrade --version`
  finds it; in a directory holding this project's install, the release it runs (without
  `--version`), from the registry it pulls from with the keys it trusts (with or without `--version`: an older installer never brings back a key a
  later release dropped), elsewhere from `--registry` (a mirror, `--registry-ca` its CA) or this
  installer's registry, with this installer's keys. A directory holding an install whose record
  does not read is refused. The SBOMs of every image, the installer, the
  OS bundle and each disk (`disk-<format>`) are read as attested by a trusted key; an OS bundle
  or a disk a mirror (`--registry`, or the one the install pulls from) does not hold (exported
  without the `kairos` shape, or with `--disks` naming other formats) is left out, logged as `<artifact> <digest> is not in <prefix>
  (…): not read`.
- `sbom export DIR` writes each artifact's SBOM, verified, as `DIR/<artifact>.spdx.json` and
  `DIR/index.json` (`{project, version, revision, channel, tag, format: "spdx-2.3+json",
  artifacts: [{name, digest, file, error}]}`), for the operator's own tools (Dependency-Track,
  Trivy, grype); it prints each artifact, its digest and its file, and exits 1 when one has no
  SBOM by a trusted key (the others are written, the index naming why).
- `audit` scans each SBOM with the release's own scanner, read by digest with the same registry
  client (the pull account, `--registry-ca`) and loaded into the local engine as
  `stowage.local/<8 hex>/<name>-audit:<16 hex>` (`docker load`; the first hex are the registry
  prefix's hash, the others the scanner's digest, so a scanner read from another registry is
  another image) and logged as `pulling <ref> as <local>`: the engine's own trust is never changed.
  The scanner runs only when the engine's image is the release's: its layers' diff IDs, its
  entrypoint, command, environment, working directory and user those of the config the release
  names (one tagged so by another image is loaded over), and it runs by the image ID checked, so a
  tag moved meanwhile runs nothing else
  (an install made offline, or `--offline`, takes the one loaded by tag under the install's own
  prefix, `<prefix>/<name>-audit:<tag>`, whatever `--registry` reads,
  never pulls, and `audit` refuses `--version` — `sbom export --version` pulls no image and
  takes it; the SBOMs are still read from the registry the install records). Before the scans, grype's database is brought up to date once (`grype
  db update`; `--db` imports the file instead), and no scan checks it again. It prints and exits as the publisher's `stowage audit` does
  (`--json`, `--fail-on`, `--db`, `not audited: <why>`). The scanner's database is kept in
  `<dir>/.stowage-grype` (about 1 GB; in the user's cache dir when the directory holds no
  install). A disk's SBOM is its bundle's: it is scanned once, and the summary counts a finding
  shared by several artifacts once. A release that is not signed, or was shipped before releases carried SBOMs (no
  scanner), is refused, and so is a directory holding another project's install.

- Defaults: `--dir ./<name>`, `--address 127.0.0.1` (a 127.x address or `::1`, recorded bare
  in `STOWAGE_LOOPBACK_ADDRESS` — Compose's port syntax takes `::1:8080:80` — and bracketed in
  the routes' URLs, `http://[::1]:8080`; `localhost` is 127.0.0.1, and its URL `http://localhost:…`;
  any other address, a zone included, is refused), `--http-port` the project's `front.http_port`,
  else 80, `--https-port` its `front.https_port`, else 443. `--ports` and `--address` are for loopback
  mode only, and `--http-port` / `--https-port` for TLS mode only; the two ports must differ.
  `--cert` or `--key` alone picks `byo`.
- `--ipv6` turns IPv6 on for the stack's default network (`enable_ipv6` in the front's compose
  file; Docker ≥ 27.0 gives it a unique local /64 and forwards the published ports over IPv6 to
  the front itself), so the front sees each IPv6 client's own address — its source allowlists
  and `forward_auth` judge it. Off (the default), an IPv6 client still reaches a TLS front, through
  Docker's userland proxy, but the front sees the bridge's IPv4 gateway instead of it. Recorded as
  `STOWAGE_IPV6` (`true` or `false`, `false` when never given); a re-run without the flag keeps
  it, `--ipv6=false` turns it off. With `--ipv6` the default network carries the label
  `io.stowage.ipv6: "true"` (without it, the front's compose file says nothing of the network, so
  a project's own definition, an external one included, stands): a run whose choice differs from
  the label stops the stack first (`docker compose down --remove-orphans`, volumes kept; a Compose
  that does not make a changed network again would keep it), then starts it on a new one; IPv6
  the project's compose file or the daemon gives the network by itself never stops it. With `--ipv6` (not under
  `--dry-run`), the checks before anything is written make and remove a network of their own with IPv6
  (`stowage-ipv6-check-<random>`, labelled `io.stowage.probe=true`): an engine that cannot is
  refused, the install as it was. The VM takes it from
  `vm create --ipv6`.
- `--tune-profile default|network` chooses the stack's tuning profile over the release's
  `tune.profile` (see "Network tuning" ([tuning](tuning.md))), for a host whose load differs; `release` gives the choice
  back to the release. Recorded as `STOWAGE_TUNE_PROFILE` (absent: the release's); a re-run
  without the flag keeps it. A change of profile changes the containers' settings, so the next
  start recreates the services; it never changes the host's own settings (`tune --apply` does).
  The VM takes it from `vm create --tune-profile`.
- `--interface ROLE=NIC` (repeatable) names the NIC of this host playing a role of the release's
  NICs (`host.interfaces`, not a `tun`): the parent of a service's address of its own
  (`services.<name>.interface`). Recorded as `STOWAGE_INTERFACE_<ROLE>` (the role upper-cased);
  `ROLE=` removes it, a re-run without the flag keeps it. A role the release does not name, or a
  NIC that is not an interface's name (letters, digits and `_ . : @ -`, at most 15), is refused;
  so is the flag on the VM, which names its NICs by their roles (`vm create --interface`).
- `--registry PREFIX` pulls the images from a mirror holding the same `<name>-<key>`
  repositories (and `<name>-release` and `<name>-installer`, which `upgrade --version` reads
  there). The project's pull account is never sent there: the pulls use the operator's own
  `docker login`.
- `--registry-ca FILE` is a PEM file of the certificate authority the registry's certificate
  chains to — a mirror's own CA (certificates only: a file holding a private key, or nothing
  else, is refused, before anything is written). The installer's own reads (`upgrade
  --version`) trust it besides the system's (`k8s`, `mirror` and `vm create` trust only the
  `--registry-ca` they are given, never a file in a directory; the recorded file is read only
  in a directory an install owns, one with its `.env`); the install keeps it as `<dir>/registry-ca.pem`
  (0644), and later runs trust that file. Before pulling, the installer gives it to the Docker
  daemon as `/etc/docker/certs.d/<host[:port]>/ca.crt` (an IPv6 host bracketed,
  `[fd00::5]:5000`; none for a loopback registry, `127.x` or `[::1]`), which only root writes: when the run may
  not, it stops before writing anything and prints the `install -D -m 0644 …` to run as root. A
  `ca.crt` already there holding the CA's certificates (alone or in a bundle) is kept; one that
  does not is refused, never replaced. A first install that fails removes the `ca.crt` it wrote
  with the rest (and the host directory it made); a `registry-ca.pem` that was in the
  directory before is replaced by the CA given, not removed. A loopback registry gets
  none (the daemon takes it as insecure). The daemon is this machine's: a remote `DOCKER_HOST`
  or rootless Docker (`~/.config/docker/certs.d`) needs the file put there by hand.
  `--registry` and `--registry-ca` are recorded at the first install: a re-run given either is
  refused.
- `--answers FILE` is a YAML map, `KEY: value`; a file input's also `KEY: {file: PATH}` (see "File inputs"), the path relative to the answers file.
  Its reserved `tls` section gives the front's TLS settings to an install or its re-run, on Docker and on the VM, as `--tls`, `--cert`, `--key`, `--ca` and `--client-ca` do, a flag winning over the file:
  `tls: { mode: byo, cert: front.crt, key: front.key, ca: root.pem, client_ca: root.pem }`, every field optional, a relative path relative to the answers file (no `~`, as `{file: PATH}`), and `none` for `ca` or `client_ca` the flags' word (`./none` is a file). A flag wins whole: `--cert` or `--key` replaces the section's `mode`, `cert`, `key` and `ca`; `--tls` with a mode other than `byo` replaces `mode` and leaves out `cert`, `key` and `ca`; `--domains` replaces a section's `mode: none` (loopback serves no domain); `--ca` and `--client-ca` replace only their own. The run logs which flags the section stands in for, as a later check names those flags (`the tls section of FILE gives --tls, --cert`), and which fields a flag left out; a file left out need not exist. The flags' checks apply unchanged, so a re-run given the section replaces the certificate as `--cert` and `--key` do; left out, a re-run keeps what the install recorded.
  A field it does not know, one given twice or empty, a mode that is no TLS mode and a file it takes that does not exist are refused, naming the file and the field. `vm create` takes it too, its material given over SSH ("Given over SSH"). Refused too: the section on any other command taking `--answers` (`upgrade`, `k8s install`, `k8s upgrade`; on Kubernetes the chart's ingress serves the routes): remove it for that command.
- `--given FILE|-` takes what another install gives, in the clear, at most 1 MiB, strict YAML: `project`, `inputs` of KEY: value with a file input as `stowage-file:<base64>`, `tls` of `cert`, `key`, `ca`, `client_ca` as PEM (a certificate picks `byo`). An empty value is refused, as for `--kit` and `--kit-code-file`, so a script's unset variable cannot drop them. It is refused when encrypted, for another project, holding nothing, or with a `cert` or `key` alone or a `ca` without its `cert`. Its inputs are taken as an answers file's and its TLS material as `--cert`, `--key`, `--ca` and `--client-ca`: `--set`, an exported variable, `--answers` and the flags win, what they leave out of its `tls` logged (`--given: tls: cert, key left out, as the flags given decide`), and the form asks the rest. On a re-run its values replace the recorded ones, each change logged. An input the project declares for other shapes alone is left out and logged (`--given: KEY left out: not an input of the docker shape`), and it is refused when that leaves it nothing, so it cannot end a VM's wait for what `vm create` gives (`--given: nothing in the file is for this install: …`). One this install does not ask, undeclared or of another TLS mode (`when`), is refused as any (`no input of this install is named KEY`, naming the mode that asks it): on a first install `--tls` such a mode; on a re-run, which cannot cross between loopback and a TLS mode, get a new file from its issuer or make a new install. TLS material this install cannot take is refused as its flag is, and the message names that flag (`--ca: …`). The bootstrap script run from its file (`sh <name>.sh --kit - < FILE`) leaves standard input to the installer, whatever it is; arriving on standard input itself (`… | sh -s --`), it gives the installer the terminal, so `-` has nothing to read: save it first. With `-`, or any other run whose standard input is no terminal, nothing is asked: the form's questions take their defaults, and a kit's code must come from `--kit-code-file` or `STOWAGE_KIT_CODE`.
- `--kit FILE|-` takes the same material sealed with a code: a handoff kit. A kit is age, armored, to a scrypt recipient (work factor at most 18), at most 2 MiB, holding at most 1 MiB of strict YAML: `format: stowage-kit/v1`, `for` (the project), `expires` (RFC 3339), and optionally `name`, `instance`, `issued`, `inputs` and `tls` as in `--given`. So `age --passphrase --armor` makes one. It is refused with the wrong code or a changed file (`--kit: this code does not open it: …`), when not armored, in another format, for another project, at or past `expires` by this machine's clock (which is no revocation), and as `--given` is for what it holds; left-out material is logged under `--kit`, and `--kit: "NAME", instance "I", until T` names what was opened. It never logs the code, and logs a value as any re-run does: a secret `<redacted>`, a non-secret in the clear (`changing KEY: old → new`). The code comes from `--kit-code-file FILE` (at most 4096 bytes, surrounding space ignored), else the variable `STOWAGE_KIT_CODE` (removed from the environment once read, so no program the run starts inherits it), else it is asked, unechoed, on the terminal (a plain line under `--accessible`); with none of them, or `--no-input` without the first two, the run is refused naming them. A file that is no kit is refused before the code is asked. A wrong code is not asked again: run it again. `--given` with `--kit` or `--kit-code-file`, `--kit-code-file` without `--kit`, and either with `--secrets` are refused. `upgrade`, on Docker and on the VM, refuses all three (`--kit: an upgrade takes none of it: …`), naming the re-run that takes them.
- `--dry-run` on a first install prints every `KEY=VALUE` `.env` would record (secrets
  `<redacted>`) and writes nothing. On a re-run, it logs the changes a run would record and says
  the stack would be brought up again.
- `status` prints the release (version, revision), the registry, the stack's tuning profile and
  whose it is (`tune      network (operator)`, or `(release)`), then, while the first install
  has not finished, `not finished: the first install failed (<the first line of its error>); a
  re-run of the installer with --dir D finishes it` (or `… runs, or was stopped; …`); while a
  restore left the volumes half restored, `not running: a restore left <name>'s volumes half
  restored … (or a restore is running)` instead; each
  route's URL (after it, while an install given `--secrets` awaits its data, `not provisioned: a
  backup of the install --secrets came from is awaited; once loaded, `<short>-stow provision --dir D`
  provisions it`), each `l4` route as `<name> <protocol>/<port>` followed by its endpoint when it has
  an `address_input` (`echo      tcp/17007 localhost:17007`) and where it listens (`· on
  192.0.2.7 alone` from its `bind_input`, else `· on every address, open to whoever reaches this
  host`, adding `(<BIND_INPUT> gives it one address)` when the route has a `bind_input`; `· not
  served: <service>'s profiles are off` when the service it reaches runs in no profile on), `endpoint  <URL>` when the spec names an `endpoint` (that route's URL), a `tunable   KEY=value` line per advanced input (`(not set)` when none is
  recorded — the compose file's default, or off for one the front reads — a secret one
  `<redacted>`), in byo mode `cert      <its names>, valid until <date> (<N> days)` and, from 30
  days before that date or once it passed, `warning: the certificate expires in N day(s), …` (or
  `expired on …`) naming the re-run that renews it, with `hooks.maintenance.disk_pressure` a
  `pressure  <level>: <used>% used (soft …, hard …)` line and the watch's last POST, `docker
  compose ps --all`, and a line for each service down (`warning: which services are down could
  not be read: …` when compose ps or its JSON fails): `down: <service> <why>`, where a service is
  down when its container exited with a code other than 0, restarts, is dead, is being removed
  or was created and never started (`exited 137 (SIGKILL), OOM-killed (memory limit 96 MiB)`: its exit code, whether its memory
  cgroup killed it, the memory limit its container runs with, and the signal an exit of 128+N
  tells, N from 1 to 64: `(SIGTERM)` for 143, `(SIGKILL)` for 137, `SIGHUP` to `SIGTERM` by name,
  `(signal N)` for the others). A container exited 0 is not down (a one-shot that finished, which
  Compose does not tell from a long-running service that stopped of itself). One the operator
  stopped (`stop SERVICE`) is down when its process exits non-zero, its signal saying so (`exited
  143 (SIGTERM)`, `130 (SIGINT)` for a `stop_signal: SIGINT`, `137 (SIGKILL)` after the stop's
  timeout), and not down when it exits 0 on its stop signal: Docker records no stop's author. A crashed replica of a scaled service is down, and so is a
  start-first overlap's failed new container that could not be removed (the start warned of it).
- `status --json`, `describe` (`--json`), `--json` on the verbs that change an install and the
  exit status of every run: the machine interface, in [machine-interface.md](machine-interface.md).
- `ps` prints `docker compose ps --all` of the stack (the front among its services), `--json`
  Compose's JSON, one service per line. `logs SERVICE` prints a service's last `--tail` lines
  (1 to 5000, default 200, no colour), and with `--follow` the lines it writes next until
  interrupted. `restart`, `stop` and `start SERVICE` run Compose's verb on one service, under
  the install's lifecycle lock (refused while a provision, upgrade, rollback or reset-db runs,
  and while a restore left the volumes half restored); a service `stop` stops stays stopped until
  `start`, or until a run brings the stack up again (a re-run, `upgrade`, `rollback`,
  `reset-db`). A name that is no service of the stack is refused, naming them.
- `exec [--stdin] SERVICE -- COMMAND [ARG…]` runs the command in the service's running container
  (`docker compose exec`), with a terminal when standard input is one; without one (`-T`), the
  command reads this command's standard input only with `--stdin` (`-i`), else none, as on the VM,
  so one command line drives either shape; a pipe or a non-empty file it drops is warned
  (`warning: this command's standard input is a pipe or a file, which exec passes only with
  --stdin (-i): the command reads none`). The run exits with the command's exit status. The
  command goes after `--`.
- `schema` reads the database's schema as `rollback` checks it, on the running stack (the
  project's `database.schema_compat`; refused without one): `migration  N`; when the install
  keeps a release for `rollback`, `kept       V (revision R), at migration M before the upgrade:
  \`rollback\` goes back to it` and `barriers   after M: <the barriers, or none>`. `--after N`
  reports the barriers after migration N instead. `--json` prints `{"migration", "kept":
  {"project_version", "revision", "schema"}, "after", "barriers"}`, the absent ones left out.
- `uninstall` removes the install's containers and networks (`docker compose down
  --remove-orphans`, the containers a hook or a schema check left among them). The data — the
  stack's volumes and the install directory (its `.env` and secrets, the dumps, the release kept
  for `rollback`) — is kept, and it prints `kept: its volumes and D (its secrets): \`D/<short>-stow
  --dir D\` installs it again on them, \`… uninstall --data --dir D\` deletes them`; with `--data`
  (or "Yes, and delete the data" on a terminal) it deletes the stack's volumes (`--volumes`; an
  external one never) and then the directory. It asks on a terminal ("No", "Yes, and keep the
  data", "Yes, and delete the data"; default no); without one, or with `--no-input`, it refuses
  unless `--yes`, which keeps the data unless `--data`. The install's timers on the host — the
  scheduled backup's and the disk-pressure watch's units — are disabled and removed either way;
  a later install on the kept data writes both again (the schedule from the settings `backup
  config` keeps in D). The network declarations `tune --apply` applied (see "A Docker host's
  gateway") are removed too, as `tune --revert` removes them, consent included: a later install
  applies them again only after another `tune --apply`; without root it refuses first (`the
  stack's network declarations are applied on this host (tune --apply), and removing them takes
  root: …`). A record that does not read refuses the uninstall before anything goes (`…: fix or
  remove it, then uninstall again`); one of another directory (copied there) is left as it is
  (`note: …: left as it is`); one of a directory gone (moved) is this install's, removed. Removing a unit written as root takes root: a run as another user refuses
  before anything goes (`<unit> is the install's, and removing it takes root: run \`sudo … uninstall
  --dir D\``). It runs under the install's lifecycle
  lock, and refuses a directory holding an install of another project. `--dry-run` prints what
  it would remove and keep. The host's own settings (`tune`) are not touched.
- A failed first install removes the directory it created.
- The install directory holds:
  - `.env`, the project's `compose.yaml`, `.env.prev` and `.stowage-changing` (a re-run's
    changes, above);
  - `.stowage-installing`, from the first install's `.env` until its stack came up and the
    provision hook ran (or `--no-provision` skipped it): empty while a run is under way or after
    a reboot or a stop cut one, `failed: <the first line of its error, at most 300 bytes>` once
    one failed (a second line `stack: up` when the stack had come up and the hook failed); a
    re-run, `reset-db` or an upgrade whose stack comes up removes it, and so does `provision`
    after a `stack: up` failure (it brings nothing up itself); a dry run or a refused re-run
    leaves it as it is;
  - the files Stowage adds: `stowage-front.yaml` and `stowage-front.Caddyfile` (the front; without
    one, the first carries the L4 ports and the host API alone and the second is a comment),
    `stowage-front-client-ca/ca.pem` (the client CA `--client-ca` gave),
    `stowage-content.yaml`, `stowage-provision.yaml`, `stowage-schema.yaml`.
    `stowage-content.yaml` labels each service that mounts a compose `configs:` entry with inline
    `content:` or a `secrets:` entry from an `environment:` variable with an HMAC-SHA256 of what
    Compose renders into it (`io.stowage.content: hmac-sha256:…`), rewritten from `docker compose
    config` before each start, and reset (no label) whenever the stack's files are written with
    another compose file, as its labels name that compose file's services: Compose copies these into a
    container as it makes it, and makes none again when only they change, so the label makes their change the service's own, and the
    start recreates it. A file-backed config is a bind mount, which a change reaches as it is. The
    HMAC's key is the install's own, `.stowage-content.key` (0600, made at the first start), so a
    label tells nothing of the secrets it covers; a lost key only recreates those services once.
    On the VM the same file runs each `l4` route's service on the host's network and maps the
    names between it and the rest of the stack (see "L4 routes on the VM").
    The same file gives each service with a network namespace of its own (no `network_mode`, or
    `network_mode: bridge`, and not an L4 route's on the VM) the network settings a namespace keeps apart from the host's, by the
    stack's tuning profile: the host's own values, and on the `network` profile its values where
    the host holds less; each service its open files (`ulimits.nofile`); and each bridge network
    the stack makes the host route's MTU — each as "Network tuning" (under the installer) says,
    and none on Docker Desktop; a start whose `docker info` does not answer fails rather than
    drop them. The network profile's values follow RAM by doubling (the buffer is a power of
    two), so a small change of RAM recreates nothing. A change of them (the host's values, its
    RAM or the profile changed) recreates the services at the next start, as any change of the
    service does; a change of the host's MTU makes the networks again.
    The same file gives each service of `services` its memory limit, its cores and its address of
    its own — see "What a service asks of the host" ([host](host.md)).
    An install made by an earlier Stowage has no `stowage-content.yaml` until its next start, and
    a rollback to a release it kept removes the file (the labels go with their compose file);
  - `provision.env` and the hook's `outputs` file;
  - `.stowage-restore-awaited`, written by an install given `--secrets` with provision values
    (with them, before its stack starts): while it is there no run provisions; `provision`, a
    `restore` that loaded and `reset-db` remove it;
  - `<short>-stow`, `tls/cert.pem` and `tls/key.pem` (`byo`);
  - `snapshots/`, `.stowage-prev/`, `.stowage-failed/`, `.stowage-next-*/` (an installer
    `upgrade --version` fetched, kept only when its run failed), `.stowage-crossed` (the releases
    a `reset-db --new-baseline` crossed from and to, until the next upgrade), and `.stowage.lock` (the run
    lock).

  The front keeps its certificates in the volume `stowage-front-data`, its user's (see the front below).

- Inputs: `--set`, then the environment, then `--answers`, then — on a terminal — one form asks for
  the rest, then the spec default (which
  may name an earlier input, `${KEY}`, including one just answered). Any field can be revisited
  (Shift+Tab), and a review screen then offers to install, change an answer or quit writing
  nothing. A secret input is never echoed. With no terminal, or `--no-input`, nothing is asked; every missing required input is
  then reported at once. `--accessible` (or `ACCESSIBLE` set, or `TERM=dumb`) asks with plain line
  prompts for screen readers; there, Enter keeps the answer already given (or takes the default),
  and end of input aborts like Ctrl-C. Before that form, the install directory is asked on its
  own (unless `--dir` was given; `~/` is expanded), the host is checked (Docker, Compose, the
  directory), then how the front serves is chosen (unless given) and its ports checked, so a
  refusal never discards answers; ports are checked again after the form, whose review can also
  go back to how the front serves, keeping the answers. A key that names no input is an error.
- Nothing is pulled or written before every check passed: every value decided and recordable,
  Docker and Compose ≥ 2.20 (and Docker ≥ 27.0 with `--ipv6`), free ports (on 127.0.0.1, the
  default, `::1` too: `localhost` names both, an IPv6 host trying `::1` first), a loopback address, no foreign `compose.yaml` in the
  directory. Images are pulled by digest, only those not already present. When the project has a
  `token` pull account (the one baked into the installer, or its password from the named
  variable, which wins in a rotation), the pulls read it from a private Docker config that the
  installer writes and then removes. It never runs `docker login`: the operator's own config and
  credential store are untouched, and the operator's current Docker context is kept. `docker
  compose up` and the provision hook's run take the same config, so an image removed meanwhile
  (a prune) is pulled again with the account rather than anonymously; and when `up` fails while
  one of the release's images is gone from the engine (Compose then cannot create its container),
  the installer pulls the missing ones again and starts the stack once more. With
  `docker-config` or `none`, a mirror, or no password, the pulls use the operator's own Docker
  login, and the installer says so. The front's image is the release's own, pulled from the
  project registry (or the mirror) by digest like the others. `--offline` uses the images the operator loaded, by
  the release's tag (`docker load` keeps tags, not digests): each must have an ID the release
  records (image, linux/amd64 manifest or config digest), and a tag is never pulled — on any later
  run either. Each missing one is named. Make the archive on a host with the same image store as
  the target (containerd → containerd keeps the registry digests; classic → classic keeps the
  config digest), e.g. `docker pull` each image by digest, then `docker save`. Load the front,
  `<prefix>/<name>-front:<tag>`, too.
- `.env` (0600) records every decision — every safe default included —
  as single-quoted values compose reads literally (a value with a `'`, a line break or a trailing
  `\`, or over 64 KiB, is refused);
  its values win over the shell's environment on every run. Besides the values the compose file
  reads, it records the installer's own: `STOWAGE_REGISTRY` (the prefix images are pulled from),
  `STOWAGE_FRONT_IMAGE` (with a front), `STOWAGE_CHOWN_IMAGE` (a release with the volume helper), `STOWAGE_PORT_<ROUTE>` (loopback mode), `STOWAGE_HTTP_PORT` and
  `STOWAGE_HTTPS_PORT` (TLS mode) and `COMPOSE_PROJECT_NAME`. Edit none of them by hand. The compose project is
  `<name>-<directory name>-<hash of its path>`, so two installs in two directories stay two
  projects. The install keeps the installer that wrote it as `<dir>/<short>-stow` (the running file
  itself, checked to carry the release).
- A re-run (the installer again on a directory holding an install of the same release; a
  different release is an upgrade) finishes setup: it brings the stack up again and runs the
  provision hook again. Given nothing, it changes nothing recorded, and asks nothing. It changes
  what it is given, and only that:
  - `--set`, `--answers` and `--domains` (TLS modes) replace a recorded input, checked as a first
    install checks it (a required one cannot be emptied; a domain must be one, distinct from the
    other routes', and with `byo` covered by the install's certificate, which must not have
    expired). A loopback install takes no route's domain. The environment never changes a
    recorded value: an exported variable named like an input is ignored on a re-run. A key that
    names no input of the install is an error. An input whose default names another (`${KEY}`)
    keeps its recorded value when that other changes: give it too.
  - `--ports route=port` (loopback mode) or `--http-port` and `--https-port` (TLS modes) move the
    front; the other mode's flags are refused. Only a port the install does not already hold is
    checked free, so two routes may swap theirs.
  - `--tls` moves the install to another TLS mode (between `acme`, `dns-cloudflare`, `byo` and
    `internal`; to or from loopback is a new install, refused), the inputs the new mode requires
    given with it (`--set STOWAGE_CLOUDFLARE_API_TOKEN=…` for `dns-cloudflare`) and the front's
    inputs it does not read removed from `.env` (logged `removing KEY`). `--cert` and `--key`
    (both; they alone pick `byo`) give a new `byo` certificate, checked as at a first install —
    a pair, unexpired, covering every route's domain — and given without `--ca`, it must chain
    to the install's CA (`tls/ca.pem`, kept) when there is one; `--ca` alone gives a new CA for
    the install's certificate, and `--ca none` publishes none. The new files are written into a
    new directory that takes `tls/`'s place, the replaced one kept as `tls.prev/` until the stack next
    comes up (a re-run, `secrets rotate`, `secrets set`, `reset-db`, an upgrade) (one already there, from a run that failed, is the last that came up: it is
    kept, and the failed files go). A move away
    from `byo` removes `tls/` once a re-run's stack came up in the new mode (also when the run
    that recorded the move failed). A re-run without `--tls` keeps the
    recorded mode even when the release no longer offers it. A new Cloudflare token is `--set STOWAGE_CLOUDFLARE_API_TOKEN=…`. The front is
    recreated whenever its configuration changes (the Caddyfile or the `byo` files, whose digest
    it carries as the label `io.stowage.front-config`).
  - An advanced input given empty is removed from `.env`: the compose file's default applies
    again.
  - What derives from them is computed again: each route's `STOWAGE_URL_<ROUTE>`, the
    `STOWAGE_PORT_<ROUTE>` or `STOWAGE_HTTP_PORT`/`STOWAGE_HTTPS_PORT`, each `l4` route's
    `STOWAGE_L4_<ROUTE>` and `COMPOSE_PROFILES`. The generated secrets are kept.
  - A value given as recorded is no change. Each change is logged as `changing KEY: old → new`
    (a secret input's values `<redacted>`) once the host checks passed. `.env.prev` (0600,
    secrets included) keeps the `.env` a change replaced: a change writes it from the `.env` it
    replaces, unless an earlier change has not come up since (marked by `.stowage-changing`,
    removed once a re-run's stack comes up and, unless `--no-provision`, its hook ran), so a
    second try never loses the values from before the first. An upgrade or a rollback removes
    both: values of another release are no way back. `.env` is rewritten, then the stack is brought up
    again with it and the hook run. When a step after that fails, the changes stay recorded and
    the error says so, naming `.env.prev`: a re-run giving its values goes back. The `byo`
    certificate is read only for a domain that changes, so an expired one never stops a re-run
    (nor the VM's boot).
  - `--address`, `--registry`, `--registry-ca` and `--channel` are first-install only: a re-run
    given one is refused.
- `upgrade`, run by the new release's installer, moves an install of an earlier release (ordered
  by the scheme the release records; a `command` scheme is not checked) to it. It refuses, before
  anything changes, a release that drops the install's TLS mode, or names an input the release
  does not add (`--set` and `--answers` only give what it adds; on a terminal those are asked),
  or an install whose kept `<short>-stow` is not the installer of the release it runs (a rollback
  goes back with it). A route the release adds is decided as a first install decides it, and
  recorded as one records it (`STOWAGE_URL_<ROUTE>`; in loopback mode `STOWAGE_PORT_<ROUTE>` too;
  `--dry-run` prints them), the front then serving it:
  - in a TLS mode, on its `domain_input`: one the install records (a `path` route on a domain it
    serves) asks nothing; one the release adds is given as any added input (`--set`, `--answers`,
    asked on a terminal, else `missing required input(s)`), and one nothing asks is refused
    (`<name> <version> adds the route <r>, whose domain <KEY> nothing gives: --set
    <KEY>=<domain>`); routes the domains given make clash are refused (`<name> <version>: <a> and
    <b> both serve the domain <d>`); in `byo` mode the install's certificate must cover the domain of
    a route the release adds (`<name> <version> adds the route <r>: the install's certificate does
    not cover <domain> (<KEY>)`; one that does not read, or expired, is refused: `the install's
    certificate: …`), and is not read when the release adds none;
  - in loopback mode, on its port: `--ports route=port` (repeatable; a route the release adds
    alone: `--ports: the route <r> is recorded at the install, and an upgrade keeps its port`),
    else the release's `front.loopback_ports`, else refused (`<name> <version> adds the route <r>,
    which has no loopback port: give it one with --ports <r>=PORT`); a port another route has, an
    L4 route's, or one in use on the host is refused as at a first install; `--ports` in a TLS mode
    is refused (`--ports is for loopback mode: …`).

  A route the install publishes whose `domain_input` it does not record (a release that renames
  the input) is refused, as is one it records empty. It also refuses:
  - an install of a later release (the message points to `rollback`);
  - `--set` of a value already recorded, which an upgrade keeps;
  - an install that a stopped rollback left, until `rollback` is run again;
  - `--offline` on an install that pulls.

  An install already on the release — the installer run is the one the install keeps — is
  upgraded to that version's latest revision on its channel, as `upgrade --version <its
  version>` does (the other flags passed on), so a fix shipped as a revision reaches it; with no
  later revision it is left as it is, with a note (`<dir> already runs <name> <version>
  (revision N)`), and so it is when the channel cannot be read (logged: `no later revision of
  <name> <version> looked for: …`) or holds only an earlier revision (its own yanked). An
  install made with `--offline`, which never pulls, is left as it is, with a note. The upgrade follows how the
  install pulls its images (an install made with `--offline` upgrades from loaded images). Then it pulls the images, dumps the database (`pg_dump` as the migrate role, gzip,
  `snapshots/<UTC time>.sql.gz`, 0600; a failed dump stops the upgrade), records the schema
  version, keeps the running release — compose files, `.env`, `provision.env`, installer — in
  `.stowage-prev/` (one level), writes `.env` (every recorded value kept but an input the release
  no longer declares, on any shape — dropped, with `input KEY is not in <name> <version>: dropped`,
  never asked again —; the release's added inputs, secrets and safe defaults decided as at a
  first install; version, revision and images rewritten), starts it and provisions (a key the
  previous release's hook declared in `returns` and this one's does not is dropped from
  `provision.env`, with `the provision hook of <name> <version> no longer returns KEY: dropped`).
  `rollback` takes both back: the previous release's values `.env` lacks, and what its hook
  returned. A failure names the rollback. `--dry-run` prints the
  values that would change (secrets redacted).
- `upgrade --version V`, run by any installer of the project (the one the install keeps,
  `<dir>/<short>-stow`, is the usual one), fetches V's installer and runs its `upgrade` with the other
  flags given (`--set`, `--answers`, `--ports`, `--no-input`, `--accessible`, `--dry-run`,
  `--no-provision`); its exit status is the command's. V is looked up among the release
  manifests (`<prefix>/<name>-release`) of the registry the install pulls from
  (`STOWAGE_REGISTRY`, a mirror included: nothing is read from anywhere else), on the channel of
  the install's release: V's latest revision that is not yanked, V matched by the scheme each
  release records (any spelling it holds equal: among equal revisions the one typed, else the first tag in sorted order; a
  `command` scheme takes V as written), among those signed by a trusted key (**Installs
  verify**, above). Before anything is fetched, the install is ordered
  against it by that scheme: an install already on that release is left as it is, with a note;
  one on a later release or revision is refused, naming `rollback`, as is an install whose
  version that scheme does not read. A `command` scheme is not ordered (the release's `upgrade`
  says so): any other version is fetched and run.
  Otherwise the installer is pulled by the digest the manifest records
  (`<prefix>/<name>-installer@sha256:…`), written to `<dir>/.stowage-next-<random>/<short>-stow`
  (0700), and refused unless it carries that release: checked by its release manifest alone,
  so the installer of a release a newer Stowage built runs, its spec read by that installer
  (Docker and Kubernetes alike). The reads use the pull account as the
  image pulls do: on the project's registry, the `token` account, or anonymously with `none`;
  otherwise (a mirror, `docker-config`, no password) the Docker client's own login. It refuses V
  when it is not published on the channel, when any release manifest of the repository cannot be
  read (one from a newer Stowage, or with a scheme this installer does not build: it could be
  V's latest revision; run that release's installer instead), only yanked revisions are left (naming the reason),
  or the manifest records no installer; an install made with `--offline` (run the new installer
  instead); and `--offline`; and a V with no revision signed by a trusted key, or whose
  signatures cannot be read (**Installs verify**). When the fetched installer fails, it is kept (its messages name
  it); otherwise it is removed once it ran (a removal that fails is reported and leaves it).
- **Schema baselines.** A release's schema baseline is the `sql` schema-compat's `baseline`
  (absent, 0 and every other provider are baseline 1; a release without a database has none and
  is never checked). Two releases at different baselines share no schema: `upgrade` refuses
  before anything changes (no dump, no pull), naming the way across, `reset-db --new-baseline`
  (`… only with its data wiped: `<installer> reset-db --new-baseline --dir D` wipes it and installs
  <v>`); `rollback` — and so the VM's boot going back and its health gate, which then
  keeps the boot — refuses a previous release at another baseline, decided from the two releases'
  specs before any image is pulled. A baseline that does not read is logged and not checked, like
  the rest of the schema check (`the schema baseline of <v> is not known (…): not checked`). The
  baseline of another release — the one the install runs, the one it goes back to, the one a
  helm release runs, the one a VM upgrade writes — is read from its installer's `spec.json`
  alone (`database.schema_compat`, wherever any Stowage since 0.4.1 writes it), every other field
  ignored: a release an older or newer Stowage built, whose spec this one refuses whole, still
  tells it. On Kubernetes the
  baseline of the release a helm release runs is read from that release's installer (its spec
  travels in it), fetched by the tag the release records from the registry it was installed from
  and checked against this installer's keys (an installer carrying none checks no signature, as
  `upgrade --version`): `k8s upgrade` refuses another baseline, and
  `k8s reset-db --new-baseline`, run by the new release's installer, is the way across (it wipes
  the data). One that cannot be read is logged and not checked by `upgrade`. On Docker,
  `reset-db --new-baseline` (below) is the way across; on the VM, `upgrade` refuses another
  baseline before it writes the OS (`… its data cannot carry over, so its OS is not written;
  `<short>-stow reset-db --new-baseline` wipes the data and installs <v>`), and the VM's
  `reset-db --new-baseline` is the way across.
- **Across Stowage versions.** The installer an install keeps is identified by its release
  manifest alone, whichever Stowage built it; its spec is read only when this Stowage reads the
  payload as its own (none is read leniently or migrated). A payload this Stowage refuses is put
  to that installer itself (`<short>-stow --help`, which reads its own payload before anything
  else, its output discarded, given 30 s), once its manifest names the release expected: one it
  reads is another Stowage's format; one that does not answer in time stops as below (`…, and
  whether that installer reads it is not known (it did not answer within 30s)`); one it does
  not read either (a spec cut short, another project's) stops `upgrade`, `rollback` and `audit` before
  anything changes (`the previous release's installer: payload …, nor does that installer read
  it (…)`, or `<short>-stow is not the installer of …`): running that release's installer again
  keeps a sound copy. Such an installer carrying no key where this Stowage looks is not taken as
  carrying none: `upgrade --version` and `audit` refuse (`the keys the install trusts cannot be
  told; run its own installer`) rather than trust this installer's keys. So an install of a release whose spec an
  older Stowage wrote in another format (a key moved since) upgrades to one this Stowage built,
  and what only that spec tells is not checked: its schema baseline, the inputs and provision hook
  returns it had that the new release drops, which are kept (`the inputs and provision hook
  returns <name> <v> removes are not known (…): kept`), and, going back, whether it reads a
  newer schema (`going back unchecked: whether <v> reads the newer schema <s> is not known:
  another Stowage built it`). The rollback barriers, the current release's own, still refuse.
- `rollback`, run by the installer the install keeps, goes back to `.stowage-prev/`: the previous
  compose files, installer, version and images; every other value stays as it is now
  (a secret the upgrade added is kept for the next upgrade). The previous release is then started
  by its own installer (`starting <name> <v> (revision <r>) with its own installer: …`): on Docker
  a re-run of it (`<short>-stow --dir <dir> --no-input --no-provision`, and `--offline` for an
  install that runs loaded images), on the VM its `boot` (which runs the provision hook, as every
  boot of the VM does); a release is always started by the
  Stowage that built it. Should that start fail (another run holding the install meanwhile), the
  install already records the previous release: running its installer again starts it. The release it leaves is kept in
  `.stowage-failed/`. Like an upgrade it switches `.env` before the installer, and keeps
  `.stowage-prev/` until the end: a rollback that stopped is finished by running it again (so is
  an upgrade that stopped before it switched `.env`: the kept release's compose files, installer
  and start come back). When the database moved since the upgrade, it refuses — naming the
  snapshot — if a rollback barrier came since, or the previous release does not read a newer
  schema; a check that cannot run (an unreadable database, no answer) does not refuse, and says
  so. `--dry-run` pulls the previous images only when the check reads them. One install run,
  provision, upgrade, rollback or reset-db works on an install at a time, reading it under its lock; a hook or schema container a
  killed run left is refused, naming the `docker rm --force` that clears it.
- Once the stack is up, the provision hook runs (`--no-provision` skips it; `provision` runs it
  later). Each `KEY=VALUE` line it prints is recorded in `provision.env` (0600) as it arrives, and
  copied to the hook's `outputs` file (0600, `KEY=VALUE`) at the end, even when it fails; the
  terminal shows the keys and the file, never the values. A re-run of the installer runs the hook
  again. A failed hook leaves the stack running and names the command that continues it.
  One run at a time: a second is refused while one is in progress, as is a run while a stopped
  one's container is still there; a run the installer stops (timeout, interrupt, output past its
  bounds) has its container removed. `provision --set` corrects an input only the hook reads —
  one its `env` lists and the compose file never names, no route's domain, and none at all when the stack may read `.env` itself (an `env_file`; a mount of
  `.env`, of the install directory or a directory above it, or of a source with `$` or `~`, but one that is exactly a file input's `${STOWAGE_INPUT_FILE_<KEY>}` or a secret's delivered as a file `${STOWAGE_SECRET_FILE_<KEY>}`; a top-level `secrets` or `configs` entry whose `file` is such a path; a
  volume with `driver_opts`) — recorded in `.env`; or it sets a value the hook returned before or
  declares it returns (a key `provision.env` holds, or one `hooks.provision.returns` lists, that
  the install does not record), recorded in `provision.env` and handed to the next run, where an
  empty value removes the key so the hook runs without it. Both are recorded once no other run
  holds the install, before the hook runs; any other key is refused, naming the ones it takes. A
  failed hook's message names `provision --set KEY=VALUE` and the keys it returned or declares.
  Any other input is changed by a re-run with `--set`, which brings the stack up again; the
  refusal names that command.
- `secrets export --recipient age1…` (repeatable, 1 to 16 keys) writes the install's secrets for a
  disaster recovery: the generated ones (a rotated secret's `KEY_NEXT` and `KEY_PREVIOUS` with
  them), the secret inputs (the Cloudflare token among them), what the provision hook returned
  (`provision.env`) and, for an install in the `internal` TLS mode, the front's local CA (its
  root's certificate and key, PEM, read from the front's data volume by a container of the
  front's image, running or not; a front that made none yet is exported without it, said, and one
  that cannot be read — Docker down, the image gone — is a warning: the secrets are exported
  without it, and a machine made again from them issues from a new root), as
  YAML (`project`, `secrets`, `provision`, `front_ca: {cert, key}`), encrypted to those age public
  keys and ASCII-armored, into `-o` (default `<name>-secrets.age`, 0600, never over a file; `-`
  for standard output). No private key is needed on the machine; keep the file off it.
  `age --decrypt -i KEY <name>-secrets.age > secrets.yaml` gives what `--secrets` takes back.
- `--secrets FILE` (`-` for standard input) makes a first install with those decrypted secrets,
  a machine made again: every generated secret the file holds is taken, not generated (one it
  lacks is generated, and the run says so); its secret inputs are given (under `--set` and
  `--answers`, over the environment); what the hook returned is written to `provision.env`
  before the stack starts, so the hook's next run is given it. Those values belong to the old
  install's data, so the hook does not run on the new, empty database — not by this run, a
  re-run or a boot: the stack starts unprovisioned, `status` says a backup is awaited
  (`.stowage-restore-awaited`), and `sudo <short>-stow restore OBJECT` (the VM) loads it and runs
  the hook; on Docker, which has no `restore`, a backup loaded by hand, then `provision` runs it,
  as it does an empty database. A file without provision values awaits nothing. The front's
  local CA the file holds is recorded (`.stowage-front-ca`, 0600) for an install in the
  `internal` mode (in another, it is left out, said), and put in the front's data volume before
  the front first starts: a front container already there is removed, the root and its key
  written, the intermediate and the certificates an earlier root issued deleted (Caddy makes them
  again from the root), then the record deleted; a start that fails before keeps it, and the
  next start (a re-run) does it. So every client that trusted the old root (a browser, a
  service `publish_ca` gives it to) still trusts the new machine. Refused: a file still encrypted,
  one of another project, an unknown field, over 1 MiB, a `front_ca` whose certificate is not a
  CA's or whose key is not its own, and `--secrets` on an install that
  exists (it keeps its own). A key that is no secret of the release is left out, and said.
  Docker and the Kairos VM; on Kubernetes, not yet.
- `secrets rotate KEY` replaces a generated secret whose spec entry has `rotation: true` (any
  other is refused, naming the ones that have it), in three passes, each recorded in `.env` and
  followed by the stack brought up again (the services reading a changed value are recreated)
  and the provision hook run, under the install's lock:
  - `--stage` records a new value as `KEY_NEXT`, which the services accept and do not use
    (refused while one is staged, or while `KEY_PREVIOUS` holds one: finish first);
  - without a flag, `KEY_NEXT` becomes `KEY`, the old value is kept as `KEY_PREVIOUS` (still
    accepted) and `KEY_NEXT` is emptied (refused when nothing is staged);
  - `--finish` empties `KEY_PREVIOUS`; with it empty already, it changes nothing and succeeds.
  `--finish` while a new value is staged is refused. A pass run again once its state is recorded
  (its restart failed) changes nothing and brings the stack up again. Between two passes every
  service runs with both values it may meet, so none refuses the other age's tokens. `KEY_NEXT` and `KEY_PREVIOUS` are recorded empty from the first install (or the
  upgrade that adds the secret); the compose file passes them to the services that check the
  value. `KEY_ACCEPTED` is recorded with them, the one of the two that holds a value (`KEY_NEXT`
  while one is staged, else `KEY_PREVIOUS`; empty outside a rotation, as the two are never both
  set), each pass and each upgrade writing it again: a service that accepts one more value beside
  `KEY` reads `${KEY_ACCEPTED}` alone. It is a secret, as the two are.
- `secrets set KEY` records a value the operator chooses for a generated secret whose spec entry
  has `settable: true` (any other is refused, naming the ones that have it): a login's password,
  a local identity provider's user, set to a known value or reset after a leak. The value is read
  from standard input, one trailing line break dropped (`printf '%s\n' "$pw" | … secrets set
  KEY`), or asked, unechoed, when standard input is a terminal (`--accessible`: a plain line
  prompt; without a terminal to ask on, it is refused, naming that pipe); never an argument. Refused before anything changes: an empty value, one over 4096
  bytes, one `.env` cannot hold (a single quote, a line break, a trailing backslash), and, for a
  front route's login password (`basic_auth.secret`), one over the 72 bytes bcrypt takes. It is
  recorded in `.env`, then the stack is brought up again as for a changed input (the services
  reading it are recreated, a login's hash made again) and the provision hook run, under the
  install's lock; the value is never printed. The same value again records nothing and brings
  the stack up again. `secrets export` carries it from then on. A secret the database or the
  data was made with (a database password, an encryption key) is never `settable`: the
  database keeps the password it was created with (lint refuses `settable` on a secret the
  database's service reads in its `environment` — by name or interpolated, an `env_file` not
  read: `secrets[N].settable: KEY is read by the database's service …`). Docker and the Kairos VM; on Kubernetes,
  not yet.
- `secrets show KEY` prints the value the install records for a secret with `settable: true` (a
  login an operator signs in with; any other is refused, naming the ones that have it), one line
  on standard output, which must be a terminal: into a pipe or a file it is refused before the
  value is read, so it never lands in a log or a script's output (`secrets export` carries the
  secrets off the machine, encrypted). On Docker it reads `.env`, so it needs the user that can
  (root, for an install under `/opt`); on the VM the host daemon answers it on the operator's
  socket alone (`POST /v1/secrets/show {"key"}` → `{"value"}`, never logged; 400 for a key that is
  not settable, 404 for one the install does not record, 409 for an install running another
  release than the OS's); on Kubernetes (`k8s secrets show KEY`) it reads the release's Secret
  `<release>-stowage` (`<release>-stowage-secret-files` for a secret delivered as a file). Each
  shape shows only with the installer of the release the install runs (another is refused,
  naming the one to use), whose spec says which secrets are settable. The verb is there only when the spec has a settable secret, and an
  install's, a re-run's and an upgrade's closing report then names them on a `logins` line with
  the command that shows one (`<short>-stow secrets show KEY --dir D`, `sudo <short>-stow secrets
  show KEY` on the VM, `<short>-stow k8s secrets show KEY …` on Kubernetes).
- `reset-db` takes the install back to its first setup, run by the installer of the release it
  runs (another is refused, naming the one to use). It refuses, before anything changes, a
  `data.reset` volume the release's compose file does not declare at top level, declares
  `external` or names with an interpolation (`name: ${…}`), and an install whose `.env` records no `COMPOSE_PROJECT_NAME`. It stops the stack
  (`docker compose down`), deletes the Docker volumes of the `data.reset` volumes (found by their
  Compose project and volume labels, or one Compose adopted without them by the name Compose gives
  it, `<project>_<key>` or the compose `name:`, when a container of the stack (read before `down`)
  mounts it — one of that name another Compose project owns, or one no container of the stack
  mounts, is refused, never deleted; one never created is named, not an error, and a lookup that fails
  otherwise fails the run; the `keep_on_reset` ones and the front's stay), deletes `provision.env`, the
  hook's `outputs` file and `.stowage-restore-awaited` (the restored values gone, nothing awaits a
  backup), then starts the stack — which migrates an empty database — and runs the
  provision hook as on a first install, from the values `.env` records (unchanged: secrets,
  inputs, TLS, ports). Nothing is dumped first, and the service is down meanwhile. It asks on a
  terminal (default no), without holding the install's lock, then checks again under it; without
  a terminal, or with `--no-input`, it refuses unless `--yes`.
  `--dry-run` prints what it would delete. A `data.reset` that names nothing is refused. It holds
  the install's lifecycle lock; one that stops after the stack went down is finished by running
  it again.
- `reset-db --new-baseline`, run by the installer of a later release at another schema baseline
  than the one the install runs, crosses it: it checks as `upgrade` does (the release the
  install runs read from the installer it keeps; every refusal of the upgrade — front, ports,
  hook, an upgrade that stopped, Docker, the L4 routes — before the question, and again under
  the lock after it; the inputs this release adds asked as an upgrade asks them, its images
  fetched), then wipes the data as `reset-db` does — the stack
  down, the `data.reset` volumes deleted, what the hook returned forgotten, nothing dumped — and
  installs this release with the values `.env` records, provisioned afresh. `.stowage-prev/` and
  `.stowage-failed/` go only once the wipe is done (a wipe that fails leaves them), and with the
  `.env` switch the install records the releases it crossed from and to in `.stowage-crossed`
  (`<from version> <revision> <to version> <revision>`), which the next upgrade removes. A new release naming
  no volume in `data.reset` crosses all the same when the old one names some. No release is kept to
  go back to: `.stowage-prev/` and `.stowage-failed/` are removed. It refuses, before anything changes, the
  release it carries (`… already runs <name> <v> (revision <r>): --new-baseline installs another
  release, which this installer does not carry; reset-db without it wipes this one`), an earlier
  release, a release at the same baseline (`… --new-baseline only crosses a baseline; `<installer>
  upgrade --dir D` moves it`) and a baseline that cannot be told (`the schema baseline of <name>
  <v>, which the install runs, cannot be told (the log says why): --new-baseline never wipes on a
  guess`). It wipes the `data.reset` volumes of both releases — the one the install runs, whose
  stack holds the data (read from the installer it keeps, its `data.reset`, compose file and
  provision outputs file alone when another Stowage built it; one that does not tell what to
  wipe is refused), and this one's, but for a volume this release keeps on a reset
  (`keep_on_reset`), which is kept whatever the old one said; it forgets both releases'
  provision outputs files. Neither
  naming a volume is refused (`… --new-baseline has nothing to wipe`).
  It asks as `reset-db` does (`… then install <name> <v>, at the new schema baseline,
  with the same values`); `--dry-run` prints that and changes nothing. An install that moved to
  another release after the answer is refused, nothing wiped. Should the new release not come up,
  the error names the re-run that starts it again: its data is already wiped.
- The front is the Caddy Stowage adds (`stowage-front`), the release's `front` image. An
  installer refuses a release of a project with a front that records no front image. `STOWAGE_TLS_MODE` records how it serves.
  It runs confined, as every service Stowage adds: as a user of its own (uid and gid 65532, or the
  `data.volumes.<V>.uid` of the volume `publish_ca` names, which it writes in), with no capability
  (`cap_drop: [ALL]`), `no-new-privileges`, a read-only root filesystem, `/tmp` and `/config` (each
  a `tmpfs`; Caddy never persists its configuration: `persist_config off`) its only writable
  paths besides its volume `stowage-front-data` and the published CA's; it binds 80 and 443 in its
  container as its network namespace lets any user (`net.ipv4.ip_unprivileged_port_start: 0`, as
  Docker's engines set by default). It mounts nothing of the install directory but
  `stowage-front-client-ca/` (0755, its `ca.pem` 0644, whatever the umask): it reads its
  configuration and a `byo` certificate from its own volume, where each start copies them. Each
  start, before the stack's containers are made when the front's volumes are there and no volume
  awaits its uid (after, when one is not, `up --no-start` making it, or after the volumes are
  given their uids), runs the front's image as the front's user with no
  capability (`docker run --rm -i --pull never --network none --read-only --user <uid>:<uid>
  --cap-drop ALL --security-opt no-new-privileges:true`), given on its standard input a tar of
  the Caddyfile (`<dir>/stowage-front.Caddyfile`, 0600: it holds the logins' hashes) and, with
  `byo`, the certificate's files (`<dir>/tls/`; a `byo` install whose `<dir>/tls/cert.pem` or
  `key.pem` is gone is refused: `the byo certificate's copy is gone (…): a re-run with --cert FILE --key FILE
  gives the front one`): it unpacks them into `/data/.stowage-in` and renames them to
  `/data/stowage-Caddyfile` and `/data/stowage-tls/` (0600 and 0700, a copy no longer given
  removed), all as that user, so no link in its volume leads a write anywhere that user could
  not write — unless it finds what it cannot do (below), which it says before writing anything.
  A running front of another user (an older release's ran as root, or one whose inspection
  fails) is then stopped (`stopping the front before giving its volume to its user`) and the
  volume judged again (unless that first run refused it). Only for what that run says it cannot do does root run, the front stopped
  first, with the capabilities to change owners and
  reach and remove the files it changes alone (`--user 0 --cap-drop ALL --cap-add CHOWN
  --cap-add FOWNER --cap-add DAC_OVERRIDE --cap-add DAC_READ_SEARCH`, off the network, its root
  read-only): when a file in `stowage-front-data` is another user's (a volume an older front
  filled as root, or one of another uid) it gives the volume to the front's user (`chown -hR`,
  no link followed), and the user's run takes the files again; and when the published CA's
  volume's directory is root's (as Docker makes a volume an image puts nothing in), it gives the
  front's user that directory alone (the files in it stay as they are; the front's temporary copy
  `.ca.crt` an interrupted earlier one left removed). A directory another user holds — a
  service's image made it so, or an earlier release's front ran as another uid there — is refused
  by the user's run, before it writes anything — the first run, before the front is stopped, or
  the run judging again after a front of another user was stopped (the start-first services,
  started first, and the stack's containers when they were made first, as above, stay as that
  made them) — (`front.publish_ca.volume: the volume <V>'s directory belongs to uid <N>, not the
  front's (<uid>), which writes its CA there as its own user`, exit 3), the fix naming the
  operator's way out when the volume's services write nothing there (`docker run --rm --user 0
  --entrypoint chown -v <volume>:/v <front image> <uid>:<uid> /v`) and the project's
  (`data.volumes.<V>.uid: <N>`, the uid holding it, which the front then runs as — offered where the
  project declares no uid for the volume). The user's
  run removes the front's temporary copy (`.ca.crt`) another user holds in a directory that is its
  own (an interrupted earlier front's, run as root; one its own user holds, the front overwrites).
  Whether the front's volumes are there is `docker volume inspect`'s answer. A
  front the step stopped (once its volume is to change hands) stays stopped when the start fails,
  as a failed start leaves its stack, until a start whose error's fix is applied.
  Any other failure fails the start (`giving the front its files: …`, `giving the front its
  volumes: …` — a file of another user's, or the CA's directory, still there after root's run
  among them —, `the front's container: …` when the engine cannot list it, `the front's volume
  <volume>: …` when it cannot say whether a volume is there). A local CA `install --secrets`
  restores is written into the volume after that step, as the front's user. The front's image
  holds `/data` and `/config` as its
  user's (built with BuildKit, as `ship` builds it); each start's step is what guarantees the
  volume's owner. In `internal` mode the local CA's root is never put in the front's own trust
  store (`skip_install_trust`); `secrets export` reads the local CA as root with
  `DAC_READ_SEARCH` alone. A front of L4 routes alone in `acme` mode, which publishes HTTP alone,
  turns off the TLS-ALPN challenge (`cert_issuer acme { disable_tlsalpn_challenge }`).
  The front's host ports are those something serves on, published by Docker and opened by the
  VM's firewall alike: HTTP's and HTTPS's with a front route in a TLS mode; HTTP's alone for a
  front of L4 routes alone in `acme` mode (Let's Encrypt's HTTP-01 challenge for their
  certificates); neither otherwise (`--http-port` and `--https-port` are still recorded, and
  checked free only once something serves on them: an install's, a re-run's moving to such a
  mode and an upgrade whose release serves them first, which refuses ports held on the host,
  naming the re-run that moves them).
  On each route, in this order:
  - `allow_input`: a source address the input's list (IP addresses and CIDRs, separated by commas
    or spaces) does not hold gets 403 — everyone while it is empty. The source is the address the
    front sees: through Docker's published ports, a client on another host keeps its own, while
    this machine's own requests may come from the Docker bridge's gateway. So does every IPv6
    client while the stack's IPv6 is off (no `--ipv6`): Docker's userland proxy hands it to the
    front as the gateway of the stack's network. On a host with a global IPv6 address, an
    install, re-run or upgrade with IPv6 off warns, once the stack is up, of each list holding
    that gateway, read from the stack's default network (`warning: route R: K=…: S holds the stack
    network's gateway G, as which every IPv6 client passes …: install --ipv6, or list narrower
    sources`, S the list's source that holds it); when the gateway cannot be read, of each list
    overlapping Docker's default pools, `172.16.0.0/12` or `192.168.0.0/16` (`…: S holds an
    address of Docker's default pools, as which …`).
  - `basic_auth: { user, secret }`: the client logs in, as the user named (1 to 64 letters,
    digits, `.`, `_` or `-`) with the password the key `secret` names holds — a spec `secrets:`
    key (one with `rotation` is refused: the front holds one password) or a `secret: true`
    input; lint refuses any other key, and `basic_auth` beside `forward_auth` on one route.
    Caddy's `basic_auth` answers 401 without the login; the request's `Authorization` header is
    then removed, so the upstream never sees the password. The Caddyfile holds the password's
    bcrypt hash (cost 10), which `<dir>/.stowage-front-logins` (0600) keeps by route and makes
    again only when the password changes (a new hash would change the front's configuration,
    and restart it, at every start). While the password is empty, or longer than the 72 bytes
    bcrypt takes, the route answers 403 to everyone (the second logged at each start: `route R's
    password (K) is N bytes, over the 72 bcrypt takes …`); an input holding one is checked when
    given, and refused over 72 bytes. On Kubernetes the chart's ingress serves the routes: Stowage asks for no login
    there.
  - `strip_headers` are removed, whatever the client sent, and so are the headers
    `forward_auth.copy_headers` lists (even while forward auth is off), so a client never forges
    one.
  - `forward_auth`: while the input `address_input` names is not empty, each request is first
    sent to that service (`[http[s]://]host[:port]`, an IPv6 address bracketed, `[fd00::1]:9091`, neither zoned nor
    IPv4-mapped — one outside the stack needs `--ipv6`, the front's network otherwise having no
    IPv6; at the path `uri_input`'s value gives, `/`
    when it is empty) with Caddy's `forward_auth`; its non-2xx answer is the client's, and on a
    2xx the headers `copy_headers` lists are taken from its answer onto the request.
  - The upstream serves. An upstream the front cannot reach is tried again for 5 s (Caddy's
    `lb_try_duration`), its name resolved at each try: a container that stops — a restart, a
    start-first overlap's old one — costs a request a wait, not an error (a `GET` is also tried
    again after the connection failed; another method only when it could not connect).
  The values are checked when given (an address, a path `/…`, IPs and CIDRs) and written into
  the Caddyfile, so a change is a re-run's `--set`.
- `publish_ca: { volume: V }`: the front copies the CA its certificates chain to into the
  project's volume `V` as `ca.crt` (0644), for the services that mount it (read-only) to trust:
  with `internal` the local CA's root (as soon as Caddy made it), with `byo` the `--ca FILE`
  given (PEM certificates, one of which the certificate's chain must end at; copied into
  `<dir>/tls/ca.pem`). The front writes it as its user: the volume's `data.volumes.<V>.uid` when the
  project gives one (lint refuses `0`: the front never runs as root), else 65532, given the
  volume's directory at each start. The allowlist takes no IPv6 zone (`%eth0`). In the other modes, and with `byo` without `--ca`, it removes `ca.crt`.
  `--ca` is refused outside `byo` and for a project without `publish_ca`.
- `client_auth: true` on a route (or `tls.client_auth` on an L4 route the front terminates TLS
  on, which closes the connection where a front route answers 403, and counts as such a route
  for `--client-ca` and the messages below): the front requires a client certificate on its domain, one
  chaining to the install's client CA, and verifies it (Caddy's `client_auth`, `mode
  require_and_verify`), before anything else the route does; the TLS handshake of a client
  without one fails. Caddy then also refuses a request whose `Host` is not the domain the TLS
  handshake named (421). `--client-ca FILE` gives the CA (PEM CA certificates, one at least, and
  nothing else: a private key, a certificate that is not a CA's, text after the certificates, or
  no certificate is refused, naming the file), its certificates alone copied (encoded again) into
  `<dir>/stowage-front-client-ca/ca.pem` (0644), that directory mounted read-only into the front, whose configuration digest it enters (a new CA recreates the front);
  a re-run's `--client-ca` replaces it, `--client-ca none` removes it; a file there that holds no
  CA certificate any more is taken as none, and said (`<file> <why>: <routes> refuse(s)
  everyone`). Until one is given — `vm
  create` takes none: give it to the VM's `install` over SSH — the route answers 403 to everyone,
  and the install says so (`<routes> refuse(s) everyone until --client-ca FILE gives the CA their
  client certificates chain to`); in loopback mode, whose plain HTTP carries no client
  certificate, it always does (`<routes> refuse(s) everyone in loopback mode`), a client CA given
  being kept for a TLS install. The other routes are left as they are. `--client-ca` is refused
  for a project without such a route (`--client-ca: no route of <name> asks for a client
  certificate (client_auth)`). On Kubernetes the chart's ingress serves the routes: Stowage asks
  for no client certificate there.
- In a TLS mode each route's domain is an alias of the front on the stack's default network
  (`stowage-front.yaml`'s `networks.default.aliases`), whatever the front's ports: a service
  calling a route by its URL, as its clients do (an identity provider's issuer URL), reaches the
  front, on 443 in its container or on the URL's moved port, which it serves too. On the VM an L4
  route's service, on the host's network where that alias is not, maps each route's domain to
  `127.0.0.1` instead (`extra_hosts: <domain>:127.0.0.1`), where the front publishes its ports
  (on every address: `services.<name>.front_hosts: host-gateway` maps them to Docker's
  `host-gateway` address instead, `none` maps none, as for a service of `host_network`);
  with `acme` on a moved `--https-port` its URLs name 443, which nothing on the VM holds, so it
  reaches a route by name only through the operator's forward.
- Loopback mode (`none`, the default when the project offers it and no domain is given): each
  route on its own port of a 127.x address, plain HTTP, a route with a `path` serving its prefix
  alone there (another path answers 404), its URL `http://<address>:<port><path>`;
  `loopback_env` relaxes safe defaults.
- L4 routes (`l4.routes`): in every mode, loopback included, each route's host port is published
  on every address to its upstream, over its protocol, in the front's compose file
  (`stowage-front.yaml`; compose adds it to the service's own); the front never sees that traffic.
  A route with a `bind_input` whose input holds an address is published on that address alone
  (`host_ip`), so installs on one host can hold one port on different addresses; the input is
  `advanced` (never asked: `--set`, `--answers` or the VM's settings give it; a re-run giving it
  empty publishes on every address again, as does an unspecified address, `0.0.0.0` or `::`), and
  its value must be an IP address without a zone (`"<v>" is not an IP address without a zone`; an
  IPv4-mapped IPv6 address is its IPv4 address). The port is then probed on that address alone,
  and an upgrade counts the port the stack publishes there as its own (the address an upgrade's
  `--set` or `--answers` gives a route the release adds included; an address the stack holds the
  port on, there or on every address, is never probed; the wildcard always is). A re-run
  changing a route's address probes the port at the new one the same way. The address must be
  one of the host's (when the host's addresses can be listed): an install, a re-run or an upgrade
  refuses another (`<v> (<KEY>, the L4 route <name>'s address) is not an address of this
  host`).
  With `--ipv6`, Docker forwards them over IPv6 too. A route with an `address_input` gets
  `STOWAGE_L4_<ROUTE>` = `<the input's value>:<port>` (an IPv6 address bracketed,
  `[fd00::7]:7443`), recorded in `.env` and empty while the input is; the address is never
  guessed (behind NAT the host's own is not the one clients dial). A first install refuses a
  route's port a process of the host already holds — a TCP port one listens on, a UDP port a
  socket is bound to on any address, probed as Docker binds it, on the wildcard addresses (a
  port below the unprivileged ones, the installer run as a user, is not checked); a route to a
  service on the host's network (`host_network`, and every route on the VM), which no one
  publishes, is probed where that service binds, over its protocol: the host's addresses and the
  loopback's `127.0.0.1` and `::1`, never the wildcard (so a local resolver's stub on
  `127.0.0.53:53` keeps no DNS service from the host's addresses) — (`port(s) 7443 (data,
  held on 0.0.0.0) already in use on this host: the project's L4 routes publish them`, naming the
  addresses the kernel's socket tables, `/proc/net`, show holding the port — every one when the
  wildcard was probed, else those probed and the wildcard; `ss -lntup` names the process) — and so does an upgrade for a route the
  stack does not publish already; an install, re-run or upgrade refuses front ports on one of them
  (an upgrade's refusal says to move the front with a re-run of the installer the install runs,
  then upgrade) (`the front's https port 7443 is the L4 route data's: choose another with
  --https-port`, `--http-port`, or `--ports` in loopback mode). Stowage never probes them: the
  stack's health is the `health` provider's. On Docker the host's firewall is its operator's: a
  route's `allow_input` holds once the operator consents (`tune --apply`, see "A Docker host's
  gateway" under `tune`), and the end of an install, re-run or upgrade logs each one's exposure
  (`L4 route echo: tcp/17007 on every address, open to whoever reaches this host`; with an
  allowlist `… open to whoever reaches this host: ECHO_SOURCES's sources hold once `<short>-stow
  tune --apply` consents`, consented but not held (no start as root applied it since this
  release or these values, or the last one failed on it) `… open to whoever reaches this host:
  ECHO_SOURCES's list is not held yet: a start as root applies it, on IPv4 alone (consented;
  `status` says what the last one did)`, held once such a start brought the stack up `… on IPv4's every
  address, from the IPv4 sources ECHO_SOURCES holds (the install's chain in DOCKER-USER, applied
  by each start as root)`; on one address `on 192.0.2.7 alone`), as `status` says it; on the VM the firewall
  opens each route's port, from its `allow_input`'s sources alone when it has one (see "The
  firewall" under the installer on the Kairos VM).
- L4 routes the front terminates TLS on (`l4.routes[].tls`): the route's port is the front's,
  published (on its bind address when it has one) to a port of the front's container of its own
  (20000 plus the route's index among `l4.routes`, apart from the front's 80, 443 and loopback
  ports), never the upstream's. The front (Caddy's `layer4` app, from caddy-l4, in the same image) closes
  a source the route's `allow_input` does not hold before any handshake (every source while the
  list is empty; it is the front's on every shape, never the host's firewall, so a Docker host's
  route needs no `tune --apply` consent and its port is published on every family; without
  `--ipv6` an IPv6 client reaches it as the stack network's IPv4 gateway, a list holding which
  is warned, as a front route's), then terminates TLS with the certificate Caddy holds for the
  route's domain — a client sending no server name (SNI) given it too, a domain no front route
  serves being a site of the front's of its own that answers 404 over HTTPS, so Caddy obtains,
  renews and stores its certificate as a route's (`acme`'s challenges on the front's ports 80 and
  443; `byo`'s certificate must cover it: an install, a re-run and an upgrade refuse one that
  does not, naming the domain and the route) —, selects the route's `alpn` protocols alone in the
  handshake (none without: Caddy's own `h2` and `http/1.1` would answer for an upstream that never
  negotiated them; a front terminating such a route starts from its Caddyfile adapted to JSON,
  where an empty list can be written: a front whose adapted configuration still names
  `stowage-no-alpn` stops with `stowage-no-alpn left in /tmp/stowage-caddy.json: the front's ALPN
  cannot be cleared`, never serving it), requires with `client_auth` a client certificate
  the install's `--client-ca` issued (everyone closed while none was given), and forwards the
  plaintext stream to the upstream by its name (on the VM, the host's gateway). The domain is
  asked as a front route's (`Domain for <route>`); an upgrade to a release adding a route or its
  `tls` refuses a domain nothing gives (`… whose domain KEY nothing gives: --set KEY=<domain>`).
  In loopback mode the front passes the stream in the clear on the loopback address alone
  (`STOWAGE_LOOPBACK_ADDRESS`, never every address): TLS and the sources are the proxy's in front
  of the install's — the route takes no allowlist there, as the front would see every client as the
  stack network's gateway —, and a route with `client_auth` closes everyone, as plain TCP carries
  no certificate. `status`
  and the end of an install say it (`db  tcp/7443 · on every address, TLS by the front for
  db.example.com, from the sources DB_SOURCES holds (the front closes any other)`; in loopback
  mode `· on the loopback address alone, in the clear: TLS and the sources are the proxy's in
  front of this install`), on the VM
  `on the interfaces <roles> alone` for a route opened on its `interfaces`, and a route the front
  closes to everyone as such (`closed to every source: …`: its allowlist empty, its domain empty,
  a client certificate asked with no client CA held — none given, or a file holding no CA
  certificate — or in loopback mode, which carries none), by the same rule the front applies; a
  client CA that cannot be read is `unknown: the client CA cannot be read: …`. On the VM the firewall opens the route's port from every source (none in loopback
  mode), and the upstream's port from the stack's bridges alone; the upstream still runs on the
  host's network, on a port other than the route's.
- L4 routes on the VM: each route's service runs on the host's network (`network_mode: host` in
  `stowage-content.yaml`), so no Docker port publishing, NAT, bridge or virtual interface sits
  between its clients and it: it listens on the route's port itself (lint refuses another
  upstream port with a `kairos` shape, and on a route with `tls` the route's own: the front holds
  it, and the service listens on its upstream port, which the front reaches it at), and
  `stowage-front.yaml` publishes nothing for it. Its
  namespace is the host's, so it is given none of the network settings a container's own
  namespace takes. The rest of the stack still reaches it by name: every other service with a
  network of its own (the front included), and the provision hook's and a `command` provider's
  containers, map its name to the host's gateway (`extra_hosts: <service>:host-gateway`); the
  firewall accepts, from the stack's bridges (`iifname "br-*"`), the ports it is reached on
  inside: its routes' ports (a route with `tls`: its upstream port, never the route's), the front's routes' upstream ports to it, and the ports it exposes
  (its `expose:` and its image's `EXPOSE`) — from every container of the stack, so a front
  route's guard does not hold there (lint warns of a guarded route to it). It reaches the services it `depends_on` by name too:
  each one with a network namespace of its own is published on a loopback address of its own,
  `127.0.4.<n>` (n from 1 to 254, at most 254 such services; n follows from the service's name
  and the install's compose project, the next free one on a clash, so a service added or removed
  moves another only when their names clash, and two installs on one Docker host seldom share
  one), on the ports it
  exposes (its `expose:`: a port or a range of at most 64, over TCP unless `/udp`, another value
  failing the start and naming the service; then its image's `EXPOSE`, one it cannot take left
  out and logged, as is the whole `EXPOSE` of an image the engine cannot read at that start —
  the start's re-pull of a gone image writes them again) but those the host holds on every
  address — SSH's 22, the TLS front's ports, and the services on the host's network's own ports (their routes',
  the front's routes' upstream ports to them and the ports they expose) — each logged (`S's tcp/80 is not
  published for T: the host holds it on every address`); the service maps their names to those
  addresses (another L4 route's service to `127.0.0.1`). A dependency in another service's
  namespace or the host's is logged and not mapped (`S (…) depends on P, whose network is M: it
  cannot reach P by name`), and so is one left with no port, at each start (`S (on the host's
  network) depends on P, which exposes no port it can be published on: it cannot reach
  P by name (give P's ports in its expose:)`) — but not one without a network (`network_mode:
  none`), nor one with no port that mounts a volume or a host path the service mounts too (a
  Unix socket there): neither is reached by name. The front's loopback address (`--address`) is never
  one of `127.0.4.0/24` (`address A is in 127.0.4.0/24, the addresses the services on the host's network reach their
  dependencies at: choose another`), on every shape. Upgrades and re-runs count a route's port as the stack's own
  while its service runs on the host's network. The health probe and the disk-pressure call
  reach such a service at `127.0.0.1`. A service of a profile that is off gets none of this.
- Services on the host's network (`services.<name>.host_network: true`): on Docker and the VM the
  service runs as the VM runs an L4 route's, all of the above included (`network_mode: host` in
  `stowage-content.yaml`, its dependencies by name at their `127.0.4.<n>` addresses, the rest of
  the stack, the provision hook and a `command` provider reaching it by name at the host's
  gateway, the VM's firewall accepting its ports from the stack's bridges): a VPN daemon and the
  tun device it opens, a resolver bound to it, a proxy reading a connection's original
  destination (`SO_ORIGINAL_DST`, which on a bridge returns Docker's DNAT target), none of them a
  route. An L4 route to such a service is published by no one, on Docker too: the service listens
  on the route's port itself, on the addresses it binds (no `bind_input` with a `docker` shape),
  and a Docker host's firewall is its operator's (the end of the install logs the route `on the
  addresses <service> binds on the host's network, behind this host's own firewall`, or `not
  served: <service>'s profiles are off`). A Docker
  host's own listeners are its operator's too: a dependency's port one of them holds on every
  address cannot be published at `127.0.4.<n>`, and the start fails, naming it.
- File inputs (an input's `type`: `file` or `pem`): a certificate, a CA bundle or a key handed to
  a service as a file. On each command taking `--set` on Docker and the VM, but `provision`:
  `--set-file KEY=PATH` (repeatable; `--set` and `--set-file` of one input are refused, as is one
  that is no file input); in `--answers`, `KEY: {file: PATH}`; on a terminal the form asks its path
  (never masked, as a path is no secret, and checked as it is typed). The environment never gives
  one; `--secrets` gives a secret one. The content is at most 48000 bytes and an empty file is refused (`--set KEY=` leaves an optional input empty). A file for an input this release takes as text is refused as it is given, as is any value in the recorded form below (`KEY: is no file input of this release`); a fetched release's installer judges them, as `upgrade --version` passes `--set-file` on. A `pem` input
  takes PEM blocks with nothing after the last (text before or between them, a bundle's comments, is kept), every block must decode (`3 PEM blocks open, and 2 of them decode`): a `CERTIFICATE`, `PRIVATE KEY`, `RSA PRIVATE KEY` or
  `EC PRIVATE KEY` block must be well-formed DER, any other type is taken as it is, and an encrypted key (an `OPENSSH PRIVATE KEY`'s too) is
  refused (`CA: PEM block 2 (CERTIFICATE): does not parse`, `no PEM block`,
  `text after PEM block 1`, `an encrypted key: give it without its passphrase`). `.env` records the
  value as `stowage-file:<the content's base64>`, on its one line, so re-runs, upgrades, rollbacks,
  `secrets export` (a secret one), `--secrets` and the VM's settings carry it as any input; `--set
  KEY=stowage-file:<base64>`, an answer's plain string and the settings' `inputs` give that same form, and
  anything else but empty is refused (`not a file's content: give the file with --set-file
  KEY=PATH, or KEY: {file: PATH} in --answers`). A log, `--dry-run` and `vm create`'s summary show
  a non-secret one as `sha256:<12 hex>`, a secret one `<redacted>`. `vm create` puts a non-secret
  one in the seed, refused when it carries the cloud-config past what the cloud takes as user data (Amazon EC2: 16 KiB, Microsoft Azure: 64 KiB; `the cloud-config is N bytes, and … takes …: give smaller files for KEY; an optional one can be left out and given over SSH once the VM answers, with `install --set-file``); a secret one is refused there as any secret input, given over SSH with
  `--secrets`. An upgrade to a release that cannot read a recorded input, a text it declares as a file, a file it declares as text or a file a `pem` input refuses, in any mode, is refused before it writes anything (`KEY: <project> <version> declares it as a file, which cannot read the value recorded: the project must declare a changed input under a new key`); one that requires a file input recorded empty asks it as an input it adds; and a `rollback` to a release that cannot read a value given since takes back the one it ran with, or none (`KEY goes back to the value <project> <version> ran with`). Each start writes each file input's file under `.stowage-inputs/` in the install
  directory (the directory 0700, each file 0644: a bind mount lets a container of any uid read the
  file, and the host's other users stay out), named `<KEY>-<12 hex of the value's digest>`, and
  records its path as `STOWAGE_INPUT_FILE_<KEY>` in `.env`, relative to the install directory, for
  every file input whatever its `when` or `shapes`; a value left empty writes an empty file, so a
  mount never makes a directory. A service mounts it: `- ${STOWAGE_INPUT_FILE_CA}:/etc/app/ca.pem:ro`.
  A new value is a new path, so compose recreates the services mounting it; the files of earlier
  values and of inputs no longer declared are removed once the stack has started (a warning when
  that fails). `uninstall` keeps them with `.env`, and `--data` removes them. Lint refuses `${KEY}`
  of a file input in the compose file, `STOWAGE_INPUT_FILE_<KEY>` of what is no file input, and a
  file input read as text: as a route's domain, allowlist, forward auth or basic auth, an L4 route's
  or a service's address or bind, a set, a table's input, an extension's,
  or in another input's `default`. Reading `STOWAGE_INPUT_FILE_<KEY>` counts as reading the input.
  A provision hook's `env`, and a handoff step's (see [kits](kits.md)), may name a file input: its container gets the file read-only at
  `/run/stowage/inputs/<KEY>` and `STOWAGE_INPUT_FILE_<KEY>` holding that path, never the recorded
  value (these are the hook's only mounts). The file is empty while the input is, so a hook tests
  its size; `provision` writes it first, as no start did. `provision --set` refuses a file input,
  whatever reads it (`--set: KEY is a file input`), naming the re-run that changes it:
  `--set-file KEY=PATH` or, for an optional one, `--set KEY=` (on the VM, `sudo <short>-stow install`). On Kubernetes the hook's Job gets the same: the file read-only at `/run/stowage/inputs/<KEY>`, from the Secret below, and the variable holding that path; a hook cannot return that variable. A `command`
  provider's container gets neither on any shape.
  On Kubernetes a file input is asked as on Docker, with `--set-file` on `k8s install` and, for an input the release adds, `k8s upgrade` (`--set-file` passes on with `upgrade --version`, and an installer made before it refuses it as unknown). The release's Secret records its value as above, and the Secret `<release>-stowage-files` (labelled, applied server-side beside it, one of that name another tool wrote never written over) holds its content under its key, an empty file while the input is empty, for the chart to mount from `.Values.stowage.files`. A release with no file input writes no such Secret, gives `files: ""` on every deploy and leaves one of that name another tool wrote alone. A key no input declares any more stays with what it holds until `k8s uninstall` deletes the Secret (in every case, as the release's Secret brings it back): the release a rollback or a failed upgrade leaves running mounts it. So the content of a dropped file input, a secret one's too, stays in that Secret until then; `kubectl delete secret` before the next deploy clears it. A release declaring that input again asks it, as any input it adds; left empty, its file is emptied, logged (`input KEY: nothing recorded, so its file in the Secret <release>-stowage-files is emptied`). An `upgrade`, or `install` over a kept Secret, is refused when a value it keeps is one the release cannot read (the message above; an install given the key replaces it). `--given` replaces a value of the right form this release's checks refuse, and on an upgrade never changes its form, text to file or back, as the release a rollback brings back reads it too. An upgrade asks a required file input recorded empty, and restarts what reads it. `k8s reset-db --new-baseline` takes `--set-file` for a file input the release adds; without `--new-baseline` it refuses the flag. A hook whose `env` names a file input no longer asked on Kubernetes gets neither its file nor its value.
- Secrets delivered as files (`as_file: true` on a `secrets[]` entry, or on an input with `secret:
  true`): the value is recorded as any other, but a service reads it from a file, never from a
  variable — a variable is in `docker inspect`, in every child process's environment and in
  whatever in the container parses untrusted input. On Docker and the VM each start writes it
  beside the file inputs, `.stowage-inputs/<KEY>-<12 hex>` (the same modes: the directory 0700,
  the file 0644), the hex an HMAC-SHA256 of the value keyed with the install's random
  `STOWAGE_FILE_SALT` (recorded in `.env` at the first start that writes one, never logged or
  shown), so the name `docker inspect` and the container's mounts show tells nothing of the value;
  and records its path as `STOWAGE_SECRET_FILE_<KEY>` in `.env`; a service mounts it (`- ${STOWAGE_SECRET_FILE_DB_KEY}:/run/secrets/db_key:ro`) and is
  told its path in a variable of its own (`DB_KEY_FILE: /run/secrets/db_key`, the `_FILE`
  convention many images read). An optional input left empty writes an empty file. A new value
  (`secrets set`, a re-run giving the input) is a new path, so compose recreates what mounts it,
  and the file of the old value goes once the stack has started; the path's change is not
  logged, and `--dry-run` shows the path and the salt `<redacted>`. Lint
  refuses `${KEY}` of such a secret anywhere in the compose file (`${KEY} is delivered as a file
  (as_file: true), never as a variable: mount ${STOWAGE_SECRET_FILE_KEY}`), `as_file` on an input
  that is no secret or is a file input already, on a secret that rotates (its passes hand the
  services `KEY_NEXT` and `KEY_PREVIOUS` as variables beside it). A provision hook's `env`, and a
  handoff step's (see [kits](kits.md)), may name one: its container gets the file read-only at
  `/run/stowage/inputs/<KEY>` and `STOWAGE_SECRET_FILE_<KEY>` holding that path, never the
  variable, on every shape. `provision` (and a handoff's step) refuses an install whose `.env` no
  longer holds `STOWAGE_FILE_SALT`, naming the `install` re-run that records it, and on
  Kubernetes a listed key the release's Secrets do not hold, naming it and `k8s install`. A `command` provider's container receives it as a variable, as it
  receives every recorded value: that is the `command` contract, and its image is the project's
  own tool, not a service. On Kubernetes the value is written to the Secret
  `<release>-stowage-secret-files` (labelled, applied server-side, one of that name another tool
  wrote never written over), never to `<release>-stowage` (one that Secret already holds, from a
  release that read it as a variable, stays there with its value, as that release reads it after
  a rollback or a failed upgrade, while a revision of the release's helm history reads it there: a
  run drops it once every deployed revision lists it in its values' `stowage.asFile`, the keys a
  release delivers as files), so an `envFrom` of the release's Secret cannot
  reach it; a key that Secret holds stays there too, given the current value, while a later
  release no longer delivers it as a file, so a rollback never mounts an older value, and one no
  release declares any more stays there alone until `k8s uninstall`; the chart mounts it from `.Values.stowage.secretFiles` as a Secret
  volume, as [examples/full](../examples/full/deploy/chart/templates/api.yaml) does. Every key the
  release delivers so is there, empty while it holds none. An upgrade reads it back from there,
  and `k8s upgrade --given` restarts what mounts it; `k8s uninstall` deletes it, and keeps it
  with `<release>-stowage` when the data is kept. `secrets show` reads it, and `status --json`
  lists a secret input recorded there.
- Optional services (an input's `profile`): the install records `COMPOSE_PROFILES` in `.env`, the
  profiles whose input holds a value, comma-separated and sorted (recorded empty while none is;
  not recorded for a project whose inputs name no profile). Every `docker compose` the installer
  runs starts exactly those, a shell's `COMPOSE_PROFILES` never overriding it; one an operator runs
  in the install directory reads them from `.env`, unless their shell exports `COMPOSE_PROFILES`
  (compose prefers the environment). A run that turns a profile off removes its services'
  containers before the stack comes up (`docker compose --profile <each> rm --stop --force
  <services>`: `up` would leave them running).
- TLS mode: each route on its domain over HTTPS, only the HTTP and HTTPS ports published (80
  and 443 on every address, HTTP/1.1 and HTTP/2; `--http-port` / `--https-port` move them: with
  `acme` the public ports stay 80 and 443, forwarded to them, otherwise the moved HTTPS port is the
  public one, in the URLs and the redirect from plain HTTP; `--https-port 80` is refused, 80
  being the front's plain HTTP in its container). An upgrade writes each route's URL again from
  the release's routes: its domain input's recorded value, the recorded port and its path (in
  loopback mode the recorded address and port, with its path); it refuses a release whose route
  takes its domain from an input the install does not record, or records empty, and one whose
  routes clash on the recorded domains, naming the re-run that gives them (`<ctl> --dir <dir>`). `vm create` refuses these ports
  (an L4 route's, and 22, the VM's SSH, included) before its form, and the domains after it, as the VM's install would,
  before anything is written. The front listens on 80 and 443 in
  its container whatever the host ports (`stowage-front.yaml` publishes `${STOWAGE_HTTP_PORT}:80`
  and `${STOWAGE_HTTPS_PORT}:443`), and with a moved public HTTPS port it also serves each domain
  on that port (the site `<domain>, <domain>:<port>`), for the stack's services that dial a
  route's URL. The routes of one domain are one site: each route with a `path` serves its prefix
  (the path itself and what is under it; the longest first), the route without one every other
  path, and without such a route another path answers 404 (paths match whatever their case, so
  two differing by case alone are one path). Each route's URL is
  `https://<domain>[:<port>]<path>` (`STOWAGE_URL_<ROUTE>`). Two routes given one domain (two
  inputs of one value) are refused when they serve the same path, at an install, a re-run and an
  upgrade, and so is a route with `client_auth` given a domain another route has (rendered all
  the same, it answers 403 to everyone) (`<route> asks for a client certificate on
  <domain>, which <route> has too: a certificate is asked for a whole domain`). Given values are checked before the
  form opens. `--domains`, `--set` and `--answers` give a domain; an exported variable alone picks TLS only
  for a project that offers no loopback mode, and on a terminal it is asked, as the default, never
  recorded unseen. `--cert` picks `byo`; a `byo` install refuses a directory already holding a
  `tls/`, and a mismatched certificate and key re-open their form, saying why. A route's domain is its `domain_input`, an input like any other
  (`--domains route=host` is the shorthand, an L4 route with `tls` among the routes it names); giving one picks TLS: `acme`, or the project's
  first HTTPS mode when it does not offer `acme`, unless `--tls` says otherwise. Domains are lowercase DNS names, one per `domain_input` (asked once for the routes sharing it; `--domains` giving those routes two values is refused). Safe defaults stay safe. One
  certificate source per mode, never a fallback: `acme` (Let's Encrypt alone, HTTP-01 or
  TLS-ALPN, so ports 80 and 443 must reach the host — with moved ports, a run says to forward
  them, and that IPv6 forwards no port: a domain whose AAAA record names the host is reached on
  80 and 443 themselves; `STOWAGE_ACME_EMAIL`, optional, is checked
  as an address), `byo` (`--cert` / `--key`, PEM: checked as a pair, unexpired and covering every
  domain, and its chain — without `--ca`, it must verify to a CA this host trusts with the
  intermediates the file holds, so a leaf given without its intermediate is refused, naming its
  issuer; a chain ending at a self-signed certificate no host trusts, a private CA's, is said and
  kept — then copied into `<dir>/tls/` 0600 (readable by the installer's user alone; each start copies it into the front's volume, see the front above); `status` shows its end, and warns from 30 days
  before it), `internal` (Caddy's local CA; its root is
  `/data/caddy/pki/authorities/local/root.crt` in the front; `secrets export` carries it and its
  key, so a machine made again with `--secrets` keeps the root). `dns-cloudflare`
  (Let's Encrypt alone, DNS-01 through Cloudflare, so no port needs to reach the host for
  issuance; `STOWAGE_ACME_EMAIL` as with `acme`; `STOWAGE_CLOUDFLARE_API_TOKEN`, required and
  secret, a token with Zone DNS Edit on each domain's zone, in the form Cloudflare issues — 35 to
  50 letters, digits, `-` or `_`, or `cfut_` / `cfat_` and at least 32 more — recorded in `.env`
  and given to the front's container as `CLOUDFLARE_API_TOKEN`; the project's services and the
  provision hook are not given it, though a stack that reads `.env` itself — an `env_file` or a
  mount of the install directory — sees it like every recorded value). HSTS is sent except with `internal`.

**Backups, restore and the disk-pressure watch on Docker.** The VM's host daemon's code runs in
the Docker installer's process for one command (no daemon stays): run by the installer the install
keeps (`<dir>/<short>-stow`, the release it runs; another is refused).

- `backup config|run|status|list|verify` and `restore OBJECT` (`--dir D`, the other flags as on
  the VM — see "Backups" under the installer on the Kairos VM) take a project's `pg-age-rclone`
  or `volumes-age-rclone` backup: the settings in `backup.yaml` (root's alone) and the last run in `backup-last.json` in
  the install directory, the volumes' archives and the dump encrypted to the recipients as they
  stream to the backup runner
  (`<name>-backup`, pulled with the stack's images, by digest, since a release of a project with
  the `docker` shape and a `pg-age-rclone` or `volumes-age-rclone` backup publishes one, and pulled again when the engine
  lost it), and the restore into a database
  of its own, swapped in only once it loaded, then the stack up and provisioned. The schedule is
  a systemd timer of the install's own, `<name>-backup-<8 hex of the install directory's
  digest>.timer` and its `.service` (`ExecStart="<dir>/<short>-stow" backup run --if-configured
  --dir "<dir>"`: each path quoted, its `%` and `$` doubled and a control character escaped, as in
  every unit Stowage writes on a Docker host; in `Description=` the directory's `%` doubled, a
  control character `?`) in `/etc/systemd/system`, written, enabled and started by `backup config` (and
  `--remove` removes them); on a host without systemd, or a run not as root, there is none (a
  backup runs when `backup run` is), and the schedule is kept unchecked. One process at a time
  runs the jobs (`backup run`, `verify`, `restore`): it holds `.stowage-host.lock` in the install
  directory, and another waits for it, saying so; the reads and the settings (`backup status`,
  `list`, `config`, a settings change holding `.stowage-settings.lock` alone) never wait for a job, and `backup status` says `running` while another process runs a
  job (a backup, a verify or a restore). A signal stops the job the command runs.
- The disk-pressure watch (`hooks.maintenance.disk_pressure`): the install (each run of the
  installer that brings the stack up) writes, enables and starts `<name>-disk-<8 hex>.timer` and
  its service, which runs `<dir>/<short>-stow disk-check --dir <dir>` two minutes after boot and
  every 10 minutes: it reads the filesystem of the Docker engine's data directory (`docker info`'s
  `DockerRootDir`), and from `soft_pct` POSTs the level as the VM's watch does, the last POST kept
  in `disk-pressure.json`, which `status` shows. Without systemd or root it is not installed, and
  the install says so; a release without the hook removes the units.
