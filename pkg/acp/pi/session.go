package pi

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

type session struct {
	id      acpsdk.SessionId
	cwd     string
	proc    *process
	cleanup func()

	promptMu       acp.PromptGate
	mu             sync.Mutex
	closed         bool
	models         []modelEntry
	currentModel   string
	thinkingLevels []string
	thinking       string

	title       string
	promptTitle string
	autoTitle   bool

	cancelTurn context.CancelFunc
	closeOnce  sync.Once
}

func newSession(id acpsdk.SessionId, cwd string, proc *process) *session {
	return &session{id: id, cwd: cwd, proc: proc, thinking: defaultThinkingLevel}
}

func (s *session) configOptions() []acpsdk.SessionConfigOption {
	s.mu.Lock()
	defer s.mu.Unlock()
	return buildConfigOptions(s.models, s.currentModel, s.thinkingLevels, s.thinking)
}

func (s *session) refreshConfiguration(ctx context.Context) error {
	stateData, err := s.proc.getState(ctx)
	if err != nil {
		return err
	}
	state := parseState(stateData)

	levelsData, levelsErr := s.proc.getAvailableThinkingLevels(ctx)
	levels := parseAvailableThinkingLevels(levelsData)
	if levelsErr != nil || len(levels) == 0 {
		levels = append([]string(nil), fallbackThinkingLevels...)
	}

	s.mu.Lock()
	if current := state.currentModel(); current != "" {
		s.currentModel = current
	}
	s.thinkingLevels = levels
	s.thinking = state.thinking()
	s.mu.Unlock()
	return nil
}

// Restored titles omit updatedAt because loading a session is not activity.
func (s *session) titleUpdate(title string, activity bool) *acpsdk.SessionUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if title == "" || title == s.title {
		return nil
	}
	s.title = title
	info := &acpsdk.SessionSessionInfoUpdate{SessionUpdate: "session_info_update", Title: &title}
	if activity {
		now := time.Now().UTC().Format(time.RFC3339)
		info.UpdatedAt = &now
	}
	return &acpsdk.SessionUpdate{SessionInfoUpdate: info}
}

func (s *session) fallbackTitle() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promptTitle
}

func (s *session) restoreTitle(path string) string {
	name, prompt := readSessionTitle(path)
	s.mu.Lock()
	s.promptTitle = prompt
	s.mu.Unlock()
	if name != "" {
		return name
	}
	return prompt
}

func (s *session) nameFromPrompt(ctx context.Context, prompt []acpsdk.ContentBlock) *acpsdk.SessionUpdate {
	title := promptTitle(prompt)
	s.mu.Lock()
	pending := s.autoTitle && title != ""
	if pending {
		s.autoTitle = false
		s.promptTitle = title
	}
	s.mu.Unlock()
	if !pending {
		return nil
	}

	stateData, err := s.proc.getState(ctx)
	if err != nil {
		return nil
	}
	if name := strings.TrimSpace(parseState(stateData).SessionName); name != "" {
		return s.titleUpdate(name, true)
	}
	if err := s.proc.setSessionName(ctx, title); err != nil {
		return nil
	}
	return s.titleUpdate(title, true)
}

const contextUsageTimeout = time.Second

func (s *session) publishContextUsage(ctx context.Context, conn acp.ClientConnection) {
	if conn == nil {
		return
	}
	data, err := s.proc.getSessionStats(ctx, contextUsageTimeout)
	if err != nil {
		return
	}
	if u := parseContextUsage(data); u != nil {
		_ = acp.Notify(ctx, conn, s.id, *u)
	}
}

