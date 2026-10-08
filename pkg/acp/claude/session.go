package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"

	acpcommon "github.com/adrianliechti/wingman-agent/pkg/acp"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/internal/process"
)

type session struct {
	id    acp.SessionId
	cwd   string
	agent *Agent

	promptMu       acpcommon.PromptGate
	mu             sync.Mutex
	closed         bool
	modelID        string
	modelOverride  bool
	effort         string
	mode           string
	prePlanMode    string
	allowBypass    bool
	mcpServers     []acp.McpServer
	additionalDirs []string
	resumeFrom     string
	forkOnResume   bool
	started        bool
	lastTitle      string
	cancel         context.CancelFunc
	proc           *claudeProc
}

func (a *Agent) newSession(id acp.SessionId, cwd, model, effort string, additionalDirs []string) *session {
	s := &session{
		id:             id,
		cwd:            cwd,
		agent:          a,
		modelID:        model,
		modelOverride:  model != "" && model != "default",
		effort:         effort,
		mode:           defaultModeID,
		allowBypass:    true,
		additionalDirs: append([]string(nil), additionalDirs...),
	}
	return s
}

// setModeLocked remembers the mode the session left when it entered plan mode.
func (s *session) setModeLocked(modeID string) {
	switch {
	case modeID != planModeID:
		s.prePlanMode = ""
	case s.mode != planModeID:
		s.prePlanMode = s.mode
	}
	s.mode = modeID
}

func (s *session) currentPrePlanMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prePlanMode
}

func (s *session) cancelTurn() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *session) close() {
	s.mu.Lock()
	s.closed = true
	cancel := s.cancel
	proc := s.proc
	s.proc = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if proc != nil {
		proc.shutdown()
	}
	// A closing session must let its cancelled prompt release the turn gate
	// before it is replaced or reported closed. Keep a bound for wedged clients.
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := s.promptMu.Lock(ctx); err == nil {
		s.promptMu.Unlock()
	} else {
		fmt.Fprintf(s.agent.stderr, "claude-acp: session %s prompt cleanup: %v\n", s.id, err)
	}
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *session) runTurn(ctx context.Context, prompt []acp.ContentBlock) (acp.StopReason, *acp.Usage, error) {
	p, err := s.ensureProc()
	if err != nil {
		return "", nil, err
	}

	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return acp.StopReasonCancelled, nil, nil
	}
	s.cancel = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
	}()

	p.beginTurn(turnCtx)
	input := promptMessage(prompt)
	p.turnMu.Lock()
	input.UUID = p.turnID
	p.turnMu.Unlock()
	if err := p.out.writeJSON(input); err != nil {
		p.finishTurn()
		s.dropProc(p)
		return "", nil, fmt.Errorf("write prompt: %w", err)
	}

	select {
	case <-turnCtx.Done():

		_ = p.out.writeJSON(interruptRequest())
		select {
		case r := <-p.results:
			return acp.StopReasonCancelled, r.usage, nil
		case <-p.dead:
		case <-time.After(5 * time.Second):
			s.dropProc(p)
		}
		return acp.StopReasonCancelled, nil, nil
	case r := <-p.results:
		if turnCtx.Err() != nil {
			return acp.StopReasonCancelled, r.usage, nil
		}
		if r.err != nil {
			s.dropProc(p)
			return "", nil, r.err
		}
		s.pushTitleUpdate(ctx)
		return r.stop, r.usage, nil
	case <-p.dead:
		if turnCtx.Err() != nil {
			return acp.StopReasonCancelled, nil, nil
		}
		// Preserve a result already read immediately before process EOF.
		select {
		case r := <-p.results:
			if r.err != nil {
				s.dropProc(p)
			}
			return r.stop, r.usage, r.err
		default:
		}
		s.dropProc(p)
		return "", nil, fmt.Errorf("claude process exited unexpectedly")
	}
}

