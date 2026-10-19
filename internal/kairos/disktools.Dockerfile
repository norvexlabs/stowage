# qemu-img, which converts the raw disk into the other formats, and sgdisk, which moves a grown
# disk's backup GPT to its end.
FROM debian:13-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
RUN apt-get update \
    && apt-get install -y --no-install-recommends qemu-utils gdisk \
    && rm -rf /var/lib/apt/lists/*
