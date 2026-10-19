package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
)

const (
	defaultExecWait    = 120
	defaultSessionWait = 5
	maxExecWait        = 300
	maxExecSessions    = 16
	maxUnreadBytes     = 1 << 20
	maxExecTimeout     = math.MaxInt64 / int64(time.Second)
)

// execIdleGrace backgrounds a command that printed output then went quiet, as a started server does.
var execIdleGrace = 15 * time.Second

// ExecExit reports the exit of a backgrounded exec session that no
// tool call was waiting on, so the host can deliver it as a notification
// instead of leaving the model to poll.
type ExecExit struct {
	SessionID   int
	Command     string
	Description string
	Output      string
	Notice      string
	Failed      bool
	Elapsed     time.Duration
}

// ExecSessionInfo is a read-only snapshot of a command that exec
// returned while it continued running in the background.
type ExecSessionInfo struct {
	ID          int
	Command     string
	Description string
	Output      string
	Started     time.Time
}

type ExecManager struct {
	onExit func(ExecExit)

	mu       sync.Mutex
	nextID   int
	sessions map[int]*execSession // Nil after Close; no more sessions may be added.
}

func NewExecManager(onExit func(ExecExit)) *ExecManager {
	return &ExecManager{onExit: onExit, sessions: map[int]*execSession{}}
}

// BackgroundSessions returns the live command sessions that are no longer
// owned by a waiting tool call. Callers can expose these alongside other
// background work without draining output needed by exec_session.
func (m *ExecManager) BackgroundSessions() []ExecSessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]ExecSessionInfo, 0, len(m.sessions))
	for _, session := range m.sessions {
		if !session.backgrounded || session.exited() {
			continue
		}
		out = append(out, session.snapshot())
	}
	slices.SortFunc(out, func(a, b ExecSessionInfo) int { return a.ID - b.ID })
	return out
}

// BackgroundSession returns one live background command without consuming its
// buffered output.
func (m *ExecManager) BackgroundSession(id int) (ExecSessionInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[id]
	if session == nil || !session.backgrounded || session.exited() {
		return ExecSessionInfo{}, false
	}
	return session.snapshot(), true
}

// StopBackgroundSession terminates a live command returned by exec.
func (m *ExecManager) StopBackgroundSession(id int) error {
	m.mu.Lock()
	session := m.sessions[id]
	if session == nil || !session.backgrounded || session.exited() {
		m.mu.Unlock()
		return fmt.Errorf("background command session %d not found", id)
	}
	m.mu.Unlock()
	session.cancel()
	return nil
}

func (m *ExecManager) Close() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = nil
	m.mu.Unlock()

	for _, s := range sessions {
		s.cancel()
	}
}

func (m *ExecManager) add(s *execSession) (int, error) {
	m.mu.Lock()

	if m.sessions == nil {
		m.mu.Unlock()
		s.cancel()
		return 0, fmt.Errorf("agent session is closed")
	}

	running := 0
	for _, existing := range m.sessions {
		if !existing.exited() {
			running++
		}
	}
	if running >= maxExecSessions {
		m.mu.Unlock()
		s.cancel()
		return 0, fmt.Errorf("too many running sessions (max %d); kill sessions you no longer need via exec_session", maxExecSessions)
	}

	// Exited sessions linger so late polls can still read their output, but
	// only until the map grows past twice the running cap.
	if len(m.sessions) >= 2*maxExecSessions {
		var exited []int
		for id, existing := range m.sessions {
			if existing.exited() && existing.waiters == 0 {
				exited = append(exited, id)
			}
		}
		slices.Sort(exited)
		for _, id := range exited {
			if len(m.sessions) < 2*maxExecSessions {
				break
			}
			delete(m.sessions, id)
		}
	}

	m.nextID++
	s.id = m.nextID
	m.sessions[s.id] = s
	m.mu.Unlock()

	return s.id, nil
}

func (m *ExecManager) get(id int) *execSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *ExecManager) remove(id int) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

func (m *ExecManager) markBackgrounded(s *execSession) {
	m.mu.Lock()
	s.backgrounded = true
	m.mu.Unlock()
	if s.exited() {
		m.notifyExit(s)
	}
}

