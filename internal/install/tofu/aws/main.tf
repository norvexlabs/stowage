# One EC2 instance of the release: its raw image uploaded to S3, imported as a snapshot, registered
# as a UEFI image, and launched with the cloud-config as its user data. A preview: not run on AWS
# yet.

locals {
  id  = substr(var.image_sha256, 0, 12)
  sgs = compact(split(",", var.security_group_ids))
}

resource "aws_s3_object" "image" {
  bucket      = var.bucket
  key         = "stowage/${var.name}-${local.id}.raw"
  source      = var.image
  source_hash = var.image_sha256
}

resource "aws_ebs_snapshot_import" "image" {
  role_name   = var.vmimport_role
  description = var.description

  disk_container {
    format = "RAW"
    user_bucket {
      s3_bucket = aws_s3_object.image.bucket
      s3_key    = aws_s3_object.image.key
    }
  }

  tags = {
    Name = "${var.name}-${local.id}"
  }
}

resource "aws_ami" "image" {
  name                = "${var.name}-${local.id}"
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

resource "aws_instance" "vm" {
  ami                    = aws_ami.image.id
  instance_type          = var.instance_type
  subnet_id              = var.subnet_id
  vpc_security_group_ids = length(local.sgs) > 0 ? local.sgs : null
  user_data_base64       = filebase64(var.seed)

  root_block_device {
    volume_size = var.disk_gib
    volume_type = "gp3"
  }

  metadata_options {
    http_tokens = "required"
  }

  tags = {
    Name = var.name
  }

  lifecycle {
    # A later release reaches the instance as an upgrade, never as a replacement.
    ignore_changes = [ami, user_data_base64]
  }
}
