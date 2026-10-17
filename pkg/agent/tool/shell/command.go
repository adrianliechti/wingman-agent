package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func safetyGuardLine(elicit *tool.Elicitation) string {
	if elicit == nil || elicit.Confirm == nil {
		return "- There is NO confirmation gate: commands run immediately. Never run destructive or privilege-escalating commands (recursive deletes, sudo, force-push) unless the user explicitly asked for that exact action."
	}
	if elicit.Review != nil {
		return "- Safety guard: routine mutating commands run directly, but destructive or privilege-escalating commands are checked by an automatic reviewer first; a denial states its reason and must not be worked around. An approved command re-runs without re-checking for the rest of the session."
	}
	return "- Safety guard: routine mutating commands run directly, but destructive or privilege-escalating commands require user confirmation first. An approved command re-runs without re-asking for the rest of the session."
}

// approvalWorkdir returns the directory to surface in approval prompts: empty
// for the workspace default, the effective directory otherwise.
func approvalWorkdir(workDir, dir string) string {
	if filepath.Clean(dir) == filepath.Clean(workDir) {
		return ""
	}
	return dir
}

func resolveWorkdir(workDir string, args map[string]any) (string, error) {
	value, _ := args["workdir"].(string)
	value = strings.TrimSpace(value)

	if value == "" {
		return workDir, nil
	}

	if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			value = filepath.Join(home, value[1:])
		}
	}

	if !filepath.IsAbs(value) {
		value = filepath.Join(workDir, value)
	}
	value = filepath.Clean(value)

	info, err := os.Stat(value)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("workdir %q is not an accessible directory", value)
	}
	if workspaceSandboxEnabled() && !pathWithin(workDir, value) {
		return "", fmt.Errorf("workdir %q is outside sandbox workspace %q", value, workDir)
	}

	return value, nil
}

// Command runs user-authored scripts, such as `!` commands and hooks, outside the sandbox.
func Command(ctx context.Context, command, workingDir string) *exec.Cmd {
	cmd, _ := buildCommandWithEnvironment(ctx, command, workingDir, os.Environ(), nil)
	return cmd
}

// ToolCommand applies the environment policy and workspace sandbox for model-generated scripts.
func ToolCommand(ctx context.Context, command, workspaceDir, workingDir string, opts *Options) (*exec.Cmd, error) {
	return buildToolCommand(ctx, command, workspaceDir, workingDir, opts)
}

func buildToolCommand(ctx context.Context, command, workspaceDir, workingDir string, opts *Options) (*exec.Cmd, error) {
	var sandbox *sandboxOptions
	if workspaceSandboxEnabled() {
		sandbox = &sandboxOptions{WorkspaceDir: workspaceDir}
		if opts != nil {
			sandbox.ExtraWritableRoots = opts.ExtraWritableRoots
		}
	}
	return buildCommandWithEnvironment(ctx, command, workingDir, environmentForTools(opts), sandbox)
}

func buildCommandWithEnvironment(ctx context.Context, command, workingDir string, environment []string, sandbox *sandboxOptions) (*exec.Cmd, error) {
	var cmd *exec.Cmd

	if runtime.GOOS == "windows" {
		if sandbox != nil {
			return nil, fmt.Errorf("workspace shell sandbox is not supported on Windows")
		}
		ps := findPowerShell()

		wrapped := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; " + command
		cmd = exec.CommandContext(ctx, ps, "-NoProfile", "-NoLogo", "-NonInteractive", "-Command", wrapped)
	} else {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		if sandbox == nil {
			cmd = exec.CommandContext(ctx, shell, "-c", command)
		} else {
			var err error
			if cmd, err = buildSandboxedCommand(ctx, shell, command, workingDir, *sandbox); err != nil {
				return nil, err
			}
			environment = setEnvironment(environment, workspaceSandboxActiveEnv, "1")
		}
	}

	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = workingDir
	cmd.Env = setEnvironment(environment, "GIT_EDITOR", "true")
	cmd.Env = setEnvironment(cmd.Env, "WINGMAN", "1")

	setupProcessGroup(cmd)

	cmd.Cancel = func() error { return killProcessGroup(cmd) }

	return cmd, nil
}

// sandboxOptions sets the workspace boundary and any extra writable roots, such as project memory.
type sandboxOptions struct {
	WorkspaceDir       string
	ExtraWritableRoots []string
}

func findPowerShell() string {
	if ps, err := exec.LookPath("pwsh"); err == nil {
		return ps
	}
	return "powershell"
}
