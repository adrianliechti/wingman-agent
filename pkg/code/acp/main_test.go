package acp

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

const warmUpEnv = "WINGMAN_ACP_CLIENT_WARM_UP"

func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" && os.Getenv(warmUpEnv) == "" {
		// The first child launch of a fresh test binary can stall for many
		// seconds while it is scanned; absorb that before tests with startup
		// deadlines re-execute it as an ACP agent.
		if exe, err := os.Executable(); err == nil {
			warm := exec.Command(exe, "-test.run=^$")
			warm.Env = append(os.Environ(), warmUpEnv+"=1")
			_ = warm.Run()
		}
	}
	os.Exit(m.Run())
}
