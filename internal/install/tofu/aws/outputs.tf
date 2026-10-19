output "id" {
  value = aws_instance.vm.id
}

output "address" {
  description = "Its public address, else its private one."
  value       = coalesce(aws_instance.vm.public_ip, aws_instance.vm.private_ip)
}
