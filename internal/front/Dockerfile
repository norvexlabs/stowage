# The front every Docker install runs: Caddy with the Cloudflare DNS plugin, which the
# dns-cloudflare TLS mode needs. `stowage ship` builds it for each project, for linux/amd64, and
# publishes it next to the project's images; this file's bytes are its inputs. Every base is pinned
# by digest and the plugin by version. Never Caddy 2.11.6: it cuts the response to any request
# that carries a body at 60 s.
# Caddy is cross-compiled on the publisher's own platform, never under emulation.
FROM --platform=$BUILDPLATFORM caddy:2.11.7-builder@sha256:b25f47453fa02f7e66c0828b4b1343658808b9000950b6825c4a1cb0c988f5f8 AS builder
ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH xcaddy build v2.11.7 --with github.com/caddy-dns/cloudflare@v0.2.4

FROM caddy:2.11.7-alpine@sha256:d8542f48d34a9cf4e4c11a478865229840e87e4c96ea3f439101f31a5d35f75f
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
RUN caddy version | grep -q '^v2\.11\.7 ' && caddy list-modules | grep -qx 'dns.providers.cloudflare'
