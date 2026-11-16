# One TrueNAS VM of a release, for the VM suites (test/e2e-vm.sh): its zvol written from the
# release's disk, its cloud-config on a NoCloud seed ISO attached as a CD-ROM, its NICs by role.
# Test machinery only: the suites apply and destroy it with tofu.
#
# truenas/truenas declares the VM, its devices, its power, its zvol and a staging dataset. What it
# cannot declare — the disk's bytes into the zvol, the seed's file on the host, the VM stopped
# before its devices are deleted — goes through Stowage's stopgap steps, `<short>-stow vm truenas
# import-image|seed|stop`, run by local-exec; they read the API's endpoint and key from the
# environment as the provider does, so no credential enters the state.

locals {
  base = "${var.pool}/${var.parent_dataset}/${lower(var.name)}"

  # A locally administered MAC derived from the name, the root's own, for the primary NIC its seed
  # gives no MAC (its interfaces are [], or one role without a mac: the VM's first boot pins that
  # NIC's name to whatever MAC it has): the same on every apply, a NIC added later beside it
  # included (a DHCP lease, and the host name a DHCP server registers, follow it). Every other NIC
  # takes the MAC its seed printed, or one given for a NIC added later.
  mac_hex = sha256("stowage-vm/${var.name}")
  mac = format("02:%s:%s:%s:%s:%s",
    substr(local.mac_hex, 0, 2), substr(local.mac_hex, 2, 2), substr(local.mac_hex, 4, 2),
  substr(local.mac_hex, 6, 2), substr(local.mac_hex, 8, 2))

  nics = [for n in(length(var.nics) > 0 ? var.nics : [{ role = "", mac = "", attach = "" }]) : {
    mac    = n.mac != "" ? n.mac : local.mac
    attach = n.attach != "" ? n.attach : var.nic_attach
  }]
}

# Holds the uploads (the disk's staging copy, the seed): TrueNAS's API deletes no file, so they
# live as long as this dataset. force_destroy deletes it with its snapshots.
resource "truenas_dataset" "staging" {
  name          = "${local.base}-staging"
  force_destroy = true
}

# The disk image's digest: a new image is a new disk. It replaces the zvol, the disk device and
# terraform_data.stop, so the VM is stopped before the old device and zvol go, the image is written
# into a fresh zvol, and the device made again after it.
resource "terraform_data" "image_digest" {
  input = var.image_sha256
}

resource "truenas_zvol" "disk" {
  name          = local.base
  volsize       = var.disk_gib * 1024 * 1024 * 1024
  volblocksize  = 16384
  compression   = "lz4"
  force_destroy = true
  lifecycle {
    replace_triggered_by = [terraform_data.image_digest]
  }
}

# The disk's contents, written into each new zvol (import-image marks the zvol with the image it
# holds, so a re-run with the same image changes nothing).
resource "terraform_data" "image" {
  triggers_replace = [truenas_zvol.disk.id, var.image_sha256]

  provisioner "local-exec" {
    command = "exec \"$CTL\" vm truenas import-image"
    environment = {
      CTL          = var.ctl
      IMAGE        = var.image
      IMAGE_SHA256 = var.image_sha256
      STAGING_DIR  = truenas_dataset.staging.mountpoint
      ZVOL         = truenas_zvol.disk.id
    }
  }
}

# The seed's file on the host, named by its digest: a new seed is a new file.
resource "terraform_data" "seed" {
  triggers_replace = [truenas_dataset.staging.id, var.seed_sha256]
  input            = "${truenas_dataset.staging.mountpoint}/seed-${substr(var.seed_sha256, 0, 16)}.iso"

  provisioner "local-exec" {
    command = "exec \"$CTL\" vm truenas seed"
    environment = {
      CTL       = var.ctl
      SEED      = var.seed
      SEED_PATH = self.input
    }
  }
}

resource "truenas_vm" "vm" {
  name             = var.name
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

# Attached once the image is written: import-image refuses a zvol a VM device uses. Replaced with
# the zvol on a new image.
resource "truenas_vm_device" "disk" {
  vm    = truenas_vm.vm.id
  order = 1001
  attributes = jsonencode({
    dtype  = "DISK"
    path   = "/dev/zvol/${truenas_zvol.disk.id}"
    type   = "VIRTIO"
    iotype = "THREADS"
  })
  depends_on = [terraform_data.image]
  lifecycle {
    replace_triggered_by = [terraform_data.image_digest]
  }
}

resource "truenas_vm_device" "nic" {
  for_each = { for i, n in local.nics : tostring(i) => n }
  vm       = truenas_vm.vm.id
  order    = 1002 + tonumber(each.key)
  attributes = jsonencode({
    dtype      = "NIC"
    type       = "VIRTIO"
    nic_attach = each.value.attach
    mac        = each.value.mac
  })
}

resource "truenas_vm_device" "seed" {
  vm    = truenas_vm.vm.id
  order = 9999
  attributes = jsonencode({
    dtype = "CDROM"
    path  = terraform_data.seed.output
  })
}

# Destroyed before the devices, which it depends on: TrueNAS refuses to delete a running VM's
# device, and truenas_vm_power's delete leaves the VM running.
resource "terraform_data" "stop" {
  input = {
    vm_id = tostring(truenas_vm.vm.id)
    ctl   = var.ctl
  }
  depends_on = [truenas_vm_device.disk, truenas_vm_device.nic, truenas_vm_device.seed]
  lifecycle {
    replace_triggered_by = [terraform_data.image_digest]
  }

  provisioner "local-exec" {
    when    = destroy
    command = "exec \"$CTL\" vm truenas stop"
    environment = {
      CTL   = self.input.ctl
      VM_ID = self.input.vm_id
    }
  }
}

# Running, and restarted when what the VM reads at boot changes: a NIC added (TrueNAS applies a
# device at a start), the disk written again, the seed, the size.
resource "truenas_vm_power" "vm" {
  vm_id   = truenas_vm.vm.id
  running = true
  restart_triggers = {
    image = terraform_data.image.id
    shape = jsonencode([truenas_vm.vm.cores, truenas_vm.vm.memory])
    disk  = truenas_vm_device.disk.attributes
    nics  = jsonencode([for k, d in truenas_vm_device.nic : d.attributes])
    seed  = truenas_vm_device.seed.attributes
  }
  depends_on = [terraform_data.stop]
}
