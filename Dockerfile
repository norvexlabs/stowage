# syntax=docker/dockerfile:1
# Every build, test and check runs here: the host needs Docker and nothing else.
#   make check  → target checks   (gofmt, go vet, staticcheck, govulncheck, go test -race)
#   make build  → target bin      (static binaries exported to ./bin: stowage, which embeds the
#                                  linux/amd64 stowage-install it ships, stowage-install, and the
#                                  installer's Docker smoke test)
#   make image  → target image    (distroless runtime image)
ARG GO_VERSION=1.27.3

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
    GOOS=linux GOARCH=amd64 go build -ldflags "-s -w -buildid= -X main.version=${VERSION}" -o /out/stowage-install ./cmd/stowage-install; \
    cp /out/stowage-install internal/installerbin/stowage-install; \
    go build -tags stowage_installer -ldflags "-s -w -buildid= -X main.version=${VERSION}" -o /out/stowage ./cmd/stowage; \
    go test -c -tags dockersmoke -o /out/install-smoke.test ./internal/install
# No binary leaves this file without green checks.
COPY --from=checks /checks.ok /out/.checks.ok

FROM scratch AS bin
COPY --from=build /out/stowage /out/stowage-install /out/install-smoke.test /

FROM gcr.io/distroless/static-debian12:nonroot AS image
COPY --from=build /out/stowage /usr/local/bin/stowage
ENTRYPOINT ["/usr/local/bin/stowage"]
