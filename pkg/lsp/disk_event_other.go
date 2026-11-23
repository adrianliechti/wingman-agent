//go:build !darwin

package lsp

import (
	"os"
	"time"

	"github.com/fswatcher/fswatcher"
)

func existingWrite(fswatcher.Op, os.FileInfo, time.Time) bool { return false }
