package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/adrianliechti/wingman-agent/internal/sandboxpolicy"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/google/uuid"
)

// fileChange is one entry in a best-effort atomic filesystem transaction.
// Every replacement is staged before any original is moved, and failures
// restore already-touched files in reverse order.
type fileChange struct {
	beforeRef, afterRef *tool.FileContentRef
	movedFrom           string
	target              fileTarget
	displayPath         string
	before              []byte
	beforePresent       bool
	beforeMode          os.FileMode
	after               []byte
	afterPresent        bool
	afterMode           *os.FileMode

	root       *os.Root
	path       string
	closeRoot  func()
	tempPath   string
	backupPath string
	installed  bool
}

type fileTransaction struct {
	changes    []fileChange
	prunedDirs []string
}

// Serialize agent transactions, including transactions from other sessions.
// External edits are checked again after staging and before each replacement.
var fileEditMu sync.Mutex

// commitFileTransactionLocked requires fileEditMu for preparation and commit.
func commitFileTransactionLocked(ctx context.Context, workspaceRoot *os.Root, tx *fileTransaction) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	guard := sandboxpolicy.NewCredentialGuard(workspaceRoot.Name())
	byPath := make(map[string]*fileChange)
	for i := range tx.changes {
		change := &tx.changes[i]
		if change.beforeRef != nil {
			byPath[filepath.Clean(change.displayPath)] = change
		}
		if err := sandboxpolicy.CheckWrite(workspaceRoot.Name(), change.target.AbsPath); err != nil {
			closeTransactionRoots(tx)
			return err
		}
		change.root, change.path, change.closeRoot, err = transactionLocation(workspaceRoot, change.target, !change.afterPresent || change.movedFrom != "" || change.afterRef != nil)
		if err != nil {
			closeTransactionRoots(tx)
			return fmt.Errorf("prepare %s: %w", change.displayPath, err)
		}
		if err := sandboxpolicy.CheckWrite(change.root.Name(), change.path); err != nil {
			closeTransactionRoots(tx)
			return err
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
		temp := filepath.Join(dir, ".wingman-edit-"+uuid.NewString())
		if change.afterRef != nil {
			src := byPath[filepath.Clean(change.movedFrom)]
			if src == nil || !src.beforeRef.Equal(change.afterRef) {
				return fmt.Errorf("cannot restore %s: matching move source is missing", change.displayPath)
			}
			if err := stageRename(ctx, src, change, temp, guard); err != nil {
				return fmt.Errorf("stage rename to %s: %w", change.displayPath, err)
			}
			continue
		}
		file, openErr := change.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if openErr != nil {
			return fmt.Errorf("stage %s: %w", change.displayPath, openErr)
		}
		change.tempPath = temp
		_, writeErr := file.Write(change.after)
		closeErr := file.Close()
		if writeErr != nil {
			return fmt.Errorf("stage %s: %w", change.displayPath, writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("stage %s: %w", change.displayPath, closeErr)
		}
		mode := change.beforeMode.Perm()
		if change.afterMode != nil {
			mode = change.afterMode.Perm()
		} else if mode == 0 {
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
				if err := checkTransactionContent(context.WithoutCancel(ctx), change.root, change.path, change.after, change.afterRef, guard); err != nil {
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
		if err := validateFileChange(ctx, &tx.changes[i], true, guard); err != nil {
			return err
		}
	}

	for i := range tx.changes {
		change := &tx.changes[i]
		if err := ctx.Err(); err != nil {
			return errors.Join(err, rollback())
		}
		// The backup is hashed immediately after rename. Check its metadata here
		// without streaming the same large file twice back-to-back.
		if err := validateFileChange(ctx, change, change.beforeRef == nil, guard); err != nil {
			return errors.Join(err, rollback())
		}
		if change.beforePresent {
			change.backupPath = filepath.Join(filepath.Dir(change.path), ".wingman-backup-"+uuid.NewString())
			if err := change.root.Rename(change.path, change.backupPath); err != nil {
				change.backupPath = ""
				return errors.Join(fmt.Errorf("prepare %s: %w", change.displayPath, err), rollback())
			}
			if info, statErr := change.root.Lstat(change.backupPath); statErr != nil || !info.Mode().IsRegular() || !samePermissions(info.Mode(), change.beforeMode) {
				return errors.Join(fmt.Errorf("%s changed type or permissions during edit", change.displayPath), rollback())
			}
			if err := checkTransactionContent(ctx, change.root, change.backupPath, change.before, change.beforeRef, guard); err != nil {
				return errors.Join(fmt.Errorf("%s changed on disk during edit: %w", change.displayPath, err), rollback())
			}
		}
		if change.afterPresent {
			if change.afterRef != nil {
				info, err := change.root.Lstat(change.tempPath)
				if err != nil || !info.Mode().IsRegular() || !samePermissions(info.Mode(), *change.afterMode) {
					return errors.Join(fmt.Errorf("%s changed type or permissions during move", change.displayPath), rollback())
				}
				if err := checkTransactionContent(ctx, change.root, change.tempPath, nil, change.afterRef, guard); err != nil {
					return errors.Join(err, rollback())
				}
			}
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
	for i := range tx.changes {
		change := &tx.changes[i]
		if change.beforePresent && !change.afterPresent {
			tx.prunedDirs = append(tx.prunedDirs, pruneEmptyParents(change)...)
		}
	}
	return nil
}

// Like git rm: moving a directory file by file would otherwise leave an empty husk.
func pruneEmptyParents(change *fileChange) []string {
	// An unrooted target resolves against the filesystem root, which gives no bound.
	if !change.target.InWorkspace && change.target.RootPath == "" {
		return nil
	}
	var pruned []string
	display := filepath.ToSlash(change.displayPath)
	for dir := filepath.Dir(change.path); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		// os.Root.Remove can unlink an ordinary file if another process
		// replaces the empty directory. Use a directory-only operation.
		parent, err := change.root.OpenFile(filepath.Dir(dir), readFlags, 0)
		if err != nil {
			break
		}
		err = removeEmptyDir(parent, filepath.Base(dir))
		parent.Close()
		if err != nil {
			break
		}
		display = pathpkg.Dir(display)
		pruned = append(pruned, display)
	}
	return pruned
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
func transactionLocation(workspaceRoot *os.Root, target fileTarget, keepFinalName bool) (*os.Root, string, func(), error) {
	root, path, closeRoot, err := fileTargetRoot(workspaceRoot, target)
	if err != nil {
		return nil, "", nil, err
	}
	if root == nil {
		// Only read targets are unrooted; a write must never follow one.
		return nil, "", nil, fmt.Errorf("%s is outside the workspace and writable roots", target.AbsPath)
	}
	path = filepath.Clean(filepath.FromSlash(path))

	if keepFinalName {
		// Keep the named directory entry. Resolving a final symlink here
		// would delete its referent and leave the link dangling.
		if sub, ok := resolveRootPath(root, filepath.Dir(path)); ok {
			path = filepath.Join(sub, filepath.Base(path))
		}
	} else if sub, ok := resolveRootPath(root, path); ok {
		path = sub
	}
	return root, path, closeRoot, nil
}

func validateFileChange(ctx context.Context, change *fileChange, compareContent bool, guard sandboxpolicy.CredentialGuard) error {
	info, err := change.root.Lstat(change.path)
	if !change.beforePresent && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s changed on disk; read it again: %w", change.displayPath, err)
	}
	size := int64(len(change.before))
	if change.beforeRef != nil {
		size = change.beforeRef.Size
	}
	if !change.beforePresent || !info.Mode().IsRegular() || !samePermissions(info.Mode(), change.beforeMode) || info.Size() != size {
		return fmt.Errorf("%s changed on disk; read it again", change.displayPath)
	}
	if !compareContent {
		return nil
	}
	if err := checkTransactionContent(ctx, change.root, change.path, change.before, change.beforeRef, guard); err != nil {
		return fmt.Errorf("%s changed on disk; read it again: %w", change.displayPath, err)
	}
	return nil
}

// samePermissions compares the permission bits the platform can represent.
// Windows only reports the read-only attribute (0444 or 0666), so a mode
// recorded or staged as 0644 must still match once it is read back.
func samePermissions(a, b os.FileMode) bool {
	if runtime.GOOS == "windows" {
		return a.Perm()&0o200 == b.Perm()&0o200
	}
	return a.Perm() == b.Perm()
}

// A file can grow after Stat, including during rollback. Read at most one byte
// beyond the expected contents so the comparison detects growth without an
// unbounded allocation. Even an expected empty file must have a bounded read.
func checkTransactionContent(ctx context.Context, root *os.Root, path string, data []byte, ref *tool.FileContentRef, guard sandboxpolicy.CredentialGuard) error {
	file, err := root.OpenFile(path, readFlags, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if err := guard.CheckOpened(file, info); err != nil {
		return err
	}
	if ref != nil {
		got, err := hashFileContent(ctx, file, ref.Size)
		if err != nil {
			return err
		}
		if ref.Equal(got) {
			return nil
		}
	} else {
		got, err := readContent(ctx, io.LimitReader(file, int64(len(data))+1), -1)
		if err != nil {
			return err
		}
		if bytes.Equal(data, got) {
			return nil
		}
	}
	return fmt.Errorf("contents differ")
}
