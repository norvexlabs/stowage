FROM python:3.13-alpine@sha256:7984cf56ceaddebe4fefeb75d64000b9f45d8b77b11a09b24524da6cdc3af0c8
# The image runs Python's standard library alone: pip, and the packages it vendors, are removed.
RUN python -m pip uninstall -y -q pip && rm -rf /usr/local/lib/python3.13/ensurepip
COPY server/atlas-server /usr/local/bin/atlas-server
# A new named volume takes this directory's owner.
RUN mkdir -p /var/spool/atlas && chown 65532:65532 /var/spool/atlas
USER 65532
EXPOSE 8080
CMD ["atlas-server", "ingest"]
