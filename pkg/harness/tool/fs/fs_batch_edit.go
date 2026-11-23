package fs

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/internal/sandboxpolicy"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

// batchEditTool exposes one provider-neutral mutation tool. Its JSON schema is
// intentionally small: exact replacements and file creation are enough for
// normal coding flows, while an array gives structured tool calling the same
// multi-file transaction boundary as a proprietary patch dialect.
func batchEditTool(root *os.Root, freshness *Freshness, allowedWriteRoots ...string) tool.Tool {
	return tool.Tool{
		Name:   "edit",
		Effect: tool.StaticEffect(tool.EffectMutates),
		Description: strings.Join([]string{
			"Create or edit UTF-8 text up to 10 MiB per file. A pure move accepts a regular file of any size, including binary files. Put every independent replacement, creation, and move you know about into one `edits` array. Entries for the same file run in order.",
			"Read existing text before replacing it. Copy text without line numbers. Use a unique old_string or set replace_all. Matching tolerates minor whitespace and typography differences.",
			"To create a file or fill an empty file, use an empty old_string. To rename, pass move_to and two empty strings. The tool checks both paths, creates destination parents, and refuses occupied destinations and file symlinks. Use the delete tool to remove files.",
			"Validation finishes before any file changes. Commit failures attempt rollback and report restoration errors. A successful result lists the changes; use it without re-reading the files.",
		}, "\n"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"edits": map[string]any{
					"type":        "array",
					"minItems":    1,
					"description": "Ordered changes to apply together.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"file_path":   map[string]any{"type": "string", "description": writePathScope},
							"old_string":  map[string]any{"type": "string", "description": "Text to match. Use an empty string for creation or a pure move."},
							"new_string":  map[string]any{"type": "string", "description": "Replacement text or new file contents. Use an empty string for a pure move."},
							"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence instead of requiring a unique match.", "default": false},
							"move_to":     map[string]any{"type": "string", "description": "Destination for a move. It must be vacant at this point in the batch."},
						},
						// Some providers write arguments in schema order and drop an optional one they have passed; required ones survive.
						"required":             []string{"file_path", "old_string", "new_string"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"edits"},
			"additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			guard := sandboxpolicy.NewCredentialGuard(root.Name())
			entries, err := parseBatchEdits(ctx, root, args, allowedWriteRoots, guard)
			if err != nil {
				return tool.Result{}, fmt.Errorf("%w (no files changed)", err)
			}
			return applyFileEdits(ctx, root, freshness, guard, entries, false)
		},
	}
}

