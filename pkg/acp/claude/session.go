package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
)

// session holds per-conversation state. A single long-lived `claude` process
// (streaming stdio mode) serves every turn of the session: user messages are
// written to its stdin and it keeps conversation state in memory, so turns are
// fast and need no per-turn --resume reload. The process is (re)spawned lazily
// and respawned (with --resume, restoring on-disk state) when the model/effort/
// mode change or it dies.
//
// The ACP SessionId is reused as the `claude` CLI session UUID.
type session struct {
	id  acp.SessionId
	cwd string

	mu             sync.Mutex
	modelID        string
	effort         string   // "" or "default" means no --effort flag
	mode           string
	mcpServers     []acp.McpServer // forwarded via --mcp-config
	additionalDirs []string        // forwarded to --add-dir
	resumeFrom     string   // CLI session UUID to --resume from on first spawn
	forkOnResume   bool     // when resumeFrom is set, also pass --fork-session
	started        bool     // true once the process has been spawned under this id
	cancel         context.CancelFunc // interrupts the active turn; nil when idle
	proc           *claudeProc        // live streaming process; nil when not running
}

func newSession(id acp.SessionId, cwd, model, effort string, additionalDirs []string) *session {
	return &session{
		id:             id,
		cwd:            cwd,
		modelID:        model,
		effort:         effort,
		mode:           defaultModeID,
		additionalDirs: append([]string(nil), additionalDirs...),
	}
}

// cancelTurn interrupts the active turn (if any) without killing the process,
// so the session stays warm for the next turn. Safe to call from any goroutine.
func (s *session) cancelTurn() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// close terminates the session's process. Used on session close/delete.
func (s *session) close() {
	s.mu.Lock()
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
}

func (s *session) runTurn(ctx context.Context, conn *acp.AgentSideConnection, path string, env []string, prompt []acp.ContentBlock) (acp.StopReason, *acp.Usage, error) {
	p, err := s.ensureProc(conn, path, env)
	if err != nil {
		return "", nil, err
	}

	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
	}()

	if err := p.out.writeJSON(promptMessage(prompt)); err != nil {
		s.dropProc(p)
		return "", nil, fmt.Errorf("write prompt: %w", err)
	}

	select {
	case <-turnCtx.Done():
		// User cancelled: interrupt the turn but keep the process warm. Drain
		// the turn's terminating result so the stream stays in sync. If the
		// interrupt stalls, discard the process so a late result can't leak
		// into the next turn (the next turn respawns with --resume).
		_ = p.out.writeJSON(interruptRequest())
		select {
		case <-p.results:
		case <-p.dead:
		case <-time.After(5 * time.Second):
			s.dropProc(p)
		}
		return acp.StopReasonCancelled, nil, nil
	case r := <-p.results:
		if r.err != nil {
			s.dropProc(p)
			return "", nil, r.err
		}
		return r.stop, r.usage, nil
	case <-p.dead:
		s.dropProc(p)
		return "", nil, fmt.Errorf("claude process exited unexpectedly")
	}
}

// ensureProc returns the session's live process, spawning (or respawning) it
// when absent, dead, or started under a now-stale config (model/effort/mode).
func (s *session) ensureProc(conn *acp.AgentSideConnection, path string, env []string) (*claudeProc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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
	cmd := exec.CommandContext(procCtx, path, args...)
	cmd.Dir = s.cwd
	if env != nil {
		cmd.Env = env
	}
	cmd.Stderr = os.Stderr
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
		cmd:     cmd,
		out:     &streamWriter{w: stdin},
		stdin:   stdin,
		sig:     sig,
		kill:    kill,
		cwd:     s.cwd,
		tools:   toolUseCache{},
		results: make(chan turnResult, 1),
		dead:    make(chan struct{}),
	}
	go p.read(procCtx, conn, s.id, stdout)

	// The process now owns the on-disk session id; future respawns resume it.
	s.started = true
	s.resumeFrom = ""
	s.forkOnResume = false
	s.proc = p
	return p, nil
}

// dropProc tears down p and clears it from the session if still current, so the
// next turn respawns.
func (s *session) dropProc(p *claudeProc) {
	p.shutdown()
	s.mu.Lock()
	if s.proc == p {
		s.proc = nil
	}
	s.mu.Unlock()
}

// spawnSigLocked is the config fingerprint a running process was started with;
// a change forces a respawn. Caller must hold s.mu.
func (s *session) spawnSigLocked() string {
	return strings.Join(append([]string{s.modelID, s.effort, s.mode}, s.additionalDirs...), "\x00")
}

// claudeProc is one long-lived streaming `claude` process. A single reader
// goroutine drains its stdout for the whole lifetime, delivering each turn's
// terminating result on results and closing dead when the process exits.
type claudeProc struct {
	cmd     *exec.Cmd
	out     *streamWriter
	stdin   io.Closer
	sig     string
	kill    context.CancelFunc
	cwd     string
	tools   toolUseCache
	results chan turnResult
	dead    chan struct{}
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
	// Close stdin first: streaming mode exits on EOF and flushes its on-disk
	// session before doing so. Killing first (context cancel = SIGKILL / Windows
	// TerminateProcess) skips that flush, which on Windows drops every turn and
	// leaves a session file with only its title.
	_ = p.stdin.Close()
	select {
	case <-p.dead:
	case <-time.After(5 * time.Second):
		p.kill()
		<-p.dead
	}
	_ = p.cmd.Wait()
}

