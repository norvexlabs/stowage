# The front every Docker install runs: Caddy with the Cloudflare DNS plugin, which the
# dns-cloudflare TLS mode needs, and the layer4 app (caddy-l4, by its tagged release, which has
# none for each Caddy release: one built against a Caddy no newer than this), which terminates TLS
# on an L4 route. `stowage ship` builds it for each project, for linux/amd64, and publishes it next
# to the project's images; this file's bytes are its inputs. Every base is pinned by digest, and
# Caddy, the plugins and the modules raised past a published vulnerability by version.
# Never Caddy 2.11.6: it cuts the response to any request that carries a body at 60 s.
# Caddy is cross-compiled on the publisher's own platform, never under emulation.
FROM --platform=$BUILDPLATFORM caddy:2.11.9-builder@sha256:3fddbec729442a56b942d4c9eaef1df80893dbf2cf5b86701bfae6aad1408eeb AS builder
ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH xcaddy build v2.11.9 --with github.com/caddy-dns/cloudflare@v0.2.4 --with github.com/mholt/caddy-l4@v0.1.2 \
      --replace golang.org/x/net=golang.org/x/net@v0.62.0 --replace github.com/go-jose/go-jose/v4=github.com/go-jose/go-jose/v4@v4.1.7 \
      --replace github.com/klauspost/compress=github.com/klauspost/compress@v1.20.2

# Caddy's image less what the front never runs (curl and its libraries, the capability tools: the
# front runs as root), zlib past CVE-2026-4738, which the pinned base does not carry.
FROM caddy:2.11.9-alpine@sha256:0bbb055bda6793c999bde78b1b5ce3e89f829fb031af2cf50056ea1246559fee
RUN apk del --no-cache curl libcap libcap-utils && apk add --no-cache 'zlib>=1.4'
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
RUN caddy version | grep -q '^v2\.11\.9 ' && caddy list-modules | grep -qx 'dns.providers.cloudflare' && \
    caddy list-modules | grep -qx 'layer4.handlers.tls' && caddy list-modules | grep -qx 'layer4.matchers.remote_ip' && ! command -v curl
