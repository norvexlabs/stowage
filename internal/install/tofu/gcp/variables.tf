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

variable "project" {
  description = "The Google Cloud project."
  type        = string
}

variable "zone" {
  description = "The zone the instance runs in, e.g. europe-west1-b."
  type        = string
}

variable "bucket" {
  description = "The Cloud Storage bucket the image's tar.gz is uploaded to."
  type        = string
}

variable "network" {
  description = "The VPC network."
  type        = string
  default     = "default"
}

variable "subnetwork" {
  description = "The subnetwork, empty for the network's own in the zone's region."
  type        = string
  default     = ""
}

variable "machine_type" {
  description = "The machine type."
  type        = string
  default     = "e2-medium"
}

variable "nic_type" {
  description = "The instance's NIC: GVNIC (Compute Engine's faster one) or VIRTIO_NET."
  type        = string
  default     = "GVNIC"
  validation {
    condition     = contains(["GVNIC", "VIRTIO_NET"], var.nic_type)
    error_message = "nic_type is GVNIC or VIRTIO_NET."
  }
}

variable "firewall" {
  description = "Make the firewall rules reaching the instance (SSH from ssh_sources, open_ports from every source): the account must be allowed to create firewall rules."
  type        = bool
  default     = false
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
  }))
  default = []
}
