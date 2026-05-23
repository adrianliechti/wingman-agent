package claude

import (
	"context"
	"io"
	"log/slog"

	"github.com/coder/acp-go-sdk"
)

// Run serves a Claude ACP agent over the supplied reader/writer pair until
// ctx is cancelled or the connection ends. It is the convenience entry
// point for standalone usage; in-process hosts can instead call [New]
// directly and pass the returned *Agent to [acp.NewAgentSideConnection]
// with their own transport (e.g. [io.Pipe]).
func Run(ctx context.Context, opts Options, in io.Reader, out io.Writer, logger *slog.Logger) error {
	a := New(opts)
	conn := acp.NewAgentSideConnection(a, out, in)
	if logger != nil {
		conn.SetLogger(logger)
	}
	a.SetAgentConnection(conn)
	select {
	case <-conn.Done():
	case <-ctx.Done():
	}
	return nil
}
