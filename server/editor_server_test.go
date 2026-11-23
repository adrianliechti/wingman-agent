package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func newEditorTestServer(t *testing.T, root string) *Server {
	t.Helper()
	workspace, err := code.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace.WarmUp()
	t.Cleanup(workspace.Close)
	ctx, cancel := context.WithCancel(t.Context())
	server := &Server{Project: &Project{workspace: workspace, ctx: ctx, cancel: cancel, scope: workspaceScope(root)}}
	server.Project = webProject(server, root)
	t.Cleanup(server.Close)
	router := chi.NewRouter()
	server.registerRoutes(router)
	server.mux = router
	server.handler = router
	return server
}

func writeEditorTestFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
