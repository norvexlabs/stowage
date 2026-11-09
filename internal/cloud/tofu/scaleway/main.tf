# A release's disk registered on Scaleway: its upload imported as a Block Storage snapshot, and an
# x86_64 instance image of it, which instances of the publisher's project boot (UEFI).

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

variable "upload" {
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

# Tagged with the description, which names the disk's digest: the probe finds this disk's
# snapshot by it.
resource "scaleway_block_snapshot" "image" {
  name = var.name
  tags = [var.description]

  import {
    bucket = var.bucket
    key    = var.upload
  }

  timeouts = {
    create = "60m"
  }

  # The snapshot is imported once: its upload is read only then, and removed after.
  lifecycle {
    ignore_changes = [import]
  }
}

resource "scaleway_instance_image" "image" {
  name           = var.name
  root_volume_id = scaleway_block_snapshot.image.id
  architecture   = "x86_64"
  tags           = [var.description]
}

output "id" {
  value = scaleway_instance_image.image.id
}
