//go:build !linux

package host

import (
	"syscall"
)

func platformStatfs(path string) (StatfsData, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return StatfsData{}, err
	}
	return StatfsData{
		Blocks: uint64(s.Blocks),
		Bfree:  uint64(s.Bfree),
		Bavail: uint64(s.Bavail),
		Bsize:  uint64(s.Bsize),
	}, nil
}
