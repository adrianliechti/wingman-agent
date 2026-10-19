// Package statefile writes private application state using temporary files.
package statefile

import (
	"errors"
	"os"
	"path/filepath"
)

// Write replaces path with data using a private (0600) temporary file in the same
// directory. It syncs and closes the file before renaming it. The parent directory
// must exist; replacement has the same atomicity guarantees as os.Rename.
func Write(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())

	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
