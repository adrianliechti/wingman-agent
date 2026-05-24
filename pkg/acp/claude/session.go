package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/coder/acp-go-sdk"
)

// session holds per-conversation state. A new `claude` subprocess is spawned
// per turn (claude --print exits when the model returns) — this keeps
// isolation simple and matches how the CLI is designed to be invoked.
//
// The ACP SessionId is reused as the `claude` CLI session UUID, so prompts
// across turns share state: the first turn passes `--session-id <uuid>` to
// pin it, every subsequent turn passes `--resume <uuid>` to load it.
type session struct {
	id  acp.SessionId
	cwd string

	mu             sync.Mutex
	modelID        string
	effort         string   // "" or "default" means no --effort flag
	mode           string
	additionalDirs []string // forwarded to --add-dir
	resumeFrom     string   // CLI session UUID to --resume from on the next turn
	forkOnResume   bool     // when resumeFrom is set, also pass --fork-session
	started        bool     // true once we've spawned a turn under this id
	cancel         context.CancelFunc // non-nil while a turn is running
}

func newSession(id acp.SessionId, cwd, model, effort string, additionalDirs []string) *session {
	return &session{
		id:             id,
		cwd:            cwd,
		modelID:        model,
		effort:         effort,
		additionalDirs: append([]string(nil), additionalDirs...),
	}
}

// cancelTurn cancels the active turn if any. Safe to call from any goroutine.
func (s *session) cancelTurn() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *session) runTurn(ctx context.Context, conn *acp.AgentSideConnection, path string, env []string, prompt []acp.ContentBlock) (acp.StopReason, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.mu.Lock()
	s.cancel = cancel
	args := s.cliArgsLocked()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
	}()

	cmd := exec.CommandContext(turnCtx, path, args...)
	cmd.Dir = s.cwd
	if env != nil {
		cmd.Env = env
	}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start claude: %w", err)
	}
	// Now that the CLI process owns the session id (or has forked into ours),
	// every future turn must use --resume rather than --session-id/--resume-from.
	s.mu.Lock()
	s.started = true
	s.resumeFrom = ""
	s.forkOnResume = false
	s.mu.Unlock()
	// Signal the process before waiting: on error returns the parent context
	// may still be live, but claude is no longer being read from. Without the
	// cancel, its stdout pipe fills and Wait blocks forever.
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	if err := writePrompt(stdin, prompt); err != nil {
		return "", fmt.Errorf("write prompt: %w", err)
	}
	_ = stdin.Close()

	stop, parseErr := translateStream(turnCtx, conn, s.id, stdout)
	if turnCtx.Err() != nil {
		return acp.StopReasonCancelled, nil
	}
	if parseErr != nil {
		return "", parseErr
	}
	return stop, nil
}

// cliArgsLocked returns the argv for `claude`. Caller must hold s.mu.
//
// Session-pinning rules:
//   - subsequent turn (started): --resume <our-uuid>
//   - first turn, fork: --resume <src> --session-id <our-uuid> --fork-session
//     (pins the forked transcript to the UUID we already handed back to the client)
//   - first turn, resume/load (resumeFrom == our-uuid): --resume <our-uuid>
//   - first turn, fresh: --session-id <our-uuid>
//
// The CLI persists the session to ~/.claude/projects/<cwd>/<uuid>.jsonl, so
// state survives across turns even though each turn is a one-shot process.
func (s *session) cliArgsLocked() []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
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
	if s.modelID != "" && s.modelID != "default" {
		args = append(args, "--model", s.modelID)
	}
	if s.effort != "" && s.effort != "default" {
		args = append(args, "--effort", s.effort)
	}
	if s.mode != "" {
		args = append(args, "--permission-mode", s.mode)
	}
	return args
}

func writePrompt(w io.Writer, blocks []acp.ContentBlock) error {
	in := cliInput{Type: "user", Message: cliInputMessage{Role: "user"}}
	for _, b := range blocks {
		if b.Text != nil {
			in.Message.Content = append(in.Message.Content, cliInputContent{
				Type: "text",
				Text: b.Text.Text,
			})
		}
		// Image / Resource / ResourceLink blocks are dropped: the CLI's
		// stream-json input schema only documents text content.
	}
	return json.NewEncoder(w).Encode(in)
}

// translateStream consumes claude's stdout, emitting ACP session updates and
// returning the final StopReason. Returns early with StopReasonCancelled if
// ctx is cancelled.
func translateStream(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, r io.Reader) (acp.StopReason, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return acp.StopReasonCancelled, nil
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var env cliEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			fmt.Fprintf(os.Stderr, "claude-acp: skipping non-JSON line: %s\n", line)
			continue
		}

		switch env.Type {
		case "assistant":
			if err := emitAssistant(ctx, conn, sid, env.Message); err != nil {
				return "", err
			}
		case "user":
			// tool_result blocks that the CLI echoes back to itself — surface
			// them as ToolCallUpdate completions so the client sees output.
			if err := emitToolResults(ctx, conn, sid, env.Message); err != nil {
				return "", err
			}
		case "result", "system":
			// `result` is the turn terminator; `system` carries init metadata.
			// Nothing actionable for us to surface.
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return "", fmt.Errorf("read claude stdout: %w", err)
	}
	return acp.StopReasonEndTurn, nil
}

func emitAssistant(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var m cliMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("parse assistant message: %w", err)
	}
	for _, b := range m.Content {
		var update acp.SessionUpdate
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			update = acp.UpdateAgentMessageText(b.Text)
		case "thinking":
			if b.Thinking == "" {
				continue
			}
			update = acp.UpdateAgentThoughtText(b.Thinking)
		case "tool_use":
			title := b.Name
			if title == "" {
				title = "Tool call"
			}
			var input map[string]any
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &input)
			}
			update = acp.StartToolCall(
				acp.ToolCallId(b.ID),
				title,
				acp.WithStartKind(toolKindFor(b.Name)),
				acp.WithStartStatus(acp.ToolCallStatusInProgress),
				acp.WithStartRawInput(input),
			)
		default:
			continue
		}
		if err := conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sid,
			Update:    update,
		}); err != nil {
			return err
		}
	}
	return nil
}

func emitToolResults(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var m cliMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil // best-effort; user echoes can be skipped on parse error
	}
	for _, b := range m.Content {
		if b.Type != "tool_result" || b.ToolUseID == "" {
			continue
		}
		status := acp.ToolCallStatusCompleted
		if b.IsError {
			status = acp.ToolCallStatusFailed
		}
		opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(status)}
		if text := extractToolResultText(b.Content); text != "" {
			opts = append(opts, acp.WithUpdateContent([]acp.ToolCallContent{
				acp.ToolContent(acp.TextBlock(text)),
			}))
		}
		if err := conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.UpdateToolCall(acp.ToolCallId(b.ToolUseID), opts...),
		}); err != nil {
			return err
		}
	}
	return nil
}

// extractToolResultText flattens tool_result.content (string OR
// [{type:"text",text:...}, ...]) into a single string.
func extractToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []cliMsgBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		out := ""
		for _, blk := range blocks {
			if blk.Type == "text" {
				out += blk.Text
			}
		}
		return out
	}
	return string(raw)
}

func toolKindFor(name string) acp.ToolKind {
	switch name {
	case "Read", "Glob", "Grep", "WebFetch", "WebSearch":
		return acp.ToolKindRead
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return acp.ToolKindEdit
	case "Bash", "BashOutput", "KillShell":
		return acp.ToolKindExecute
	case "Task":
		return acp.ToolKindThink
	}
	return acp.ToolKindOther
}
