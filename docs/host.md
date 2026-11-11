# What a service asks of the host (`services`)

[README](../README.md) · [Documentation index](../README.md#documentation)

**Memory shares** (`services.<name>.memory`). A service with a share gets that percent of the host's RAM as its memory limit,
written in `stowage-content.yaml` before each start — the install, each re-run, `upgrade`,
`rollback`, `reset-db` and, on the VM, each boot — so a host given more RAM gives its services
more at the next start, and a change of the limit recreates the service then. The RAM is read
from `/proc/meminfo` in 64 MiB steps; a limit from the percent is whole MiB, one from a `min` is
its part to the byte (rounded up), and none is less than 64 MiB.

- **Never below the compose file's own limit**: a service whose `mem_limit` (or
  `deploy.resources.limits.memory`) is larger than its share keeps it. A share gives no service
  less than its compose file does; a service with no limit of its own gets its share as one.
  Nor below the memory it reserves (`mem_reservation`, `deploy.resources.reservations.memory`),
  which Docker refuses a limit under. These floors can take the limits past 90 % of a small
  host's RAM: the 90 % bounds the shares, not the floors.
- **A maximum caps it**: `{ percent: 25, max: 16G }` is a quarter of the RAM up to 16 GiB, for a
  service whose use stops growing past some size (a database's caches, a gateway's buffers). The
  floors above still win over it.
- **A minimum floors it**: `{ percent: 5, min: 256M }` is 5 % of the RAM, never under 256 MiB:
  the least the service needs, which a small percent of a small host would not give. It wins over
  the maximum (lint refuses a `min` over the `max`), and the compose file's own limit still wins
  over it when larger.
- **The share is the service's**: a service run as several containers (`scale`,
  `deploy.replicas`) splits it, its minimum and its maximum, each container's limit its part.
- The limit is set on the key the compose file sets it with — `mem_limit`, or
  `deploy.resources.limits.memory`, or both when it sets both (Compose refuses them distinct);
  `mem_limit` when it sets neither. A `memswap_limit` it sets moves by as much as the limit, so
  the swap it allows stays the same.
