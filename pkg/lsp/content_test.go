package lsp

import (
	"context"
	"net"
	"testing"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

type contentRecorder struct {
	protocol.UnimplementedServer
}

func (contentRecorder) TextDocumentContent(_ context.Context, params *protocol.TextDocumentContentParams) (*protocol.TextDocumentContentResult, error) {
	return &protocol.TextDocumentContentResult{Text: "content of " + string(params.URI)}, nil
}

func TestVirtualContentUsesTheAdvertisedScheme(t *testing.T) {
	session := &Session{rpc: contentRecorder{}, capabilities: protocol.ServerCapabilities{
		Workspace: &protocol.WorkspaceOptions{TextDocumentContent: &protocol.TextDocumentContentOptions{Schemes: []string{"virtual"}}},
	}}
	text, err := session.VirtualContent(context.Background(), "virtual://module/file.txt")
	if err != nil || text != "content of virtual://module/file.txt" {
		t.Fatalf("content = %q, %v", text, err)
	}
	if _, err := session.VirtualContent(context.Background(), "other://module/file.txt"); err == nil {
		t.Fatal("an unadvertised scheme was read")
	}
}

func TestVirtualContentReadsJdtlsClassFiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hostSide, serverSide := net.Pipe()
	session := &Session{server: Server{Name: "jdtls", Command: "jdtls"}}
	_, conn, rpc := protocol.NewClient(ctx, &sessionClient{session: session}, jsonrpc2.NewStream(hostSide))
	defer conn.Close()
	session.conn, session.rpc = conn, rpc
	server := jsonrpc2.NewConn(jsonrpc2.NewStream(serverSide))
	server.Go(ctx, func(_ context.Context, req *jsonrpc2.Request) (any, error) {
		if req.Method() != "java/classFileContents" {
			return nil, jsonrpc2.ErrMethodNotFound
		}
		return "class String {}", nil
	})
	defer server.Close()
	text, err := session.VirtualContent(ctx, "jdt://contents/rt.jar/java.lang/String.class")
	if err != nil || text != "class String {}" {
		t.Fatalf("class file = %q, %v", text, err)
	}
}
