terraform {
  required_version = ">= 1.8"
  required_providers {
    # A community provider over TrueNAS's JSON-RPC API (iXsystems ships none), pinned exactly: an
    # upgrade is a reviewed change of `<short>-stow`, never a drift.
    truenas = {
      source  = "pjsalty/truenas"
      version = "3.1.0"
    }
  }
}

# The API's address and key come from TRUENAS_URL and TRUENAS_API_KEY: never from a file here.
provider "truenas" {
  insecure_skip_verify = var.insecure_skip_verify
}
