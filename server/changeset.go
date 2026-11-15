package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/changes"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

const changesetContentLimit = 128 << 20

// changesetLayers are the workspace changes every project publishes.
var changesetLayers = []changes.DiffLayer{changes.DiffCombined, changes.DiffStaged, changes.DiffUnstaged}

func (p *Project) changesetPrefix() string {
	return "ahp-changeset:/" + p.namespace + "/"
}

// changesChannel names the channel of a layer of this project's changes.
func (p *Project) changesChannel(layer changes.DiffLayer) string {
	name := "uncommitted"
	switch layer {
	case changes.DiffStaged:
		name = "staged"
	case changes.DiffUnstaged:
		name = "unstaged"
	}
	return p.changesetPrefix() + name
}

type changeset struct {
	state       types.ChangesetState
	contents    map[string]string
	touched     time.Time
	comparison  bool
	fingerprint uint64
}

func (p *Project) changesetLayer(channel string) (changes.DiffLayer, bool) {
	for _, layer := range changesetLayers {
		if channel == p.changesChannel(layer) {
			return layer, true
		}
	}
	return "", false
}

func (p *Project) changesetCatalogue() []types.Changeset {
	if p.workspace == nil {
		return nil
	}
	return []types.Changeset{
		{Label: "Uncommitted Changes", UriTemplate: p.changesChannel(changes.DiffCombined), ChangeKind: "uncommitted", Description: new("Current workspace changes, shared by all sessions")},
		{Label: "Staged Changes", UriTemplate: p.changesChannel(changes.DiffStaged), ChangeKind: "staged"},
		{Label: "Unstaged Changes", UriTemplate: p.changesChannel(changes.DiffUnstaged), ChangeKind: "unstaged"},
	}
}

func (p *Project) ensureChangeset(channel string) error {
	if p.workspace == nil {
		return ahp.InvalidParams("workspace unavailable")
	}
	p.changesMu.Lock()
	defer p.changesMu.Unlock()
	if _, ok := p.changesetLayer(channel); !ok {
		p.host.mu.Lock()
		defer p.host.mu.Unlock()
		if c := p.changesets[channel]; c != nil {
			c.touched = time.Now()
			return nil
		}
		return &ahp.Error{Code: int(types.ErrorCodeNotFound), Message: "changeset is no longer available; compare revisions again"}
	}
	p.refreshChangesetLocked(channel, false)
	p.changesOnce.Do(func() {
		p.background.Go(func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-p.ctx.Done():
					return
				case <-ticker.C:
					p.refreshChangesets()
				}
			}
		})
	})
	return nil
}

// followedLocked reports whether a client subscribes to a channel. Caller holds host.mu.
func (p *Project) followedLocked(channel string) bool {
	for client := range p.host.clients {
		if client.subscriptions[channel] {
			return true
		}
	}
	return false
}

func (p *Project) refreshChangesets() {
	p.changesMu.Lock()
	defer p.changesMu.Unlock()
	for _, layer := range changesetLayers {
		channel := p.changesChannel(layer)
		p.host.mu.Lock()
		active := p.followedLocked(channel)
		p.host.mu.Unlock()
		if active {
			p.refreshChangesetLocked(channel, false)
		}
	}
}

