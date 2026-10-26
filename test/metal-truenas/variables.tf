# Written by `<short>-stow vm create --platform metal --module test/metal-truenas`, and the suite's
# --tofu-var: a bare-metal machine played by a TrueNAS VM (test/e2e-full-vm.sh).

variable "insecure_skip_verify" {
  description = "Accept the TrueNAS host's self-signed certificate (TRUENAS_INSECURE_SKIP_VERIFY)."
  type        = bool
  default     = false
}

variable "name" {
  description = "The machine's name on TrueNAS: letters and digits."
  type        = string
}

variable "description" {
  type    = string
  default = ""
}

variable "pool" {
  type = string
}

variable "parent_dataset" {
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

variable "nic_attach" {
  type = string
}

variable "image" {
  description = "The release's installer ISO, which `<short>-stow` fetched by digest: the first USB stick."
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

variable "stop_file" {
  description = "A file on this machine whose existence ends the power button's loop (the suite touches it once the machine answers)."
  type        = string
}

variable "nics" {
  description = "The NICs the release names, the primary first: each by role, its MAC and its bridge (empty: nic_attach)."
  type = list(object({
    role   = string
    mac    = string
    attach = string
  }))
  default = []
}
