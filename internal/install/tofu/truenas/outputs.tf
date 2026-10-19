output "id" {
  value = truenas_vm.vm.id
}

output "address" {
  description = "Unknown to TrueNAS: the VM's DHCP lease (keyed on its MAC), its static address, or --host gives it."
  value       = ""
}

output "mac" {
  description = "The NIC's MAC, which a DHCP lease is keyed on."
  value       = local.mac
}
