output "id" {
  value = libvirt_domain.vm.uuid
}

output "address" {
  description = "The IPv4 address the network's DHCP leased, empty without DHCP."
  value = try([
    for a in flatten(data.libvirt_domain_interface_addresses.vm[0].interfaces[*].addrs) : a.addr if a.type == "ipv4"
  ][0], "")
}
