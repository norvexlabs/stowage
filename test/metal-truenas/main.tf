# A bare-metal machine played by a TrueNAS VM, for test/e2e-full-vm.sh: an empty disk, the
# installer ISO and the seed as two CD-ROMs (the two USB sticks), UEFI. The disk boots first: empty,
# the firmware goes on to the installer, which installs the OS on it and powers the machine off;
# the power button is a loop that starts the machine again, whose disk then boots the installed
# system. Both media stay in, as an operator may leave them: the installed system pulls no seed.
# Test machinery only: no `<short>-stow` platform uses it.

locals {
  mac_hex = sha256("stowage-metal/${var.name}")
  mac = format("02:%s:%s:%s:%s:%s",
    substr(local.mac_hex, 0, 2), substr(local.mac_hex, 2, 2), substr(local.mac_hex, 4, 2),
  substr(local.mac_hex, 6, 2), substr(local.mac_hex, 8, 2))
  step = "exec \"$STOWAGE_CTL\" vm truenas"
}

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

resource "terraform_data" "installer" {
  triggers_replace = [truenas_dataset.staging.id, var.image_sha256]
  input            = "${truenas_dataset.staging.mount_point}/installer-${substr(var.image_sha256, 0, 16)}.iso"

  provisioner "local-exec" {
    command = "${local.step} seed"
    environment = {
      SEED      = var.image
      SEED_PATH = self.input
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
  autostart        = false
  shutdown_timeout = 90
}

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

resource "truenas_vm_device" "installer" {
  vm    = tonumber(truenas_vm.vm.id)
  dtype = "CDROM"
  order = 1002
  attributes = {
    path = terraform_data.installer.output
  }
}

# The server's NICs, by the MACs the suite gives them (its own on a server), each on its bridge.
resource "truenas_vm_device" "nic" {
  for_each = { for i, n in(length(var.nics) > 0 ? var.nics : [{ role = "", mac = local.mac, attach = "" }]) : tostring(i) => n }
  vm       = tonumber(truenas_vm.vm.id)
  dtype    = "NIC"
  order    = 1003 + tonumber(each.key)
  attributes = {
    type       = "VIRTIO"
    nic_attach = each.value.attach != "" ? each.value.attach : var.nic_attach
    mac        = each.value.mac != "" ? each.value.mac : local.mac
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

# The power button: started now, and again whenever it is off until the suite says it answers.
resource "terraform_data" "power" {
  triggers_replace = [truenas_vm.vm.id, truenas_vm_device.disk.id, truenas_vm_device.installer.id, [for d in truenas_vm_device.nic : d.id], truenas_vm_device.seed.id]
  input            = truenas_vm.vm.id

  provisioner "local-exec" {
    command = <<-EOT
      "$STOWAGE_CTL" vm truenas start
      setsid sh -c 'i=0; while [ $i -lt 240 ] && [ ! -e "$STOP_FILE" ]; do sleep 15; "$STOWAGE_CTL" vm truenas start; i=$((i + 1)); done' </dev/null >/dev/null 2>&1 &
    EOT
    environment = {
      VM_ID     = self.input
      STOP_FILE = var.stop_file
    }
  }

  provisioner "local-exec" {
    when    = destroy
    command = "exec \"$STOWAGE_CTL\" vm truenas stop"
    environment = {
      VM_ID = self.input
    }
  }
}
