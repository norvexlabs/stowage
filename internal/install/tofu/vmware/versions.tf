terraform {
  required_version = ">= 1.8"
  required_providers {
    # Pinned exactly: an upgrade is a reviewed change of `<short>-stow`, never a drift.
    vsphere = {
      source  = "hashicorp/vsphere"
      version = "2.17.0"
    }
  }
}

# The server and the account come from VSPHERE_SERVER, VSPHERE_USER and VSPHERE_PASSWORD (and
# VSPHERE_ALLOW_UNVERIFIED_SSL): never from a file here.
provider "vsphere" {}