// acquireWait claims the session before its exit can be notified or evicted.
func (m *ExecManager) acquireWait(id int) *execSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s != nil {
		s.waiters++
	}
	return s
}

func (m *ExecManager) endWait(s *execSession) {
	m.mu.Lock()
	s.waiters--
	m.mu.Unlock()
	if s.exited() {
		m.notifyExit(s)
	}
}

// notifyExit delivers a backgrounded session's exit as a background event.
// A notified flag claims exactly-once delivery while retaining the result for
// late polls. A waiting tool call reports the exit inline instead
// (endWait re-runs this check for a waiter that woke on its timeout while the
// process exited).
func (m *ExecManager) notifyExit(s *execSession) {
	m.mu.Lock()
	if m.onExit == nil || !s.backgrounded || s.waiters > 0 || s.notified || m.sessions[s.id] != s {
		m.mu.Unlock()
		return
	}
	s.notified = true
	output := s.peek()
	m.mu.Unlock()

	m.onExit(ExecExit{
		SessionID:   s.id,
		Command:     s.command,
		Description: s.description,
		Output:      output,
		Notice:      s.exitNotice(),
		Failed:      s.exitErr != nil,
		Elapsed:     time.Since(s.started),
	})
}

type execSession struct {
	id          int
	sandboxed   bool
	validation  bool
	command     string
	workdir     string
	description string
	started     time.Time
	tty         bool
	cancel      context.CancelFunc
	interrupt   func() error
	stdin       io.WriteCloser

	done     chan struct{}
	exitErr  error
	exitedAt time.Time

	// timeout is the kill deadline in seconds; 0 means none.
	timeout  int
	timedOut atomic.Bool

	// backgrounded and waiters are lifecycle state guarded by the manager mutex:
	// exit notifications fire only for backgrounded sessions no tool call is
	// currently waiting on.
	backgrounded bool
	notified     bool
	waiters      int

	mu          sync.Mutex
	unread      bytes.Buffer
	dropped     int
	lastWrite   time.Time
	outputReady chan struct{}

	inputOnce      sync.Once
	inputGate      chan struct{}
	pendingInput   string
	inputOverflow  bool
	inputUncertain bool
}

func (s *execSession) snapshot() ExecSessionInfo {
	return ExecSessionInfo{
		ID:          s.id,
		Command:     s.command,
		Description: s.description,
		Output:      s.peek(),
		Started:     s.started,
	}
}

func (s *execSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	// Drop before writing, so one large Write cannot allocate an unbounded
	// backing buffer even though only the last MiB is retained.
	if len(p) >= maxUnreadBytes {
		s.dropped += s.unread.Len() + len(p) - maxUnreadBytes
		s.unread.Reset()
		p = p[len(p)-maxUnreadBytes:]
	} else if over := s.unread.Len() + len(p) - maxUnreadBytes; over > 0 {
		s.unread.Next(over)
		s.dropped += over
	}
	s.unread.Write(p)
	s.lastWrite = time.Now()
	if s.outputReady != nil {
		select {
		case s.outputReady <- struct{}{}:
		default:
		}
	}
	return n, nil
}

func (s *execSession) outputAvailable() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outputReady == nil {
		s.outputReady = make(chan struct{}, 1)
	}
	if s.unread.Len() > 0 || s.dropped > 0 {
		select {
		case s.outputReady <- struct{}{}:
		default:
		}
	}
	return s.outputReady
}

// idleFor reports output seen, then quiet for d. drain() does not reset the clock.
func (s *execSession) idleFor(d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.lastWrite.IsZero() && time.Since(s.lastWrite) >= d
}

func (s *execSession) drain() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.unread.Bytes()
	end := len(data)
	if !s.exited() {
		// A read can stop inside a UTF-8 character; retain its unfinished suffix.
		start := max(0, end-utf8.UTFMax+1)
		for start < end && !utf8.RuneStart(data[start]) {
			start++
		}
		for start < end {
			if !utf8.FullRune(data[start:]) {
				end = start
				break
			}
			_, size := utf8.DecodeRune(data[start:])
			start += size
		}
		end = completeTerminalPrefix(data[:end])
	}
	out := sanitizeOutput(string(s.unread.Next(end)))
	if s.unread.Len() == 0 {
		s.unread.Reset()
	}
	select {
	case <-s.outputReady:
	default:
	}
	if s.dropped > 0 {
		out = fmt.Sprintf("[%d bytes of earlier output dropped]\n", s.dropped) + out
		s.dropped = 0
	}
	return out
}

