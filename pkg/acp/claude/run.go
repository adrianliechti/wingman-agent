package claude

import (
	"context"
	"io"
	"log/slog"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
	acpsdk "github.com/coder/acp-go-sdk"
)

func Run(ctx context.Context, opts Options, in io.Reader, out io.Writer, logger *slog.Logger) error {
	a := New(opts)
	defer a.Close()
	writer := acp.NewConnectionWriter(out, 0)
	defer writer.Close()
	conn := acpsdk.NewAgentSideConnection(a, writer, in)
	if logger != nil {
		conn.SetLogger(logger)
	}
	a.SetAgentConnection(conn)
	select {
	case <-conn.Done():
	case <-writer.Done():
		return writer.Err()
	case <-ctx.Done():
	}
	return nil
}
