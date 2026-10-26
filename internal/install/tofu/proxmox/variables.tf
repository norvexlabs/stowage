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

variable "image_format" {
  description = "The image's format: qcow2 or raw."
  type        = string
  validation {
    condition     = contains(["qcow2", "raw"], var.image_format)
    error_message = "image_format must be qcow2 or raw."
  }
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

variable "node" {
  description = "The Proxmox VE node the VM runs on."
  type        = string
}

variable "datastore" {
  description = "The datastore its disks go on."
  type        = string
  default     = "local-lvm"
}

variable "import_datastore" {
  description = "The datastore the image is uploaded to: a file-based one with Import content enabled (Datacenter > Storage)."
  type        = string
  default     = "local"
}

variable "iso_datastore" {
  description = "The datastore (with ISO content) the seed is uploaded to."
  type        = string
  default     = "local"
}

variable "bridge" {
  description = "The bridge its NIC attaches to."
  type        = string
  default     = "vmbr0"
}

variable "nics" {
  description = "The NICs the release names (host.interfaces), the primary first: each by role, its MAC (empty: the platform's) and the network it attaches to (empty: the module's own). None: one NIC."
  type = list(object({
    role   = string
    mac    = string
    attach = string
  }))
  default = []
}
