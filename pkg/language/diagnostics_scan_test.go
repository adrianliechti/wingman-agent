package language

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/lsp"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

func TestWorkspaceDiagnosticsCollectsIndependentProjectReports(t *testing.T) {
	t.Setenv("WINGMAN_TEST_DIAGNOSTICS_SERVER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	registry, err := lsp.ParseRegistry([]byte(`[
		{"name":"diagnostic-test","markers":["*.fixtureproj"],"servers":[{
			"name":"diagnostic-test","command":"diagnostic-test",
			"args":["-test.run=^TestDiagnosticsServerProcess$"],"languages":["fixture"],"language_id":"fixture"
		}]},
		{"name":"missing-test","markers":["*.failedproj"],"servers":[{
			"name":"missing","command":"missing-test","languages":["missing"],"language_id":"missing"
		}]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, name := range []string{
		"app.fixtureproj", "app.failedproj", "error.fixture", "clean.fixture", "unknown.fixture",
		"nested/app.fixtureproj", "nested/app.failedproj", "nested/error.fixture",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	service := New(root, lsp.WithRegistry(registry), lsp.WithManagedTools(diagnosticTestTools{
		"diagnostic-test": executable,
		"missing-test":    filepath.Join(root, "nonexistent-server"),
	}))
	t.Cleanup(service.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	report := service.Diagnostics(ctx)
	if report.CheckedFiles != 4 || report.DiscoveredFiles != 4 || report.UnknownFiles != 1 || report.DiscoveryTruncated {
		t.Fatalf("workspace coverage = %+v", report)
	}
	if !slices.Equal(report.UnavailableServers, []string{"missing", "missing (nested)"}) {
		t.Fatalf("unavailable servers = %v", report.UnavailableServers)
	}
	if len(report.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %+v", report.Diagnostics)
	}
	for _, file := range []string{"error.fixture", "nested/error.fixture"} {
		values := report.Diagnostics[filepath.Join(root, filepath.FromSlash(file))]
		if len(values) != 1 || values[0].Severity != SeverityError || values[0].Message != "fixture error" {
			t.Errorf("diagnostics for %s = %+v", file, values)
		}
	}
	// A cancelled scan must not reuse results from a previous invocation.
	cancel()
	report = service.Diagnostics(ctx)
	if len(report.Diagnostics) != 0 || report.CheckedFiles != 0 || report.DiscoveredFiles != 0 || report.UnknownFiles != 0 || len(report.UnavailableServers) != 0 {
		t.Fatalf("cancelled scan = %+v", report)
	}
}

type diagnosticTestTools map[string]string

func (tools diagnosticTestTools) Resolve(command string) string { return tools[command] }

func TestDiagnosticsServerProcess(t *testing.T) {
	if os.Getenv("WINGMAN_TEST_DIAGNOSTICS_SERVER") != "1" {
		return
	}
	stream := &diagnosticTestStream{Reader: os.Stdin, Writer: os.Stdout}
	_, conn, _ := protocol.NewServer(context.Background(), &diagnosticTestServer{}, jsonrpc2.NewStream(stream))
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
		DiagnosticProvider: &protocol.DiagnosticOptions{},
	}}, nil
}

func (*diagnosticTestServer) Initialized(context.Context, *protocol.InitializedParams) error {
	return nil
}
func (*diagnosticTestServer) Shutdown(context.Context) error { return nil }
func (*diagnosticTestServer) Exit(context.Context) error {
	os.Exit(0)
	return nil
}

func (*diagnosticTestServer) Diagnostic(_ context.Context, params *protocol.DocumentDiagnosticParams) (protocol.DocumentDiagnosticReport, error) {
	uri := params.TextDocument.URI.String()
	if strings.HasSuffix(uri, "/unknown.fixture") {
		return nil, errors.New("diagnostics unavailable")
	}
	items := []protocol.Diagnostic{}
	if strings.HasSuffix(uri, "/error.fixture") {
		items = append(items, protocol.Diagnostic{Severity: protocol.DiagnosticSeverityError, Message: protocol.String("fixture error")})
	}
	return &protocol.RelatedFullDocumentDiagnosticReport{FullDocumentDiagnosticReport: protocol.FullDocumentDiagnosticReport{
		Kind: "full", Items: items,
	}}, nil
}