func (s *execSession) peek() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := sanitizeOutput(s.unread.String())
	if s.dropped > 0 {
		out = fmt.Sprintf("[%d bytes of earlier output dropped]\n", s.dropped) + out
	}
	return out
}

// sanitizeOutput resolves captured terminal control flow into plain text:
// escape sequences are stripped and a carriage-return overwritten line keeps
// its final content. TUI programs on a PTY otherwise fill results with cursor
// noise that neither the model nor any UI can read.
func sanitizeOutput(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if !strings.ContainsAny(s, "\x1b\r") {
		return s
	}

	s = ansi.Strip(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")

	if !strings.ContainsRune(s, '\r') {
		return s
	}

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if !strings.ContainsRune(line, '\r') {
			continue
		}
		segments := strings.Split(line, "\r")
		keep := ""
		for _, segment := range slices.Backward(segments) {
			if segment != "" {
				keep = segment
				break
			}
		}
		lines[i] = keep
	}
	return strings.Join(lines, "\n")
}

// Keep an unfinished escape sequence with the next read; stripping each read
// independently would turn a split "ESC [ 31" / "m" into visible "m" noise.
func completeTerminalPrefix(data []byte) int {
	for i := 0; i < len(data); i++ {
		if data[i] != 0x1b {
			continue
		}
		start := i
		i++
		if i == len(data) {
			return start
		}
		switch data[i] {
		case '[':
			for i++; i < len(data) && (data[i] < 0x40 || data[i] > 0x7e); i++ {
			}
			if i == len(data) {
				return start
			}
		case ']':
			for i++; i < len(data); i++ {
				if data[i] == 0x07 {
					break
				}
				if data[i] == 0x1b && i+1 < len(data) && data[i+1] == '\\' {
					i++
					break
				}
			}
			if i == len(data) {
				return start
			}
		}
	}
	return len(data)
}

func (s *execSession) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *execSession) exitNotice() string {
	if s.timedOut.Load() {
		return fmt.Sprintf("Command killed after exceeding its %ds timeout", s.timeout)
	}
	if s.exitErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](s.exitErr); ok {
			if code := exitErr.ExitCode(); code >= 0 {
				return fmt.Sprintf("Command exited with code %d", code)
			}
			return fmt.Sprintf("Command terminated (%s)", exitErr.ProcessState.String())
		}
		return fmt.Sprintf("Command failed to run: %v", s.exitErr)
	}
	return "Command completed"
}

