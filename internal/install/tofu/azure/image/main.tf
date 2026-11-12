# The release's disk on Azure, shared by every VM `<short>-stow vm create` makes of it in this
# resource group and storage account, and kept by `vm destroy`: its fixed VHD uploaded as a page
# blob once, made a generation 2 image.

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
