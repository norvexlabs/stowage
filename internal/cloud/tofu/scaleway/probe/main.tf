# Looks for the available image of this name in the publisher's Scaleway project and zone: its id,
# or "" when none.

terraform {
  required_version = ">= 1.8"
  required_providers {
    # Pinned exactly: an upgrade is a reviewed change, never a drift.
    scaleway = {
      source  = "scaleway/scaleway"
      version = "2.91.0"
    }
  }
}

# The account comes from the environment (SCW_ACCESS_KEY / SCW_SECRET_KEY, or the scw CLI's
# configuration file and profile): never from a file here.
provider "scaleway" {
  zone       = var.zone
  region     = try(regex("^([a-z]+-[a-z]+)-[0-9]+$", var.zone)[0], null)
  project_id = var.project
}

variable "name" {
  type = string
}

variable "description" {
  type = string
}

variable "bucket" {
  type = string
}

variable "zone" {
  type = string
  validation {
    condition     = can(regex("^[a-z]+-[a-z]+-[0-9]+$", var.zone))
    error_message = "SCW_DEFAULT_ZONE is a Scaleway zone, e.g. fr-par-1, not a region."
  }
}

variable "project" {
  type = string
}

locals {
  # This disk's (the snapshot's tag, the description, names the disk's digest).
  tag = var.description
  # The list's name filter matches a part of the name, and its tags any of them: both are checked.
  snapshots = [for s in data.scaleway_block_snapshots.existing.snapshots : s if s.name == var.name && contains(s.tags, local.tag)]
  image     = length(data.scaleway_instance_image.named) > 0 ? data.scaleway_instance_image.named[0] : null
}

data "scaleway_block_snapshots" "existing" {
  name       = var.name
  tags       = [local.tag]
  project_id = var.project
}

# Scaleway lists no images by tag: the image is looked up by name once this disk's snapshot is
# found. Its lookup fails when none has the name: a snapshot left without its image (deleted by
# hand, or a run stopped between them whose working directory is gone) is deleted by hand.
data "scaleway_instance_image" "named" {
  count        = length(local.snapshots) > 0 ? 1 : 0
  name         = var.name
  architecture = "x86_64"
  project_id   = var.project
}

# The image's state when this disk's image is not available (creating, or error): "" when none.
output "unready" {
  value = local.image != null && try(local.image.state, "") != "available" ? "state ${try(local.image.state, "")}" : ""
}

output "id" {
  value = local.image != null && try(local.image.state, "") == "available" ? local.image.id : ""
}
