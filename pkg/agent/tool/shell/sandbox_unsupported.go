//go:build !linux && !darwin && !windows

package shell

import (
	"fmt"
	"runtime"
)

func platformSandboxCommand(_, _, _ string, _ sandboxPolicy) (string, []string, error) {
	return "", nil, fmt.Errorf("workspace shell sandbox is not supported on %s", runtime.GOOS)
}
