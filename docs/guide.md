# Package an application, step by step

[README](../README.md) · [Documentation index](../README.md#documentation)

This walk-through packages a stack like [examples/minimal](../examples/minimal/) — Postgres, an
`api`, a `web` frontend — for the Docker shape, then adds Kubernetes and the VM. Each step names
the reference section that holds the full rules. [examples/full](../examples/full/) uses every key
and is the place to copy from once the basics work.

## 0. What you need

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

  The release carries the binary's SBOM, `stowage-linux-amd64.spdx.json` (SPDX), checked by the
  same `SHA256SUMS`. A release made while the repository is public also carries a build provenance attestation:
  `gh attestation verify stowage-linux-amd64 --repo norvexlabs/stowage --signer-workflow
  norvexlabs/stowage/.github/workflows/release.yml --source-ref refs/tags/v<x.y.z>`. Put the
  binary on `PATH` as `stowage`. Or build it: Docker is the only build requirement, and
  `make build` writes `./bin/stowage` (see
  [Building and testing Stowage](testing.md)).
- **On the publishing machine**: Docker with `buildx`, network access to your base images, and
  `cosign` v3 when you sign. The `kairos` shape also needs privileged containers and loop
  devices (the disks are made by auroraboot).
- **An OCI registry** you can push to, and a read-only account for installs (or public images).

## 1. Lay out the project

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
placeholders run through the rest of this guide and the reference:

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

## 2. Declare the images

```yaml
images:
  api: { context: api, dockerfile: api/Dockerfile, inputs: [api/] }
  web: { context: web, dockerfile: web/Dockerfile, inputs: [web/] }
  postgres: { ref: "postgres:18-alpine@sha256:…" }       # an upstream image, pinned by digest
```

- A built image's `inputs` are the paths whose content decides whether it is rebuilt (default:
  the whole context less `.dockerignore`).
- Every `FROM` is pinned by digest. A Dockerfile that declares `ARG STOWAGE_PROJECT_VERSION`
  receives the version (and is rebuilt on each one); any other image is only re-tagged on a
  version bump.
- An image that runs as a user and writes to a volume creates that directory with that owner
  (a new named volume takes it).

