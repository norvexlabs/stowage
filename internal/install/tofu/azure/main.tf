# One Azure VM of the release: its fixed VHD uploaded as a page blob, made a generation 2 image, and
# a VM made from it with the cloud-config as its custom data. A preview: not run on Azure yet.

locals {
  id = substr(var.image_sha256, 0, 12)
}

data "azurerm_storage_account" "images" {
  name                = var.storage_account
  resource_group_name = var.resource_group
}

data "azurerm_storage_container" "images" {
  name               = var.container
  storage_account_id = data.azurerm_storage_account.images.id
}

resource "azurerm_storage_blob" "image" {
  name                 = "stowage-${var.name}-${local.id}.vhd"
  storage_container_id = data.azurerm_storage_container.images.id
  type                 = "Page"
  source               = var.image
}

resource "azurerm_image" "image" {
  name                = "${var.name}-${local.id}"
  location            = var.location
  resource_group_name = var.resource_group
  hyper_v_generation  = "V2"

  os_disk {
    os_type      = "Linux"
    os_state     = "Generalized"
    blob_uri     = azurerm_storage_blob.image.url
    storage_type = "StandardSSD_LRS"
  }
}

resource "azurerm_public_ip" "vm" {
  name                = "${var.name}-ip"
  location            = var.location
  resource_group_name = var.resource_group
  allocation_method   = "Static"
  sku                 = "Standard"
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
    public_ip_address_id          = azurerm_public_ip.vm.id
  }
}

resource "azurerm_linux_virtual_machine" "vm" {
  name                       = var.name
  location                   = var.location
  resource_group_name        = var.resource_group
  size                       = var.vm_size
  admin_username             = var.admin_user
  network_interface_ids      = [azurerm_network_interface.vm.id]
  source_image_id            = azurerm_image.image.id
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
