package fs

import (
	"os"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

// Tools returns the standard fs tools. allowedReadRoots are absolute paths
// outside the workspace that the read tool is permitted to access (e.g. the
// user's personal skill directories). Write, edit, grep, and glob stay
// strictly sandboxed to the workspace.
func Tools(root *os.Root, allowedReadRoots ...string) []tool.Tool {
	return []tool.Tool{
		ReadTool(root, allowedReadRoots...),
		WriteTool(root),
		EditTool(root),
		GrepTool(root),
		GlobTool(root),
	}
}
