<p align="center">
  <img src="docs/img/logo.svg" alt="Stowage" width="300">
</p>

<p align="center">
  <b>Package a small container stack once. Ship it signed to your OCI registry.<br>
  Install, upgrade, roll back and recover it on a Docker host, a Kubernetes cluster or a
  self-updating VM appliance — with one installer and one set of verbs.</b>
</p>

<p align="center">
  <a href="LICENSE"><img alt="Licence: Apache 2.0" src="https://img.shields.io/badge/licence-Apache%202.0-7571F9"></a>
  <a href="CHANGELOG.md"><img alt="Status: pre-1.0" src="https://img.shields.io/badge/status-pre--1.0-F780E2"></a>
  <img alt="Shapes: Docker, Kubernetes, Kairos VM" src="https://img.shields.io/badge/shapes-Docker%20%C2%B7%20Kubernetes%20%C2%B7%20Kairos%20VM-02BF87">
  <a href="#packed-with-stowage"><img alt="packed with Stowage" src="docs/img/packed-with-stowage.svg" height="20"></a>
</p>

<p align="center">
  <img src="docs/img/install.gif" alt="The installer of examples/minimal: install directory, how the front serves, the administrator email, the review screen, then the stack up and its portal URL" width="820">
  <br><sub>An operator installs <a href="examples/minimal">examples/minimal</a>: one bootstrap
  script, a form, a review screen, a running stack. Recorded with
  <a href="docs/demo/">docs/demo</a>.</sub>
</p>

Stowage is for the team that ships a product — typically one Postgres, a backend or two and a
web frontend behind TLS — to *other people's* infrastructure: a customer's server, their cluster,
their hypervisor, sometimes a site without internet. You describe the stack once, in a
`stowage.yaml` beside your compose file. Stowage turns it into signed, versioned releases in one
registry, and gives each operator an installer that asks a few questions, starts the stack fully
configured and keeps managing it afterwards.

