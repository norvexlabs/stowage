# Looks for the ready image of this name in the publisher's Google Cloud project: its id, or "" when none.

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

variable "project" {
  type = string
}

variable "bucket" {
  type = string
}

# A ready image of this disk only (its description names the disk's digest): one whose creation
# failed, or another disk's, is not registered.
data "google_compute_images" "existing" {
  filter = "(name = \"${var.name}\") (status = \"READY\") (description = \"${var.description}\")"
}

data "google_compute_images" "named" {
  filter = "name = \"${var.name}\""
}

# "not ready, or another disk's" when an image of this name is not this disk's ready one: "" when
# none.
# The images' list carries no status: the filtered list is compared with the whole one.
output "unready" {
  value = length(data.google_compute_images.named.images) > length(data.google_compute_images.existing.images) ? "not ready, or another disk's" : ""
}

output "id" {
  value = length(data.google_compute_images.existing.images) > 0 ? "projects/${var.project}/global/images/${var.name}" : ""
}
