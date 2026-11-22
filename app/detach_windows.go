//go:build windows

package main

import (
	"os/exec"
	"syscall"

	"github.com/adrianliechti/wingman-agent/internal/process"
	"golang.org/x/sys/windows"
)

// detach starts the host without a console and outside the window's process
// group, so that it outlives the window that started it.
func detach(cmd *exec.Cmd) {
	process.Hide(cmd)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = new(syscall.SysProcAttr)
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}
