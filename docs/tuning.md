# Network tuning (`<short>-stow tune`)

[README](../README.md) · [Documentation index](../README.md#documentation)

The host's kernel, Docker engine and containers, tuned for the stack's network without holding
memory: every memory ceiling is a share of the host's RAM, which the kernel uses under load only,
and never below the kernel's own default. Two profiles, each a list of rules:

- **`default`** makes Docker's own path as fast as the host's network, and nothing more: each
  container runs as it would on the host (below), and the host gets what that path goes through
  — the NAT's connection table, the queue each packet a bridge forwards waits in, forwarded UDP
  kept batched. It is the profile of a project without `tune`, or with `tune.profile: default`.
- **`network`** is `default` and everything a host serving a heavy network load needs: larger
  socket ceilings, BBR and `fq`, the ports and neighbours of many connections, NIC rings,
  interrupts and flow steering, TUN devices' queues, the containers' logs rotated. The CPUs'
  speed is not a profile's: `host.power` (below). The project picks it with `tune.profile: network`.

The project's author picks the profile in `stowage.yaml` (`tune.profile`); the operator may
choose another for a host whose load differs: `--tune-profile default|network` on the Docker
install or a re-run, `vm create --tune-profile` and `install --tune-profile` on the VM. It is
recorded in `STOWAGE_TUNE_PROFILE` (absent: the release's); `--tune-profile release` removes the
record, a re-run without the flag keeps it, and a run that changes the stack's profile logs
`tuning: the network profile, the operator's (was default)`. `status` prints it with whose it is:
`tune      network (operator)` (the VM's `tune       default (release)`). On a Docker host the
OS is the operator's, so the host's own settings change only when they ask (`tune --apply`);
the install and `upgrade` end with one line, `tuning   N network settings of this host differ
from what the <profile> profile calls for …`, when the host differs. A Kairos VM is Stowage's:
it tunes itself at each boot (below). On Kubernetes `tune.profile` does not apply: a node is
tuned by the same `tune` run on it, and the pods get the network profile's namespace settings
with `k8s install --unsafe-sysctls` (see the installer on Kubernetes); lint warns a project
with `tune` and neither a `docker` nor a `kairos` shape.

- **Each container**, at each start (the install, a re-run, `upgrade`, `rollback`, the VM's
  boot), in `stowage-content.yaml`, on every profile, unless Docker Desktop runs it (its own VM)
  or the engine does not answer (the start then fails):
  - its network namespace's own settings — `net.core.somaxconn`, `net.ipv4.ip_local_port_range`,
    `tcp_max_syn_backlog`, `tcp_mtu_probing`, `tcp_notsent_lowat`, `tcp_rmem`,
    `tcp_slow_start_after_idle`, `tcp_tw_reuse`, `tcp_wmem`, most of which a new namespace takes at
    the kernel's default rather than the host's — at the host's value; on the `network` profile
    at the profile's value where the host holds less (field by field, a ceiling the host holds
    higher kept), so the containers are tuned before the host is — the range of local ports
    included, so a service listening at 10000 or above in its own namespace may see an outgoing
    connection of that namespace take its port while it restarts. A service's own `sysctls`
    key wins; a service on the host's namespace or another service's (`network_mode`) gets none;
    a key the host's kernel lacks is left out (its start would fail); none on a kernel before
    4.15 or under a user namespace (rootless Docker, `userns-remap`), whose namespace hides keys.
  - its open files, `ulimits.nofile`: the hard limit the host's `fs.nr_open`, at most 1048576
    and at most the hard limit the host's own root `dockerd` and `containerd` run with
    (`/proc/<pid>/limits`; one in a container of its own, kind or dind, whose `NSpid` names two
    pids, is not the host's: a container above theirs fails to start where they may not raise
    it, as in an unprivileged LXC container), the soft limit at most 65536 (a program that needs
    more raises it to the hard limit itself; one that walks every descriptor stays fast); none
    when neither process is seen, on rootless Docker or under `userns-remap`, where the engine's
    own limit bounds it. A service's own `ulimits.nofile` wins.
  - each network the stack makes as a bridge (`default` and the compose file's own, not an
    external one nor one setting `com.docker.network.driver.mtu` itself) gets the MTU of the
    host's default route as that driver option, 1500 included (so the daemon's own
    `default-network-opts` never stands in for it); a start that finds one of them made with
    another MTU (its driver option, none being 1500) stops the stack first (`docker compose down
    --remove-orphans`, volumes kept; a network's MTU is set when it is made), logs it, and starts
    it on new ones. A host whose default route does not read at that moment keeps the MTU the last
    start gave the networks.
- `tune` reports each item, its value here and the one wanted, `ok`, `differs`, `advice` (a
  setting outside the OS: never applied, never counted) or `n/a`, after the host's RAM, CPUs and
  profile; it needs no root, and no engine but for the daemon's file, which it leaves out (and
  says so) when `docker info` does not answer. Its profile is `--profile`, else on a Docker
  host the higher of the stack's and the one the last `--apply` recorded in `tune.json` (the host
  being shared by every stack on it), on the VM the stack's (an unknown name is refused). The
  stack's choice and its ports are read from the install's record, which is root's (0600): a
  report run as a user that cannot read it says so (`note: the install's record does not read
  …`) and takes the release's profile (unless `--profile` names one) and no port. A host
  applied with `network` refuses `--profile default` — report, `--check`, `--print` or `--apply`
  — until `--revert`: the network profile's own keys would be neither judged, kept nor put back
  (a `tune.json` written before the profiles, naming `shared`, is read as `default`). `--check`
  exits non-zero when one differs (monitoring, CI). It also reports each of the release's
  `host.modules` (`module <name>`, its reason beside what is wanted): one to load and
  not loaded, one blocked and loaded, and each option the loaded module does not hold
  (`/sys/module/<name>/parameters`; a boolean shown `Y`/`N` holds `1`, `y`, `t`, `on` or `0`,
  `n`, `f`, `off`; one not loaded or not shown there is `n/a`). On the VM, whose image carries
  them, they `differ`; on another host, whose kernel is its operator's, they are `advice`:
  `tune` sets no module there. Each of its `host.kernel_args` (`kernel <name>[=<value>]`) is
  judged against `/proc/cmdline`, the last value given for its name: one missing or another
  `differs` on the VM and is `advice` elsewhere, naming the boot loader's command line
  (`GRUB_CMDLINE_LINUX` in `/etc/default/grub`, then `update-grub`), from the next boot; `tune`
  writes no boot loader. On the `network` profile, an IOMMU that translates devices' DMA (an
  `/sys/kernel/iommu_groups/*/type` of `DMA` or `DMA-FQ`) is `advice` naming `iommu=pt`
  (`iommu.passthrough=1` on arm64) for `host.kernel_args`, unless the command line or the project
  gives that argument: several percent of CPU under a throughput load, and a device the host owns
  is then not confined. Its `host.sysctls` are settings like the profile's (`sysctl <key>`, its
  reason as the why): judged exactly, written by `--apply` into `60-stowage-net.conf` after the
  profile's and applied, put back by `--revert`, written by the VM's boot. On the VM, a project of more than one
  NIC role (`host.interfaces`, `kind: nic`, an optional one included) adds two such settings,
  `net.ipv4.conf.all.arp_ignore = 1` and `net.ipv4.conf.all.arp_announce = 2` (each NIC answers ARP
  for its own addresses alone, so a client on an L2 two roles share caches each address with its
  own NIC's MAC; the kernel takes the larger of `all`'s and a NIC's value, so they cover every
  NIC, and a NIC's own larger value holds), each unless `host.sysctls` sets it on `all` itself; a
  NIC's or `default`'s value below `all`'s (the project's own, else 1 and 2) is lint's error with a
  `kairos` shape, naming the `all` key to set instead (`… is below the arp_ignore 1 all holds on a
  VM of several NIC roles, and the kernel takes the larger: set net.ipv4.conf.all.arp_ignore to 0
  to choose it for every NIC`). Under `host.power:
  performance`, each CPU's `scaling_governor` is judged and set `performance` where its cpufreq
  driver offers it (one that does not keeps its own), its `energy_performance_preference`
  `performance` where exposed and offered, `intel_pstate/min_perf_pct` 100 where intel_pstate
  runs (`cpu governor`, `cpu energy_performance_preference`, `cpu intel_pstate min_perf_pct`); a
  host without cpufreq, as most VMs (the hypervisor drives it), is `n/a`; `power-profiles-daemon`
  or `tuned` running is `advice` (`systemctl disable --now <name>`: each re-asserts its own
  profile), and `--apply` says so and leaves it running. Set on the running host by `--apply`
  and at each VM boot; `--revert` leaves them until the next boot. Under `platform` (the
  default) none of it is judged or set. And each of its `host.packages` (`package <name>`), read from
  dpkg's database (`/var/lib/dpkg/status`): one not installed `differs` on the VM and is `advice`
  elsewhere, naming `apt-get install <name>` (`tune` installs nothing); on a host without dpkg it
  is `n/a`, the package to be found under its own name in the host's package manager. And each of
  its `host.firmware` (`firmware <path>`): a regular file under `/lib/firmware`, as it is or
  compressed (`.zst`, `.xz`); one missing `differs` on the VM and is `advice` elsewhere, naming the
  distribution's firmware package (`linux-firmware` on Debian and Ubuntu; `tune` installs
  nothing). And each of its `host.interfaces` (`interface <role>`): on the VM, a NIC named by its
  role, of its `mtu` (`differs` when absent or of another MTU), a `tun` device `n/a` until its
  service opens it, and each of a NIC's `ethtool` settings — each feature (`interface <role>
  <feature>`; one the driver fixes `n/a`), its channels (`interface <role> channels`, `max` the
  device's maximum) and its rings (`interface <role> rings`) — which the VM's boot gives it (each
  feature on its own where the driver lets it, the channels with `ethtool -L`, the rings with
  `ethtool -G` on a link not yet up, EEE with `ethtool --set-eee`; a NIC whose rings the project
  gives is left out of the profile's ring sizing; an `ethtool -L` an Intel E810 refuses while its
  RDMA driver holds the port is noted with what frees it: `host.modules { irdma: { block: true } }`);
  on a Docker host, `advice` naming networkd's settings and the ethtool commands that give a NIC
  of this host the role's, and, for a role that forwards (`forward`) or that a service intercepts
  on (`services.<name>.transparent`), a second `advice` (`the project forwards or intercepts
  traffic on <role>, which this host's firewall drops:`) naming the rules the VM's firewall has:
  `iptables -I DOCKER-USER -i <role> -o <out> -j ACCEPT` and the replies' `-i <out> -o <role> -m
  conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT`, for `iptables` and `ip6tables`, and nft's
  `iifname "<role>" meta l4proto { tcp, udp } socket transparent 1 accept` in an input chain that
  drops. And under `services.<name>.cpus` (`irq: off`), on any profile, the NICs'
  interrupts (and, with the network profile, RPS) steer off those cores (an interrupt that reaches one is `differs`), and a
  running `irqbalance` is `advice` (`IRQBALANCE_BANNED_CPULIST=…`).
  And on a Docker host each service's nftables table is `advice` (`nftables table inet <name>`),
  which `--print` prints after the files (consented and applied by a start as root,
  through roles that each have a device here, a role's forwarding is reported `ok`, `its
  forwarding out of <roles> applied by each start (tune --apply)`, its rules no longer advised) — once the operator consented, `ok`, `loaded by each
  start (tune --apply)`, when it is loaded, else `differs`, `not loaded` (`loaded by the next start
  as root`); with the consent revoked and the table still loaded, `differs`, `still loaded, the
  consent withdrawn` (`<short>-stow tune --revert` again removes it). Loaded means listed in
  `.stowage-nftables` and, as root, held by the kernel (`nft list table`: a boot drops it until
  the boot's tables unit or the next start loads it); `status`'s `tables` are the same. `--print` prints the files `--apply` writes, for a host configured by other
  means. `--apply` (root) prints what it writes, asks (`--yes` without a terminal, else it
  refuses), writes the files and applies them to the running host; `--revert` (root, asks too)
  removes them and puts back what the first `--apply` found.
- **A Docker host's gateway** (`--apply` with `--dir` on an install whose release declares a
  service's `nftables`, a role's `forward`, a `tun` role, or an L4 route's `allow_input`): the same `--apply` is the operator's
  consent to the stack's network declarations, said before it asks (`and the stack's network
  declarations, …: applied by the install's start now, by every start and at every boot (a unit),
  until --revert`). It records the consent in `<dir>/.stowage-gateway`, enables a boot unit,
  `/etc/systemd/system/<name>-gateway-<8 hex>.service` (the 8 hex digits are the first of the
  sha256 of the install directory's real path (absolute, its links resolved) when `--apply` first ran, recorded in
  `.stowage-gateway` with that directory — 0644: nothing in it is secret, and a start not as root
  reads it; the unit is enabled before the consent is recorded, and when systemd does not enable
  it the record and the unit stay as they were, a unit this run wrote removed again; after Docker and the network: `<dir>/<short>-stow --dir <dir>
  --no-input --no-provision`, the install's start, since the kernel keeps no table or chain across
  a reboot), and its tables' unit, `/etc/systemd/system/<name>-gateway-<8 hex>-tables.service`
  (before Docker, once the network is up: `<dir>/<short>-stow tables-boot --dir <dir>`, the
  services' tables the last start loaded — see "A service's nftables table" ([host](host.md)) under the installer —
  and the L4 routes' source allowlists),
  both enabled together (`systemctl enable`), and runs that start now. Each start as root writes
  the two units again when either differs from what this release writes (a consent an earlier
  release recorded gets the tables' unit so), and enables them then; one it cannot write is
  warned (`warning: the boot units of … are not written (…): `<short>-stow tune --apply` writes
  them again`). From then on each start as root applies, as the VM's does:
  the services' tables (loaded whole, recorded in `<dir>/.stowage-nftables`, deleted once a release
  drops them; the services whose table a start loads are recreated, and feed its sets again); the
  forwarding between roles, the chain `STOWAGE-FWD-<8 hex>` (the VM's `STOWAGE-FORWARD`), which
  `DOCKER-USER` jumps to, recorded under `/run/<name>-gateway-<8 hex>-forward.rules`, each role
  by its device on this host — a `tun` role's `device`, else its own name, a NIC role's the NIC `--interface
  ROLE=NIC` named (recorded in `.env`; a re-run sets it; a recorded name that is not an
  interface's, a wildcard `+` included, names none), as a NIC keeps its own name on a Docker
  host; a pair through a NIC role with no NIC named is not applied (`warning: host.interfaces.<role>:
  no NIC of this host plays it (`--interface <role>=<NIC>` on a re-run names one): the forwarding
  through it is not applied`, and `status` adds `no forwarding through <roles> (no NIC named:
  --interface)`); the L4 routes' source allowlists and the published ports' interfaces, the chain `STOWAGE-ALW-<8 hex>`,
  which `DOCKER-USER` jumps to (`iptables`), replaced whole at each start (`iptables-restore`, one
  transaction: a chain flushed or edited meanwhile holds again) and recorded under
  `/run/<name>-gateway-<8 hex>-allow.rules`: for each route with an `allow_input`, a connection
  whose original destination is the route's published port (`-m conntrack --ctstate DNAT
  --ctorigdstport <port>`, its protocol's) — and its bind address when it has one (`--ctorigdst
  <address>`, so another install's port on another address is not judged by its list) — returns
  to `DOCKER-USER` from each IPv4 source of the input, and is dropped from any other, this host's
  own containers on another network dialling the host's address included (list their network to
  let them in); an empty list drops every source. Such a route is published on IPv4 alone while
  the consent stands (`host_ip: 0.0.0.0` unless its `bind_input` gives an address), whatever
  `--ipv6`: Docker's userland proxy may carry IPv6 clients past `DOCKER-USER`; an IPv6 source of
  the list holds on the VM alone, and a route bound to an IPv6 address is refused: as its bind
  input is given while the consent stands (`<BIND>: an IPv6 address, and this Docker host holds
  the route's allowlist for IPv4 alone (tune --apply): an IPv4 address, or none`), by `tune
  --apply` while one is recorded, before it consents (`l4 route <name>: <BIND>=<address> is an
  IPv6 address, and a Docker host holds <KEY> for IPv4 alone: an IPv4 address in <BIND>, or none
  (a re-run sets it), then tune --apply again`; a record that does not read is refused there too:
  `the install's record: …: fix it, then tune --apply again`), and by a start. A start whose list does not read, or refused, drops every
  source of the routes' ports on every IPv4 address, whatever they are published on now (another
  install's route of such a port on another address included: closed rather than open; a
  publication on IPv6 from before the consent stays until a start succeeds) and fails (`l4 route <name>: <KEY>: …`),
  nothing saying the list holds; at boot, before Docker, a missing `DOCKER-USER` is made
  (`iptables -N`), which Docker keeps with its rules as it starts; a start after which iptables'
  `FORWARD` does not jump to `DOCKER-USER` (the engine's `iptables` off, or its nftables
  backend, publishes the ports past it) fails (`the L4 routes' source allowlists cannot hold:
  iptables' FORWARD does not jump to DOCKER-USER, …: turn its iptables on, or `<short>-stow tune
  --revert``, and `the forwarding between the host's interfaces cannot hold: …` for a release
  that forwards). The same chain holds a published port to its `host.interfaces` roles (the
  front's two, `front.interfaces`, outside loopback mode; an L4 route's `interfaces`, the front's
  port for one it terminates TLS on): a connection to it returns to `DOCKER-USER` arriving on a
  role's NIC (`--interface ROLE=NIC`, as the forwarding names it), crossed with the route's
  allowlist when it has one, and is dropped from any other (another network's container dialling
  the host's address included) — of the connections Docker
  forwards to a container bridge (`-o br-+`, `-o docker0`), so another translation to the port is
  not judged. A port held so needs its service on a bridge Docker makes and names itself, which
  it translates the port to: a network of the `bridge` driver with no `driver_opts` but the neutral ones
  (`com.docker.network.driver.mtu`, `com.docker.network.bridge.enable_icc`,
  `….enable_ip_masquerade`, `….host_binding_ipv4`, `com.docker.network.container_iface_prefix`,
  `com.docker.network.enable_ipv4`, `….enable_ipv6`); another driver, and any other option — a bridge it is told to
  use, a routed gateway, trusted host interfaces, one Stowage does not know — let clients reach the
  container past the holds. Lint refuses, for the front (`front.interfaces`, or an L4 route it
  terminates TLS on with `interfaces`: the project's default network) and each interfaced route's
  upstream Docker publishes (not on the host's network, as every L4 upstream is on the VM), such a
  compose network (`networks.<key> is of the <driver> driver` or `sets <option> <value>, and
  <service> is on it while <key> holds its ports to interfaces: …; leave its driver and driver_opts
  to Docker (the neutral ones aside)`), a `network_mode` (`services.<name>.network_mode <mode>: …: leave
  network_mode out`) and an address of its own on a NIC (`services.<name>.interface: <name> has an
  address of its own on <role>, past what <key> holds: leave one of them out`); and a start, on the
  VM and on a Docker host where the holds apply, refuses the same before it stops or starts
  anything, an external network as `docker network inspect` gives its driver and options (`… make
  the external network <name> again with Docker's own bridge (`docker network create <name>`, no
  -d and no -o but an MTU)`; output that does not read is refused, naming the network). In an
  upgrade that refusal comes with the new release's files in place, as a missing external network
  does, and the upgrade's failure says how to go on. A host daemon's own defaults
  (`default-network-opts`) are its operator's. Values that do not read (a boot before the start)
  hold the front's ports the spec gives, its own unless the install moved them; and
  a role no NIC is named for adds none (closed), warned (`warning:
  host.interfaces.<role>: no NIC of this host plays it (…): the ports held to it answer no client
  through it`), and `status` adds `no client through <roles> to the ports held to it (no NIC
  named: --interface)`. Such a route is published on IPv4 alone while the consent
  stands, as an allowlisted one is; the front's ports too while the stack's IPv6 is off, and with
  `--ipv6` the hold is ip6tables's too (Docker translates the front's IPv6 clients then), a start
  failing when ip6tables' `FORWARD` does not jump to `DOCKER-USER` (`… cannot hold for IPv6: …:
  turn its ip6tables on, or `<short>-stow tune --revert``); a hold that was IPv6's and is no more
  leaves that chain empty. `status` says `interfaces of <front, routes> held`, or `… not held (…)`,
  and `tune --revert` notes that they answer on every interface, published as the consent
  published them, until the next start. At boot the tables' unit (below) applies
  them from the install's recorded values before Docker restores the containers — a record that
  does not read drops every source of those ports — so no route is open a moment past its list;
  a route to a service on the host's network is not translated, so its allowlist holds nothing
  there; `status` adds `source allowlists of <routes>` once a start as root applied the lists the
  release and the recorded values give, saw Docker's `FORWARD` jump to `DOCKER-USER` and brought
  the stack up (a boot alone never says so), else `source allowlists of <routes> not held (no start as root applied
  them since this release or these values, or its error says why)`, and `published ports' holds left (the next start as root removes them)` once the release
  declares none while a chain is recorded; `forwarding` once every table took the rules the
  release and the devices give now, else `forwarding not applied (no start as root applied it since this
  release or these devices, or its error says why)`, or `forwarding left (the next start as root removes it)` once the
  release forwards nothing; with nothing in force, the line reads `consented (tune --apply), nothing
  in force: …`. A route to a service on the host's
  network is left out of them all. Should a boot's own `iptables` fail, Docker still starts (the
  unit is ordered before it, never required by it, so no other stack of the host waits on it):
  the routes are then open until the install's start, which applies the lists or fails. Both
  chains are replaced at each start, and removed (their jumps first) once the release declares
  none; `--revert` removes them and leaves the routes published on IPv4 alone until the next
  start, which it never runs itself (the stack may be stopped): `note: the L4 routes <names> are
  open to every source now, and stay published on IPv4 alone until the next start, which
  publishes them on every address: `<dir>/<short>-stow --dir <dir> --no-input --no-provision``;
  and each
  `tun` role's whole networkd file, as the VM's image carries it,
  `/etc/systemd/network/10-<name>-<8 hex>-<role>.network`, which matches the device by name once
  its service opens it (networkd reloads when one changed, which applies it; a file whose role a
  release drops is removed). Each file is listed in `.stowage-gateway` before it is written,
  `pending` until networkd reloaded (a start that fails in between is applied again by the next;
  `status` counts them as pending); a listed path that is not such a file is never followed: a
  record that names one does not read (below), and a revert refuses before anything is removed.
  The record is its directory's, compared by file identity (a link to it is the same): at another
  directory — the install moved or copied — it applies nothing, and a start warns (`warning: this
  install's .stowage-gateway is <dir>'s (the directory moved or copied): nothing of the stack's
  network declarations is applied here; `<short>-stow tune --revert --dir <dir>` removes what it
  applied, from there, or remove the file and `<short>-stow tune --apply` here`), as `status`
  does (`not applied: …`); `--apply` there refuses with it before changing the host, and
  `--revert` there leaves the record (a note) and reverts the tuning alone. When that directory is
  gone (the install moved), the revert is this one's: `… is <dir>'s, which is gone (the directory
  moved): nothing … is applied here until `<short>-stow tune --revert` here removes what it
  applied, then `<short>-stow tune --apply` here`. A record naming no directory applies nothing
  either (`… names no directory: … remove it, then `<short>-stow tune --apply` here`). The services' tables are
  named after the services, as on the VM: one install of a project per host applies them. A host where `systemd-networkd` does not run is
  warned and given no file (`warning: systemd-networkd does not run on this host: the networkd
  files of <roles> are not written or reloaded (`<short>-stow tune` gives their settings)`, or, a
  release declaring none, `the networkd files an earlier
  release wrote (…) stay until a start as root finds it running`). What stays the operator's: a
  NIC role's MTU and policy routing, its NIC configured by the host's own files (each start warns:
  `warning: the MTU and policy routing of <roles> are this host's NICs', its operator's: `<short>-stow
  tune` gives networkd's settings for them`), and a transparent role's input accept (an accept in
  a table of the install's cannot override a drop in the host's own input chain): `tune` advises
  both. A start not as root applies none of them (`warning: … are applied by a start as root
  alone`; a release declaring none, with something still applied: `warning: what an earlier
  release applied of the stack's network declarations is removed by a start as root alone`).
  While the consent stands, a release that declares none of them has each start as root remove
  what earlier ones applied. Without the consent, each start warns (`warning: the stack's network
  declarations, … are not in force on this host, whose firewall and routing are its operator's:
  `<short>-stow tune` lists them, `<short>-stow tune --apply` (as root) applies them, and each
  start then again`; `the source allowlists of the L4 routes <names>` among them). `--revert` also
  removes them — the boot units, the tables, the two chains and `DOCKER-USER`'s jumps to them, the
  networkd files (networkd reloaded when it runs) — and the
  consent. A step that fails (a table nft keeps: `the nftables tables <names> are still loaded`)
  leaves the consent revoked: no start applies anything, the record stays, and `--revert` again
  removes the rest (`the consent is withdrawn, but not all it applied is removed (`<short>-stow
  tune --revert` again removes the rest): …`); a record that does not read refuses `--revert`, and an `--apply` of
  declarations, before the host is changed (`…: fix or remove it, then tune again`). `--apply` and `--revert` take the install's
  lock, refused while a start, upgrade, rollback, provision or reset-db holds it (`… runs: wait for
  it to finish, then tune --apply again`); `--dir` relative is taken from the current directory.
  `--revert` asks for both (`… and remove the stack's network declarations, … (the consent
  withdrawn)`) and removes the install's first, so a tuning revert that fails does not keep them.
  A record that does not read is no consent: nothing is applied from it, each start warns
  (`warning: <why>: nothing of the stack's network declarations is applied (fix it, or remove it
  and `<short>-stow tune --apply` again)`), and a revert refuses until it is fixed or removed; a
  revoked one makes each start warn (`warning: the consent to the stack's network declarations is
  withdrawn, but not all it applied is removed: `<short>-stow tune --revert` again removes the
  rest`). `status` says `gateway   not in force: …`, `applied at each start (tune
  --apply): tables <names>; forwarding; <n> networkd file(s)` (and `<n> networkd file(s) pending
  (written, not yet seen loaded by networkd: the next start as root reloads it)`), `not applied: <why the record does not
  read> (fix it, or remove it and `<short>-stow tune --apply` again)`, `not applied: this install's
  .stowage-gateway is <dir>'s (the directory moved or copied): …` (or `…, which is gone (the
  directory moved): …`, `… names no directory: …`), `consented (tune --apply): nothing applied yet, the
  next start as root applies them`, `revoked, but not all it applied is removed: `<short>-stow tune --revert`
  again removes the rest`, or, consented to a release that declares none, `consented (tune
  --apply): the release declares none (`<short>-stow tune --revert` withdraws the consent)`.
