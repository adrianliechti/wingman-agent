package shell

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const (
	defaultTimeout = 120
	maxLines       = 2000
	maxBytes       = 50 * 1024
)

func Tools(workDir string, elicit *tool.Elicitation) []tool.Tool {
	description := strings.Join([]string{
		fmt.Sprintf("Execute a command. On Unix/macOS this is `sh`; on Windows this is PowerShell. Default timeout %ds, max 600s.", defaultTimeout),
		"- Use for build, test, run, package-manager, git, GitHub CLI (`gh`), and other operations that genuinely require a process. Prefer dedicated tools for codebase discovery and file work: `grep`/`find`/LSP for search, `read` for known files, `edit`/`write` for changes.",
		"- Match command syntax to the host OS shown in your environment section. Examples: list dir → `ls` on Unix, `Get-ChildItem` on PowerShell. Read a file → use the `read` tool, not `cat` / `Get-Content`.",
		"- Working directory persists across calls; shell state (env vars, aliases, `cd` from a prior call) does NOT.",
		"- Don't use shell to read, edit, or write files (`cat`, `Get-Content`, `sed`, `Set-Content`, heredocs, `> file`, etc.) — use `read` / `edit` / `write` so the user can review the change.",
		"- For GitHub URLs or PR/issue/release data, prefer `gh` commands (`gh pr view`, `gh issue view`, `gh api`) over `fetch`; they return structured authenticated data.",
		"- For commits: only commit when asked, inspect `git status`, `git diff`, and recent `git log` first, stage specific files by name, never skip hooks, and create a new commit instead of amending unless explicitly requested.",
		"- Quote paths with spaces. Chain dependent commands with `&&` (Unix) or `;` (PowerShell) in one call; make separate calls for independent commands (parallel beats sequential).",
		"- Once a check has passed (tests, build, lint), trust it — don't re-run to be sure.",
		"- Increase timeout for long-running commands; do not insert `sleep` / `Start-Sleep`.",
	}, "\n")

	return []tool.Tool{{
		Name:        "shell",
		Description: description,
		Effect:      ClassifyEffect,

		Parameters: map[string]any{
			"type": "object",

			"properties": map[string]any{
				"command":     map[string]any{"type": "string", "description": "Command to run."},
				"description": map[string]any{"type": "string", "description": "Short label (e.g. \"Run unit tests\")."},
				"timeout":     map[string]any{"type": "integer", "description": fmt.Sprintf("Seconds (default %d, max 600).", defaultTimeout)},
			},

			"required": []string{"command"},
		},

		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			return executeShell(ctx, workDir, elicit, args)
		},
	}}
}

func executeShell(ctx context.Context, workDir string, elicit *tool.Elicitation, args map[string]any) (string, error) {
	command, ok := args["command"].(string)

	if !ok || command == "" {
		return "", fmt.Errorf("command is required")
	}

	timeout := shellIntArg(args, "timeout", defaultTimeout)
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	if timeout > 600 {
		timeout = 600
	}

	if elicit != nil && elicit.Confirm != nil && ClassifyEffect(args) == tool.EffectDangerous {
		approved, err := elicit.Confirm(ctx, "❯ "+command)

		if err != nil {
			return "", fmt.Errorf("failed to get user approval: %w", err)
		}

		if !approved {
			return "", fmt.Errorf("command execution denied by user")
		}
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := buildCommand(ctx, command, workDir)

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start command: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		killProcessGroup(cmd)
		return "", fmt.Errorf("command timed out after %d seconds", timeout)
	case err := <-done:
		truncated := truncateOutput(output.String())

		if err != nil {
			exitCode := -1
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
			truncated += fmt.Sprintf("\n\nCommand exited with code %d", exitCode)
		}

		return truncated, nil
	}
}

func shellIntArg(args map[string]any, key string, fallback int) int {
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		if v > int64(math.MaxInt) || v < int64(math.MinInt) {
			return fallback
		}
		return int(v)
	case float64:
		if v > float64(math.MaxInt) || v < float64(math.MinInt) {
			return fallback
		}
		return int(v)
	default:
		return fallback
	}
}

func buildCommand(ctx context.Context, command, workingDir string) *exec.Cmd {
	var cmd *exec.Cmd

	if runtime.GOOS == "windows" {
		ps := findPowerShell()
		// Force UTF-8 output to avoid PowerShell 5.1's UTF-16 default
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
		"GIT_EDITOR=true", // Prevent git from opening interactive editors
		"WINGMAN=1",       // Marker so scripts can detect agent context
	)

	setupProcessGroup(cmd)

	return cmd
}

// findPowerShell prefers pwsh (PowerShell 7+, supports && and ||) and falls
// back to powershell (5.1).
func findPowerShell() string {
	if ps, err := exec.LookPath("pwsh"); err == nil {
		return ps
	}
	return "powershell"
}

func truncateOutput(output string) string {
	totalLines := strings.Count(output, "\n") + 1
	totalBytes := len(output)

	if totalLines <= maxLines && totalBytes <= maxBytes {
		return output
	}

	// Head + tail elision: preserve diagnostic output at the start (e.g. the
	// first failing test, the first compiler error) and the trailing summary,
	// drop the middle. Tail-only truncation loses errors that print at the
	// start of long output.
	lines := strings.Split(output, "\n")
	if len(lines) > maxLines {
		head := maxLines / 2
		tail := maxLines - head
		elided := len(lines) - head - tail
		lines = append(append(append([]string{}, lines[:head]...),
			fmt.Sprintf("... [%d lines elided] ...", elided)),
			lines[len(lines)-tail:]...)
	}

	truncated := strings.Join(lines, "\n")
	if len(truncated) > maxBytes {
		half := maxBytes / 2
		truncated = truncated[:half] + "\n... [bytes elided] ...\n" + truncated[len(truncated)-half:]
	}

	notice := fmt.Sprintf("[truncated %d→%d lines, %dKB→%dKB; head+tail elided]\n", totalLines, strings.Count(truncated, "\n")+1, totalBytes/1024, len(truncated)/1024)

	return notice + truncated
}
