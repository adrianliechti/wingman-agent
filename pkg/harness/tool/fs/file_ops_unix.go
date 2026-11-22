//go:build unix

package fs

import (
	"os"

	"golang.org/x/sys/unix"
)

// Avoid blocking on a special file substituted for a regular file.
const readFlags = unix.O_RDONLY | unix.O_NONBLOCK

func removeEmptyDir(parent *os.File, name string) error {
	return unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}
