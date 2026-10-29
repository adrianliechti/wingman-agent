package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/changes"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

const ahpWorkspaceChanges = "ahp-changeset:/wingman/uncommitted"

func (h *ahpHost) changesetCatalogue() []t.Changeset {
	if h.server.workspace == nil {
		return nil
	}
	return []t.Changeset{{Label: "Uncommitted Changes", UriTemplate: ahpWorkspaceChanges, ChangeKind: "uncommitted", Description: ahpPtr("Current workspace changes, shared by all sessions")}}
}

func (h *ahpHost) ensureChangeset() error {
	if h.server.workspace == nil {
		return ahpInvalid("workspace unavailable")
	}
	h.refreshChangeset()
	h.changesOnce.Do(func() {
		h.server.background.Go(func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-h.done:
					return
				case <-ticker.C:
				}
				h.mu.Lock()
				active := false
				for c := range h.clients {
					if c.subscriptions[ahpWorkspaceChanges] {
						active = true
						break
					}
				}
				h.mu.Unlock()
				if active {
					h.refreshChangeset()
				}
			}
		})
	})
	return nil
}

func (h *ahpHost) refreshChangeset() {
	h.changesMu.Lock()
	defer h.changesMu.Unlock()
	diffs, fingerprint, err := h.server.workspace.DiffsIfChanged(h.server.ctx, h.changesFingerprint)
	h.mu.Lock()
	editing := h.editingFilesLocked()
	if err == nil && h.changesFingerprint != 0 && fingerprint == h.changesFingerprint && h.changes != nil {
		for i, file := range h.changes.Files {
			path, _ := h.resourcePath(file.Id)
			active := editing[filepath.Clean(path)]
			if file.IsEditing == nil || *file.IsEditing != active {
				file.IsEditing = ahpPtr(active)
				h.changes.Files[i] = file
				h.publish(ahpWorkspaceChanges, t.StateAction{Value: &t.ChangesetFileSetAction{Type: t.ActionTypeChangesetFileSet, File: file}}, nil, nil)
			}
		}
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	next := &t.ChangesetState{Status: t.ChangesetStatusReady, Files: []t.ChangesetFile{}}
	content := map[string]string{}
	bytes := 0
	if err == nil {
		for _, diff := range diffs {
			bytes += len(diff.Original) + len(diff.Modified)
			if bytes > 64<<20 {
				next.Status = t.ChangesetStatusError
				next.Error = &t.ErrorInfo{ErrorType: "limit", Message: "Workspace changes exceed the 64 MiB content limit"}
				break
			}
			uri := h.fileURI(diff.Path)
			edit := t.FileEdit{}
			side := func(file, text string) *t.FileEditSide {
				hash := sha256.Sum256([]byte(text))
				resource := "ahp-content:/" + hex.EncodeToString(hash[:])
				content[resource] = text
				return &t.FileEditSide{Uri: h.fileURI(file), Content: t.ContentRef{Uri: resource, Nonce: ahpPtr(hex.EncodeToString(hash[:])), SizeHint: ahpPtr(int64(len(text))), ContentType: ahpPtr("text/plain")}}
			}
			if diff.Status != changes.StatusAdded {
				original := diff.OriginalPath
				if original == "" {
					original = diff.Path
				}
				edit.Before = side(original, diff.Original)
			}
			if diff.Status != changes.StatusDeleted {
				edit.After = side(diff.Path, diff.Modified)
			}
			var added, removed int64
			for _, line := range strings.Split(diff.Patch, "\n") {
				if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
					added++
				}
				if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
					removed++
				}
			}
			edit.Diff = &t.FileEditDiffStats{Added: &added, Removed: &removed}
			identity, _ := json.Marshal(edit)
			nonce := sha256.Sum256(identity)
			next.Files = append(next.Files, t.ChangesetFile{Id: uri, Edit: edit, Nonce: ahpPtr(hex.EncodeToString(nonce[:])), IsEditing: ahpPtr(editing[filepath.Clean(diff.Path)])})
		}
	} else {
		next.Status = t.ChangesetStatusError
		next.Error = &t.ErrorInfo{ErrorType: "workspace", Message: err.Error()}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.changes
	if next.Status == t.ChangesetStatusError && previous != nil {
		next.Files = previous.Files
	} else {
		h.contents = content
	}
	h.changes = next
	h.changesFingerprint = 0
	if next.Status == t.ChangesetStatusReady {
		h.changesFingerprint = fingerprint
	}
	if previous == nil {
		return
	}
	if previous.Status != next.Status || !reflect.DeepEqual(previous.Error, next.Error) {
		h.publish(ahpWorkspaceChanges, t.StateAction{Value: &t.ChangesetStatusChangedAction{Type: t.ActionTypeChangesetStatusChanged, Status: next.Status, Error: next.Error}}, nil, nil)
	}
	old := map[string]t.ChangesetFile{}
	for _, file := range previous.Files {
		old[file.Id] = file
	}
	for _, file := range next.Files {
		if !reflect.DeepEqual(old[file.Id], file) {
			h.publish(ahpWorkspaceChanges, t.StateAction{Value: &t.ChangesetFileSetAction{Type: t.ActionTypeChangesetFileSet, File: file}}, nil, nil)
		}
		delete(old, file.Id)
	}
	for id := range old {
		h.publish(ahpWorkspaceChanges, t.StateAction{Value: &t.ChangesetFileRemovedAction{Type: t.ActionTypeChangesetFileRemoved, FileId: id}}, nil, nil)
	}
}

// Editing is advisory UI state. It never grants permission to overwrite a file.
// Only report known file-edit tools with real locations; shell commands can
// change arbitrary files and cannot reliably be attributed to individual paths.
func (h *ahpHost) editingFilesLocked() map[string]bool {
	files := map[string]bool{}
	for _, session := range h.sessions {
		if session.chat.ActiveTurn == nil {
			continue
		}
		for _, part := range session.chat.ActiveTurn.ResponseParts {
			tool, ok := part.Value.(*t.ToolCallResponsePart)
			if !ok {
				continue
			}
			call, ok := tool.ToolCall.Value.(*t.ToolCallRunningState)
			if !ok {
				continue
			}
			var meta struct {
				ToolKind  string                 `json:"toolKind"`
				Locations []harness.ToolLocation `json:"toolLocations"`
			}
			if json.Unmarshal(call.Meta["wingman"], &meta) != nil || meta.ToolKind != "edit" {
				continue
			}
			for _, location := range meta.Locations {
				path := location.Path
				if filepath.IsAbs(path) {
					var err error
					path, err = filepath.Rel(h.server.workspace.RootPath, path)
					if err != nil {
						continue
					}
				}
				if filepath.IsLocal(path) {
					files[filepath.Clean(path)] = true
				}
			}
		}
	}
	return files
}
