# The vulnerability scanner a signing release carries, which `<short>-stow audit` runs: grype,
# built from its release's source with the Go toolchain and the modules raised past a published
# vulnerability, each pinned by version (the checksum database checks every module), as grype's
# own image is laid out (the static binary and the CA certificates, its working directory /tmp).
# `stowage ship` builds it for each signing project and publishes it next to the project's images;
# this file's bytes are its inputs. The base is pinned by digest. grype is cross-compiled on the
# publisher's own platform, and its version read from its build information, never run there.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder
ARG TARGETOS TARGETARCH
RUN apk add --no-cache ca-certificates
WORKDIR /build
RUN go mod init build \
 && go get github.com/anchore/grype/cmd/grype@v0.121.0 golang.org/x/net@v0.62.0 github.com/go-jose/go-jose/v4@v4.1.7 \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags '-s -w -X main.version=0.121.0' -o /grype github.com/anchore/grype/cmd/grype \
 && go version -m /grype | awk '$1 == "mod" && $2 == "github.com/anchore/grype" && $3 == "v0.121.0" {ok = 1} END {exit !ok}'

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /grype /grype
WORKDIR /tmp
ENTRYPOINT ["/grype"]
