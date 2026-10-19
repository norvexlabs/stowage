# Stowage — design

## 0. Scope

Stowage gives a small container stack a complete packaging and lifecycle: a release pipeline,
three deploy shapes from one set of images, a host daemon + `hostctl`, a Caddy TLS front, a health
gate with A/B rollback, backups, a network override with auto-revert and cloud-config rendering.
Everything project-specific is declared in `stowage.yaml`; nothing in Stowage knows a particular
project. The lifecycle is split into **core** (**C**, built in phases 1–2 — the publisher side,
including signing and the release manifest, is phase 1) and **extensions** (**X**), collected in
[§ 10 Extensions](#10-extensions) so each can be accepted or cut.

Target project shape: one Postgres, one or more backends, one frontend, behind a TLS front that
routes by hostname. Sessions are the application's cookies; the front does plain vhost routing (no
sticky routing).

Non-goals: generating Helm charts, orchestrating more than one host per Docker/VM install, what a
rotated secret protects (re-encrypting data under a new key is the application's: Stowage rotates
the value and keeps the old one accepted meanwhile), destroying VMs (the hypervisor's job), supporting databases other
than Postgres in v1 (the `command` providers leave the door open).

## 1. Roles and artifacts

| Role | Tool | Where it runs |
|---|---|---|
| Publisher | `stowage` — Go, single static binary | developer machine, CI |
| Registry | **the single deployment point**: images, chart, OS bundle, disk images, installer, release manifests | any OCI registry |
| Operator | the installer (a Go binary with a terminal UI, carried in an image) + one lifecycle command per shape | the target host / cluster |

Nothing reaches an operator except through the registry. The registry is mandatory.

### Publisher CLI

```
stowage lint                      validate stowage.yaml + the files it names
stowage project-version           the project version and the tag a channel ships it under
stowage changelog [--from V]      the project's changelog sections up to the current version
stowage plan [--channel C]        show the build graph and which nodes are stale
stowage ship [--channel C] [--revision]
stowage verify [--channel C] [<version>]
stowage promote <version> --from C --to C
stowage yank <version> --reason TEXT [--channel C]
stowage prune --channel C [<version>]   mutable channels only
stowage releases [--channel C]    list from release manifests
stowage lock [--update]           pin the stowage version into stowage.lock
stowage hook post-commit          ship when the project version moved (wired by `stowage hook install`)
```

## 2. The project format — `stowage.yaml`

Full examples: [full](../examples/full/stowage.yaml) (every key),
[minimal](../examples/minimal/stowage.yaml). Top-level keys:

| Key | Purpose |
|---|---|
| `apiVersion` | `stowage/v1` — format version (§ 3.3) |
| `name` | drives every derived name: images `<name>-<svc>`, `/etc/<name>`, `/run/<name>`, systemd `<name>-*`, `<short>-stow`, charts `charts/<name>-<channel>` |
| `stowage.requires` | accepted Stowage range; exact version in `stowage.lock` |
| `version`, `changelog`, `channels` | § 3 |
| `registry` | prefix, pull credentials (baked into the installer), push auth |
| `images` | build table: `context`/`dockerfile`/`target`/`args`/`inputs`, or `ref` for a pinned upstream image |
| `stack.compose` | **the single service definition**, no `build:` (lint-enforced) |
| `front` | TLS modes offered, hostname → upstream routes, header stripping, forward auth and a source allowlist per route, which route is the portal, the CA published to the stack |
| `health` | readiness probe (provider) used by every consumer: compose healthcheck, health gate, install wait |
| `database` | optional: engine, service, db name, migration and app roles, schema-compat provider; without it, no backup, `data.reset`, `reset-db`, restore or schema check |
| `data` | persistent volumes with owner uid, which ones `reset-db` wipes |
| `inputs` | what the installer asks: key, prompt, default, secret, required, which shapes |
| `secrets` | generated once: key + generator (`hex32`, `pass18`, …) |
| `safe_defaults` | values the compose template defaults to; only the installer's loopback mode relaxes them |
| `hooks` | the only application code: `provision`, `maintenance` |
| `backup` | provider + profiles |
| `extensions` | the apps an install can be pointed at: name, the optional input holding the endpoint (empty: off), an optional secret input for its key, the tip's prompt |
| `endpoint` | the route that is this project's endpoint when another project uses it as an extension |
| `shapes` | per-shape settings: `docker`, `kubernetes.chart`, `kairos.{base,kairos_init,disk_formats,cloud_publish,health_gate,host_api}` |

Lint rules (`stowage lint`): compose has no `build:` and no `name:`; every service image is
`${STOWAGE_IMAGE_<KEY>}` for an `images` key (no literal image: the registry is the single
deployment point); every `${VAR}` without a default is an input, a secret, a safe default, a route
domain or a Stowage-provided variable (`${VAR:-x}` is an optional tunable); every input is consumed
by compose, a route or the provision hook's `env` (a Kubernetes-only input is the chart's);
the provision hook's `env` names only inputs, secrets and safe defaults; every `safe_defaults` key appears in compose as
`${KEY:-<value>}`; routes, the portal, the health and database services resolve to compose
services; image refs are pinned by digest; the current version validates under the scheme and has a
changelog section; every built image starts from base images pinned by digest; spec paths are
relative and stay inside the project. Each rule is held by a case in the tables of
`internal/lint/lint_test.go`.

