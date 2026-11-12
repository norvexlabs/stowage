# One EC2 instance of the release, launched from the image module's image (image/) with the
# cloud-config as its user data. A preview: not run on AWS yet.

locals {
  sgs = compact(split(",", var.security_group_ids))
}

resource "aws_instance" "vm" {
  ami                    = var.image_id
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

# The Elastic IP eip_allocation_id names, reserved beforehand: the instance's public address,
# released from it (never deleted) by `vm destroy`.
resource "aws_eip_association" "vm" {
  count         = var.eip_allocation_id == "" ? 0 : 1
  instance_id   = aws_instance.vm.id
  allocation_id = var.eip_allocation_id
}

data "aws_eip" "given" {
  count = var.eip_allocation_id == "" ? 0 : 1
  id    = var.eip_allocation_id
}