func (p *Project) buildChangeset(diffs []changes.FileDiff, editing map[string]bool) (*changeset, error) {
	next := &changeset{state: types.ChangesetState{Status: types.ChangesetStatusReady, Files: []types.ChangesetFile{}}, contents: map[string]string{}, touched: time.Now()}
	bytes := 0
	contentRef := func(text, contentType string) types.ContentRef {
		hash := sha256.Sum256([]byte(text))
		id := hex.EncodeToString(hash[:])
		uri := "ahp-content:/" + id
		if _, exists := next.contents[uri]; !exists {
			bytes += len(text)
		}
		next.contents[uri] = text
		return types.ContentRef{Uri: uri, Nonce: new(id), SizeHint: new(int64(len(text))), ContentType: new(contentType)}
	}
	for _, diff := range diffs {
		edit := types.FileEdit{}
		contentType := "text/plain"
		if diff.Binary {
			contentType = "application/octet-stream"
		}
		if diff.Status != changes.StatusAdded {
			original := diff.OriginalPath
			if original == "" {
				original = diff.Path
			}
			edit.Before = &types.FileEditSide{Uri: p.fileURI(original), Content: contentRef(diff.Original, contentType)}
		}
		if diff.Status != changes.StatusDeleted {
			edit.After = &types.FileEditSide{Uri: p.fileURI(diff.Path), Content: contentRef(diff.Modified, contentType)}
		}
		var added, removed int64
		for line := range strings.SplitSeq(diff.Patch, "\n") {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				added++
			}
			if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				removed++
			}
		}
		edit.Diff = &types.FileEditDiffStats{Added: &added, Removed: &removed}
		// The patch also covers changes the contents cannot show, such as modes.
		identity, _ := json.Marshal(struct {
			Edit  types.FileEdit
			Patch string
		}{edit, diff.Patch})
		nonce := sha256.Sum256(identity)
		next.state.Files = append(next.state.Files, types.ChangesetFile{
			Id: p.fileURI(diff.Path), Edit: edit, Nonce: new(hex.EncodeToString(nonce[:])),
			Status: changesetFileStatus(editing[filepath.Clean(diff.Path)]),
		})
		if bytes > 64<<20 {
			return nil, fmt.Errorf("changeset exceeds the 64 MiB content limit")
		}
	}
	return next, nil
}

func changesetFileStatus(editing bool) *types.ChangesetFileStatus {
	var status types.ChangesetFileStatus
	if editing {
		status = types.ChangesetFileStatusEditing
	}
	return &status
}

func changesetOperations(layer changes.DiffLayer, isGit bool, hasFiles bool) []types.ChangesetOperation {
	if !hasFiles {
		return nil
	}
	ops := []types.ChangesetOperation{}
	if isGit && layer == changes.DiffUnstaged {
		ops = append(ops, types.ChangesetOperation{Id: "stage", Label: "Stage", Scopes: []types.ChangesetOperationScope{types.ChangesetOperationScopeChangeset, types.ChangesetOperationScopeResource}, Status: types.ChangesetOperationStatusIdle})
	}
	if isGit && layer == changes.DiffStaged {
		ops = append(ops, types.ChangesetOperation{Id: "unstage", Label: "Unstage", Scopes: []types.ChangesetOperationScope{types.ChangesetOperationScopeChangeset, types.ChangesetOperationScopeResource}, Status: types.ChangesetOperationStatusIdle})
	}
	if layer == changes.DiffUnstaged {
		confirmation := types.StringOrMarkdown{Plain: new("Discard this file's working changes? This cannot be undone in Wingman.")}
		ops = append(ops, types.ChangesetOperation{Id: "discard-changes", Label: "Discard changes", Confirmation: &confirmation,
			Scopes: []types.ChangesetOperationScope{types.ChangesetOperationScopeResource}, Status: types.ChangesetOperationStatusIdle})
	}
	return ops
}

func (p *Project) refreshChangesetLocked(channel string, force bool) {
	layer, _ := p.changesetLayer(channel)
	p.host.mu.Lock()
	editing := p.editingFilesLocked()
	var previous *changeset
	if cached := p.changesets[channel]; cached != nil {
		copy := *cached
		previous = &copy
	}
	fingerprint := uint64(0)
	if !force && previous != nil && previous.state.Status == types.ChangesetStatusReady {
		fingerprint = previous.fingerprint
	}
	p.host.mu.Unlock()
	diffs, current, err := p.workspace.DiffsLayerIfChanged(p.ctx, layer, fingerprint)
	var next *changeset
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
		next.state.Operations = changesetOperations(layer, p.workspace.IsGitRepo(), len(next.state.Files) > 0)
	}
	p.host.mu.Lock()
	defer p.host.mu.Unlock()
	if p.changesets == nil {
		p.changesets = map[string]*changeset{}
	}
	for err == nil && !p.changesetFitsLocked(channel, next) {
		if !p.evictComparisonLocked(channel) {
			err = fmt.Errorf("retained changesets exceed the 128 MiB content limit")
		}
	}
	if err != nil {
		next = &changeset{state: types.ChangesetState{Status: types.ChangesetStatusError, Files: []types.ChangesetFile{}, Error: &types.ErrorInfo{ErrorType: "workspace", Message: err.Error()}}, touched: time.Now()}
		if previous := p.changesets[channel]; previous != nil {
			next.state.Files = previous.state.Files
			next.contents = previous.contents
		}
	}
	p.publishChangesetLocked(channel, next)
}

