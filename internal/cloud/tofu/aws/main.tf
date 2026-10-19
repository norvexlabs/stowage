# A release's disk registered on AWS: its upload imported as a snapshot (the VM Import service,
# through its vmimport role) and registered as a UEFI AMI. Destroying it removes the AMI and the
# snapshot.

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

variable "upload" {
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

resource "aws_ebs_snapshot_import" "image" {
  role_name   = var.vmimport_role
  description = var.description

  disk_container {
    format = "RAW"
    user_bucket {
      s3_bucket = var.bucket
      s3_key    = var.upload
    }
  }

  tags = {
    Name = var.name
  }
}

resource "aws_ami" "image" {
  name                = var.name
  description         = var.description
  virtualization_type = "hvm"
  architecture        = "x86_64"
  boot_mode           = "uefi"
  ena_support         = true
  root_device_name    = "/dev/xvda"

  ebs_block_device {
    device_name = "/dev/xvda"
    snapshot_id = aws_ebs_snapshot_import.image.id
    volume_type = "gp3"
  }
}

output "id" {
  value = aws_ami.image.id
}