func parseContextUsage(data json.RawMessage) *acpsdk.SessionUpdate {
	var stats struct {
		Cost         float64 `json:"cost"`
		ContextUsage *struct {
			Tokens        *int `json:"tokens"`
			ContextWindow int  `json:"contextWindow"`
		} `json:"contextUsage"`
	}
	if json.Unmarshal(data, &stats) != nil || stats.ContextUsage == nil {
		return nil
	}
	usage := stats.ContextUsage
	// Pi reports null tokens until the first reply after a compaction.
	if usage.Tokens == nil || *usage.Tokens < 0 || usage.ContextWindow <= 0 {
		return nil
	}
	update := &acpsdk.SessionUsageUpdate{SessionUpdate: "usage_update", Used: *usage.Tokens, Size: usage.ContextWindow}
	if stats.Cost > 0 {
		update.Cost = &acpsdk.Cost{Amount: stats.Cost, Currency: "USD"}
	}
	return &acpsdk.SessionUpdate{UsageUpdate: update}
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		cancel := s.cancelTurn
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		s.proc.dispose()
		if s.cleanup != nil {
			s.cleanup()
		}
	})
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *session) cancel() {
	s.mu.Lock()
	cancel := s.cancelTurn
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

type turnResult struct {
	stop acpsdk.StopReason
	err  error
}

func (s *session) runTurn(ctx context.Context, conn acp.ClientConnection, prompt []acpsdk.ContentBlock) (acpsdk.StopReason, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return acpsdk.StopReasonCancelled, nil
	}
	s.cancelTurn = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cancelTurn = nil
		s.mu.Unlock()
	}()

	message, images := promptToPi(prompt)

	t := &turn{
		ctx:       turnCtx,
		conn:      conn,
		sess:      s,
		done:      make(chan turnResult, 1),
		settled:   make(chan struct{}),
		tools:     map[string]bool{},
		mutations: map[string]bool{},
		snapshots: map[string]fileSnapshot{},
	}

	s.proc.setHandler(t.handle)
	defer s.proc.setHandler(nil)

	if u := s.nameFromPrompt(turnCtx, prompt); u != nil {
		t.emit(*u)
	}

	go func() {
		err := s.proc.prompt(turnCtx, message, images)
		t.onPromptResult(err)
	}()

	select {
	case <-turnCtx.Done():
		abortCtx, abortCancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := s.proc.abort(abortCtx)
		abortCancel()
		if err == nil {
			select {
			case <-t.settled:
				s.publishContextUsage(ctx, conn)
				return acpsdk.StopReasonCancelled, nil
			case <-s.proc.done:
			case <-time.After(2 * time.Second):
			}
		}
		// An abort response only acknowledges the command. Without the settled
		// event, old output could otherwise complete the next prompt.
		s.close()
		return acpsdk.StopReasonCancelled, nil
	case <-s.proc.done:
		// The final settled event can precede EOF in the same read batch.
		select {
		case res := <-t.done:
			return res.stop, res.err
		default:
		}
		return "", errors.New("pi process exited unexpectedly")
	case res := <-t.done:
		if res.err == nil {
			s.publishContextUsage(ctx, conn)
		}
		return res.stop, res.err
	}
}

type turn struct {
	ctx  context.Context
	conn acp.ClientConnection
	sess *session

	done       chan turnResult
	settled    chan struct{}
	settleOnce sync.Once

	tools     map[string]bool
	mutations map[string]bool
	snapshots map[string]fileSnapshot
}

// fileSnapshot captures a file's contents before pi's edit/write tools mutate
// it, so tool completion can render a structured ACP diff — pi itself only
// reports diffs as plain strings. oldText nil means the file didn't exist yet.
type fileSnapshot struct {
	path    string
	oldText *string
}

func (t *turn) emit(u acpsdk.SessionUpdate) {
	ctx := t.ctx
	// Finish tool calls already announced in_progress; the SDK won't write on a cancelled ctx.
	if ctx.Err() != nil {
		detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		ctx = detached
	}
	_ = acp.Notify(ctx, t.conn, t.sess.id, u)
}

func (t *turn) resolve(stop acpsdk.StopReason, err error) {
	select {
	case t.done <- turnResult{stop: stop, err: err}:
	default:
	}
}

func (t *turn) onPromptResult(err error) {
	if err == nil {
		return
	}
	if t.ctx != nil && t.ctx.Err() != nil {
		return
	}
	if isAuthError(err) {
		t.resolve("", acpsdk.NewAuthRequired(nil))
		return
	}
	t.resolve("", err)
}

type piToolCall struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Arguments   json.RawMessage `json:"arguments"`
	PartialArgs string          `json:"partialArgs"`
}

