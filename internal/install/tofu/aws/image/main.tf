# The release's disk on AWS, shared by every VM `<short>-stow vm create` makes of it in this region
# and bucket, and kept by `vm destroy`: its raw image uploaded to S3 once, imported as a snapshot,
# registered as a UEFI image.

locals {
  id = substr(var.image_sha256, 0, 12)
  # The disk's digest and this state's key: what every name holds.
  tag = "${local.id}-${substr(var.state_key, 0, 8)}"
}

resource "aws_s3_object" "image" {
  bucket      = var.bucket
  key         = "stowage/${var.name}-${local.tag}.raw"
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
    Name = "${var.name}-${local.tag}"
  }
}

resource "aws_ami" "image" {
  name                = "${var.name}-${local.tag}"
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
