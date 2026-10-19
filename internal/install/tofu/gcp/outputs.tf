output "id" {
  value = google_compute_instance.vm.instance_id
}

output "address" {
  description = "Its external address."
  value       = google_compute_instance.vm.network_interface[0].access_config[0].nat_ip
}
