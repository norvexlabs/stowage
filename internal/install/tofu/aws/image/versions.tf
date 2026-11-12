terraform {
  required_version = ">= 1.8"
  required_providers {
    # Pinned exactly: an upgrade is a reviewed change of `<short>-stow`, never a drift.
    aws = {
      source  = "hashicorp/aws"
      version = "6.69.1"
    }
  }
}

# The account comes from the environment (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, AWS_PROFILE,
# or the instance's role): never from a file here.
provider "aws" {
  region = var.region
}
