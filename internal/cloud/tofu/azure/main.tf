# A release's disk registered on Azure: a generation 2 image made from its upload in the publisher's
# resource group.

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

variable "description" {
  type = string
}

variable "upload" {
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

resource "azurerm_image" "image" {
  name                = var.name
  location            = var.location
  resource_group_name = var.resource_group
  hyper_v_generation  = "V2"
  tags = {
    description = var.description
  }

  os_disk {
    os_type      = "Linux"
    os_state     = "Generalized"
    blob_uri     = var.upload
    storage_type = "StandardSSD_LRS"
  }
}

output "id" {
  value = azurerm_image.image.id
}