func ExecTools(manager *ExecManager, workDir string, elicit *tool.Elicitation, opts *Options) []tool.Tool {
	commandLines := []string{
		fmt.Sprintf("Run a command in the host shell. Waits up to `wait` seconds (default %d) for it to finish; if still running, the command keeps running in the background and a session_id is returned.", defaultExecWait),
		"- Use directly for both short commands and long-running or interactive work such as dev servers, watch tasks, log tails, REPLs, and prompting CLIs.",
		fmt.Sprintf("- Commands that finish within the wait return their exit status inline. A command that prints output and then stays quiet for %.0fs is handed back as a running session; silence does not establish readiness or success.", execIdleGrace.Seconds()),
		"- Set `timeout` for anything that could hang (network calls, flaky suites, unfamiliar binaries) — the process is killed at that deadline. Omit it for servers and watch tasks you intend to keep running.",
		"- Set `tty` for programs that need a terminal (REPLs, prompts, programs that buffer output when piped). Unix only — ignored on Windows. Written input is echoed back in the output.",
		"- Starts in the workspace directory (override with `workdir`). Shell state does not carry over between calls. stdout and stderr are merged. Output between reads is buffered (oldest dropped past 1MB); poll with `exec_session` to collect it.",
		"- Not for file content work: use `read`/`edit`/`grep`/`glob` instead of cat, head, tail, sed, awk, echo-redirects, or find.",
		"- The process keeps running when the wait elapses or a waiting call is cancelled. Kill sessions you no longer need via `exec_session`.",
	}

	if manager.onExit != nil {
		commandLines = append(commandLines,
			"- When a backgrounded command exits on its own, its remaining output arrives automatically as a background notification — keep working instead of polling `exec_session` just to watch for the exit.",
			"- Completed sessions are retained temporarily for a final `exec_session` read, including after notifications. Collect a background validation command's exit status this way before reporting it as checked.",
		)
	}

	commandLines = append(commandLines, safetyGuardLine(elicit))
	properties := map[string]any{
		"command":     map[string]any{"type": "string", "description": "Command to run."},
		"validation":  map[string]any{"type": "boolean", "description": "Set true for a test, build, lint or other check of the changes. The review records its actual exit status; use false for ordinary commands."},
		"description": map[string]any{"type": "string", "description": "Short label (e.g. \"Start dev server\")."},
		"workdir":     map[string]any{"type": "string", "description": "Directory to run the command in (absolute, or relative to the workspace). Defaults to the workspace root."},
		"tty":         map[string]any{"type": "boolean", "description": "Run in a pseudo-terminal (Unix only)."},
		"wait":        map[string]any{"type": "integer", "minimum": 0, "description": fmt.Sprintf("Seconds to wait before backgrounding (default %d, capped at %d; 0 backgrounds immediately).", defaultExecWait, maxExecWait)},
		"timeout":     map[string]any{"type": "integer", "minimum": 0, "maximum": maxExecTimeout, "description": "Seconds after which the command is killed. Omit or set 0 to let it run until it exits or the agent session closes."},
	}
	if workspaceSandboxEnabled(opts) {
		commandLines = append(commandLines, sandboxGuideLine(opts))
		properties["sandbox_permissions"] = map[string]any{"type": "string", "enum": []string{"use_default", "require_permissions", "require_escalated"}, "description": "require_permissions grants network or additional writable roots for one command after review while retaining the sandbox. require_escalated runs outside the sandbox after review."}
		properties["network"] = map[string]any{"type": "boolean", "description": "Request IP network access with require_permissions."}
		properties["writable_roots"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Existing directories to make writable with require_permissions. Protected configuration and credentials stay restricted."}
		properties["justification"] = map[string]any{"type": "string", "description": "Why this command needs the requested permissions."}
	}
	commandDescription := strings.Join(commandLines, "\n")

	sessionDescription := strings.Join([]string{
		"Interact with a session started by `exec`: poll new output (no input), write to its stdin (`input`), close stdin (`eof`), or terminate it (`kill`).",
		"- Waits up to `wait` seconds for output (or exit) before returning.",
		"- `input` supports C-style escapes (\\n Enter, \\e Esc, \\t, \\uHHHH; \\\\ for a literal backslash); anything else is sent verbatim with nothing appended. Include \\n to submit a line to an interactive program, else it is typed but not entered (e.g. save in vi with \"\\e:w file\\n\"). Destructive or privilege-escalating input lines require the same user confirmation as shell commands.",
		"- \\u0003 (Ctrl-C) interrupts the process: on tty sessions via the terminal, otherwise via SIGINT to the process group (Unix only).",
		"- On tty sessions, `eof` sends Ctrl-D instead of closing stdin.",
		"- Cancelling a blocked stdin write requests termination and closes stdin, because some input may already have been delivered. Poll cancellation alone leaves the command running.",
		"- Destructive input is confirmed for each submission; approvals are not reused across changing interactive process state.",
		"- Sessions end when the process exits, is killed, or the agent session closes.",
	}, "\n")

	return []tool.Tool{
		{
			Name:        "exec",
			Description: commandDescription,
			Effect:      ClassifyEffect,

			Parameters: map[string]any{
				"type":       "object",
				"properties": properties,

				"required":             []string{"command"},
				"additionalProperties": false,
			},

			Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
				return executeExecCommand(ctx, manager, workDir, elicit, opts, args)
			},
		},
		{
			Name:        "exec_session",
			Description: sessionDescription,
			Effect:      classifyExecSession,

			Parameters: map[string]any{
				"type": "object",

				"properties": map[string]any{
					"session_id": map[string]any{"type": "integer", "minimum": 1, "description": "Session id returned by exec."},
					"input":      map[string]any{"type": "string", "description": "Text to write to the process stdin."},
					"eof":        map[string]any{"type": "boolean", "description": "Close stdin after writing input."},
					"kill":       map[string]any{"type": "boolean", "description": "Terminate the process."},
					"wait":       map[string]any{"type": "integer", "minimum": 0, "description": fmt.Sprintf("Seconds to wait for output before returning (default %d, capped at %d; 0 returns immediately).", defaultSessionWait, maxExecWait)},
				},

				"required":             []string{"session_id"},
				"additionalProperties": false,
			},

			Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
				return executeExecSession(ctx, manager, elicit, args)
			},
		},
	}
}

