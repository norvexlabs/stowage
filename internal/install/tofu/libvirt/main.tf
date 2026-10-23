# One libvirt / KVM domain of the release: its disk a copy-on-write qcow2 volume over the release's
# image, grown to disk_gib, and its NoCloud seed a second, read-only volume (the OS reads the cidata
# label from any disk). The firmware is libvirt's UEFI autoselection, Secure Boot off. A preview:
# not run on a libvirt host yet.

resource "libvirt_volume" "image" {
  name = "${var.name}-image-${substr(var.image_sha256, 0, 12)}.${var.image_format}"
  pool = var.pool
  target = {
    format = { type = var.image_format }
  }
  create = {
    content = { url = var.image }
  }
}

resource "libvirt_volume" "disk" {
  name     = "${var.name}.qcow2"
  pool     = var.pool
  capacity = var.disk_gib * 1024 * 1024 * 1024
  target = {
    format = { type = "qcow2" }
  }
  backing_store = {
    path   = libvirt_volume.image.path
    format = { type = var.image_format }
  }
}

resource "libvirt_volume" "seed" {
  name = "${var.name}-seed-${substr(var.seed_sha256, 0, 12)}.iso"
  pool = var.pool
  target = {
    format = { type = "raw" }
  }
  create = {
    content = { url = var.seed }
  }
}

resource "libvirt_domain" "vm" {
  name        = var.name
  description = var.description
  type        = "kvm"
  vcpu        = var.cpus
  memory      = var.memory_mib
  memory_unit = "MiB"
  autostart   = true
  running     = true

  os = {
    type         = "hvm"
    type_machine = "q35"
    firmware     = "efi"
    firmware_info = {
      features = [
        { name = "secure-boot", enabled = "no" },
        { name = "enrolled-keys", enabled = "no" },
      ]
    }
    boot_devices = [{ dev = "hd" }]
  }

  cpu = {
    mode = "host-passthrough"
  }

  devices = {
    disks = [
      {
        source = {
          volume = { pool = var.pool, volume = libvirt_volume.disk.name }
        }
        target = { dev = "vda", bus = "virtio" }
        driver = { name = "qemu", type = "qcow2" }
      },
      {
        source = {
          volume = { pool = var.pool, volume = libvirt_volume.seed.name }
        }
        target    = { dev = "vdb", bus = "virtio" }
        driver    = { name = "qemu", type = "raw" }
        read_only = true
      },
    ]
    interfaces = [
      {
        model = { type = "virtio" }
        # A queue per vCPU, each on its own CPU in the guest, and the largest receive ring virtio
        # offers: the guest's tuning grows its ring to it.
        driver = { queues = var.cpus, rx_queue_size = 1024 }
        source = {
          network = { network = var.network }
        }
        # With DHCP the apply waits for the network's lease of an IPv4 address.
        wait_for_ip = var.dhcp ? { timeout = 300, source = "lease", network = "0.0.0.0/0" } : null
      },
    ]
  }
}

# The network's DHCP leases of the domain, read once it has one (with DHCP only).
data "libvirt_domain_interface_addresses" "vm" {
  count  = var.dhcp ? 1 : 0
  domain = libvirt_domain.vm.uuid
  source = "lease"
}
