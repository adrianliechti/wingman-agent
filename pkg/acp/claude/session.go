package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
)

type session struct {
	id  acp.SessionId
	cwd string

	mu             sync.Mutex
	modelID        string
	effort         string
	mode           string
	mcpServers     []acp.McpServer
	additionalDirs []string
	resumeFrom     string
	forkOnResume   bool
	started        bool
	lastTitle      string
	cancel         context.CancelFunc
	proc           *claudeProc
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
		s.pushTitleUpdate(ctx, conn)
		return r.stop, r.usage, nil
	case <-p.dead:
		s.dropProc(p)
		return "", nil, fmt.Errorf("claude process exited unexpectedly")
	}
}

// pushTitleUpdate notifies the client when the CLI's auto-generated session
// title has changed since the last time we looked. The CLI has no push event
// for it — it's regenerated in the background and persisted to the session's
// JSONL file — so we read it back at turn end, the same point a new title
// would have landed, and only notify when it actually changed.
func (s *session) pushTitleUpdate(ctx context.Context, conn *acp.AgentSideConnection) {
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
	_ = conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: s.id,
		Update: acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
			SessionUpdate: "session_info_update",
			Title:         &t,
			UpdatedAt:     &updatedAt,
		}},
	})
}

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
		emitted: newToolCallTracker(),
		results: make(chan turnResult, 1),
		dead:    make(chan struct{}),
	}
	go p.read(procCtx, conn, s.id, stdout)

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
	cmd     *exec.Cmd
	out     *streamWriter
	stdin   io.Closer
	sig     string
	kill    context.CancelFunc
	cwd     string
	tools   toolUseCache
	emitted *toolCallTracker
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

	_ = p.stdin.Close()
	select {
	case <-p.dead:
	case <-time.After(5 * time.Second):
		p.kill()
		<-p.dead
	}
	_ = p.cmd.Wait()
}

func (p *claudeProc) read(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, r io.Reader) {
	defer close(p.dead)
	app := &approver{ctx: ctx, conn: conn, sid: sid, out: p.out, cwd: p.cwd, emitted: p.emitted}

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
			if err := emitAssistant(ctx, conn, sid, env.Message, p.cwd, p.tools, p.emitted, true); err != nil {
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