func executeExecCommand(ctx context.Context, m *ExecManager, workDir string, elicit *tool.Elicitation, opts *Options, args map[string]any) (tool.Result, error) {
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	if err := validateExecArgs(args, []string{"command", "description", "workdir", "sandbox_permissions", "justification"}, []string{"tty", "validation", "network"}); err != nil {
		return tool.Result{}, err
	}
	command, ok := args["command"].(string)

	if !ok || strings.TrimSpace(command) == "" {
		return tool.Result{}, fmt.Errorf("command is required")
	}

	wait := defaultExecWait
	if value, present, err := tool.NonNegIntArg(args, "wait"); present {
		if err != nil {
			return tool.Result{}, err
		}
		wait = value
	}
	if wait > maxExecWait {
		wait = maxExecWait
	}

	timeout := 0
	if value, present, err := tool.NonNegIntArg(args, "timeout"); present {
		if err != nil {
			return tool.Result{}, err
		}
		timeout = value
	}
	if int64(timeout) > maxExecTimeout {
		return tool.Result{}, fmt.Errorf("timeout must not exceed %d seconds", maxExecTimeout)
	}

	opts, permissionMode, err := executionOptions(workDir, args, opts)
	if err != nil {
		return tool.Result{}, err
	}
	dir, err := resolveWorkdir(workDir, args, opts)
	if err != nil {
		return tool.Result{}, err
	}

	sandboxed := workspaceSandboxEnabled(opts)
	escalated := sandboxed && permissionMode == "require_escalated"
	justification, _ := args["justification"].(string)
	err = confirmExecution(ctx, elicit, command, workDir, dir, permissionMode, justification, opts)
	if err != nil {
		return tool.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}

	tty, _ := args["tty"].(bool)
	if runtime.GOOS == "windows" {
		tty = false
	}

	sctx, cancel := context.WithCancel(context.Background())

	cmd, err := buildToolCommand(sctx, command, workDir, dir, opts, escalated)
	if err != nil {
		cancel()
		return tool.Result{}, err
	}
	cmd.Env = setEnvironment(cmd.Env, "NO_COLOR", "1")
	cmd.Env = setEnvironment(cmd.Env, "PAGER", "cat")
	cmd.Env = setEnvironment(cmd.Env, "GIT_PAGER", "cat")

	description, _ := args["description"].(string)
	validation, _ := args["validation"].(bool)

	s := &execSession{
		sandboxed:   sandboxed && !escalated,
		validation:  validation,
		command:     command,
		workdir:     dir,
		description: strings.TrimSpace(description),
		started:     time.Now(),
		tty:         tty,
		timeout:     timeout,
		cancel:      cancel,
		interrupt:   func() error { return interruptProcessGroup(cmd) },
		done:        make(chan struct{}),
	}
	// Reserve capacity before starting the process. Rejected calls must never
	// execute a command, and Close must be able to cancel a concurrent launch.
	id, err := m.add(s)
	if err != nil {
		return tool.Result{}, err
	}
	started := false
	defer func() {
		if !started {
			cancel()
			m.remove(id)
		}
	}()

	if tty {
		master, err := startTTY(cmd)
		if err != nil {
			cancel()
			return tool.Result{}, fmt.Errorf("failed to start command: %w", err)
		}
		s.stdin = master

		copyDone := make(chan struct{})
		go func() {
			io.Copy(s, master)
			close(copyDone)
		}()
		go func() {
			defer cancel()
			err := cmd.Wait()
			exitedAt := time.Now()
			select {
			case <-copyDone:
			case <-time.After(2 * time.Second):
			}
			master.Close()
			s.exitErr = err
			s.exitedAt = exitedAt
			close(s.done)
			m.notifyExit(s)
		}()
	} else {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			cancel()
			return tool.Result{}, err
		}
		s.stdin = stdin
		cmd.Stdout = s
		cmd.Stderr = s

		if err := cmd.Start(); err != nil {
			cancel()
			return tool.Result{}, fmt.Errorf("failed to start command: %w", err)
		}

		go func() {
			defer cancel()
			s.exitErr = cmd.Wait()
			s.exitedAt = time.Now()
			close(s.done)
			m.notifyExit(s)
		}()
	}
	started = true

	if timeout > 0 {
		go func() {
			t := time.NewTimer(time.Duration(timeout) * time.Second)
			defer t.Stop()
			select {
			case <-s.done:
			case <-t.C:
				if !s.exited() {
					s.timedOut.Store(true)
					s.cancel()
				}
			}
		}()
	}

	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()

	grace := execIdleGrace

	var idleTick <-chan time.Time
	if grace > 0 {
		idle := time.NewTicker(min(time.Second, grace))
		defer idle.Stop()
		idleTick = idle.C
	}

	idled := false

