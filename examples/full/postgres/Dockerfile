# The database: PostgreSQL 18 as upstream ships it, its Alpine packages raised to the fixes
# published when the image is built, and its entrypoint's user switch done by Alpine's su-exec in
# place of upstream's gosu, a Go binary built with a Go release past its fixes (the release's
# audit gate refuses a fixable critical). The entrypoint runs `gosu postgres <command>`, which
# su-exec takes as it is.
FROM postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5
RUN apk upgrade --no-cache && apk add --no-cache su-exec && ln -sf /sbin/su-exec /usr/local/bin/gosu
