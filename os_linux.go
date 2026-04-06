//go:build linux

package flat

import (
	"fmt"
	"syscall"
)

func nlink(sys any) (int, error) {
	stat, ok := sys.(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("invalid type")
	}
	return int(stat.Nlink), nil
}