// Both mutation tools use the same staging, rollback, freshness, and review metadata.
func applyFileEdits(ctx context.Context, root *os.Root, freshness *Freshness, guard sandboxpolicy.CredentialGuard, entries []parsedBatchEntry, deleting bool) (tool.Result, error) {
	fileEditMu.Lock()
	defer fileEditMu.Unlock()
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	states := make(map[string]*stagedFileState)
	var stateOrder []string
	var summaries []string
	var batchErrs []string

	loadState := func(target fileTarget, displayPath string, pureMove, requireVacant bool) (*stagedFileState, string) {
		key := normalizePathForComparison(resolveForCompare(target.AbsPath))
		if st, ok := states[key]; ok {
			return st, ""
		}
		info, statErr := statFileTarget(root, target)
		exists := statErr == nil
		switch {
		case exists && requireVacant:
			return nil, fmt.Sprintf("cannot move to %s because destination already exists", displayPath)
		case exists && info.IsDir():
			return nil, fmt.Sprintf("%s is a directory", displayPath)
		case exists && !info.Mode().IsRegular():
			return nil, fmt.Sprintf("%s is not a regular file", displayPath)
		case statErr != nil && !os.IsNotExist(statErr):
			return nil, fmt.Sprintf("stat %s: %v", displayPath, statErr)
		}
		if exists && freshness.stale(ctx, target, info) {
			return nil, fmt.Sprintf("%s: the file changed on disk after you last read it; read it again first", displayPath)
		}
		var before []byte
		var ref *tool.FileContentRef
		if exists {
			var err error
			before, ref, err = readEditContent(ctx, root, target, info, guard, pureMove)
			if err != nil {
				return nil, fmt.Sprintf("read %s: %v", displayPath, err)
			}
		}
		mode := os.FileMode(0o644)
		if exists {
			mode = info.Mode().Perm()
		}
		bom, content := stripBom(string(before))
		ending := detectLineEnding(content)
		st := &stagedFileState{
			target:        target,
			displayPath:   displayPath,
			before:        before,
			raw:           before,
			beforeRef:     ref,
			ref:           ref,
			beforePresent: exists,
			beforeMode:    mode,
			present:       exists,
			mode:          mode,
			bom:           bom,
			ending:        ending,
			content:       normalizeToLF(content),
		}
		states[key] = st
		stateOrder = append(stateOrder, key)
		return st, ""
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return tool.Result{}, err
		}
		pureMove := entry.op.moveTo != "" && !entry.op.hasEdit
		src, loadErr := loadState(entry.target, entry.displayPath, pureMove, false)
		if loadErr != "" {
			batchErrs = append(batchErrs, entry.argument+": "+loadErr)
			continue
		}
		if src.failed {
			continue
		}

		if src.ref != nil && !pureMove {
			batchErrs = append(batchErrs, fmt.Sprintf("%s: %s supports only a pure move", entry.argument, entry.displayPath))
			src.failed = true
			continue
		}
		if entry.op.deleteFile {
			if !src.present {
				batchErrs = append(batchErrs, fmt.Sprintf("%s for %s: cannot delete file that does not exist", entry.argument, entry.displayPath))
				src.failed = true
				continue
			}
			src.present = false
			src.bom = ""
			src.ending = "\n"
			src.content = ""
			src.touched = true
			continue
		}

		if entry.op.moveTo != "" {
			if !src.present {
				batchErrs = append(batchErrs, fmt.Sprintf("%s for %s: cannot move file that does not exist", entry.argument, entry.displayPath))
				src.failed = true
				continue
			}
			dst, dstErr := loadState(*entry.moveTarget, entry.moveDisplayPath, false, true)
			if dstErr != "" {
				batchErrs = append(batchErrs, entry.argument+".move_to: "+dstErr)
				src.failed = true
				continue
			}
			if dst == src {
				batchErrs = append(batchErrs, fmt.Sprintf("%s for %s: move_to destination is the same file", entry.argument, entry.displayPath))
				src.failed = true
				continue
			}
			if dst.failed {
				src.failed = true
				continue
			}
			if dst.present {
				batchErrs = append(batchErrs, fmt.Sprintf("%s for %s: cannot move to %s because destination already exists", entry.argument, entry.displayPath, entry.moveDisplayPath))
				src.failed = true
				dst.failed = true
				continue
			}
			movedContent := src.content
			if entry.op.hasEdit {
				next, entryErr := applyEditOp(movedContent, entry.op, entry.displayPath)
				if entryErr != nil {
					batchErrs = append(batchErrs, fmt.Sprintf("%s for %s: %v", entry.argument, entry.displayPath, entryErr))
					src.failed = true
					continue
				}
				movedContent = next
			}
			dst.present = true
			dst.mode = src.mode
			dst.bom = src.bom
			dst.ending = src.ending
			dst.content = movedContent
			dst.raw, src.raw = src.raw, nil
			if entry.op.hasEdit {
				dst.raw = nil
			}
			dst.ref, src.ref = src.ref, nil
			dst.touched = true
			dst.movedFrom = src.movedFrom
			if dst.movedFrom == "" {
				dst.movedFrom = src.displayPath
			}
			src.movedFrom = ""

			src.present = false
			src.bom = ""
			src.ending = "\n"
			src.content = ""
			src.touched = true
			src.movedAway = true
			continue
		}

		if !src.present && entry.op.oldText != "" {
			batchErrs = append(batchErrs, fmt.Sprintf("cannot edit %s: file does not exist; use an empty old_string to create it", entry.displayPath))
			src.failed = true
			continue
		}
		next, entryErr := applyEditOp(src.content, entry.op, entry.displayPath)
		if entryErr != nil {
			batchErrs = append(batchErrs, fmt.Sprintf("%s for %s: %v", entry.argument, entry.displayPath, entryErr))
			src.failed = true
			continue
		}
		if !src.present {
			src.present = true
			src.ending = detectLineEnding(entry.op.newText)
		}
		src.content = next
		src.raw = nil
		src.touched = true
	}

	tx := &fileTransaction{}
	for _, key := range stateOrder {
		st := states[key]
		if st.failed || !st.touched {
			continue
		}
		var after []byte
		if st.present && st.raw != nil {
			after = st.raw
		} else if st.present && st.ref == nil {
			size := int64(len(st.bom)) + int64(len(st.content))
			if st.ending == "\r\n" {
				size += int64(strings.Count(st.content, "\n"))
			}
			if err := checkEditSize(st.displayPath, size); err != nil {
				batchErrs = append(batchErrs, err.Error())
				continue
			}
			after = []byte(st.bom + restoreLineEndings(st.content, st.ending))
		}
		// A swap of byte-identical native files still exchanges their inodes.
		nativeMove := st.ref != nil && st.movedFrom != ""
		unchanged := st.beforeRef.Equal(st.ref) && bytes.Equal(st.before, after) && samePermissions(st.beforeMode, st.mode)
		if st.beforePresent == st.present && (!st.present || unchanged && !nativeMove) {
			if !st.movedAway && st.movedFrom == "" {
				batchErrs = append(batchErrs, fmt.Sprintf("edits made no changes to %s", st.displayPath))
			}
			continue
		}

		switch {
		case !st.present && st.movedAway:
			// Summarized by the move destination ("R src -> dst").
		case !st.present:
			summaries = append(summaries, "D "+st.displayPath)
		case st.movedFrom != "":
			summaries = append(summaries, "R "+st.movedFrom+" -> "+st.displayPath)
		case st.beforePresent:
			summaries = append(summaries, "M "+st.displayPath)
		default:
			summaries = append(summaries, "A "+st.displayPath)
		}

		mode := st.beforeMode
		if !st.beforePresent {
			mode = st.mode
		}
		tx.changes = append(tx.changes, fileChange{
			target:        st.target,
			displayPath:   st.displayPath,
			before:        st.before,
			beforePresent: st.beforePresent,
			beforeMode:    mode,
			after:         after,
			beforeRef:     st.beforeRef,
			afterRef:      st.ref,
			movedFrom:     st.movedFrom,
			afterPresent:  st.present,
			afterMode:     &st.mode,
		})
	}
	if len(batchErrs) > 0 {
		return tool.Result{}, fmt.Errorf("%s (no files changed)", strings.Join(batchErrs, "\n\n"))
	}

	if err := commitFileTransactionLocked(ctx, root, tx); err != nil {
		return tool.Result{}, err
	}
	for _, change := range tx.changes {
		if change.afterPresent {
			freshness.record(ctx, change.target)
		} else {
			freshness.forget(ctx, change.target)
		}
	}
	changes := make([]tool.FileChange, 0, len(tx.changes))
	for _, change := range tx.changes {
		record := tool.FileChange{
			Path: change.displayPath, Mode: uint32(change.beforeMode.Perm()),
			Before: string(change.before), After: string(change.after),
			BeforeExists: change.beforePresent, AfterExists: change.afterPresent,
			BeforeRef: change.beforeRef, AfterRef: change.afterRef,
		}
		if change.afterRef != nil {
			record.MovedFrom = change.movedFrom
		}
		if change.afterPresent && change.afterMode != nil && change.afterMode.Perm() != change.beforeMode.Perm() {
			mode := uint32(change.afterMode.Perm())
			record.AfterMode = &mode
		}
		changes = append(changes, record)
	}
	summary := fmt.Sprintf("Applied %d %s across %d %s.", len(entries), plural(len(entries), "edit"), len(tx.changes), plural(len(tx.changes), "file"))
	if deleting {
		summary = fmt.Sprintf("Deleted %d %s.", len(tx.changes), plural(len(tx.changes), "file"))
	}
	summary += "\n" + strings.Join(summaries, "\n")
	if len(tx.prunedDirs) > 0 {
		summary += "\nRemoved empty directories: " + strings.Join(tx.prunedDirs, ", ")
	}
	return tool.Result{Content: tool.TextContent(summary), Metadata: map[string]any{tool.FileChangesMetadata: changes}}, nil
}

