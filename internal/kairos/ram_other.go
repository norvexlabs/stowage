//go:build !linux

package kairos

func refuseRAM(string) error { return nil }
