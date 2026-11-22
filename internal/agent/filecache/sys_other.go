//go:build !linux

package filecache

// defaultSys: no page cache calls outside Linux.
func defaultSys() Sys { return nil }
