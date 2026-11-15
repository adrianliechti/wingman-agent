package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/changes"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	t "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

const ahpChangesetContentLimit = 128 << 20

// changesetLayers are the workspace changes every project publishes.
var changesetLayers = []changes.DiffLayer{changes.DiffCombined, changes.DiffStaged, changes.DiffUnstaged}

func (p *ahpProject) changesetPrefix() string {
	return "ahp-changeset:/" + p.namespace + "/"
}

// changesChannel names the channel of a layer of this project's changes.
func (p *ahpProject) changesChannel(layer changes.DiffLayer) string {
	name := "uncommitted"
	switch layer {
	case changes.DiffStaged:
		name = "staged"
	case changes.DiffUnstaged:
		name = "unstaged"
	}
	return p.changesetPrefix() + name
}

type ahpChangeset struct {
	state       t.ChangesetState
	contents    map[string]string
	touched     time.Time
	comparison  bool
	fingerprint uint64
}

func (p *ahpProject) changesetLayer(channel string) (changes.DiffLayer, bool) {
	for _, layer := range changesetLayers {
		if channel == p.changesChannel(layer) {
			return layer, true
		}
	}
	return "", false
}

func (p *ahpProject) changesetCatalogue() []t.Changeset {
	if p.server.workspace == nil {
		return nil
	}
	return []t.Changeset{
		{Label: "Uncommitted Changes", UriTemplate: p.changesChannel(changes.DiffCombined), ChangeKind: "uncommitted", Description: ahpPtr("Current workspace changes, shared by all sessions")},
		{Label: "Staged Changes", UriTemplate: p.changesChannel(changes.DiffStaged), ChangeKind: "staged"},
		{Label: "Unstaged Changes", UriTemplate: p.changesChannel(changes.DiffUnstaged), ChangeKind: "unstaged"},
	}
}

func (p *ahpProject) ensureChangeset(channel string) error {
	if p.server.workspace == nil {
		return ahpInvalid("workspace unavailable")
	}
	p.changesMu.Lock()
	defer p.changesMu.Unlock()
	if _, ok := p.changesetLayer(channel); !ok {
		p.mu.Lock()
		defer p.mu.Unlock()
		if c := p.changesets[channel]; c != nil {
			c.touched = time.Now()
			return nil
		}
		return &ahpRPCError{Code: int(t.ErrorCodeNotFound), Message: "changeset is no longer available; compare revisions again"}
	}
	p.refreshChangesetLocked(channel, false)
	p.changesOnce.Do(func() {
		p.server.background.Go(func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-p.done:
					return
				case <-ticker.C:
					p.refreshChangesets()
				}
			}
		})
	})
	return nil
}

func (p *ahpProject) subscribedChangesetLocked(channel string) bool {
	for client := range p.clients {
		if client.subscriptions[channel] {
			return true
		}
	}
	return false
}

func (p *ahpProject) refreshChangesets() {
	p.changesMu.Lock()
	defer p.changesMu.Unlock()
	for _, layer := range changesetLayers {
		channel := p.changesChannel(layer)
		p.mu.Lock()
		active := p.subscribedChangesetLocked(channel)
		p.mu.Unlock()
		if active {
			p.refreshChangesetLocked(channel, false)
		}
	}
}

func (p *ahpProject) buildChangeset(diffs []changes.FileDiff, editing map[string]bool) (*ahpChangeset, error) {
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
			edit.Before = &t.FileEditSide{Uri: p.fileURI(original), Content: contentRef(diff.Original, contentType)}
		}
		if diff.Status != changes.StatusDeleted {
			edit.After = &t.FileEditSide{Uri: p.fileURI(diff.Path), Content: contentRef(diff.Modified, contentType)}
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
			Id: p.fileURI(diff.Path), Edit: edit, Nonce: ahpPtr(hex.EncodeToString(nonce[:])),
			Status: changesetFileStatus(editing[filepath.Clean(diff.Path)]),
		})
		if bytes > 64<<20 {
			return nil, fmt.Errorf("changeset exceeds the 64 MiB content limit")
		}
	}
	return next, nil
}

