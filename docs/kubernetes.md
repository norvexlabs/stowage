# The installer on Kubernetes (`<short>-stow k8s`)

[README](../README.md) · [Documentation index](../README.md#documentation)

A project with the `kubernetes` shape gets a `k8s` command in the same installer (bootstrap it as
above, then `sh <name>.sh k8s install …`). It drives the operator's own `helm` (≥ 3.14) and
`kubectl`, on the current kubeconfig context unless `--kube-context` says otherwise, and works in
the current directory. A release with no chart (published without the shape) is refused.

```bash
<short>-stow k8s install  [--namespace NS] [--release R] [--kube-context C] [--values FILE]…
                       [--image-registry PREFIX] [--image-pull-login] [--registry-ca FILE] [--set KEY=VALUE]… [--set-file KEY=PATH]…
                       [--answers FILE] [--given FILE|- | --kit FILE|- [--kit-code-file FILE]]
                       [--no-input] [--accessible] [--timeout 10m] [--dry-run] [--no-provision]
                       [--unsafe-sysctls[=false]] [--json]
<short>-stow k8s updates  [--namespace NS] [--release R] [--kube-context C] [--channel C] [--version V]
          [--image-registry PREFIX] [--registry-ca FILE] [--json]      # the release upgrade --version would move to
<short>-stow k8s upgrade  [the same flags; --timeout 6m]                 # the NEW release's installer
<short>-stow k8s upgrade --version V [the same flags, but --given, --kit] # fetches V's installer
<short>-stow k8s rollback [--namespace NS] [--release R] [--kube-context C] [--revision N] [--timeout 6m] [--dry-run] [--json]
<short>-stow k8s status   [--namespace NS] [--release R] [--kube-context C] [--json]
<short>-stow k8s reset-db [--namespace NS] [--release R] [--kube-context C] [--yes] [--no-input]
                       [--accessible] [--registry-ca FILE] [--timeout 10m] [--dry-run] [--no-provision]
                       [--new-baseline [--set-file KEY=PATH]…]        # the NEW release's installer, across a baseline
<short>-stow k8s uninstall [--namespace NS] [--release R] [--kube-context C] [--data] [--yes]
                       [--no-input] [--accessible] [--timeout 10m] [--dry-run] [--json]
<short>-stow k8s provision [--namespace NS] [--release R] [--kube-context C] [--set KEY=VALUE]…
                       [--registry-ca FILE]                           # a project with a provision hook
<short>-stow k8s secrets rotate KEY [--stage | --finish] [--namespace NS] [--release R]
<short>-stow k8s secrets show KEY [--namespace NS] [--release R]   # a settable secret's value from the release's Secret, on a terminal only
                       [--kube-context C] [--timeout 6m]
<short>-stow k8s tune                                                 # what tunes the network (prints)
```

- Defaults: `--namespace <name>`, `--release <name>` (each a DNS label; a release name is at most
  53 characters).
- `--given FILE|-` takes what another install gives, in the format of `install --given` (`project`, `inputs`, `tls`; at most 1 MiB, strict, refused when encrypted, for another project or holding nothing). Its inputs are taken as an answers file's, so `--set`, an exported variable and `--answers` win. An input the project declares but not on Kubernetes (another `shapes`, a `when`) is left out and logged (`--given: KEY left out: not an input on Kubernetes`), one it does not declare is refused (`--given: <project> <version> declares no input KEY`), and a `tls` section is left out and logged (`--given: tls left out: on Kubernetes the chart's ingress owns the certificate`): a project that hands a certificate to a chart declares it as file inputs. On `k8s upgrade` the given inputs replace the recorded values, each logged (`changing KEY: <old> → <new>`, a secret `<redacted>`, a file by its digest), and once helm has settled the release the installer restarts each Deployment, StatefulSet and DaemonSet of the release reading a key given (`kubectl rollout restart`, then `rollout status --timeout`; a file input's readers include what mounts the files Secret), as helm rolls only what its templates change. The same value given again restarts them too. `--dry-run` prints the restart and does none. A restart that fails prints the release's endpoint and tips, leaves the provision hook unrun, and names the way on: `kubectl rollout restart <every workload reading them> --namespace NS --context C` once it can start, then `k8s provision`. `upgrade --version` refuses `--given` (`--given with --version: …`, naming the two steps and the bootstrap script of that version's installer): the installer it fetches may not take it, and standard input is read once. `upgrade --version` to the release that runs applies no input given, says so, and exits 0. Secrets outlive helm: a failed upgrade helm rolls back, or a `k8s rollback`, keeps the values given in the Secrets. `--set` and `--answers` on an upgrade keep their rule: only an input the release adds, the refusal naming `--given FILE` when the key is an input it could replace.
- `--kit FILE|-` and `--kit-code-file FILE` take a handoff kit (its format, its code's sources and its refusals under the Docker shape's `--kit`), which then goes as `--given` does here, on an install and an upgrade: what it logs says `--kit:`, one left with nothing once its other-shape inputs and `tls` are left out is refused, as is `--given`'s (`--kit: nothing in the file is for this install: …`), and `upgrade --version` refuses it the same way (`--kit with --version: …`).
- `--unsafe-sysctls` gives the release's pods the network profile's settings of "Network
  tuning" (whatever the release's `tune.profile`, which Kubernetes does not read) whose
  namespace starts at a fixed default — `net.core.somaxconn` 8192, `net.ipv4.tcp_mtu_probing` 1,
  `net.ipv4.tcp_notsent_lowat` 131072, `net.ipv4.tcp_slow_start_after_idle` 0 — as the chart's
  `stowage.sysctls`; `--unsafe-sysctls=false` takes them away, and an `upgrade` without the flag
  keeps the release's list as it was given — the operator's own, set with `--values`, included;
  `--unsafe-sysctls` again gives the upgrading installer's (none after an install without it;
  `reset-db` installs again with the values it has). `upgrade --version V` to the release that
  runs applies nothing, and says so when the release does not have what was asked:
  `k8s upgrade --unsafe-sysctls` by that release's installer does. The installer of an earlier
  release, made before the flag, refuses it as unknown: `upgrade --version V --unsafe-sysctls`
  then fails, and its failure is followed by a note saying, if that is why, to run it without
  the flag (that release gives the pods none).
  The kubelet calls them unsafe: a node runs such a pod only when its configuration allows each
  (`allowedUnsafeSysctls`), else the pod fails (`SysctlForbidden`), helm waits out `--timeout`
  and rolls back, and the error names `k8s tune`. Pod Security admits them only at the
  `privileged` level: `install`, `upgrade` and `reset-db` given `--unsafe-sysctls` refuse at once
  (an upgrade keeping the release's, its pods running with them today, warns instead: the
  cluster's admission configuration may exempt the namespace),
  before anything changes, a namespace whose `pod-security.kubernetes.io/enforce` label names
  another level (`--unsafe-sysctls: namespace NS enforces the Pod Security level baseline, which
  refuses pods given those settings: label it privileged (…), or give the pods none (--unsafe-sysctls=false)`); a
  namespace without the label (the cluster's default level applies), not there yet, or one the
  operator may not read is not checked. A pod's namespace copies its node's TCP buffer
  sizes and sizes its SYN queue from the node's memory, which the installer does not read: those
  are the node's own tuning. `k8s tune` prints the settings, the kubelet's `allowedUnsafeSysctls`
  list (and its `--allowed-unsafe-sysctls` flag) and how a node is tuned: `<short>-stow tune` on
  the node (see "Network tuning" ([tuning](tuning.md)); the daemon's file is left out where no Docker answers). With a
  `host` block it also prints what the project asks of every node, which the node's operator
  gives it: each module (`module <name>: loaded at boot` or `blocked`, its options, its reason) and
  each package (`package <name> (Debian's name)`, its reason), each firmware file (`firmware
  /lib/firmware/<path> (or compressed)`, its reason), each service's whole cores (`service <name>:
  N whole cores of its own`: a pod of the Guaranteed class on a kubelet of the static CPU manager
  policy), each interface (`interface <role>: a NIC playing it` or `a tun device its service opens`,
  with networkd's settings, and under it the node's firewall rules its `forward` and a service's
  `transparent` need: `iptables -I FORWARD …` and `ip6tables -I FORWARD …` for the role's traffic
  out and its replies, nft's `socket transparent 1` accept), each kernel argument (`kernel
  argument <name>[=<value>]`), each setting (`sysctl <key> = <value>`) and, under `host.power:
  performance`, what it holds the CPUs at.
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
  values file of the installer's, `stowage: {registry, secret, pullSecret, files, release:
  {installer, installerRelease}}`, then each `--values FILE` in order (they win).
  `release.installerRelease` is the release it was recorded for (`<version>/<revision>`), and
  `release.installer`, merged into
  the chart's `stowage.release`, is the release's installer image by the digest its release
  manifest records, read as the provision hook's below, on every install and upgrade (empty when
  it cannot be read, which is logged: `the release's installer image is not recorded (status
  --json leaves it out): …`, so a reused value never names another release's); `helm rollback`
  takes it back with the revision's values.
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
  Security standard requires —, no privilege gain, every capability dropped, a read-only root
  filesystem with `/tmp` an `emptyDir`, its only writable path besides the installer's), wrapped by that
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
  before), each by `secretKeyRef` (never `envFrom` of either Secret; a file input as its file, see "File inputs" ([installer](installer.md)) under the installer), a safe default it lists as
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
  (see "Schema baselines" ([installer](installer.md)); it names `reset-db --new-baseline`). The same release is applied again: that is
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
- `updates` says which release `upgrade --version` would move the release to, as the Docker
  shape's `updates` does ([installer](installer.md)): the latest revision of the version the
  channel's pointer names (the channel the release's values record, or `--channel C`), or with
  `--version V` V's latest revision on it,
  read from the registry the release records (or `--image-registry`; `--registry-ca` its CA, the
  Docker client's login for its host where it asks for one) and refused unless it is signed by a
  key `upgrade --version` trusts (the installer's, narrowed to those the values list). It prints
  `<release> on <channel>: release R in NS runs <release>` with the changelog, or `no upgrade on
  <channel>: release R in NS runs <release>, the release found is <release>`, another channel's
  release shown, never offered (`release R in NS follows <channel>: \`k8s upgrade --version\`
  moves it on <channel> alone`), and a later release published without an installer said so
  as on Docker; `--json` the same object
  as Docker's. No helm release there is refused with the missing release's refusal of every verb
  (exit 3, no object printed), and a `pending-…` release as `upgrade` refuses it. It changes
  nothing.
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
  (see "Extensions" ([extensions](extensions.md))), then `helm status`. `status --json` prints one JSON object for a program
  instead, with the Docker shape's `status --json` names (`format`, `name`, `installed` with `installer`,
  `channel`, `installing`, `previous`, `inputs`) and `helm` in place of `services`, never a
  value of the release's Secret:
  ```json
  {
    "format": 1,
    "name": "notes",
    "installed": { "version": "2.0.0", "revision": 1, "installer": "<prefix>/notes-installer@sha256:…" },
    "channel": "stable",
    "previous": { "version": "1.0.0", "revision": 1, "installer": "<prefix>/notes-installer@sha256:…" },
    "inputs": ["ADMIN_EMAIL"],
    "registry": "<prefix>",
    "helm": { "revision": 2, "status": "deployed" }
  }
  ```
  `installed` is what the release's values record (`stowage.release`, its `installer` absent
  when empty or when `installerRelease` names another release than the one they record — a value
  helm reused from another release's — as for `previous`); `installing` is `true` while helm's status is `pending-install`, and
  `install_failed` is never set (helm's status tells a failed operation); `previous` is the
  release the revision `k8s rollback` (no `--revision`) goes back to records in its values,
  `null` when there is no such revision (before an upgrade) or its values record no release of the
  project — never the revision `installed` is read from, though it may be the same version and
  revision of the project run with other values (an upgrade that changed only values). After `helm uninstall --keep-history`
  nothing runs: `installed` is `null` (and `channel`, `installing` and `inputs` are left out),
  `previous` is the latest revision's release, which `k8s rollback` brings back, and `helm` says
  `uninstalled`. A helm read that fails fails the command; `inputs` the keys of the release's inputs the Secret
  `<release>-stowage` holds, sorted (`[]` when none), told only when something is installed and this
  installer carries that release (its version and revision) — absent (unknown) otherwise; only
  then is the Secret read, and one of its name another tool wrote refused, as every verb refuses
  it; `registry` the image prefix the release's values record (`stowage.registry`: the last
  deploy's `--image-registry`, else the release's); `helm` the release's latest revision (`helm history`) and its status (`helm list`).
  With no release of that name in the namespace, `status` (with or without `--json`) is refused
  (exit 3, as Docker's `status` of a directory that holds no install); without `--json` it prints
  nothing on standard output, with it the refusal's object, `absent` `true` (as Docker's: `{"format":
  1, "refused": {"reason": "…", "fix": "…", "absent": true}}`), which every other refusal of
  `status --json` prints too, without `absent` (a release not installed from Stowage's chart);
  standard error says ``error: no release <release> in <namespace>: `<short>-stow
  k8s install --namespace NS --release R` installs <version>`` (`--kube-context C` added when
  given), and the refusal's fix is ``install first: `<that install>` ``. `provision`, `reset-db`,
  `secrets rotate` and `kit create` / `kit check` refuse it the same way.
- `secrets rotate KEY` is the Docker shape's three passes on the release's Secret
  `<release>-stowage`, run by the installer of the release it runs (another is refused, as are a
  release with no such Secret and a `pending-…` one): each pass
  applies the Secret, then restarts the Deployments, StatefulSets and DaemonSets the release's
  manifest makes in its namespace that read the key (a `secretKeyRef` of the Secret naming `KEY`,
  `KEY_NEXT`, `KEY_PREVIOUS` or `KEY_ACCEPTED`, or the whole Secret by `envFrom`, a secret volume or a projected
  one), one after the other (`kubectl rollout restart`, then `kubectl
  rollout status`, each bounded by `--timeout`). The chart gives `KEY_NEXT` and `KEY_PREVIOUS`
  (or `KEY_ACCEPTED`, the one of them that holds a value, as on Docker; the Secret holds the three)
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
  `<name>-installer` for `upgrade --version` and the provision hook's Job). No pull Secret is
  written for it unless asked, so give the chart your own pull Secret through `--values`, or
  `--image-pull-login`: on `k8s install` or `k8s upgrade` (passed on by `--version`), it writes
  the pull Secret `<release>-stowage-pull` for the mirror's host (`stowage.pullSecret`) from the
  Docker client's login the installer reads the mirror with (its config, `DOCKER_CONFIG`, its
  credential helpers): its user and password (or `auth`). It is noted (`note: the pull Secret …
  holds the Docker login for <host> (user U), which anyone reading Secrets in the namespace
  reads: …`) — give it a long-lived, read-only pull account, as a password a credential helper
  renews (ECR, gcloud) expires in the Secret. Refused (exit 3) before any input is asked and
  before the cluster changes: no login for the host (`--image-pull-login: no Docker login for
  <host>`), one with no user and password a node pulls with (a token, a user or a password
  alone), and the flag on the host of the release's own registry when its pull account is
  `token` (a mirror's path on that host included, the host's letter case aside: the operator's own
  login there is never copied; with the token's variable set, the run writes the pull account's
  Secret); an `auth` that is not base64 fails. On the release's own registry
  read with the Docker login (`docker-config`, `none`) it writes the Secret as for a mirror. An
  upgrade from the mirror the release records takes it too, `--image-registry` left out;
  `--dry-run` reads the login and notes the Secret a run writes (`note: a run writes the pull
  Secret …`), writing nothing. `k8s upgrade --version` of the release that runs applies nothing,
  and says `--image-pull-login is not applied: an upgrade to the release that runs changes
  nothing`; one fetching an installer made before the flag fails on it as unknown, and the run
  notes that installer writes no pull Secret from a login (run that version without the flag, and
  give the chart your own pull Secret through `--values`). `reset-db` keeps the pull Secret and the
  values naming it. A program with a pull account for the mirror gives it as a Docker config:
  `DOCKER_CONFIG` pointing at a directory whose `config.json` holds it.
