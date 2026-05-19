package fs

import (
	"os"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// Options configures the fs toolset returned by Tools.
//
// AllowedReadRoots are absolute paths outside the workspace that `read`,
// `grep`, and `glob` are additionally permitted to access (e.g. discovered
// skill directories, the memory directory).
//
// AllowedWriteRoots are absolute paths outside the workspace that `write`
// and `edit` are additionally permitted to modify. Read access to a write
// root is not implicit — list the same path in AllowedReadRoots if reads
// should also be allowed.
type Options struct {
	AllowedReadRoots  []string
	AllowedWriteRoots []string
}

// Tools returns the standard fs tools bound to root and configured by opts.
// Pass nil to sandbox everything to the workspace.
func Tools(root *os.Root, opts *Options) []tool.Tool {
	if opts == nil {
		opts = &Options{}
	}
	return []tool.Tool{
		ReadTool(root, opts.AllowedReadRoots...),
		WriteTool(root, opts.AllowedWriteRoots...),
		EditTool(root, opts.AllowedWriteRoots...),
		GrepTool(root, opts.AllowedReadRoots...),
		GlobTool(root, opts.AllowedReadRoots...),
	}
}
