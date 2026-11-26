# Creating a VM (`<short>-stow vm create`)

[README](../README.md) · [Documentation index](../README.md#documentation)

A project with the `kairos` shape whose release publishes disk images gets `vm` commands in the
same installer (bootstrap it as above, then `sh <name>.sh vm …`, or run the kept `<short>-stow`).
Stowage owns no infrastructure state: it never creates a VM. `vm seed --json` prints what a new
VM of the release needs at its first boot, and an OpenTofu root of yours creates the VM from it
(see [Creating the VM with OpenTofu](#creating-the-vm-with-opentofu)); `vm create` writes the same
disk and seed as files and prints the platform's steps by hand. Every verb that reaches the VM
over SSH from your machine takes its host key with `--known-hosts-line`.

```bash
<short>-stow vm create [--platform P] [--out DIR] [--hostname H] [--user U] [--ssh-key FILE]
          [--network dhcp|static] [--address A/P] [--gateway G] [--dns D,…]
          [--ipv6 [--address6 A/P] [--gateway6 G]] [--tune-profile default|network]
          [--ssh-allow CIDR,…] [--timezone ZONE] [--device DISK] [--interface ROLE=SPEC]…
          [--add-host NAME=IP]… [--secrets FILE|-]
          [--tls MODE] [--cert FILE --key FILE] [--ca FILE] [--client-ca FILE]
          [--domains route=host,…] [--http-port N] [--https-port N]
          [--set KEY=VALUE]… [--set-file KEY=PATH]… [--answers FILE] [--no-input] [--accessible]
          [--registry PREFIX] [--registry-ca FILE] [--registry-login-file FILE]
<short>-stow vm seed --json [--platform P] [--hostname H] [--user U] [--ssh-key FILE] [--channel C] [--await-secrets] [--disk-dir DIR]
          [the machine's flags, as vm create's: --network … --device DISK, --interface ROLE=SPEC…, --add-host NAME=IP…]
          [--tls MODE] [--domains route=host,…] [--http-port N] [--https-port N]
          [--set KEY=VALUE]… [--set-file KEY=PATH]… [--answers FILE] [--registry PREFIX] [--registry-ca FILE] [--registry-login-file FILE]
                                                   # a VM's first-boot material as JSON, the VM made by your own means
<short>-stow vm host-key HOSTNAME --json --known-hosts-line LINE [--user U] [--ssh-key FILE] [--host H]   # the VM's own SSH host key; the seed's one-time key retired
<short>-stow vm kit create|check|take HOSTNAME … --known-hosts-line LINE   # a handoff kit issued on the VM or taken by it, over SSH (see [kits](kits.md))
<short>-stow vm truenas import-image|seed|stop [--timeout DURATION]   # TrueNAS steps an OpenTofu root runs (see "The TrueNAS steps")
```

- `vm create` makes no VM: it writes the files and prints the steps. An OpenTofu root makes one
  from `<short>-stow vm seed --json` (the OpenTofu provider's examples,
  `norvexlabs/terraform-provider-stowage` `examples/guides/<platform>-vm`).
- A project whose front offers no mode a VM can take (only `none`, or `byo` and no certificate given) is refused first, saying to give `--cert` and `--key` when it offers `byo`.
- **`vm seed --json`** prints, as one JSON object, what a new VM needs at its first boot, for a
  tool that creates the VM with its own means (a hypervisor's or a cloud's own provider): it
  creates nothing, never asks, and writes no file but the disk `--disk-dir` asks for. It takes `vm create`'s machine flags
  (`--platform`, required, `--hostname`, `--user`, `--ssh-key`, `--network` and its addressing,
  `--ipv6`, `--tune-profile`, `--ssh-allow`, `--timezone`, `--device`, `--interface`,
  `--add-host`), the front's (`--tls`, `--domains`, `--http-port`, `--https-port`), `--set`,
  `--set-file` and `--answers` (inputs that are not secret, an answers file's `tls` section, and
  the secret inputs it holds, which the first install waits for over SSH, as `vm create`'s does),
  `--registry`, `--registry-ca` and `--registry-login-file` (checked as `vm create` checks it;
  the seed names its host alone, `registry_login`, and the program gives the file over SSH once
  the VM answers, `sudo <short>-stow registry-login -`), `--channel` (the channel the first install follows; default
  the release's; another channel must hold a release of the project in the registry the seed
  names, else it is refused: `--channel C: the registry P holds no release of <name> on C (this
  release is on <channel>)`), and its own: `--await-secrets` (the first install waits for a decrypted
  `secrets export`, given over SSH once the VM answers, `sudo <short>-stow install --secrets -
  --no-input`, as `vm create --secrets` makes it; a machine made again, or a new install's
  secrets file without `export: true`, a plain install: see `--secrets` in [installer](installer.md)), `--tls byo` (it waits
  for the certificate, its key and CA, and the secret inputs, given over SSH, `sudo <short>-stow
  install --given - --no-input`, its front serving that certificate; the release must offer
  `byo`), and `--disk-dir DIR` (an existing directory `--platform`'s disk is written into,
  decompressed, as `vm create` fetches it; a file of that name already holding it is kept).
  `--await-secrets` with `--tls byo` or with a secret input the answers file holds is refused by
  `vm create`'s one rule (`--await-secrets and <names>: the first install waits for one of them
  over SSH, a secrets export (install --secrets -) or what install --given - gives`, `<names>` the
  inputs, then `--tls byo`; its fix: `remove <inputs> from the --answers file and leave out --tls
  byo, or leave out --await-secrets`, naming only what was given), as is a
  `--disk-dir` that is no directory, and a certificate, key, CA or client CA an answers file's
  `tls` section gives (`none` aside; the seed takes none of `--cert`, `--key`, `--ca`,
  `--client-ca`): the seed carries none (a path the file names that does not exist fails first,
  as the answers file is read). A release whose
  front offers no mode a VM is made with, but `byo`, is refused naming `--tls byo` as the fix (`vm
  create`'s names `--cert` and `--key`; both exit 3). `disk_file`'s path is absolute, whatever
  `--disk-dir` was given as; `--json` is required. It is the seed `vm create` writes,
  made by the same steps and refused as `vm create` refuses (a release with no
  OS bundle or no disk, a front with no mode a VM takes, `--platform` missing or not offered, an
  input missing or refused), and TLS material given to it, or a secret input on its command line
  (`--set`, `--set-file`), is refused, naming them (`<names>: a seed carries no secret input and
  no TLS material; they go after the boot, over SSH`), never printing a value: they go once the
  VM answers, `sudo <short>-stow install --given - --no-input` over SSH. The first install waits
  over SSH by `vm create`'s one rule: for a secrets export with `--await-secrets`; for the secret
  inputs `--answers` holds (named in `await_given`) and, with `--tls byo`, the certificate, in one
  `install --given -` file (`project: <name>`, `inputs: {KEY: value}`, and its `tls` section),
  past the first boot's switch to this release's OS when its disk carries another; the seed never
  holds a value. A cloud platform's `--ssh-allow` default is this machine's
  public address, as `vm create`'s.
  ```json
  {
    "format": 1,
    "platform": "truenas",
    "user_data": "#cloud-config\n…",
    "known_hosts_line": "vm-seed-host-key ssh-ed25519 AAAA…",
    "fingerprint": "SHA256:…",
    "host_key_private": "-----BEGIN OPENSSH PRIVATE KEY-----\n…",
    "meta_data": "instance-id: notes-1\nlocal-hostname: notes-1\n",
    "disk": {
      "truenas": { "format": "qcow2", "image": "<prefix>/<name>-disk@sha256:…", "os_gib": 23, "disk_gib": 43 },
      "aws": { "format": "raw", "image": "…", "os_gib": 23, "disk_gib": 43, "cloud_image": "<name>-0123456789ab" }
    },
    "app_gib": 20,
    "disk_file": { "path": "/abs/DIR/<the disk's file name>.qcow2", "format": "qcow2", "sha256": "…", "disk_gib": 43 },
    "await_secrets": true,
    "waiting": ["API_KEY"],
    "interfaces": [
      { "role": "edge", "mac": "02:5e:41:0c:9a:17", "attach": "br1" },
      { "role": "uplink", "mac": "02:8b:d2:44:01:6e", "primary": true }
    ],
    "size": {
      "cpus": { "min": 2, "recommended": 4 },
      "memory_mib": { "min": 2048, "recommended": 8192 },
      "app_gib": { "min": 20, "recommended": 60 }
    },
    "firewall": {
      "ssh_sources": ["0.0.0.0/0", "::/0"],
      "open_ports": [
        { "protocol": "tcp", "port": 443, "sources": ["0.0.0.0/0", "::/0"] },
        { "protocol": "udp", "port": 51820, "sources": ["198.51.100.0/24"] }
      ]
    }
  }
  ```
  `user_data` is the cloud-config for `--platform` (a NoCloud seed's `user-data`, or a cloud's
  user data); it carries no secret input, but the one-time SSH host key it makes the VM serve
  until its install and the reboot after it, as `vm create`'s does for what it gives over SSH.
  `interfaces` are the NICs the VM is made with, sorted by role, which the program creating the
  VM gives it: the release's `host.interfaces` NICs, an optional one only when `--interface`
  names it (`[]` when that leaves none). Each one's `role`, the `mac` its cloud-config names it
  by, the network it `attach`es to (`attach=`; left out: the primary's; for the primary, the
  network the program making the VM gives it) and `primary` on the primary NIC. `mac` is:
  - with no `host.interfaces`: no NIC is listed, `"interfaces": []`; the VM has one NIC, of the
    platform's own MAC, which the cloud-config leaves the kernel's name;
  - one NIC role on a hypervisor: left out, unless `--interface ROLE=mac=…` gives it. The NIC is
    made with any MAC (the platform's own), the cloud-config names it by its original name
    (`en*`, `eth*`), and the VM's first boot pins that name to the NIC's MAC (below);
  - several NIC roles on a hypervisor: present on each, given by `--interface ROLE=mac=…` or
    generated (a locally administered unicast MAC drawn at random by each run);
  - on metal: present on each, given by `--interface ROLE=mac=…`, the server's own;
  - on a cloud: left out; the cloud gives its one NIC its MAC, named and pinned as a
    hypervisor's one NIC.

  A `mac` printed is the one the VM's NIC must have: the program making the VM gives each NIC
  device the `mac` of its role, and the VM is made from the seed that printed it (each run draws
  new ones). Samples of `interfaces`:
  ```json
  "interfaces": []
  "interfaces": [ { "role": "uplink", "primary": true } ]
  "interfaces": [ { "role": "uplink", "mac": "02:00:00:00:00:0c", "primary": true } ]
  "interfaces": [
    { "role": "edge", "mac": "02:5e:41:0c:9a:17", "attach": "br1" },
    { "role": "uplink", "mac": "02:8b:d2:44:01:6e", "primary": true }
  ]
  ```
  The first is a release with no `host.interfaces`; the second one NIC role on a hypervisor or a
  cloud; the third the same role with `--interface uplink=mac=02:00:00:00:00:0c` on a hypervisor;
  the fourth several NIC roles on a hypervisor.
  `known_hosts_line` is that key under the name `vm-seed-host-key`, which `--known-hosts-line`
  takes (below); `fingerprint` its SHA256 as ssh prints it; `host_key_private` the key itself,
  an OpenSSH private key, which the caller keeps secret until the key retires (`vm host-key`
  retires it). `meta_data` is the NoCloud `meta-data` for a hypervisor that builds its own seed
  ISO beside `user_data` (`instance-id` and `local-hostname`, the host name, as the seed ISO's).
  `disk` names, for each platform the registry holds a disk for, the disk that platform boots: its
  format, as the table below, its image by digest (in the mirror with `--registry`), `os_gib`
  what Kairos takes of a VM's disk of it, and `disk_gib` the VM's disk size to create, `os_gib`
  plus `app_gib`, the app's space the release recommends (else 20, at least its minimum and 4),
  as `vm create` sizes it; on a cloud the release publishes to (`cloud_publish`) in that cloud's
  format, `cloud_image` is the name `ship --disks` registers that disk's image under there,
  `<name>-` and the first 12 hex digits of the disk's digest, which that cloud's own provider
  looks the image up by: the ship that published the disk registered it only when it had that
  cloud's settings, so a consumer checks the image exists. Left out for a release booting another
  release's disks (the channel's set), whose ship's `cloud_publish` this release does not tell. `disk_file`,
  with `--disk-dir`, is `--platform`'s disk as a file — named as `ship` named the disk (its
  artifact's title, the name `vm create` writes it under) —: its absolute path, format, the SHA-256 of its
  bytes (decompressed: what a hypervisor imports) and `disk_gib`; left out otherwise.
  `await_secrets` (`--await-secrets`) and `await_tls` (`--tls byo`) say what the first install
  waits for over SSH, left out when false; `await_given` names, sorted, the secret inputs the
  `--answers` file holds (never a value; left out when none), which it waits for too, given over
  SSH (`sudo <short>-stow install --given - --no-input`, with the certificate when `await_tls`). `registry_login` (`--registry-login-file`) is the
  host the login is for, never the login; left out without it. `waiting` names, sorted,
  the keys of the required secret inputs nothing gives (never a value; left out when there are
  none): the VM's first install waits for them, as `vm create` says, until they are given over
  SSH (`sudo <short>-stow install --given - --no-input`). `--platform` missing, not a platform
  Stowage knows, or one the registry holds no disk for is refused (exit 3) once the registry has
  been read, its fix naming the platforms that registry offers; a registry that cannot be read,
  or that offers no disk for any platform, is a failure (exit 1), whatever `--platform` says. A
  refusal prints one object on standard output, as `status --json`'s (`{"format": 1,
  "refused": {"reason": "…", "fix": "…"}}`), and exits 3; a failure prints none.
  `size` is the VM's size the release takes (`shapes.kairos.size`): for its CPUs, its memory in
  MiB and the app's space on its disk in GiB (to which each disk's `disk_gib` adds Kairos's
  share), `min` is the least — the floor every VM needs (1 CPU, 1024 MiB, 4 GiB) raised by
  `size.min` — and `recommended` the default — `size.recommended`, else 2 CPUs, 4096 MiB and
  20 GiB, raised to that least (`app_gib.recommended` is the top-level `app_gib`). A program
  creating the VM refuses a size under `min` and warns under `recommended`, as the provider's
  examples do.
  `firewall` is what a platform's own firewall in front of the VM (a cloud's security group or
  firewall rules) lets through, as the VM's own firewall opens it: `ssh_sources`, the CIDRs SSH
  accepts (`--ssh-allow`; every source, `0.0.0.0/0` and `::/0`, without it), and `open_ports`,
  each port the VM opens besides SSH with its `protocol` (`tcp` or `udp`), `port` and `sources`:
  the front's HTTP and HTTPS ports something serves on in the `--tls` mode given (both when none
  is given: the VM's install may take any), from every source, and each L4 route's port — but
  one the front terminates TLS on in loopback mode — from its `allow_input` list's value given
  (`--set`, `--answers`), else the input's default, each address a network's prefix (an
  IPv4-mapped address as IPv4, one another holds left out), sorted by family; from every source
  for a route without a list, one the front terminates TLS on (the front filters its port), a
  secret list, or a list the install refuses; `[]` for an empty list. A platform's rules made
  from them are fixed when applied: a later change of the list on the VM does not reach them (the
  VM's own firewall follows it).
- **`vm host-key HOSTNAME --json`** reads the VM's own ed25519 SSH host key over SSH, checked by
  the key `--known-hosts-line` gives (the seed's `known_hosts_line`, or the VM's own), and retires the seed's one-time key at once — its file and sshd drop-in
  removed, its retirement marked on the OEM partition, sshd reloaded — so the VM serves its own
  key alone from then on (as its first boot after the install would). It takes `--user`,
  `--ssh-key`, `--host` (the address, default HOSTNAME) and `--known-hosts-line`, and prints
  `{"format": 1, "public_key": "ssh-ed25519 AAAA…", "known_hosts_line": "<host name> ssh-ed25519
  AAAA…", "fingerprint": "SHA256:…", "retired": true}`: the VM's key, and under the host name the
  run checked it by (the line's) its known_hosts line: the next run's `--known-hosts-line`. The retirement's sshd
  reload is checked: a running `ssh.service` or `sshd.service` reloaded, or none running and
  `ssh.socket` there to start one (which reads its configuration per connection); one that did
  not take prints the key all the same with `"retired": false` and a `reason`, and fails the
  verb (exit 1: `the seed's one-time host key's files are gone, but sshd did not reload: it may
  serve that key until it does; `sudo systemctl reload ssh` (or reboot the VM), then run
  host-key again`). Stowage keeps no record of the VM: the line given is written to a file of
  its own (as for every verb taking it), and the printed line, the VM's key from then on, is
  yours to keep. Run again it changes nothing more, through the VM's own key. Refused (exit 3,
  nothing changed): the VM answering from its recovery or live system (its first boot not done:
  the one-time key is kept, as the active system serves it; run it again once it rebooted), ssh
  not reaching the VM through the key (exit 255: the VM down, the key not authorized, or a VM that
  rebooted since its install serving its own key alone — the fix names the console's check,
  `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`), and the reach's own errors (no
  `--known-hosts-line`, a bad `--known-hosts-line`, `--user` or `--host`). A refusal prints its object as `vm seed`'s.
- **`sudo <short>-stow host-key [--json]`**, on the VM itself, runs the same steps locally (no
  SSH, so no SSH config of the operator's): it prints the VM's own ed25519 host key as a
  `known_hosts` line under the VM's host name (`<hostname> ssh-ed25519 AAAA…`), or with `--json`
  `{"format": 1, "public_key": …, "known_hosts_line": …, "fingerprint": "SHA256:…", "retired":
  true}`, and retires the seed's one-time key at once (as root, its steps run without sudo), its
  sshd reload checked as `vm host-key`'s (not taken: `"retired": false` with a `reason`, exit 1,
  the fix `sudo systemctl reload ssh` or a reboot); run again it changes nothing more.
  Refused (exit 3, nothing changed; with `--json` the refusal as one object, as `status
  --json`'s): on Kairos's recovery or a live boot, told first (`this is Kairos's recovery system
  or a live boot, whose host keys are not the active system's: the one-time key is kept`; run it
  again once the VM rebooted into its active system), not as root (`host-key retires the seed's one-time host key,
  which root alone can`, the fix `run the same command with sudo`), before the first install
  (`absent`), and while it is not done, still running or failed (it may still wait for secrets or
  a certificate given over SSH, pinned to the one-time key; the install retires it itself). A step that fails (the mark on `/oem` not
  written, say) fails the verb (exit 1) naming it; the steps run again whole. The host name is
  taken as ssh matches it (lower-cased, without a root dot); one the line cannot hold (none,
  `localhost`, one `vm create --hostname` refuses, which a known_hosts line would read as a
  pattern) leaves the line out — the key and its retirement are no name's: the public key is
  printed (as the line, without `--json`), and a `note` (on standard error without `--json`)
  says why and names `hostnamectl set-hostname NAME`. The steps
  are `vm host-key`'s over SSH (with sudo); the first boot after the install runs the same file steps, its reload unchecked (an sshd not started yet reads
  its configuration as it starts).
- The platform decides the disk format (the first of its formats the registry holds: a mirror
  carries the formats its `mirror export --disks` named) and how the cloud-config reaches the VM.
  Only the platforms the registry has a disk for are offered: asked on a terminal (no
  `--platform`), every platform a disk set the VM may boot holds a format of — the release's own
  set, else the channel's release's or its disk set record, whichever holds the chosen platform's
  format — and that set is the one the VM then boots; a source that cannot be read or checked
  (a disk set record that is malformed or not signed by a trusted key, a registry error) ends that
  look-up without failing the question, which offers the platforms of the sets read before it and
  prints `note: the platforms offered are those of the disk sets read before one that could not
  be: <why>` (with none read before it, the run fails with that error, as with `--platform`); a platform none of those
  holds is then looked up as `--platform` does, so the question never fails where `--platform`
  succeeds; a `--platform` whose formats the
  release publishes but the registry lacks is refused (exit 3) with `--platform P: the <format> disk
  <digest> is not in <prefix> (…)` and the platforms it does offer:

  | `--platform` | disk | cloud-config |
  |---|---|---|
  | `truenas`, `proxmox`, `libvirt` | `qcow2`, else `raw` | `<hostname>-seed.iso` |
  | `vmware` | `ova` | `<hostname>-seed.iso` |
  | `hyperv` | `vhd` | `<hostname>-seed.iso` |
  | `azure` | `vhd` | `<hostname>-user-data.yaml` (custom data) |
  | `gcp` | `gce` | `<hostname>-user-data.yaml` (`user-data` metadata) |
  | `aws` | `raw` | `<hostname>-user-data.yaml` (user data) |
  | `scaleway` | `qcow2` | `<hostname>-user-data.yaml` (`cloud-init` user data, which the OS bundle's datasource reads from Scaleway's metadata service; it writes the project's SSH keys there for root too, whose login sshd refuses: the OS bundle's build fails when sshd would take it) |
  | `metal` | `iso` (the installer) | `<hostname>-seed.iso`, on a second USB stick |

  The seed ISO is a NoCloud seed (ISO 9660 with Rock Ridge and Joliet names, labelled `cidata`,
  holding `user-data` and `meta-data` — `instance-id` and `local-hostname`, the host name; the
  volume's sectors whole and 300 KiB of zeros after them: VMware Workstation's CD-ROM reads no
  image that ends inside a sector), which
  Kairos reads from the VM's CD-ROM at its first boot; a cloud's user-data is read from its
  metadata service. The first boot keeps what it read as `/oem/95_userdata`, and no later boot
  reads the seed or the metadata service again: a user-data changed after it (a new seed, a
  cloud's edited user data) is never applied — the VM's settings change through `<short>-stow`'s
  verbs (`network set`, a re-run of `install`). Both files, and the disk, are written 0644 (a hypervisor reads them as its
  own user; they carry no secret). A seed that exists already is never replaced (it may be
  attached to a VM): the run is refused before anything is asked to be written. Every stage of
  the seed runs on the installed system alone (`if: [ ! -e /run/cos/live_mode ]`): the installer
  ISO boots the OS live, with the seed, to install it.
- **Bare metal** (`--platform metal`, amd64): the disk is the installer ISO, written with the seed
  to two USB sticks (`dd if=<file> of=/dev/<stick> bs=4M conv=fsync`, or any image writer); the
  server boots the first in UEFI mode, Secure Boot off. `--device` (required on `metal`, asked on
  a terminal; refused on another platform) is the disk the OS installs on, wiped: a path under
  `/dev/` (letters, digits and `_ . : + @ / = -`, no `..`; a `/dev/disk/by-id/` path names it
  whatever the boot order), or `auto`, Kairos's choice: the largest disk, a data disk if it is the
  largest. The seed then holds an install block, `install: {auto: true, device: <DISK>,
  poweroff: true}`: the installer wipes the disk, installs the OS on it (the persistent partition
  the rest of the disk, which must hold the OS and the app's space, the steps' size) and powers
  the server off; the operator takes both sticks out and powers it on, and its first boot installs
  the release, as a VM's. The installer keeps the cloud-config it ran with on the installed system,
  which an `after-install-chroot` stage moves to `/oem/95_userdata/userdata.yaml` (a second
  source's `9N_custom.yaml` beside it; the stage fails without one), as a VM's first boot keeps it: no
  boot pulls the seed again, its stick left in or not, nor runs its stages twice. The machine's
  network is the seed's (its NICs as below).
- **The NICs** (`host.interfaces`): a release that names none has one NIC, whatever its name
  (`en*`, `eth*`), addressed by the machine's network. A release that names NICs has one each,
  named by its role: the seed's `initramfs` stage writes `/etc/systemd/network/10-<name>-<role>.link`
  (`[Match] MACAddress=<mac>`, `[Link] Name=<role>`), before the booted system's udev names the
  NICs, and its `boot` stage renames a NIC a first boot named before (down, renamed, up): the NIC
  of the role's MAC (a NIC of a device, never a virtual function beside it nor a bridge, as the
  pin below counts them; of several, none is renamed, logged `<name>: the interface <role>: the
  NICs <nics> all have its MAC (<mac>): which one is its NIC is not known, so none is renamed`),
  or, for a role named by its original name, the NIC of the default route, and
  that one only while the role is neither pinned (below) nor a NIC's name already, as a NIC added
  later may get the first default route. A name another NIC holds is never taken, the NIC left
  up (logged `<name>: the interface <role>: another NIC holds its name: <nic> keeps its own`), and
  a rename that fails brings the NIC back up (`<name>: the interface <role>: <nic> is not renamed
  so: it is brought back up`); the pin's rename below does the same. The
  primary's addressing is the machine's network (`05-<name>-static.network` matches its role's
  name, and so does `network set`'s override); each other NIC's is
  `/etc/systemd/network/10-<name>-<role>.network`, given by `--interface ROLE=SPEC` (repeatable):
  `dhcp` (an address, never the default route, the DNS servers nor the host's name: `UseGateway=no`,
  `UseRoutes=no`, `UseDNS=no`, `SendHostname=no`), `static,address=A/P` (an IPv4 address with its
  prefix), or `none` (the default: the link up, no address), then `mac=` (a unicast MAC) and, on a
  hypervisor, `attach=` (the platform's network or bridge, default the primary's); an `optional`
  role's NIC is made only when `--interface` names it (`--interface <role>=none` for the default
  addressing), else the machine has none and no MAC is drawn for it; no NIC waits
  for the boot (`RequiredForOnline=no`). Once the system runs, a NIC no role names — one added
  to the machine before `network attach` names it, or a port the machine has beyond its roles —
  **carries no address**: no DHCP, no IPv6 (no router advertisement, no link-local address), so
  no route. `/etc/systemd/network/15-<name>-unnamed.network` matches the kernel's names (`en*`,
  `eth*`; a role is never one) of physical NICs (`Kind=!*`), after the roles' files and before
  Kairos's DHCP default, which would configure it, so a NIC no role names is configured by no file
  sorting after it (a bond's port file of networkd's own sorts before it, or the port is left
  unaddressed, out of the bond); a NIC whose name is neither (one a udev rule names) is not
  covered, nor listed. The link stays up, and one the kernel or another program enslaved to another
  link — a failover's standby or virtual function, at boot or plugged later, a bond made outside
  networkd — keeps its master, which carries it (`KeepMaster=yes`); an accelerated networking's
  virtual function on Azure is the bundle's `00-<name>-sriov.network`'s, which leaves it
  unmanaged. A cable in the wrong port gets no address and no route; the link, up, is no isolation
  boundary (it answers ARP for the machine's other addresses, `arp_ignore=0`, and takes IPv4 to them,
  the loose reverse-path filter; IPv6's neighbour discovery answers for the link's own addresses
  alone, none), so a port that must reach nothing is unplugged or disabled in the
  firmware. `network` lists the NIC with its MAC, and
  the console's login screen says so (`/etc/issue.d/07-<name>-unnamed.issue`). The initrd still
  asks DHCP on every NIC for its few seconds (Kairos's, dropped at switch-root), and the first
  boot until its `boot` stage writes the file. Both are written at each boot only while the
  primary role's NIC is named, once udev has settled and the primary is there (each awaited up to
  30 s, so a NIC probed late is named first): a primary its rename missed (its NIC replaced, so
  its MAC no longer matches) keeps its kernel's name, and the boot then removes them, logging
  `<name>: no NIC is named <role>: none is left unaddressed, and DHCP configures each`, so the
  machine stays reachable on DHCP, recovery system included — at each boot about a minute later
  than before (the file an earlier boot left keeps the NIC unaddressed until that stage, past its
  waits), until the role is named again (a pinned primary's NIC replaced is pinned in its place by
  that same boot, below; one the seed names by its MAC needs a NIC of that MAC). A machine whose
  seed names no NIC role has no such file: every
  NIC is the primary's. On a hypervisor, a VM of several NIC roles has each
  named by its MAC: given, else generated (`02:` and 5 random bytes, drawn by each run and
  written into its seed, `vm seed`'s `interfaces` and `vm create`'s steps: the NICs take the MACs
  the run that wrote their seed printed). A VM of one NIC role (a hypervisor's, its MAC not
  given, or a cloud's) has it named by its original name: the `.link` names the NIC it has
  (`OriginalName=en* eth*`, an accelerated networking's virtual function left out by its driver,
  `Driver=!mlx4_en mlx5_core mana`, the network device's driver as udev records it,
  `ID_NET_DRIVER`, as this match cannot know the NIC's own), and its `boot` stage, after the
  rename, **pins it to its MAC and driver**. The VM's NICs are counted by their device
  (`/sys/class/net/<nic>/device`), a NIC another enslaves left out (`/sys/class/net/<nic>/master`:
  an accelerated networking's virtual function, enslaved to its NIC, has that NIC's MAC). The pin
  counts the free ones: a NIC whose MAC no attached role's `.link` file names
  (`/oem/96_<name>-interfaces.yaml`, `network attach`'s: another role's NIC is never taken), and
  not a virtual function beside its NIC (a NIC of `mlx4_en`, `mlx5_core` or `mana` with the MAC of
  a NIC of another driver, before that NIC enslaves it). With exactly one free NIC and that NIC
  named as the role, the pin replaces the seed's `/etc/systemd/network/10-<name>-<role>.link`
  with one matching that NIC's MAC and driver (`[Match] MACAddress=<its MAC>`, `Driver=<value>`;
  `[Link] Name=<role>`), the driver read from udev's record (`ID_NET_DRIVER` in
  `/run/udev/data/n<ifindex>`), the same rule as `network attach`'s: the NIC's own driver when it
  is one of `mlx4_en`, `mlx5_core`, `mana` (a physical NIC of one, on metal), else
  `!mlx4_en mlx5_core mana` (any other driver, or none udev records): a change of the NIC's model
  keeping its MAC (virtio_net to e1000) still matches, and a virtual function sharing that MAC
  never does. It writes `/oem/96_<name>-<role>-pinned.yaml`
  (synced before and after its rename), a cloud-config whose `initramfs` stage writes that file
  at every later boot, before udev names the NICs. The role then has one `.link` file, matching
  by its MAC and driver: a NIC added later, in any order, never takes the role's name. Every later boot,
  the seed's own `initramfs` step writing the file by the original name skips while the pin
  reads whole, so the pin's file alone writes it (yip also runs `/oem`'s files in lexical order,
  `96_` after the seed's `95_userdata`). The OEM file marks the pin done when it holds the pin's
  cloud-config whole, of a MAC and a `Driver=` value the pin writes: every later boot leaves it
  while its NIC is there (named the role at once when it is not, by the seed's rename's rule: a
  name another NIC holds is never taken), and an empty, truncated or otherwise altered one is
  written again. A
  boot that finds another free NIC there, or none named as the role, pins nothing and logs
  `<name>: the interface <role> is not pinned to its NIC's MAC: the NICs here no other role names
  are <nics>, not one NIC named <role>` (`none` when there is none; a NIC whose MAC does not read:
  `…: the NIC <nic> has no MAC`); the next boot tries again. Once pinned, a boot that finds no NIC
  matching the pin's MAC and `Driver=` value, as udev matches them (the NIC replaced on the
  platform), and exactly one free NIC pins that one in its place and names it the role at once
  (`ip link set dev <nic> down`, `name <role>`, `up`, as the seed's rename does: the pin's `.link`
  file, of the NIC gone, left it its kernel name at this boot), logging `<name>: the interface
  <role>'s pinned NIC (<mac>, Driver=<value>) is not here: the only NIC here no other role names,
  <nic>, is pinned in its place`; with no free NIC, or several, it leaves the pin as it is and
  logs `<name>: the interface <role>'s pinned NIC (<mac>, Driver=<value>) is not here, and the
  NICs here no other role names are <nics>, not one: its pin is left as it is`. A `.link` file by
  a MAC the seed names (several NIC roles) matches that MAC alone, with no `Driver=` clause: a
  cloud, where accelerated networking gives a NIC a virtual function of its MAC, takes one NIC,
  named by its original name, so a NIC the seed names by its MAC has no such twin; one `network attach` writes
  matches the MAC and the `Driver=` value of the same rule. A VM seeded before 0.40.0 has no pin: see
  [vm's](vm.md) "A VM seeded before 0.40.0". A server's NICs are its own, each named
  by the MAC given (`--interface ROLE=mac=…` for each, one NIC or more: a server may have more
  ports than the release names; one missing is refused naming the NICs this run makes, and the
  optional ones not asked apart: `; the optional <role> too, when given: --interface <role>=mac=…`). A cloud, which gives its NICs' MACs itself, takes one NIC (an optional one not asked is none), and no
  `mac=` (`<cloud> names its VMs' NICs itself, one here: …`; `mac=: <cloud> gives its VMs' NICs
  their MACs`). A VM keeps its NICs as its cloud-config named them: `network set`'s override
  matches what the cloud-config's `05-<name>-static.network` matches, whatever the running
  release declares (without that file, the release's primary role when the cloud-config named a
  NIC so, else every NIC). After an upgrade of a VM made by a release that named no NIC, a port on a NIC
  role no NIC is named for (no `10-<name>-<role>.link`) keeps its interface's match, which nothing
  meets — never opened on the other interfaces — warned (once a run for each role), with what names one, as `network set
  --role` tells it ([vm](vm.md)): `warning: no NIC of this VM is named <role>: its ports reach
  nothing: ` then the case and its fix, as `network set --role`'s refusal words them ([vm](vm.md)) —
  the cloud-config names it by a MAC no NIC has (`the VM's cloud-config
  names it by its MAC (<mac>): add the NIC of that MAC on the VM's platform (or fix the MAC of the
  one there) and restart the VM`), or several NICs have (`…, and <n> NICs of this VM have that
  MAC, so the boot names none of them: remove the other NICs with that MAC …`), its NIC here
  while another device holds the role's name (`…, but another device holds the name <role> (MAC
  <mac>): remove or rename that device …`), the boot's pin names it (`the VM's boot pinned it to its NIC's
  MAC (<mac>, Driver=<value>), and no NIC of that MAC is on this VM: leave one NIC on the VM that
  no other role names (…) and restart the VM, which pins the role to that NIC`), by its
  original name (`the VM's cloud-config names it by its original name, as the VM's only NIC:
  leave its NIC alone on the VM (remove the others on its platform[ but the NICs `network
  attach` recorded, …]) and restart the VM, which names it (…)`), its `.link` file names no NIC
  (`its .link file <path> names no NIC (…): restore <path> …`), `network attach` recorded it (`its NIC (<mac>) is recorded and
  there, and is named so from the next boot: `sudo <short>-stow reboot`` or `its NIC (<mac>) is
  recorded, and not on this VM yet: add it on the VM's platform with that MAC and restart the
  VM`), or nothing names it (`none is recorded for it: `sudo <short>-stow network attach --role
  <role> --mac <its MAC>` records one, named from the next boot: add that NIC on the VM's
  platform and restart the VM`); a case that does not read says `what names one is not known
  (<error>)`, followed by `: <fix>` when it is a refusal (`network attach`'s record that does not
  decode), and a `.link` file whose presence does not read (but for its absence) says
  `the VM's cloud-config's .link file of <role> does not read (<error>): restore it as the VM's
  cloud-config writes it (`sudo <short>-stow reboot` writes it again)`, never that the
  cloud-config names none. A role is named when a NIC of that name is on the VM (`/sys/class/net/<role>`). The
  primary the cloud-config names (its `.link` file, by a MAC or by its original name) gets its
  case as above. One it does not name is not attached (`network attach` refuses it): its warning
  names the way to a VM made by the release, which names it (`…, the primary (…): its ports reach nothing; a VM made by this
  release names it: `sudo <short>-stow secrets export --recipient <age key>` and `sudo
  <short>-stow backup run` here, a new VM of this release made with those secrets (`vm create
  --secrets`, or `vm seed --await-secrets`), then there `sudo <short>-stow backup config` with
  this VM's destination and rclone config, and `sudo <short>-stow restore <backup> --unrecorded``). By hand, the steps of a VM of several NICs list each NIC with its MAC and network (a VM
  of one NIC lists none); an OpenTofu root takes them from the seed's `interfaces`. A role that is not one of the release's NICs, the primary's addressing
  (`--network`'s), a `static` without its address or an address without `static`, two NICs of one
  MAC, and `attach=` on metal are refused, naming the flag.
- Then the machine: its host name (default the project's name; lowercase labels of letters,
  digits and `-`), its administrator (default your user name when the VM may take it, else
  `sysadmin`; never `root`, `admin`, `kairos`, a name starting with `_` or `systemd-`, or another
  account of the OS such as `www-data` or `nobody`; the base's own user, `ubuntu`, may be taken), the administrator's SSH
  public key file (default `~/.ssh/id_ed25519.pub`, `id_ecdsa.pub` or `id_rsa.pub`; one key of
  a type OpenSSH takes), and its network: DHCP, or a static `--address` with its prefix, an
  optional `--gateway` and `--dns` servers (systemd-resolved's; the VM's `/etc/resolv.conf` is
  a link, made at every boot, to resolved's list of them, not its stub, so the host and a container
  on the host's network resolve through them — a container through those of its creation); IPv6, off unless `--ipv6` (off, the VM's link has no
  IPv6 at all); with it, in either mode, router advertisements and DHCPv6 address the VM, and an
  optional static IPv6 `--address6` with its prefix and its `--gateway6` (asked with the static
  mode's questions when `--ipv6` is given, empty by default) sit beside them; `--address6` or
  `--gateway6` without `--ipv6` is refused, as is an IPv6 `--dns` server; `--add-host NAME=IP`
  (repeatable, at most 64) a host record the VM and its stack resolve: NAME a lower-case host name
  whose last label is not all digits, IP an address without a zone (an IPv6 one with `--ipv6`
  alone), the same record twice refused (`--add-host "<flag>": …`); every boot writes them to
  `/etc/<name>/hosts` (`IP NAME` lines) and into the VM's `/etc/hosts`, each line marked `#
  <name>: vm create --add-host` and replacing those an earlier boot added (resolved answers them),
  and each start gives them to every service of the stack as `extra_hosts` (in
  `stowage-content.yaml`; not a service in another's namespace or of `network_mode: none`), and
  to those Stowage runs on demand (the provision hook, a command provider), as a container
  resolves through Docker's DNS, which never reads the VM's `/etc/hosts`; a line of the file that
  does not read is left out, warned (`warning: /etc/<name>/hosts: "<line>" is not a host record
  (IP NAME): the containers do not get it`); the plan names them (`host records: …`), and
  `network` shows them (`hosts      NAME:IP, … (vm create --add-host)`); `--ipv6` also turns IPv6
  on for the install's network (`install --ipv6`, recorded in the settings); `--tune-profile`
  (`default` or `network`, another refused) is the VM's tuning profile over the release's
  `tune.profile`, which its boots apply and its first install records (`install
  --tune-profile`, recorded in the settings as `tune_profile`); the whole
  addressing, IPv4 and IPv6, is checked by the rules `network set` follows (below), a refusal
  starting "the VM's network:"; `--ssh-allow` (CIDR, repeatable or comma-separated,
  IPv4 or IPv6; each masked to its prefix) the sources SSH accepts — without it SSH accepts every
  source, and the run warns; on a cloud (`aws`, `azure`, `gcp`, `scaleway`), whose VM answers on a public
  address, without it SSH accepts this machine's public address alone, as
  `https://checkip.amazonaws.com` sees it (logged: `SSH accepts this machine's public address
  alone, <address>`), and a run that cannot learn it is refused (`--ssh-allow: a cloud's VM
  answers on a public address …`): `--ssh-allow 0.0.0.0/0,::/0` is every source —, and `--timezone` an IANA zone name (`Europe/Paris`; letters,
  digits, `_`, `+`, `-` and `/`, a zone Go's database knows, never `Local`, `localtime`,
  `posixrules`, `Factory` or one under `right/` or `posix/`; default the OS's, UTC; a zone the
  VM's own database lacks leaves it on UTC, said in its boot log). When `--ssh-allow` leaves out
  the address this machine reaches the VM from (its static addresses; each
  address a name resolves to, IPv4 and IPv6, as ssh may take either; a link-local source without
  its zone; a name not resolving yet is not checked), the run warns: the steps that follow go over
  SSH from here, unless the VM sees this machine at another address (NAT, a VPN). The
  administrator logs in with the SSH key alone: there is no password, on SSH or the console.
  `--secrets FILE` (`-` for standard input) is a decrypted `secrets export` file, a machine made
  again (or a new install's secrets file without `export: true`, a plain install): checked as `install --secrets` checks it before anything else, it is never written into
  the cloud-config, whose settings say the first install waits for it (`await_secrets: true`); a
  secret input it holds is not named as due. The secrets go over SSH to a host key known
  beforehand: `vm create` generates a one-time ed25519 host key, which the cloud-config carries
  (the seed, `seed.iso` or `user-data.yaml`, is then written 0600). While the install does not
  exist (`/usr/local/<name>/.env`) and the key was never retired, each boot writes it to
  `/run/<name>-seed-host-key` (0600) and `/etc/ssh/sshd_config.d/01-<name>-seed-host-key.conf`,
  which names it before the system's own keys, and reloads sshd; the first boot after the install
  removes both and marks the key retired (`/oem/<name>-seed-host-key.retired`, on the OEM
  partition, which a reset keeps), so no later boot serves it again: not an uninstalled or reset
  VM's, nor its recovery system's.
  `vm create` prints the key's fingerprint (`ssh must show SHA256:… (ED25519), or do not send the
  secrets`): after the install and a reboot the VM serves its own key, so `ssh-keygen -R <address>`
  then. Then the product, as the VM's `install` asks it: the
  TLS mode (an HTTPS one: never `none`, whose loopback address nothing outside the VM reaches,
  nor `byo` unless its certificate is given, whose private key the cloud-config never carries), the domains and the inputs of the
  `kairos` shape — never from the environment, which says nothing about the VM — but no secret
  input is asked: one in `--answers` goes over SSH ("Given over SSH"), one given by `--set` or
  `--set-file` is refused, with no fix (exit 3: `KEY is a secret input, which a command line
  (--set, --set-file) never carries`; `vm seed` refuses it as a seed refuses a secret input on
  its command line),
  and one the install
  requires that nothing gives is named at the end, for the operator to give over SSH (`ssh <user>@<address> sudo
  <short>-stow install`). The answers are reviewed ("Go on with these answers"), the whole is shown,
  and on a terminal confirmed; without one the flags and the defaults decide. The whole names the
  ports the VM opens to each `l4` route (`opens tcp/15432 (L4 route db) to every source`, or `to
  the sources of <its allow_input>`; a route with `tls` `to every source, TLS by the front`, then
  `, which admits the sources of <its allow_input>` when it has one) and the host devices each service maps (`devices:`, its own
  and those it extends: `the service S maps the host's /dev/…: the VM must have them`), which
  the VM's platform must give it.
- It boots the disk set the release records: its own disks, or, for a release shipped without
  them, the set its manifest names (`boot_disks`); a set no longer in the registry (a newer one
  replaced it) gives way to the set of the channel's pointer (its manifest signed by a key this
  `<short>-stow` carries), and a channel holding none is refused, naming `stowage ship --disks`.
  The release's own set, the registry holding some of its disks, is the one it boots: with
  `--platform` given, one the registry holds none of that platform's formats of (a mirror
  exported with some formats alone) is refused naming the format this registry lacks, never
  replaced by another release's set; so is a channel's set holding none.
  Another release's set must have been published by stowage 0.6.0 or later, whose OS takes another
  release (refused otherwise). The VM's settings then name the release to install (`release`:
  its version, revision, tag and OS bundle digest): its first boot writes that release's OS
  bundle into the passive slot (`kairos-agent upgrade`, by digest, from the registry or the
  mirror the settings name), records it (version and revision) in `/usr/local/<name>.os-switch`, and reboots into it;
  the boot after it installs (a boot still on another OS after that stops, naming the file).
  `vm create` says so as it fetches the disk (`fetching <name>'s <format> disk of <tag> (…): <name> <v> published none`,
  or `has none left in <prefix> (a later ship's disk set replaced them)`, `. The VM boots <tag>'s
  OS first; its first boot writes <tag>'s OS bundle and reboots into it, and an install on the VM
  is refused until then`). That release alone decides the first install: an operator's `sudo
  <short>-stow install` on the VM before it, on the OS of another release, is refused (`<settings>
  names <name> <v> (revision <r>) as the first install, and this OS carries <v'> (revision <r'>):
  the boot writes that release's OS bundle and reboots into it (`journalctl -u <name>.service`
  follows it); install once the VM runs it`), unless an `upgrade` before the first install wrote
  this OS's release since (below).
- It fetches that disk of the format (`<prefix>/<name>-disk@<digest>`, the digest the
  release records) with the pull account, as `upgrade --version` reads the registry, into
  `--out` (default the current directory) under the layer's title less its `.zst`
  (`<name>-<version>.<format>`), decompressed as it is written, checking the compressed layer's
  size and digest and the disk file's own digest; a file of that name holding that disk is kept,
  any other is refused, never replaced. Progress (of the compressed bytes) is reported every tenth.
  A download cut short (a dropped connection, a stream reset) resumes from the byte reached with
  a ranged request (`Range: bytes=N-`; a registry or CDN that ignores the range is read again with
  the bytes already read skipped), logging `download <file> (<digest>): <error>; resuming at N of
  M bytes`; it fails after 5 attempts in a row that bring no new byte (waits of 2 s doubling to
  30 s), and at once on a refusal (a 4xx but 408 and 429) or an interrupt. A stream that stays
  open but brings no byte for 120 s while it is read is dropped and resumed the same way (`no
  byte for 120 s`); bytes skipped from a range ignored count as bytes brought. With `--registry
  PREFIX` (a mirror, checked as a repository name) the disk is read there instead, with the
  Docker client's login, and `--registry-ca FILE` (certificates only) is trusted for that read;
  the review shows the registry and the CA file.
- **The VM's own registry login.** `--registry-login-file FILE|-` (an empty value is refused) is the login the VM reads its
  registry with — a mirror that asks for a sign-in, or the release's own registry with a login
  minted for this VM (revoking it leaves every other install pulling): a Docker config holding one
  `auths` entry for the host of the release's own registry or of `--registry`'s mirror (as
  `registry-login` takes it, see [vm](vm.md)), checked before anything is made. It is refused
  for another host (the refusal names the hosts the VM reads), for a file that is not one such
  entry, and when the VM would boot another release's disk set whose first boot reads this
  release's OS bundle before the login reaches it: from the mirror the login is for (give the
  mirror this release's own disks), or from the release's own registry when the installer carries
  no pull account to read it with (that first boot reads with the baked account). It never enters the
  cloud-config, a log or the review: the steps say to run `ssh <user>@<address>
  sudo <short>-stow registry-login - < FILE` once it answers (`vm seed` names the host alone,
  `registry_login`, and the program creating the VM gives the file the same way, pinned to the
  seed's one-time host key). The VM's first install reads
  nothing from the mirror (its images are preloaded), so it does not wait for the login; its
  updates and upgrades take it.
- The cloud-config (`#cloud-config`, Kairos's stages, applied at every boot) sets the host name
  in the `initramfs` stage, before the network starts (a DHCP request carries it, so a DHCP
  server that registers names registers it), and there too makes Kairos's automatic reset
  (`kairos-reset.service`, the first boot's, which lays the system out from the recovery
  system the disk holds) wait for udev's queue before it reads the partitions:
  `/etc/systemd/system/kairos-reset.service.d/10-<name>-udev-settle.conf`, `udevadm settle
  --timeout=120`, the reset tried after the bound all the same; with the host name, it writes
  `/etc/systemd/networkd.conf.d/10-<name>-duid.conf` (`[DHCPv4]` and `[DHCPv6]`
  `DUIDType=link-layer`) and `/etc/systemd/network/20-dhcp.network.d/10-<name>-duid.conf`
  and `20-dhcp-legacy.network.d/10-<name>-duid.conf` (`[DHCPv4] ClientIdentifier=duid`, over
  Kairos's `ClientIdentifier=mac` in its DHCP files, `en*` and `eth*`, for the boot's first
  exchange): the VM's DHCP clients name themselves by a DUID of the NIC's MAC, not of the
  machine-id (DHCPv4's client id is the IAID and that DUID), so the booted system holds one
  lease (the initrd's own DHCP exchange, before any stage, keeps its identity) and the recovery
  system, whose machine-id is its own, takes the VM's. The three directories, which the image does
  not ship, are made `0755` in that step: networkd reads them as its own user. Then, in `boot`: first, on the
  recovery system alone, it sets the host name again (Kairos's recovery cloud-config names it
  `cos-recovery` in that stage: it keeps the VM's name, which its DHCP requests carry, so a DHCP
  server registering names keeps the VM's name on it, at the VM's address); on every system it creates the administrator with the SSH key and no password, then, in a step of its own,
  puts them in the `admin` group (`usermod`, which also covers a user the base already has:
  passwordless `sudo`, the host daemon's socket), and writes the console's connect line,
  `/etc/issue.d/07-<name>-connect.issue` (`connect: ssh <user>@<address>, then `<short>-stow
  status``, and with `--ipv6` a second line, `or ssh <user>@<IPv6 address>`, agetty's `\6`);
  writes the VM's network, DHCP included, as
  `/etc/systemd/network/05-<name>-static.network` (`Name=en* eth*`, or the primary's role with
  `host.interfaces`, and each other NIC's file — see "The NICs" above; sorted before Kairos's DHCP file,
  which would take router advertisements, and after the override `<short>-stow network set`
  makes; the same sections as that override's — see `network set`) and reloads networkd; with `--ssh-allow`
  writes `/etc/nftables.d/ssh.nft` (`ip saddr <cidr> tcp dport 22 accept`, `ip6 saddr` for IPv6;
  replacing the bundle's, open to every source) and then, in a step of its own, loads the
  firewall again (`nft -f /etc/nftables.conf`, never a restart of `nftables.service`, whose stop
  would flush Docker's rules); with `--timezone` links `/etc/localtime` to the zone and writes
  `/etc/timezone`; and writes the first install's settings, `/etc/<name>/install.yaml`
  (0600): the channel this installer follows, the TLS mode, the ports given and every input
  answered that is not empty — but one that took its default naming another input, given no
  value, which the VM's install expands again and records as derived (`STOWAGE_DERIVED`, so a
  re-run changing that other input expands it again) — and with `--registry` its `registry` and with `--registry-ca` its
  `registry_ca`. With `--registry-ca` it also writes the CA to
  `/usr/local/share/ca-certificates/<name>-registry.crt` (0644) and then, in a step of its own,
  runs `update-ca-certificates`: the VM's trust store holds it from every boot on, so its
  `<short>-stow`, the host daemon and `kairos-agent upgrade` trust the mirror. The trust is
  system-wide, not the mirror's alone: `kairos-agent upgrade` reads the system's store and no
  other, so every program of the VM that uses it trusts a certificate that CA issues for any
  name; give a CA that issues the mirror's certificate alone (a dedicated one, not a company's
  root). It carries no secret,
  but with `--secrets` or given material (see "Given over SSH") the one-time host key (its header says so).
- **Given over SSH.** A secret input in `--answers` (`--set` and `--set-file` of one are still refused, as a command line is no place for it) and the front's `byo` certificate, key and CA and its client CA (`--cert`, `--key`, `--ca`, `--client-ca`, or the answers file's `tls` section, `--tls byo` asking the paths on a terminal) never enter the cloud-config. `vm create` checks each before a VM exists (the pair, its expiry, its chain, that it covers each route's domain, the input's own check, 1 MiB in all), the review names them and never a value, the seed says only `await_secrets: true`, carries no `tls` for `byo`, and writes `/etc/<name>/install.yaml.given` (`byo`, else `given`), and the first boot waits. A chain without `--ca` is checked against this machine's trusted CAs, and again by the install against the VM's, so give `--ca` for a private CA. A required secret input the file does not give is refused, as the install over SSH has no terminal to ask it, and so is `--secrets` with any of them, by the rule `vm seed --await-secrets` follows (`--secrets and <names>: the first install waits for one of them over SSH, …`; its fix names the inputs to remove from the answers file, the TLS material to leave out — `--cert`, `--key`, `--ca`, `--client-ca` and the answers file's `tls` section — or `--secrets`): it restores an install, so create the VM with `--secrets` alone and give them by a re-run over SSH.
  By hand, `vm create` writes `<out>/<hostname>.given` (0600) and `<out>/<hostname>.given.known_hosts` beside the seed, refuses a `.given` already there, and prints the step, for `<address>` the VM's: `ssh -o HostKeyAlias=vm-seed-host-key -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o 'UserKnownHostsFile="<file>.known_hosts"' -o GlobalKnownHostsFile=/dev/null -o HashKnownHosts=no [-i KEY -o IdentitiesOnly=yes] <user>@<address> -- sudo <short>-stow install --given - --no-input < <file> && rm <file> <file>.known_hosts`. Run again after a failure, it finishes the install, and only success removes the file; a VM that rebooted since the install began has retired the one-time key, so ssh refuses it: check the VM's own key on its console, run the command without its `HostKeyAlias`, `StrictHostKeyChecking`, `UserKnownHostsFile`, `GlobalKnownHostsFile` and `HashKnownHosts` options, and remove the file. An `--out` with a character ssh reads in a path (`%`, a backslash, `"`) is refused for this file. With a disk of another release the first boot also writes this release's OS and reboots: the step is for that OS, and `vm create` says so. An install finished by this step leaves the one-time key served until the VM's next reboot, and `known_hosts` empty. `install --given` is refused on Kairos's recovery system, as the reset wipes what it installs: the step works once the first boot rebooted.
  A handoff kit goes to a VM made: `vm kit take HOSTNAME FILE --known-hosts-line LINE` opens it
  here and gives what it holds to the VM's install over SSH (see [kits](kits.md)).

## Creating the VM with OpenTofu

Stowage creates no VM and keeps no state of one: an OpenTofu root of yours does, from `vm seed
--json`, with the platform's own provider and Stowage's (`norvexlabs/stowage`, whose
`stowage_vm_seed` runs `vm seed` and whose install resources wait for the VM and give it what
goes over SSH). Each platform has a complete root among the provider's examples,
`norvexlabs/terraform-provider-stowage` `examples/guides/<platform>-vm` (TrueNAS, Proxmox VE,
libvirt, VMware, AWS, Azure, Google Cloud, Scaleway): copy it into your own root. An example is a
preview until it has been applied on its platform. The root:

1. reads the seed (`stowage_vm_seed`, or `vm seed --json` run as above), with `--disk-dir` for a
   hypervisor that is given the disk as a file;
2. makes the VM's disk from `disk[<platform>]` (`disk_file` on a hypervisor; on a cloud the image
   `cloud_image` names, which `ship --disks` registered, else one the root registers itself),
   sized `disk_gib`; its seed from `user_data` and `meta_data` (a NoCloud ISO on a hypervisor, the
   cloud's user data on a cloud); its NICs from `interfaces` (each with its `mac`, on its
   `attach` network); its CPUs and memory within `size` (refused under `min`, warned under
   `recommended`); on a cloud, its firewall rules from `firewall`;
3. once the VM answers over SSH, checked by the seed's `known_hosts_line`, gives it what the seed
   says it waits for (`registry_login`, `await_secrets`, `await_tls`, `await_given`, `waiting`) over SSH, waits
   for its first install, and retires the one-time host key (`vm host-key --json
   --known-hosts-line <the seed's line>`), keeping the VM's own key's line for every later verb.

`tofu destroy` removes what the root made; the root's state is OpenTofu's, under its own lock.

**Verbs over SSH.** `vm host-key` and `vm kit create|check|take` reach the VM over SSH from this
machine and require `--known-hosts-line LINE`: the VM's host key as one known_hosts line, `HOST
KEYTYPE BASE64` — the seed's `known_hosts_line` until the install retires that one-time key (the
first reboot after the install, or `vm host-key`), then the VM's own, which `vm host-key --json`
prints (`known_hosts_line`) or which its console shows (`ssh-keygen -lf
/etc/ssh/ssh_host_ed25519_key.pub`). That key alone is accepted (ssh's `StrictHostKeyChecking=yes`
against a file holding that line alone, under `$XDG_STATE_HOME/<name>/known-hosts/`, never your
own known hosts); without it a verb is refused before any connection (exit 3: `<host>: the VM's
host key is not known: --known-hosts-line gives it`). A line that is not one host name or address
and one key (a pattern, a list, a hashed name, `[host]:port`, a `@` marker) is refused. The verbs
on the VM itself (`backup`, `upgrade`, `rollback`, `reset-db`, `network …`) run there, over your
own `ssh <user>@<address>`.

**Attaching a NIC.** A NIC for a role of the release (a secondary one, `host.interfaces`) is
attached on its platform, its MAC yours to choose (a locally administered unicast one), and named
on the VM by `sudo <short>-stow network attach --role ROLE --mac MAC [--mode dhcp|static|none]
[--address A/P]` from the next boot (see [vm](vm.md)). The VM's existing NICs are named by their
MACs already (the seed's, or its first boot's pin, above), so the two steps go in either order
(a VM seeded before 0.40.0 has its NIC pinned by hand first: [vm's](vm.md) "A VM seeded before
0.40.0"; until a NIC named by its original name is pinned, `network attach` refuses, saying so).
The root adds the NIC device (TrueNAS: a `truenas_vm_device` of type NIC, the VM restarted by
`truenas_vm_power`'s `restart_triggers`), and `network attach` prints what is left:
`<role>: the NIC <mac> is named and addressed (<mode>) from the next boot: ` then

- `the NIC is there: `sudo <short>-stow reboot` names it` — a NIC of that MAC is on the VM
  (under the kernel's own name): the drained reboot names it;
- `add the NIC on the VM's platform, then restart the VM: the boot names it` — none is.

A MAC another role's NIC holds (the seed's, its pin, the record's, or a NIC named as another
role now) is refused, as is an attach while the VM's NICs do not read (the error named, nothing
written). NICs are counted by their device (`/sys/class/net/<nic>/device`): a bridge, veth or
tunnel is none, nor is a NIC another enslaves (`/sys/class/net/<nic>/master`: an accelerated
networking's virtual function, of its NIC's MAC).

## The TrueNAS steps (`vm truenas`)

TrueNAS's provider (`truenas/truenas`) puts no file on the host, writes no disk image into a zvol
and does not stop a VM before deleting its devices. Until it does, the TrueNAS example runs
these steps from `terraform_data` resources' `local-exec` provisioners, with the release's
installer, `<short>-stow` (the program `stowage_vm_seed.ctl_path` names):

```bash
<short>-stow vm truenas import-image [--timeout DURATION]   # IMAGE, IMAGE_SHA256, STAGING_DIR, ZVOL; default 1h
<short>-stow vm truenas seed [--timeout DURATION]           # SEED, SEED_PATH; default 10m
<short>-stow vm truenas stop [--timeout DURATION]           # VM_ID; default 10m
```

- **The API.** Each reads the TrueNAS host from `TRUENAS_ENDPOINT` — the WebSocket URL the
  provider reads, `wss://<host>[:<port>]/api/current` (the legacy `/websocket` path is taken,
  rewritten to `/api/current`, as the truenas/truenas provider does) — or `TRUENAS_URL`, its HTTPS
  root `https://<host>[:<port>]`; both set must name the same host and port (443 when none is
  written, in either). `wss` and `https` only: the API key goes in every login and upload, and TrueNAS revokes a
  key used over plain HTTP. The key is `TRUENAS_API_KEY`. The host's certificate is checked
  against the system's CAs, or against the CA in the PEM file `TRUENAS_CA_CERT` alone, or not at
  all with `TRUENAS_INSECURE_SKIP_VERIFY=1` (`true`, `yes`; a TrueNAS host's own self-signed
  certificate); both set is refused. They talk to TrueNAS 25.04 or later: its JSON-RPC API over
  the WebSocket and its `/_upload/` endpoint.
- **`import-image`** writes the disk image file `IMAGE` into the zvol `ZVOL` (a dataset path,
  `pool/…`, not `/dev/zvol/…`; made beforehand, e.g. `truenas_zvol`, at least the image's virtual
  size): `IMAGE_SHA256` is 64 lower-case hex digits (else refused before the host is asked). A run
  goes, in order:
  1. **Reads the zvol.** One holding this image already (`org.stowage:image` `sha256:<hex>`, below)
     is left as it is: exit 0, `the zvol <zvol> holds the image sha256:<hex> already: left as it
     is` on standard error, nothing written, no lock taken, the file not read. Otherwise `IMAGE`
     is opened before the lock is taken: one that cannot be (`open <IMAGE>: …`) is refused with no
     lock made and nothing staged.
  2. **Takes the zvol's lock**, held for the whole run: one import at a time writes into a zvol.
     The lock is a zvol made beside it, `<zvol>-stowage-ref` — sparse, 1 MiB, of the zvol's
     `volblocksize`, its user properties `org.stowage:ref` naming the zvol and `org.stowage:run`
     a random nonce naming the run. TrueNAS refuses to make a dataset of a name that exists, so
     one run makes it. When the create fails, the run reads the name (on a new connection when
     the create got no answer):
     - this run's own nonce (TrueNAS made it, its answer lost): the run holds it and goes on;
     - another run's, whatever its age: refused, never deleted — while a job of that import runs
       (below), naming the job (`another import into <zvol> is running: job <id> (…); let it
       end, then run this again`); otherwise ``another import into <zvol> holds its lock
       <zvol>-stowage-ref (run <nonce>, made <creation time, UTC>): once no `tofu apply` or `vm
       truenas import-image` of <zvol> runs on any machine, and TrueNAS runs no job of it (Jobs: a
       vm.device.convert into <zvol>, or a filesystem.put or vm.device.convert of a file
       <STAGING_DIR>/<prefix><nonce>-…), delete the zvol <zvol>-stowage-ref on TrueNAS (Datasets,
       or pool.dataset.delete with {"recursive": true}), then run this again; while either runs,
       let it end``. The creation time tells a lock long dead from one just made. A run cut off
       before it could delete its lock (killed, its machine gone) leaves it so, and nothing but
       the operator deletes it: no run takes another's lock over;
     - a dataset of that name without `org.stowage:ref=<zvol>`, or not a zvol: refused, never
       deleted (`<zvol>-stowage-ref, the name of the lock of imports into <zvol>, is a dataset
       without org.stowage:ref=<zvol>: not Stowage's, refusing to delete it; rename or delete it`);
     - nothing of that name: the create's error (`the lock <zvol>-stowage-ref of imports into
       <zvol>: …`). A read that fails is an error too, and the lock, if the run's own, is deleted.

     The run deletes its lock as it ends, recursively (a periodic task's snapshots with it), even
     when it fails or is cancelled — within 30 s, always on a connection of its own — unless it is
     gone or another run's (`org.stowage:run` another nonce), and unless a convert may still run
     (step 6). It deletes only a lock whose `org.stowage:run` it read as its own: no run deletes
     another's, so the lock stays the run's from that read to its deletion — unless an operator
     deletes a live lock, against the refusal's conditions above, and another run makes its own
     in between: that voids the exclusion. A deletion that fails names the fix: `the lock <zvol>-stowage-ref not deleted
     (<the API's error>): once no job of this import runs, delete the zvol <zvol>-stowage-ref on
     TrueNAS (Datasets, or pool.dataset.delete with {"recursive": true}), then run this again`.
  3. **Reads the zvol again, under the lock**, refusing it (below), and once more after the
     upload, right before the zvol is marked, as well as the lock, which must still be this
     run's (`this run (<nonce>) no longer holds the lock …: refusing to write the zvol; run this
     again`); a zvol whose `org.stowage:image` changed in between is refused (`the zvol <zvol>'s
     org.stowage:image changed during the upload (<before>, now <after>), …`).
  4. **Uploads `IMAGE`** into `STAGING_DIR` (a dataset's mount point, `/mnt/…`) as `<prefix><nonce>-<the
     file's name>`, mode 0600 — a name no other run shares —, where `<prefix>` is `<the digest's
     first 16 hex digits>-<the first 8 hex digits of the SHA-256 of ZVOL>-`, and hashes the bytes
     as it sends them: the one read of the file, so the bytes converted are the bytes checked. A
     file that is not `IMAGE_SHA256` (a wrong digest, or a file that changed) is refused after
     that one upload, before the zvol is marked or written, the staged file emptied (`<IMAGE>:
     sha256 <got>, expected <want>: refusing to build a disk from it`).
  5. **Marks the zvol** `org.stowage:image` `importing:sha256:<hex>`.
  6. **Converts** the staged file into the zvol (`vm.device.convert`, which reads the format off
     the file's extension: `.qcow2`, `.raw`). TrueNAS runs one convert at a time, host-wide, and
     queues one more: a further call is answered with the queued convert's job, another
     import's. The run reads the job it is given by its arguments, its paths compared cleaned, a
     `/dev/zvol/` prefix dropped. One whose arguments read as another convert's is refused, never
     waited for (`TrueNAS answered vm.device.convert of <staged> into <zvol> with job <id>,
     another convert's (of <source> into <destination>): TrueNAS runs one convert at a time and
     queues one more, handing a further call the queued job; let the other converts end, then run
     this again`). One the run cannot tell — gone from TrueNAS's jobs (`… with job <id>, which
     TrueNAS's jobs do not hold`), of arguments that are no convert's (`…, whose arguments TrueNAS
     gives as no convert's`, or `which TrueNAS gives as a <method>`), or not read — may be its
     own, and is followed as a convert cut off, below. Its own convert is waited for: `SUCCESS`
     marks the zvol written (step 7); `FAILED` ends the run as below with TrueNAS's job's error
     and no run again, as its cause fails every run the same (`vm.device.convert of <staged>: job
     <id> FAILED: <its error>`: fix that cause, e.g. a larger zvol or the right image), the zvol
     left `importing:`, written again by the next run; `ABORTED` or gone is followed as below, on the same
     connection. A convert cut off — by `--timeout`, a cancellation or a broken connection — is
     not aborted: TrueNAS marks an aborted convert `ABORTED` at once while its `qemu-img` writes
     the zvol on to the end. The run reads the job instead, on a connection of its own, for up to
     20 s, its watch ending 30 s before the 80 s it has, which its last read keeps (so less when
     that connection took over 30 s), and reads its arguments again itself, from the last read of
     the job: one that then reads as another convert's is the refusal above, and one that then
     reads as the run's own is its own, below, whatever its first read gave. Only `SUCCESS` and
     `FAILED` end a convert (TrueNAS fails one once its `qemu-img` has exited), and only the run's
     own convert's ending ends the run:
     - `SUCCESS`: the zvol is written, and marked so (step 7) — even when the run's first read of
       the job failed; the step succeeds, printing `warning: the zvol <zvol> holds the image
       sha256:<hex>; the wait for the convert job <id> was cut off (<why>), and the job then ended
       SUCCESS`;
     - `FAILED`: the run ends as below, `…; the convert job <id> then ended FAILED: <its error>`,
       the zvol left `importing:`, written again by the next run;
     - any other state — still `RUNNING` or `WAITING` at the watch's end, `ABORTED`, gone from
       TrueNAS's jobs — a job that cannot be read, or one that ended with arguments the run cannot
       tell leaves the lock and the staged file as they are, as a `qemu-img` of the run's may still
       write the zvol, and is refused naming them: `…; the convert job <id> is still RUNNING on
       TrueNAS after <the time watched, about 20s>` (or `is ABORTED, which TrueNAS marks an
       aborted convert at once while its qemu-img writes on: it may still run`, `is gone from
       TrueNAS's jobs: whether its qemu-img still writes cannot be told`, `could not be read (…):
       it may still run`, `ended SUCCESS|FAILED, <why its arguments tell nothing>: whether a
       convert of this run's still writes the zvol cannot be told`) `: the lock
       <zvol>-stowage-ref and the staged file <staged> are left; once TrueNAS's Jobs shows no
       vm.device.convert of <staged>, delete the zvol <zvol>-stowage-ref on TrueNAS (Datasets, or
       pool.dataset.delete with {"recursive": true}), then run this again (TrueNAS's API deletes no
       file: delete <staged> on the host)`.

     A convert call whose answer was lost (`vm.device.convert of <staged> into <zvol>: …`) is
     looked for among TrueNAS's jobs, on a connection of its own, by its source, which names
     the run, three times over 5 s (the first at once), as the middleware may make the job after
     the first look: one found is followed as a convert cut off, above, on that connection; none
     means nothing ran (`… (its answer lost; TrueNAS has no job of it, looked for 3 times over 5s:
     nothing ran)`), and the run ends as below. A middleware that makes the job later than that
     is the one case where the lock is released while a convert of the run's may then write the
     zvol. A look that fails leaves the lock and the staged file (`… (its answer lost, its job not
     read: …; a convert may run): the lock … are left; …`).
  7. **Marks the zvol written**, `org.stowage:image` `sha256:<hex>`. Run again, a zvol an earlier
     run began writing this image into (`importing:` this digest) is uploaded and written again.

  Once TrueNAS may hold the staged file, it is emptied as the run ends — converted, refused,
  failed or cancelled — unless a convert may still run, on a connection of its own. TrueNAS may
  hold it as the upload ends:
  - accepted (HTTP 200): it holds the file;
  - no request made, or none answered before a byte of its body was sent (the dial or TLS
    failed), or refused with HTTP 401 or 403 (answered on its headers) before its body was sent
    whole: nothing was written, nothing is emptied (`upload of <file> refused: HTTP <status> …`,
    and no fix on the host);
  - any other — no answer once a byte of its body was sent, 401 or 403 after its whole body,
    413 or any other refusal —: it may hold a part, once a `filesystem.put` job of that path is
    found among TrueNAS's jobs (none found: nothing was written, nothing is emptied).

  Before it is emptied, a `filesystem.put` job of that path `RUNNING` or `WAITING` (the upload's,
  which a run cut off leaves) is followed to its end for up to 20 s, as an upload still running
  would write on after the emptying; one still running then leaves the file (`the staged file
  <path> not emptied (TrueNAS's API deletes no file: delete it on the host): its upload's job
  <id> is still RUNNING on TrueNAS after <the time watched>`).
  TrueNAS's API deletes no file, so an empty file of that name stays in `STAGING_DIR`, a few
  bytes per run (`the staged file <path> not emptied (TrueNAS's API deletes no file: delete it on
  the host): …` when that fails). Before the zvol is marked written, a lock not deleted or a file
  not emptied is an error (exit 1) beside the run's own; after, the zvol holds the image and the
  step succeeds (exit 0), each printed on standard error as `warning: the zvol <zvol> holds the
  image sha256:<hex>; <the message above>`. Refused, nothing written (exit 1):
  - a zvol that does not exist, a dataset that is not a zvol, or a zvol whose `logicalreferenced`,
    `volblocksize` or user properties TrueNAS does not give (`pool.dataset.query gave no
    logicalreferenced|volblocksize|user_properties for the zvol <zvol>: …`, as whether it holds
    data or the image is not known);
  - a zvol holding another image (`org.stowage:image` another digest, written or being written:
    give the image a new zvol);
  - a zvol without the property holding data: its `logicalreferenced` past a zvol never
    written's, which is the lock's, read as soon as it is made (`the zvol <zvol> holds data
    (logicalreferenced <n> bytes, a zvol never written <m>) and no org.stowage:image: …`).
    `logicalreferenced` counts the bytes written before compression, so a block of data written
    passes the bound, and an all-zero block, a hole once compressed, holds nothing to lose;
    TrueNAS 25.10 gives 28672 bytes for a zvol never written, whatever its size, block size or
    sparseness, 45056 or more once one 4 KiB block is written, and 1028096 for an empty ext4 on
    1 GiB (which references 378 KiB once compressed: the compressed size cannot tell);
  - a zvol a VM's device uses as its disk (`attributes.path` `/dev/zvol/<zvol>`: the import comes
    before the VM's disk device, which the root orders after it);
  - a job of this import `RUNNING` or `WAITING` — a `vm.device.convert` into the zvol (written
    `pool/…` or `/dev/zvol/pool/…`) or from a staged file of this image into this zvol (its path
    beginning with `<STAGING_DIR>/<prefix>`), or a `filesystem.put` (an upload) of such a file,
    paths compared cleaned (`another import into <zvol> is running: job <id> (vm.device.convert of
    <source> into <destination>|filesystem.put of <path>); let it end, then run this again`): a
    second sign beside the lock, checked as the lock is found held, under it, and again before
    the zvol is marked. Imports of one image into two zvols stage under two prefixes and do not
    block each other.

  Runs in one apply (parallel resources, a tainted one applied again) or two operators' run in
  parallel and are serialised by the lock: the ones refused succeed on the next apply.
- **`seed`** uploads the seed ISO file `SEED` to the path `SEED_PATH` on the host (under a
  dataset's mount point), mode 0600 (it holds the one-time SSH host key), replacing the file
  there: run again, the same ISO is written again.
- **`stop`** powers the VM `VM_ID` (TrueNAS's id, a positive number) off in any state but
  `STOPPED`, so TrueNAS lets its devices be deleted: a `RUNNING` VM by a clean shutdown
  (`vm.stop`), forced after the VM's own shutdown timeout; any other (`SUSPENDED`, …) by
  `vm.poweroff`, as TrueNAS shuts down only a running VM. The TrueNAS example runs it at destroy,
  before its devices go. Its state is read again after, whether the call failed or not: one
  `STOPPED` or gone succeeds (a VM that stopped or went meanwhile fails the call), any other fails
  (exit 1, `VM <id> is <state> after vm.stop|vm.poweroff, not STOPPED: …`, or `VM <id> is <state>
  after vm.stop|vm.poweroff failed (<the API's error>): …`). A VM stopped already, or gone, is left
  (exit 0).
- Each needs its own variables (`<VAR> is not set`, exit 1, before anything is dialled), as `VM_ID`
  a number; `--timeout` bounds the whole step, the API's answers and its jobs included (past it:
  `--timeout <d> ran out: the step starts over: run it again with a longer --timeout` — nothing
  a run cut off did resumes: a run again uploads and converts the image again, except that a zvol
  marked holding the image is left as it is —, after the fix a lock not deleted names, which its
  error holds (`the lock <zvol>-stowage-ref not deleted (…): once no job of this import runs,
  delete the zvol …, then run this again`); or only `--timeout <d> ran out` after a convert that
  may still run, which leaves the zvol's lock until it ends, its error naming the fix (`…: the
  lock <zvol>-stowage-ref and the staged file <staged> are left; …`), or after a convert that
  `FAILED`, whose error is TrueNAS's job's (`vm.device.convert of <staged>: job <id> FAILED: <its
  error>`, or `…; the convert job <id> then ended FAILED: <its error>`) and names no run again:
  its cause is TrueNAS's, the same on every run with any time — fix that cause, e.g. a zvol
  larger than the image's virtual size or the right image, then run this again). A failure
  exits 1 naming the API's error: a `local-exec` provisioner that fails taints its resource at
  create, and the next apply runs it again, which each step takes.

## VMs made by `vm create --create opentofu` (0.39.0 and earlier)

Such a VM's OpenTofu state is in Stowage's state directory, `$XDG_STATE_HOME/<name>/vms/<host>`
(`~/.local/state` by default), which this release no longer reads. Either destroy it with 0.39.0's
`<short>-stow vm destroy <host>` before you move to this release, or adopt it into a root of your
own with `import` blocks — each example's README lists its resources' import IDs. A `moved`
block cannot carry it over: TrueNAS's examples use `truenas/truenas`, and the modules up to
0.39.0 used the community `pjsalty/truenas` provider. Verbs over SSH take the VM's key from that
directory's `known_hosts` no more: give it with `--known-hosts-line` (`cat
~/.local/state/<name>/vms/<host>/known_hosts` holds it).
