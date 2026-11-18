//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/adrianliechti/wingman-agent/internal/process"
	"golang.org/x/sys/windows"
)

// lockFile locks a file for one process. Without wait, it fails while another
// holds the lock.
func lockFile(file *os.File, wait bool) (func(), error) {
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

// detach starts the host without a console and outside the window's process
// group, so that it outlives the window that started it.
func detach(cmd *exec.Cmd) {
	process.Hide(cmd)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = new(syscall.SysProcAttr)
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}
