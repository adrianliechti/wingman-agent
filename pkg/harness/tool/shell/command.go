package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	switch {
	case opts != nil && opts.Network:
		network = "IP network access is enabled by host configuration"
	case runtime.GOOS == "darwin" && (opts == nil || !opts.DisableLoopback):
		network = "IP network access is blocked by default except loopback, so local servers and tests work"
	case runtime.GOOS == "linux":
		network = "IP network access is blocked by default; each command has a private loopback, so start a server and its client in the same command"
	}
	temp := "do not assume /tmp is writable"
	switch {
	case opts != nil && opts.SharedTemp && runtime.GOOS != "windows":
		temp = "/tmp and the user's temporary directory are writable too"
	case runtime.GOOS == "darwin":
		temp += `; mktemp ignores TMPDIR on macOS unless given a template such as "$TMPDIR/x.XXXXXX"`
	}
	protection := "Read-only in project roots: " + protectedPathsText() + ". Private temp permits configuration fixtures."
	if runtime.GOOS == "darwin" {
		protection += " A directory that contains one cannot be renamed or removed."
	} else {
		protection += " OS enforcement covers only existing protected paths at project roots; do not bypass this restriction with newly created or nested paths."
	}
	credentials := ""
	if opts == nil || !opts.ReadCredentials {
		credentials = " Commands cannot read the credential stores " + hiddenCredentialsText() + "."
	}
	cacheExample := `npm ci --cache "$TMPDIR/npm-cache"`
	if runtime.GOOS == "windows" {
		cacheExample = `npm ci --cache "$env:TEMP\npm-cache"`
	}
	return strings.Join([]string{
		"- Commands run in an OS sandbox. Use the Shell writable roots in Environment context to choose output paths and workdir." + credentials + " " + protection,
		"- Use private TMPDIR/TEMP/TMP for temporary files; " + temp + ". Expand environment variables inside command using the host shell's syntax. In workdir and writable_roots, supply literal paths, not environment-variable references.",
		"- Preserve inherited tool/cache configuration; many tools still read a cache they cannot write. Go commands already run with GODEBUG=goindex=0, so they read the host build cache without writing it; a `writing stat cache` warning is harmless. Only if a command still fails writing a disposable cache, redirect that cache to private temp for the command, e.g. " + cacheExample + "; likewise PIP_CACHE_DIR, UV_CACHE_DIR or GOCACHE/GOTMPDIR. Do not reset HOME, GOPATH, GOMODCACHE, CARGO_HOME or XDG_CACHE_HOME to repair one cache error.",
		"- " + network + ". Use default permissions when configured access suffices. For required network access or writes outside listed roots, request require_permissions with the needed network/writable_roots and justification. Grants retain the sandbox and apply to the launched command, not later exec calls.",
		"- Request require_escalated only when the required operation cannot run with scoped grants. It removes the local sandbox for the launched command; supply justification and omit network/writable_roots.",
		"- Run commands that need host credentials, such as git push, with default permissions. When a command that finishes within the wait fails with a likely sandbox denial, the harness requests approval and re-runs the whole command, inside the sandbox with network access when only the network was missing and otherwise outside it; the result says so. Keep such commands out of long or non-repeatable command chains. Validation runs are not re-run.",
	}, "\n")
}

// sandboxDenialHints are the OS errors of a blocked write or read, including the
// Windows spelling, and ssh failing to read its hidden known hosts.
var sandboxDenialHints = []string{"operation not permitted", "permission denied", "read-only file system", "is denied", "host key verification failed"}

// offlineDenialHints are the name-resolution and routing failures of a sandbox without IP networking, such as git push.
var offlineDenialHints = []string{"could not resolve host", "temporary failure in name resolution", "name or service not known", "nodename nor servname", "getaddrinfo", "network is unreachable", "no such host"}