Variables Stowage provides on every shape (the `STOWAGE_` prefix is reserved):
`STOWAGE_IMAGE_<KEY>` (the image's pinned reference; key upper-cased, `-` → `_`),
`STOWAGE_URL_<ROUTE>` (the route's public URL: `https://<domain>` behind TLS,
`http://<address>:<port>` in loopback mode — projects read it rather than assembling URLs),
`STOWAGE_NAME`, `STOWAGE_PROJECT_VERSION`, `STOWAGE_REVISION`, `STOWAGE_LOOPBACK_ADDRESS`. Service
names starting `stowage-` are Stowage's: the front is the `stowage-front` service, added beside the
project's compose file.

### 2.1 One pattern for everything project-specific

Every concern that varies between projects is `{ provider: <name>, …config }`, with built-in
providers and a **`command`** provider as the escape hatch. Where a concern can be left out, the
first provider listed is its default (version source `changelog`, scheme `calver`, changelog
`keepachangelog`, signing `none`); the others — registry auth, health, schema compat — name their
provider explicitly. Stowage core never interprets project semantics beyond the provider
contract.

| Concern | Contract | Built-in providers (default first) |
|---|---|---|
| version source | → one string | `changelog`, `file`, `git-tag`, `command` |
| version scheme | validate · compare(a,b) · optional commit guard (a version is its own tag) | `calver`, `semver`, `regex` |
| changelog | version → section text | `keepachangelog`, `dir-per-version`, `command` |
| channels | tag template · mutability · `latest` | declared list, at least one (typically `stable` + `dev`) |
| registry auth | pull / push credentials (standard registry login only — no client certificates) | `token` (account + API key/token as password), `docker-config`, `none` |
| signing | sign a digest · verify a digest | `none` (default), `cosign-key` (file / env / `hashivault://` / cloud KMS URI) |
| health | ready yes/no + reason | `http-json`, `http-status`, `command` |
| schema compat | current schema version · barrier after v? · reads-newer? · baseline (`sql`) | `sql`, `command`, `none` |
| provision | the keys its `env` declares (plus Stowage's values and what it returned before) in → `KEY=VALUE` out, idempotent, resumable | `container` |
| secrets sealing | per shape | `env-file` (Docker), `k8s-secret` / `existing-secret` (K8s), `env-file` on the persistent partition (VM) |
| backup | dump · restore · list · verify | `pg-age-rclone`, `command` |
| TLS | certificate source | `acme`, `dns-cloudflare`, `byo`, `internal`, `none` |

**`command` contract**: an executable (or `{image, cmd}` for runtime hooks) receives a JSON request
on stdin (`{"op": "...", ...}`), writes a JSON answer on stdout, exits 0 on success, 2 on "no
answer" (treated as unknown, never as a negative), anything else on error. It runs in its own
session with no controlling terminal (so it cannot prompt), under a timeout (`timeout_s`, default
30); whatever it leaves running is killed once it has answered, an interrupt to Stowage kills it,
and Ctrl-Z stops and resumes it with Stowage. `scheme.validate` must answer `{"valid": bool}`:
a version check that did not run never passes. One contract, every
concern. Example ops: `version.get`, `scheme.compare`, `changelog.section`, `schema.current`,
`schema.barrier_after`, `schema.reads_newer`.

## 3. Versioning — three independent axes

### 3.1 Project version — opaque to Stowage

The project owns its version and how it is spelled. Stowage asks the **scheme** provider to
validate it and order two versions — nothing more; the version itself is its tag, so it must be a
legal OCI tag. CalVer `YYYY.M.MICRO`
taken from the first `## [x]` heading of `CHANGELOG.md`, with the "month must be the current UTC
month" commit guard, is **one project's configuration** (see `examples/full`), not Stowage
behaviour.

- `calver`: `format` (`YYYY.M.MICRO`, `YY.MM.MICRO`, …), `month_guard`.
- `semver`: SemVer 2.0 ordering, pre-release honoured.
- `regex`: a pattern with named groups + an ordering key list (`[major:int, minor:int, label:str]`).

Helm needs a SemVer chart `version` that orders a channel's releases. Stowage never derives it
from the project version (a revision has no SemVer precedence, and a scheme need not be SemVer):
each channel publishes its own chart repository (`charts/<name>-<channel>`, keyed by the channel
name alone), and a release's chart version is
`0.<release_seq>.0`, `release_seq` numbering the channel's releases in publication order. The
project version is the chart's `appVersion`, and `<short>-stow k8s` resolves versions through the
release manifests.

The chart stays the project's: Stowage packages it (in-process, as `helm package` would, with
Helm's own `.helmignore` rules, and without the helm binary or the Kubernetes client libraries a
helm SDK import brings) and appends one values block, `stowage:` — the registry prefix, the
release's identity, every image by digest and the trusted public keys. Images by digest, never
by tag: the signed release manifest pins the chart's digest and the chart pins the images', so
an install verifies the whole release from one signature, and a tag moved in the registry cannot
change what runs. The registry stays a value, so an operator mirroring the release points the
chart at the mirror without repackaging it. The block is appended as text and the result is
checked to read as the chart's values plus the block alone, so the project's `values.yaml` is
published byte for byte. The packaged chart has no timestamp: like the installer, it is made
again from the published manifest on every re-ship and compared by digest.

A chart version is a release number, and the chart repository holds only versions a release
manifest names. The chart is pushed and signed by digest with the other artifacts, but its
version tag moves only once the manifest naming it is written: a publication that fails earlier
leaves a signed, untagged digest and no version. A mutable release rebuilt with new content takes
a new number, so the build it replaces leaves a version no manifest names; every mutable `ship`
and `prune` sweeps the channel's chart versions that no manifest of the channel nor its pointer
names, once the channel settled (the pointer may still hold a rewritten manifest, whose chart it
offers). The sweep also catches what a stopped run left. It lists the chart tags before reading
the manifests, so a version tagged meanwhile always has its manifest seen; it deletes only tags of
the chart-version form (a signature's fallback tag lives in the same repository), and nothing at
all when a manifest cannot be read, since that release's chart cannot be told unnamed. A
registry refusing a deletion is a warning: the release is complete. An immutable channel is never swept: it never rebuilds, so it
never leaves one. This does not make the highest version the channel's offer: a yank deletes
nothing (chart included), and a mutable rebuild of an older version takes the highest number.
The manifests are the offer, and `<short>-stow k8s` installs the version a release names.

### 3.2 Stowage version — the tool and its runtime

"Stowage version" covers the CLI **and every runtime piece it puts on a target**: the installer
and `<short>-stow` (with the host daemon, the health gate and the VM verbs inside it), the Caddy
front, the backup runner and the VM bundle's recipe. All of them are in the `stowage` binary:
the installer's executable (`stowage-install`, linux/amd64) is embedded, and the front, the backup
runner and the OS bundle are recipes (Dockerfiles, bases pinned by digest) that `ship` builds into
each project's own registry. There is **no Stowage registry**: an install pulls from the project
registry only, and the recipe's bytes are each piece's inputs, so one `stowage` version fixes them
all.

A project pins a range in `stowage.yaml` (`stowage.requires: ">=1.0 <2"`) and the exact version in a
committed **`stowage.lock`**:

```yaml
stowage: 1.4.2
```

`stowage lock` writes it, `stowage lock --update` moves it; the diff is reviewable and a rebuild is
reproducible. `ship` and `promote` refuse a `stowage` of another version than the lock's, or a
project without one: a newer publisher would otherwise publish a changed installer and front
silently, and only the lock's diff makes that a reviewed change.

### 3.3 Format versions

`apiVersion: stowage/v1` (spec) and `schemaVersion: 2` (release manifest) move only on a breaking
change. A consumer that meets an unknown major refuses with an actionable error ("this release needs
installer ≥ 2 — re-run the bootstrap one-liner" / "upgrade hostctl first").

### 3.4 Release identity = project version + package revision

Every channel writes into the same repositories, so no tag one channel can write may be a tag
another channel can write, nor — for any version — a channel pointer tag: lint checks the
channels' templates against each other for the current version, and `ship` for the version it
publishes (`v{v}` / `v{v}-dev`
is safe under CalVer; under SemVer a suffix such as `{v}-beta` collides with pre-releases, a
prefix such as `beta-{v}` does not).

Repackaging the same project version with a new Stowage runtime (a `hostctl` fix, a base-OS CVE) is a
new **revision**. Tag `{v}` for revision 1, `{v}-r{N}` afterwards (the channel's tag template decides
the spelling). Order is `(scheme order of project version, revision)`. A stable tag stays immutable;
a revision is how a stable release receives a runtime fix without lying about the app version.

### 3.5 What the operator sees

- **Changelog**: the project's sections between the running and the target version (project
  changelog provider) **plus** a **Platform** block listing Stowage runtime changes between the two
  releases' `stowage_version`s (Stowage's own changelog, embedded in the manifest at ship time). One
  read, both stories; the operator never needs to know they are versioned separately.
- **Status**: `<project> <v> (rev N) · stowage <x.y.z>` on every shape. The VM keeps both in
  `/etc/<name>/release.json`.

## 4. Idempotent build graph

```
images ──► os-bundle ──► disk images
   │           │
   └──► chart  └──► installer ──► release manifest ──► channel pointer
```

Each node has an **inputs digest**: a content hash of its declared paths (working tree, so an
uncommitted edit counts; for an image, its `.dockerignore` applied exactly as docker applies it;
`.git` is never read, so inside a build context it must be excluded by `.dockerignore`, which lint
enforces) + dockerfile + target + build args, and for the nodes that carry Stowage's runtime
(chart, OS bundle, disks, installer, release), the `stowage.lock` version. Base images enter through the Dockerfile's bytes: lint requires every
`FROM` to be pinned by digest, so a base-image update is a Dockerfile change and reaches the
digest (an unpinned base would change without one). The project version enters an image's digest
only when its Dockerfile
declares `ARG STOWAGE_PROJECT_VERSION` (Stowage then passes it); every other image is re-tagged on
a version bump. The revision enters every artifact that embeds it (chart, OS bundle, installer,
release manifest), the channel every one but the OS bundle, never an image. The installer's inputs also hold its
executable and the pull account baked into it. The digest is stored as annotation `dev.stowage.inputs` on the
pushed artifact.

- `stowage ship` skips a node whose tag already carries the same inputs digest; it rebuilds only
  stale nodes and their dependents. An image whose own inputs did not change is **re-tagged**, not
  rebuilt.
- On an **immutable** channel, a changed digest under an existing tag is refused. If only the runtime
  moved, `ship --revision` publishes `{v}-r{N}`. Mutable channels (a `-dev` channel) overwrite.
- `verify`: every ref of the release manifest exists, and each floating tag (`latest`, the channel
  pointer) is digest-equal to its target — an existence check alone would pass on the previous
  build.
- Every image is planned before anything is written, so a refusal leaves the registry untouched.
- Builds and mirrors are pushed by digest, untagged, before any tag moves; a failed build or an
  interrupt leaves every tag as it was.
- The channel pointer moves **last**, only after `verify`, and never backwards to an earlier
  release or to a yanked one; when it falls back (yank, prune) it goes to the latest release that
  is not yanked **and** verifies. The installer's `:latest` follows the pointer of the channel
  that sets `latest`: `ship`, `yank` and `prune` end by settling it from the final pointer — the
  pointer's installer when its release is offered and complete, none without a pointer — and
  finish a yank or prune that stopped early (a pointer on a release no longer offered moves
  back); `verify` of that channel compares its digest with the pointer's installer.
- The installer image is made in process, without a timestamp: the same release makes the same
  digest, so a re-ship writes nothing and a lost installer tag is put back; on an immutable
  channel an installer that would differ is refused like an image. It carries the release
  manifest, so it embeds outputs no inputs digest covers (image digests, `release_seq`, date):
  it has no `dev.stowage.inputs` annotation and is never kept on its inputs — ship makes it again
  from the manifest and compares digests. Its node's digest still enters the release's.
- Images are built for `linux/amd64`, and the platform is one of an image's inputs: an image built
  elsewhere is never kept or re-tagged in its place. Multi-platform builds are a later step.
- `promote` publishes a release on another channel by re-tagging only: it runs `ship` for the
  target channel with building forbidden, so every image must be found with the same inputs (the
  tree is checked out at that version), and before anything is written the tree's release inputs
  for the source channel must equal the source release's — images, compose, spec, changelog,
  `stowage.lock`, the installer executable and the baked pull account exactly as published.
- A published release on an immutable channel is never republished with other artifacts: if an
  image would have to be rebuilt (registry retention removed it), ship refuses and asks for a new
  revision.
- Re-shipping a yanked release fails: it is not offered again; a fix is a new revision. The channel
  never moves forward to it; a pointer an interrupted yank left on it moves back.
- **One publisher at a time per project.** A registry offers no compare-and-swap, so two
  concurrent ships could claim the same `release_seq` or race on a new tag; CI serialises
  publication (one concurrency group per project), and `stowage hook post-commit` takes a lock.
- An image tag with no release manifest under the same tag (a publication interrupted after
  tagging) is replaced even on an immutable channel: nothing was published under it.
- Deletion (`prune`, a pointer with no release left) is always by tag, never by digest: one digest
  carries the tags of several channels. The registry must support tag deletion.
- The OS bundle names no channel: its OS version is the release's version and revision, and it
  preloads each image as an OCI layout archive named by the image's digest
  (`<repo>:sha256-<hex>`), so a promotion re-tags it and its disks instead of rebuilding a
  multi-gigabyte image. It is kept on its node's inputs **and** the digests of the images it
  preloads (an image rebuilt with the same inputs is another image); each disk is kept on the
  bundle's digest, its format and the disk recipe, so a format added makes that disk alone. Both
  are planned once the images are pushed by digest — still before any tag moves; on an immutable
  channel the images are then all kept, so a refusal still writes nothing.
- Floating tags (the installer's `:latest`, each disk's `latest-<fmt>`) follow the pointer of the
  channel that sets latest and are re-tagged in place; a format the pointer's release lacks loses
  its tag.
- `stowage hook post-commit`: version moved forward between `HEAD~1` and `HEAD` (both read from the
  commits, ordered by the scheme) → detached `ship` under a lock, logged per version; skipped
  during rebase/merge/cherry-pick/revert, on a merge commit or a cherry-pick (its first parent
  is not the history the release follows) and on an amend of a commit already at that version
  (its ship was started). The detection is synchronous and instant, so a commit never waits for
  or fails on its release; the ship outlives the terminal (its own session). The lock, the logs
  and the checkouts live in the git common directory: never in the working tree, one lock across
  linked worktrees; a later hook ship waits for the running one rather than being dropped. A
  `git-tag` or `command` version source does not move with a commit, so its project ships with
  `stowage ship`. The ship runs in a detached checkout of the commit, so the release is that
  commit whatever happens to the working tree meanwhile; a relative signing key is read from the
  project directory, since a key file is never committed. Git runs without the hook's `GIT_*`
  variables, which describe the commit in progress, not the repository layout `-C` finds.

## 5. Registry layout and release manifest

```
# <tag> = channel template applied to the version (+ -r<N> for revisions)
<prefix>/<name>-<svc>:<tag>                 app images
<prefix>/<name>-front:<tag>                 the front (Caddy), built by ship from Stowage's recipe
<prefix>/<name>-backup:<tag>                the VM's backup runner, built by ship from Stowage's recipe
<prefix>/<name>-os:<tag>                    Kairos OS bundle (project images preloaded + VM runtime)
<prefix>/<name>-disk:<tag>-<fmt>            installer disk images (+ latest-<fmt>)
<prefix>/charts/<name>-<channel>:0.<seq>.0  Helm chart (OCI), one repository per channel
<prefix>/<name>-installer:<tag>, :latest    installer image (all three shapes)
<prefix>/<name>-release:<tag>               release manifest
<prefix>/<name>-release:<channel>           channel pointer (stable, dev, …)
```

The release manifest is an OCI artifact, media type `application/vnd.stowage.release.v1+json`:

```json
{
  "schemaVersion": 2,
  "name": "atlas",
  "short_name": "atlas",
  "project_version": "2026.10.25",
  "revision": 1,
  "release_seq": 412,
  "channel": "stable",
  "tag": "2026.10.25",
  "date": "2026-10-08T12:00:00Z",
  "inputs": "sha256:…",
  "scheme": { "provider": "calver", "format": "YYYY.M.MICRO" },
  "stowage_version": "1.4.2",
  "artifacts": {
    "images": { "admin-api": "sha256:…", "proxy": "sha256:…", "front": "sha256:…" }, // + the front
    "manifests": { "admin-api": "sha256:…", "proxy": "sha256:…", "front": "sha256:…" }, // linux/amd64 manifest digests
    "configs": { "admin-api": "sha256:…", "proxy": "sha256:…", "front": "sha256:…" },   // linux/amd64 config digests
    // an offline install accepts a loaded image whose ID is its image, manifest or config digest
    "os": "sha256:…",
    "disks": { "qcow2": "sha256:…" },
    "chart": { "repository": "charts/atlas-stable", "version": "0.412.0", "digest": "sha256:…" },
    "installer": "sha256:…"
  },
  "changelog": { "project": "…markdown section…", "platform": "…stowage section(s)…" },
  "yanked": null,
  "compat": {
    "schema_version": 23,
    "rollback_barrier": false,
    "reads_newer_schema": true,
    "min_upgrade_from": null
  }
}
```

`ship` fills `images`, `installer`, `chart`, `os` and `disks` today; the Platform changelog and
`compat` are filled as their phases land (§ 11), and a consumer must treat a missing one as "not
published". `release_seq` is claimed from a per-channel record (`<prefix>/<name>-release-seq:<channel>`)
before the release is written, so a number is never handed out twice — not after a prune, not
for a mutable tag rebuilt with new content, which gets a new number; a failed publication leaves a
gap. Consumers and chart versions can therefore order by it.

The installer carries its release's manifest (§ 6, the payload) less two fields: `installer`,
which names the installer itself, and `yanked`, which a consumer reads from the registry — a yank
rewrites the manifest and never the installer. The installer is built after the release's number
is claimed and before the manifest is written.

Every consumer reads this one document — `hostctl updates` / `changelog`, `<short>-stow updates` /
`upgrade`, the Kubernetes lifecycle, the application's own "update available" banner. Consumers order
releases with the scheme recorded in the manifest; **they never parse tags** (no `tags/list`
sorting, no raw changelog sidecar).

`yank` sets `yanked: {reason, date}` on the manifest and moves the channel pointer back; nothing is
deleted (installed systems may run it, and rollback may need it). Consumers refuse to upgrade *to* a
yanked release and warn when *running* one.

### 5.1 Signing — a dedicated release key

Off until the project configures a key (`signing: { provider: cosign-key, key: … }`); then mandatory.

- **Key**: one dedicated signing key per project. The private key is a publisher secret (a file, an
  env var, or a KMS/Vault URI — cosign accepts the same `--key` forms, so moving the key into Vault
  later changes one value, not the design). The public key is baked into the installer image, the VM
  (`/etc/<name>/signing.pub`) and the chart values.
- **Transport**: push auth is a standard registry login (§ 5.2), so cosign (v3) pushes its
  signatures itself with the publisher token, as an OCI referrer of the signed digest — a sigstore
  bundle — with no transparency-log upload: the trust anchor is the baked public key. Reads need
  only the pull account, so verification works everywhere. The publisher hands cosign the
  credentials in a private Docker config per call, and refuses to sign where cosign could fall back
  to plain HTTP (a private IP or `*.localhost` registry other than loopback).
- **Order**: an image is signed by digest before its tag moves; the release manifest is pushed by
  digest, signed, then tagged — so no tag of a signed project ever holds an unsigned artifact. A
  signature already made by a trusted key is not made again, which keeps `ship` idempotent.
- **What is signed**: every artifact an install pulls **and the release manifest** (the
  publisher's own bookkeeping, the `-release-seq` record, is not). The manifest signature is
  the root of trust: it pins every other digest, and every consumer pulls by digest only — so the
  whole release is verified even where per-image verification is not wired.
- **How**: in-process, with sigstore-go — the bundle's DSSE signature by a listed key, over an
  in-toto statement whose subject is the artifact's own digest; no cosign on any target, and the
  publisher's `verify` uses the same verifier (cosign only signs). A signature that cannot be read
  is a failure, never "unsigned". A manifest's digest is always its content's hash (a registry
  asserting another is refused), so a verified digest covers the bytes read.
- **Who verifies**: every release manifest an install reads from a registry, before anything of
  it is fetched — `<short>-stow upgrade --version` (Docker, Kubernetes) and `hostd`'s `updates` /
  `upgrade` (the VM). The manifest pins every other digest (installer, images, chart, OS bundle),
  each pulled by digest. The trust anchor is the installed release's keys: the kept
  `<short>-stow`'s payload, or the `<short>-stow` run's when the kept one carries none (Docker: an
  older installer must not bring back a key a later release dropped), the OS's `<short>-stow` (VM), the `<short>-stow` run's, narrowed to those the
  release's `stowage.publicKeys` value lists (Kubernetes: values, which `--values` overrides, never
  add a key; a key a later release dropped is not trusted again). A VM's channel pointer is followed
  only to a release its tag still holds and that is not yanked. Among a version's
  revisions the latest signed one is taken: an unsigned one cannot hide it, nor yank it; one whose
  signatures cannot be read stops the search, so a read failure never falls back to an older one. Only
  `cosign sign`'s predicate counts (not an attestation by the same key). The first install trusts the installer the operator runs (verifiable with
  `cosign verify` by its tag); the installer's carried release needs no registry read. Stowage
  may later render an optional image admission policy from the public key (enforcing it is the
  cluster owner's choice).
- **No silent downgrade**: the keys are carried by the installed release itself, so an install
  that accepted a signed release refuses an unsigned or wrongly signed one from then on, with no
  extra state; an install of a project that never signed checks nothing.
- **Rotation**: the baked file holds a key *list*. A release first ships with old + new public keys,
  is signed with the old one; the next release is signed with the new one and may drop the old.

### 5.2 Registry and its credentials

Stowage works with **any OCI registry that supports the standard login** (Basic auth or the token
flow) and OCI referrers. There are exactly two credentials, both plain registry logins every tool
speaks natively (docker, helm, oras, cosign, kairos-agent, containerd):

| Credential | Rights | Holder |
|---|---|---|
| **pull** account | read on the project's repositories | baked into the installer, the VM, the K8s pull Secret; near-public by design |
| **publisher** token (API key) | read + create + update + delete on the project's repositories | `stowage ship / promote / yank / prune` only; env `STOWAGE_PUSH_TOKEN`, never baked anywhere |

No client certificates anywhere: one auth path to build and test, and no per-tool cert flags.
With `registry.pull: none` there is no pull account: every install reads anonymously, and a
registry that creates repositories private (ghcr) would let a release publish that no install can
read. `ship` reads each repository it writes anonymously — before it builds (what exists),
after the images' push (what the push created, before the long bundle and disk builds), and
before a channel points at the release (all it recorded, by digest) — and stops naming them. The
last check sits after the release is written and before the pointer moves, so nothing an operator
can reach is unreadable, and the next run of the same inputs carries on. The disk builder does not
depend on it: it reads the bundle through a loopback proxy (see the Kairos shape's disks).
Rotating the publisher token touches only the publisher; rotating the pull account gives installs
the new password through its variable at their next run (§ 7 row 25).

**Reference registry: zot** (what the docs and the test stack use; not required). A single Go binary
in one container, no database, OCI-native (referrers, so cosign signatures live beside the image),
with per-repository access control and retention/GC built in:

```json
{
  "http": {
    "address": "0.0.0.0", "port": "5000",
    "auth": { "htpasswd": { "path": "/etc/zot/htpasswd" } },
    "accessControl": {
      "repositories": {
        "atlas/**": {
          "policies": [
            { "users": ["pull"],      "actions": ["read"] },
            { "users": ["publisher"], "actions": ["read", "create", "update", "delete"] }
          ],
          "defaultPolicy": []
        }
      }
    }
  }
}
```

The publisher's htpasswd password is a long random token — that *is* the API key. zot also has a
native API-key extension (per-user keys, labelled, revocable, scoped to repositories), and it works
with htpasswd logins alone, no OpenID (`"apikey": true`; checked by `make e2e` against zot
v2.1.22, which fails if they stop working): a publisher can hold a revocable key instead of the
account password. TLS terminates at zot or at a Caddy in front. Harbor was
considered and rejected as too heavy (many containers + Postgres + Redis) for a single deployment
point.

## 6. Operator journey

1. **Bootstrap** — one line, the same for all shapes; the installer image writes a shell script to
   stdout: `docker run --rm <prefix>/<name>-installer:latest > <name>.sh && bash <name>.sh`
   (piping it, `… | bash -s -- <flags>`, works too). The script is a short POSIX stub with the
   installer binary inside it (base64, in a here-document, so nothing is read back from `$0`); it
   extracts the binary into a private directory under the current directory (not `/tmp`, often
   `noexec` on a hardened host), runs it with the script's arguments and removes it. The binary
   carries the operator docs (`docs`) and the release's payload — the manifest, the compose file,
   the spec's runtime view and the trusted keys, appended to the executable — so an offline install
   needs nothing else. First question: shape `docker | k8s | vm` (or `--shape`).
2. **Inputs** — a terminal form (below) built from `inputs:`: each field pre-filled from the
   environment or the default, secrets masked and generated from the OS's CSPRNG, validation
   inline, a review screen before anything is written; then pre-flight, shape-specific (§ 7 row 6),
   still **before anything is written**.
3. **Fetch** — the channel's release manifest, then exactly its digests.
4. **Start + provision** — Docker: `.env` (0600) + `compose up --wait`. K8s: Secret +
   `helm upgrade --install --atomic`. VM: render cloud-config (+ NoCloud ISO, or point at the disk
   image) and boot; first boot needs no registry (images are preloaded). Then the `provision` hook,
   resumable. Docker: a service Stowage adds (`stowage-provision.yaml`, profile-gated, no
   capability, no host file) run with `compose run --rm --no-deps -T`; its stdout `KEY=VALUE`
   lines are recorded one by one in `provision.env` (0600) and fed back on the next run, so a
   value it minted survives a run that stops later; inputs come from `.env`, never a second
   questionnaire. It receives only the keys its `env` declares: a hook holds no secret it did not
   ask for, in an image that may not be the stack's own. K8s: once helm has settled the release, a
   Job the installer creates (§ 7 row 8) — the hook's stdout goes to the pod's `emptyDir`, never
   its log, and a step of the release's own installer image records it in a Secret the release
   owns, through a ServiceAccount that may patch that Secret alone; a failure leaves the release
   running and names `k8s provision`.
5. **Done** — print the `front.portal` URL and the credentials file path.
6. **Upgrade / rollback** — Docker: the new release's installer runs `upgrade` on the install;
   `<short>-stow upgrade --version V` fetches it first. It resolves V from the release manifests
   (the runtime view carries no channel templates, so a tag is never rendered): the install's
   channel, V's latest revision not yanked, read from the registry the install pulls from (a
   mirror holds the release and installer repositories too). The installer is pulled in process
   by the digest the manifest records, checked to carry that manifest, and run as a child with
   the flags given, so its own checks, lock and messages apply unchanged.
   Every check (order by the recorded scheme, same routes and TLS mode, added inputs only) and
   every pull come first; then `pg_dump` into `snapshots/`, the schema version read, the running
   release (compose files, `.env`, `<short>-stow`, a state record) kept in `.stowage-prev/`, `.env`
   extended and rewritten, `up --wait`, provision. `rollback` is run by the installer the install
   keeps: the schema-compat provider decides on proof only (a barrier since the recorded version,
   or a previous release that does not read a newer schema; `schema.reads_newer` runs in the
   previous release's image), then the previous files come back and only the release's own
   values (version, revision, images) are taken from the kept `.env`.
   A schema baseline is an epoch: a release that starts a new one carries a new initial
   migration, so no schema check between two baselines can answer. Both sides of the decision
   are in the two releases' specs (the kept installer carries the installed one's), so the
   database is not read for it; an upgrade or a rollback across one is refused, and a fresh
   install is the way across. On Kubernetes nothing on the cluster holds the running release's
   spec, so it is read from that release's installer, fetched by the tag the helm release records
   and signature-checked like an upgrade's; the way across is `k8s reset-db --new-baseline`, run
   by the new release's installer — the reset's wipe, then the new release installed with the
   release's own values. It refuses a baseline it cannot read: a reset never wipes on a guess.

### The Kubernetes shape — `<short>-stow k8s`

The same installer binary drives the operator's own `helm` and `kubectl`, as the operator would:
their kubeconfig, their context, their RBAC. A Kubernetes client library would bring client-go
into a binary every shape carries, for what two CLIs every cluster operator has already do.

- **The chart is pulled in process, by digest**: the release the installer carries names the
  chart's digest, so the installer reads exactly that manifest and its one layer (with the same
  pull account and mirror rules as the Docker shape's image pulls) and hands helm a local
  archive. helm never logs in to the registry, and no tag can change what is installed.
- **The secrets live outside the chart**, in a Secret the installer writes (`<release>-stowage`,
  server-side apply, labelled; one of that name it did not write is never written over): a value
  generated once is never generated again — not by an upgrade, not by a helm rollback (which
  restores the chart's objects only), not by an install retried after `--atomic` removed a
  failed one. The chart reads it by name (`stowage.secret`), as it reads the pull Secret
  (`stowage.pullSecret`). Inputs are recorded there too: Kubernetes has no `.env`.
- **Full values on every upgrade, without the operator repeating them**: helm does not merge a
  release's earlier values into an upgrade, so a forgotten `--values` would reset the release to
  chart defaults. `upgrade` passes `--reset-then-reuse-values` (helm ≥ 3.14): the new chart's
  defaults — which carry the new release's images by digest — then every value given before,
  then this run's.
- **helm rolls back what fails**: `--atomic` (helm 3) or `--rollback-on-failure` (helm 4) with
  `--wait`, gated by the chart's readiness probes, as the VM's health gate reverts a bad OS swap.
  `rollback` is `helm rollback`; a barrier pre-flight before it is an extension (§ 7 row 22).
  With no revision given, helm goes to the revision before the latest, which after a revert is
  the failed upgrade and after a rollback is the release just left; `<short>-stow k8s rollback`
  reads `helm history` instead. After a revision that did not deploy, it restores the latest one
  that did (`superseded`, or `deployed`: a failed upgrade leaves the previous revision deployed).
  Otherwise it goes back one upgrade: the latest revision that deployed below the one the running
  revision descends from (following helm's `Rollback to N` descriptions) whose chart or values
  (`helm get values --revision N --all`) differ from what runs, since a re-applied upgrade with
  nothing changed is a revision too.
- **What runs is read from the release**: the values block records the release's identity, so
  `upgrade` orders the release it brings against the one running (by the recorded scheme), and
  `upgrade --version` finds the channel and the registry it was installed from, with no state
  outside the cluster.

### The installer's terminal interface

The installer is the first thing an operator meets, so it is a real terminal application, built on
Charm's maintained components (MIT): **huh** for forms (inputs, selects, confirmations, masked
secrets, inline validation), **bubbles** spinners and progress for pulls and waits, **lipgloss**
for layout and colour. Raw `read` prompts are not used.

- Every form field comes from the spec (`inputs:` prompt, default, `secret`, `required`, `when`,
  `shapes`), so a project gets a polished installer without writing UI code.
- **Non-interactive is first-class**: every input is also an environment variable and a flag, and
  `--answers <file>` takes a YAML answers file; with no TTY, or with `--no-input`, the installer
  never prompts and fails listing every missing required input at once. The CI path and the TTY path
  write identical files.
- **Accessible mode** (`--accessible`, `ACCESSIBLE` set, or `TERM=dumb`): Stowage's own
  line-by-line prompts (not huh's accessible runner, which reads end of input as an answer), no
  cursor movement, readable by screen readers; end of input or Ctrl-C aborts, Enter keeps the
  answer given; colour follows `NO_COLOR`.
- The same binary is the Docker shape's lifecycle command (`<short>-stow` is a name for it), so a
  re-run takes the first install's flags (`--set`, `--answers`, `--domains`, the front's ports) to
  change what the install recorded.

### Canonical lifecycle command per shape — one verb set

`status health version releases updates changelog upgrade rollback provision
backup{run,list,status,verify} restore reset-db logs ps restart stop start uninstall`
(+ VM-only `network`, `reboot`, `shutdown`).

| Shape | Command | Backend |
|---|---|---|
| VM | `hostctl` (unprivileged, no sudo) → host daemon socket | kairos-agent A/B, health gate, rollback-check, network override, cloud-config replace |
| Docker | `<short>-stow` (the installer binary, installed next to `compose.yaml`) | compose, pre-change `pg_dump` snapshot, `.prev` compose, barrier-aware rollback |
| Kubernetes | `<short>-stow k8s …` | helm OCI `--atomic`, `helm rollback`; data on managed PG / CNPG |

Where a verb does not apply, the command says who owns it ("backups are your managed database's job
on this shape") instead of failing obscurely.

## 7. Full lifecycle matrix

**C** = core. **X** = extension (§ 10). **owner** = deliberately delegated.

| # | Stage | Publisher / Docker | Kubernetes | Kairos VM |
|---|---|---|---|---|
| 1 | Author | `stowage.yaml` + `stowage lint` **C** | same file, `shapes.kubernetes` | same file, `shapes.kairos` |
| 2 | Version + changelog | providers; version from a changelog + CalVer month guard as configuration **C**; revisions, `stowage.lock`, Platform block **C** | chart version from the release sequence § 3.1 | `release.json` |
| 2′ | Upgrade Stowage itself | `stowage lock --update` → diff → `ship --revision` or next version **C** | arrives with the next chart/images | runtime inside the OS bundle, via `upgrade` |
| 3 | Build + ship | graph, mutable dev / immutable stable, `verify` **C**; inputs digest, skip/re-tag **C** | chart OCI push **C** | OS bundle + disk images (raw, qcow2, ova, vhd, gce) + cloud publish (aws, azure, gcp) **C** |
| 3′ | Sign / verify | detached sign + push by Stowage, verify before pull/upgrade **C** (§ 5.1) | manifest + chart verified by `<short>-stow k8s`; optional admission policy (owner: cluster) **C** | `hostd` verifies before staging **C** |
| 4 | Promote / retire | `promote`, `prune --channel dev` **C**; `yank` **C** | same manifests | same manifests |
| 5 | Discover | installer `:latest` + bundled docs **C**; `releases` from manifests **C** | same installer | same installer; disk image per format |
| 6 | Pre-flight | compose ≥ 2.20, ports, registry login, image presence, offline image check **C** | kubectl/helm present, namespace, DB grant SQL printed **C** | values rendered + validated, disk ≥ minimum **C** |
| 7 | Install | inputs → secrets → `.env` → `up --wait`, loopback vs TLS mode **C** | Secret (idempotent) + `helm upgrade --install --atomic` **C**; DB `external` / `embedded` / `cnpg` **C** (the chart's modes; `examples/full` runs the first two) | cloud-config render → NoCloud ISO / disk → boot → preload → secrets generated **C** |
| 8 | First-run provision | `provision` hook, resumable state, credentials file; `provision --set` replaces a value it returned or sets one its `returns` declares **C** | the same hook as a Job after the release is settled, its outputs in `<release>-stowage-provision` (never a log) and handed back; a failure leaves the release running, `k8s provision [--set]` **C** | provision unit + `hostctl provision [--set]` **C** |
| 9 | Reach the portal | prints `front.portal` URL **C** | prints ingress URL **X** | console banner + login issue text **C** |
| 10 | Air-gapped | `--offline` with preloaded images **C**; `--registry` mirror with its CA, filled by `mirror export` / `import` (§ 12) | `--image-registry` mirror **C**, with its CA | first boot is registry-free **C**; mirror and CA from cloud-config for updates and upgrades |
| 11 | Observe | `status`, `health`, `logs`, `ps`; liveness ≠ readiness **C** | `status` **C** | `hostctl status/health/ps/logs/version`, console-info timer **C** |
| 12 | Reconfigure | installer re-run is an idempotent reconcile **C**; it changes what it is given — `--set`, `--answers`, `--domains`, the front's ports, its TLS mode (between HTTPS modes) and `byo` certificate — and brings the stack up again; advanced inputs (`advanced: true`) are tunables only `--set` records | `upgrade` with full values **C** | `reconfigure-userdata` (+ `.prev`) then reboot **C** → `sudo <short>-stow install --set …` over SSH, the Docker re-run; the settings file is the first install's |
| 13 | Network / SSH | owner: host | owner: cluster | `<short>-stow network` set / confirm / revert / reset, an addressing change undone unless confirmed in its window (also at a daemon restart or a reboot inside it) **C**; SSH key-only, the admin group **C** |
| 14 | TLS | Caddy acme / dns / byo / internal / none, auto-renew, certs survive reset-db **C** | owner: ingress + cert-manager **C** | same front as Docker **C** |
| 15 | Backup | pre-upgrade snapshot **C**; scheduled backup **X** | owner: managed PG / CNPG **C** | timer, pg_dump → age → rclone, retention, `backup config/run/list/status/verify` **C** |
| 16 | Restore | manual from snapshot **C** → `restore <file>` **X** | owner: managed PG PITR | `restore OBJECT`: a host daemon job, the backup streamed from the destination, decrypted with the key given on stdin, `pg_restore` in one transaction, the services but the database stopped **C** |
| 17 | Check for update | `updates` from manifests **C** | same **C** | `hostctl updates` from manifests **C** |
| 18 | Read changelog | project + Platform sections current → target **C** | same | `hostctl changelog new` **C**, source becomes the manifest |
| 19 | Upgrade pre-flight | mandatory snapshot **C**; yanked / `min_upgrade_from` / barrier warning **C** | same checks **C** | pre-upgrade backup **C**; same checks **C** |
| 20 | Upgrade | compose file moves with images, new `.env` keys merged, `up --wait` **C** | `--atomic`, gated by readiness **C** | kairos-agent stages passive slot while serving; reboot now or later **C** |
| 21 | Health gate / auto-rollback | failure → prints the rollback command **C** | helm `--atomic` reverts **C** | health gate: deadline, all-healthy, diagnostics, grub fallback, loop guard; kept at a rollback barrier **C** |
| 22 | Manual rollback | barrier + reads-newer check, else points to the snapshot **C** | `helm rollback` **C** + barrier pre-flight **X** | `hostctl rollback`, rollback-check refuses only on proof **C** |
| 23 | Reset data | `reset-db`: stack down, the `data.reset` volumes deleted (`keep_on_reset` kept), provisioned as first installed **C** | `k8s reset-db`: uninstall, the release's claims deleted, installed again with its values and Secrets **C** | `reset-db`, a host daemon job, the Docker shape's, no reboot **C** |
| 24 | Rotate secrets | `secrets rotate KEY` for `rotation: true`, three passes (`--stage` → `KEY_NEXT` accepted; swap → `KEY_PREVIOUS` accepted; `--finish`), the stack up again at each **C** | `k8s secrets rotate`: the same passes on the Secret, each workload's rollout restarted and awaited **C** | the Docker shape's verb, a host daemon job **C** |
| 25 | Registry credential rotation | `.env` / installer flag **C** | pull Secret **C** | sealed env + `reconfigure-userdata` **C** → the pull account the OS bundle carries; a mirror's is root's Docker login on the VM |
| 26 | Power / maintenance | `stop` / `start` **C** | owner: cluster | `hostctl reboot/shutdown --drain-secs`, disk-pressure hook **C** |
| 27 | Uninstall | `uninstall [--keep-data]` **X** | `k8s uninstall`: `helm uninstall`, the installer's Secrets, the hook's account and Job; the claims only with `--data`, kept data keeping the Secrets that answer for it, a chart's own claim (which helm deletes) needing `--data` **C** | `vm destroy` (a VM `vm create` made with OpenTofu) **C**; else owner: hypervisor |
| 28 | Move host / disaster recovery | `secrets export` on old → `install --secrets` on new **C** (data: the snapshot by hand) | owner: managed PG; secrets **X** | `secrets export`, then `vm create --secrets` + `restore OBJECT` **C** |

Rules:
- A verb has the **same name** on every shape where it applies; elsewhere the owner is named.
- Every destructive verb (`rollback`, `restore`, `uninstall`) takes a snapshot first where
  data is local, and refuses only on **proof** of incompatibility — an unreadable database or a check
  that did not run lets the operation proceed (the rollback-check rule).
- `reset-db` means "back to the first setup", on every shape, and is kept simple: it is asked for
  when the data is not wanted, so it takes no snapshot (a confirmation instead) and runs at once,
  with the service down. The initial seed is what a first install runs: the release's migrations
  on an empty database, then the provision hook from the recorded inputs and secrets — so the
  hook's own record (`provision.env`, `outputs`) is forgotten, or it would answer for data that is
  gone. On the VM the host daemon is not in the stack, so no reboot is needed (a reboot would only
  matter combined with an upgrade: `upgrade --reset-db`, later). On Kubernetes the chart owns its
  storage: the release is uninstalled and installed again with the values it was given, the
  claims its manifest makes or its StatefulSets claim deleted between, the installer's Secrets
  kept; no label contract is asked of the chart.
- Disk pressure is the VM's alone: its disk is fixed at creation, while a Docker host's is the
  operator's and a cluster's storage the cluster's. The daemon tells the level, not a policy: the
  body is `{level, used_pct, soft_pct, hard_pct}`, and what to purge, or for how long to keep data,
  is the project's endpoint's to decide. It reaches the service as the health gate does, at its
  container's address on the stack's network, so the service publishes no port for it; a POST
  not taken is tried again after a minute (at boot the daemon starts before the stack), not at
  once, and nothing is persisted (the reading is the
  disk's, the last POST is the daemon's memory).
- The VM's network override has one record, a state file on the persistent partition (the
  override, and the change awaiting confirmation with the settings before it); networkd's file is
  rendered from it — at boot by a unit ordered before networkd, which first undoes a change never
  confirmed, and by the host daemon at each change — rather than kept as a cloud-config fragment
  on `/oem`: the record is strictly decoded, and the boot never runs networkd on a change nobody
  confirmed. It sorts before the cloud-config's static file, so it outranks it without editing
  it. A change is recorded and its window armed before networkd sees it; a revert or a restore
  that fails is retried (15 s, 5 times), and every change disarms what is still pending, so no
  stale retry undoes a later change. The change is applied on the daemon's context, not the
  request's: the change itself may cut the client's session. A record that does not read sets
  nothing, and `network reset` drops it. Beside its networkd file the cloud-config writes the
  network `vm create` gave as data (`/etc/<name>/install-network.yaml`, DHCP included), which the
  daemon answers so an app editing the network starts from what the install set, rather than
  parsing networkd's file back. The file decides what runs and so the answer's `source`; the
  record only describes it, and one that does not read is left out of the answer (logged), never
  failing it. The cloud-config is frozen at `vm create`, so the record's fields may only be added.
  Addressing is IPv4, static or leased. IPv6 is opt-in (`--ipv6`) and,
  once on, on par: off, the link has none at all (no link-local address, router advertisement or
  DHCPv6), so a network the operator never planned for IPv6 never addresses the VM over it, and
  the cloud-config writes its network file even for DHCP, as Kairos's own file would take router
  advertisements. On, networkd takes the router advertisements and, with DHCP, starts its DHCPv6
  client at once (`WithoutRA=solicit`) rather than on a managed flag: the clouds hand out the
  address by DHCPv6, and a LAN without a DHCPv6 server still gets SLAAC. One static IPv6 address
  and gateway may sit beside the advertised ones, checked as the IPv4 pair is (a link-local
  gateway is on any link); an IPv6 DNS server needs IPv6 on. All of it is on the VM's one wired
  interface, and one networkd section renderer serves the override and the cloud-config's file.
  An IPv6 address counts as applied only once duplicate address detection passed, so an address
  another host holds is undone like one networkd never applied; the IPv6 default routes are read
  from multipath routes too (the kernel merges a static and an advertised route of one metric).
  The record carries a format version only when an older release would read it otherwise: IPv6 on
  without a static IPv6 address is written in the older format (no version, no `ipv6` key), which
  reads with IPv6 on — so a rollback keeps that override — and one of a later version does not read. Its keys are strictly
  decoded: a release without `ipv6` reading a record with it (a rollback) sets nothing, and the
  cloud-config's network comes back.
- The stack's IPv6 (`install --ipv6`, `vm create --ipv6`): Docker's userland proxy carries an
  IPv6 client to a container without IPv6, but as the bridge's IPv4 gateway, which defeats the
  front's source allowlists and `forward_auth`. `--ipv6` turns IPv6 on for the default network
  (Docker ≥ 27 allocates a unique local /64 and NATs IPv6 with ip6tables), so the client's address
  reaches Caddy, which listens on every address of both families. Compose never changes an
  existing network everywhere (Compose 5 does; 2.20 need not), so with `--ipv6` the default network
  carries a label, and a run whose choice differs from it stops the stack first (orphans too, which
  would hold the network); the label, not the network's IPv6, decides, so IPv6 the project gives
  the network never loops, and without `--ipv6` the project's network definition (an external one
  included) is left alone.
  The engine's ability is probed before anything is written (a throwaway network). Off by default: a host without IPv6 cannot create an IPv6 network. The backup runner
  runs on the host's network, so it reaches a destination and DNS servers as the VM does, IPv6
  included, without needing the stack's choice. The app socket serves these verbs too, as the whole API (the
  operator's choice: the app's admin pages change the network as `<short>-stow` does).
- `/health` is always-200 liveness; every readiness consumer polls the `health` provider.

## 8. Runtime pieces (generic, parameterised by `name` + spec)

| Piece | Built from | Behaviour |
|---|---|---|
| Installer / `<short>-stow` | one static Go binary (linux/amd64; arm64 later) carried by the installer image, the release payload appended at ship time; terminal UI on Charm huh / bubbles / lipgloss | one binary, three shapes, interactive or fully non-interactive |
| Front | Caddy 2.11.7 + the Cloudflare DNS plugin, from a Dockerfile embedded in `stowage` (bases pinned by digest, never 2.11.6), built by `ship` into the project registry | Caddyfile generated by the installer from `front.routes` × TLS mode (no entrypoint script); per route, in one ordered `route` block: the source allowlist (an input of CIDRs, empty refusing everyone: a scraper's route needs no port of its own), header stripping, then `forward_auth` (its address an input, off while empty; the answer's headers copied after the strip, so a client never forges an identity); the values the front reads are checked when given and written into the Caddyfile, and the front carries the digest of its configuration as a label, so a change recreates it; `publish_ca` copies the local CA's root (or byo's `--ca`) into a project volume from the front's own container (a shell loop before `exec caddy`, as the root exists only once Caddy started), not from a second service `up --wait` would have to tolerate exiting; HSTS except `internal`; domains are the routes' `domain_input` inputs (`--domains` is shorthand), checked at install; Stowage owns the ACME contact (`STOWAGE_ACME_EMAIL`, refused when malformed, never silently dropped); a byo certificate is checked against every domain at install; `dns-cloudflare` takes its token as a secret given to the front's container (not to the project's services or the hook; a stack that reads `.env` itself sees it), checked in the form the plugin accepts, since the plugin's own refusal prints it; one issuer per mode |
| Backup | the host daemon (pg_dump inside the database service, age in Go) and a runner image (rclone, from a Dockerfile embedded in `stowage`, base pinned by digest, built by `ship` into the project registry for a `kairos` shape with a `pg-age-rclone` backup, preloaded by the bundle) | the dump streamed through age to rclone `rcat`, never on the VM's disk; the size reached checked; retention by `rclone delete --min-age` on the profile's own file names; settings (destination, rclone config, recipients, retention, schedule, profile) the operator's, on the persistent partition, set over SSH; the timer's schedule a runtime drop-in; a backup before each upgrade, warn-only |
| Host daemon (`hostd`) | Go, in `stowage`: `<short>-stow hostd`, a systemd service on the VM (not a privileged container) | JSON over HTTP on a unix socket (`root:admin` 0660): status, services, logs, service actions, provision, updates, upgrade (kairos-agent writes the release's bundle, by digest, to the passive slot; then a reboot), reboot / shutdown after a drain; one job at a time; the same API on an app socket, behind the install's token, mounted only into the services `shapes.kairos.host_api` names. health, rollback to the passive system (a job: the boot assessment marked, then a reboot), reset-db (a job, no reboot), the network override (set / confirm / revert / reset, its revert timer), backups (settings, a job, status, list, verify — a job too), an event journal in the install (jobs started, ended or lost, reboots scheduled and cancelled, boots and the health gate's verdicts, the first install's end; appended by the daemon and the gate under one lock, numbered for the journal's life, read with a long poll), the disk-pressure watch (the persistent partition read at start and every 10 min; at `soft_pct` a POST to the hook's service at its container address, bearer `auth_secret`; past `hard_pct` a console banner) |
| `hostctl` | the VM verbs of `<short>-stow` | client of `hostd` for root and the `admin` group, names derived from `name` |
| VM install | `<short>-stow` in the OS bundle, the release it carries without its publication | the Docker shape's install from the preloaded images (by digest tag) into the persistent partition; at every boot the same release comes up again, the release the last upgrade replaced goes back (with the install's own, newer `<short>-stow`), any other is upgraded to the OS's release |
| Health gate | Go + systemd | deadline / all-healthy / diagnostics / Kairos boot assessment for the fallback / loop guard; probe from the `health` provider |
| rollback-check | shell | record / check / exit 3 on proof; queries from the `schema compat` provider |
| VM base | the OS bundle's Dockerfile and files, embedded in `stowage` (`internal/kairos`): kairos-init, Docker, preload, expand, sshd, sudoers, console; then bootstrap, apply, nftables, console-info, units | paths and units derived from `name`, volume ownership from `data.volumes` |
| Cloud-config | template + values + render + seal + cidata | generic sections (vm, network, secrets, registry, TLS, backup) + `inputs` |
| Helm library chart | named templates | secret `lookup`-reuse, PG modes external / embedded / cnpg, two-URL contract |