- Each such service's container is given `STOWAGE_MEM_LIMIT_MB` (its limit, in MiB, the maximum
  applied),
  `STOWAGE_HOST_MEM_MB` (the host's RAM, in MiB) and `STOWAGE_HOST_CPUS` (its online CPUs), to
  size its own caches. Compose does no arithmetic, so a command that needs a fraction computes
  it in a shell, e.g. Postgres at a quarter of its limit:

  ```yaml
  postgres:
    command: [sh, -c, 'exec docker-entrypoint.sh postgres -c shared_buffers=$$(( $${STOWAGE_MEM_LIMIT_MB:-512} / 4 ))MB']
    mem_limit: 256m   # the least it gets
  ```

- A service a profile leaves off gets nothing until it runs. On Docker Desktop no service is
  given a share: its containers run in a VM of its own, whose RAM is not the host's. On
  Kubernetes the chart sizes its pods (`resources:`): a share does not reach it. In both,
  and in a service no share names, the three variables are unset: a command that reads them
  gives a default (`$${STOWAGE_MEM_LIMIT_MB:-512}`).
- A start that cannot read the host's RAM, or whose `docker info` does not answer, fails
  (`the containers' network settings and memory: …`) rather than start the services unsized.
- **A service may size itself from its own cgroup instead**: its limit is in
  `/sys/fs/cgroup/memory.max` inside its container (`max` when it has none), which also holds
  where `STOWAGE_MEM_LIMIT_MB` is unset — on Kubernetes (the pod's `resources.limits.memory`), on
  Docker Desktop, and for a service no share names but whose compose file sets a limit.

**Whole cores** (`services.<name>.cpus`). At each start (Docker and the VM, not Docker Desktop) a
service given `cpus: N` runs on N whole cores of its own: its `cpuset` in `stowage-content.yaml`,
the count of its CPUs (the cores' SMT threads) in `STOWAGE_CPUS`. The cores are read from sysfs
(`core_id` within `physical_package_id`; a CPU whose topology does not read is a core of its own)
and given from the last core down, the services in their names' order, never the core of CPU 0,
which stays the host's: the same host gives the same sets at every start and every boot. A host
with fewer cores beside CPU 0's than the services ask fails the start (`services.*.cpus: <services>
ask N whole cores, and this host has M beside the one of CPU 0, which stays the host's`), and an
upgrade to a release asking that many is refused before it changes anything, reading this host's
cores as the start does (`<name> <version> cannot run here, and nothing was changed: …`; Docker
Desktop, running or stopped, has none to check; an engine that does not answer, or a host whose
cores do not read, refuses it, nothing changed, until the upgrade is tried again: `reading this
host's cores for the services' cpus: …`); on the
VM before its OS is written (`… so its OS is not written`), the boot into it never failing for it.
`lint` checks the cores against `shapes.kairos.size` (see `services` in the spec). With
`irq: off` (the default) `tune` keeps the NICs' interrupts and their receive steering (RPS) off
those cores: `--apply` and the VM's boot write every NIC interrupt's affinity, on any profile,
one to a CPU in turn over the other CPUs alone (all of them when none is left), wherever the
kernel spread it, and the network profile sets `rps_cpus` over them; `--check` reports an
interrupt whose affinity (its effective one, else the one asked —
also for one that never fired, `per_cpu_count` all zeros, a virtio NIC's config interrupt, while
its effective CPUs lie outside the one asked) reaches those cores as a
difference (`<nic> interrupts: N of M on the services' cores (IRQ …)`; an interrupt the kernel
manages refuses the move, which `--apply` notes; one whose CPUs do not read is advice), and where
`irqbalance` runs, `tune` advises
`IRQBALANCE_BANNED_CPULIST=<their CPUs>` in its environment file (`/etc/default/irqbalance`, or
`/etc/sysconfig/irqbalance` where the RHEL family's is alone), `ok` once irqbalance bans each of
them, as it reads its environment: the running daemon's own (`/proc/<pid>/environ`), else the
file's (its last assignment, quoted or not, exported or not); `IRQBALANCE_BANNED_CPUS` (a mask)
first, else `IRQBALANCE_BANNED_CPULIST`, else the CPUs the kernel isolates (`isolcpus`) or runs
without ticks (`nohz_full`). A file changed without a restart of irqbalance is advice until it is. `irq: on` leaves them on the
service's cores too (a service that reads its NIC's queues on its own cores). On Kubernetes, `k8s
tune` prints the service's cores as the chart's: a pod of the Guaranteed class (whole CPU requests
equal to its limits) on a kubelet of the static CPU manager policy.

**A service's nftables table** (`services.<name>.nftables`). The installer carries the file (its
content enters the installer's and the OS bundle's inputs). On the VM, each start loads it into the
firewall before the services start, replaced whole in one transaction (`table inet <name>`,
`delete table inet <name>`, then the file's table, `<dir>/nftables/<name>.nft` once loaded), which
nft checks before it commits (`services.<name>.nftables: nft refuses its table: …` fails the
start) — at a start that finds the table absent (a boot that did not load it) or its file changed; a table that holds
already is left as it is, its sets as the service fed them. A load empties its sets, so each load
writes a token of its own (`<dir>/nftables/<name>.load`), the service's `io.stowage.nftables`
label: the start that loaded the table recreates the service, which feeds them again. A table is
recorded as soon as it is in the firewall, so one a start loaded before another of its tables
failed is deleted once a release drops it; a load that fails is tried again at the next start. The installer reading a release refuses a table no service declares, one of the
project's name (the VM's own firewall's) or of a word of nft's own, and a body that is not its
table alone (as lint checks it); a
table
of a service the release no longer declares is deleted (the tables loaded are recorded in
`<dir>/.stowage-nftables`). At boot, before Docker starts and its restart policy brings the
stack's containers back, a unit loads again each table the last start loaded, from
`<dir>/nftables/<name>.nft`, so no service runs a moment without its table: the VM's
`<name>-tables.service` (in the OS bundle, once the network is up, after the VM's own firewall)
and a consented Docker host's `<name>-gateway-<8 hex>-tables.service`, each running
`<short>-stow tables-boot` (hidden), which logs each table (`loaded the nftables table inet
<name>, before the containers start`). Each load writes a new token, so the boot's start, which
finds the tables loaded, recreates their services, which feed their sets again. A table that
does not load then (`services.<name>.nftables: its table does not load at boot (the install's
start loads it again): …`, in the unit's journal) leaves the others loaded, and Docker starts all
the same. Until the boot's start, the input chain's accepts of the install's ports and of the
transparent sockets (under `/run`) are absent, so it drops that traffic, and what the host
forwards between roles meets Docker's FORWARD policy (DROP). The host's own table stays Stowage's. A connection open before a load keeps its
conntrack entry, so a rule for new connections only (after `ct state established,related
accept`) or a nat rule (`redirect`, `dnat`) of the new table leaves it be until it ends (TCP's
established timeout is 5 days by default): a start that loads a table whose rules changed says so
(`note: services.<name>.nftables: its table changed, and the connections open before keep their
conntrack entries: …`). With `services.<name>.flush_conntrack: true`, a start that loads the
service's table anew (its first load, another file, changed rules, a table absent) flushes the
host's connection-tracking table once its tables are loaded, IPv4 and IPv6, before the services
start (`flushed the host's connection-tracking table (services.<name>.flush_conntrack): the connections open before meet the tables and sets now loaded as new ones`;
through netlink, no `conntrack` tool needed): each open connection's next packet meets the
firewall as a new one. The VM's input chain accepts SSH and the install's ports as new, so those
connections go on, as do the stack's published ports (Docker's DNAT is made again); a connection
the host or a container opened outward may be cut, and its client connects again: a reply that
comes before its next outgoing packet has no entry, so it meets the input chain as new, which the
VM's drops, and a container's is no longer translated back (Docker's masquerade), so the host
answers it with a reset; on a Docker host every container's and process's connections are judged
anew, the host's other workloads' too. A table held as the
last start loaded it flushes too when the start changes a set an input fills
(`services.<name>.sets`: the input's value written otherwise than at the last start, an element
added or removed — a set may admit or deny by its elements, so either can end a flow's verdict; a
set the release no longer fills, whose elements a load of the table then empties, the table
loading again even while other sets stay filled, which also empties what the app socket fed; a
set an input fills anew; or the last start's record of the sets, `<dir>/nftables/<name>.sets`,
unreadable or not of the form it writes, the table then loading again); the same values flush
nothing. A change nft refuses changes nothing and flushes nothing, then or later. A change through
the app socket never flushes (below). The boot's load flushes nothing, and drops a flush still
due (a reboot empties the table). The flush runs however
the start's loads end, a later table's refusal included. A flush that fails is warned, and the
start goes on (`warning: services.<name>.flush_conntrack: the host's connection-tracking table
does not flush: …: the connections open before keep the previous table's verdict until they end,
or until `sudo conntrack -F` (the next start tries again)`): the load is marked first
(`<dir>/nftables/<name>.flush`, holding the boot it was written in), so each next start flushes
until one succeeds, even though it finds the table held; the boot drops a mark of an earlier boot
(a reboot empties the table), and keeps one of its own (the unit run again by hand). A service the release gives the
app socket feeds its table's named sets through it (`POST /v1/nftables/<table>/sets/<set>`, below).
A service feeding another's table (`services.<name>.feeds: [<table's service>]`: the
least-privilege shape, where the socket's token stays with a service the clients do not face)
carries that table's load token too in its `io.stowage.nftables` label (each table's
`<table> load <token>`, `; ` apart), so a start that loads the table recreates it beside the
table's own service, and it feeds the sets again. A set filled from an input
(`services.<name>.sets: { <set>: <INPUT> }`) is the input's alone — the app socket refuses to
write it (409, `the set <set> of the table inet <name> is filled from the input <INPUT> at each
start (services.<name>.sets): the input is its whole content`), and the input is checked as it is
given (a prompt, a flag, `--answers`), as at the start: each start empties it and adds the
input's elements (comma-separated, each an IP address, a prefix, a port from 0 to 65535 or a
range of two of one kind (low first, one address family), or their concatenation
`a . b`, written as such — a host or service
name, which nft would resolve as it loads, at boot before the network, is refused; at most 1024;
a blank input leaves it empty) in the table's own transaction — the load's when the start loads
the table, one of its own (`nft -f -`) when the table holds and the elements changed since the
last start — so a set of its rules is never empty a moment, and a re-run that changes the input
changes the set without recreating a service. A release that no longer fills a set of an
unchanged table loads the table again, which empties the last input's elements. An element nft does not read fails the start before any table
loads (`<INPUT> (services.<name>.sets.<set>): element "…": …`); a set nft refuses (a value of
another family, overlapping intervals) fails it naming the key (`services.<name>.sets (<INPUT>,
…): nft refuses their elements: …`, or at a load `services.<name>.nftables: nft refuses its
table, or services.<name>.sets (…) their elements: …`). The script is kept beside the table
(`<dir>/nftables/<name>.sets`), and the boot loads it in the table's transaction too; elements
nft refuses then leave the table loaded alone (`services.<name>.sets (…): nft refuses their
elements at boot, so the table loads without them (the install's start fills them again): …`,
in the unit's journal), their script removed so the boot's start writes them again; a script
that does not read makes the start load the table again with its sets. On a Docker host (once consented, below), a set
of a table the app socket feeds (its service or a feeder of it in `shapes.kairos.host_api`) that
no input fills is warned at each start (`warning: the set <set> of the table inet <name> is fed
through the VM's app socket, which a Docker host does not serve: it stays as the table declares
it here unless its own rules fill it (services.<name>.sets fills a set from an input)`), once a release.
On a Docker host, whose firewall is its operator's, a table is loaded only once the operator
consented with `tune --apply` (see "A Docker host's gateway" ([tuning](tuning.md)) under `tune`), by each start as
root; until then each start warns that the stack's network declarations are not in force, `tune`
gives the table as advice (`nftables table inet <name>`) and `tune --print` prints it.

**An address of its own** (`services.<name>.interface` and `address_input`). A service with an
address in its input joins an ipvlan network (L2 mode) on the NIC playing that role beside the
networks it joins, at that address (`ipv4_address`): the network `stowage-<role>`, its subnet the
NIC's at the start (from its IPv4 address, `ip -4 -o addr show`), its parent the NIC — on the VM the
role's own name, on a Docker host the NIC `install --interface <role>=<NIC>` named
(`STOWAGE_INTERFACE_<ROLE>` in the install's `.env`). An empty address gives none: the service is
on the stack's networks alone. A NIC that has no IPv4 address, an address outside its subnet, the NIC's own address or the
subnet's network or broadcast address, or a
Docker host whose install named no NIC for the role fails the start, naming what to give. The
ipvlan's traffic does not cross the host's firewall (its rules are the service's table's).

**An upgrade that overlaps** (`services.<name>.update: start-first`). Each start (an install, a
re-run, an upgrade, a rollback, a boot; Docker and the VM) first replaces each start-first service
whose running container differs from what the start runs (its configuration's digest, Compose's
`com.docker.compose.config-hash`, or its image): the new container starts beside the old one
(`docker compose up --wait --no-deps --no-recreate --scale <service>=2`), and once it runs —
healthy, when it has a healthcheck — the old one is stopped with its `stop_grace_period` (10 s by
default) as the drain, then removed (`services.<name>: start-first: the new container starts
beside the old one`, then `… the new container is healthy, the old one stopped`). The rest of the
stack then starts as before. A new container that does not become healthy is removed and the old one
serves on: the start fails (`services.<name>: its new container did not become healthy beside the
old one, which serves on: …`), quoting the last lines it wrote, read before it goes (`…; its last
lines (docker logs --tail 20), the container removed:`, then each line after `  | `, at most 4 KiB, a control or format
character but a tab shown as `?`; each container's ID before its lines when there are several), and
then without the `see: docker compose … logs` pointer of a failed start, whose logs no longer hold
it; one that wrote nothing, whose log does not read, or that stays (`a new container left (see the
warning)` when its removal fails) keeps the pointer. A service none runs yet, or one unchanged, is the stack's start's
as any other. For a moment two containers run: twice its memory share and the same cores.
Sharing the work is the app's: through the front, both containers answer its upstream's name
until the old one stops, so the old one drains on SIGTERM (no new request, those in flight
answered), and the front tries a request it could not deliver again; on the host's network, both listen on one port with `SO_REUSEPORT`, or the old one
hands its listening sockets to the new over a Unix socket in `update_socket`, a volume of the
service's own (`<project>_stowage-update-<service>`, Docker's: made from the image's directory
at its first mount, kept between starts). A server whose old process sends its listeners only
once told to go names that signal in `update_signal` (with `update_socket`, an old container that does not mount
the service's update volume, `<project>_stowage-update-<service>`, has no socket to hand over: the
start replaces the service stop-first, `services.<name>.update: its old container does not mount
the update_socket volume (no socket to hand over), so this start replaces it stop-first`): the new container is started without
waiting (`docker compose up --detach --no-deps --no-recreate --scale <service>=2`), and once it
runs the old one is sent the signal (`docker kill --signal <signal>`; `services.<name>: the new
container runs: <signal> sent to the old one (update_signal)`); the overlap then reads the new
one's health until it is healthy (or, without a healthcheck, at once), and stops the old one as
above (one that ended on its own after the handoff is removed). A new container that stops
running or turns unhealthy fails the start as above, its message saying the old one was sent the
signal and serves on as far as the app keeps serving after it. With `update_ready`, the signal waits until
the new container holds that file in its `update_socket` directory as the directory did not hold
it before the new one started (read with `docker cp <container>:<update_socket>/<file> -`, first
through the old container where it mounts the service's update volume — an earlier release's
`update_socket` may be another directory: none then, or a file whose time, size or content differs
from the one an earlier overlap left; a read of it that fails otherwise than on the engine's
answer for a missing path, like a failed read of where the old container mounts the volume,
fails the start before the new container is made, the old one serving on (`services.<name>:
before its new container starts, which it does not: …`)), so a container still preparing (validating its settings, rendering its configuration)
or failing during it never has the old one hand its listeners over (`services.<name>: the new
container wrote <update_socket>/<file> (update_ready)`); one that does not write it within 10
minutes fails the start, the old one never signalled (`… did not write <update_socket>/<file>
(update_ready) within 10m0s: the directory must be writable by the container's user …`). The directory must be
writable by the container's user: a new volume takes the image's directory, owner included, and
a volume made before keeps its owner (remove it, `docker volume rm`, for the next start to make it
again). With `update_decline_status`, a new container that exits
with that status before the old one is signalled (under `update_ready`, before it writes the
file; under a restart policy, restarting after it) declines the overlap: it is removed, and the stack's start replaces the service stop-first
(`services.<name>.update: its new container declined the overlap (exit status N,
update_decline_status), so this start replaces it stop-first`) — for a server whose handoff is
sound only while its listeners and their options are unchanged; a decline after the signal fails
the start as above. The start replaces it stop-first, logging why
(`services.<name>.update: <why>, so this start replaces it stop-first`), when the stack it runs
publishes the service's ports from its own network (an L4 route's upstream on Docker), gives it a
fixed name or address (`mac_address`, a network's `ipv4_address`, `ipv6_address` or `mac_address`), runs it in another's network namespace, has another service join its namespaces or mount its volumes (`network_mode`, `pid` or `ipc: service:<name>` or `container:<project>-<name>-<n>`, `volumes_from: [<name>]`, `[service:<name>]` or its container, `links: [<name>[:alias]]`, `external_links: [<its container>[:alias]]`, its container named `<project>-<name>-<n>` or `<project>_<name>_<n>`, written or from a variable), or has a container of the engine, running or not, the stack's or not, join its container by ID (a prefix of it no container is named, too) or name, mount its volumes or link to it (`links:`, on any network), as the engine records them, or mounts a new volume that awaits its `uid` (the containers are then made first), and
when it has several containers, a container that is not running or not healthy, or an anonymous
volume (an image's `VOLUME`: only a recreate hands it on to the next container), when the start
makes or changes a service it depends on (`depends_on`, theirs included) or finds one not serving
(running, healthy when it has a healthcheck, or, for one every service of the service's
`depends_on` chain that depends on it waits for with `condition: service_completed_successfully`,
exited 0): the new container would
otherwise start against it, and when the stack's networks differ from those the last start that
came up ran, or no such start recorded them (their digest, `.stowage-networks` in the install's
directory: read and removed as each start begins — one that cannot be removed, a non-empty
directory in its place say, fails the start there, before it changes anything, naming it to
remove — put back when the start fails before it moved a network (before the stack's own start, or a
teardown for a changed MTU or IPv6), and
written again only by a start that comes up with a start-first service, so a start that fails
after that leaves none; a record that cannot be read counts as none):
Compose makes a changed network again under every container on it. The start fails when the engine's containers cannot be listed or read (`listing the engine's
containers: …`, `reading the engine's container <id>: …`, or `reading the engine's containers:
"<line>"` for one that does not parse; one removed meanwhile is skipped). A
service the start leaves as it is logs nothing. Start-first services are replaced the ones they depend on first. On
Kubernetes the chart's Deployment strategy is the equivalent: a `RollingUpdate` with
`maxUnavailable: 0` (one replica's default rounds to it) starts the new pod before the old one
stops.
