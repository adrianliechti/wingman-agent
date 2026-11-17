//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/adrianliechti/wingman-agent/internal/process"
	"github.com/adrianliechti/wingman-agent/pkg/layout"
	"golang.org/x/sys/windows"
)

// lockHostState takes the lock under which windows find or start the shared
// host and the host retires. Without wait, it fails while someone holds it.
// The system releases it when its process dies.
func lockHostState(wait bool) (func(), error) {
	path, err := layout.WingmanPath("app", "host.lock")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	handle := windows.Handle(file.Fd())
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(handle, flags, 0, 1, 0, overlapped); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(handle, 0, 1, 0, overlapped)
		file.Close()
	}, nil
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
