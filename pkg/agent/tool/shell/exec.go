package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
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
	closed   bool
	nextID   int
	sessions map[int]*execSession
}

func NewExecManager() *ExecManager {
	return &ExecManager{sessions: map[int]*execSession{}}
}

func (m *ExecManager) Close() {
	m.mu.Lock()
	m.closed = true
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

func (m *ExecManager) add(s *execSession) (int, error) {
	m.mu.Lock()

	if m.closed {
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
			if existing.exited() {
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

type execSession struct {
	id        int
	command   string
	tty       bool
	cancel    context.CancelFunc
	interrupt func() error
	stdin     io.WriteCloser

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
			if code := exitErr.ExitCode(); code >= 0 {
				return fmt.Sprintf("Command exited with code %d", code)
			}
			return fmt.Sprintf("Command terminated (%s)", exitErr.ProcessState.String())
		}
		return fmt.Sprintf("Command failed to run: %v", s.exitErr)
	}
	return "Command completed"
}

func ExecTools(manager *ExecManager, workDir string, elicit *tool.Elicitation, appr *Approvals) []tool.Tool {
	if appr == nil {
		appr = NewApprovals()
	}

	commandDescription := strings.Join([]string{
		fmt.Sprintf("Start a command for long-running or interactive work. Waits up to `wait` seconds (default %d) for it to finish; if still running, the command keeps running in the background and a session_id is returned.", defaultExecWait),
		"- Use for dev servers, watch tasks, log tails, and interactive programs (REPLs, CLIs that prompt for input). Use `shell` for commands expected to finish promptly.",
		"- Set `tty` for programs that need a terminal (REPLs, prompts, programs that buffer output when piped). Unix only — ignored on Windows. Written input is echoed back in the output.",
		"- Runs in the same host shell as `shell` and starts in the workspace directory. stdout and stderr are merged. Output between reads is buffered (oldest dropped past 1MB); poll with `exec_session` to collect it.",
		"- The process is NOT killed when the wait elapses. Kill sessions you no longer need via `exec_session`.",
		safetyGuardLine(elicit),
	}, "\n")

	sessionDescription := strings.Join([]string{
		"Interact with a session started by `exec_command`: poll new output (no input), write to its stdin (`input`), close stdin (`eof`), or terminate it (`kill`).",
		"- Waits up to `wait` seconds for output (or exit) before returning.",
		"- `input` supports C-style escapes (\\n Enter, \\e Esc, \\t, \\uHHHH; \\\\ for a literal backslash); anything else is sent verbatim with nothing appended. Include \\n to submit a line to an interactive program, else it is typed but not entered (e.g. save in vi with \"\\e:w file\\n\"). Destructive or privilege-escalating input lines require the same user confirmation as shell commands.",
		"- \\u0003 (Ctrl-C) interrupts the process: on tty sessions via the terminal, otherwise via SIGINT to the process group (Unix only).",
		"- On tty sessions, `eof` sends Ctrl-D instead of closing stdin.",
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
				return executeExecSession(ctx, manager, elicit, appr, args)
			},
		},
	}
}

func executeExecCommand(ctx context.Context, m *ExecManager, workDir string, elicit *tool.Elicitation, appr *Approvals, args map[string]any) (string, error) {
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

	tty, _ := args["tty"].(bool)
	if runtime.GOOS == "windows" {
		tty = false
	}

	sctx, cancel := context.WithCancel(context.Background())

	cmd := buildCommand(sctx, command, workDir)
	cmd.Env = append(cmd.Env, "NO_COLOR=1", "PAGER=cat", "GIT_PAGER=cat")

	s := &execSession{
		command:   command,
		tty:       tty,
		cancel:    cancel,
		interrupt: func() error { return interruptProcessGroup(cmd) },
		done:      make(chan struct{}),
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

	id, err := m.add(s)
	if err != nil {
		return "", err
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

	notice := fmt.Sprintf("Still running with session_id %d — use exec_session to poll output, send input, or kill it", id)
	return sessionResult(s.drain(), notice), nil
}

func executeExecSession(ctx context.Context, m *ExecManager, elicit *tool.Elicitation, appr *Approvals, args map[string]any) (string, error) {
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
		alreadyExited := s.exited()
		s.cancel()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}

		notice := fmt.Sprintf("Session %d killed", id)
		if alreadyExited {
			notice = s.exitNotice()
		} else if !s.exited() {
			notice = fmt.Sprintf("Session %d kill signalled; the process has not exited yet", id)
		}

		m.remove(id)
		return sessionResult(s.drain(), notice), nil
	}

	input, _ := args["input"].(string)
	input = decodeInput(input)

	if input == "\u0003" && !s.tty {
		if err := s.interrupt(); err != nil {
			return "", fmt.Errorf("cannot interrupt on this platform; use kill instead: %w", err)
		}
	} else if input != "" {
		if err := confirmIfDangerous(ctx, elicit, appr, strings.TrimRight(input, "\n"), IsDangerousCommand(input)); err != nil {
			return "", err
		}

		if _, err := io.WriteString(s.stdin, input); err != nil {
			if s.exited() {
				m.remove(id)
				return sessionResult(s.drain(), s.exitNotice()+" (input was not delivered)"), nil
			}
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
	if input, _ := args["input"].(string); input != "" && IsDangerousCommand(input) {
		return tool.EffectDangerous
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
