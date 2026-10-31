package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/changes"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

const (
	ahpWorkspaceChanges      = "ahp-changeset:/wingman/uncommitted"
	ahpStagedChanges         = "ahp-changeset:/wingman/staged"
	ahpUnstagedChanges       = "ahp-changeset:/wingman/unstaged"
	ahpChangesetContentLimit = 128 << 20
)

type ahpChangeset struct {
	state       t.ChangesetState
	contents    map[string]string
	touched     time.Time
	comparison  bool
	fingerprint uint64
}

func changesetLayer(channel string) (changes.DiffLayer, bool) {
	switch channel {
	case ahpWorkspaceChanges:
		return changes.DiffCombined, true
	case ahpStagedChanges:
		return changes.DiffStaged, true
	case ahpUnstagedChanges:
		return changes.DiffUnstaged, true
	default:
		return "", false
	}
}

func (h *ahpHost) changesetCatalogue() []t.Changeset {
	if h.server.workspace == nil {
		return nil
	}
	return []t.Changeset{
		{Label: "Uncommitted Changes", UriTemplate: ahpWorkspaceChanges, ChangeKind: "uncommitted", Description: ahpPtr("Current workspace changes, shared by all sessions")},
		{Label: "Staged Changes", UriTemplate: ahpStagedChanges, ChangeKind: "staged"},
		{Label: "Unstaged Changes", UriTemplate: ahpUnstagedChanges, ChangeKind: "unstaged"},
	}
}

func (h *ahpHost) ensureChangeset(channel string) error {
	if h.server.workspace == nil {
		return ahpInvalid("workspace unavailable")
	}
	h.changesMu.Lock()
	defer h.changesMu.Unlock()
	if _, ok := changesetLayer(channel); !ok {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c := h.changesets[channel]; c != nil {
			c.touched = time.Now()
			return nil
		}
		return &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "changeset is no longer available; compare revisions again"}
	}
	h.refreshChangesetLocked(channel, false)
	h.changesOnce.Do(func() {
		h.server.background.Go(func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-h.done:
					return
				case <-ticker.C:
					h.refreshChangesets()
				}
			}
		})
	})
	return nil
}

func (h *ahpHost) subscribedChangesetLocked(channel string) bool {
	for client := range h.clients {
		if client.subscriptions[channel] {
			return true
		}
	}
	return false
}

func (h *ahpHost) refreshChangesets() {
	h.changesMu.Lock()
	defer h.changesMu.Unlock()
	for _, channel := range []string{ahpWorkspaceChanges, ahpStagedChanges, ahpUnstagedChanges} {
		h.mu.Lock()
		active := h.subscribedChangesetLocked(channel)
		h.mu.Unlock()
		if active {
			h.refreshChangesetLocked(channel, false)
		}
	}
}

