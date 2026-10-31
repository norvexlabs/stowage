FROM python:3.13-alpine@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a AS build
ARG FEATURES=core
COPY server/ /src/server/
RUN echo "features: ${FEATURES}" > /src/build-info

FROM python:3.13-alpine@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a AS runtime
ARG STOWAGE_PROJECT_VERSION=dev
ENV ATLAS_VERSION=${STOWAGE_PROJECT_VERSION}
# "true": this release reads a schema newer than its own, so a rollback to it is allowed until a
# rollback barrier.
LABEL com.example.atlas.reads-newer-schema="true"
RUN apk add --no-cache postgresql17-client
COPY --from=build /src/server/atlas-server /usr/local/bin/atlas-server
COPY --from=build /src/build-info /srv/build-info
COPY migrations/ /srv/migrations/
# The update_socket directory, the user's: a new volume takes it, owner included.
RUN install -d -o 65532 -g 65532 /run/atlas/handoff
USER 65532
EXPOSE 9090
CMD ["atlas-server", "api"]
