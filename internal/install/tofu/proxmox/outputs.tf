output "id" {
  value = proxmox_virtual_environment_vm.vm.vm_id
}

output "address" {
  description = "Unknown without a guest agent: the VM's DHCP lease, or --host, gives it."
  value       = ""
}