- `uninstall` removes the release and what the installer made beside it: `helm uninstall
  <release> --wait`, then the provision Job `<release>-stowage` (`kubectl delete
  --cascade=foreground --wait`), the provision hook's RoleBinding, Role and ServiceAccount
  `<release>-stowage-provision`, and the Secrets `<release>-stowage-pull`, `<release>-stowage-provision`,
  `<release>-stowage-files`, `<release>-stowage-secret-files` and `<release>-stowage`. It deletes only an object whose labels say this installer made it for this
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
  - Data kept keeps the Secrets that answer for it: `<release>-stowage` and
    `<release>-stowage-secret-files` (the values it was made with, a database's password among
    them) and `<release>-stowage-provision` (what the provision
    hook returned). `k8s install` of the release then takes the data back with them, as it takes
    a Secret an install left. Data is kept when its claims are not deleted, when the chart keeps
    one (`helm.sh/resource-policy: keep`, `--data` or not), and when helm no longer knows the
    release. `kept:` names the `kubectl delete persistentvolumeclaims` and `kubectl delete secret`
    that remove them later (the release is gone then, so a second `uninstall` cannot tell its
    claims); an `uninstall` that finds nothing but those Secrets is refused, naming them.
  - A release helm no longer knows (gone, or `uninstalled` with its history kept) has its other
    objects deleted all the same, its claims untold (none deleted, `--data` or not); nothing of it
    at all is refused (`no release R in NS, and nothing <short>-stow made for it
    there`), and so is one whose names hold only another tool's objects, naming them (`…; another
    tool made secret R-stowage`).

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
