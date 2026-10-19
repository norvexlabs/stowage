# A release's raw disk uploaded to S3, for the image module to import; destroyed once the AMI
# exists (the disk may carry the registry's pull account).

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

variable "file" {
  type = string
}

variable "sha256" {
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

resource "aws_s3_object" "image" {
  bucket      = var.bucket
  key         = "stowage/${var.name}.raw"
  source      = var.file
  source_hash = var.sha256
}

output "upload" {
  value = aws_s3_object.image.key
}