**Stays in the application** (as hooks or project files): the provisioning wizard calls, backup
profile table lists, the maintenance endpoint, application env blocks, the
migration barrier naming convention (exposed through the `schema compat` provider), the Helm chart.

## 9. Repository layout (Stowage itself)

```
cmd/stowage/            CLI entry point
internal/cli/           commands and output
internal/spec/          stowage.yaml types, strict decoding, the Provider shape
internal/lint/          lint rules, compose parsing
internal/version/       version sources, schemes, channel tags
internal/changelog/     changelog providers
internal/command/       the `command` provider contract
internal/providers/     runtime-side provider configs (auth, signing, health, schema, provision, backup)
internal/digest/        inputs digests
internal/graph/         build graph, skip/re-tag
internal/registry/      OCI client (go-containerregistry / oras-go), manifest, channel pointers
internal/helm/          chart load (.helmignore), checks, packaging with the release's values
runtime/installer/      installer + <short>-stow binary (Go; terminal UI on Charm huh/bubbles/lipgloss)
runtime/ctl/            <short>-stow templates
internal/front/         the front's Dockerfile (embedded)
internal/backup/        the VM's backup runner's Dockerfile (embedded)
internal/kairos/        the OS bundle's recipe (Dockerfile, files) and the disk image builder
runtime/hostd/          host daemon (Go) + tests
runtime/vm/             systemd units, health gate, rollback-check
runtime/cloud-config/   template, render, seal, cidata
charts/stowage-lib/     Helm library chart
examples/               minimal, full
tests/                  lifecycle smoke per shape
```

