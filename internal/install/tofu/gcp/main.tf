# One Compute Engine instance of the release: its image's tar.gz uploaded to Cloud Storage, made a
# UEFI image, and an instance booted from it with the cloud-config as its user-data. A preview: not
# run on Google Cloud yet.

locals {
  id = substr(var.image_sha256, 0, 12)
}

resource "google_storage_bucket_object" "image" {
  name   = "stowage/${var.name}-${local.id}.tar.gz"
  bucket = var.bucket
  source = var.image
}

# A Compute Engine name is at most 63 characters: the VM's name is cut, and a hash of the whole
# name keeps two names with a common start apart.
resource "google_compute_image" "image" {
  name        = "${substr(var.name, 0, 37)}-${substr(sha256(var.name), 0, 12)}-${local.id}"
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

resource "google_compute_instance" "vm" {
  name         = var.name
  description  = var.description
  machine_type = var.machine_type

  boot_disk {
    initialize_params {
      image = google_compute_image.image.self_link
      size  = var.disk_gib
    }
  }

  network_interface {
    nic_type   = var.nic_type
    network    = var.network
    subnetwork = var.subnetwork == "" ? null : var.subnetwork
    access_config {}
  }

  metadata = {
    user-data = file(var.seed)
  }

  lifecycle {
    # A later release reaches the instance as an upgrade, never as a replacement.
    ignore_changes = [boot_disk[0].initialize_params[0].image, metadata]
  }
}
