# One Proxmox VE VM of the release: its disk imported from the release's image, uploaded to a
# datastore with Import content, and its NoCloud seed, uploaded to one with ISO content, on the
# CD-ROM. Both uploads go through the API, with the token alone (no SSH to the node). A preview: not
# run on a Proxmox VE host yet.

resource "proxmox_virtual_environment_file" "image" {
  node_name    = var.node
  datastore_id = var.import_datastore
  content_type = "import"

  source_file {
    path      = var.image
    file_name = "${var.name}-${substr(var.image_sha256, 0, 12)}.${var.image_format}"
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
    import_from  = proxmox_virtual_environment_file.image.id
    interface    = "virtio0"
    size         = var.disk_gib
  }

  cdrom {
    file_id   = proxmox_virtual_environment_file.seed.id
    interface = "ide2"
  }

  # Virtio with a queue per vCPU (Proxmox's limit is 64), each on its own CPU in the guest.
  network_device {
    bridge = var.bridge
    model  = "virtio"
    queues = min(var.cpus, 64)
  }

  operating_system {
    type = "l26"
  }
}
