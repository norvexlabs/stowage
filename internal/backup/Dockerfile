# The runner of the VM's backups: rclone, which uploads the encrypted dump the host daemon streams
# to it, lists the destination and prunes it. `stowage ship` builds it for each project with a
# pg-age-rclone backup and a kairos shape, publishes it next to the project's images, and the OS
# bundle preloads it; this file's bytes are its inputs. The base is pinned by digest.
FROM rclone/rclone:1.75.1@sha256:45401ad7410db1d67ffdb58e19059ad20b0d8e0285a60e38bbec55cc1019c7a5
RUN rclone version | grep -q '^rclone v1\.75\.1$'
