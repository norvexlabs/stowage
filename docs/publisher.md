# The publisher (`stowage`)

[README](../README.md) · [Documentation index](../README.md#documentation)

A project is a directory holding `stowage.yaml`. Every path in it is relative to that directory.
Start from [examples/minimal](../examples/minimal/) (the smallest complete project) or
[examples/stateless](../examples/stateless/) (no database, nothing built and no front: one pulled
image served by an L4 route alone, its pages rendered into compose `configs:`, a volume given its
user's uid; installed end to end on Docker, its chart linted) or
[examples/full](../examples/full/) (every spec key in use, and a working stack on Docker,
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
stowage -C <project> ship [--channel C] [--revision N] [--disks] [--audit-db FILE] [--audit-override REASON] [--json]   # build what changed, push, audit, move the pointer
stowage -C <project> verify [<version>] [--channel C] [--revision N] [--json]
stowage -C <project> audit [<version>] [--channel C] [--revision N] [--db FILE] [--fail-on S] [--json]   # the release's SBOMs, scanned
stowage -C <project> sbom export [<version>] DIR [--channel C] [--revision N]   # the release's SBOMs, verified, as files
stowage -C <project> promote <version> --from C --to C [--revision N] [--audit-db FILE] [--audit-override REASON] [--json]
stowage -C <project> yank <version> --reason TEXT [--channel C] [--revision N]
stowage -C <project> prune --channel C [<version>]    # mutable channels only
stowage -C <project> prune --channel C --keep N      # any channel: all but its N latest and the pointer's
stowage -C <project> releases [--channel C] [--json]
stowage -C <project> hook install [--channel C] [--disks]       # write git's post-commit hook for this project
stowage -C <project> hook post-commit [--channel C] [--disks]   # what that hook runs: ship a version bump in the background
stowage --version
stowage -C <project> --test-registry PREFIX [--test-push USER:ENV] [--test-pull USER:ENV|none] [--test-key KEY --test-public-key FILE] [--shapes S,…] <command>   # a test release
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
  tag (absent for a format the release has none of). A retired tag counts as absent. With a
  version, it checks that release.
- `audit` (a project that signs) checks the release as `verify` does, then reads the SBOM each
  of its images, its installer and its OS bundle carries, attested by a key of
  `signing.public_keys` (with the pull account alone), and scans each with grype
  (`docker.io/anchore/grype:v0.121.3`, its linux/amd64 image pinned by digest, on the local
  engine, as the publisher's user). It prints the vulnerabilities by severity, highest first (the artifact, the CVE, the
  package and version, its type, `in <file>` — the file the SBOM says the package was found in,
  where it says (a Go module's binary) — and the versions that fix it), then their count by
  severity. A package found in several files is a finding per file: the same Go module in two
  binaries is two findings, each `in` its binary; an SBOM whose files or relationships do not
  decode leaves its artifact `not audited`; an
  artifact without an SBOM by a trusted key (or whose scan failed) is listed as `not audited:
  <why>`. `--json` prints the report: `{project, version, revision, channel, tag, db_built,
  subjects: [{name, owner, digest, error, findings: [{id, severity, package, version, type,
  fixed_in, path}]}]}`, `owner` being who fixes the artifact's findings: `stowage` for what Stowage
  supplies (the images `front`, `backup`, `chown` and `audit`, built from its recipes, and the
  installer), `project` for the rest: the project's own images, and the OS bundle and its disks,
  whose contents its `shapes.kairos` pins (`base`, `kairos_init`) decide. The printed summary splits the counts the same
  way, each side's into `fixable` (a published version fixes it) and `no fix`:
  `  the project's: fixable 1 critical; no fix none` / `  Stowage's: fixable none; no fix 2 high`. It exits 1 when an artifact is not audited, and with `--fail-on
  negligible|low|medium|high|critical` (default `none`) when a vulnerability is at or above that
  severity. grype's database is fetched from grype's servers into `~/.cache/stowage/grype` (about
  1 GB, kept between audits; grype refreshes it when it is older than its servers'); `--db FILE`
  imports an archive from grype's listing (`https://grype.anchore.io/databases`) instead and
  fetches nothing, whatever the archive's age (grype's own limit, 5 days, is not applied to it).
  The database is made ready once, and only when an artifact has an SBOM to scan. A project that does not sign is refused (its releases carry no SBOM).
- **The audit gate** (`audit: { fail_on, fixable_only, allow }` in the spec, a project that signs):
  `ship` and `promote` check the gate first, before anything is built (an unsigned project, a
  `fail_on` or `allow` file that does not parse, an `--audit-db` file that is not there stop it
  there), then scan the release as `audit` does — every image's, the installer's and the OS
  bundle's SBOM (a disk carries its bundle's, which that scan judges), with the same grype and
  database — once its artifacts are pushed by digest, and before the release manifest is written,
  any release tag moves (the images', the bundle's, the disks', the installer's) or a cloud image
  of `cloud_publish` is registered, and before a channel points at it. A refused release leaves
  every tag as it was; it consumes its release number (`release_seq`). A finding at or
  above `fail_on` (`negligible`, `low`, `medium`, `high`, `critical`) that a published version
  fixes (`fixable_only: true`, the default; `false` counts those with no fix too) and that no
  allowance in force accepts stops the ship: `release <tag>: audit: N fixable vulnerabilities at
  or above high: high CVE-… in web (libcurl 8.22.0, fixed in 8.23.0); …; no channel moves to it:
  fix them and ship again, accept one for a while in audit.allow, or ship with --audit-override
  REASON (recorded in the release)`, after printing the findings and the summary by owner. Fixed
  (a rebuilt image, a bumped pin), the same command carries on; nothing is published until it
  passes. `allow` is a file of the project's: a YAML list of `{id, artifact, package, path,
  reason, expires}` — the finding's id, the artifact it is in (an image key, `installer`, `front`,
  `backup`, `chown`, `audit`, `os` with the kairos shape — a disk's findings are its bundle's,
  under `os` — or `"*"` for any), optionally the package (its name as `audit` prints it:
  `golang.org/x/net`, `stdlib`, `openssl`) and the file it is found in (absolute, as `audit`
  prints it after `in`: `/usr/bin/containerd`) the entry is narrowed to, why, and its last day
  (`YYYY-MM-DD`, UTC, at most a year ahead: an allowance is renewed, never kept for good). An
  entry with `package` accepts its finding in that package alone, with `path` in the package
  found in that file alone (a finding whose SBOM names no file is outside every `path`), so one
  written for an upstream binary does not accept the same id in another binary, one added later
  included. Two entries that can both accept one finding (the same id, and artifacts, packages
  and paths each equal or one of them any: `"*"` beside `os`, a whole artifact beside a narrowed
  entry, an entry narrowed by `package` beside one narrowed by `path`, or the same entry twice) are
  refused, since one would undo the other's narrowing. `lint` checks the file: each field but
  `package` and `path` required, an `id`, `artifact` and `package` without spaces around them, a valid date, a `path`
  absolute and clean without spaces around it, no two entries that can accept one finding, nothing more than a year ahead, and an `artifact` a release
  of the project can carry — one of its image keys, `os` with the kairos shape, or one Stowage
  supplies (`front`, `backup`, `chown`, `audit`, `installer`), whether or not this project's
  releases carry it — refused otherwise, naming those; it warns of an entry expired (`allowance <id> in <artifact> … expired on <date>: the
  finding counts again; renew it, fix the finding, or remove it`) and of one whose last day is
  within 30 days (`… expires on <date>, within 30 days: renew it or fix the finding before
  then`), so a commit hook or a CI step running `lint` catches them before a release. It also
  refuses `audit` on a project that does not sign. `ship` logs each allowance in force that
  matched a finding, `audit: allowance <id> in <artifact> [(package P)] [(path F)] until <date>
  accepts N finding(s): <reason>` (N is 0 when the policy counts none of them today), `audit:
  allowance … expired on <date>: the finding counts again whenever the policy counts it; renew the
  allowance, fix the finding, or remove the allowance` for an expired one, `audit: allowance …
  matches no finding: remove it` for one that matched no finding (an entry with `package` or
  `path`: `… matches no finding: correct its package or path (audit prints each finding's package
  and files), or remove it`), whether or not the policy counts its finding today, and `audit:
  <id> in <artifact> (<package> <version> in <file>) counts: the N allowance(s) of it in force
  are narrowed to other packages or files` once per refused finding entries name outside their
  package or file (with `; the SBOM names no file for it, so only an entry without path accepts
  it` when the SBOM names none and an entry narrowed by `path` alone — or by `path` and the
  finding's own package — names it); a release that passes
  logs `audit: <tag> passes (N artifacts, M findings accepted by audit.allow)`. `--audit-override REASON` publishes past a refused finding:
  `ship` logs `audit: <tag> published past its gate (…): REASON` and records the reason (less the
  blanks around it) on the
  release manifest's artifact (the annotation `dev.stowage.audit-override`, which its signature
  covers and no install reads), which `verify` and `ship` print as `published past its audit gate:
  REASON` (`audit_override` in `--json`) and a yank keeps. A release shipped again unchanged is
  judged again before a tag or the channel moves (a finding published since stops it too; a
  yanked one is refused as yanked instead). Its manifest is not written again, so it takes an
  override only when one is recorded on it already, which stands (`--audit-override` given again
  keeps it: `audit: <tag> past its gate (…) by the override recorded on it: …`, and a different
  reason given then is logged as not recorded); a release
  published without one is refused (`publish a new revision of the release (N+1) with
  --audit-override, or fix them`); such a refusal says `this ship moves nothing (a channel already
  pointing at the release stays there)`, as the channel may serve it already. `--audit-override` on a project without `audit`, or with a blank reason, is refused, and one
  given to a release that passes logs `audit: --audit-override is not needed: nothing is
  recorded`. An
  artifact not audited (its SBOM unreadable, or its scan failed) is refused whatever the override.
  An allowance matching a finding the policy does not count today (no fix yet) is in use, not
  reported. The database is grype's, kept in `~/.cache/stowage/grype` and brought up to date
  before the scan, one audit or gate at a time on the cache (a second logs `waiting for the
  scanner's cache <dir> (another audit or ship holds it)` and waits, until interrupted);
  `--audit-db FILE` (or `STOWAGE_AUDIT_DB` in the environment, as a hook ship has it) imports an
  archive instead. `audit` is the publisher's alone: no artifact carries it, and changing it
  changes no release's inputs. Without `audit`, nothing is scanned before publication.
- `sbom export [<version>] DIR` checks the release as `verify` does and writes the SBOMs `audit`
  reads, as the installer's `sbom export` does (`DIR/<artifact>.spdx.json`, `DIR/index.json`).
- `prune` without a version deletes every release of the channel except the pointer's. With a
  version, it deletes every revision of that version, and the pointer moves back when it held
  one. An immutable channel is refused: yank instead. `--keep N` (1 or more; not with a version)
  is the channel's retention, on an immutable channel too: every release but its N latest (by
  the project's scheme; each revision is a release, a yanked one counted) and the pointer's is deleted as above (tags, disks,
  installer, chart, referrers; the index and `:latest` settled); a release the current scheme
  refuses is skipped, as `releases` does. Nothing to delete prints nothing and succeeds.
- `releases` lists the published releases, oldest first in each channel. It checks no signature.
  A release whose version the current scheme refuses is skipped with a warning.
- `ship`, `promote` and `verify` print, besides the release line and its digest (not with
  `--json`), one line per shape the spec declares, saying what the release can install:
  `docker: installable`, `kubernetes: installable` (`kubernetes: no chart — k8s install
  refused` for a release carrying none), and for kairos `kairos: no OS bundle — vm create and VM
  upgrades refused` for a release carrying none, else one of `kairos: disks
  <formats>`, `kairos: no disks of its own — vm create boots <release>'s disks, then writes this
  release's OS`, `kairos: no disks — vm create refused; a ship of <v> with them publishes them
  (stowage ship --disks --channel <channel> --revision N, from a tree at <v>)` (N the same
  revision on a mutable channel, the next on an immutable one, whose published releases take no
  new artifact), or, for a kairos shape with no `disk_formats`, `kairos: no disk_formats — the OS
  bundle alone, which upgrades a VM; vm create refused`. Disks are published by a ship
  asked to (`--disks`), or by every ship when `shapes.kairos.disks` is `always`; a promotion
  publishes the disks of the release it promotes alone.
- `ship` refuses a version that `version.scheme`'s `month_guard` puts out of its month (`lint`
  only warns of it) unless the channel holds a release of it already — a ship resumed after the
  month's turn, or a later revision of a version out: `version.scheme month_guard: version V is
  not in the current month (Y.M, UTC): start a new month's series; not shipped`, before anything
  is built. `promote` re-tags a published release, whatever its month.

**A test release** (`--test-registry`, before or after the command): every command that opens the
project — `ship`, `plan`, `verify`, `releases`, `promote`, `yank`, `prune` — takes it as a
release for a test run, never the project's, without editing the spec:

- `--test-registry PREFIX` publishes to and reads from that registry prefix instead of
  `registry.prefix`; the release's installs pull from it. It is written as `registry.prefix`
  is (host[:port][/path], lower case, no scheme or trailing `/`; a registry's host alone, as
  `localhost:5000`, is one). The project's own prefix, however its host is spelt (case, a
  trailing dot, an IP address's forms, a port with leading zeros, the default ports 80 and 443
  — taken as one —, the loopback's names — `localhost`, `*.localhost`, 127.0.0.0/8, `::1` — and
  Docker Hub's other host names), is refused, and so is a prefix within
  it or holding it (`is the project's registry (registry.prefix P), or within it`): its
  repositories would be the project's. Only the spelling is read: another DNS name or address
  of the same registry (a second name its certificate covers) is not told, so give a registry —
  and an account (`--test-push`) — that cannot write the project's repositories. A port is 1 to
  65535. Each flag below is refused without `--test-registry`. Deleting (`prune`, an older disk
  set) uses the test registry's own API, never `registry.delete`'s.
- The registry's credentials are the run's, never the project's: `--test-push USER:ENV`
  publishes as USER with the password in `$ENV`, and `--test-pull USER:ENV` (or `none`) is the
  pull account the test release's installers carry. A project whose `registry.push` is a
  `token` refuses a test release without `--test-push` (its token would be sent to another
  registry), and one whose `registry.pull` is a `token` without `--test-pull` (its account would
  be baked into an installer published there), as is a `docker-config` push naming the
  project's own `config` directory; the publisher's default Docker config, read per registry
  host, is kept.
- `--test-key KEY` (a cosign private key file, `env://VAR` or a KMS URI) signs it, and
  `--test-public-key FILE` (a project file) is the one key its installs, charts and OS bundle
  trust — the project's `public_keys` are left out, so no install of the project trusts the run's
  key, and an install of the test release trusts no key of the project's. The two go together;
  a project that signs refuses a test release without them (never signed with the project's
  key), and refuses its own key (`signing.key`, or a copy of it) and its own `public_keys` as
  them (compared as keys, whatever their encoding; a project key that cannot be read refuses the
  test release); the public key must be a PEM public key (PKIX or PKCS#1, as installs read).
  Before a command signs anything, cosign derives the public key of `--test-key` (`cosign
  public-key`): one that is not `--test-public-key` is refused (`--test-key K is not the key of
  --test-public-key F: not signed`), so no form of the project's key — `env://`, a copy encrypted
  again — signs a test release. An unsigned project's test release is signed
  with them when given.
- `--shapes docker,kubernetes,kairos` keeps only those of the project's shapes (`kairos` left
  out: no OS bundle or disk is built); one that leaves none is refused. A test release's disks
  are registered on no cloud (`cloud_publish` is left out).
- Each command prints first `test release: registry PREFIX[, signed with KEY, its installs
  trusting FILE alone], shapes …`. `lint` reads the spec as it is. `hook` refuses the flags: a
  commit ships the project's release.

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

  A version `HEAD` records that the scheme refuses is reported as an error, and so is one
  `month_guard` puts out of its month (no ship starts; a ship of a version the channel holds
  already is `stowage ship`'s). Every outcome exits 0: a commit never fails on its release.
- The ship publishes the disks (as `ship --disks`) when the hook was written with `--disks`, when
  the commit was made with `STOWAGE_SHIP_DISKS=1` (or `true`) in its environment, or when
  `shapes.kairos.disks` is `always` — so a project chooses its own cadence, a release at a time;
  the started line then ends `, with its disks`. A project with the kairos shape and
  `disk_formats` whose ship publishes none is told so on that line (`…, without disks (vm create of it refused unless the
  channel's disks boot it)`). The background run's log ends with the ship's summary, its shape
  lines included.
- The ship passes the project's audit gate (`audit`) as `stowage ship` does, scanning with the
  database `STOWAGE_AUDIT_DB` names in the commit's environment (else grype's, fetched); a refusal
  ends the background run's log, and no override is given: the publisher fixes the finding,
  allows it in `audit.allow`, or ships by hand with `--audit-override`.
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
| `plan` | `{"project_version", "revision", "channel", "nodes": [{"id", "kind", "deps", "inputs", "pinned", "converts", "reads_version"}]}` |
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
| `<name>-backup` | the backup runner, built by Stowage (a `docker` or `kairos` shape with a `pg-age-rclone` or `volumes-age-rclone` backup), by the release tag |
| `<name>-chown` | the volume helper, built by Stowage (a `docker` or `kairos` shape with a `data.volumes` entry of a `uid`), by the release tag |
| `<name>-audit` | the vulnerability scanner a signing release carries (grype, built from its source by ship), by the release tag |
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
- signing uses `~/.cache/stowage/sign`, and syft's private Docker config `~/.cache/stowage/sbom`,
  removed after each call; `audit` keeps grype's database in `~/.cache/stowage/grype`.

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
   (`image/front` in `plan`): Caddy 2.11.9 with the Cloudflare DNS plugin and the layer4 app (caddy-l4, which terminates an L4 route's TLS), built from a
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
   `verify` of that channel compares their digests with the pointer's and the set's. On a
   registry that deletes no tag alone (ghcr), a floating tag with nothing to offer is retired
   instead of removed: moved onto a small artifact of type
   `application/vnd.stowage.retired.v1+json`, which every reader (`ship`, `verify`, the installer,
   a mirror, the VM's upgrades) takes for absent, and the ship logs `… retired: the registry
   deletes no tag alone`. A retired tag holds
   no image or disk (its one blob is `{}`): nothing is on offer under it. Once the registry
   deletes tags alone, the next ship deletes it;
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

**A disk's push** goes up in chunks (the OCI distribution API's chunked upload, 1 to 64 MiB each but
the last, raised to the `OCI-Chunk-Min-Length` a registry answers, sized to about a minute at the
rate the last one went and halved after a failure). A 413 to a chunk, or a 416 to any but the first
chunk of a disk's upload (below), asks the registry how far the upload got: an upload it no longer
knows is lost (below), one further on goes on from there, one behind is retried from there, and a
416 it cannot tell is retried while the chunk can shrink. Otherwise — the upload still at the last
byte acknowledged (ghcr.io caps a request at 4 MiB and answers 416), or a 413 it cannot tell — the
chunk's size was refused: the chunk is halved at once, said in the log, and no later chunk to that
registry in the same run (the other disks of a `ship` included) is larger, unless the registry's own
minimum is. The smallest chunk refused so is retried once this disk's upload had a whole chunk that
large acknowledged, and before that gets the disk in one stream. A 413 or 416 naming a refusal of
the push (`DENIED`, `SIZE_INVALID`, …) fails it at once. After a failure (a dropped or stalled
connection, a 5xx, a 408 or 429, a 413 to any other request, with `Retry-After` not read) it waits
(2 s, doubled up to a minute) and goes on from the last byte the registry acknowledged, asking it
how far it got. A registry that lost the upload (a 404, a 416 it no longer knows the upload after, a
status claiming bytes never sent, or a 400 `BLOB_UPLOAD_*`, which Docker Distribution answers to an
upload resumed after a chunk cut short; one that cannot tell how far it got keeps the last offset
acknowledged) is asked again whether it holds the blob, then gets a new upload, from the first byte,
said in the log. A push with no byte acknowledged for 5 minutes fails, naming the disk (its file and
digest); so does, at once, a refusal no retry changes: a credential (401, 403), the repository
(`NAME_UNKNOWN`, `NAME_INVALID`, `DENIED`), a registry that takes no push (405, `UNSUPPORTED`), the
digest or size, or any other 400. Between chunks, every 30 s at most, the push logs its progress to
stderr (bytes sent, of the total, and the rate since its upload began), and each retry with its
cause. A registry that refuses the first chunk of a disk's upload, before any was acknowledged (400,
405, 416 or 501), or that lost the upload 4 times, gets the disk in one stream, as any other blob.
An upload given up (lost, failed, or replaced by the one stream) is deleted, unless it holds every
byte: its commit may be under way.

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
`ship` and `promote` end their report (not with `--json`) with the release's installer by digest,
which a tag cannot move: `Check the installer: cosign verify --key <public key>
--insecure-ignore-tlog=true <prefix>/<name>-installer@sha256:…`, the first of `public_keys` (a
test release's `--test-public-key`), from the project's root, then `  or, with another key: …` for
each other one, as in a rotation either may sign (or, for a project that does not
sign, `Not signed (no signing): an install trusts whatever the registry serves.`), then `Then:
docker run --rm <prefix>/<name>-installer@sha256:… > <name>.sh`.
Among a version's revisions, `upgrade --version` takes the latest one signed by a trusted key: one
pushed without the key is passed over, its yank not believed; one whose signatures cannot be read
stops it (an older revision never stands in for it). Each revision passed over is logged
(`passed over: …`). The VM's `updates` / `upgrade` without a version refuse a channel pointer on a
yanked release, or on a publication its tag no longer holds (`the channel C's pointer names T,
which its tag no longer holds`). A signature only counts as one made
by `cosign sign` (an attestation the key made over the same digest does not), checked with the
hash cosign uses for the key type.

**SBOMs.** A project that signs gives each image of a release (its own, the mirrored ones and
Stowage's), its installer, its OS bundle and each disk an SBOM: an SPDX 2.3 JSON document made by syft
(`docker.io/anchore/syft:v1.55.0`, pinned by digest, run on the local engine on the host network,
reading the image from the registry with the push account), attached with `cosign attest --type
spdxjson` signed by the release key as an in-toto attestation (predicate type
`https://spdx.dev/Document`), stored as a sigstore bundle referrer of the artifact's digest, as the
signatures are. A disk's is its bundle's, attested on the disk (`attested the SBOM of <ref> (its
bundle's)`), with no scan of its own. `ship` and `promote` log `attested the SBOM of <ref>` for each one made; an
artifact that already carries one by a trusted key is left (a re-ship makes none, an SBOM deleted
from the registry is made again), and one made is read back before the release is written. The
SBOM of a digest is made once: a later syft makes no new one, and a digest that already carries
one (a pulled image's included) keeps it, so a document added to a built image is merged when the
image is next built (a new digest). An attestation never counts as a
signature. A signing release also carries the scanner `<short>-stow audit` runs: grype v0.121.3,
built by `ship` (`image audit: build`) from grype's source with Go 1.27.4 and the modules raised
past a published vulnerability (a recipe embedded in `stowage`, its base pinned by digest, the
binary and the CA certificates on an empty image, laid out as grype's own image), published as the
image `audit` (`<prefix>/<name>-audit`), signed and attested as every image, and found again by its
inputs by a later ship. Every image Stowage builds for itself is an OCI image, which a registry
taking OCI manifests alone (zot's default) takes — a project's own pinned image
(`images.<key>.ref`) is mirrored as is, a Docker v2 one needing a registry that takes those (zot:
`http.compat: ["docker2s2"]`). No OS bundle preloads the scanner: `mirror
export` carries it and the SBOMs to a site without internet, where an operator's `<short>-stow
audit` pulls it. `stowage sbom export` and `<short>-stow sbom export` write the verified SBOMs as
files for other tools. **An image's own SBOM documents are merged**: syft runs with its SBOM cataloger
and without its kernel cataloger (`--select-catalogers +sbom-cataloger,-linux-kernel-cataloger`),
so each SBOM document found anywhere in an image, a base image's included, adds its packages, with
their purls and CPEs, to the image's SBOM, which `audit` scans and `sbom export` writes, and no
SBOM lists a kernel (`linux-kernel`) or its modules: an OS bundle's kernel is its distribution's
packages (`linux-image-*`, `linux-modules-*`), scanned by their own versions, which carry the
fixes the distribution backports. A document is a file named `*.cdx`, `*.cdx.*`, `*.spdx`, `*.spdx.*`,
`*.bom`, `*.bom.*`, `bom`, `*.sbom`, `*.sbom.*`, `sbom` or `*.syft.json`, in CycloneDX (JSON or XML),
SPDX (JSON or tag-value) or syft JSON; a file of such a name that is none is skipped. The convention
for an app's: `/usr/share/sbom/<name>.cdx.json`. A package a document names that a cataloger also
found is listed twice, and so are its findings. An app ships a document for what no scanner
sees in its image: the scripts a front bundles (from the bundler's own list of the modules it
bundled: `cyclonedx-npm --omit dev` misses a dependency a bundle takes from the development tree),
vendored C, a downloaded binary. What syft reads by itself needs none: a distribution's package
database, `node_modules` with its lock file, Python's installed packages, Go binaries, Rust binaries
built with `cargo auditable build`, ELF and Windows PE alike (plain `cargo build` records no crates). To check an SBOM by hand: `cosign verify-attestation --key <public key> --type spdxjson
--insecure-ignore-tlog=true <prefix>/<name>-<key>@sha256:… | jq -r .payload | base64 -d | jq
.predicate`.

Run one publisher at a time per project (in CI, one concurrency group): a registry offers no
compare-and-swap.

`promote` re-tags a published release onto another channel without building (the OS bundle and
its disks name no channel, so they are re-tagged too); the tree must be
exactly what was published (same release inputs: images, compose, spec, changelog,
`stowage.lock`, and the same `stowage` and pull account password, which make the installer
and the front),
checked before anything is written. Re-shipping a yanked release fails: publish a new revision (the channel never moves forward to it; a pointer an interrupted yank left on it moves back). Image keys `release`,
`release-seq`, `os`, `disk`, `installer`, `front`, `backup`, `chown` and `audit` are reserved for Stowage's own repositories. `yank` marks a release (nothing is deleted) and moves the pointer back to the latest
release that is not yanked and verifies, removing it when none is left; a broken release can
always be yanked. `prune` deletes tags (the release's images, installer, chart, OS bundle and
disk tags first, its manifest last; disks as "Disk sets" says): a registry that deletes no tag
alone has a manifest deleted by digest through `registry.delete` when no other tag holds it; when
another does (the channel pointer, `:latest`, another release's tag of the same image), the tag is
retired as step 6 of `ship` says and logged `<tag> retired: the registry deletes no tag alone; its
content stays under <tags>`. Once the pointer and `:latest` have moved, the prune deletes by digest
what no tag holds any more (logged `<ref> deleted: no tag holds it any more`). A retired release
tag is no release: `prune`, `verify`, installs and mirrors skip it; it stays in the registry.

Global flags: `-C <dir>` project root (default `.`), `-f <file>` spec file (default
`<dir>/stowage.yaml`). Exit codes: `0` success, `1` failure or lint errors, `3` a release `ship`
or `promote` published whose disk cleanup failed. With `--json`, stdout
is one JSON document and nothing else; diagnostics go to stderr.
