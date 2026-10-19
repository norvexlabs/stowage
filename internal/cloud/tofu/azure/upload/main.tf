# A release's fixed VHD uploaded as a page blob, for the image module to make an image of;
# destroyed once the image exists (the disk may carry the registry's pull account). The container
# is named by its id, so the storage account (and its keys) is never read.

terraform {
  required_version = ">= 1.8"
  required_providers {
    # Pinned exactly: an upgrade is a reviewed change, never a drift.
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "5.10.0"
    }
  }
}

# The account comes from the environment (ARM_CLIENT_ID, ARM_CLIENT_SECRET, ARM_TENANT_ID,
# ARM_SUBSCRIPTION_ID, or the az login): never from a file here. Blobs are written with that
# account (Entra ID), never with the storage account's keys.
provider "azurerm" {
  features {}
  storage_use_azuread = true
}

variable "name" {
  type = string
}

variable "file" {
  type = string
}

variable "sha256" {
  type = string
}

variable "location" {
  type = string
}

variable "resource_group" {
  type = string
}

variable "storage_account" {
  type = string
}

variable "container" {
  type = string
}

data "azurerm_client_config" "me" {}

resource "azurerm_storage_blob" "image" {
  name                 = "stowage-${var.name}.vhd"
  storage_container_id = "/subscriptions/${data.azurerm_client_config.me.subscription_id}/resourceGroups/${var.resource_group}/providers/Microsoft.Storage/storageAccounts/${var.storage_account}/blobServices/default/containers/${var.container}"
  type                 = "Page"
  source               = var.file
}

output "upload" {
  value = azurerm_storage_blob.image.url
}
