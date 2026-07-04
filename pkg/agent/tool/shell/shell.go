package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const (
	defaultTimeout = 120
	maxOutputBytes = 16 * 1024 * 1024
)

var errCommandTimeout = errors.New("command timeout")

type cappedBuffer struct {
	buf     bytes.Buffer
	dropped int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := maxOutputBytes - b.buf.Len(); remaining > 0 {
		n := min(len(p), remaining)
		b.buf.Write(p[:n])
		b.dropped += len(p) - n
	} else {
		b.dropped += len(p)
	}
	return len(p), nil
}

func (b *cappedBuffer) result() string {
	out := b.buf.String()
	if b.dropped > 0 {
		out += fmt.Sprintf("\n\n[output capped at %dMB; %d further bytes dropped]", maxOutputBytes/(1024*1024), b.dropped)
	}
	return out
}

func safetyGuardLine(elicit *tool.Elicitation) string {
	if elicit == nil || elicit.Confirm == nil {
		return "- There is NO confirmation gate: commands run immediately. Never run destructive or privilege-escalating commands (recursive deletes, sudo, force-push) unless the user explicitly asked for that exact action."
	}
	return "- Safety guard: routine mutating commands run directly, but destructive or privilege-escalating commands require user confirmation first. An approved command re-runs without re-asking for the rest of the session."
}

func Tools(workDir string, elicit *tool.Elicitation, appr *Approvals) []tool.Tool {
	if appr == nil {
		appr = NewApprovals()
	}

	description := strings.Join([]string{
		fmt.Sprintf("Execute a command in the host shell. On Unix/macOS this uses the user's shell (`$SHELL`, falling back to `/bin/sh`); on Windows this uses PowerShell. Default timeout %ds, max 600s.", defaultTimeout),
		"- Use for build, test, run, package-manager, git, GitHub CLI (`gh`), Docker/Kubernetes, project scripts, diagnostics, and other terminal operations.",
		"- Prefer dedicated tools when they are clearly better for the job: `grep`/`glob`/LSP for code search, `read` for targeted file reads, `edit`/`write` for reviewable file changes. Shell is fine when a command is the natural interface or combines several process-level steps.",
		"- Match command syntax to the host OS shown in your environment section. Examples: list dir -> `ls` on Unix, `Get-ChildItem` on PowerShell.",
		"- Each call starts in the workspace directory. Shell state (env vars, aliases, `cd` from a prior call) does not persist between calls. Use absolute paths or chain dependent commands in one call.",
		"- For GitHub URLs or PR/issue/release data, prefer `gh` commands (`gh pr view`, `gh issue view`, `gh api`) over `web_fetch`; they return structured authenticated data.",
		"- Only commit when the user explicitly asked; stage specific files by name; never skip hooks.",
		"- Quote paths with spaces. Chain dependent commands with `&&` on Unix or PowerShell 7+, and with `; if ($?) { ... }` on Windows PowerShell 5.1. Use separate tool calls for independent commands.",
		"- Increase timeout for long-running commands. Avoid unnecessary `sleep` / `Start-Sleep`; if polling is needed, run a check command instead of sleeping first.",
		"- For processes that should keep running (dev servers, watch tasks) or need interactive stdin, use `exec_command` instead.",
		safetyGuardLine(elicit),
	}, "\n")

	return []tool.Tool{{
		Name:        "shell",
		Description: description,
		Effect:      ClassifyEffect,
		Timeout:     15 * time.Minute,

		Parameters: map[string]any{
			"type": "object",

			"properties": map[string]any{
				"command":     map[string]any{"type": "string", "description": "Command to run."},
				"description": map[string]any{"type": "string", "description": "Short label (e.g. \"Run unit tests\")."},
				"timeout":     map[string]any{"type": "integer", "description": fmt.Sprintf("Seconds (default %d, max 600).", defaultTimeout)},
			},

			"required":             []string{"command"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			return executeShell(ctx, workDir, elicit, appr, args)
		},
	}}
}

func executeShell(ctx context.Context, workDir string, elicit *tool.Elicitation, appr *Approvals, args map[string]any) (string, error) {
	command, ok := args["command"].(string)

	if !ok || command == "" {
		return "", fmt.Errorf("command is required")
	}

	timeout := defaultTimeout
	if value, present, err := tool.OptionalIntArg(args, "timeout"); present {
		if err != nil {
			return "", err
		}
		timeout = value
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	if timeout > 600 {
		timeout = 600
	}

	if err := confirmDangerous(ctx, elicit, appr, args); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeoutCause(ctx, time.Duration(timeout)*time.Second, errCommandTimeout)
	defer cancel()

	cmd := buildCommand(ctx, command, workDir)

	var output cappedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	runErr := cmd.Run()
	result := output.result()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		notice := fmt.Sprintf("Command timed out after %d seconds", timeout)
		if !errors.Is(context.Cause(ctx), errCommandTimeout) {
			notice = "Command aborted: the tool call deadline expired before the command finished"
		}
		if result == "" {
			return notice, nil
		}
		return result + "\n\n" + notice, nil
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			result += fmt.Sprintf("\n\nCommand exited with code %d", exitErr.ExitCode())
		} else {
			result += fmt.Sprintf("\n\nCommand failed to run: %v", runErr)
		}
	}
	return result, nil
}

// Command builds an *exec.Cmd that runs a script with the same
// interpreter the shell tool uses on this platform.
func Command(ctx context.Context, command, workingDir string) *exec.Cmd {
	return buildCommand(ctx, command, workingDir)
}

func buildCommand(ctx context.Context, command, workingDir string) *exec.Cmd {
	var cmd *exec.Cmd

	if runtime.GOOS == "windows" {
		ps := findPowerShell()

		wrapped := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; " + command
		cmd = exec.CommandContext(ctx, ps, "-NoProfile", "-NoLogo", "-NonInteractive", "-Command", wrapped)
	} else {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.CommandContext(ctx, shell, "-c", command)
	}

	cmd.Dir = workingDir
	cmd.Env = append(os.Environ(),
		"GIT_EDITOR=true",
		"WINGMAN=1",
	)

	setupProcessGroup(cmd)

	cmd.Cancel = func() error { return killProcessGroup(cmd) }

	return cmd
}

func findPowerShell() string {
	if ps, err := exec.LookPath("pwsh"); err == nil {
		return ps
	}
	return "powershell"
}
