package pathutil

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openedPath(f *os.File) (string, error) {
	var buf [unix.PathMax]byte
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, f.Fd(), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return "", errno
	}
	return unix.ByteSliceToString(buf[:]), nil
}
