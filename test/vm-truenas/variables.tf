# Written by the VM suites (test/vm.sh, vm_root): one TrueNAS VM of a release, from `vm seed --json`.

variable "insecure" {
  description = "Accept the TrueNAS host's self-signed certificate (TRUENAS_INSECURE_SKIP_VERIFY)."
  type        = bool
  default     = false
}

variable "name" {
  description = "The VM's name on TrueNAS: letters and digits; its zvol and staging dataset are named after it."
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
  description = "The dataset under the pool the VM's zvol and staging dataset go in."
  type        = string
}

variable "nic_attach" {
  description = "The bridge a NIC that names none attaches to."
  type        = string
}

variable "cpus" {
  type = number
}

variable "memory_mib" {
  type = number
}

variable "disk_gib" {
  description = "The zvol's size: the seed's disk.<platform>.os_gib and the app's space."
  type        = number
}

variable "image" {
  description = "The release's disk on this machine (vm seed --disk-dir: disk_file.path)."
  type        = string
}

variable "image_sha256" {
  description = "Its SHA-256 (disk_file.sha256)."
  type        = string
}

variable "seed" {
  description = "The NoCloud seed ISO on this machine, of the seed's user_data and meta_data."
  type        = string
}

variable "seed_sha256" {
  type = string
}

variable "nics" {
  description = "The NICs, the primary first: each by role, its MAC (the seed's: vm seed --json's interfaces; empty for the primary of a seed of one NIC role, which gives it none, and takes a MAC derived from the name, as does the one NIC of a release that names none) and its bridge (empty: nic_attach)."
  type = list(object({
    role   = string
    mac    = string
    attach = string
  }))
  default = []
  validation {
    condition     = alltrue([for i, n in var.nics : can(regex("^[0-9a-f]{2}(:[0-9a-f]{2}){5}$", n.mac)) || (n.mac == "" && i == 0)])
    error_message = "Each NIC takes the MAC its seed printed (vm seed --json's interfaces[].mac), a NIC added later a MAC of its own; only the primary, first, may have none: a seed of one NIC role gives it none, and the VM's first boot pins its name to the MAC it has."
  }
}

variable "ctl" {
  description = "The program running Stowage's TrueNAS steps (`<short>-stow vm truenas import-image|seed|stop`): the release's installer."
  type        = string
}
