FROM python:3.13-alpine@sha256:7984cf56ceaddebe4fefeb75d64000b9f45d8b77b11a09b24524da6cdc3af0c8 AS build
ARG FEATURES=core
COPY server/ /src/server/
RUN echo "features: ${FEATURES}" > /src/build-info

FROM python:3.13-alpine@sha256:7984cf56ceaddebe4fefeb75d64000b9f45d8b77b11a09b24524da6cdc3af0c8 AS runtime
ARG STOWAGE_PROJECT_VERSION=dev
ENV ATLAS_VERSION=${STOWAGE_PROJECT_VERSION}
# "true": this release reads a schema newer than its own, so a rollback to it is allowed until a
# rollback barrier.
LABEL com.example.atlas.reads-newer-schema="true"
RUN apk add --no-cache postgresql18-client
# The image runs Python's standard library alone: pip, and the packages it vendors, are removed.
RUN python -m pip uninstall -y -q pip && rm -rf /usr/local/lib/python3.13/ensurepip
COPY --from=build /src/server/atlas-server /usr/local/bin/atlas-server
COPY --from=build /src/build-info /srv/build-info
COPY migrations/ /srv/migrations/
# The update_socket directory, the user's: a new volume takes it, owner included.
RUN install -d -o 65532 -g 65532 /run/atlas/handoff
USER 65532
EXPOSE 9090
CMD ["atlas-server", "api"]