// read drains stdout for the process lifetime: assistant/user events become ACP
// updates, can_use_tool control requests are bridged to permission prompts, and
// each `result` is delivered to the waiting turn. Closing dead signals exit.
func (p *claudeProc) read(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, r io.Reader) {
	defer close(p.dead)
	app := &approver{ctx: ctx, conn: conn, sid: sid, out: p.out}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
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
		case "stream_event":
			if err := emitStreamEvent(ctx, conn, sid, env.Event); err != nil {
				fmt.Fprintf(os.Stderr, "claude-acp: emit stream event: %v\n", err)
			}
		case "assistant":
			if err := emitAssistant(ctx, conn, sid, env.Message, p.cwd, p.tools, true); err != nil {
				fmt.Fprintf(os.Stderr, "claude-acp: emit assistant: %v\n", err)
			}
		case "user":
			if err := emitToolResults(ctx, conn, sid, env.Message, p.tools); err != nil {
				fmt.Fprintf(os.Stderr, "claude-acp: emit tool result: %v\n", err)
			}
		case "control_request":
			var req controlRequest
			if json.Unmarshal(line, &req) == nil {
				go app.handle(req)
			}
		case "result":
			tr, usageUpd := resultToTurn(line)
			if usageUpd != nil {
				_ = conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: *usageUpd})
			}
			select {
			case p.results <- tr:
			default:
			}
		}
	}
}

// resultToTurn maps a `result` line to a turn outcome: recoverable
// terminations become a StopReason, failures become a *RequestError. It also
// derives the turn's token usage and, when a context window is known, a
// usage_update for the caller to emit.
func resultToTurn(line []byte) (turnResult, *acp.SessionUpdate) {
	var r cliResult
	_ = json.Unmarshal(line, &r)

	tr := resultOutcome(r)
	tr.usage = resultUsage(r)
	return tr, usageUpdate(r, tr.usage)
}

func resultOutcome(r cliResult) turnResult {
	if strings.Contains(r.Result, "Please run /login") {
		return turnResult{err: acp.NewAuthRequired(nil)}
	}
	switch r.Subtype {
	case "success", "error_during_execution":
		if r.StopReason == "max_tokens" {
			return turnResult{stop: acp.StopReasonMaxTokens}
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

// resultUsage builds the ACP Usage from the result line's token counts.
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

// usageUpdate builds a usage_update session update: `used` is the turn's total
// token footprint, `size` the active model's context window (the widest one
// the result reports, so the main model dominates over any subagent's).
func usageUpdate(r cliResult, usage *acp.Usage) *acp.SessionUpdate {
	if usage == nil {
		return nil
	}
	size := 0
	for _, mu := range r.ModelUsage {
		if mu.ContextWindow > size {
			size = mu.ContextWindow
		}
	}
	if size == 0 {
		return nil
	}
	upd := &acp.SessionUsageUpdate{SessionUpdate: "usage_update", Used: usage.TotalTokens, Size: size}
	if r.TotalCostUSD > 0 {
		upd.Cost = &acp.Cost{Amount: r.TotalCostUSD, Currency: "USD"}
	}
	return &acp.SessionUpdate{UsageUpdate: upd}
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
		RequestID: newUUID(),
		Request:   controlInterruptBody{Subtype: "interrupt"},
	}
}

// streamWriter serializes newline-delimited JSON writes to the CLI's stdin.
// The prompt and any control responses (from concurrent permission handlers)
// share this writer, so writes are mutex-guarded.
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
	// Streaming mode (no --print): the process reads user messages from stdin
	// and stays alive until stdin closes, which lets the stdio control protocol
	// answer tool-permission prompts mid-turn. --permission-prompt-tool stdio
	// routes those approvals over the same channel.
	args := []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-prompt-tool", "stdio",
		// AskUserQuestion has no ACP representation; disable it (matches the
		// reference) so it doesn't surface as an unanswerable generic tool.
		"--disallowed-tools", "AskUserQuestion",
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
	if s.modelID != "" && s.modelID != "default" {
		args = append(args, "--model", s.modelID)
	}
	if s.effort != "" && s.effort != "default" {
		args = append(args, "--effort", s.effort)
	}
	mode := s.mode
	if mode == "" {
		mode = defaultModeID
	}
	args = append(args, "--permission-mode", mode)
	return args
}

// promptMessage builds the stream-json user message from ACP content blocks:
// text verbatim, base64 images as image source blocks, resource_links as
// markdown links, and embedded text resources wrapped in a <context> block
// (mirroring the reference's promptToClaude).
func promptMessage(blocks []acp.ContentBlock) cliInput {
	in := cliInput{Type: "user", Message: cliInputMessage{Role: "user"}}
	add := func(c cliInputContent) { in.Message.Content = append(in.Message.Content, c) }
	for _, b := range blocks {
		switch {
		case b.Text != nil:
			add(cliInputContent{Type: "text", Text: b.Text.Text})
		case b.Image != nil && b.Image.Data != "":
			add(cliInputContent{Type: "image", Source: &cliImageSource{
				Type:      "base64",
				MediaType: b.Image.MimeType,
				Data:      b.Image.Data,
			}})
		case b.ResourceLink != nil:
			add(cliInputContent{Type: "text", Text: fmt.Sprintf("[@%s](%s)", b.ResourceLink.Name, b.ResourceLink.Uri)})
		case b.Resource != nil && b.Resource.Resource.TextResourceContents != nil:
			r := b.Resource.Resource.TextResourceContents
			add(cliInputContent{Type: "text", Text: fmt.Sprintf("\n<context ref=%q>\n%s\n</context>", r.Uri, r.Text)})
		}
	}
	return in
}
