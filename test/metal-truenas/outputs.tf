output "id" {
  value = truenas_vm.vm.id
}

output "zvol" {
  value = truenas_zvol.disk.id
}

output "staging" {
  value = truenas_dataset.staging.id
}