func (h *ahpHost) buildChangeset(diffs []changes.FileDiff, editing map[string]bool) (*ahpChangeset, error) {
	next := &ahpChangeset{state: t.ChangesetState{Status: t.ChangesetStatusReady, Files: []t.ChangesetFile{}}, contents: map[string]string{}, touched: time.Now()}
	bytes := 0
	contentRef := func(text, contentType string) t.ContentRef {
		hash := sha256.Sum256([]byte(text))
		id := hex.EncodeToString(hash[:])
		uri := "ahp-content:/" + id
		if _, exists := next.contents[uri]; !exists {
			bytes += len(text)
		}
		next.contents[uri] = text
		return t.ContentRef{Uri: uri, Nonce: ahpPtr(id), SizeHint: ahpPtr(int64(len(text))), ContentType: ahpPtr(contentType)}
	}
	for _, diff := range diffs {
		edit := t.FileEdit{}
		contentType := "text/plain"
		if diff.Binary {
			contentType = "application/octet-stream"
		}
		if diff.Status != changes.StatusAdded {
			original := diff.OriginalPath
			if original == "" {
				original = diff.Path
			}
			edit.Before = &t.FileEditSide{Uri: h.fileURI(original), Content: contentRef(diff.Original, contentType)}
		}
		if diff.Status != changes.StatusDeleted {
			edit.After = &t.FileEditSide{Uri: h.fileURI(diff.Path), Content: contentRef(diff.Modified, contentType)}
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
		// The patch also covers changes the contents cannot show, such as modes.
		identity, _ := json.Marshal(struct {
			Edit  t.FileEdit
			Patch string
		}{edit, diff.Patch})
		nonce := sha256.Sum256(identity)
		next.state.Files = append(next.state.Files, t.ChangesetFile{
			Id: h.fileURI(diff.Path), Edit: edit, Nonce: ahpPtr(hex.EncodeToString(nonce[:])),
			IsEditing: ahpPtr(editing[filepath.Clean(diff.Path)]),
		})
		if bytes > 64<<20 {
			return nil, fmt.Errorf("changeset exceeds the 64 MiB content limit")
		}
	}
	return next, nil
}

func changesetOperations(channel string, isGit bool, hasFiles bool) []t.ChangesetOperation {
	if !hasFiles {
		return nil
	}
	ops := []t.ChangesetOperation{}
	if isGit && channel == ahpUnstagedChanges {
		ops = append(ops, t.ChangesetOperation{Id: "stage", Label: "Stage", Scopes: []t.ChangesetOperationScope{t.ChangesetOperationScopeChangeset, t.ChangesetOperationScopeResource}, Status: t.ChangesetOperationStatusIdle})
	}
	if isGit && channel == ahpStagedChanges {
		ops = append(ops, t.ChangesetOperation{Id: "unstage", Label: "Unstage", Scopes: []t.ChangesetOperationScope{t.ChangesetOperationScopeChangeset, t.ChangesetOperationScopeResource}, Status: t.ChangesetOperationStatusIdle})
	}
	if channel == ahpUnstagedChanges {
		confirmation := t.StringOrMarkdown{Plain: ahpPtr("Discard this file's working changes? This cannot be undone in Wingman.")}
		ops = append(ops, t.ChangesetOperation{Id: "discard-changes", Label: "Discard changes", Confirmation: &confirmation,
			Scopes: []t.ChangesetOperationScope{t.ChangesetOperationScopeResource}, Status: t.ChangesetOperationStatusIdle})
	}
	return ops
}

func (h *ahpHost) refreshChangesetLocked(channel string, force bool) {
	layer, _ := changesetLayer(channel)
	h.mu.Lock()
	editing := h.editingFilesLocked()
	var previous *ahpChangeset
	if cached := h.changesets[channel]; cached != nil {
		copy := *cached
		previous = &copy
	}
	fingerprint := uint64(0)
	if !force && previous != nil && previous.state.Status == t.ChangesetStatusReady {
		fingerprint = previous.fingerprint
	}
	h.mu.Unlock()
	diffs, current, err := h.server.workspace.DiffsLayerIfChanged(h.server.ctx, layer, fingerprint)
	var next *ahpChangeset
	if err == nil {
		if fingerprint != 0 && current == fingerprint {
			copy := *previous
			copy.state.Files = slices.Clone(previous.state.Files)
			for i := range copy.state.Files {
				path, _ := h.resourcePath(copy.state.Files[i].Id)
				copy.state.Files[i].IsEditing = ahpPtr(editing[filepath.Clean(path)])
			}
			next = &copy
		} else {
			next, err = h.buildChangeset(diffs, editing)
		}
		if err == nil {
			next.fingerprint = current
		}
	}
	if err == nil {
		next.state.Operations = changesetOperations(channel, h.server.workspace.IsGitRepo(), len(next.state.Files) > 0)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.changesets == nil {
		h.changesets = map[string]*ahpChangeset{}
	}
	for err == nil && !h.changesetFitsLocked(channel, next) {
		if !h.evictComparisonLocked(channel) {
			err = fmt.Errorf("retained changesets exceed the 128 MiB content limit")
		}
	}
	if err != nil {
		next = &ahpChangeset{state: t.ChangesetState{Status: t.ChangesetStatusError, Files: []t.ChangesetFile{}, Error: &t.ErrorInfo{ErrorType: "workspace", Message: err.Error()}}, touched: time.Now()}
		if previous := h.changesets[channel]; previous != nil {
			next.state.Files = previous.state.Files
			next.contents = previous.contents
		}
	}
	h.publishChangesetLocked(channel, next)
}

func (h *ahpHost) changesetFitsLocked(channel string, next *ahpChangeset) bool {
	content := map[string]string{}
	for uri, c := range h.changesets {
		if uri != channel {
			for key, value := range c.contents {
				content[key] = value
			}
		}
	}
	for key, value := range next.contents {
		content[key] = value
	}
	bytes := 0
	for _, value := range content {
		bytes += len(value)
		if bytes > ahpChangesetContentLimit {
			return false
		}
	}
	return true
}

func (h *ahpHost) comparisonChangeset(diffs []changes.FileDiff) (string, error) {
	next, err := h.buildChangeset(diffs, nil)
	if err != nil {
		return "", err
	}
	next.comparison = true
	identity, _ := json.Marshal(next.state.Files)
	hash := sha256.Sum256(identity)
	channel := "ahp-changeset:/wingman/compare/" + hex.EncodeToString(hash[:])
	h.changesMu.Lock()
	defer h.changesMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.changesets == nil {
		h.changesets = map[string]*ahpChangeset{}
	}
	comparisons := 0
	for _, c := range h.changesets {
		if c.comparison {
			comparisons++
		}
	}
	for (h.changesets[channel] == nil && comparisons >= 32) || !h.changesetFitsLocked(channel, next) {
		if !h.evictComparisonLocked(channel) {
			return "", &ahpRPCError{Code: -32009, Message: "too many retained comparisons; close comparisons and retry"}
		}
		comparisons--
	}
	h.changesets[channel] = next
	return channel, nil
}

// evictComparisonLocked drops the least recently used idle comparison other
// than keep. It reports whether one was dropped.
func (h *ahpHost) evictComparisonLocked(keep string) bool {
	oldest := ""
	for uri, c := range h.changesets {
		if uri != keep && c.comparison && !h.subscribedChangesetLocked(uri) && time.Since(c.touched) > time.Minute &&
			(oldest == "" || c.touched.Before(h.changesets[oldest].touched)) {
			oldest = uri
		}
	}
	if oldest == "" {
		return false
	}
	delete(h.changesets, oldest)
	return true
}

func (h *ahpHost) changesetContentLocked(uri string) (string, bool) {
	for _, c := range h.changesets {
		if content, ok := c.contents[uri]; ok {
			c.touched = time.Now()
			return content, true
		}
	}
	return "", false
}

func (h *ahpHost) publishChangesetLocked(channel string, next *ahpChangeset) {
	previous := h.changesets[channel]
	h.changesets[channel] = next
	if previous == nil {
		return
	}
	if previous.state.Status != next.state.Status || !reflect.DeepEqual(previous.state.Error, next.state.Error) {
		h.publish(channel, t.StateAction{Value: &t.ChangesetStatusChangedAction{Type: t.ActionTypeChangesetStatusChanged, Status: next.state.Status, Error: next.state.Error}}, nil, nil)
	}
	old := map[string]t.ChangesetFile{}
	for _, f := range previous.state.Files {
		old[f.Id] = f
	}
	for _, f := range next.state.Files {
		if !reflect.DeepEqual(old[f.Id], f) {
			h.publish(channel, t.StateAction{Value: &t.ChangesetFileSetAction{Type: t.ActionTypeChangesetFileSet, File: f}}, nil, nil)
		}
		delete(old, f.Id)
	}
	for id := range old {
		h.publish(channel, t.StateAction{Value: &t.ChangesetFileRemovedAction{Type: t.ActionTypeChangesetFileRemoved, FileId: id}}, nil, nil)
	}
	if !reflect.DeepEqual(previous.state.Operations, next.state.Operations) {
		h.publish(channel, t.StateAction{Value: &t.ChangesetOperationsChangedAction{Type: t.ActionTypeChangesetOperationsChanged, Operations: next.state.Operations}}, nil, nil)
	}
}

func (h *ahpHost) operationStatusLocked(channel, id string, status t.ChangesetOperationStatus, err error) {
	c := h.changesets[channel]
	if c == nil {
		return
	}
	for i := range c.state.Operations {
		op := &c.state.Operations[i]
		if op.Id != id {
			continue
		}
		op.Status, op.Error = status, nil
		if err != nil {
			op.Error = &t.ErrorInfo{ErrorType: "git", Message: err.Error()}
		}
		h.publish(channel, t.StateAction{Value: &t.ChangesetOperationStatusChangedAction{Type: t.ActionTypeChangesetOperationStatusChanged, OperationId: id, Status: status, Error: op.Error}}, nil, nil)
	}
}

func (h *ahpHost) invokeChangesetOperation(data json.RawMessage) (any, error) {
	var p t.InvokeChangesetOperationParams
	if err := decodeGitParams(data, &p); err != nil {
		return nil, err
	}
	if p.Target != nil {
		// The target union decodes leniently; a misspelled nonce must not skip the stale check.
		var raw struct {
			Target json.RawMessage `json:"target"`
		}
		_ = json.Unmarshal(data, &raw)
		if err := decodeGitParams(raw.Target, p.Target.Value); err != nil {
			return nil, err
		}
	}
	if _, ok := changesetLayer(p.Channel); !ok {
		return nil, ahpInvalid("changeset has no operations")
	}
	// Refresh and validate nonces before admitting a mutation; serialize
	// operations across clients so two stale previews cannot both be accepted.
	h.changesMu.Lock()
	defer h.changesMu.Unlock()
	h.refreshChangesetLocked(p.Channel, true)
	h.mu.Lock()
	c := h.changesets[p.Channel]
	operation := slices.IndexFunc(c.state.Operations, func(op t.ChangesetOperation) bool { return op.Id == p.OperationId })
	if operation < 0 || c.state.Status != t.ChangesetStatusReady {
		h.mu.Unlock()
		return nil, ahpInvalid("operation is not available on this changeset")
	}
	op := c.state.Operations[operation]
	scope := t.ChangesetOperationScopeChangeset
	paths := []string{}
	if p.Target != nil {
		target, ok := p.Target.Value.(*t.ChangesetOperationResourceTarget)
		if !ok {
			h.mu.Unlock()
			return nil, ahpInvalid("range operations are not supported")
		}
		if target.Side != nil && *target.Side != "before" && *target.Side != "after" {
			h.mu.Unlock()
			return nil, ahpInvalid("invalid target side")
		}
		scope = t.ChangesetOperationScopeResource
		found := false
		for _, file := range c.state.Files {
			if file.Id != target.Resource {
				continue
			}
			found = true
			if target.Side != nil && (*target.Side == "before" && file.Edit.Before == nil || *target.Side == "after" && file.Edit.After == nil) {
				h.mu.Unlock()
				return nil, ahpInvalid("target side is not present in this change")
			}
			if target.Nonce != nil && file.Nonce != nil && *target.Nonce != *file.Nonce {
				h.mu.Unlock()
				return nil, &ahpRPCError{Code: -32011, Message: "file changed since the displayed diff; refresh before retrying"}
			}
			path, err := h.resourcePath(file.Id)
			if err != nil {
				h.mu.Unlock()
				return nil, err
			}
			paths = append(paths, path)
		}
		if !found {
			h.mu.Unlock()
			return nil, ahpInvalid("file is not in this changeset")
		}
	} else {
		for _, file := range c.state.Files {
			path, err := h.resourcePath(file.Id)
			if err != nil {
				h.mu.Unlock()
				return nil, err
			}
			paths = append(paths, path)
		}
	}
	if !slices.Contains(op.Scopes, scope) {
		h.mu.Unlock()
		return nil, ahpInvalid("operation does not support this scope")
	}
	h.operationStatusLocked(p.Channel, p.OperationId, t.ChangesetOperationStatusRunning, nil)
	h.mu.Unlock()
	action := p.OperationId
	if action == "discard-changes" {
		action = "revert"
	}
	ctx, cancel := context.WithTimeout(h.server.ctx, 30*time.Second)
	defer cancel()
	_, err := h.server.runGit(ctx, gitAction{Action: action, Paths: paths})
	for _, channel := range []string{ahpWorkspaceChanges, ahpStagedChanges, ahpUnstagedChanges} {
		h.mu.Lock()
		exists := h.changesets[channel] != nil
		h.mu.Unlock()
		if exists {
			h.refreshChangesetLocked(channel, false)
		}
	}
	h.mu.Lock()
	status := t.ChangesetOperationStatusIdle
	if err != nil {
		status = t.ChangesetOperationStatusError
	}
	h.operationStatusLocked(p.Channel, p.OperationId, status, err)
	h.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", p.OperationId, err)
	}
	return t.InvokeChangesetOperationResult{}, nil
}

// Editing is advisory UI state. It never grants permission to overwrite a file.
// Only report known file-edit tools with real locations; shell commands can
// change arbitrary files and cannot reliably be attributed to individual paths.
func (h *ahpHost) editingFilesLocked() map[string]bool {
	files := map[string]bool{}
	for _, ch := range h.chats {
		if ch.chat.ActiveTurn == nil {
			continue
		}
		for _, part := range ch.chat.ActiveTurn.ResponseParts {
			tool, ok := part.Value.(*t.ToolCallResponsePart)
			if !ok {
				continue
			}
			call, ok := tool.ToolCall.Value.(*t.ToolCallRunningState)
			if !ok || string(call.Meta["toolKind"]) != `"edit"` {
				continue
			}
			for _, content := range call.Content {
				resource, ok := content.Value.(*t.ToolResultResourceContent)
				if !ok {
					continue
				}
				u, err := url.Parse(string(resource.Uri))
				if err != nil || u.Scheme != "file" {
					continue
				}
				path, err := filepath.Rel(h.server.workspace.RootPath, filepath.FromSlash(u.Path))
				if err != nil {
					continue
				}
				if filepath.IsLocal(path) {
					files[filepath.Clean(path)] = true
				}
			}
		}
	}
	return files
}
