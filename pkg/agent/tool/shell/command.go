package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func safetyGuardLine(elicit *tool.Elicitation) string {
	if elicit == nil || (elicit.Confirm == nil && elicit.Review == nil) {
		return "- Risky commands and permission expansions cannot run because no approver is available. Routine commands can run within the configured sandbox."
	}
	if elicit.Review != nil {
		return "- Safety guard: routine commands run directly. Risky actions and permission expansions receive automatic review for each execution. A denial states its reason and must not be worked around."
	}
	return "- Safety guard: routine commands run directly; risky actions and permission expansions require approval for each execution."
}

func sandboxGuideLine() string {
	network := "IP network access is blocked by default"
	if sandboxNetworkEnabled() {
		network = "IP network access is enabled by host configuration"
	}
	protection := "Protected Git metadata, editor settings and agent configuration stay read-only."
	if runtime.GOOS != "darwin" {
		protection += " The native backend only protects paths that exist when the command starts; never create protected configuration through a command."
	}
	return "- Commands run in an OS sandbox. Writes are limited to the workspace, project memory and private temporary storage ($TMPDIR/$TEMP). Known credential stores are hidden and common authentication variables are removed. " + protection + " " + network + ". For a download or additional directory access, use sandbox_permissions=\"require_permissions\", network=true and/or writable_roots, with a justification. These grants apply to one execution and retain the sandbox. Reserve require_escalated for operations that genuinely need to run outside the sandbox. Every permission expansion is reviewed again."
}

// sandboxDenialHints follows Codex (plus the Windows spellings): output words that suggest the sandbox blocked a failed command.
var sandboxDenialHints = []string{"operation not permitted", "permission denied", "read-only file system", "sandbox", "is denied"}

func looksLikeSandboxDenial(output string, err error) bool {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() <= 0 {
		return false
	}
	output = strings.ToLower(output)
	return slices.ContainsFunc(sandboxDenialHints, func(hint string) bool { return strings.Contains(output, hint) })
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
	if workspaceSandboxEnabled() && args["sandbox_permissions"] != "require_escalated" && !pathWithin(workDir, value) {
		if args["sandbox_permissions"] == "require_permissions" {
			if roots, ok := args["writable_roots"].([]any); ok {
				for _, root := range roots {
					if path, ok := root.(string); ok {
						if !filepath.IsAbs(path) {
							path = filepath.Join(workDir, path)
						}
						if pathWithin(path, value) {
							return value, nil
						}
					}
				}
			}
		}
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
	return buildToolCommand(ctx, command, workspaceDir, workingDir, opts, false)
}

func buildToolCommand(ctx context.Context, command, workspaceDir, workingDir string, opts *Options, escalated bool) (*exec.Cmd, error) {
	var sandbox *sandboxOptions
	if workspaceSandboxEnabled() && !escalated {
		sandbox = &sandboxOptions{WorkspaceDir: workspaceDir}
		if opts != nil {
			sandbox.ExtraWritableRoots = append([]string(nil), opts.ExtraWritableRoots...)
			sandbox.TempDir = opts.TempDir
			sandbox.Network = opts.Network
		}
		if sandbox.TempDir == "" {
			dir, err := os.MkdirTemp("", "wingman-command-")
			if err != nil {
				return nil, fmt.Errorf("create private sandbox temp: %w", err)
			}
			sandbox.TempDir = canonicalPath(dir)
			context.AfterFunc(ctx, func() { _ = os.RemoveAll(dir) })
		}
		sandbox.ExtraWritableRoots = append(sandbox.ExtraWritableRoots, sandbox.TempDir)
	}
	return buildCommandWithEnvironment(ctx, command, workingDir, environmentForTools(opts), sandbox)
}

func buildCommandWithEnvironment(ctx context.Context, command, workingDir string, environment []string, sandbox *sandboxOptions) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if sandbox != nil && sandbox.TempDir != "" {
		for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
			environment = setEnvironment(environment, key, sandbox.TempDir)
		}
	}

	if runtime.GOOS == "windows" {
		ps := findPowerShell()

		wrapped := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; " + command
		if sandbox == nil {
			cmd = exec.CommandContext(ctx, ps, "-NoProfile", "-NoLogo", "-NonInteractive", "-Command", wrapped)
		} else {
			var err error
			if cmd, err = buildSandboxedCommand(ctx, ps, wrapped, workingDir, *sandbox); err != nil {
				return nil, err
			}
			environment = setEnvironment(environment, workspaceSandboxActiveEnv, "1")
		}
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
	TempDir            string
	Network            bool
}

func findPowerShell() string {
	if ps, err := exec.LookPath("pwsh"); err == nil {
		return ps
	}
	return "powershell"
}
