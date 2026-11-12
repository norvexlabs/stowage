# One Compute Engine instance of the release, booted from the image module's image (image/) with
# the cloud-config as its user-data, and, with
# firewall, the rules that reach it: SSH from its sources, the ports its own firewall opens. A preview:
# no suite runs it on Google Cloud.

locals {
  # The rules reach the instance by a tag, its name; each rule's name is at most 63 characters.
  rule = "${substr(var.name, 0, 37)}-${substr(sha256(var.name), 0, 12)}"
  # IPv4 alone: the instance has no IPv6 address, and a rule takes one family.
  ssh_sources = [for s in var.ssh_sources : s if !strcontains(s, ":")]
  # The opened ports grouped by their IPv4 sources, a rule each: a port none reaches has none.
  port_groups = { for p in var.open_ports : join(",", sort([for s in p.sources : s if !strcontains(s, ":")])) => p... if length([for s in p.sources : s if !strcontains(s, ":")]) > 0 }
}

# The VM's own firewall accepts these sources alone; the network's other rules (the default
# network's default-allow-ssh) still let others reach it.
resource "google_compute_firewall" "ssh" {
  count         = var.firewall && length(local.ssh_sources) > 0 ? 1 : 0
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

# The front's ports from every source, an L4 route's from the sources its allowlist held at
# create; the VM's own firewall narrows them again.
resource "google_compute_firewall" "ports" {
  for_each      = var.firewall ? local.port_groups : {}
  name          = "${local.rule}-p${substr(sha256(each.key), 0, 8)}"
  description   = var.description
  network       = var.network
  direction     = "INGRESS"
  source_ranges = split(",", each.key)
  target_tags   = [var.name]

  dynamic "allow" {
    for_each = { for p in each.value : p.protocol => tostring(p.port)... }
    content {
      protocol = allow.key
      ports    = allow.value
    }
  }
}

resource "google_compute_instance" "vm" {
  name         = var.name
  description  = var.description
  machine_type = var.machine_type
  tags         = [var.name]

  boot_disk {
    initialize_params {
      image = var.image_id
      size  = var.disk_gib
    }
  }

  network_interface {
    nic_type   = var.nic_type
    network    = var.network
    subnetwork = var.subnetwork == "" ? null : var.subnetwork
    # The external address: nat_ip's, reserved beforehand and kept by `vm destroy`, else an
    # ephemeral one.
    access_config {
      nat_ip = var.nat_ip == "" ? null : var.nat_ip
    }
  }

  metadata = {
    user-data = file(var.seed)
  }

  lifecycle {
    # A later release reaches the instance as an upgrade, never as a replacement.
    ignore_changes = [boot_disk[0].initialize_params[0].image, metadata]
  }
}
