#!/bin/sh
# The default pool and network, as a stock host has them, then libvirtd in the foreground.
set -e
mkdir -p /var/lib/libvirt/images
/usr/sbin/virtlogd -d
/usr/sbin/libvirtd -d
for _ in $(seq 30); do virsh -c qemu:///system version >/dev/null 2>&1 && break; sleep 1; done
virsh -c qemu:///system pool-info default >/dev/null 2>&1 || {
	virsh -c qemu:///system pool-define-as default dir --target /var/lib/libvirt/images
	virsh -c qemu:///system pool-start default
}
virsh -c qemu:///system net-start default 2>/dev/null || true
touch /run/libvirt/e2e-ready
exec sleep infinity