// pushTitleUpdate notifies the client when the CLI's auto-generated session
// title has changed since the last time we looked. The CLI has no push event
// for it — it's regenerated in the background and persisted to the session's
// JSONL file — so we read it back at turn end, the same point a new title
// would have landed, and only notify when it actually changed.
func (s *session) pushTitleUpdate(ctx context.Context) {
	dir := projectDirFor(s.cwd)
	if dir == "" {
		return
	}
	title, _ := scanSessionMetadata(filepath.Join(dir, string(s.id)+".jsonl"))
	if title == "" {
		return
	}

	s.mu.Lock()
	changed := title != s.lastTitle
	if changed {
		s.lastTitle = title
	}
	s.mu.Unlock()
	if !changed {
		return
	}

	t := title
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	_ = s.agent.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: s.id,
		Update: acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
			SessionUpdate: "session_info_update",
			Title:         &t,
			UpdatedAt:     &updatedAt,
		}},
	})
}

func (s *session) ensureProc() (*claudeProc, error) {
	a := s.agent
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("session %s is closed", s.id)
	}

	sig := s.spawnSigLocked()
	if s.proc != nil && s.proc.sig == sig && !s.proc.isDead() {
		return s.proc, nil
	}
	if s.proc != nil {
		s.proc.shutdown()
		s.proc = nil
	}

	args := s.cliArgsLocked()
	procCtx, kill := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, a.path, args...)
	cmd.WaitDelay = 2 * time.Second
	process.Hide(cmd)
	cmd.Dir = s.cwd
	if a.env != nil {
		cmd.Env = a.env
	}
	cmd.Stderr = a.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		kill()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		kill()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		kill()
		return nil, fmt.Errorf("start claude: %w", err)
	}

	p := &claudeProc{
		cmd:             cmd,
		out:             &streamWriter{w: acpcommon.NewConnectionWriter(stdin, 0)},
		stdin:           stdin,
		sig:             sig,
		kill:            kill,
		cwd:             s.cwd,
		session:         s,
		models:          append([]ModelEntry(nil), a.models...),
		tools:           toolUseCache{},
		emitted:         newToolCallTracker(),
		streamedContent: &streamedBlockTracker{},
		subagentParents: make(map[string]string),
		results:         make(chan turnResult, 1),
		dead:            make(chan struct{}),
	}
	go p.read(procCtx, a.conn, s.id, stdout)

	s.started = true
	s.resumeFrom = ""
	s.forkOnResume = false
	s.proc = p
	return p, nil
}

func (s *session) dropProc(p *claudeProc) {
	p.shutdown()
	s.mu.Lock()
	if s.proc == p {
		s.proc = nil
	}
	s.mu.Unlock()
}

func (s *session) spawnSigLocked() string {
	return strings.Join(append([]string{s.modelID, s.effort, s.mode}, s.additionalDirs...), "\x00")
}

type claudeProc struct {
	cmd             *exec.Cmd
	out             *streamWriter
	stdin           io.Closer
	sig             string
	kill            context.CancelFunc
	cwd             string
	session         *session
	models          []ModelEntry
	tools           toolUseCache
	emitted         *toolCallTracker
	streamedContent *streamedBlockTracker
	contextUsage    contextUsage

	turnMu              sync.Mutex
	turnActive          bool
	turnID              string
	turnCtx             context.Context
	turnCancel          context.CancelFunc
	deliveredText       bool
	deliveredCompaction bool
	pendingResultIdles  int
	subagentMu          sync.Mutex
	subagentParents     map[string]string
	results             chan turnResult
	dead                chan struct{}
	shutdownOnce        sync.Once
}

func (p *claudeProc) beginTurn(ctx context.Context) {
	p.turnMu.Lock()
	if p.turnCancel != nil {
		p.turnCancel()
	}
	p.turnCtx, p.turnCancel = context.WithCancel(ctx)
	p.turnID = uuid.NewString()
	p.turnActive = true
	p.deliveredText = false
	p.deliveredCompaction = false
	p.turnMu.Unlock()
}

func (p *claudeProc) markTurnOutput(text, compaction bool) {
	p.turnMu.Lock()
	defer p.turnMu.Unlock()
	if p.turnActive {
		p.deliveredText = p.deliveredText || text
		p.deliveredCompaction = p.deliveredCompaction || compaction
	}
}

