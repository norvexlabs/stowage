# Written by `<short>-stow vm create` into terraform.tfvars.json: the project's name and the disk,
# and the VM module's own variables of the same names, never the credentials.

variable "name" {
  description = "The project's name: the uploads and the image are named by it and the disk's digest."
  type        = string
}


variable "description" {
  type    = string
  default = ""
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


variable "project" {
  description = "The Google Cloud project."
  type        = string
}




variable "bucket" {
  description = "The Cloud Storage bucket the image's tar.gz is uploaded to."
  type        = string
}

variable "state_key" {
  description = "The key of this image's state, which `<short>-stow vm create` derives from the disk and the place: in every name, so two states never make the same object or image."
  type        = string
  validation {
    condition     = can(regex("^[0-9a-f]{16}$", var.state_key))
    error_message = "state_key must be 16 lowercase hex digits."
  }
}
