# The front every Docker install runs: Caddy with the Cloudflare DNS plugin, which the
# dns-cloudflare TLS mode needs. `stowage ship` builds it for each project, for linux/amd64, and
# publishes it next to the project's images; this file's bytes are its inputs. Every base is pinned
# by digest, and Caddy, the plugin and the modules raised past a published vulnerability by
# version. Never Caddy 2.11.6: it cuts the response to any request that carries a body at 60 s.
# Caddy is cross-compiled on the publisher's own platform, never under emulation.
FROM --platform=$BUILDPLATFORM caddy:2.11.7-builder@sha256:c4cd20eaa0b69112fd5d4f721ffc9671687e1d5d0b155c8620cf7546dbef94c0 AS builder
ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH xcaddy build v2.11.7 --with github.com/caddy-dns/cloudflare@v0.2.4 \
      --replace golang.org/x/net=golang.org/x/net@v0.62.0 --replace github.com/go-jose/go-jose/v4=github.com/go-jose/go-jose/v4@v4.1.7 \
      --replace github.com/go-chi/chi/v5=github.com/go-chi/chi/v5@v5.4.0 --replace github.com/klauspost/compress=github.com/klauspost/compress@v1.20.2

# Caddy's image less what the front never runs (curl and its libraries, the capability tools: the
# front runs as root), zlib past CVE-2026-4738, which the pinned base does not carry.
FROM caddy:2.11.7-alpine@sha256:e70b164da81ef698b3ad19af799ea6086ce6d9e48ed4079eb9fe26f6fbf14ee5
RUN apk del --no-cache curl libcap libcap-utils && apk add --no-cache 'zlib>=1.4'
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
RUN caddy version | grep -q '^v2\.11\.7 ' && caddy list-modules | grep -qx 'dns.providers.cloudflare' && ! command -v curl
