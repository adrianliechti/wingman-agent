package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
)

// fileChange is one entry in a best-effort atomic filesystem transaction.
// Every replacement is staged before any original is moved, and failures
// restore already-touched files in reverse order.
type fileChange struct {
	target        fileTarget
	displayPath   string
	before        []byte
	beforePresent bool
	beforeMode    os.FileMode
	after         []byte
	afterPresent  bool

	root       *os.Root
	path       string
	closeRoot  func()
	tempPath   string
	backupPath string
	installed  bool
}

type fileTransaction struct {
	changes []fileChange
}

// Serialize agent transactions, including transactions from other sessions.
// External edits are checked again after staging and before each replacement.
var fileTransactionMu sync.Mutex
var fileEditMu sync.Mutex

func commitFileTransaction(ctx context.Context, workspaceRoot *os.Root, tx *fileTransaction) (err error) {
	fileTransactionMu.Lock()
	defer fileTransactionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for i := range tx.changes {
		change := &tx.changes[i]
		change.root, change.path, change.closeRoot, err = transactionLocation(workspaceRoot, change.target)
		if err != nil {
			closeTransactionRoots(tx)
			return fmt.Errorf("prepare %s: %w", change.displayPath, err)
		}
	}
	defer closeTransactionRoots(tx)

	cleanup := func() {
		for i := range tx.changes {
			change := &tx.changes[i]
			if change.tempPath != "" {
				_ = change.root.Remove(change.tempPath)
			}
		}
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	// Stage every new version before moving any original out of place.
	for i := range tx.changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		change := &tx.changes[i]
		if !change.afterPresent {
			continue
		}
		dir := filepath.Dir(change.path)
		if dir != "." {
			if err := change.root.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("prepare directory for %s: %w", change.displayPath, err)
			}
		}
		change.tempPath = filepath.Join(dir, ".wingman-edit-"+uuid.NewString())
		file, openErr := change.root.OpenFile(change.tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if openErr != nil {
			return fmt.Errorf("stage %s: %w", change.displayPath, openErr)
		}
		_, writeErr := file.Write(change.after)
		closeErr := file.Close()
		if writeErr != nil {
			return fmt.Errorf("stage %s: %w", change.displayPath, writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("stage %s: %w", change.displayPath, closeErr)
		}
		mode := change.beforeMode.Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := change.root.Chmod(change.tempPath, mode); err != nil {
			return fmt.Errorf("stage permissions for %s: %w", change.displayPath, err)
		}
	}

	rollback := func() error {
		var errs []error
		for i := len(tx.changes) - 1; i >= 0; i-- {
			change := &tx.changes[i]
			if change.installed {
				current, readErr := change.root.ReadFile(change.path)
				if readErr != nil || !bytes.Equal(current, change.after) {
					errs = append(errs, fmt.Errorf("cannot roll back %s: changed externally; original retained at %s", change.displayPath, change.backupPath))
					continue
				}
				if removeErr := change.root.Remove(change.path); removeErr != nil {
					errs = append(errs, removeErr)
					continue
				}
			}
			if change.backupPath != "" {
				// Link refuses to overwrite a file created by someone else after staging.
				if restoreErr := change.root.Link(change.backupPath, change.path); restoreErr != nil {
					errs = append(errs, fmt.Errorf("restore %s from %s: %w", change.displayPath, change.backupPath, restoreErr))
					continue
				}
				_ = change.root.Remove(change.backupPath)
				change.backupPath = ""
			}
		}
		return errors.Join(errs...)
	}

	// Validate the whole batch before moving even its first file.
	for i := range tx.changes {
		if err := validateFileChange(&tx.changes[i]); err != nil {
			return err
		}
	}

	for i := range tx.changes {
		change := &tx.changes[i]
		if err := ctx.Err(); err != nil {
			return errors.Join(err, rollback())
		}
		if err := validateFileChange(change); err != nil {
			return errors.Join(err, rollback())
		}
		if change.beforePresent {
			change.backupPath = filepath.Join(filepath.Dir(change.path), ".wingman-backup-"+uuid.NewString())
			if err := change.root.Rename(change.path, change.backupPath); err != nil {
				change.backupPath = ""
				return errors.Join(fmt.Errorf("prepare %s: %w", change.displayPath, err), rollback())
			}
			moved, readErr := change.root.ReadFile(change.backupPath)
			if readErr != nil || !bytes.Equal(moved, change.before) {
				return errors.Join(fmt.Errorf("%s changed on disk during edit; read it again", change.displayPath), rollback())
			}
		}
		if change.afterPresent {
			if err := change.root.Link(change.tempPath, change.path); err != nil {
				return errors.Join(fmt.Errorf("install %s: %w", change.displayPath, err), rollback())
			}
			_ = change.root.Remove(change.tempPath)
			change.tempPath = ""
			change.installed = true
		}
	}

	for i := range tx.changes {
		change := &tx.changes[i]
		if change.backupPath != "" {
			_ = change.root.Remove(change.backupPath)
			change.backupPath = ""
		}
	}
	cleanup()
	return nil
}

func closeTransactionRoots(tx *fileTransaction) {
	for i := range tx.changes {
		if tx.changes[i].closeRoot != nil {
			tx.changes[i].closeRoot()
			tx.changes[i].closeRoot = nil
		}
	}
}

// transactionLocation turns all supported target forms into an os.Root plus
// a relative path. The symlink fallback preserves the existing file-tool
// behavior for absolute in-root links while keeping containment enforced by
// os.Root during staging and renames.
func transactionLocation(workspaceRoot *os.Root, target fileTarget) (*os.Root, string, func(), error) {
	root, path, closeRoot, err := fileTargetRoot(workspaceRoot, target)
	if err != nil {
		return nil, "", nil, err
	}
	if root == nil {
		root, path, closeRoot, err = absoluteTransactionLocation(path)
		if err != nil {
			return nil, "", nil, err
		}
	}
	path = filepath.Clean(filepath.FromSlash(path))

	if sub, ok := resolveRootPath(root, path); ok {
		path = filepath.Clean(sub)
	}
	return root, path, closeRoot, nil
}

func absoluteTransactionLocation(path string) (*os.Root, string, func(), error) {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	base := string(filepath.Separator)
	if volume != "" {
		base = volume + string(filepath.Separator)
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return nil, "", nil, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, "", nil, err
	}
	return root, rel, func() { _ = root.Close() }, nil
}

func validateFileChange(change *fileChange) error {
	info, err := change.root.Stat(change.path)
	if !change.beforePresent && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s changed on disk; read it again: %w", change.displayPath, err)
	}
	if !change.beforePresent || !info.Mode().IsRegular() || info.Mode().Perm() != change.beforeMode.Perm() {
		return fmt.Errorf("%s changed on disk; read it again", change.displayPath)
	}
	current, err := change.root.ReadFile(change.path)
	if err != nil {
		return fmt.Errorf("check %s: %w", change.displayPath, err)
	}
	if !bytes.Equal(current, change.before) {
		return fmt.Errorf("%s changed on disk; read it again", change.displayPath)
	}
	return nil
}
