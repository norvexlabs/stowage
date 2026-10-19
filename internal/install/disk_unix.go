//go:build unix

package install

import "syscall"

// diskOf is the space of the file system holding dir; nil when it cannot be read.
func diskOf(dir string) *Disk {
	var fs syscall.Statfs_t
	if syscall.Statfs(dir, &fs) != nil {
		return nil
	}
	bs := uint64(fs.Bsize)
	return &Disk{Path: dir, Total: uint64(fs.Blocks) * bs, Available: uint64(fs.Bavail) * bs,
		Used: (uint64(fs.Blocks) - min(uint64(fs.Bfree), uint64(fs.Blocks))) * bs}
}
