# Written by test/e2e-full-vm.sh (test/vm.sh, vm_root): a bare-metal machine played by a TrueNAS VM,
# from `vm seed --json --platform metal`.

variable "insecure" {
  description = "Accept the TrueNAS host's self-signed certificate (TRUENAS_INSECURE_SKIP_VERIFY)."
  type        = bool
  default     = false
}

variable "name" {
  description = "The machine's name on TrueNAS: letters and digits; its zvol and staging dataset are named after it."
  type        = string
  validation {
    condition     = can(regex("^[A-Za-z0-9]{1,50}$", var.name))
    error_message = "name: letters and digits, at most 50."
  }
}

variable "pool" {
  type = string
}

variable "parent_dataset" {
  type = string
}

variable "nic_attach" {
  type = string
}

variable "cpus" {
  type = number
}

variable "memory_mib" {
  type = number
}

variable "disk_gib" {
  description = "The machine's empty disk, which the installer wipes and installs the OS on."
  type        = number
}

variable "image" {
  description = "The release's installer ISO on this machine (vm seed --disk-dir: disk_file.path): the first USB stick."
  type        = string
}

variable "image_sha256" {
  type = string
}

variable "seed" {
  description = "The NoCloud seed ISO: the second USB stick."
  type        = string
}

variable "seed_sha256" {
  type = string
}

variable "nics" {
  description = "The NICs the release names, the primary first: each by role, its MAC (the seed's: vm seed --json's interfaces; none named, one NIC of a MAC derived from the name) and its bridge (empty: nic_attach)."
  type = list(object({
    role   = string
    mac    = string
    attach = string
  }))
  default = []
  validation {
    condition     = alltrue([for n in var.nics : can(regex("^[0-9a-f]{2}(:[0-9a-f]{2}){5}$", n.mac))])
    error_message = "Each NIC takes the MAC its seed printed (vm seed --json's interfaces[].mac): a NIC without one is named by no MAC, and a NIC added later could take its name."
  }
}

variable "ctl" {
  description = "The program running Stowage's TrueNAS steps (`<short>-stow vm truenas seed|stop`): the release's installer."
  type        = string
}
