//go:build linux

package shell

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const linuxSandboxHelper = "__wingman-linux-sandbox-exec"

func init() {
	if len(os.Args) >= 5 && os.Args[1] == linuxSandboxHelper {
		runtime.LockOSThread()
		if err := installSandboxSeccomp(os.Args[2] == "network"); err != nil {
			fmt.Fprintln(os.Stderr, "wingman sandbox:", err)
			os.Exit(126)
		}
		if err := unix.Exec(os.Args[3], os.Args[3:], os.Environ()); err != nil {
			fmt.Fprintln(os.Stderr, "wingman sandbox exec:", err)
			os.Exit(126)
		}
	}
}

// Mounts and a network namespace do not isolate pathname Unix sockets.
// Deny creating host-service sockets even when IP networking is approved.
// socketpair remains available for local parent/child IPC.
func sandboxSeccompFilter(network bool) ([]unix.SockFilter, error) {
	var arch uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	default:
		return nil, fmt.Errorf("sandbox seccomp is unsupported on %s", runtime.GOARCH)
	}
	statement := func(code uint16, value uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: value} }
	jump := func(value uint32, yes, no uint8) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: value, Jt: yes, Jf: no}
	}
	deny := statement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM))
	filter := []unix.SockFilter{
		statement(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 4),
		jump(arch, 1, 0), statement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
		statement(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 0),
	}
	// Reject x32 syscall numbers, which share the x86-64 audit architecture.
	if runtime.GOARCH == "amd64" {
		filter = append(filter, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, K: 0x40000000, Jf: 1}, deny)
	}
	for _, nr := range []uint32{unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER} {
		filter = append(filter, jump(nr, 0, 1), deny)
	}
	filter = append(filter, jump(unix.SYS_SOCKET, 1, 0), statement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW), statement(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 16))
	if network {
		filter = append(filter, jump(unix.AF_INET, 2, 0), jump(unix.AF_INET6, 1, 0), deny, statement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
	} else {
		filter = append(filter, deny)
	}
	return filter, nil
}

func installSandboxSeccomp(network bool) error {
	filter, err := sandboxSeccompFilter(network)
	if err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	_, _, errno := unix.RawSyscall6(unix.SYS_PRCTL, unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0, 0)
	runtime.KeepAlive(filter)
	if errno != 0 {
		return errno
	}
	return nil
}