func (t *turn) handle(raw json.RawMessage) {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return
	}

	switch probe.Type {
	case "message_start":
		var event struct {
			Message struct {
				Role      string `json:"role"`
				Timestamp int64  `json:"timestamp"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &event) == nil && event.Message.Role == "user" {
			acp.PromptAccepted(t.ctx, piMessageID(event.Message.Role, event.Message.Timestamp))
		}
	case "message_update":
		t.handleMessageUpdate(raw)

	case "tool_execution_start":
		t.handleToolStart(raw)

	case "tool_execution_update":
		t.handleToolUpdate(raw)

	case "tool_execution_end":
		t.handleToolEnd(raw)

	case "extension_ui_request":
		// The backend can send abort responses and settled events while the
		// client is displaying a dialog. Keep the RPC reader available for them.
		var request struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(raw, &request)
		if request.Method == "confirm" || request.Method == "select" {
			go t.handleExtensionUI(raw)
		} else {
			t.handleExtensionUI(raw)
		}

	case "auto_retry_start":
		t.emit(acpsdk.UpdateAgentMessageText("Retrying...\n\n"))

	case "auto_retry_end":
		t.emit(acpsdk.UpdateAgentMessageText("Retry finished, resuming.\n\n"))

	case "compaction_start":
		t.emit(acpsdk.UpdateAgentMessageText("Context nearing limit, running automatic compaction...\n\n"))

	case "compaction_end":
		t.emit(acpsdk.UpdateAgentMessageText("Automatic compaction finished.\n\n"))

	case "session_info_changed":
		var p struct {
			Name json.RawMessage `json:"name"`
		}
		if json.Unmarshal(raw, &p) != nil {
			return
		}
		// Pi omits the name when it is cleared, which lists the session by its first prompt.
		title := t.sess.fallbackTitle()
		if len(p.Name) > 0 {
			var name string
			if string(p.Name) == "null" || json.Unmarshal(p.Name, &name) != nil {
				return
			}
			if strings.TrimSpace(name) != "" {
				title = strings.TrimSpace(name)
			}
		}
		if u := t.sess.titleUpdate(title, true); u != nil {
			t.emit(*u)
		}

	case "agent_settled":
		reason := acpsdk.StopReasonEndTurn
		if t.ctx != nil && t.ctx.Err() != nil {
			reason = acpsdk.StopReasonCancelled
		}
		t.resolve(reason, nil)
		if t.settled != nil {
			t.settleOnce.Do(func() { close(t.settled) })
		}
	}
}

func (t *turn) handleMessageUpdate(raw json.RawMessage) {
	var p struct {
		AssistantMessageEvent struct {
			Type         string      `json:"type"`
			Delta        string      `json:"delta"`
			ToolCall     *piToolCall `json:"toolCall"`
			ContentIndex int         `json:"contentIndex"`
			Partial      struct {
				Content []piToolCall `json:"content"`
			} `json:"partial"`
		} `json:"assistantMessageEvent"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}

	ame := p.AssistantMessageEvent
	switch ame.Type {
	case "text_delta":
		if ame.Delta != "" {
			t.emit(acpsdk.UpdateAgentMessageText(ame.Delta))
		}
	case "thinking_delta":
		if ame.Delta != "" {
			t.emit(acpsdk.UpdateAgentThoughtText(ame.Delta))
		}
	case "toolcall_start", "toolcall_delta", "toolcall_end":
		tc := ame.ToolCall
		if tc == nil && ame.ContentIndex >= 0 && ame.ContentIndex < len(ame.Partial.Content) {
			tc = &ame.Partial.Content[ame.ContentIndex]
		}
		if tc == nil || tc.ID == "" {
			return
		}
		seen := t.tools[tc.ID]
		if !seen {
			t.tools[tc.ID] = true
		}
		if seen {
			return
		}
		args := toolArgs(tc)
		presentation := presentTool(tc.Name, args, t.sess.cwd)
		t.emit(acpsdk.StartToolCall(
			acpsdk.ToolCallId(tc.ID), presentation.title,
			startToolOptions(presentation, acpsdk.ToolCallStatusPending)...,
		))
	}
}

func (t *turn) handleToolStart(raw json.RawMessage) {
	var p struct {
		ToolCallID string          `json:"toolCallId"`
		ToolName   string          `json:"toolName"`
		Args       json.RawMessage `json:"args"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ToolCallID == "" {
		return
	}

	var args map[string]any
	_ = json.Unmarshal(p.Args, &args)
	presentation := presentTool(p.ToolName, args, t.sess.cwd)
	t.snapshotFileMutation(p.ToolName, p.ToolCallID, args, presentation.locations)

	seen := t.tools[p.ToolCallID]
	t.tools[p.ToolCallID] = true

	if !seen {
		t.emit(acpsdk.StartToolCall(
			acpsdk.ToolCallId(p.ToolCallID), presentation.title,
			startToolOptions(presentation, acpsdk.ToolCallStatusInProgress)...,
		))
		return
	}

	opts := []acpsdk.ToolCallUpdateOpt{
		acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusInProgress),
		acpsdk.WithUpdateTitle(presentation.title),
		acpsdk.WithUpdateKind(presentation.kind),
	}
	if presentation.rawInput != nil {
		opts = append(opts, acpsdk.WithUpdateRawInput(presentation.rawInput))
	} else if args != nil || len(presentation.locations) > 0 {
		// A streamed partial call may have exposed partialArgs. Replace it with
		// an empty object once the complete call is represented solely by a chip.
		opts = append(opts, acpsdk.WithUpdateRawInput(map[string]any{}))
	}
	if len(presentation.locations) > 0 {
		opts = append(opts, acpsdk.WithUpdateLocations(presentation.locations))
	}
	t.emit(acpsdk.UpdateToolCall(acpsdk.ToolCallId(p.ToolCallID), opts...))
}

func (t *turn) snapshotFileMutation(toolName, toolCallID string, args map[string]any, locs []acpsdk.ToolCallLocation) {
	if toolName != "edit" && toolName != "write" {
		return
	}
	t.mutations[toolCallID] = true
	path := toolPath(args)
	if path == "" {
		return
	}
	snap := fileSnapshot{path: path}
	if data, err := os.ReadFile(absPath(path, t.sess.cwd)); err == nil {
		old := string(data)
		snap.oldText = &old
		if toolName == "edit" && len(locs) > 0 {
			for _, needle := range editOldTexts(args) {
				if line := findUniqueLine(old, needle); line != nil {
					locs[0].Line = line
					break
				}
			}
		}
	}
	t.snapshots[toolCallID] = snap
}

func (t *turn) handleToolUpdate(raw json.RawMessage) {
	var p struct {
		ToolCallID    string          `json:"toolCallId"`
		PartialResult json.RawMessage `json:"partialResult"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ToolCallID == "" {
		return
	}
	t.ensureToolStarted(p.ToolCallID)

	opts := []acpsdk.ToolCallUpdateOpt{acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusInProgress)}
	if !t.mutations[p.ToolCallID] {
		if text := toolResultToText(p.PartialResult); text != "" {
			opts = append(opts, acpsdk.WithUpdateContent([]acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(text))}))
		}
	}
	t.emit(acpsdk.UpdateToolCall(acpsdk.ToolCallId(p.ToolCallID), opts...))
}

