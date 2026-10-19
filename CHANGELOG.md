# Changelog

Stowage's own releases. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [SemVer](https://semver.org/).

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
