package language

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

func TestEditDiagnosticsStartTheServerAndReportProblems(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WINGMAN_TEST_DIAGNOSTICS_SERVER", "1")
	root := t.TempDir()
	for name, content := range map[string]string{"bad.fixture": "broken", "clean.fixture": "fine", "notes.txt": "broken", "quiet.silent": "broken"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	quiet := false
	server := lsp.Server{Name: "fixture", Command: executable, Args: []string{"-test.run=^TestDiagnosticsServerProcess$"}, ExtensionToLanguage: map[string]string{".fixture": "fixture"}}
	silent := lsp.Server{Name: "silent", Command: executable, Args: server.Args, ExtensionToLanguage: map[string]string{".silent": "fixture"}, Diagnostics: &quiet}
	service := New(root, server, silent)
	t.Cleanup(service.Close)

	for _, name := range []string{"notes.txt", "quiet.silent"} {
		if note := service.EditDiagnostics(t.Context(), filepath.Join(root, name), "broken"); note != "" || service.Running() {
			t.Fatalf("%s: note = %q, running = %v; want nothing and no server", name, note, service.Running())
		}
	}
	note := service.EditDiagnostics(t.Context(), filepath.Join(root, "bad.fixture"), "broken")
	for _, want := range []string{"lsp diagnostics for bad.fixture (1 errors, 1 warnings):", "bad.fixture:4:6 Error: [fixture] broken input", "bad.fixture:1:1 Warning: [fixture] questionable input"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note = %q, want %q", note, want)
		}
	}
	if !service.Running() {
		t.Fatal("the first edit did not start the server")
	}
	if note := service.EditDiagnostics(t.Context(), filepath.Join(root, "clean.fixture"), "fine"); note != "" {
		t.Fatalf("clean file note = %q", note)
	}
}

func TestDiagnosticsServerProcess(t *testing.T) {
	if os.Getenv("WINGMAN_TEST_DIAGNOSTICS_SERVER") != "1" {
		return
	}
	_, conn, _ := protocol.NewServer(context.Background(), &diagnosticTestServer{}, jsonrpc2.NewStream(&diagnosticTestStream{Reader: os.Stdin, Writer: os.Stdout}))
	<-conn.Done()
	os.Exit(0)
}

type diagnosticTestStream struct {
	io.Reader
	io.Writer
}

func (*diagnosticTestStream) Close() error { return nil }

type diagnosticTestServer struct{ protocol.UnimplementedServer }

func (*diagnosticTestServer) Initialize(context.Context, *protocol.InitializeParams) (*protocol.InitializeResult, error) {
	return &protocol.InitializeResult{Capabilities: protocol.ServerCapabilities{
		TextDocumentSync:   protocol.TextDocumentSyncKindFull,
		DiagnosticProvider: &protocol.DiagnosticOptions{},
	}}, nil
}

func (*diagnosticTestServer) Initialized(context.Context, *protocol.InitializedParams) error {
	return nil
}
func (*diagnosticTestServer) DidOpen(context.Context, *protocol.DidOpenTextDocumentParams) error {
	return nil
}
func (*diagnosticTestServer) Shutdown(context.Context) error { return nil }
func (*diagnosticTestServer) Exit(context.Context) error {
	os.Exit(0)
	return nil
}

func (*diagnosticTestServer) Diagnostic(_ context.Context, params *protocol.DocumentDiagnosticParams) (protocol.DocumentDiagnosticReport, error) {
	items := []protocol.Diagnostic{}
	if path, _ := strings.CutPrefix(params.TextDocument.URI.String(), "file://"); strings.HasSuffix(path, "bad.fixture") {
		source := protocol.NewOptional("fixture")
		items = append(items,
			protocol.Diagnostic{Severity: protocol.DiagnosticSeverityWarning, Source: source, Message: protocol.String("questionable input")},
			protocol.Diagnostic{Severity: protocol.DiagnosticSeverityHint, Source: source, Message: protocol.String("a hint")},
			protocol.Diagnostic{
				Range:    protocol.Range{Start: protocol.Position{Line: 3, Character: 5}, End: protocol.Position{Line: 3, Character: 9}},
				Severity: protocol.DiagnosticSeverityError, Source: source,
				Message: &protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: "broken input"},
			},
		)
	}
	return &protocol.RelatedFullDocumentDiagnosticReport{FullDocumentDiagnosticReport: protocol.FullDocumentDiagnosticReport{Kind: "full", Items: items}}, nil
}
