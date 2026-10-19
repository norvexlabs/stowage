# A release's disk uploaded to Cloud Storage, for the image module to make an image of; destroyed
# once the image exists (the disk may carry the registry's pull account).

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

variable "file" {
  type = string
}

variable "sha256" {
  type = string
}

variable "project" {
  type = string
}

variable "bucket" {
  type = string
}

resource "google_storage_bucket_object" "image" {
  name   = "stowage/${var.name}.tar.gz"
  bucket = var.bucket
  source = var.file
}

output "upload" {
  value = "https://storage.googleapis.com/${var.bucket}/${google_storage_bucket_object.image.name}"
}