func readEditContent(ctx context.Context, root *os.Root, target fileTarget, expected os.FileInfo, guard sandboxpolicy.CredentialGuard, pureMove bool) ([]byte, *tool.FileContentRef, error) {
	file, err := openFileTarget(root, target)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("path is not a regular file")
	}
	if !os.SameFile(expected, info) {
		return nil, nil, fmt.Errorf("file changed while opening %s", target.displayPath())
	}
	if err := guard.CheckOpened(file, info); err != nil {
		return nil, nil, err
	}
	if pureMove && info.Size() > MaxEditFileBytes {
		ref, err := hashFileContent(ctx, file, info.Size())
		return nil, ref, err
	}
	if err := checkEditSize(target.AbsPath, info.Size()); err != nil {
		return nil, nil, err
	}
	content, err := readContent(ctx, file, MaxEditFileBytes)
	if err != nil {
		return nil, nil, err
	}
	if err := checkEditSize(target.AbsPath, int64(len(content))); err != nil {
		return nil, nil, err
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		if !pureMove {
			return nil, nil, fmt.Errorf("only UTF-8 text files without NUL bytes are supported")
		}
		ref, err := hashFileContent(ctx, bytes.NewReader(content), int64(len(content)))
		return nil, ref, err
	}
	return content, nil, nil
}