waiting:
	for {
		select {
		case <-s.done:
			m.remove(id)
			return completedSessionResult(s), nil
		case <-timer.C:
			break waiting
		case <-ctx.Done():
			break waiting
		case <-idleTick:
			if s.idleFor(grace) {
				idled = true
				break waiting
			}
		}
	}

	if s.exited() {
		m.remove(id)
		return completedSessionResult(s), nil
	}

	output := s.drain()
	m.markBackgrounded(s)

	notice := fmt.Sprintf("Still running with session_id %d — use exec_session to poll output, send input, or kill it", id)
	if idled {
		notice = fmt.Sprintf("Started and idle (no new output for %s), running with session_id %d — use exec_session to poll output, send input, or kill it", grace, id)
	}
	return s.result(sessionResult(output, notice)), nil
}

func executeExecSession(ctx context.Context, m *ExecManager, elicit *tool.Elicitation, args map[string]any) (tool.Result, error) {
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	if err := validateExecArgs(args, []string{"input"}, []string{"eof", "kill"}); err != nil {
		return tool.Result{}, err
	}
	id, present, err := tool.PositiveIntArg(args, "session_id")
	if err != nil {
		return tool.Result{}, err
	}
	if !present {
		return tool.Result{}, fmt.Errorf("session_id is required")
	}

	wait := defaultSessionWait
	if value, present, err := tool.NonNegIntArg(args, "wait"); present {
		if err != nil {
			return tool.Result{}, err
		}
		wait = value
	}
	if wait > maxExecWait {
		wait = maxExecWait
	}
	s := m.acquireWait(id)
	if s == nil {
		return tool.Result{}, fmt.Errorf("no session with id %d (it may have exited and been cleaned up)", id)
	}
	defer m.endWait(s)

	if kill, _ := args["kill"].(bool); kill {
		alreadyExited := s.exited()
		s.cancel()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}

		if s.exited() {
			m.remove(id)
			result := completedSessionResult(s)
			if !alreadyExited && s.exitErr != nil {
				result.Content += fmt.Sprintf("\nSession %d killed", id)
			}
			return result, nil
		}
		m.markBackgrounded(s)
		notice := fmt.Sprintf("Session %d kill signalled; the process has not exited yet", id)
		return s.result(sessionResult(s.drain(), notice)), nil
	}

	input, _ := args["input"].(string)
	input = decodeInput(input)
	if s.exited() {
		m.remove(id)
		result := completedSessionResult(s)
		if input != "" {
			result.Content += " (input was not delivered)"
		}
		return result, nil
	}

	eof, _ := args["eof"].(bool)
	if result, err := applySessionInput(ctx, m, s, elicit, input, eof); err != nil || result != nil {
		if result != nil {
			return *result, err
		}
		return tool.Result{}, err
	}

	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()

	select {
	case <-s.done:
		m.remove(id)
		return completedSessionResult(s), nil
	case <-timer.C:
	case <-ctx.Done():
	case <-s.outputAvailable():
		// Coalesce a short burst and let the process reaper report a command
		// that printed its final line immediately before exiting.
		if wait > 0 {
			settle := time.NewTimer(20 * time.Millisecond)
			select {
			case <-s.done:
			case <-timer.C:
			case <-ctx.Done():
			case <-settle.C:
			}
			settle.Stop()
		}
	}

	if s.exited() {
		m.remove(id)
		return completedSessionResult(s), nil
	}

	output := s.drain()
	if output == "" {
		return s.result(fmt.Sprintf("(no new output; session %d still running)", id)), nil
	}
	return s.result(sessionResult(output, fmt.Sprintf("Session %d still running", id))), nil
}