func (p *Project) changesetFitsLocked(channel string, next *changeset) bool {
	content := map[string]string{}
	for uri, c := range p.changesets {
		if uri != channel {
			maps.Copy(content, c.contents)
		}
	}
	maps.Copy(content, next.contents)
	bytes := 0
	for _, value := range content {
		bytes += len(value)
		if bytes > changesetContentLimit {
			return false
		}
	}
	return true
}

func (p *Project) comparisonChangeset(diffs []changes.FileDiff) (string, error) {
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
	p.host.mu.Lock()
	defer p.host.mu.Unlock()
	if p.changesets == nil {
		p.changesets = map[string]*changeset{}
	}
	comparisons := 0
	for _, c := range p.changesets {
		if c.comparison {
			comparisons++
		}
	}
	for (p.changesets[channel] == nil && comparisons >= 32) || !p.changesetFitsLocked(channel, next) {
		if !p.evictComparisonLocked(channel) {
			return "", &ahp.Error{Code: -32009, Message: "too many retained comparisons; close comparisons and retry"}
		}
		comparisons--
	}
	p.changesets[channel] = next
	return channel, nil
}

// evictComparisonLocked drops the least recently used idle comparison other
// than keep. It reports whether one was dropped.
func (p *Project) evictComparisonLocked(keep string) bool {
	oldest := ""
	for uri, c := range p.changesets {
		if uri != keep && c.comparison && !p.followedLocked(uri) && time.Since(c.touched) > time.Minute &&
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

func (p *Project) retains(uri string) bool {
	p.host.mu.Lock()
	defer p.host.mu.Unlock()
	_, ok := p.changesetContentLocked(uri)
	return ok
}

func (p *Project) changesetContentLocked(uri string) (string, bool) {
	for _, c := range p.changesets {
		if content, ok := c.contents[uri]; ok {
			c.touched = time.Now()
			return content, true
		}
	}
	return "", false
}

func (p *Project) publishChangesetLocked(channel string, next *changeset) {
	previous := p.changesets[channel]
	p.changesets[channel] = next
	if previous == nil {
		return
	}
	if previous.state.Status != next.state.Status || !reflect.DeepEqual(previous.state.Error, next.state.Error) {
		p.host.publish(channel, types.StateAction{Value: &types.ChangesetStatusChangedAction{Type: types.ActionTypeChangesetStatusChanged, Status: next.state.Status, Error: next.state.Error}}, nil, nil)
	}
	old := map[string]types.ChangesetFile{}
	for _, f := range previous.state.Files {
		old[f.Id] = f
	}
	for _, f := range next.state.Files {
		if !reflect.DeepEqual(old[f.Id], f) {
			p.host.publish(channel, types.StateAction{Value: &types.ChangesetFileSetAction{Type: types.ActionTypeChangesetFileSet, File: f}}, nil, nil)
		}
		delete(old, f.Id)
	}
	for id := range old {
		p.host.publish(channel, types.StateAction{Value: &types.ChangesetFileRemovedAction{Type: types.ActionTypeChangesetFileRemoved, FileId: id}}, nil, nil)
	}
	if !reflect.DeepEqual(previous.state.Operations, next.state.Operations) {
		p.host.publish(channel, types.StateAction{Value: &types.ChangesetOperationsChangedAction{Type: types.ActionTypeChangesetOperationsChanged, Operations: next.state.Operations}}, nil, nil)
	}
}

func (p *Project) operationStatusLocked(channel, id string, status types.ChangesetOperationStatus, err error) {
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
			op.Error = &types.ErrorInfo{ErrorType: "git", Message: err.Error()}
		}
		p.host.publish(channel, types.StateAction{Value: &types.ChangesetOperationStatusChangedAction{Type: types.ActionTypeChangesetOperationStatusChanged, OperationId: id, Status: status, Error: op.Error}}, nil, nil)
	}
}

