# Looks for the available AMI of this name the publisher's AWS account owns in the region: its id, or "" when none.

terraform {
  required_version = ">= 1.8"
  required_providers {
    # Pinned exactly: an upgrade is a reviewed change, never a drift.
    aws = {
      source  = "hashicorp/aws"
      version = "6.69.1"
    }
  }
}

# The account comes from the environment (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, AWS_PROFILE,
# or the machine's role): never from a file here.
provider "aws" {
  region = var.region
}

variable "name" {
  type = string
}

variable "description" {
  type = string
}

variable "region" {
  type = string
}

variable "bucket" {
  type = string
}

variable "vmimport_role" {
  type = string
}

# An available AMI only (one whose registration failed is not registered), deprecated ones
# included: the name is taken all the same.
data "aws_ami_ids" "existing" {
  owners             = ["self"]
  include_deprecated = true
  filter {
    name   = "name"
    values = [var.name]
  }
  filter {
    name   = "state"
    values = ["available"]
  }
  # This disk's (its description names the disk's digest).
  filter {
    name   = "description"
    values = [var.description]
  }
}

data "aws_ami_ids" "named" {
  owners             = ["self"]
  include_deprecated = true
  filter {
    name   = "name"
    values = [var.name]
  }
}

# "not available, or another disk's" when an AMI of this name the account owns is not this disk's
# available one: "" when none.
output "unready" {
  value = length(data.aws_ami_ids.named.ids) > length(data.aws_ami_ids.existing.ids) ? "not available, or another disk's" : ""
}

output "id" {
  value = length(data.aws_ami_ids.existing.ids) > 0 ? data.aws_ami_ids.existing.ids[0] : ""
}
