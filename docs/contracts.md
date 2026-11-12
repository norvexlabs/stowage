# The contracts a project must honour

[README](../README.md) · [Documentation index](../README.md#documentation)

These are what `lint` checks; each finding names the key path or `file:line` to fix.

- **Compose file** (`stack.compose`) is the single service definition:
  - no `build:` (images are built by `stowage ship` and pulled, never built on a host), no
    top-level `name:` (Stowage names each install);
  - every service's `image:` is exactly `${STOWAGE_IMAGE_<KEY>}`, where `<KEY>` is an `images` key
    upper-cased with `-` as `_` (`admin-api` → `STOWAGE_IMAGE_ADMIN_API`). The registry is
    the single deployment point, so a literal image is an error. A service with `extends`
    (`extends: base` or `extends: {service: base}`) takes the `image:` and `build:` it does not set
    from the service it extends, and its `profiles:` and `ports:` are its own and the base's, as
    compose joins them; an `extends` naming
    another file (`file:`, which the release does not carry), a service the file does not declare,
    or a loop is an error;
  - a service publishes no port past this host of its own (a `ports:` entry not bound to a
    loopback address or to `${STOWAGE_LOOPBACK_ADDRESS}`; a bare port, or one lint cannot read,
    counts as published): with a `kairos` shape that is an error (the VM's firewall opens only
    what Stowage publishes), on the `docker` shape a warning (`published past Stowage, which
    neither checks nor lists it`). A port clients reach as it is
    — TCP or UDP, beside the front — is an `l4` route (see "Spec keys" ([spec](spec.md)));
  - a service on the host's network (`network_mode: host`, its own or the one it extends) is,
    likewise, an error with a `kairos` shape (a port on the host's network is none Stowage
    publishes) and a warning on the `docker` shape (`listens on the host past Stowage`). The VM
    runs an `l4` route's service on the host's network itself, and any shape runs a service there
    that asks for it (`services.<name>.host_network`, the one way onto the host's network): with
    a `kairos` shape such a route's service, and any `host_network` service, declares no
    `networks:` (its `extends` base's included, merged by network as Compose merges them) and
    runs one container (`scale`, `deploy.replicas`), and the route's port is its upstream's;
  - a service given the host's powers is warned, on every shape, never refused (a stack may need
    one, as a VPN's `NET_ADMIN`): `privileged: true` (or a string Compose casts to true — `true`, `yes`, `y`, `on`, any case — or any other value but `false`, `no`, `n`, `off` — an interpolation lint cannot read, or one Compose refuses, a number included — warned as only a run knowing: `the
    container holds every power of the host's root`), `cap_add` (`powers of the host's root beyond Docker's defaults`, naming them),
    `pid: host` (`sees and signals every process of the host`), and a `volumes:` entry
    bind-mounting the Docker socket, `/var/run/docker.sock` or `/run/docker.sock`, or a directory
    holding it (`/var/run`, `/run`, `/`; a long-syntax entry without a `type` counts as a bind, a
    `${VAR:-default}` source reads as its default, and a bare container path is no bind) (`the service drives the host's engine, so holds the
    host`); a service extending another takes its `privileged`, `pid` and `cap_add` too;
  - a service that waits for a one-shot to complete (`depends_on: { <one-shot>: { condition:
    service_completed_successfully } }`) without `restart: true` in that entry is warned (`waits
    for the one-shot <one-shot> without restart: true …`; a `restart` from a variable, or one
    Compose does not read as a boolean, is warned as only a run knowing): a re-run that changes the one-shot makes
    it again, and only `restart: true` has Compose restart the service then, so one that reads a
    file the one-shot renders once at start keeps the old one;
  - with a `docker` or `kairos` shape, a long-running service — any but a one-shot: one another
    service waits for with `condition: service_completed_successfully`, or one that says so with
    `restart: "no"` (or `deploy.restart_policy.condition: none`) — with no `restart` nor `deploy.restart_policy` (its `extends` base's count) is warned (`no restart
    policy: a crash or an OOM kill leaves it down until the next start; give it restart:
    unless-stopped, or restart: "no" for a one-shot`): Stowage sets no restart policy, the compose
    file's is the one that applies; and the long-running services with no healthcheck (none, or
    one disabled: `disable: true`, `test: ["NONE"]`; a `healthcheck` without a `test` tunes the
    image's own, and counts) are warned together
    (`<services> set no healthcheck: the install (and vm create) is done once they run, not once
    they are ready …`; an image's own `HEALTHCHECK` counts, which lint does not read);
  - a service with `profiles:` runs only while an input turns one of its profiles on (an input's
    `profile`, see "Spec keys" ([spec](spec.md))); a profile no input names is an error (no install would ever
    start the service);
  - a variable without a default (`${X}`) must be an input, a secret, a safe default or a route's
    `domain_input`; a variable with a default (`${X:-v}`) is an optional tunable;
  - each `safe_defaults` key appears as `${KEY:-<safe value>}`: the safe value is the default and
    only the installer relaxes it;
  - `STOWAGE_` is reserved. Stowage provides `STOWAGE_IMAGE_<KEY>`, `STOWAGE_URL_<ROUTE>` (the
    route's public URL, its path included — use it rather than building one from a domain), `STOWAGE_NAME`,
    `STOWAGE_PROJECT_VERSION`, `STOWAGE_REVISION`, `STOWAGE_TLS_MODE` (never set without a
    front: lint refuses it then), `STOWAGE_LOOPBACK_ADDRESS`
    (a loopback address in every mode and without a front, for a port only this host may reach: 127.x or `::1`,
    bare — Compose's port syntax takes `::1:5432:5432`), `STOWAGE_IPV6` (`true` with `--ipv6`, else `false`; Docker and the VM),
    `STOWAGE_L4_<ROUTE>` (an `l4` route's endpoint, `<address>:<port>`, for a route with an
    `address_input`; Docker and the VM), `STOWAGE_INPUT_FILE_<KEY>` (the path of a file input's file, to mount; Docker and the VM), `STOWAGE_SECRET_FILE_<KEY>` (the path of the file a secret with `as_file: true` is delivered as, to mount; Docker and the VM). The variables a service's `services` entry gives it
    (`STOWAGE_MEM_LIMIT_MB`, `STOWAGE_HOST_MEM_MB`, `STOWAGE_HOST_CPUS`; `STOWAGE_CPUS`) are in its container's
    environment, not the compose file's: a command reads them escaped, `$${STOWAGE_MEM_LIMIT_MB}`,
    and `${STOWAGE_MEM_LIMIT_MB}` is an unknown variable;
  - an unknown `STOWAGE_…` variable, a `STOWAGE_IMAGE_<KEY>` naming no image, a
    `STOWAGE_URL_<ROUTE>` naming no route, a `STOWAGE_L4_<ROUTE>` naming no `l4` route with an
    `address_input`, a `STOWAGE_INPUT_FILE_<KEY>` naming no file input or a `STOWAGE_SECRET_FILE_<KEY>` naming no secret delivered as a file is an error, and so is a compose file with no service;
  - service names starting `stowage-` are Stowage's (it adds the front as `stowage-front`), and so
    are top-level `volumes` and `networks` starting `stowage-` (the front's data volume is
    `stowage-front-data`).
- **Helm chart** (`shapes.kubernetes.chart`, the project's own chart directory): `ship` packages
  it as `helm package` would, without the helm binary:
  - every file of the directory, less what its `.helmignore` excludes (Helm's rules, with
    Helm's default `templates/.?*`) and any `.git` (directory or file); a UTF-8 byte order mark
    is dropped, as helm does. The
    packaged files are the chart node's inputs, so an excluded file changes no release;
  - `Chart.yaml` must be `apiVersion: v2`, with a `name` (no `/`, `\`, `.` or `..`) and `type`
    `application` or none (a `library` chart cannot be installed). Its `version` and `appVersion`
    are set by `ship` (comments and other keys are kept); the source values are ignored;
  - a dependency in `Chart.yaml` must be vendored under `charts/` (a directory with a
    `Chart.yaml`, or a `.tgz`, of that chart name): `ship` fetches nothing;
  - every file must be a regular file of at most 5 MiB (a symlink is refused);
  - `values.yaml`, when present, is one block-style mapping document. `ship` appends to it, as
    text (the chart's own lines are kept byte for byte), a top-level `stowage:` block, so the
    key `stowage` is reserved, and a file where appended text would change the chart's own
    values (a flow mapping, a second document, a block scalar ending the file without a line
    break) is refused. A `values.schema.json` must allow the `stowage` key. The block:
    ```yaml
    stowage:
      registry: <registry.prefix>       # set it to a mirror at install time
      release: { name, version, revision, channel, tag }
      images:
        <key>: { repository: <name>-<key>, digest: "sha256:…" }   # every image, the front included
      publicKeys: [ "<PEM>" ]           # signing.public_keys, when the project signs
      l4:                               # the l4 routes, when there are any
        - { name, protocol: TCP|UDP, port, service, targetPort,    # service: the upstream's compose service
            tls: { domainInput, clientAuth, alpn } }                # a route with tls alone: the chart terminates it (alpn: when declared)
    ```
    Stowage creates nothing in the cluster for `l4`: a route with `tls` is the chart's to
    terminate (a Gateway's `TLSRoute`, cert-manager's certificate for the domain its
    `domainInput` holds, which the chart reads as any input; never its `targetPort` published in
    the clear), and the chart publishes each route as it
    chooses (a `NodePort` or `LoadBalancer` Service to the pods of `service`, as
    [examples/full](../examples/full/deploy/chart/templates/echo.yaml) does);
  - the templates run the release's images by digest:
    `{{ printf "%s/%s@%s" .Values.stowage.registry $img.repository $img.digest }}` with
    `$img := index .Values.stowage.images "<key>"`. `lint` warns when no template reads
    `.Values.stowage.images`;
  - `<short>-stow k8s` adds these values at install time (absent from the published chart, so guard
    them with `{{ with }}`, or `stowage.secret` with `{{ required }}`, and `stowage.files` too in a chart whose project always asks a file input on Kubernetes): `stowage.files`, the name of the Secret `<release>-stowage-files` holding each file input's content under its key, empty when the release has no file input (mount it, one `subPath: KEY` per file, as [examples/full](../examples/full/deploy/chart/templates/api.yaml) does; a file input's key in `stowage.secret` is its recorded form, no file to read); `stowage.secretFiles`, the name of the Secret `<release>-stowage-secret-files` holding each secret delivered as a file (`as_file: true`) under its key, empty when the release delivers none (mount it as a volume with one `items` entry per key; at `/run/secrets`, as on Docker, the pod sets `automountServiceAccountToken: false`, as the example's api does: the kubelet otherwise mounts the ServiceAccount's token below `/var/run/secrets`, which an image's `/var/run` linked to `/run` makes inside that read-only mount, and the container cannot start; a key there is in `stowage.secret` too only while a revision of the helm history reads it there, for a rollback); `stowage.asFile`, the keys the release delivers as files; `stowage.secret`, the name of the Secret holding
    the release's secrets and Kubernetes inputs as `KEY: value` (give each container only the
    keys it reads, with `env: [{name: K, valueFrom: {secretKeyRef: {name: …, key: K}}}]`;
    `envFrom` would give every container every secret), and `stowage.pullSecret`, the name of the `kubernetes.io/dockerconfigjson` Secret
    holding the pull account, empty when there is none (give it to `imagePullSecrets`), and
    `stowage.sysctls`, the pods' network settings as a list of `{name, value}`, empty unless the
    operator gave `--unsafe-sysctls` (give it to each pod's `securityContext.sysctls`, as
    [examples/full](../examples/full/deploy/chart/templates/_helpers.tpl) does). It also
    sets `stowage.registry` to the prefix the release is pulled from. `lint` warns when no
    template reads `.Values.stowage.secret` while the project has a secret or a text input on
    Kubernetes, when no template reads `.Values.stowage.pullSecret` while its pull account is
    a `token`, when no template reads `.Values.stowage.files` while a file input is asked on
    Kubernetes, when no template reads `.Values.stowage.secretFiles` while a secret is delivered
    as a file there, and when no template reads `.Values.stowage.sysctls`;
  - with an `endpoint` (see "Extensions" ([extensions](extensions.md))), the chart exposes that route's `upstream:
    <service>:<port>` as the Service `<release>-<service>` on that port: `<short>-stow k8s` prints
    `http://<release>-<service>.<namespace>.svc:<port>` as the project's endpoint.
- **Kairos OS bundle** (`shapes.kairos`): `ship` builds `<prefix>/<name>-os` with `docker buildx`
  from a Dockerfile embedded in `stowage`: `base` converted by `kairos_init` (`-s install`, then
  `-s init --skip-step cisHardening`) into a Kairos image with no Kubernetes provider, carrying the release's version
  (without its channel) in `/etc/<name>/os-version` and as its Kairos version (given to the init
  stage only: the slow layers before it — the base's packages, `tzdata` and, where the base has it, `tzdata-legacy` among them, kairos-init's
  install stage and Docker's packages — are the same from one release to the next, so
  BuildKit's cache serves them). The base is Ubuntu: the build reads its `/etc/os-release` and
  fails on another distribution, naming `shapes.kairos.base`. kairos-init installs one package
  list on every Debian-family image; the build purges what the appliance does without, so none of
  it runs, listens or reports to a vendor: before the init stage (its initrd carries none of them)
  `haveged`, `lldpd`, `mdadm`, `nbd-client`, `nfs-common`, `open-iscsi`, `os-prober`,
  `packagekit`, `rpcbind`, `snmpd`, `ubuntu-advantage-tools`, `xauth`, `xclip`, `xdg-user-dirs`
  and `zfsutils-linux`; after it `fail2ban`, which that stage
  enables. `multipath-tools` stays (the init stage adds dracut's multipath module, which needs
  it), its daemon masked. Before the init stage, what the purged packages leave unneeded goes with
  them (`apt-get autoremove`); after it, nothing else. A purged package `host.packages` declares
  is purged too, and installed again with the project's packages after the init stage, as is a
  purged one a declared package depends on (the initrd lacks both). The build fails, after the
  purges, when a purged package is still installed (but those declared), and at its last step when a declared package, or one of the
  bundle's own (`containerd.io`, `docker-ce`, `docker-compose-plugin`, `dracut`,
  `linux-image-generic`, `nftables`, `openssh-server`, `systemd`, `systemd-resolved`,
  `systemd-sysv`), is not
  installed, or one of the masked timers below is not masked, or when the tmpfiles rules of
  `/etc/resolv.conf` are not the bundle's alone or systemd-resolved is not enabled. On top of it:
  - Docker CE and its compose plugin from Docker's apt repository (unpinned: the version of the
    day of the build), the containerd image store on, data under `/usr/local/docker` (the
    persistent partition), started after `nftables`;
  - every image of the release as an OCI layout archive in `/usr/share/<name>-preload/<key>.tar`,
    named `<prefix>/<name>-<key>:sha256-<hex>` (its digest), which `<name>-preload.service`
    `docker load`s at every boot (an A/B upgrade brings the new archives);
  - `<name>-expand.service` and `/system/oem/01_<name>_expand.yaml`: the persistent partition grows
    to fill the disk at boot; `/system/oem/52_<name>_reset_settle.yaml`: udev settles before the
    first-boot reset;
  - `/usr/sbin/<short>-stow`, the installer with the release as the bundle carries it, and its units
    `<name>.service` (the boot), `<name>-health.service` (the health gate),
    `<name>-hostd.service` (the host daemon), `<name>-network.service` (the network override),
    `<name>-tune.service` and `<name>-tune-online.service` (the network tuning, at each boot
    before the network starts and again once it is up, before Docker: `<short>-stow tune-boot`;
    see [Network tuning](tuning.md)), `<name>-route6.service` (with IPv6
    on, the wait for the IPv6 default route before Docker: `<short>-stow route6-boot`; see
    `network set` below), `<name>-tables.service` (the
    services' nftables tables the last start loaded, at each boot before Docker: `<short>-stow
    tables-boot`; see "A service's nftables table" ([host](host.md)) under the installer) and
    `<name>-backup.timer` (not enabled: the host daemon starts it) with `<name>-backup.service` (the
    scheduled backup); see "The installer
    on the Kairos VM" below;
  - Kairos's `/system/oem/08_grub.yaml` (its GRUB boot assessment) without the step that marks
    every boot of the active system good as it starts: the health gate marks it. A boot of the
    active entry chosen once (`next_entry`, which an upgrade sets) is judged too, and while a failed
    attempt is marked that choice boots the passive system as well. The build fails
    when kairos-init's file is not the one this copy is derived from (by its sha256), naming
    `shapes.kairos.kairos_init` and the release to use. An OS upgrade also refreshes the boot
    loader on the EFI partition (shim, GRUB and its `grub.cfg`, when the partition has room for
    them);
  - Kairos's `/system/oem/09_systemd_services.yaml` without its fixed `net.core.rmem_max`
    (7500000), which its boot stage would set after the tuning units, below what they size from
    RAM, and without its enable of `iscsid` when `open-iscsi` is purged (host.packages bringing
    it back keeps it); its other settings stay. The build fails when either line is no longer
    there;
  - dracut's crypt generator in the initrd exiting cleanly when no disk is encrypted (its
    `return` outside a function, which `sh` refuses with status 2, becomes `exit`; the build fails
    when the script is no longer that one), and `/etc/default/locale` reading `LANG=C.UTF-8`
    (written to `/etc/locale.conf`, which Ubuntu links it to), so a healthy boot, an SSH session
    and `sudo` log no error (`journalctl -p err` is empty);
  - a default-deny firewall, `/etc/nftables.conf`, with SSH open to every source
    (`/etc/nftables.d/ssh.nft`), `/run/nftables.d` made at boot for the install's ports, and a
    forward chain including the install's `nftables.d` (see "The firewall" ([vm](vm.md)) under the installer on
    the Kairos VM);
  - SSH by key only (`/etc/ssh/sshd_config.d/10-<name>.conf`), over kairos-init's SSH hardening
    (`/usr/lib/<name>/sshd_config.d/05-kairos-hardening.conf`, in the image itself, read first
    through `/etc/ssh/sshd_config.d/00-<name>-hardening.conf`'s `Include`, so a rollback to a
    release on an older OpenSSH never reads it: modern ciphers, key exchanges
    and MACs only; no TCP, agent or X11 forwarding and no tunnels; at most 4 authentication tries;
    a client that stops answering is dropped after 10 minutes: an agent holding more keys than 4 needs `ssh -i
    KEY -o IdentitiesOnly=yes`, which `vm create`'s own SSH passes: the private key next to
    `--ssh-key`'s `.pub`, else that `.pub`, so the agent offers its key alone), and root's login
    refused (`/usr/lib/<name>/sshd_config.d/00-<name>.conf`, read before kairos-init's, which allows
    it by key: a cloud's datasource writes its metadata's SSH keys for root); the build fails when
    that drop-in no longer sets `AllowTcpForwarding no`, and when sshd's effective settings
    (`sshd -T`) are not `PermitRootLogin no`, `AllowTcpForwarding no`, `MaxAuthTries 4` and
    `PasswordAuthentication no`, naming the setting and the value it found; kairos-init's CIS hardening step is not run (its shell umask,
    password ageing and lockout would reach the install's files and the operator's account), the
    build failing when its module blocklist (`/etc/modprobe.d/cis-blocklist.conf`) or a umask of
    027 (`/etc/profile`, `/etc/profile.d`, `/etc/bash.bashrc`, `/etc/login.defs`) is there; an
    `admin` group (created when the
    base lacks it), `sudo` without a password for it (`/etc/sudoers.d/10-<name>`: its members log in with an SSH key and have none), a console banner (`/etc/issue.d/05-<name>.issue`),
    consoles on the screen and the serial port (serial last, so it owns `/dev/console`; no
    account logs in there with a password, but GRUB has none either, and its menu edits the
    kernel's command line: whoever reaches the hypervisor's console or the serial port has root,
    so they are as trusted as the hypervisor), the QEMU
    and VMware guest agents, `multipathd` masked, and `apt-daily.timer`, `apt-daily-upgrade.timer`
    and `motd-news.timer` (an image-based system is never upgraded in place, and fetches no news)
    masked; the project's `host.kernel_args`, appended to the command line by the image's own boot
    script (`/etc/cos/bootargs.cfg`, after Kairos's arguments, on every entry but the recovery
    system's, so the active and passive systems each boot with their release's: a release whose
    arguments fail to boot falls back to the passive one with its own; the build fails when the
    script no longer ends by computing the command line); the project's `host.packages`, installed from Ubuntu's archive
    (`--no-install-recommends`, the version of the day of the build) after kairos-init's init
    stage, which would strip a kernel module one ships, with what they depend on, over the image
    as that stage built its initrd: the packages the boot and the stack run on (the kernel, systemd
    and udev, dracut, kmod, GRUB and shim, cryptsetup, LVM and device-mapper, multipath, Docker and
    containerd, OpenSSH, nftables) are held, so apt neither upgrades nor removes one for them
    (`--no-remove`), and one that needs it fails the build, as one that brings a new package of
    that kind (a kernel, its modules, a service of the boot's) does, naming it; a declared package
    installed already is left as it is; the kernel modules kairos-init strips are
    reinstalled (the build fails with fewer than 100); the project's `host.modules` in
    `/usr/lib/modules-load.d/50-<name>.conf` (each one `load`ed at boot) and
    `/usr/lib/modprobe.d/50-<name>.conf` (each one `block`ed, as `blacklist <module>` and `install
    <module> /bin/false`, so neither a dependency nor udev loads it; each one's `options`), each
    line under its `reason`, in the image's `/usr` (Kairos keeps `/etc` across upgrades and
    rollbacks) and so in its initramfs, from the first boot. The build fails, naming the key, when a
    module loaded or given options is in no kernel of the image, one is built into it (modprobe's
    files do not reach it, and `tune` could not tell it loaded), an option is not one of the module's
    parameters, or modprobe and systemd-modules-load do not read the files.

  The build records the bundle's unpacked size on its manifest (`dev.stowage.os.rootfs-mib`),
  which sizes its disks' partitions, and logs it (`os bundle: N MiB unpacked`). The bundle
  preloads the release's images but its scanner (`audit`), which no VM runs.
  Its inputs are the version and revision, the name, `base`, `kairos_init`, the embedded recipe,
  the images' inputs, and what its `<short>-stow` carries — the runtime spec less `disk_formats` and
  `cloud_publish`, `stowage.lock`, the changelog section, the version scheme, the compose file,
  the trusted keys, the installer executable and the pull account; it is kept on them and on the digests of the images it preloads, so it
  names no channel and a promotion re-tags it; every new version or revision rebuilds it, with
  the distribution's and Docker's packages of the day. The build needs network access (apt, kairos-init's
  packages) and takes several minutes; the bundle is about 3 GB.
- **Disk images** (`shapes.kairos.disk_formats`): their first boot lays out Kairos's partitions
  and gives the rest of the disk to the persistent one (`/usr/local`: the install, Docker's
  data, the volumes). The state partition (the active, passive and upgrade-transition systems) is
  sized for three system images and a quarter more for later releases (`disk.state_size`), not
  auroraboot's 3 × the recovery partition: first estimated from the bundle (each image its
  unpacked size × 3/2 + 200 MiB), then checked against the recovery partition auroraboot made
  (two images and 150 MiB, sized by auroraboot from the bundle as it measures it): an estimate
  short of it (the partition named `recovery` in the raw disk's GPT; none so named fails the
  build) makes the raw disk again, once, with the size it calls for, and a second run whose
  recovery partition grew again fails (`disks: the state
  partition of N MiB holds no three system images of this bundle: made again with M MiB`). Each
  disk records what Kairos takes of a disk, measured (`dev.stowage.disk.os-mib`: the raw disk's
  partitions and the state one), which `vm create` adds to the app's space; a release outgrowing
  its state partition cannot upgrade a VM made before it. A bundle that records no size (built by
  an earlier Stowage) makes no disk. A raw disk whose GPT is not sound (its header's or entries'
  CRC32, a header of 92 to 512 bytes, entries and partitions within the disk) fails the build.
  `vm create` boots a disk set only when every disk of it records a size of 1 MiB to 16 TiB; it
  then takes the channel's set, or refuses, naming the disk (`records no dev.stowage.disk.os-mib:
  published by an earlier Stowage`, or `records … "<value>", not a size in MiB (1 to 16777216): not
  as Stowage publishes it`).
  Made from the bundle by the pinned auroraboot
  (v0.28.0, run privileged on the host network). auroraboot holds no registry account: it reads the
  bundle by digest from a read-only proxy `ship` serves on a loopback port for the disks' build
  alone (`127.0.0.1:<port>/<repository>@<digest>`), with the proxy's own account — user
  `stowage-proxy` and a random password, new for each proxy, given to auroraboot in a private
  Docker config file (0600), never on a command line, so another user of the machine cannot read
  through it. The proxy reads the `<name>-os` repository with the publisher's account (pull-scoped
  to that repository where the registry issues tokens; with Basic auth, the proxy's filter is the
  limit) and passes only `GET` and `HEAD` of that bundle's manifest, by its digest, and of the
  repository's blobs (and the `/v2/` ping), so the publisher's account never leaves `ship`, and a
  disk builds whoever else may read the registry. auroraboot runs on the host's network, so the
  Docker engine must share this machine's loopback (a rootful engine; not rootless Docker nor
  Docker Desktop's VM). Each container the disks' build runs (auroraboot, qemu-img, the files'
  `chown`) is named `stowage-disks-<16 hex>`; a `ship` interrupted (Ctrl-C, SIGTERM) removes the
  one running (`docker rm --force`) before it removes its work directory, as the killed `docker`
  CLI would leave it running, holding that directory and its loop devices (one it cannot remove
  is named in the error: `the cancelled container <name> may still run: docker rm --force <name>:
  …`). The formats: `raw` (EFI) by
  auroraboot; then, with qemu-img and tar from the raw disk, `vhd` (a fixed VHD, its virtual size
  the raw disk's grown to a whole number of MiB, as Azure imports it), `gce` (the raw disk grown
  to a whole number of GiB, as `disk.raw` alone in a sparse, oldgnu `.tar.gz`, as Google Cloud
  imports it), `qcow2`
  (compressed) and `ova` (an OVF descriptor — EFI firmware, Secure Boot off, 2 vCPUs, 4 GiB, a
  serial port — then a streamOptimized VMDK of 64 GiB, or Kairos's partitions and 8 GiB beyond them
  when that is more, in a tar). A grown disk (`vhd`, `gce`, `ova`) has its backup GPT moved to its new end (sgdisk). `ship`
  refuses a `vhd` whose footer is not a fixed disk's, with a valid checksum, of a whole number of
  MiB, and a `gce` that does not hold a `disk.raw` of a whole number of GiB alone. `iso` is the
  installer (bare metal, `vm create --platform metal`): auroraboot's `build-iso` of the bundle,
  read through the same proxy (unprivileged, no device), a UEFI and BIOS bootable ISO labelled
  `COS_LIVE` that boots the bundle live and installs it on a machine's disk, Kairos's partitions
  laid out there by its installer. It embeds a cloud-config (`/config.yaml`) merged with the
  machine's seed: `install.partitions.state.size`, the raw disk's state partition (the installer
  otherwise sizes it to the images it writes, leaving a later release no room), and
  `install.grub_options.extra_cmdline: console=tty1`, the screen's console last on every boot
  entry, the recovery one included, so that `/dev/console`, the boot's messages and the recovery
  shell are on a server's screen, not its serial port. Its size is the raw disk's, which `vm
  create` adds to the app's space for the disk it installs on. The live system is no host of the
  stack: the bundle's units and its SSH (`ssh.socket`, `ssh.service`) do not start there
  (`ConditionPathExists=!/run/cos/live_mode`), nor do the seed's stages; its console logs in
  (Kairos's live system): the installer is run by whoever has the machine's console.
- **Cloud images** (`shapes.kairos.cloud_publish`): each cloud listed adds the format it imports
  to the disks `ship` makes, whether `disk_formats` lists it or not — `aws`: `raw` (for `aws ec2
  import-snapshot` or `import-image`), `azure`: `vhd` (a page blob, then a managed disk), `gcp`:
  `gce` (for `gcloud compute images create --source-uri`), `scaleway`: `qcow2` (for `scw block
  snapshot import-from-object-storage`). They are published as the other disks,
  and `ship` registers each in the publisher's cloud account it has settings for, from the
  environment (never from a file or a release):

  | Cloud | Settings (`ship` registers its image when one of its `STOWAGE_` settings without a default is set; every setting without a default is then required, or `ship` stops naming those missing) | Credentials (read by the cloud's OpenTofu provider) | Registered as |
  |---|---|---|---|
  | `aws` | `STOWAGE_AWS_BUCKET` (the S3 bucket the disk is uploaded to), `AWS_REGION`, `STOWAGE_AWS_VMIMPORT_ROLE` (default `vmimport`: the VM Import service role, which reads the bucket) | `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, `AWS_PROFILE`, or the machine's role | the disk uploaded as `stowage/<image>.raw`, imported as an EBS snapshot (import-snapshot), registered as a UEFI x86_64 AMI |
  | `azure` | `STOWAGE_AZURE_RESOURCE_GROUP`, `STOWAGE_AZURE_STORAGE_ACCOUNT`, `STOWAGE_AZURE_CONTAINER` (existing), `STOWAGE_AZURE_LOCATION` | `ARM_CLIENT_ID`, `ARM_CLIENT_SECRET`, `ARM_TENANT_ID`, `ARM_SUBSCRIPTION_ID`, or the `az` login; the blob is written with that identity (Entra ID: it needs *Storage Blob Data Contributor* on the storage account), never with the account's keys | the disk uploaded as the page blob `stowage-<image>.vhd`, made a generation 2 Linux image in the resource group |
  | `gcp` | `STOWAGE_GCP_PROJECT`, `STOWAGE_GCP_BUCKET` (the Cloud Storage bucket the disk is uploaded to) | `GOOGLE_APPLICATION_CREDENTIALS`, or the gcloud application-default login | the disk uploaded as `stowage/<image>.tar.gz`, made a UEFI-compatible Compute Engine image |
  | `scaleway` | `STOWAGE_SCALEWAY_BUCKET` (the Object Storage bucket the disk is uploaded to, in the zone's region), `SCW_DEFAULT_ZONE` (a zone, e.g. `fr-par-1`: another value, a region among them, stops OpenTofu with `SCW_DEFAULT_ZONE is a Scaleway zone, e.g. fr-par-1, not a region.`), `SCW_DEFAULT_PROJECT_ID` | `SCW_ACCESS_KEY` / `SCW_SECRET_KEY`, or the `scw` CLI's configuration file and profile (`SCW_PROFILE`) | the disk uploaded as `stowage/<image>.qcow2` (one PUT: at most 5 GB), imported as a Block Storage snapshot named and tagged as the image, made an x86_64 instance image (UEFI) in the zone |

  The image is named `<name>-<first 12 hex digits of the disk's digest>`, and is the published
  disk itself: `ship` downloads it back from the registry by digest, checked, uploads it, makes
  the image of the upload, then deletes the upload (the image holds its own copy). A disk carries
  what the OS bundle carries — `<short>-stow` with the pull account, when `registry.pull` is a
  token — so whoever may read or launch the image in the account may read that account, as a
  holder of the registry's pull account may: register in an account no wider than the registry's
  readers. The bucket or container is the publisher's to keep private, and deleting the upload is
  the cloud's delete: a bucket that keeps deleted objects (Cloud Storage soft delete, S3
  versioning, Azure blob soft delete or versioning) keeps the disk restorable for that time. An
  upload is deleted whatever fails after it is made (`ship` names it when even that fails), and a
  run killed once its image was made deletes its upload, and the disk it downloaded, when resumed. Each step is a built-in OpenTofu module, pinned to its provider's exact release
  (`hashicorp/aws` 6.69.1, `hashicorp/azurerm` 5.10.0, `hashicorp/google` 8.7.0, `scaleway/scaleway`
  2.91.0), run by the `tofu`
  on `PATH`: `ship` and `promote` stop before anything else when it is missing or a cloud's
  settings are incomplete, while `yank` and `prune` go on. Each image has a working directory
  `$XDG_CACHE_HOME/stowage/cloud/<cloud>/<settings digest>/<image>/` (`~/.cache` by default; the
  settings' digest keeps another project, region, bucket or resource group apart — not other
  credentials on the same settings), holding the image module (`main.tf`, and
  `.terraform.lock.hcl`: the providers' checksums, so a provider is used only when it matches),
  its variables (`terraform.tfvars.json`: the image's name, the settings, the description, where
  the upload is; never a credential), OpenTofu's state, the disk while it uploads, the upload
  module in `upload/` (its variables name the disk's path and SHA-256; its state is empty once the
  upload is deleted), and once registered the image's id in a file `id`; the providers are kept
  once in `$XDG_CACHE_HOME/stowage/cloud/plugins`, shared by every working directory and checked
  against the lock files. So an image is registered once: before uploading, `ship` takes the kept
  id, else the id of a run stopped once its image was made (deleting its upload then), else looks
  the image up by name in the account (a probe module in `probe/`: a ready Compute Engine image, an
  available AMI the account owns, an Azure image of the resource group, whose description — a tag
  on Azure — names this disk's digest; on Scaleway, which lists images by name alone, the image of
  that name once a Block Storage snapshot of the name is tagged with that description, and a
  snapshot so tagged left without its image — deleted by hand, or a run stopped between them whose
  working directory is gone — fails the probe, naming the snapshot, until it is deleted by hand; one another machine registered, or whose directory was
  removed, is taken as it is; one of that name not ready — failed, or still being made — or another
  disk's stops `ship`, naming it, to be waited for or deleted). The account is trusted: an image of
  that name and description made there by anyone is taken. The kept id is trusted too: to
  register an image again, run `tofu destroy` in its working directory (it removes the image and,
  on AWS and Scaleway, its snapshot), then remove the directory. A later `ship`, `promote` or re-run prints
  `cloud <cloud>: image <id>` and uploads nothing; a run stopped part way goes on from its states:
  once the image module's state holds a resource (a snapshot, an import), the next run still
  looks the image up in the account and takes it when found (one the stopped run made without
  recording it, or another machine's), and otherwise — none found, or on Scaleway a lookup failed past `tofu init` (as it
  does on the run's own snapshot, made without its image), which is then logged (`…: going on from
  the stopped run's states in <dir>`); any other failed lookup stops `ship` — uploads again and
  applies the states (a state `tofu state list` cannot read stops `ship`: `cloud <cloud>: read the state
  of image <image> in <dir>: …`). On Scaleway a failed lookup past `tofu init` ends with `when
  OpenTofu's error above is that no image has the name, the Block Storage snapshot <image> was
  left without its image: delete it, then ship again` — a run killed during the snapshot's
  import, before OpenTofu recorded it, leaves such a snapshot — and an image not ready is deleted
  with the snapshot of its name.
  OpenTofu runs with the publisher's environment less every `STOWAGE_`, `COSIGN_` and `TF_`
  variable, the registry accounts' token variables, an `env://` signing key's and, for a
  `hashivault://` key, every `VAULT_` variable; plus `TF_IN_AUTOMATION=1`, `TF_INPUT=0`,
  `TF_PLUGIN_CACHE_DIR` and `TF_CLI_CONFIG_FILE` naming an empty `$XDG_CACHE_HOME/stowage/cloud/tofurc`
  (the publisher's `~/.tofurc` is not read). Its output goes to `ship`'s standard error. A cloud
  listed without any setting is skipped (`cloud <cloud>: not uploaded (set …)`); a failed
  registration fails `ship` before the release is published.
  Each is an artifact of type `application/vnd.stowage.disk.v1`, annotated
  `dev.stowage.disk.format`, holding one file compressed with zstd (level 3): media type
  `application/vnd.stowage.disk.<format>+zstd`, titled `<name>-<version>.<ext>.zst` (`oras pull`
  writes it under that name; `zstd -d <name>-<version>.<ext>.zst` gives the disk), annotated
  `dev.stowage.uncompressed.digest` with the disk file's own `sha256:` digest. `vm create` and
  `ship`'s cloud upload decompress it as they download it, into the disk file, and refuse it when
  that file is not of that digest; a disk of the plain media type
  `application/vnd.stowage.disk.<format>` (a release before 0.5.0) is read as it is. Each disk is
  kept on the bundle's digest, its format, the disk recipe and its compression, so adding a format
  makes only that disk (and the first `ship` after 0.5.0 makes every disk again, compressed). A disk holds the EFI,
  OEM and recovery partitions; its first boot resets from recovery, creating the active system and
  the persistent partition.
- **Reserved keys**: secret, input and safe-default keys are environment variable names, unique
  across all three. They may not start with `STOWAGE_`, `COMPOSE_` or `DOCKER_`. They also may not
  be a variable the Docker client reads from its own environment (`PATH`, `HOME`, `USER`, `TMPDIR`,
  `TERM`, `LANG`, `TZ`, the proxy variables, …).
- **Provision hook** (`hooks.provision`, provider `container`: `image` an `images` key, `cmd`,
  `env` required, optional `returns` and `outputs` a file name): Stowage runs `cmd` as the container's whole
  command line (the image's entrypoint is not used). On Docker and the VM it runs in a service it
  adds (`stowage-provision`), on the project's `default` network, as the image's own user when it declares one other than root, else as uid and gid 65534 (no hook is given root; a hook so run that fails says it ran so and that the fix is the image's: `give the image a USER of its own, non-root, that its program runs as`; the image's user read from the engine, `docker image inspect`, the image pulled first when the engine no longer holds it, another failure to read it failing the run: `the hook's image <ref>, whose user it runs as: …`), with no capability,
  `no-new-privileges`, a read-only root filesystem (`/tmp` a `tmpfs`, its only writable path) and no host file but the file inputs its `env` names (see "File inputs" ([installer](installer.md)) under the installer); on Kubernetes as a Job (see the installer on Kubernetes).
  `env` lists the keys it receives as environment — each an input, a secret or a safe default
  (lint: anything else is an error; `[]` for none; a key listed twice or not a variable name is an
  error) — of those the install records; besides them it always receives the values Stowage
  provides (on Docker and the VM `STOWAGE_NAME`, `STOWAGE_PROJECT_VERSION`, `STOWAGE_REVISION`,
  `STOWAGE_LOOPBACK_ADDRESS`, `STOWAGE_TLS_MODE`, `STOWAGE_IPV6`, each route's `STOWAGE_URL_<ROUTE>` and each `l4` route's `STOWAGE_L4_<ROUTE>`; on
  Kubernetes the first three) and every value it returned before. No other input, secret or safe
  default reaches it. It must be idempotent (it runs on every install
  re-run), print progress on stderr and only `KEY=VALUE` lines on stdout (at most 1 MiB a run,
  lines of 64 KiB, 30 minutes; 256 keys and 256 KiB kept across runs), printing a value it mints
  before the step that depends on it; a last line without a line break counts only when the hook
  exits 0. A returned key may not
  be one the install records, nor start `STOWAGE_` or `COMPOSE_`; a refused line fails the run
  after the others are recorded. Generated passwords are the hook's to mint and return. A hook
  that finds a value it returned before no longer true (a credential the app's own users revoked,
  data restored from elsewhere) fails, naming `provision --set KEY=VALUE` (or `--set KEY=`, which
  forgets the value so the run mints another): that command replaces the recorded value before the
  hook runs, on every shape, and the installer's own failure message names it too. `returns`
  (optional) lists keys the hook may return; the hook may still return others, and nothing
  requires it to return these. `provision --set KEY=VALUE` accepts a key it lists even when the
  hook never returned it, so a hook that finds an account it did not create (a database restored
  or shared from another install, a value the operator made it forget) can fail naming the
  `--set` that gives it the credential instead of minting one. Lint: a `returns` key that is an
  input, a secret, a safe default or a route's domain input is an error (the install records it),
  as is a key listed twice, one that is not a variable name, or one starting `STOWAGE_` or
  `COMPOSE_`.
- **Front**: when `tls_modes` offers `none` (loopback mode), every route has a `loopback_ports`
  entry (distinct ports); `loopback_env` relaxes a `safe_defaults` key in loopback mode only (plain
  HTTP on the operator's machine). A project that does not offer `none` owes no loopback port, and
  either key is then an error (no install would use it).
- **Images**: keys are lowercase words joined by single `-`. Built (`context` + `dockerfile`,
  optional `target` naming a stage, `args`, `add_hosts`, `network`, `inputs`) or pinned upstream
  (`ref: name:tag@sha256:<64 hex>`). Every `FROM` of a built image is pinned by digest (or names an
  earlier stage, or `scratch`). An image whose Dockerfile declares `ARG STOWAGE_PROJECT_VERSION`
  receives the project version; any other image is re-tagged, not rebuilt, on a version bump.
- **Paths** in the spec are relative to the project root and stay inside it (the one exception is
  `docker-config`'s `config`: a Docker config directory, the one holding `config.json`, on the
  publisher's machine).
- **Build contexts** that contain `.git` exclude it in their `.dockerignore` (`**/.git` also covers
  submodules).
- **The front** routes by hostname, then by path: each route maps a `domain_input` (and, for
  routes sharing one, a `path` prefix) to `<service>:<port>`; `front.portal` names the route whose
  URL the installer prints at the end.
- **Signing** (`cosign-key`): `key` and `public_keys` are required; each public key is a project
  file holding a PEM `PUBLIC KEY` block (as `cosign generate-key-pair` writes it).
- **Providers**: every project-specific concern is `{ provider: <name>, …config }`
  ([DESIGN § 2.1](DESIGN.md#21-one-pattern-for-everything-project-specific)). Version source,
  scheme, changelog and signing have defaults; registry auth, health and schema compat must name
  their provider. Unknown config keys are errors.
- **`command` provider** (the escape hatch of every concern): one JSON request on stdin
  (`{"op": "...", ...}`), one JSON answer on stdout, exit `0` success, `2` "no answer" (read as
  unknown, never as a negative), anything else an error. It runs in its own session with no
  controlling terminal (it cannot prompt) under `timeout_s` (default 30); anything it leaves
  running is killed, and Ctrl-C / Ctrl-Z reach it through Stowage. Ops: `version.get`, `scheme.validate` (must answer `{"valid": true|false, "reason": "..."}`),
  `scheme.compare`, `changelog.versions`, `changelog.section`, `schema.current`,
  `schema.barrier_after`, `schema.reads_newer`, `health.ready`.
- **Health** (`health`) is the readiness the VM's health gate and `<short>-stow health` check, with
  every service of the stack running (healthy, when it has a healthcheck) or exited 0, judged as
  the Docker installer judges a start (a healthcheck still starting is reported as `S is
  starting`, which the gate's next poll asks again). `http-status`
  and `http-json` send `GET path` to the service's container at its address on the stack's
  network (the first network by name that gives one; `127.0.0.1` for a service on the host's
  network, as the VM runs an L4 route's and `host_network` runs one), within 10 s: ready is the status `expect`
  (`http-status`; a redirect is not followed, no proxy is used, no connection kept), or a 2xx JSON object carrying every field of `expect` with the same value
  (`http-json`, an answer of at most 64 KiB). `command` runs `cmd` in its image, confined like the
  schema command (a `stowage-health` service, a container of its own per call, its compose file
  `.stowage-health-<id>.yaml` in the install directory, removed after the call; the health gate
  removes those an interrupted call left, once older than an hour):
  `health.ready` → `{"ready": bool, "reason": "..."}`; no answer (exit 2) is not ready.
- **Schema compat** (`database.schema_compat`) decides whether `rollback` may go back after the
  database moved. `sql`: `current` returns the migration number; `barrier_after` (with `$1`, the
  number recorded before the upgrade) returns false or nothing when no rollback barrier came
  since, else true or the barriers' description; `reads_newer_label` is a label an image of
  `reads_newer_image` carries set to `true` when it reads a schema newer than its own (its absence
  means it does not). The queries run with `psql` in the database service as the migrate role.
  `command` runs `cmd` in its image, confined like the provision hook (a `stowage-schema`
  service, a 64 KiB answer; its environment is every recorded input, secret and safe default,
  each route's URL and domain, and the values Stowage provides — not narrowed by an `env` as the
  hook's is): `schema.current` → `{"version": N}`,
  `schema.barrier_after` `{"version": N}` → `{"barriers": [...]}`, and `schema.reads_newer`
  `{"version": N}` → `{"reads_newer": bool}`, run in the previous release's image with the
  previous release's own provider. Migration numbers are whole numbers of at most 15 digits
  (leading zeros dropped), sent as JSON numbers.
- **Versions** are opaque to Stowage: the scheme provider (`calver`, `semver`, `regex`, `command`)
  validates, orders and renders them as tags. A version is its own tag, never rewritten, so it must
  be a legal OCI tag; a SemVer release carries no build metadata (`+…`). A channel's `tag` template
  holds `{v}` once; a revision above 1 renders as `{v}-r<N>`, so a version whose tag already ends in
  `-r<digits>` is refused. Channels write into the same repositories, so no channel's tag may be
  another's: under SemVer use a prefix (`beta-{v}`), not a suffix (`{v}-beta`, a pre-release).
- **Interpolation** that needs a value is `${X}`, `$X`, `${X:?msg}`; `${X:-v}`, `${X-v}`,
  `${X:+v}`, `${X+v}` are defined when X is unset.
- **`stowage.lock`** (next to `stowage.yaml`, committed; written by `stowage lock`). It is
  decoded strictly and holds only `stowage: <SemVer release, no build metadata>`: the installer,
  `<short>-stow`, the host daemon and the recipes of the front, the backup runner and the volume
  helper are all part of that `stowage`, and `ship` builds them into the project's own registry
  (there is no Stowage registry to pin pieces from). Without it lint reports
  an error, and `ship` and `promote` refuse; they also refuse a `stowage` of another version than
  `stowage:`, before reading the registry, naming the pinned `stowage` to use (`ship` with a newer
  `stowage` also offers `stowage lock --update`). Lint also reports a pin `stowage.requires` does
  not accept.
- **`stowage.requires`** is the range of Stowage versions the project accepts: comparators
  separated by spaces, all of which must hold — `>=`, `>`, `<=`, `<`, `=` and a version
  `MAJOR[.MINOR[.PATCH]]` (missing parts are 0, no pre-release), e.g. `">=0.8 <1"`. Versions
  compare as SemVer, field by field as numbers (0.10 comes after 0.9). Every `stowage` command
  that reads the project (`lint`, `plan`, `ship`, `promote`, `lock`, the hook commands) reads it
  first, before the rest of the spec, so a `stowage` outside the range refuses with
  `stowage.yaml: stowage.requires accepts Stowage <range>, and this is stowage <version>: run a
  Stowage it accepts` rather than stopping on a key it does not know. A range that does not parse
  is a lint error (so `ship` and `promote` refuse it). A build whose version is not a SemVer
  release (`dev`) is not checked.
