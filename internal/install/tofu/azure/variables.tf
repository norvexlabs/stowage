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

variable "resource_group" {
  description = "The resource group everything goes in."
  type        = string
}

variable "location" {
  description = "The Azure region, e.g. westeurope."
  type        = string
}

variable "storage_account" {
  description = "The storage account, in the resource group, the VHD is uploaded to as a page blob."
  type        = string
}

variable "container" {
  description = "The storage container the VHD goes in."
  type        = string
  default     = "vhds"
}

variable "subnet_id" {
  description = "The subnet the VM's NIC attaches to."
  type        = string
}

variable "vm_size" {
  description = "The VM size."
  type        = string
  default     = "Standard_B2s"
}

variable "admin_user" {
  description = "The administrator the platform requires: the same as vm create's --user."
  type        = string
}

variable "ssh_public_key" {
  description = "The administrator's SSH public key: the same as vm create's --ssh-key."
  type        = string
}