func (t *turn) handleToolEnd(raw json.RawMessage) {
	var p struct {
		ToolCallID string          `json:"toolCallId"`
		Result     json.RawMessage `json:"result"`
		IsError    bool            `json:"isError"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ToolCallID == "" {
		return
	}
	t.ensureToolStarted(p.ToolCallID)

	status := acpsdk.ToolCallStatusCompleted
	if p.IsError {
		status = acpsdk.ToolCallStatusFailed
	}

	var content []acpsdk.ToolCallContent
	if snap, ok := t.snapshots[p.ToolCallID]; ok && !p.IsError {
		if data, err := os.ReadFile(absPath(snap.path, t.sess.cwd)); err == nil {
			newText := string(data)
			if snap.oldText == nil || newText != *snap.oldText {
				content = []acpsdk.ToolCallContent{{Diff: &acpsdk.ToolCallContentDiff{
					Type:    "diff",
					Path:    snap.path,
					OldText: snap.oldText,
					NewText: newText,
				}}}
			}
		}
	}
	if content == nil {
		if text := toolResultToText(p.Result); text != "" {
			content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(text))}
		}
	}

	opts := []acpsdk.ToolCallUpdateOpt{acpsdk.WithUpdateStatus(status)}
	if len(content) > 0 {
		opts = append(opts, acpsdk.WithUpdateContent(content))
	}
	t.emit(acpsdk.UpdateToolCall(acpsdk.ToolCallId(p.ToolCallID), opts...))

	delete(t.tools, p.ToolCallID)
	delete(t.mutations, p.ToolCallID)
	delete(t.snapshots, p.ToolCallID)
}

func (t *turn) ensureToolStarted(id string) {
	seen := t.tools[id]
	if !seen {
		t.tools[id] = true
	}
	if !seen {
		t.emit(acpsdk.StartToolCall(acpsdk.ToolCallId(id), "Tool call",
			acpsdk.WithStartKind(acpsdk.ToolKindOther),
			acpsdk.WithStartStatus(acpsdk.ToolCallStatusInProgress),
		))
	}
}

func (t *turn) handleExtensionUI(raw json.RawMessage) {
	var p struct {
		ID         string   `json:"id"`
		Method     string   `json:"method"`
		Title      string   `json:"title"`
		Message    string   `json:"message"`
		NotifyType string   `json:"notifyType"`
		Options    []string `json:"options"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ID == "" {
		return
	}

	switch p.Method {
	case "confirm":
		ok := t.askConfirm(p.ID, p.Title, p.Message)
		t.sess.proc.sendExtensionResponse(map[string]any{"id": p.ID, "confirmed": ok})

	case "select":
		if value, ok := t.askSelect(p.ID, p.Title, p.Options); ok {
			t.sess.proc.sendExtensionResponse(map[string]any{"id": p.ID, "value": value})
		} else {
			t.sess.proc.sendExtensionResponse(map[string]any{"id": p.ID, "cancelled": true})
		}

	case "notify":
		if p.Message != "" {
			u := acpsdk.UpdateAgentMessageText(p.Message + "\n\n")
			level := p.NotifyType
			if level == "" {
				level = "info"
			}
			u.AgentMessageChunk.Meta = map[string]any{"piAcp": map[string]any{"notify": map[string]any{"level": level}}}
			t.emit(u)
		}

	case "input", "editor":
		// ACP has no free-form input primitive. Cancel dialog-style requests so
		// the extension can continue instead of waiting forever.
		t.sess.proc.sendExtensionResponse(map[string]any{"id": p.ID, "cancelled": true})

	default:
		// setStatus, setWidget, setTitle, and set_editor_text are fire-and-forget
		// in Pi RPC mode. Sending a response for them is a protocol violation.
	}
}

func (t *turn) askConfirm(id, title, message string) bool {
	tcTitle := title
	if tcTitle == "" {
		tcTitle = "Confirm"
	}
	status := acpsdk.ToolCallStatusPending
	kind := acpsdk.ToolKindOther
	tc := acpsdk.ToolCallUpdate{
		ToolCallId: acpsdk.ToolCallId("pi-ui-" + id),
		Title:      &tcTitle,
		Kind:       &kind,
		Status:     &status,
	}
	if message != "" {
		tc.Content = []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(message))}
	}

	resp, err := acp.Call(t.ctx, func() (acpsdk.RequestPermissionResponse, error) {
		return t.conn.RequestPermission(t.ctx, acpsdk.RequestPermissionRequest{
			SessionId: t.sess.id,
			ToolCall:  tc,
			Options: []acpsdk.PermissionOption{
				{OptionId: "yes", Name: "Yes", Kind: acpsdk.PermissionOptionKindAllowOnce},
				{OptionId: "no", Name: "No", Kind: acpsdk.PermissionOptionKindRejectOnce},
			},
		})
	})
	if err != nil || resp.Outcome.Selected == nil {
		return false
	}
	return resp.Outcome.Selected.OptionId == "yes"
}

