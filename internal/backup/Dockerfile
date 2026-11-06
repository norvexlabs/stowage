# The runner of the VM's backups: rclone, which uploads the encrypted dump the host daemon streams
# to it, lists the destination and prunes it, in a shell script. `stowage ship` builds it for each
# project with a pg-age-rclone backup and a kairos shape, publishes it next to the project's
# images, and the OS bundle preloads it; this file's bytes are its inputs. rclone is built from its
# release's source with the Go toolchain and the modules raised past a published vulnerability,
# each pinned by version (the checksum database checks every module), onto Alpine as its pinned
# base is (its CA certificates; no FUSE: the runner mounts nothing), so the recipe's bytes decide
# the image. Every base is pinned by digest.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder
ARG TARGETOS TARGETARCH
WORKDIR /build
RUN go mod init build \
 && go get github.com/rclone/rclone@v1.75.1 golang.org/x/net@v0.62.0 google.golang.org/grpc@v1.85.0 github.com/go-chi/chi/v5@v5.4.0 github.com/klauspost/compress@v1.20.2 github.com/andybalholm/brotli@v1.2.7 \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags '-s -w -X github.com/rclone/rclone/fs.Version=v1.75.1' -o /rclone github.com/rclone/rclone

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
COPY --from=builder /rclone /usr/local/bin/rclone
ENV XDG_CONFIG_HOME=/config
WORKDIR /data
ENTRYPOINT ["rclone"]
RUN rclone version | grep -q '^rclone v1\.75\.1$'
