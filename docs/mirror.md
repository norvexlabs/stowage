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
- A mirror that refuses anonymous reads: Docker reads it with the Docker client's login,
  Kubernetes with the pull Secret `--image-pull-login` writes from that login, and the VM with the
  login `vm create --registry-login-file FILE` gives it over SSH (or `sudo <short>-stow
  registry-login -` on the VM), for the mirror's host alone ([vm](vm.md)).
- `vm create --create opentofu` also needs its module's OpenTofu provider, which `tofu init`
  fetches from the OpenTofu registry: on a site without internet, give `tofu` a provider mirror
  (a `provider_installation { filesystem_mirror { … } }` block in a CLI configuration file named
  by `TF_CLI_CONFIG_FILE`, filled by `tofu providers mirror` on a machine with internet).
