terraform {
  required_version = ">= 1.8"
  required_providers {
    # Pinned exactly: an upgrade is a reviewed change of `<short>-stow`, never a drift.
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
  zone    = var.zone
}
