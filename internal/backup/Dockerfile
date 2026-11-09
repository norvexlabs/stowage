# The runner of the VM's backups: rclone, which uploads the encrypted dump the host daemon streams
# to it, lists the destination and prunes it, in a shell script. `stowage ship` builds it for each
# project with a pg-age-rclone backup and a kairos shape, publishes it next to the project's
# images, and the OS bundle preloads it; this file's bytes are its inputs. rclone is built from its
# release's source with the Go toolchain, its modules as the release pins them (the checksum
# database checks every module; a module raised past a published vulnerability is pinned here by
# version until a release carries it), onto Alpine as its pinned
# base is (its CA certificates; no FUSE: the runner mounts nothing), but for zlib, raised past
# CVE-2026-4738, which the base does not carry. Every base is pinned by digest.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS builder
ARG TARGETOS TARGETARCH
WORKDIR /build
RUN go mod init build \
 && go get github.com/rclone/rclone@v1.76.0 \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags '-s -w -X github.com/rclone/rclone/fs.Version=v1.76.0' -o /rclone github.com/rclone/rclone

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache 'zlib>=1.4'
COPY --from=builder /rclone /usr/local/bin/rclone
ENV XDG_CONFIG_HOME=/config
WORKDIR /data
ENTRYPOINT ["rclone"]
RUN rclone version | grep -q '^rclone v1\.76\.0$'
