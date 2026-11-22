//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach starts the host in a session of its own, so that it outlives the
// window that started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
