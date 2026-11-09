FROM python:3.13-alpine@sha256:1e1f6334529d1fb26a2ef619d7cffb30a7c7d7cc1cc7338ebee283a58d1aeaca
# zlib past CVE-2026-4738, which the pinned base does not carry.
RUN apk add --no-cache 'zlib>=1.4'
# The image runs Python's standard library alone: pip, and the packages it vendors, are removed.
RUN python -m pip uninstall -y -q pip && rm -rf /usr/local/lib/python3.13/ensurepip
COPY server/atlas-server /usr/local/bin/atlas-server
# A new named volume takes this directory's owner.
RUN mkdir -p /var/spool/atlas && chown 65532:65532 /var/spool/atlas
USER 65532
EXPOSE 8080
CMD ["atlas-server", "ingest"]
