package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const (
	defaultExecWait    = 10
	defaultSessionWait = 5
	maxExecWait        = 300
	maxExecSessions    = 16
	maxUnreadBytes     = 1 << 20
)

type ExecManager struct {
	mu       sync.Mutex
	nextID   int
	sessions map[int]*execSession
}

func NewExecManager() *ExecManager {
	return &ExecManager{sessions: map[int]*execSession{}}
}

func (m *ExecManager) Close() {
	m.mu.Lock()
	sessions := make([]*execSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = map[int]*execSession{}
	m.mu.Unlock()

	for _, s := range sessions {
		s.cancel()
	}
}

func (m *ExecManager) add(s *execSession) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	s.id = m.nextID
	m.sessions[s.id] = s
	return s.id
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

func (m *ExecManager) runningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, s := range m.sessions {
		if !s.exited() {
			count++
		}
	}
	return count
}

type execSession struct {
	id      int
	command string
	tty     bool
	cancel  context.CancelFunc
	stdin   io.WriteCloser

	done    chan struct{}
	exitErr error

	mu      sync.Mutex
	unread  bytes.Buffer
	dropped int
}

func (s *execSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unread.Write(p)
	if over := s.unread.Len() - maxUnreadBytes; over > 0 {
		s.unread.Next(over)
		s.dropped += over
	}
	return len(p), nil
}

func (s *execSession) drain() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.unread.String()
	s.unread.Reset()
	if s.dropped > 0 {
		out = fmt.Sprintf("[%d bytes of earlier output dropped]\n", s.dropped) + out
		s.dropped = 0
	}
	return out
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
	if s.exitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(s.exitErr, &exitErr) {
			return fmt.Sprintf("Command exited with code %d", exitErr.ExitCode())
		}
		return fmt.Sprintf("Command failed to run: %v", s.exitErr)
	}
	return "Command completed"
}

func ExecTools(manager *ExecManager, workDir string, elicit *tool.Elicitation) []tool.Tool {
	commandDescription := strings.Join([]string{
		fmt.Sprintf("Start a command for long-running or interactive work. Waits up to `wait` seconds (default %d) for it to finish; if still running, the command keeps running in the background and a session_id is returned.", defaultExecWait),
		"- Use for dev servers, watch tasks, log tails, and interactive programs (REPLs, CLIs that prompt for input). Use `shell` for commands expected to finish promptly.",
		"- Set `tty` for programs that need a terminal (REPLs, prompts, programs that buffer output when piped). Unix only — ignored on Windows. Written input is echoed back in the output.",
		"- Runs in the same host shell as `shell` and starts in the workspace directory. stdout and stderr are merged. Output between reads is buffered (oldest dropped past 1MB); poll with `exec_session` to collect it.",
		"- The process is NOT killed when the wait elapses. Kill sessions you no longer need via `exec_session`.",
		safetyGuardLine(elicit),
	}, "\n")

	appr := newApprovals()

	sessionDescription := strings.Join([]string{
		"Interact with a session started by `exec_command`: poll new output (no input), write to its stdin (`input`), close stdin (`eof`), or terminate it (`kill`).",
		"- Waits up to `wait` seconds for output (or exit) before returning.",
		"- `input` is written verbatim; include a trailing newline to submit a line to interactive programs. On tty sessions, `eof` sends Ctrl-D instead of closing stdin, and control characters like \\u0003 (Ctrl-C) can be sent via `input`.",
		"- Sessions end when the process exits, is killed, or the agent session closes.",
	}, "\n")

	return []tool.Tool{
		{
			Name:        "exec_command",
			Description: commandDescription,
			Effect:      ClassifyEffect,

			Parameters: map[string]any{
				"type": "object",

				"properties": map[string]any{
					"command":     map[string]any{"type": "string", "description": "Command to run."},
					"description": map[string]any{"type": "string", "description": "Short label (e.g. \"Start dev server\")."},
					"tty":         map[string]any{"type": "boolean", "description": "Run in a pseudo-terminal (Unix only)."},
					"wait":        map[string]any{"type": "integer", "description": fmt.Sprintf("Seconds to wait before backgrounding (default %d, max %d; 0 backgrounds immediately).", defaultExecWait, maxExecWait)},
				},

				"required":             []string{"command"},
				"additionalProperties": false,
			},

			Execute: func(ctx context.Context, args map[string]any) (string, error) {
				return executeExecCommand(ctx, manager, workDir, elicit, appr, args)
			},
		},
		{
			Name:        "exec_session",
			Description: sessionDescription,
			Effect:      classifyExecSession,

			Parameters: map[string]any{
				"type": "object",

				"properties": map[string]any{
					"session_id": map[string]any{"type": "integer", "description": "Session id returned by exec_command."},
					"input":      map[string]any{"type": "string", "description": "Text to write to the process stdin."},
					"eof":        map[string]any{"type": "boolean", "description": "Close stdin after writing input."},
					"kill":       map[string]any{"type": "boolean", "description": "Terminate the process."},
					"wait":       map[string]any{"type": "integer", "description": fmt.Sprintf("Seconds to wait for output before returning (default %d, max %d; 0 returns immediately).", defaultSessionWait, maxExecWait)},
				},

				"required":             []string{"session_id"},
				"additionalProperties": false,
			},

			Execute: func(ctx context.Context, args map[string]any) (string, error) {
				return executeExecSession(ctx, manager, args)
			},
		},
	}
}

