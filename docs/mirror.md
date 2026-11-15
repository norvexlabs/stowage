# Sites without internet (`<short>-stow mirror`)

[README](../README.md) · [Documentation index](../README.md#documentation)

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
  takes `mirror import --older` — but when the channel went back from a yank: the export of a
  channel carries, under their own tags, the manifests (with their signatures, not their
  artifacts) of the channel's later publications yanked since — each yank signed by a key this
  `<short>-stow` trusts (when it carries any), as an install counts a yank; an unsigned one is
  passed over (`not carrying the yank of <name> <tag>: …`), and a tag holding a manifest this
  Stowage does not read is passed over — logging `carrying the yank of <name>
  <version> (<tag>): <reason>`, and the import marks them yanked in the mirror, so `upgrade
  --version` there refuses them as at the source.
- The archive is a standard OCI image layout (`oci-layout`, `index.json`, `blobs/sha256/`) as one
  tar; each top-level manifest carries `org.opencontainers.image.ref.name` (its tag; none for a
  signature) and `dev.stowage.repository` (its repository under the registry prefix), so skopeo
  and oras read it as well.
- `mirror import FILE --to PREFIX` (on the site) pushes every manifest of the archive, with what
  it names, into `<PREFIX>/<repository>`, by digest and under its tag. Two imports into one
  prefix from one machine (as one user) run one after the other: each takes the lock
  `$XDG_CACHE_HOME/stowage/mirror-<hash of PREFIX>.lock` (`~/.cache` when unset) over its checks
  and its push, the second logging `waiting for another import into PREFIX` (an interrupt ends the
  wait: `waiting for another import into PREFIX: context canceled`); imports from several
  machines into one mirror are the operators' to order. The bytes are the
  published ones, so every digest is the same and the signatures still verify there. Before
  anything is pushed, the import checks offline, with the importing `<short>-stow`'s trusted keys
  and the verifier an install uses, that the archive's release manifest and each yank it carries
  are signed by one of them, reading the signatures from the archive's referrers (the release
  names every other manifest by digest, so its signature covers them, as an install's check of
  the release does); one unsigned, signed by another key or whose signature does not verify is
  refused, naming it — `<file>: <name> <version> (revision N, <tag>) is not signed by a key this
  install trusts: refused (<prefix>/<name>-release@<digest>: <why>): nothing imported`, the fix
  being to export the release again from its registry, whose signatures an export carries, or to
  import the archive with the `<short>-stow` of a release whose keys sign it. A `<short>-stow`
  that trusts no key (its project does not sign) checks nothing, as its installs check nothing.
  The release manifest and the channel's tag are pushed last, so the mirror never names a release
  whose artifacts it lacks. A blob or manifest whose content is not its digest's, a repository
  path or tag that is not one (`..`), or a repository that is not the release's own
  (`<name>-release`, `-installer`, `-os`, `-disk`, `<name>-<key>` for each image, and the chart's
  `charts/<name>-<channel>`; checked for the whole archive before anything is pushed) is
  refused. So is an archive whose `<name>-release` does not hold the release's tag and its
  channel's, on one manifest of a `<name>` release, and beside them only the tags of that channel's
  later publications, each on its yanked manifest; that tags anything else than the
  release's artifacts under the tags the release gives them (by the digests it records); whose
  release names an image this
  `<short>-stow`'s project does not (a release that adds one is imported with its own `<short>-stow`,
  in the archive's `<name>-installer`); whose release is an earlier publication of the channel
  than the one the mirror's channel holds (by the channel's publication number: the mirror's
  channel never goes back) — unless the archive carries the yank of the mirror's channel's
  publication, or `--older`, for a channel that went back where it is published (a yank moves it
  to the release before); whose yank of a tag finds another publication of it in the mirror
  (`<name> <tag> in <prefix> is publication N, not the yanked publication M the archive carries`;
  a tag the mirror lacks gets the yanked manifest); whose channel holds another project's
  release, or another channel's; or whose release, that same publication, the mirror records as
  yanked (a rebuild of its tag is another publication). It logs in with the Docker client's own login
  for that registry (`docker login`, `DOCKER_CONFIG`) and trusts `--registry-ca` besides the
  system's CAs. It prints `Imported <file> into <prefix>` with the archive's manifests,
  blobs and bytes. Importing again uploads no blob the registry holds already.
- Then each shape reads the mirror: `<short>-stow --registry PREFIX --registry-ca FILE` (Docker),
  `<short>-stow k8s install --image-registry PREFIX --registry-ca FILE` (Kubernetes) and `<short>-stow
  vm create --registry PREFIX --registry-ca FILE` (the Kairos VM). A newer release crosses the
  same way; `upgrade --version`, `updates` and the VM's `upgrade` find it in the mirror.
- A mirror that refuses anonymous reads: Docker reads it with the install's own login
  (`--registry-login-file FILE`, or `registry-login FILE` later; [installer](installer.md)), else
  the Docker client's login; Kubernetes with the pull Secret `--registry-login-file FILE` writes,
  or `--image-pull-login` from the Docker client's login ([kubernetes](kubernetes.md)); and the VM
  with the login `vm create --registry-login-file FILE` gives it over SSH (or `sudo <short>-stow
  registry-login -` on the VM), for the mirror's host alone ([vm](vm.md)).
- `vm create --create opentofu` also needs its module's OpenTofu provider, which `tofu init`
  fetches from the OpenTofu registry: on a site without internet, give `tofu` a provider mirror
  (a `provider_installation { filesystem_mirror { … } }` block in a CLI configuration file named
  by `TF_CLI_CONFIG_FILE`, filled by `tofu providers mirror` on a machine with internet).
