# One Compute Engine instance of the release: its image's tar.gz uploaded to Cloud Storage, made a
# UEFI image, and an instance booted from it with the cloud-config as its user-data, and the
# firewall rules that reach it: SSH from its sources, the ports its own firewall opens. A preview:
# no suite runs it on Google Cloud.

locals {
  id = substr(var.image_sha256, 0, 12)
  # The rules reach the instance by a tag, its name; each rule's name is at most 63 characters.
  rule = "${substr(var.name, 0, 37)}-${substr(sha256(var.name), 0, 12)}"
  # IPv4 alone: the instance has no IPv6 address, and a rule takes one family.
  ssh_sources = [for s in var.ssh_sources : s if !strcontains(s, ":")]
  ports       = { for p in var.open_ports : p.protocol => tostring(p.port)... }
}

# The VM's own firewall accepts these sources alone; the network's other rules (the default
# network's default-allow-ssh) still let others reach it.
resource "google_compute_firewall" "ssh" {
  count         = length(local.ssh_sources) > 0 ? 1 : 0
  name          = "${local.rule}-ssh"
  description   = var.description
  network       = var.network
  direction     = "INGRESS"
  source_ranges = local.ssh_sources
  target_tags   = [var.name]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# The front's ports and the L4 routes' from every source: a route's allowlist is the VM's own
# firewall's.
resource "google_compute_firewall" "ports" {
  count         = length(local.ports) > 0 ? 1 : 0
  name          = "${local.rule}-ports"
  description   = var.description
  network       = var.network
  direction     = "INGRESS"
  source_ranges = ["0.0.0.0/0"]
  target_tags   = [var.name]

  dynamic "allow" {
    for_each = local.ports
    content {
      protocol = allow.key
      ports    = allow.value
    }
  }
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
  tags         = [var.name]

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
