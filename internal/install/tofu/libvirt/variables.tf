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

variable "cpus" {
  type = number
}

variable "memory_mib" {
  type = number
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
  description = "The NoCloud seed ISO (the cloud-config) on this machine."
  type        = string
}

variable "seed_sha256" {
  description = "The seed's SHA-256: a change attaches it again."
  type        = string
}

variable "uri" {
  description = "The libvirt connection, e.g. qemu+ssh://root@kvm.example.org/system (LIBVIRT_DEFAULT_URI is not read)."
  type        = string
  default     = "qemu:///system"
}

variable "pool" {
  description = "The storage pool the disk and the seed go in."
  type        = string
  default     = "default"
}

variable "network" {
  description = "The libvirt network the VM attaches to (its DHCP gives the address)."
  type        = string
  default     = "default"
}

variable "firmware" {
  description = "The UEFI firmware on the libvirt host."
  type        = string
  default     = "/usr/share/OVMF/OVMF_CODE.fd"
}

variable "dhcp" {
  description = "The VM takes its IPv4 address by DHCP: the apply waits for its lease, the address the module returns."
  type        = bool
  default     = true
}
