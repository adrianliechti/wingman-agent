package fs

import (
	"os"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

type Options struct {
	AllowedReadRoots  []string
	AllowedWriteRoots []string
}

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
