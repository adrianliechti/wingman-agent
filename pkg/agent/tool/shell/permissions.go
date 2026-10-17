package shell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/internal/sandboxpolicy"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// executionOptions validates permission requests before either review or launch.
// A request expands capabilities for one command, never for its later retries.
func executionOptions(workspace string, args map[string]any, base *Options) (*Options, string, error) {
	opts := &Options{}
	if base != nil {
		*opts = *base
		opts.ExtraWritableRoots = slices.Clone(base.ExtraWritableRoots)
	}
	mode, _ := args["sandbox_permissions"].(string)
	if mode == "" {
		mode = "use_default"
	}
	switch mode {
	case "use_default", "require_permissions", "require_escalated":
	default:
		return nil, "", fmt.Errorf("invalid sandbox_permissions %q", mode)
	}
	if mode != "use_default" && !workspaceSandboxEnabled() {
		return nil, "", fmt.Errorf("sandbox permission requests require an active, non-inherited sandbox")
	}
	network, _ := args["network"].(bool)
	var roots []string
	if raw, present := args["writable_roots"]; present {
		values, ok := raw.([]any)
		if !ok {
			return nil, "", fmt.Errorf("writable_roots must be an array of directory paths")
		}
		for _, value := range values {
			path, ok := value.(string)
			if !ok || strings.TrimSpace(path) == "" {
				return nil, "", fmt.Errorf("writable_roots must contain nonempty directory paths")
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(workspace, path)
			}
			// A protected directory must not become the root of a new grant:
			// backend exclusions beneath that root would miss the directory itself.
			if sandboxpolicy.ProtectedRelative(path) || sandboxpolicy.ProtectedRelative(canonicalPath(path)) {
				return nil, "", fmt.Errorf("cannot grant a protected configuration directory %q; request explicit escalation", path)
			}
			path = canonicalPath(path)
			if err := validateWritableRoot(path); err != nil {
				return nil, "", err
			}
			if err := sandboxpolicy.CheckWrite(workspace, path); err != nil {
				return nil, "", err
			}
			if info, err := os.Stat(path); err != nil || !info.IsDir() {
				return nil, "", fmt.Errorf("requested writable root %q is not an accessible directory", path)
			}
			roots = append(roots, path)
		}
	}
	if (network || len(roots) > 0) && mode != "require_permissions" {
		return nil, "", fmt.Errorf("network and writable_roots require sandbox_permissions require_permissions")
	}
	if mode == "require_permissions" && !network && len(roots) == 0 {
		return nil, "", fmt.Errorf("require_permissions needs network or writable_roots")
	}
	if mode != "use_default" {
		if justification, _ := args["justification"].(string); strings.TrimSpace(justification) == "" {
			return nil, "", fmt.Errorf("permission requests require a justification")
		}
	}
	opts.Network = opts.Network || network
	opts.ExtraWritableRoots = append(opts.ExtraWritableRoots, roots...)
	return opts, mode, nil
}

func validateWritableRoot(root string) error {
	home, _ := os.UserHomeDir()
	sharedTemp := root == canonicalPath(os.TempDir()) || (runtime.GOOS != "windows" && root == canonicalPath("/tmp"))
	if filepath.Dir(root) == root || (home != "" && pathWithin(root, home)) || sharedTemp {
		return fmt.Errorf("refusing broad writable root %q; choose a project or dedicated temporary directory", root)
	}
	return nil
}

func confirmExecution(ctx context.Context, elicit *tool.Elicitation, command, workspace, workdir, mode, justification string, opts *Options) error {
	sandboxed := workspaceSandboxEnabled() && mode != "require_escalated"
	if mode == "use_default" && !IsDangerousCommand(command) {
		return nil
	}
	if elicit == nil || (elicit.Review == nil && elicit.Confirm == nil) {
		return fmt.Errorf("command requires approval, which is unavailable here")
	}
	kind := "shell_command"
	if mode == "require_escalated" {
		kind = "sandbox_escalation"
	} else if mode == "require_permissions" {
		kind = "sandbox_permissions"
	}
	roots := writableRootsFor(workspace, opts.ExtraWritableRoots)
	if opts.TempDir != "" {
		roots = append(roots, canonicalPath(opts.TempDir))
	}
	policy := map[string]any{
		"sandboxed": sandboxed, "network": !sandboxed || sandboxNetworkEnabled() || opts.Network,
		"writable_roots": roots, "protected_directories": protectedDirs,
		"protected_files":   append(slices.Clone(protectedFiles), protectedRootFiles...),
		"denied_read_paths": credentialPaths(), "host_unix_sockets": false,
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		policy["host_unix_sockets"] = "not specified by this backend"
	}
	if runtime.GOOS != "darwin" {
		policy["protected_path_limit"] = "Only existing protected paths at each writable root are enforced; newly created and nested protected paths are not covered."
	}
	if !sandboxed {
		policy = map[string]any{"sandboxed": false, "network": true, "filesystem": "user privileges", "host_unix_sockets": true}
	}
	text := command + "  [in " + workdir + "]"
	if mode == "require_escalated" {
		text += "  [outside the sandbox]"
	} else if mode == "require_permissions" {
		text += fmt.Sprintf("\nRequested access for this command: network=%t, writable roots=%v", opts.Network, opts.ExtraWritableRoots)
	}
	if justification != "" {
		text += "\nJustification: " + justification
	}
	// Review can take time. A changed symlink must not silently redirect a
	// reviewed grant, workspace, or working directory before launch.
	boundaries := map[string]string{}
	for _, path := range append([]string{workspace, workdir, opts.TempDir}, opts.ExtraWritableRoots...) {
		if path != "" {
			boundaries[path] = canonicalPath(path)
		}
	}
	verdict, err := elicit.Approve(ctx, tool.Approval{
		Message: "❯ " + approvalDisplayText(text),
		Action: map[string]any{"type": kind, "command": command, "workdir": workdir,
			"workspace": workspace, "execution_policy": policy, "justification": justification},
	})
	if err != nil {
		return fmt.Errorf("failed to get approval: %w", err)
	}
	if err := verdict.Err("command execution denied by user"); err != nil {
		return err
	}
	for path, approved := range boundaries {
		if canonicalPath(path) != approved {
			return fmt.Errorf("execution path %q changed during approval; request review again", path)
		}
	}
	return nil
}
