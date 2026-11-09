# The database: PostgreSQL 18 as upstream ships it, its Alpine packages raised to the fixes
# published when the image is built, and its entrypoint's user switch done by Alpine's su-exec in
# place of upstream's gosu, a Go binary built with a Go release past its fixes (the release's
# audit gate refuses a fixable critical). The entrypoint runs `gosu postgres <command>`, which
# su-exec takes as it is.
FROM postgres:18-alpine@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395
RUN apk upgrade --no-cache && apk add --no-cache su-exec && ln -sf /sbin/su-exec /usr/local/bin/gosu
