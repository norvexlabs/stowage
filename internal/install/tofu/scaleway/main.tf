# One Scaleway instance of the release: its qcow2 image uploaded to Object Storage, imported as a
# Block Storage snapshot, made an x86_64 image (UEFI), and an instance booted from it with the
# cloud-config as its cloud-init user data, a routed public IPv4 address and, unless one is given,
# a security group dropping what SSH from its sources and the opened ports leave. A preview: not
# run on Scaleway yet.

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

# The VM's own firewall narrows these again: SSH from its sources, the front's and the L4 routes'
# ports from every source (a route's allowlist is the VM's own firewall's); replies and outbound
# traffic pass.
resource "scaleway_instance_security_group" "vm" {
  count                   = var.security_group_id == "" ? 1 : 0
  name                    = var.name
  description             = var.description
  stateful                = true
  inbound_default_policy  = "drop"
  outbound_default_policy = "accept"

  dynamic "inbound_rule" {
    for_each = var.ssh_sources
    content {
      action   = "accept"
      protocol = "TCP"
      port     = 22
      ip_range = inbound_rule.value
    }
  }

  dynamic "inbound_rule" {
    for_each = var.open_ports
    content {
      action   = "accept"
      protocol = upper(inbound_rule.value.protocol)
      port     = inbound_rule.value.port
      ip_range = "0.0.0.0/0"
    }
  }
}

resource "scaleway_instance_ip" "vm" {
  type = "routed_ipv4"
}

resource "scaleway_instance_server" "vm" {
  name              = var.name
  type              = var.instance_type
  image             = scaleway_instance_image.image.id
  ip_id             = scaleway_instance_ip.vm.id
  security_group_id = var.security_group_id == "" ? scaleway_instance_security_group.vm[0].id : var.security_group_id

  # Scaleway sizes volumes in GB (10^9 bytes), disk_gib is GiB: rounded up.
  root_volume {
    volume_type = "sbs_volume"
    size_in_gb  = ceil(var.disk_gib * 1.073741824)
  }

  user_data = {
    cloud-init = file(var.seed)
  }

  lifecycle {
    # A later release reaches the instance as an upgrade, never as a replacement.
    ignore_changes = [image, user_data]
  }
}
