//go:build !windows

package filelock

import (
	"os"
	"syscall"
)

// Lock locks a file for one process. Without wait, it fails while another
// holds the lock. The system releases it when the process dies.
func Lock(file *os.File, wait bool) (func(), error) {
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }, nil
}
