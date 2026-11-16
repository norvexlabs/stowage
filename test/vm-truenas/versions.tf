terraform {
  required_version = ">= 1.8"
  required_providers {
    # TrueNAS's own provider, pinned exactly with its checksums in .terraform.lock.hcl.
    truenas = {
      source  = "truenas/truenas"
      version = "1.9.1"
    }
  }
}

# The API's endpoint and key come from TRUENAS_ENDPOINT and TRUENAS_API_KEY: never from a file here.
provider "truenas" {
  insecure = var.insecure
}