## 10. Extensions

| Extension | Matrix row | Size | Why |
|---|---|---|---|
| K8s portal URL | 9 | S | parity with Docker |
| scheduled backup on Docker | 15 | S | the backup container is already generic (deferred, § 12) |
| `restore` verb on Docker, Kubernetes secrets export/import | 16, 28 | M | the VM has both; Docker restores by hand |
| K8s barrier pre-flight before `helm rollback` | 22 | S | parity with Docker and VM |
| `uninstall` (Docker) | 27 | S | `down` and removing the volumes by hand |

## 11. Phases

Progress: phase 1 has `lint`, `project-version`, `changelog`, `plan`, every version and
changelog provider, the inputs digests, and the registry side for images and the release manifest
(`ship`, `verify`, `promote`, `yank`, `prune`, `releases`, tested against zot by `make e2e`),
signing with a cosign key, and the Docker shape's installer (`install`, re-run, `status`,
`provision`, `upgrade`, `rollback`, every TLS mode), published by `ship` as the installer image
with the front it runs, and `upgrade --version`. `make e2e-docker` walks it with the real binaries
against zot and the local Docker engine: the bootstrap one-liner, a re-run, `upgrade --version` to
two later releases, a rollback the schema check allows, and one a rollback barrier refuses;
`make e2e-full` walks `examples/full` the same way on the Docker shape (CalVer, token pushes, a key
from the environment, the `sql` schema check against a real Postgres, a provision hook's outputs,
an extension's input set and cleared) — the parity proof on a project shaped like the production one.
`stowage lock` pins the publishing `stowage`, and `stowage hook post-commit` ships a commit that
moves the version. `ship` publishes the Kubernetes shape's chart (one repository per channel,
images by digest, signed, verified, promoted and pruned with the release), and `<short>-stow k8s`
installs it (`install`, `upgrade`, `upgrade --version`, `rollback`, `status`); `make e2e-k8s`
walks it on kind, the cluster pulling from zot with the pull Secret, and `make e2e-full-k8s`
walks `examples/full`'s chart there (an external database by secret inputs, an embedded one
`reset-db` wipes, migrations under a lock with two replicas, the provision hook a Job the
installer runs after the release, its outputs handed back, a failure leaving an upgrade running),
and `make e2e-full-vm` installs it on a TrueNAS VM from a mirror (its upgrade and rollback are the VM machinery's, which `make e2e-vm` proves). `ship` builds the Kairos
shape's OS bundle (kairos-init on a pinned base, Docker, the release's images preloaded) and its
disk images (raw, qcow2, ova, vhd, gce; `cloud_publish` adds the format each cloud imports — AWS
raw, Azure a fixed VHD, Google Cloud a `disk.raw` tar.gz), keeps them across ships and re-tags them on a
promotion; `make e2e-kairos` builds them for real against zot. Only the raw disk comes from
auroraboot; every other format is converted from it by the clouds' documented recipes (qemu-img,
tar), so one auroraboot run serves them all and each format's conformance is checked by `ship`,
not taken on trust. auroraboot, a privileged third-party image, holds no registry credential: `ship`
serves it the bundle read-only on a loopback port (GET and HEAD of that manifest, by digest, and
its repository's blobs), the proxy holding the publisher's account (pull-scoped where the
registry issues tokens) and asking auroraboot for an account of its own, a one-run random password
in a private Docker config file — a loopback port is open to every local user, and a command
line is readable by them. So the disks build from the registry whoever else may read it, and the
publisher's account never enters a container. `ship` registers each `cloud_publish` disk in the
publisher's account of that cloud through built-in OpenTofu modules per cloud — an upload (bucket
object, S3 object, page blob), the image made of it (Compute Engine image; import-snapshot → AMI;
Azure image), then the upload destroyed, since a disk may carry the pull account and a bucket is
often read more widely than the images — with the publisher's credentials from the environment,
which nothing in a release carries. The modules pin each provider's exact release with its lock
file's checksums, and OpenTofu runs with an empty CLI configuration, so the provider binary that
receives the cloud credentials is the one checked. OpenTofu,
not the clouds' SDKs: the providers already pinned for `vm create` know each cloud's upload and
import, and adding three SDK trees to `stowage` would only repeat them. The disk is read back from
the registry by digest, so the image is exactly what an install fetches; the image is named by
that digest and its OpenTofu state and id kept in the publisher's cache, keyed by the cloud's
settings; without them, a probe module looks the image up by name in the account, so it is
registered once whatever machine ships after, and a stopped run resumes. The release manifest does not record it: an
image lives in one account, and an install takes the disk. The bundle carries `<short>-stow`:
it installs the release on the VM from the preloaded images, brings it up, upgrades it or goes
back at boot, and serves the host daemon (status, services, logs, provision, updates, upgrade
through kairos-agent, reboot); `make e2e-kairos` runs it on the local Docker engine, including a
boot that upgrades and one that goes back. `<short>-stow vm create` writes a new VM's disk (by
digest, in its platform's format) and its cloud-config (a NoCloud seed ISO, or a cloud's
user-data), whose settings the first boot installs from; the services the spec names reach the
host daemon on its app socket with a token. With OpenTofu, `vm create` creates the VM on TrueNAS
and waits until it has installed, and `vm destroy` removes it; `make e2e-vm` does both on a real
TrueNAS host. The other canonical platforms (libvirt, Proxmox VE, vSphere, AWS, Google Cloud,
Azure) have built-in modules as previews, and any platform takes the operator's module holding the
same contract (`--module`). Next: the mirror for sites without internet (`mirror export` / `import`, a CA on
every shape, the VM end-to-end test upgrading through it), then the health gate.

