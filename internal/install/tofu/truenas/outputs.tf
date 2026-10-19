output "vm_id" {
  value = truenas_vm.vm.id
}

output "mac" {
  description = "The NIC's MAC, which a DHCP lease is keyed on."
  value       = local.mac
}
