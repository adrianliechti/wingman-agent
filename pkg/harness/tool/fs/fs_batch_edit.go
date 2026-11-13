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
			"Create, edit, rename/move, or delete one or more UTF-8 text files, with batch validation and rollback on commit errors.",
			"- Put every independent replacement, file creation, move, or deletion you already know about into one `edits` array; do not call this tool repeatedly when the changes can be batched.",
			"- Read an existing file before editing, moving, or deleting it, and preserve its indentation exactly. Never include the read tool's line-number prefix.",
			"- Each `old_string` must occur exactly once unless `replace_all` is true. Include enough unchanged surrounding text to make it unique.",
			"- Exact matches take priority. If none exist, matching tolerates typography, trailing whitespace, copied line numbers, uniform indentation shifts, and small blank-line differences; ambiguous matches are rejected.",
			"- Input and resulting files are limited to 10 MiB each. Binary files are not supported.",
			"- To create a new file (or replace an empty file), use an empty `old_string` and put the complete file in `new_string`.",
			"- To delete an existing file, set `delete: true` (omit `old_string` and `new_string`).",
			"- To move or rename an existing file, set `move_to` to the destination path; omit `old_string`/`new_string` for a pure move, or include them to move and edit in one step.",
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
					"description": "All file creations, replacements, moves, and deletions to validate and stage together before committing.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"file_path":   map[string]any{"type": "string", "description": "Workspace-relative path, absolute workspace path, or path in an explicitly allowed write root."},
							"old_string":  map[string]any{"type": "string", "description": "Exact text to replace; empty when creating a file or replacing an empty file. Omit when `delete` is true or for a pure `move_to`."},
							"new_string":  map[string]any{"type": "string", "description": "Replacement text or complete content for a new file. Omit when `delete` is true or for a pure `move_to`."},
							"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence instead of requiring a unique match.", "default": false},
							"delete":      map[string]any{"type": "boolean", "description": "Set true to delete `file_path`."},
							"move_to":     map[string]any{"type": "string", "description": "Destination path when moving or renaming `file_path`. Destination must not already exist unless deleted or moved away in the same batch."},
						},
						"required":             []string{"file_path"},
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
			entries, editCount, err := parseBatchEdits(root, args, allowedWriteRoots)
			if err != nil {
				return tool.Result{}, err
			}

			states := make(map[string]*stagedFileState)
			var stateOrder []string
			var summaries []string
			var batchErrs []string

			loadState := func(target fileTarget, displayPath string) (*stagedFileState, string) {
				key := normalizePathForComparison(resolveForCompare(target.AbsPath))
				if st, ok := states[key]; ok {
					return st, ""
				}
				info, statErr := statFileTarget(root, target)
				exists := statErr == nil
				switch {
				case exists && info.IsDir():
					return nil, fmt.Sprintf("cannot edit %s: path is a directory", displayPath)
				case exists && !info.Mode().IsRegular():
					return nil, fmt.Sprintf("cannot edit %s: path is not a regular file", displayPath)
				case statErr != nil && !os.IsNotExist(statErr):
					return nil, fmt.Sprintf("stat %s: %v", displayPath, statErr)
				}
				if exists && info.Size() > MaxEditFileBytes {
					return nil, fmt.Sprintf("file %s is %d bytes; edits are capped at %d bytes", displayPath, info.Size(), MaxEditFileBytes)
				}
				if exists && freshness.stale(ctx, target, info) {
					return nil, fmt.Sprintf("cannot edit %s: the file changed on disk after you last read it; read it again first", displayPath)
				}
				var before []byte
				if exists {
					before, err = readEditContent(ctx, root, target)
					if err != nil {
						return nil, fmt.Sprintf("read %s: %v", displayPath, err)
					}
					if !utf8.Valid(before) || bytes.IndexByte(before, 0) >= 0 {
						return nil, fmt.Sprintf("cannot edit %s: only UTF-8 text files without NUL bytes are supported", displayPath)
					}
					if freshness.staleContent(ctx, target, before) {
						return nil, fmt.Sprintf("cannot edit %s: content changed since the preview; refresh it (no files changed)", displayPath)
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
				src, loadErr := loadState(entry.target, entry.displayPath)
				if loadErr != "" {
					batchErrs = append(batchErrs, loadErr)
					continue
				}
				if src.failed {
					continue
				}

				if entry.op.deleteFile {
					if !src.present {
						batchErrs = append(batchErrs, fmt.Sprintf("edits[%d] for %s: cannot delete file that does not exist (no files changed)", entry.index, entry.displayPath))
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
						batchErrs = append(batchErrs, fmt.Sprintf("edits[%d] for %s: cannot move file that does not exist (no files changed)", entry.index, entry.displayPath))
						src.failed = true
						continue
					}
					dst, dstErr := loadState(*entry.moveTarget, entry.moveDisplayPath)
					if dstErr != "" {
						batchErrs = append(batchErrs, dstErr)
						src.failed = true
						continue
					}
					if dst == src {
						batchErrs = append(batchErrs, fmt.Sprintf("edits[%d] for %s: move_to destination is the same file (no files changed)", entry.index, entry.displayPath))
						src.failed = true
						continue
					}
					if dst.failed {
						src.failed = true
						continue
					}
					if dst.present {
						batchErrs = append(batchErrs, fmt.Sprintf("edits[%d] for %s: cannot move to %s because destination already exists (no files changed)", entry.index, entry.displayPath, entry.moveDisplayPath))
						src.failed = true
						dst.failed = true
						continue
					}
					movedContent := src.content
					if entry.op.hasEdit {
						next, entryErr := applyEditOp(movedContent, entry.op, entry.displayPath)
						if entryErr != nil {
							batchErrs = append(batchErrs, fmt.Sprintf("edits[%d] for %s: %v (no files changed)", entry.index, entry.displayPath, entryErr))
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
					dst.touched = true
					dst.movedFrom = entry.displayPath

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
					batchErrs = append(batchErrs, fmt.Sprintf("edits[%d] for %s: %v (no files changed)", entry.index, entry.displayPath, entryErr))
					src.failed = true
					continue
				}
				if !src.present {
					src.present = true
					src.ending = detectLineEnding(entry.op.newText)
				}
				src.content = next
				src.touched = true
			}

			tx := &fileTransaction{}
			for _, key := range stateOrder {
				st := states[key]
				if st.failed || !st.touched {
					continue
				}
				var after []byte
				if st.present {
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
				if st.beforePresent == st.present && (!st.present || (bytes.Equal(st.before, after) && samePermissions(st.beforeMode, st.mode))) {
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
					afterPresent:  st.present,
					afterMode:     &st.mode,
				})
			}
			if len(batchErrs) > 0 {
				return tool.Result{}, fmt.Errorf("%s", strings.Join(batchErrs, "\n\n"))
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
				record := tool.FileChange{Path: change.displayPath, Before: string(change.before), After: string(change.after), BeforeExists: change.beforePresent, AfterExists: change.afterPresent, Mode: uint32(change.beforeMode.Perm())}
				if change.afterPresent && change.afterMode != nil && change.afterMode.Perm() != change.beforeMode.Perm() {
					mode := uint32(change.afterMode.Perm())
					record.AfterMode = &mode
				}
				changes = append(changes, record)
			}
			return tool.Result{Content: tool.TextContent(fmt.Sprintf("Applied %d edits across %d files.\n%s", editCount, len(tx.changes), strings.Join(summaries, "\n"))), Metadata: map[string]any{tool.FileChangesMetadata: changes}}, nil
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

type stagedFileState struct {
	target        fileTarget
	displayPath   string
	before        []byte
	beforePresent bool
	beforeMode    os.FileMode
	present       bool
	mode          os.FileMode
	bom           string
	ending        string
	content       string
	touched       bool
	failed        bool
	movedAway     bool
	movedFrom     string
}

type parsedBatchEntry struct {
	index           int
	target          fileTarget
	displayPath     string
	moveTarget      *fileTarget
	moveDisplayPath string
	op              editOp
}

// parseBatchEdits also accepts the previous single-file call shapes. They are
// deliberately absent from the schema so providers and models see one clear,
// efficient interface while integrations get a non-breaking migration path.
func parseBatchEdits(root *os.Root, args map[string]any, allowedWriteRoots []string) ([]parsedBatchEntry, int, error) {
	rawEntries, err := compatibleBatchEntries(args)
	if err != nil {
		return nil, 0, err
	}

	entries := make([]parsedBatchEntry, 0, len(rawEntries))
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

		displayPath := target.AbsPath
		if target.InWorkspace {
			displayPath = filepath.ToSlash(filepath.Clean(target.RelPath))
		}
		parsed := parsedBatchEntry{
			index:       index,
			target:      target,
			displayPath: displayPath,
			op:          op,
		}
		if op.moveTo != "" {
			dstTarget, err := resolveFileTarget(op.moveTo, root.Name(), allowedWriteRoots, "move file")
			if err != nil {
				return nil, 0, fmt.Errorf("edits[%d].move_to: %w", index, err)
			}
			dstDisplay := dstTarget.AbsPath
			if dstTarget.InWorkspace {
				dstDisplay = filepath.ToSlash(filepath.Clean(dstTarget.RelPath))
			}
			parsed.moveTarget = &dstTarget
			parsed.moveDisplayPath = dstDisplay
		}
		entries = append(entries, parsed)
	}
	return entries, len(rawEntries), nil
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
