package shell

import (
	"fmt"
	"os"
	"strings"
)

// ExecutionContext describes the configured shell boundary for the prompt, not per-command grants.
func ExecutionContext(workspace string, opts *Options) string {
	var b strings.Builder
	switch {
	case os.Getenv(workspaceSandboxActiveEnv) == "1":
		b.WriteString("- Shell sandbox: inherited; the parent determines filesystem and network access. This runner cannot expand it.\n")
	case !workspaceSandboxEnabled(opts):
		b.WriteString("- Shell sandbox: disabled by host configuration; commands run with host user permissions.\n")
	default:
		b.WriteString("- Shell sandbox: enabled (workspace-write). Treat protected project configuration as read-only; private temp permits configuration fixtures. Known credential stores are hidden.\n")
		fmt.Fprintf(&b, "- Shell writable roots: %s. Choose command output paths and workdir within these roots. Shared host temporary directories are not granted.\n", formatPaths(opts.writableRoots(workspace)))
		if opts != nil && opts.Network {
			b.WriteString("- Shell IP network: enabled by host configuration.\n")
		} else {
			b.WriteString("- Shell IP network: blocked by default; commands requiring network access need a require_permissions request with network=true and justification.\n")
		}
	}
	if opts != nil && opts.TempDir != "" {
		fmt.Fprintf(&b, "- Shell temporary storage: TMPDIR = TEMP = TMP = `%s`, removed on session close. Store persistent outputs in a project directory.", opts.TempDir)
		if workspaceSandboxEnabled(opts) {
			b.WriteString(" Escalated commands use the host temporary directory; pass staged files to them by absolute path.")
		}
	} else if workspaceSandboxEnabled(opts) {
		b.WriteString("- Use the command's private TMPDIR/TEMP/TMP for temporary files and caches; inspect their values in the shell.")
	}
	return strings.TrimSpace(b.String())
}

func formatPaths(paths []string) string {
	quoted := make([]string, len(paths))
	for i, path := range paths {
		quoted[i] = "`" + path + "`"
	}
	return strings.Join(quoted, ", ")
}
