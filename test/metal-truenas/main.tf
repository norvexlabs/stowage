# A bare-metal machine played by a TrueNAS VM, for test/e2e-full-vm.sh: an empty disk, the
# installer ISO and the seed as two CD-ROMs (the two USB sticks), UEFI. The disk boots first: empty,
# the firmware goes on to the installer, which installs the OS on it and powers the machine off;
# truenas_vm_power reads the machine stopped, so the next apply — the suite's power button —
# starts it again, and its disk then boots the installed system. Both media stay in, as an
# operator may leave them: the installed system pulls no seed. Test machinery only.
#
# What truenas/truenas cannot declare — the files on the host, the machine stopped before its
# devices are deleted — goes through Stowage's stopgap steps, `<short>-stow vm truenas seed|stop`,
# run by local-exec; they read the API's endpoint and key from the environment as the provider
# does, so no credential enters the state.

locals {
  base = "${var.pool}/${var.parent_dataset}/${lower(var.name)}"

  # A locally administered MAC derived from the name, for the one NIC of a release that names none.
  mac_hex = sha256("stowage-metal/${var.name}")
  mac = format("02:%s:%s:%s:%s:%s",
    substr(local.mac_hex, 0, 2), substr(local.mac_hex, 2, 2), substr(local.mac_hex, 4, 2),
  substr(local.mac_hex, 6, 2), substr(local.mac_hex, 8, 2))

  # The server's NICs, by the MACs the suite gives them (its own on a server), each on its bridge.
  nics = [for n in(length(var.nics) > 0 ? var.nics : [{ role = "", mac = local.mac, attach = "" }]) : {
    mac    = n.mac
    attach = n.attach != "" ? n.attach : var.nic_attach
  }]
}

# Holds the two media's files: TrueNAS's API deletes no file, so they live as long as this
# dataset. force_destroy deletes it with its snapshots.
resource "truenas_dataset" "staging" {
  name          = "${local.base}-staging"
  force_destroy = true
}

resource "truenas_zvol" "disk" {
  name          = local.base
  volsize       = var.disk_gib * 1024 * 1024 * 1024
  volblocksize  = 16384
  compression   = "lz4"
  force_destroy = true
}

resource "terraform_data" "installer" {
  triggers_replace = [truenas_dataset.staging.id, var.image_sha256]
  input            = "${truenas_dataset.staging.mountpoint}/installer-${substr(var.image_sha256, 0, 16)}.iso"

  provisioner "local-exec" {
    command = "exec \"$CTL\" vm truenas seed"
    environment = {
      CTL       = var.ctl
      SEED      = var.image
      SEED_PATH = self.input
    }
  }
}

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
  autostart        = false
  shutdown_timeout = 90
}

resource "truenas_vm_device" "disk" {
  vm    = truenas_vm.vm.id
  order = 1001
  attributes = jsonencode({
    dtype  = "DISK"
    path   = "/dev/zvol/${truenas_zvol.disk.id}"
    type   = "VIRTIO"
    iotype = "THREADS"
  })
}

resource "truenas_vm_device" "installer" {
  vm    = truenas_vm.vm.id
  order = 1002
  attributes = jsonencode({
    dtype = "CDROM"
    path  = terraform_data.installer.output
  })
}

resource "truenas_vm_device" "nic" {
  for_each = { for i, n in local.nics : tostring(i) => n }
  vm       = truenas_vm.vm.id
  order    = 1003 + tonumber(each.key)
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
  depends_on = [truenas_vm_device.disk, truenas_vm_device.installer, truenas_vm_device.nic, truenas_vm_device.seed]

  provisioner "local-exec" {
    when    = destroy
    command = "exec \"$CTL\" vm truenas stop"
    environment = {
      CTL   = self.input.ctl
      VM_ID = self.input.vm_id
    }
  }
}

# The power button: running, read back from the machine, so an apply starts it whenever it is off.
resource "truenas_vm_power" "vm" {
  vm_id      = truenas_vm.vm.id
  running    = true
  depends_on = [terraform_data.stop]
}
