package lsp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"go.lsp.dev/protocol"
)

const maxPreviewBytes = 24000

type previewEdit struct {
	Path string `json:"file_path"`
	Old  string `json:"old_string"`
	New  string `json:"new_string"`
}

func workspaceEditPreview(manager *lsp.Manager, edit *lsp.WorkspaceEdit, queryStarted ...time.Time) (tool.Result, error) {
	if edit == nil {
		return tool.Text("No edits proposed."), nil
	}
	byURI := make(map[lsp.DocumentURI][]lsp.TextEdit)
	versions := make(map[lsp.DocumentURI]*int32)
	if len(edit.DocumentChanges) > 0 && len(edit.Changes) > 0 {
		return tool.Result{}, fmt.Errorf("cannot preview an edit containing both changes and documentChanges")
	}
	for uri, edits := range edit.Changes {
		byURI[uri] = append(byURI[uri], edits...)
	}
	for _, change := range edit.DocumentChanges {
		doc, ok := change.(*lsp.TextDocumentEdit)
		if !ok {
			return tool.Result{}, fmt.Errorf("preview contains file creation, rename, or deletion; use the editor to review the complete operation")
		}
		uri := doc.TextDocument.URI
		if _, exists := byURI[uri]; exists {
			return tool.Result{}, fmt.Errorf("multiple document edits for %s require editor review", uri)
		}
		versions[uri] = doc.TextDocument.Version
		byURI[uri] = []lsp.TextEdit{}
		for _, element := range doc.Edits {
			switch e := element.(type) {
			case *lsp.TextEdit:
				byURI[uri] = append(byURI[uri], *e)
			case *protocol.AnnotatedTextEdit:
				if annotation, ok := edit.ChangeAnnotations[e.AnnotationID]; ok && annotation.NeedsConfirmation != nil && *annotation.NeedsConfirmation {
					return tool.Result{}, fmt.Errorf("edit requires annotation confirmation; review it in the editor")
				}
				byURI[uri] = append(byURI[uri], e.TextEdit)
			default:
				return tool.Result{}, fmt.Errorf("unsupported snippet/edit type; review it in the editor")
			}
		}
	}
	var uris []lsp.DocumentURI
	for uri := range byURI {
		uris = append(uris, uri)
	}
	slices.SortFunc(uris, func(a, b lsp.DocumentURI) int { return cmp.Compare(a.String(), b.String()) })
	if len(uris) > 20 {
		return tool.Result{}, fmt.Errorf("preview affects %d files (limit 20); review the refactoring in the editor", len(uris))
	}
	var edits []previewEdit
	var snapshots []tool.FileSnapshot
	for _, uri := range uris {
		path, ok := fileuri.Path(uri.String())
		if !ok {
			return tool.Result{}, fmt.Errorf("edit URI is not a local file: %s", uri)
		}
		path, err := resolveExistingFile(manager.WorkingDir(), path)
		if err != nil {
			return tool.Result{}, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return tool.Result{}, err
		}
		if len(queryStarted) > 0 && info.ModTime().After(queryStarted[0]) {
			return tool.Result{}, fmt.Errorf("%s changed during the LSP query; refresh the preview", path)
		}
		if info.Size() > 2*1024*1024 {
			return tool.Result{}, fmt.Errorf("%s exceeds the 2 MiB preview input limit; use the editor", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return tool.Result{}, err
		}
		if !utf8.Valid(data) {
			return tool.Result{}, fmt.Errorf("cannot preview non-UTF-8 file %s", path)
		}
		if session, ok := manager.ActiveSession(path); ok {
			content, version, opened := session.DocumentSnapshot(path)
			if opened && content != string(data) {
				return tool.Result{}, fmt.Errorf("%s changed or has an unsaved buffer; refresh the preview after saving", path)
			}
			if !previewVersionMatches(versions[uri], version, opened) {
				return tool.Result{}, fmt.Errorf("stale document version for %s; refresh the preview", path)
			}
		} else if !previewVersionMatches(versions[uri], 0, false) {
			return tool.Result{}, fmt.Errorf("cannot verify document version for %s", path)
		}
		updated, err := applyTextEdits(string(data), byURI[uri])
		if err != nil {
			return tool.Result{}, fmt.Errorf("%s: %w", path, err)
		}
		if updated == string(data) {
			continue
		}
		// The edit tool preserves BOM and line endings itself.
		old, next := compactReplacement(strings.TrimPrefix(string(data), "\ufeff"), strings.TrimPrefix(updated, "\ufeff"))
		if strings.HasPrefix(string(data), "\ufeff") != strings.HasPrefix(updated, "\ufeff") {
			return tool.Result{}, fmt.Errorf("preview changes the BOM; review it in the editor")
		}
		edits = append(edits, previewEdit{Path: relativePath(manager.WorkingDir(), path), Old: old, New: next})
		snapshots = append(snapshots, tool.FileSnapshot{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(data))})
	}
	if len(edits) == 0 {
		return tool.Text("No edits proposed."), nil
	}
	payload, err := json.MarshalIndent(struct {
		Edits []previewEdit `json:"edits"`
	}{edits}, "", "  ")
	if err != nil {
		return tool.Result{}, err
	}
	if len(payload) > maxPreviewBytes {
		return tool.Result{}, fmt.Errorf("preview exceeds %d bytes; review the complete refactoring in the editor (no partial edit payload returned)", maxPreviewBytes)
	}
	return tool.Result{Content: "Preview only; no files changed. Review these exact-match replacements, then pass this JSON to edit for authorized changes. If files change, refresh the preview.\n" + string(payload), Metadata: map[string]any{tool.FileSnapshotsMetadata: snapshots}}, nil
}