type stagedFileState struct {
	beforeRef, ref *tool.FileContentRef
	target         fileTarget
	displayPath    string
	before         []byte
	raw            []byte // Preserve the original bytes, including mixed line endings, through pure moves.
	beforePresent  bool
	beforeMode     os.FileMode
	present        bool
	mode           os.FileMode
	bom            string
	ending         string
	content        string
	touched        bool
	failed         bool
	movedAway      bool
	movedFrom      string
}

type parsedBatchEntry struct {
	argument        string
	target          fileTarget
	displayPath     string
	moveTarget      *fileTarget
	moveDisplayPath string
	op              editOp
}

// parseBatchEdits also accepts the previous single-file call shapes. They are
// deliberately absent from the schema so providers and models see one clear,
// efficient interface while integrations get a non-breaking migration path.
func parseBatchEdits(ctx context.Context, root *os.Root, args map[string]any, allowedWriteRoots []string, guard sandboxpolicy.CredentialGuard) ([]parsedBatchEntry, error) {
	rawEntries, err := compatibleBatchEntries(args)
	if err != nil {
		return nil, err
	}

	entries := make([]parsedBatchEntry, 0, len(rawEntries))
	for index, entry := range rawEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pathArg, ok := entry["file_path"].(string)
		if !ok || strings.TrimSpace(pathArg) == "" {
			return nil, fmt.Errorf("edits[%d].file_path is required", index)
		}
		op, err := newEditOp(entry)
		if err != nil {
			return nil, fmt.Errorf("edits[%d]: %w", index, err)
		}
		target, err := mutationSource(pathArg, root.Name(), allowedWriteRoots, guard, op)
		if err != nil {
			return nil, fmt.Errorf("edits[%d].file_path: %w", index, err)
		}
		parsed := parsedBatchEntry{
			argument:    fmt.Sprintf("edits[%d]", index),
			target:      target,
			displayPath: target.displayPath(),
			op:          op,
		}
		if op.moveTo != "" {
			dstTarget, err := resolveGuardedTarget(op.moveTo, root.Name(), allowedWriteRoots, "move file", guard)
			if err != nil {
				return nil, fmt.Errorf("edits[%d].move_to: %w", index, err)
			}
			if err := rejectFileSymlink(op.moveTo, root.Name(), "move to"); err != nil {
				return nil, fmt.Errorf("edits[%d].move_to: %w", index, err)
			}
			parsed.moveTarget = &dstTarget
			parsed.moveDisplayPath = dstTarget.displayPath()
		}
		entries = append(entries, parsed)
	}
	return entries, nil
}

// Review and undo track regular files. A move or deletion of a
// link must never be rewritten to a removal of the file it points to.
func mutationSource(path, workspace string, roots []string, guard sandboxpolicy.CredentialGuard, op editOp) (fileTarget, error) {
	action := "edit file"
	if op.deleteFile {
		action = "delete file"
	} else if op.moveTo != "" {
		action = "move file"
	}
	target, err := resolveGuardedTarget(path, workspace, roots, action, guard)
	if err != nil || (!op.deleteFile && op.moveTo == "") {
		return target, err
	}
	return target, rejectFileSymlink(path, workspace, action)
}

func rejectFileSymlink(path, workspace, action string) error {
	name := cleanPathArg(path, workspace)
	if !filepath.IsAbs(name) {
		name = filepath.Join(workspace, name)
	}
	if info, err := os.Lstat(name); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cannot %s %q: path is a symbolic link", action, path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func compatibleBatchEntries(args map[string]any) ([]map[string]any, error) {
	raw, hasBatch := args["edits"]
	if !hasBatch {
		return []map[string]any{args}, nil
	}

	if _, hasOld := args["old_string"]; hasOld {
		return nil, fmt.Errorf("provide either edits or old_string/new_string, not both")
	}
	if _, hasNew := args["new_string"]; hasNew {
		return nil, fmt.Errorf("provide either edits or old_string/new_string, not both")
	}
	if replaceAll, _ := args["replace_all"].(bool); replaceAll {
		return nil, fmt.Errorf("replace_all cannot be combined with edits; set it per edit entry instead")
	}

	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("edits must be a non-empty array")
	}
	commonPath, _ := args["file_path"].(string)
	entries := make([]map[string]any, 0, len(list))
	for index, value := range list {
		entry, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("edits[%d] must be an object", index)
		}
		if _, hasPath := entry["file_path"]; !hasPath && commonPath != "" {
			copy := make(map[string]any, len(entry)+1)
			maps.Copy(copy, entry)
			copy["file_path"] = commonPath
			entry = copy
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
