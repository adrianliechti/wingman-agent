package codex

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/coder/acp-go-sdk"
)

// Options configures the codex subprocess and the [Agent]'s defaults for
// new sessions.
type Options struct {
	// Model is the default model id used for new sessions. Empty / "default"
	// defers to codex's configured default.
	Model string

	// Effort is the default reasoning effort applied to new sessions.
	// Empty / "default" disables the override.
	Effort string

	// Env is the environment for the `codex app-server` subprocess. nil
	// means inherit the parent process env. To layer Wingman routing on
	// top, callers can pass
	// `pkg/external/codex.BuildEnv(os.Environ(), cfg)`.
	Env []string

	// ExtraArgs are extra CLI args prefixed before `app-server`. Use this
	// for top-level codex flags such as the `--config` pairs returned by
	// `pkg/external/codex.BuildArgs`.
	ExtraArgs []string
}

// Spawn starts a `codex app-server` subprocess and returns an Agent that
// talks to it. Use [Agent.Close] (or rely on ctx cancellation) to
// terminate the subprocess. The agent is ready to be wired into an
// [acp.AgentSideConnection] via [Agent.SetAgentConnection].
//
// codexPath is the path to (or PATH name of) the `codex` binary; empty
// defaults to "codex".
func Spawn(ctx context.Context, codexPath string, opts Options) (*Agent, error) {
	if codexPath == "" {
		codexPath = "codex"
	}

	args := append(append([]string{}, opts.ExtraArgs...), "app-server")
	cmd := exec.CommandContext(ctx, codexPath, args...)
	cmd.Stderr = os.Stderr
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("codex: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("codex: start %s: %w", codexPath, err)
	}

	rpc := newRPCClient(stdin, stdout)
	client := newCodexClient(rpc)
	rpc.start()

	a := newAgent(client, opts.Model, opts.Effort)
	a.cmd = cmd
	a.stdin = stdin
	return a, nil
}

// Done returns a channel closed when the codex subprocess exits.
func (a *Agent) Done() <-chan struct{} {
	if a.codex == nil || a.codex.rpc == nil {
		return a.closed
	}
	return a.codex.rpc.done
}

// Close terminates the codex subprocess. Idempotent.
func (a *Agent) Close() error {
	a.closeOnce.Do(func() {
		if a.stdin != nil {
			_ = a.stdin.Close()
		}
		if a.cmd != nil && a.cmd.Process != nil {
			exited := make(chan struct{})
			go func() {
				_ = a.cmd.Wait()
				close(exited)
			}()
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
				_ = a.cmd.Process.Kill()
				<-exited
			}
		}
		close(a.closed)
	})
	return nil
}

// Run is the convenience entry point for standalone usage: spawn codex,
// serve ACP over in/out until the connection ends, codex exits, or ctx is
// cancelled.
func Run(ctx context.Context, codexPath string, opts Options, in io.Reader, out io.Writer, logger *slog.Logger) error {
	a, err := Spawn(ctx, codexPath, opts)
	if err != nil {
		return err
	}
	defer a.Close()

	conn := acp.NewAgentSideConnection(a, out, in)
	if logger != nil {
		conn.SetLogger(logger)
	}
	a.SetAgentConnection(conn)

	select {
	case <-conn.Done():
	case <-a.Done():
	case <-ctx.Done():
	}
	return nil
}
