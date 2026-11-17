package server

import (
	"context"
	"path"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/diff"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// A session list shows what each session does and whether it holds anything
// new: an unread session ran a turn no client has looked at since. The host
// keeps a chat unread from the start or end of a turn until a client that
// shows it marks it read, and a session is read when all its chats are.

// withRead sets or clears the read flag of a status.
func withRead(status types.SessionStatus, read bool) types.SessionStatus {
	if read {
		return status | types.SessionStatusIsRead
	}
	return status &^ types.SessionStatusIsRead
}

// markRead sets a chat's read flag where it differs. Caller holds host.mu.
func (a *Chat) markRead(read bool) {
	if a.chat.Status.Has(types.SessionStatusIsRead) != read {
		a.chatAction(types.StateAction{Value: &types.ChatIsReadChangedAction{Type: types.ActionTypeChatIsReadChanged, IsRead: read}})
	}
}

// chatsRead reports whether a client has seen every chat's latest turn. Caller holds host.mu.
func (a *Session) chatsRead() bool {
	for _, ch := range a.chats {
		if !ch.chat.Status.Has(types.SessionStatusIsRead) {
			return false
		}
	}
	return true
}

// syncUnread marks the session unread once one of its chats is. Reading is a
// client's to report, so a session only becomes read through its actions.
// Caller holds host.mu.
func (a *Session) syncUnread() {
	if a.state.Status.Has(types.SessionStatusIsRead) && !a.chatsRead() {
		a.sessionAction(types.StateAction{Value: &types.SessionIsReadChangedAction{Type: types.ActionTypeSessionIsReadChanged, IsRead: false}})
	}
}

// activityLimit bounds the activity summaries carry: lists show one line of it.
const activityLimit = 120

// syncActivity tells the session's subscribers what it does now. Caller holds host.mu.
func (a *Session) syncActivity() {
	next := a.currentActivity()
	current := ""
	if a.state.Activity != nil {
		current = *a.state.Activity
	}
	if next == current {
		return
	}
	var value *string
	if next != "" {
		value = &next
	}
	a.sessionAction(types.StateAction{Value: &types.SessionActivityChangedAction{Type: types.ActionTypeSessionActivityChanged, Activity: value}})
}

// currentActivity says what the session does, for lists: the approval or
// question it waits for, then a running chat's retry, compaction or tool
// progress, then the tool its latest step runs. An idle session does nothing.
// Caller holds host.mu.
func (a *Session) currentActivity() string {
	for _, request := range a.state.InputNeeded {
		switch r := request.Value.(type) {
		case *types.SessionToolConfirmationRequest:
			if call, ok := r.ToolCall.Value.(*types.ToolCallPendingConfirmationState); ok {
				return clip(firstLine(call.InvocationMessage.AsText()), "Waiting for approval")
			}
		case *types.SessionChatInputRequest:
			message := ""
			if r.Request.Message != nil {
				message = *r.Request.Message
			}
			return clip(firstLine(message), "Waiting for an answer")
		}
	}
	for _, ch := range a.chats {
		turn := ch.chat.ActiveTurn
		if turn == nil {
			continue
		}
		if ch.chat.Activity != nil && *ch.chat.Activity != "" {
			return clip(*ch.chat.Activity, "")
		}
		if text := turnActivity(turn); text != "" {
			return clip(text, "")
		}
	}
	return ""
}

// turnActivity names the tool a turn's latest step runs and what it works on.
func turnActivity(turn *types.ActiveTurn) string {
	parts := turn.ResponseParts
	if len(parts) == 0 {
		return ""
	}
	part, ok := parts[len(parts)-1].Value.(*types.ToolCallResponsePart)
	if !ok {
		return ""
	}
	switch call := part.ToolCall.Value.(type) {
	case *types.ToolCallStreamingState:
		return describeTool(call.DisplayName, call.Intention, nil)
	case *types.ToolCallRunningState:
		return describeTool(call.DisplayName, call.Intention, call.Content)
	}
	return ""
}

// describeTool is a tool's name with its intention, or else the file it works on.
func describeTool(name string, intention *string, content []types.ToolResultContent) string {
	detail := ""
	if intention != nil {
		detail = firstLine(*intention)
	}
	for _, item := range content {
		if detail != "" {
			break
		}
		if resource, ok := item.Value.(*types.ToolResultResourceContent); ok {
			uri, _, _ := strings.Cut(resource.Uri, "#")
			if file, ok := fileuri.Path(uri); ok {
				detail = path.Base(strings.ReplaceAll(file, "\\", "/"))
			}
		}
	}
	if detail == "" {
		return name
	}
	return name + ": " + detail
}

func firstLine(text string) string {
	for line := range strings.Lines(text) {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// clip shortens text to the activity limit; empty text is the fallback.
func clip(text, fallback string) string {
	if text == "" {
		return fallback
	}
	if utf8.RuneCountInString(text) <= activityLimit {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:activityLimit-1])) + "…"
}

// changesOf sums what a chat's turns changed, net per file, leaving out the
// turns that were undone. A file edited back to how it was counts as unchanged.
func changesOf(reviews []harness.TurnReview) *types.ChangesSummary {
	var edits []tool.FileChange
	for _, review := range reviews {
		if !review.Undone {
			edits = append(edits, review.Files...)
		}
	}
	net, _ := tool.NetFileChanges(edits)
	var additions, deletions, files int64
	for _, file := range net {
		if file.BeforeExists == file.AfterExists && file.Before == file.After && file.ResultMode() == file.Mode {
			continue
		}
		added, removed := diff.LineStats(file.Before, file.After, time.Second)
		additions += int64(added)
		deletions += int64(removed)
		files++
	}
	return &types.ChangesSummary{Additions: &additions, Deletions: &deletions, Files: &files}
}

// refreshChangesLocked recounts in the background the files the chat's native
// session changed. Only agents that record their edits can say. A refresh
// requested while one runs runs again after it. Caller holds host.mu.
func (a *Chat) refreshChangesLocked() {
	if _, ok := a.controller.backend.agent.(turnReviewer); !ok || !a.project.availableLocked() {
		return
	}
	if a.changesRunning {
		a.changesStale = true
		return
	}
	a.changesRunning = true
	go a.countChanges()
}

func (a *Chat) countChanges() {
	h := a.project.host
	for {
		changes, counted := a.countChangesOnce()
		h.mu.Lock()
		if counted && a.project.availableLocked() && h.chats[a.chat.Resource] == a && !reflect.DeepEqual(a.chat.Changes, changes) {
			previous := a.session.summary()
			// No chat action carries changes: subscribers learn them from the session.
			a.chat.Changes = changes
			a.session.syncChats(previous)
		}
		again := a.changesStale && a.project.availableLocked()
		a.changesStale = false
		a.changesRunning = again
		h.mu.Unlock()
		a.project.saveAliases()
		if !again {
			return
		}
	}
}

// countChangesOnce reads the chat's turns as one admitted backend operation.
func (a *Chat) countChangesOnce() (*types.ChangesSummary, bool) {
	b := a.controller.backend
	reviewer, ok := b.agent.(turnReviewer)
	if !ok || !b.Project.beginOperation() {
		return nil, false
	}
	defer b.Project.operations.Done()
	ctx, cancel := context.WithTimeout(b.Project.ctx, time.Minute)
	defer cancel()
	reviews, err := reviewer.TurnReviews(ctx, a.controller.ref.SessionID)
	if err != nil {
		return nil, false
	}
	return changesOf(reviews), true
}

// refreshChanges recounts a chat's changes after an operation outside its turns, such as an undo.
func (c *sessionController) refreshChanges() {
	c.mu.Lock()
	ch := c.chat
	c.mu.Unlock()
	if ch == nil {
		return
	}
	ch.project.host.mu.Lock()
	defer ch.project.host.mu.Unlock()
	if ch.project.host.chats[ch.chat.Resource] == ch {
		ch.refreshChangesLocked()
	}
}
