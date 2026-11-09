# A release's disk uploaded to Object Storage, for the image module to import as a snapshot;
# destroyed once the image exists (the disk may carry the registry's pull account).

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
# configuration file and profile): never from a file here. The bucket is in the zone's region,
# where the snapshot's import reads it.
provider "scaleway" {
  zone       = var.zone
  region     = try(regex("^([a-z]+-[a-z]+)-[0-9]+$", var.zone)[0], null)
  project_id = var.project
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

# One PUT: Object Storage takes at most 5 GB so.
resource "scaleway_object" "image" {
  bucket     = var.bucket
  key        = "stowage/${var.name}.qcow2"
  file       = var.file
  hash       = var.sha256
  visibility = "private"

  timeouts {
    create = "60m"
  }
}

output "upload" {
  value = scaleway_object.image.key
}
