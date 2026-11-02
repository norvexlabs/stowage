# The helper that gives an install's data volumes their uid: busybox's sh, stat and chown, run as
# root with the volumes mounted, before the stack starts. `stowage ship` builds it for each project
# with a data volume of a uid and the docker or kairos shape, publishes it next to the project's
# images, and the OS bundle preloads it; this file's bytes are its inputs. The base is pinned by
# digest.
FROM busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
RUN busybox | head -n 1 | grep -q '^BusyBox v1\.37\.0 '