func writeSessionInput(ctx context.Context, m *ExecManager, id int, s *execSession, elicit *tool.Elicitation, input string) (*tool.Result, error) {
	if err := s.lockInput(ctx); err != nil {
		return nil, err
	}
	defer s.unlockInput()
	return writeSessionInputLocked(ctx, m, id, s, elicit, input)
}

func writeSessionInputLocked(ctx context.Context, m *ExecManager, id int, s *execSession, elicit *tool.Elicitation, input string) (*tool.Result, error) {
	// Serialize input writes and retain the current logical line. Without this,
	// an agent can submit "r" and "m -rf ...\n" in separate calls while each
	// individual call looks harmless to the classifier.
	combined := s.pendingInput + input
	if s.tty {
		combined = applyInputEditing(s.pendingInput, input)
	}
	submitted, remainder := splitSubmittedInput(combined)
	immediateControl := hasImmediateTerminalControl(input)
	dangerous := immediateControl || s.inputUncertain
	approvalText := strings.TrimRight(submitted, "\r\n")
	if dangerous && approvalText == "" {
		approvalText = s.pendingInput + input
	}
	if immediateControl {
		approvalText += "  [input contains an active terminal control]"
	}
	if s.inputUncertain {
		approvalText += "  [terminal input state is unknown]"
	}
	if submitted != "" {
		overflowed := s.inputOverflow || len(submitted) > maxClassifiableBytes
		dangerous = dangerous || overflowed || IsDangerousCommand(submitted)
		if overflowed {
			approvalText += "  [input line exceeded classifier limit]"
		}
	}
	// An interactive process can change cwd, aliases, or interpreter between
	// writes. An earlier approval for identical input is not reusable.
	if err := confirmIfDangerous(ctx, elicit, "terminal_input", approvalText, dangerous); err != nil {
		return nil, err
	}

	if err := s.writeInput(ctx, input); err != nil {
		s.inputUncertain = true
		if ctx.Err() != nil {
			return nil, err
		}
		if s.exited() {
			m.remove(id)
			result := completedSessionResult(s)
			result.Content += " (input was not delivered)"
			return &result, nil
		}
		return nil, fmt.Errorf("failed to write to stdin: %w", err)
	}

	s.inputOverflow = (s.inputOverflow && submitted == "") || len(remainder) > maxClassifiableBytes
	if len(remainder) > maxClassifiableBytes {
		remainder = remainder[:maxClassifiableBytes]
		for len(remainder) > 0 && !utf8.ValidString(remainder) {
			remainder = remainder[:len(remainder)-1]
		}
	}
	s.pendingInput = remainder
	s.inputUncertain = terminalInputUncertainAfter(s.inputUncertain, input)
	return nil, nil
}

func confirmPendingSessionInputLocked(ctx context.Context, s *execSession, elicit *tool.Elicitation) error {
	if s.pendingInput == "" && !s.inputOverflow && !s.inputUncertain {
		return nil
	}
	text := s.pendingInput
	if s.inputOverflow {
		text += "  [input line exceeded classifier limit]"
	}
	if s.inputUncertain {
		text += "  [terminal input state is unknown]"
	}
	return confirmIfDangerous(ctx, elicit, "terminal_input", text, s.inputOverflow || s.inputUncertain || IsDangerousCommand(s.pendingInput))
}

func hasImmediateTerminalControl(input string) bool {
	for _, r := range input {
		if isUnmodelledTerminalControl(r) {
			return true
		}
	}
	return false
}

