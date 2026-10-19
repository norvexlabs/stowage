output "id" {
  value = libvirt_domain.vm.id
}

output "address" {
  description = "The address the network's DHCP leased."
  value       = try(libvirt_domain.vm.network_interface[0].addresses[0], "")
}