func previewVersionMatches(requested *int32, current int, opened bool) bool {
	if requested == nil {
		return true
	}
	if opened {
		return int(*requested) == current
	}
	// gopls uses version 0 for disk files, where LSP also allows null.
	// There is no editor version until didOpen; disk freshness and the private
	// content snapshot guard these files. Never waive an opened version check.
	return *requested == 0
}

// Keep a unique, complete-line old_string so the normal atomic edit tool can
// check its precondition. Never substitute a global textual rename.
func compactReplacement(before, after string) (string, string) {
	prefix := 0
	for prefix < min(len(before), len(after)) && before[prefix] == after[prefix] {
		prefix++
	}
	start := strings.LastIndex(before[:prefix], "\n") + 1
	suffix := 0
	for suffix < min(len(before)-start, len(after)-start) && before[len(before)-suffix-1] == after[len(after)-suffix-1] {
		suffix++
	}
	end := len(before) - suffix
	if next := strings.IndexByte(before[end:], '\n'); next >= 0 {
		end += next + 1
	} else {
		end = len(before)
	}
	newEnd := len(after) - (len(before) - end)
	old, next := before[start:end], after[start:newEnd]
	if old == "" || strings.Count(before, old) != 1 || strings.Index(before, old) != start {
		return before, after
	}
	return old, next
}

func textOffset(text string, pos lsp.Position) (int, error) {
	start := 0
	for line := uint32(0); line < pos.Line; line++ {
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			return 0, fmt.Errorf("edit line out of bounds")
		}
		start += end + 1
	}
	units := uint32(0)
	for offset, r := range text[start:] {
		if units == pos.Character {
			return start + offset, nil
		}
		if r == '\n' || r == '\r' {
			break
		}
		units++
		if r > 0xffff {
			units++
		}
		if units > pos.Character {
			return 0, fmt.Errorf("edit splits a UTF-16 surrogate pair")
		}
	}
	if units == pos.Character {
		return len(text), nil
	}
	return 0, fmt.Errorf("edit column out of bounds")
}

func applyTextEdits(text string, edits []lsp.TextEdit) (string, error) {
	type change struct {
		start, end int
		text       string
	}
	var changes []change
	for _, edit := range edits {
		start, err := textOffset(text, edit.Range.Start)
		if err != nil {
			return "", err
		}
		end, err := textOffset(text, edit.Range.End)
		if err != nil {
			return "", err
		}
		if end < start {
			return "", fmt.Errorf("reversed edit range")
		}
		changes = append(changes, change{start, end, edit.NewText})
	}
	slices.SortStableFunc(changes, func(a, b change) int { return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.end, b.end)) })
	var out strings.Builder
	previous := 0
	for i, c := range changes {
		if c.start < previous || i > 0 && c.start == changes[i-1].start {
			return "", fmt.Errorf("overlapping edits require editor review")
		}
		out.WriteString(text[previous:c.start])
		out.WriteString(c.text)
		previous = c.end
	}
	out.WriteString(text[previous:])
	return out.String(), nil
}

