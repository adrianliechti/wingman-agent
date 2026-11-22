package fs

import (
	"os"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

type Options struct {
	// AllowedReadRoots confines reads to the workspace and these roots; empty allows ordinary local files outside the workspace.
	AllowedReadRoots []string
	// AllowedWriteRoots are writable besides the workspace; empty means the workspace only.
	AllowedWriteRoots []string
	Freshness         *Freshness

	// MaxReadFileBytes switches larger files to bounded streaming reads.
	// Zero uses MaxReadFileBytes; a negative value loads files without a size limit.
	MaxReadFileBytes int64
}

func Tools(root *os.Root, opts *Options) []tool.Tool {
	if opts == nil {
		opts = &Options{}
	}
	return []tool.Tool{
		readTool(root, opts.Freshness, opts.MaxReadFileBytes, opts.AllowedReadRoots, opts.AllowedWriteRoots),
		batchEditTool(root, opts.Freshness, opts.AllowedWriteRoots...),
		deleteTool(root, opts.Freshness, opts.AllowedWriteRoots...),
		GrepTool(root, opts.AllowedReadRoots...),
		GlobTool(root, opts.AllowedReadRoots...),
	}
}

const writePathScope = "Workspace-relative or absolute. Paths outside the workspace must be under one of the File-tool writable roots listed in the session."

// readPathScope describes the same boundary on all three read tools.
func readPathScope(roots []string) string {
	if len(roots) > 0 {
		return "Within the workspace or a configured read root."
	}
	return "May be outside the workspace. Credential stores are excluded."
}