func changesetFileStatus(editing bool) *t.ChangesetFileStatus {
	var status t.ChangesetFileStatus
	if editing {
		status = t.ChangesetFileStatusEditing
	}
	return &status
}

func changesetOperations(layer changes.DiffLayer, isGit bool, hasFiles bool) []t.ChangesetOperation {
	if !hasFiles {
		return nil
	}
	ops := []t.ChangesetOperation{}
	if isGit && layer == changes.DiffUnstaged {
		ops = append(ops, t.ChangesetOperation{Id: "stage", Label: "Stage", Scopes: []t.ChangesetOperationScope{t.ChangesetOperationScopeChangeset, t.ChangesetOperationScopeResource}, Status: t.ChangesetOperationStatusIdle})
	}
	if isGit && layer == changes.DiffStaged {
		ops = append(ops, t.ChangesetOperation{Id: "unstage", Label: "Unstage", Scopes: []t.ChangesetOperationScope{t.ChangesetOperationScopeChangeset, t.ChangesetOperationScopeResource}, Status: t.ChangesetOperationStatusIdle})
	}
	if layer == changes.DiffUnstaged {
		confirmation := t.StringOrMarkdown{Plain: ahpPtr("Discard this file's working changes? This cannot be undone in Wingman.")}
		ops = append(ops, t.ChangesetOperation{Id: "discard-changes", Label: "Discard changes", Confirmation: &confirmation,
			Scopes: []t.ChangesetOperationScope{t.ChangesetOperationScopeResource}, Status: t.ChangesetOperationStatusIdle})
	}
	return ops
}

