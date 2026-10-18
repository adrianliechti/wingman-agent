//go:build windows

package sandboxpolicy

import (
	"path/filepath"
	"testing"
)

func TestCheckWriteAcrossVolumes(t *testing.T) {
	root := `F:\workspace`
	if err := CheckWrite(root, `C:\Users\dev\notes.txt`); err != nil {
		t.Fatalf("write on another drive: %v", err)
	}
	if err := CheckWrite(root, `\\server\share\notes.txt`); err != nil {
		t.Fatalf("write on a share: %v", err)
	}
	if err := CheckWrite(root, filepath.Join(root, ".git", "config")); err == nil {
		t.Fatal("protected path on the workspace drive was writable")
	}
}
