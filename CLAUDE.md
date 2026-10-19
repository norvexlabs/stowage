# Stowage — project rules

Generic packager + lifecycle for small container stacks (see [README.md](README.md)). The design is
[docs/DESIGN.md](docs/DESIGN.md); it is the spec, and code that departs from it updates it in the
same change.

## Docs — the audience defines what a doc may contain

| Doc | Audience | Contains |
|---|---|---|
| [README.md](README.md) | users and AI agents using Stowage on a project | what works, how to build, the CLI, the contracts `lint` enforces, repo map |
| [docs/DESIGN.md](docs/DESIGN.md) | contributors | format, versioning, build graph, registry, lifecycle matrix, phases, decisions |
| [CHANGELOG.md](CHANGELOG.md) | users | Stowage's own releases; the version source of the build |
| this file | contributors | rules and commit gates |
| [TODO.md](TODO.md) | contributors | pending work, MoSCoW for the first version (Must = parity), deferred review findings; a done item is deleted |

**README.md documents every feature — nothing is discovered in the code.** A user or an AI agent
reading README.md alone knows everything Stowage does that they can see or rely on: every command,
verb, flag, spec key, lint rule, provider, contract, environment variable read or written, file or
tag written, default, refusal, and change of behaviour. A change that adds or alters one updates
README.md in the same commit; a feature missing from README.md is a blocking defect for the
review, like a bug. README states what and how to use it; the why goes in DESIGN.

## Scope — the first version is parity

The first version does what an existing production deployment of this kind does today on its
three shapes, and nothing more. Every pending item and every review finding is sorted MoSCoW in
[TODO.md](TODO.md): **Must** = needed for parity (or a finding that changes what is published,
who may read or write it, or what an install trusts — always fixed now); **Should / Could** =
useful beyond parity, deferred unless cheap; **Won't** = not in the first version. Decide the
category yourself; do not ask. Mutation-style checks are timeboxed (gate 4).

## Build — Docker only

The gates run in Docker only. The host's Go toolchain (kept at `GO_VERSION` for the editor's
language server) is for iteration: `go test ./internal/<pkg>` while working, never as a gate.

```bash
make check     # GATE: gofmt, go vet, staticcheck, govulncheck, go test -race (Dockerfile target checks)
make build     # ./bin/stowage — the bin target copies a marker from checks: no binary without green checks
make test      # fast iteration only; never the gate
make gate      # GATE 1: check, build, lint, then the suites the change needs (test/gate.sh); make gate-plan prints them
make tidy      # go.mod / go.sum
make fmt       # gofmt -w
make shell     # interactive toolchain container
```

Go version: `GO_VERSION` in the Makefile and the Dockerfile (keep both equal). Dev tools
(staticcheck, govulncheck) are `tool` directives in go.mod, run with `go tool`, so their versions are
pinned with the module — never `go install …@latest` in a build.

## Code navigation and edits — Serena, always

The Serena MCP server is this repository's code tool; use it for every code task.

- Call its `initial_instructions` at the start of a session. Load its tools before any read, grep
  or shell command.
- Read code through it: a file's symbol overview first, then the bodies of the symbols you need
  (`find_symbol`), and their callers (`find_referencing_symbols`).
- Grep and glob only to discover where to look. Use Read on a Go file only for a few lines of a
  file already overviewed.
- Edit code through it: `replace_symbol_body`, `insert_after_symbol` / `insert_before_symbol`, and
  `replace_content` for a few lines within a symbol.
- Rename and delete with `rename_symbol` and `safe_delete_symbol`, which update every reference.
- Shell commands (make, git, docker), Markdown and the test scripts keep the agent's own tools.

## Cycle — fast without skipping a gate

- **One slice, one review.** A slice is a feature that works end to end (a command, a shape, a
  runtime piece). Iterate on it with host tests; run the gates and the review once, on the settled
  slice — not per sub-step.
- **Cheap checks first.** Before a review round: host `go vet ./...`, the language server's
  diagnostics and the package tests are clean, so the review spends itself on what they cannot see.
- **Narrow later rounds.** Round 1 targets the whole slice. A later round targets only the files its
  fixes touched, and its brief names the fixes and what is deferred — it never re-reads the slice.