func isUnmodelledTerminalControl(r rune) bool {
	switch r {
	case '\n', '\r', '\b', '\x7f', '\x15':
		return false
	}
	return r < ' '
}

// A newline after an unmodelled key submits the unknown readline buffer and
// restores a known empty line. A later control key makes it uncertain again.
func terminalInputUncertainAfter(uncertain bool, input string) bool {
	for _, r := range input {
		switch {
		case r == '\n' || r == '\r':
			uncertain = false
		case isUnmodelledTerminalControl(r):
			uncertain = true
		}
	}
	return uncertain
}

func applyInputEditing(pending, input string) string {
	line := []rune(pending)
	for _, r := range input {
		switch r {
		case '\b', '\x7f':
			if len(line) > 0 && line[len(line)-1] != '\n' && line[len(line)-1] != '\r' {
				line = line[:len(line)-1]
			}
		case '\x15': // Ctrl-U: erase back to the current line boundary.
			for len(line) > 0 && line[len(line)-1] != '\n' && line[len(line)-1] != '\r' {
				line = line[:len(line)-1]
			}
		default:
			line = append(line, r)
		}
	}
	return string(line)
}

func splitSubmittedInput(input string) (submitted, remainder string) {
	last := strings.LastIndexAny(input, "\r\n")
	if last < 0 {
		return "", input
	}
	return input[:last+1], input[last+1:]
}

func (s *execSession) result(content string) tool.Result {
	return tool.Result{
		Content: content,
		Metadata: map[string]any{
			"session_id": s.id,
			"validation": s.validation,
			"command":    s.command,
			"workdir":    s.workdir,
		},
	}
}

func completedSessionResult(s *execSession) tool.Result {
	duration := s.exitedAt.Sub(s.started)
	if s.exitedAt.IsZero() {
		duration = time.Since(s.started)
	}
	output, notice := s.drain(), s.exitNotice()
	if s.sandboxed && looksLikeSandboxDenial(output, s.exitErr) {
		notice += "\nThe sandbox may have blocked this command. Request the needed access with sandbox_permissions \"require_permissions\", network or writable_roots, and a justification. Use require_escalated only when the sandbox itself must be removed."
	}
	result := s.result(sessionResult(output, notice))
	result.IsError = s.exitErr != nil
	result.Metadata["duration_ms"] = duration.Milliseconds()
	if exitErr, ok := errors.AsType[*exec.ExitError](s.exitErr); ok {
		result.Metadata["exit_code"] = exitErr.ExitCode()
	} else if s.exitErr == nil {
		result.Metadata["exit_code"] = 0
	}
	return result
}

func classifyExecSession(args map[string]any) tool.Effect {
	if args == nil {
		return tool.EffectDynamic
	}
	if input, _ := args["input"].(string); input != "" {
		decoded := decodeInput(input)
		if decoded != "\x03" && (IsDangerousCommand(decoded) || hasImmediateTerminalControl(decoded)) {
			return tool.EffectDangerous
		}
	}
	if kill, _ := args["kill"].(bool); kill {
		return tool.EffectMutates
	}
	if eof, _ := args["eof"].(bool); eof {
		return tool.EffectMutates
	}
	if input, _ := args["input"].(string); input != "" {
		return tool.EffectMutates
	}
	return tool.EffectReadOnly
}

func sessionResult(output, notice string) string {
	if output == "" {
		return notice
	}
	return output + "\n\n" + notice
}

// decodeInput turns C-style escape sequences in interactive input into their
// real bytes, so models that emit escape TEXT like \u001b or \n instead of the
// actual control bytes still drive interactive programs. strconv.UnquoteChar
// handles the standard escapes; \e (Esc) is the only common alias it lacks.
// Unrecognized escapes keep their backslash so regexes and paths survive.
func decodeInput(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			i++
			continue
		}

		if i+1 < len(s) && s[i+1] == 'e' {
			b.WriteByte(0x1b)
			i += 2
			continue
		}

		r, multibyte, tail, err := strconv.UnquoteChar(s[i:], 0)
		if err != nil {
			b.WriteByte(s[i])
			i++
			continue
		}

		if multibyte {
			b.WriteRune(r)
		} else {
			b.WriteByte(byte(r))
		}
		i = len(s) - len(tail)
	}

	return b.String()
}