func (p *claudeProc) finishTurn() bool {
	p.turnMu.Lock()
	defer p.turnMu.Unlock()
	wasActive := p.turnActive
	p.turnActive = false
	if p.turnCancel != nil {
		p.turnCancel()
		p.turnCancel = nil
	}
	return wasActive
}

func (p *claudeProc) failTurn(err error) {
	if p.finishTurn() {
		select {
		case p.results <- turnResult{err: err}:
		default:
		}
	}
}

func (p *claudeProc) parentForAgent(agentID string) string {
	if agentID == "" {
		return ""
	}
	p.subagentMu.Lock()
	defer p.subagentMu.Unlock()
	return p.subagentParents[agentID]
}

type turnResult struct {
	stop  acp.StopReason
	err   error
	usage *acp.Usage
}

func (p *claudeProc) isDead() bool {
	select {
	case <-p.dead:
		return true
	default:
		return false
	}
}

func (p *claudeProc) shutdown() {
	p.shutdownOnce.Do(func() {
		_ = p.stdin.Close()
		exited := make(chan struct{})
		go func() {
			_ = p.cmd.Wait()
			close(exited)
		}()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			p.kill()
			<-exited
		}
	})
}

func (p *claudeProc) read(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, r io.Reader) {
	defer close(p.dead)
	defer p.finishTurn()
	stderr := p.session.agent.stderr
	p.session.mu.Lock()
	p.contextUsage.setModel(p.session.modelID)
	allowBypass := p.session.allowBypass
	p.session.mu.Unlock()
	app := &approver{ctx: ctx, conn: conn, sid: sid, out: p.out, cwd: p.cwd, emitted: p.emitted, parentForAgent: p.parentForAgent,
		askForm:     p.session.agent.supportsFormElicitation(),
		applyMode:   func(modeID string) { p.applyMode(ctx, conn, sid, modeID) },
		allowBypass: allowBypass,
		prePlanMode: p.session.currentPrePlanMode}

	scanner := newCLIScanner(r)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var env cliEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			fmt.Fprintf(stderr, "claude-acp: skipping non-JSON line: %s\n", line)
			continue
		}
		switch env.Type {
		case "stream_event":
			if env.ParentToolUseID != "" || env.ParentAgentID != "" {
				continue
			}
			var event streamEvent
			if json.Unmarshal(env.Event, &event) != nil {
				continue
			}
			p.contextUsage.observeStream(event)
			if err := emitStreamEvent(ctx, conn, sid, env.Event, p.streamedContent); err != nil {
				p.failTurn(fmt.Errorf("send stream event: %w", err))
				return
			} else {
				p.markTurnOutput(event.Type == "content_block_delta" && event.Delta.Type == "text_delta" && event.Delta.Text != "",
					event.ContentBlock.Type == "compaction" || event.Delta.Type == "compaction_delta")
			}
		case "assistant":
			root := env.ParentToolUseID == "" && env.ParentAgentID == ""
			var message cliMessage
			if root && json.Unmarshal(env.Message, &message) == nil {
				// The client owns the login UI; the CLI's TUI advice must not reach the chat.
				if isSyntheticLoginMessage(message) {
					if p.finishTurn() {
						select {
						case p.results <- turnResult{err: acp.NewAuthRequired(nil)}:
						default:
						}
					}
					continue
				}
				p.contextUsage.observeAssistant(message)
			}
			if err := emitAssistant(ctx, conn, sid, env.Message, p.cwd, p.tools, p.emitted, p.streamedContent, env.ParentToolUseID); err != nil {
				p.failTurn(fmt.Errorf("send assistant: %w", err))
				return
			} else if root {
				for _, block := range message.Content {
					_, visible := stripMarkerTags(block.Text)
					p.markTurnOutput(block.Type == "text" && visible, block.Type == "compaction")
				}
			}
		case "user":
			if err := emitToolResults(ctx, conn, sid, env.Message, p.tools, p.emitted, env.ParentToolUseID); err != nil {
				p.failTurn(fmt.Errorf("send tool result: %w", err))
				return
			}
		case "tool_progress":
			if err := p.handleToolProgress(ctx, conn, sid, env); err != nil {
				p.failTurn(fmt.Errorf("send tool progress: %w", err))
				return
			}
		case "control_request":
			var req controlRequest
			if json.Unmarshal(line, &req) == nil {
				// Capture this turn before launching the dialog goroutine. A later
				// turn must never inherit an earlier request's permission answer.
				perTurn := *app
				p.turnMu.Lock()
				perTurn.ctx = p.turnCtx
				p.turnMu.Unlock()
				if perTurn.ctx == nil {
					app.respondDeny(req.RequestID)
					continue
				}
				go perTurn.handle(req)
			}
		case "result":
			if env.ParentToolUseID != "" || env.ParentAgentID != "" {
				continue
			}
			var result cliResult
			if json.Unmarshal(line, &result) != nil {
				continue
			}
			p.turnMu.Lock()
			matches := p.turnActive && result.answers(p.turnID)
			// Each root result has a trailing idle, which can arrive after
			// the next prompt starts. It must not fail that new turn.
			p.pendingResultIdles++
			fallback := matches && p.turnCtx != nil && p.turnCtx.Err() == nil && !p.deliveredText && !p.deliveredCompaction
			p.turnMu.Unlock()
			if !matches {
				continue
			}
			tr := resultToTurn(line)
			if !p.finishTurn() {
				continue
			}
			// Cached answers may arrive only in result, with no generated tokens.
			if fallback && result.Subtype == "success" && tr.err == nil && tr.stop == acp.StopReasonEndTurn &&
				(result.Usage == nil || result.Usage.OutputTokens == 0) {
				if text, ok := stripMarkerTags(result.Result); ok {
					if err := acpcommon.Notify(ctx, conn, sid, acp.UpdateAgentMessageText(text)); err != nil {
						tr.err = fmt.Errorf("send result text: %w", err)
					}
				}
			}
			if usageUpd := p.contextUsage.resultUpdate(result, p.models); usageUpd != nil {
				if err := acpcommon.Notify(ctx, conn, sid, *usageUpd); err != nil && tr.err == nil {
					tr.err = fmt.Errorf("send usage: %w", err)
				}
			}
			select {
			case p.results <- tr:
			default:
			}
		case "rate_limit_event":
			if note := rateLimitNote(env); note != "" {
				if err := acpcommon.Notify(ctx, conn, sid, acp.UpdateAgentMessageText(note)); err != nil {
					p.failTurn(fmt.Errorf("send rate limit notice: %w", err))
					return
				}
			}
		case "system":
			p.handleSystem(ctx, conn, sid, env)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(stderr, "claude-acp: scan error: %v\n", err)
	}
}

