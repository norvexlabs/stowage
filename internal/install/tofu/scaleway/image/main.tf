# The release's disk on Scaleway, shared by every VM `<short>-stow vm create` makes of it in this zone
# and bucket, and kept by `vm destroy`: its qcow2 image uploaded to Object Storage once, imported
# as a Block Storage snapshot, made an x86_64 image (UEFI).

locals {
  id = substr(var.image_sha256, 0, 12)
}

resource "scaleway_object" "image" {
  bucket     = var.bucket
  key        = "stowage/${var.name}-${local.id}.qcow2"
  file       = var.image
  hash       = var.image_sha256
  visibility = "private"

  timeouts {
    create = "60m"
  }
}

resource "scaleway_block_snapshot" "image" {
  name = "${var.name}-${local.id}"

  import {
    bucket = scaleway_object.image.bucket
    key    = scaleway_object.image.key
  }

  timeouts = {
    create = "60m"
  }
}

resource "scaleway_instance_image" "image" {
  name           = "${var.name}-${local.id}"
  root_volume_id = scaleway_block_snapshot.image.id
  architecture   = "x86_64"
}