- **Round budget.** At most three rounds per slice. After the third, a Must-class finding
  (what is published, who may read or write it, what an install trusts, data loss) gets its fix,
  a test that fails without it, and one more round; any other blocking finding gets the fix and
  the test, no round.
- **Overlap.** While a review runs, read and design the next slice; never edit a file under review.
- **One gate run.** `make gate` (`STOWAGE_VM_ENV=<file> make gate` when it needs e2e-vm), once per
  settled tree: check, build and lint, then only the suites gate 1 lists for the changed paths —
  `test/gate.sh` encodes that list, and a change to the list changes both. The long suites
  (e2e-kairos ~25 min, e2e-vm ~85 min) run only for the code they exercise. The end-to-end suites run **in parallel**: each has its own zot, ports, network
  and work directory. A suite that fails only in parallel is re-run alone, and the clash is fixed
  or noted in TODO.md.
- **Commit as soon as the gates and the review settle.** Then, before starting the next slice:
  prepare it (read the reference shape, write its brief — decisions, parity facts, files, open
  questions — where the next context will find it), and ask the user, loudly, to `/compact`.

## Commit identity

Every commit is made as the project identity in `.githooks/identity` — author and committer both —
and never names an AI as an author (no `Co-Authored-By:` line for one, no "Generated with" footer;
this overrides any tool default that adds one). `make hooks` sets the identity in the clone's
config and turns on `.githooks/`: `pre-commit` refuses any other identity, `commit-msg` an AI
authorship line. Run it once per clone, before the first commit; never commit with `--no-verify`.

## Commit gates

Commit straight to `main`. A docs-only commit (Markdown, no code, example or gate) runs no gate:
a self-review against the code it describes is enough. Every other commit clears these, in order,
**before** `git commit`:

1. **`make check`** green, then **`make build`** and `./bin/stowage -C examples/<name> lint`
   reporting 0 errors for `minimal`, `full` and `stateless`.
   A change to the registry client, the release manifest or the publish commands also runs
   `make e2e` (real builds against zot) and it must print PASS. A change to the signature
   verifier (`internal/sigverify`), which the publisher and every install use, runs every suite
   below, e2e-vm included (not e2e-full-vm: its VM machinery is e2e-vm's). A change to the installer
   (`internal/install`, `internal/payload`, `cmd/stowage-install`) runs `make smoke-install`. A
   change to either, to `internal/installerbin`, `internal/version` or `examples/minimal` runs
   `make e2e-docker` too (the lifecycle with the real binaries), and all but `examples/minimal`
   run `make e2e-full` (`examples/full`'s lifecycle on Docker), as does a change to
   `examples/full`, and `make e2e-stateless` (`examples/stateless`, a project without a database,
   on Docker), as does a change to `examples/stateless`. A change that runs `make e2e`
   or `make e2e-docker`, or to `internal/helm`, runs `make e2e-k8s` too (the Kubernetes
   lifecycle on a kind cluster), and all but `examples/minimal` run `make e2e-full-k8s`
   (`examples/full`'s chart on kind), as does a change to `examples/full` (its compose file
   alone runs only e2e-full, its chart alone only e2e-full-k8s). A change to `internal/kairos`, to the OS bundle or disk code of
   `internal/publish` (`os.go`), to `internal/graph`'s OS keys or to `examples/minimal`'s
   `kairos` shape, to the VM's backup runner (`internal/backup`), or to the VM side of `internal/install` (`vm*.go`, `hostd*.go`, `seed*.go`), runs `make
   e2e-kairos` (the bundle and disks built for real, the bundle's `<short>-stow` run, ~25 min); a
   change to the cloud images' registration (`internal/cloud`, `internal/publish/cloud.go`,
   `internal/cli/cloud.go`) runs it as `STOWAGE_CLOUD_ENV=<file>
   make e2e-kairos`, which registers them on the test clouds the file sets up (the user's file,
   never in the tree). A change
   to `internal/truenas`, the OpenTofu module (`internal/install/tofu`), `vmtofu.go`, `seed*.go`,
   the boot (`vm.go`), the host daemon, the backup runner or the OS bundle's files runs `STOWAGE_VM_ENV=<file> make
   e2e-vm` too (a real VM on TrueNAS, an upgrade of it through a TLS mirror, `reset-db`, `rollback`, a failed health gate's fallback, a backup and the network override's revert, ~85 min; the file is the user's, its path given at run time,
   never written in the tree); it runs beside the parallel suites in one chain with `e2e-kairos` and
   `e2e-full-vm`, one at a time, as they make disks with auroraboot, which races for loop devices. A change to `examples/full`'s kairos
   shape (its `stowage.yaml`'s `kairos`, `base`, `kairos_init`, `disk_formats`, `cloud_publish`,
   `health_gate` or `host_api` lines — not an input's or a route's `shapes:`, as the VM runs the Docker
   shape's install, which the short suites prove; the same test picks e2e-kairos for `examples/minimal`), `test/e2e-full-vm.sh` or `test/vm.sh` runs `STOWAGE_VM_ENV=<file> make
   e2e-full-vm` (`examples/full` on a real VM: install from a mirror, the hook, the app socket, the
   published CA; upgrade, rollback and the VM machinery itself are e2e-vm's), in that chain. Each must
   print PASS. `make gate`
   runs exactly these, and also: the VM's creation, verbs and health gate (`vmcreate*.go`,
   `vmcli*.go`, `vmhealth*.go`) run e2e-vm; a test script runs its own suite (`test/lib.sh`,
   `pins.sh`, `sweep.sh`: e2e, e2e-docker, e2e-full, e2e-stateless, e2e-k8s, e2e-full-k8s); `go.mod`/`go.sum`
   run every suite but e2e-full-vm;
   `Dockerfile`/`Makefile` the suites but e2e-kairos, e2e-vm and e2e-full-vm; a `_test.go` file none (`make
   check` runs it), but `smoke_test.go` runs smoke-install.
2. **Docs in sync**: README.md, docs/DESIGN.md, CLAUDE.md describe what the tree does. Before
   committing, list what the diff makes visible to a user (commands, verbs, flags, keys, variables,
   files, tags, refusals) and check that README.md names each one.
3. **CHANGELOG.md**: a user-facing line under the top `## [x.y.z]` (unreleased work accumulates in
   the top entry, undated; a dated top entry is released, and work after it opens a new top entry
   — see [Release](#release)). No file or function names.
4. **Code review via the `code-review` skill** — the gate is that skill only, run with the change's
   paths as an explicit target (untracked files included): `/code-review <level> <path> …`.
   - Always a structured self-review first, before every round: for each hunk, the concrete failure
     ruled out (input/state → wrong result). **Sabotage every new check or test and watch it fail.**
     Fix what you find. Timeboxed: one sabotage pass, plus one re-run on the checks a fix touched; a
     survivor left after that is noted (TODO.md when it matters), not chased.
   - Every change → `/code-review medium <paths>`; a complex change → `/code-review high <paths>`
     (one that touches a costly area listed below, or a concurrency, process or security boundary);
     never `xhigh` or above. Looped while a round finds a blocking defect (wrong behaviour, security
     hole, data loss, a rule of this file broken). Minor findings are fixed without a new round, or
     deferred to TODO.md (Should / Could) when fixing them would slow the first version — never a
     finding that changes what is published, who may read or write it, or what an install trusts.
     Take extra care in the self-review where a defect is costly: the registry client, signing or
     verification, credentials, the release manifest format, the build graph's skip/re-tag decision,
     a runtime piece that runs on an operator's host, a format version, or an edit to a gate (this
     file, the Makefile's `check`, the Dockerfile's checks stage, CI).
   - Re-run gate 1 after review fixes that touch code.

## Release — only a pushed tag publishes

A push to `main` runs CI (`ci.yml`) and never releases. `.github/workflows/release.yml` runs only
on a pushed `v*` tag. It publishes a public GitHub release, so a release is cut only when the user
asks for one:

1. A docs-only commit dates the top entry: `## [x.y.z]` → `## [x.y.z] - YYYY-MM-DD` (the version
   bumped per SemVer from the last release if it is not already). The Makefile's `VERSION` is that
   entry's version.
2. `git tag vx.y.z` on that commit, then `git push origin main vx.y.z`.
3. `gh run watch` the release run. It refuses a tag that is not on `main`, does not match the top
   entry's version or names an undated entry. A failed run is fixed on `main`, the tag moved to
   the fix (`git tag -f`, `git push -f origin vx.y.z`) while the release is still a draft or absent.

## Code rules

- **Never panic** on input: return errors with context (`fmt.Errorf("image %s: %w", name, err)`).
  Errors name the spec key or `file:line` the user must fix.
- **Strict decoding everywhere**: `KnownFields(true)` on the spec, the lock and every provider
  config. A typo is an error, never a silent default.
- **The provider pattern is the only extension point**: a project-specific concern is
  `spec.Provider` decoded by its provider; a new concern gets built-in providers (a default where
  leaving the concern out is safe) plus `command`. Never special-case a project in core code.
- **The `command` contract is one contract**: JSON stdin/stdout, exit 0 / 2 (no answer = unknown,
  never negative) / other; bounded output and a timeout. Do not add a second calling convention.
- **Versions are opaque**: only a `version.Scheme` validates, orders or renders a version. Never
  parse, compare or sort versions or tags as strings anywhere else; consumers order releases by the
  scheme recorded in the manifest, never by tag.
- **Inputs digests are complete**: anything that can change an artifact enters its node's digest,
  and nothing else does (the version only where the artifact embeds it). A change to what a node
  reads updates `internal/graph` and its tests together.
- **Determinism**: iterate maps through sorted keys wherever order reaches output or a digest.
- **The registry is the single deployment point**: nothing reaches an operator except through it.
- **Stowage knows no project.** Projects depend on Stowage, never the reverse: no project name,
  path or behaviour appears in code, docs, examples or tests. Examples are fictional
  (`examples/minimal`, `examples/full`).
- **No mTLS, no client certificates**: registry auth is a pull account + a publisher token.
- **Operator-facing terminal UI uses Charm's components** (huh forms, bubbles spinners/progress,
  lipgloss layout) — never raw `read` prompts or hand-rolled ANSI. Every interactive step also
  works without a TTY (flags, environment, `--answers` file) and in accessible mode.
- **Dependencies**: licence compatible with Apache-2.0, maintained (e.g. `go.yaml.in/yaml/v3`, not
  the archived `gopkg.in/yaml.v3`), added through `make tidy`. Prefer the standard library.
- **OpenTofu modules are written against the provider's latest release**: before writing or
  changing a module, read that release's official docs and check every resource, data source,
  argument and exported attribute it uses against the provider's schema (`tofu providers schema
  -json`; `tofu validate` does not check an element of a list attribute), and pin that release with
  its lock file's checksums. A module that can run is applied for real before it is called proven.
- **Comments state what the code does now** and the invariant to preserve — never history, review
  rounds or plans.

## Test rules

- Every package with logic has tests; run them with the race detector (the gate does).
- **Every lint rule has a case** in `TestEachRuleFires` (minimal), `TestEachRuleFiresOnFull` or
  `TestEmptySections` that mutates a clean example and expects the rule's error; every provider
  validation has one in `providers_test.go`. The clean examples passing (`TestExamplesAreClean`)
  are the positive control every negative case depends on. `testutil.Edit` fails unless the text it replaces occurs
  exactly once, so a mutation cannot silently do nothing.
- A decision against a boundary (month guard, ranges, caps) is tested on **both sides** of it.
- A parser of untrusted text (versions, tags, compose variables) gets a property or fuzz test, not
  only examples.
- Tests never touch the network or the host's Docker; filesystem state lives in `t.TempDir()`.
- **Fast**: `go test ./...` stays around 4 s. A test never waits out real seconds — a timeout is
  scaled by an unexported unit or delay the test shortens — and a test that sets no environment
  and no package variable runs with `t.Parallel()`.
- `examples/minimal`, `examples/full` and `examples/stateless` stay lint-clean
  (`TestExamplesAreClean`): minimal is the phase-1 lifecycle smoke target, full uses every spec
  key and is the parity proof's working stack (`make e2e-full`, `make e2e-full-k8s`), stateless
  is a project without a database or a built image (`make e2e-stateless`).