func (t *turn) askSelect(id, title string, options []string) (string, bool) {
	if len(options) == 0 {
		return "", false
	}
	tcTitle := title
	if tcTitle == "" {
		tcTitle = "Select"
	}
	status := acpsdk.ToolCallStatusPending
	kind := acpsdk.ToolKindOther
	tc := acpsdk.ToolCallUpdate{
		ToolCallId: acpsdk.ToolCallId("pi-ui-" + id),
		Title:      &tcTitle,
		Kind:       &kind,
		Status:     &status,
	}

	opts := make([]acpsdk.PermissionOption, 0, len(options))
	for i, name := range options {
		opts = append(opts, acpsdk.PermissionOption{
			OptionId: acpsdk.PermissionOptionId(choiceID(i)),
			Name:     name,
			Kind:     acpsdk.PermissionOptionKindAllowOnce,
		})
	}

	resp, err := acp.Call(t.ctx, func() (acpsdk.RequestPermissionResponse, error) {
		return t.conn.RequestPermission(t.ctx, acpsdk.RequestPermissionRequest{
			SessionId: t.sess.id,
			ToolCall:  tc,
			Options:   opts,
		})
	})
	if err != nil || resp.Outcome.Selected == nil {
		return "", false
	}
	for i := range options {
		if string(resp.Outcome.Selected.OptionId) == choiceID(i) {
			return options[i], true
		}
	}
	return "", false
}

