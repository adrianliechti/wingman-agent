package shell

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/adrianliechti/wingman-agent/internal/sandboxpolicy"
)

// ExecutionContext states the configured shell boundary for the prompt, not
// per-command grants; the exec tool describes how to work within it.
func ExecutionContext(workspace string, opts *Options) string {
	var b strings.Builder
	switch {
	case os.Getenv(workspaceSandboxActiveEnv) == "1":
		b.WriteString("- Shell sandbox: inherited; the parent determines filesystem and network access. This runner cannot expand it.\n")
	case !workspaceSandboxEnabled(opts):
		b.WriteString("- Shell sandbox: disabled by host configuration; commands run with host user permissions.\n")
	default:
		b.WriteString("- Shell sandbox: enabled (workspace-write).\n")
		fmt.Fprintf(&b, "- Shell writable roots: %s.\n", formatPaths(opts.writableRoots(workspace)))
		switch {
		case opts != nil && opts.Network:
			b.WriteString("- Shell IP network: enabled by host configuration.\n")
		case runtime.GOOS == "linux" || (runtime.GOOS == "darwin" && (opts == nil || !opts.DisableLoopback)):
			b.WriteString("- Shell IP network: loopback only by default.\n")
		default:
			b.WriteString("- Shell IP network: blocked by default.\n")
		}
		if opts != nil && opts.SharedTemp {
			if dirs := sharedTempDirs(); len(dirs) > 0 {
				fmt.Fprintf(&b, "- Shell shared temporary directories, also writable: %s.\n", formatPaths(dirs))
			}
		}
	}
	if opts != nil && opts.TempDir != "" {
		fmt.Fprintf(&b, "- Shell temporary storage: TMPDIR = TEMP = TMP = `%s`, removed on session close. Store persistent outputs in a project directory.", opts.TempDir)
	} else if workspaceSandboxEnabled(opts) {
		b.WriteString("- Use the command's private TMPDIR/TEMP/TMP for temporary files and caches; inspect their values in the shell.")
	}
	return strings.TrimSpace(b.String())
}

// hiddenCredentialsText and protectedPathsText name what the sandbox denies.
// Both render the lists the backends enforce, so the model-facing description
// cannot drift from the policy.
func hiddenCredentialsText() string {
	text := "~/{" + strings.Join(sandboxpolicy.CredentialHomePaths, ",") + "}"
	if len(sandboxpolicy.CredentialStateFiles) > 0 {
		text += " and Wingman's " + strings.Join(sandboxpolicy.CredentialStateFiles, ", ")
	}
	return text
}

func protectedPathsText() string {
	return fmt.Sprintf("{%s} and %s at any depth, plus root-level {%s}",
		strings.Join(protectedDirs, ","), strings.Join(protectedFiles, ", "), strings.Join(protectedRootFiles, ","))
}

func formatPaths(paths []string) string {
	quoted := make([]string, len(paths))
	for i, path := range paths {
		quoted[i] = "`" + path + "`"
	}
	return strings.Join(quoted, ", ")
}
