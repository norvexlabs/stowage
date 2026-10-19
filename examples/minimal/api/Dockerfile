FROM python:3.13-alpine@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a
ARG STOWAGE_PROJECT_VERSION=dev
ENV NOTES_VERSION=${STOWAGE_PROJECT_VERSION}
COPY notes-api /usr/local/bin/notes-api
COPY schema.json /usr/local/share/notes/schema.json
# A new named volume takes this directory's owner.
RUN mkdir -p /data/uploads && chown 1000:1000 /data/uploads
USER 1000
EXPOSE 8000
CMD ["notes-api", "serve"]
