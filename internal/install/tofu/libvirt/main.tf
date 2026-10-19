# One libvirt / KVM domain of the release: its disk a copy-on-write volume over the release's image,
# grown to disk_gib, and its NoCloud seed a second volume (the OS reads the cidata label from any
# disk). A preview: not run on a libvirt host yet.

resource "libvirt_volume" "image" {
  name   = "${var.name}-image-${substr(var.image_sha256, 0, 12)}.qcow2"
  pool   = var.pool
  source = var.image
  format = "qcow2"
}

resource "libvirt_volume" "disk" {
  name           = "${var.name}.qcow2"
  pool           = var.pool
  base_volume_id = libvirt_volume.image.id
  size           = var.disk_gib * 1024 * 1024 * 1024
}

resource "libvirt_volume" "seed" {
  name   = "${var.name}-seed-${substr(var.seed_sha256, 0, 12)}.iso"
  pool   = var.pool
  source = var.seed
  format = "raw"
}

resource "libvirt_domain" "vm" {
  name        = var.name
  description = var.description
  vcpu        = var.cpus
  memory      = var.memory_mib
  firmware    = var.firmware
  machine     = "q35"
  autostart   = true

  cpu {
    mode = "host-passthrough"
  }

  disk {
    volume_id = libvirt_volume.disk.id
  }

  disk {
    volume_id = libvirt_volume.seed.id
  }

  network_interface {
    network_name   = var.network
    wait_for_lease = var.dhcp
  }

  boot_device {
    dev = ["hd"]
  }
}
