# One vSphere VM of the release: deployed from the release's OVA, its disk grown to disk_gib, and its
# NoCloud seed uploaded to the datastore and on the CD-ROM. A preview: not run on vSphere yet.

data "vsphere_datacenter" "dc" {
  name = var.datacenter
}

data "vsphere_compute_cluster" "cluster" {
  name          = var.cluster
  datacenter_id = data.vsphere_datacenter.dc.id
}

data "vsphere_datastore" "ds" {
  name          = var.datastore
  datacenter_id = data.vsphere_datacenter.dc.id
}

data "vsphere_host" "host" {
  name          = var.host
  datacenter_id = data.vsphere_datacenter.dc.id
}

locals {
  nics = [for n in(length(var.nics) > 0 ? var.nics : [{ role = "", mac = "", attach = "" }]) : {
    mac    = n.mac
    attach = n.attach != "" ? n.attach : var.network
  }]
}

data "vsphere_network" "nic" {
  for_each      = toset([for n in local.nics : n.attach])
  name          = each.value
  datacenter_id = data.vsphere_datacenter.dc.id
}

resource "vsphere_file" "seed" {
  datacenter         = var.datacenter
  datastore          = var.datastore
  source_file        = var.seed
  destination_file   = "stowage/${var.name}-seed-${substr(var.seed_sha256, 0, 12)}.iso"
  create_directories = true
}

resource "vsphere_virtual_machine" "vm" {
  name             = var.name
  datacenter_id    = data.vsphere_datacenter.dc.id
  annotation       = var.description
  folder           = var.folder == "" ? null : var.folder
  resource_pool_id = data.vsphere_compute_cluster.cluster.resource_pool_id
  datastore_id     = data.vsphere_datastore.ds.id
  host_system_id   = data.vsphere_host.host.id
  num_cpus         = var.cpus
  memory           = var.memory_mib
  firmware         = "efi"

  # No VMware tools to report an address: the VM is not waited for here.
  wait_for_guest_net_timeout = 0
  wait_for_guest_ip_timeout  = 0

  ovf_deploy {
    local_ovf_path    = var.image
    disk_provisioning = "thin"
    ovf_network_map = {
      "VM Network" = data.vsphere_network.nic[local.nics[0].attach].id
    }
  }

  # The release's NICs, the primary first, each on its network (default var.network) with its MAC
  # (default vSphere's); or one.
  dynamic "network_interface" {
    for_each = local.nics
    content {
      network_id     = data.vsphere_network.nic[network_interface.value.attach].id
      use_static_mac = network_interface.value.mac != ""
      mac_address    = network_interface.value.mac != "" ? network_interface.value.mac : null
    }
  }

  disk {
    label = "disk0"
    size  = var.disk_gib
  }

  cdrom {
    datastore_id = data.vsphere_datastore.ds.id
    path         = vsphere_file.seed.destination_file
  }

  lifecycle {
    # The OVA is read once, at deployment: a later release's image is an upgrade, not a new VM.
    ignore_changes = [ovf_deploy]
  }
}
