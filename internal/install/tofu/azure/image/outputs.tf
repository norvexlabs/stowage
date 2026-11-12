output "image_id" {
  description = "The image the VM module boots from."
  value       = azurerm_image.image.id
}