| Phase | Scope | Proof |
|---|---|---|
| 1 | CLI: spec, lint, providers, graph, ship / verify / promote / yank, release manifest. Docker shape: installer + `<short>-stow` with every **C** verb | `examples/minimal` walks every Docker row of § 7 on a clean host (lifecycle smoke) |
| 2 | Kubernetes lifecycle + installer; Kairos bundle, hostd, hostctl, health gate, cloud-config | the same walk on kind and on a Kairos VM, including a forced health-gate rollback and a refused barrier rollback |
| 2b | accepted **X** items | each added to the lifecycle smoke walk |
| 3 | Adoption proof: `examples/full`, a working stack shaped like a production project (stand-in services migrating a real Postgres through two roles, a provision hook, an extension it takes the URL of, signed CalVer releases with token pushes, external database URLs on Kubernetes) | its Docker, Kubernetes and VM install, upgrade and rollback end to end (`make e2e-full` for Docker, `make e2e-full-k8s` for Kubernetes, `make e2e-full-vm` for the VM) |

## 12. Decisions

- **Signing** — a dedicated release key; cosign pushes signatures as referrers with the publisher
  token (§ 5.1).
- **Registry auth** — no mTLS: a pull account + a publisher token (API key), standard login only;
  reference registry zot (§ 5.2).
