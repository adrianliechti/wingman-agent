package pathutil

import (
	"fmt"
	"os"
)

func openedPath(f *os.File) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
}
