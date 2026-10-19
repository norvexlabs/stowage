//go:build !unix

package install

// diskOf is the space of the file system holding dir; nil when it cannot be read.
func diskOf(string) *Disk { return nil }
