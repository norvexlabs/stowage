output "image_id" {
  description = "The image the VM module boots from."
  value       = google_compute_image.image.self_link
}