func (p *Project) invokeChangesetOperation(data json.RawMessage) (any, error) {
	var params types.InvokeChangesetOperationParams
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
		return nil, ahp.InvalidParams("changeset has no operations")
	}
	// Refresh and validate nonces before admitting a mutation; serialize
	// operations across clients so two stale previews cannot both be accepted.
	p.changesMu.Lock()
	defer p.changesMu.Unlock()
	p.refreshChangesetLocked(params.Channel, true)
	p.host.mu.Lock()
	c := p.changesets[params.Channel]
	operation := slices.IndexFunc(c.state.Operations, func(op types.ChangesetOperation) bool { return op.Id == params.OperationId })
	if operation < 0 || c.state.Status != types.ChangesetStatusReady {
		p.host.mu.Unlock()
		return nil, ahp.InvalidParams("operation is not available on this changeset")
	}
	op := c.state.Operations[operation]
	scope := types.ChangesetOperationScopeChangeset
	paths := []string{}
	if params.Target != nil {
		target, ok := params.Target.Value.(*types.ChangesetOperationResourceTarget)
		if !ok {
			p.host.mu.Unlock()
			return nil, ahp.InvalidParams("range operations are not supported")
		}
		if target.Side != nil && *target.Side != "before" && *target.Side != "after" {
			p.host.mu.Unlock()
			return nil, ahp.InvalidParams("invalid target side")
		}
		scope = types.ChangesetOperationScopeResource
		found := false
		for _, file := range c.state.Files {
			if file.Id != target.Resource {
				continue
			}
			found = true
			if target.Side != nil && (*target.Side == "before" && file.Edit.Before == nil || *target.Side == "after" && file.Edit.After == nil) {
				p.host.mu.Unlock()
				return nil, ahp.InvalidParams("target side is not present in this change")
			}
			if target.Nonce != nil && file.Nonce != nil && *target.Nonce != *file.Nonce {
				p.host.mu.Unlock()
				return nil, &ahp.Error{Code: -32011, Message: "file changed since the displayed diff; refresh before retrying"}
			}
			path, err := p.resourcePath(file.Id)
			if err != nil {
				p.host.mu.Unlock()
				return nil, err
			}
			paths = append(paths, path)
		}
		if !found {
			p.host.mu.Unlock()
			return nil, ahp.InvalidParams("file is not in this changeset")
		}
	} else {
		for _, file := range c.state.Files {
			path, err := p.resourcePath(file.Id)
			if err != nil {
				p.host.mu.Unlock()
				return nil, err
			}
			paths = append(paths, path)
		}
	}
	if !slices.Contains(op.Scopes, scope) {
		p.host.mu.Unlock()
		return nil, ahp.InvalidParams("operation does not support this scope")
	}
	p.operationStatusLocked(params.Channel, params.OperationId, types.ChangesetOperationStatusRunning, nil)
	p.host.mu.Unlock()
	action := params.OperationId
	if action == "discard-changes" {
		action = "revert"
	}
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()
	_, err := p.runGit(ctx, gitAction{Action: action, Paths: paths})
	for _, layer := range changesetLayers {
		channel := p.changesChannel(layer)
		p.host.mu.Lock()
		exists := p.changesets[channel] != nil
		p.host.mu.Unlock()
		if exists {
			p.refreshChangesetLocked(channel, false)
		}
	}
	p.host.mu.Lock()
	status := types.ChangesetOperationStatusIdle
	if err != nil {
		status = types.ChangesetOperationStatusError
	}
	p.operationStatusLocked(params.Channel, params.OperationId, status, err)
	p.host.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", params.OperationId, err)
	}
	return types.InvokeChangesetOperationResult{}, nil
}

// Editing is advisory UI state. It never grants permission to overwrite a file.
// Only report known file-edit tools with real locations; shell commands can
// change arbitrary files and cannot reliably be attributed to individual paths.
func (p *Project) editingFilesLocked() map[string]bool {
	files := map[string]bool{}
	for _, ch := range p.host.chats {
		if ch.chat.ActiveTurn == nil {
			continue
		}
		for _, part := range ch.chat.ActiveTurn.ResponseParts {
			tool, ok := part.Value.(*types.ToolCallResponsePart)
			if !ok {
				continue
			}
			call, ok := tool.ToolCall.Value.(*types.ToolCallRunningState)
			if !ok || string(call.Meta["toolKind"]) != `"edit"` {
				continue
			}
			for _, content := range call.Content {
				resource, ok := content.Value.(*types.ToolResultResourceContent)
				if !ok {
					continue
				}
				native, ok := fileuri.Path(string(resource.Uri))
				if !ok {
					continue
				}
				path, err := filepath.Rel(p.workspace.RootPath, native)
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
