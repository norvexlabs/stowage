package kairos

import (
	"fmt"
	"syscall"
)

// refuseRAM refuses a directory on tmpfs or ramfs: what is written there is memory.
func refuseRAM(dir string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	// Type is int32 on 32-bit platforms, where RAMFS_MAGIC overflows it.
	if t := uint32(st.Type); t == 0x01021994 || t == 0x858458f6 { // TMPFS_MAGIC, RAMFS_MAGIC
		return fmt.Errorf("%s %w", dir, errRAM)
	}
	return nil
}