func choiceID(i int) string {
	return "choice-" + strconv.Itoa(i)
}

func toolArgs(tc *piToolCall) map[string]any {
	if len(tc.Arguments) > 0 {
		var m map[string]any
		if json.Unmarshal(tc.Arguments, &m) == nil {
			return m
		}
	}
	if tc.PartialArgs != "" {
		var m map[string]any
		if json.Unmarshal([]byte(tc.PartialArgs), &m) == nil {
			return m
		}
		return map[string]any{"partialArgs": tc.PartialArgs}
	}
	return nil
}

func toolPath(args map[string]any) string {
	if args == nil {
		return ""
	}
	if p, ok := args["path"].(string); ok && p != "" {
		return p
	}
	if p, ok := args["file_path"].(string); ok && p != "" {
		return p
	}
	return ""
}

func toolLocations(args map[string]any, cwd string) []acpsdk.ToolCallLocation {
	path := toolPath(args)
	if path == "" {
		return nil
	}
	location := acpsdk.ToolCallLocation{Path: acp.ResolvePath(path, cwd)}
	if offset, ok := tool.IntArg(args, "offset"); ok && offset > 0 {
		location.Line = new(offset)
	}
	return []acpsdk.ToolCallLocation{location}
}

type toolPresentation struct {
	title     string
	kind      acpsdk.ToolKind
	rawInput  any
	locations []acpsdk.ToolCallLocation
}

func presentTool(name string, args map[string]any, cwd string) toolPresentation {
	kind := toolKind(name)
	locations := toolLocations(args, cwd)
	title := name
	omit := []string{}

	switch strings.ToLower(name) {
	case "read":
		title = "Read file"
		omit = append(omit, "path", "file_path", "offset")
	case "write":
		title = "Write file"
		omit = append(omit, "path", "file_path", "content")
	case "edit":
		title = "Edit file"
		omit = append(omit,
			"path", "file_path", "oldText", "newText", "old_string", "new_string",
			"replaceAll", "replace_all", "edits",
		)
	case "bash", "shell", "exec", "exec_command":
		title = "Run command"
	case "grep":
		title = "Search files"
		omit = append(omit, "path", "file_path")
	case "glob":
		title = "Find files"
		omit = append(omit, "path", "file_path")
	case "view_image":
		title = "View image"
		omit = append(omit, "path", "file_path")
	}

	if title == "" {
		title = "Tool call"
	}
	if len(locations) > 0 && len(omit) == 0 {
		omit = append(omit, "path", "file_path")
	}
	return toolPresentation{
		title: title, kind: kind,
		rawInput: compactToolArgs(args, omit...), locations: locations,
	}
}

func compactToolArgs(args map[string]any, omit ...string) any {
	if len(args) == 0 {
		return nil
	}
	display := maps.Clone(args)
	for _, key := range omit {
		delete(display, key)
	}
	if len(display) == 0 {
		return nil
	}
	return display
}

func startToolOptions(p toolPresentation, status acpsdk.ToolCallStatus) []acpsdk.ToolCallStartOpt {
	opts := []acpsdk.ToolCallStartOpt{
		acpsdk.WithStartKind(p.kind),
		acpsdk.WithStartStatus(status),
	}
	if len(p.locations) > 0 {
		opts = append(opts, acpsdk.WithStartLocations(p.locations))
	}
	// Apply rawInput after locations to undo the SDK's synthetic path mirror.
	if p.rawInput != nil || len(p.locations) > 0 {
		opts = append(opts, acpsdk.WithStartRawInput(p.rawInput))
	}
	return opts
}

func absPath(path, cwd string) string {
	return acp.ResolvePath(path, cwd)
}

func editOldTexts(args map[string]any) []string {
	var out []string
	if s, ok := args["oldText"].(string); ok && s != "" {
		out = append(out, s)
	}
	edits := args["edits"]
	if s, ok := edits.(string); ok {
		var arr []any
		if json.Unmarshal([]byte(s), &arr) == nil {
			edits = arr
		}
	}
	if arr, ok := edits.([]any); ok {
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				if s, ok := m["oldText"].(string); ok && s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

func findUniqueLine(text, needle string) *int {
	if needle == "" {
		return nil
	}
	first := strings.Index(text, needle)
	if first < 0 || strings.Contains(text[first+1:], needle) {
		return nil
	}
	line := 1 + strings.Count(text[:first], "\n")
	return &line
}

func toolKind(name string) acpsdk.ToolKind {
	switch strings.ToLower(name) {
	case "read":
		return acpsdk.ToolKindRead
	case "write", "edit":
		return acpsdk.ToolKindEdit
	case "bash", "shell", "exec", "exec_command":
		return acpsdk.ToolKindExecute
	case "grep", "glob":
		return acpsdk.ToolKindSearch
	case "view_image":
		return acpsdk.ToolKindRead
	default:
		return acpsdk.ToolKindOther
	}
}

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "auth") && strings.Contains(msg, "required") ||
		strings.Contains(msg, "api key") ||
		strings.Contains(msg, "no models")
}

