# VHS (Charm's terminal recorder) with the Docker CLI and Compose, to record the installer
# against the host's engine.
FROM ghcr.io/charmbracelet/vhs:v0.10.0@sha256:e88ed3faa06183a197fd44ded83e706098d9e4038b72da94bcdb9cb9b67e3527
COPY --from=docker:28.3.3-cli@sha256:0135662b510037ea581d99c2e5929c5e01185139c0b86986a418bd4da0b98a44 /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker:28.3.3-cli@sha256:0135662b510037ea581d99c2e5929c5e01185139c0b86986a418bd4da0b98a44 /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
