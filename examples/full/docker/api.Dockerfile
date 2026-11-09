FROM python:3.13-alpine@sha256:1e1f6334529d1fb26a2ef619d7cffb30a7c7d7cc1cc7338ebee283a58d1aeaca AS build
ARG FEATURES=core
COPY server/ /src/server/
RUN echo "features: ${FEATURES}" > /src/build-info

FROM python:3.13-alpine@sha256:1e1f6334529d1fb26a2ef619d7cffb30a7c7d7cc1cc7338ebee283a58d1aeaca AS runtime
ARG STOWAGE_PROJECT_VERSION=dev
ENV ATLAS_VERSION=${STOWAGE_PROJECT_VERSION}
# "true": this release reads a schema newer than its own, so a rollback to it is allowed until a
# rollback barrier.
LABEL com.example.atlas.reads-newer-schema="true"
# zlib past CVE-2026-4738, which the pinned base does not carry.
RUN apk add --no-cache postgresql18-client 'zlib>=1.4'
# The image runs Python's standard library alone: pip, and the packages it vendors, are removed.
RUN python -m pip uninstall -y -q pip && rm -rf /usr/local/lib/python3.13/ensurepip
COPY --from=build /src/server/atlas-server /usr/local/bin/atlas-server
COPY --from=build /src/build-info /srv/build-info
# The schema from its named build context (images.api.contexts in stowage.yaml).
COPY --from=schema / /srv/migrations/
# The update_socket directory, the user's: a new volume takes it, owner included.
RUN install -d -o 65532 -g 65532 /run/atlas/handoff
USER 65532
EXPOSE 9090
CMD ["atlas-server", "api"]
