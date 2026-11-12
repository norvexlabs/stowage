# One Azure VM of the release, made from the image module's image (image/) with the cloud-config as
# its custom data. A preview: not run on Azure yet.

# The VM's public address: the one public_ip_name names in the resource group (reserved
# beforehand, kept by `vm destroy`), else one made for it.
resource "azurerm_public_ip" "vm" {
  count               = var.public_ip_name == "" ? 1 : 0
  name                = "${var.name}-ip"
  location            = var.location
  resource_group_name = var.resource_group
  allocation_method   = "Static"
  sku                 = "Standard"
}

# A state holding the address made without its index keeps it.
moved {
  from = azurerm_public_ip.vm
  to   = azurerm_public_ip.vm[0]
}

data "azurerm_public_ip" "given" {
  count               = var.public_ip_name == "" ? 0 : 1
  name                = var.public_ip_name
  resource_group_name = var.resource_group
}

locals {
  public_ip_id = var.public_ip_name == "" ? azurerm_public_ip.vm[0].id : data.azurerm_public_ip.given[0].id
  address      = var.public_ip_name == "" ? azurerm_public_ip.vm[0].ip_address : data.azurerm_public_ip.given[0].ip_address
}

resource "azurerm_network_interface" "vm" {
  name                = "${var.name}-nic"
  location            = var.location
  resource_group_name = var.resource_group
  # The NIC's own hardware path (ConnectX, MANA), whose drivers the image carries: the size must
  # offer it.
  accelerated_networking_enabled = var.accelerated_networking

  ip_configuration {
    name                          = "primary"
    subnet_id                     = var.subnet_id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = local.public_ip_id
  }
}

resource "azurerm_linux_virtual_machine" "vm" {
  name                       = var.name
  location                   = var.location
  resource_group_name        = var.resource_group
  size                       = var.vm_size
  admin_username             = var.admin_user
  network_interface_ids      = [azurerm_network_interface.vm.id]
  source_image_id            = var.image_id
  custom_data                = filebase64(var.seed)
  provision_vm_agent         = false
  allow_extension_operations = false

  admin_ssh_key {
    username   = var.admin_user
    public_key = var.ssh_public_key
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "StandardSSD_LRS"
    disk_size_gb         = var.disk_gib
  }

  lifecycle {
    # A later release reaches the VM as an upgrade, never as a replacement.
    ignore_changes = [source_image_id, custom_data]
  }
}
