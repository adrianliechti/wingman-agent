//go:build windows

package filelock

import (
	"os"

	"golang.org/x/sys/windows"
)

// Lock locks a file for one process. Without wait, it fails while another
// holds the lock. The system releases it when the process dies.
func Lock(file *os.File, wait bool) (func(), error) {
	handle := windows.Handle(file.Fd())
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(handle, flags, 0, 1, 0, overlapped); err != nil {
		return nil, err
	}
	return func() { _ = windows.UnlockFileEx(handle, 0, 1, 0, overlapped) }, nil
}
