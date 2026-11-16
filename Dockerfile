# syntax=docker/dockerfile:1
# Every build, test and check runs here: the host needs Docker and nothing else.
#   make check  → target checks   (gofmt, go vet, staticcheck, govulncheck, go test -race)
#   make build  → target bin      (static binaries exported to ./bin: stowage, which embeds the
#                                  stowage-install it ships for each platform installerbin.Platforms
#                                  names (cmd/platforms prints them), gzip-compressed
#                                  with its sha256 and size (internal/installerbin), the linux/amd64
#                                  stowage-install, the
#                                  installer's Docker smoke test, and self-audit, the release's
#                                  vulnerability gate)
#   make image  → target image    (distroless runtime image)
ARG GO_VERSION=1.27.4

FROM golang:${GO_VERSION}-bookworm AS base
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify
COPY . .

FROM base AS checks
# -race needs cgo; everything else builds static.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    unformatted="$(gofmt -l .)"; \
    if [ -n "$unformatted" ]; then echo "gofmt needed:"; echo "$unformatted"; exit 1; fi; \
    go vet ./...; \
    go vet -tags dockersmoke ./internal/install; \
    go tool staticcheck ./...; \
    go tool govulncheck ./...; \
    CGO_ENABLED=1 go test -race -count=1 ./...; \
    touch /checks.ok

FROM base AS build
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    mkdir -p /out/installers; \
    platforms=$(go run ./internal/installerbin/cmd/platforms); \
    test -n "$platforms"; \
    for platform in $platforms; do \
        os=${platform%/*}; arch=${platform#*/}; \
        GOOS=$os GOARCH=$arch go build -ldflags "-s -w -buildid= -X main.version=${VERSION}" -o /out/installers/stowage-install-$os-$arch ./cmd/stowage-install; \
        gzip -9 -n -c /out/installers/stowage-install-$os-$arch > internal/installerbin/stowage-install-$os-$arch.gz; \
        sha256sum < /out/installers/stowage-install-$os-$arch | cut -d' ' -f1 > internal/installerbin/stowage-install-$os-$arch.sha256; \
        wc -c < /out/installers/stowage-install-$os-$arch > internal/installerbin/stowage-install-$os-$arch.size; \
    done; \
    cp /out/installers/stowage-install-linux-amd64 /out/stowage-install; \
    go build -tags stowage_installer -ldflags "-s -w -buildid= -X main.version=${VERSION}" -o /out/stowage ./cmd/stowage; \
    go test -c -tags dockersmoke -o /out/install-smoke.test ./internal/install; \
    go build -o /out/self-audit ./test/selfaudit
# No binary leaves this file without green checks.
COPY --from=checks /checks.ok /out/.checks.ok

FROM scratch AS bin
COPY --from=build /out/stowage /out/stowage-install /out/install-smoke.test /out/self-audit /

FROM gcr.io/distroless/static-debian12:nonroot AS image
COPY --from=build /out/stowage /usr/local/bin/stowage
ENTRYPOINT ["/usr/local/bin/stowage"]
