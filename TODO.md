# TODO

Pending features, prioritised MoSCoW for the **first version**. Its goal is parity: Stowage does
what an existing production deployment of this kind does today on its three shapes — nothing
more. A finding (a bug, a wrong behaviour, a security hole, a review's leftover, a weak test) is
never written here: it is fixed in the slice that finds it; only one blocked outside the
repository is, under Blocked, with what unblocks it. A finished item is deleted, not ticked.
The plan behind it is [docs/DESIGN.md § 11](docs/DESIGN.md#11-phases).

## Must — parity

Batched by the gate they share: a batch is one slice, one review, one gate run. The beta is every
Must above Blocked (user, 2026-11-15): the clouds under Blocked ship in it as preview, Google
Cloud alone proven.

- **`describe --json` names each secret's `secrets rotate` way** (`rotate`: `passes` for
  `rotation: true`, `replace` for `replace: true`, `none`): the provider's `secret_rotations`
  refuses at plan only from it (its TODO, Should 3). An additive field of the machine interface.
- **e2e-vm crosses a schema baseline for real**: no suite crosses one on the VM; the crossing and
  0.45.0's inputs for it are proven by unit tests alone. Ship a release at a new baseline adding a
  required input, `reset-db --new-baseline --set` over SSH, the boot that crosses (data wiped, the
  input recorded, the staged file gone), and the refusal without `--set` naming that command; run
  with the next slice that runs e2e-vm.

### Supply chain — stopgaps back to canonical

1. **Check every stopgap at each release, and drop each one the upstream has caught up with**:
   images are kept past published vulnerabilities by forcing things over what their upstream
   ships. Each is a stopgap that must give way to the upstream's own fix as soon as it is released.
   - Go modules forced over a release's own `go.mod` (`make self-audit` prints each one as
     `override:`): the front over Caddy 2.11.9 (golang.org/x/net 0.62.0 over its 0.60.0, go-jose/v4
     4.1.7 over 4.1.6, klauspost/compress 1.20.2 over 1.20.1) and the scanner over grype 0.121.3
     (go-jose 4.1.7 over 4.1.6); the backup runner, rclone 1.76.0, has none. The
     canonical form is a later upstream release that carries the module at that version or later.
     Move the recipe to that release and drop its `--replace` / `go get`. Checked 2026-11-11:
     Caddy 2.11.9 carries chi 5.4.0, rclone 1.76.0 x/net, chi, compress, brotli and grpc 1.84.0
     (past GO-2026-6443, which hit the 1.84.0-dev snapshot 1.75.1 shipped), grype 0.121.3 x/net,
     compress and brotli — those raises are gone.
   - Alpine packages raised over a pinned base: `zlib>=1.4` (CVE-2026-4738), `libssl3>=3.5.9-r1`
     and `libcrypto3>=3.5.9-r1` (OpenSSL, CVE-2026-107496, -88823, -95691, -95692; checked
     2026-11-19: alpine 3.24.4 and caddy 2.11.9-alpine still ship 3.5.9-r0) in
     `internal/front/Dockerfile`, `internal/backup/Dockerfile` and the examples' Python images
     (`examples/minimal/api`, `examples/full/docker/api`, `ingest`, `provision`); `apk upgrade`
     with su-exec in place of gosu in `examples/full/postgres`. These make the image depend on what
     Alpine's mirror serves at build time, so the recipe's digest no longer decides the image. The
     canonical form is a base image whose own build carries the fix: repin its digest and drop the
     raise. `examples/minimal`'s `ref` postgres and the suites' pinned PG carry the same zlib, with
     no raise. Checked 2026-11-11: Alpine 3.24's repository serves zlib 1.4-r0 (CVE-2026-4738 fixed,
     per its security database), but the latest builds of alpine 3.24.4, caddy 2.11.9-alpine,
     python 3.13-alpine and postgres 18-alpine still ship zlib 1.3.2-r1; postgres 18-alpine's gosu
     is still 1.19 built with Go 1.24.6.
   - Base images pinned by digest: repin each to its upstream's latest patch build whenever one
     is published, not only when an audit refuses it.
   - Findings with no upstream fix yet, to take once one is released (`make self-audit`
     2026-11-09): the front's go-jose/v3 3.0.5, which Caddy 2.11.9 (and 2.11.7 before it) still
     links beside v4, GHSA-mgcc-cww5-fp2q high (v3's last release is affected; v4 4.1.6 is
     fixed); golang.org/x/crypto 0.58.0 in every Go artifact (Stowage's own binaries included),
     GO-2026-5932, fixed only in a pseudo-version after 0.58.0 — take the first tagged release.
   - The audit allowances in `examples/full/audit-allow.yaml` (the OS bundle's containerd,
     containerd-shim-runc-v2, ctr and runc, until 2027-01-31, each narrowed to its package and
     binary; every project with the kairos shape and `audit` needs the same): removed once Docker
     ships fixed builds. Those builds do not link x/net's HTTP/2 server, where GO-2026-6612's bug
     is, but the scanner matches on the module's version. Checked
     2026-11-11: Docker's containerd.io 2.4.2-3 is containerd v2.4.2 (x/net 0.58.0) with runc v1.5.2
     (x/net 0.55.0), upstream's latest: no fix yet.

   How: `make self-audit` fails on a stale stopgap: an override the release the recipe builds
   already requires at that version or past it, a raise (the recipes' and the examples'
   Dockerfiles) its pinned base already carries. What stays by hand, before every release: read
   each live `override:` and `raise:` line it prints against the upstream's latest release (a later
   Caddy, grype or rclone carrying the module: move the recipe, then drop the override; a base
   build carrying the package: repin it, then drop the raise), the `apk upgrade` and any module the
   release does not require it leaves to judge, base repins, and the findings with no fix yet.
