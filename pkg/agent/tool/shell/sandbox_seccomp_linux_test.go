//go:build linux

package shell

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSeccompBlocksHostSocketsWithEitherNetworkPolicy(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("unsupported seccomp architecture")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"isolated", "network"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(exe, linuxSandboxHelper, mode, exe, "-test.run=^TestSeccompSocketProbe$")
			cmd.Env = append(os.Environ(), "WINGMAN_SECCOMP_TEST_MODE="+mode)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("seccomp helper: %v\n%s", err, output)
			}
		})
	}
}

// Run only in the filtered child. This tests enforcement across exec, not just
// the generated BPF instructions, and never connects to a real host service.
func TestSeccompSocketProbe(t *testing.T) {
	mode := os.Getenv("WINGMAN_SECCOMP_TEST_MODE")
	if mode == "" {
		t.Skip("seccomp subprocess only")
	}
	for _, family := range []int{unix.AF_UNIX, unix.AF_VSOCK, unix.AF_INET, unix.AF_INET6} {
		fd, err := unix.Socket(family, unix.SOCK_STREAM, 0)
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		allowed := mode == "network" && (family == unix.AF_INET || family == unix.AF_INET6)
		if allowed {
			if err != nil && !errors.Is(err, unix.EAFNOSUPPORT) {
				t.Errorf("IP socket %d: %v", family, err)
			}
		} else if !errors.Is(err, unix.EPERM) {
			t.Errorf("socket %d escaped filter: %v", family, err)
		}
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("parent/child IPC blocked: %v", err)
	}
	_ = unix.Close(pair[0])
	_ = unix.Close(pair[1])
}
