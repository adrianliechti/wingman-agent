//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// lockFile locks a file for one process. Without wait, it fails while another
// holds the lock.
func lockFile(file *os.File, wait bool) (func(), error) {
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }, nil
}

// detach starts the host in a session of its own, so that it outlives the
// window that started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