// rateLimitNote skips the routine "allowed" heartbeat.
func rateLimitNote(env cliEnvelope) string {
	status := strings.TrimSpace(env.Status)
	if status == "" || status == "allowed" {
		return ""
	}
	note := "Rate limit " + status
	if env.RateLimitType != "" {
		note += " (" + strings.ReplaceAll(env.RateLimitType, "_", " ") + ")"
	}
	if resets := formatResetsAt(env.ResetsAt); resets != "" {
		note += ", resets " + resets
	}
	return "*" + note + ".*\n\n"
}

func formatResetsAt(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return time.Unix(int64(t), 0).Format(time.RFC3339)
	}
	return ""
}

// subagentParent falls back to tool_use_id when the task was never registered.
func (p *claudeProc) subagentParent(taskID, toolUseID string) string {
	if taskID != "" {
		p.subagentMu.Lock()
		id := p.subagentParents[taskID]
		p.subagentMu.Unlock()
		if id != "" {
			return id
		}
	}
	return toolUseID
}

func taskProgressNote(env cliEnvelope) string {
	parts := make([]string, 0, 3)
	if d := strings.TrimSpace(env.Description); d != "" {
		parts = append(parts, d)
	}
	if t := strings.TrimSpace(env.SubagentType); t != "" {
		parts = append(parts, "("+t+")")
	}
	if tool := strings.TrimSpace(env.LastToolName); tool != "" {
		parts = append(parts, "— "+tool)
	}
	return strings.Join(parts, " ")
}

