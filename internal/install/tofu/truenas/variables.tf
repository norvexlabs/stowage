# Written by `<short>-stow vm create` into terraform.tfvars.json: the VM, never the credentials.

variable "insecure_skip_verify" {
  description = "Accept the TrueNAS host's self-signed certificate (TRUENAS_INSECURE_SKIP_VERIFY)."
  type        = bool
  default     = false
}

variable "name" {
  description = "The VM's name on TrueNAS: letters and digits (the provider refuses anything else)."
  type        = string
  validation {
    condition     = can(regex("^[A-Za-z0-9]+$", var.name))
    error_message = "name must be letters and digits."
  }
}

variable "description" {
  type    = string
  default = ""
}

variable "pool" {
  description = "The pool holding the VM's zvol and its staging dataset."
  type        = string
}

variable "parent_dataset" {
  description = "The dataset, under the pool, the zvol and the staging dataset are created in."
  type        = string
}

variable "cpus" {
  type = number
}

variable "memory_mib" {
  type = number
}

variable "disk_gib" {
  description = "The zvol's size: the OS grows its persistent partition into it at first boot."
  type        = number
}

variable "nic_attach" {
  description = "The host interface or bridge the VM's NIC attaches to (e.g. br0)."
  type        = string
}

variable "image" {
  description = "The release's disk image on this machine, which `<short>-stow` fetched by digest."
  type        = string
}

variable "image_sha256" {
  description = "The image's SHA-256: checked before it is uploaded; a change rebuilds the disk."
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
  description = "The seed's SHA-256: a change uploads it again."
  type        = string
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
