terraform {
  required_version = ">= 1.8"
  required_providers {
    # Pinned exactly: an upgrade is a reviewed change of `<short>-stow`, never a drift.
    proxmox = {
      source  = "bpg/proxmox"
      version = "0.116.1"
    }
  }
}

# The API's address and token come from PROXMOX_VE_ENDPOINT and PROXMOX_VE_API_TOKEN (or
# PROXMOX_VE_USERNAME / PROXMOX_VE_PASSWORD): never from a file here.
provider "proxmox" {}