type piImage struct {
	Type     string `json:"type"`
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

func promptToPi(blocks []acpsdk.ContentBlock) (string, []piImage) {
	var b strings.Builder
	var images []piImage

	for _, block := range blocks {
		switch {
		case block.Text != nil:
			b.WriteString(block.Text.Text)

		case block.Image != nil:
			img := block.Image
			data := img.Data
			mime := img.MimeType
			if data == "" && img.Uri != nil {
				if m, d, ok := splitDataURL(*img.Uri); ok {
					mime, data = m, d
				}
			}
			if data != "" {
				images = append(images, piImage{Type: "image", MimeType: mime, Data: data})
			}

		case block.ResourceLink != nil:
			b.WriteString("\n[Context] " + block.ResourceLink.Uri)

		case block.Resource != nil:
			resource := block.Resource.Resource
			switch {
			case resource.TextResourceContents != nil:
				r := resource.TextResourceContents
				b.WriteString("\n[Embedded Context] " + r.Uri + "\n" + r.Text)
			case resource.BlobResourceContents != nil:
				r := resource.BlobResourceContents
				mimeType := "application/octet-stream"
				if r.MimeType != nil && *r.MimeType != "" {
					mimeType = *r.MimeType
				}
				if strings.HasPrefix(strings.ToLower(mimeType), "image/") && r.Blob != "" {
					images = append(images, piImage{Type: "image", MimeType: mimeType, Data: r.Blob})
					break
				}
				b.WriteString("\n[Embedded Context] " + r.Uri + " (" + mimeType + ", base64)\n" + r.Blob)
			}
		}
	}

	return b.String(), images
}

func promptTitle(blocks []acpsdk.ContentBlock) string {
	var texts []string
	for _, block := range blocks {
		if block.Text != nil {
			texts = append(texts, block.Text.Text)
		}
	}
	return sessionTitleText(texts)
}

func splitDataURL(s string) (mime, data string, ok bool) {
	rest, found := strings.CutPrefix(s, "data:")
	if !found {
		return "", "", false
	}
	mime, data, ok = strings.Cut(rest, ";base64,")
	return mime, data, ok
}

func toolResultToText(result json.RawMessage) string {
	if len(result) == 0 {
		return ""
	}

	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		Output   string `json:"output"`
		ExitCode *int   `json:"exitCode"`
		Details  struct {
			Diff     string `json:"diff"`
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
			Output   string `json:"output"`
			ExitCode *int   `json:"exitCode"`
		} `json:"details"`
	}
	if json.Unmarshal(result, &r) != nil {
		return string(result)
	}

	if strings.TrimSpace(r.Details.Diff) != "" {
		return r.Details.Diff
	}

	var texts []string
	for _, c := range r.Content {
		if c.Type == "text" && c.Text != "" {
			texts = append(texts, c.Text)
		}
	}
	if len(texts) > 0 {
		return strings.Join(texts, "")
	}

	stdout := firstNonEmpty(r.Details.Stdout, r.Stdout, r.Details.Output, r.Output)
	stderr := firstNonEmpty(r.Details.Stderr, r.Stderr)
	exit := r.ExitCode
	if exit == nil {
		exit = r.Details.ExitCode
	}

	if strings.TrimSpace(stdout) != "" || strings.TrimSpace(stderr) != "" {
		var parts []string
		if strings.TrimSpace(stdout) != "" {
			parts = append(parts, stdout)
		}
		if strings.TrimSpace(stderr) != "" {
			parts = append(parts, "stderr:\n"+stderr)
		}
		if exit != nil {
			parts = append(parts, "exit code: "+strconv.Itoa(*exit))
		}
		return strings.TrimRight(strings.Join(parts, "\n\n"), "\n")
	}

	return string(result)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
