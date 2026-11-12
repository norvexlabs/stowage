# The release's disk on Google Cloud, shared by every VM `<short>-stow vm create` makes of it in
# this project and bucket, and kept by `vm destroy`: its tar.gz uploaded to Cloud Storage once,
# made a UEFI image declaring gVNIC.

locals {
  id = substr(var.image_sha256, 0, 12)
  # The disk's digest and this state's key: what every name holds.
  tag = "${local.id}-${substr(var.state_key, 0, 8)}"
}

resource "google_storage_bucket_object" "image" {
  name   = "stowage/${var.name}-${local.tag}.tar.gz"
  bucket = var.bucket
  source = var.image
}

# A Compute Engine name is at most 63 characters: the project's name is cut, and a hash of the
# whole name and the state's key keeps two names with a common start, or two states, apart.
resource "google_compute_image" "image" {
  name        = "${substr(var.name, 0, 30)}-${substr(sha256("${var.name}/${var.state_key}"), 0, 12)}-${local.id}"
  description = var.description

  raw_disk {
    source = "https://storage.googleapis.com/${var.bucket}/${google_storage_bucket_object.image.name}"
  }

  guest_os_features {
    type = "UEFI_COMPATIBLE"
  }

  # The image carries gVNIC's driver (gve): an instance may take Compute Engine's faster NIC.
  guest_os_features {
    type = "GVNIC"
  }
}
