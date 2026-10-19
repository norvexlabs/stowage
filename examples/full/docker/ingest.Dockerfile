FROM python:3.13-alpine@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a
COPY server/atlas-server /usr/local/bin/atlas-server
# A new named volume takes this directory's owner.
RUN mkdir -p /var/spool/atlas && chown 65532:65532 /var/spool/atlas
USER 65532
EXPOSE 8080
CMD ["atlas-server", "ingest"]