func previewCodeActions(ctx context.Context, manager *lsp.Manager, args map[string]any, p resultPage) (tool.Result, error) {
	started := time.Now()
	path, err := requiredFileArg(manager.WorkingDir(), args, "file_path")
	if err != nil {
		return tool.Result{}, err
	}
	session, uri, err := openFile(ctx, manager, path)
	if err != nil {
		return tool.Result{}, err
	}
	if err := requireCapability(session.Capabilities(), "codeActions"); err != nil {
		return tool.Result{}, err
	}
	lines, err := readLines(path)
	if err != nil {
		return tool.Result{}, err
	}
	selection := lsp.Range{End: lsp.Position{Line: uint32(len(lines) - 1), Character: uint32(utf16ColumnFromRune(lines[len(lines)-1], len([]rune(lines[len(lines)-1]))))}}
	if args["line"] != nil || args["column"] != nil || args["symbol"] != nil {
		position, lookup, err := resolvePosition(path, args)
		if err != nil {
			return tool.Result{}, err
		}
		if lookup != "" {
			line, _, _ := optionalPositiveInt(args, "line")
			position, err = symbolPosition(ctx, session, uri, lookup, line)
			if err != nil {
				return tool.Result{}, err
			}
		}
		selection = lsp.Range{Start: position, End: position}
	}
	actions, err := session.CodeActions(ctx, uri, selection, nil, protocol.CodeActionTriggerKindInvoked)
	if err != nil {
		return tool.Result{}, fmt.Errorf("code actions failed: %w. %s", err, fallbackAdvice)
	}
	title, _ := args["action_title"].(string)
	if title == "" {
		var lines []string
		for _, value := range actions {
			switch action := value.(type) {
			case *lsp.CodeAction:
				note := ""
				if action.Disabled.Reason != "" {
					note = " (disabled: " + action.Disabled.Reason + ")"
				} else if action.Command.Command != "" {
					note = " (requires command; editor only)"
				}
				lines = append(lines, action.Title+note)
			case *lsp.Command:
				lines = append(lines, action.Title+" (requires command; editor only)")
			}
		}
		slices.Sort(lines)
		return tool.Text(fmt.Sprintf("Code actions (%d found). Pass action_title to preview an editable action:\n%s%s", len(lines), strings.Join(pageSlice(lines, p), "\n"), pageNote(len(lines), p, "actions"))), nil
	}
	var matched []*lsp.CodeAction
	for _, value := range actions {
		if action, ok := value.(*lsp.CodeAction); ok && action.Title == title {
			matched = append(matched, action)
		}
	}
	if len(matched) != 1 {
		return tool.Result{}, fmt.Errorf("action %q has %d editable matches; choose a unique title and specific position", title, len(matched))
	}
	action := matched[0]
	if action.Edit == nil && action.Data != nil {
		if options, ok := session.Capabilities().CodeActionProvider.(*lsp.CodeActionOptions); ok && options.ResolveProvider != nil && *options.ResolveProvider {
			action, err = session.ResolveCodeAction(ctx, *action)
			if err != nil {
				return tool.Result{}, err
			}
		}
	}
	if action == nil {
		return tool.Result{}, fmt.Errorf("server returned no resolved action")
	}
	if action.Disabled.Reason != "" {
		return tool.Result{}, fmt.Errorf("action disabled: %s", action.Disabled.Reason)
	}
	if action.Command.Command != "" {
		return tool.Result{}, fmt.Errorf("action requires command execution; review and run it in the editor")
	}
	if action.Edit == nil {
		return tool.Result{}, fmt.Errorf("action has no previewable edit; use the editor")
	}
	return workspaceEditPreview(manager, action.Edit, started)
}
