package fs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
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
			"Create or edit one or more UTF-8 text files using string replacements, with batch validation and rollback on commit errors.",
			"- Put every independent replacement or file creation you already know about into one `edits` array; do not call this tool repeatedly when the changes can be batched.",
			"- Read an existing file before your first edit to it, and preserve its indentation exactly. Never include the read tool's line-number prefix.",
			"- Each `old_string` must occur exactly once unless `replace_all` is true. Include enough unchanged surrounding text to make it unique.",
			"- Exact matches take priority. If none exist, matching tolerates typography, trailing whitespace, copied line numbers, uniform indentation shifts, and small blank-line differences; ambiguous matches are rejected.",
			"- Input and resulting files are limited to 10 MiB each. Binary files are not supported.",
			"- To create a new file (or replace an empty file), use an empty `old_string` and put the complete file in `new_string`.",
			"- Entries for the same file run in order, so later entries see earlier replacements.",
			"- Every entry is validated and every result is staged before any file is changed. Validation failures leave files unchanged; commit failures attempt rollback and report any restoration errors. External readers may observe the batch being committed.",
			"- The result is authoritative; do not re-read files merely to verify a successful edit.",
		}, "\n"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"edits": map[string]any{
					"type":        "array",
					"minItems":    1,
					"description": "All file creations and replacements to validate and stage together before committing.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"file_path":   map[string]any{"type": "string", "description": "Workspace-relative path, absolute workspace path, or path in an explicitly allowed write root."},
							"old_string":  map[string]any{"type": "string", "description": "Exact text to replace; empty only when creating a file or replacing an empty file."},
							"new_string":  map[string]any{"type": "string", "description": "Replacement text or complete content for a new file."},
							"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence instead of requiring a unique match.", "default": false},
						},
						"required":             []string{"file_path", "old_string", "new_string"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"edits"},
			"additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			fileEditMu.Lock()
			defer fileEditMu.Unlock()
			if err := ctx.Err(); err != nil {
				return tool.Result{}, err
			}
			files, editCount, err := parseBatchEdits(root, args, allowedWriteRoots)
			if err != nil {
				return tool.Result{}, err
			}

			tx := &fileTransaction{}
			summaries := make([]string, 0, len(files))
			var batchErrs []string
			for _, file := range files {
				if err := ctx.Err(); err != nil {
					return tool.Result{}, err
				}
				info, statErr := statFileTarget(root, file.target)
				exists := statErr == nil
				switch {
				case exists && info.IsDir():
					batchErrs = append(batchErrs, fmt.Sprintf("cannot edit %s: path is a directory", file.displayPath))
					continue
				case exists && !info.Mode().IsRegular():
					batchErrs = append(batchErrs, fmt.Sprintf("cannot edit %s: path is not a regular file", file.displayPath))
					continue
				case statErr != nil && !os.IsNotExist(statErr):
					batchErrs = append(batchErrs, fmt.Sprintf("stat %s: %v", file.displayPath, statErr))
					continue
				}
				if !exists && file.entries[0].op.oldText != "" {
					batchErrs = append(batchErrs, fmt.Sprintf("cannot edit %s: file does not exist; use an empty old_string to create it", file.displayPath))
					continue
				}
				if exists && info.Size() > MaxEditFileBytes {
					batchErrs = append(batchErrs, fmt.Sprintf("file %s is %d bytes; edits are capped at %d bytes", file.displayPath, info.Size(), MaxEditFileBytes))
					continue
				}
				if exists && freshness.stale(ctx, file.target, info) {
					batchErrs = append(batchErrs, fmt.Sprintf("cannot edit %s: the file changed on disk after you last read it; read it again first", file.displayPath))
					continue
				}

				var before []byte
				if exists {
					before, err = readEditContent(ctx, root, file.target)
					if err != nil {
						batchErrs = append(batchErrs, fmt.Sprintf("read %s: %v", file.displayPath, err))
						continue
					}
				}
				if !utf8.Valid(before) || bytes.IndexByte(before, 0) >= 0 {
					batchErrs = append(batchErrs, fmt.Sprintf("cannot edit %s: only UTF-8 text files without NUL bytes are supported", file.displayPath))
					continue
				}
				if freshness.staleContent(ctx, file.target, before) {
					batchErrs = append(batchErrs, fmt.Sprintf("cannot edit %s: content changed since the preview; refresh it (no files changed)", file.displayPath))
					continue
				}
				bom, content := stripBom(string(before))
				ending := detectLineEnding(content)
				updated := normalizeToLF(content)
				fileFailed := false
				for _, entry := range file.entries {
					if err := ctx.Err(); err != nil {
						return tool.Result{}, err
					}
					next, entryErr := applyEditOp(updated, entry.op, file.displayPath)
					if entryErr != nil {
						batchErrs = append(batchErrs, fmt.Sprintf("edits[%d] for %s: %v (no files changed)", entry.index, file.displayPath, entryErr))
						fileFailed = true
						continue
					}
					updated = next
				}
				if fileFailed {
					continue
				}
				size := int64(len(bom)) + int64(len(updated))
				if ending == "\r\n" {
					size += int64(strings.Count(updated, "\n"))
				}
				if err := checkEditSize(file.displayPath, size); err != nil {
					batchErrs = append(batchErrs, err.Error())
					continue
				}
				after := []byte(bom + restoreLineEndings(updated, ending))
				if exists && bytes.Equal(before, after) {
					batchErrs = append(batchErrs, fmt.Sprintf("edits made no changes to %s", file.displayPath))
					continue
				}

				mode := os.FileMode(0o644)
				if exists {
					mode = info.Mode().Perm()
					summaries = append(summaries, "M "+file.displayPath)
				} else {
					summaries = append(summaries, "A "+file.displayPath)
				}
				tx.changes = append(tx.changes, fileChange{
					target:        file.target,
					displayPath:   file.displayPath,
					before:        before,
					beforePresent: exists,
					beforeMode:    mode,
					after:         after,
					afterPresent:  true,
				})
			}
			if len(batchErrs) > 0 {
				return tool.Result{}, fmt.Errorf("%s", strings.Join(batchErrs, "\n\n"))
			}

			if err := commitFileTransactionLocked(ctx, root, tx); err != nil {
				return tool.Result{}, err
			}
			for _, change := range tx.changes {
				freshness.record(ctx, change.target)
			}
			changes := make([]tool.FileChange, 0, len(tx.changes))
			for _, change := range tx.changes {
				changes = append(changes, tool.FileChange{Path: change.displayPath, Before: string(change.before), After: string(change.after), BeforeExists: change.beforePresent, AfterExists: change.afterPresent, Mode: uint32(change.beforeMode.Perm())})
			}
			return tool.Result{Content: fmt.Sprintf("Applied %d edits across %d files.\n%s", editCount, len(files), strings.Join(summaries, "\n")), Metadata: map[string]any{tool.FileChangesMetadata: changes}}, nil
		},
	}
}