2. **`vm truenas import-image|seed|stop` back to the TrueNAS provider**: the TrueNAS example's
   `local-exec` steps run them because `truenas/truenas` (1.9.1, checked 2026-11-15) cannot put a
   file on the host (its issue #55, `truenas_filesystem_file`, open: no API deletes a file, and an
   HTTP `/_upload/` transport is needed), write a disk image into a zvol (`vm.device.convert`; no
   issue found) or stop a VM before its devices are deleted (`truenas_vm_power`'s delete does
   nothing, and `truenas_vm_device`'s delete refuses a running VM). Each goes once the provider
   covers it: drop the verb and its step from the example, and `internal/truenas` with the last.

### Blocked — upstream or accounts

1. **Cloud images on AWS and Azure**: `ship` registers the `cloud_publish` images through
   OpenTofu, run for real on Google Cloud only (`STOWAGE_CLOUD_ENV=<file> make e2e-kairos`). Run it
   with the AWS and Azure test accounts once they exist, and fix what they find. Azure, known:
   its probe takes an image of the name whatever its provisioning state (its resource list gives
   none), and a page blob left by an earlier upload makes the next upload of that name fail
   (`azurerm_storage_blob` refuses an existing blob): check both there, and that the service
   principal needs *Storage Blob Data Contributor* on the storage account (blobs are written with
   Entra ID).
2. **Scaleway, run for real**: `cloud_publish: [scaleway]` is written against `scaleway/scaleway`
   2.91.0 and checked against its schema, not yet run on Scaleway (`STOWAGE_CLOUD_ENV=<file> make
   e2e-kairos`, then a VM from the provider's Scaleway example). Check there: the import takes the
   compressed qcow2 (`qemu-img -c`), a tag holding the description's spaces and colon is accepted
   and filtered on, the 5 GB single PUT holds the disk, and the bundle's `scaleway` datasource reads
   the `cloud-init` user data at first boot (its source port below 1024).
3. **OVHcloud**: the same for OVHcloud Public Cloud (OpenStack): a `vm seed` platform and
   `cloud_publish` (the disk uploaded as a Glance image of the project, `ovh/ovh` and/or
   `terraform-provider-openstack/openstack`, whichever the latest official docs make canonical).
   Before writing the registration's module, read the latest provider's official docs and its
   schema, and pin that release.
4. **The darwin installer, run on a real Mac**: needs Mac hardware (arm64, and amd64 if one is at
   hand). The darwin images of the installer index are Go's ad-hoc linker-signed Mach-O with the
   payload appended after the code signature's range, checked by structure alone. Run `script`,
   `describe --json` and a `k8s` verb from each; if macOS refuses one (killed, `Code Signature
   Invalid`), re-sign it ad hoc after the payload is appended (the code directory's code limit
   covering it, as Go's `cmd/internal/codesign` writes one), and add that run to the proof.

## Should

- **Encrypted data on the Kairos shape**: `COS_PERSISTENT` (the app's data, its volumes, the
  install's secrets) encrypted by Kairos's kcrypt — a spec key carried to the install's config,
  the installer ISO's install and the disks' first boot (`kcrypt.encrypt_on_boot`, to prove),
  the recovery system unlocking, the health gate's and the upgrade's unattended reboots kept. The
  key source decides what it protects: the TPM (unattended; without Trusted Boot a stolen machine
  unlocks itself), a passphrase at boot (Kairos 4.5; every reboot needs the console), a key
  server (kcrypt-challenger), or the TPM with a passphrase fallback. Needs a test machine with a
  TPM (TrueNAS's virtual TPM to check, else libvirt with swtpm).
- **Trusted Boot on bare metal**: signed UKIs booted by systemd-boot, the encryption key sealed to
  the measured boot. Replaces what the VM shape builds on GRUB — the health gate's boot assessment
  (systemd-boot's boot counting), `host.kernel_args` (in the signed command line), the recovery
  switch and `rollback` — and adds the publisher's Secure Boot keys (signing at `ship`, enrolment,
  rotation, revocation), AuroraBoot's UKI installer ISO, and a test machine with Secure Boot and
  a TPM. A variant beside GRUB (a project's choice; one release builds both): a UKI install
  encrypts `COS_OEM` and `COS_PERSISTENT` with a key sealed in the TPM, and without a TPM only a
  passphrase typed at every boot unlocks it (the key server attests with the TPM too), so a
  machine without a TPM 2.0 stays on GRUB. A GRUB machine never becomes UKI in place (Kairos
  upgrades by the boot mode it runs): it moves by `backup run`, `secrets export`, a new UKI
  machine made with `--secrets`, and `restore` — an e2e suite proving that move (data, secrets,
  the address kept by the MAC or DNS, the old machine frozen from its last backup), then an
  upgrade and a rollback on UKI.
- **The recovery system's DHCP IAID pinned**: a VM's DHCP clients name themselves by a DUID of
  the NIC's MAC, with an IAID networkd derives from udev's naming data, so the recovery system
  (the install-time image) keeps the VM's leases. A release whose systemd changes that naming
  scheme gives the active system another IAID than the recovery system's, which then takes
  another lease. Pinning `IAID=` in the seed's network files (from the NIC's MAC, at the first
  boot) would hold it whatever systemd names.

- **The installer's binary and its payload as two layers**: each entry of the installer index
  carries the Stowage binary and the release's payload as two layers rather than one file, the
  payload appended at `ship`. The binary layer is then the same in every release a Stowage
  version ships (pushed once, one SBOM), and a darwin binary keeps the code signature it was
  built with. A format change of the installer and the release manifest: the target still keeps
  one `<short>-stow`, and a release is still run by the Stowage that built it.

- **`dns-cloudflare` issued end to end**: e2e-stateless proves `acme` against Pebble
  (`--acme-ca`); `dns-cloudflare`'s DNS-01 step goes through Caddy's Cloudflare module, which
  calls Cloudflare's API to write the TXT record, so a DNS stub alone cannot stand in for it.
  Proving it needs a fake Cloudflare API (the zone and record endpoints the module calls,
  writing into pebble-challtestsrv) or a test zone on Cloudflare; the mode's Caddyfile and
  `--acme-ca` in it are unit-tested.

- **An L4 route hands its upstream the client's address**: a TCP route's upstream sees the
  connection's source as Docker's NAT or userland proxy leaves it, and a front terminating TLS
  on the route (`l4.routes[].tls`) hides it entirely; an upstream that
  logs, rate-limits or judges by the client's address then sees the front or a bridge gateway.
  Offer
  `l4.routes[].proxy_protocol: v2`, off by default (an upstream not reading it would take its
  header for data): the front, or the forwarder the route uses, sends the PROXY protocol v2
  header with the client's own address, IPv4 and IPv6, on every shape. Lint refuses it on a UDP
  route. Prove it with an e2e: an upstream reads the header and sees the test client's address
  over IPv4 and IPv6, on a pass-through route and on a TLS route.

- **An HTTP route's client address is a stated, tested contract**: the front's `reverse_proxy`
  sets `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host` by Caddy's default, but the
  README says nothing of what an upstream may read, and no test holds it: a Caddy upgrade or a
  front change could drop or loosen it silently, and an upstream would then log or judge by the
  front's own address, or by a header the client wrote. State it under the front: the upstream
  gets the client's address in `X-Forwarded-For`, a client-sent value replaced, never appended
  to (no `trusted_proxies`); without `--ipv6` an IPv6 client is the bridge's IPv4 gateway. Prove
  it with an e2e: a request with a forged `X-Forwarded-For` reaches an echo upstream with the
  test client's own address alone, over IPv4 and, with `--ipv6`, over IPv6.

- **A file input on a chart**: lint refuses one asked on the `kubernetes` shape, as the Secret holds
  its recorded form and a chart can only mount a key as it is. A second Secret of the decoded
  contents, named in `.Values.stowage`, would give it the same reach as on Docker and the VM.
- **A real-iptables suite for the Docker host's L4 allowlist**: the rules are proven by unit tests
  and by a one-off probe on Docker-in-Docker (a source off the list dropped, one on it let through),
  but no suite runs a consented start on a real engine, as e2e-full stops at "not in force". A
  privileged Docker-in-Docker step in e2e-full would prove the chain, the IPv4-only publication and
  the boot's order.

- **A volumes-age-rclone backup reads its volumes live**: without a dump, the archive is the
  whole backup, and files a service writes together (a CA's index and serial) may be archived
  between two writes. A provider option stopping the services that mount the volumes for the
  archive (a short downtime per backup), or a service's own quiesce hook, would make it consistent.

- **A host-network service's ports, per client container**: the VM accepts them from every
  container of the stack (`iifname "br-*"`); lint warns of a guarded front route to one. Accepting
  each port only from the containers that reach it (the front, a dependent) needs addresses
  Stowage owns: a network of its own per target with a subnet Stowage picks and its gateway as
  the target's name, or the containers' addresses kept in a set the host daemon refreshes on
  Docker's events.

- **How many NICs of a role is an install's choice, not the release's**: `host.interfaces`
  fixes the roles, so a gateway serving one client LAN on one site and three on another needs
  `lan1` … `lanN` declared in every release, their `forward`, `transparent` and L4 routes
  repeated, and the unused ones warned on every VM ("its ports reach nothing"). Let a role be
  declared once as a family (`lan: { count: { max: 4 } }`, named `lan0` … on the VM, the
  service's table matching `iifname "lan*"`) and an install name as many NICs of it as it has,
  `vm create --interface lan=…` repeated; a role an install names no NIC for then says nothing.

- **A NIC role's device on a Docker host, beyond `--interface`**: the forwarding and `tune`'s
  rules name the NIC `--interface ROLE=NIC` recorded; the `primary` role could default to the
  default route's NIC (said in the output), and `status` / `tune --check` could judge each role's
  device (it exists, is up, holds the default route for `primary`).
- **An upgrade adding a route on the VM, leftovers**: a new domain input cannot be given on the
  VM (neither the daemon nor the boot takes `--set`: `UpgradeRequest` would carry values to the
  boot); the daemon's check before the OS is written leaves out the kept-installer and L4 checks,
  which need the boot's OS.
- **A Docker host's gateway proven end to end**: `tune --apply`'s tables, forwarding chain,
  networkd files and boot unit are unit-tested; no suite applies them to a real Docker host (the
  gate's host is not one to change). A disposable Docker host (a VM of the Docker shape) running
  e2e-full's `tune --apply --dir`, a reboot, `status`, then `tune --revert`, would prove them.

- **A start-first start's engine calls**: each dependency of each start-first service costs four
  sequential calls (`ps`, `inspect`, `compose config --hash`, `image inspect`), repeated across
  services sharing one, and `startFirst` renders `compose config` a second time after `model`.
  One `config --hash` of every service and one batched inspect per start would do. The example
  api's drain still has a microsecond window (a connection completing between its last accept
  and the listener's shutdown is reset; the front retries a `GET` alone).
- **One engine snapshot per start**: each start-first service that reaches its overlap lists and
  inspects every container of the engine (`ps --all`, a batch `inspect`) to find one joining its
  old container; one snapshot per start would serve them all, and a batch that fails (a container
  removed meanwhile) is read again one container at a time, which a retry without the missing
  IDs would spare.
- **The networks record per service**: `.stowage-networks` digests every network of the project, so
  a change to a network a start-first service does not join replaces it stop-first all the same;
  a digest of each service's own networks would keep it overlapping.
- **Several NICs on a cloud**: `vm seed` and `vm create` refuse more than one `host.interfaces` NIC on AWS,
  Azure and Google Cloud, whose NICs' MACs are the cloud's (the seed cannot name them by MAC) and
  which need a subnet (AWS, Azure) or a VPC network (Google Cloud) for each. Naming them by their
  order on the bus (`Path=` in the `.link`), each cloud's root taking a list of subnets, once the
  test accounts can prove it.
- **`tune` judging a Docker host's mapped NIC**: `install --interface ROLE=NIC` records which NIC
  plays a role (for an ipvlan's parent); `tune` could judge that NIC's MTU and ethtool settings
  as it does the VM's, rather than advise them for any NIC.

- **The data partition encrypted with the TPM** (`shapes.kairos.encryption: tpm`): Kairos's LUKS of
  `COS_PERSISTENT` (`install.encrypted_partitions` from the installer, `kcrypt.encrypt_on_boot` on a disk
  image's first boot), its key in the TPM's NV memory. The VM gets a virtual TPM where the
  platform has one (TrueNAS `trusted_platform_module`, Proxmox's TPM state disk, libvirt's swtpm, a
  Google Cloud Shielded VM, Azure's Trusted Launch, AWS NitroTPM registered with `tpm_support`), metal
  needs TPM 2.0; a boot without the TPM halts (no plaintext fallback). What it protects, said: the key
  is not sealed to the boot's measurements, so a disk taken from its machine (a drive, a copied zvol or
  snapshot) is unreadable, the machine itself is not; sealing to the boot is Kairos's Trusted Boot
  (UKI, Secure Boot), another boot chain (systemd-boot, no `grub_options`): Won't for the first version.
  The OEM partition stays clear (the seed holds no secret but the one-time host key).

- **PM QoS under `host.power: performance`**: a held `/dev/cpu_dma_latency` request bounding the
  idle states where a cpuidle driver is bound. Needs a process holding it for the host's life
  (the host daemon on the VM; a unit of its own on a Docker host); the static cap is a kernel
  argument today (`intel_idle.max_cstate=`, `host.kernel_args`), which `tune` could advise.
- **`tune` advising the C-state cap and PCIe ASPM under `host.power: performance`**
  (`intel_idle.max_cstate=1`, `pcie_aspm.policy=performance` as `host.kernel_args`), with their
  cost stated: power and heat, and a fanless box held at its turbo frequency meets its limits
  sooner.
- **Ubuntu's HWE kernel on the OS bundle**: kairos-init installs `linux-image-generic` on 26.04,
  so the bundle stays on 7.0 (security fixes only). Once 26.04's first HWE kernel ships, a
  `host.kernel: ga | hwe` key (default `ga`) chooses it; the kernel stays refused in
  `host.packages`.

- **A driver's RDMA freed per port** (a NIC whose RDMA function holds its channels): blocking the
  RDMA module (`host.modules`) takes RDMA off every port of the host. A port that uses none could
  instead have its RDMA auxiliary device unbound only on `ethtool -L`'s EBUSY, logged, and again
  at each boot, keeping RDMA on the others; a device-level action, not a module's setting.

- **Kairos's CIS hardening on the VM**: kairos-init's `cisHardening` step is skipped (its shell
  umask 027, password ageing and faillock). Adopting it needs the install's writes to set their
  modes whatever the umask, and the operator account exempt from ageing (it has no password).

- **The compose model in a package of its own**: the installer imports `internal/lint` for
  `ServiceProfiles`, `ServiceDevices`, `CheckL4Address` and the `extends` resolution, linking the
  publisher's lint code (and `dockerfile`, `lock`, `digest`) into `stowage-install`.
- **Kubernetes schema check for a `command` schema-compat provider**: `shapes.kubernetes.schema`
  runs the `sql` provider's queries in a pod; a `command` provider's ops (JSON on stdin) are not
  run on Kubernetes, so its rollback is not checked there.
- **Docker restore drill of a machine made again**: e2e-docker restores into the install the
  backup came from; an install made with `--secrets` that then restores (`restore_awaited`) is
  unit-tested only on Docker.
- **The Docker disk-pressure check end to end**: `disk-check` and its timer are unit-tested; no
  suite runs the check against a filled volume on a Docker host.

- **`schema` on the VM and Kubernetes, `secrets set` and the one-pass `secrets rotate` on
  Kubernetes**: Docker's `schema` has no VM verb (the host daemon would run it, as it runs
  `exec`) nor `k8s schema` (a query pod, as the rollback check runs); `secrets set` has no `k8s`
  verb, and `k8s secrets rotate` takes a secret with `rotation: true` alone (the release's Secret
  — its secret files' one for an `as_file` secret — changed and its readers restarted, as its
  three passes do).

- **Cloud images, account identity**: an image's working directory is keyed by the cloud's
  settings, not by the account the credentials reach; two accounts on the same settings (a
  shared bucket and role name) share a kept id. Key it by the caller's account too (AWS caller
  identity, the Azure subscription). The GCP probe takes a READY image even when deprecated.

- **Disks with rootless Docker or Docker Desktop**: the disk builder (auroraboot, on the host
  network) reads the bundle from `ship`'s proxy on 127.0.0.1, which an engine in another network
  namespace does not reach. Serve the proxy where that engine reaches it (its bridge's gateway),
  with the same one-run account.

- **IPv6 on a cloud**: the VM asks DHCPv6 at once (`WithoutRA=solicit`), the canonical way AWS,
  Azure and GCP hand out IPv6 addresses, proven on a LAN's router advertisements and DHCPv6 only
  (e2e-vm); untested on a cloud (no cloud account). The IPv6 client an allowlist is checked against
  is the host's own global address (smoke-install), which crosses Docker's DNAT as an outside one
  does; a host without a global IPv6 address skips the check.

- **VM: move an install to another channel carrying its release**: `upgrade --channel C` refuses
  when C's release is the one the OS runs ("no upgrade on C") — the normal case after `promote`,
  or a release shipped to two channels from one commit (identical digests). The install keeps
  its recorded channel, so `status`, the host daemon's update check and a plain `upgrade` read a
  channel that may since have been pruned, and only an explicit `--channel` finds the next
  release. Same release (version, revision, digests) on C: record C, write no OS, no reboot.

- **Kubernetes: reconfigure a release's inputs without a new release**: `k8s upgrade` keeps every
  recorded input, so changing one (an extension's endpoint set after the install) needs the
  Secret edited by hand. Design lessons from an attempt: an immutable values Secret per revision,
  the deployed revision's values first, an explicit choice for a release-less install, no
  inferred stamps; the interplay with secret rotation (a rolled-back pass, generated values a
  later revision added) is the hard part.
- **The chart's Service of the endpoint**: `<short>-stow k8s` prints
  `http://<release>-<service>.<namespace>.svc:<port>` from the endpoint route's upstream, the
  chart's contract; nothing checks the chart keeps it. Lint could render the chart and warn when
  it has no Service `<release>-<service>` on that port.
- **`k8s provision` from another release's installer**: it is refused unless the installer is the
  release's that runs; after a failed `k8s upgrade --version` the operator fetches that release's
  installer (or uses the one the upgrade kept). Fetching and running it, as `upgrade --version`
  does, would spare that step.
- **Shaping the Kubernetes hook's pod**: the Job's pod sets no resource requests or limits, node
  selector or tolerations, so a namespace whose ResourceQuota requires requests, or nodes all
  tainted, refuse or never schedule it (the run fails after 2 minutes, naming why). A chart value
  or the hook's ConfigMap giving those fields would cover them.
- **The Kubernetes hook's stderr while it runs**: the Job's logs are copied to the installer's
  output when it ends; following them live would show a long hook's progress.
- **The front holds a long request with a body**: Caddy 2.11.6 cut the response to any request
  carrying a body at 60 s (the front's Dockerfile refuses that version by comment only). An e2e
  case holding a POST's streamed answer past 60 s through the front, on every shape with a front,
  would catch a regressed Caddy release before a project's long streams do.
- **Encrypted DNS upstreams for the VM's network override**: `network set` writes resolvers in the
  clear. DNS-over-TLS is a `resolved.conf.d` drop-in (`DNSOverTLS=` off / opportunistic / strict,
  per-server hostname pins, `UseDNS=false` so DHCP cannot bypass it); strict fails closed — a VM
  that cannot reach its resolver over TLS resolves nothing — so the verb and the console should
  say so. DNS-over-HTTPS would need a resolver daemon in the OS bundle; only on demand.
- **A hypervisor-side watchdog for the VM**: the health gate covers a bad boot, not a reboot that
  never completes (a wedge before GRUB or userspace). A watchdog on the hypervisor that power-cycles
  a VM on a missed heartbeat would; on TrueNAS it cannot use the QEMU guest agent (`vm.device.create`
  offers no channel/serial dtype), so it would read the VM power state the host already sees.

- **The `cnpg` Postgres mode on Kubernetes** (`postgres_modes` validates it; DESIGN § 7 row 7).
  The mode is the project's chart's, and Stowage has no code for it: `examples/full`'s chart
  proves `embedded` and `external` (`make e2e-full-k8s`) but has no CloudNativePG `Cluster`, which
  needs the operator in the kind cluster. Also open: `k8s reset-db` deletes the claims of the
  release's StatefulSets, not those an operator's resource makes, so it would not wipe a cnpg
  database.
- **Reconfigure beyond the re-run**: on a terminal, a re-run asks nothing; a form pre-filled with
  the recorded values would let an operator change inputs without naming them. A re-run also keeps
  the loopback address, the registry and its CA, and the channel (TLS: done); changing them
  needs a new install today. Rotating the pull password is its variable at the next run.
- **Sealed secrets, opt-in per deployment**: `<short>-stow answers seal` generates the generated
  secrets and asks for the supplied ones, and encrypts them all with the sops library to the
  operator's age key plus a per-deployment age key (or a KMS); `install --answers x.sops.yaml`
  then takes every secret from the file instead of generating it. Docker: the deployment key and
  the payload next to the install, decrypted at each start. Kubernetes: `<short>-stow` decrypts and
  writes the Secret (an External Secrets Operator source later). VM: cloud-config carries the
  payload and the deployment key (never the operator's), the seed is removed after the first
  boot (the OpenTofu root detaches it; the manual steps say so; cloud steps block containers from the
  metadata service), the payload is decrypted at each boot into `/run`. KMS seal flags and a real
  KMS test come with it or after.
- **`gen` fallback on a secret input**: `{ key: K, prompt: …, secret: true, gen: pass18 }` asks,
  and generates when the answer is left empty (lets an operator keep an old value, as in a
  disaster recovery).
- **Kairos at-rest encryption**: the persistent partition encrypted with a TPM-sealed key
  (Kairos kcrypt), or the sealed secrets' deployment key sealed by the TPM; later a key server or
  a cloud KMS releasing it at boot after attestation. The boot flow stays the same.
- **Hook ship with submodules**: refused today (a checkout has no submodule content). Fetch them
  into the checkout from the repository's own `.git/modules` (no network, no change to the
  repository's configuration).

- **The lock pins a version label, not bytes**: a `stowage` built between two of its releases
  reports the top CHANGELOG version, so it passes the pin with another installer or front. An
  immutable channel still refuses those changed inputs under an existing tag; pinning the
  embedded installer's and front recipe's digests too would close it on mutable channels.

- **Offline archives across image stores**: an archive `docker save`d on a classic-store host and
  loaded on a containerd-store host gets a regenerated manifest digest the release does not record,
  so the right image is refused (the README tells operators to keep the stores alike). Compare the
  loaded image's layer diff IDs with the release's config instead.

- **A mirror's CA changes**: `--registry-ca` is recorded at the first install; a mirror that
  moves to another CA needs `<dir>/registry-ca.pem` and the Docker daemon's `ca.crt` replaced by
  hand (and the VM's cloud-config). A `<short>-stow registry-ca FILE` verb would replace both.
- **The Docker daemon's CA file is not exercised end to end**: the e2e mirror is on the loopback,
  which the daemon takes as insecure, and the run is not root, so `/etc/docker/certs.d` is
  covered by unit tests only (a directory variable). A root e2e with a non-loopback mirror
  address would cover it.
- **A test release on another name of the project's registry**: `--test-registry` is compared
  by its spelling, folded (case, dots, ports, loopback, Docker Hub); another DNS name or address
  of the same registry is not told. Before publishing, a test release of a signing project could
  read the test prefix's release repository and refuse when a release there verifies with the
  project's own keys; meanwhile README asks for an account that cannot write the project's
  repositories.

- **Installers for more platforms**: `<short>-stow` is linux/amd64, so a Kubernetes install (and
  the OpenTofu provider's Kubernetes target) runs from a Linux amd64 machine only: no macOS
  runner, a common place to run OpenTofu. Build darwin/arm64, darwin/amd64 and linux/arm64
  installers, the payload appended to each; needed before the provider's Kubernetes target ships.

## Could

- **Optional OS add-ons as signed extensions**: a large add-on only some machines need (a GPU
  driver stack, a vendor's firmware tools on bare metal) rebuilds and ships with every OS bundle
  today (`host.packages`). A systemd system extension published through the registry beside the
  bundle — by digest, signed, its SBOM audited, enabled per boot entry and rolled back with it —
  would let it ship once; the bundle masks systemd-sysext and systemd-confext until then (DESIGN,
  "No system extension on the VM"). Kairos's own tooling covers none of those guarantees on a
  GRUB install: it checks an extension's signature only under Trusted Boot (UKI), checks nothing
  when `kairos-agent sysext install` downloads one, and keeps its profiles (`common/` loaded on
  every boot) across an upgrade and a rollback, so Stowage would verify and enable them itself.
- **A front route's upstream exposed on Kubernetes past the chart's ingress**: lint warns of a
  guarded route while the chart has routing resources (an Ingress, a Gateway API route, a
  controller's or a mesh's); a chart exposing a guarded route's upstream another way — a Service
  of type `NodePort` or `LoadBalancer` or with `externalIPs`, a pod's `hostPort` or
  `hostNetwork`, a Knative Service, a manifest a template writes whole from values — is not
  warned. Telling those from the L4 routes' own NodePort Services (built from
  `.Values.stowage.l4`) needs that range expanded from the spec, and a written manifest needs
  rendering; a chart doing so departs from the chart contract (front routes through its ingress).
- **A `network reset` retried without end while the seed does not read**: a reset whose apply
  fails while the cloud-config's network files do not read has its restore of the settings
  before it tried again every few seconds, never given up; a restore that stops and says what to
  restore would close it.
- **The re-pin and a NIC added for another role**: when the pinned NIC is removed and a NIC for
  another role added in the same power cycle, before `network attach` records it, the boot may
  pin that NIC as the role's; a re-pin that waits for the attach (or asks) would close it.
- **The attached role's reboot hint after a boot**: `network set --role` on an attached role whose
  NIC is there says `reboot`, even when a boot has run since the attach and did not name it; a
  record of the boot that applied the attach would tell the two apart.

- **The lifecycle lock read from a paged table**: the host daemon tells a held lifecycle lock (the
  service watch, the upgrade before a first install) from `/proc/locks`, which the kernel serves a
  page at a time; a held entry can fall between two pages while other locks come and go across
  that boundary. Three reads make it rare, not impossible. A holder record the daemon can check
  (or a lock type with a test-without-taking call that older installers' handed locks honour)
  would close it.
- **A reboot the daemon's own stop cut**: the host daemon counts a `systemctl reboot` ended by a
  signal as the system going down; one its own context killed (`systemctl restart <name>-hostd`
  while a scheduled reboot's `systemctl` runs, or Ctrl-C on the installer's switch) is counted
  the same, so that reboot is journaled as coming and not given back. Telling the shutdown's
  SIGTERM from the context's kill (a distinct cancel signal) would close it.
- **Two app calls on one set at once**: under `flush_conntrack`, the host daemon lists a set before
  and after each `POST /v1/nftables/<table>/sets/<set>` and ends the flows of the difference;
  nothing serialises two calls on one set (or an element timing out between the listings), so
  each may end flows of the other's elements and count them. A lock per set around the change
  and its listings would close it.
- **The VM's upgrade check between unreleased builds**: the host daemon judges an upgrade's values
  only for a target published by its own Stowage version; two unreleased builds of one top
  CHANGELOG entry share it, so one may judge by the other's rules (the boot judges again, and
  falls back). Comparing the build's commit as well would close it.
- **A command cut off while Docker is still creating its container**: a `command` provider's run
  past its timeout is removed by name (`docker rm --force`); on a daemon slow enough that the
  create is still in flight, the removal finds no container, the create then lands, and the
  container stays (`Created`) — the next lifecycle run refuses it, naming `docker rm --force`
  (seen once in smoke-install under load, a rollback's schema check). The timeout's message also
  leaves out a removal that failed. Rare; a removal confirmed over a settle window, or the next
  run removing a schema check's container that is not running, would close it.

- **A file input swept while a command mounts it**: a `command` provider's run checks that a file
  input's file exists and hands its path to Compose for a bind mount, with no lock against a
  concurrent re-run's sweep of the inputs' files; the host daemon's health probe can race a
  re-run that changes that input, and Docker then creates a root-owned directory at the missing
  path. Rare (a re-run changing a file input during a probe); a lock the sweep and the command's
  mount share, or a mount from a per-run copy, would close it.

- **Cloud resume without a second transfer**: a run stopped once its snapshot was imported
  (Scaleway, AWS) downloads and uploads the disk again to make the image, which never reads that
  upload. Skip the upload module when the image module's state holds the imported snapshot.

- **A bound on an L4 TLS route's connections** (`max_connections`), for an upstream that does not
  bound its own: caddy-l4 (v0.1.2) has no concurrency limit, only `throttle` (bandwidth); a
  handler of its own, or an upstream release that adds one.

- **An L4 TLS route's domain names the front on the stack's network too**: a front route's domain
  is an alias of the front (`routeAliases`), so a service dialing it reaches the front; an
  `l4.routes[].tls` domain is not, so a service of the stack dialing `<domain>:<port>` leaves
  through the host's public address (hairpin NAT) rather than straight to the front.


- **A pre-commit hook running `lint`**: `hook install` writes git's post-commit hook alone; a
  project wanting `lint`'s allowance checks (an artifact the release cannot carry, an entry
  expired or ending within 30 days) before a version bump is committed adds its own pre-commit
  hook running `stowage lint`. `hook install --pre-commit` could write it.

- **networkd's router solicitations, upstream**: networkd (systemd v259, `sd-ndisc.c`,
  `ndisc_handle_router`) stops soliciting on any router's advertisement, a lifetime-0 one
  included, where RFC 4861 § 6.3.7 desists only on a non-zero lifetime; Stowage solicits itself
  (`route6-boot`, the daemon's watch). Report it to systemd, and drop the workaround once a fixed
  networkd ships in the bundle. e2e-vm proves the daemon brings back a dropped advertised route; a
  test link with a lifetime-0 router answering first and the default router's solicited answer
  dropped (the field case) is not built.

- **A decline proven in e2e-full**: the overlap reads the new container itself (never Compose's
  `--wait`), a decline under a restart policy included, unit-tested against the engine's calls; a
  real decline in e2e-full (an environment flag on the example's api) would prove it against
  Docker.

- **Images from packages** (a project hand-writes the Dockerfile stage today, which works): a project that ships Debian packages — software its
  operators install on a host, whose stack runs the same packages one service per container —
  hand-writes a Dockerfile today that repeats what Stowage could own, and gets it subtly wrong:
  the distribution base pinned again beside the project's other pins (two pins drift), the
  packages of exactly the release picked out of a directory holding several versions (a lexical
  "latest" glob installs an older build without a word), maintainer scripts that try to start
  services in a build, and the per-install files a postinst generates (a CA, a default
  configuration) left in an image every install then shares. Debian and Ubuntu bases (`dpkg`,
  `apt`); another family is refused by lint. `images.<k>` built from packages, linted, signed
  and SBOM'd as any image:
  - `base`: the distribution image, tag and digest (lint: pinned; the release records it);
  - `apt`: the distribution's packages the image needs beside them (Debian's name rule);
  - `packages`: the project's `.deb` files, each a pattern relative to the project with
    `{version}` (the release's version, an optional Debian revision `-N` after it) and `{arch}`
    (the build platform's Debian architecture, `amd64`/`arm64`), each `required: true` or not;
    exactly one file per pattern (none: a required one fails `ship` naming the pattern and the
    files it saw; several: refused, naming them); installed together with `apt` so their
    dependencies come from the base's repositories, with a `policy-rc.d` refusing every service
    start for the build's length (removed after); the files are the image's inputs, so a new build
    of a package is a new image;
  - `files`: project files and directories copied in (source → absolute path; a directory whole;
    a glob into a directory), as a stack's renderers, templates and entrypoint are;
  - `ca_certificates`: project PEM files (a glob) added to the image's trust store
    (`update-ca-certificates`);
  - `purge`: absolute paths (globs allowed) removed after the install — the files maintainer
    scripts generated per install; a path that removed nothing warns at build (a stale list),
    and `/`, a top-level directory or a whole `/usr`, `/etc`, `/var` are refused;
  - `entrypoint` / `cmd`: as a Dockerfile's.
  The second form builds the packages first: `packages: { image: <key>, path: <dir in it> }`, where
  `<key>` is a Dockerfile image of the project marked build-only (never published, no SBOM of its
  own) whose target writes the `.deb`s at `path` — `ship` alone then builds the release, the
  build-only image's `add_hosts` and `args` applying to its build. The packages' systemd units
  stay the project's to turn into services (an entrypoint per unit). The SBOM lists the packages
  (syft), and a package's own CycloneDX documents under `/usr/share/sbom` merge in as an image's
  do.
- **What packages install on a host, taken from them** (a project renders its table at development time and feeds its sets over the app socket today, which works): a package does more than its files in a
  container — it installs what only a host applies, and a stack built from it repeats each piece
  by hand in the spec, a second declaration that drifts from the package's. Three kinds:
  - **A service table generated by its image**: `services.<name>.nftables` as `{ command: [...] }`
    besides a file — run in the service's image at install (and at each start whose inputs
    changed), the stack's inputs in its environment, its output the one `table inet <name>` lint
    and the loader judge today (refused as a file's would be, the previous table kept). A package
    shipping its ruleset as a generator (rendered from its own declaration and its operator's
    settings) then has one source: no table copied into the project, no drift, and a setting that
    shapes the table is an ordinary input, changed by a re-run.
  - **Static host files read from the packages**: `usr/lib/sysctl.d`, `etc/sysctl.d` →
    `host.sysctls`; `modules-load.d`, `modprobe.d` → `host.modules` (`load`, `options`,
    `blacklist` → `block`); `udev/rules.d` → the OS bundle and `tune`'s advice; each with the
    package and file as its `reason`, the spec's own key winning (said by lint), a tune profile's
    key refused as today.
  - **The rest reported**: systemd units, timers and sockets (and their `AmbientCapabilities`,
    `User=`), `tmpfiles.d`, AppArmor profiles, cron and logrotate files — what no container
    applies — listed by `lint` and `ship` for each image from packages, with the services and
    keys the project mapped them to (`# stowage: <unit> -> <service>`, or a spec list), so none is
    lost without a word.

- **Debian 13 as the OS bundle's base**: the build takes Ubuntu alone. Debian is the one cheap
  alternative (apt, the same package names and purge, glibc, Docker's repository), but it ships
  6.12 (a modern kernel only from backports). Choosing Docker's repository from
  `/etc/os-release`, and a Debian bundle through e2e-kairos and e2e-vm.
- **Hadron as the OS bundle's base**: Kairos's own minimal distribution (musl, no package
  manager). Its way to add software, system extensions built with its musl toolchain, changes how
  a package ships, not where it comes from: Docker, containerd, runc and nftables would still be
  builds from source Stowage maintains (no official extension exists for any of them), and
  `host.packages` would have no meaning. Revisit once Hadron publishes SBOMs (today `audit` would
  see next to nothing of the OS) and ships nftables and a container runtime — in the image or as
  official signed extensions, with Kairos enforcing an extension's signature on a GRUB install
  (today on Trusted Boot alone) and tying an enabled extension to its boot entry, so a rollback
  takes it back.

- **HSTS `includeSubDomains` opt-out** (security review, Low): the front sends it on every route,
  as the reference deployment does, binding the names under a route's to HTTPS for a year. A spec
  key (`front.hsts_subdomains: false`) for an operator whose route name has other services
  under it.

- **`k8s install --unsafe-sysctls` on a kubelet that refuses them**: the pods fail at once
  (`SysctlForbidden`), yet helm waits out `--timeout` before it rolls back. Failing early needs to
  tell this deploy's pods from any other (a label contract the charts keep, which lint cannot
  judge from template text reliably) and helm rolling back when interrupted; a watch tried on
  label selection kept missing pods Pod Security never admitted.

- **A Docker host's NIC rings on the `network` profile**: a resize resets the link, so `tune`
  never resizes an up link and reports it as advice with the `ethtool -G` to run while it is
  down; a host's own boot does not grow them either. A unit ordered before the network (as the
  VM's) would.

- **Ports the network profile's local range may take**: the host reserves the front's and the
  L4 routes' ports, not a port a service publishes itself (`ports:`, on loopback too): the start
  knows them from the compose model, but `tune` and the VM's boot read no model; recording the
  published host ports at each start (a `.env` key) would let them be reserved.

- **One pass over the stack's networks before `up`**: the IPv6 check and the MTU check each list
  the networks and may each stop the stack; one listing, judging both, would stop it once.

- **A Docker host's interrupts (`network` profile) and CPU posture (`host.power: performance`)**:
  `tune --apply` sets them on the running host only, so the host's reboot loses them until the next apply (the VM's
  boot sets them each time). A unit ordered after the NICs' probe, written by `--apply`, would
  keep them.

- **Retired release tags stay**: on a registry that deletes no tag alone, a tag `prune` retired
  (a release's, an image's, an installer's) is never visited again, even once the registry deletes
  tags alone; each costs a read in every release listing. Only floating tags are deleted then.
- **`compose config` on every start**: a start reads the stack as Compose runs it for the content
  labels and the new volumes, even for a stack with no inline config, no environment-sourced
  secret and no volume of a `uid`, where it finds nothing; skip it there.
- **No front as one install mode**: a project without a front is a set of `HasFront` guards across
  the installer (preflight, decide, renderStack, up, openPorts, reconfig, upgrade, vm create, the
  CLI's flags); one front mode with no ports, Caddyfile or image would keep a new path from
  forgetting its guard.
- **A mirror import's blobs in chunks too**: `mirror import` pushes each blob in one stream, as
  a ship pushed disks before; a disk set imported over a lossy link restarts from zero. Push the
  archive's large blobs as a ship pushes a disk.
- **A volume quiesced for its backup**: `backup.volumes` are read while the stack runs,
  which suits material written once; a service writing a volume continuously would need its own
  pause (a hook around the archive).
- Not built for non-HTTP stacks: x509/key generators (product PKI; DR is covered by volume
  backup), a pre-start hook.

- **A compressed disk's file size**: a disk's layer is annotated with its file's digest, not its
  size, so a different file of that name in `vm create`'s `--out` is hashed whole before it is
  refused, and the download's progress counts compressed bytes. Annotate the size too.

- **IPv6 beyond the switch**: the VM suite against an IPv6-only TrueNAS or mirror (`test/vm.sh`
  reads IPv4 addresses and routes); `test/sweep.sh` sweeping `[::1]` registries; a lint warning on
  a project network declaring `enable_ipv6` (`--ipv6` owns the default network's); Docker on the
  VM pulling from an IPv6-literal mirror, checked on the pinned docker-ce; `network` leaving out
  temporary IPv6 addresses.

- **The host's installer installs an extension itself**: fetch the extension's installer, install
  it beside the host (the same compose network, namespace or VM) and set the host's input from the
  endpoint it recorded. Today the operator runs the extension's installer and gives the endpoint
  (the tip a host install ends with names both steps).
- **An extension's loopback endpoint on Docker**: in loopback mode the endpoint printed is
  `http://127.0.0.1:<port>`, which another compose project's containers cannot reach (two
  projects never share a network); a host on the same machine needs the extension behind a TLS
  front, or the host's address.
- **A verb deleting the backups of a dropped profile**: retention never prunes the backups of a
  profile the release no longer declares (`backup list` marks them); the operator deletes them
  with the destination's own tools. `backup delete <name>` (the dump, or the manifest, with its
  volume archives) would do it through the daemon's rclone config.

- **`upgrade --reset-db`**: an upgrade that wipes the data in the same run — the way across a
  schema baseline, which today takes a fresh install (on the VM, in the boot that upgrades, judged
  with `health_gate.deadline_after_reset_s`, validated only today). A review of one attempt
  found: the confirmation must list every volume either release's `data.reset` names; a run
  stopped between the `.env` switch and the wipe leaves the new release recorded on old data
  (the hints and `rollback` must know); the recovering `reset-db` must wipe the old release's
  volumes too.
- **A helm release's schema baseline in its chart values**: `k8s upgrade` fetches the installer of
  the release the helm release runs only to read its baseline; recorded in the chart's values, it
  would be read without that fetch.
- **Reset extras**: a `seed` provider (a project's dataset loaded after a reset, beyond what the
  provision hook creates); `keep_on_reset` on Kubernetes (today every claim of the release goes);
  a database outside the Kubernetes release (`external`) is not reset.

- **What `test/sweep.sh` does not sweep**: a TrueNAS VM `stowage-e2e-<run>` whose run died
  without its cleanup (a host crash; its destroy needs the run's state, gone with it), and the
  build cache, which lives in the host's shared buildx builder.

- **A disk that fails rebuilds the bundle on the next ship**: the bundle was pushed by digest,
  untagged, and nothing records it. Recording built digests (a staging tag) would keep it.

- **Kept release's front rendered by the newer Stowage**: an upgrade renders the kept release's
  front files with its own renderer (from the kept release's spec and `.env`), so a rollback runs
  them as this Stowage writes them, not as the older one did. Equal while the front's format does
  not change; render through the kept installer when it does.

- **Provision hook networks**: the hook joins the project's `default` network only; a stack whose
  services live on named networks only would need `hooks.provision` to name one.
- **Charm UI everywhere in the CLIs** (`stowage` and `<short>-stow`, every shape): lipgloss styles and
  layout for every message, status and error; bubbles spinners and progress for builds, pushes,
  pulls, disk images and waits (a ship's steps as a live checklist); tables for `plan`,
  `releases`, `status` and `verify`; huh for every prompt. Plain, uncoloured output when not on a
  terminal, under `--json` and in accessible mode. Docker's own progress output streams as is.
- **Accessible mode: back to a default** after "Change an answer" (Enter keeps the answer
  given; the full form lets the operator clear it).

- **Yank freshness**: a yank writes a new signed manifest, but the earlier, un-yanked one stays
  validly signed, so someone with registry write access and without the key can re-tag it back.
  Needs a signed, expiring channel record (TUF-style timestamp) that installs check.
- **Fewer attestation reads**: `ship`'s check that an artifact carries its SBOM reads and verifies
  the whole attestation (an OS bundle's runs to MiB): read no predicate where presence alone
  matters. (Signature checks already pass over the referrers cosign annotates with another
  predicate; an unannotated SBOM bundle is still read up to 4 MiB.)
- **Unsigned revisions slow `upgrade --version`**: an unsigned revision of V with unreadable
  signatures stops `upgrade --version` (fail closed), and V's revisions are checked one at a time,
  newest first, so many unsigned revisions make it slow: check them concurrently (bounded).
- `ship` makes the installer image again on every run (payload, tar and gzip in memory, about
  three copies of the executable) only to compare its digest with the published one, and twice
  on a mutable re-push whose installer changed: stream it, or hash without compressing first.
- `ship` checks signatures twice per artifact, and before signing a digest it just pushed:
  skip the pre-check for fresh digests.
- `docker save` writes gzip layers that `ship` decompresses and recompresses: export as OCI and
  push the layers as they are.
- `releases` and pointer fallback read release manifests one at a time, and the fallback checks
  each candidate's signatures one at a time: read and check concurrently
  (bounded), and stop at a candidate's first missing signature.
- `upgrade --version` reads every release manifest of the repository (8 at a time) to find one
  version: the channel's tag template, carried in the payload, would bound the reads to V's tags.
- `planPinned` scans every source release for each pinned image: index the sources once.
- `ship` reads the changelog section twice (graph and manifest): pass it through.

- **No suite runs a failing start-first successor**: the quoting of its last lines is unit-tested
  against the engine's calls; an e2e case (a release whose new container exits) would prove it
  against Docker's own `logs`.

## Won't — not in the first version

- Taking over an install made without Stowage: a project adopting Stowage installs fresh.
- A route's login page (a form, a session cookie, a sign-out, a lockout): a route's login is
  `basic_auth`, the browser's own prompt.

- The other extensions of DESIGN § 10 (restore verb on Kubernetes, Kubernetes provision hook +
  portal URL): none exists today; revisit after parity. The Docker backups and restore, the
  Kubernetes barrier pre-flight are done.
- Secret rotation (DESIGN § 7 row 24, an application concern).
