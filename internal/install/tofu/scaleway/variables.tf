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
  description = "The cloud-config on this machine, given to the instance as its cloud-init user data."
  type        = string
}

variable "seed_sha256" {
  description = "The seed's SHA-256 (the instance takes its user data once, at creation)."
  type        = string
}

variable "zone" {
  description = "The zone the instance runs in, e.g. fr-par-1."
  type        = string
  validation {
    condition     = can(regex("^[a-z]+-[a-z]+-[0-9]+$", var.zone))
    error_message = "zone is a Scaleway zone, e.g. fr-par-1."
  }
}

variable "bucket" {
  description = "The Object Storage bucket, in the zone's region, the qcow2 image is uploaded to for the snapshot's import."
  type        = string
}

variable "project" {
  description = "The Scaleway project ID, empty for the credentials' default project."
  type        = string
  default     = ""
}

variable "instance_type" {
  description = "The instance type (its commercial type)."
  type        = string
  default     = "PLAY2-NANO"
}

variable "security_group_id" {
  description = "The instance's security group (zoned ID), empty for one made from ssh_sources and open_ports."
  type        = string
  default     = ""
}

variable "ssh_sources" {
  description = "The sources SSH accepts, as CIDR: --ssh-allow's, or this machine's public address."
  type        = list(string)
  default     = []
}

variable "open_ports" {
  description = "The ports the VM's firewall opens besides SSH: the front's and the L4 routes'."
  type = list(object({
    protocol = string
    port     = number
    # The sources the VM's own firewall lets reach it, as `vm create` knows them; every one by default.
    sources = optional(list(string), ["0.0.0.0/0", "::/0"])
  }))
  default = []
}

variable "ip_id" {
  description = "A flexible IP of the zone (routed IPv4, its ID) the instance takes, reserved beforehand and kept by `vm destroy`; empty for one made with the VM."
  type        = string
  default     = ""
}

variable "image_id" {
  description = "The image the VM boots from: the image module's, which `<short>-stow vm create` applies first."
  type        = string
}