// reportLoadErrors surfaces system/init's mcp_server_errors and plugin_errors.
func reportLoadErrors(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, env cliEnvelope) {
	var lines []string
	for _, e := range env.MCPServerErrors {
		lines = append(lines, fmt.Sprintf("- MCP server %q skipped (%s): %s", e.label(), e.Type, e.Message))
	}
	for _, e := range env.PluginErrors {
		lines = append(lines, fmt.Sprintf("- Plugin %q failed to load (%s): %s", e.label(), e.Type, e.Message))
	}
	for _, s := range env.MCPServers {
		if s.Status == "failed" {
			lines = append(lines, fmt.Sprintf("- MCP server %q failed to connect", s.Name))
		}
	}
	if len(lines) == 0 {
		return
	}
	_ = conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid,
		Update: acp.UpdateAgentMessageText("Startup warnings:\n" + strings.Join(lines, "\n") + "\n\n")})
}

func (p *claudeProc) handleSystem(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, env cliEnvelope) {
	switch env.Subtype {
	case "init":
		p.contextUsage.setModel(env.Model)
		reportLoadErrors(ctx, conn, sid, env)
	case "session_state_changed":
		if env.ParentToolUseID != "" || env.ParentAgentID != "" {
			return
		}
		if env.State == "idle" {
			p.turnMu.Lock()
			skipped := p.pendingResultIdles > 0
			if skipped {
				p.pendingResultIdles--
			}
			p.turnMu.Unlock()
			if skipped {
				return
			}
		}
		if env.State == "idle" && p.finishTurn() {
			select {
			case p.results <- turnResult{err: acp.NewInternalError("claude went idle without producing a result; partial output may be incomplete")}:
			default:
			}
		}

	case "model_refusal_fallback":
		if env.OriginalModel == "" || env.FallbackModel == "" {
			return
		}
		category := ""
		if env.RefusalCategory != "" {
			category = " (" + env.RefusalCategory + ")"
		}
		outcome := "The session will continue on " + env.FallbackModel + "."
		if env.Direction == "revert" {
			outcome = "The session stays on " + env.OriginalModel + "."
		}
		message := fmt.Sprintf("**Model fallback:** %s declined this request%s; retried with %s. %s", env.OriginalModel, category, env.FallbackModel, outcome)
		if env.RefusalExplanation != "" {
			message += "\n\n" + env.RefusalExplanation
		}
		_ = acpcommon.Notify(ctx, conn, sid, acp.UpdateAgentMessageText(message))
		if env.Direction != "revert" {
			p.applyFallbackModel(ctx, conn, sid, env.FallbackModel)
		}

	case "status":
		if env.ParentToolUseID != "" || env.ParentAgentID != "" {
			return
		}
		var text string
		switch {
		case env.Status == "compacting":
			text = "Compacting context...\n\n"
		case env.CompactResult == "success":
			text = "Context compacted.\n\n"
		case env.CompactResult == "failed":
			text = "Compacting failed.\n\n"
		}
		if text != "" {
			p.markTurnOutput(false, true)
			_ = acpcommon.Notify(ctx, conn, sid, acp.UpdateAgentMessageText(text))
		}

	case "compact_boundary":
		if env.ParentToolUseID != "" || env.ParentAgentID != "" {
			return
		}
		p.markTurnOutput(false, true)
		var postTokens *int
		if env.CompactMetadata != nil {
			postTokens = env.CompactMetadata.PostTokens
		}
		p.contextUsage.compact(postTokens)
		if update := p.contextUsage.update(0); update != nil {
			_ = acpcommon.Notify(ctx, conn, sid, *update)
		}

	case "informational":
		var content string
		if json.Unmarshal(env.Content, &content) != nil || strings.TrimSpace(content) == "" {
			return
		}
		text := content
		if env.Level != "" && env.Level != "info" {
			text = "**" + strings.ToUpper(env.Level[:1]) + env.Level[1:] + ":** " + content
		}
		update := acp.UpdateAgentMessageText(text)
		update.AgentMessageChunk.Meta = map[string]any{"claudeCode": map[string]any{"kind": "informational", "level": env.Level}}
		p.markTurnOutput(true, false)
		_ = acpcommon.Notify(ctx, conn, sid, update)

	case "local_command_output":
		var out string
		if json.Unmarshal(env.Content, &out) == nil && strings.TrimSpace(out) != "" {
			p.markTurnOutput(true, false)
			_ = acpcommon.Notify(ctx, conn, sid, acp.UpdateAgentMessageText(out))
		}

	case "permission_denied":
		toolCallID := env.ToolUseID
		if !p.emitted.has(toolCallID) {
			toolCallID = env.ParentToolUseID
		}
		if p.emitted.has(toolCallID) {
			msg := "Permission denied"
			var detail string
			if json.Unmarshal(env.Message, &detail) == nil && detail != "" {
				msg = "Permission denied: " + detail
			}
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.UpdateToolCall(acp.ToolCallId(toolCallID),
				acp.WithUpdateStatus(acp.ToolCallStatusFailed),
				acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(msg))}),
			)})
		}

	case "task_started":
		if env.TaskID != "" && env.ToolUseID != "" && p.tools[env.ToolUseID] != "Monitor" && env.TaskType != "local_monitor" {
			p.subagentMu.Lock()
			p.subagentParents[env.TaskID] = env.ToolUseID
			p.subagentMu.Unlock()
		}
	case "task_progress":
		if toolCallID := p.subagentParent(env.TaskID, env.ToolUseID); p.canReportTask(toolCallID, env.TaskType) {
			if note := taskProgressNote(env); note != "" {
				_ = conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.UpdateToolCall(acp.ToolCallId(toolCallID),
					acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(note))}),
				)})
			}
		}

	case "task_notification":
		if toolCallID := p.subagentParent(env.TaskID, env.ToolUseID); p.canReportTask(toolCallID, env.TaskType) && strings.TrimSpace(env.Summary) != "" {
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.UpdateToolCall(acp.ToolCallId(toolCallID),
				acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(strings.TrimSpace(env.Summary)))}),
			)})
		}
		p.subagentMu.Lock()
		delete(p.subagentParents, env.TaskID)
		p.subagentMu.Unlock()
	case "task_updated":
		if env.Patch.Status == "completed" || env.Patch.Status == "failed" || env.Patch.Status == "killed" {
			p.subagentMu.Lock()
			delete(p.subagentParents, env.TaskID)
			p.subagentMu.Unlock()
		}
	}
}

