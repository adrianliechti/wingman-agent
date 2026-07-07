//go:build !windows

package shell

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

func startTTY(cmd *exec.Cmd) (*os.File, error) {
	// pty needs Setsid/Setctty instead of Setpgid; the session leader's pgid
	// equals its pid, so killProcessGroup keeps working.
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	cmd.Env = append(cmd.Env, "TERM=dumb")
	return pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 120})
}
