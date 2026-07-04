//go:build windows

package shell

import (
	"errors"
	"os"
	"os/exec"
)

func startTTY(cmd *exec.Cmd) (*os.File, error) {
	return nil, errors.New("tty sessions are not supported on windows")
}