func (p *claudeProc) canReportTask(toolCallID, taskType string) bool {
	return p.emitted.has(toolCallID) && p.tools[toolCallID] != "Monitor" && taskType != "local_monitor"
}

func (p *claudeProc) handleToolProgress(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, env cliEnvelope) error {
	toolCallID := env.ToolUseID
	if !p.emitted.has(toolCallID) {
		toolCallID = env.ParentToolUseID
	}
	if !p.emitted.has(toolCallID) {
		return nil
	}
	name := env.ToolName
	if name == "" {
		name = p.tools[toolCallID]
	}
	update := acp.UpdateToolCall(acp.ToolCallId(toolCallID), acp.WithUpdateStatus(acp.ToolCallStatusInProgress))
	claudeMeta := map[string]any{"toolName": name, "toolResponse": map[string]any{"elapsedTimeSeconds": env.ElapsedTimeSeconds}}
	if env.SubagentType != "" {
		claudeMeta["toolResponse"].(map[string]any)["subagentType"] = env.SubagentType
	}
	if env.SubagentRetry != nil {
		claudeMeta["toolResponse"].(map[string]any)["subagentRetry"] = env.SubagentRetry
	}
	if update.ToolCallUpdate != nil {
		update.ToolCallUpdate.Meta = map[string]any{"claudeCode": claudeMeta}
	}
	return acpcommon.Notify(ctx, conn, sid, update)
}

func (p *claudeProc) applyFallbackModel(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, fallback string) {
	modelID := fallback
	if m := resolveModel(p.models, fallback); m != nil {
		modelID = m.ID
	}

	p.session.mu.Lock()
	p.session.modelID = modelID
	p.session.modelOverride = false
	if m := findModel(p.models, modelID); m != nil && !acpcommon.IsValidEffort(m.EffortLevels, p.session.effort) {
		p.session.effort = "default"
	}
	p.sig = p.session.spawnSigLocked()
	effort := p.session.effort
	p.session.mu.Unlock()

	_ = acpcommon.Notify(ctx, conn, sid, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
		SessionUpdate: "config_option_update",
		ConfigOptions: buildConfigOptions(p.models, modelID, effort),
	}})
}

