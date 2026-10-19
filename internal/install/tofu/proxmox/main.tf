# One Proxmox VE VM of the release: its disk imported from the release's image, uploaded as an ISO
# volume's file, and its NoCloud seed on the CD-ROM. A preview: not run on a Proxmox VE host yet.

resource "proxmox_virtual_environment_file" "image" {
  node_name    = var.node
  datastore_id = var.iso_datastore
  content_type = "iso"

  source_file {
    path      = var.image
    file_name = "${var.name}-${substr(var.image_sha256, 0, 12)}.img"
    checksum  = var.image_sha256
  }
}

resource "proxmox_virtual_environment_file" "seed" {
  node_name    = var.node
  datastore_id = var.iso_datastore
  content_type = "iso"

  source_file {
    path      = var.seed
    file_name = "${var.name}-seed-${substr(var.seed_sha256, 0, 12)}.iso"
    checksum  = var.seed_sha256
  }
}

resource "proxmox_virtual_environment_vm" "vm" {
  node_name   = var.node
  name        = var.name
  description = var.description
  bios        = "ovmf"
  machine     = "q35"
  on_boot     = true
  boot_order  = ["virtio0"]

  cpu {
    cores = var.cpus
    type  = "host"
  }

  memory {
    dedicated = var.memory_mib
  }

  efi_disk {
    datastore_id = var.datastore
    type         = "4m"
  }

  disk {
    datastore_id = var.datastore
    file_id      = proxmox_virtual_environment_file.image.id
    interface    = "virtio0"
    size         = var.disk_gib
  }

  cdrom {
    file_id   = proxmox_virtual_environment_file.seed.id
    interface = "ide2"
  }

  network_device {
    bridge = var.bridge
  }

  operating_system {
    type = "l26"
  }
}
