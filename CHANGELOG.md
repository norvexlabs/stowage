# Changelog

Stowage's own releases. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [SemVer](https://semver.org/).

## [0.46.0]

### Added

- `describe --json` lists the release's secrets, `secrets`: for each shape the release declares,
  how `secrets rotate` changes each one there (`passes`, `replace` or `none`: Kubernetes takes
  the three passes alone) and whether `secrets set` takes a chosen value there (never on
  Kubernetes); and whether a service reads it from a file — never a value. `describe` prints a
  `secret` line for each.

### Fixed

- The `secrets` verbs refused with exit 1, as failures: `secrets rotate` of a key or a pass the
  release does not take, a pass out of order, its one pass unconfirmed (no terminal, or `--no-input`, without
  `--yes`), `--stage` with `--finish`; `secrets set` of a key that is not settable or a value it
  cannot record; `secrets show` to no terminal; `secrets export` without `--recipient` or with a
  key that is not an age public key. Each is a refusal, exit 3, nothing changed. The command
  `secrets rotate` names in its hints is the one to run there: with `--dir` on Docker, `sudo` on
  the VM, under `k8s` on Kubernetes.
- On the VM, a refusal the host daemon answered with its own status reached the verb as a
  failure, exit 1: a request it cannot take (a body that is no request, a network change or its
  revert window, a backup config or a schedule systemd rejects, an export's keys), a verb run while the install runs another release than the OS
  carries. Each exits 3, with its fix where one holds.

## [0.45.0] - 2026-11-25

### Added

- `replace: true` on a secret (a session signing key, an internal client secret, one delivered as
  a file): `secrets rotate KEY` replaces it in one pass on Docker and the VM — a new value, the
  services reading it recreated. It asks first, as nothing of the old value is kept (`--yes`
  confirms without asking). Lint refuses `replace` on a secret the database's service reads, and
  beside `rotation` or `settable`. `secrets --help` names only the verbs the release has.
- On the VM, `reset-db --new-baseline` takes `--set`, `--set-file` and `--answers`, as `upgrade`
  does: the inputs the release it crosses to adds, judged before its OS is written and given to
  the boot that crosses. A release adding a required input with no default can now be crossed
  to. Its refusals name the `reset-db` that gives them, or say why none can (a release, or the
  OS the VM goes back to, built before this version).
- `lint.accept`: a file of the project accepting lint warnings it weighed, each with its reason
  and its last day. `lint` lists them as accepted rather than warned, warns again of one expired,
  and says to remove an entry that matches no warning.

### Fixed

- Lint refuses `settable` (and now `rotation`) on a secret the database's service reads however
  the service reads it: through its `extends` base, its command, a file secret's mount, a
  top-level compose secret or config, an alias or merge key, or an `env_file`.

## [0.44.0] - 2026-11-24

### Added

- `kit create --json` on Docker and Kubernetes, as on the VM: a program issues a kit from a JSON
  request and gets the kit and its code back as one JSON object, with no file written and nothing
  asked. `--request FILE` reads the request from a file instead of standard input.
- `describe` lists the handoffs a release issues: the project each kit is for, what it asks
  (secret answers marked), the keys it gives and whether it can be checked.

### Fixed

- A kit request read as JSON (the VM's `kit create --json`) is held to what `kit create`'s flags
  hold: good for at most 36500 days, at most 1 MiB, and its instance, left out, is the handoff's
  name. Its refusals name the request's field, not a flag `--json` refuses.
- `--expires` given in hours over 36500 days is refused before anything is asked, as in days.
- `--expires` with a count of days far out of range, such as `-213503d`, is refused, where it
  wrapped into a kit good for about a day.
- The Kubernetes suites' checks that an object or a release is gone fail when `kubectl` or `helm`
  cannot answer, instead of reading it as gone.

## [0.43.0] - 2026-11-23

### Added

- The VM's `upgrade` gives the inputs the new release adds, as Docker's does: `--set`,
  `--set-file` and `--answers`. The host daemon checks them as the boot into that release will
  take them, stages them root-only for that boot, and the boot gives them to its upgrade, then
  removes them. A release adding a required input with no default can now be upgraded to on a VM,
  where its boot used to refuse and the health gate went back. A VM on an OS built with an earlier
  Stowage first upgrades to a release built with this one that adds no required input.
- A dry run of an upgrade to a release another Stowage published names what this host daemon's
  rules would refuse in it, such as a required input the upgrade does not give.
- The VM's `updates --json` says whether an upgrade to the release it offers takes the inputs that
  release adds (`takes_inputs`): not when the VM's OS or that release was built with an earlier
  Stowage, whose upgrade or boot takes none.

### Fixed

- A VM boot whose install failed carries the installer's own error in the health gate's verdict,
  `status` and the failed boot's diagnostics (`<name>.service failed: <error>`), where it said only
  that the unit failed, with a `journalctl -b` hint that no longer pointed at that boot once the
  VM went back.
- A VM boot refusing an upgrade for a required input the release adds names the fix that works
  there (`upgrade --set`), not flags and an environment the boot does not take.

## [0.42.1] - 2026-11-22

### Fixed

- A wait for a TrueNAS job cut off (Ctrl-C, a timeout) names the cut-off the same way wherever
  it landed, where it sometimes named the read of the job it stopped instead: a disk's import
  into a zvol says its convert's wait was cut off (`context canceled`), a cleanup whose own time
  ran out on a read after its first names the job still running rather than unread, and any
  other job's wait names the job (`<method>: the wait for job <id> was cut off: …`). An answer
  TrueNAS gave is kept as it is.

## [0.42.0] - 2026-11-22

### Changed

- The OS bundle is built on Kairos 4.6.0 (kairos-init
  `quay.io/kairos/kairos-init:v4.6.0@sha256:9c013af11b66189ad8d5a0a73c995eae53b65a2f5a620cc592bd5d0b01ed7915`,
  was v4.4.0); a project pinning another kairos-init is warned by `lint`. An install from the
  installer ISO on bare metal writes a UEFI boot entry of the disk it installed, first in the
  firmware's boot order, so a machine whose list names an earlier system boots the new one. Upgrades
  made from a 4.6 system refresh GRUB's configuration with GRUB itself; the first upgrade, from an
  earlier bundle, is made by that system's Kairos. A VM on an earlier bundle upgrades to a 4.6
  one, and rolls back to it.
- The OS bundle masks `systemd-sysext` and `systemd-confext`: no extension image found on the
  disk is merged over `/usr` or `/etc`, so what runs is the bundle the release signed, rolled back
  whole.

### Fixed

- A VM's secondary NIC kept, from the boot's first seconds, IPv6 routes of the LAN its primary NIC
  is on, so replies to an IPv6 client could leave by the secondary and never arrive (now and then,
  as the router answered in time or not). The OS bundle's boot now drops that early network as
  the system starts, on every VM running a release of this bundle: each NIC carries only what
  Stowage's files give it, so a secondary NIC, or a primary one with IPv6 off, has no IPv6.
- A NIC no role names, on a VM or a machine seeded by this release whose seed names its NICs by
  role, carries no address — no DHCP, no IPv6, no route — its link up, and one another link
  carries (a failover's standby or virtual function, a bond made outside networkd) left to it (one seeded
  earlier keeps its seed's network until it is seeded again):
  Kairos's DHCP default configured it, so a NIC added before `network attach` named it, on the
  primary's LAN, took the primary's routes, and a cable in a port no role names brought the
  machine up there. Only while the primary's NIC is named: one its boot cannot name (a NIC
  replaced) leaves every NIC to DHCP, so the machine stays reachable. `network` lists each such NIC
  with its MAC and the `network attach` command that names it (`unnamed` in
  `--json`), and the console's login screen says where to look.
- `network` on a VM without a default route shows the primary role's NIC, and `network set`
  reconfigures it, where both took the first NIC of a kernel's name.

## [0.41.2] - 2026-11-21

### Fixed

- The OS bundle, the front and the backup runner install their packages from the
  distribution's archive at every build, never from Docker's build cache: the OS bundle, built
  for every release, now takes the kernel's and Docker's current security updates though its
  base stays pinned, where it kept the packages of the publisher's first build and `ship`'s audit
  refused the release for fixes the archive already carried. The front and the backup runner are
  still re-tagged while their recipe is unchanged, and take the archive's fixes when a Stowage
  release changes it.
- A VM booted into its recovery system keeps its host name and its DHCP leases: it is reached at
  the VM's address and sends the VM's name with its DHCP requests, where it came up as
  `cos-recovery` on leases of its own. A VM's DHCP
  clients name themselves by a DUID of the NIC's MAC rather than of the machine-id. It applies to
  a VM created with this release (its seed); one created before keeps its own.

## [0.41.1] - 2026-11-19

### Fixed

- The front and the backup runner (and the examples' Alpine images) carry OpenSSL 3.5.9-r1, past
  CVE-2026-107496, CVE-2026-88823, CVE-2026-95691 and CVE-2026-95692, which their pinned bases
  still ship: `ship`'s audit no longer refuses a release that installs them.

## [0.41.0] - 2026-11-18

### Added

- `shapes.kubernetes.unserved` and `shapes.kubernetes.guarded` answer lint's warning of a
  guarded front route on Kubernetes: the first lists the routes the chart does not serve, the
  second those it serves whose guards the chart's ingress or the operator's enforces. Lint no longer warns of
  either, and refuses a name that is no route, one given twice, a route without a guard, a route
  in both lists, and either key written with nothing under it.

### Changed

- The warning also counts a chart's TLSRoute, TCPRoute or UDPRoute, and the routes of Traefik,
  Istio, OpenShift, Contour, NGINX Inc, Kong, APISIX, Emissary, Gloo, Skipper and Kuma, as
  routing, and a kind key right after a template action; it no longer counts a templated value
  elsewhere on a `kind:` line as a computed kind, and it names the two keys that answer it.

## [0.40.0] - 2026-11-16

### Added

- `vm seed --json` gives `size` (`cpus`, `memory_mib` and `app_gib`, each with its `min` and
  `recommended`: the release's `shapes.kairos.size` over the floor every VM needs) and `firewall`
  (`ssh_sources` and `open_ports`, each port with its protocol and sources: what a platform's
  firewall in front of the VM lets through, the front's ports and each L4 route's allowlist), so
  a root sizes and fences the VM as the release asks.
- `status --json --values` (Docker, Kubernetes, the VM) gives `pull_login`, whether the install
  holds a pull login (never the login), and `acme_ca_root`, the `sha256:` digest of the root
  `--acme-ca-root` gave; when either cannot be read, `pull_login_error` or `acme_ca_root_error`
  says why instead of reporting none (on Kubernetes, the pull Secret unread by kubectl or its
  login not parsing; on Docker and the VM, as `registry_login_error`).
- `vm seed --json` takes the secret inputs of `--answers` as `vm create` does: the first install
  waits for them over SSH (`install --given -`), past the first boot's switch to the release's
  OS, and the seed names them in `await_given`, never their values (it refused them before);
  with `--await-secrets` they are refused, as the install waits for one of them.

### Changed

- A VM of one NIC role (a hypervisor's without `--interface ROLE=mac=…`, or a cloud's) has that
  NIC pinned to its MAC by its first boot, on the OEM partition: the role's one `.link` file then
  matches that MAC alone, so a NIC added later, in any order, can no longer take the role's name,
  and `network attach` runs before or after the platform adds the NIC. The pin matches the NIC's
  MAC and a `Driver=` line: the NIC's own driver when it is one of an accelerated networking's
  virtual function (`mlx4_en`, `mlx5_core`, `mana`: a physical Mellanox NIC on metal), else those
  drivers left out, so a virtual function of that MAC (Azure's) is never named as the role and a
  change of the NIC's model keeping its MAC still is. The VM's NICs leave out a NIC another
  enslaves (such a virtual function) rather than every NIC of those drivers, so a physical
  Mellanox NIC is named and counted, and a `.link` file by a MAC the cloud-config is given no
  longer leaves those drivers out. The pin counts only free NICs: not one an attached role's
  `.link` file names, nor a virtual function beside its NIC before the NIC enslaves it. A pin
  file altered or cut short is written again. `network attach` writes the same `Driver=` line
  from the driver of the NIC there, recorded in `interfaces.yaml` (`driver`): of several NICs of
  that MAC, the one not of those drivers, and refused when that does not tell them apart;
  attached before the NIC is there, it leaves those drivers out and says so, and a run again with
  the NIC there records its driver, which names a physical Mellanox NIC attached that way. Two
  attaches at once keep both NICs (a lock, `interfaces.lock` in the install directory). A pinned
  NIC replaced on the platform, the VM's only free NIC, is pinned in its place at the next boot
  and named the role at once; with no free NIC or several, the pin is left and the boot says
  why. A pinned NIC there under another name is named the role at boot, as is the NIC of a MAC
  the cloud-config names, never a virtual function or a bridge of that MAC (of several NICs of
  it, none, said). `vm seed --json` gives such a NIC no `mac` (several NIC roles still get one each).
- Cut-over: a VM seeded before this release is not pinned, its only NIC still matched by its
  original name: pin it by hand before adding a NIC (a cloud-config on the OEM partition, given
  in the VM reference, "A VM seeded before 0.40.0"); `network attach` refuses while a NIC is
  named by its original name and not pinned, naming that fix. `network attach`, `network set --role` and
  the install's warning refuse an `interfaces.yaml` holding 0.39.0's `pin: true` entries, saying
  to remove them (after that pin by hand) and run again.
- `network set --role` on a role no NIC is named as is a refusal (exit 3) in every case, its fix
  what names one, as the install's warning words it: for a role the cloud-config names by its
  original name, never pinned, leave its NIC alone on the VM (keeping the NICs `network attach`
  recorded, named) and restart it, as `network attach` refuses that role; for one the boot
  pinned, its NIC gone or of another driver, leave one NIC no other role names and restart the
  VM, which pins it again; for a NIC of the cloud-config's MAC or the pin there but not named,
  why (several NICs of that MAC, or another device holding the role's name, named) and what
  frees it; for a `.link` file naming no NIC, the file to restore. The install's warning comes
  once a run for each role, and gives a refusal's fix too. A `.link` file
  whose presence does not read is said so, with what to restore, by the install's warning and
  `network attach`, never taken for one absent. At boot,
  the primary's override is left as an earlier boot rendered it, said on the log and the console
  banner, when the cloud-config's network files do not read; a change of it awaiting
  confirmation then is undone by removing its file (at boot, its window's end, the daemon's start
  and `network revert`), the cloud-config's settings applying, as the banner and `network` say.
- `network attach` ends by saying what is left: `sudo <short>-stow reboot` when the NIC is there,
  else add it on the platform and restart the VM; it refuses while the VM's NICs do not read.
  `network set --role` on a role no NIC is named for, and the install's warning about it, say
  the same: the NIC of the MAC the cloud-config names it by, or whether `network attach`
  recorded it (and whether its NIC is there, and whether its link matches the NIC's driver), with
  the step to take; an `interfaces.yaml` that does not read refuses it with its fix (409 on
  `network set --role`), as does a NIC record that does not read (naming what to restore); the
  install's warning about a primary the cloud-config names gives its case the same way, and about
  one it does not name, the way to a VM of the release (its secrets, its backup configuration and
  a backup restored), as `network attach` takes secondaries alone.
  `network set` of the primary refuses (409, with its fix) while the cloud-config's network files
  do not read, rather than match every NIC.
- The boot's rename of a NIC never takes a name another NIC holds, nor takes the NIC down for
  it, and brings a NIC whose rename fails back up; a VM of one NIC renames the NIC of the default
  route only before its NIC is pinned or named, so a NIC added on the platform keeps its name.
- `vm host-key` and `vm kit create`, `check` and `take` require `--known-hosts-line`: the VM's SSH
  host key as one known_hosts line (`vm seed --json`'s `known_hosts_line` until the install
  retires that one-time key, then `vm host-key --json`'s), as no record of the VM is kept. Without
  it they are refused before any connection; a VM made by an earlier `vm create --create
  opentofu` has its key in that state directory's `known_hosts`.
- `vm truenas import-image`, `seed` and `stop` are documented, no longer hidden: the steps the
  TrueNAS example runs where the `truenas/truenas` provider has none yet (write a disk image into
  a zvol, upload the seed ISO, stop the VM before its devices are deleted) — a stopgap until that
  provider covers them. They read `TRUENAS_ENDPOINT` (the provider's WebSocket URL) or
  `TRUENAS_URL`, `TRUENAS_API_KEY`, and `TRUENAS_CA_CERT` (a CA file) or
  `TRUENAS_INSECURE_SKIP_VERIFY`, take `--timeout`, and are safe to run again: `import-image`
  leaves a zvol holding the image already, writes again one it began, and refuses a zvol holding
  other data, another image or used by a VM's disk device (it records the image in the zvol's
  `org.stowage:image` property; a zvol without it counts as empty only while it holds no more
  than a zvol never written, counted before compression and measured on the host each run on a
  throwaway zvol beside it, and one whose size or properties TrueNAS does not report is refused;
  the image is checked against its digest as it is uploaded, its one read — a wrong one is
  refused after that upload, before the zvol is written —, under a name of the run's own and of
  the zvol's, emptied once its upload began and no longer runs; a missing image is refused before anything is
  made). One import at a time
  writes into a zvol: the throwaway zvol, made at the start, is its lock, held for the whole run;
  a second import is refused while it is held (runs in one apply run in parallel, and the ones
  refused succeed on the next apply), and a lock a run cut off left is never taken over: the
  refusal names it, its age, and the zvol to delete once neither a run nor a TrueNAS job of that
  import runs; the zvol, its devices
  and the lock are read again before it is written, and any change since is refused; a convert
  cut off is waited for, never aborted (TrueNAS's abort leaves its `qemu-img` writing): one that
  then succeeds marks the zvol written, with a warning, and one still running, aborted or unread
  leaves the lock and the staged file, named (a `--timeout` that ran out then suggests no plain
  run again); a convert call whose answer was lost looks for its job first; a convert job TrueNAS hands over that
  is another import's, from its one-at-a-time queue, is refused; once the zvol is written, a lock
  not deleted or a file not emptied is a warning); `seed`
  replaces the ISO; `stop` stops a VM in any state but stopped — a running one by a clean
  shutdown forced after its timeout, a suspended one powered off at once —, fails if it is not
  stopped after (a call that fails as the VM stops or goes meanwhile is no failure), and leaves
  one stopped or gone. `vm
  truenas start` and `drop-snapshots` are gone (the provider starts the VM, and its zvol and
  dataset delete their snapshots with `force_destroy`).
- The installer image is an index of one `<short>-stow` per platform — linux/amd64, linux/arm64,
  darwin/amd64 and darwin/arm64 —, each one layer of one file, signed and carrying its own SBOM,
  so a client without Docker pulls its own platform's from the registry and runs it (the
  publisher's reference describes the contract); the release manifest still names the installer
  by one digest, the index's. A target still runs linux/amd64: on another platform (an arm64
  Docker host takes the linux/arm64 image) every verb but `script`, `describe`, `k8s`, `vm` and
  `mirror` refuses, naming the platform and `docker run --rm --platform linux/amd64`; `upgrade
  --version` fetches the image of the platform it runs on, and refuses an older release's
  single linux/amd64 installer elsewhere, naming the fix (run it from a linux/amd64 machine); a
  Kubernetes Job running the installer runs its linux/amd64 image by digest, on a linux/amd64
  node alone (a cluster without one leaves it pending, and the hook's refusal says so). `audit`
  and `sbom export` read each installer image's SBOM (the installer's other platforms are
  subjects of their own, with `platform`, exported as `installer/<os>-<arch>.spdx.json`; an
  index that cannot be read is one subject `installer (other platforms)` not audited, the rest of
  the release read as ever), `verify` checks each image's signature, `mirror export` carries each
  image's signatures, and `prune` and `yank` remove an index's images with it, in any
  repository, but those another tagged index, a tag or an index still awaiting removal names
  (an interrupted removal leaves them marked, for the next prune; one two indexes name goes once,
  after both). The `stowage` binary embeds the
  four, compressed, with each one's sha256 and size, which the release's inputs take; `ship`
  refuses one whose bytes differ. The darwin images are unproven on a Mac until one runs them.

### Removed

- Stowage no longer creates or destroys VMs, and keeps no state of one: `vm create --create
  opentofu` (with `--module`, `--tofu-var`, `--vm-name`, `--pool`, `--dataset`, `--bridge`,
  `--cpus`, `--memory`, `--disk`, `--host` and `--wait`), `vm destroy`, `vm attach`, `vm create
  --kit` and the OpenTofu modules embedded in the installer are gone. An OpenTofu root of yours
  creates the VM from `vm seed --json`, with the `norvexlabs/stowage` provider's `stowage_vm_seed`
  and the platform's own provider: each platform has a complete root among the provider's
  examples, `norvexlabs/terraform-provider-stowage` `examples/guides/<platform>-vm`, to copy.
  `vm create` keeps writing the disk and seed as files and printing the steps by hand (its
  `--create` flag is gone with the OpenTofu way); `vm kit take` gives a
  kit to a VM made; a NIC is attached on the platform, then `network attach` on the VM names it
  from the next boot, whose message now says to reboot the VM once the platform attached the NIC
  and the VM restarted. Cloud images `ship --disks` registers are unchanged.
- A VM made with `--create opentofu` by 0.39.0 or earlier keeps its OpenTofu state in Stowage's
  state directory (`~/.local/state/<name>/vms/<host>`), which this release no longer reads:
  destroy it with 0.39.0's `vm destroy` before upgrading, or adopt it into a root of your own with
  `import` blocks (each example lists its resources' import IDs). A `moved` block cannot carry
  it over: the TrueNAS example uses the official `truenas/truenas` provider, the old modules the
  community `pjsalty/truenas`.

### Fixed

- `mirror export` carries the signatures and SBOM attestations of every image an index names,
  not only the index's.

## [0.39.0] - 2026-11-15

### Added

- Another ACME CA than Let's Encrypt: `--acme-ca URL` (an https ACME directory) and
  `--acme-ca-root FILE` (the PEM root that directory's TLS chains to) on a Docker install and the
  VM's install re-run, in the `acme` and `dns-cloudflare` modes alone. The URL is recorded, the
  root kept in the install and mounted read-only into the front; a re-run keeps both, `none`
  removes either, a new `--acme-ca` without its root removes the old root, and a move to another
  mode removes both. `status --json --values` shows the URL (`acme_ca`). The end-to-end suite of
  the stateless example now proves a front holding a domain for L4 TLS routes alone obtaining and
  renewing its certificate from a real ACME server.
- A pull credential per install: the operator mints a login per install in the registry and gives
  it with `--registry-login-file FILE|-` — on a Docker install (recorded beside the install, 0600,
  before anything is pulled; a re-run with it replaces it, and a run that fails before the images
  are fetched with it puts the one held before back), on `k8s install`, `k8s upgrade` and `k8s
  upgrade --version` (the pull Secret is written from it, labelled, and kept by a later run without
  the flag, whose own reads of that host take it, a rollback's schema check and a kit's included;
  a login for another host than the images' is never named for them, nor in place of another pull
  Secret the release's values name; a pull Secret the cluster does not let the run read stops an
  install, an upgrade or a reset before anything is asked or applied, so the login is never
  written over unseen, and is logged by a run that only reads; one whose login does not read is
  logged with the way to give a login again, and the run goes on without it; the installer
  `--version` runs gets the login on a pipe, never a file on disk), and on
  `vm create` / `vm seed`, now for the release's own registry too and not only a mirror. A new
  `registry-login FILE|-|--remove` verb on Docker records or removes it later, as the VM's does,
  which now takes a login for the release's own registry too. On the release's own registry the
  password the pull account's variable gives comes first, then the install's login, then the
  account baked into the installer, so an install without a login keeps pulling with the baked
  one, and revoking one install's login leaves every other install pulling. `status` and `status
  --json` (`registry_login`) name the login's host, never the login; `secrets export` does not
  carry it; `uninstall --data` deletes it.
- Every service's container is given `STOWAGE_SHAPE` on Docker (`docker`) and on the VM
  (`kairos`), so a service can tell a machine that is the stack's alone from a shared one; a chart
  sets `kubernetes` in its own pods. The first start after the upgrade recreates each service once.
- Under a `command` version scheme, `upgrade --version`, `k8s upgrade --version` and the VM's
  `reset-db --new-baseline --version` to a release other than the one the install runs are refused
  unless `--unordered` is given: the scheme's command runs only where the project publishes, so
  the installer cannot order the two releases; the refusal says to run the same command again with
  `--unordered` added, or the plain verb. A plain `upgrade` follows the channel, which `ship`
  moves forward only. `--unordered` without `--version` is refused. On the VM the host daemon's
  `upgrade` and `reset-db` requests take `unordered` from the operator's socket alone (the app's
  is refused), `<short>-stow` refuses before its confirmation, `updates --json` answers
  `needs_unordered`; a hint naming the release the install runs never adds `--unordered`. The
  app socket's refusal of `unordered` names the operator's command over SSH, and `reset-db
  --new-baseline`'s refusals say it installs the channel's release, or with `--unordered` wipes
  the data and installs the version named.
- `mirror import` checks, before it pushes anything, that the archive's release and each yank it
  carries are signed by a key the importing `<short>-stow` trusts, reading the signatures from the
  archive itself: one unsigned, signed by another key or whose signature does not verify is
  refused with nothing imported, naming the release and why, and how to get a signed archive.
- Disaster recovery on Kubernetes: `k8s secrets export --recipient AGE_KEY…` writes the release's
  secrets (generated, secret inputs, what the provision hook returned; never the pull login)
  age-encrypted in the same file format as Docker's and the VM's `secrets export`, and `k8s
  install --secrets FILE|-` makes a first install from such a file, from any shape, by the same
  rules (refused on a release whose Secret is already there, beside `--given` or `--kit`; a key
  that is no secret of the release on Kubernetes left out, said). The provision hook's values it
  carries are written to the release's outputs Secret, marked, and no deploy runs the hook until
  `k8s provision` does, once the old install's backup is loaded.

### Changed

- A `command` provider of `health`, `database.schema_compat` or `backup` takes `env`, as the
  provision hook does: the inputs, secrets and safe defaults its container receives, required (`[]`
  for none), each one the project declares; a file input or a secret delivered as a file reaches it
  as its file. Its container no longer receives every input, secret and safe default; a spec
  without `env` is a `lint` error, and is refused when it runs, naming the key to add.

- `restore --unrecorded` says, before it loads, what it trusts in each unchecked object: a dump's
  statements run with the migrate role's rights, a volume's archive replaces the volume's files as
  they are (owners and modes included).
- A rollback holds the install's lock until the previous release's installer takes it over, so no
  other run can come between them.
- A re-run updates an input whose default names another input when that one changes, unless the
  input was ever given a value.
- A start-first overlap tells a missing `update_ready` file the same way on any engine; an
  `update_socket` directory whose files total over 1 MiB fails the start.
- `vm create` asks among every platform a disk set of the release can boot, reading the
  registry's disk sets once; a disk-set source it cannot read ends the question with the
  platforms read before it, and fails only where `--platform` would.
- Lint judges `hooks.provision.returns` per shape: a key the install records on some shapes only is
  allowed, and a key of the chart's provision ConfigMap counts as recorded on Kubernetes.
- `make e2e` and `make smoke-install` remove the images they pulled or pushed from their test
  registry.
- `make self-audit` fails on a stale stopgap: a Go module a recipe forces that its upstream's
  release already requires at that version or past it, or an Alpine package a recipe or an
  example's Dockerfile raises that its base already carries at that version or past it. Live ones
  are printed with the versions they are raised over; an `apk upgrade` and a module the release
  does not require are left to judge by hand.

### Fixed

- The VM's baseline hints name `sudo <short>-stow`, which reaches the host daemon.
- A vendored chart archive whose entries start with `./` is read.
- An install directory whose name holds a control character is refused.
- `vm create` no longer writes an input that took its default from another input into the VM's
  settings: the VM's install derives it, and a re-run expands it again.
- `lint` reports every error of an L4 route's allowlist input, not only the first.
- A start no longer flushes connection tracking when a set's input lists the same elements in
  another order or spacing.
- A start retried after an image left the engine applies the Docker host's firewall chains once.
- `k8s rollback`, `k8s upgrade` and `reset-db` read helm, the fetched installer and the provision
  hook once each.
- `prune` on a registry that deletes no tag alone (ghcr) reads each repository's tags once,
  rather than once per tag it removes, so a prune of many releases stays clear of rate limits.
- Moving the latest channel removes a disk's `latest-<format>` of a format the channel's disk set
  has none of even when this `stowage` does not make that format (a newer one's), and `verify` and
  the moves read only the tags present and the set's formats.
- A disk's chunk ceiling recovers: after a few chunks taken whole it
  rises halfway back to the chunk refused, so one transient refusal no longer slows every later
  disk of the run; a 416 whose upload status cannot be read lowers it too, rather than each disk
  paying it again; and a short last chunk refused on every attempt goes up in one stream after 3
  retries instead of retrying until the stall bound.
- A VM link given a static `address6` without a `gateway6` expects no IPv6 router: the boot no
  longer waits 60 s for an IPv6 default route on it, the daemon no longer solicits one, and
  `status` no longer warns of its absence.
- `tune --check` judges a NIC interrupt that fired before the boot moved it, and not since (a
  virtio NIC's config interrupt), by the CPU asked rather than reporting its old CPU: the apply
  records its counts when it moves it.
- Under `flush_conntrack`, a change of a set through the app socket ends the flows of the elements
  nft changed (their connection-tracking entries deleted, the others kept), and a start's change
  of a set an input fills ends those alone too, rather than flushing the whole table. A set whose
  key type cannot be matched to flows keeps them through the app socket (its answer warns), and
  flushes the whole table at a start, as before.
- A `command` backup with a `kairos` shape is a `lint` error: the VM runs none.
- `backup list` marks a backup of a profile the release no longer declares, which retention never
  prunes.
- A VM backup cut off by its hour's limit or the host daemon's stop stops its runner gracefully
  before removing it, so rclone aborts a multipart upload rather than leaving its parts in the
  destination; the host daemon's stop waits up to 5 min 45 s for it (its unit's stop timeout is
  7 min).
- An OS bundle with `host.packages` fetches the package archive's indexes once; `tune` reads each
  CPU's frequency settings once under `host.power: performance`.
- The VM's host daemon judges an upgrade's routes, inputs and cores before it writes the OS only
  for a release published by the Stowage it was built with; another Stowage's release is judged
  by the boot into it, with that Stowage's rules (falling back on a refusal), and the dry run and
  the job's log say so. `upgrade` says the release's installer is downloaded before the check.
- A schema baseline crossing is refused in the same words on Docker, Kubernetes and the VM: `…
  only with its data wiped: <the way across>`.
- The disk-pressure watch retries a POST that may be taken soon every minute for 10 checks in a
  row, then every 10 minutes (a service whose address never comes).
- A network confirmation refused after its window, or with nothing awaiting confirmation, ends
  with the settings in force.
- The health gate's diagnostics are numbered in the order kept (`failed-boots/<N>-…`), never by
  the clock, keep the services as the failed judgement listed them (`ps.json`), and the last 5 by
  number are kept; a health command's compose file names the boot it ran in, and the gate removes
  those of an earlier boot rather than judging their age by the clock.
- A reboot whose `systemctl` the shutdown stops is the reboot coming wherever it is asked: a
  rollback's keeps the boot assessment's marks and is not journaled cancelled; the health gate,
  the scheduled reboots and the first boot's switch judge it one way.
- The VM's service watch drops a look during which the install's lifecycle lock was taken, even
  taken and released inside it, and reads `docker inspect` only for the services it sees down.

## [0.38.0] - 2026-11-15

### Added

- `vm seed --json` names the NICs it makes the VM with (`interfaces`: each one's role, the MAC its
  cloud-config names it by, generated or given, its network and the primary), so the program
  creating the VM gives each NIC the MAC the VM expects.
- `upgrade --dry-run` and `rollback --dry-run` on the VM run every check the verb runs before its
  job, ask nothing and change nothing (no job, no backup, no OS written, no reboot), and print the
  plan; `--json` answers `nothing`, and a check that says no exits 3, refused.
- On the VM, every verb of `<short>-stow` told "not now" — a job running, a reboot scheduled, the
  boot not judged yet, the install's lock held, a system written that no boot ran yet; for
  `upgrade` and `rollback` another release than the one confirmed — exits 3, refused (nothing
  changed), with a fix naming what to wait for, not 1; a state that cannot be read stays a failure. The host daemon's
  `upgrade` and `rollback` take `dry_run`, from the app socket too.
- `status --json --values` (Docker, `k8s status`, the VM's `status`) adds the values the install
  records that a configuration gives again: the inputs that are not secret (a file as its
  content's digest), the TLS mode, each route's domain, the front's host ports, each route's
  loopback port. A program taking over an install compares its configuration
  with them. On the VM the operator alone reads them.
- `describe --json` lists `tls_inputs`, the inputs a TLS mode the release offers adds (the ACME
  contact, the secret Cloudflare token), with the modes that ask for each: a program knows the
  token is a secret and that the install records it.

### Fixed

- On TrueNAS, the seed ISO (which holds the VM's one-time SSH host key) and the disk image are
  uploaded readable by root alone, not by every user of the host.
- `lint` refuses an `audit.allow` entry for an image of Stowage's the release does not carry (a
  front, backup runner, volume helper or scanner its project makes no use of): it could never
  match a finding.
- The front image's build refuses a module raised by `--replace` that Caddy now requires at or
  past that version, instead of holding Caddy's requirement back.
- A re-run and an upgrade refuse a port of the front that `.env` should record and does not, or
  records as no port (an edited `.env`), naming the key and the flag that gives it, instead of
  publishing port 0, failing at Compose, or blaming `--http-port 0`; a signed or zero-padded port
  is no port either.
- Two disk builds on one host (two `ship`s of a project with the `kairos` shape, whoever runs them
  and wherever, a container included) no longer fail on auroraboot's loop devices: auroraboot's
  container is always `stowage-disks-loop`, and a second build waits for that name, saying so.
- A run waiting for an install's lock while that install's directory is removed (a first install
  that failed, an uninstall) is refused naming the fix, `run again`, instead of failing with `no
  such file or directory`.
- `vm create --create opentofu` holds the VM's state lock for its whole run, as `vm attach` and
  `vm destroy` do: a second create of the same host, or one of them, is refused while it runs;
  and a lock of the platform's VM name: a create of the same `--vm-name` for another host meanwhile
  is refused.
- Every lock Stowage takes is on the file at its path once taken: a run waiting for one that
  another run removed or replaced takes the file there now, never a removed one; a backup, a
  settings change and the image's wait of `vm create` end with Ctrl-C.
- A rollback on Docker to a release with a provision hook says that the hook is not run on a
  rollback, and when to run `provision`, after the previous installer's own `not provisioned
  (--no-provision)` line.
- `services:`, `tune:` and `shapes.kairos.size` (or its `min` / `recommended`) written with
  nothing under them are refused as the spec is read, naming the line, by every command, not by
  `lint` alone.
- `lint` refuses an ARP setting in `host.sysctls` out of the kernel's range (`arp_announce` 0–2,
  `arp_ignore` 0–3 or 8), `all`'s included, on every shape.
- `lint` refuses a `data.volumes` entry the compose file does not declare, and a `data.reset`
  volume that is `external` or whose `name:` is interpolated, which `reset-db` refused only at
  run time. The full example's unused `certs` data volume is now its front CA's `ca` volume.
- `lint` no longer warns of `host.sysctls` keys the kernel holds once for every namespace
  (`tcp_max_orphans`, `inet_peer_*`, `fib_sync_mem`, `nf_conntrack_expect_max`,
  `nf_conntrack_buckets`).
- `lint` warns of each front route with a guard (`client_auth`, `allow_input`, `forward_auth`,
  `basic_auth`) when the Kubernetes chart, or a subchart, declares an Ingress, HTTPRoute or
  GRPCRoute, or a kind a template computes: there the chart's ingress serves the route and Stowage
  enforces none of its guards.
- `lint` refuses a `command` provider's `timeout_s` past 3600 (an hour), the health, schema and
  backup commands' included: a longer one could hold a run without bound.
- A signing key file is read once per command, and every signature and attestation is made with
  the key as read — the one a test release's `--test-public-key` was checked against: a key file
  replaced during the run signs nothing. cosign gets a 0600 copy of it for each call, in the call's
  private directory, removed when the call ends.
- A signature check reads at most a digest's 64 newest signatures, ordered by the annotations
  cosign writes, instead of refusing every digest with more than 64 referrers: one found among
  them verifies, and none found with others left unread is still a failure to check, never
  "unsigned". SBOM attestations are neither read for a signature nor counted in "N signature(s)".
- A hook ship reads its `-f` spec through the links the commit records, not those the working
  tree holds when the queued ship runs.
- A disk push's commit is awaited for a bound scaled to the disk (2 minutes and a second per
  20 MiB), so a registry verifying a large disk for longer no longer fails a push whose every byte
  went up.
- Two ships or promotions on one cache registering the same cloud image take turns, instead of
  working over the same OpenTofu state.
- On a registry that deletes no tag alone, content whose tag is retired while another tag holds it
  is marked with a `held-` tag in the registry and deleted once no tag holds it: by the run that
  marked it (a prune, a yank, a ship), or by the next prune after a run that stopped part way;
  `:latest`'s retirement of an installer no release tag holds marks it too. A channel name,
  template or release tag starting with `held-` is refused.
- A mutable release shipped again without an image key, an installer or an OS bundle its earlier
  build had removes that tag, which nothing removed before.
- An install whose recorded values total over 1 MiB is refused when they are recorded: every
  compose and hook process gets them all in its environment, which the kernel bounds.
- `k8s status` after an uninstall that kept the release's history says it is uninstalled and
  which revision `k8s rollback` brings back, as `k8s status --json` does, instead of printing the
  uninstalled revision as if it ran.
- An L4 route's bind address on the loopback range other than `127.0.0.1` (`127.0.0.2`) is taken
  as the host's when the host has a loopback address, as Linux answers on all of `127.0.0.0/8`.
- A start whose memory shares' floors together pass the part of the host's RAM the shares take
  warns that services may be OOM-killed, naming the share's `min` to lower.
- `status` run by another release's installer names the tuning profile of the release the
  install runs (read from the installer it keeps), or says it is unknown.
- An upgrade asks again a recorded value the new release's checks refuse (an element a set
  refuses, a narrower check), as it does a table it picks no file of, instead of failing at the
  start once the tables loaded.
- A Kubernetes release in helm's `unknown` status is a release: `k8s install` no longer goes over
  it, and the verbs acting on the installed release refuse it, naming `helm history`.
- `k8s upgrade` without a pull password gives the chart the pull Secret the release names once it
  sees it exists, instead of reusing it unseen; one deleted out of band is refused on the
  project's registry (naming the token to set) and dropped on a mirror.
- `tune` on a consented Docker host reports a service's table loaded from other rules than the
  install's values pick now (a re-run picked another file) as differing until the next start.
- A start making a volume that awaits its uid creates only the services mounting it (and the
  front when its volumes are new), so a running stack keeps running if the volume helper fails.
- Two `mirror import`s into one prefix from one machine run one after the other; a failed first
  install keeps the Docker daemon's `ca.crt` it wrote, which another install may rely on.
- A mirror export carries the yanks of the channel's later publications, and the import marks
  them yanked in the mirror: `upgrade --version` there refuses them as at the source.
- A start-first service's failed new container's quoted log lines show the install's secret values
  as `<redacted>`.
- A start-first overlap is judged by the installer's own reads of the new container, never
  Compose's `--wait`: a decline is seen under a restart policy, and a new container its restart
  policy restarts after the signal is read on for 10 minutes instead of failing at once.
- An `update_ready` file is read at most 1 MiB, and one that is a directory or a link fails the
  overlap naming it.
- A rollback pulls the previous release's images before it switches, also for a release another
  Stowage built (from what its record names).
- A start pulls an image pinned by digest that left the engine before it reads the stack, and
  Compose never pulls during a start.
- A plain `upgrade` whose channel cannot be read fails (exit non-zero) instead of saying the
  install already runs the release.
- On the VM, an upgrade's refusals name no flag its `upgrade` does not take; an added loopback
  route may take the port of a route the release removes.
- `kubectl` commands in messages name `--context` when `--kube-context` is given, as the run's own
  kubectl commands do.
- A re-run giving a new byo certificate refuses a domain that is no domain name before it reads
  the certificate files.
- An upgrade whose start fails leaves its change marked, so a later re-run keeps no `.env.prev`
  of values that never came up.
- A file input's file is written with every `.env` write, so a compose command run before the
  first start never makes a directory at its path.
- A release payload naming a picked table's value no file may be named after is refused as lint
  refuses it.
- On bare metal, `vm create`'s refusal of a NIC without its MAC also names the optional NICs
  `--interface <role>=mac=…` takes.
- `vm create --platform P` boots the disk set holding one of P's formats (the channel's, when a
  mirror holds the release's own in another format alone) instead of refusing the platform.
- `vm seed --channel C` refuses a channel the registry holds no release of the project on.
- A Docker host's `tune --apply` refuses a second install of the same project while one holds the
  consent, and an allowlisted route whose port another install's allowlists hold.
- On a Docker host whose firewall reloaded (DOCKER-USER flushed), `status` and `tune` no longer
  call the forwarding chain or the allowlists applied; the next start as root applies them again.
- `tune` not as root on an install whose record it cannot read says the roles' forwarding is not
  judged, instead of advising rules for a NIC named by none.
- `tune --print` on a consented Docker host says each start loads the services' tables, instead of
  telling the operator to load them.
- A consented Docker host's tables boot unit is inert without the consent's record and fails no
  boot, so a rollback to a release built by an earlier Stowage leaves it harmless.
- A host whose kernel tracks no connection no longer warns at every start that
  `flush_conntrack`'s flush failed.
- The network profile no longer widens a container's range of local ports, which could take a
  port a service of that container listens on.
- An interrupted disk build removes its container whenever the interrupt came, even while the
  container was being created.
- `vm create --tofu-var` checks every value a module's bool or number variable takes before
  OpenTofu runs, in the form too.
- `vm attach` run again with the right `attach=` bridge after an apply failed on a wrong one adds
  the NIC on it.
- A restore loads only the objects the install's own backups uploaded: each backup records the
  SHA-256 of its objects, and a restore downloads and checks every one before anything stops
  (and again as it loads), refusing one that differs; a backup the install recorded nothing of
  (a machine made again) is refused unless `restore --unrecorded` is given.
- A backup destination that writes to the backup runner's own disk or memory (a `local` or
  `memory` remote, a bare path, or a remote leading to one) is refused at `backup config`, `backup verify` and every run,
  as its backups would be lost with the runner's container.
- The VM's upgrade before a first install no longer finds the install's lock held at the instant
  another run takes it: the daemon reads who holds it without taking it.
- Each VM NIC's network revert is its own, undoing that NIC's change on the interface it was made
  on: two changes awaiting confirmation at the daemon's start are each undone, and a record the
  daemon's start could not apply is tried again. A change of one NIC still waits while another
  NIC's change awaits confirmation.
- `health` and `GET /v1/health` share one check among the requests arriving while it runs and
  reuse its verdict for 5 s, unless a job or a service verb ran since: polling it no longer
  starts a health container per request. A check that hangs ends at its bound.
- `status` no longer shows the last failed boot once a later upgrade's boot on trial is judged
  healthy after it in the event journal; the passive system's boot after a fallback keeps it.
- The host daemon's refusal of a `reset-db` or a restore on an install of another release names
  the project the OS carries.
- The VM acts on an `upgrade` made before its first install, and on a `reset-db --new-baseline`,
  at the boot of the OS they wrote (their marks now name the release by version and revision,
  which the OS's installer carries), and its health gate keeps the OS such an upgrade wrote, as
  it keeps the one a first boot switched to, rather than fall back to an OS that cannot run the
  install.
- `tune --check` gives an interrupt the kernel manages (its move refused at `--apply`) as advice
  naming `isolcpus=managed_irq`, not as a difference that never clears.
- `tune` run as a user who sees no dockerd (`/proc` mounted with `hidepid`) says so, rather than
  judge `/etc/docker/daemon.json` in place of the engine's own file.
- The VM's first `install --channel` is checked against the registry as `vm seed --channel` is: a
  channel the publisher ships no release of the project on is refused.
- `vm create` refuses a cloud-config past the cloud's user-data limit before asking to go on.
- The container of a `command` health check a reboot interrupted is removed by the next boot's
  health gate.
- The VM's service watch and `status`: a service the stack declares with no container is down
  (`missing`), a `compose run` container left exited is no service's, and a container gone between
  two reads no longer hides the others' facts.
- The host daemon does not start when the kernel's boot id does not read: its event journal tells
  boots apart by it.
- The disk banner whose login-screen reload failed is reloaded by the next check, and a disk that
  stops reading takes the banner down.
- A restore downloads each object once into a private file of the install, checks its digest and
  loads that checked copy: an object changed after its check never loads; a disk that cannot hold
  the largest object is refused before anything stops.
- The backups' digest record is bounded in bytes, and keyed by the object's path in its
  destination: the same name in another destination has none (a restore of a backup recorded
  before this release needs `--unrecorded`).
- A network change not confirmed is undone on the interface it was applied on, wherever the
  default route moved since, its retries included.
- A disk build's `docker create` runs in a session of its own, and one that fails has its
  container removed when it carries that create's label, never another build's of the name.
- `tune` reports a table whose recorded value picks no file on that table alone.
- Two Docker installs' L4 allowlists on one port bound to different addresses no longer conflict
  (`0.0.0.0` and `::` overlapping every address of their family);
  every start checks the conflict, not the consent alone; and the refusal of a second install's
  consent names how to remove the other's units when its directory is gone.
- `vm create` boots the release's own disk set: a `--platform` format the registry lacks of it is
  refused, naming it, never replaced by another release's set.
- A module's `number` variable takes an HCL number literal alone (no `NaN`, infinity or
  hexadecimal), and a variable's `type` and `default` are read however its block is indented,
  never an object attribute's.
- A scheduled backup that fails is tried again 15 minutes later, at most 6 times in 3 hours, on
  the VM and on Docker.
- A one-shot container of the installer's (the provision hook, a schema or health command, a
  kit's run, the backup runner) whose run is cut off is removed by its name at once, the docker
  CLI killed by a signal included.
- The rclone config a Docker install's read writes goes into the install's own directory, never
  the system's temporary one; one a killed run left is removed by a later run, and by the VM's
  daemon as it starts.
- `k8s install` on a release uninstalled with its history kept is refused as `k8s upgrade` is,
  both naming what works: `k8s rollback`, or `helm uninstall` (dropping its history) then
  `k8s install`.
- A Docker host's boot unit loading the services' tables shows its failure as the unit's.

## [0.37.0] - 2026-11-12

### Added

- `images.<key>.untracked`: files an image's build reads that git does not record (a binary made
  elsewhere). Each one's SHA-256 enters the image's inputs digest, every ship logs it and records
  it on the release, a required one absent is refused, and a hook ship takes them from the
  working tree when the commit is made, so its release is built with the files the commit was
  made beside. Lint refuses one git records or the build is not sent.
- The cloud modules of `vm create --create opentofu` take an address reserved beforehand, which
  `vm destroy` keeps: `ip_id` on Scaleway, `eip_allocation_id` on AWS, `public_ip_name` on Azure,
  `nat_ip` on Google Cloud, so a stack's first install can know its own address, or another VM's,
  before the VM exists.

### Changed

- On a cloud, `vm create` uploads a disk once and makes its image once for every VM of it in one
  place (zone, bucket, project…), in a state of its own that `vm destroy` keeps; the next VM of
  that disk uploads nothing.
- The security group Scaleway's module makes, and the firewall rules Google Cloud's makes, open an
  L4 route's port only to the sources its allowlist holds when the VM is created, not to every
  source.

### Fixed

- `hook post-commit` reads a version file the commit records as a symbolic link through the
  link, as a checkout does, not as the link's own target path.
- `ship`'s refusal of a release already published on an immutable channel with other inputs
  names what differs: each part of the build (`installer`, `os-bundle`, an image…) and the inputs
  of it that changed (`changelog`, `spec`, `pull`…), read from what each ship now records on the
  release.
- `vm create` stops waiting as soon as the VM's first boot gives up in Kairos's recovery system,
  naming the automatic reset's last journal lines, instead of waiting out `--wait`; `status
  --json` on the VM says so in `reset_failed`, and `status` after the boot.
- A VM's first boot no longer stays in its recovery system when Kairos's automatic reset reads
  the disk before udev has recorded its partitions: the reset waits for udev's queue (two
  minutes at most).
- `vm create`'s message when the VM is not ready by `--wait` names the platform's own console
  (Scaleway's Console tab, a cloud's serial console, `virsh console`, …), not TrueNAS's on every
  platform.
- Lint's `cap_add` warning names only the capabilities a service holds beyond Docker's defaults:
  a service that drops every capability and adds back some of the defaults (`cap_drop: [ALL]`,
  `cap_add: [CHOWN, SETUID]`) is no longer warned, and `CAP_`-prefixed or lowercase names are read
  as Docker reads them.

## [0.36.0] - 2026-11-12

### Added

- `<short>-stow updates` on Docker and `k8s updates` on Kubernetes say which release the channel
  offers after the one the install runs (or `--version V`'s latest revision), with its changelog,
  or that there is none: read from the install's registry (a mirror with its CA, the operator's
  Docker login where the mirror asks for one), signed by a key the install trusts, as on the VM.
  They name what `upgrade --version` installs: the latest revision of the channel's version, and
  another channel's release only shown. `--json` prints the VM's `updates` object.

- The VM's `status --json` tells when a boot has settled (`boot_settled`: the boot and its health
  gate ran to an end) and gives the health gate's verdict on that boot (`boot_judged`), which
  records whether the gate reboots: a program following an upgrade or a rollback reads the status
  alone, and a gate that ended without a verdict, or a fallback whose reboot never comes, is told
  at once. The `boot.judged` event carries `reboots` too, and a reboot that fails is a second
  verdict saying so.
- A VM reads a mirror that asks for a sign-in: `vm create --registry-login-file FILE` (a Docker
  config holding one `auths` entry for the mirror's host) gives the login over SSH once the VM
  answers, never in the cloud-config, and `sudo <short>-stow registry-login FILE|-` on the VM
  records or replaces it (`--remove` removes it). It is kept through upgrades and rollbacks, used
  for the mirror's host alone, and `status --json` says a login is held (`registry_login`, the
  host), never its value. `vm seed --registry-login-file` checks it and names its host in the seed.

### Changed

- A handoff's `run` and `check` may list a file input or a secret delivered as a file
  (`as_file: true`) in their `env`, which lint refused: the step gets each as its file, mounted
  read-only as the provision hook's, its path in `STOWAGE_INPUT_FILE_<KEY>` or
  `STOWAGE_SECRET_FILE_<KEY>`, the value in neither its environment, its Job, a log nor the kit.
- `provision` and a handoff's step refuse to run without the file they list: on Docker and the VM
  an install whose `.env` lost `STOWAGE_FILE_SALT`, on Kubernetes a key the release's Secrets do
  not hold; each names the install that writes it.
- Lint counts an input a handoff's `run` or `check` lists in its `env` as read: it no longer warns
  that nothing reads it.

### Fixed

- A VM's `rollback` ends its job 5 s before it marks the boot assessment and runs `systemctl
  reboot`, so the verb run on the VM over SSH prints its result (`--json`'s Result included)
  before the reboot stops the session; nothing is marked while it waits, so a host daemon stopping
  then leaves the next boot as it was. A mark that cannot be written is now a `reboot.cancelled`
  after the job's end, as a failed reboot is; the journal's `reboot.scheduled` and `job.ended`
  name the 5 s.

## [0.35.0] - 2026-11-12

### Added

- `status --json` reports what an install holds that a program would otherwise have to remember:
  the image registry it pulls from (Docker and Kubernetes), and on Docker the installer's short
  name, the loopback address and the digest of the registry CA its first install recorded.
- `describe --json` lists the release's default registry, its front routes (the names `--ports`
  takes, with their loopback ports) and its L4 routes, so a program tells a route an upgrade adds
  from one the install holds. `describe` prints them too.
- `status --json` `inputs` lists every input the install records, whichever release declared it:
  after a rollback, an input only the newer release declares is still listed. Each install, re-run,
  upgrade, rollback and provision records the keys.
- `<short>-stow --version` (`-v`) prints the release the installer carries and the Stowage release that
  built it, which `describe --json` gives as `stowage_version`.
- `--image-pull-login` on `k8s install` and `k8s upgrade` writes the cluster's pull Secret for
  a mirror's host from the Docker client's login the installer reads it with, so an authenticated
  mirror serves the cluster; a login the cluster's nodes cannot pull with is refused.
- A refusal of `status --json` prints one object on standard output (`refused`, with its reason
  and fix), and both it and the verbs' `--json` result say `absent` when the refusal is that no
  install is there — no install in the directory, no helm release in the namespace (never `k8s
  uninstall`'s, which does not tell what helm keeps) — so a program tells "nothing installed here"
  from any other refusal.
- `secrets show KEY` prints a `settable` secret's value (a login an operator signs in with) on a
  terminal, refused into a pipe or a file, on every shape (`k8s secrets show` on Kubernetes, the
  VM's host daemon on its operator's socket alone); an install's closing report names the logins
  and that command, so a generated login is no longer read out of the install's files.
- The front's ports are held to `front.interfaces` (roles, as an L4 route's `interfaces`) for
  real on the VM: Docker translates them to the front's container past the input chain, and the
  firewall's `hold` chain, ahead of its forward chain, drops every packet of a connection to them
  from any other interface (a container on any of the stack's bridges included). A consented
  Docker host holds the front's and the L4 routes' interfaces in `DOCKER-USER` too.
- `as_file: true` on a secret, or on a secret input, delivers its value as a file a service
  mounts (`${STOWAGE_SECRET_FILE_<KEY>}`), never as a variable `docker inspect` or a child process
  could read; lint refuses the variable read anywhere in the compose file. On Kubernetes the value
  is in its own Secret (`stowage.secretFiles`), never in the release's, so `envFrom` cannot reach
  it; the provision hook gets it as a file on every shape. `describe --json` gives `as_file`.

- `vm seed --json` gives what a tool creating the VM itself would otherwise rebuild: the NoCloud
  `meta_data`, each disk's sizes (`os_gib`, `disk_gib`, with `app_gib`), the cloud image name a
  `ship --disks` registered from it, and with `--disk-dir` the disk itself as a file. Its first
  install can wait for a secrets export (`--await-secrets`) or a `byo` certificate (`--tls byo`)
  given over SSH. A refusal prints its object on standard output.
- `vm host-key HOSTNAME --json` reads a VM's own SSH host key through the key it is checked by
  and retires its seed's one-time key at once; `sudo <short>-stow host-key [--json]` does the same
  on the VM itself, with no SSH.

### Changed

- `front.interface` (one role) is `front.interfaces` (a list, as an L4 route's): `interfaces:
  [uplink]`.
- On the VM, a verb run before the first install is a refusal (exit 3) whose `--json` result says
  `absent`, as on Docker and Kubernetes; its `status --json` reports the first install's registry,
  installer short name, loopback address and registry CA digest, as Docker's.
- The Kubernetes refusals of a missing release read one way on every verb: the install to run,
  with what the run was given that it takes (the mirror, its CA, `--image-pull-login`, the values
  files, `--unsafe-sysctls`, `--no-provision`), as reason and fix; `k8s upgrade --version V` names the version
  asked. A Docker host's `backup`, `backup config` or `restore` with a `--dir` that does not
  exist is refused the same way, instead of failing on the daemon's lock, and whatever fails as
  that daemon starts is reported as itself, never as an unreachable daemon.
- `vm create` and `vm seed` of a release whose front offers no TLS mode a VM is made with refuse
  it (exit 3) instead of failing, `vm seed` naming `--tls byo` where the release offers it.
- `k8s uninstall` with no release refuses naming another tool's objects of the release's names it
  finds there.
- The front Stowage adds runs as a user of its own (65532, or the uid the project gives the
  volume it publishes its CA in), with no capability, no privilege gain and a read-only root;
  each start gives it its volumes, an older front's included, and copies a `byo` certificate into
  its own volume. Lint refuses uid 0 on the volume `front.publish_ca` names. A start refuses a
  published-CA volume whose directory another uid holds (a service's image made it so, or the
  front ran as another uid there), naming the `chown` that gives it to the front.
- The provision hook, the schema check and a handoff's step run with a read-only root, `/tmp`
  their only writable path, on Docker and Kubernetes alike, and on Docker never as root: as their
  image's own user when it declares one other than root, else as uid 65534 (a failure then says
  so, the fix the image's own `USER`).
- The volume helper and the backup runner drop every capability but those they need: `CHOWN`
  and `DAC_READ_SEARCH` for the volume helper; `DAC_OVERRIDE` and `DAC_READ_SEARCH` for rclone
  (its configuration, whoever owns it); `CHOWN`, `DAC_OVERRIDE`, `DAC_READ_SEARCH`, `FOWNER`,
  `FSETID` and `MKNOD` for the runner's archives and restores.
- `host-key` and `vm host-key` check sshd's reload as they retire the seed's one-time key: one
  that did not take prints the key with `"retired": false` and a reason, and exits 1 naming
  `sudo systemctl reload ssh`; `vm create` then keeps its pin of the one-time key and warns. A VM host name no known_hosts line can hold no longer refuses
  `host-key`: the key and its retirement as ever, the line left out with a note; `--json` adds
  `public_key`.
- A front serving L4 routes alone no longer publishes or opens ports 80 and 443, which nothing
  served on: in `acme` mode it opens 80 alone, for the certificates' challenge. An install checks
  free only the ports the front serves on, and an upgrade whose release serves them first checks
  them then. On the VM, a plaintext L4 route to a service whose profiles are off is no longer
  opened.

### Security

- The VM's firewall drops what it would forward between its own interfaces, IPv6 as IPv4, unless
  a role's `host.interfaces.<role>.forward` declares the pair, whatever Docker's forward policy
  is: a neighbour could route new IPv6 flows through a VM that forwards IPv6.

## [0.34.0] - 2026-11-11

### Added

- `status --json` on the Docker installer and `k8s status --json` on Kubernetes: one object for a
  program — the installed and the previous release (with the installer image each came from, by
  digest), the channel, a first install still running or failed, and the recorded input names
  (never a value) — beside the services or the helm revision. Each install now records its
  release's installer image.
- `describe [--json]`: what the release an installer carries offers, read with no install — the
  release, the shapes, every input (a secret's default never shown) and the settings a re-run
  cannot change. The JSON carries `format`, the machine interface's version.
- `--json` on install and its re-run, `upgrade`, `rollback` and `uninstall`, Docker and
  Kubernetes: one result object at the end (`done` or `nothing`, the release installed, the
  refusal and its fix), the run's other output on standard error. A refusal — a rule saying no
  before anything changed, an operator's "no" included — now exits 3 instead of 1; a rollback
  whose start fails after the files went back exits 1.
- The VM's `<short>-stow` speaks the same interface: `describe [--json]`; `status --json` gains
  `format`, `previous` (the release `rollback` goes back to) and `inputs` (the recorded input
  names, never a value); `install`, `upgrade` and `rollback` take `--json`, and every verb exits 3
  on a refusal, the host daemon's included (its error answer now says `refused`, with the fix
  when it has one). A boot whose rollback check refuses after the OS fell back exits 3 when it
  changed nothing before it, else 1. A refusal's fix is the one that holds on the shape that
  refused: on the VM, a refusal whose Docker fix runs another installer with `--dir`, the kept
  installer, or a flag the VM's upgrade does not take carries no fix and still exits 3 — another
  release's or project's install, a kept installer that is not the release the install runs
  (whatever their order), a directory holding another project's files; an upgrade the boot
  would refuse names the re-run of the install that clears it, where one does; an upgrade by an
  earlier release's installer, or a baseline crossing confirmed for another release, carries no
  fix there, and a verb run before the first install names `sudo <short>-stow install`. `vm seed`
  and `vm create` refuse a missing, unknown or unoffered `--platform` with exit 3 once the
  registry has been read, naming the platforms it offers; a registry that cannot be read, or that
  offers no disk, is a failure (exit 1).
- `k8s status`, with or without `--json`, is refused (exit 3, nothing on standard output) when the
  namespace holds no release of that name, as Docker's `status` of a directory without an
  install, its message and its fix naming the `k8s install` to run; `k8s provision`, `k8s
  reset-db`, `k8s secrets rotate` and `k8s kit create` / `kit check` refuse it the same way. They
  exited 1 before.
- `vm seed --json`: a new VM's first-boot material for a tool that creates the VM itself — the
  cloud-config (no secret input), a one-time SSH host key with its `known_hosts` line and
  fingerprint, the disk image each platform boots, by digest, and the secret inputs the first
  install waits for (`waiting`, names only); it never asks.
- `--known-hosts-line LINE` on `vm kit` and `vm attach`: the VM's host key given as one
  `known_hosts` line, checked strictly, for a VM `vm create` did not make.
- `vm seed` given a secret input on its command line refuses it as a seed (exit 3), and `vm create`
  refuses one there with exit 3 too, naming no fix.

### Security

- Built with Go 1.27.4 (net/http HTTP/1 server fix GO-2026-6859): Stowage's own binaries, the
  front (Caddy built from source), the backup runner and the scanner.

## [0.33.2] - 2026-11-10

### Fixed

- The Kairos OS bundle builds again: since 0.33.0 its build demanded that SSH refuse root's login
  while nothing in the image refused it (kairos-init allows root by key), so every `ship` of a
  project with `shapes.kairos` failed. The image now refuses root's login over SSH, and each check
  of the bundle's build that fails names itself (an SSH setting with the value it found, the CIS
  settings left out, the kernel modules) instead of a bare `exit status 1`.

## [0.33.1] - 2026-11-10

### Fixed

- An L4 route the front terminates TLS on no longer negotiates HTTP/2 (or HTTP/1.1) on its
  upstream's behalf: the front selects no ALPN protocol, so an HTTP/2-capable client reaching an
  HTTP/1 upstream through the route works. A route whose upstream needs one declares it
  (`l4.routes[].tls.alpn`, e.g. `[postgresql]` for PostgreSQL's direct TLS); the chart is given
  it too (`stowage.l4[].tls.alpn`).

## [0.33.0] - 2026-11-10

### Added

- Scaleway: `cloud_publish: [scaleway]` registers the release's qcow2 disk as an instance image in
  the publisher's Scaleway project (uploaded to an Object Storage bucket, imported as a Block
  Storage snapshot, made a UEFI x86_64 image; settings `STOWAGE_SCALEWAY_BUCKET`,
  `SCW_DEFAULT_ZONE`, `SCW_DEFAULT_PROJECT_ID`), and `vm create --platform scaleway` creates an
  instance from it, by hand or with OpenTofu (a preview: a security group from the SSH sources
  and the opened ports, a routed public IPv4 address, the cloud-config as its cloud-init user
  data). The OS bundle now reads its user data from Scaleway's metadata service.

### Changed

- A front may serve L4 routes alone: with an `l4` route whose TLS the front terminates,
  `front.routes` and `front.portal` may be left out (lint refused them before). The install then
  ends naming the L4 routes, with no URL; the routes' certificates are obtained, renewed and
  published as before, in every TLS mode.

## [0.32.1] - 2026-11-09

### Changed

- Repository maintenance only: Stowage itself is unchanged from 0.32.0.

## [0.32.0] - 2026-11-09

### Added

- An L4 route can have the front terminate its TLS (`l4.routes[].tls: { domain_input, client_auth }`):
  the front serves the certificate its TLS mode gives the route's domain (`acme`,
  `dns-cloudflare`, `byo`, `internal`, renewed with the HTTP routes'), optionally asks for a
  client certificate, filters the route's allowlist itself, and passes the plaintext stream to the
  upstream. In loopback mode the route passes in the clear on the loopback address alone; on
  Kubernetes the chart gets the route's `tls` in its values and terminates it. `--domains` names
  such a route.
- `vm kit create`, `vm kit check` and `vm kit take`, run from the operator's machine on a VM
  `vm create` made, over the SSH link it pinned: a kit is issued on the VM and written on the
  operator's machine, a kit opened there is given to the VM's install, and neither the code nor a
  secret answer touches the VM's disk. The VM's `kit create --json` is the request and answer
  they exchange.

### Changed

- The front runs Caddy 2.11.9, the backup runner rclone 1.76.0 and the scanner grype 0.121.3: the
  modules Stowage forced over their earlier releases are now their own (the backup runner's all
  of them), but for golang.org/x/net, go-jose and klauspost/compress in the front and go-jose in
  the scanner. Their bases, and the examples', are repinned to their latest builds.
- The SSH links to a VM never read the operator's global known hosts, and `vm create` records the
  VM's key in the clear, under the address the later links look it up by, whatever the operator's
  ssh configuration makes of the name.

## [0.31.0] - 2026-11-09

### Added

- `images.<key>.contexts`: named build contexts, `{<name>: <directory of the project>}`, each
  passed to the build as `--build-context <name>=<dir>` — the source a stage reads with
  `COPY --from=<name>` (or the stage of that name it replaces) that the main context does not
  hold, such as a submodule the main context's `.dockerignore` keeps out. Each is sent less its
  own `.dockerignore`, as BuildKit applies it, and enters the image's inputs digest so: its name,
  its path and every file it sends. A stage may start `FROM` one. Lint refuses a name that is
  not lowercase letters and digits separated by single `.`, `_` or `-`, a path that is no
  directory of the project or goes through a symbolic link, a
  `.git` its own `.dockerignore` does not exclude (a git submodule needs a `.dockerignore` of its
  own excluding `.git`), `scratch` and `context` (BuildKit's own names for the empty base and the
  main build context), and `contexts` beside `ref`; it warns of a named context named like a stage,
  which it replaces.

### Changed

- An image's `context` reached through a symbolic link, at any component of its path, is refused
  by lint and by every command that computes the release's inputs (`ship` among them): BuildKit
  sends the directory the link points to while the inputs digest follows no link, so an edit there
  would never rebuild the image. Name the directory itself.

## [0.30.1] - 2026-11-07

### Fixed

- `k8s status` no longer stops listing a handoff's leftover Secrets at the first one it cannot read,
  and no longer calls a Secret holding only a check's exit status a leftover of issued values.
- The front and the backup runner are built with zlib 1.4, past the critical CVE-2026-4738, which
  their Alpine base images do not carry yet: a project signing its releases with `fail_on: critical`
  was refused at `ship`. The examples' Python images are raised the same way; a project's own Alpine
  images need the same until their base carries it.
- A VM whose first boot took a newer release's OS, and whose first install waited for what
  `vm create` gives over SSH (secrets, secret inputs, a `byo` certificate, a kit), no longer falls
  back to the older OS at its next reboot, where the older installer refused the install: the
  health gate marks such a boot good, as the older OS holds no install to go back to. Its first
  install unhealthy before any upgrade, that OS is kept instead of rebooting into the older one,
  which served nothing.

### Changed

- Handoff kits are proven further on real installs. A VM's first
  install is run from `vm create --kit`, with a `byo` certificate and a secret input over the pinned
  SSH and in no seed; kits cross between Docker, Kubernetes and the VM in both directions; a release
  shipped after a Stowage upgrade issues and takes one; and a secret input reaches a bare-metal
  machine's first install past the switch to its release's OS.

## [0.30.0] - 2026-11-07

### Added

- An answers file can carry the front's TLS settings in a `tls` section (`mode`, `cert`, `key`, `ca`,
  `client_ca`, with paths relative to the file), so one file describes a whole install on Docker or
  the VM. Flags still win over the file.
- `vm create` can give a new VM its secret inputs and its `byo` certificate: from `--answers` or from
  the new `--cert`, `--key`, `--ca` and `--client-ca` flags. They are checked before the VM exists,
  never enter the cloud-config, and reach the first install over SSH, pinned to the VM's one-time host
  key. A project offering only `byo` can now create a VM.
- `install --given` on the VM takes that material from a file or standard input, and creating a VM
  by hand writes it to a private `.given` file and prints the one command that delivers it.
- File inputs now work on Kubernetes: `k8s install` and `k8s upgrade` take `--set-file`, the chart
  mounts each file from a Secret named in `.Values.stowage.files`, and the provision hook's Job gets
  the file as it does on Docker. Lint no longer refuses a file input on the `kubernetes` shape, and
  warns when a chart does not read that value.
- `k8s install` and `k8s upgrade` take `--given`. On an upgrade, the inputs it gives replace the
  recorded values and restart the workloads reading them; a `tls` section is left out and said, as
  the chart's ingress owns TLS on Kubernetes.
- Handoff kits: `--kit FILE` on the Docker and VM installs, `vm create`, `k8s install` and `k8s upgrade`
  takes inputs and TLS material sealed with a code, which comes from `--kit-code-file`,
  `STOWAGE_KIT_CODE` or the terminal. A kit is refused with the wrong code, when changed, for another
  project or once expired; an input meant for another shape is left out and said.
- Projects declare `handoffs`, and `kit create` issues a kit from a one-shot container on every shape,
  writing it 0600 and printing its code once; `kit check` runs the handoff's check, exiting 0 proven,
  2 unknown, 1 not proven. On Kubernetes the values reach no pod log, and their Secret is deleted.
- The Docker install takes `--given` too. On Docker and the VM, an input given for another shape is
  left out and said, instead of refused.

### Changed

- On Kubernetes, an extension's tip names `k8s upgrade --kit` or `--given` to give its endpoint, in
  place of uninstalling the release.

### Fixed

- The bootstrap script run from its file leaves standard input to the installer, so `sh <name>.sh
  secrets set KEY < FILE` and `exec -i` read it; only a script arriving on standard input itself
  hands the installer the terminal.
- A dry run on Kubernetes shows a file input by its digest, and a Kubernetes upgrade or install over
  a kept Secret refuses a recorded value the release cannot read.
- `vm create` with OpenTofu on a disk of another release now waits for that release's OS before it
  sends anything, so `--secrets` no longer dies in the first boot's reboot.

### Changed

- `make self-audit` names, on every run, each module the recipes force over what their upstream's
  release ships, and reminds that each is a stopgap to drop once that upstream carries the fix.

## [0.29.0] - 2026-11-06

### Fixed

- A crafted compressed frame could crash the zstd code Stowage's binaries carry (GO-2026-6865):
  they now use the fixed compression library.
- The images Stowage supplies now run the fixed zstd library: the front, the VM's backup runner and
  the scanner. The backup runner and the scanner also move past excessive memory use when
  decompressing crafted brotli streams (GO-2026-6888, which the scanner rates critical). A project
  gets these images on its next `ship` with this Stowage; an install keeps the ones it has until it
  upgrades to that release.

### Changed

- The full example no longer accepts GO-2026-6612 in Docker's compose plugin or GHSA-cxf3-9h37-7rr8
  in Kairos's installer: today's Docker package and the Kairos 4.4.0 image carry the fixes. A project
  with the kairos shape can drop those two allowances once `ship` says, on a freshly built bundle,
  that they match no finding; its GO-2026-6612 entries for containerd and runc are still needed.
- The examples' OS bundles build on `ubuntu:26.04@sha256:6a06111c…`, whose `pebble` is built with a
  fixed Go (1.27.3), and the full example no longer accepts GO-2026-6612 or GO-2026-6604 in it. A
  project with the kairos shape moves `shapes.kairos.base` to that image or a newer one, and drops
  those two allowances once `ship` says, on a freshly built bundle, that they match no finding.
- The full example's GO-2026-6612 entries left are for the containerd and runc of Docker's
  containerd.io 2.4.2: the bug is in `golang.org/x/net`'s HTTP/2 server, which those builds do not
  link, but the scanner goes by the library's version. Check this again for a later build.

## [0.28.1] - 2026-11-05

### Added
- A provision hook's `env` may name a file input (`type: file` or `pem`): the hook gets the file
  read-only at `${STOWAGE_INPUT_FILE_<KEY>}`, so a check against a private CA takes the operator's
  PEM file, checked by Stowage, instead of a base64 text the hook decodes itself. On Docker and the
  VM; `provision --set` refuses a file input, naming the re-run with `--set-file` that changes it.

### Fixed
- `provision --set` of an input only the hook reads is no longer refused on a project where a
  service mounts a file input.
- A `command` provider's container no longer receives a file input's encoded value.
- `provision --set` of an input only the hook reads is refused when a compose `secrets` or
  `configs` entry may read the install's `.env`, as for a mount of it.

## [0.28.0] - 2026-11-05

### Added
- An L4 route's `allow_input` is enforced on a Docker host too, once its operator consents with
  `tune --apply`: each start as root lets the input's sources reach the route's published port
  and drops every other, from boot on, before Docker restores the containers; such a route is
  then published on IPv4 alone, and an engine with its iptables off is refused, as nothing would
  hold the list. Until then, the install's end and `status` say the route is open to whoever
  reaches the host. Its input is now asked on the `docker` and `kairos`
  shapes the project has (`shapes: [docker, kairos]`).
- File inputs (`inputs[].type: file` or `pem`): a certificate, bundle or key given with `--set-file
  KEY=PATH`, `KEY: {file: PATH}` in `--answers`, or its path on a terminal, at most 48000 bytes; a `pem`
  one is checked and an encrypted key refused. Each start writes it under the install directory and
  a service mounts `${STOWAGE_INPUT_FILE_<KEY>}`; a new value recreates the services mounting it.
- A `tun` interface role can name the device its service opens (`host.interfaces.<role>.device`),
  such as one with a `-` in its name, which no role name may hold: networkd's file, the firewall's
  rules, the forwarding and `tune` then use that device.
- `vm create --add-host NAME=IP` (repeatable) gives the VM host records: written into its
  `/etc/hosts` at every boot, given to the stack's containers, and shown by `network`.
- `exec --stdin` (`-i`) on the Docker shape, as on the VM.
- A consented Docker host whose firewall was reloaded or flushed gets its forwarding again at the
  next start, which now replaces the chain each time; an engine that sends nothing through
  `DOCKER-USER` fails the start, naming the fix.

### Changed
- On the Docker shape, `exec` without a terminal reads its standard input only with `--stdin`,
  as on the VM: one command line drives either shape. Input it drops is warned.
- `lint` refuses an L4 allowlist input asked on the `kairos` shape alone when the project has a
  `docker` shape (ask it on both), and warns of an allowlisted route to a service on the host's
  network on Docker, where nothing translates its port.

### Fixed
- An interrupted `ship --disks` no longer leaves its disk-building container running: it is
  removed before the work directory.

## [0.27.1] - 2026-11-04

### Fixed
- `vm create --platform libvirt --cpus 1` no longer fails its apply (`Provider produced
  inconsistent result after apply … driver.queues`): with one vCPU the NIC sets no queue count.
- The Kairos VM's `/etc/resolv.conf` was an empty file, so the host and a service on the host's
  network resolved no name; it is now a link to systemd-resolved's list of the DNS servers,
  made at every boot.

## [0.27.0] - 2026-11-03

### Fixed
- The built-in libvirt module no longer drifts after its apply: its seed volume is of format
  `iso`, as libvirt reads it, where a plan after the apply wanted to change it back to `raw`.

### Added
- A memory share takes a floor: `memory: { percent: 5, min: 256M, max: 2G }` never gives the
  service less than `min`, whatever the host's RAM; `min` wins over `max`, and the compose file's
  own limit still wins when larger.
- `lint` computes each memory share on the least VM (`shapes.kairos.size.min.memory`): a share
  with no `min` whose limit comes out under 128 MiB there is warned, and so are the shares' floors
  together when they exceed what that VM leaves the shares. A `min` under 64 MiB, or over the
  share's `max`, is an error.
- `status` and `ps` name each service down — exited with a code other than 0, restarting, dead
  or never started — with its exit code and the signal it tells, whether its memory cgroup
  killed it, and the memory limit its container ran with (`down: api exited 137 (SIGKILL),
  OOM-killed (memory limit 96 MiB)`); their JSON carries `oom_killed`, `mem_limit_bytes` and
  `restarts`.
- The VM's host daemon looks at the services every minute once a boot is judged, and journals
  `service.down` when one becomes down.
- `lint` warns a long-running service with no restart policy (Stowage sets none), and the
  long-running services with no healthcheck: the install is done once they run, not once they
  are ready. README states the contract: the install is done when every service runs and every
  healthcheck passes.
- The built-in libvirt module takes `domain_type`: `kvm` (the default) or `qemu`, for a host
  without KVM.
- Stowage's own tests apply the built-in libvirt module through `vm create` against a real
  libvirt daemon, check that a plan right after the apply changes nothing, and that `vm destroy`
  leaves no domain, volume or NVRAM file.

## [0.26.3] - 2026-11-03

### Fixed
- `vm create --platform libvirt --create opentofu` no longer fails once the domain runs ("Provider
  produced inconsistent result after apply"): the built-in module states the firmware's features
  in the order libvirt returns them.

## [0.26.2] - 2026-11-03

### Fixed
- `vm create --platform libvirt --create opentofu` starts its domain on a host whose libvirt
  confines QEMU with AppArmor (Ubuntu, Debian): the built-in module gives the disk and the seed
  by their paths, which libvirt writes into the domain's profile, where QEMU was refused them.
- A failed `vm create` on the built-in libvirt module names `virsh -c <uri> undefine <vm-name>
  --nvram`: a domain whose start failed is left defined, out of what `vm destroy` removes.
- README names the AppArmor rule a libvirt host may need for `virt-aa-helper`.

## [0.26.1] - 2026-11-03

### Fixed
- `vm create --platform libvirt --create opentofu` defines its domain on current libvirt: the
  built-in module turns ACPI and the APIC on, without which libvirt refused the UEFI domain
  ("UEFI requires ACPI on this architecture").

## [0.26.0] - 2026-11-03

### Added
- An entry of the audit gate's `allow` file can be narrowed with `package` and `path`: it then
  accepts its finding only in that package, found in that file, so an allowance written for one
  upstream binary no longer accepts the same finding in every binary of the artifact. `ship` says
  when a refused finding falls outside an entry's package or file, and refuses two entries that
  can both accept one finding (a whole artifact beside a narrowed entry of the same finding).
- When grype reports the same vulnerability twice for one package, once with a fix and once
  without, the audit keeps the fix: the gate no longer passes it as unfixable.
- `audit` names the file each vulnerable package was found in, where the SBOM says (`in
  /usr/bin/containerd`; `path` in `--json`); a module found in two binaries is two findings.
- `ship` lists each allowance it applied: what it accepts, until when, how many findings, and why.
- `lint` refuses an allowance naming an artifact no release of the project can carry, and warns of one
  expired or expiring within 30 days, so a commit hook or CI running `lint` catches them before a
  release.

## [0.25.0] - 2026-11-02

### Added
- The audit gate: a project that signs declares `audit: { fail_on, fixable_only, allow }`, and
  `ship` and `promote` scan the release's SBOMs before the release manifest is written and before
  a channel points at it. A fixable finding at or above `fail_on` stops the ship unless an entry of
  the `allow` file accepts it (each entry names the finding, the artifact and a reason, and
  expires at most a year ahead); `--audit-override REASON` publishes past it, recorded on the
  release and printed by `verify`. `--audit-db FILE` (or `STOWAGE_AUDIT_DB`) scans offline.
- `audit` reports split the findings between the project's artifacts and what Stowage supplies (the
  images built from its recipes, the installer), each into fixable and no fix, and `--json` names
  each artifact's `owner`.
- Stowage's own release is gated the same way: `make self-audit` builds the images Stowage
  supplies, scans them and Stowage's binaries, and refuses a fixable high or critical.

### Changed
- The scanner a signing release carries (`audit`) is grype built from its source with Go 1.27.3
  and patched modules, no longer upstream's image (built with Go 1.26.8).
- The front is rebuilt with patched Go modules and without curl and its libraries, which it never
  ran; the backup runner is rclone built from its source with Go 1.27.3 and patched modules, on
  Alpine 3.24 with only its CA certificates (no FUSE), every base pinned by digest. Together they
  clear every fixable critical and high an audit found in them (curl, brotli, OpenSSL, zlib, the
  Go standard library, golang.org/x/net, go-jose, grpc, chi).
- Stowage's own Go dependencies raise golang.org/x/net to 0.62.0.
- The examples run PostgreSQL 18 (its data volume mounted at `/var/lib/postgresql`, as the
  PostgreSQL 18 image expects) and serve their static pages with busybox's httpd instead of nginx.
  `examples/full` declares an audit gate (`fail_on: critical`) and passes it: its PostgreSQL image
  upgrades its Alpine packages and switches user with su-exec in place of upstream's gosu (built
  with an outdated Go), and the examples' Python images drop pip, which they never run.

## [0.24.0] - 2026-11-02

### Changed
- The OS bundle is built on Kairos 4.4.0 (kairos-init `v4.4.0`, was the release candidate) and its
  disks with AuroraBoot v0.29.0. A project's `shapes.kairos.kairos_init` pins
  `quay.io/kairos/kairos-init:v4.4.0@sha256:eb131bd9b4ed5b9b25cf761af9afa7d462bef27c2d0e9453ce884cf7733a6556`:
  the build refuses `v4.4.0-rc2` now, and lint warns of it.

### Fixed
- With IPv6 on, a VM whose link also carries a router that is not a default one (router lifetime
  0) could run for minutes without an IPv6 default route, its services' IPv6 traffic going nowhere,
  as networkd stops soliciting at the first router that answers. The boot now waits up to a minute
  for the route before Docker starts, soliciting the routers again with backoff, and the host
  daemon keeps soliciting while it is missing; a minute without one is a `status` warning and a
  `network.no_ipv6_route` event.

- `flush_conntrack` now also flushes when a start changes a set an input fills — the input's
  value, a set the release no longer fills, a set an input starts filling — so open flows meet the
  set as it is now (an element removed from an allow-list, or added to a deny-list) instead of
  riding the table's established fast path. The same values flush nothing; a flush still due from
  an earlier boot is dropped at boot. The app socket's set changes never flush.

- A release that stops filling one of a table's sets while it still fills another left that set
  holding the last input's elements until a reboot; the table now loads again, which empties it.

## [0.23.0] - 2026-11-02

### Added
- `services.<name>.flush_conntrack: true`: a start that loads the service's nftables table anew
  (another posture picked, changed rules) flushes the host's connection-tracking table, so the
  connections the previous table admitted meet the new one's rules instead of riding its
  established fast path; without it such a load notes that open connections keep their state.

- `exec --stdin` (`-i`) on the VM: the command reads the caller's standard input, at most 4 MiB.

## [0.22.0] - 2026-11-01

### Added
- `optional: true` on a `host.interfaces` NIC role: a NIC some machines alone have. Absent, `tune`
  reports it `n/a` and the start no longer warns; `vm create` makes it only when `--interface`
  names it, so a cloud VM takes such a release with its one NIC.

- A service's nftables table picked by an input: `nftables: { input: KEY, files: { <value>:
  <file> } }`, each file a table of the same sets, the value checked as it is given and at each
  start, a re-run with another value loading the other table (a default-deny floor and an
  observe mode, one release).

- `secrets set KEY`: a value the operator chooses for a generated secret marked `settable: true`
  (a login's password, set or reset after a leak), read from standard input or asked unechoed,
  the services reading it recreated; on Docker and the VM (the operator's socket alone).

- `prune --channel C --keep N`: a channel's retention, its N latest releases and the pointer's
  kept, on an immutable channel too.

- `exec SERVICE -- COMMAND` on the VM, through the host daemon (the operator's socket alone): its
  outputs and its exit status, without a terminal or input.

### Fixed
- A machine made again with `--secrets` (`install`, `vm create`) no longer issues a new local CA
  for `--tls internal`: `secrets export` carries the front's root and its key, and the new
  install's front starts with them, so the clients trusting the old root still trust it.
- A start-first service's new container that fails no longer takes its log with it: the start's
  error quotes its last lines, read before it is removed, in place of a pointer to compose's
  logs, which only showed the old container.
- A VM of more than one NIC role no longer lets each NIC answer ARP for every address of the VM
  (a client on a shared L2 could reach a role's address on the wrong NIC, its per-interface rules
  missed): `arp_ignore=1` and `arp_announce=2` are set and judged by `tune`, unless
  `host.sysctls` sets them on `all` (a NIC's own lower value is a lint error, as the
  kernel takes the larger).

## [0.21.1] - 2026-11-01

### Fixed
- An upgrade on the VM to a release whose services no longer reach the host daemon (none in
  `shapes.kairos.host_api`) no longer fails its boot and goes back: the installed release, brought
  up first, keeps its app socket for that boot.
- `vm create --secrets` no longer stops with `Job for <name>.service canceled.` when the VM's own
  boot is still starting the install's unit: the start is tried once more after the boot.

## [0.21.0] - 2026-11-01

### Added
- **`backup: { provider: volumes-age-rclone, volumes: […] }`**: a project without a database backs
  its volumes up, encrypted, on the same schedule, settings and destination as a database's
  backup, and `restore` brings them back (on a machine made again with `--secrets` too): a private
  CA's key, a VPN node's identity or an operator's edits survive a disaster recovery.
- **`vm attach HOSTNAME --interface ROLE=SPEC`** and **`network attach`**: a VM made on TrueNAS
  gains the NIC a newer release names (a gateway's client LAN beside its uplink), named by its MAC
  and addressed from its next boot, without making the VM again.
- **`network set --role ROLE`** and **`network reset --role ROLE`**: a secondary NIC is
  re-addressed on a running VM (DHCP, a static address, or none) with the primary's revert
  window, one change awaiting confirmation at a time across the NICs.

## [0.20.0] - 2026-11-01

### Added
- **`services.<name>.feeds`**: a service feeding another's nftables sets through the app socket
  is recreated when that table loads, as the table's own service is, so it feeds them again; the
  socket's token can stay off the service clients face.
- **`services.<name>.sets`**: a table's named set filled from an input at each start, on Docker
  and the VM alike (an allow-list as an operator setting).
- **`l4.routes[].interfaces`**: a route opens on several roles at once (a resolver on a VPN and a
  client LAN, never the uplink). It replaces `interface`.

### Changed
- **`upgrade --version V` exits non-zero when it refuses V** (the install went back from it, or
  it is not an upgrade), and still 0 when V is installed already; the message says a new
  revision is the way forward.
- **A Docker host warns of a set only the VM's app socket feeds**, which stays empty there.
- **The app socket's sets route takes literal elements only** (addresses, prefixes, ports and
  ranges written as such): a host or service name, which nft would resolve, is refused, and so
  is a write to a set an input fills.
- **Lint warns of a guarded front route to a service on the host's network**: every container of
  the stack reaches that port directly, past the guard.

## [0.19.1] - 2026-10-31

### Fixed
- **`tune --check` no longer reports an interrupt that never fired** (a virtio NIC's config
  interrupt) as one on the services' cores after every boot: it is judged by the CPUs it was
  given, which it takes when it first fires.

## [0.19.0] - 2026-10-31

### Added
- **`services.<name>.update_ready`**: a start-first overlap sends `update_signal` only once the
  new container wrote that file in its `update_socket` directory, so a new container still
  preparing, or failing while it does, never makes the old one hand its listeners over.
- **`services.<name>.update_decline_status`**: a new container exiting with that status before
  the signal declines the overlap, and the start replaces the service stop-first instead.
- **`services.<name>.front_hosts`** (`loopback`, `host-gateway`, `none`): what a service on the
  host's network reaches the front's domains at, for a server that refuses the loopback.
- **`lint` checks the services' `cpus` against the VM's size**, and an upgrade to a release
  asking more cores than the host has is refused before it changes anything (on the VM, before
  its OS is written).

### Fixed
- **At boot, the services' nftables tables load before Docker brings their containers back**
  (the VM and a Docker host after `tune --apply`), instead of a few seconds after.
- **A dependency a host-network service reaches over a shared volume**, or one without a network,
  is no longer warned unreachable at every install.
- **`lint` says the `host.sysctls` namespace warning once**, naming every key and the services on
  the host's network, instead of once per key.
- **A disk's push to a registry that caps a request's size** (ghcr.io, at 4 MiB) shrinks its
  chunks and goes on, instead of losing the upload four times and falling back to one stream.

## [0.18.0] - 2026-10-30

### Added
- **`reset-db --new-baseline` on Docker and the VM**: run by the installer of a later release at
  another schema baseline, it wipes the data and installs that release with the install's
  values, keeping the host, its keys, certificates and generated secrets. On the VM it writes
  the release's OS and its boot does the crossing. Kubernetes had it already.
- **`shapes.kairos.disks: always`** publishes the VM disks with every ship, the commit hook's
  included (default `opt-in`, as before).
- A release's summary (`ship`, `promote`, `verify`) names what each shape can install; for VMs
  it says whether `vm create` has disks to boot, and the exact `ship --disks` that publishes
  them when it does not. The commit hook's started line says whether its ship publishes disks.
- `vm destroy` warns, before asking, when neither the release nor its channel has a disk to
  create the VM again.
- On a VM that fell back to its passive system, `updates` offers the release it runs and
  `upgrade` writes it to the active slot again, replacing the release that failed.

### Changed
- An upgrade across a schema baseline names `reset-db --new-baseline` as the way across. The
  VM's `upgrade` refuses it before writing the OS, instead of a boot that falls back.
- `vm create` booting another release's disk says so, and why, as it fetches it.

### Fixed
- The schema baseline of a release an older (or newer) Stowage built is read again: it was
  unknown whenever that release's spec held a field this Stowage dropped, so `k8s reset-db
  --new-baseline` refused to cross out of it and an upgrade across a baseline went unchecked.
- A VM's first install had two deciders: an operator's `install` run while the first boot
  switched to the release the settings name installed the OS's own release, and the switch's
  reboot then left the VM on its passive slot. That `install` now waits for the switch.
- A disk download cut short (a dropped connection, an HTTP/2 stream reset) or stalled resumes
  from the byte reached instead of failing, or hanging, `vm create`.

## [0.17.0] - 2026-10-29

### Added
- **An upgrade before the first install on the VM**: when a VM's first install was refused (or
  none was made yet), `updates` and `upgrade` read the channel and registry of the VM's settings
  and write a newer release's OS; the boot into it makes the first install of that release. A
  fixed release, a revision of the same version included, no longer needs the VM made again.
- **A gateway stack on a Docker host**: `tune --apply` on an install is also the operator's
  consent to the stack's network declarations — the services' nftables tables, the forwarding
  between `host.interfaces` roles (in `DOCKER-USER`), and each `tun` device's networkd file (a
  NIC's MTU and policy routing stay the operator's). From then on every start as root, and a boot
  unit, apply them as the VM does;
  `tune --revert` removes them. Without consent, each start warns they are not in force, and
  `status` says which are applied.
- **An upgrade adding a front route**: a route the release adds is decided as a first install
  decides it — on a domain the install serves (a `path` route) without asking, a new domain input
  given or asked like any added input, a loopback route's port by the new `upgrade --ports` or the
  release's — rather than refused. On the VM, `upgrade` checks the target release's installer
  before it writes the OS, and refuses there a route or an input its boot could not decide.
- Lint refuses a compose service's `volumes_from`, `links` or `external_links` that is not a list
  of strings, as Compose does.

### Fixed
- **A DNS service on the VM**: an L4 route to a service on the host's network (every route on
  the VM, `host_network` on Docker) is probed where that service binds — the host's addresses
  and the loopback's — not on the wildcard, which a local resolver's stub on
  `127.0.0.53:53` held, so a UDP route on port 53 was refused. A port refused as in use now names
  the addresses holding it.
- An L4 route to a service whose profiles are all off is said to be served by no one, in the
  install's log and `status`, rather than to listen.

## [0.16.0] - 2026-10-27

### Added
- **Services on the host's network, on Docker too**: `services.<name>.host_network` runs a service
  on the host's network on Docker and the VM, as the VM runs an L4 route's service: the stack
  reaches it by its name, and an L4 route to it is its own port, published by no one.
- **A transparent proxy on the VM**: `services.<name>.transparent` names the interfaces a
  service's table intercepts traffic on (TPROXY); the VM's firewall accepts there what reaches
  the service's transparent sockets, and `tune` advises the rule on a Docker host.
- **Forwarding between the host's interfaces**: `host.interfaces.<role>.forward` names the roles
  the host forwards a role's traffic out of, past Docker's FORWARD policy: the VM accepts it and
  its replies in `DOCKER-USER`, the stack's own table judging it; `tune` and `k8s tune` print the
  rules for a host the operator runs.

### Changed
- **A build's name with several addresses**: `images.<key>.add_hosts` gives each name a list of
  addresses (`{origin.test: [127.0.0.1, 127.0.0.2]}`), each passed to the build in its order; a
  single address is no longer accepted.
- **Service tables' names**: lint refuses every word nft 1.1.6 reads as its own as a service's
  table name (`tunnel`, `typeof`, `time` among them), which the VM's nft refused at install.

## [0.15.0] - 2026-10-27

### Added
- **A handoff the old process starts**: `services.<name>.update_signal` names the signal a
  start-first overlap sends the old container once the new one runs, before it waits for the new
  one's health, for a server whose old process hands its listeners over only when told to go.
- **Routes by path**: `front.routes[].path` serves a route under a path prefix of a domain other
  routes share (a console, an identity provider and metrics on one name and one certificate); the
  route's URL carries the path.
- **Build options**: `images.<key>.add_hosts` gives a build's steps names resolved to fixed
  addresses, and `images.<key>.network` runs them on the host's network or none; both enter the
  image's inputs digest.

### Changed
- **Installs cross Stowage versions**: an install of a release an older Stowage built upgrades to
  one this Stowage built (it was refused when the older spec held a key this one moved, such as
  memory shares), and `rollback` starts the previous release with its own installer. When the
  older release's spec is in another format, what only it tells (its schema baseline, the inputs
  it drops, whether it reads a newer schema) is logged as not known and not checked; rollback
  barriers still refuse.
- **Loopback ports only where loopback is offered**: `front.loopback_ports` is required only
  when `tls_modes` offers `none`, and it and `loopback_env` are refused otherwise.
- Lint warns of a service that waits for a one-shot without `restart: true`, which keeps what
  it read of the one-shot's last run when a re-run makes it again.

### Fixed
- A start-first overlap reads a dependency without a healthcheck on a Docker that renders an
  absent health as a missing key.
- The IPv6 allowlist warning names the source that holds the gateway before "holds".
- **The front listens on 80 and 443 in its container** whatever host ports an install gives it,
  and serves its domains on a moved HTTPS port too: a service calling a route by its URL reaches
  the front on every mode and port, `acme` on moved ports included. On the VM, an L4 route's
  service on the host's network reaches the routes' domains too.
- The OS bundle's boot, SSH sessions and `sudo` log no error on a healthy system: the boot no
  longer enables the purged `iscsid`, the initrd's crypt generator exits cleanly without an
  encrypted disk, and the locale file every login reads exists.
- `audit` no longer fails an OS bundle on its kernel: no SBOM lists the kernel by upstream's
  version (which a scanner matched against fixes the distribution backports) or its modules; the
  kernel is scanned through its distribution's packages.
- A service's own cores (`services.<name>.cpus`, `irq: off`) no longer take the NICs'
  interrupts when the kernel spread them onto those cores: `tune --apply` and the VM's boot move
  every NIC interrupt off them, and `tune --check` reports one that reaches them.
- A signing release ships to, and `mirror import`s into, a registry that takes OCI manifests
  alone (zot's default): its scanner image is published with OCI media types, no longer as
  upstream's Docker v2 manifest.
- `vm create` boots from a mirror that carries only some of a release's disk formats (`mirror
  export --disks`): it needs only the format the platform boots, and names that format when the
  mirror lacks it. `audit` and `sbom export` leave out, and log, an OS bundle or a disk the
  registry does not hold, instead of failing on it.

## [0.14.0] - 2026-10-26

### Added
- **Upgrades that overlap**: `services.<name>.update: start-first` starts a service's new
  container beside the old one and stops the old one, with its grace period as the drain, only once
  the new one is healthy; a new container that fails is removed and the old one serves on.
  `update_socket` gives both containers a directory to hand listening sockets over. Lint refuses
  what two containers cannot share (a published port off the host's network, a fixed name, an
  address of its own, several replicas, a disabled healthcheck). The front tries an upstream it
  cannot reach again for 5 s, so a service that restarts costs a request a wait, not an error.
- **SBOMs, signed, exported and audited**: a project that signs gives each image of a release,
  its installer, its OS bundle and each disk an SPDX SBOM, attested with the release key beside
  the signatures (`cosign verify-attestation --type spdxjson` reads it), and carries a pinned
  vulnerability scanner (grype) as the release's `audit` image. `stowage sbom export` and
  `<short>-stow sbom export DIR` write the verified SBOMs as files for the operator's own tools;
  `stowage audit` and `<short>-stow audit` scan them, printing the vulnerabilities by severity,
  `--json`, failing at `--fail-on`, with grype's database fetched or given offline (`--db`). Both
  run on the operator's machine, by version (`--version`, `--registry` for a mirror), never on
  the VM; a mirror carries the SBOMs and the scanner. `audit` is a reserved image key. Stowage's
  own release carries its binary's SBOM, in its signed checksums.
- **An app's own SBOMs in the stack's**: an SBOM document an image ships (CycloneDX or SPDX JSON,
  by convention `/usr/share/sbom/<name>.cdx.json`) is merged into the image's attested SBOM, so
  one audit covers what an app bundles that no scanner sees, such as a front's scripts.
- **What a stack asks of its host** (`host`), each entry with its reason: kernel modules
  (`host.modules`) to load at boot, to block or to give options; Ubuntu packages
  (`host.packages`); kernel arguments (`host.kernel_args`), which the VM's active and passive
  systems boot with, each with its own release's, the recovery system without them; kernel
  settings beyond the tune profile's (`host.sysctls`); and the CPUs' posture (`host.power:
  performance`, their full speed: the performance governor, energy preference and intel_pstate
  floor; `platform`, the default, leaves them). The OS bundle carries them from its first boot,
  `lint` checks them (Kairos's own kernel arguments, the boot's own packages and a profile's
  settings are refused), `tune` reports what is not as asked — on the VM as a difference, on a
  Docker host, whose kernel, boot loader and packages are its operator's, as advice saying how to
  set it there; `--apply` writes the settings and the power posture — and `k8s tune` prints them
  as what every node needs. On the network profile, `tune` advises `iommu=pt` where an IOMMU
  translates every device's DMA.
- **Bare metal** (amd64): the `iso` disk format is an installer, Kairos's live system, and
  `vm create --platform metal --device <disk>` writes it with the machine's seed for two USB
  sticks. The installer wipes the named disk, installs the OS with room for later releases and
  powers the server off; its first boot installs the release, as a VM's, and the seed is never
  pulled again, its stick left in or not. The console and the recovery shell are on the server's
  screen; the installer serves no SSH and runs nothing of the stack. The OS bundle's build checks
  the drivers of servers' common NICs and disk controllers.
- **Firmware a stack's drivers need** (`host.firmware`, by path under `/lib/firmware`, with its
  reason): the OS bundle's build fails without it, `tune` reports it missing on a host (advice on
  a Docker host) and `k8s tune` prints it for the nodes.
- **Interfaces by role** (`host.interfaces`): a stack between networks names its NICs (a client
  side, an upstream side, a management one) and the tun devices its services open, each with its
  MTU, its policy routing in networkd's own terms and, for a NIC, its ethtool settings (channels,
  rings, features one by one, EEE). `vm create --interface` gives each NIC its addressing, MAC and
  network; the VM names each NIC by its role, keeps the default route on the primary, and the
  built-in TrueNAS, libvirt, Proxmox and vSphere modules attach one NIC each. The front and an L4
  route can listen on one interface alone (`interface`), which the VM's firewall enforces. A
  Docker host's `tune` advises the settings, `k8s tune` prints them.
- **What a service asks of the host** (`services.<name>`): whole cores of its own (`cpus`), which
  the NICs' interrupts and receive steering stay off unless `irq: on`; its own nftables table
  (`nftables`), loaded on the VM before the service starts, its named sets fed through the host
  daemon's app socket; an address of its own on a NIC (`interface`, `address_input`), an ipvlan
  network beside the stack's (`install --interface ROLE=NIC` names the NIC on a Docker host).

### Changed
- **Memory shares moved**: `stack.memory.shares.<service>` is now `services.<service>.memory`,
  beside the service's other host settings.
- **`tune` sets each NIC feature on its own**, where the driver lets it be changed: a fixed
  feature no longer makes the driver refuse the others (ENA).
- **A smaller, quieter OS bundle**: the packages kairos-init installs that the appliance does
  without are removed — NFS and its `rpcbind` (which listened on port 111 on every address),
  SNMP, LLDP (which announced the host to its network), fail2ban, Canonical's subscription and
  news tooling, PackageKit, ZFS, iSCSI, RAID and network block devices, haveged, os-prober and X11
  leftovers — and the apt and news timers masked, so only SSH listens beyond loopback, on the
  active system as on the recovery one, and the OS's SBOM lists less to audit. `host.packages`
  keeps one a project needs.
- **The network profile no longer sets the CPUs' governor**: `host.power: performance` does, on
  any profile.
- **A VM's changed user-data is never applied**, as Kairos keeps its first boot's: said, and
  proven across an upgrade.
- **The OS bundle is built on Ubuntu alone**: `lint` warns of another `shapes.kairos.base`, and the
  build refuses it at once, naming the key.
- **A VM's disk is the app's space and Kairos's**: `vm create --disk` and `shapes.kairos.size`'s
  `disk` are the app's space (default 20 GiB, at least 4); `vm create` adds what Kairos takes,
  which each disk records, and says both (`a 43 GiB disk (the OS 23 GiB, the app 20 GiB)`). The
  state partition is sized from the OS bundle rather than auroraboot's default, which left a
  large bundle's VM with next to no persistent space (1.5 GB on a 32 GB disk). Disks published by
  an earlier Stowage are refused: ship a new revision with its disks.

### Security
- **An rclone config is run only when the operator saved it**: on the VM, the host daemon keeps
  the digest of each rclone config the operator saves, and a saved config without it — saved
  before this release, perhaps by an app through its socket under 0.12, or edited on disk — is
  never run: backups, `verify`, `list` and `restore` fail and say why, and `backup status` and the
  app's settings show it as not known to be the operator's, until the operator saves one over SSH.
  An install upgraded to this release saves its rclone config again
  (`backup config --rclone-config FILE`) to keep its backups running.
- **The VM's posture, proven on a real VM**: Docker forwards nothing no rule of its admits, over
  IPv4 and IPv6 (`--ipv6`), and the OEM partition holds the boot's own state alone — the first
  boot's user-data, which no later boot pulls again, GRUB's environment and the seed host key's
  retirement.

## [0.13.0] - 2026-10-25

### Added
- **A front route can ask for a login**: `basic_auth: { user, secret }` on a route makes the
  front ask for that user and the password a spec secret (`gen:`) or a secret input holds, after
  the route's allowlist and client certificate, on Docker and the VM. The upstream never sees
  the login's `Authorization` header. The front keeps the password's bcrypt hash, made again only
  when the password changes; while the password is empty, or longer than the 72 bytes bcrypt
  takes, the route refuses everyone. A login
  page (a form and a session) is planned.
- **Inside the stack, a route's domain names the front**: in a TLS mode each route's domain is
  an alias of the front on the stack's network, so a service calling another route by its public
  name (an identity provider's issuer URL) reaches the front.
- `lint` warns when `shapes.kairos.kairos_init` is not the kairos-init release the OS bundle's
  build takes (today v4.4.0-rc2), before `ship` refuses it minutes into the build.

### Changed
- **On the VM, an L4 route's service runs on the host's network**: no Docker port publishing,
  NAT or bridge sits between its clients and it. The VM's firewall opens the route's port from
  the route's allowlist (on its bind address alone when it has one), and lets the stack's own
  services reach the ports it is reached on inside (the L4 ports, the front's routes to it and
  the ports it exposes). The stack still reaches it by name, at the host's gateway, the provision
  hook and a `command` provider included; it reaches the services it `depends_on` by name too,
  each published on a loopback address of its own (127.0.4.n) on the ports it exposes (its
  `expose:` and its image's). With a
  `kairos` shape, `lint` refuses an L4 route whose port differs from its upstream's (nothing
  maps one to the other on the host's network), and a route's service that declares
  `networks:` or runs more than one container. Docker publishes the routes as before.
- **A plain `upgrade` takes a new revision of the release the install runs**: run by the
  installer the install keeps, it now looks for that version's latest revision on the install's
  channel and upgrades to it, as `upgrade --version` does, so a fix shipped as a revision
  reaches the install. With none, or a channel it cannot read, it says the install already
  runs the release, as before.
- **The VM's health gate judges the stack's services as an install's start does**: a
  healthcheck still starting is reported as starting, and the next poll asks again.
- A front's loopback address (`--address`) is refused in `127.0.4.0/24`, the addresses the VM
  gives the L4 routes' services' dependencies.
- `vm create`'s SSH offers the agent's key of `--ssh-key` alone when its private key is not
  next to it, rather than every key the agent holds (the VM drops a client after 4 failed
  keys).

### Security
- **The rclone config is the operator's alone**: a service holding the install's token can no
  longer save or verify backup settings with an rclone config other than the one the operator
  saved over SSH (nor post that one back): rclone's options name programs it runs (sftp's `ssh`,
  a `*_command`, S3's `credential_process`), and the host daemon runs rclone as root. A config
  a service saved through the app socket before this release is kept and run: if `events`
  shows `changed through the app socket: … rclone_config`, save the config again over SSH
  (`backup config`). The app still moves the destination among the operator's remotes, and sets the
  recipients, retention, schedule and profile, unless the settings are locked. While they are
  locked, the app's `backup verify` tries the saved settings alone.

### Fixed
- **The Docker warning on an allowlist every IPv6 client passes** now judges the stack
  network's own gateway, read once the network exists, rather than Docker's default pools: a
  daemon given other pools is warned, and a list overlapping a pool without holding the gateway
  is not.

## [0.12.0] - 2026-10-24

### Changed
- **The Kairos VM is built on Kairos 4.4** (`kairos_init` v4.4.0-rc2, the disks made by
  AuroraBoot v0.28.0). A project's `kairos_init` must move to that release: the OS bundle's
  build refuses another. An OS upgrade now also refreshes the boot loader (shim, GRUB and its
  configuration) on the EFI partition, which earlier VMs only got by being reinstalled.
- **The examples' VM base is Ubuntu 26.04 LTS**, the release the suites now prove the OS bundle
  on (24.04 before).
- **SSH on the VM is hardened**: modern ciphers, key exchanges and MACs only, no port, agent or X11
  forwarding, at most 4 authentication tries (an SSH agent holding more keys needs `ssh -i KEY -o
  IdentitiesOnly=yes`, and `vm create` the private key next to `--ssh-key`'s `.pub`), and a
  client that stops answering is dropped after 10 minutes.
- **A VM installed with an earlier release upgrades to this one in place** (Ubuntu 24.04 to
  26.04, Kairos 4.4 with it), and a `rollback` or a failed health gate's fallback to the earlier
  release keeps SSH working: each release's SSH hardening comes from its own system, so an older
  OpenSSH never reads settings it does not know.
- `kairos_init` is Kairos 4.4.0's release candidate: a later version moves to the final release.

### Security
- **The VM's app socket no longer serves the operator's routes**: a service `host_api` names,
  holding the install's token, is answered 403 for `provision`, `reset-db`, `secrets export`,
  `secrets rotate` and `restore`, which stay the operator's, over SSH. Its other verbs
  (status, events, updates, upgrade, rollback, network, backup, service actions, reboot and
  shutdown) are served as before, but for removing the backup settings and skipping the backup
  before an upgrade, now the operator's.
- **The backup settings can be locked against the app**: `backup config --lock` keeps a service
  holding the install's token from changing where backups go, whom they are encrypted to, how
  long they are kept or when they run (it still reads them, runs one and verifies them), and
  each backup setting the app changes is journaled as a `backup.config` event.
- **`vm create --secrets` sends the secrets only to the VM it made**: the VM serves a one-time
  SSH host key its cloud-config carries until its install, and `vm create` refuses any other
  (it used to take whatever key answered first). It then keeps the VM's own key and prints its
  fingerprint. Without OpenTofu it prints the one-time key's fingerprint to check before sending
  them.
- **A cloud's VM takes SSH from this machine alone by default**: `vm create` on AWS, Azure or
  Google Cloud without `--ssh-allow` lets SSH in from this machine's public address only (it used
  to accept every source on the VM's public address); `--ssh-allow 0.0.0.0/0,::/0` keeps every
  source. Google Cloud's module makes firewall rules for SSH's sources and for the front's and
  the L4 routes' ports with `--tofu-var firewall=true` (the account must be allowed to create
  firewall rules); without it `vm create` warns which ports the network's own rules must open.
- **`ship` and `promote` print the installer by digest**, with the `cosign verify` that checks it
  before an operator runs it (or say the release is not signed): a first install trusts the
  installer it runs, and a digest is what a tag cannot be moved under.
- **lint warns of a service given the host's powers**: `privileged`, `cap_add`, `pid: host` or the
  Docker socket mounted.
- **Docker: a route's allowlist every IPv6 client passes is warned**: with IPv6 off, Docker hands
  an IPv6 client to the front as its bridge's gateway, so a list holding Docker's private ranges
  admitted them all. The install warns on a host with a global IPv6 address; `--ipv6` keeps each
  client's own address.
- **Docker: each L4 route says where it listens**: `status` and the install's end tell whether a
  route is on one address or on every address, open to whoever reaches the host.

## [0.11.2] - 2026-10-23

### Fixed
- The VM's seed ISO is read by VMware Workstation: its last sector was cut short, which
  Workstation's CD-ROM refuses, so a VM imported from the OVA never read its cloud-config and
  stopped at the recovery system's login. The image now holds its whole volume and 300 KiB of
  zeros after it. `vm create` refuses to replace a seed it finds: move away one an
  earlier version wrote before running it again.

## [0.11.1] - 2026-10-23

### Fixed
- The VM's host daemon sockets (its own and the app socket the stack's services reach) appear only
  once they listen, their owner and mode already set: a command run as the daemon starts is no
  longer refused, and no other process can connect in between. A daemon stopping leaves the
  sockets a restarted one made.

## [0.11.0] - 2026-10-23

### Added
- **Client certificates on a route**: `client_auth: true` makes the front require, on that
  route's domain, a client certificate the install's client CA issued (`--client-ca FILE`, a
  re-run replaces it, `none` removes it). Until one is given, and in loopback mode, the route
  refuses everyone, and the install says so.
- **An L4 route on one host address**: `bind_input` names an advanced input whose address the
  route's port is published on alone, so installs sharing a host can each hold the port on
  their own address; the port is probed on that address alone.

### Fixed
- A one-shot service no other service waits for no longer fails the Docker install when it
  exits: the install is judged by each service's state — running (healthy, when it has a
  healthcheck) or exited 0 — rather than by `docker compose up --wait`'s exit code.

## [0.10.0] - 2026-10-23

### Added
- **Memory follows the host**: `stack.memory.shares` gives a service a percent of the host's RAM
  as its memory limit, on Docker and the VM, recomputed at each start and each VM boot — never
  below the limit its compose file sets itself. The service is told its limit, the host's RAM and
  CPUs (`STOWAGE_MEM_LIMIT_MB`, `STOWAGE_HOST_MEM_MB`, `STOWAGE_HOST_CPUS`) to size its own caches.
- **A memory share takes a maximum**: `{ percent: 25, max: 16G }` gives a service a quarter of
  the host's RAM up to 16 GiB, still never below its compose file's own limit; lint refuses a
  maximum under 64 MiB or under that limit, and says when the percent can never be reached.
- **The VM's size**: `shapes.kairos.size` states the least and the recommended VM. `vm create`
  sizes a VM at the recommendation by default, refuses one under the minimum, warns under the
  recommendation, and prints the size when the VM is made by hand or by a cloud's instance type.
- **Test releases**: `--test-registry`, `--test-key` with `--test-public-key`, and `--shapes`
  make `ship`, `verify`, `releases` and `promote` work on a test release of the project —
  published elsewhere, signed with the run's key, which its installs trust alone, with only the
  shapes asked, with the run's own registry accounts — without editing the spec. The project's
  registry, its signing key and its registry tokens are refused for one.
- **Docker service verbs**: `ps`, `logs SERVICE` (`--follow`), `restart`, `stop` and `start
  SERVICE`, and `exec SERVICE -- COMMAND` (its exit status the run's), as the VM has them.
- **`schema`** on Docker: the database's migration, the one the release kept for `rollback`
  recorded, and the rollback barriers after it (`--after`, `--json`).
- **`uninstall` on Docker**: the containers, networks and host timers go, the volumes and the
  install directory are kept for a later install, or deleted with `--data`.
- **`KEY_ACCEPTED`** beside a rotated secret: the other value its services accept during a
  rotation, so a service reads one variable instead of two.
- **Tuning profiles**: `tune.profile` in `stowage.yaml` picks how the hosts are tuned — `default`
  (also without `tune`) makes Docker's path as fast as the host's network and nothing more,
  `network` adds the tuning of a host serving a heavy network load. The operator may choose
  another with `--tune-profile` (Docker install and re-run, `vm create`, the VM's `install`),
  `release` to follow the release again; `status` shows the profile and whose it is.
- **Containers as on the host**, on every profile: each container takes the host's own network
  settings, the open files the host allows, and the stack's networks the host route's MTU.
- **The network profile goes further**: `tcp_tw_reuse`, a wide range of local ports that keeps
  the stack's own ports out of it, larger neighbour tables, a 65536 floor for the packet
  backlog, a NIC's interrupts spread over the CPUs (unless `irqbalance` runs), its packets
  spread over every CPU when it has fewer queues than CPUs, and the CPUs at full speed.

### Changed
- **The Kairos VM is no longer always tuned for the network**: it applies its stack's profile,
  `default` unless the release or the operator chooses `network`. A VM raised to `network` by a
  re-run is tuned at once; lowered, at its next reboot.
- `tune --profile` takes `default` or `network` (`shared` is gone; a host applied with it reads
  as `default`), and defaults to the stack's profile, or the last `--apply`'s when higher.
  `tune --apply` no longer writes the open-file limit or the MTU into Docker's file: the
  containers get them from the install.
- `ship` and the commit hook refuse a version that `month_guard` puts out of its month, unless
  the channel holds it already (a resumed ship, a later revision); `lint` still only warns.
- `k8s install`, `upgrade` and `reset-db` with `--unsafe-sysctls` refuse at once a namespace
  whose Pod Security label refuses the pods' settings, instead of waiting out the timeout; an
  upgrade to a release whose installer predates the flag says to run it without.

### Fixed
- A first install refuses a UDP port of an L4 route that another program holds, as it does a
  TCP one, instead of failing as the stack starts.
- The VM's host daemon refuses a request during another job as `a job (<kind>) is running`, which
  reads right for every kind of job.

## [0.9.0] - 2026-10-23

### Added
- **`tune`**: the Docker host's network tuning for many connections, UDP and QUIC, and tunnels,
  sized from its RAM and CPUs — every memory ceiling a share of RAM, used under load only. `tune`
  reports what differs (`--check` exits non-zero), `--print` prints the files, `--apply` (root,
  asks first) writes and applies them, `--revert` puts the host back. Docker is never restarted
  unless `--restart-docker`. The install and `upgrade` say in one line when the host differs.
  Every value is at least the kernel's own default. `--profile network` leans harder on the
  network, for a host that runs the stack alone.
- **The Kairos VM tunes its network at each boot** with the `network` profile, for its RAM and
  CPUs (a resized VM is retuned): larger socket buffers and budgets, NIC rings and flow
  steering, the containers' logs rotated; `tune` on the VM reports and `--check` checks. The image
  carries `ethtool` and the gVNIC and Azure MANA drivers.
- **Faster VM NICs**: libvirt and Proxmox give the VM a virtio queue per vCPU (libvirt a larger
  receive ring), Compute Engine a gVNIC (`nic_type`), Azure accelerated networking
  (`accelerated_networking`, on; the default size is now `Standard_B2ls_v2`, which offers it).
- **The containers get the network settings their namespace keeps of its own** (listen queues,
  TCP buffers, latency and MTU probing), sized from the host, except those a service sets itself
  or shares another's network for. A stack's services are recreated once, at their next start.
- **Kubernetes pods get their network settings** with `k8s install --unsafe-sysctls` (and
  `upgrade`): listen queues, latency and MTU probing, given to the chart as `stowage.sysctls`,
  which each node's kubelet must allow. `k8s tune` prints them, the kubelet's setting, and how a
  node is tuned (`tune` on the node). `lint` warns when a chart does not give them to its pods.

## [0.8.0] - 2026-10-22

### Added
- **A project without a front**: leave `front` out and the stack is served by its L4 routes
  alone, on Docker and the VM. Nothing about a front is asked, probed or recorded, the VM opens
  the L4 ports and SSH alone, and the flags only a front takes are refused naming it.
- **`data.volumes.<name>.uid` is applied** on Docker and the VM: a volume the stack makes (its
  first start, or after `reset-db`) is given to that uid before its services start, so a service
  running as a user can write a volume mounted where its image has no directory. A volume that
  exists keeps its owner. The release then carries a small volume helper image, `<name>-chown`.
- **`stowage.requires` is enforced**: every command refuses a Stowage outside the range before
  reading the rest of the spec, naming both versions, and lint refuses a range that does not
  parse and a `stowage.lock` pin outside it.

### Fixed
- On a registry that deletes no tag alone (ghcr), a `ship` that leaves a disk's
  `latest-<format>` with nothing to offer no longer fails after publishing: the tag is retired
  (moved onto a marker that reads as absent), and `verify` passes.
- On such a registry, `prune` of the release the channel pointer is on (or of a tag another
  release shares) no longer stops part way: the shared tags are retired, the pointer moves back,
  and what no tag holds any more is deleted. The installer, a mirror, `verify` and the VM's
  upgrades of this version read a retired tag as absent (a retired channel pointer: the channel
  has no release); an earlier one reports it as not a release.
- A change to an inline compose `configs:` content or to an environment-sourced compose secret
  now recreates the services that mount it: Compose alone kept them running with the old one.

## [0.7.0] - 2026-10-21

### Added
- **L4 routes** (`l4.routes`): host ports published to a service as they are, over TCP or UDP,
  beside the front, in every mode; a route's endpoint for the stack (`STOWAGE_L4_<ROUTE>`) from
  an input holding the address clients dial; `status` lists them, and a chart receives them in
  `stowage.l4`. Front ports and L4 ports never clash: lint and the installer refuse it.
- `front.http_port` and `front.https_port`: a project's own defaults for the TLS front's host
  ports, which `--http-port` and `--https-port` still move.
- **Optional services**: an input's `profile` turns a compose profile on while it holds a value;
  turned off, its services are removed.
- Compose `extends` within the file: a service takes the image of the one it extends.
- **An L4 route's allowlist on the VM** (`allow_input`): the VM's firewall lets only the sources
  an input holds reach the route's port, over IPv4 and IPv6, from boot on; a Docker host's
  firewall stays the operator's.
- **Volumes in backups** (`backup.volumes`): each backup also archives the volumes listed,
  encrypted beside the dump, and a restore brings them back with the database, on Docker and on
  the VM. A restore that cannot bring every volume back keeps the stack down, and refuses the
  verbs that would start or change it, until a restore again finishes; `status` says so.
- `vm create` names the ports the VM opens to each L4 route and the host devices the stack maps.
- **Resumable disk pushes**: a disk goes up in chunks and, after a dropped connection, goes on
  from the last byte the registry acknowledged; a push with no byte acknowledged for 5 minutes
  fails naming the disk, and a running push logs its progress and rate every 30 s.

### Changed
- A service that publishes a port of its own past this host (not on a loopback address) is an
  error with a `kairos` shape (the VM's
  firewall opens only what Stowage publishes) and a warning on `docker`: declare it in
  `l4.routes`.
- An input named by a route's `domain_input` and also declared under `inputs` is no longer
  warned as read by nothing.
- A service on the host's network (`network_mode: host`) is an error with a `kairos` shape and a
  warning on `docker`: publish its ports with `l4.routes`.

### Fixed
- `upgrade --version` and `k8s upgrade --version` to a release a newer Stowage built: the fetched
  installer is checked by its release alone, so a spec key the running installer does not know no
  longer refuses the upgrade. From this release on; an installer of 0.6.0 or earlier still refuses
  one: run the new release's installer.

## [0.6.0] - 2026-10-21

### Added
- **Disks only when asked, one disk set per channel.** `ship --disks` (or `STOWAGE_SHIP_DISKS=1`,
  or `hook install --disks`) publishes the VM disks and registers the cloud images; a release
  shipped without it publishes none and records the disk set its VMs boot, the channel's newest.
  `vm create` of such a release boots that set, and the VM's first boot takes the release's own
  OS bundle and reboots into it before it installs. A ship that publishes disks removes the
  channel's older disk sets once the channel points at it; a cleanup that fails after the release
  is out exits 3, the release published.
- `registry.delete`: `github` deletes manifests through GitHub's packages API (ghcr deletes
  nothing through the registry API); a registry that deletes no tag alone has a manifest deleted
  by digest when no other tag holds it.
- **Backups, `restore` and the disk-pressure watch on Docker**: `backup config|run|status|list|verify`
  and `restore OBJECT`, as on the VM, the schedule and the watch as systemd timers of the install;
  `status` shows the disk pressure.
- **`k8s rollback` checks the database's schema** (`shapes.kubernetes.schema`): a rollback barrier
  since the schema `k8s upgrade` recorded, or a release that does not read the newer schema, is
  refused before helm is called.
- `mirror export --version V [--revision N]` exports an earlier release of the channel;
  `--shapes` leaves out what the site does not run (the OS bundle and the disks, the chart).
- A byo certificate: `status` (Docker and the VM) shows its end and warns from 30 days before
  it, the VM's journal says so daily (`cert.expiring`); without `--ca`, a chain lacking its
  intermediate is refused.

### Changed
- An upgrade (Docker, the VM, Kubernetes) drops the recorded value of an input the release no
  longer declares, and a value its provision hook no longer declares it returns, saying so; a
  rollback takes them back.
- A ship, yank or prune that publishes no disk never moves, rewrites or deletes a disk tag:
  `latest-<format>` follows the channel's disk set, not its pointer.
- A project with the Docker shape and a `pg-age-rclone` backup publishes the backup runner.
- Built with Go 1.27.3 (crypto/x509 fix GO-2026-6809).

## [0.5.0] - 2026-10-20

### Added
- **`k8s uninstall`** removes a Kubernetes release and what the installer made beside it (its
  Secrets, the provision hook's account and Job), and prints what it kept. The release's
  PersistentVolumeClaims go only with `--data` or when answered; data kept keeps the Secrets it
  needs, so `k8s install` takes it back. It removes a release an older installer made, of any
  project name, so moving a release to another registry or name is an uninstall then an install.
- **`vm create --create opentofu` on every canonical platform**: built-in modules for libvirt,
  Proxmox VE, vSphere, AWS, Google Cloud and Azure beside TrueNAS's, marked as previews until they
  have run on their platform (by hand stays the default there). Any other platform takes your own
  module (`--module DIR`) holding the module contract README describes; a module's own settings
  are `--tofu-var NAME=VALUE`, asked when missing. A cloud VM is waited for at the address its
  module returns, and sized by its instance type (`--cpus` and `--memory` are for hypervisors).
- **Cloud images registered**: `ship` uploads each `cloud_publish` disk to the publisher's AWS,
  Azure or Google Cloud account and registers it as an image (an AMI, an Azure image, a Compute
  Engine image), with the cloud's settings and credentials from the environment, once per disk. A
  cloud without settings is skipped and named. Needs OpenTofu (`tofu`) then. Run for real on
  Google Cloud; AWS and Azure have not run yet.
- `ship` with `registry.pull: none` stops when a repository it writes refuses an anonymous read
  (a registry that creates repositories private, as ghcr does), naming them: before it builds,
  after the images' push, and before a channel points at the release.

### Changed
- The disk images no longer depend on the registry's visibility or on `registry.pull`: the disk
  builder reads the OS bundle through a read-only loopback proxy that holds the publisher's
  account, and no account enters its container. `registry.pull` may now be the default Docker
  login with `disk_formats`.
- **Disk images are published compressed** (zstd), every format: a release's disks take about
  half the registry space and upload time (a raw or fixed VHD disk a third). `oras pull` now
  writes `<name>-<version>.<format>.zst`: `zstd -d` it. `vm create` and the cloud upload
  decompress them as they download them and check the disk's own digest; a disk published before
  is still read. The first `ship` after the upgrade makes the disks again.
- `vm create`'s OpenTofu modules run on their providers' latest releases, each with its
  providers' checksums: libvirt's on the new 0.9 provider (libvirt's own UEFI firmware, so its
  `firmware` setting is gone; a raw disk works too), vSphere's on `vmware/vsphere` (an OVA
  deployment no longer fails after the VM is made), Proxmox VE's uploading the disk through the
  API alone (its datastore needs *Import* content; `import_datastore` names it). A module of your
  own may declare `image_format` to be told the disk's format.

## [0.4.1] - 2026-10-19

### Changed
- **BREAKING for Go importers — the module moved to `github.com/norvexlabs/stowage`**, and the
  repository with it; release binaries and their attestations are published from there. The CLI
  and every spec are unchanged.

## [0.4.0] - 2026-10-18

### Added
- **IPv6, opt-in**: `--ipv6` turns it on, and it then works as IPv4 does.
  - On the VM (`vm create --ipv6`, `network set --ipv6`): router advertisements and DHCPv6 address
    it — the DHCPv6 client starts at once, as the clouds give the address that way — and a static
    `--address6` / `--gateway6` may sit beside them, checked like the IPv4 pair, carried by the
    same confirmation window, and undone when another host already has the address.
  - On the stack (`install --ipv6`, which `vm create --ipv6` sets too; Docker 27 or later): IPv6 on
    its network, so the front sees an IPv6 client's own address — its source allowlists and
    forward auth judge that client, not the Docker bridge. Turning it on or off on a re-run
    restarts the stack on a new network, volumes kept.
- `network` shows the VM's IPv6 addresses and default gateways, and the console its IPv6 address.
- Loopback mode on `::1` (`--address ::1`).
- A forward-auth address, a registry prefix and a mirror may be IPv6 literals (`[fd00::1]:9091`).

### Changed
- **BREAKING — without `--ipv6` the VM has no IPv6**: no link-local address, router advertisement
  or DHCPv6. A VM made before keeps its cloud-config's network, and a `network set` override it
  holds keeps IPv6 on, as it ran; the next `network set` decides. An IPv6 DNS
  server needs `--ipv6`.
- `vm create` checks a static network as `network set` does: an IPv6 `--address`, a network or
  broadcast address, or a gateway outside the subnet is refused before anything is written.
- DNS servers given to `vm create` replace those of the router advertisements and DHCP, as
  `network set`'s already did.
- The VM's console banner prints the override as `network` does.
- The host API's `GET network` answers the network `vm create` gave as `install`, DHCP included,
  so an app editing the network starts from what the install set; `source` reads `install` for a
  DHCP install too. `network` prints that network.
- A loopback install on 127.0.0.1 refuses a port another service holds on `::1`, which an IPv6
  host tries first for `localhost`.
- The backup runner runs on the host's network: it reaches an IPv6 destination and the VM's
  IPv6 DNS servers.
- The health gate and the disk-pressure POST reach a service by its IPv6 address when it has no
  IPv4 one.
- `vm create` brackets an IPv6 host in its `scp` hint, takes `--host [2001:db8::7]`, and checks
  `--ssh-allow` against every address a host name resolves to, the static IPv6 address included.

### Fixed
- Signatures verify on a registry without the referrers API, such as the GitHub Container Registry:
  `ship` failed there with "signed …, and the signature does not verify", and an install of a
  release published there would have refused it, when the signing client listed the signature
  under its config's type rather than its own.

## [0.3.0] - 2026-10-18

### Changed
- **BREAKING — the installer is `<short_name>-stow`**, no longer `<name>ctl`: the binary an install
  keeps, the VM's `/usr/sbin` command and its units, the bootstrap script's payload and every
  message name it. `short_name` is an optional spec key (absent is `name`), checked by `lint`, and
  fixed for a project's life: `ship` refuses a release whose `short_name` differs from one its
  channels already hold, and an install refuses one before anything changes.
- **BREAKING — release manifest `schemaVersion` 2**, which records `short_name`. A manifest of
  schema 1 is refused, so a release repository holding releases published by an earlier Stowage
  is started afresh (a new prefix, or its tags deleted) before this one ships to it. No
  compatibility with an install a `<name>ctl` made: reinstall it.

## [0.2.0] - 2026-10-17

### Added
- **Schema baselines**: the `sql` schema-compat provider's `baseline` is the schema epoch a
  release starts from (absent is 1). On Docker an upgrade to a release at another baseline is
  refused before anything changes; a rollback (and the VM's health-gate fallback) to one is
  refused before any image is pulled. On the VM an upgrade is refused at the boot after the OS
  write, and the health gate falls back. On Kubernetes `k8s upgrade` refuses a release at another
  baseline (read from the installer of the release the cluster runs), and
  `k8s reset-db --new-baseline`, run by the new release's installer, wipes the release and
  installs the new one: the way across.
- A logo, and a "packed with Stowage" badge a project shipped with Stowage can put in its
  README. The README opens with a recording of the installer and a diagram of the lifecycle.

## [0.1.0] - 2026-10-16

The first release. Stowage packages a small container stack described once in its spec,
publishes it signed to an OCI registry, and installs, upgrades, rolls back and recovers it on a
Docker host, a Kubernetes cluster or a Kairos VM appliance. The README documents every command,
key and contract. The release's checksums are signed keyless with cosign: `cosign verify-blob`
checks them.

### Added
- **The publisher, `stowage`**: `lint`, `project-version`, `changelog`, `plan`, `lock`,
  `hook install` / `hook post-commit`, `ship`, `verify`, `promote`, `yank`, `prune`, `releases`.
  `ship` builds only the images whose inputs changed and re-tags the others, and publishes the
  front, the Helm chart, the Kairos OS bundle and its disk images, each cloud's importable image
  (`cloud_publish`), the installer image and the release manifest.
- **Versions and channels**: the `calver` (with its month guard), `semver`, `regex` and `command`
  version schemes; version sources and changelog providers; channel tags and release revisions.
- **Signing**: cosign keys (file, `env://`, KMS, Vault) sign every image and release manifest;
  `stowage verify` and every install check signatures in-process against the release's keys.
- **Registry access**: a publisher token and a read-only pull account; sites without internet
  through `mirror export` / `mirror import` and a mirror's own CA.
- **The Docker installer, `<name>ctl`**: a terminal form, or flags, environment and an
  `--answers` file; a front in loopback or TLS (`acme`, `dns-cloudflare`, `byo`, `internal`) with
  forward authentication, source allowlists and a published CA; re-runs that change the inputs,
  the TLS mode and the ports; `status`, `provision` (a hook given only the keys it declares,
  `--set`), `script`, `upgrade` (`--version`), `rollback` refused at a rollback barrier,
  `reset-db`, `secrets rotate`, `secrets export` and a disaster recovery (`--secrets`),
  `--dry-run`, `--offline`.
- **The Kubernetes installer, `<name>ctl k8s`**: `install`, `upgrade` (`--version`), `rollback`,
  `status`, `reset-db`, `provision` (the hook as a Job, what it returns kept in a Secret),
  `secrets rotate`.
- **The Kairos VM appliance**: `<name>ctl vm create` for each platform, or on TrueNAS with
  OpenTofu, and `vm destroy`; a first boot that installs from the cloud-config; a host daemon and
  its verbs (`status`, `health`, `events`, `logs`, `upgrade`, `rollback`, `reset-db`, `network`
  with its auto-revert, `backup` encrypted to age keys and uploaded with rclone, `restore`,
  `secrets`, `reboot`, `shutdown`); a health gate that falls back to the passive system; a
  disk-pressure watch, a default-deny firewall, the time zone, and an app socket for the
  services that ask for it.
- **Project shapes**: a project without a database, extensions an install can be pointed at,
  and three working examples (`minimal`, `full`, `stateless`).

### Known limitations
- The cloud images are published as disks: uploading and registering them in a cloud account is
  not done yet.
- The binaries are built for linux/amd64 only.
- The Docker shape has no scheduled backup or restore (the VM has both); Kubernetes checks no
  rollback barrier before `rollback`.
