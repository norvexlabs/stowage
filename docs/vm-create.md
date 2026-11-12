# Creating a VM (`<short>-stow vm create`)

[README](../README.md) · [Documentation index](../README.md#documentation)

A project with the `kairos` shape whose release publishes disk images gets a `vm create` command
in the same installer (bootstrap it as above, then `sh <name>.sh vm create …`, or run the kept
`<short>-stow`). It writes what a new VM of that release needs, then prints the platform's steps:

```bash
<short>-stow vm create [--platform P] [--out DIR] [--hostname H] [--user U] [--ssh-key FILE]
          [--network dhcp|static] [--address A/P] [--gateway G] [--dns D,…]
          [--ipv6 [--address6 A/P] [--gateway6 G]] [--tune-profile default|network]
          [--ssh-allow CIDR,…] [--timezone ZONE] [--device DISK] [--interface ROLE=SPEC]…
          [--add-host NAME=IP]… [--secrets FILE|-]
          [--tls MODE] [--cert FILE --key FILE] [--ca FILE] [--client-ca FILE]
          [--domains route=host,…] [--http-port N] [--https-port N]
          [--set KEY=VALUE]… [--set-file KEY=PATH]… [--answers FILE] [--no-input] [--accessible]
          [--create opentofu|manual] [--module DIR] [--tofu-var NAME=VALUE]…
          [--vm-name N] [--pool P] [--dataset D] [--bridge B]
          [--cpus N] [--memory MiB] [--disk GiB] [--host H] [--wait DURATION]
          [--registry PREFIX] [--registry-ca FILE] [--registry-login-file FILE]
<short>-stow vm seed --json [--platform P] [--hostname H] [--user U] [--ssh-key FILE] [--channel C] [--await-secrets] [--disk-dir DIR]
          [the machine's flags, as vm create's: --network … --device DISK, --interface ROLE=SPEC…, --add-host NAME=IP…]
          [--tls MODE] [--domains route=host,…] [--http-port N] [--https-port N]
          [--set KEY=VALUE]… [--set-file KEY=PATH]… [--answers FILE] [--registry PREFIX] [--registry-ca FILE] [--registry-login-file FILE]
                                                   # a VM's first-boot material as JSON, the VM made by your own means
<short>-stow vm destroy HOSTNAME [--yes] [--no-input] [--accessible]
<short>-stow vm attach HOSTNAME --interface ROLE=SPEC [--user U] [--ssh-key FILE] [--host H] [--known-hosts-line LINE] [--wait DURATION]
<short>-stow vm host-key HOSTNAME --json [--user U] [--ssh-key FILE] [--host H] [--known-hosts-line LINE]   # the VM's own SSH host key; the seed's one-time key retired
<short>-stow vm kit create|check|take HOSTNAME … [--known-hosts-line LINE]   # a handoff kit issued on the VM or taken by it, over SSH (see "Handoff kits")
```

- A project whose front offers no mode a VM can take (only `none`, or `byo` and no certificate given) is refused first, saying to give `--cert` and `--key` when it offers `byo`.
- **`vm seed --json`** prints, as one JSON object, what a new VM needs at its first boot, for a
  tool that creates the VM with its own means (a hypervisor's or a cloud's own provider): it
  creates nothing, never asks, and writes no file but the disk `--disk-dir` asks for. It takes `vm create`'s machine flags
  (`--platform`, required, `--hostname`, `--user`, `--ssh-key`, `--network` and its addressing,
  `--ipv6`, `--tune-profile`, `--ssh-allow`, `--timezone`, `--device`, `--interface`,
  `--add-host`), the front's (`--tls`, `--domains`, `--http-port`, `--https-port`), `--set`,
  `--set-file` and `--answers` (inputs that are not secret, and an answers file's `tls` section),
  `--registry`, `--registry-ca` and `--registry-login-file` (checked as `vm create` checks it;
  the seed names its host alone, `registry_login`, and the program gives the file over SSH once
  the VM answers, `sudo <short>-stow registry-login -`), `--channel` (the channel the first install follows; default
  the release's), and its own: `--await-secrets` (the first install waits for a decrypted
  `secrets export`, given over SSH once the VM answers, `sudo <short>-stow install --secrets -
  --no-input`, as `vm create --secrets` makes it; a machine made again), `--tls byo` (it waits
  for the certificate, its key and CA, and the secret inputs, given over SSH, `sudo <short>-stow
  install --given - --no-input`, its front serving that certificate; the release must offer
  `byo`), and `--disk-dir DIR` (an existing directory `--platform`'s disk is written into,
  decompressed, as `vm create` fetches it; a file of that name already holding it is kept).
  `--tls byo` with `--await-secrets` is refused (the first install waits for one of them), as is a
  `--disk-dir` that is no directory, and a certificate, key, CA or client CA an answers file's
  `tls` section gives (`none` aside; the seed takes none of `--cert`, `--key`, `--ca`,
  `--client-ca`): the seed carries none (a path the file names that does not exist fails first,
  as the answers file is read). A release whose
  front offers no mode a VM is made with, but `byo`, is refused naming `--tls byo` as the fix (`vm
  create`'s names `--cert` and `--key`; both exit 3). `disk_file`'s path is absolute, whatever
  `--disk-dir` was given as; `--json` is required. It is the seed `vm create --create
  manual` writes, made by the same steps and refused as `vm create` refuses (a release with no
  OS bundle or no disk, a front with no mode a VM takes, `--platform` missing or not offered, an
  input missing or refused), and a secret input or TLS material given to it is refused, naming
  them (`<names>: a seed carries no secret input and no TLS material; they go after the boot,
  over SSH`), never printing a value: they go once the VM answers, `sudo <short>-stow install
  --given - --no-input` over SSH. A cloud platform's `--ssh-allow` default is this machine's
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
    "waiting": ["API_KEY"]
  }
  ```
  `user_data` is the cloud-config for `--platform` (a NoCloud seed's `user-data`, or a cloud's
  user data); it carries no secret input, but the one-time SSH host key it makes the VM serve
  until its install and the reboot after it, as `vm create`'s does for what it gives over SSH.
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
  waits for over SSH, left out when false. `registry_login` (`--registry-login-file`) is the
  mirror's host the login is for, never the login; left out without it. `waiting` names, sorted,
  the keys of the required secret inputs nothing gives (never a value; left out when there are
  none): the VM's first install waits for them, as `vm create` says, until they are given over
  SSH (`sudo <short>-stow install --given - --no-input`). `--platform` missing, not a platform
  Stowage knows, or one the registry holds no disk for is refused (exit 3) once the registry has
  been read, its fix naming the platforms that registry offers; a registry that cannot be read,
  or that offers no disk for any platform, is a failure (exit 1), whatever `--platform` says. A
  refusal prints one object on standard output, as `status --json`'s (`{"format": 1,
  "refused": {"reason": "…", "fix": "…"}}`), and exits 3; a failure prints none.
- **`vm host-key HOSTNAME --json`** reads the VM's own ed25519 SSH host key over SSH, checked by
  the key `--known-hosts-line` gives (the seed's `known_hosts_line`, or the VM's own) or `vm
  create` recorded, and retires the seed's one-time key at once — its file and sshd drop-in
  removed, its retirement marked on the OEM partition, sshd reloaded — so the VM serves its own
  key alone from then on (as its first boot after the install would). It takes `--user`,
  `--ssh-key`, `--host` (the address, default HOSTNAME) and `--known-hosts-line`, and prints
  `{"format": 1, "public_key": "ssh-ed25519 AAAA…", "known_hosts_line": "<host name> ssh-ed25519
  AAAA…", "fingerprint": "SHA256:…", "retired": true}`: the VM's key, and under the host name the
  run checked it by (the line's, else the address) its known_hosts line. The retirement's sshd
  reload is checked: a running `ssh.service` or `sshd.service` reloaded, or none running and
  `ssh.socket` there to start one (which reads its configuration per connection); one that did
  not take prints the key all the same with `"retired": false` and a `reason`, and fails the
  verb (exit 1: `the seed's one-time host key's files are gone, but sshd did not reload: it may
  serve that key until it does; `sudo systemctl reload ssh` (or reboot the VM), then run
  host-key again`). With `vm create`'s record (no `--known-hosts-line`), the record is replaced by that
  line; without it and without that record it is refused (a pin `vm create` keeps while it waits
  is never read: a create may still be using it). With `--known-hosts-line` no record of `vm
  create`'s is written (the line given is written to its own file, as for every verb taking it):
  keep the printed line, the VM's key from then on. Run again it changes nothing more, through the VM's own key. Refused (exit 3,
  nothing changed): the VM answering from its recovery or live system (its first boot not done:
  the one-time key is kept, as the active system serves it; run it again once it rebooted), ssh
  not reaching the VM through the key (exit 255: the VM down, the key not authorized, or a VM that
  rebooted since its install serving its own key alone — the fix names the console's check,
  `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`), and the reach's own errors (no key known,
  a bad `--known-hosts-line`, `--user` or `--host`). A refusal prints its object as `vm seed`'s.
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
  are `vm host-key`'s over SSH (with sudo) and `vm create`'s after its install (which, when sshd
  did not reload, keeps its pin of the one-time key and warns, naming the fix); the first boot
  after the install runs the same file steps, its reload unchecked (an sshd not started yet reads
  its configuration as it starts).
- The platform decides the disk format (the first of its formats the registry holds: a mirror
  carries the formats its `mirror export --disks` named) and how the cloud-config reaches the VM.
  Only the platforms the registry has a disk for are offered; a `--platform` whose formats the
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
  network is the seed's (its NICs as below). OpenTofu creates no machine:
  `--create opentofu` there needs `--module` (a module of the operator's, given the installer as
  its `image` and the seed as its `seed`).
- **The NICs** (`host.interfaces`): a release that names none has one NIC, whatever its name
  (`en*`, `eth*`), addressed by the machine's network. A release that names NICs has one each,
  named by its role: the seed's `initramfs` stage writes `/etc/systemd/network/10-<name>-<role>.link`
  (`[Match] MACAddress=<mac>`, `[Link] Name=<role>`), before the booted system's udev names the
  NICs, and its `boot` stage renames a NIC a first boot named before (down, renamed, up). The
  primary's addressing is the machine's network (`05-<name>-static.network` matches its role's
  name, and so does `network set`'s override); each other NIC's is
  `/etc/systemd/network/10-<name>-<role>.network`, given by `--interface ROLE=SPEC` (repeatable):
  `dhcp` (an address, never the default route, the DNS servers nor the host's name: `UseGateway=no`,
  `UseRoutes=no`, `UseDNS=no`, `SendHostname=no`), `static,address=A/P` (an IPv4 address with its
  prefix), or `none` (the default: the link up, no address), then `mac=` (a unicast MAC) and, on a
  hypervisor, `attach=` (the platform's network or bridge, default the primary's); an `optional`
  role's NIC is made only when `--interface` names it (`--interface <role>=none` for the default
  addressing), else the machine has none and no MAC is drawn for it; no NIC waits
  for the boot (`RequiredForOnline=no`). A machine of one such NIC needs no MAC: the `.link` names
  the NIC it has (`OriginalName=en* eth*`, an accelerated networking's virtual function left out:
  `Driver=!mlx4_core mlx5_core mana`), so a cloud takes it. Of more, each NIC is named by its MAC:
  given, else generated on a hypervisor (`02:` and 5 random bytes, drawn by each run and written
  into its seed and, with OpenTofu, the module's variables, which every later apply of that state
  reuses: the NICs take the MACs the run that wrote their seed printed). A server's NICs are its own, each named
  by the MAC given (`--interface ROLE=mac=…` for each, one NIC or more: a server may have more
  ports than the release names). A cloud, which gives its NICs' MACs itself, takes one NIC (an optional one not asked is none), and no
  `mac=` (`<cloud> names its VMs' NICs itself, one here: …`; `mac=: <cloud> gives its VMs' NICs
  their MACs`). A VM keeps its NICs as its cloud-config named them: `network set`'s override
  matches what the cloud-config's `05-<name>-static.network` matches, whatever the running
  release declares (without that file, the release's primary role when the cloud-config named a
  NIC so, else every NIC). After an upgrade of a VM made by a release that named no NIC, a port on a NIC
  role no NIC is named for (no `10-<name>-<role>.link`) keeps its interface's match, which nothing
  meets — never opened on the other interfaces — warned (`warning: no NIC of this VM is named
  <role> …: its ports reach nothing`); a VM made by the release names it, and `vm attach` (below)
  attaches one to this VM. By hand, the steps list each NIC with its MAC and network; OpenTofu's modules take
  them as `nics` (below). A role that is not one of the release's NICs, the primary's addressing
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
  the address this machine reaches the VM from (its `--host`, else its static addresses; each
  address a name resolves to, IPv4 and IPv6, as ssh may take either; a link-local source without
  its zone; a name not resolving yet is not checked), the run warns: `vm create` and the steps that follow go over
  SSH from here, unless the VM sees this machine at another address (NAT, a VPN). The
  administrator logs in with the SSH key alone: there is no password, on SSH or the console.
  `--secrets FILE` (`-` for standard input) is a decrypted `secrets export` file, a machine made
  again: checked as `install --secrets` checks it before anything else, it is never written into
  the cloud-config, whose settings say the first install waits for it (`await_secrets: true`); a
  secret input it holds is not named as due. The secrets go over SSH to a host key known
  beforehand: `vm create` generates a one-time ed25519 host key, which the cloud-config carries
  (the seed, `seed.iso` or `user-data.yaml`, is then written 0600). While the install does not
  exist (`/usr/local/<name>/.env`) and the key was never retired, each boot writes it to
  `/run/<name>-seed-host-key` (0600) and `/etc/ssh/sshd_config.d/01-<name>-seed-host-key.conf`,
  which names it before the system's own keys, and reloads sshd; the first boot after the install
  removes both and marks the key retired (`/oem/<name>-seed-host-key.retired`, on the OEM
  partition, which a reset keeps), so no later boot serves it again: not an uninstalled or reset
  VM's, nor its recovery system's. Without OpenTofu,
  `vm create` prints the key's fingerprint (`ssh must show SHA256:… (ED25519), or do not send the
  secrets`): after the install and a reboot the VM serves its own key, so `ssh-keygen -R <address>`
  then. Then the product, as the VM's `install` asks it: the
  TLS mode (an HTTPS one: never `none`, whose loopback address nothing outside the VM reaches,
  nor `byo` unless its certificate is given, whose private key the cloud-config never carries), the domains and the inputs of the
  `kairos` shape — never from the environment, which says nothing about the VM — but no secret
  input is asked: one in `--answers` goes over SSH ("Given over SSH"), one given by `--set` or
  `--set-file` is refused, with no fix (exit 3: `KEY is a secret input, which a command line
  (--set, --set-file) never carries`; `vm seed` refuses it as a seed refuses any secret input),
  and one the install
  requires that nothing gives is named at the end, for the operator to give over SSH (`ssh <user>@<address> sudo
  <short>-stow install`). The answers are reviewed ("Go on with these answers"), the whole is shown,
  and on a terminal confirmed; without one the flags and the defaults decide. The whole names the
  ports the VM opens to each `l4` route (`opens tcp/15432 (L4 route db) to every source`, or `to
  the sources of <its allow_input>`; a route with `tls` `to every source, TLS by the front`, then
  `, which admits the sources of <its allow_input>` when it has one) and the host devices each service maps (`devices:`, its own
  and those it extends: `the service S maps the host's /dev/…: the VM must have them`), which
  the VM's platform or module must give it.
- It boots the disk set the release records: its own disks, or, for a release shipped without
  them, the set its manifest names (`boot_disks`); a set no longer in the registry (a newer one
  replaced it) gives way to the set of the channel's pointer (its manifest signed by a key this
  `<short>-stow` carries), and a channel holding none is refused, naming `stowage ship --disks`.
  Another release's set must have been published by stowage 0.6.0 or later, whose OS takes another
  release (refused otherwise). The VM's settings then name the release to install (`release`:
  its version, revision, tag and OS bundle digest): its first boot writes that release's OS
  bundle into the passive slot (`kairos-agent upgrade`, by digest, from the registry or the
  mirror the settings name), records it in `/usr/local/<name>.os-switch`, and reboots into it;
  the boot after it installs (a boot still on another OS after that stops, naming the file).
  `vm create --create opentofu` waits 40 minutes by default then, rather than 20, and says so as
  it fetches the disk (`fetching <name>'s <format> disk of <tag> (…): <name> <v> published none`,
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
- **A mirror that asks for a sign-in.** `--registry-login-file FILE|-` (an empty value is refused) is the login the VM reads its
  mirror with: a Docker config holding one `auths` entry for the mirror's host (as
  `registry-login` takes it, see [vm](vm.md)), checked before anything is made. It is refused
  without `--registry` (the release's own registry is read with its pull account), for another
  host than `--registry`'s, for a file that is not one such entry, and when the VM would boot
  another release's disk set (its first boot reads this release's OS bundle from the mirror
  before the login reaches it: give the mirror this release's own disks). It never enters the
  cloud-config, a log or the review: the cloud-config carries the one-time host key, and with
  OpenTofu, once the VM answers, `vm create` runs `sudo <short>-stow registry-login -` over SSH
  pinned to that key, the login on its standard input, before any install over SSH (a failure
  keeps the VM, naming the step to run by hand and what the install still waits for); the seed's key is retired after it when no
  install over SSH follows. With `--create manual` the steps say to run `ssh <user>@<address>
  sudo <short>-stow registry-login - < FILE` once it answers. The VM's first install reads
  nothing from the mirror (its images are preloaded), so it does not wait for the login; its
  updates and upgrades take it.
- The cloud-config (`#cloud-config`, Kairos's stages, applied at every boot) sets the host name
  in the `initramfs` stage, before the network starts (a DHCP request carries it, so a DHCP
  server that registers names registers it), and there too makes Kairos's automatic reset
  (`kairos-reset.service`, the first boot's, which lays the system out from the recovery
  system the disk holds) wait for udev's queue before it reads the partitions:
  `/etc/systemd/system/kairos-reset.service.d/10-<name>-udev-settle.conf`, `udevadm settle
  --timeout=120`, the reset tried after the bound all the same; then, in `boot`, creates the administrator with the SSH key and no password, then, in a step of its own,
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
  answered that is not empty, and with `--registry` its `registry` and with `--registry-ca` its
  `registry_ca`. With `--registry-ca` it also writes the CA to
  `/usr/local/share/ca-certificates/<name>-registry.crt` (0644) and then, in a step of its own,
  runs `update-ca-certificates`: the VM's trust store holds it from every boot on, so its
  `<short>-stow`, the host daemon and `kairos-agent upgrade` trust the mirror. The trust is
  system-wide, not the mirror's alone: `kairos-agent upgrade` reads the system's store and no
  other, so every program of the VM that uses it trusts a certificate that CA issues for any
  name; give a CA that issues the mirror's certificate alone (a dedicated one, not a company's
  root). It carries no secret,
  but with `--secrets` or given material (see "Given over SSH") the one-time host key (its header says so).
- **Given over SSH.** A secret input in `--answers` (`--set` and `--set-file` of one are still refused, as a command line is no place for it) and the front's `byo` certificate, key and CA and its client CA (`--cert`, `--key`, `--ca`, `--client-ca`, or the answers file's `tls` section, `--tls byo` asking the paths on a terminal) never enter the cloud-config. `vm create` checks each before a VM exists (the pair, its expiry, its chain, that it covers each route's domain, the input's own check, 1 MiB in all), the review names them and never a value, the seed says only `await_secrets: true`, carries no `tls` for `byo`, and writes `/etc/<name>/install.yaml.given` (`byo`, else `given`), and the first boot waits. A chain without `--ca` is checked against this machine's trusted CAs, and again by the install against the VM's, so give `--ca` for a private CA. A required secret input the file does not give is refused, as the install over SSH has no terminal to ask it, and so is `--secrets` with any of them: it restores an install, so create the VM with `--secrets` alone and give them by a re-run over SSH.
  With OpenTofu the material goes over SSH, on standard input, to `sudo <short>-stow install --given - --no-input`, pinned to the one-time host key as `--secrets` is. `<state>/given.yaml` (0600, beside `<state>/given.yaml.known_hosts`) holds it from before the VM exists until the install took it, and `vm destroy` removes it: if the wait or the install fails the VM is kept, and the error prints the step below for that file. By hand, `vm create` writes `<out>/<hostname>.given` (0600) and `<out>/<hostname>.given.known_hosts` beside the seed, refuses a `.given` already there, and prints the step, for `<address>` the VM's: `ssh -o HostKeyAlias=vm-seed-host-key -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o 'UserKnownHostsFile="<file>.known_hosts"' -o GlobalKnownHostsFile=/dev/null -o HashKnownHosts=no [-i KEY -o IdentitiesOnly=yes] <user>@<address> -- sudo <short>-stow install --given - --no-input < <file> && rm <file> <file>.known_hosts`. Run again after a failure, it finishes the install, and only success removes the file; a VM that rebooted since the install began has retired the one-time key, so ssh refuses it: check the VM's own key on its console, run the command without its `HostKeyAlias`, `StrictHostKeyChecking`, `UserKnownHostsFile`, `GlobalKnownHostsFile` and `HashKnownHosts` options, and remove the file. An `--out` with a character ssh reads in a path (`%`, a backslash, `"`) is refused for this file. With a disk of another release the first boot also writes this release's OS and reboots: the step is for that OS, and `vm create` says so. An install finished by this step leaves the one-time key served until the VM's next reboot, and `known_hosts` empty. OpenTofu failing keeps `given.yaml` for a VM it may have left waiting, for `vm destroy` to remove, and removes it when it failed before it wrote its variables, as no VM exists then. `install --given` is refused on Kairos's recovery system, as the reset wipes what it installs: the step works once the first boot rebooted.
  `--kit FILE|-` and `--kit-code-file FILE` take a handoff kit (its format, its code's sources and its refusals under the Docker shape's `--kit`), opened on this machine. Its inputs are taken as the answers file's, an answers file and a flag winning, and its TLS material as the flags', so its secret inputs and TLS material take the route above: over the pinned SSH, or by hand from the `.given` file, on this machine's disk in the clear, 0600, until the install took it, as `--answers`' are. Its other inputs reach the cloud-config, as `--answers`' do. An input declared for other shapes alone is left out and logged (`--kit: KEY left out: not an input of the kairos shape`). `--secrets -` with `--kit -` is refused, as standard input is read once.
- **With OpenTofu** (`--create opentofu`). `vm create` then creates the VM itself instead of
  printing steps, with a module: the platform's built-in one, or the operator's (`--module DIR`).
  The built-in modules, each pinning its provider's release exactly, with its lock file (the
  provider's checksums for Linux and macOS, amd64 and arm64: `tofu init` refuses a provider binary
  that does not match):

  | `--platform` | provider | what the module makes | the module's own variables (`--tofu-var`, or asked) |
  |---|---|---|---|
  | `truenas` | `pjsalty/truenas` 3.1.0 | below | its flags: `--pool`, `--dataset`, `--bridge` |
  | `libvirt` (preview) | `dmacvicar/libvirt` 0.9.9 | the image uploaded as a volume, a copy-on-write qcow2 volume over it grown to `disk_gib` (the app's `--disk` and Kairos's share); the seed as a second, read-only volume; both given to the domain by their paths (file disks, which the AppArmor security driver writes into the domain's profile with the image behind the disk); a KVM domain with libvirt's UEFI firmware (Secure Boot off, ACPI and the APIC on) on `network`, its virtio NIC with a queue per vCPU (none set with one vCPU: one is libvirt's default) and a receive ring of 1024, its DHCP lease of an IPv4 address the address (waited for, up to 5 minutes, only with DHCP) | `uri` (default `qemu:///system`), `pool` (`default`), `network` (`default`), `domain_type` (`kvm`, or `qemu` for a host without KVM: emulated, much slower, its CPU mode then `maximum`; any other value refused by the module) |
  | `proxmox` (preview) | `bpg/proxmox` 0.117.0 | the image uploaded to a datastore with *Import* content (not enabled by default: Datacenter > Storage), the seed to one with *ISO* content, both through the API (the token alone, no SSH to the node); an OVMF VM importing the image, grown to `disk_gib` (the app's `--disk` and Kairos's share), the seed on its CD-ROM, a virtio NIC with a queue per vCPU (at most 64); no address (no guest agent) | `node` (required), `datastore` (`local-lvm`), `import_datastore` (`local`), `iso_datastore` (`local`), `bridge` (`vmbr0`) |
  | `vmware` (preview) | `vmware/vsphere` 2.17.1 | the OVA deployed, its disk grown to `disk_gib` (the app's `--disk` and Kairos's share), the seed uploaded to the datastore on its CD-ROM; its address when VMware tools report one | `datacenter`, `cluster`, `datastore`, `host` (required), `network` (`VM Network`), `folder` |
  | `aws` (preview) | `hashicorp/aws` 6.69.1 | by its image module, the raw image uploaded to S3 (`stowage/<name>-<hash>.raw`), imported as a snapshot (the `vmimport` role) and registered as a UEFI image; an instance launched from it with the cloud-config as its user data (IMDSv2), with `eip_allocation_id` that Elastic IP (reserved beforehand, released from it and kept by `vm destroy`); that address, else its public one, else its private one | `region`, `bucket`, `subnet_id` (required), `security_group_ids` (comma-separated), `instance_type` (`t3.medium`), `vmimport_role` (`vmimport`), `eip_allocation_id` |
  | `gcp` (preview) | `hashicorp/google` 8.7.0 | by its image module, the image's `tar.gz` uploaded to Cloud Storage (`stowage/<name>-<hash>.tar.gz`) and a UEFI image declaring gVNIC; an instance booted from it with the cloud-config as its `user-data`, tagged with its name; with `firewall`, firewall rules on its network reaching that tag (`<name>-<hash>-ssh`: TCP 22 from `ssh_sources`' IPv4 sources; `<name>-<hash>-p<hash>`: one for each set of IPv4 `sources` among `open_ports`, its ports from them; a port with no IPv4 source makes none), for which the account OpenTofu runs as must be allowed to create and delete firewall rules (`compute.firewalls.*`, e.g. `roles/compute.securityAdmin`); its external address. Without `firewall`, `vm create` warns `no firewall rule is made on Google Cloud: the network's own rules must let tcp/22, <the opened ports> reach the VM`: the default network's `default-allow-ssh` opens SSH to every source, which the VM's firewall then narrows, and nothing opens the front's ports. With `firewall=true` and `--ssh-allow` naming IPv6 sources alone, no SSH rule is made and `vm create` warns `SSH's sources name no IPv4 address`. A value other than `true`, `false`, `1` or `0` is refused before OpenTofu runs. The network's own rules apply either way | `project`, `zone`, `bucket` (required), `firewall` (`false`), `network` (`default`), `subnetwork`, `machine_type` (`e2-medium`), `nic_type` (`GVNIC`, or `VIRTIO_NET`), `nat_ip` (a static external address of the zone's region, reserved beforehand and kept by `vm destroy`; else an ephemeral one) |
  | `azure` (preview) | `hashicorp/azurerm` 5.10.0 | by its image module, the fixed VHD uploaded as a page blob (`stowage-<name>-<hash>.vhd`) and a generation 2 image; a VM with a static public address (`public_ip_name`'s, a Standard public IP of the resource group reserved beforehand, which `vm destroy` keeps, else one made with the VM) and accelerated networking made from it with the cloud-config as its custom data (no VM agent); its public address | `resource_group`, `location`, `storage_account`, `subnet_id` (required), `container` (`vhds`), `vm_size` (`Standard_B2ls_v2`: a size that offers accelerated networking), `accelerated_networking` (`true`), `public_ip_name` |
  | `scaleway` (preview) | `scaleway/scaleway` 2.91.0 | by its image module, the qcow2 image uploaded to Object Storage (`stowage/<name>-<hash>.qcow2`), imported as a Block Storage snapshot and made an x86_64 image (UEFI); a routed public IPv4 address (no IPv6): `ip_id`'s, a flexible IP of the zone reserved beforehand, which `vm destroy` keeps, else one made with the VM; unless `security_group_id` is given, a stateful security group named as the VM that drops every inbound packet but TCP 22 from `ssh_sources` (IPv4 and IPv6) and each of `open_ports` from its `sources` (a rule per source: none for a route whose allowlist is empty), and accepts every outbound one; an instance of `instance_type` from the image with an SBS root volume of `disk_gib` (in GB, rounded up from GiB) and the cloud-config as its `cloud-init` user data; its public address. The credentials are the provider's (`SCW_ACCESS_KEY` / `SCW_SECRET_KEY`, or the `scw` configuration file); the account needs Instances, Block Storage and Object Storage access in the project | `zone`, `bucket` (required: the bucket in the zone's region), `project` (the credentials' default project), `instance_type` (`PLAY2-NANO`), `security_group_id`, `ip_id` |

  The cloud modules (`aws`, `azure`, `gcp`, `scaleway`) make the disk's image with an **image
  module** of their own (their `image/`), applied first in a state of its own:
  `$XDG_STATE_HOME/<name>/images/<platform>-<key>`, the key a digest of the disk's SHA-256 and of
  the module's other variables the operator gives (the zone on Scaleway, the region, bucket,
  project, resource group, location, storage account: the VM module's values of the same names;
  Google Cloud's image module takes no zone, as an image is the project's). Every VM of one disk
  in one place shares it: the second finds the image there and uploads nothing, `vm create`
  logging `the disk's image on <platform>: <directory> (shared by the VMs of this disk, kept by
  vm destroy)`; two `vm create` runs on it at once take turns (`.stowage.lock` there; the second
  logs `waiting for another vm create making the disk's image in <directory>`). Its variables
  are `name` (the project's name), `description` (`<name>'s disk <the SHA-256's first 12
  digits>, …`), `image`, `image_sha256` and `state_key` (the key above), and every name it gives
  holds the disk's digest and the key (`<name>-<12 digits>-<the key's first 8>` on AWS, Azure and
  Scaleway; on Google Cloud the image `<name, cut to 30>-<a hash of the name and the
  key>-<12 digits>`), so two states never make the same object or image: a value given in one
  run and left to the default in another makes a second state, and a second upload. Its output
  `image_id` is the VM module's variable of that name; `--tofu-var image_id` is refused (`the
  image module makes the image the VM boots from`). `vm destroy` keeps it, with the uploads: `tofu -chdir=<that directory> destroy`
  removes them, with the platform's credentials, once no VM boots from the image. A reserved
  address (`ip_id`, `eip_allocation_id`, `public_ip_name`, `nat_ip`) lets a stack's first install
  know its address, or another VM's (`--add-host`), before the VM exists: give it as `--host`.

  On a libvirt host whose security driver is AppArmor (Ubuntu, Debian), `virt-aa-helper` must be
  able to write each domain's profile; where its own profile denies it `sys_admin` or
  `sys_resource`, the domain does not start until a local rule allows them (`capability
  sys_admin,` and `capability sys_resource,` in
  `/etc/apparmor.d/local/usr.lib.libvirt.virt-aa-helper`, then `apparmor_parser -r
  /etc/apparmor.d/usr.lib.libvirt.virt-aa-helper`). The provider defines a domain, then starts it:
  a start that fails leaves the domain defined but not in the state, so `vm destroy` does not
  remove it, and a failed apply on the built-in libvirt module names the command that does:
  `virsh -c <uri> undefine <vm-name> --nvram`.
  `hyperv` has none: `--create opentofu` there needs `--module`. A **preview** module has not run
  on its platform yet: `vm create` prints `OpenTofu's <platform> module is a preview: it has not
  run on <platform> yet`, with the fallback (`vm destroy <host>` removes what it made, and
  `--create manual` writes the disk and the cloud-config and prints the steps by hand); a module
  loses the mark at its first passing run there. On a platform with a built-in module a terminal
  asks "OpenTofu or by hand", default OpenTofu on `truenas` and by hand on a preview; without a
  terminal and without `--create`, by hand. `--module` alone is `--create opentofu`, and refused
  with `--create manual`.
  - Credentials come from the environment only, as each provider reads them (`TRUENAS_URL` /
    `TRUENAS_API_KEY`; `PROXMOX_VE_ENDPOINT` / `PROXMOX_VE_API_TOKEN`; `VSPHERE_SERVER` /
    `VSPHERE_USER` / `VSPHERE_PASSWORD`; `AWS_*` or a profile; `GOOGLE_APPLICATION_CREDENTIALS`;
    `ARM_*` or the `az` login; a libvirt `uri` over SSH uses the SSH agent); no module declares one,
    and no `--tofu-var` should carry one: the variables are written to a file.
  - `--tofu-var NAME=VALUE` (repeatable) sets a module variable; one the module does not declare,
    one given twice, one of the contract's (below, which `vm create` writes from its own
    flags), and on TrueNAS's built-in module `pool`, `parent_dataset`, `nic_attach` and
    `insecure_skip_verify` (its flags and `TRUENAS_INSECURE_SKIP_VERIFY` give them) are refused. A required variable not given is asked on a terminal (its description as
    the question), else refused (`--tofu-var NAME=<value> is required by the module`); one with a
    default is asked too, empty keeping the module's default. The review lists them.
  - `--vm-name` is letters and digits on `truenas` (as TrueNAS names a VM); elsewhere lower-case
    letters, digits and `-`, starting with a letter, at most 63 (default the host name in lower
    case). `--pool`, `--dataset` and `--bridge` are TrueNAS's built-in module's alone.
  - **The module contract**, which `--module DIR` holds (its `*.tf`, `*.tf.json` and
    `.terraform.lock.hcl`, symbolic links followed, not its subdirectories, are copied into the
    VM's directory, so `vm destroy` needs DIR no more): it declares the variables `name`,
    `description`, `disk_gib` (the VM's disk in GiB: the app's `--disk` and what Kairos takes, which the disk records; an OVA's never under the capacity it declares), `image` (the disk on this machine, in the `--platform`'s format),
    `image_sha256`, `seed` (the NoCloud seed ISO on a hypervisor, the cloud-config file on a cloud:
    `--platform` decides) and `seed_sha256`, refused before anything is fetched when one is
    missing. It may declare `cpus` and `memory_mib` (then asked and given `--cpus` and `--memory`;
    a module that declares neither sizes the VM by a variable of its own, as the clouds' instance
    types do, and those flags are refused: `--cpus: the module sizes the VM by a variable of its
    own`), `image_format` (the format of `image`, the first of the `--platform`'s formats the
    release publishes: `qcow2` before `raw` on `truenas`, `proxmox` and `libvirt`), `dhcp` (true when the VM takes its IPv4 address by DHCP, false with `--network
    static`), `admin_user` and `ssh_public_key` (given `--user` and the `--ssh-key` file's key),
    `ssh_sources` (a list of CIDR: the sources SSH accepts, `--ssh-allow`'s or the cloud default's
    above, `["0.0.0.0/0", "::/0"]` for every source) and `open_ports` (a list of `{protocol,
    port, sources}`: the ports the VM's firewall opens besides SSH, the front's HTTP and HTTPS ports that
    something serves on in the `--tls` mode given (both when none is given; see the front under
    the installer on Docker), from every source (`["0.0.0.0/0", "::/0"]`), and each `l4` route's,
    from the sources the VM's firewall lets reach it as `vm create` knows them — every source for
    a route without an `allow_input`, one the front terminates TLS on (the front filters its
    port) and a secret list, which `vm create` is not given; else the list's value given (an
    answers file, `--set`) or the input's default, each address a prefix of its own (`/32`,
    `/128`); none (`[]`) when that list is empty, as the VM's firewall then refuses every
    source — for a firewall of the platform's, fixed at create:
    `vm create` logs, for each route with an `allow_input`, `l4 route <name>: the module's rule
    for <protocol>/<port> takes the sources <input> holds now; a later change of <input> on the VM
    does not reach it`; a module whose `open_ports` type is `{protocol, port}` drops `sources`), `image_id` (the built-in cloud modules': the image their image module made, below), and `nics` (a list of `{role, mac, attach}`: the NICs the release names, the
    primary first, each MAC the one the cloud-config names it by — empty for a machine's only NIC,
    whose MAC is the platform's — and each `attach` its network, empty for the module's own; an
    empty list when the release names none, for the module's one NIC). A release that names more
    than one NIC is refused with a module that declares no `nics` (`… declares no nics variable to
    attach them`). The built-in TrueNAS, libvirt, Proxmox and vSphere modules declare it: one NIC
    each, its MAC and its bridge or network (TrueNAS's default `--bridge`; libvirt's `network`,
    Proxmox's `bridge`, vSphere's `network` variable; vSphere's OVF network map takes the
    primary's). Its
    `*.tf` files are read as `tofu fmt` writes them (a variable block on one line, or closing at a
    lone `}`). It outputs `id` (what the platform calls the VM;
    required: `the module has no output id` fails the run after the apply, the state kept) and
    `address` (where the VM answers, empty when the platform does not tell); `vm create` reads them
    with `tofu output -json`, logs `OpenTofu created the VM <name> (<id>)`, and waits at `address`
    when neither `--host` nor a static address names one. Its steps may run `$STOWAGE_CTL` (the
    running `<short>-stow`), as the TrueNAS module runs `vm truenas`.
  - It needs OpenTofu's `tofu` on `PATH` (it runs `tofu version` first; the modules require
    1.8 or later) and, for TrueNAS's built-in module, in the environment only, `TRUENAS_URL` (`https://<host>[:<port>]`; plain
    `http://` is refused, as the key travels with every call) and `TRUENAS_API_KEY`; `TRUENAS_INSECURE_SKIP_VERIFY` (`1`, `true` or `yes`) accepts the host's
    self-signed certificate. These are the names OpenTofu's TrueNAS provider reads; the key is
    never written to a file. Each is checked before anything is asked.
  - After the product's questions it asks the VM's name, where it goes and how big — on TrueNAS
    `--vm-name` (default the host name's letters and digits), `--pool`, `--dataset` (under the pool, e.g. `vms`),
    `--bridge` (the bridge or interface the NIC attaches to), `--cpus` (default 2), `--memory`
    in MiB (default 4096, at least 1024) and `--disk`, the app's space in GiB (default 20, at
    least 4): the VM's disk is that and what Kairos takes of it, which the disk records (its
    boot, recovery and state partitions, sized from the OS bundle), and the review says both
    (`a 43 GiB disk (the OS 23 GiB, the app 20 GiB)`; an OVA's disk is never under the capacity
    it declares: 64 GiB, or Kairos's partitions and 8 GiB beyond them when that is more); a disk set recording none (published by an earlier Stowage) boots no VM, the
    channel's set then tried, then elsewhere the module's own variables. A release whose
    `shapes.kairos.size` states a size changes these: each default is its `recommended` size
    (else the default above, raised to its `min`), a value under its `min` is refused (`"1": a
    whole number, at least 2, the least <name> runs on`), and one under its `recommended` is
    logged (`2 CPUs: under the 4 <name> recommends`). A module that sizes the VM by a variable of
    its own logs the size it cannot check (`the module sizes the VM by its own variables: <name>
    runs on at least …; … recommended`); `--create manual` prints it before the steps (`Size the
    VM: <name> runs on …`). The review shows
    them with the address it will wait at (`the address its module returns, else <host>` where the
    module may return one). These flags, `--tofu-var`, `--host` and `--wait` are refused with
    `--create manual`.
  - The disk is fetched into `$XDG_CACHE_HOME/<name>/disks/<16 hex>/` (the first 16 hex digits of
    the disk artifact's digest, which the release records; the file's own SHA-256 is
    `image_sha256` below; `$XDG_CACHE_HOME` defaults to `~/.cache`), not `--out`, and kept, so
    a second VM of that disk reuses it. The VM's directory is
    `$XDG_STATE_HOME/<name>/vms/<hostname>/` (default `~/.local/state`, 0700; a relative
    `XDG_*` value is ignored, as the XDG spec says, and `HOME` must then be absolute; a path
    holding `%`, `\` or `"` is refused, as ssh would misread it): the module (the built-in one's
    `main.tf`, `variables.tf`, `outputs.tf`, `versions.tf` and `.terraform.lock.hcl`, or `--module`'s files),
    `terraform.tfvars.json` (the VM's name, place, size, the module's variables, the disk's and
    the seed's paths and SHA-256 — no credential), `platform` (`<platform> builtin` or
    `<platform> module`, which `vm destroy` reads), the seed (`seed.iso`, or `user-data.yaml` on a
    cloud), OpenTofu's `terraform.tfstate`, and `known_hosts` (the VM's SSH host key; with
    `--secrets`, `known_hosts.seed` holds the one-time key while it is pinned, and with given material `given.yaml.known_hosts`). A host name
    whose directory holds the variables (an apply ran, or was tried) is refused: `vm destroy` it
    first, or choose another `--hostname`. So is a `--vm-name` another VM there already has,
    whatever its case (on TrueNAS its zvol and staging dataset would be the same).
  - `tofu` runs in a session of its own: interrupted (Ctrl-C, or the run stopped), it is sent one
    interrupt and given 5 minutes to record what it made; a second Ctrl-C is passed on (OpenTofu
    then stops at once), and after 5 minutes it and what it started are killed. Interrupted
    while it waits for the VM, `vm create` says the VM is kept and names `vm destroy`.
  - It runs `tofu init`, `tofu apply -auto-approve` and `tofu output -json` there
    (`TF_IN_AUTOMATION=1`, `TF_INPUT=0`, and `STOWAGE_CTL` set to the running `<short>-stow`, which
    the module's steps run). TrueNAS's module creates a staging dataset `<vm-name>-staging` and a zvol `<dataset>/<vm-name>` of `disk_gib` (the app's `--disk` and Kairos's share; 16K blocks,
    LZ4), uploads the disk image (checked against its SHA-256 first; an upload already there at
    its size is not sent again) and converts it into the zvol, uploads the seed, creates the VM
    (UEFI, host CPU, `--cpus` cores, autostart) with the disk, a VirtIO NIC whose MAC is derived
    from the VM's name (the same at every start, so a DHCP lease follows it) — or one for each NIC
    the release names (`nics`), each with its MAC on its bridge — and the seed as its
    CD-ROM, and starts it. A changed disk or seed rebuilds what depends on it, powering the VM off
    and on. A failed apply keeps the state, and the error names `vm destroy`.
  - It then waits, up to `--wait` (default 20m; past it, `not ready after <wait> (last: <what the
    VM last answered>); the VM is kept: look at <the platform's console>, or `<short>-stow vm
    destroy <name>``, the console named per `--platform` — TrueNAS's display, Proxmox VE's console,
    `virsh console`, vSphere's, Hyper-V Manager's, the machine's screen, Azure's serial console,
    Google Cloud's serial port output, EC2's serial console or system log, Scaleway's Console
    tab), polling every 10 s over `ssh` (batch mode, the
    private key next to `--ssh-key`'s `.pub` alone when it exists, else ssh's own keys and agent:
    the VM's sshd drops a client after 4 failed keys, so an agent holding more needs that private
    key next to the `.pub`) at `--host` (an address or a
    host name; an IPv6 address bracketed or not, `[2001:db8::7]` taken as `2001:db8::7`; default
    the static IPv4 address, else the module's `address`, else the host name — with DHCP and a static IPv6 address alone,
    `--host` gives that address when this machine reaches the VM over it; a DHCP server must
    register it) until `<short>-stow status
    --json` there answers from a system that is not Kairos's recovery (a first boot runs the
    recovery system, then resets and reboots into the active one; a recovery system whose
    automatic reset failed, `"reset_failed"` in its status, fails the wait at once: `the first
    boot gave up in Kairos's recovery system, which stays: kairos-reset.service failed: <its last
    journal lines>; the VM is kept: look at <the platform's console>, or `<short>-stow vm destroy
    <name>` removes it`) and reports the install
    finished (its stack up and the provision hook done; `"installing": true` until then; a
    first install that failed, `"install_failed": "<why>"` — also when the boot's install failed
    before writing the install, and not while the boot runs it again — fails the wait at once
    with why, the `journalctl -u <name>` to read and the `sudo <short>-stow install` that runs it
    again; the key of the system that answered is kept) — or,
    when a secret input is due, until it answers at all, and prints the `sudo <short>-stow install`
    to give it (which, on the VM, first waits for the preload unit `<name>-preload.service` to
    have loaded the release's images: run at once, it is not refused while the boot loads them;
    a preload that failed is run again; when it fails again the install goes on if every image is
    loaded, saying so, and is refused naming the unit, its failure and the `journalctl -u` to
    read when one is missing; a preload unit that failed already is not run again when every image
    is loaded, saying so; before any wait, the install is refused at once when this user cannot
    write its directory, or the nearest one above it that exists (`D is not writable by this user:
    the VM's install runs as root, \`sudo <short>-stow install\``; as root, `D is not writable, by root
    either (a read-only filesystem?)`), `--dry-run` checks the same and waits for nothing, saying
    what a run would wait for, and a host whose init is not systemd (`systemctl` there, but no
    `/run/systemd/system`) waits for nothing). On a disk of another release it also waits until the VM answers from this release's OS (up to 40 min by default), saying what the old OS's boot reported if that wait ends. With `--secrets` it waits until the VM answers, then runs `sudo systemctl start
    <name>.service && sudo <short>-stow install --secrets - --no-input` there over the same SSH (the
    boot unit first: it runs once the release's images are loaded), the file on its standard input (a secret
    input still due: it prints how to give both instead — `chmod 600 FILE; scp -p FILE
    <user>@<host>:secrets.yaml` (an IPv6 host bracketed, `<user>@[2001:db8::7]:secrets.yaml`),
    then `ssh -t <user>@<host> 'sudo <short>-stow install --secrets secrets.yaml; rm
    secrets.yaml'`, on a terminal that asks the rest); a failure keeps the VM and says that
    `status` tells where it stopped, a re-run of `sudo <short>-stow install` finishes an install
    begun, and those steps make one that did not begin. By hand, the steps end with them. The recovery and active systems have different SSH host keys: each attempt takes
    the key on first contact into a file of its own, and the VM's `known_hosts` keeps the key of
    the attempt that answered. With `--secrets`, every call is pinned to the one-time host key
    instead (`StrictHostKeyChecking=yes` against `known_hosts.seed`): a VM serving another key is
    never sent the secrets, and is not ready until its wait ends. Once the install took them, a
    last call through the pinned key reads the VM's own ed25519 key into `known_hosts`, removes
    the one-time key and its drop-in, marks it retired and reloads sshd, and prints the VM's key's fingerprint; a
    failure there is a warning (the next boot retires the key). A VM not ready in time is kept; the error says so.
  - It prints `ssh <user>@<host> <short>-stow status` and where the state is.
- **`vm destroy HOSTNAME`** destroys a VM `vm create --create opentofu` made: `tofu destroy` of its
  module (the VM, its disk and data, its uploads — on a cloud, the disk's image and its uploads
  are the image module's and stay, as does a reserved address), then its state directory, holding
  `.<host>.lock` beside it meanwhile, which stays (refused while a `vm attach` runs on the VM). It needs the same
  environment and `tofu` (the TrueNAS API's variables for TrueNAS's built-in module, as its
  `platform` file records; a VM made before that file is TrueNAS's), asks on a terminal ("Destroy
  the VM H: its disk, its data and its uploads on <platform>", on a cloud "its disk and its data
  (the disk's image stays for the next VM of it)", default no), and on a cloud prints after it
  `The disk's image stays, for the next VM of it: each state in $XDG_STATE_HOME/<name>/images holds one,
  which `tofu -chdir=<state> destroy` removes (its uploads included) once no VM boots from it.`; without a terminal it refuses
  unless `--yes`. Before asking (and before `--yes` goes on) it looks, as `vm create` does (in
  `--registry` when given; for 30 s at most), for a disk set that could make the VM again, and
  warns when the registry answers there is none (the channel's release read, or one this
  Stowage does not read) (`Warning: <name> <version> has no disk in <prefix>, nor does its channel <channel>: …
  (stowage ship --disks); until then, a VM destroyed is not created again.`) or when the
  registry cannot be read (`Whether a VM of <name> <version> can be created again is not known:
  …`); neither stops the destroy. On TrueNAS, snapshots of the zvol and the staging dataset (a periodic task's) are
  deleted first, as TrueNAS refuses to delete a dataset that has any. The cached disk stays.
- **`vm attach HOSTNAME --interface ROLE=SPEC`** attaches a NIC for a role of the release (a
  secondary NIC its `host.interfaces` names, which the VM's cloud-config does not) to a VM `vm
  create --create opentofu` made with TrueNAS's built-in module, keeping the VM and its data:
  `SPEC` is `vm create --interface`'s (`dhcp`, `static,address=A/P` or `none`, the default; then
  `mac=`, default generated, and `attach=`, the bridge, default the primary's). Over SSH, as `--user`
  (default your user name, else `sysadmin`) with `--ssh-key`'s key (vm create's defaults), at
  `--host` (default HOSTNAME: give the address `vm create` waited at, its `--host`), checked
  strictly against the key `--known-hosts-line` gives (see `vm kit`), else the host key `vm create` recorded for that address in the state directory
  (`known_hosts`; none, or none for that address, is refused: a VM whose install was finished by
  hand has none, and its host key, checked on its console, goes there), it reads the VM's boot id (one that does not read is refused); runs `tofu init` and
  `tofu plan -detailed-exitcode -out=attach.tfplan -var nics=…` (in the state directory; the NICs
  with the new one appended, the primary's entry unchanged, or added as it was applied when the VM
  had one NIC: the module's own MAC) and reads the plan (`tofu show -json`): it goes on with a plan
  that creates the attached role's NIC device and replaces the power step, every other resource
  unchanged, or with one that changes no resource when the state holds the NIC already (for a new
  role it is refused: `the VM's OpenTofu plan does not add the NIC <role> …; nothing changed`); anything else
  (drift on the disk, the VM, the datasets or another NIC; the power step alone; the NIC without
  the power step: an earlier vm attach left half done) is refused before the VM or the state
  changes: drift as `the VM's OpenTofu plan changes more than its NIC: <address> (<actions>), …;
  nothing changed`, with the `STOWAGE_CTL=<ctl> tofu -chdir=<state> plan -var 'nics=…'` that shows
  it (drift is the operator's to resolve: an apply would carry it out); a half-done attach as
  `does not add the NIC with the VM's power cycle`, with the way to finish it by hand: power the VM
  off on TrueNAS, then `STOWAGE_CTL=<ctl> tofu -chdir=<state> apply` with the TrueNAS API's
  variables, and the same `vm attach` command, which the message repeats with its flags, checks it. Then it runs `sudo <short>-stow network attach` on the VM (below), which
  records the NIC, writes the NICs into the module's `nics` in `terraform.tfvars.json`, and applies
  the saved plan, which powers the VM off and on (TrueNAS applies a device at a start); an apply
  that fails says the NIC is recorded on the VM and in the state's variables, and names the same
  way to finish by hand once its cause is fixed. The plan file is removed whatever happens, and
  `vm attach` and `vm destroy` hold `.<host>.lock`, beside the state directory (kept: a lock file removed while held would exclude no run that opened it), for their whole run: one on a VM at a time
  (`another vm attach or vm destroy runs on it`). It waits up to `--wait` (default 10m) for the VM to answer from a new boot naming a NIC
  by the role (`has
  not booted again naming a NIC <role> after …`, with the journal to read, when it does not). Run
  again for a role the state attaches, it goes on with the state's MAC and bridge (another `mac=`
  or `attach=` is refused) and, when the plan has no changes, reboots the VM instead (`sudo
  <short>-stow reboot`: its 30 s drain, its refusal the error) for the addressing given. Refused before the VM is changed: a role that is not one of the release's
  NICs, the primary, a MAC another NIC of the state has (the module's own derived one included),
  a VM with no state or no VM name in it, a VM made with another module, a `--user` that is not a
  user name, a `--host` holding spaces, quotes, `%`, `@` or starting with `-`, a state whose module predates NICs by role
  (no `nics` variable: `has one NIC`), and `tofu` or the TrueNAS API's variables missing, as `vm
  destroy` needs them. On another platform, or with such a module: attach the NIC there, run `sudo
  <short>-stow network attach --role ROLE --mac MAC` on the VM, then power it off and on.
- `vm truenas import-image|seed|start|stop|drop-snapshots` (hidden) are the module's steps, run
  by OpenTofu: each reads `TRUENAS_URL`, `TRUENAS_API_KEY`, `TRUENAS_INSECURE_SKIP_VERIFY` and its
  own variables (`IMAGE`, `IMAGE_SHA256`, `STAGING_DIR`, `ZVOL`; `SEED`, `SEED_PATH`; `VM_ID`;
  `DATASET`) from the environment, and talks to TrueNAS's JSON-RPC API (`/api/current`) and its
  `/_upload/` endpoint.