Licence: Apache 2.0 · Design: [docs/DESIGN.md](docs/DESIGN.md) · Release notes:
[CHANGELOG.md](CHANGELOG.md) · Status: pre-1.0, [see below](#status).

## In thirty seconds

```bash
# The publisher: check the project, then build what changed, sign it and push it.
stowage -C notes lint
stowage -C notes ship

# The operator: fetch the installer, answer the form, get a running stack.
docker run --rm registry.example.com/notes/notes-installer > notes.sh
sh notes.sh

# Later, from the install, signed and reversible.
./notes/notes-stow upgrade --version 1.1.0
./notes/notes-stow rollback
```

## What it brings

- **One description, three deploy shapes.** The same images run under Docker Compose on one
  host, under Helm on Kubernetes (your chart, given the release's images by digest), and inside
  a Kairos VM appliance — an immutable OS image with A/B updates, built from your release and
  published as `qcow2`, `raw`, `ova`, `vhd` and `gce` disks.
- **The registry is the only way in.** Images, the chart, the OS bundle, the disks, the
  installer and a release manifest all live in one OCI registry under your prefix. Nothing
  reaches an operator any other way, and every install upgrades from there (or from a mirror).
- **Incremental, reproducible releases.** Every artifact has a content digest of exactly what
  can change it. `stowage ship` rebuilds only what changed, re-tags the rest, and re-running it
  publishes nothing new.
- **Signed end to end.** With a cosign key, every artifact is signed by digest before its tag
  moves. Installs carry your public keys and refuse an upgrade not signed by one of them.
- **An installer operators can run.** A terminal form with inline validation and a review
  screen — or, for automation, flags, environment variables or an answers file, with no
  terminal at all. It generates the secrets, puts a TLS front in place (ACME, Cloudflare DNS,
  your own certificate, a local CA, or plain loopback), runs your provision hook, and prints the
  portal URL.
- **Day-2 operations, not just day 1.** Re-running the installer reconfigures. `upgrade`,
  `rollback` (guarded by your schema's rollback barriers), `reset-db`, secret rotation without
  downtime, offline mirrors, and on the VM a health gate, encrypted backups and a host daemon.
- **Nothing project-specific in Stowage.** Everything that differs between projects — version
  scheme, changelog format, registry auth, health check, schema compatibility, backup — is a
  provider with built-in choices and a `command` escape hatch (JSON in, JSON out).

## The lifecycle, end to end

<p align="center">
  <img src="docs/img/lifecycle.svg" alt="The publisher describes the stack in stowage.yaml, checks it with lint and plan, and ships it signed to one OCI registry; operators install and manage it from there on a Docker host, a Kubernetes cluster or a Kairos VM, directly or through an offline site mirror" width="960">
</p>

| Stage | Who | What Stowage does |
|---|---|---|
| **Describe** | developer | `stowage.yaml` names the images, the compose file (the single service definition), the front's routes, the health check, the database, the volumes, the secrets to generate and the inputs to ask. `stowage lint` checks the whole project against the [contracts](#the-contracts-a-project-must-honour) and names the `key` or `file:line` to fix. |
| **Version** | developer | The version comes from your changelog, a file, a git tag or a command; a scheme (CalVer, SemVer, regex, command) validates and orders it. Stowage never compares versions as strings. `stowage.lock` pins the Stowage that publishes. |
| **Publish** | developer, CI | `stowage ship` builds what changed, pushes it by digest, signs it, builds the installer and the release manifest, then moves the channel's pointer. `verify`, `promote` (beta → stable without rebuilding), `yank`, `prune`, `releases`. `stowage hook install` ships each version bump on commit. |
| **Install** | operator | `docker run --rm <prefix>/<name>-installer:latest > <name>.sh` gets the bootstrap script, which carries the installer, `<short>-stow`; then `sh <name>.sh` on Docker; `sh <name>.sh k8s install` on Kubernetes; `sh <name>.sh vm create` for a VM (seed ISO or cloud user-data, or the VM created with OpenTofu: TrueNAS; libvirt, Proxmox VE, vSphere, AWS, Google Cloud and Azure as previews; or a module of your own). Inputs asked once, secrets generated once, provision hook run, portal URL printed. |
| **Operate** | operator | `status`, a re-run to change inputs, ports or the TLS mode (the changes logged, the previous values kept), `provision`, `secrets rotate` (stage, swap, finish: the old value accepted meanwhile), `secrets export` (age-encrypted, to rebuild a machine), `reset-db`. |
| **Upgrade** | operator | `upgrade --version V` fetches that release's installer, checks its signature against the keys the install trusts and brings the stack up on the new images — on Docker after a `pg_dump` snapshot, on Kubernetes through Helm. On the VM, `updates` lists what the channel offers and `upgrade` reboots into the new OS. |
| **Roll back** | operator | `rollback` returns to the previous release — unless a migration since then is a rollback barrier your schema-compat provider reports, in which case it refuses and names it. On the VM the health gate does it alone: an upgraded boot that is not healthy in time falls back to the previous system. |
| **Recover** | operator | On the VM: scheduled and pre-upgrade backups (`pg_dump` and the volumes the project lists, encrypted to age keys, uploaded with rclone to any rclone remote), `restore` into a side database swapped in once loaded whole, and a disaster recovery from exported secrets. |
| **Disconnected sites** | operator | `mirror export` carries a release, signatures included, as one file; `mirror import` loads it into a local registry; installs and upgrades then read the mirror (`--registry`, `--registry-ca`). |

### What each shape gets

| | Docker | Kubernetes | Kairos VM |
|---|---|---|---|
| Install with the terminal form or an answers file | ✓ | ✓ | ✓ (`vm create`, first boot) |
| TLS front (Caddy) built in | ✓ | — (your chart's ingress) | ✓ |
| Signed `upgrade` / `rollback` | ✓ | ✓ (Helm) | ✓ (A/B reboot) |
| Rollback refused past a schema barrier | ✓ | ✓ (`shapes.kubernetes.schema`) | ✓ |
| `reset-db`, `secrets rotate` | ✓ | ✓ | ✓ |
| `secrets export` | ✓ | — | ✓ |
| Install from a mirror | ✓ | ✓ | ✓ |
| A failed upgrade undone on its own | — | ✓ (Helm `--wait`, rolled back when not ready) | ✓ (health gate) |
| Scheduled encrypted backups, `restore` | ✓ (systemd timer) | — | ✓ |
| Disk-pressure hook | ✓ (systemd timer) | — | ✓ |
| Host daemon, firewall, network change with auto-revert | — | — | ✓ |

### What it is not

- **Not a platform.** One install is one host (Docker, VM) or one Helm release. Stowage
  orchestrates nothing across hosts.
- **Not a chart generator.** On Kubernetes you bring your own chart; Stowage packages it and
  hands it the release's images, secrets and pull account.
- **Postgres only** for the database-aware features (snapshots, `reset-db`, schema barriers,
  backups) in this version.
- **Pre-1.0.** The first version targets parity with an existing production deployment of this
  kind on its three shapes; the format is `stowage/v1` but nothing has been released yet.

## Package an application, step by step

This walk-through packages a stack like [examples/minimal](examples/minimal/) — Postgres, an
`api`, a `web` frontend — for the Docker shape, then adds Kubernetes and the VM. Each step names
the reference section that holds the full rules. [examples/full](examples/full/) uses every key
and is the place to copy from once the basics work.

### 0. What you need

- **Stowage itself.** Download `stowage-linux-amd64`, `SHA256SUMS` and
  `SHA256SUMS.sigstore.json` from the project's GitHub release, check that the checksums were
  signed by this repository's release workflow from the release's tag (cosign v3), then the
  binary against them:

  ```bash
  cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
    --certificate-identity https://github.com/norvexlabs/stowage/.github/workflows/release.yml@refs/tags/v<x.y.z> \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  sha256sum -c SHA256SUMS
  ```

  A release made while the repository is public also carries a build provenance attestation:
  `gh attestation verify stowage-linux-amd64 --repo norvexlabs/stowage --signer-workflow
  norvexlabs/stowage/.github/workflows/release.yml --source-ref refs/tags/v<x.y.z>`. Put the
  binary on `PATH` as `stowage`. Or build it: Docker is the only build requirement, and
  `make build` writes `./bin/stowage` (see
  [Building and testing Stowage](#building-and-testing-stowage)).
- **On the publishing machine**: Docker with `buildx`, network access to your base images, and
  `cosign` v3 when you sign. The `kairos` shape also needs privileged containers and loop
  devices (the disks are made by auroraboot).
- **An OCI registry** you can push to, and a read-only account for installs (or public images).

### 1. Lay out the project

```
<project>/
├── stowage.yaml          # what to package (this guide)
├── stowage.lock          # the Stowage that publishes it (step 9)
├── CHANGELOG.md          # or VERSION + changes/<v>.md — the version source (step 6)
├── api/Dockerfile
├── web/Dockerfile
└── deploy/
    ├── compose.yaml      # the single service definition (step 3)
    └── chart/            # only with the kubernetes shape (step 11)
```

Every path in `stowage.yaml` is relative to the project directory and stays inside it. These
placeholders run through the rest of this README:

| Placeholder | Is | In this guide |
|---|---|---|
| `<project>` | the project's directory, given to `stowage -C` | `notes/` |
| `<name>` | the spec's `name:` key; it names everything an operator sees | `notes` |
| `<prefix>` | `registry.prefix` | `registry.example.com/notes` |
| `<name>.sh` | the installer's bootstrap script (step 10) | `notes.sh` |
| `<short>` | the spec's `short_name:` key, absent is `<name>` | `notes` |
| `<short>-stow` | the installer and lifecycle command inside it | `notes-stow` |

Start the spec with its identity:

```yaml
apiVersion: stowage/v1
name: notes                # → images notes-<key>, the installer notes-stow (short_name: absent), /etc/notes on the VM
stowage:
  requires: ">=0.1 <1"
```

### 2. Declare the images

```yaml
images:
  api: { context: api, dockerfile: api/Dockerfile, inputs: [api/] }
  web: { context: web, dockerfile: web/Dockerfile, inputs: [web/] }
  postgres: { ref: "postgres:17-alpine@sha256:…" }       # an upstream image, pinned by digest
```

- A built image's `inputs` are the paths whose content decides whether it is rebuilt (default:
  the whole context less `.dockerignore`).
- Every `FROM` is pinned by digest. A Dockerfile that declares `ARG STOWAGE_PROJECT_VERSION`
  receives the version (and is rebuilt on each one); any other image is only re-tagged on a
  version bump.
- An image that runs as a user and writes to a volume creates that directory with that owner
  (a new named volume takes it).

### 3. Write the compose file

Your existing compose file mostly carries over. What changes:

```yaml
services:
  api:
    image: ${STOWAGE_IMAGE_API}                    # never a literal image, never build:
    environment:
      DATABASE_URL: postgres://notes:${POSTGRES_PASSWORD}@postgres:5432/notes   # a generated secret
      SESSION_SECRET: ${SESSION_SECRET}            # a generated secret
      SECURE_COOKIES: ${SECURE_COOKIES:-true}      # a safe default: only loopback mode relaxes it
      PUBLIC_URL: ${STOWAGE_URL_APP}               # the route's public URL, given by Stowage
      LOG_LEVEL: ${LOG_LEVEL:-info}                # an optional tunable
```

- Every `${VAR}` without a default must be an input, a secret, a safe default or a route's
  domain; `STOWAGE_*` names are provided by Stowage. Leave out a top-level `name:` (Stowage names each
  install). The services need no published ports: the TLS front Stowage adds (`stowage-front`) is the way in.
- Give the database a healthcheck and make the services depend on it: the Docker installer
  waits for every healthcheck before it reports success.

Then tell Stowage where it is, and what the installer generates and asks:

```yaml
stack: { compose: deploy/compose.yaml }
secrets:                                  # generated once, at the first install
  - { key: SESSION_SECRET,    gen: hex32 }
  - { key: POSTGRES_PASSWORD, gen: pass18 }
safe_defaults: { SECURE_COOKIES: "true" }
inputs:                                   # asked by the installer's form, or given as flags / answers
  - { key: ADMIN_EMAIL, prompt: "Administrator email", required: true }
```

### 4. Route traffic, define health, describe the data

```yaml
front:
  tls_modes: [acme, byo, internal, none]          # what the operator may choose
  routes:
    - { name: app, domain_input: APP_DOMAIN, upstream: web:8080 }
    - { name: api, domain_input: API_DOMAIN, upstream: api:8000 }
  portal: app                                      # the URL printed at the end
  loopback_ports: { app: 8080, api: 8000 }        # mode none: plain HTTP on 127.0.0.1
  loopback_env: { SECURE_COOKIES: "false" }

health: { provider: http-status, service: api, port: 8000, path: /healthz/ready, expect: 200 }

database:
  engine: postgres
  service: postgres
  name: notes
  roles: { migrate: notes, app: notes }
  schema_compat: { provider: sql, … }               # or command, or none — see below

data:
  volumes: { postgres: { uid: 70 }, uploads: { uid: 1000 } }
  reset: [postgres, uploads]                        # what reset-db wipes
```

The front routes by hostname only; each route's domain is an input the installer asks in TLS
mode. `schema_compat` is what makes `rollback` safe: it reports the schema's migration number
and whether a migration since an upgrade is a rollback barrier
([Schema compat](#the-contracts-a-project-must-honour)). Without it (`none`), a rollback is not
checked against the database.

### 5. Add a provision hook (optional)

The only application code Stowage runs at install time — creating the first administrator, for
example. It runs in one of your images after the stack is up, on every install and re-run, so it
must be idempotent; it receives the keys its `env` lists (and nothing else of the install's
secrets) and prints `KEY=VALUE` lines that the install keeps and hands back to its next run:

```yaml
hooks:
  provision: { provider: container, image: api, cmd: [notes-api, create-admin], env: [ADMIN_EMAIL] }
```

### 6. Choose how versions and channels work

```yaml
version:   { source: { provider: file, file: VERSION }, scheme: { provider: semver } }
changelog: { provider: dir-per-version, dir: changes/ }      # changes/1.0.0.md
channels:
  stable: { tag: "{v}", mutable: false, latest: true }       # moves the installer's :latest
  beta:   { tag: "beta-{v}", mutable: true }
```

The defaults are a Keep a Changelog `CHANGELOG.md` as the version source and CalVer
(`YYYY.M.MICRO`). The current version must have a changelog section: it ships inside the release,
and the VM's `updates` shows it.

### 7. Point at the registry, and sign

```yaml
registry:
  prefix: registry.example.com/notes
  pull: { provider: token, user: notes-pull, token_env: STOWAGE_PULL_TOKEN }   # baked into every installer
  push: { provider: token, user: publisher,  token_env: STOWAGE_PUSH_TOKEN }   # the publisher's only
signing:
  provider: cosign-key
  key: env://NOTES_SIGNING_KEY              # or a file, or a KMS / Vault URI
  public_keys: [keys/release.pub]           # what every install trusts
```

The pull account is read-only and ships to operators; it may not share a secret with the push
account. Create the key pair with `cosign generate-key-pair`; keep the private key out of the
repository and commit the public key. To rotate, list the old and the new public keys for a
while.

### 8. Declare the shapes

```yaml
shapes:
  docker: {}
```

### 9. Check, plan, publish

```bash
stowage -C <project> lock                 # writes stowage.lock: commit it
stowage -C <project> lint                 # fix every error; warnings do not block
stowage -C <project> plan                 # the build graph and what a ship would build
stowage -C <project> ship                 # build, push, sign, move the stable pointer
stowage -C <project> verify               # check the published release and its signatures
stowage -C <project> releases
```

Ship again without a change and nothing is built or pushed. Bump the version and ship: only
what the change reaches is rebuilt. To try a version first, ship it to the beta channel
(`stowage -C <project> ship --channel beta`); once it has proved itself,
`stowage -C <project> promote 1.1.0 --from beta --to stable` re-tags that release onto stable
without building (from the tree that published it). To ship each version bump on commit:
`stowage -C <project> hook install`.

### 10. Install it as an operator would

An operator needs Docker and the registry's address — and, when the images are private, a
`docker login` with the pull account to fetch the installer image. That image does not install
anything itself: run, it prints `<name>.sh`, a POSIX `sh` script that carries `<short>-stow`, and
`<short>-stow` carries the release (its manifest, the compose file, your public keys, the pull
account). Running the script unpacks `<short>-stow` to a private directory, runs it with the
script's arguments, and deletes it; the install keeps its own copy, `./<name>/<short>-stow`, for
everything after.

```bash
docker run --rm <prefix>/<name>-installer:latest > <name>.sh   # get the bootstrap script
sh <name>.sh                                      # install: the form asks mode, domains, inputs, then a review
sh <name>.sh --no-input --tls internal \
   --domains app=notes.lan,api=api.notes.lan --set ADMIN_EMAIL=ops@example.com   # the same, unattended
./<name>/<short>-stow status
./<name>/<short>-stow upgrade --version 1.1.0
./<name>/<short>-stow rollback
```

For `notes`: `docker run --rm registry.example.com/notes/notes-installer:latest > notes.sh`, then
`sh notes.sh`, and `./notes/notes-stow` afterwards. `<short>-stow script` prints the script again, to
carry the installer to another host.

The last two lines need a later release: ship 1.1.0 (step 9), upgrade to it, roll back. That is
the loop to try before your operators do.

### 11. Add Kubernetes and the VM appliance

- **Kubernetes**: add `kubernetes: { chart: deploy/chart }` to `shapes`. Your chart runs the
  release's images by digest from `.Values.stowage.images`, reads the secrets from the Secret
  named in `.Values.stowage.secret` and pulls with `.Values.stowage.pullSecret`
  ([Helm chart contract](#the-contracts-a-project-must-honour);
  `examples/minimal/deploy/chart` is a working one). Operators run `sh <name>.sh k8s install`.
- **Kairos VM**: add `kairos` with a `base` distribution and the `kairos_init` image, both pinned
  by digest, and the `disk_formats` to publish. `ship` then also builds the OS bundle (about
  3 GB, several minutes) and the disks. Operators run `sh <name>.sh vm create`, boot the disk with
  its seed, and manage the VM with `<short>-stow` on it. Add a `backup` provider for the VM's
  encrypted backups, `health_gate` to tune the boot check, and `host_api` for services that may
  call the host daemon.

### Stacks that are not only HTTP

A control plane with daemons of its own (raw TCP or UDP, its own TLS, its own config formats, an
API it is set up through) packages without changing the software, through the compose file:

- **Ports clients dial as they are** are `l4` routes (see "Spec keys"), the endpoint the stack
  advertises given by the route's `address_input` (`STOWAGE_L4_<ROUTE>`), the sources the VM's
  firewall lets reach it by its `allow_input`. The front can move off 443 with
  `front.https_port`, when a daemon owns it.
- **Material minted once** (a CA, a server key and certificate, a token): a one-shot service of a
  small image (openssl, step-cli) writes it into a named volume, keeping what it finds there, and
  the services that read it `depends_on` it with `condition: service_completed_successfully` and
  mount the volume `:ro`. `up --wait` counts a one-shot done when it exits 0. Its inputs (a
  hostname for a SAN) are inputs like any other; its policy is the project's. List the volume in
  the backup's `volumes`, so a restore brings the material back with the data it belongs to.
- **A daemon's own config format**: a thin entrypoint renders it from the environment (the
  recorded values compose interpolates) and from the volume's material, then `exec`s the daemon
  unchanged; or compose `configs:` with interpolated `content:`.
- **Setting up the control plane through its API** (entities, enrolment tokens): a long-lived,
  idempotent compose service that waits for the API, makes what is missing and hands what the
  daemons need through a shared volume, one marker file per finished step. The provision hook
  is not the vehicle: it mounts no project volume and its results go back to it alone.
- **Health behind authentication**: a `command` health provider, in an image holding a client of
  the API, or an unauthenticated readiness endpoint.
- **An optional daemon**: a compose profile an input's `profile` turns on.

### What to read next

- [The contracts a project must honour](#the-contracts-a-project-must-honour) — every rule
  `lint` enforces, and the contract of each provider and hook.
- [Spec keys](#spec-keys) — every key of `stowage.yaml`.
- The installer sections below — what an operator sees and can do on each shape.

## Packed with Stowage

A project that ships with Stowage can say so in its README:
[![packed with Stowage](docs/img/packed-with-stowage.svg)](https://github.com/norvexlabs/stowage)

```markdown
[![packed with Stowage](https://raw.githubusercontent.com/norvexlabs/stowage/main/docs/img/packed-with-stowage.svg)](https://github.com/norvexlabs/stowage)
```

The logo ([`docs/img/logo.svg`](docs/img/logo.svg), the mark alone
[`docs/img/mark.svg`](docs/img/mark.svg), a square icon [`docs/img/icon.svg`](docs/img/icon.svg) /
[`icon-512.png`](docs/img/icon-512.png), the repository's social preview
[`social-preview.png`](docs/img/social-preview.png)) uses the installer's colours: indigo `#7571F9`,
fuchsia `#F780E2`, green `#02BF87`. Use them to point at Stowage, not as your own product's logo.

## Status

The publisher and the three installers are built and run end to end in the test suites, against
a real registry, the local Docker engine, a kind cluster and a VM on TrueNAS
([DESIGN § 11](docs/DESIGN.md#11-phases) lists the phases).

| Built | Not built yet |
|---|---|
| `lint`, `project-version`, `changelog`, `plan`, `lock`, `hook post-commit`, `hook install` | |
| `ship`, `verify`, `promote`, `yank`, `prune`, `releases` (images, the front, the Helm chart, the Kairos OS bundle and its disk images + `latest-<format>`, each cloud's importable image (`cloud_publish`) registered in the publisher's AWS, Azure or Google Cloud account, installer image + `:latest`, release manifest) | registering on AWS and Azure (written, run on Google Cloud only) |
| every version / changelog provider, registry client, release manifest, cosign signing | |
| inputs digests and the build graph | |
| installer (Docker): loopback and every TLS front (`acme`, `dns-cloudflare`, `byo`, `internal`), terminal form, `install`, re-run (changes the inputs and the front's ports), `status`, `provision`, `script`, `upgrade` (`--version` fetches the release's installer), `rollback`, `reset-db`, `--dry-run`, `--offline`, a mirror and its CA (`--registry`, `--registry-ca`); `mirror export` / `mirror import` (a release carried as one file to a site without internet); `secrets rotate`, `secrets export` and `--secrets` (a machine made again) | scheduled backup, restore, chart library, `upgrade --reset-db` |
| installer (Kubernetes): `<short>-stow k8s install`, `upgrade` (`--version`), `rollback`, `status`, `reset-db`, `uninstall` (`--data`), `provision` (the hook as a Job after the release, its outputs in a Secret and handed back, `--set`), `secrets rotate`, `--dry-run`, `--no-provision`, `--image-registry`, `--registry-ca` | barrier pre-flight before `rollback` |
| installer (Kairos VM): `<short>-stow vm create` (the disk image by digest, the cloud-config on a seed ISO or as user-data, the platform's steps; or the VM created with OpenTofu — TrueNAS's module, the previews for libvirt, Proxmox VE, vSphere, AWS, Google Cloud and Azure, or the operator's own holding the module contract — waited for until it installed; from a mirror whose CA the VM then trusts) and `vm destroy`; `<short>-stow` in the OS bundle — the first install from the cloud-config's settings, `install` from the preloaded images, the boot (install up again, upgrade to the OS's release, back to the replaced one), the host daemon and its verbs (`status`, `health`, `events`, `ps`, `logs`, `restart`/`stop`/`start`, `provision`, `updates`, `upgrade`, `rollback` to the passive system, `reset-db`, `network` with its auto-revert, `backup` — scheduled, before an upgrade, or now, encrypted to age keys and uploaded with rclone; `verify` of proposed settings —, `restore`, `secrets export` and `rotate`, `reboot`, `shutdown`), its disk-pressure watch (`hooks.maintenance.disk_pressure`), its app socket for the services `host_api` names, the health gate judging each boot (Kairos boot assessment: an upgraded boot not healthy in time falls back to the passive system, unless going back is refused at a rollback barrier: it is then kept), a default-deny firewall (`vm create --ssh-allow`), the time zone (`--timezone`), a disaster recovery (`vm create --secrets`) | |

## Reference — for people and AI agents

A project is a directory holding `stowage.yaml`. Every path in it is relative to that directory.
Start from [examples/minimal](examples/minimal/) (the smallest complete project) or
[examples/stateless](examples/stateless/) (no database and nothing built: one pulled image
behind the front, the endpoint of an extension; installed end to end on Docker, its chart linted) or
[examples/full](examples/full/) (every spec key in use, and a working stack on Docker,
Kubernetes and a Kairos VM: stand-in services that migrate a real Postgres through two roles, a provision hook, an
optional search extension it takes the URL of; its chart runs them with the database embedded or external, the
external one's URLs given as secret inputs, the hook's in-cluster URL given by its ConfigMap). All three are lint-clean,
and they run end to end on Docker (`make e2e-docker`, `make e2e-stateless`, `make e2e-full`), Kubernetes (`make
e2e-k8s`, `make e2e-full-k8s`) and a Kairos VM on TrueNAS (`make e2e-vm`, `make e2e-full-vm`).

**The loop**: write or edit `stowage.yaml` → `stowage lint --json` → fix every `error` finding →
`stowage plan` to see what a ship would build. Lint errors block `ship` and `promote`; warnings
do not.

```bash
stowage -C <project> lint [--json]                     # exit 1 on any error finding
stowage -C <project> project-version [--channel C] [--revision N] [--json]
stowage -C <project> changelog [--from V] [--to V] [--json]
stowage -C <project> plan [--revision N] [--channel C] [--json]   # build graph + inputs digests
stowage -C <project> lock [--update]                  # pin this stowage in stowage.lock
stowage -C <project> ship [--channel C] [--revision N] [--disks] [--json]   # build what changed, push, move the pointer
stowage -C <project> verify [<version>] [--channel C] [--revision N] [--json]
stowage -C <project> promote <version> --from C --to C [--revision N] [--json]
stowage -C <project> yank <version> --reason TEXT [--channel C] [--revision N]
stowage -C <project> prune --channel C [<version>]    # mutable channels only
stowage -C <project> releases [--channel C] [--json]
stowage -C <project> hook install [--channel C] [--disks]       # write git's post-commit hook for this project
stowage -C <project> hook post-commit [--channel C] [--disks]   # what that hook runs: ship a version bump in the background
stowage --version
```

- For `plan`, `ship`, `verify` and `yank`, `--channel` defaults to the channel with
  `latest: true`, else the first by name. `--revision` defaults to 1 everywhere.
- `project-version` prints the current version, and with `--channel` also its tag. `releases`
  without `--channel` lists every channel.
- `changelog` without `--from` prints the one section of `--to`. With `--from`, it prints every
  section after `--from` up to `--to`. `--to` defaults to the current version.
- `plan` lists every node of the build graph with its inputs digest, including the OS bundle and
  disks nodes of the `kairos` shape. It warns when this `stowage` carries no installer, when `stowage.lock` pins another version
  (or is missing, a lint error), or when the pull account's password variable is unset (`ship` would refuse).
- `lock` writes `stowage.lock` next to the spec, pinning this `stowage`'s version (see the
  contracts below). When the file already pins this version it changes nothing. When it pins
  another, `lock` refuses unless given `--update`, which moves the pin (saying so when it moves
  back to an older `stowage`); the file is rewritten, so comments
  in it are not kept. A build whose version is not a SemVer release (such as a plain
  `go build`'s `dev`) cannot pin, and a lock that does not decode is never overwritten.
- `verify` without `<version>` checks the channel pointer's release. On the `latest` channel it
  also checks the installer's `:latest` and, with the `kairos` shape, each `latest-<format>` disk
  tag (absent for a format the release has none of). With a version, it checks that release.
- `prune` without a version deletes every release of the channel except the pointer's. With a
  version, it deletes every revision of that version, and the pointer moves back when it held
  one. An immutable channel is refused: yank instead.
- `releases` lists the published releases, oldest first in each channel. It checks no signature.
  A release whose version the current scheme refuses is skipped with a warning.

**Ship on commit** (`hook`): a commit that moves the project's version publishes that release.

- `hook install` writes git's post-commit hook (where git looks: `.git/hooks/post-commit`, or
  `core.hooksPath` when set) as a script running `<this stowage's absolute path> -C <project,
  relative to the working tree's top level> [-f <spec>] hook post-commit [--channel C] [--disks]`
  (`--disks`: every release it ships publishes its disks). It
  replaces a hook only it wrote, and refuses another (printing the line to add to it). Run it
  again after moving the `stowage` binary or the project. A spec (`-f`) outside the repository, or
  not committed, is refused, here and by `hook post-commit`: the ship runs in a checkout of the
  commit.
- `hook post-commit` reads the version recorded by `HEAD` and by `HEAD~1` (from the commits, not
  the working tree). It starts a ship only when `HEAD`'s version is later than `HEAD~1`'s by the
  version scheme, or `HEAD~1` records none (the first commit, a file added, a version the scheme
  refuses). It prints why it does not otherwise:
  - a rebase, merge, cherry-pick or revert in progress (`rebase-merge`, `rebase-apply`,
    `sequencer`, `CHERRY_PICK_HEAD` or `MERGE_HEAD` in the git directory);
  - `HEAD` is a merge commit or a cherry-pick (as the reflog records it);
  - `HEAD` amends a commit already at its version (that commit's ship was started; run
    `stowage ship` to publish the amended one); an amend that changes the version is a release;
  - no version bump, or a version that moved back;
  - `HEAD` records no version;
  - a `git-tag` or `command` version source: its version does not move with a commit (a tag comes
    after it), so such a project publishes with `stowage ship`.

  A version `HEAD` records that the scheme refuses is reported as an error. Every outcome exits
  0: a commit never fails on its release.
- The ship publishes the disks (as `ship --disks`) when the hook was written with `--disks`, or
  when the commit was made with `STOWAGE_SHIP_DISKS=1` (or `true`) in its environment — so a
  project chooses its own cadence, a release at a time; the started line then says `, with its
  disks,`.
- The ship runs in the background, in a session of its own with no input, and `hook post-commit`
  returns at once, printing `follow: tail -f <log>`. The background run is `stowage -C <project>
  hook post-commit --channel C --ship <version> --commit <id> [--disks]` (`--ship` and `--commit`
  together), which can also be run by hand:
  - it takes `<git common dir>/stowage/ship.lock`, so one hook ship runs at a time per
    repository; a later one waits for it (its log says so);
  - it checks `<id>` out, detached, as a linked worktree at
    `<git common dir>/stowage/tree/<full id>` (removed when it ends, and replaced when a killed
    run left it, even locked; no other worktree is touched), and runs `stowage ship` there
    for the channel (`--channel`, by default the one with `latest: true`, else the first) at
    revision 1: the release is that commit, whatever the working tree holds or wherever `HEAD`
    moved meanwhile, and a file the commit does not record (untracked or ignored) is not in it;
    the repository's `post-checkout` hook runs for the checkout as for any, and its failure
    fails the ship; it refuses an `<id>` that is not a commit, a commit not at `<version>`, and
    a project holding submodules (their content would not be in the checkout; publish such a
    project with `stowage ship`);
  - a relative `signing.key` is read from the project's own directory (an untracked key file is
    not in the checkout);
  - git runs, here and in the detection, without git's per-hook variables (`GIT_DIR`,
    `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_PREFIX`), and the ship has the commit's environment
    otherwise (the registry credentials, the signing key).

  Each release's log is `<git common dir>/stowage/log/<name>-<tag>.log`, appended to by every run;
  nothing is written in the working tree. The hook needs a unix host.

**JSON output** (`--json`):

| Command | Output |
|---|---|
| `lint` | `{"findings": [{"severity", "where", "message"}], "version"}`, errors first |
| `plan` | `{"project_version", "revision", "channel", "nodes": [{"id", "kind", "deps", "inputs", "pinned", "reads_version"}]}` |
| `project-version` | `{"name", "project_version", "revision"}`, plus `"channel"` and `"tag"` with `--channel` |
| `changelog` | `[{"version", "text"}]` |
| `ship`, `verify`, `promote` | `{"digest", "manifest"}` |
| `releases` | `[{"digest", "manifest"}]` |

The release manifest (artifact type `application/vnd.stowage.release.v1+json`) holds:

- `schemaVersion` (2; a manifest of another schema is refused), `name`, `short_name` (the installer is `<short_name>-stow`), `project_version`, `revision`, `channel`, `tag`, `date`;
- `release_seq`: the channel's publication number, never reused;
- `inputs`: the release's inputs digest;
- `scheme`: the provider and its config, so a consumer orders releases without the spec;
- `stowage_version`: the `stowage` that published it, the version `stowage.lock` pins;
- `artifacts`:
  - `images`: each image's digest, keyed by the spec's image key, plus `front` (the front
    Stowage builds);
  - `configs` and `manifests`: each image's linux/amd64 config and manifest digests;
  - `installer`: the installer image's digest;
  - `chart` (a project with the `kubernetes` shape): `{repository, version, digest}`, the chart's
    repository under the prefix (`charts/<name>-<channel>`), its version (`0.<release_seq>.0`)
    and its manifest digest;
  - `os` and `disks` (a project with the `kairos` shape): the OS bundle's digest, and each disk
    image's digest by format (a format this `stowage` does not know is kept, not refused, when its
    name is 1 to 16 lowercase letters, digits, `.`, `_` or `-`);
- `changelog.project`: the version's changelog section;
- `yanked`: `{reason, date}`, or `null`.

**Registry layout**, under `registry.prefix`:

| Repository | Tags |
|---|---|
| `<name>-<key>` | each image (built, or mirrored from a pinned `ref`) by the release tag |
| `<name>-front` | the front, built by Stowage, by the release tag |
| `<name>-backup` | the backup runner, built by Stowage (a `docker` or `kairos` shape with a `pg-age-rclone` backup), by the release tag |
| `<name>-installer` | the release tag, and `:latest` |
| `<name>-os` | the Kairos OS bundle (an image), by the release tag |
| `<name>-disk` | each disk image by `<tag>-<format>` (one disk set per channel), and `latest-<format>` |
| `charts/<name>-<channel>` | the channel's Helm charts (OCI), each by its version `0.<release_seq>.0`. The release manifests are the channel's offer: a yank deletes nothing, so the repository's highest version may be a yanked release's — install the version a release names (`--version`), never the latest |
| `<name>-release` | the release tag; `:<channel>` (the channel pointer) |
| `<name>-release-seq` | `:<channel>`: the last sequence number claimed, written before the release so a number is never reused, even after a prune or a failure |

On the publisher's machine:

- builds use `~/.cache/stowage/build`;
- each build loads a temporary local tag `stowage-build/<name>-<key>:<id>-<random>` (`<id>`: the
  start of its inputs digest; the front's key is `front`, the OS bundle's `os`), its own even when
  two ships build the same inputs at once, removed afterwards;
- the OS bundle's build context goes to `~/.cache/stowage/os` and the disks to
  `~/.cache/stowage/disks`, each removed after the ship; both are refused on a tmpfs or ramfs
  (`XDG_CACHE_HOME` moves them), and the disks need about 15 GB while they are made;
- the disks leave a local image `stowage-disktools:<16 hex>` (qemu-img, built from a Debian base
  pinned by digest), and pull the pinned auroraboot image;
- signing uses `~/.cache/stowage/sign`.

Publishing commands read `registry.push` (for `token`, the password from the named environment
variable); `verify` and `releases` read with `registry.pull` only. A `token` pull account is baked
into every installer: `ship` and `promote` read its password from the named variable in the
publisher's environment and refuse without it (the account is read-only, near-public by design;
its password is one of the installer's inputs, so rotating it is a new revision). The installer
`<short>-stow` is embedded in the `stowage` binary that `make build` makes (a plain `go build` has
none and cannot publish); it is one of the installer's inputs too. So `ship` and `promote` refuse
a `stowage` other than the version `stowage.lock` pins, or a project without the file: moving the
pin (`stowage lock --update`) is a reviewed change, and the release it publishes next carries the
new installer and front (a version already on an immutable channel needs a new `--revision`). A registry is reached over
HTTPS; plain HTTP is allowed to a loopback registry only.

With `registry.pull: { provider: none }`, every install reads the registry anonymously, so `ship`
and `promote` stop a release an install could not read. A registry may create every new repository
private (ghcr does, and no API makes a container package public). The run is checked three times;
each refusal names `registry.pull is none, so every install reads <prefix> anonymously, and
<repositories> refuse an anonymous read`, what makes them readable (made public once, in the
package's settings, or an account given to `registry.pull`), then `ship again`:
- before anything is built: each repository the release writes that already holds tags (read with
  the publisher's account) must list them anonymously;
- after the images are pushed by digest, before the OS bundle and its disks: each image must
  answer an anonymous read of its digest (a repository the push just created). The images were
  pushed untagged, so the next run builds them again (Docker's cache makes that quick);
- after the release is written, before a channel points at it: every artifact it records (images,
  bundle, disks, the channel's chart, the installer, the release manifest) must answer an anonymous
  read of its digest. The release is published, no channel points at it yet, and a run of the same
  inputs once they are public carries on and moves the channel.

Publishing builds with `docker buildx build --load` on the local engine, for `linux/amd64` (the
platform is part of an image's inputs). What `ship` does, in order:

1. every image is planned before anything is written: a tag already carrying the same inputs
   digest (annotation `dev.stowage.inputs`) is kept, an image with those inputs under another
   release's tag is re-tagged, the rest is built; a pinned upstream image is mirrored into
   `<prefix>/<name>-<key>` by digest; builds and mirrors are pushed by digest before any tag
   moves, so a failed build leaves every tag as it was. The front is one of these images
   (`image/front` in `plan`): Caddy 2.11.7 with the Cloudflare DNS plugin, built from a
   Dockerfile embedded in `stowage` (bases pinned by digest; it needs network access to download
   Caddy's Go modules), whose bytes and the platform are its only inputs, so it is built once
   and re-tagged by every later release until a `stowage` with another recipe publishes. It is
   signed, verified, promoted, yanked and pruned with the images;
2. on an immutable channel, a tag holding other inputs is refused before any write — publish a
   new `--revision`;
3. with the `kubernetes` shape, the chart is packaged (see the chart contract below) at version
   `0.<release_seq>.0` with `appVersion` the project version, and pushed to
   `<prefix>/charts/<name>-<channel>` by digest and signed; its version tag moves once the
   release manifest naming it is written (step 6), so a publication that fails leaves no chart
   version. It has no timestamp, so the same release makes the same digest (a re-ship rewrites
   nothing, and puts back a lost tag); on an immutable channel, a published release whose chart
   would now differ (or that has none) is refused before any write — publish a new `--revision`.
   A mutable release rebuilt with new content gets a new number, so a new chart version. On a
   mutable channel, after a ship that completes and after every `prune`, each chart version tag
   (`0.<N>.0`) that neither a release manifest of the channel nor its pointer names is deleted —
   a replaced build's, or one a stopped run left. Other tags of the repository (a signature's, one
   added by hand) are left. When a manifest of the release repository cannot be read (another
   artifact, a newer format), nothing is deleted. A failure is a warning; the next ship or prune
   retries. An immutable channel's chart repository is never swept;
4. with the `kairos` shape, the OS bundle (and, with `--disks`, its disks — see the Kairos contract
   below and "Disk sets" after this list) are kept, re-tagged or built after the images are in
   the registry and before any tag moves: built ones are pushed by digest and signed, and the
   tags `<prefix>/<name>-os:<tag>` and `<prefix>/<name>-disk:<tag>-<format>` move with the
   images'. A failure leaves every tag as it was (a bundle built before a disk failed is built
   again by the next ship). On an immutable channel, a published release whose bundle or a disk
   would now differ is refused — publish a new `--revision`;
5. the installer image `<prefix>/<name>-installer:<tag>` is made: one linux/amd64 file,
   `<short>-stow`, with the release appended (the manifest less its own installer digest and yank,
   the compose file, the runtime spec, the trusted keys, the pull account); its default command
   prints the bootstrap script. It has no timestamp, so the same release makes the same digest
   (a re-ship rewrites nothing, and puts back a lost tag); it is pushed by digest, signed, then
   tagged;
6. the release manifest, naming the installer, the chart, the OS bundle and the disks, is pushed to
   `<prefix>/<name>-release:<tag>`, verified, and only then the channel pointer
   `<prefix>/<name>-release:<channel>` moves — never backwards to an earlier release, never to
   a yanked one. On the channel with `latest: true` the installer's `:latest` follows the pointer
   (a yank or prune moves it back, or removes it with the last release) and each disk's
   `latest-<format>` follows the channel's disk set (a format the set has none of loses its tag);
   `verify` of that channel compares their digests with the pointer's and the set's;
7. a ship that published disks, once the pointer is on its release, records them as the channel's
   disk set (`<prefix>/<name>-disk:set-<channel>`, an artifact of type
   `application/vnd.stowage.disk-set.v1+json`: `{channel, release, stowage_version, disks}`,
   signed), moves each `latest-<format>` to them (on the channel moving latest), then removes,
   with their signatures, the disks the channel's releases name (their own, or the set they
   boot) that no channel's set holds: the registry then holds one disk set per channel. Only
   disks a release manifest of the channel names are removed, so a ship running on another
   channel never loses the disks it is pushing; a disk no release names (a failed ship's) is left.
   A release shipped while its channel points at a later one publishes its disks and leaves the
   channel's set as it is. A failure here comes after the release is out: `ship`
   prints the release, then `stowage: <tag> is published; the cleanup of the older disk sets
   failed: …`, and exits `3`, so the release is not shipped again (the next ship with `--disks`
   cleans up).

**Disk sets** (`--disks`). A disk set is several GB, while most releases only move the OS bundle
and the images, so `ship` publishes the kairos shape's disks, and registers `cloud_publish`'s
images, only when given `--disks` (or `STOWAGE_SHIP_DISKS=1` or `true` in its environment). A
release shipped without it publishes no disk, says so in one line, and records in its manifest
the disk set its installs boot (`boot_disks`): the channel's, the disks of the newest release (by
release number) that published disks, as its record holds it; `vm create` of it boots that set, and the VM's first boot
takes the release's own OS bundle before it installs (see `vm create`). A ship, yank or prune
that publishes no disk never moves, rewrites or deletes a disk tag. A release shipped again with
the same inputs keeps the disks it published while they are there, and records them as they
were once a newer set replaced them (`--disks` makes them again); `verify` checks a release's
disks while they are its channel's set (and the ship that publishes them). A promotion publishes
the disks the release it promotes published, while they are there. A yank or a prune never
moves a channel's set: a yank leaves the channel booting the yanked release's disks until a ship
publishes disks. `prune` deletes the disks a pruned release boots by digest, but those a release
left names (kept whole) and a channel's set — of another channel's it deletes the release's tag
alone, where the registry deletes a tag alone. Removing a tag never
relies on a registry deleting a tag: one that refuses has the manifest deleted by digest when no
other tag holds it, through `registry.delete` (`oci`: the distribution API, the default;
`github`: GitHub's packages API, for ghcr, whose distribution API deletes nothing — the token
needs `delete:packages`).

**Signing** (`signing: { provider: cosign-key, key: …, public_keys: [...] }`): every image, chart,
OS bundle, disk image, installer and release manifest is signed by digest before its tag moves, with the `cosign` CLI (v3, on
`PATH`, or `STOWAGE_COSIGN`); a signature already made by a listed public key is not made again.
`key` is a file (relative to the project root), `env://VAR` or a KMS/Vault URI; an encrypted key
file reads its password from `COSIGN_PASSWORD`. Signatures are OCI referrers (sigstore bundles),
read from the registry's referrers API or, on a registry without one (the GitHub Container
Registry), its tag fallback, where a referrer listed under its config's type or none is read
and judged by its own manifest; there is no transparency log: the trust anchor is `public_keys`, PEM files in the project that every
install carries (list old + new while rotating). `verify` — and every fallback of the channel
pointer — requires a signature by one of them, checked in-process (the bundle's signature by the
key, over the artifact's own digest; cosign is needed only to sign). A signature that cannot be
read is an error, never "unsigned". cosign gets the registry credentials in a private
Docker config for one call, never on its command line; signing is refused for a registry that
cosign could reach over plain HTTP (a private IP or `*.localhost`, loopback excepted): name it by
DNS. A manifest read (a release manifest, a signature, an artifact) whose content does not hash to
the digest the registry gives is refused.

**Installs verify** the release manifest of every release they read from a registry — `upgrade
--version` on Docker and on Kubernetes, the VM's `updates` and `upgrade` — before anything of it
is fetched: it must carry a signature by a key the install's own release trusts (Docker: the
kept `<dir>/<short>-stow`'s, or the `<short>-stow` run's only when the kept one carries none; the VM:
its OS's `<short>-stow`'s; Kubernetes: the `<short>-stow` run's — not the release's
`stowage.publicKeys` value, which `--values` can override, but narrowed to the keys that value
lists when it lists any: a key a later release dropped is not trusted again, and with none left
the run is refused), or it is refused: `<name> V (revision N, <tag>) is not signed by a
key this install trusts: refused`. Its manifest pins every digest the upgrade pulls (installer,
images, chart, OS bundle), each by digest. An install whose release carries no key (its project
does not sign) checks nothing; one that carries keys never accepts an unsigned release. The
first install trusts the installer the operator runs; check it before running it with
`cosign verify --key <public key> --insecure-ignore-tlog=true <prefix>/<name>-installer:<tag>`.
Among a version's revisions, `upgrade --version` takes the latest one signed by a trusted key: one
pushed without the key is passed over, its yank not believed; one whose signatures cannot be read
stops it (an older revision never stands in for it). Each revision passed over is logged
(`passed over: …`). The VM's `updates` / `upgrade` without a version refuse a channel pointer on a
yanked release, or on a publication its tag no longer holds (`the channel C's pointer names T,
which its tag no longer holds`). A signature only counts as one made
by `cosign sign` (an attestation the key made over the same digest does not), checked with the
hash cosign uses for the key type.

Run one publisher at a time per project (in CI, one concurrency group): a registry offers no
compare-and-swap.

`promote` re-tags a published release onto another channel without building (the OS bundle and
its disks name no channel, so they are re-tagged too); the tree must be
exactly what was published (same release inputs: images, compose, spec, changelog,
`stowage.lock`, and the same `stowage` and pull account password, which make the installer
and the front),
checked before anything is written. Re-shipping a yanked release fails: publish a new revision (the channel never moves forward to it; a pointer an interrupted yank left on it moves back). Image keys `release`,
`release-seq`, `os`, `disk`, `installer`, `front` and `backup` are reserved for Stowage's own repositories. `yank` marks a release (nothing is deleted) and moves the pointer back to the latest
release that is not yanked and verifies, removing it when none is left; a broken release can
always be yanked. `prune` deletes tags (the release's images, installer, chart, OS bundle and
disk tags first, its manifest last; disks as "Disk sets" says): a registry that deletes no tag
alone has a manifest deleted by digest through `registry.delete` when no other tag holds it, and
refuses, naming the tags, otherwise.

Global flags: `-C <dir>` project root (default `.`), `-f <file>` spec file (default
`<dir>/stowage.yaml`). Exit codes: `0` success, `1` failure or lint errors, `3` a release `ship`
or `promote` published whose disk cleanup failed. With `--json`, stdout
is one JSON document and nothing else; diagnostics go to stderr.

### The contracts a project must honour

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
    — TCP or UDP, beside the front — is an `l4` route (see "Spec keys");
  - a service on the host's network (`network_mode: host`, its own or the one it extends) is,
    likewise, an error with a `kairos` shape (a port on the host's network is none Stowage
    publishes) and a warning on the `docker` shape (`listens on the host past Stowage`);
  - a service with `profiles:` runs only while an input turns one of its profiles on (an input's
    `profile`, see "Spec keys"); a profile no input names is an error (no install would ever
    start the service);
  - a variable without a default (`${X}`) must be an input, a secret, a safe default or a route's
    `domain_input`; a variable with a default (`${X:-v}`) is an optional tunable;
  - each `safe_defaults` key appears as `${KEY:-<safe value>}`: the safe value is the default and
    only the installer relaxes it;
  - `STOWAGE_` is reserved. Stowage provides `STOWAGE_IMAGE_<KEY>`, `STOWAGE_URL_<ROUTE>` (the
    route's public URL — use it rather than building one from a domain), `STOWAGE_NAME`,
    `STOWAGE_PROJECT_VERSION`, `STOWAGE_REVISION`, `STOWAGE_TLS_MODE`, `STOWAGE_LOOPBACK_ADDRESS`
    (a loopback address in every mode, for a port only this host may reach: 127.x or `::1`,
    bare — Compose's port syntax takes `::1:5432:5432`), `STOWAGE_IPV6` (`true` with `--ipv6`, else `false`; Docker and the VM),
    `STOWAGE_L4_<ROUTE>` (an `l4` route's endpoint, `<address>:<port>`, for a route with an
    `address_input`; Docker and the VM);
  - an unknown `STOWAGE_…` variable, a `STOWAGE_IMAGE_<KEY>` naming no image, a
    `STOWAGE_URL_<ROUTE>` naming no route or a `STOWAGE_L4_<ROUTE>` naming no `l4` route with an
    `address_input` is an error, and so is a compose file with no service;
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
        - { name, protocol: TCP|UDP, port, service, targetPort }   # service: the upstream's compose service
    ```
    Stowage creates nothing in the cluster for `l4`: the chart publishes each route as it
    chooses (a `NodePort` or `LoadBalancer` Service to the pods of `service`, as
    [examples/full](examples/full/deploy/chart/templates/echo.yaml) does);
  - the templates run the release's images by digest:
    `{{ printf "%s/%s@%s" .Values.stowage.registry $img.repository $img.digest }}` with
    `$img := index .Values.stowage.images "<key>"`. `lint` warns when no template reads
    `.Values.stowage.images`;
  - `<short>-stow k8s` adds two values at install time (absent from the published chart, so guard
    them with `{{ with }}`, or `stowage.secret` with `{{ required }}`): `stowage.secret`, the name of the Secret holding
    the release's secrets and Kubernetes inputs as `KEY: value` (give each container only the
    keys it reads, with `env: [{name: K, valueFrom: {secretKeyRef: {name: …, key: K}}}]`;
    `envFrom` would give every container every secret), and `stowage.pullSecret`, the name of the `kubernetes.io/dockerconfigjson` Secret
    holding the pull account, empty when there is none (give it to `imagePullSecrets`). It also
    sets `stowage.registry` to the prefix the release is pulled from. `lint` warns when no
    template reads `.Values.stowage.secret` while the project has a secret or an input on
    Kubernetes, and when no template reads `.Values.stowage.pullSecret` while its pull account is
    a `token`;
  - with an `endpoint` (see "Extensions"), the chart exposes that route's `upstream:
    <service>:<port>` as the Service `<release>-<service>` on that port: `<short>-stow k8s` prints
    `http://<release>-<service>.<namespace>.svc:<port>` as the project's endpoint.
- **Kairos OS bundle** (`shapes.kairos`): `ship` builds `<prefix>/<name>-os` with `docker buildx`
  from a Dockerfile embedded in `stowage`: `base` converted by `kairos_init` (`-s install`, then
  `-s init`) into a Kairos image with no Kubernetes provider, carrying the release's version
  (without its channel) in `/etc/<name>/os-version` and as its Kairos version (given to the init
  stage only: the slow layers before it — the base's packages, `tzdata` and, where the base has it, `tzdata-legacy` among them, kairos-init's
  install stage and Docker's packages — are the same from one release to the next, so
  BuildKit's cache serves them). On top of it:
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
    `<name>-hostd.service` (the host daemon), `<name>-network.service` (the network override) and
    `<name>-backup.timer` (not enabled: the host daemon starts it) with `<name>-backup.service` (the
    scheduled backup); see "The installer
    on the Kairos VM" below;
  - Kairos's `/system/oem/08_grub.yaml` (its GRUB boot assessment) without the step that marks
    every boot of the active system good as it starts: the health gate marks it. The build fails
    when kairos-init's file is not the one this copy is derived from (by its sha256);
  - a default-deny firewall, `/etc/nftables.conf`, with SSH open to every source
    (`/etc/nftables.d/ssh.nft`), `/run/nftables.d` made at boot for the install's ports, and a
    forward chain enforcing the L4 routes' allowlists (see "The firewall" under the installer on
    the Kairos VM);
  - SSH by key only (`/etc/ssh/sshd_config.d/10-<name>.conf`), an `admin` group (created when the
    base lacks it), `sudo` without a password for it (`/etc/sudoers.d/10-<name>`: its members log in with an SSH key and have none), a console banner (`/etc/issue.d/05-<name>.issue`),
    consoles on the screen and the serial port (serial last, so it owns `/dev/console`), the QEMU
    and VMware guest agents, `multipathd` masked; the kernel modules kairos-init strips are
    reinstalled (the build fails with fewer than 100).

  Its inputs are the version and revision, the name, `base`, `kairos_init`, the embedded recipe,
  the images' inputs, and what its `<short>-stow` carries — the runtime spec less `disk_formats` and
  `cloud_publish`, `stowage.lock`, the changelog section, the version scheme, the compose file,
  the trusted keys, the installer executable and the pull account; it is kept on them and on the digests of the images it preloads, so it
  names no channel and a promotion re-tags it; every new version or revision rebuilds it, with
  the distribution's and Docker's packages of the day. The build needs network access (apt, kairos-init's
  packages) and takes several minutes; the bundle is about 3 GB.
- **Disk images** (`shapes.kairos.disk_formats`): made from the bundle by the pinned auroraboot
  (v0.23.0, run privileged on the host network). auroraboot holds no registry account: it reads the
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
  Docker Desktop's VM). The formats: `raw` (EFI) by
  auroraboot; then, with qemu-img and tar from the raw disk, `vhd` (a fixed VHD, its virtual size
  the raw disk's grown to a whole number of MiB, as Azure imports it), `gce` (the raw disk grown
  to a whole number of GiB, as `disk.raw` alone in a sparse, oldgnu `.tar.gz`, as Google Cloud
  imports it), `qcow2`
  (compressed) and `ova` (an OVF descriptor — EFI firmware, Secure Boot off, 2 vCPUs, 4 GiB, a
  serial port — then a streamOptimized VMDK of 64 GiB, or the raw disk's size when larger, in a
  tar). A grown disk (`vhd`, `gce`, `ova`) has its backup GPT moved to its new end (sgdisk). `ship`
  refuses a `vhd` whose footer is not a fixed disk's, with a valid checksum, of a whole number of
  MiB, and a `gce` that does not hold a `disk.raw` of a whole number of GiB alone.
- **Cloud images** (`shapes.kairos.cloud_publish`): each cloud listed adds the format it imports
  to the disks `ship` makes, whether `disk_formats` lists it or not — `aws`: `raw` (for `aws ec2
  import-snapshot` or `import-image`), `azure`: `vhd` (a page blob, then a managed disk), `gcp`:
  `gce` (for `gcloud compute images create --source-uri`). They are published as the other disks,
  and `ship` registers each in the publisher's cloud account it has settings for, from the
  environment (never from a file or a release):

  | Cloud | Settings (`ship` registers its image when one of its `STOWAGE_` settings without a default is set; every setting without a default is then required, or `ship` stops naming those missing) | Credentials (read by the cloud's OpenTofu provider) | Registered as |
  |---|---|---|---|
  | `aws` | `STOWAGE_AWS_BUCKET` (the S3 bucket the disk is uploaded to), `AWS_REGION`, `STOWAGE_AWS_VMIMPORT_ROLE` (default `vmimport`: the VM Import service role, which reads the bucket) | `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, `AWS_PROFILE`, or the machine's role | the disk uploaded as `stowage/<image>.raw`, imported as an EBS snapshot (import-snapshot), registered as a UEFI x86_64 AMI |
  | `azure` | `STOWAGE_AZURE_RESOURCE_GROUP`, `STOWAGE_AZURE_STORAGE_ACCOUNT`, `STOWAGE_AZURE_CONTAINER` (existing), `STOWAGE_AZURE_LOCATION` | `ARM_CLIENT_ID`, `ARM_CLIENT_SECRET`, `ARM_TENANT_ID`, `ARM_SUBSCRIPTION_ID`, or the `az` login; the blob is written with that identity (Entra ID: it needs *Storage Blob Data Contributor* on the storage account), never with the account's keys | the disk uploaded as the page blob `stowage-<image>.vhd`, made a generation 2 Linux image in the resource group |
  | `gcp` | `STOWAGE_GCP_PROJECT`, `STOWAGE_GCP_BUCKET` (the Cloud Storage bucket the disk is uploaded to) | `GOOGLE_APPLICATION_CREDENTIALS`, or the gcloud application-default login | the disk uploaded as `stowage/<image>.tar.gz`, made a UEFI-compatible Compute Engine image |

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
  (`hashicorp/aws` 6.69.1, `hashicorp/azurerm` 5.10.0, `hashicorp/google` 8.7.0), run by the `tofu`
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
  on Azure — names this disk's digest; one another machine registered, or whose directory was
  removed, is taken as it is; one of that name not ready — failed, or still being made — or another
  disk's stops `ship`, naming it, to be waited for or deleted). The account is trusted: an image of
  that name and description made there by anyone is taken. The kept id is trusted too: to
  register an image again, run `tofu destroy` in its working directory (it removes the image and,
  on AWS, its snapshot), then remove the directory. A later `ship`, `promote` or re-run prints
  `cloud <cloud>: image <id>` and uploads nothing; a run stopped part way goes on from its states.
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
  adds (`stowage-provision`), on the project's `default` network, with no capability,
  `no-new-privileges` and no host file; on Kubernetes as a Job (see the installer on Kubernetes).
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
- **Front**: every route has a `loopback_ports` entry (distinct ports); `loopback_env` relaxes a
  `safe_defaults` key in loopback mode only (plain HTTP on the operator's machine).
- **Images**: keys are lowercase words joined by single `-`. Built (`context` + `dockerfile`,
  optional `target` naming a stage, `args`, `inputs`) or pinned upstream
  (`ref: name:tag@sha256:<64 hex>`). Every `FROM` of a built image is pinned by digest (or names an
  earlier stage, or `scratch`). An image whose Dockerfile declares `ARG STOWAGE_PROJECT_VERSION`
  receives the project version; any other image is re-tagged, not rebuilt, on a version bump.
- **Paths** in the spec are relative to the project root and stay inside it (the one exception is
  `docker-config`'s `config`: a Docker config directory, the one holding `config.json`, on the
  publisher's machine).
- **Build contexts** that contain `.git` exclude it in their `.dockerignore` (`**/.git` also covers
  submodules).
- **The front** routes by hostname only: each route maps a `domain_input` to `<service>:<port>`;
  `front.portal` names the route whose URL the installer prints at the end.
- **Signing** (`cosign-key`): `key` and `public_keys` are required; each public key is a project
  file holding a PEM `PUBLIC KEY` block (as `cosign generate-key-pair` writes it).
- **Providers**: every project-specific concern is `{ provider: <name>, …config }`
  ([DESIGN § 2.1](docs/DESIGN.md#21-one-pattern-for-everything-project-specific)). Version source,
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
  every service of the stack running (healthy, when it has a healthcheck) or exited 0. `http-status`
  and `http-json` send `GET path` to the service's container at its address on the stack's
  network (the first network by name that gives one), within 10 s: ready is the status `expect`
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
  `<short>-stow`, the host daemon and the recipes of the front and the backup runner are all part of
  that `stowage`, and `ship` builds the front and the runner into the project's own registry
  (there is no Stowage registry to pin pieces from). Without it lint reports
  an error, and `ship` and `promote` refuse; they also refuse a `stowage` of another version than
  `stowage:`, before reading the registry, naming the pinned `stowage` to use (`ship` with a newer
  `stowage` also offers `stowage lock --update`).

### Spec keys

Keys of `stowage.yaml`, decoded strictly: an unknown key is an error. [examples/full](examples/full/stowage.yaml) uses every one.

**A project without a database** ([examples/stateless](examples/stateless/stowage.yaml)) leaves
`database` out. `health`, `front` and the rest stay as they are; what only a database needs is
then refused where it is declared: a `backup` (`a backup dumps the database, and the project
declares none`) and a non-empty `data.reset` (`reset-db resets a database, and the project
declares none`); `data.volumes` alone stays the project's own. Its installs skip the database
steps: an upgrade dumps nothing and records no schema version, a rollback (and the VM's fallback
at a failed health gate) goes back unchecked by any schema, and `reset-db` (Docker, the VM, `k8s
reset-db`) and the VM's `restore` refuse, before anything changes, with `<name> <version> has no
database: reset-db has nothing to reset` (`… there is no backup to restore`). On Kubernetes
`postgres_modes` is left out; the chart is given no database value either way.

| Key | What it takes |
|---|---|
| `apiVersion` | `stowage/v1` (another value: upgrade Stowage) |
| `name` | 2–40 lowercase letters, digits or `-`, starting with a letter; it prefixes every repository |
| `short_name` | optional, the same form as `name` (absent is `name`): the installer is `<short_name>-stow` — the binary that installs and runs the infrastructure's lifecycle. An install keeps it under that name and records it in its `.env` (`STOWAGE_SHORT_NAME`, not a value compose or a hook reads), so `short_name` is fixed for a project's life: `ship` refuses a release whose `short_name` differs from one its channels already hold, and an install refuses one before anything changes (a re-run, `upgrade`, `rollback`, the VM's boot) |
| `stowage.requires` | the accepted Stowage range; accepted but neither validated nor enforced yet (lint warns when it is missing; `stowage.lock` is what pins) |
| `version.source` | `changelog` (default; `file`, default `CHANGELOG.md`, the first released heading), `file` (`file`, default `VERSION`, the first non-empty line), `git-tag` (`match`, a `git describe --match` pattern; `strip`, a prefix removed), `command` |
| `version.scheme` | `calver` (default; `format`, default `YYYY.M.MICRO`, tokens `YYYY YY 0Y MM M 0M WW 0W DD D 0D MAJOR MINOR MICRO` joined by `.`; `month_guard: utc\|local`, which needs a year and a month in the format and only warns), `semver` (no config), `regex` (`pattern`, anchored, with named groups; `order`, a list of `group:int` or `group:str` compared in turn), `command` |
| `changelog` | `keepachangelog` (default; `file`, default `CHANGELOG.md`; `## [x]` or `## x` headings, `Unreleased` skipped), `dir-per-version` (`dir`, required; `pattern`, default `{v}.md`, holding `{v}` once and no `/`), `command`. The current version must have a section |
| `channels.<name>` | `tag` (template holding `{v}` once), `mutable`, `latest`. Names are lowercase letters, digits, `.`, `_`, `-`. At most one channel sets `latest` (none, or a mutable one, is a warning) |
| `registry.prefix` | `<host>[:port][/path]`, the host a name or a bracketed IPv6 address (`[fd00::5]:5000/notes`): no scheme, tag or trailing slash |
| `registry.delete` | how the publisher deletes a manifest by digest (an older disk set, a pruned release's): `oci` (the default, the distribution API) or `github` (GitHub's packages API: `api`, default `https://api.github.com`; `token_env`, default `registry.push`'s, a token with `delete:packages`) |
| `registry.pull`, `registry.push` | `token` (`user`, `token_env`), `docker-config` (`config`, a Docker config directory; empty: the default Docker login), `none` (pull only). `registry.pull` is baked into every installer: it may not use `registry.push`'s `token_env` or Docker config. With `none`, `ship` refuses a repository the release writes that an anonymous read is refused (see `ship`) |
| `signing` | `cosign-key` (`key`, `public_keys`), or absent / `none` |
| `images.<key>` | built (`context`, `dockerfile`, `target`, `args`, `inputs`: the paths whose content enters the image's inputs digest, default the whole context less `.dockerignore`) or `ref` (exclusive with the build keys). An image no service or hook uses is a warning; a `# syntax=` frontend or a `FROM $ARG` cannot be checked (warning) |
| `stack.compose` | the compose file (its contract: "The contracts a project must honour"): `extends` within the file, `profiles:` only those an input's `profile` names, a `ports:` entry past this host an error with a `kairos` shape and a warning on `docker` |
| `front` | `tls_modes` (`acme`, `dns-cloudflare`, `byo`, `internal`, `none`), `routes[]` (`name`, `domain_input`, `upstream: <service>:<port>`, `strip_headers`, where a trailing `*` matches a prefix; `forward_auth`: `address_input` (required), `uri_input`, `copy_headers` (header names); `allow_input`), `portal`, `loopback_ports`, `loopback_env`, `publish_ca` (`volume`: a volume the compose file declares; refused when the project offers neither `internal` nor `byo`), `http_port` and `https_port` (the host ports a TLS front publishes unless an install's `--http-port` / `--https-port` move them; absent or 0 is 80 and 443; they must differ). Route names must give distinct `STOWAGE_URL_` variables; each `domain_input` routes one upstream. An input `forward_auth` or `allow_input` names must be an input of the `docker` shape and, with a `kairos` shape, of that one too, and neither `required` (empty is how the front leaves it off) nor asked in one mode only (`when`); an `allow_input` input is not `advanced` (never asked, it would refuse everyone). Lint counts these inputs as read |
| `l4` | `routes[]`: host ports published to a service as they are, beside the front and never through it — `name` (1 to 15 lowercase letters, digits or `-`, starting with a letter, no `-` at its end or beside another, as a chart names a Service port after it; neither another L4 route's nor a front route's name), `protocol` (`tcp` or `udp`), `port` (the host port, 1–65535; one route per protocol and port), `upstream: <service>:<port>` (a compose service), `address_input` (optional: an input, not `secret`, holding the host name or address clients dial — a lowercase host name whose last label is not all digits, or an IP address; no port, scheme or zone. It is checked when given, and lint checks its `default`; the install records the route's endpoint `STOWAGE_L4_<ROUTE>` from it, and lint counts it as read), `allow_input` (optional: an input holding the source addresses and CIDRs, separated by commas or spaces, the VM's firewall lets reach the port — empty refuses every source; checked when given. Only the VM enforces it, so without a `kairos` shape it is a warning (the route is open to every source), and its input is asked on that shape alone (`shapes: [kairos]`: a Docker host's firewall is the operator's), is neither `secret` (the firewall's rules show it) nor `advanced` (never asked, it would refuse every source) nor asked in one mode only (`when`); lint counts it as read). With a `kubernetes` shape, a chart whose templates never read `.Values.stowage.l4` is warned. Over TCP a route's port may not be one the front publishes by default: `front.http_port` / `https_port` (80 and 443) in a TLS mode, a route's `loopback_ports` in loopback mode, and 80 and 443 while `acme` is offered (its challenges need them). The same port over UDP is free. See "L4 routes" under the installer |
| `health` | `http-json` (`service`, `port`, `path`, `liveness_path`, `expect`: the JSON fields a ready answer carries), `http-status` (the same, `expect` a 200–399 status, default 200), `command` (`image`, `cmd`, `timeout_s`). The VM's health gate and `<short>-stow health` run it; the Docker installer waits on the compose healthchecks (`up --wait`) |
| `database` | optional (see "A project without a database" above); given, `engine` (`postgres` only), `service`, `name`, `roles.migrate`, `roles.app` (all required; one role twice is allowed), `schema_compat` (`sql`, `command`, `none`; `sql`'s `baseline`, an integer ≥ 0 where absent or 0 means 1 (every other provider is at 1), is the schema epoch the release's migrations start from: see the upgrade notes on schema baselines) |
| `data` | optional: `volumes.<name>.{uid, keep_on_reset}`, `reset` (volumes not `keep_on_reset`; empty is a warning, and `reset-db` refuses it; without a `database`, an error when not empty): the volumes `reset-db` wipes on Docker and the VM (on Kubernetes it deletes every claim of the release). `uid` is validated only (on Docker, a new named volume takes the owner of the image's directory it mounts on, so an image that runs as a user creates that directory owned by it) |
| `secrets[]` | `key`, `gen`: `hex32`, `hex16` (random bytes, hex), `pass18`, `pass32` (alphanumeric), `b64-32` (base64url); `rotation: true` lets `secrets rotate` replace it (`KEY_NEXT` and `KEY_PREVIOUS` are then reserved names, recorded empty beside it, and the compose file must read both). Generated once at the first install, or at the upgrade that adds it |
| `safe_defaults` | `KEY: value`, the value the compose file defaults to |
| `inputs[]` | `key`, `prompt` (required), `default` (may name `${KEY}`), `required`, `optional` (exclusive with `required`; otherwise no effect), `secret` (no default), `when: tls\|loopback` (asked only in that mode), `shapes` (`docker`, `kubernetes`, `kairos`; asked only on those), `profile` (a compose profile name, `[a-zA-Z0-9][a-zA-Z0-9_.-]*`, that a service of the compose file carries: the input turns it on while it holds a value — see "Optional services" under the installer; not `required`, and asked on the `docker` or `kairos` shape; lint counts it as read), `advanced` (a tunable: never asked, recorded only when `--set`, `--answers` or the VM's settings give it — never from an exported variable — its default the compose file's — so no `required` or `default`, and the compose file must write it `${KEY:-default}`; a re-run giving it empty removes it). An input left empty is recorded empty. An input asked on the `docker` or `kairos` shape that no compose service, no route and no provision hook's `env` reads is a warning (one asked on `kubernetes` only is the chart's) |
| `extensions[]` | the apps an install of this project can be pointed at (see "Extensions"): `name` (an app name, as `name` is; not this project's; none twice), `input` (required: an input holding the extension's endpoint, empty while it is off — `optional`, not `secret`, no `default`, and read by a compose service, a route or the provision hook's `env`, or asked on `kubernetes` only, where the chart reads it), `prompt` (required: what the install's tip calls it; the `input` is neither `advanced` nor asked in one mode only, `when`, so every install records it), `key_input` (an `optional` `secret` input holding its credential, asked on the same shapes as `input`, neither `advanced` nor `when`, and read as `input` is). No input is named by two extensions, or twice by one |
| `endpoint` | a route of `front.routes`: this project's endpoint when another project uses it as an extension. Its installer prints the route's URL at the end of an install (see "Extensions") |
| `hooks.provision` | `container` (`image`, `cmd`, `env`: the inputs, secrets and safe defaults it receives, required, `[]` for none; `returns`: keys it may return, which `provision --set` sets before it returned them; `outputs`) |
| `hooks.maintenance.disk_pressure` | `soft_pct`, `hard_pct` (1 ≤ soft < hard ≤ 99), `post` (`http://<service>:<port>/…`), `auth_secret` (a `secrets` key). The Kairos VM's host daemon acts on it (see "Disk pressure" under the installer on the Kairos VM), and a Docker install's own timer (see "Backups, restore and the disk-pressure watch on Docker"); Kubernetes ignores it, and without a `docker` or `kairos` shape it is a warning |
| `backup` | optional, and only with a `database`: `pg-age-rclone` (`profiles`: a name of 1 to 32 lowercase letters and digits each, `all`, a list of the tables whose data it keeps — `table` in any schema, or `schema.table` —, or `{ exclude: [pattern, …] }`, the tables whose rows it leaves out, structure kept (`table` or `schema.table` of letters, digits and `_`, with pg_dump's `*` and `?`); `default_profile`, required with several; `volumes`, the compose volumes every backup also archives and a restore brings back with the database — each listed once, a volume the compose file declares and Compose creates (not `external`), under a literal name (no `${…}` in its `name:`), and none the database's service mounts: its data is the dump's), which the Kairos VM and a Docker install run (see "Backups" under the installer on the Kairos VM); `command`, validated only (with a `kairos` shape, a warning: the VM schedules no backup of it). A `docker` or `kairos` shape with a `pg-age-rclone` backup makes `ship` build and publish the backup runner (`<name>-backup`), which the OS bundle preloads and a Docker install pulls |
| `shapes` | at least one of `docker: {}`, `kubernetes` (`chart`, a directory holding `Chart.yaml`; `postgres_modes`: `external`, `embedded`, `cnpg`, optional and validated only; an error without a `database`; `schema`: `url_env`, the variable holding the migrate role's URL in the container of the workload running `reads_newer_image`, and `image`, an images key holding `psql` — the schema check of `k8s upgrade` and `k8s rollback`, which needs the `sql` schema-compat provider with `reads_newer_image`; absent with that provider, a warning), `kairos` (`base`, the distribution converted into Kairos, and `kairos_init`, the kairos-init image, both pinned by digest; `disk_formats`: `raw`, `qcow2`, `ova`, `vhd`, `gce`, none publishing the bundle alone; `cloud_publish`: `aws`, `azure`, `gcp`, each adding the disk format it imports (`raw`, `vhd`, `gce`); `health_gate`: 0 < `poll_s` < `deadline_s` ≤ `deadline_after_reset_s`, the VM's health gate's window and polling (default 600, 1800 and 30 s; `deadline_after_reset_s` is validated only: `reset-db` does not reboot, so no boot is judged after it); `host_api`: the compose services given the host daemon's app socket on the VM, each in the compose file, none twice; no `backup` is a warning when there is a `database`). `ship` publishes the `kubernetes` chart and the `kairos` OS bundle and disks; `docker` and `kubernetes` have a runtime |

A `command` provider answers per op: `version.get` → `{"version"}`; `scheme.validate`
`{version}` → `{"valid", "reason"}`; `scheme.compare` `{a, b}` → `{"result": -1|0|1}`;
`changelog.versions` → `{"versions": [...]}`; `changelog.section` `{version}` → `{"text"}` (exit 2:
no section). Its config is `cmd` (required) and `timeout_s`. It runs from the project root, and
its stdout is capped at 1 MiB.

### The installer (Docker shape)

What an operator runs: `docker run --rm <prefix>/<name>-installer:latest > <name>.sh && sh
<name>.sh` (or `… | sh -s -- <flags>`). The script is POSIX `sh` and needs `base64`. It carries the
`<short>-stow` binary, which carries the release (manifest, compose file, runtime spec, trusted keys,
pull account), so an install needs nothing else. The script unpacks `<short>-stow` into the first
private directory where a program may run: the current directory, then the user's cache, then
`$TMPDIR`. It runs it there and removes it afterwards. `<short>-stow script` prints that script
again, to copy an installer to another host.

```bash
<short>-stow [--dir D] [--registry PREFIX] [--registry-ca FILE] [--offline] [--dry-run] # install / finish setup
          [--set KEY=VALUE]… [--answers FILE] [--no-input] [--accessible] [--no-provision]
          [--ports route=port,…] [--address 127.x.x.x|::1]                 # loopback mode
          [--tls acme|dns-cloudflare|byo|internal] [--domains route=host,…] # TLS mode
          [--cert FILE --key FILE [--ca FILE]] [--http-port N] [--https-port N]
          [--ipv6[=false]]                                                 # IPv6 on the stack's network
          [--secrets FILE|-]                                               # a machine made again
<short>-stow status [--dir D]
<short>-stow secrets export --recipient AGE_KEY… [-o FILE|-] [--dir D]   # the secrets, encrypted
<short>-stow secrets rotate KEY [--stage | --finish] [--dir D]           # a secret with rotation: true
<short>-stow provision [--dir D] [--set KEY=VALUE]…                      # run the provision hook again; --set an input only it reads, or a value it returned or declares it returns
<short>-stow upgrade [--dir D] [--set KEY=VALUE]… [--answers FILE] [--no-input] # the NEW release's installer
          [--offline] [--dry-run] [--no-provision] [--accessible]
<short>-stow upgrade --version V [--dir D] [--set KEY=VALUE]… [--answers FILE] # fetches V's installer
          [--no-input] [--accessible] [--dry-run] [--no-provision]
<short>-stow rollback [--dir D] [--offline] [--dry-run]                  # the installer the install keeps
<short>-stow reset-db [--dir D] [--yes] [--no-input] [--accessible] [--dry-run] [--no-provision]
<short>-stow backup config|run|status|list|verify [--dir D] [the VM's flags]   # a pg-age-rclone backup
<short>-stow restore OBJECT [--dir D] [--identity FILE|-] [--yes] [--accessible]
<short>-stow script                                                      # print the bootstrap script
<short>-stow mirror export [--channel C] [--version V [--revision N]] [--shapes S,…] [--output FILE] [--disks F,…|none]
<short>-stow mirror import FILE --to PREFIX [--registry-ca FILE] [--older]
```

- Defaults: `--dir ./<name>`, `--address 127.0.0.1` (a 127.x address or `::1`, recorded bare
  in `STOWAGE_LOOPBACK_ADDRESS` — Compose's port syntax takes `::1:8080:80` — and bracketed in
  the routes' URLs, `http://[::1]:8080`; `localhost` is 127.0.0.1, and its URL `http://localhost:…`;
  any other address, a zone included, is refused), `--http-port` the project's `front.http_port`,
  else 80, `--https-port` its `front.https_port`, else 443. `--ports` and `--address` are for loopback
  mode only, and `--http-port` / `--https-port` for TLS mode only; the two ports must differ.
  `--cert` or `--key` alone picks `byo`.
- `--ipv6` turns IPv6 on for the stack's default network (`enable_ipv6` in the front's compose
  file; Docker ≥ 27.0 gives it a unique local /64 and forwards the published ports over IPv6 to
  the front itself), so the front sees each IPv6 client's own address — its source allowlists
  and `forward_auth` judge it. Off (the default), an IPv6 client still reaches a TLS front, through
  Docker's userland proxy, but the front sees the bridge's IPv4 gateway instead of it. Recorded as
  `STOWAGE_IPV6` (`true` or `false`, `false` when never given); a re-run without the flag keeps
  it, `--ipv6=false` turns it off. With `--ipv6` the default network carries the label
  `io.stowage.ipv6: "true"` (without it, the front's compose file says nothing of the network, so
  a project's own definition, an external one included, stands): a run whose choice differs from
  the label stops the stack first (`docker compose down --remove-orphans`, volumes kept; a Compose
  that does not make a changed network again would keep it), then starts it on a new one; IPv6
  the project's compose file or the daemon gives the network by itself never stops it. With `--ipv6` (not under
  `--dry-run`), the checks before anything is written make and remove a network of their own with IPv6
  (`stowage-ipv6-check-<random>`, labelled `io.stowage.probe=true`): an engine that cannot is
  refused, the install as it was. The VM takes it from
  `vm create --ipv6`.
- `--registry PREFIX` pulls the images from a mirror holding the same `<name>-<key>`
  repositories (and `<name>-release` and `<name>-installer`, which `upgrade --version` reads
  there). The project's pull account is never sent there: the pulls use the operator's own
  `docker login`.
- `--registry-ca FILE` is a PEM file of the certificate authority the registry's certificate
  chains to — a mirror's own CA (certificates only: a file holding a private key, or nothing
  else, is refused, before anything is written). The installer's own reads (`upgrade
  --version`) trust it besides the system's (`k8s`, `mirror` and `vm create` trust only the
  `--registry-ca` they are given, never a file in a directory; the recorded file is read only
  in a directory an install owns, one with its `.env`); the install keeps it as `<dir>/registry-ca.pem`
  (0644), and later runs trust that file. Before pulling, the installer gives it to the Docker
  daemon as `/etc/docker/certs.d/<host[:port]>/ca.crt` (an IPv6 host bracketed,
  `[fd00::5]:5000`; none for a loopback registry, `127.x` or `[::1]`), which only root writes: when the run may
  not, it stops before writing anything and prints the `install -D -m 0644 …` to run as root. A
  `ca.crt` already there holding the CA's certificates (alone or in a bundle) is kept; one that
  does not is refused, never replaced. A first install that fails removes the `ca.crt` it wrote
  with the rest (and the host directory it made); a `registry-ca.pem` that was in the
  directory before is replaced by the CA given, not removed. A loopback registry gets
  none (the daemon takes it as insecure). The daemon is this machine's: a remote `DOCKER_HOST`
  or rootless Docker (`~/.config/docker/certs.d`) needs the file put there by hand.
  `--registry` and `--registry-ca` are recorded at the first install: a re-run given either is
  refused.
- `--answers FILE` is a YAML map, `KEY: value`.
- `--dry-run` on a first install prints every `KEY=VALUE` `.env` would record (secrets
  `<redacted>`) and writes nothing. On a re-run, it logs the changes a run would record and says
  the stack would be brought up again.
- `status` prints the release (version, revision), the registry, then, while the first install
  has not finished, `not finished: the first install failed (<the first line of its error>); a
  re-run of the installer with --dir D finishes it` (or `… runs, or was stopped; …`); while a
  restore left the volumes half restored, `not running: a restore left <name>'s volumes half
  restored … (or a restore is running)` instead; each
  route's URL (after it, while an install given `--secrets` awaits its data, `not provisioned: a
  backup of the install --secrets came from is awaited; once loaded, `<short>-stow provision --dir D`
  provisions it`), each `l4` route as `<name> <protocol>/<port>` followed by its endpoint when it has
  an `address_input` (`echo      tcp/17007 localhost:17007`), `endpoint  <URL>` when the spec names an `endpoint` (that route's URL), a `tunable   KEY=value` line per advanced input (`(not set)` when none is
  recorded — the compose file's default, or off for one the front reads — a secret one
  `<redacted>`), in byo mode `cert      <its names>, valid until <date> (<N> days)` and, from 30
  days before that date or once it passed, `warning: the certificate expires in N day(s), …` (or
  `expired on …`) naming the re-run that renews it, with `hooks.maintenance.disk_pressure` a
  `pressure  <level>: <used>% used (soft …, hard …)` line and the watch's last POST, and `docker
  compose ps --all`.
- A failed first install removes the directory it created.
- The install directory holds:
  - `.env`, the project's `compose.yaml`, `.env.prev` and `.stowage-changing` (a re-run's
    changes, above);
  - `.stowage-installing`, from the first install's `.env` until its stack came up and the
    provision hook ran (or `--no-provision` skipped it): empty while a run is under way or after
    a reboot or a stop cut one, `failed: <the first line of its error, at most 300 bytes>` once
    one failed (a second line `stack: up` when the stack had come up and the hook failed); a
    re-run, `reset-db` or an upgrade whose stack comes up removes it, and so does `provision`
    after a `stack: up` failure (it brings nothing up itself); a dry run or a refused re-run
    leaves it as it is;
  - the files Stowage adds: `stowage-front.yaml` and `stowage-front.Caddyfile` (the front),
    `stowage-provision.yaml`, `stowage-schema.yaml`;
  - `provision.env` and the hook's `outputs` file;
  - `.stowage-restore-awaited`, written by an install given `--secrets` with provision values
    (with them, before its stack starts): while it is there no run provisions; `provision`, a
    `restore` that loaded and `reset-db` remove it;
  - `<short>-stow`, `tls/cert.pem` and `tls/key.pem` (`byo`);
  - `snapshots/`, `.stowage-prev/`, `.stowage-failed/`, `.stowage-next-*/` (an installer
    `upgrade --version` fetched, kept only when its run failed), and `.stowage.lock` (the run
    lock).

  The front keeps its certificates in the volume `stowage-front-data`.

- Inputs: `--set`, then the environment, then `--answers`, then — on a terminal — one form asks for
  the rest, then the spec default (which
  may name an earlier input, `${KEY}`, including one just answered). Any field can be revisited
  (Shift+Tab), and a review screen then offers to install, change an answer or quit writing
  nothing. A secret input is never echoed. With no terminal, or `--no-input`, nothing is asked; every missing required input is
  then reported at once. `--accessible` (or `ACCESSIBLE` set, or `TERM=dumb`) asks with plain line
  prompts for screen readers; there, Enter keeps the answer already given (or takes the default),
  and end of input aborts like Ctrl-C. Before that form, the install directory is asked on its
  own (unless `--dir` was given; `~/` is expanded), the host is checked (Docker, Compose, the
  directory), then how the front serves is chosen (unless given) and its ports checked, so a
  refusal never discards answers; ports are checked again after the form, whose review can also
  go back to how the front serves, keeping the answers. A key that names no input is an error.
- Nothing is pulled or written before every check passed: every value decided and recordable,
  Docker and Compose ≥ 2.20 (and Docker ≥ 27.0 with `--ipv6`), free ports (on 127.0.0.1, the
  default, `::1` too: `localhost` names both, an IPv6 host trying `::1` first), a loopback address, no foreign `compose.yaml` in the
  directory. Images are pulled by digest, only those not already present. When the project has a
  `token` pull account (the one baked into the installer, or its password from the named
  variable, which wins in a rotation), the pulls read it from a private Docker config that the
  installer writes and then removes. It never runs `docker login`: the operator's own config and
  credential store are untouched, and the operator's current Docker context is kept. `docker
  compose up` and the provision hook's run take the same config, so an image removed meanwhile
  (a prune) is pulled again with the account rather than anonymously; and when `up` fails while
  one of the release's images is gone from the engine (Compose then cannot create its container),
  the installer pulls the missing ones again and starts the stack once more. With
  `docker-config` or `none`, a mirror, or no password, the pulls use the operator's own Docker
  login, and the installer says so. The front's image is the release's own, pulled from the
  project registry (or the mirror) by digest like the others. `--offline` uses the images the operator loaded, by
  the release's tag (`docker load` keeps tags, not digests): each must have an ID the release
  records (image, linux/amd64 manifest or config digest), and a tag is never pulled — on any later
  run either. Each missing one is named. Make the archive on a host with the same image store as
  the target (containerd → containerd keeps the registry digests; classic → classic keeps the
  config digest), e.g. `docker pull` each image by digest, then `docker save`. Load the front,
  `<prefix>/<name>-front:<tag>`, too.
- `.env` (0600) records every decision — every safe default included —
  as single-quoted values compose reads literally (a value with a `'`, a line break or a trailing
  `\`, or over 64 KiB, is refused);
  its values win over the shell's environment on every run. Besides the values the compose file
  reads, it records the installer's own: `STOWAGE_REGISTRY` (the prefix images are pulled from),
  `STOWAGE_FRONT_IMAGE`, `STOWAGE_PORT_<ROUTE>` (loopback mode), `STOWAGE_HTTP_PORT` and
  `STOWAGE_HTTPS_PORT` (TLS mode) and `COMPOSE_PROJECT_NAME`. Edit none of them by hand. The compose project is
  `<name>-<directory name>-<hash of its path>`, so two installs in two directories stay two
  projects. The install keeps the installer that wrote it as `<dir>/<short>-stow` (the running file
  itself, checked to carry the release).
- A re-run (the installer again on a directory holding an install of the same release; a
  different release is an upgrade) finishes setup: it brings the stack up again and runs the
  provision hook again. Given nothing, it changes nothing recorded, and asks nothing. It changes
  what it is given, and only that:
  - `--set`, `--answers` and `--domains` (TLS modes) replace a recorded input, checked as a first
    install checks it (a required one cannot be emptied; a domain must be one, distinct from the
    other routes', and with `byo` covered by the install's certificate, which must not have
    expired). A loopback install takes no route's domain. The environment never changes a
    recorded value: an exported variable named like an input is ignored on a re-run. A key that
    names no input of the install is an error. An input whose default names another (`${KEY}`)
    keeps its recorded value when that other changes: give it too.
  - `--ports route=port` (loopback mode) or `--http-port` and `--https-port` (TLS modes) move the
    front; the other mode's flags are refused. Only a port the install does not already hold is
    checked free, so two routes may swap theirs.
  - `--tls` moves the install to another TLS mode (between `acme`, `dns-cloudflare`, `byo` and
    `internal`; to or from loopback is a new install, refused), the inputs the new mode requires
    given with it (`--set STOWAGE_CLOUDFLARE_API_TOKEN=…` for `dns-cloudflare`) and the front's
    inputs it does not read removed from `.env` (logged `removing KEY`). `--cert` and `--key`
    (both; they alone pick `byo`) give a new `byo` certificate, checked as at a first install —
    a pair, unexpired, covering every route's domain — and given without `--ca`, it must chain
    to the install's CA (`tls/ca.pem`, kept) when there is one; `--ca` alone gives a new CA for
    the install's certificate, and `--ca none` publishes none. The new files are written into a
    new directory that takes `tls/`'s place, the replaced one kept as `tls.prev/` until the stack next
    comes up (a re-run, `secrets rotate`, `reset-db`, an upgrade) (one already there, from a run that failed, is the last that came up: it is
    kept, and the failed files go). A move away
    from `byo` removes `tls/` once a re-run's stack came up in the new mode (also when the run
    that recorded the move failed). A re-run without `--tls` keeps the
    recorded mode even when the release no longer offers it. A new Cloudflare token is `--set STOWAGE_CLOUDFLARE_API_TOKEN=…`. The front is
    recreated whenever its configuration changes (the Caddyfile or the `byo` files, whose digest
    it carries as the label `io.stowage.front-config`).
  - An advanced input given empty is removed from `.env`: the compose file's default applies
    again.
  - What derives from them is computed again: each route's `STOWAGE_URL_<ROUTE>`, the
    `STOWAGE_PORT_<ROUTE>` or `STOWAGE_HTTP_PORT`/`STOWAGE_HTTPS_PORT`, each `l4` route's
    `STOWAGE_L4_<ROUTE>` and `COMPOSE_PROFILES`. The generated secrets are kept.
  - A value given as recorded is no change. Each change is logged as `changing KEY: old → new`
    (a secret input's values `<redacted>`) once the host checks passed. `.env.prev` (0600,
    secrets included) keeps the `.env` a change replaced: a change writes it from the `.env` it
    replaces, unless an earlier change has not come up since (marked by `.stowage-changing`,
    removed once a re-run's stack comes up and, unless `--no-provision`, its hook ran), so a
    second try never loses the values from before the first. An upgrade or a rollback removes
    both: values of another release are no way back. `.env` is rewritten, then the stack is brought up
    again with it and the hook run. When a step after that fails, the changes stay recorded and
    the error says so, naming `.env.prev`: a re-run giving its values goes back. The `byo`
    certificate is read only for a domain that changes, so an expired one never stops a re-run
    (nor the VM's boot).
  - `--address`, `--registry`, `--registry-ca` and `--channel` are first-install only: a re-run
    given one is refused.
- `upgrade`, run by the new release's installer, moves an install of an earlier release (ordered
  by the scheme the release records; a `command` scheme is not checked) to it. It refuses, before
  anything changes, a release that adds a route, drops the install's TLS mode, or names an input
  the release does not add (`--set` and `--answers` only give what it adds; on a terminal those
  are asked), or an install whose kept `<short>-stow` is not the installer of the release it runs
  (a rollback goes back with it). It also refuses:
  - an install of a later release (the message points to `rollback`);
  - `--set` of a value already recorded, which an upgrade keeps;
  - an install that a stopped rollback left, until `rollback` is run again;
  - `--offline` on an install that pulls.

  An install already on the release is left as it is, with a note. The upgrade follows how the
  install pulls its images (an install made with `--offline` upgrades from loaded images). Then it pulls the images, dumps the database (`pg_dump` as the migrate role, gzip,
  `snapshots/<UTC time>.sql.gz`, 0600; a failed dump stops the upgrade), records the schema
  version, keeps the running release — compose files, `.env`, `provision.env`, installer — in
  `.stowage-prev/` (one level), writes `.env` (every recorded value kept but an input the release
  no longer declares, on any shape — dropped, with `input KEY is not in <name> <version>: dropped`,
  never asked again —; the release's added inputs, secrets and safe defaults decided as at a
  first install; version, revision and images rewritten), starts it and provisions (a key the
  previous release's hook declared in `returns` and this one's does not is dropped from
  `provision.env`, with `the provision hook of <name> <version> no longer returns KEY: dropped`).
  `rollback` takes both back: the previous release's values `.env` lacks, and what its hook
  returned. A failure names the rollback. `--dry-run` prints the
  values that would change (secrets redacted). An install already on the release says so and
  names `upgrade --version`.
- `upgrade --version V`, run by any installer of the project (the one the install keeps,
  `<dir>/<short>-stow`, is the usual one), fetches V's installer and runs its `upgrade` with the other
  flags given (`--set`, `--answers`, `--no-input`, `--accessible`, `--dry-run`,
  `--no-provision`); its exit status is the command's. V is looked up among the release
  manifests (`<prefix>/<name>-release`) of the registry the install pulls from
  (`STOWAGE_REGISTRY`, a mirror included: nothing is read from anywhere else), on the channel of
  the install's release: V's latest revision that is not yanked, V matched by the scheme each
  release records (any spelling it holds equal: among equal revisions the one typed, else the first tag in sorted order; a
  `command` scheme takes V as written), among those signed by a trusted key (**Installs
  verify**, above). Before anything is fetched, the install is ordered
  against it by that scheme: an install already on that release is left as it is, with a note;
  one on a later release or revision is refused, naming `rollback`, as is an install whose
  version that scheme does not read. A `command` scheme is not ordered (the release's `upgrade`
  says so): any other version is fetched and run.
  Otherwise the installer is pulled by the digest the manifest records
  (`<prefix>/<name>-installer@sha256:…`), written to `<dir>/.stowage-next-<random>/<short>-stow`
  (0700), and refused unless it carries that release. The reads use the pull account as the
  image pulls do: on the project's registry, the `token` account, or anonymously with `none`;
  otherwise (a mirror, `docker-config`, no password) the Docker client's own login. It refuses V
  when it is not published on the channel, when any release manifest of the repository cannot be
  read (one from a newer Stowage, or with a scheme this installer does not build: it could be
  V's latest revision; run that release's installer instead), only yanked revisions are left (naming the reason),
  or the manifest records no installer; an install made with `--offline` (run the new installer
  instead); and `--offline`; and a V with no revision signed by a trusted key, or whose
  signatures cannot be read (**Installs verify**). When the fetched installer fails, it is kept (its messages name
  it); otherwise it is removed once it ran (a removal that fails is reported and leaves it).
- **Schema baselines.** A release's schema baseline is the `sql` schema-compat's `baseline`
  (absent, 0 and every other provider are baseline 1; a release without a database has none and
  is never checked). Two releases at different baselines share no schema: `upgrade` refuses
  before anything changes (no dump, no pull), saying the install moves to that release only by a
  fresh install; `rollback` — and so the VM's boot going back and its health gate, which then
  keeps the boot — refuses a previous release at another baseline, decided from the two releases'
  specs before any image is pulled. A baseline that does not read (a spec this Stowage's decoder
  refuses) is logged and not checked, like the rest of the schema check. On Kubernetes the
  baseline of the release a helm release runs is read from that release's installer (its spec
  travels in it), fetched by the tag the release records from the registry it was installed from
  and checked against this installer's keys (an installer carrying none checks no signature, as
  `upgrade --version`): `k8s upgrade` refuses another baseline, and
  `k8s reset-db --new-baseline`, run by the new release's installer, is the way across (it wipes
  the data). One that cannot be read is logged and not checked by `upgrade`. On the VM an upgrade is decided at the boot that follows the OS write:
  the boot keeps the install on its release and the health gate falls back to the passive system.
- `rollback`, run by the installer the install keeps, goes back to `.stowage-prev/`: the previous
  compose files, installer, version and images; every other value stays as it is now
  (a secret the upgrade added is kept for the next upgrade). The release it leaves is kept in
  `.stowage-failed/`. Like an upgrade it switches `.env` before the installer, and keeps
  `.stowage-prev/` until the end: a rollback that stopped is finished by running it again (so is
  an upgrade that stopped before it switched `.env`: the kept release's compose files, installer
  and start come back). When the database moved since the upgrade, it refuses — naming the
  snapshot — if a rollback barrier came since, or the previous release does not read a newer
  schema; a check that cannot run (an unreadable database, no answer) does not refuse, and says
  so. `--dry-run` pulls the previous images only when the check reads them. One install run,
  provision, upgrade, rollback or reset-db works on an install at a time, reading it under its lock; a hook or schema container a
  killed run left is refused, naming the `docker rm --force` that clears it.
- Once the stack is up, the provision hook runs (`--no-provision` skips it; `provision` runs it
  later). Each `KEY=VALUE` line it prints is recorded in `provision.env` (0600) as it arrives, and
  copied to the hook's `outputs` file (0600, `KEY=VALUE`) at the end, even when it fails; the
  terminal shows the keys and the file, never the values. A re-run of the installer runs the hook
  again. A failed hook leaves the stack running and names the command that continues it.
  One run at a time: a second is refused while one is in progress, as is a run while a stopped
  one's container is still there; a run the installer stops (timeout, interrupt, output past its
  bounds) has its container removed. `provision --set` corrects an input only the hook reads —
  one its `env` lists and the compose file never names, no route's domain, and none at all when the stack may read `.env` itself (an `env_file`; a mount of
  `.env`, of the install directory or a directory above it, or of a source with `$` or `~`; a
  volume with `driver_opts`) — recorded in `.env`; or it sets a value the hook returned before or
  declares it returns (a key `provision.env` holds, or one `hooks.provision.returns` lists, that
  the install does not record), recorded in `provision.env` and handed to the next run, where an
  empty value removes the key so the hook runs without it. Both are recorded once no other run
  holds the install, before the hook runs; any other key is refused, naming the ones it takes. A
  failed hook's message names `provision --set KEY=VALUE` and the keys it returned or declares.
  Any other input is changed by a re-run with `--set`, which brings the stack up again; the
  refusal names that command.
- `secrets export --recipient age1…` (repeatable, 1 to 16 keys) writes the install's secrets for a
  disaster recovery: the generated ones (a rotated secret's `KEY_NEXT` and `KEY_PREVIOUS` with
  them), the secret inputs (the Cloudflare token among them) and what the provision hook returned
  (`provision.env`), as YAML (`project`, `secrets`, `provision`), encrypted to those age public
  keys and ASCII-armored, into `-o` (default `<name>-secrets.age`, 0600, never over a file; `-`
  for standard output). No private key is needed on the machine; keep the file off it.
  `age --decrypt -i KEY <name>-secrets.age > secrets.yaml` gives what `--secrets` takes back.
- `--secrets FILE` (`-` for standard input) makes a first install with those decrypted secrets,
  a machine made again: every generated secret the file holds is taken, not generated (one it
  lacks is generated, and the run says so); its secret inputs are given (under `--set` and
  `--answers`, over the environment); what the hook returned is written to `provision.env`
  before the stack starts, so the hook's next run is given it. Those values belong to the old
  install's data, so the hook does not run on the new, empty database — not by this run, a
  re-run or a boot: the stack starts unprovisioned, `status` says a backup is awaited
  (`.stowage-restore-awaited`), and `sudo <short>-stow restore OBJECT` (the VM) loads it and runs
  the hook; on Docker, which has no `restore`, a backup loaded by hand, then `provision` runs it,
  as it does an empty database. A file without provision values awaits nothing. Refused: a file still encrypted,
  one of another project, an unknown field, over 1 MiB, and `--secrets` on an install that
  exists (it keeps its own). A key that is no secret of the release is left out, and said.
  Docker and the Kairos VM; on Kubernetes, not yet.
- `secrets rotate KEY` replaces a generated secret whose spec entry has `rotation: true` (any
  other is refused, naming the ones that have it), in three passes, each recorded in `.env` and
  followed by the stack brought up again (the services reading a changed value are recreated)
  and the provision hook run, under the install's lock:
  - `--stage` records a new value as `KEY_NEXT`, which the services accept and do not use
    (refused while one is staged, or while `KEY_PREVIOUS` holds one: finish first);
  - without a flag, `KEY_NEXT` becomes `KEY`, the old value is kept as `KEY_PREVIOUS` (still
    accepted) and `KEY_NEXT` is emptied (refused when nothing is staged);
  - `--finish` empties `KEY_PREVIOUS`; with it empty already, it changes nothing and succeeds.
  `--finish` while a new value is staged is refused. A pass run again once its state is recorded
  (its restart failed) changes nothing and brings the stack up again. Between two passes every
  service runs with both values it may meet, so none refuses the other age's tokens. `KEY_NEXT` and `KEY_PREVIOUS` are recorded empty from the first install (or the
  upgrade that adds the secret); the compose file passes them to the services that check the
  value.
- `reset-db` takes the install back to its first setup, run by the installer of the release it
  runs (another is refused, naming the one to use). It refuses, before anything changes, a
  `data.reset` volume the release's compose file does not declare at top level, declares
  `external` or names with an interpolation (`name: ${…}`), and an install whose `.env` records no `COMPOSE_PROJECT_NAME`. It stops the stack
  (`docker compose down`), deletes the Docker volumes of the `data.reset` volumes (found by their
  Compose project and volume labels, or one Compose adopted without them by the name Compose gives
  it, `<project>_<key>` or the compose `name:`, when a container of the stack (read before `down`)
  mounts it — one of that name another Compose project owns, or one no container of the stack
  mounts, is refused, never deleted; one never created is named, not an error, and a lookup that fails
  otherwise fails the run; the `keep_on_reset` ones and the front's stay), deletes `provision.env`, the
  hook's `outputs` file and `.stowage-restore-awaited` (the restored values gone, nothing awaits a
  backup), then starts the stack — which migrates an empty database — and runs the
  provision hook as on a first install, from the values `.env` records (unchanged: secrets,
  inputs, TLS, ports). Nothing is dumped first, and the service is down meanwhile. It asks on a
  terminal (default no), without holding the install's lock, then checks again under it; without
  a terminal, or with `--no-input`, it refuses unless `--yes`.
  `--dry-run` prints what it would delete. A `data.reset` that names nothing is refused. It holds
  the install's lifecycle lock; one that stops after the stack went down is finished by running
  it again.
- The front is the Caddy Stowage adds (`stowage-front`), the release's `front` image. An
  installer refuses a release that records no front. `STOWAGE_TLS_MODE` records how it serves.
  On each route, in this order:
  - `allow_input`: a source address the input's list (IP addresses and CIDRs, separated by commas
    or spaces) does not hold gets 403 — everyone while it is empty. The source is the address the
    front sees: through Docker's published ports, a client on another host keeps its own, while
    this machine's own requests may come from the Docker bridge's gateway.
  - `strip_headers` are removed, whatever the client sent, and so are the headers
    `forward_auth.copy_headers` lists (even while forward auth is off), so a client never forges
    one.
  - `forward_auth`: while the input `address_input` names is not empty, each request is first
    sent to that service (`[http[s]://]host[:port]`, an IPv6 address bracketed, `[fd00::1]:9091`, neither zoned nor
    IPv4-mapped — one outside the stack needs `--ipv6`, the front's network otherwise having no
    IPv6; at the path `uri_input`'s value gives, `/`
    when it is empty) with Caddy's `forward_auth`; its non-2xx answer is the client's, and on a
    2xx the headers `copy_headers` lists are taken from its answer onto the request.
  - The upstream serves.
  The values are checked when given (an address, a path `/…`, IPs and CIDRs) and written into
  the Caddyfile, so a change is a re-run's `--set`.
- `publish_ca: { volume: V }`: the front copies the CA its certificates chain to into the
  project's volume `V` as `ca.crt` (0644), for the services that mount it (read-only) to trust:
  with `internal` the local CA's root (as soon as Caddy made it), with `byo` the `--ca FILE`
  given (PEM certificates, one of which the certificate's chain must end at; copied into
  `<dir>/tls/ca.pem`). The allowlist takes no IPv6 zone (`%eth0`). In the other modes, and with `byo` without `--ca`, it removes `ca.crt`.
  `--ca` is refused outside `byo` and for a project without `publish_ca`.
- Loopback mode (`none`, the default when the project offers it and no domain is given): each
  route on its own port of a 127.x address, plain HTTP; `loopback_env` relaxes safe defaults.
- L4 routes (`l4.routes`): in every mode, loopback included, each route's host port is published
  on every address to its upstream, over its protocol, in the front's compose file
  (`stowage-front.yaml`; compose adds it to the service's own); the front never sees that traffic.
  With `--ipv6`, Docker forwards them over IPv6 too. A route with an `address_input` gets
  `STOWAGE_L4_<ROUTE>` = `<the input's value>:<port>` (an IPv6 address bracketed,
  `[fd00::7]:7443`), recorded in `.env` and empty while the input is; the address is never
  guessed (behind NAT the host's own is not the one clients dial). A first install refuses a
  route's TCP port a process of the host already listens on (`port(s) 7443 (data) already in use
  on this host: the project's L4 routes publish them`), and so does an upgrade for a route the
  stack does not publish already; an install, re-run or upgrade refuses front ports on one of them
  (an upgrade's refusal says to move the front with a re-run of the installer the install runs,
  then upgrade) (`the front's https port 7443 is the L4 route data's: choose another with
  --https-port`, `--http-port`, or `--ports` in loopback mode). Stowage never probes them: the
  stack's health is the `health` provider's. On Docker it never restricts them either (the host's
  firewall is the operator's, and an `allow_input` is never asked there); on the VM the firewall
  opens each route's port, from its `allow_input`'s sources alone when it has one (see "The
  firewall" under the installer on the Kairos VM).
- Optional services (an input's `profile`): the install records `COMPOSE_PROFILES` in `.env`, the
  profiles whose input holds a value, comma-separated and sorted (recorded empty while none is;
  not recorded for a project whose inputs name no profile). Every `docker compose` the installer
  runs starts exactly those, a shell's `COMPOSE_PROFILES` never overriding it; one an operator runs
  in the install directory reads them from `.env`, unless their shell exports `COMPOSE_PROFILES`
  (compose prefers the environment). A run that turns a profile off removes its services'
  containers before the stack comes up (`docker compose --profile <each> rm --stop --force
  <services>`: `up` would leave them running).
- TLS mode: each route on its own domain over HTTPS, only the HTTP and HTTPS ports published (80
  and 443 on every address, HTTP/1.1 and HTTP/2; `--http-port` / `--https-port` move them: with
  `acme` the public ports stay 80 and 443, forwarded to them, otherwise the moved HTTPS port is the
  public one, in the URLs and the redirect from plain HTTP). Given values are checked before the
  form opens. `--domains`, `--set` and `--answers` give a domain; an exported variable alone picks TLS only
  for a project that offers no loopback mode, and on a terminal it is asked, as the default, never
  recorded unseen. `--cert` picks `byo`; a `byo` install refuses a directory already holding a
  `tls/`, and a mismatched certificate and key re-open their form, saying why. A route's domain is its `domain_input`, an input like any other
  (`--domains route=host` is the shorthand); giving one picks TLS: `acme`, or the project's
  first HTTPS mode when it does not offer `acme`, unless `--tls` says otherwise. Domains are lowercase DNS names, one per route. Safe defaults stay safe. One
  certificate source per mode, never a fallback: `acme` (Let's Encrypt alone, HTTP-01 or
  TLS-ALPN, so ports 80 and 443 must reach the host — with moved ports, a run says to forward
  them, and that IPv6 forwards no port: a domain whose AAAA record names the host is reached on
  80 and 443 themselves; `STOWAGE_ACME_EMAIL`, optional, is checked
  as an address), `byo` (`--cert` / `--key`, PEM: checked as a pair, unexpired and covering every
  domain, and its chain — without `--ca`, it must verify to a CA this host trusts with the
  intermediates the file holds, so a leaf given without its intermediate is refused, naming its
  issuer; a chain ending at a self-signed certificate no host trusts, a private CA's, is said and
  kept — then copied into `<dir>/tls/` 0600; `status` shows its end, and warns from 30 days
  before it), `internal` (Caddy's local CA; its root is
  `/data/caddy/pki/authorities/local/root.crt` in the front). `dns-cloudflare`
  (Let's Encrypt alone, DNS-01 through Cloudflare, so no port needs to reach the host for
  issuance; `STOWAGE_ACME_EMAIL` as with `acme`; `STOWAGE_CLOUDFLARE_API_TOKEN`, required and
  secret, a token with Zone DNS Edit on each domain's zone, in the form Cloudflare issues — 35 to
  50 letters, digits, `-` or `_`, or `cfut_` / `cfat_` and at least 32 more — recorded in `.env`
  and given to the front's container as `CLOUDFLARE_API_TOKEN`; the project's services and the
  provision hook are not given it, though a stack that reads `.env` itself — an `env_file` or a
  mount of the install directory — sees it like every recorded value). HSTS is sent except with `internal`.

**Backups, restore and the disk-pressure watch on Docker.** The VM's host daemon's code runs in
the Docker installer's process for one command (no daemon stays): run by the installer the install
keeps (`<dir>/<short>-stow`, the release it runs; another is refused).

- `backup config|run|status|list|verify` and `restore OBJECT` (`--dir D`, the other flags as on
  the VM — see "Backups" under the installer on the Kairos VM) take a project's `pg-age-rclone`
  backup: the settings in `backup.yaml` (root's alone) and the last run in `backup-last.json` in
  the install directory, the volumes' archives and the dump encrypted to the recipients as they
  stream to the backup runner
  (`<name>-backup`, pulled with the stack's images, by digest, since a release of a project with
  the `docker` shape and a `pg-age-rclone` backup publishes one, and pulled again when the engine
  lost it), and the restore into a database
  of its own, swapped in only once it loaded, then the stack up and provisioned. The schedule is
  a systemd timer of the install's own, `<name>-backup-<8 hex of the install directory's
  digest>.timer` and its `.service` (`ExecStart=<dir>/<short>-stow backup run --if-configured
  --dir <dir>`) in `/etc/systemd/system`, written, enabled and started by `backup config` (and
  `--remove` removes them); on a host without systemd, or a run not as root, there is none (a
  backup runs when `backup run` is), and the schedule is kept unchecked. One process at a time
  runs the jobs (`backup run`, `verify`, `restore`): it holds `.stowage-host.lock` in the install
  directory, and another waits for it, saying so; the reads and the settings (`backup status`,
  `list`, `config`, a settings change holding `.stowage-settings.lock` alone) never wait for a job, and `backup status` says `running` while another process runs a
  job (a backup, a verify or a restore). A signal stops the job the command runs.
- The disk-pressure watch (`hooks.maintenance.disk_pressure`): the install (each run of the
  installer that brings the stack up) writes, enables and starts `<name>-disk-<8 hex>.timer` and
  its service, which runs `<dir>/<short>-stow disk-check --dir <dir>` two minutes after boot and
  every 10 minutes: it reads the filesystem of the Docker engine's data directory (`docker info`'s
  `DockerRootDir`), and from `soft_pct` POSTs the level as the VM's watch does, the last POST kept
  in `disk-pressure.json`, which `status` shows. Without systemd or root it is not installed, and
  the install says so; a release without the hook removes the units.

### The installer on Kubernetes (`<short>-stow k8s`)

A project with the `kubernetes` shape gets a `k8s` command in the same installer (bootstrap it as
above, then `sh <name>.sh k8s install …`). It drives the operator's own `helm` (≥ 3.14) and
`kubectl`, on the current kubeconfig context unless `--kube-context` says otherwise, and works in
the current directory. A release with no chart (published without the shape) is refused.

```bash
<short>-stow k8s install  [--namespace NS] [--release R] [--kube-context C] [--values FILE]…
                       [--image-registry PREFIX] [--registry-ca FILE] [--set KEY=VALUE]… [--answers FILE]
                       [--no-input] [--accessible] [--timeout 10m] [--dry-run] [--no-provision]
<short>-stow k8s upgrade  [the same flags; --timeout 6m]                 # the NEW release's installer
<short>-stow k8s upgrade --version V [the same flags]                    # fetches V's installer
<short>-stow k8s rollback [--namespace NS] [--release R] [--kube-context C] [--revision N] [--timeout 6m] [--dry-run]
<short>-stow k8s status   [--namespace NS] [--release R] [--kube-context C]
<short>-stow k8s reset-db [--namespace NS] [--release R] [--kube-context C] [--yes] [--no-input]
                       [--accessible] [--registry-ca FILE] [--timeout 10m] [--dry-run] [--no-provision]
                       [--new-baseline]                               # the NEW release's installer, across a baseline
<short>-stow k8s uninstall [--namespace NS] [--release R] [--kube-context C] [--data] [--yes]
                       [--no-input] [--accessible] [--timeout 10m] [--dry-run]
<short>-stow k8s provision [--namespace NS] [--release R] [--kube-context C] [--set KEY=VALUE]…
                       [--registry-ca FILE]                           # a project with a provision hook
<short>-stow k8s secrets rotate KEY [--stage | --finish] [--namespace NS] [--release R]
                       [--kube-context C] [--timeout 6m]
```

- Defaults: `--namespace <name>`, `--release <name>` (each a DNS label; a release name is at most
  53 characters).
- `install` refuses a namespace that already has the release (it names `upgrade`). It creates
  the namespace when missing (one the operator may not read is taken to exist), then applies, with `kubectl apply --server-side --field-manager
  stowage`, the Secret `<release>-stowage` (type `Opaque`): every `secrets` key, generated as the
  Docker shape generates it, and every input that applies to Kubernetes (`shapes` empty or
  holding `kubernetes`, and no `when`: a front mode is Docker's), resolved as the Docker shape
  resolves inputs (`--set`, the environment, `--answers`, the terminal form, the default). When
  the project has a `token` pull account with a password (the baked one, or its variable's,
  which wins) and the release is pulled from the project's registry, it also applies
  `<release>-stowage-pull` (type `kubernetes.io/dockerconfigjson`, the account for the registry's
  host); a first install with a `token` account and no password is refused (the cluster could not
  pull the images). Both carry the labels `app.kubernetes.io/managed-by: stowage`, `app.kubernetes.io/part-of:
  <name>` and `app.kubernetes.io/instance: <release>`; a Secret of either name without all three
  (another tool's, another project's, another release's) is never read or written over (the run is
  refused). A Secret an earlier
  install left is kept: its values are never asked or generated again (a first install still
  takes `--set` over them), and keys it holds that the release does not know are kept.
- Then it pulls the chart by the digest the release manifest records
  (`<prefix>/charts/<name>-<channel>@sha256:…`, at most 64 MiB, checked against that digest) with
  the pull account, or anonymously for public images, or the Docker client's own login (a
  mirror, `docker-config`, no password), into a private temporary directory, and runs `helm
  upgrade --install <release> <chart> --wait --timeout <T>` with `--atomic` (helm 3) or
  `--rollback-on-failure` (helm 4): a release that does not become ready is rolled back (removed
  on a first install; the Secrets are kept, and the next `install` reuses them). helm gets a
  values file of the installer's, `stowage: {registry, secret, pullSecret}`, then each
  `--values FILE` in order (they win).
- Once helm has settled the release (`install`, `upgrade` — the same release applied again
  included — and `reset-db`), the installer runs the provision hook, unless `--no-provision`
  (`k8s provision` runs it later). A failed hook leaves the release as it runs (an upgrade is not
  rolled back) and names `<short>-stow k8s provision …` and its `--set KEY=VALUE`, with the keys the
  hook returned. Before anything changes, the run reads the release manifest of its own release
  under its tag in the registry the release is pulled from (`<prefix>/<name>-release`; it must be
  this installer's version and revision, signed by a key the installer carries when it carries
  any) for the digest of the release's installer image (`<prefix>/<name>-installer@sha256:…`):
  none there refuses the run, as does an account that may not, in the namespace (each asked with
  `kubectl auth can-i`: the first alone, so a credential plugin signs in once, then the rest at once;
  a question it cannot answer yes or no is an error, not a refusal), create,
  get, list, watch and delete `jobs.batch`, get, list and watch `pods`, get the `log` subresource of `pods`, create and
  patch `serviceaccounts`, `roles` and `rolebindings` (`rbac.authorization.k8s.io`), get, create
  and patch `secrets` and get `configmaps` — `--no-provision` deploys without them. The hook then runs as the Job `<release>-stowage` (created with
  `kubectl create`, waited for, its pod's logs copied to the installer's output, then deleted,
  whatever happened; one an earlier run left is deleted once it has ended, one that still runs
  refuses the run, one of that name the installer did not create is refused): one pod, never
  retried (`backoffLimit: 0`), `restartPolicy: Never`, an `activeDeadlineSeconds` of the hook's
  30 minutes and 5 more, `ttlSecondsAfterFinished: 3600`, the release's pull Secret (its
  `stowage.pullSecret` value), `seccompProfile: RuntimeDefault`, no service links and no
  ServiceAccount token mounted; the Job and its pod carry the three Stowage labels and
  `app.kubernetes.io/component: stowage-provision` (a chart with default-deny NetworkPolicies
  admits it by them: ingress to the Services the hook calls, and egress for `record` to the
  Kubernetes API server). Its steps: `copy` (the installer image, an init container) puts the
  installer on the pod's `emptyDir`; `check` (the same, an init container) patches the outputs
  Secret with nothing, proving the way to it (its grant, the network) before the hook makes
  anything; then side by side `provision` runs the hook's `cmd` in the
  hook's image (as its own user, which must not be root — `runAsNonRoot`, as the restricted Pod
  Security standard requires —, no privilege gain, every capability dropped), wrapped by that
  installer, which sends its stdout to the `emptyDir` — never to the pod's log, which a cluster's
  log collectors ship — and its stderr to the log; and `record` (the installer image) follows that
  stdout and writes each `KEY=VALUE` line into the Secret `<release>-stowage-provision` as it
  arrives, as Docker records them (the same grammar, bounds and refusals), so a value printed
  survives a pod stopped later (an eviction, the deadline, an interrupted installer, which
  deletes the Job: a last read first); it ends once the hook has, trying the last values for up
  to 2 minutes while the API server cannot answer (a refusal, a 4xx, is final). As on Docker, the hook is stopped past
  1 MiB of stdout or a line past 64 KiB (a CRLF line's `\r` not counted). A read of the Job or its pod that fails is
  tried again (for up to 2 minutes), never taken as the hook's end.
  The three installer steps (`copy`, `check`, `record`) run as uid 65532 (non-root, read-only root filesystem, no privilege
  gain, no capability); only `check` and `record` hold the token of the ServiceAccount
  `<release>-stowage-provision`, whose Role allows `patch` on that one Secret by name and nothing
  else. The hook's container receives the keys its `env` lists that the release's Secret
  `<release>-stowage` holds and every key of `<release>-stowage-provision` (what it returned
  before), each by `secretKeyRef` (never `envFrom` of either Secret), a safe default it lists as
  its value (the Secret holds none) unless the chart's ConfigMap names that key (the chart's
  value then stands), plus `STOWAGE_NAME`,
  `STOWAGE_PROJECT_VERSION` and `STOWAGE_REVISION`, and, as `envFrom`, the ConfigMap
  `<release>-stowage-provision` when the chart renders one (optional: how a chart gives the hook
  what only it knows, such as its Services' in-cluster URLs; no secret belongs there). A pod that
  cannot start (an image it cannot pull, a configuration refused), whose hook step ended without
  recording how the hook ended (killed for its memory, say), or whose record step ended while the
  hook ran is given up after 2 minutes, as is a Job whose pod is not created (naming `kubectl
  describe job`); one no node takes, after 10 (a cluster autoscaler may be adding one). A failed
  pod's error is the `check` or `record` step's own (keys, never values), and the logs of the
  steps that ran are copied. The outputs Secret, the
  ServiceAccount, the Role and the RoleBinding carry the three Stowage labels, are applied
  server-side (the Secret's data only ever patched) and stay, as the other Secrets do. A key of
  `<release>-stowage`, a safe default or a key of the chart's ConfigMap is not one the hook may return (`record` refuses
  it, as Docker refuses a recorded key), and one the outputs hold already (or one that is no
  variable name) is dropped before the run. The keys recorded are
  printed (`provisioned: K… (in the Secret <release>-stowage-provision)`), never their values. A
  chart needs no helm hook Job of its own for this.
- `k8s provision` runs the hook again on the release (refused: a `pending-…` release, one with no
  Secret `<release>-stowage`, one running another release than this installer's, a project
  without a hook). `--set KEY=VALUE` sets a value the hook returned before (a key of
  `<release>-stowage-provision`) or declares it returns (`hooks.provision.returns`), `--set KEY=`
  forgets it: a merge patch of `<release>-stowage-provision` from a private file (never a command
  line), before the Job runs, which hands it to the hook. Any other key is refused, as is one the
  release's Secret, a safe default or the chart's ConfigMap `<release>-stowage-provision` holds:
  on Kubernetes `--set` never changes a recorded input. A failed hook's message names
  `k8s provision --set` and the keys it returned or declares.
- `upgrade`, run by the new release's installer, requires the release in the namespace, refuses
  one helm is still working on (`pending-…`), one not installed from a chart Stowage published or
  of another project, and one running a later release by the scheme the release records (it
  names `rollback`; a `command` scheme is not ordered), and one running another schema baseline
  (see "Schema baselines"; it names `reset-db --new-baseline`). The same release is applied again: that is
  how values change (`--values`). It adds to the Secret the secrets and inputs the release adds
  (asked as on an install; `--set` of a recorded value is refused, as on Docker), then runs helm
  as `install` does, with `--reset-then-reuse-values`: the new chart's values, then every value
  given at earlier runs (the installer's and the operator's `--values`), then this run's. By
  default the chart and the images come from the registry the release was installed from. An
  upgrade with no pull password refreshes no pull Secret and keeps the one the release names.
- `upgrade` drops, from the Secret `<release>-stowage`, the value of an input the release no
  longer declares (fetching the installer of the release the helm release runs, as for the schema
  baseline: one that cannot be read keeps every value), saying `input KEY is not in <name>
  <version>: dropped`; the server-side apply removes it. The provision hook's run drops, from the
  outputs Secret, a key the previous release's hook declared in `returns` and this one's does not.
- `upgrade --version V` resolves V as the Docker shape does, among the release manifests of the
  registry the release records (or `--image-registry`), on its channel, refuses it unless it is
  signed by a key this `<short>-stow` carries (when it carries any), orders the release
  against it (already on it: a note; later: refused, naming `rollback`; a `pending-…` release is
  refused first, as `upgrade` refuses it), then fetches V's
  installer into `./.stowage-next-<random>/` and runs its `k8s upgrade` with the other flags given.
- `rollback` refuses a `pending-…` release, as `upgrade` does, then runs `helm rollback <release>
  <N> --wait --timeout <T>` and prints what the release runs then. Without `--revision N` (a
  negative one is refused) it reads `helm history`: a release uninstalled with its history kept
  goes back to its last revision; after a revision that did not deploy (an upgrade or a rollback
  that failed and was left), it goes to the latest revision that deployed; otherwise it goes back one upgrade, to the latest revision
  that deployed before the one the release runs came from (a revision a rollback made comes from
  the revision it copies) and ran other values or another chart than what runs, so it never picks
  a failed revision or a copy of what runs (an upgrade that changed nothing). When there is none
  (a first install, or a release back to it) it is refused, and `helm history` lists the
  revisions to pick one. The Secrets are not changed. `--dry-run` passes helm's own.
- The schema check (`shapes.kubernetes.schema: { url_env, image }`, with the `sql` schema-compat
  provider): `upgrade` (to another release) reads the database's schema before helm migrates and
  records it in the revision's values (`stowage.schemaFrom`; empty when it could not be read, so an
  earlier value is never reused), and `rollback`, before helm is called, refuses as Docker's and
  the VM's rollbacks do: a rollback barrier (`barrier_after`) since the schema the upgrade from the
  target revision recorded, or a newer schema than that, which the target release's
  `reads_newer_image` does not read (its label, read from the image in the registry, its release
  manifest signed by a key this installer carries). Each query runs as a pod
  `<release>-stowage-schema-<8 hex>` in the namespace, made from the pod template of the workload
  running `reads_newer_image` (its `env`, `envFrom`, pull Secrets, service account, node selector
  and tolerations), with `image` (the release's, by digest) in its place running `psql … -c
  <query> "$(<url_env>)"`, non-root, read-only, deleted once read (it needs `pods` create, get and
  delete and `pods/log` get). A check that cannot run (nothing recorded, a revision helm made by a
  rollback, the pod refused) is logged and does not refuse; without `shapes.kubernetes.schema`
  nothing is checked, and lint warns.
- `status` prints what the release runs (name, version, revision, channel) and its registry,
  `endpoint  http://<release>-<service>.<namespace>.svc:<port>` when the spec names an `endpoint`
  (see "Extensions"), then `helm status`.
- `secrets rotate KEY` is the Docker shape's three passes on the release's Secret
  `<release>-stowage`, run by the installer of the release it runs (another is refused, as are a
  release with no such Secret and a `pending-…` one): each pass
  applies the Secret, then restarts the Deployments, StatefulSets and DaemonSets the release's
  manifest makes in its namespace that read the key (a `secretKeyRef` of the Secret naming `KEY`,
  `KEY_NEXT` or `KEY_PREVIOUS`, or the whole Secret by `envFrom`, a secret volume or a projected
  one), one after the other (`kubectl rollout restart`, then `kubectl
  rollout status`, each bounded by `--timeout`). The chart gives `KEY_NEXT` and `KEY_PREVIOUS`
  to the pods that check the value.
- `reset-db` takes the release back to its first setup, run by the installer of the release it
  runs (another is refused, as is a `pending-…` release and a project whose `data.reset` names
  nothing). With `--new-baseline`, run by the installer of a release at another schema baseline,
  it wipes the release the same way and installs the installer's own release instead — the one
  way a release crosses a baseline. Before the wipe it writes the release's Secrets as `upgrade`
  does: the secrets and inputs the new release adds (asked, or refused without `--yes` values
  under `--no-input`) and the pull account it carries; the confirmation names what it adds. It is
  refused when the running release is this installer's, at the same baseline (`upgrade` moves
  it), or at one that cannot be told (never wiped on a guess: a release the registry no longer
  holds, one not signed by a key this installer carries, a mirror without it); without it, a
  release at another baseline is refused naming the flag. It reads the values the release was given (`helm get values`: the installer's and the
  operator's `--values`) and its manifest, pulls the chart as `upgrade` does from the registry the
  release records (`--registry-ca` trusts a mirror's CA for that read), then runs `helm uninstall <release> --wait`, deletes with
  `kubectl delete --wait` the PersistentVolumeClaims the manifest makes in the release's namespace
  and the ones its StatefulSets there claim (`<template>-<statefulset>-<ordinal>`, carrying the
  set's `selector.matchLabels`, as Kubernetes labels them; a set without them is skipped) — every
  one but those the chart marks `helm.sh/resource-policy: keep`, which it keeps (`keep_on_reset`
  is Docker's and the VM's) — and installs the same chart again with those values, as `install` runs
  helm. The Secrets `<release>-stowage` and `<release>-stowage-pull` are kept, so the secrets and
  inputs are the ones first installed; the Secret `<release>-stowage-provision` (what the
  provision hook returned) is deleted after the claims, before the install, and the hook runs
  once the release is settled, as on a first install (`--no-provision` skips it), the dry run and
  the confirmation naming it; the chart's own hooks seed the empty data. Nothing is
  dumped first, a database outside the release (`external`) is not touched, and the release is
  down meanwhile. It asks on a terminal (default no); without one, or with `--no-input`, it
  refuses unless `--yes`. `--dry-run` prints what it would uninstall and delete. A failure from
  the uninstall on (helm may forget the release even when its wait fails) writes the values to a
  new file, `./<release>-values-<random>.yaml` (0600; an existing file is never written over; when
  the current directory refuses it, the run's temporary directory is kept and named), names the
  claims still to delete when they may hold the data (the `kubectl delete` with the namespace and
  `--context` the run used, read from `kubectl config current-context` when `--kube-context` was
  not given, shell-quoted), and the `k8s install --values` that installs the release again (with
  the run's namespace, release, that context and `--timeout`, `--image-registry` when the release came
  from another registry than the project's, and the `--registry-ca` given). `--timeout` bounds each helm and kubectl step.
- `--image-registry PREFIX` pulls the chart and the images from a mirror holding the same
  repositories (`charts/<name>-<channel>`, `<name>-<key>`, and `<name>-release` and
  `<name>-installer` for `upgrade --version` and the provision hook's Job); no pull Secret is written for it, so give the
  chart your own pull Secret through `--values`.
- `uninstall` removes the release and what the installer made beside it: `helm uninstall
  <release> --wait`, then the provision Job `<release>-stowage` (`kubectl delete
  --cascade=foreground --wait`), the provision hook's RoleBinding, Role and ServiceAccount
  `<release>-stowage-provision`, and the Secrets `<release>-stowage-pull`, `<release>-stowage-provision`
  and `<release>-stowage`. It deletes only an object whose labels say this installer made it for this
  release (`app.kubernetes.io/managed-by: stowage`, `app.kubernetes.io/instance: <release>`); one of
  those names another tool made is kept and named. It takes any release a chart Stowage published
  (one whose values hold `stowage.release`), whatever its project name or version, so it removes a
  release an older installer made, and moving a release to another registry or project name is an
  uninstall then an install; another release is refused, naming `helm uninstall`. The data:
  - The release's PersistentVolumeClaims (as `reset-db` finds them) are deleted only with `--data`,
    or when the operator answers so on a terminal ("Yes, and keep the data" or "Yes, and delete the
    data"; default no); `--yes` never asks and keeps them.
  - `helm uninstall` itself deletes the claims the chart makes (not its StatefulSets' claims, nor one
    marked `helm.sh/resource-policy: keep`): when the chart makes one, `uninstall` without `--data`
    is refused (`the chart of release R makes the PersistentVolumeClaims …, which helm uninstall
    deletes with it`), and a terminal offers only deleting the data.
  - Data kept keeps the Secrets that answer for it: `<release>-stowage` (the values it was made
    with, a database's password among them) and `<release>-stowage-provision` (what the provision
    hook returned). `k8s install` of the release then takes the data back with them, as it takes
    a Secret an install left. Data is kept when its claims are not deleted, when the chart keeps
    one (`helm.sh/resource-policy: keep`, `--data` or not), and when helm no longer knows the
    release. `kept:` names the `kubectl delete persistentvolumeclaims` and `kubectl delete secret`
    that remove them later (the release is gone then, so a second `uninstall` cannot tell its
    claims); an `uninstall` that finds nothing but those Secrets is refused, naming them.
  - A release helm no longer knows (gone, or `uninstalled` with its history kept) has its other
    objects deleted all the same, its claims untold (none deleted, `--data` or not); nothing of it
    at all is refused (`no release R in NS, and nothing <short>-stow made for it
    there`).

  It prints `release R in NS is uninstalled` (`… was uninstalled already: what the installer left of
  it is deleted` when helm no longer knew it), then `kept:` and what it kept, each with the
  `kubectl` that deletes it later: the claims and Secrets of the data, the claims the chart keeps,
  another tool's objects, and the namespace, always kept. It asks on a terminal (default no); without one, or with `--no-input`,
  it refuses unless `--yes`. `--dry-run` prints what a run would delete and keep, and changes
  nothing.
- `--registry-ca FILE` (on `install`, `upgrade`, `reset-db` and `provision`; certificates only, as for Docker) makes the
  installer's reads of the registry (the chart, `upgrade --version`, the release manifest the hook's Job reads) trust that CA besides the
  system's, for that run: it is not recorded. The cluster's nodes pull the images and must trust
  the CA already — that is the cluster owner's (the container runtime's registry configuration
  on each node), not the installer's.
- `--dry-run` checks everything and pulls the chart, then prints the namespace it would create,
  the Secret values it would add or change (secrets `<redacted>`), the installer's values file,
  the helm command and, with a hook, `then the provision hook, as the Job <release>-stowage (the
  installer image …)`, and changes nothing.
- The chart is checked against the digest the release the installer carries records; `upgrade
  --version` checks the fetched release's signature first, with the keys this `<short>-stow`
  carries that the release in the cluster still lists (`stowage.publicKeys`), and refuses to run
  when none is left: `release R in N trusts signing keys this <short>-stow does not carry`.

### Extensions

An optional component of a project is an app of its own — its own Stowage project, release and
installer — and the project (the host) takes its endpoint as an ordinary input: empty, the
component is off. The host's spec names the extensions it can use:

```yaml
extensions:
  - { name: atlas-search, input: SEARCH_URL, key_input: SEARCH_KEY, prompt: "Full-text search over the events" }
```

The extension is published on the same registry prefix under its own name, so its installer is
`<prefix>/<name>-installer`, signed by the same keys, on the same channels. Its own spec names the
route that is its endpoint (`endpoint: api`). Installing the extension from the host is never
required; giving the host its endpoint is all the host's install owes it. Two ways get there:

- **Extension first.** Install the extension with its own installer. At the end of an install, a
  re-run or an upgrade on Docker, and of `install` on the VM, it prints the endpoint route's
  `STOWAGE_URL_<ROUTE>`, recorded in its `.env`: behind a TLS front `endpoint  https://<its domain>
  (the URL a project using <name> as an extension is given)`; in loopback mode `endpoint
  http://<address>:<port> (loopback: this machine only; another compose project's containers
  cannot reach it, a project using <name> as an extension reaches it through a domain: a new install
  with --tls)` — another project reaches an extension on Docker only through its domain,
  and the Docker shape's `status` shows it as `endpoint  <URL>` (the VM's `status` does not). On Kubernetes, `k8s install` and `k8s upgrade` print,
  and `k8s status` shows, `http://<release>-<service>.<namespace>.svc:<port>`, from the route's
  `upstream: <service>:<port>`: the chart's contract is to expose that upstream as the Service
  `<release>-<service>` on that port. Then give the URL to the host's install as its input (asked,
  `--set KEY=…`, `--answers`), and the credential, when the extension needs one, as its
  `key_input`.
- **Host first.** When a host install ends with an extension's input empty — an install, a
  re-run or an upgrade on Docker, `install` on the VM, `k8s install` or `k8s upgrade` — it prints
  a tip per extension, from the spec (an offline or mirrored install shows it too):

  ```text
  tip: the extension atlas-search (Full-text search over the events) is off: SEARCH_URL is empty
    install it: docker run --rm <prefix>/atlas-search-installer:<tag> > atlas-search.sh && sh atlas-search.sh
    then give its endpoint: <dir>/at-stow --dir <dir> --set SEARCH_URL=<its endpoint> --answers <a file: SEARCH_KEY: <its key>>
  ```

  `<prefix>` is the registry the install pulls from (a mirror included). `<tag>` is the tag of the
  extension's release on the install's channel, read from the registry's
  `<prefix>/<name>-release:<channel>` (at most 5 s for all the extensions, skipped with
  `--offline`); when the registry answers that it holds none, the tip adds `<prefix>/<name>-installer
  is not published on the <channel> channel`; on any other failure it says nothing more, and
  `<tag>` is `latest`, the bootstrap one-liner's. On Kubernetes the install line ends `sh <name>.sh
  k8s install`; `k8s upgrade` keeps a recorded input, so the release takes the endpoint at a new
  install: `helm uninstall <release> --namespace NS` (its Secret is kept), then `<short>-stow k8s
  install --namespace NS --release R --set KEY=…`; on the VM it is `sh <name>.sh` (Docker) or `sh
  <name>.sh vm create` (a VM), then `sudo <short>-stow install --set KEY=…`. An extension's
  `key_input`, a secret, is never a `--set` in the tip: an `--answers` file holding `KEY2: …` on
  Docker and the VM (a re-run reads no environment), the environment (`KEY2=… <short>-stow k8s
  install …`) on Kubernetes. An extension whose input this shape does not ask has no tip. A
  dry run prints neither the endpoint nor the tips, and neither ever fails the run.

### Sites without internet (`<short>-stow mirror`)

A site without internet runs its own registry, a mirror with its own certificate authority. A
release crosses the gap as one file, and the operator, who has `<short>-stow` but not `stowage`,
carries it:

- `mirror export` (on a machine with internet) reads the channel's latest release — `--channel`,
  default the channel of the release this `<short>-stow` carries; with `--version V` that release
  of the channel instead (its latest revision not yanked, or `--revision N`; signed by a key this
  `<short>-stow` carries, as `upgrade --version` reads one) — from the project's registry with
  the pull account, and writes it to `--output` (default `<name>-<tag>.oci.tar` in the current
  directory; an existing file is refused, then and when the archive, written beside it, is
  linked to its name once whole; on a filesystem without links, FAT or exFAT, it is renamed
  after a last check; a mode the filesystem refuses is logged, not an error). It holds the release manifest under its tag and under the channel's tag, every image
  (`<name>-<key>`, every platform of an index), the installer (`<name>-installer`), the chart
  (`charts/<name>-<channel>`, under its chart version), the OS bundle (`<name>-os`) and the disks
  its installs boot (`<name>-disk`, under `<tag>-<format>` of the release that published them: its
  own, or the set its manifest records; `--disks` limits them to the formats given, `none`
  exports none; a format the release has no disk of is refused, as is a disk a newer set
  replaced, which is no longer published), each by digest and under the
  tag it was published with, with its signatures (each one's referrers). Every blob is checked
  against its digest as it is read. It prints `Exported <name> <version> (revision N, <tag>) to
  <file>` with the manifests, blobs and bytes written.
  `--shapes S,…` (`docker`, `kubernetes`, `kairos`; default every shape the project has) exports
  only what the site runs: without `kubernetes` no chart, without `kairos` no OS bundle and no
  disk (`--disks` is then refused). The release manifest still names them: an install of a shape
  left out says, from the mirror, what is missing. The channel's tag in the archive is on the
  release exported, so importing an earlier release into a mirror whose channel holds a later one
  takes `mirror import --older`.
- The archive is a standard OCI image layout (`oci-layout`, `index.json`, `blobs/sha256/`) as one
  tar; each top-level manifest carries `org.opencontainers.image.ref.name` (its tag; none for a
  signature) and `dev.stowage.repository` (its repository under the registry prefix), so skopeo
  and oras read it as well.
- `mirror import FILE --to PREFIX` (on the site) pushes every manifest of the archive, with what
  it names, into `<PREFIX>/<repository>`, by digest and under its tag. The bytes are the
  published ones, so every digest is the same and the signatures still verify there (the import
  does not check them: an install checks the release when it reads it from the mirror). The
  release manifest and the channel's tag are pushed last, so the mirror never names a release
  whose artifacts it lacks. A blob or manifest whose content is not its digest's, a repository
  path or tag that is not one (`..`), or a repository that is not the release's own
  (`<name>-release`, `-installer`, `-os`, `-disk`, `<name>-<key>` for each image, and the chart's
  `charts/<name>-<channel>`; checked for the whole archive before anything is pushed) is
  refused. So is an archive whose `<name>-release` does not hold exactly the release's tag and
  its channel's, on one manifest of a `<name>` release; that tags anything else than the
  release's artifacts under the tags the release gives them (by the digests it records); whose
  release names an image this
  `<short>-stow`'s project does not (a release that adds one is imported with its own `<short>-stow`,
  in the archive's `<name>-installer`); whose release is an earlier publication of the channel
  than the one the mirror's channel holds (by the channel's publication number: the mirror's
  channel never goes back) — unless `--older`, for a channel that went back where it is
  published (a yank moves it to the release before); whose channel holds another project's
  release, or another channel's; or whose release, that same publication, the mirror records as
  yanked (a rebuild of its tag is another publication). It logs in with the Docker client's own login
  for that registry (`docker login`, `DOCKER_CONFIG`) and trusts `--registry-ca` besides the
  system's CAs. It prints `Imported <file> into <prefix>` with the archive's manifests,
  blobs and bytes. Importing again uploads no blob the registry holds already.
- Then each shape reads the mirror: `<short>-stow --registry PREFIX --registry-ca FILE` (Docker),
  `<short>-stow k8s install --image-registry PREFIX --registry-ca FILE` (Kubernetes) and `<short>-stow
  vm create --registry PREFIX --registry-ca FILE` (the Kairos VM). A newer release crosses the
  same way; `upgrade --version`, `updates` and the VM's `upgrade` find it in the mirror.
- `vm create --create opentofu` also needs its module's OpenTofu provider, which `tofu init`
  fetches from the OpenTofu registry: on a site without internet, give `tofu` a provider mirror
  (a `provider_installation { filesystem_mirror { … } }` block in a CLI configuration file named
  by `TF_CLI_CONFIG_FILE`, filled by `tofu providers mirror` on a machine with internet).

### Creating a VM (`<short>-stow vm create`)

A project with the `kairos` shape whose release publishes disk images gets a `vm create` command
in the same installer (bootstrap it as above, then `sh <name>.sh vm create …`, or run the kept
`<short>-stow`). It writes what a new VM of that release needs, then prints the platform's steps:

```bash
<short>-stow vm create [--platform P] [--out DIR] [--hostname H] [--user U] [--ssh-key FILE]
          [--network dhcp|static] [--address A/P] [--gateway G] [--dns D,…]
          [--ipv6 [--address6 A/P] [--gateway6 G]]
          [--ssh-allow CIDR,…] [--timezone ZONE] [--secrets FILE|-]
          [--tls MODE] [--domains route=host,…] [--http-port N] [--https-port N]
          [--set KEY=VALUE]… [--answers FILE] [--no-input] [--accessible]
          [--create opentofu|manual] [--module DIR] [--tofu-var NAME=VALUE]…
          [--vm-name N] [--pool P] [--dataset D] [--bridge B]
          [--cpus N] [--memory MiB] [--disk GiB] [--host H] [--wait DURATION]
          [--registry PREFIX] [--registry-ca FILE]
<short>-stow vm destroy HOSTNAME [--yes] [--no-input] [--accessible]
```

- A project whose front offers no mode a VM can take (only `none` and `byo`) is refused first.
- The platform decides the disk format (the first of its formats the release publishes) and how
  the cloud-config reaches the VM. Only the platforms the release has a disk for are offered:

  | `--platform` | disk | cloud-config |
  |---|---|---|
  | `truenas`, `proxmox`, `libvirt` | `qcow2`, else `raw` | `<hostname>-seed.iso` |
  | `vmware` | `ova` | `<hostname>-seed.iso` |
  | `hyperv` | `vhd` | `<hostname>-seed.iso` |
  | `azure` | `vhd` | `<hostname>-user-data.yaml` (custom data) |
  | `gcp` | `gce` | `<hostname>-user-data.yaml` (`user-data` metadata) |
  | `aws` | `raw` | `<hostname>-user-data.yaml` (user data) |

  The seed ISO is a NoCloud seed (ISO 9660 with Rock Ridge and Joliet names, labelled `cidata`,
  holding `user-data` and `meta-data` — `instance-id` and `local-hostname`, the host name), which
  Kairos reads from the VM's CD-ROM at its first boot; a cloud's user-data is read from its
  metadata service. Both files, and the disk, are written 0644 (a hypervisor reads them as its
  own user; they carry no secret). A seed that exists already is never replaced (it may be
  attached to a VM): the run is refused before anything is asked to be written.
- Then the machine: its host name (default the project's name; lowercase labels of letters,
  digits and `-`), its administrator (default your user name when the VM may take it, else
  `sysadmin`; never `root`, `admin`, `kairos`, a name starting with `_` or `systemd-`, or another
  account of the OS such as `www-data` or `nobody`; the base's own user, `ubuntu`, may be taken), the administrator's SSH
  public key file (default `~/.ssh/id_ed25519.pub`, `id_ecdsa.pub` or `id_rsa.pub`; one key of
  a type OpenSSH takes), and its network: DHCP, or a static `--address` with its prefix, an
  optional `--gateway` and `--dns` servers; IPv6, off unless `--ipv6` (off, the VM's link has no
  IPv6 at all); with it, in either mode, router advertisements and DHCPv6 address the VM, and an
  optional static IPv6 `--address6` with its prefix and its `--gateway6` (asked with the static
  mode's questions when `--ipv6` is given, empty by default) sit beside them; `--address6` or
  `--gateway6` without `--ipv6` is refused, as is an IPv6 `--dns` server; `--ipv6` also turns IPv6
  on for the install's network (`install --ipv6`, recorded in the settings); the whole
  addressing, IPv4 and IPv6, is checked by the rules `network set` follows (below), a refusal
  starting "the VM's network:"; `--ssh-allow` (CIDR, repeatable or comma-separated,
  IPv4 or IPv6; each masked to its prefix) the sources SSH accepts — without it SSH accepts every
  source, and the run warns —, and `--timezone` an IANA zone name (`Europe/Paris`; letters,
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
  secret input it holds is not named as due. Then the product, as the VM's `install` asks it: the
  TLS mode (an HTTPS one: never `none`, whose loopback address nothing outside the VM reaches,
  nor `byo`, whose private key the cloud-config never carries), the domains and the inputs of the
  `kairos` shape — never from the environment, which says nothing about the VM — but no secret
  input: one given is refused, and one the install
  requires is named at the end, for the operator to give over SSH (`ssh <user>@<address> sudo
  <short>-stow install`). The answers are reviewed ("Go on with these answers"), the whole is shown,
  and on a terminal confirmed; without one the flags and the defaults decide. The whole names the
  ports the VM opens to each `l4` route (`opens tcp/15432 (L4 route db) to every source`, or `to
  the sources of <its allow_input>`) and the host devices each service maps (`devices:`, its own
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
  `vm create --create opentofu` waits 40 minutes by default then, rather than 20.
- It fetches that disk of the format (`<prefix>/<name>-disk@<digest>`, the digest the
  release records) with the pull account, as `upgrade --version` reads the registry, into
  `--out` (default the current directory) under the layer's title less its `.zst`
  (`<name>-<version>.<format>`), decompressed as it is written, checking the compressed layer's
  size and digest and the disk file's own digest; a file of that name holding that disk is kept,
  any other is refused, never replaced. Progress (of the compressed bytes) is reported every tenth. With `--registry
  PREFIX` (a mirror, checked as a repository name) the disk is read there instead, with the
  Docker client's login, and `--registry-ca FILE` (certificates only) is trusted for that read;
  the review shows the registry and the CA file.
- The cloud-config (`#cloud-config`, Kairos's stages, applied at every boot) sets the host name
  in the `initramfs` stage, before the network starts (a DHCP request carries it, so a DHCP
  server that registers names registers it); then, in `boot`, creates the administrator with the SSH key and no password, then, in a step of its own,
  puts them in the `admin` group (`usermod`, which also covers a user the base already has:
  passwordless `sudo`, the host daemon's socket), and writes the console's connect line,
  `/etc/issue.d/07-<name>-connect.issue` (`connect: ssh <user>@<address>, then `<short>-stow
  status``, and with `--ipv6` a second line, `or ssh <user>@<IPv6 address>`, agetty's `\6`);
  writes the VM's network, DHCP included, as
  `/etc/systemd/network/05-<name>-static.network` (`Name=en* eth*`, sorted before Kairos's DHCP file,
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
  `<short>-stow`, the host daemon and `kairos-agent upgrade` trust the mirror. It carries no secret.
- **With OpenTofu** (`--create opentofu`). `vm create` then creates the VM itself instead of
  printing steps, with a module: the platform's built-in one, or the operator's (`--module DIR`).
  The built-in modules, each pinning its provider's release exactly, with its lock file (the
  provider's checksums for Linux and macOS, amd64 and arm64: `tofu init` refuses a provider binary
  that does not match):

  | `--platform` | provider | what the module makes | the module's own variables (`--tofu-var`, or asked) |
  |---|---|---|---|
  | `truenas` | `pjsalty/truenas` 3.1.0 | below | its flags: `--pool`, `--dataset`, `--bridge` |
  | `libvirt` (preview) | `dmacvicar/libvirt` 0.9.9 | the image uploaded as a volume, a copy-on-write qcow2 volume over it grown to `--disk`; the seed as a second, read-only volume; a KVM domain with libvirt's UEFI firmware (Secure Boot off) on `network`, its DHCP lease of an IPv4 address the address (waited for, up to 5 minutes, only with DHCP) | `uri` (default `qemu:///system`), `pool` (`default`), `network` (`default`) |
  | `proxmox` (preview) | `bpg/proxmox` 0.117.0 | the image uploaded to a datastore with *Import* content (not enabled by default: Datacenter > Storage), the seed to one with *ISO* content, both through the API (the token alone, no SSH to the node); an OVMF VM importing the image, grown to `--disk`, the seed on its CD-ROM; no address (no guest agent) | `node` (required), `datastore` (`local-lvm`), `import_datastore` (`local`), `iso_datastore` (`local`), `bridge` (`vmbr0`) |
  | `vmware` (preview) | `vmware/vsphere` 2.17.1 | the OVA deployed, its disk grown to `--disk`, the seed uploaded to the datastore on its CD-ROM; its address when VMware tools report one | `datacenter`, `cluster`, `datastore`, `host` (required), `network` (`VM Network`), `folder` |
  | `aws` (preview) | `hashicorp/aws` 6.69.1 | the raw image uploaded to S3, imported as a snapshot (the `vmimport` role), registered as a UEFI image, an instance launched from it with the cloud-config as its user data (IMDSv2); its public address, else its private one | `region`, `bucket`, `subnet_id` (required), `security_group_ids` (comma-separated), `instance_type` (`t3.medium`), `vmimport_role` (`vmimport`) |
  | `gcp` (preview) | `hashicorp/google` 8.7.0 | the image's `tar.gz` uploaded to Cloud Storage, a UEFI image, an instance booted from it with the cloud-config as its `user-data`; its external address | `project`, `zone`, `bucket` (required), `network` (`default`), `subnetwork`, `machine_type` (`e2-medium`) |
  | `azure` (preview) | `hashicorp/azurerm` 5.10.0 | the fixed VHD uploaded as a page blob, a generation 2 image, a VM with a static public address made from it with the cloud-config as its custom data (no VM agent); its public address | `resource_group`, `location`, `storage_account`, `subnet_id` (required), `container` (`vhds`), `vm_size` (`Standard_B2s`) |

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
    `description`, `disk_gib`, `image` (the disk on this machine, in the `--platform`'s format),
    `image_sha256`, `seed` (the NoCloud seed ISO on a hypervisor, the cloud-config file on a cloud:
    `--platform` decides) and `seed_sha256`, refused before anything is fetched when one is
    missing. It may declare `cpus` and `memory_mib` (then asked and given `--cpus` and `--memory`;
    a module that declares neither sizes the VM by a variable of its own, as the clouds' instance
    types do, and those flags are refused: `--cpus: the module sizes the VM by a variable of its
    own`), `image_format` (the format of `image`, the first of the `--platform`'s formats the
    release publishes: `qcow2` before `raw` on `truenas`, `proxmox` and `libvirt`), `dhcp` (true when the VM takes its IPv4 address by DHCP, false with `--network
    static`), `admin_user` and `ssh_public_key` (given `--user` and the `--ssh-key` file's key). Its
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
    in MiB (default 4096, at least 1024) and `--disk` in GiB (default 40, at least 16; the OS
    grows into it at first boot), then elsewhere the module's own variables. The review shows
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
    cloud), OpenTofu's `terraform.tfstate`, and `known_hosts` (the VM's SSH host key). A host name
    whose directory holds the variables (an apply ran, or was tried) is refused: `vm destroy` it
    first, or choose another `--hostname`. So is a `--vm-name` another VM there already has,
    whatever its case (on TrueNAS its zvol and staging dataset would be the same).
  - `tofu` runs in a session of its own: interrupted (Ctrl-C, or the run stopped), it is sent one
    interrupt and given 5 minutes to record what it made; a second Ctrl-C is passed on (OpenTofu
    then stops at once), and after 5 minutes it and what it started are killed. Interrupted
    while it waits for the VM, `vm create` says the VM is kept and names `vm destroy`.
  - It runs `tofu init`, `tofu apply -auto-approve` and `tofu output -json` there
    (`TF_IN_AUTOMATION=1`, `TF_INPUT=0`, and `STOWAGE_CTL` set to the running `<short>-stow`, which
    the module's steps run). TrueNAS's module creates a staging dataset `<vm-name>-staging` and a zvol `<dataset>/<vm-name>` (16K blocks,
    LZ4), uploads the disk image (checked against its SHA-256 first; an upload already there at
    its size is not sent again) and converts it into the zvol, uploads the seed, creates the VM
    (UEFI, host CPU, `--cpus` cores, autostart) with the disk, a VirtIO NIC whose MAC is derived
    from the VM's name (the same at every start, so a DHCP lease follows it) and the seed as its
    CD-ROM, and starts it. A changed disk or seed rebuilds what depends on it, powering the VM off
    and on. A failed apply keeps the state, and the error names `vm destroy`.
  - It then waits, up to `--wait` (default 20m), polling every 10 s over `ssh` (batch mode, the
    private key next to `--ssh-key`'s `.pub` when it exists) at `--host` (an address or a
    host name; an IPv6 address bracketed or not, `[2001:db8::7]` taken as `2001:db8::7`; default
    the static IPv4 address, else the module's `address`, else the host name — with DHCP and a static IPv6 address alone,
    `--host` gives that address when this machine reaches the VM over it; a DHCP server must
    register it) until `<short>-stow status
    --json` there answers from a system that is not Kairos's recovery (a first boot runs the
    recovery system, then resets and reboots into the active one) and reports the install
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
    `/run/systemd/system`) waits for nothing). With `--secrets` it waits until the VM answers, then runs `sudo systemctl start
    <name>.service && sudo <short>-stow install --secrets - --no-input` there over the same SSH (the
    boot unit first: it runs once the release's images are loaded), the file on its standard input (a secret
    input still due: it prints how to give both instead — `chmod 600 FILE; scp -p FILE
    <user>@<host>:secrets.yaml` (an IPv6 host bracketed, `<user>@[2001:db8::7]:secrets.yaml`),
    then `ssh -t <user>@<host> 'sudo <short>-stow install --secrets secrets.yaml; rm
    secrets.yaml'`, on a terminal that asks the rest); a failure keeps the VM and says that
    `status` tells where it stopped, a re-run of `sudo <short>-stow install` finishes an install
    begun, and those steps make one that did not begin. By hand, the steps end with them. The recovery and active systems have different SSH host keys: each attempt takes
    the key on first contact into a file of its own, and the VM's `known_hosts` keeps the key of
    the attempt that answered. A VM not ready in time is kept; the error says so.
  - It prints `ssh <user>@<host> <short>-stow status` and where the state is.
- **`vm destroy HOSTNAME`** destroys a VM `vm create --create opentofu` made: `tofu destroy` of its
  module (the VM, its disk and data, its uploads), then its state directory. It needs the same
  environment and `tofu` (the TrueNAS API's variables for TrueNAS's built-in module, as its
  `platform` file records; a VM made before that file is TrueNAS's), asks on a terminal ("Destroy
  the VM H: its disk, its data and its uploads on <platform>", default no), and without one refuses
  unless `--yes`. On TrueNAS, snapshots of the zvol and the staging dataset (a periodic task's) are
  deleted first, as TrueNAS refuses to delete a dataset that has any. The cached disk stays.
- `vm truenas import-image|seed|start|stop|drop-snapshots` (hidden) are the module's steps, run
  by OpenTofu: each reads `TRUENAS_URL`, `TRUENAS_API_KEY`, `TRUENAS_INSECURE_SKIP_VERIFY` and its
  own variables (`IMAGE`, `IMAGE_SHA256`, `STAGING_DIR`, `ZVOL`; `SEED`, `SEED_PATH`; `VM_ID`;
  `DATASET`) from the environment, and talks to TrueNAS's JSON-RPC API (`/api/current`) and its
  `/_upload/` endpoint.

### The installer on the Kairos VM (`<short>-stow` in the OS bundle)

The OS bundle carries `/usr/sbin/<short>-stow`: the installer with the release as the bundle carries
it — the manifest without its publication (no channel, tag, number, date or artifact beyond the
images), the compose file, the runtime spec, the trusted keys and the pull account — so one
bundle serves every channel it is promoted to. On the VM it runs as root or for the `admin`
group, installs into `/usr/local/<name>` (the persistent partition, which an A/B upgrade keeps)
and never pulls: it runs the images the bundle preloads, by their digest tag
(`<prefix>/<name>-<key>:sha256-<hex>`), each checked against the IDs the release records.

```bash
sudo <short>-stow install [--channel C] [--set KEY=VALUE]… [--answers FILE] [--no-input] [--accessible]
          [--ports route=port,…] [--address 127.x.x.x] [--tls MODE] [--domains route=host,…]
          [--cert F --key F [--ca F]] [--http-port N] [--https-port N] [--no-provision] [--dry-run]
          [--secrets FILE|- | --new-secrets]   (both: refused)
<short>-stow status [--json]          # this OS's release, the install's (and `not finished` while a first install's stack comes up or its provision hook runs, or after one failed — with why, and the `sudo <short>-stow install` that runs it again; `"installing": true` and `"install_failed": "<why>"` in JSON; `not provisioned` while an install given `--secrets` awaits the backup `restore` loads, `"restore_awaited": true`; `not running` while a restore left the volumes half restored, `"volumes_split": true`), the channel, the system Kairos booted (active, passive or recovery; the fallback), the last failed boot, the disk, a byo install's certificate (`cert`: its names, end and days left, with a warning from 30 days before it; `"cert"` in JSON), the services
<short>-stow health [--json]          # ready, or fails saying why: the health gate's check, once
<short>-stow events [--after N] [--follow|-f] [--json]   # the event journal: jobs, reboots, boots and the health gate's verdicts, the first install; --follow waits for more until interrupted, through daemon restarts
<short>-stow ps [--json]              # the stack's services, state and health
<short>-stow logs SERVICE [--tail N]  # 1 to 5000 lines, default 200
<short>-stow restart|stop|start SERVICE
<short>-stow provision [--set KEY=VALUE]…   # the provision hook again; --set as on Docker (an input only it reads, or a value it returned or declares it returns; empty forgets it)
<short>-stow updates [--channel C] [--version V] [--json]
<short>-stow upgrade [--channel C] [--version V] [--yes] [--no-reboot] [--skip-backup] [--accessible]   # ends at the reboot it schedules: status and events after it tell how it went
<short>-stow rollback [--yes] [--accessible]   # reboot into the passive system: back to the release the last upgrade replaced; ends at that reboot
<short>-stow reset-db [--yes] [--no-provision] [--accessible]   # wipe the data, start again as first installed
<short>-stow restore OBJECT [--identity FILE|-] [--yes] [--accessible]   # load a backup into the database
<short>-stow secrets export --recipient age1…… [-o FILE|-]   # the install's secrets, encrypted
<short>-stow secrets rotate KEY [--stage | --finish]   # a secret with rotation: true
<short>-stow network [--json]          # the address, gateway and DNS servers, what sets them, a change awaiting confirmation
<short>-stow network set --mode dhcp|static [--address A/P] [--gateway G] [--ipv6 [--address6 A/P] [--gateway6 G]] [--dns D,…] [--revert-secs N | --no-revert]
<short>-stow network confirm|revert   # keep the change awaiting confirmation, or undo it now
<short>-stow network reset [--revert-secs N | --no-revert]   # drop the override: back to the cloud-config's network
<short>-stow backup config [--json] [--destination REMOTE:PATH] [--rclone-config FILE|-] [--recipient age1…]…
          [--retention-days N] [--schedule CALENDAR] [--profile P] [--remove]   # show, change or drop the backup settings
<short>-stow backup run [--profile P] [--if-configured]   # back up now, and wait for it
<short>-stow backup status|list [--json]   # the settings, the next and the last backup; the destination's backups
<short>-stow backup verify [--destination REMOTE:PATH] [--rclone-config FILE|-] [--recipient age1…]…   # write a probe file, as a job it waits for; the flags try settings unsaved
<short>-stow reboot|shutdown [--drain-secs N]   # 0 to 600, default 30
```

- `install` is the Docker shape's first install (the same flags, form, checks and refusals) from
  the preloaded images, asking the inputs of the `kairos` shape (`inputs[].shapes`); its re-run
  is the Docker shape's too, moving between TLS modes and taking a new certificate (`--tls`,
  `--cert`, `--key`, and `--ca` for a project with `front.publish_ca`), and `--secrets` makes a
  first install with exported secrets (see `secrets export` under the Docker shape). A first
  install takes the settings file `/etc/<name>/install.yaml` (written by the cloud-config `vm
  create` makes; YAML: `channel`, `tls`, `http_port`, `https_port`, `inputs` of KEY: value,
  `await_secrets`, `ipv6`, `release` (see `vm create`: the release whose OS a first boot takes first), and `registry` and `registry_ca` — a mirror and the file of its CA, taken as `--registry` and
  `--registry-ca`, so the install records them and its updates and upgrades read the mirror,
  with root's Docker login on the VM, anonymously when it has none —; any other key refused) for what its flags and `--answers` do not give (its `tls` is not taken when
  `--cert` or `--key` is given, nor when its `tls` is `none` and a domain is given, by
  `--domains`, `--set` or `--answers`; with `--tls none`, its domains are not taken); `tls: byo` and a secret
  input in it are refused (they are given over SSH). `--channel` (a channel name, as `lint` takes them) is recorded as
  `STOWAGE_CHANNEL`, the channel `updates` and `upgrade` follow, at the first install only (a
  re-run given it is refused, as with the other first-install flags; `--channel` on `updates` and
  `upgrade` names another for one run). A second run is a re-run, as on Docker: over SSH,
  `sudo <short>-stow install --set KEY=VALUE` (or `--answers`, `--domains`, `--ports`, `--http-port`,
  `--https-port`) changes what the install recorded and brings the stack up again; the settings
  file is read by the first install only, and every boot keeps what the install records.
- At every boot `<name>.service` runs `<short>-stow boot` (after Docker, `<name>-preload.service`
  and the cloud-config's boot and network stages, `cos-setup-boot.service` and
  `cos-setup-network.service`). With no install, it makes the first one from the settings file,
  with nothing asked; without that file, while a secret input the install requires has no
  value, or when the file says `await_secrets: true` (`vm create --secrets`), it does nothing,
  saying that `sudo <short>-stow install` (`--secrets`) makes it — and an install without
  `--secrets` is then refused, as it would generate secrets the backups do not match, unless
  `--new-secrets` (the file lost) generates them (that run takes the
  settings file and asks the rest). An upgrade or a rollback a reboot stopped is finished first by
  the install's own `<short>-stow` (`.stowage-prev/` recording the release the install runs gives
  it away). Then an install of the OS's release comes up again; an install of
  the release the last upgrade replaced (Kairos booted the passive slot) goes back to it with the
  install's own `<short>-stow` — the newer release's — which first brings the database up, so the
  check reads the schema the newer release left, and refuses as `rollback` refuses (a rollback
  barrier, a release that does not read the newer schema) or when the database does not come up; an install that went back from the
  OS's release stays as it is, brought up again (no upgrade loop: upgrade to a later release, or
  remove `.stowage-failed/` to try that one again); any other install is upgraded to the
  OS's release as `upgrade` does (its own `<short>-stow` brings its stack up first, then the database
  is dumped to `snapshots/`, the replaced release is kept in `.stowage-prev/`, and the install
  keeps the OS's `<short>-stow`). The install's own `<short>-stow` is run only when it carries the
  release the install runs; otherwise the boot fails, naming it. It waits up to 30 minutes.
  `boot` and `boot-rollback` (the install's own `<short>-stow` going back, or finishing a stopped
  run without bringing the database up) are the boot's verbs, hidden from the help, and so is
  `health-gate`. Hidden flags:
  `--dir` (default `/usr/local/<name>`) on `install`, `boot`, `boot-rollback`, `health-gate` and `hostd`;
  `--settings` (default `/etc/<name>/install.yaml`) on `install` and `boot`;
  `--socket` (default `/run/<name>/host.sock`) on every verb — the socket a verb asks, and the one
  `hostd` serves on.
- The health gate: after the boot, `<name>-health.service` runs `<short>-stow health-gate` (as
  root, hidden from the help), which judges the boot by Kairos's boot assessment (GRUB images).
  An upgrade enables the assessment; GRUB, booting the active system with it enabled, marks the
  attempt (`enable_boot_assessment` and `boot_assessment_tentative`, both `yes`, in
  `/run/initramfs/cos-state/boot_assessment`), and a boot that finds the attempt marked boots the
  fallback entry — the passive system — with `upgrade_failure` on its command line
  (`/run/cos/upgrade_failure`). A boot of the active system with both set is on trial. The gate
  polls every `poll_s` until `deadline_s` (`shapes.kairos.health_gate`): the boot is healthy when
  `<name>.service` did not fail, the install runs the release this OS carries, and it is ready (as
  `health` judges). Healthy, a boot on trial is marked good (both cleared, the partition
  remounted read-write meanwhile); a failure to mark it fails the gate, as the next boot would
  fall back. Not healthy by the deadline (at once when `<name>.service` failed), the gate keeps
  diagnostics in `/usr/local/<name>/failed-boots/<UTC time>-<version>-r<revision>/`
  (`failed-boot.json` — the release, the time, why —, `boot.log`, the boot unit's journal of this
  boot, `ps.txt` and `logs-<service>.txt`, each service's last 500 lines; the last 5 are kept)
  and fails. On trial, it first runs the rollback the passive system's boot would run, dry: when
  the database refuses it (a rollback barrier since the upgrade, or a previous release that does
  not read the newer schema), falling back would leave a release that cannot start, so the gate
  keeps this system — marks the boot good, records why in `failed-boot.json` (`kept`), and
  `status` shows it under the failed boot (`kept on this system, as going back is refused: …`)
  — and does not reboot. Otherwise a boot on trial reboots, unless there is no passive system
  (`/run/initramfs/cos-state/cOS/passive.img`), and Kairos boots the passive system, whose boot
  goes back to the release the upgrade replaced. Off trial (a boot of the passive or recovery
  system, a boot after no upgrade, or an attempt GRUB did not mark) nothing reboots, so a failure
  never loops. A reboot before the gate marks a boot on trial good is a failed attempt too: the
  next boot falls back. The fallback sticks: every later boot boots the passive system, until an
  upgrade.
  With no install yet, the gate judges nothing. Each judgement is a `boot.judged` entry of the
  event journal (below).
- Every other verb asks the host daemon, `<name>-hostd.service` (`<short>-stow hostd`, root, hidden
  from the help; its hidden `--group`, default `admin`, names the socket's group, empty for root
  alone), on the unix socket `/run/<name>/host.sock`: the socket is `root:admin` 0660 in a 0750
  directory (`root` alone, 0600, on a host without that group, which the bundle creates). Until
  `<name>.service` ran (its state is `active` or `failed`: before, the boot is queued, or is
  installing, upgrading or going back; a host without that unit has no boot to wait for), `upgrade`, `rollback`, `provision` and `restart`/`stop`/`start` are
  refused; `upgrade` and `rollback` also until `<name>-health.service` judged the boot (its state
  is `active` or `failed`). `restart`/`stop`/`start`, `reboot` and `shutdown` are refused while an install,
  upgrade, rollback or provision holds the install's lock — but `reboot` and `shutdown` are not
  while the boot runs, so a hung boot can be power-cycled (the next boot finishes what it
  stopped). The boot waits up to 10 minutes for an operator's run to release the lock. A verb
  needing the install answers that `sudo <short>-stow install` makes one until it exists.
- `updates` reads the channel's pointer in the install's registry (`--version V`: V's latest
  revision on the channel) with the pull account, refuses it unless it is signed by a key the
  OS's `<short>-stow` carries (when it carries any), and says whether it comes after the OS's
  release (by the scheme the release records; revisions order one version), with its changelog;
  a release the install went back from (`.stowage-failed/`) is not offered.
- `upgrade` shows the same, asks to confirm on a terminal (without one, `--yes` is required),
  then the daemon runs `kairos-agent upgrade --source oci:<prefix>/<name>-os@<digest>` — the
  release's bundle by digest, pulled with the pull account given in a private Docker config, root's
  alone, removed afterwards — writing it to the passive slot, and reboots after 30 s
  (`--no-reboot`: the next boot runs it); the answer names the job's target. The job ends, `done`,
  as it schedules the reboot; a reboot that then fails leaves the job `done`, adds the failure to
  its log, and is journaled as a `reboot.cancelled` naming the job. The boot then upgrades
  the install. The job writes `/run/<name>/os-written` (the target's tag; a reboot removes it)
  before `kairos-agent` runs, and keeps it when the job fails (`kairos-agent` may have moved the
  running system to the passive slot already): until a reboot, another `upgrade` and `rollback`
  are refused (another upgrade would replace the running system's copy in the passive slot), and
  so are they when the mark cannot be read. When the boot assessment still marks an attempt (after a fallback or a
  `rollback`), the job clears the mark once the new system is written (`boot_assessment_tentative`
  emptied), so the next boot tries it on trial; a failure to clear it fails the job, without a
  reboot. It is refused for a
  yanked release, one that does not come after the OS's, one published without an OS bundle, one
  the install went back from, and one other than the release confirmed (published meanwhile).
  `<short>-stow` prints the job's log as it runs (a carriage return ends a line too, a line is cut
  every 4 KiB, a blank line is kept; the daemon keeps at least a job's last 2000 lines), and fails
  when the job fails or when the daemon's job is another (`status` then tells how it went). Every
  verb that starts a job (`upgrade`, `rollback`, `reset-db`, `backup run`, `backup verify`,
  `restore`, `secrets rotate`) follows it this way, telling it from any other by the `started`
  its 202 carries — another job of the same kind and target is another job. Between two reads of
  the log it waits on the event journal from the 202's `events`, so a job's end is seen at once,
  and read from the journal when reading the job again then fails in any way (the daemon gone,
  an error answer, an answer that does not decode — the daemon stopping for the job's reboot), or
  when the next job (the timer's backup, woken by that end) took the slot before it: a job done
  answers as done (with its `result`), a job failed fails with its error, and the verb says `the
  <kind>'s last log lines were not read: its end is the journal's (<short>-stow events)`. A
  `job.lost` read is likewise the answer, whatever reading the job again then meets. A job the daemon
  no longer holds and whose end the journal does not hold fails the verb: `<kind> <target> lost: <reason>` when the journal's `job.lost`
  names it (the daemon restarted), else naming the job the daemon holds, or none. A job that ends
  `done` having scheduled its reboot or power-off (an upgrade's, a rollback's: `GET job`'s, or its
  `job.ended`'s, `reboot`) ends the verb there — the verb runs on the VM, which that reboot
  stops, so it never follows the reboot. It prints `<kind> done: the VM reboots in <drain> s to
  run it — after it, <short>-stow status and <short>-stow events tell how it went`, exits 0 and asks the
  daemon nothing more. The drain is counted down only when `GET job` told the end and the drain
  is not 0 (a rollback's is): otherwise the line says `the VM reboots to run it`, whatever the
  next request met (the daemon's absence never tells a reboot under way). A power-off says
  `the VM powers off in <drain> s, and its next start runs it — after that start, …` (`powers
  off` likewise). The reboot's outcome is the event journal's: another boot's
  `boot.started`, a `reboot.cancelled` naming the job (`systemctl reboot` failed, or the daemon
  restarted in the drain), the health gate's `boot.judged` — on the VM, read after the boot with
  `<short>-stow status` and `<short>-stow events` (the journal keeps its entries across the reboot). The
  host API is served only on the VM (its operator socket, and the app socket mounted into the
  stack's services `host_api` names): such a service, and what it serves to clients off the VM,
  reads `GET /v1/events` again once the VM is back, from the number it held. Each request the follow sends is given its wait and 2 minutes to be
  answered, then counts as the daemon not answering, so a daemon that takes a request and never
  answers cannot hold a verb. A verb stopped by a signal answers from the job's end when it read
  it: a job failed fails the verb with its error, a job done exits 0 (with the line above when it
  scheduled a reboot); a job lost it read fails with its reason. Otherwise, interrupted (Ctrl-C,
  SIGTERM, SIGHUP, SIGQUIT), a verb fails (`interrupted before …`) and never reports the job as
  done: `status` and `events` tell how it went. So a job-following verb exits 0 when its job ends
  `done` (a reboot it scheduled included), and non-zero when the job failed or was lost; when
  the daemon holds another job or none and the journal does not hold the job's end; when, before
  the job's end or loss was read, the daemon cannot be reached, a request's bound runs out, or
  `GET job` answers an error or an answer that does not decode; when `GET job` answers a state
  other than `running`, `done` or `failed` (`the job is <state>`); or when the follow was
  interrupted. A 202 whose `events` is `null` (its start could not be journaled) is followed by
  the job's log alone, never the journal, which the verb says first (`the event journal does not
  hold this <kind>'s start (<events_error>): …`): a job's end is then only `GET job`'s, and a
  daemon gone during the job fails the verb.
- `rollback` goes back to the release the last upgrade replaced by booting the passive system,
  which carries it: the daemon checks that the VM runs the active system (refused on the passive
  one: there is no earlier one, and on recovery), that the install runs the release this OS
  carries, that a passive system and a previous release (`.stowage-prev/`) are kept, that no
  upgrade began to write a system no boot ran yet (`--no-reboot`, or an upgrade that failed:
  the running system may be the passive one now; see `os-written` under `upgrade`), and runs the
  install's rollback dry (`rollback --dry-run`'s checks: refused when the database carries a
  rollback barrier since the upgrade, or moved past a release that does not read a newer schema,
  naming the snapshot to restore instead). `<short>-stow` shows the release the passive system
  carries, asks to confirm on a terminal (without one, `--yes` is required) and follows the job:
  after 30 s it marks the boot assessment as after a failed attempt (both variables `yes`), so
  GRUB boots the fallback entry from then on, and runs `systemctl reboot`; the boot of the
  passive system takes the install back. When the marks cannot be written, they are put back as
  they were and the job fails. Once they are written the job ends, `done`, before `systemctl
  reboot` runs; when the reboot then fails, the job stays `done` and the marks are put back as
  they were. The journal's `reboot.cancelled` names the job, and its `reason`, like the job's log,
  says whether putting them back worked: `systemctl reboot failed: <error>: the boot assessment is
  put back as it was, nothing changed`, or `systemctl reboot failed: <error>; putting the boot
  assessment back failed too, so the next boot may choose the passive system (<grubenv>):
  <error>`. `rollback` itself answers from the job's end (exit 0 once it read it), so this is read
  with `<short>-stow events`. After the 30 s, the checks run again
  and the install's lock is taken and held to the reboot: a check that no longer passes, or an
  install, upgrade, rollback, provision, reset-db or service action holding the lock, fails the job
  before anything is written. It is refused for a release other than the one confirmed.
- `health` answers whether the install is ready, as the health gate judges it (every service up,
  the health provider's answer); not ready, it fails with the reason. A probe of a service (the
  `http-json` and `http-status` health providers, the disk-pressure POST) reaches it at its
  address — IPv4, else IPv6 (a network without IPv4) — on the first network by name that gives
  it one and is not `internal: true` (the host
  has no route into an internal network); a service on internal networks alone is not ready.
- `secrets rotate` is the Docker shape's three passes, run by the daemon as a job (`rotate`)
  once the health gate judged the boot (its restarts could fail the gate's deadline); `<short>-stow`
  follows it. `secrets export` is the Docker shape's, the daemon writing the encrypted file it
  answers; both refuse (409) an install running another release than the OS's, whose spec
  names the secrets.
- `restore OBJECT` loads a backup into the install's database (the VM's destination and rclone
  config, so backups must be configured): `<short>-stow` reads the age identity that decrypts it
  from standard input (`--identity FILE` instead), asks to confirm on a terminal (without one,
  `--yes` is required) naming the release the install runs, which the request carries, and
  follows the job. For a backup with `volumes` it is refused (409, `reboot first, then restore`)
  while an upgrade's system waits in the passive slot for its boot. The daemon refuses (400, 409) a request without that release or with
  another, an install running another release than the OS's, an object not named as this
  project's backups are (`<name>-<profile>-<UTC time>-<version>-r<revision>.pgdump.age`, no
  path), a backup of another release (a backup restores onto the release whose schema it
  holds), a backup of a profile that does not keep every table's rows (a table list, or an
  `exclude`; one a later release dropped too: the restore replaces the whole database, and
  such a dump holds every table's structure, so the rows it left out would be lost —
  the refusal names the profiles to restore from), an identity that is not one (never echoed), and every verb's refusals until the health
  gate judged the boot. The job takes the install's lock, reads the install again (refusing
  another release, and, before anything is stopped, a provision or schema container an earlier
  run left), refuses, before anything is stopped, a backup missing one of its volumes' archives
  (`has no archive of the volume V to restore with it`), stops every service but the database
  (none to stop: nothing), extracts each volume's archive into a directory of its own at the
  volume's root (`.stowage-restore`, made afresh with the root's owner and mode: it and
  `.stowage-replaced` are names the stack's own data must not use, never extracted from an
  archive; a volume not there is made as the compose file declares it —
  driver, `driver_opts`, labels, and Compose's labels — refused when those hold a `$`),
  streams the object out of the destination (the backup runner's `rclone cat`), decrypts it as
  it streams, and loads it beside the database, never into it: as the migration role — which
  owns the database, may create one (`CREATEDB`) and `SET ROLE` to every owner the dump names, as
  a stack's own Postgres superuser does — through the maintenance database (`postgres`, or
  `template1` for a database named `postgres`), a new database `stowage_restore` (a fixed name)
  made as the stack's Postgres made the database (from `template0`, its owner, encoding and libc
  locale; nothing set at the database's level — `ALTER DATABASE … SET`, its grants — is carried,
  as a stack keeps its configuration in its schemas) takes the archive by `pg_restore
  --single-transaction --exit-on-error`, owners and privileges kept (every role the dump names
  must exist: a role made by hand on the old database is made again first, or the restore fails
  naming it). The load never touches the install's database: whatever ends it — a failure, the
  job stopped — the database is as it was (a load that fails drops its database; one left behind
  is dropped by the next restore, which says so). Only once it loaded, and while the job still
  waits, does a second step end the database's sessions (those the migration role may end) and
  swap the two by renaming them in one transaction (under a short timeout of its own, which
  neither the job's deadline nor the daemon's stop cuts, and bounded inside the database's
  container by Postgres — connect, lock and statement timeouts on every step, both legs together
  well within it — so nothing of it still runs when the job reads how it ended; any end but success is judged by whether
  `stowage_replaced`, which only its rename makes, exists: there, it committed and the restore
  goes on; if that cannot be read, the job removes the volumes' staging and says that a
  `stowage_replaced` means it committed, that it holds the data from before the restore, and to
  restore again, which loads and swaps afresh whether it committed or not, dropping it), then, once the stack is up again and provisioned, drop the replaced one (its own
  ten minutes, bounded the same way)
  (`stowage_replaced`; if that drop fails the restore is still done, says so, and the next
  restore drops it); a swap that fails drops the loaded copy (one the job could not see fail is left for the next
  restore to drop). A restore that ends before the swap removes the volumes' staging directories:
  their data is as it was. Once the database is swapped in, each volume's data is replaced by
  what its staging directory holds, by renames alone: the volume's entries move into
  `.stowage-replaced` (made afresh), then the staging's into the root, which takes the archive's
  owner and mode — each volume within ten minutes, every one tried. The volumes that could not
  be are the job's error, naming them (`restore again`): each holds its files split between its
  root, `.stowage-restore` (the backup's) and `.stowage-replaced` (its own), the services stay
  stopped (they would run, and be backed up, on data of two times) and the install still awaits
  a restore. From the database's swap until every volume is in, `<dir>/.stowage-volumes-split`
  marks the install (a swap whose outcome cannot be read keeps it too, with the staging): while
  it is there nothing brings the stack up, writes the data or moves the release — the boot, a
  re-run, `secrets rotate`, `start` and `restart` (409), `provision`, `reset-db`, `upgrade` and
  `rollback` refuse before changing anything, saying so; the stopped services' containers are
  removed (`compose rm --stop --force`), so no restart policy starts them at a reboot —, no backup is taken (409, an upgrade's first one
  failing), and `status` says `not running` (on Docker `… (or a restore is running)`; the VM's
  daemon leaves it out while its restore job runs). Only a restore again, onto the release the
  install runs, removes it once it finishes: it brings the database's service alone up first
  (`up --detach --wait --no-deps`), and any end short of finishing keeps the mark. Once the stack is up and provisioned, the `.stowage-replaced` directories are
  deleted, each within ten minutes, cut by the daemon's stop (one that is not is logged; the next
  restore's staging deletes it, and what an earlier restore left of `.stowage-restore`). An install that awaited a restore stops
  awaiting it only once the database and every volume are in. If the stack does not come up
  after a swap, the job says so and names `provision`. A database named `stowage_restore`, `stowage_replaced` or a template is
  refused. Nothing is dropped object by object, so partitioned tables, large objects and
  extensions come back as the dump holds them, and nothing the replaced database held stays. Then, whatever happened, the stack comes up again; after a
  restore that swapped the backup and every volume in, the install no longer awaits one (`.stowage-restore-awaited` goes) and
  the provision hook runs on the restored data (a failure there is the job's, naming `sudo
  <short>-stow provision`). The identity is kept nowhere.
- `reset-db` is the Docker shape's `reset-db` on the install, run by the daemon as a job without
  a reboot: the stack stops, the `data.reset` volumes are deleted (`keep_on_reset` ones and the
  front's certificates stay), the provision hook's record is forgotten, then the stack starts and
  is provisioned as on a first install (`--no-provision` skips the hook). `<short>-stow` asks to
  confirm on a terminal, as the Docker shape asks (without one, `--yes` is required), once the
  daemon's checks passed, and follows the job. The daemon refuses it (409) until the health gate judged the boot, when the install
  runs a release other than the OS's, during another job, and while a reboot is scheduled; the
  job fails on the Docker shape's refusals. The question names the release; the request names
  it, and another is refused, and carries the plan's single-use token. Nothing is dumped first. The app socket serves it too,
  as the whole API.
- `network` shows the interface (that of the default route, else the first `en*`/`eth*`), its
  MAC, its global IPv4 and IPv6 addresses (static, leased or from router advertisements; an IPv6
  one once duplicate address detection passed), the default gateways (IPv4's, then each IPv6
  default route's on that interface, static or advertised) and the DNS servers in use
  (`resolvectl`), and what sets them: `network set`'s override (`set by     \`<short>-stow
  network set\`:` and its settings — `DHCP` or the address via its gateway, then the IPv6
  address via its gateway, `automatic IPv6` or `no IPv6`, then the DNS servers), the network
  `vm create` gave (its cloud-config's file; with its settings, which the cloud-config also
  records in `/etc/<name>/install-network.yaml`, rewritten at every boot — a record that does not
  read is logged, once per error, and left out), or DHCP (a VM whose cloud-config wrote none).
- `network set` sets an override that outranks the cloud-config's network, as a whole (a setting
  left out is not kept from before): `--mode static` with
  `--address` (IPv4 with its prefix, 1 to 32) and an optional `--gateway`, or `--mode dhcp`
  (DHCPv4); `--ipv6` turns IPv6 on, in either mode — without it the link has no IPv6
  (`LinkLocalAddressing=no`, `IPv6AcceptRA=no`, and `DHCP=ipv4` with dhcp). With it, networkd
  takes the router advertisements (SLAAC and the IPv6 default route) and, with dhcp
  (`DHCP=yes`), DHCPv6, its client started at once rather than on a router advertisement's
  managed flag (`[DHCPv6] WithoutRA=solicit`): the clouds (AWS, Azure, Google Cloud) give the
  address by DHCPv6. Also with it,
  `--address6` (a static IPv6 address with its prefix, 1 to 128, beside the advertised ones)
  and an optional `--gateway6` (link-local, such as `fe80::1`, or in the address's subnet; /127
  and /128 reach any gateway on the link); `--dns` (at most 4, IPv4 or IPv6, duplicates dropped) in either mode, and with `dhcp` in place of
  the lease's; given in either mode, they replace DHCPv6's and the router advertisements' too. Refused before anything changes: another mode, an address or gateway with `dhcp`,
  no address with `static`, `--address6`, `--gateway6` or an IPv6 `--dns` server without
  `--ipv6`, an address with a zone, a loopback, link-local, multicast,
  unspecified or 240.0.0.0/4 address, an IPv6 `--address` or `--gateway`, an `--address6` that is
  not IPv6 (an IPv4-mapped one included), loopback, link-local or multicast, or up to /126 its
  subnet's anycast address (all zeros after the prefix), a `--gateway6` without `--address6`, not
  IPv6, with a zone, multicast, outside the subnet up to /126 (unless link-local) or equal to the
  address, up to /30 the subnet's network
  or broadcast address or a gateway outside the subnet (/31 and /32 reach their gateway on the
  link: networkd is given an on-link route to it), a gateway equal to the address, and a change while another awaits confirmation or once
  a reboot is scheduled. The override is recorded in `/usr/local/<name>/network.yaml` (0600, on
  the persistent partition, strictly read), rendered as
  `/etc/systemd/network/01-<name>-override.network` (`Name=en* eth*`; sorted first, so networkd
  takes it), then `networkctl reload` and `networkctl reconfigure` of the interface; with static
  addresses, each address (an IPv6 one past duplicate address detection: one another host has
  never does) and the default routes via the gateways must reach the interface within 10 s. A change that fails is undone at once and reported (the settings before it rendered and
  applied again).
- An addressing change (mode, `--ipv6`, an address or a gateway, IPv4 or IPv6; any change from no override) awaits
  confirmation: unless `network confirm` keeps it within `--revert-secs` (default 90, 30 to
  3600; a confirmation that comes once the window's timer fired is refused, 409, and the change
  undone; `network confirm` with nothing awaiting confirmation fails, saying that a change not
  confirmed in time is undone), the settings before it come back — at the window's end, when the host daemon restarts
  inside it (no network request runs before that; a record the daemon stopped before applying is
  applied then), and at a reboot inside it (`<name>-network.service` runs `<short>-stow network-boot`
  before networkd starts, rendering the override and undoing a change never confirmed). A
  DNS change alone, or `--no-revert`, applies with no window. `network revert` undoes the change
  awaiting confirmation now (refused when none does). A state file that does not read (unknown
  key, invalid setting) sets nothing at boot, fails `network` and `network set` naming the file,
  and `network reset` drops it, with no window. The file carries `version: 2` when IPv6 is off or a static IPv6
  address is set — in the override, or in the settings a change awaiting confirmation would bring
  back; without a version (IPv6 on everywhere, no static IPv6 address — the form a release older
  than `--ipv6` writes and reads) it is read with IPv6 on, and one holding an `ipv6`, `address6`
  or `gateway6` key, or a version other than 2, does not read. A release older than `--ipv6` reads an
  override with the version as such a file: after `rollback` to it, the
  whole override (its IPv4 addressing too) is dropped at boot and the cloud-config's network
  comes back — before rolling back to it, `network set --ipv6` without `--address6`, then `network confirm`
  (the settings before a change awaiting confirmation count too), whose record it reads. A revert that fails is tried again every
  15 s, 5 times in all; a later change stops it.
- `network reset` drops the override, back to the cloud-config's static address or DHCP, on the
  same window (the override comes back unless confirmed); with no override it changes nothing.
- The console's login screen (`/etc/issue.d`, after the host name and `IPv4: <address>`) tells
  what sets the network (`network: <the override's settings, as \`network\` prints them>; set
  with \`<short>-stow network set\``; else "the VM's cloud-config (DHCP, or its static
  address)") and, while a change awaits confirmation, when it goes back
  (`06-<name>-network.issue`, written at boot and at each change, then `agetty --reload`); the
  cloud-config adds the connect line.
- Disk pressure (`hooks.maintenance.disk_pressure`; the VM only): the daemon reads the use of the
  persistent partition (the filesystem of `/usr/local`, which holds the install and Docker's
  volumes: used of used and available, as `df` counts it, the blocks reserved for root in
  neither) as it starts and every 10 minutes — 1 minute after a check that could not read the
  disk, or whose POST was not sent for want of the install or the service's address (at boot the
  daemon starts before the stack is up), got no answer, or was answered 5xx. At
  `soft_pct` or past it — `soft`, or `hard` at `hard_pct` or past it — it POSTs
  `{"level": "soft", "used_pct": 80.3, "soft_pct": 75, "hard_pct": 90}` (`application/json`;
  `used_pct` rounded down to a tenth) to `post`, at the service's address on the stack's network
  with `<service>:<port>` as the `Host` (no proxy, no redirect followed; 30 s for the lookup and
  the request), with `Authorization: Bearer <value>` when
  `auth_secret` names a secret; below `soft_pct` it sends nothing. It sends nothing either before
  the first install, while the service has no container (the stack down, an upgrade), or when the
  install has no value for `auth_secret`. Each POST is logged to the daemon's journal (`journalctl -u <name>-hostd`), never the token. What to
  do about it (purge, shorter retention) is the service's. `status` prints `pressure   <level>:
  <used>% used (soft <soft_pct>%, hard <hard_pct>%)`, the use read now (`unknown: the disk cannot
  be read` when it cannot), and the last POST (its time, level and use, and the answer's status —
  with the first 512 bytes of a non-2xx answer, unprintable characters as spaces, when no
  `auth_secret` is set: with one, the status alone, as an answer may echo the token — or why none
  was sent, the token shown as `<auth_secret>` should an error quote it); `status --json` gives them as `disk_pressure`, beside `disk` (`total_bytes`,
  `available_bytes`, `used_bytes`). Past `hard_pct`, the
  console's login screen says so (`08-<name>-disk.issue`, then `agetty --reload`), until a check
  finds the use below it; a release without the hook removes that banner as the daemon starts.
- Backups (a `pg-age-rclone` `backup`): the daemon runs `pg_dump --format=custom --compress=9`
  (owners and privileges kept: the stack's roles exist on every install) as `database.roles.migrate` inside the database service (`docker
  compose exec`: the server's own version), encrypts the dump with age as it streams, to every
  recipient of the settings (any one's private key decrypts it; the VM holds none), and pipes it to
  the backup runner — rclone 1.75.1, preloaded, run with `docker run --rm --pull never --network host`
  (it reaches the destination, and names it, as the VM does — over IPv6 too), the settings' rclone config mounted from a directory under `/run/<name>/`
  (root's, removed afterwards) — which uploads it with `rclone rcat` to
  `<destination>/<name>-<profile>-<UTC time, 20261010T030000Z>-<version>-r<revision>.pgdump.age`;
  nothing is written to the VM's disk. Before the dump, each of the backup's `volumes` is
  archived the same way beside it, as `<the same name>.<volume>.tar.age`: a tar of the volume's
  files (owners and modes kept), read while the stack runs by the runner's own `tar`, the volume
  mounted read-only and the container on no network, a restore's `.stowage-restore` and
  `.stowage-replaced` left out
  (Docker's volume of the compose project, found by Compose's labels, else by the name Compose
  gives it when it carries no other project's labels — another project's is refused; one never
  created archives as empty). A volume's files are read live, unlike the dump's snapshot (a file changing as it is
  read fails the backup, which says so): the volumes suit what a
  stack writes once or rarely (minted keys and certificates, a service's own state between
  writes). The dump is the backup's last object: a backup that fails deletes every object it
  sent, and `backup list` lists the dumps alone. Bytes are the whole backup's. A profile listing tables keeps the data of those tables
  only (every table's schema is kept: the other tables' data is excluded with
  `--exclude-table-data`, quoted, so names are taken literally); a listed name no table has is
  logged. A profile `{ exclude: [pattern, …] }` keeps every table, leaving out the rows of the
  tables its patterns match (`--exclude-table-data=<pattern>`, pg_dump's own matching: `*` and
  `?` wildcards, `schema.table` or a table in any schema; their structure kept). The backup then fails unless the destination holds an object of exactly the bytes
  sent. A backup that fails — the size check included — deletes the object its upload may have
  left (a deletion that fails, other than for an object not there, is logged), and says what
  failed: the dump, the upload, the hour's limit, or the daemon stopping. The daemon's stop waits
  up to 2 min 15 s for a running backup to clean up and record its failure
  (`<name>-hostd.service`'s `TimeoutStopSec` is 3 min); each runner's container is named
  `<name>-backup-<random>` and removed when its run is cut off. After a backup,
  the profile's backups (its file names) older than `retention_days` are deleted (`rclone delete
  --min-age`), then — whatever the retention — the profile's volume archives whose dump is gone (retention goes by the
  dump's age; one younger than two hours may be a backup's still running, another install's in a
  shared destination, and stays); a pruning that fails is logged and recorded, the backup stands. A backup takes at
  most an hour. `backup run` is a job (`<short>-stow` prints its log as it runs; refused until
  `<name>.service` ran, during another job, and once a reboot is scheduled); its last run —
  object, profile, release, start and end, bytes, error, pruning error — is kept in
  `/usr/local/<name>/backup-last.json`, which `backup status` shows with the timer's next run.
- The backup settings are the operator's, in `/usr/local/<name>/backup.yaml` (0600, on the
  persistent partition, strictly read; none: no backup runs), set over SSH with `backup config`
  (the cloud-config carries none): `destination` (`remote:path`, the remote a section of the
  rclone config — a remote name is letters, digits, `_`, `.`, `+`, `@`, `-` and spaces, at most
  64; at most 1024 bytes, no control character), `rclone_config` (the rclone config file's text,
  at most 32 KiB: the destination's credentials; `-` reads it from standard input; never shown —
  `backup config` and its JSON say only that it is set), `recipients` (1 to 16 age public keys,
  `age1…`, X25519 or post-quantum, one per flag; given, they replace the list), `retention_days`
  (default 30; 0 keeps every backup; at most 3650), `schedule` (a systemd calendar event, checked
  with `systemd-analyze calendar`, without `%` or `\`; default `*-*-* 03:00:00 UTC`) and `profile`
  (one of the release's, default its `default_profile`). A change gives the flags given and keeps
  the rest; the first needs a destination, the rclone config and a recipient; `--remove` drops the
  settings. `<name>-backup.timer` (`RandomizedDelaySec` 15 min, `Persistent`: a run missed while
  the VM was off runs once the timer starts again) is never enabled: the daemon starts it once
  settings exist, at each change and as it starts, on their schedule — one other than the default
  is the runtime drop-in `/run/systemd/system/<name>-backup.timer.d/schedule.conf`, then
  `systemctl daemon-reload` and a restart of the timer —, and stops it when they are removed (on a
  host without that unit, nothing); no boot runs it on a schedule not the operator's. Settings this release cannot read still start it,
  so its runs fail and say why; on the default schedule when the file does not decode, or when
  its schedule is one the unit cannot hold or `systemd-analyze` refuses (the daemon's log says so;
  `backup config` still shows the stored one). As the
  timer is `Persistent`, a restart whose schedule had a time pass since the last scheduled run (a
  new schedule, settings set again) runs a backup at once. The timer starts `<name>-backup.service`, which
  runs `<short>-stow backup run --if-configured` (nothing, saying why, without settings, before the
  install, or for a release without a `pg-age-rclone` backup or an OS without the runner; while
  another job runs — a `backup verify`, a restore, an upgrade — it waits for that job to end, at
  most 10 minutes, then runs at once, reading its settings, the install and its secrets then (a
  change made during the wait counts; settings removed skip it), or fails as refused (409) when the job still runs; a reboot
  scheduled, and any other refusal, fails it at once; at most 75 minutes). `backup run` asked by
  hand is refused at once (409) while a job runs. `backup list` lists the destination's backups of
  the project — `<name>-<letters and digits>-<UTC time>-…`, a profile a later release dropped
  included, another project's in a shared destination not — (name, size, time); `backup verify` writes `<destination>/<name>-verify.txt` (again each time)
  and reads its size back. Given `--destination`, `--rclone-config` or `--recipient`, it tries
  those over the saved settings (or, with none saved, as the whole settings; checked as `backup
  config` checks a change) and saves nothing; the answer says the settings were proposed. It is a
  job (`backup-verify`, its target the destination, one at a time with the others), which the
  verb follows and waits for: done, it prints that the destination is reachable and writable;
  failed, it fails with the destination's reason. A release whose spec has no `pg-age-rclone` backup refuses every
  `backup` verb (409), but the timer's run. `backup status` shows a backup running, the one an
  `upgrade` takes first included.
- `upgrade` takes a backup first, in its job, when settings exist (`--skip-backup`: none) with the
  settings' profile; a backup that fails is logged and the upgrade goes on (the boot still dumps
  the database to `snapshots/` before it migrates).
- One job runs at a time; none starts once a reboot or a shutdown is scheduled, and neither is
  scheduled during a job.
- The event journal: the host daemon and the health gate append what happened to
  `/usr/local/<name>/events.jsonl` (root's, 0600; one JSON object per line), which survives the
  daemon's restarts and reboots; `/usr/local/<name>/.stowage-events` is the writers' lock and holds
  the last number given, the last one a rotation dropped and the last boot recorded. Every entry is `{"seq", "at", "kind", …}`: `seq` is one past the last
  entry written in the journal's life (never reused; a crash may skip one), `at` the time
  (RFC 3339, nanoseconds, UTC). Past 2000 entries the journal keeps its last 1000. The kinds and
  their fields (a field without a value is left out):
  - `job.started` — `job` (the job's `started`, as `GET job` answers it: its identity),
    `job_kind` (`upgrade`, `rollback`, `reset-db`, `backup`, `backup-verify`, `restore`,
    `rotate`), `target`, `boot_id` (the boot it started in; absent when its id was not read).
  - `job.ended` — `job`, `job_kind`, `target`, `state` (`done` or `failed`), `error` (failed),
    `result` (done: what the job found, `GET job`'s `result` — `backup-verify`'s message),
    `reboot` (`{"action", "drain_secs"}`: the reboot the job scheduled as its last step — an
    upgrade's, `{"action": "reboot", "drain_secs": 30}`, a rollback's, `{"action": "reboot",
    "drain_secs": 0}`; absent when it scheduled none). A job that reboots at its end journals its
    `reboot.scheduled` and its `job.ended` (done) in one append, in that order, so a long poll
    answers both or neither; both are written before the job's `state` reads `done` in `GET job`,
    and before the reboot runs, so a shutdown that stops the daemon never leaves that job without
    an end. When the reboot then fails, the end stands (the job stays `done`) and a
    `reboot.cancelled` with the job's `by` tells it.
  - `job.lost` — `job`, `job_kind`, `target`, `reason`: `the daemon restarted` (its
    `job.started` was written in this boot), `the system rebooted` (in another), or `the daemon
    restarted, or the system rebooted (a boot id was not read)` (its `boot_id` or this boot's was
    not read: only two ids read tell two boots apart); written by a starting daemon for each
    `job.started` the journal holds without an end.
  - `reboot.scheduled` — `action` (`reboot` or `poweroff`), `drain_secs`, `by` (the `job` that
    scheduled it: an upgrade's reboot, a rollback's; absent when asked for), `boot_id` (absent
    when its id was not read). A `job.started` and a `reboot.scheduled` carry the id of the very
    `boot.started` record their append writes with, never one read apart from it.
  - `reboot.cancelled` — `action`, `reason`: `systemctl <action> failed: …` (with `by`, the
    `job` whose reboot it was, when a job scheduled it; a rollback's goes on to say whether the
    boot assessment was put back, see `rollback`), or `the daemon restarted before it`
    (written by a starting daemon that finds the last `reboot.scheduled` without a cancel,
    scheduled in this same boot — both ids read and equal; a `reboot.scheduled` without one is
    left open).
  - `boot.started` — `boot_id`, `release` (`{"version", "revision"}`: the release of the
    writer, the OS's), `slot` (`active`, `passive` or `recovery`; absent off Kairos),
    `fallback`; once per boot id, before any other entry of that boot: whichever writer appends
    first in a boot (the daemon as it starts, the health gate's verdict, the first install's
    end) writes it before its own entries — a starting daemon's `job.lost` and
    `reboot.cancelled` for what the boot before left open come after it, whichever process wrote
    first — when the last boot recorded — kept in
    `.stowage-events`, so through a rotation — is another. A boot whose id cannot be read is not
    recorded; a writer that could not read it (the daemon, the health gate) reads it again at its
    first entry a minute or more after the failed read (never at every entry), and keeps it once
    read.
  - `boot.judged` — the health gate's verdict, at each judgement: `boot_id`, `release`,
    `on_trial`, `verdict` and `reason` (why the install was not healthy, or why marking a boot
    good failed). `verdict` is `healthy`; `went_back` (on trial, unhealthy: the gate reboots and
    Kairos boots the passive system — also when marking the boot good or kept failed, as the next
    boot falls back); or `kept_unhealthy`, with `kept` telling why the system keeps running:
    going back is refused (the dry rollback's refusal), there is no passive system, or the boot
    is not on trial.
  - `install.finished`, `install.failed` — `release`, and `reason` (failed): a run of the VM's
    first install that brought it to its end, or that failed (a run stopped by a reboot or a stop
    writes nothing; the boot runs it again). A Docker install's journal holds the jobs its
    backup and restore verbs run (no verb reads it there).
  - `cert.expiring` — `reason`, the warning `status` gives: a byo install's certificate within 30
    days of its end, or past it, journaled as the daemon starts and once a day.
  - `<short>-stow events` prints the entries after `--after` (default 0), one per line (`--json`:
    the daemon's JSON), and says when entries asked for are no longer kept. `--follow` then waits
    for more until interrupted; a daemon it cannot reach (a restart) is asked again from the same
    number, after a wait that doubles from 1 s to 30 s. Each request is given its wait (60 s
    following) and 2 minutes to be answered, then counts as the daemon not reached, so a daemon
    that takes one and never answers is asked again. It runs on the VM, so a reboot ends it with
    the VM: run it again after the boot.
- The app socket: when `shapes.kairos.host_api` names services, the daemon also serves its whole
  API on `/run/<name>/app/app.sock` (0666, in a 0755 directory beside its socket), and every
  request there must carry `Authorization: Bearer <token>`, the install's `STOWAGE_HOST_TOKEN`
  (64 hex digits generated at the first install, or by the upgrade to a release that names
  services first); before an install records one, or with another token, the answer is 401.
  A daemon serving elsewhere (the hidden `--socket`) says so, as the services still mount
  `/run/<name>/app`. The services named, and only they, mount `/run/<name>/app` read-only at `/run/stowage-host`
  and are given `STOWAGE_HOST_SOCKET=/run/stowage-host/app.sock` and `STOWAGE_HOST_TOKEN`
  (through the front's compose file, `stowage-front.yaml`). A Docker install records no token and
  mounts nothing.
- The daemon's API is JSON over HTTP on the socket, under `/v1`: `GET status` (`boot`;
  `fallback`: Kairos booted the passive system; `on_trial`: the boot runs an upgrade's new system, which the gate has not judged yet; `boot_id`: the kernel's id of this boot, which changes with every reboot; `events`: the event journal's last `seq`, 0 before the first, which a client follows with `GET events?after=` — `null` when the journal cannot be read, `events_error` then saying why (a client never takes a cursor from such a status); `failed_boot`: the last boot the gate found
  unhealthy — `release`, `at`, `reason`, `kept` when the gate kept it, `dir`), `GET health` (`{"ready", "reason"}`), `GET services` (each with its `exit_code`),
  `GET services/{service}/logs?tail=N`, `POST services/{service}/{restart|stop|start}`,
  `POST provision` (an optional `{"set": {"KEY": "VALUE"}}`, as `provision --set`), `GET reset-db` (`{"target": {"version", "revision"}, "plan", "token"}`: the release a reset
resets, what it does, and a new token, which replaces the last; refusals as `reset-db`'s, a
running job and a scheduled reboot included), `POST reset-db` (`{"confirm": {"version",
"revision"}, "token", "no_provision"}`, 202, a job; refused without `confirm`, with a release
other than the one the install runs, and without the last GET's token, which it uses up), `GET updates?channel=&version=` (`left_by_rollback` marks a release the
  install went back from), `POST upgrade` (`{"channel", "version", "confirm": {"version", "revision"}, "reboot", "skip_backup"}`, 202), `GET job?since=N` (`log_total` is the line to ask from next; the last job's `started` (its identity), kind, target, state — `idle`, `running`, `done`, `failed`
  — and its log from line N; `result`, what a job that is done found: `backup-verify`'s message;
  `reboot`, `{"action", "drain_secs"}`, the reboot a job that is done scheduled as its last step), `GET events?after=N&wait=S` (`{"events": [...],
  "last", "dropped", "truncated"}`: the journal's entries after N (default 0), at most 200,
  oldest first; `last` the last entry's `seq`; `dropped` the journal's floor, every entry up to
  it gone (0 when none is); `truncated` when the journal no longer holds N's successor — N is
  below `dropped` (the entries answered are then those after `dropped`) — or N is past `last` (a
  journal started again). A truncated answer comes at once, whatever `wait`: a client goes on
  after `dropped`, or, past `last`, from 0 (then after `dropped`), and never asks the same N
  again, which would be answered the same. With
  nothing after N and `wait` S from 1 to 60 (default 0, more refused with a 400, as is a bad
  `after`), the answer waits until an entry is appended — the daemon's own at once, the health
  gate's within half a second — or S seconds pass (then empty), or the daemon stops), `GET rollback` (`{"from", "to"}`: the release the install runs, the
  one the passive system carries; refusals as `rollback`'s), `POST rollback` (`{"confirm":
  {"version", "revision"}}`, 202, a job), `POST reboot` and `POST shutdown` (`{"drain_secs"}`, 202), `GET network` (`{"live":
  {"interface", "mac", "addresses", "gateway", "gateways6", "dns"}, "source": "override|install|dhcp",
  "override", "install", "pending": {"previous", "deadline", "secs_left"}}`; `install` is the
  network `vm create` gave, DHCP or static, which `network reset` returns to while its networkd
  file applies it (`source` `install`) — absent when the VM's
  cloud-config predates its record (`source` `install` then means a static network the answer does
  not detail) or when the record does not read (logged); `source` is `override`, `install` (that
  network) or `dhcp` (the OS's default); `previous` null is the cloud-config's network), `POST network` (`{"mode", "address", "gateway", "ipv6", "address6", "gateway6",
  "dns", "revert_secs"}`; `ipv6` absent is off), `POST network/confirm`, `POST network/revert`, `POST network/reset`
  (`{"revert_secs"}`), each answering `{"message", "override", "pending"}`, `GET backup/config`
  (`{"configured", "destination", "rclone_config_set", "recipients", "retention_days", "schedule",
  "profile", "profiles"}`), `POST backup/config` (`{"destination", "rclone_config", "recipients",
  "retention_days", "schedule", "profile"}`: a field left out, or an empty `recipients`, keeps its
  value; answers the settings), `DELETE backup/config`, `POST backup` (`{"profile",
  "if_configured"}`: 202, a job, the target the backup's file name; with
  `if_configured`, 200 `{"skipped"}` without settings, before the install, or when the release
  has no `pg-age-rclone` backup or the OS no runner), `GET backup/status` (`{"settings", "next",
  "running", "last"}`), `GET backup/list` (`[{"name", "size", "time"}]`), `POST backup/verify`
  (no body, or `POST backup/config`'s fields to try unsaved, checked at once — 400 when invalid
  —; 202, a `backup-verify` job, the target the destination: done, its
  `result` is the message; failed, its `error` the destination's reason), `POST
  secrets/export` (`{"recipients"}`: `{"file"}`, the armored file), `POST secrets/rotate`
  (`{"key", "pass": "stage|swap|finish"}`, 202, a job), `POST restore` (`{"object", "identity",
  "confirm": {"version", "revision"}}`, 202, a job). Every 202 that starts a job answers
  `{"job", "target", "started", "events"}`: the job's kind and target, its `started` (its
  identity, as `GET job` and the journal's `job` give it) and the journal's last `seq` once its
  `job.started` was written, which its `job.ended` (and its reboot's entries) come after — `null`
  when the start could not be journaled, with `{"events_error": "the job's start could not be
  journaled: …"}` beside it (absent otherwise): a client then takes no cursor from it. An error is `{"error": "…"}` with a 4xx or 5xx status; a body with an unknown field is refused.
- A disaster recovery: `secrets export --recipient age1…` while the VM lives (the file kept off
  it), backups to a destination off it; on a new VM, `vm create --secrets FILE` (the file
  decrypted) installs with those secrets, then `backup config` with the same destination, `backup
  list` and `restore OBJECT` load the data back.
- Not yet: `upgrade --reset-db`, secrets sealed in an encrypted answers file.
- The firewall: the bundle's `/etc/nftables.conf` holds a table of the project's own (`inet
  <name>`, flushed by itself on each load, never the whole ruleset: Docker's rules stay) whose
  input chain drops by default and accepts the loopback, replies (`ct state
  established,related`), ICMP and ICMPv6 (behind extension headers too), DHCP's answers (UDP 67→68, 547→546), SSH per
  `/etc/nftables.d/ssh.nft` (the bundle's is open to every source; `vm create --ssh-allow`
  replaces it) and the install's ports per `/run/nftables.d/<name>-ports.nft`, which each start
  of the install writes: the front's in a TLS mode (`tcp dport { <http port>, <https port> }
  accept`; none in loopback mode), and in every mode each `l4` route's (`<protocol> dport <port>
  accept`, or, with an `allow_input`, `… ip saddr { <its IPv4 sources> } accept` and `… ip6
  saddr { <its IPv6 sources> } accept`, widest first: an address as its /32 or /128, an
  IPv4-mapped IPv6 one as IPv4, a CIDR's host bits cleared, one another holds left out; a value
  that is not an allowlist fails the start). Its forward chain (`priority filter
  - 1`, ahead of Docker's; policy accept) includes `/usr/local/<name>/nftables.d/*.nft`, where
  each start writes `<name>-l4.nft` for the routes with an `allow_input`: a connection to the
  route's port that Docker's DNAT forwards (`ct direction original ct status dnat ct original
  proto-dst <port>`, per family and protocol) from a source the allowlist leaves out is dropped —
  every IPv6 one when it holds no IPv6 source, every one when it is empty (no route with one: the
  file goes). That file is on the persistent partition, so the boot's ruleset drops them before
  Docker restarts the stack's containers. The ruleset is loaded again (`nft -f
  /etc/nftables.conf`) when either file changed; a load that fails fails the start, leaves no
  ports file (the next start loads it again) and puts the allowlists' file back as it was.
  Outbound traffic, and forwarded traffic but that, are Docker's and the OS's to judge; the input
  rules serve what Docker's userland proxy takes on the host (an IPv6 client of a bridge without
  IPv6).

## Repository map

```
cmd/stowage/          entry point (version stamped at build)
cmd/stowage-install/  the installer / <short>-stow: reads the release appended to itself
internal/cli/         commands, flags, output; no logic
internal/spec/        stowage.yaml types, strict decoding, the Provider shape
internal/lint/        every lint rule + compose parsing
internal/version/     version sources (also at a git commit), schemes, channel tags, chart version mapping
internal/changelog/   changelog providers, ranges between versions
internal/digest/      content-addressed inputs digests (.dockerignore-aware)
internal/dockerfile/  a Dockerfile's ARGs and FROM stages (BuildKit's parser)
internal/graph/       the release build graph
internal/registry/    OCI registry client (go-containerregistry), credentials, CAs, artifacts, manifests, OCI layout archives
internal/helm/        the chart: load (.helmignore), check, package with the release's values
internal/release/     the release manifest format
internal/publish/     ship, verify, promote, yank, prune, releases; the Docker image builder; the OS bundle and disks
internal/kairos/      the OS bundle's recipe (Dockerfile, files) and the disk image builder
internal/command/     the `command` provider contract
internal/sigverify/   signature checks of sigstore bundles, in-process (the publisher's verify, every install)
internal/front/       the recipe of the Caddy TLS front image, built by ship
internal/providers/   runtime-side provider configs (auth, signing, health, schema, provision, backup)
internal/backup/      the recipe of the VM's backup runner (rclone, pinned by digest), built by ship
internal/lock/        stowage.lock
internal/hook/        the post-commit hook: release detection, lock, detached ship
internal/gitcmd/      git run without git's per-hook variables
internal/installerbin/ the installer executable embedded in stowage
internal/payload/     the release an installer carries; the bootstrap shell script
internal/install/     the installer: the Docker shape (install, re-run, status, provision, the front),
                      the Kubernetes one (k8s), the Kairos VM's (vm create, the cloud-config and
                      its seed, OpenTofu's modules (TrueNAS, and the previews) and their contract, boot, the host daemon, its verbs) and
                      mirror export / import
internal/cloud/       the cloud images' registration: each cloud's settings and its built-in OpenTofu module (tofu/<cloud>), applied once per image
internal/truenas/     TrueNAS's API: the disk and seed uploads, power, snapshots (the module's steps)
internal/testutil/    test helpers
examples/             minimal (the phase-1 smoke target), full (every spec key), stateless (no database, nothing built)
test/                 end-to-end scripts (make e2e, make e2e-docker, make e2e-full, make e2e-stateless, make e2e-k8s, make e2e-full-k8s, make e2e-kairos, make e2e-vm, make e2e-full-vm), the commit gate's suite selection (gate.sh), their shared helpers (lib.sh), the VM helpers (vm.sh), their pinned images (pins.sh) and the sweep of earlier runs (sweep.sh)
docs/                 DESIGN.md; img/ (the logo, icon, social preview, badge, diagram, installer recording); demo/ (how that recording is made)
```

## Building and testing Stowage

```bash
make check    # gofmt, go vet, staticcheck, govulncheck, go test -race — all in a container
make build    # ./bin/stowage, static; built only after the checks stage passed
make image    # stowage:<version> runtime image (distroless)
make test     # fast loop: unit tests without the race detector
make gate     # the commit gate: check, build, lint, then only the end-to-end suites the change (against GATE_BASE, default HEAD, untracked files included) can affect; e2e-kairos, e2e-vm and e2e-full-vm (given STOWAGE_VM_ENV) one at a time beside the others, which run in parallel; each step of a suite prints the time since it began
make gate-plan  # the suites `make gate` would run, each with the first changed path that needs it; `bash test/gate.sh --plan --paths P…` for given paths
make e2e      # build, then ship / verify / promote / yank / prune examples/minimal, signed, against zot, and run its installer image; with registry.pull none on a registry refusing anonymous reads, ship stops before it builds
make smoke-install  # the installer on the local Docker engine: registry, stack, front, provision hook, re-run, status, upgrade, rollback; behind TLS the local CA, then byo with --ca, each one's CA published to the stack and what the front serves chaining to it; the byo install moved to internal and back by re-runs; a loopback install on `::1` with `--ipv6` (the stack's network and the front have IPv6); with a global IPv6 address on the host, a TLS install whose route has an allowlist, called from that address: `--ipv6` and the allowlist holding it serves, an allowlist without it refuses (403), and the same install moved off IPv6 refuses it too (the front sees Docker's IPv4 gateway)
make e2e-docker     # build, then the real binaries against zot and the local Docker engine: ship examples/minimal 1.0.0, install it with the bootstrap one-liner, re-run, ship 2.0.0 and 3.0.0 and reach each with notes-stow upgrade --version (each release's signature checked against the kept installer's key), roll back (allowed, then refused at a rollback barrier); then `mirror export` 3.0.0, `mirror import` into a TLS zot of its own test CA (refused without `--registry-ca`, the signatures carried), a second install from it with `--registry` and `--registry-ca`, and `upgrade --version` reading it with the recorded CA
make e2e-full       # build, then examples/full as it stands on the Docker shape: ship this month's CalVer release with a token push, signed with the key its environment holds (env://), install it with the bootstrap one-liner (the installer image pulled with the project's own pull account) in loopback mode, every route serving, the api migrating a real Postgres and serving as its app role, the provision hook registering the administrator and its API key through the api and its credentials file 0600; the install prints its endpoint (the api route's URL, also in `status`) and ends with the search extension's tip (`atlas-search`, not published on the channel); a re-run sets the search extension's input (`SEARCH_URL`, the api reaching it, no tip), another clears it (the tip again), the hook handed back the key it minted; the app's users revoke that key: `provision` fails naming `provision --set`, `provision --set ADMIN_API_KEY=` mints and registers another (in no log), the next run keeps it; its record lost, `provision --set ADMIN_API_KEY=<key>` gives the key the hook never returned (declared in `hooks.provision.returns`; an undeclared key refused) and no other is minted; re-runs turn forward auth on the api route on (no identity without the authentication service's answer, a forged header stripped) and off, narrow the ingest route's allowlist to another address and to none (403), set an advanced input and give it back to its default (status shows each); `secrets rotate` takes the internal secret through --stage, the swap and --finish, each value accepted while it must be and the old one refused after; ship .2 (a migration), upgrade --version (data kept, a pg_dump snapshot), roll back (allowed by the api image's reads-newer label); ship .3 (a migration that is a rollback barrier), upgrade, and rollback is refused naming the barrier and the snapshot
make e2e-k8s        # build, then the real binaries against zot and a kind cluster pulling from it with the pull Secret: k8s install with the bootstrap script (the provision hook's Job run, what it returned in its Secret and not the log), upgrade --values, upgrade alone (what the hook returned handed back), upgrade --version 2.0.0, a refused downgrade, a failed upgrade helm rolls back, rollback, status, reset-db (what the hook returned forgotten), uninstall refused without --data (the chart's own claim, which helm deletes), then uninstall --data removing the release, its Secrets, the hook's account and the claim, the namespace kept
make e2e-stateless  # build, then examples/stateless on the Docker shape: ship 1.0.0 (a pulled image, no database) to zot, install it with the bootstrap one-liner in loopback mode, the health path and the route serving the input, the endpoint printed (the route's recorded URL); a re-run changes the input; `status` shows the endpoint; ship 1.0.1, `upgrade --version 1.0.1` (nothing dumped) and `rollback` (no schema check), the input kept; `reset-db` refused, the release having no database
make e2e-full-k8s   # build, then examples/full's chart on a kind cluster pulling from zot: k8s install with the bootstrap script against a Postgres outside the release (its two URLs and the administrator's password secret inputs from the operator's environment), two api replicas migrating once, the provision hook's Job registering the administrator and its API key (recorded in the outputs Secret, in no log; the Job deleted; no helm hook), every service answering, the api reaching the search extension's URL given at the install (`--set SEARCH_URL=…`), the endpoint printed as the api's Service URL; `k8s upgrade` of the same release, the key handed back to the hook; the app's users revoke the key, ship .2 (a migration), upgrade --version: the release runs .2 and the hook fails naming `k8s provision --set`, which mints and registers another key, and `k8s provision` again keeps it; its record lost, `k8s provision --set ADMIN_API_KEY=<key>` gives the key the hook never returned (an undeclared key refused) and no other is minted (data kept); rollback (the previous api on the newer schema); a second release in another namespace with the embedded Postgres, and reset-db deleting its database's claim, forgetting what the hook returned and registering the administrator again; `k8s secrets rotate` through its three passes, the release restarted at each, each value accepted while it must be; `k8s uninstall --yes` of the embedded release keeps its database's claim and its Secrets, and `k8s install` takes them back, the events kept
make e2e-kairos     # build, then ship examples/minimal's OS bundle and its qcow2 and ova disks and its three clouds' raw, vhd and gce to zot for real (each image docker-loaded from the bundle, each disk's zstd layer decompressed with `zstd -d` and checked in its format, the vhd and the gce as Azure and Google Cloud import them); the bundle's notes-stow on the local Docker engine as on the VM (install from the preloaded images with a settings file, boot, the host daemon's verbs, its disk-pressure POST to the api (the token never logged) and `status` showing it, its app socket and the api service's token and mount, `backup config`, `verify`, `run` to a WebDAV server through the preloaded runner, the backup decrypted and read by `pg_restore` and the uploads volume's archive beside it, `list`, `status`); with `STOWAGE_CLOUD_ENV=<file>` (a shell file of a cloud's `STOWAGE_<CLOUD>_*` settings and credentials, needs `tofu`), the images registered on the clouds it sets up, the others reported left out, and every image it registered destroyed at exit; re-ship (nothing built, nothing registered again), promote, `vm create` from the published notes-stow (the qcow2 by digest, the seed ISO), verify, prune; ship 2.0.0, whose notes-stow's boot upgrades the install, and 1.0.0's boot goes back
STOWAGE_VM_ENV=<file> make e2e-vm   # build, ship examples/minimal's OS bundle and qcow2 disk to zot, carry the release with `mirror export` / `mirror import` to a TLS zot mirror of its own test CA on this host's address toward TrueNAS, then the published notes-stow's `vm create --create opentofu --registry --registry-ca` boots a real VM on TrueNAS from the mirror's disk: its first boot installs from the cloud-config (`--ssh-allow` this host's address and its global IPv6 addresses, `--ipv6`, `--timezone Europe/Paris`); over SSH, the firewall's table (input dropped by default, SSH from this host alone, over IPv4 and IPv6, the front's ports, an `l4` route to the database open to this host's IPv4 address alone, its allowlist on the persistent partition: reached from this host, refused over IPv6) and the time zone, status, ps, passwordless sudo, the admin group, the app socket (refused without the install's token, served with it) and the api service's read-only mount; the install records the mirror and the VM trusts its CA; the front answers HTTPS; `reset-db --yes` wipes the data and provisions again (a new admin password) without a reboot; `backup config` over SSH (the rclone config on standard input, a schedule the timer takes), `backup run` of a partitioned table with a key to a WebDAV server on this host, the backup decrypted and read by `pg_restore`, the uploads volume's archive beside it; a row and files added, then `restore` (the key on standard input) brings the backup's data and the volume back as they were; `secrets export` to the operator's key decrypts into the install's secrets; a re-run changing the route's allowlist refuses this host, through the upgrade's reboot; 1.0.1 is shipped and carried through the mirror, `updates` offers it and `upgrade --yes` takes a backup first, then reboots the VM into it, whose health gate marks the boot good; `rollback --yes` reboots it into the passive system, back on 1.0.0, and a second `rollback` is refused there; 1.0.2, whose api never becomes ready, is upgraded to from the passive system, fails its health gate, and the VM falls back to 1.0.0, `status` naming the failed boot and its diagnostics, `updates` no longer offering 1.0.2; 1.0.3, a rollback barrier that never becomes ready, fails its gate too and is kept (the boot marked good, `status` saying why going back is refused); the cloud-config's network is DHCP with DHCPv6 asked at once, the stack's network has IPv6, the LAN gives the VM a global IPv6 address and an IPv6 default route, and this host reaches it over IPv6 by SSH and HTTPS; `network set` keeps the DHCP lease as a static address, a static unique local IPv6 address beside it, and `network confirm` keeps it (the console banner tells it), a change back to DHCP not confirmed comes undone at its 30 s window's end, and another, a reboot inside its window, at the boot; `vm destroy` removes it all
STOWAGE_VM_ENV=<file> make e2e-full-vm   # build, ship examples/full's this month's release with its OS bundle and qcow2 disk to zot, carry it through the TLS mirror as e2e-vm does, then the published at-stow's `vm create --create opentofu` boots a real VM on TrueNAS whose first boot installs it with the search extension's input set (`--set SEARCH_URL=http://api:9090`, the api standing in for a search service); through the VM's front over HTTPS, the api answers with the organisation, the administrator the provision hook registered and the search URL reached; the console and ingest serve, and an event reaches the api; the api reads the front's local CA (publish_ca), which the api route's certificate chains to; the hook's credentials file is 0600, the api mounts the app socket's directory read-only; `vm destroy` removes it all (upgrade, rollback and the health gate's fallback are e2e-vm's)
```

`bash docs/demo/record.sh` records `docs/img/install.gif`, the installer recording at the top of
this README: it ships `examples/minimal` to a throwaway zot as `make e2e-docker` does, then runs
the bootstrap script and the whole form in [VHS](https://github.com/charmbracelet/vhs)
(`docs/demo/install.tape`, in a container with the Docker CLI) against the Docker engine of
`/var/run/docker.sock`, and replaces the GIF only once the install it shows is running (its
`status` succeeds, the portal answers). It needs `./bin/stowage`, and refuses to start while
loopback port 8080 or 8000 is in use or `/var/tmp/operator` (the operator's home it records in,
so the paths shown name no one's home) exists; the stack, the registry, that directory and the
recorder's image are removed when it ends.

`make e2e`, `make e2e-docker`, `make e2e-full`, `make e2e-k8s` and `make e2e-full-k8s` also use `curl` and `python3`
on the host, and the network for base images and the cosign image; `E2E_LOGS=<dir>` keeps a run's
logs there. `make e2e-docker` and `make e2e-full` remove the stack, its volumes and the images
named under its registry when they end (the tools and base images they pulled stay). `make e2e-k8s` and `make e2e-full-k8s` run on a linux/amd64 host only
and also use `sha256sum` and `tar`: they download pinned kind, helm 4 and kubectl once into
`$XDG_CACHE_HOME/stowage-tools` (default `~/.cache/stowage-tools`; each checked against its SHA-256) and run them from there, never the
host's; each removes its cluster and its Docker network when it ends. `make e2e-kairos` and
`make e2e-vm` download the pinned age 1.3.2 there too, and run a WebDAV server from the pinned
rclone image (`stowage-e2e-zot-<pid>-dav`, removed when the run ends). `make e2e-kairos` needs
privileged containers and loop devices (auroraboot), about 30 GB free under `XDG_CACHE_HOME`, and
takes about 25 minutes; it removes the stack it ran as `make e2e-docker` does; the other end-to-end targets but `make e2e-vm` and `make e2e-full-vm` drop the example's `kairos` shape.
`make e2e-vm` needs what `make e2e-kairos` needs (about 15 GB), `tofu` (on `PATH`, else
`~/.local/bin/tofu`), `ssh`, `getent`, `ip`, `openssl`, a TrueNAS host whose DHCP server
registers the VM's host name, and a VM there that reaches this host's address toward TrueNAS
(the mirror listens on it, on a free port). `STOWAGE_VM_ENV` names a file it reads with `set -a` and never copies or prints:
`TRUENAS_URL`, `TRUENAS_API_KEY`, `TRUENAS_INSECURE_SKIP_VERIFY`, `TF_VAR_pool`,
`TF_VAR_parent_dataset`, `TF_VAR_nic_attach`, `TF_VAR_domain` (the DHCP server's domain) and
`DEV_VM_SSH_KEY` (a private key without a passphrase, its public key at `<key>.pub`); the
`TF_VAR_*` are unset before OpenTofu runs. The VM is `stowage-e2e-<pid>`, its state under the
run's work directory; it is destroyed when the run ends, pass or fail — unless `E2E_KEEP_VM=1`,
which keeps it (its state in `stowage-e2e-vm-state-<pid>` beside the run's work directory, in
`$XDG_CACHE_HOME` or `~/.cache`, with a copy of the run's `notes-stow` beside it; the run prints
the `vm destroy` command). `E2E_VM_WAIT` sets `vm
create --wait` (default `30m`). About 85 minutes (two more releases are shipped and the VM
reboots six times). The project's `health_gate` is shortened (`deadline_s: 120`).
`make e2e-full-vm` needs the same, reads the same `STOWAGE_VM_ENV` file and honours `E2E_KEEP_VM`
(its copy is `at-stow`) and `E2E_VM_WAIT`; it shortens the example's `health_gate` to
`deadline_s: 300` and its `disk_formats` to `[qcow2]`; about 25 minutes.

Every end-to-end suite (and `make smoke-install`) ends with `test/sweep.sh`, which removes what
earlier runs left on the host and never what a live run holds (a run is live while its process
is: each work directory records its PID in `.pid`): the test registries
(`stowage-e2e-zot-<pid>`, `-mirror`, `-dav`, `stowage-smoke-zot-<pid>`) and kind clusters
(`stowage-e2e-<pid>`) of dead runs; every image reference
`<ipv4>:<port>/{notes,atlas,smoke,site,t}/…` whose port no running container publishes; work
directories `stowage-e2e.*` of dead runs; the test installs' stacks — compose projects
`<name>-stowage-{e2e,smoke}-…-<crc>`, as the suites and the smoke test name their install
directories `stowage-e2e-…` and `stowage-smoke-…` — their containers once their directory is
gone, and their volumes and networks after an hour without a container, while no other run is
live (a volume records no directory; an install of another name is never touched); containers of the pinned helper images left created, never
started, for ten minutes; every `stowage-disktools` image but the newest. Kept on purpose, one
copy each: the pinned images the runs pull. `bash test/sweep.sh [cache dir]` runs it by hand.

No Go toolchain is installed on the host: every target runs `golang:<GO_VERSION>` in Docker, with
its caches in `~/.cache/stowage-go`. Without make: `docker buildx build --target bin --output
type=local,dest=bin .`

Releases: pushing a tag `v<x.y.z>` runs `.github/workflows/release.yml`. It refuses a tag that is
not on `main` or is not CHANGELOG.md's top entry, an entry without its date (`## [x.y.z] -
YYYY-MM-DD`), and a runner that is not x86-64; runs `make check`, `make build`, `lint` on the three
examples with the binary it publishes, all with a read-only token; the end-to-end suites run
locally, in `make gate`, not on GitHub. A second job, which runs none of the repository's code,
signs `SHA256SUMS` keyless with cosign (`SHA256SUMS.sigstore.json`; the certificate names the
workflow and the tag, and the signature is recorded in Sigstore's public transparency log), attests
the binary's build provenance when the repository is public (GitHub's attestations need a public
repository), and publishes the GitHub release `Stowage <x.y.z>` with `stowage-linux-amd64`,
`SHA256SUMS`, its signature and the tag's CHANGELOG.md entry as its notes. It creates the release
as a draft and replaces a draft's notes and assets, so a failed run is run again as it is; a
release already published is refused, never changed. One run per tag at a time. The check that the
tag is on `main` runs from the tagged commit's own workflow: the repository's tag protection (only
maintainers create `v*` tags) is what makes a release's tag mean a release of `main`.

Contributing: the project's rules and commit gates are in [CLAUDE.md](CLAUDE.md).
