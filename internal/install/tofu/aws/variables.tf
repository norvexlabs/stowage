# Written by `<short>-stow vm create` into terraform.tfvars.json: the VM, never the credentials,
# which the provider reads from the environment.

variable "name" {
  description = "The VM's name on the platform."
  type        = string
}

variable "description" {
  type    = string
  default = ""
}

variable "disk_gib" {
  description = "The disk's size: the OS grows its persistent partition into it at first boot."
  type        = number
}

variable "image" {
  description = "The release's disk image on this machine, which `<short>-stow` fetched by digest."
  type        = string
}

variable "image_sha256" {
  description = "The image's SHA-256: a change makes the disk again."
  type        = string
  validation {
    condition     = can(regex("^[0-9a-f]{64}$", var.image_sha256))
    error_message = "image_sha256 must be 64 lowercase hex digits."
  }
}

variable "seed" {
  description = "The cloud-config on this machine, given to the instance as its user data."
  type        = string
}

variable "seed_sha256" {
  description = "The seed's SHA-256: a change attaches it again."
  type        = string
}

variable "region" {
  description = "The AWS region."
  type        = string
}

variable "bucket" {
  description = "The S3 bucket the raw image is uploaded to, for the snapshot's import."
  type        = string
}

variable "subnet_id" {
  description = "The subnet the instance runs in."
  type        = string
}

variable "security_group_ids" {
  description = "The instance's security groups, comma-separated (empty: the VPC's default)."
  type        = string
  default     = ""
}

variable "instance_type" {
  description = "The instance type."
  type        = string
  default     = "t3.medium"
}

variable "vmimport_role" {
  description = "The service role EC2's import reads the bucket with."
  type        = string
  default     = "vmimport"
}
