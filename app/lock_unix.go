//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/adrianliechti/wingman-agent/pkg/layout"
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
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, nil
}

// detach starts the host in a session of its own, so that it outlives the
// window that started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
