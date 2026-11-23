package lsp

import (
	"os"
	"syscall"
	"time"

	"github.com/fswatcher/fswatcher"
)

// FSEvents can keep the create flag on later writes. The first event for a
// file that already existed when the stream started is then a modification.
func existingWrite(op fswatcher.Op, info os.FileInfo, start time.Time) bool {
	return op.Has(fswatcher.Write) && time.Unix(info.Sys().(*syscall.Stat_t).Birthtimespec.Unix()).Before(start)
}