func (p *claudeProc) applyMode(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, modeID string) {
	if findMode(modeID) == nil {
		return
	}
	p.session.mu.Lock()
	changed := p.session.mode != modeID
	p.session.setModeLocked(modeID)
	p.sig = p.session.spawnSigLocked()
	p.session.mu.Unlock()
	if !changed {
		return
	}
	_ = acpcommon.Notify(ctx, conn, sid, acp.SessionUpdate{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{
		SessionUpdate: "current_mode_update",
		CurrentModeId: acp.SessionModeId(modeID),
	}})
}

// A reply to merged user messages lists each of them in user_message_uuids.
func (r cliResult) answers(turnID string) bool {
	if r.UserMessageUUIDs != nil {
		return slices.Contains(r.UserMessageUUIDs, turnID)
	}
	if r.UserMessageUUID != "" {
		return r.UserMessageUUID == turnID
	}
	return !r.autonomous()
}

func (r cliResult) autonomous() bool {
	if r.Origin != nil {
		switch r.Origin.Kind {
		case "task-notification", "peer", "coordinator", "observer", "observer-activity":
			return true
		}
	}
	return false
}

func isSyntheticLoginMessage(m cliMessage) bool {
	return m.Model == "<synthetic>" && len(m.Content) == 1 && m.Content[0].Type == "text" &&
		strings.Contains(m.Content[0].Text, "Please run /login")
}

func resultToTurn(line []byte) turnResult {
	var r cliResult
	_ = json.Unmarshal(line, &r)

	tr := resultOutcome(r)
	tr.usage = resultUsage(r)
	return tr
}

func resultOutcome(r cliResult) turnResult {
	if r.IsError && strings.Contains(r.Result, "Please run /login") {
		return turnResult{err: acp.NewAuthRequired(nil)}
	}
	switch r.Subtype {
	case "success", "error_during_execution":
		if r.StopReason == "max_tokens" {
			return turnResult{stop: acp.StopReasonMaxTokens}
		}
		if r.StopReason == "refusal" {
			return turnResult{stop: acp.StopReasonRefusal}
		}
		if r.IsError {
			return turnResult{err: acp.NewInternalError(resultErrMessage(r))}
		}
		return turnResult{stop: acp.StopReasonEndTurn}
	case "error_max_budget_usd", "error_max_turns", "error_max_structured_output_retries":
		if r.IsError {
			return turnResult{err: acp.NewInternalError(resultErrMessage(r))}
		}
		return turnResult{stop: acp.StopReasonMaxTurnRequests}
	default:
		if r.IsError {
			return turnResult{err: acp.NewInternalError(resultErrMessage(r))}
		}
		return turnResult{stop: acp.StopReasonEndTurn}
	}
}

func resultUsage(r cliResult) *acp.Usage {
	if r.Usage == nil {
		return nil
	}
	u := *r.Usage
	cacheRead, cacheWrite := u.CacheReadInputTokens, u.CacheCreationInputTokens
	return &acp.Usage{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CachedReadTokens:  &cacheRead,
		CachedWriteTokens: &cacheWrite,
		TotalTokens:       u.InputTokens + u.OutputTokens + cacheRead + cacheWrite,
	}
}

func resultErrMessage(r cliResult) string {
	if msg := strings.Join(r.Errors, ", "); msg != "" {
		return msg
	}
	if r.Result != "" {
		return r.Result
	}
	return r.Subtype
}

func interruptRequest() controlInterrupt {
	return controlInterrupt{
		Type:      "control_request",
		RequestID: uuid.NewString(),
		Request:   controlInterruptBody{Subtype: "interrupt"},
	}
}

type streamWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *streamWriter) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(append(b, '\n'))
	return err
}

