output "image_id" {
  description = "The image the VM module boots from."
  value       = aws_ami.image.id
}
