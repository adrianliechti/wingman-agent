package fs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/adrianliechti/wingman-agent/internal/sandboxpolicy"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

// Limit the read to the observed length and check EOF separately. A growing
// file cannot keep validation running or overflow the byte limit.
func hashFileContent(ctx context.Context, r io.Reader, size int64) (*tool.FileContentRef, error) {
	hash := sha256.New()
	reader := contextReader{ctx: ctx, source: r}
	if _, err := io.CopyN(hash, reader, size); err != nil {
		return nil, err
	}
	var extra [1]byte
	if n, err := reader.Read(extra[:]); n != 0 || err != io.EOF {
		if err != nil && err != io.EOF {
			return nil, err
		}
		return nil, fmt.Errorf("file grew while reading")
	}
	return &tool.FileContentRef{Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

// A same-root move stages a hard link to the original inode. The transaction
// installs it without clobbering the destination and only then discards its
// backups. Across roots or filesystems, stream to a destination-local file.
func stageRename(ctx context.Context, src, dst *fileChange, temp string, guard sandboxpolicy.CredentialGuard) error {
	if src.root.Name() == dst.root.Name() && samePermissions(src.beforeMode, *dst.afterMode) {
		a, err := src.root.Stat(".")
		if err != nil {
			return err
		}
		b, err := dst.root.Stat(".")
		if err != nil {
			return err
		}
		if os.SameFile(a, b) {
			if err := dst.root.Link(src.path, temp); err == nil {
				dst.tempPath = temp
				return nil
			}
		}
	}
	source, err := src.root.OpenFile(src.path, readFlags, 0)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != dst.afterRef.Size {
		return fmt.Errorf("%s changed before move", src.displayPath)
	}
	if err := guard.CheckOpened(source, info); err != nil {
		return err
	}
	out, err := dst.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	dst.tempPath = temp
	got, err := hashFileContent(ctx, io.TeeReader(source, out), info.Size())
	if err == nil && !dst.afterRef.Equal(got) {
		err = fmt.Errorf("%s changed while copying", src.displayPath)
	}
	if err == nil {
		err = out.Chmod(dst.afterMode.Perm())
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
