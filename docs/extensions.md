# Extensions

[README](../README.md) · [Documentation index](../README.md#documentation)

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
  k8s install`, and the endpoint is given by `<short>-stow k8s upgrade --namespace NS --release R --kit FILE`, for a kit the extension issued, or `--given FILE`, for a file holding `project: <name>` and `inputs: {KEY: <its endpoint>}`; on the VM it is `sh <name>.sh` (Docker) or `sh
  <name>.sh vm create` (a VM), then `sudo <short>-stow install --set KEY=…`. An extension's
  `key_input`, a secret, is never a `--set` in the tip: an `--answers` file holding `KEY2: …` on
  Docker and the VM (a re-run reads no environment), the kit or file's `inputs` on Kubernetes. An extension whose input this shape does not ask has no tip. A
  dry run prints neither the endpoint nor the tips, and neither ever fails the run.
