# A release's disk registered on Google Cloud: a UEFI image made from its upload, which instances
# of the publisher's project boot from.

terraform {
  required_version = ">= 1.8"
  required_providers {
    # Pinned exactly: an upgrade is a reviewed change, never a drift.
    google = {
      source  = "hashicorp/google"
      version = "8.7.0"
    }
  }
}

# The account comes from the environment (GOOGLE_APPLICATION_CREDENTIALS, or the gcloud
# application-default login): never from a file here.
provider "google" {
  project = var.project
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

variable "project" {
  type = string
}

variable "bucket" {
  type = string
}

resource "google_compute_image" "image" {
  name        = var.name
  description = var.description

  raw_disk {
    source = var.upload
  }

  guest_os_features {
    type = "UEFI_COMPATIBLE"
  }

  # The image is made once: its upload is read only then, and removed after.
  lifecycle {
    ignore_changes = [raw_disk]
  }
}

output "id" {
  value = google_compute_image.image.id
}
