# Changelog

Stowage's own releases. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [SemVer](https://semver.org/).

## [0.17.0]

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
