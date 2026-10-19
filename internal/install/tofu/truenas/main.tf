# One TrueNAS VM of the release: its zvol written from the release's disk image, its cloud-config on
# a NoCloud seed ISO attached as a CD-ROM.
#
# The provider declares the VM, its devices, its zvol and a staging dataset. What it cannot declare
# — the bytes of the disk and the seed, the power, the snapshots that block a delete — goes through
# TrueNAS's own API from `<short>-stow vm truenas <step>`, run by local-exec: `<short>-stow` runs tofu
# with STOWAGE_CTL set to itself, and every step reads the API's address and key from
# TRUENAS_URL / TRUENAS_API_KEY / TRUENAS_INSECURE_SKIP_VERIFY, as the provider does, so no
# credential enters the state.

locals {
  # A locally administered MAC derived from the name: the same on every start and every apply (a
  # DHCP lease, and the host name a DHCP server registers, follow it), and stored nowhere.
  mac_hex = sha256("stowage-vm/${var.name}")
  mac = format("02:%s:%s:%s:%s:%s",
    substr(local.mac_hex, 0, 2), substr(local.mac_hex, 2, 2), substr(local.mac_hex, 4, 2),
  substr(local.mac_hex, 6, 2), substr(local.mac_hex, 8, 2))

  step = "exec \"$STOWAGE_CTL\" vm truenas"
}

# Holds the uploads (the image, the seed): TrueNAS's API deletes no file, so they live as long as
# this dataset.
#
# It and the zvol drop their snapshots right before they are deleted, destroyed or replaced: a
# periodic snapshot task may take them, the provider's deletes are not recursive, and TrueNAS
# refuses to delete a dataset that has any.
resource "truenas_dataset" "staging" {
  pool           = var.pool
  parent_dataset = var.parent_dataset
  name           = "${lower(var.name)}-staging"

  provisioner "local-exec" {
    when    = destroy
    command = "exec \"$STOWAGE_CTL\" vm truenas drop-snapshots"
    environment = {
      DATASET = self.id
    }
  }
}

resource "truenas_zvol" "disk" {
  pool         = var.pool
  name         = "${var.parent_dataset}/${lower(var.name)}"
  volsize      = var.disk_gib * 1024 * 1024 * 1024
  volblocksize = "16K"
  compression  = "LZ4"

  provisioner "local-exec" {
    when    = destroy
    command = "exec \"$STOWAGE_CTL\" vm truenas drop-snapshots"
    environment = {
      DATASET = self.id
    }
  }
}

# The disk's contents, written again when the zvol or the image changes. `terraform_data.power`
# depends on it, so its replacement powers the VM off first and on after: TrueNAS refuses to
# convert into a zvol a running VM uses.
resource "terraform_data" "image" {
  triggers_replace = [truenas_zvol.disk.id, var.image_sha256]

  provisioner "local-exec" {
    command = "${local.step} import-image"
    environment = {
      IMAGE        = var.image
      IMAGE_SHA256 = var.image_sha256
      STAGING_DIR  = truenas_dataset.staging.mount_point
      ZVOL         = truenas_zvol.disk.id
    }
  }
}

resource "terraform_data" "seed" {
  triggers_replace = [truenas_dataset.staging.id, var.seed_sha256]
  input            = "${truenas_dataset.staging.mount_point}/seed-${substr(var.seed_sha256, 0, 16)}.iso"

  provisioner "local-exec" {
    command = "${local.step} seed"
    environment = {
      SEED      = var.seed
      SEED_PATH = self.input
    }
  }
}

resource "truenas_vm" "vm" {
  name             = var.name
  description      = var.description
  vcpus            = 1
  cores            = var.cpus
  threads          = 1
  memory           = var.memory_mib
  bootloader       = "UEFI"
  cpu_mode         = "HOST-PASSTHROUGH"
  time             = "UTC"
  autostart        = true
  shutdown_timeout = 90
}

# The disk boots first: with the seed CD at a lower order, UEFI would try the seed.
resource "truenas_vm_device" "disk" {
  vm    = tonumber(truenas_vm.vm.id)
  dtype = "DISK"
  order = 1001
  attributes = {
    path   = "/dev/zvol/${truenas_zvol.disk.id}"
    type   = "VIRTIO"
    iotype = "THREADS"
  }
}

resource "truenas_vm_device" "nic" {
  vm    = tonumber(truenas_vm.vm.id)
  dtype = "NIC"
  order = 1002
  attributes = {
    type       = "VIRTIO"
    nic_attach = var.nic_attach
    mac        = local.mac
  }
}

resource "truenas_vm_device" "seed" {
  vm    = tonumber(truenas_vm.vm.id)
  dtype = "CDROM"
  order = 9999
  attributes = {
    path = terraform_data.seed.output
  }
}

# Power: starts the VM unless it runs. The one step that waits for the image: the provider's
# connection does not survive sitting idle through a conversion, so every provider resource is
# created before it.
#
# What the VM was applied with triggers it too — its shape and each device's attributes, read off
# the resources as applied: TrueNAS updates a running VM's definition in place and applies it at
# its next start only, so a changed one power-cycles the VM rather than report a change the guest
# does not run.
resource "terraform_data" "power" {
  triggers_replace = [
    terraform_data.image.id,
    truenas_vm.vm.id,
    truenas_vm_device.disk.id,
    truenas_vm_device.nic.id,
    truenas_vm_device.seed.id,
    [truenas_vm.vm.cores, truenas_vm.vm.memory],
    [truenas_vm_device.disk.attributes, truenas_vm_device.nic.attributes, truenas_vm_device.seed.attributes],
  ]

  input = truenas_vm.vm.id

  provisioner "local-exec" {
    command = "${local.step} start"
    environment = {
      VM_ID = self.input
    }
  }

  # Destroyed before the devices it depends on: TrueNAS refuses to delete a running VM's device,
  # so the VM is powered off first.
  provisioner "local-exec" {
    when    = destroy
    command = "exec \"$STOWAGE_CTL\" vm truenas stop"
    environment = {
      VM_ID = self.input
    }
  }
}
