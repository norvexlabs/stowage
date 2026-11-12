output "id" {
  value = scaleway_instance_server.vm.id
}

output "address" {
  description = "Its public IPv4 address."
  value       = local.address
}
