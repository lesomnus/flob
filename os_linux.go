//go:build linux

package flob

import (
	"fmt"
	"os"
	"syscall"
)

// nlink returns the number of hard links to the file at the given path.
func nlink(p string) (int, error) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unexpected stat type")
	}

	return int(stat.Nlink), nil
}
