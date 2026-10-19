# Looks for the image of this name in the publisher's Azure resource group: its id, or "" when none.

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

data "azurerm_resources" "existing" {
  type                = "Microsoft.Compute/images"
  name                = var.name
  resource_group_name = var.resource_group
  # This disk's (its description tag names the disk's digest).
  required_tags = {
    description = var.description
  }
}

# Azure's resource list gives no provisioning state: an image of this name is taken as it is.
output "unready" {
  value = ""
}

output "id" {
  value = length(data.azurerm_resources.existing.resources) > 0 ? data.azurerm_resources.existing.resources[0].id : ""
}
