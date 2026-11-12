output "id" {
  value = aws_instance.vm.id
}

output "address" {
  description = "Its Elastic IP with eip_allocation_id, else its public address, else its private one."
  value       = var.eip_allocation_id == "" ? coalesce(aws_instance.vm.public_ip, aws_instance.vm.private_ip) : data.aws_eip.given[0].public_ip
}
