//go:build !unix && !windows

package fs

import (
	"errors"
	"os"
)

const readFlags = os.O_RDONLY

// Directory pruning is optional on hosts without a rooted directory-only removal.
func removeEmptyDir(_ *os.File, _ string) error { return errors.ErrUnsupported }