## 3. Write the compose file

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
  waits for every healthcheck before it reports success. It reports success once every service
  runs (healthy, when it has a healthcheck) or exited 0: a one-shot service may run beside the
  others, no service waiting for it (`docker compose up --wait` fails once such a service exits,
  whatever its code: when compose's output ends on that refusal, `container <name> exited
  (<code>)` — it waits only once it made and started every container — the installer reads each
  service's state with `compose ps`, waiting up to about five minutes for the healthchecks still
  starting; a service exited non-zero, unhealthy, restarting or never started fails the install,
  naming it after compose's error. Any other refusal of `up` fails it as compose said).
- **The install is done when every service runs and every healthcheck passes** (or exited 0, a
  one-shot): `install`, a re-run, `upgrade` and `vm create`'s wait return then. A service without
  a healthcheck counts as ready as soon as its container runs, so a service that does work after
  it starts (a bootstrap, a seeding, migrations it runs itself) must say when it is ready with a
  healthcheck that passes only once that work is done (a marker file it writes last, a readiness
  endpoint). `lint` warns the long-running services with none. Give each long-running service a
  restart policy too (`restart: unless-stopped`): Stowage sets none, and a service that crashes
  or is OOM-killed otherwise stays down until the next start (`status` says it, `down: …`).

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

## 4. Route traffic, define health, describe the data

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

The front routes by hostname, then by path: each route's domain is an input the installer asks
in TLS mode, and routes sharing one tell themselves apart by their `path`. `schema_compat` is what makes `rollback` safe: it reports the schema's migration number
and whether a migration since an upgrade is a rollback barrier
([Schema compat](contracts.md)). Without it (`none`), a rollback is not
checked against the database.

## 5. Add a provision hook (optional)

The only application code Stowage runs at install time — creating the first administrator, for
example. It runs in one of your images after the stack is up, on every install and re-run, so it
must be idempotent; it receives the keys its `env` lists (and nothing else of the install's
secrets) and prints `KEY=VALUE` lines that the install keeps and hands back to its next run:

```yaml
hooks:
  provision: { provider: container, image: api, cmd: [notes-api, create-admin], env: [ADMIN_EMAIL] }
```

## 6. Choose how versions and channels work

```yaml
version:   { source: { provider: file, file: VERSION }, scheme: { provider: semver } }
changelog: { provider: dir-per-version, dir: changes/ }      # changes/1.0.0.md
channels:
  stable: { tag: "{v}", mutable: false, latest: true }       # moves the installer's :latest
  beta:   { tag: "beta-{v}", mutable: true }
```

The defaults are a Keep a Changelog `CHANGELOG.md` as the version source and CalVer
(`YYYY.M.MICRO`). The current version must have a changelog section: it ships inside the release,
and `updates` shows it (Docker, Kubernetes and the VM).

## 7. Point at the registry, and sign

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

## 8. Declare the shapes

```yaml
shapes:
  docker: {}
```

## 9. Check, plan, publish

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

## 10. Install it as an operator would

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

## 11. Add Kubernetes and the VM appliance

- **Kubernetes**: add `kubernetes: { chart: deploy/chart }` to `shapes`. Your chart runs the
  release's images by digest from `.Values.stowage.images`, reads the secrets from the Secret
  named in `.Values.stowage.secret`, pulls with `.Values.stowage.pullSecret` and gives its pods
  `.Values.stowage.sysctls` ([Helm chart contract](contracts.md);
  `examples/minimal/deploy/chart` is a working one). Operators run `sh <name>.sh k8s install`.
- **Kairos VM**: add `kairos` with a `base` distribution (Ubuntu: the bundle installs Docker from
  its Ubuntu repository; `examples/minimal` pins 26.04 LTS, the release the suites prove) and the
  `kairos_init` image, both pinned by digest (`kairos_init` the release `examples/minimal` pins, v4.4.0: the bundle's build
  checks kairos-init's boot assessment file against the copy Stowage carries, so another release
  fails it), and the `disk_formats` to publish. `ship` then also builds the OS bundle (about
  3 GB, several minutes) and the disks. Operators run `sh <name>.sh vm create`, boot the disk with
  its seed, and manage the VM with `<short>-stow` on it. Add a `backup` provider for the VM's
  encrypted backups, `health_gate` to tune the boot check, `host_api` for services that may
  call the host daemon, and `size` for the least and the recommended VM (`vm create`'s default).

## Stacks that are not only HTTP

A control plane with daemons of its own (raw TCP or UDP, its own TLS, its own config formats, an
API it is set up through) packages without changing the software, through the compose file:

- **Nothing served over HTTP**: leave `front` out (see "A project without a front" ([spec](spec.md))); the `l4`
  routes are then the stack's only way in.
- **A daemon's TLS by the front**: an `l4` route's `tls` has the front terminate TLS on the route's
  port with its TLS mode's certificate for a domain, the daemon getting plaintext — no terminator
  or certificate lifecycle of its own (see "L4 routes the front terminates TLS on" ([installer](installer.md))).
- **Ports clients dial as they are** are `l4` routes (see "Spec keys" ([spec](spec.md))), the endpoint the stack
  advertises given by the route's `address_input` (`STOWAGE_L4_<ROUTE>`), the sources the VM's
  firewall lets reach it by its `allow_input`. The front can move off 443 with
  `front.https_port`, when a daemon owns it.
- **UDP, QUIC and tunnels at speed**: `tune.profile: network` — the containers get the network
  settings their namespace keeps of its own, and [`tune`](tuning.md)
  tunes the host (the socket buffers QUIC asks for, forwarded UDP kept batched, the ports and
  neighbours of many connections, TUN devices' queues, the NICs' interrupts and steering); a TUN
  device in a container is the payload's to tune (same section).
- **Material minted once** (a CA, a server key and certificate, a token): a one-shot service of a
  small image (openssl, step-cli) writes it into a named volume, keeping what it finds there, and
  the services that read it `depends_on` it with `condition: service_completed_successfully` and
  `restart: true` (a re-run that changes the one-shot makes it again, and Compose then restarts
  them, so a service that reads the file once at start reads the new one), and mount the volume
  `:ro`. The installer counts a one-shot done when it exits 0. Its inputs (a
  hostname for a SAN) are inputs like any other; its policy is the project's. List the volume in
  the backup's `volumes`, so a restore brings the material back with the data it belongs to.
- **A daemon's own config format**: a thin entrypoint renders it from the environment (the
  recorded values compose interpolates) and from the volume's material, then `exec`s the daemon
  unchanged; or compose `configs:` with interpolated `content:` (or a `secrets:` entry from an
  `environment:` variable): a change to what Compose renders recreates the service that mounts it
  (see the installer's `stowage-content.yaml`).
- **A volume a non-root daemon writes** at a path its image lacks: give the volume the daemon's
  user in `data.volumes.<name>.uid`.
- **Setting up the control plane through its API** (entities, enrolment tokens): a long-lived,
  idempotent compose service that waits for the API, makes what is missing and hands what the
  daemons need through a shared volume, one marker file per finished step. The provision hook
  is not the vehicle: it mounts no project volume and its results go back to it alone.
- **Health behind authentication**: a `command` health provider, in an image holding a client of
  the API, or an unauthenticated readiness endpoint.
- **An optional daemon**: a compose profile an input's `profile` turns on.

## What to read next

- [The contracts a project must honour](contracts.md) — every rule
  `lint` enforces, and the contract of each provider and hook.
- [Spec keys](spec.md) — every key of `stowage.yaml`.
- What an operator sees and can do on each shape: [Docker](installer.md),
  [Kubernetes](kubernetes.md), [creating a VM](vm-create.md) and [the Kairos VM](vm.md).