- **Optional components are extensions, not compose profiles** — an input turning a profile on
  made every optional component part of the core release: its images in every release and OS
  bundle, its sizing on every VM, its toggle in every chart. An optional component is its own
  project (`extensions:` in the host, `endpoint:` in the extension), published beside the host
  under its own name; the host takes its URL as an ordinary input, so configuring it is all the
  host's install owes it. The tip a host install ends with comes from the spec, so an offline
  install shows it; the registry is only asked whether the extension is published, under a short
  bound, and only its answer that it is not is said. On Kubernetes the endpoint is derived from
  the route's upstream (the chart exposes `<release>-<service>`), and it is given at the install
  (an upgrade keeps a recorded input). Installing the extension from the host's installer is later work.
- **Docker backups** — deferred; revisit when phase 1 shows its cost (stays in § 10 as optional).
- **Licence** — Apache License 2.0 (`LICENSE`). Dependencies used as binaries or libraries are
  compatible: Kairos, auroraboot, Caddy, Helm, oras, go-containerregistry, cosign, sigstore-go (Apache-2.0),
  age (BSD-3), rclone, qemu-img (GPL-2.0, run as a binary in its own image),
  PostgreSQL (PostgreSQL licence), go-diskfs (MIT), coder/websocket (ISC), OpenTofu and the
  `pjsalty/truenas` provider (MPL-2.0, run as the operator's binary and a provider it fetches).
- **Kairos VM** — the host daemon is Go inside `stowage` (one static binary, the same gates), run
  as a systemd service rather than a privileged container; the fallback after a bad upgrade is
  Kairos boot assessment, which the health gate marks good, rather than editing grub's
  environment by hand. The daemon's snapshots (status, the last job) cannot tell a client what
  happened between two reads across a reboot, so both writers also append to an event journal on
  the persistent partition, which a starting daemon closes (jobs lost, reboots that never came)
  and which a client follows from the number status gives. A job that reboots at its end
  journals that end, carrying the reboot, in the same append as the reboot's entry and before the
  reboot runs: one answer says the appliance is about to restart, and a job is lost only to a
  reboot it did not ask for. A client tells a job by its start (every 202 carries it), never by
  kind and target. The VM's verbs follow a job to its end and stop there, at the reboot a job
  done scheduled: they run on the VM, which that reboot stops, so following it would only end in
  being killed by it, and any verdict they gave in between (the daemon gone, a signal) would be a
  guess. The reboot's outcome is the journal's — another boot's `boot.started`, a
  `reboot.cancelled` naming the job (a rollback's says whether the boot assessment was put back),
  the gate's `boot.judged` — on the VM, read after the boot by `status` and `events` (`events
  --follow`, run on the VM over its socket, ends with the reboot). The API is served only on the
  VM, on its operator socket and the app socket mounted into the stack's services: such a service
  (and what it serves to clients off the VM) reads `GET /v1/events` again once the VM is back, from
  the number it held, the journal having kept its entries across the reboot. A job's end or loss once read is the answer whatever happens
  next — a signal that stops the follow, or any failure of the next request, as a daemon torn
  down by the reboot may answer an error or a cut body before it stops answering (done is done,
  failed fails, lost is lost); an interrupt before it is never an outcome. Each request, the
  follow's and `events --follow`'s, is bounded by its wait and a margin (a daemon that takes a
  request and never answers is one that does not answer). A job's end in the journal answers a
  follower whose job was replaced in the slot, or whose daemon stopped, before it read the job
  again; the daemon serves only its current job's log, so such an answer says the last lines were
  not read. The line a verb ends with counts the drain down only when `GET job` told the end: an
  end read from the journal was written some of the drain ago. It never says the reboot is under
  way: a daemon that stops answering after the end was read may be one whose request bound
  expired, or one restarted in the drain (which journals `reboot.cancelled`). The boot a reboot leaves is its
  `reboot.scheduled`'s, the boot a job ran in its `job.started`'s, and two boots are told
  apart only by two ids read — an id not read is never another boot, nor the same one. A job's
  start and a reboot's scheduling take their boot id from the very record their append leads
  with, so an entry never names another boot than its append's `boot.started`. A status, or a
  202, whose journal could not give a number gives none (`events` null, `events_error` why)
  rather than 0, which would replay the journal as new; a follower then reads the job's log alone
  and says so.
  A long-lived writer (the daemon, the gate) keeps its boot's record once a read of it succeeds,
  and reads again after a failed one, at most once a minute (the read touches several files, and
  every append asks for it); the boot id and Kairos's marks are read where the
  installer's options say, for the daemon and the gate too. The scheduled backup reads its
  settings once the slot it waited for is free, woken by the running job's end, and starts only
  at the slot generation (claims and ends counted) it read them at, else reads them again. Every writer that knows its boot puts the boot's
  `boot.started` before its first entry in it, and the journal's state keeps the last boot
  recorded and the floor a rotation leaves (written before the rewrite: over-claiming entries
  gone is safe, the reverse is not), so neither depends on the entries a rotation keeps; an
  answer carries the floor, and a client moves past it, so a truncated answer cannot be asked
  for twice. The scheduled backup waits (bounded) for a running job rather than lose its day to
  a short one (a verify). A check that may outlast a client's
  deadline (backup verify) is a job, so its answer is never lost to a hop that gives up. A VM is created by `<short>-stow` with OpenTofu (a built-in module per
  canonical platform, TrueNAS's proven and the others previews, or the operator's; sizing asked,
  the disk and the cloud-init seed given to `tofu apply`) or by hand.
- **Kairos release on the VM** — the bundle is built before the release is published and serves
  every channel it is promoted to, so its `<short>-stow` carries the manifest without its
  publication (`bundled.json`: no channel, tag, number, date, yank or artifact beyond the images),
  and the channel a VM follows is recorded at its install. The VM's install is the Docker shape's,
  run at boot against the OS's release, so upgrade and rollback have one implementation: an A/B
  upgrade's boot upgrades the install (snapshot, kept previous release); a boot of the replaced
  release goes back with the newer `<short>-stow`, which knows the newer schema. Choosing the slot
  (going back by hand, the fallback after a failed boot) is the health gate's.
- **Health gate** — Kairos's GRUB boot assessment decides the slot; the gate is its judge. Stock
  Kairos marks an upgraded boot good as soon as userspace starts, which catches a kernel that
  does not boot but not a release that does not become ready, so the bundle installs Kairos's
  `08_grub.yaml` without that step (checked against kairos-init's file by hash at build) and the
  gate clears the marks once the install is healthy; unhealthy, it reboots and GRUB falls back.
  The trial is GRUB's own mark (enabled *and* attempted), so a boot GRUB did not mark never
  reboots: no loop. The fallback sticks (the marks stay) until the next upgrade, whose job clears
  the attempt so the new system gets its own trial. `rollback` by hand sets the same marks and
  reboots, rather than a one-shot `next_entry`: a one-shot would boot the newer system again at
  the next reboot, the OS and the install then apart. Before falling back, the gate runs the
  rollback the passive system's boot would run, dry: at a rollback barrier (or a previous release
  that does not read the newer schema) that boot would refuse, and nothing would serve, so the
  gate keeps the failed system — marked good, the reason recorded with its diagnostics — a
  degraded release being better than none. The probe reaches the health service at its
  container's address from the host, on a network that is not `internal: true` (the host has no
  route into one), so no probe image is needed.
- **Firewall** — nftables, as the OS bundle starts Docker after it. The ruleset is a table of the
  project's own, flushed by itself, never `flush ruleset`: Docker's iptables-nft chains live in
  their own tables and a reload must not drop them (nor may `nftables.service` restart, whose stop
  flushes everything). Input drops by default; Docker's published ports are forwarded traffic,
  which it leaves alone, so the front's ports are opened in input for what reaches the host
  itself (the userland proxy), from a runtime file each start writes, as the ports are the
  install's, changed by a re-run. SSH's sources are the cloud-config's (`--ssh-allow`), a file
  replacing the bundle's open default, so a VM made by hand is never locked out.
- **Creating a VM** — one entry, `<short>-stow vm create`, in the order an administrator decides:
  the platform (it decides the disk format, the cloud-config's medium and whether OpenTofu can
  create the VM), then the machine (host name, administrator and SSH key, network), then the
  product (the install's own form, its TLS mode and inputs, without secrets), then a review. The
  medium follows the platform and is never asked: a NoCloud seed ISO for a hypervisor (Kairos's
  cdrom datasource), the user-data field for a cloud. The cloud-config holds only Kairos stages
  (applied at every boot, as `/etc` is not kept; the host name in `initramfs`, before networkd
  sends a DHCP request, the rest in `boot`) and a settings file the boot installs
  from, so the first install is the operator's `install` with no terminal; the VM's boot waits
  for the cloud-config's stages. The administrator logs in by SSH key only and has passwordless
  `sudo` (the cloud images' default): no password ever travels in user-data. Secret inputs are
  never in it: the install waits for them, given over SSH.
- **Creating a VM with OpenTofu** — `<short>-stow` embeds a module per canonical platform:
  TrueNAS (proven by `make e2e-vm`), and as previews libvirt/KVM, Proxmox VE and vSphere on
  premises, AWS, Google Cloud and Azure in the clouds — each uploading the disk the platform takes
  and the cloud-config (seed ISO on a hypervisor, user data on a cloud), creating and starting the
  VM, and destroyed by `vm destroy`. A preview is a module not run on its platform yet: checked by
  `tofu validate` against its pinned provider only, marked in README and in `vm create`'s output,
  which names the manual steps as the fallback (by hand is its default on a terminal); it loses
  the mark at its first passing run. Any other platform is the operator's own module
  (`--module`), holding one contract: the variables `vm create` writes (name, size, the disk and
  the seed, their SHA-256; the administrator and key when declared), the outputs it reads (`id`,
  and `address` for a platform that only tells the address after the apply, as clouds do), and
  `$STOWAGE_CTL` for steps the provider cannot declare. A platform's own settings are module
  variables (`--tofu-var`, asked from the module's own descriptions), never a credential: each
  provider reads those from the environment, so none enters the variables or the state. `vm
  create` asks "OpenTofu or by hand" and, with OpenTofu, sizes the VM after the product's
  questions, applies the module, and waits over SSH until the VM has installed.
  `tofu` is the operator's own, on `PATH`: OpenTofu is a tool operators install and upgrade, not
  one a product should vendor. The provider (`pjsalty/truenas`, community, over TrueNAS's JSON-RPC
  API) is pinned exactly. What it cannot declare — the bytes of the disk and the seed, power,
  the snapshots that block a delete — are steps of `<short>-stow` itself (`vm truenas …`, Go, the
  JSON-RPC API and `/_upload/`), run by local-exec with `STOWAGE_CTL`: no Python, no
  `genisoimage`, nothing to install beyond `tofu`. The disk comes from the registry by digest
  through `<short>-stow` (the pull account it carries), never a URL OpenTofu fetches, and is checked
  again against its SHA-256 before the upload. The API's address and key are read from the
  environment by the provider and the steps alike, so no credential enters the variables or the
  state. Each VM's module, variables and state live in `$XDG_STATE_HOME/<name>/vms/<host>/` (an
  operator's own state, like any OpenTofu root), and `vm destroy` removes the VM and that
  directory. The wait is SSH plus `<short>-stow status --json`, not an HTTP probe: SSH proves the
  administrator's access, and the status the install itself.
- **The app's access to the host** — the daemon serves its whole API (the reference app drives
  status, updates, upgrade and power from its console) on a second socket, which only the
  services the spec names mount, in a directory a container cannot write; every request carries
  a token generated on the VM and recorded with the install's secrets. A socket rather than a TCP
  port on a compose network: nothing listens on the network, and no service the spec does not
  name can reach it.
- **VM backups** — the dump is taken inside the database service, so `pg_dump` is always the
  server's own version and the runner carries no PostgreSQL client; age encryption is done in Go
  by the host daemon (filippo.io/age), to public recipients only, so no key on the VM decrypts a
  backup; the runner is rclone alone, a Stowage recipe built by `ship` like the front and
  preloaded, so a VM without internet runs it. The dump streams through the pipe and never lands
  on the VM's disk; a backup counts only when the destination holds the bytes sent. The settings
  are host-owned, on the persistent partition, set over SSH (or the app socket): the rclone
  config holds credentials, which a cloud-config must not carry. Retention prunes by the
  profile's own file names (`<name>-<profile>-<UTC time>-…`, a profile being letters and digits),
  never another project's in a shared destination. The backup before an upgrade warns rather
  than refuses, as the boot dumps the database locally before it migrates anyway. `restore`
  streams the object back through the runner and age into `pg_restore --single-transaction`
  loading a new database beside the live one (owners and privileges kept), swapped in by renaming
  once it loaded whole: nothing on disk, nothing left of the replaced database, and a failure
  leaves the database as it was. The provision hook runs on the
  restored data; an install given `--secrets` waits for it before running the hook at all. The key comes on standard input
  for the one run and is never kept: the VM holds no key that decrypts a backup. A backup
  restores onto the release it was taken from (its name carries it), whose schema it holds.
- **No internet (air-gapped sites)** — a site without internet runs its own registry, a mirror
  with its own certificate authority. A release crosses the gap as one file:
  `<short>-stow mirror export` pulls the channel's latest release with the pull account — its
  manifest and signatures, every image, the installer, the chart, the OS bundle and its disks,
  all by digest — into one OCI-layout archive, and `<short>-stow mirror import <file> --to
  <registry/prefix>` pushes it into the mirror with the site's own credentials, by digest, under
  the tags it was published with. Digests do not change, so the signatures still verify and the
  mirror needs no re-signing. The operator, who has `<short>-stow` but not `stowage`, does both.
  Every shape pulls from the mirror and trusts its CA, given at install time: the Docker
  installer's `--registry` and the Kubernetes `--image-registry` with a CA file; the VM's
  cloud-config settings carry the mirror and its CA (a public certificate, not a secret), which
  the boot adds to the VM's trust store at every boot, so `<short>-stow`, the host daemon and
  `kairos-agent upgrade` pull from it. `vm create` downloads the disk from the mirror as well.
  The first install needs no registry at all — the Docker shape with `--offline`, the VM because
  its images are preloaded — and updates and upgrades come from the mirror.
  The archive is the standard OCI image layout as one tar (`oci-layout`, `index.json`,
  `blobs/sha256/`), so skopeo and oras read it too; each top-level manifest names its tag
  (`org.opencontainers.image.ref.name`) and its repository under the prefix
  (`dev.stowage.repository`) — the layout's own annotation holds a tag only, and one archive
  spans several repositories. Export streams blobs straight into the tar and import reads the
  tar in place (one pass for the offsets, then section reads), so neither needs room for a
  second copy of a multi-gigabyte disk; both check every blob against its digest. The CA is
  trusted where each client reads it: `<short>-stow` adds it to the system's roots for its own
  reads; the Docker installer records it in the install and writes it where the Docker daemon
  reads a registry's CA (`/etc/docker/certs.d/<host>/ca.crt`), since the daemon pulls the images —
  refusing, with the root command, when it may not, and never replacing another CA there; the
  VM's cloud-config adds it to the OS trust store (`update-ca-certificates`) at every boot, which
  `kairos-agent` and the host daemon read; on Kubernetes the nodes pull, and their runtime's
  trust is the cluster owner's, so the installer takes the CA for its own reads only. OpenTofu's
  provider comes from a provider mirror the operator configures (documented, not carried in the
  archive: it is the operator's tool, not the release's).
- **Secrets** — the same rule on every shape: `<short>-stow` generates the generated secrets
  (`secrets[]`) at the first install, on the target, and they never travel; the secrets an
  operator supplies (`secret: true` inputs: a mail provider's password, a third party's API key)
  are given to the install (form, `--set`, `--answers`; on the VM over SSH). Both stay in the
  shape's own store in clear, guarded by permissions: the `.env` (root-only, 0600; on the VM on the
  persistent partition), the release's Secret on Kubernetes (etcd encryption is the cluster
  owner's). No secret travels in cloud-config, which carries the SSH key and the install's
  non-secret settings only (the pull account is the one `<short>-stow` carries): cloud-config is
  readable from the seed on the hypervisor's storage and, on a cloud, from the metadata service.
  Disaster recovery: `secrets export` writes the secrets encrypted to the operator's age keys
  (age, the backups' tool; no private key on the target), and `install --secrets` / `vm create
  --secrets` take them back decrypted, the VM's over SSH as every secret (its settings only say
  the first install waits); then a backup is restored. After the first version (TODO): sealed secrets opt-in per deployment (a SOPS file, the sops library, encrypted to the
  operator's key plus a per-deployment key that alone reaches the target, decrypted at each start
  or boot); then the VM's key sealed by its TPM or released by a KMS, with the same boot flow.
- **Installer** — one installer image for all three shapes. It carries a Go binary with a terminal
  UI built on Charm's components (huh, bubbles, lipgloss), not a shell script with `read` prompts;
  answers files, flags and environment variables make every run reproducible without a TTY.
- **Provision hook** — Stowage runs the container itself rather than a project compose service,
  so the confinement is Stowage's; the hook mints and returns what is app-specific (passwords per
  role), Stowage records it. Correcting an input only the hook reads is `provision --set`; one the
  stack reads is a re-run with `--set`. A value the hook returned that has stopped being true (a
  credential the app's users revoked, data restored from elsewhere) is replaced the same way,
  before the hook runs, so a hook that refuses it can name the command that unblocks it — Stowage
  never edits a recorded value by inference. A value the hook never returned is set the same way
  when its `returns` declares the key: a hook that finds an account it did not create (a database
  restored or shared from another install, a value the operator made it forget) cannot sign in
  with a credential it never had and must not mint one for an account that exists, so the
  operator gives it, and the hook's error names a command the installer accepts. `returns` only
  widens what `--set` takes; it does not bound what the hook returns, and a key the install
  records (an input, a secret, a safe default, a route's domain) is refused, as `--set` never
  changes one through the hook's record. The hook declares the keys it receives (`env`): with
  every value it could read the stack's root secrets from an image that is not the stack's.
  On Kubernetes the hook is Stowage's Job, not a helm hook of the chart: a helm hook cannot hand
  values back (so it would sign in with install-time passwords forever), and its failure under
  `--atomic` would roll back an upgrade for a reason that is not the release's. The Job's record
  step is the release's own installer image (already published per release, found by the signed
  release manifest), so no third-party image or client is involved, and the hook's stdout never
  reaches a pod log, which cluster log collectors ship.
- **Reconfigure is the re-run, not a verb** — the reference has none (a re-run of its installer
  finishes setup; its VM re-renders the cloud-config and reboots), so the first version keeps that
  shape and lets the re-run change what it is given: inputs (`--set`, `--answers`, `--domains`) and
  the front's ports, which matter most on Docker, often run on a workstation where a port is
  taken. The environment is not read on a re-run, so a lingering export never changes an install
  unseen; the changes are logged, not confirmed, so the same run works over SSH and in scripts.
  Only a port the install does not hold is probed. `.env.prev` keeps the `.env` a change replaced
  (not overwritten by a second try before a change comes up; an upgrade or a rollback removes
  it), as a secret input's old value is logged
  redacted and going back needs it. A plain re-run never reads the `byo`
  certificate, so its expiry never stops a VM's boot. The generated secrets are never generated
  again, and the TLS mode, its certificate, the loopback address, the registry and the channel
  stay the first install's (a new install changes them). On the VM the settings file is read by
  the first install only, so a change made over SSH survives every boot.
- **The adoption proof lives in the tree** — Stowage knows no project, so the proof that a
  production project can adopt it is `examples/full` made a working stack with that project's
  traits, walked through each shape's lifecycle by its own end-to-end suite; a real project's
  adoption happens in that project's repository, and what it finds missing comes back here as a
  change to the example and its suite.
- **Upgrade runs the new release's installer** — the old binary never interprets a newer format;
  the installer the install keeps (`<dir>/<short>-stow`) is the one that rolls back, and the
  previous one is kept with the release it carries. A rollback keeps every value but the
  release's own, so a secret an upgrade minted survives a rollback and the next upgrade.
- **Installer executable embedded in `stowage`**, not a base image: `ship` makes each release's
  installer image in process (no Docker build, no base image to pin or mirror), and the Stowage
  version that publishes is the one that installs.
- **Front built by `ship` from a recipe embedded in `stowage`**, not mirrored from Docker Hub nor
  from a Stowage registry: an install pulls nothing but the project registry, the Stowage version
  that publishes chooses the Caddy (as it chooses the installer), and a front with a DNS plugin
  needs a build anyway. Its inputs are the recipe's bytes and the platform, so each project builds
  it once per recipe and every later release re-tags it; the cost is one Caddy build, with
  network access, on the publisher's machine.
- **No Stowage registry** — every runtime piece is in the `stowage` binary (an executable or a
  recipe `ship` builds into the project registry), so `stowage.lock` pins a version, not digests;
  operators only ever pull from the project registry.
