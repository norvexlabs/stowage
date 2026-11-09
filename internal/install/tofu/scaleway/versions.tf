terraform {
  required_version = ">= 1.8"
  required_providers {
    # Pinned exactly: an upgrade is a reviewed change of `<short>-stow`, never a drift.
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
  project_id = var.project == "" ? null : var.project
}
