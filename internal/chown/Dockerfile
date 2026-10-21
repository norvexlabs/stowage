# The helper that gives an install's data volumes their uid: busybox's sh, stat and chown, run as
# root with the volumes mounted, before the stack starts. `stowage ship` builds it for each project
# with a data volume of a uid and the docker or kairos shape, publishes it next to the project's
# images, and the OS bundle preloads it; this file's bytes are its inputs. The base is pinned by
# digest.
FROM busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
RUN busybox | head -n 1 | grep -q '^BusyBox v1\.37\.0 '
