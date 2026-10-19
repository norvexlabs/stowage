output "id" {
  value = vsphere_virtual_machine.vm.id
}

output "address" {
  description = "Empty without VMware tools: the VM's DHCP lease, or --host, gives it."
  value       = vsphere_virtual_machine.vm.default_ip_address == null ? "" : vsphere_virtual_machine.vm.default_ip_address
}
