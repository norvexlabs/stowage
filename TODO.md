# TODO

Pending work, prioritised MoSCoW for the **first version**. Its goal is parity: Stowage does what
an existing production deployment of this kind does today on its three shapes — nothing more.
A finished item is deleted, not ticked. The plan behind it is
[docs/DESIGN.md § 11](docs/DESIGN.md#11-phases).

## Must — parity

1. **Cloud images on AWS and Azure**: `ship` registers the `cloud_publish` images through
   OpenTofu, run for real on Google Cloud only (`STOWAGE_CLOUD_ENV=<file> make e2e-kairos`). Run it
   with the AWS and Azure test accounts once they exist, and fix what they find. Azure, known:
   its probe takes an image of the name whatever its provisioning state (its resource list gives
   none), and a page blob left by an earlier upload makes the next upload of that name fail
   (`azurerm_storage_blob` refuses an existing blob): check both there, and that the service
   principal needs *Storage Blob Data Contributor* on the storage account (blobs are written with
   Entra ID).

2. **Scaleway**: a platform of `vm create` (OpenTofu module: an instance from the release's
   disk, the cloud-config as its user data) and a cloud of `cloud_publish` (the disk uploaded to
   Object Storage and imported as an instance image, with the publisher's credentials from the
   environment), as the other clouds. Before writing a module, read the latest
   `scaleway/scaleway` provider's official docs and its schema (`tofu providers schema -json`),
   and pin that release.
3. **OVHcloud**: the same for OVHcloud Public Cloud (OpenStack): `vm create` and `cloud_publish`
   (the disk uploaded as a Glance image of the project, `ovh/ovh` and/or
   `terraform-provider-openstack/openstack`, whichever the latest official docs make canonical).
   Before writing a module, read the latest provider's official docs and its schema, and pin that
   release.

## Should

- **A prune on a registry that deletes no tag alone reads every tag per refused delete**: `untag`
  and the sweep after it list the repository and read each tag for each tag they remove, so a
  prune of many releases makes a quadratic number of requests and may meet the registry's rate
  limit. One tag-to-digest snapshot per repository per prune would do.
- **The compose model in a package of its own**: the installer imports `internal/lint` for
  `ServiceProfiles`, `ServiceDevices`, `CheckL4Address` and the `extends` resolution, linking the
  publisher's lint code (and `dockerfile`, `lock`, `digest`) into `stowage-install`.
- **Kubernetes schema check for a `command` schema-compat provider**: `shapes.kubernetes.schema`
  runs the `sql` provider's queries in a pod; a `command` provider's ops (JSON on stdin) are not
  run on Kubernetes, so its rollback is not checked there.
- **Docker restore drill of a machine made again**: e2e-docker restores into the install the
  backup came from; an install made with `--secrets` that then restores (`restore_awaited`) is
  unit-tested only on Docker.
- **Untag on a registry deleting no tag alone, at scale**: `untag` lists and reads every tag of
  the repository to find the others on a digest, once per tag; `prune` on ghcr does it for each
  image, installer, chart, bundle and release tag. Read each repository's tags once per prune.
- **One rule for a boot set's stowage**: which stowage published a release's boot set (its own
  disks' or `boot_disks`') is written in the publisher (`bootSet`) and the installer (`vm create`);
  one helper beside `release.Artifacts.Boot`.
- **A killed read leaves an rclone config**: `backup list` on Docker (and on the VM, under the
  daemon's runtime directory) writes the destination's rclone config to a temporary directory
  removed once read; a SIGKILL leaves it. Sweep stale ones.
- **The Docker disk-pressure check end to end**: `disk-check` and its timer are unit-tested; no
  suite runs the check against a filled volume on a Docker host.

- **`exec` and `schema` on the VM and Kubernetes**: Docker's `exec SERVICE -- CMD` and `schema`
  have no VM verb (the host daemon would run them, as it runs `logs`) nor `k8s schema` (a query
  pod, as the rollback check runs).

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

- **A renamed installer's refusal on `rollback` and the VM's boot**: an install refuses a release
  whose `short_name` differs from the one it records (`ship` refuses publishing one); the re-run
  and `upgrade` paths have a test, `rollback` and `boot` none.

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
- **A `command` provider receives only the keys it declares**: the schema-compat and health
  `command` providers' containers still receive every input, secret and safe default (as the
  provision hook did before its `env`). An `env` list on `command`, as on the hook, would narrow
  them; their images are usually the stack's own, which is why it can wait.
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

- **Disaster recovery on Kubernetes**: `secrets export` and `install --secrets` run on Docker and
  the VM; `k8s secrets export` (from the release's Secret) and `k8s install --secrets` would
  follow the same file format.
- **The `cnpg` Postgres mode on Kubernetes** (`postgres_modes` validates it; DESIGN § 7 row 7).
  The mode is the project's chart's, and Stowage has no code for it: `examples/full`'s chart
  proves `embedded` and `external` (`make e2e-full-k8s`) but has no CloudNativePG `Cluster`, which
  needs the operator in the kind cluster. Also open: `k8s reset-db` deletes the claims of the
  release's StatefulSets, not those an operator's resource makes, so it would not wipe a cnpg
  database.
- **An image vanishes from the engine while suites run in parallel** (nine times on 2026-10-11 and
  2026-10-13; each suite passed alone). The installer now pulls with the project's account at
  `compose up` and at the provision hook's run too, so a lost image is pulled again instead of
  failing anonymously: the symptom is gone, the cause is not found. Every lost image had the same
  content as one another suite held under its own registry's name. Not reproduced by two loops on
  this engine (containerd image store): removing one name, by tag, by digest or forced, while
  another is pulled or kept; nor by a build's temporary tag removed beside a pulled copy. Again on
  2026-10-13: e2e-docker's install from its mirror lost `notes-api` between Compose's image check
  and the container's creation, beside e2e (which ships the same content): the installer's `up`
  now pulls a gone image again and starts once more. Next clue: `docker image ls --digests` and
  `docker events --filter type=image` across a parallel gate.
- **Reconfigure beyond the re-run**: on a terminal, a re-run asks nothing; a form pre-filled with
  the recorded values would let an operator change inputs without naming them. A re-run also keeps
  the loopback address, the registry and its CA, and the channel (TLS: done, the VM's in Must 4); changing them
  needs a new install today. Rotating the pull password is its variable at the next run.
- **`make smoke-install` leaves two stacks behind**: after a passing run, the
  `stowage-smoke-notes` and `stowage-smoke-notes-internal` stacks (their front containers) and the
  test's temporary directory under `/tmp` remain, while `stowage-smoke-notes-byo` is taken down;
  `test/sweep.sh` keeps them, as their working directory still exists. Find why those two
  cleanups do not run, and let the sweep remove the smoke test's projects whose test process is
  gone.
- **Sealed secrets, opt-in per deployment**: `<short>-stow answers seal` generates the generated
  secrets and asks for the supplied ones, and encrypts them all with the sops library to the
  operator's age key plus a per-deployment age key (or a KMS); `install --answers x.sops.yaml`
  then takes every secret from the file instead of generating it. Docker: the deployment key and
  the payload next to the install, decrypted at each start. Kubernetes: `<short>-stow` decrypts and
  writes the Secret (an External Secrets Operator source later). VM: cloud-config carries the
  payload and the deployment key (never the operator's), the seed is removed after the first
  boot (OpenTofu detaches it; the manual steps say so; cloud steps block containers from the
  metadata service), the payload is decrypted at each boot into `/run`. KMS seal flags and a real
  KMS test come with it or after.
- **`gen` fallback on a secret input**: `{ key: K, prompt: …, secret: true, gen: pass18 }` asks,
  and generates when the answer is left empty (lets an operator keep an old value, as in a
  disaster recovery).
- **Kairos at-rest encryption**: the persistent partition encrypted with a TPM-sealed key
  (Kairos kcrypt), or the sealed secrets' deployment key sealed by the TPM; later a key server or
  a cloud KMS releasing it at boot after attestation. The boot flow stays the same.
- **Secrets as files (opt-in)**: deliver secrets as `/run/secrets/<KEY>` (compose `secrets:`, a
  mounted Secret on Kubernetes) instead of environment variables, which leak into `docker
  inspect`, crash dumps and child processes.
- **Hook ship with submodules**: refused today (a checkout has no submodule content). Fetch them
  into the checkout from the repository's own `.git/modules` (no network, no change to the
  repository's configuration).
- **Installer: test the `.env` write failure path** — the cleanup of a byo `tls/` copy and of the
  lock file when `.env` cannot be written (into a directory the run did not create) has no test;
  `.env` writes through `os.CreateTemp` with no failure seam.

- **`stowage.requires`**: refuse to run outside the project's accepted Stowage range (today only
  a missing range is reported); `stowage lock` and lint check the pin against it too.
- **The lock pins a version label, not bytes**: a `stowage` built between two of its releases
  reports the top CHANGELOG version, so it passes the pin with another installer or front. An
  immutable channel still refuses those changed inputs under an existing tag; pinning the
  embedded installer's and front recipe's digests too would close it on mutable channels.
- **`mirror import` checks the archive's signatures** with the importing `<short>-stow`'s keys
  (read from the archive's referrers, offline) before anything is pushed; today an install
  checks the release only when it reads it from the mirror.

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
- **Mirror imports at once**: the check that a mirror's channel never goes back reads the channel,
  then pushes: two imports run together can still leave it on the earlier one. Likewise two
  first installs against one mirror: one that fails removes the daemon's `ca.crt` the other kept.
- **A yank reaching the mirror**: an export carries the channel's release, not the yanked one's
  manifest, so the mirror keeps the yanked release's tag unmarked (`--older` moves its channel
  back). Export the yanked manifests of the channel too.
- **A VM reading a mirror that needs a login**: the VM's reads of a mirror use root's Docker login
  on the VM (`sudo docker login` over SSH); the e2e mirror allows anonymous reads. Carry the site's
  pull account (a secret, so over SSH, as other secrets) when a mirror refuses anonymous reads.

## Could

- **`k8s install --unsafe-sysctls` on a kubelet that refuses them**: the pods fail at once
  (`SysctlForbidden`), yet helm waits out `--timeout` before it rolls back. Watch the rollout's
  pods and fail on that reason early (a namespace's Pod Security label is checked before
  anything changes already; the cluster's default level, without a label, is not).

- **An image gone under `make gate`'s parallel suites**: once, e2e-full's re-run met `No such
  image: …/atlas/atlas-provision@sha256:…` at its provision step (its registry live, the image
  pulled earlier in the run); alone it passes. No suite removes another's registry references;
  the suspect is two suites pulling the same content (e2e-full and e2e-full-k8s build
  `examples/full` alike) and a removal of one reference reaching the other's. Find the removal.

- **A Docker host's NIC rings on the `network` profile**: a resize resets the link, so `tune`
  never resizes an up link and reports it as advice with the `ethtool -G` to run while it is
  down; a host's own boot does not grow them either. A unit ordered before the network (as the
  VM's) would.

- **`tune` as a user on a `hidepid` host**: without root, `/proc` hides dockerd's command line,
  so the report judges `/etc/docker/daemon.json` where the engine may read a `--config-file`
  (which `--apply`, as root, writes): the report can show the daemon keys as differing after a
  good apply. Say so in the report when no dockerd is visible to a user.

- **Retired release tags stay**: on a registry that deletes no tag alone, a tag `prune` retired
  (a release's, an image's, an installer's) is never visited again, even once the registry deletes
  tags alone; each costs a read in every release listing. Only floating tags are deleted then.
- **Content left untagged outside a prune's sweep**: on a registry that deletes no tag alone, a
  prune killed between retiring a tag and its closing sweep loses the list of content another tag
  held, and a pointer `yank` retires on an incomplete release keeps no such list; once the other
  tag moves, that manifest stays untagged and is deleted by nothing. The sweep running when the
  pointer's move back fails has no test that fails without it (the pointer then still holds the
  content).
- **An installer only `:latest` held, left untagged on retirement**: on a registry that deletes no
  tag alone, a retired `:latest` leaves the installer manifest it held in the registry, untagged,
  when no release tag holds it any more (a prune that stopped part way). Disks have the disk set
  cleanup; nothing sweeps the installer repository.
- **A failed chown after `up --no-start` leaves a running stack down**: when a start makes a
  volume of a `uid` while the stack runs (an upgrade that adds one), `up --no-start` recreates the
  changed services before the volume helper runs; if the helper then fails, they stay created and
  stopped until a re-run or a rollback. Start the volume's own services alone first, or bring the
  others back up on that failure.
- **`compose config` on every start**: a start reads the stack as Compose runs it for the content
  labels and the new volumes, even for a stack with no inline config, no environment-sourced
  secret and no volume of a `uid`, where it finds nothing; skip it there.
- **One list of the release's own images a start runs**: the front and the volume helper are
  listed from the manifest (`allPreloaded`), from the spec and the manifest (`stowageImageVars`)
  and from `.env` (`stackImages`); one helper would keep a new Stowage image from reaching some
  and not others.
- **No front as one install mode**: a project without a front is a set of `HasFront` guards across
  the installer (preflight, decide, renderStack, up, openPorts, reconfig, upgrade, vm create, the
  CLI's flags); one front mode with no ports, Caddyfile or image would keep a new path from
  forgetting its guard.
- **A disk push's commit on a slow registry**: the commit (`PUT ?digest=`) waits at most the
  client's 2-minute response bound; a registry that verifies or moves a large blob for longer
  gets the commit retried, the blob looked for until the 5-minute stall bound, then the push fails
  though every byte went up. Give the commit a bound of its own, scaled to the blob.
- **`k8s upgrade --version` reads the fetched installer's payload twice** (its check, then the
  run's release): pass the files read once along.
- **A mirror import's blobs in chunks too**: `mirror import` pushes each blob in one stream, as
  a ship pushed disks before; a disk set imported over a lossy link restarts from zero. Push the
  archive's large blobs as a ship pushes a disk.
- **e2e-kairos's `backup verify` failed once beside the parallel suites** (2026-10-21) and passed
  alone: find the clash (the WebDAV server's port, or the runner's container name) and fix it.
- **A volume quiesced for its backup**: `backup.volumes` are read while the stack runs,
  which suits material written once; a service writing a volume continuously would need its own
  pause (a hook around the archive).
- Not built for non-HTTP stacks: x509/key generators (product PKI; DR is covered by volume
  backup), a pre-start hook, front client-cert auth, a project with no front.

- **A compressed disk's file size**: a disk's layer is annotated with its file's digest, not its
  size, so a different file of that name in `vm create`'s `--out` is hashed whole before it is
  refused, and the download's progress counts compressed bytes. Annotate the size too.

- **One helper for an IPv6 host's brackets**: `secretsSteps`, `urlHost`, `vm create --host` and the
  mirror's CA directory each bracket or unbracket an IPv6 address by hand.

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
- **`hooks.provision.returns` leftovers**: lint refuses a `returns` key that is an input even when
  the input is asked only on other shapes (an input with `shapes: [kubernetes]` the hook mints on
  Docker cannot be declared); and lint cannot see a declared key the chart's ConfigMap
  `<release>-stowage-provision` also sets, which `k8s provision --set` then refuses at run time.
- **A scheduled reboot whose boot id was not read never resolves in the journal**: when the
  kernel's boot id cannot be read (never on Kairos), the `reboot.scheduled` carries no `boot_id`,
  and a starting daemon tells two boots apart only by two ids read, so it neither cancels that
  reboot (`the daemon restarted before it`) nor takes it as run: the journal leaves it open for
  good, and `events` never tells whether it happened. Comparing a monotonic boot counter, or the
  status's uptime, would tell.
- **Re-run leftovers**: the check "was a route's domain given" is written three times (`mode`,
  `applySettings`, `changes`), the release's compose file is parsed by three partial structs
  (`passesEnvOn`, `reset-db`) and "diff two `.env`, redact secrets" twice (re-run's log,
  upgrade's dry run); one helper each. An input whose default names another (`${KEY}`) is not recomputed when that one
  changes (README says to give it too); a `byo` domain change reads the certificate before the
  domain's syntax is checked, so a missing or expired file hides that error. An upgrade whose
  `up` failed leaves no change marker, so a later change's `.env.prev` holds a `.env` that never
  came up.
- **Disk-pressure leftovers** (the VM's watch): its POST repeats the health probe's steps (the
  service's address, the client, the bounded answer): one helper could serve both. e2e-kairos
  runs the daemon on the test host with the default `/etc/issue.d`: past `hard_pct` (99% there)
  it would write the host's login banner; `hostd` could take a hidden directory flag. A banner
  whose `agetty --reload` failed is not reloaded by a later check (the file is unchanged), and a
  disk that stops reading leaves the last banner up. An address that never comes (a service on
  the host's network, a broken compose file) is retried every minute, not every 10. pressurePost's
  `(result, ok, again)` could be one struct; TestServeWaitsForTheCheckUnderWay repeats served()'s
  short socket directory.
- **Backup leftovers** (the VM's backups): a scheduled run refused because another job runs (an
  upgrade at 03:00) fails the timer's service and waits for the next day: retry it after the job.
  The `command` backup provider runs nowhere (lint warns on a `kairos` shape). The
  timer's default (`*-*-* 03:00:00 UTC`) is written twice, in the bundle's unit and the daemon.
  An upload cut off on a multipart backend (S3) may leave parts `deletefile` does not see: abort
  them (`rclone cleanup`). The timer's skip and `installed()` test "not installed" apart.
  Retention prunes the current profiles only: backups of a profile a later release dropped stay
  (`backup list` shows them). A runner container cut off by its context is removed by name only
  when the docker CLI was stopped by that context.
- **Network override leftovers** (review of the network slice): a confirmation refused after its
  window answers an error only, not the settings in force (`network` shows them); the host
  daemon's start taking the network lock before it serves has no deterministic test.

- **`upgrade --reset-db`**: an upgrade that wipes the data in the same run — the way across a
  schema baseline, which today takes a fresh install (on the VM, in the boot that upgrades, judged
  with `health_gate.deadline_after_reset_s`, validated only today). A review of one attempt
  found: the confirmation must list every volume either release's `data.reset` names; a run
  stopped between the `.env` switch and the wipe leaves the new release recorded on old data
  (the hints and `rollback` must know); the recovering `reset-db` must wipe the old release's
  volumes too.
- **Schema baselines before the VM's upgrade job**: the host daemon's `upgrade` to a release at
  another baseline is refused only at the boot (a reboot, a failed health gate, the fallback) —
  check the two specs before the job writes the OS. (On Kubernetes `k8s upgrade` checks it, and
  `k8s rollback` needs no check: a helm history never spans a baseline, since upgrade refuses one
  and `reset-db --new-baseline` uninstalls.) Recording the baseline in the chart's values would
  spare the installer fetch `k8s upgrade` makes to read it.
- **Lint the data volumes against the compose file**: `data.volumes` and `data.reset` names are
  not checked to be top-level compose volumes Compose owns (not `external`); `reset-db` refuses
  them at run time.
- **A parallel clash in the gate**: once, with every suite in parallel, `e2e-docker`'s upgrade
  to 2.0.0 found its pinned postgres (`<reg>/notes/notes-postgres@sha256:…`, pulled at install)
  gone from the engine, and Compose then pulled it without credentials; alone it passes. Suspect:
  another suite's removal of an image of the same content (`docker image rm -f` in
  `e2e-kairos`, the sweep's `docker rmi` of a dead run's reference) on the containerd image
  store, where references to one target may go together. Pin it down, then remove by reference
  without `-f`, or have the installer pull what `up` needs right before it.
- **Reset leftovers** (review of `reset-db`): `K8sReset` repeats the release lookup of
  `k8sDeploy` and `K8sRollback` (one helper), reads the values twice (`--all` then user-supplied)
  and Docker `Reset` decodes the hook twice; untested: the mirror flags of the recovery command, the
  name half of the daemon's release check (which repeats `sameRelease`), and a values file whose
  write fails after it was created (it is removed and the temporary copy named). The kubectl
  hint builds its target flags by hand, beside `in.kubectl`'s: one helper for both.
- **Reset extras**: a `seed` provider (a project's dataset loaded after a reset, beyond what the
  provision hook creates); `keep_on_reset` on Kubernetes (today every claim of the release goes);
  a database outside the Kubernetes release (`external`) is not reset.

- **What `test/sweep.sh` does not sweep**: a TrueNAS VM `stowage-e2e-<pid>` whose run died
  without its cleanup (a host crash; `vm destroy` needs the run's state, gone with it), and the
  build cache, which lives in the host's shared buildx builder.

- **VM health gate leftovers** (review of the health gate): `reboot`/`shutdown` are accepted while
  the gate judges a boot on trial (documented: the next boot falls back); `GET /v1/health` with
  the `command` provider starts one container per call, unthrottled (the app socket can poll it),
  and the container of a call a reboot interrupted is not removed (its compose file is, after an
  hour);
  `status` shows the last failed boot forever, and diagnostics are named and pruned by the wall
  clock (wrong before time sync); the gate's diagnostics run `compose ps` again; the rollback plan
  parses `.stowage-prev/state.json` beside the dry run that parses it too; the stale health file
  age is wall-clock (`timeout_s` is unbounded); the gate and the daemon build the assessment and
  the reboot bookkeeping each their own way; the one-minute bound on the upgrade's assessment
  write has no test (it needs a hanging fake), nor the fresh context the rollback's restore of the
  marks takes, nor the rollback's re-checked plan finding another target (reachable only through
  a run that changes the install consistently during the drain).

- **`e2e-kairos` and `e2e-vm` cannot run in parallel**: both build disks with auroraboot, whose
  loop device setup fails ("device or resource busy") when two run at once. The gate runs `e2e-vm`
  after the others; serialise the disk builds on the host (a lock) to run them together.

- **`vm create --create opentofu` waits out a failed first install**: the VM's status reports no
  install and the first boot's failure is not in it, so the wait runs to `--wait`. Report the
  boot's install failure in the status, and stop waiting on it.

- **The app socket reads the install's `.env` at every request** to check the token: cache it,
  re-read when the file changes.

- **A VM's `--channel` is not checked against the project's channels**: the bundle's runtime spec
  carries no channels, so a typo is recorded and fails later as "no release" on `updates`.
  `vm create` will give the channel of the release it installs.

- **A disk that fails rebuilds the bundle on the next ship**: the bundle was pushed by digest,
  untagged, and nothing records it. Recording built digests (a staging tag) would keep it.
- **A format dropped from a mutable release leaves its disk tag**: re-shipping `beta-1.0.0`
  without `raw` records no raw disk, so `prune` never deletes `beta-1.0.0-raw` (an image key
  dropped from the spec behaves the same).
- **`latest-<format>` of a format only a newer stowage knows**: an older stowage moving the
  latest channel leaves that tag as it was (it lists its own formats).
- **Disk `latest-<format>` checks every format**: each sync or verify on the latest channel HEADs
  all five tags; the spec's formats plus the pointer release's would do.

- **k8s release in helm's `unknown` status**: no `helm list` status flag selects it, so `k8s`
  treats such a release as absent (`install` would try to install over it). Read `helm status`
  when `helm list` finds nothing.
- **k8s rollback reads the running values twice** (`installedRelease`, then the comparison): pass
  them through.
- **k8s pull Secret on an upgrade without a password**: kept through helm's reused values, so
  `--dry-run` does not show it, a pull Secret deleted out of band is not noticed, and an upgrade
  moved to an `--image-registry` mirror keeps naming the project registry's account. Read
  `stowage.pullSecret` from the release, check it exists, and write it back (or drop it for a
  mirror).

- **A symlinked version file at a commit**: the hook reads the link's target text, not the file it
  points to, so a project whose `VERSION` or changelog is a symbolic link gets an error from
  `hook post-commit` (`ship` reads it fine).
- **Kept release's front rendered by the newer Stowage**: an upgrade renders the kept release's
  front files with its own renderer (from the kept release's spec and `.env`), so a rollback runs
  them as this Stowage writes them, not as the older one did. Equal while the front's format does
  not change; render through the kept installer when it does.
- **A first install refused by the lock** leaves the directory it created (holding only the lock
  file): another run holds it, so it is not removed.

- **Upgrade with a new route**: refused today (a new route needs a port or a domain decided);
  decide it as a first install does.
- **`<short>-stow updates`**: list the channel's releases after the installed one, from the release
  manifest (DESIGN's verb set).
- **Upgrade order under a `command` scheme**: the project's scheme command does not run on an
  install, so an upgrade to an older release is not refused there.

- **`command` timeout after a resume**: no test catches a timer left stopped after Ctrl-Z / `fg`
  (the command could then hang with no timeout); the suspend test only proves a stop is not
  counted.
- **Environment size**: each recorded value is capped (64 KiB) and the hook's outputs (256 KiB),
  but not their sum, which a stack with many large inputs could push past the kernel's `ARG_MAX`
  for the docker CLI or the hook's command.
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
- **Signature counts name referrers**: on a registry without the referrers API every referrer
  listed under the empty config type or none is read, so the 64 cap and the "N signature(s), none
  by a trusted key" message count non-bundle referrers (an SBOM) as signatures; count the bundles
  read instead, and cover the tag-fallback shape with a verifier-level test, not only the
  registry client's.
- **Signature reads are bounded at 64 referrers per digest**: someone with push access can push
  65 junk referrers on a signed release and every check of it fails ("cannot check"). The same
  access can already delete the release's tags, so it is a denial of service, not a bypass; read
  the newest 64 instead, or prefer the key's hint. Likewise an unsigned revision of V with unreadable signatures
  stops `upgrade --version` (fail closed), and V's revisions are checked one at a time, newest
  first, so many unsigned revisions make it slow: check them concurrently (bounded).
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
- `short()` is duplicated between packages: one helper.
- `make smoke-install` leaves the images it pushed to its throwaway registry
  (`127.0.0.1:<port>/smoke/*`) in the local engine, and `make e2e` the installer and front images
  it pulls: remove them at the end, as `make e2e-docker` does.

- **The gate exercises the release workflow**: `test/gate.sh` selects nothing for a change to
  `.github/workflows/`, so the release's tag, date and notes extraction run first on a real tag.
  A script the workflow calls, with a test in the gate, would cover it.

- **Other tests write a script and run it while parallel tests fork** (`internal/command`,
  `internal/publish`, `internal/kairos`): exec can then fail with "text file busy" (a fork holds
  the file it inherited open until its own exec). `internal/install`'s hook tests write theirs
  before they turn parallel; the same would make these immune.
- **`make build` rewrites `./bin` in place**: a build in another terminal while a suite runs a
  binary from it fails that exec (text file busy); the gate's own suites reuse its one build
  (`make -o build`). Exporting to a temporary directory and renaming each file into `./bin`
  would make any build safe beside a run.
- **A test release on another name of the project's registry**: `--test-registry` is compared
  by its spelling, folded (case, dots, ports, loopback, Docker Hub); another DNS name or address
  of the same registry is not told. Before publishing, a test release of a signing project could
  read the test prefix's release repository and refuse when a release there verifies with the
  project's own keys (the user to decide: now, or as a Should).
- **A test release's key check**: `cosign public-key` runs with a background context (a cancelled
  command waits for it, up to cosign's timeout), and once, when the signer is made: a key file
  changed during the ship would sign unchecked. The check beside each sign call would close it.
- **An empty `stack.memory:` is caught by lint alone**, from the file: decoding takes it as no
  key. A presence check in spec decoding would give every reader of the spec the refusal.

## Won't — not in the first version

- Taking over an install made without Stowage: a project adopting Stowage installs fresh.

- The other extensions of DESIGN § 10 (restore verb on Kubernetes, Kubernetes provision hook +
  portal URL): none exists today; revisit after parity. The Docker backups and restore, the
  Kubernetes barrier pre-flight are Musts 6 and 10.
- Secret rotation (DESIGN § 7 row 24, an application concern).
