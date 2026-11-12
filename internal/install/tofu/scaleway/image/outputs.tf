output "image_id" {
  description = "The image the VM module boots from."
  value       = scaleway_instance_image.image.id
}