func (s *session) cliArgsLocked() []string {

	args := []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-prompt-tool", "stdio",

		"--settings", `{"disableRemoteControl":true}`,
	}
	// ExitPlanMode can switch the running process to bypass only when the
	// capability was enabled at startup. This does not select bypass mode.
	if s.allowBypass {
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	switch {
	case s.started:
		args = append(args, "--resume", string(s.id))
	case s.forkOnResume:
		args = append(args,
			"--resume", s.resumeFrom,
			"--session-id", string(s.id),
			"--fork-session",
		)
	case s.resumeFrom != "":
		args = append(args, "--resume", s.resumeFrom)
	default:
		args = append(args, "--session-id", string(s.id))
	}
	for _, d := range s.additionalDirs {
		args = append(args, "--add-dir", d)
	}
	if cfg := mcpConfigJSON(s.mcpServers); cfg != "" {
		args = append(args, "--mcp-config", cfg)
	}
	if s.modelID != "" && s.modelID != "default" && (s.resumeFrom == "" || s.modelOverride) {
		args = append(args, "--model", s.modelID)
	}
	if s.effort != "" && s.effort != "default" {
		args = append(args, "--effort", s.effort)
	}
	mode := findMode(s.mode)
	if mode == nil {
		mode = findMode(defaultModeID)
	}
	args = append(args, "--permission-mode", mode.permissionMode)
	return args
}

func promptMessage(blocks []acp.ContentBlock) cliInput {
	in := cliInput{Type: "user", Message: cliInputMessage{Role: "user"}}
	add := func(c cliInputContent) { in.Message.Content = append(in.Message.Content, c) }
	var contextBlocks []cliInputContent
	for _, b := range blocks {
		switch {
		case b.Text != nil:
			add(cliInputContent{Type: "text", Text: b.Text.Text})
		case b.Image != nil:
			switch {
			case b.Image.Data != "":
				add(cliInputContent{Type: "image", Source: &cliInputImageSource{
					Type:      "base64",
					MediaType: b.Image.MimeType,
					Data:      b.Image.Data,
				}})
			case b.Image.Uri != nil && (strings.HasPrefix(*b.Image.Uri, "http://") || strings.HasPrefix(*b.Image.Uri, "https://")):
				add(cliInputContent{Type: "image", Source: &cliInputImageSource{Type: "url", URL: *b.Image.Uri}})
			}
		case b.ResourceLink != nil:
			add(cliInputContent{Type: "text", Text: formatResourceLink(b.ResourceLink.Name, b.ResourceLink.Uri)})
		case b.Resource != nil:
			resource := b.Resource.Resource
			switch {
			case resource.TextResourceContents != nil:
				r := resource.TextResourceContents
				add(cliInputContent{Type: "text", Text: formatResourceLink("", r.Uri)})
				contextBlocks = append(contextBlocks, cliInputContent{Type: "text", Text: fmt.Sprintf("\n<context ref=%q>\n%s\n</context>", r.Uri, r.Text)})
			case resource.BlobResourceContents != nil:
				r := resource.BlobResourceContents
				mimeType := "application/octet-stream"
				if r.MimeType != nil && *r.MimeType != "" {
					mimeType = *r.MimeType
				}
				if strings.HasPrefix(strings.ToLower(mimeType), "image/") && r.Blob != "" {
					add(cliInputContent{Type: "image", Source: &cliInputImageSource{
						Type: "base64", MediaType: mimeType, Data: r.Blob,
					}})
					break
				}
				add(cliInputContent{Type: "text", Text: formatResourceLink("", r.Uri)})
				contextBlocks = append(contextBlocks, cliInputContent{Type: "text", Text: fmt.Sprintf(
					"\n<context ref=%q mimeType=%q encoding=%q>\n%s\n</context>", r.Uri, mimeType, "base64", r.Blob,
				)})
			}
		}
	}
	in.Message.Content = append(in.Message.Content, contextBlocks...)
	return in
}

func formatResourceLink(name, uri string) string {
	if name != "" {
		return fmt.Sprintf("[@%s](%s)", name, uri)
	}
	if path, ok := strings.CutPrefix(uri, "file://"); ok {
		return fmt.Sprintf("[@%s](%s)", filepath.Base(path), uri)
	}
	return uri
}