func readEditContent(ctx context.Context, root *os.Root, target fileTarget) ([]byte, error) {
	file, err := openFileTarget(root, target)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file")
	}
	if err := checkEditSize(target.AbsPath, info.Size()); err != nil {
		return nil, err
	}
	content, err := readContent(ctx, file, MaxEditFileBytes)
	if err != nil {
		return nil, err
	}
	if err := checkEditSize(target.AbsPath, int64(len(content))); err != nil {
		return nil, err
	}
	return content, nil
}

type batchEditEntry struct {
	index int
	op    editOp
}

type batchEditFile struct {
	target      fileTarget
	displayPath string
	entries     []batchEditEntry
}

// parseBatchEdits also accepts the previous single-file call shapes. They are
// deliberately absent from the schema so providers and models see one clear,
// efficient interface while integrations get a non-breaking migration path.
func parseBatchEdits(root *os.Root, args map[string]any, allowedWriteRoots []string) ([]batchEditFile, int, error) {
	rawEntries, err := compatibleBatchEntries(args)
	if err != nil {
		return nil, 0, err
	}

	files := make([]batchEditFile, 0, len(rawEntries))
	byTarget := make(map[string]int)
	for index, entry := range rawEntries {
		pathArg, ok := entry["file_path"].(string)
		if !ok || strings.TrimSpace(pathArg) == "" {
			return nil, 0, fmt.Errorf("edits[%d].file_path is required", index)
		}
		target, err := resolveFileTarget(pathArg, root.Name(), allowedWriteRoots, "edit file")
		if err != nil {
			return nil, 0, fmt.Errorf("edits[%d].file_path: %w", index, err)
		}
		op, err := newEditOp(entry)
		if err != nil {
			return nil, 0, fmt.Errorf("edits[%d]: %w", index, err)
		}

		key := normalizePathForComparison(resolveForCompare(target.AbsPath))
		fileIndex, exists := byTarget[key]
		if !exists {
			fileIndex = len(files)
			byTarget[key] = fileIndex
			displayPath := target.AbsPath
			if target.InWorkspace {
				displayPath = filepath.ToSlash(filepath.Clean(target.RelPath))
			}
			files = append(files, batchEditFile{target: target, displayPath: displayPath})
		}
		files[fileIndex].entries = append(files[fileIndex].entries, batchEditEntry{index: index, op: op})
	}
	return files, len(rawEntries), nil
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
			for key, value := range entry {
				copy[key] = value
			}
			copy["file_path"] = commonPath
			entry = copy
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