func (p *ahpProject) refreshChangesetLocked(channel string, force bool) {
	layer, _ := p.changesetLayer(channel)
	p.mu.Lock()
	editing := p.editingFilesLocked()
	var previous *ahpChangeset
	if cached := p.changesets[channel]; cached != nil {
		copy := *cached
		previous = &copy
	}
	fingerprint := uint64(0)
	if !force && previous != nil && previous.state.Status == t.ChangesetStatusReady {
		fingerprint = previous.fingerprint
	}
	p.mu.Unlock()
	diffs, current, err := p.server.workspace.DiffsLayerIfChanged(p.server.ctx, layer, fingerprint)
	var next *ahpChangeset
	if err == nil {
		if fingerprint != 0 && current == fingerprint {
			copy := *previous
			copy.state.Files = slices.Clone(previous.state.Files)
			for i := range copy.state.Files {
				path, _ := p.resourcePath(copy.state.Files[i].Id)
				copy.state.Files[i].Status = changesetFileStatus(editing[filepath.Clean(path)])
			}
			next = &copy
		} else {
			next, err = p.buildChangeset(diffs, editing)
		}
		if err == nil {
			next.fingerprint = current
		}
	}
	if err == nil {
		next.state.Operations = changesetOperations(layer, p.server.workspace.IsGitRepo(), len(next.state.Files) > 0)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.changesets == nil {
		p.changesets = map[string]*ahpChangeset{}
	}
	for err == nil && !p.changesetFitsLocked(channel, next) {
		if !p.evictComparisonLocked(channel) {
			err = fmt.Errorf("retained changesets exceed the 128 MiB content limit")
		}
	}
	if err != nil {
		next = &ahpChangeset{state: t.ChangesetState{Status: t.ChangesetStatusError, Files: []t.ChangesetFile{}, Error: &t.ErrorInfo{ErrorType: "workspace", Message: err.Error()}}, touched: time.Now()}
		if previous := p.changesets[channel]; previous != nil {
			next.state.Files = previous.state.Files
			next.contents = previous.contents
		}
	}
	p.publishChangesetLocked(channel, next)
}

func (p *ahpProject) changesetFitsLocked(channel string, next *ahpChangeset) bool {
	content := map[string]string{}
	for uri, c := range p.changesets {
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

func (p *ahpProject) comparisonChangeset(diffs []changes.FileDiff) (string, error) {
	next, err := p.buildChangeset(diffs, nil)
	if err != nil {
		return "", err
	}
	next.comparison = true
	identity, _ := json.Marshal(next.state.Files)
	hash := sha256.Sum256(identity)
	channel := p.changesetPrefix() + "compare/" + hex.EncodeToString(hash[:])
	p.changesMu.Lock()
	defer p.changesMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.changesets == nil {
		p.changesets = map[string]*ahpChangeset{}
	}
	comparisons := 0
	for _, c := range p.changesets {
		if c.comparison {
			comparisons++
		}
	}
	for (p.changesets[channel] == nil && comparisons >= 32) || !p.changesetFitsLocked(channel, next) {
		if !p.evictComparisonLocked(channel) {
			return "", &ahpRPCError{Code: -32009, Message: "too many retained comparisons; close comparisons and retry"}
		}
		comparisons--
	}
	p.changesets[channel] = next
	return channel, nil
}

// evictComparisonLocked drops the least recently used idle comparison other
// than keep. It reports whether one was dropped.
func (p *ahpProject) evictComparisonLocked(keep string) bool {
	oldest := ""
	for uri, c := range p.changesets {
		if uri != keep && c.comparison && !p.subscribedChangesetLocked(uri) && time.Since(c.touched) > time.Minute &&
			(oldest == "" || c.touched.Before(p.changesets[oldest].touched)) {
			oldest = uri
		}
	}
	if oldest == "" {
		return false
	}
	delete(p.changesets, oldest)
	return true
}

func (p *ahpProject) retains(uri string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.changesetContentLocked(uri)
	return ok
}

func (p *ahpProject) changesetContentLocked(uri string) (string, bool) {
	for _, c := range p.changesets {
		if content, ok := c.contents[uri]; ok {
			c.touched = time.Now()
			return content, true
		}
	}
	return "", false
}

func (p *ahpProject) publishChangesetLocked(channel string, next *ahpChangeset) {
	previous := p.changesets[channel]
	p.changesets[channel] = next
	if previous == nil {
		return
	}
	if previous.state.Status != next.state.Status || !reflect.DeepEqual(previous.state.Error, next.state.Error) {
		p.publish(channel, t.StateAction{Value: &t.ChangesetStatusChangedAction{Type: t.ActionTypeChangesetStatusChanged, Status: next.state.Status, Error: next.state.Error}}, nil, nil)
	}
	old := map[string]t.ChangesetFile{}
	for _, f := range previous.state.Files {
		old[f.Id] = f
	}
	for _, f := range next.state.Files {
		if !reflect.DeepEqual(old[f.Id], f) {
			p.publish(channel, t.StateAction{Value: &t.ChangesetFileSetAction{Type: t.ActionTypeChangesetFileSet, File: f}}, nil, nil)
		}
		delete(old, f.Id)
	}
	for id := range old {
		p.publish(channel, t.StateAction{Value: &t.ChangesetFileRemovedAction{Type: t.ActionTypeChangesetFileRemoved, FileId: id}}, nil, nil)
	}
	if !reflect.DeepEqual(previous.state.Operations, next.state.Operations) {
		p.publish(channel, t.StateAction{Value: &t.ChangesetOperationsChangedAction{Type: t.ActionTypeChangesetOperationsChanged, Operations: next.state.Operations}}, nil, nil)
	}
}

func (p *ahpProject) operationStatusLocked(channel, id string, status t.ChangesetOperationStatus, err error) {
	c := p.changesets[channel]
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
		p.publish(channel, t.StateAction{Value: &t.ChangesetOperationStatusChangedAction{Type: t.ActionTypeChangesetOperationStatusChanged, OperationId: id, Status: status, Error: op.Error}}, nil, nil)
	}
}

func (p *ahpProject) invokeChangesetOperation(data json.RawMessage) (any, error) {
	var params t.InvokeChangesetOperationParams
	if err := decodeGitParams(data, &params); err != nil {
		return nil, err
	}
	if params.Target != nil {
		// The target union decodes leniently; a misspelled nonce must not skip the stale check.
		var raw struct {
			Target json.RawMessage `json:"target"`
		}
		_ = json.Unmarshal(data, &raw)
		if err := decodeGitParams(raw.Target, params.Target.Value); err != nil {
			return nil, err
		}
	}
	if _, ok := p.changesetLayer(params.Channel); !ok {
		return nil, ahpInvalid("changeset has no operations")
	}
	// Refresh and validate nonces before admitting a mutation; serialize
	// operations across clients so two stale previews cannot both be accepted.
	p.changesMu.Lock()
	defer p.changesMu.Unlock()
	p.refreshChangesetLocked(params.Channel, true)
	p.mu.Lock()
	c := p.changesets[params.Channel]
	operation := slices.IndexFunc(c.state.Operations, func(op t.ChangesetOperation) bool { return op.Id == params.OperationId })
	if operation < 0 || c.state.Status != t.ChangesetStatusReady {
		p.mu.Unlock()
		return nil, ahpInvalid("operation is not available on this changeset")
	}
	op := c.state.Operations[operation]
	scope := t.ChangesetOperationScopeChangeset
	paths := []string{}
	if params.Target != nil {
		target, ok := params.Target.Value.(*t.ChangesetOperationResourceTarget)
		if !ok {
			p.mu.Unlock()
			return nil, ahpInvalid("range operations are not supported")
		}
		if target.Side != nil && *target.Side != "before" && *target.Side != "after" {
			p.mu.Unlock()
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
				p.mu.Unlock()
				return nil, ahpInvalid("target side is not present in this change")
			}
			if target.Nonce != nil && file.Nonce != nil && *target.Nonce != *file.Nonce {
				p.mu.Unlock()
				return nil, &ahpRPCError{Code: -32011, Message: "file changed since the displayed diff; refresh before retrying"}
			}
			path, err := p.resourcePath(file.Id)
			if err != nil {
				p.mu.Unlock()
				return nil, err
			}
			paths = append(paths, path)
		}
		if !found {
			p.mu.Unlock()
			return nil, ahpInvalid("file is not in this changeset")
		}
	} else {
		for _, file := range c.state.Files {
			path, err := p.resourcePath(file.Id)
			if err != nil {
				p.mu.Unlock()
				return nil, err
			}
			paths = append(paths, path)
		}
	}
	if !slices.Contains(op.Scopes, scope) {
		p.mu.Unlock()
		return nil, ahpInvalid("operation does not support this scope")
	}
	p.operationStatusLocked(params.Channel, params.OperationId, t.ChangesetOperationStatusRunning, nil)
	p.mu.Unlock()
	action := params.OperationId
	if action == "discard-changes" {
		action = "revert"
	}
	ctx, cancel := context.WithTimeout(p.server.ctx, 30*time.Second)
	defer cancel()
	_, err := p.server.runGit(ctx, gitAction{Action: action, Paths: paths})
	for _, layer := range changesetLayers {
		channel := p.changesChannel(layer)
		p.mu.Lock()
		exists := p.changesets[channel] != nil
		p.mu.Unlock()
		if exists {
			p.refreshChangesetLocked(channel, false)
		}
	}
	p.mu.Lock()
	status := t.ChangesetOperationStatusIdle
	if err != nil {
		status = t.ChangesetOperationStatusError
	}
	p.operationStatusLocked(params.Channel, params.OperationId, status, err)
	p.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", params.OperationId, err)
	}
	return t.InvokeChangesetOperationResult{}, nil
}

// Editing is advisory UI state. It never grants permission to overwrite a file.
// Only report known file-edit tools with real locations; shell commands can
// change arbitrary files and cannot reliably be attributed to individual paths.
func (p *ahpProject) editingFilesLocked() map[string]bool {
	files := map[string]bool{}
	for _, ch := range p.chats {
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
				native, ok := fileuri.Path(string(resource.Uri))
				if !ok {
					continue
				}
				path, err := filepath.Rel(p.server.workspace.RootPath, native)
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