- The rules, from the host's RAM (`MemTotal`, rounded down to 64 MiB, so a few KiB from one
  kernel to the next is the same host) and its online CPUs. The value is the one written; "at
  least" (and "at most"): a host holding more (less) keeps it, field by field — judged, while a
  value is still the one `--apply` wrote, against what the host held before the first `--apply`
  (`tune.json`), so a host that shrank goes back down to its own share; a value changed since
  is the host's own, and kept when the rule allows it. Every memory value is at least the
  kernel's default for that RAM; `vm.min_free_kbytes` is the only one that sets memory aside:

  | Setting | `default` | `network` |
  |---|---|---|
  | `net.core.netdev_max_backlog` | 1 % of RAM across the CPUs at 2 KiB a packet, from 1000 (the kernel's) to 32768, at least | 2 %, from 65536 to 262144 |
  | `net.netfilter.nf_conntrack_max` / `nf_conntrack_buckets` | 2 % of RAM at 320 B an entry / a quarter of it, each at least the kernel's table (262144 above 4 GiB, 65536 above 1 GiB, else RAM / 128 KiB, at least 1024) | 4 % |
  | a NIC backed by a device | `ethtool -K <nic> rx-udp-gro-forwarding on` and `ethtool -K <nic> rx-gro-list off`, one feature a command, where the driver lets it be changed (a driver's fixed feature refuses every feature of its command): forwarded UDP (QUIC to a container, a tunnel) stays batched | also its receive queues' steering, rings and interrupts (below) |
  | the modules loaded at boot | `tun`, `nf_conntrack` | also `tcp_bbr`, `sch_fq` |
  | `net.core.rmem_max`, `wmem_max`, the top of `net.ipv4.tcp_rmem` / `tcp_wmem` | the kernel's | RAM / 128 rounded down to a power of two, from 16 MiB (a QUIC stack asks 7) to 128 MiB (10 Gbit/s over 100 ms), at least; `tcp_rmem` `4096 131072 …`, `tcp_wmem` `4096 16384 …` |
  | `net.ipv4.tcp_mem` (all TCP sockets, in pages) | the kernel's | the kernel's default (low, pressure, high: ¾, 1 and 1½ × RAM / 16 pages, about 4.7, 6.25 and 9.4 % of RAM) × 2, at least |
  | `net.ipv4.udp_mem` (all UDP sockets: QUIC, tunnels) | the kernel's | the kernel's default (from RAM / 8 pages, about 9.4, 12.5 and 18.75 %) × 1.5, at least |
  | `vm.min_free_kbytes` | the kernel's | 0.5 % of RAM, at most 128 MiB, and at least the kernel's √(16 × RAM in KiB) |
  | `net.core.rps_sock_flow_entries` | the kernel's | RAM / 128 KiB, a power of two from 4096 to 65536, at least |
  | `net.core.netdev_budget`, `netdev_budget_usecs` | the kernel's | 600, 8000, at least: more packets a CPU takes from the NICs in one pass |
  | `net.core.default_qdisc`, `net.ipv4.tcp_congestion_control` | the kernel's | `fq`, `bbr` |
  | `net.ipv4.tcp_notsent_lowat` | the kernel's | 131072, at most: less unsent data held in a socket (latency; BBR keeps the throughput) |
  | `net.ipv4.tcp_slow_start_after_idle`, `tcp_mtu_probing` | the kernel's | 0, 1 (at least) |
  | `net.core.somaxconn`, `net.ipv4.tcp_max_syn_backlog` | the kernel's | 8192, at least |
  | `net.ipv4.tcp_tw_reuse` | the kernel's | 1: an outgoing connection reuses a port in `TIME_WAIT` (the kernel's 2 does so on loopback only) |
  | `net.ipv4.ip_local_port_range` | the kernel's | `10000 65535`, a range the host holds wider kept: its width caps the connections open to one destination |
  | `net.ipv4.ip_local_reserved_ports` | the kernel's | the stack's own ports within that range — the front's (`STOWAGE_HTTP_PORT`, `STOWAGE_HTTPS_PORT`, each loopback route's port) and each `l4` route's — added to the host's list, never given to an outgoing connection; left out when none is. A port a service publishes itself (`ports:` on loopback) is not reserved: one at 10000 or above may be taken by an outgoing connection while its service restarts — publish it below 10000, or as a route |
  | `net.ipv4.neigh.default.gc_thresh1` / `2` / `3`, and IPv6's | the kernel's | 8192, 32768, 65536, at least: the neighbour table of a network of many clients |
  | `net.ipv4.conf.all.rp_filter`, `conf.default.rp_filter` | the kernel's | 2 (loose; any value but 1 passes): a tunnel's replies may come back another way |
  | a NIC's receive queues | — | each queue's `rps_flow_cnt` its share of `rps_sock_flow_entries`, the table over the CPUs as a power of two (flows steered to the CPU that reads them; in the udev rule too, on each queue's own appearance, a change of the NIC's channels included); a NIC with fewer receive queues than CPUs also has each queue's `rps_cpus` at every online CPU (RPS: the hardware cannot spread them), in a udev rule of its own (`DEVPATH=="*/net/<nic>/queues/rx-*"`) |
  | a NIC's interrupts | — | the NIC's interrupts (its PCI device's `msi_irqs`, a virtio NIC's parent's) that all land on the same CPUs where they run now (`effective_affinity_list`, else `smp_affinity_list`) go one to a CPU, round robin over the online CPUs; a NIC whose interrupts are spread already, a driver whose interrupts the kernel manages (the write refused: said), or a host where `irqbalance` runs (`n/a`) is left as it is. The report gives interrupts all on the same CPUs as `advice` (the kernel may manage them), spread ones as `ok`. Set on the running host, never in a file: a Docker host's reboot loses it, the VM's boot sets it again |
  | a NIC's rings | — | its receive and transmit rings (`ethtool -G`) grown to the driver's maximum within 1 % of RAM at 2 KiB a slot, a power of two — never shrunk, and only on a link not yet up (`IFF_UP` clear; flags that do not read count as up), as a resize resets it: the VM's first boot pass grows them; on an up link the item is `advice` with the `ethtool -G` to run while it is down (also when a driver refuses a ring it lists), `n/a` when the driver offers no larger one |
  | a TUN or TAP device made in the host's namespace | — | `tx_queue_len` 2 % of RAM at 2 KiB a packet, from 1000 to 10000; `queues/rx-0/rps_cpus` every online CPU; `rps_flow_cnt` as `rps_sock_flow_entries` |
  | Docker's `daemon.json` | not read on a Docker host; on a Kairos VM the network profile's `log-opts` (its logs share its disk) | `log-opts` `max-size` 20m, `max-file` 5: the default `json-file` driver's rotation, for the containers made from then on, a service's own logging options still valid — `n/a`, left out, when the file names another driver or has `log-opts` of its own, or dockerd runs with `--log-opt` or another `--log-driver` |

  The `tun` module is checked on both (`present`).
- **On a Kairos VM**, `<name>-tune.service` runs `<short>-stow tune-boot` at each boot, after the
  kernel's own settings and udev's first pass, before the network starts, and
  `<name>-tune-online.service` runs it again once the network is online, before Docker starts
  (the default route's MTU is known then, and a NIC probed late is there). Its Docker file rotates
  the containers' logs on every profile (`log-opts` as the table's `network` column: the VM's
  logs share its disk). It applies the
  stack's profile — the install's record, before the first install the settings file's
  (`vm create --tune-profile`), else the release's: it loads the profile's modules, writes its
  settings for this boot's RAM and CPUs over what the kernel holds (so a resized VM is tuned for
  its new size), the udev rule to `/run/udev/rules.d/60-stowage-net.rules` (the offload of a
  NIC, and on `network` the queues of a NIC or TUN device, that appears later), on `network` the
  daemon keys into `/etc/docker/daemon.json` before Docker reads it, and sets the NICs, TUN
  devices and CPUs already there, then records the profile it applied and the ports it
  reserved in `/run/stowage/tune-profile` (0644, `network 443,15432`); it writes no file that outlives the boot and keeps no
  `tune.json`. A daemon file that does not merge is left as it is, and the rest is tuned. Its
  notes are in the units' journal. `sudo <short>-stow install --tune-profile P` on an installed
  VM records the operator's profile and restarts the containers with its settings; a profile
  higher than the one `/run/stowage/tune-profile` says the VM holds tunes the VM at once, as the
  boot does (so a re-run after one that failed before it tuned still tunes it) (`tuned by the network profile for … MiB and …
  CPUs`; a ring of an up link waits for the next boot), as does a re-run on the `network` profile that
  changes the stack's ports (their reservation); a lower one takes over at the next reboot
  (`<short>-stow reboot`), the higher one's settings staying until then (said). The install is
  done by then: a tuning that fails is a note (`tuning the VM now failed …: its next boot tunes
  it`), not the run's failure. The VM's `tune` reports against what the VM
  holds — the profile and ports `/run/stowage/tune-profile` records, else the stack's — and
  `--check` checks it; it has no `--print`, `--apply`, `--revert` or `--profile`, and
  its drift line names the reboot that tunes it again and the units' journal (`sudo
  journalctl`). On Hyper-V and Azure, `/etc/systemd/network/00-<name>-sriov.network` (first of
  the VM's network files, `network set`'s override included) leaves the accelerated networking's
  virtual function (`mlx4_en`, `mlx4_core`, `mlx5_core`, `mana`) unmanaged: the kernel bonds it
  under the VM's NIC, whose address it shares. The image carries `ethtool`, and the build fails
  when its kernel lacks `tun`, `nf_conntrack`, `tcp_bbr`, `sch_fq`, or the clouds' NIC drivers:
  `gve` (gVNIC) and `mana` (Azure), copied from the kernel's extra modules package, and
  `mlx5_core` (Azure's ConnectX), or the drivers a server boots with (bare metal): its NICs'
  (`ice`, `i40e`, `ixgbe`, `igb`, `igc`, `e1000e`, `bnxt_en`, `mlx4_en`, `tg3`, `qede`, `r8169`)
  and disk controllers' (`nvme`, `ahci`, `megaraid_sas`, `mpt3sas`, `smartpqi`, `hpsa`). The
  image carries the distribution's firmware (`linux-firmware`) and CPU microcode. The VM modules give the hypervisor's part: multiqueue virtio
  on libvirt and Proxmox, gVNIC on Compute Engine, accelerated networking on Azure ([`vm
  create`](vm-create.md)); TrueNAS offers no queue setting, which the
  report gives as `advice`.
- The files, named for Stowage rather than one stack (two stacks write the same values):
  `/etc/sysctl.d/60-stowage-net.conf`; `/etc/modules-load.d/60-stowage-net.conf` (the profile's
  modules, loaded at boot, before the settings that need them);
  `/etc/udev/rules.d/60-stowage-net.rules` (the NICs' offload, left out without `ethtool`
  installed, and on the `network` profile the TUN devices' queues, each NIC queue's steered
  flows and the RPS of a NIC with fewer queues than CPUs, at each device's or queue's
  appearance); on `network` the keys above merged into the daemon's file —
  `/etc/docker/daemon.json`, or the `--config-file` of the dockerd that runs as root (snap; a
  relative one from `/`; a user's rootless dockerd is never read) — its other keys kept (a file
  that does not parse is refused, nothing written); `/etc/stowage/tune.json`, the values the
  first `--apply` found (the modules loaded first, so their settings are there), the values the
  last one wrote, the profile, and each daemon key it set with its file, which `--revert` reads;
  a `tune.json` that does not read is reported as `differs`. `--apply` and `--revert` refuse a
  user before they read or ask anything; `--apply` and `--print` refuse Docker Desktop (running,
  or stopped: the CLI's context names it), whose containers this host's settings miss.
- On the running host `--apply` loads the modules (`modprobe`), writes the settings, runs
  `ethtool` on each NIC, on `network` spreads the NICs' interrupts and packets and sets the TUN
  devices there, and under `host.power: performance` holds the CPUs at their full speed. An interface already up takes `fq` when it next
  comes up. Docker reads its file when it restarts, which restarts the containers: `--apply`
  says so and leaves it to the operator, unless `--restart-docker` (`systemctl restart docker`).
  `--revert` puts back each daemon key still holding what `--apply` set (one changed since is
  left, and said), writes back the settings the first `--apply` found, and leaves the NICs'
  offload, interrupts and steering, the TUN queues and the CPUs' governor until the next boot.
- `advice` judges each NIC on what its hypervisor or cloud decides: an emulated NIC (e1000,
  rtl8139: give the VM a virtio or vmxnet3 one), a virtio NIC with fewer receive queues than
  vCPUs (multiqueue), virtio on Compute Engine (gVNIC), an Azure NIC without accelerated
  networking, a Xen NIC (an instance type with ENA).
- `advice` also covers a NIC `ethtool` cannot read, and a host without `ethtool` (install it,
  then `--apply` sets the offload).
- `n/a`: on Docker Desktop the containers run in its own VM, which this host's settings miss;
  rootless Docker's daemon file and network are the user's, and are left out; an offload the
  kernel or the driver fixes (`[fixed]`, or none: Linux before 5.12); an IPv6 setting of a kernel
  running without IPv6 (`ipv6.disable=1`), which `--apply` does not set either; a daemon key dockerd is
  started with as a flag (`--log-opt`), which its file may not repeat — dockerd refuses to start
  on both — so `--apply` leaves it out and says so.
- `--revert` and `--print` run without a reachable engine (`--print` then leaves the daemon
  file out): an engine that does not start may be what is reverted.
- A payload's own TUN device lives in its container's namespace, which the host's rules do not
  reach: the payload gets it with `devices: [/dev/net/tun]` and `cap_add: [NET_ADMIN]` in its
  compose file (Stowage grants neither), sets its offloads itself (`TUNSETOFFLOAD`: TCP and UDP
  segments, on kernel 6.2 or later), its queue (`ip link set <tun> txqueuelen …`, or a
  multi-queue TUN) and its MTU (the host's, less the tunnel's overhead). Its UDP socket may ask
  for the host's `rmem_max` / `wmem_max`, which hold for every namespace.
