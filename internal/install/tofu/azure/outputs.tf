output "id" {
  value = azurerm_linux_virtual_machine.vm.id
}

output "address" {
  description = "Its public address."
  value       = local.address
}
