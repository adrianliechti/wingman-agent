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
	tracker := newContentTracker()
	return []tool.Tool{
		readTool(root, tracker, opts.AllowedReadRoots...),
		writeTool(root, tracker, opts.AllowedWriteRoots...),
		editTool(root, tracker, opts.AllowedWriteRoots...),
		GrepTool(root, opts.AllowedReadRoots...),
		GlobTool(root, opts.AllowedReadRoots...),
		ImageTool(root, opts.AllowedReadRoots...),
	}
}
