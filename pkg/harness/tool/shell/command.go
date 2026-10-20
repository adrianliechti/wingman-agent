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

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func safetyGuardLine(elicit *tool.Elicitation) string {
	if elicit == nil || (elicit.Confirm == nil && elicit.Review == nil) {
		return "- No approver is available. Commands classified as risky and requests to expand permissions will be rejected; use commands within the configured access that do not require approval."
	}
	return "- The harness applies the current mode's approval policy to risky commands and permission requests on each execution. Submit execution permission requests through this tool's parameters. Do not bypass an approval denial by changing tools, command spelling or execution mode."
}

func sandboxGuideLine(opts *Options) string {
	network := "IP network access is blocked by default"
	if opts != nil && opts.Network {
		network = "IP network access is enabled by host configuration"
	}
	protection := "Treat protected Git metadata, editor settings and agent configuration in project roots as read-only. Private temp permits configuration fixtures."
	if runtime.GOOS != "darwin" {
		protection += " OS enforcement covers only existing protected paths at project roots; do not bypass this restriction with newly created or nested paths."
	}
	cacheExample := `npm ci --cache "$TMPDIR/npm-cache"`
	if runtime.GOOS == "windows" {
		cacheExample = `npm ci --cache "$env:TEMP\npm-cache"`
	}
	return strings.Join([]string{
		"- Commands run in an OS sandbox. Use the Shell writable roots in Environment context to choose output paths and workdir. Known credential stores are hidden. " + protection,
		"- Use private TMPDIR/TEMP/TMP for temporary files; do not assume /tmp is writable. Expand environment variables inside command using the host shell's syntax. In workdir and writable_roots, supply literal paths, not environment-variable references.",
		"- Preserve inherited tool/cache configuration; many tools still read a cache they cannot write. Set GODEBUG=goindex=0 for Go commands so they read the host build cache without writing its package index. Only if a command still fails writing a disposable cache, redirect that cache to private temp for the command, e.g. " + cacheExample + "; likewise PIP_CACHE_DIR, UV_CACHE_DIR or GOCACHE/GOTMPDIR. Do not reset HOME, GOPATH, GOMODCACHE, CARGO_HOME or XDG_CACHE_HOME to repair one cache error.",
		"- " + network + ". Use default permissions when configured access suffices. For required network access or writes outside listed roots, request require_permissions with the needed network/writable_roots and justification. Grants retain the sandbox and apply to the launched command, not later exec calls.",
		"- Request require_escalated only when the required operation cannot run with scoped grants. It removes the local sandbox and private TMPDIR for the launched command, so pass staged files by absolute path; supply justification and omit network/writable_roots.",
	}, "\n")
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

func resolveWorkdir(workDir string, args map[string]any, opts *Options) (string, error) {
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
	if workspaceSandboxEnabled(opts) && args["sandbox_permissions"] != "require_escalated" {
		// executionOptions has already merged validated per-command grants into opts.
		if !slices.ContainsFunc(opts.writableRoots(workDir), func(root string) bool { return pathWithin(root, value) }) {
			return "", fmt.Errorf("workdir %q is outside sandbox workspace %q and configured writable roots", value, workDir)
		}
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

func ensurePrivateTemp(opts *Options, escalated bool) error {
	if opts == nil || opts.TempDir == "" || escalated {
		return nil
	}
	// An earlier command may have removed private temp or replaced it with a link, whose target the sandbox would grant.
	if info, err := os.Lstat(opts.TempDir); err == nil && info.Mode().Type() != os.ModeDir {
		if err := os.Remove(opts.TempDir); err != nil {
			return fmt.Errorf("replace private temp: %w", err)
		}
	}
	if err := os.MkdirAll(opts.TempDir, 0o700); err != nil {
		return fmt.Errorf("create private temp: %w", err)
	}
	return nil
}

func buildToolCommand(ctx context.Context, command, workspaceDir, workingDir string, opts *Options, escalated bool) (*exec.Cmd, error) {
	if err := ensurePrivateTemp(opts, escalated); err != nil {
		return nil, err
	}
	var sandbox *sandboxOptions
	if workspaceSandboxEnabled(opts) && !escalated {
		sandbox = &sandboxOptions{WorkspaceDir: workspaceDir}
		if opts != nil {
			sandbox.ExtraWritableRoots = append([]string(nil), opts.ExtraWritableRoots...)
			sandbox.TempDir = opts.TempDir
			sandbox.Network = opts.Network
		}
		// The native Windows sandbox assigns each environment private TEMP/TMP
		// storage; a per-command root would also defeat environment reuse.
		if sandbox.TempDir == "" && runtime.GOOS != "windows" {
			dir, err := os.MkdirTemp("", "wingman-command-")
			if err != nil {
				return nil, fmt.Errorf("create private sandbox temp: %w", err)
			}
			sandbox.TempDir = canonicalPath(dir)
			context.AfterFunc(ctx, func() { _ = os.RemoveAll(dir) })
		}
	}
	environment := environmentForTools(opts)
	if sandbox == nil && !escalated && opts != nil {
		// Escalated runs keep the host temp: sandboxed commands can write private temp.
		environment = environmentWithTemp(environment, opts.TempDir)
	}
	return buildCommandWithEnvironment(ctx, command, workingDir, environment, sandbox)
}

func buildCommandWithEnvironment(ctx context.Context, command, workingDir string, environment []string, sandbox *sandboxOptions) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if sandbox != nil {
		environment = environmentWithTemp(environment, sandbox.TempDir)
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