func executeExecCommand(ctx context.Context, m *ExecManager, workDir string, elicit *tool.Elicitation, appr *approvals, args map[string]any) (string, error) {
	command, ok := args["command"].(string)

	if !ok || command == "" {
		return "", fmt.Errorf("command is required")
	}

	wait := defaultExecWait
	if value, present, err := tool.NonNegIntArg(args, "wait"); present {
		if err != nil {
			return "", err
		}
		wait = value
	}
	if wait > maxExecWait {
		wait = maxExecWait
	}

	if err := confirmDangerous(ctx, elicit, appr, args); err != nil {
		return "", err
	}

	if m.runningCount() >= maxExecSessions {
		return "", fmt.Errorf("too many running sessions (max %d); kill sessions you no longer need via exec_session", maxExecSessions)
	}

	tty, _ := args["tty"].(bool)
	if runtime.GOOS == "windows" {
		tty = false
	}

	sctx, cancel := context.WithCancel(context.Background())

	cmd := buildCommand(sctx, command, workDir)

	s := &execSession{
		command: command,
		tty:     tty,
		cancel:  cancel,
		done:    make(chan struct{}),
	}

	if tty {
		master, err := startTTY(cmd)
		if err != nil {
			cancel()
			return "", fmt.Errorf("failed to start command: %w", err)
		}
		s.stdin = master

		copyDone := make(chan struct{})
		go func() {
			io.Copy(s, master)
			close(copyDone)
		}()
		go func() {
			err := cmd.Wait()
			select {
			case <-copyDone:
			case <-time.After(2 * time.Second):
			}
			master.Close()
			s.exitErr = err
			close(s.done)
		}()
	} else {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			cancel()
			return "", err
		}
		s.stdin = stdin
		cmd.Stdout = s
		cmd.Stderr = s

		if err := cmd.Start(); err != nil {
			cancel()
			return "", fmt.Errorf("failed to start command: %w", err)
		}

		go func() {
			s.exitErr = cmd.Wait()
			close(s.done)
		}()
	}

	id := m.add(s)

	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()

	select {
	case <-s.done:
		m.remove(id)
		return sessionResult(s.drain(), s.exitNotice()), nil
	case <-timer.C:
	case <-ctx.Done():
	}

	notice := fmt.Sprintf("Still running with session_id %d — use exec_session to poll output, send input, or kill it", id)
	return sessionResult(s.drain(), notice), nil
}

func executeExecSession(ctx context.Context, m *ExecManager, args map[string]any) (string, error) {
	id, ok := tool.IntArg(args, "session_id")
	if !ok {
		return "", fmt.Errorf("session_id is required")
	}

	s := m.get(id)
	if s == nil {
		return "", fmt.Errorf("no session with id %d (it may have exited and been cleaned up)", id)
	}

	wait := defaultSessionWait
	if value, present, err := tool.NonNegIntArg(args, "wait"); present {
		if err != nil {
			return "", err
		}
		wait = value
	}
	if wait > maxExecWait {
		wait = maxExecWait
	}

	if kill, _ := args["kill"].(bool); kill {
		notice := fmt.Sprintf("Session %d killed", id)
		if s.exited() {
			notice = s.exitNotice()
		}
		s.cancel()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
		m.remove(id)
		return sessionResult(s.drain(), notice), nil
	}

	if input, _ := args["input"].(string); input != "" {
		if _, err := io.WriteString(s.stdin, input); err != nil {
			return "", fmt.Errorf("failed to write to stdin: %w", err)
		}
	}

	if eof, _ := args["eof"].(bool); eof {
		if s.tty {
			io.WriteString(s.stdin, "\x04")
		} else {
			s.stdin.Close()
		}
	}

	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()

	select {
	case <-s.done:
		m.remove(id)
		return sessionResult(s.drain(), s.exitNotice()), nil
	case <-timer.C:
	case <-ctx.Done():
	}

	output := s.drain()
	if output == "" {
		return fmt.Sprintf("(no new output; session %d still running)", id), nil
	}
	return sessionResult(output, fmt.Sprintf("Session %d still running", id)), nil
}

func classifyExecSession(args map[string]any) tool.Effect {
	if args == nil {
		return tool.EffectDynamic
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