// sandboxLauncherPrefixes start the errors of a sandbox that failed to start;
// the command never ran, so running it outside the sandbox is no remedy.
var sandboxLauncherPrefixes = []string{"bwrap:", "sandbox-exec:", "wingman sandbox:"}

// isOfflineDenial reports a denial line that shows only a missing network:
// a name-resolution or routing failure, with no blocked file access.
func isOfflineDenial(line string) bool {
	lower := strings.ToLower(line)
	contains := func(hint string) bool { return strings.Contains(lower, hint) }
	return slices.ContainsFunc(offlineDenialHints, contains) && !slices.ContainsFunc(sandboxDenialHints, contains)
}

// hostCredentialCommand matches commands that authenticate as the user: Git
// remote operations, gh, and the ssh family. The sandbox hides their keys and
// tokens, so network access alone would only move the failure.
var hostCredentialCommand = regexp.MustCompile(`(?:^|[;&|(])\s*(?:[A-Za-z_][A-Za-z0-9_]*=\S*\s+)*(?:git\s+(?:[^;&|]*\s)?(?:push|pull|fetch|clone|ls-remote|submodule)|gh|ssh|scp|sftp|rsync)(?:\s|$)`)

func usesHostCredentials(command string) bool {
	return hostCredentialCommand.MatchString(command)
}

// sandboxDenial returns the first output line suggesting the sandbox blocked a failed command, or "".
func sandboxDenial(output string, err error, network bool) string {
	// Like Codex, skip usage errors (2) and commands that cannot run or were not found (126, 127).
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() <= 0 || slices.Contains([]int{2, 126, 127}, exitErr.ExitCode()) {
		return ""
	}
	for line := range strings.Lines(output) {
		if slices.ContainsFunc(sandboxLauncherPrefixes, func(prefix string) bool { return strings.HasPrefix(line, prefix) }) {
			return ""
		}
	}
	hints := sandboxDenialHints
	if !network {
		hints = slices.Concat(sandboxDenialHints, offlineDenialHints)
	}
	for line := range strings.Lines(output) {
		lower := strings.ToLower(line)
		if slices.ContainsFunc(hints, func(hint string) bool { return strings.Contains(lower, hint) }) {
			line = strings.TrimSpace(line)
			if runes := []rune(line); len(runes) > 300 {
				line = string(runes[:300]) + "…"
			}
			return line
		}
	}
	return ""
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
		roots := opts.writableRoots(workDir)
		if opts != nil && opts.SharedTemp {
			roots = append(roots, sharedTempDirs()...)
		}
		if !slices.ContainsFunc(roots, func(root string) bool { return pathWithin(root, canonicalPath(value)) || pathWithin(root, value) }) {
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

func ensurePrivateTemp(opts *Options) error {
	if opts == nil || opts.TempDir == "" {
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
	if err := ensurePrivateTemp(opts); err != nil {
		return nil, err
	}
	var sandbox *sandboxOptions
	if workspaceSandboxEnabled(opts) && !escalated {
		sandbox = &sandboxOptions{WorkspaceDir: workspaceDir}
		if opts != nil {
			sandbox.ExtraWritableRoots = append([]string(nil), opts.ExtraWritableRoots...)
			sandbox.TempDir = opts.TempDir
			sandbox.Network = opts.Network
			sandbox.SharedTemp = opts.SharedTemp
			sandbox.DisableLoopback = opts.DisableLoopback
			sandbox.ReadCredentials = opts.ReadCredentials
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
	environment := environmentForTools(opts, escalated)
	if sandbox == nil && opts != nil {
		environment = environmentWithTemp(environment, opts.TempDir)
	}
	if sandbox != nil && !sandbox.Network {
		environment = environmentOffline(environment)
	}
	if sandbox != nil {
		environment = environmentReadOnlyCaches(environment)
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
	SharedTemp         bool
	DisableLoopback    bool
	ReadCredentials    bool
}

func findPowerShell() string {
	if ps, err := exec.LookPath("pwsh"); err == nil {
		return ps
	}
	return "powershell"
}
