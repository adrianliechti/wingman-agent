package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// Run Microsoft's black-box suite against an isolated multi-project host:
// WINGMAN_AHP_CONFORMANCE=/path/to/agent-host-protocol-contrib/tools/ahp-conformance/src/cli.ts go test ./server -run '^TestAHPUpstreamConformance$' -v
// In agent-host-protocol-contrib, first run:
//
//	npm ci --prefix tools/ahp-conformance
//	cargo build --locked -p deterministic-openai
//
// WINGMAN_AHP_FIXTURE can override the fixture binary's default target/debug path.
// WINGMAN_AHP_CONFORMANCE_REPORT preserves the JSON report outside test cleanup.
// The fixture exercises model turns and tools locally, without a real model.
// Tested with agent-host-protocol-contrib revision cfdd74a4b9aa5710d2bee3ebbdac587f7a13f3e5.
func TestAHPUpstreamConformance(t *testing.T) {
	cli := os.Getenv("WINGMAN_AHP_CONFORMANCE")
	if cli == "" {
		t.Skip("set WINGMAN_AHP_CONFORMANCE to Microsoft's installed conformance CLI")
	}
	if _, err := os.Stat(cli); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node 24+ is required for upstream conformance")
	}
	testenv.WingmanHome(t)
	t.Setenv("WINGMAN_URL", fixtureGateway(t, upstreamFixture(t, cli)))
	t.Setenv("WINGMAN_MODEL", "fixture-model")
	t.Setenv("WINGMAN_MODEL_COMPLEX", "fixture-model")
	t.Setenv("WINGMAN_MODEL_UTILITY", "fixture-model")
	root := canonicalPath(t.TempDir())
	host, err := NewHost(t.Context(), &HostOptions{Roots: []string{root}, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	server := httptest.NewServer(host)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ahp"
	// A host grants writes, watches and terminals to projects used by a session.
	// Provision the shared scratch tree so independent resource tests can run.
	client := protocolTestClient(t, endpoint, "conformance-fixture")
	if err := client.Request(t.Context(), "createSession", types.CreateSessionParams{
		Channel: "ahp-session:/conformance-workspace", Provider: new("wingman"),
		WorkingDirectories: []string{fileuri.FromPath(root)},
	}, nil); err != nil {
		t.Fatal(err)
	}
	report := os.Getenv("WINGMAN_AHP_CONFORMANCE_REPORT")
	if report == "" {
		report = filepath.Join(t.TempDir(), "conformance.json")
	}
	config := map[string]any{
		"target":    map[string]any{"url": endpoint},
		"agent":     map[string]any{"provider": "wingman", "model": "fixture-model"},
		"model":     map[string]any{"enabled": true, "deterministic": true},
		"workspace": map[string]any{"scratchDir": root, "sharedFilesystem": true},
		"run":       map[string]any{"concurrency": 4, "timeoutMs": 30000, "proposed": true},
		"report":    map[string]any{"reporter": "cli", "open": false, "jsonFile": report},
	}
	raw, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(t.TempDir(), "conformance.json")
	if err := os.WriteFile(configFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, cli, configFile, "--reporter", "cli", "--no-open")
	output, err := cmd.CombinedOutput()
	t.Logf("Upstream conformance report: %s\n%s", report, output)
	if err != nil {
		t.Fatalf("upstream conformance failed: %v", err)
	}
}

func upstreamFixture(t *testing.T, cli string) string {
	t.Helper()
	fixture := os.Getenv("WINGMAN_AHP_FIXTURE")
	if fixture == "" {
		fixture = filepath.Join(filepath.Dir(cli), "../../../target/debug/deterministic-openai")
	}
	cmd := exec.Command(fixture, "--bind", "127.0.0.1:0")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		stdout.Close()
		t.Fatalf("start deterministic fixture: %v; build it with cargo build --locked -p deterministic-openai or set WINGMAN_AHP_FIXTURE", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	ready := make(chan string, 1)
	go func() {
		defer close(ready)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if url, ok := strings.CutPrefix(scanner.Text(), "Deterministic OpenAI fixture listening at "); ok {
				ready <- url
				return
			}
		}
	}()
	select {
	case url := <-ready:
		if url == "" {
			t.Fatal("deterministic fixture exited before reporting its URL")
		}
		return url
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	case <-time.After(15 * time.Second):
		t.Fatal("deterministic fixture did not start within 15 seconds")
	}
	return ""
}

// Microsoft's fixture only accepts string content and untyped messages. Adapt
// the equivalent Responses API text-part encoding at the model boundary; the
// AHP connection and the fixture's directives and responses remain untouched.
func fixtureGateway(t *testing.T, endpoint string) string {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/responses" {
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err == nil {
				body, err = fixtureRequest(body)
			}
			if err != nil {
				t.Errorf("adapt fixture request: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gateway.Close)
	return gateway.URL
}

func fixtureRequest(body []byte) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	var input []map[string]json.RawMessage
	if err := json.Unmarshal(request["input"], &input); err != nil {
		return nil, err
	}
	for _, item := range input {
		if string(item["type"]) == `"message"` {
			delete(item, "type")
		}
		for _, field := range []string{"content", "output"} {
			content := bytes.TrimSpace(item[field])
			if len(content) == 0 || content[0] != '[' {
				continue
			}
			var parts []struct {
				Type string  `json:"type"`
				Text *string `json:"text"`
			}
			if err := json.Unmarshal(content, &parts); err != nil {
				return nil, err
			}
			var text strings.Builder
			for _, part := range parts {
				if (part.Type != "input_text" && part.Type != "output_text") || part.Text == nil {
					return nil, fmt.Errorf("fixture cannot represent %s part %q as text", field, part.Type)
				}
				text.WriteString(*part.Text)
			}
			item[field], _ = json.Marshal(text.String())
		}
	}
	request["input"], _ = json.Marshal(input)
	return json.Marshal(request)
}

func TestAHPFixtureRequest(t *testing.T) {
	input := `{"model":"fixture-model","input":[
		{"role":"user","content":[{"type":"input_text","text":"[AHP-TEST v1]\n"},{"type":"input_text","text":"雪\n[/AHP-TEST]"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"previous reply"}]},
		{"type":"function_call","call_id":"call-1","name":"read_file","arguments":"{}"},
		{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":"file contents"}]}
	],"stream":true}`
	want := `{"model":"fixture-model","input":[
		{"role":"user","content":"[AHP-TEST v1]\n雪\n[/AHP-TEST]"},
		{"role":"assistant","content":"previous reply"},
		{"type":"function_call","call_id":"call-1","name":"read_file","arguments":"{}"},
		{"type":"function_call_output","call_id":"call-1","output":"file contents"}
	],"stream":true}`
	got, err := fixtureRequest([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	var gotJSON, wantJSON any
	if err := json.Unmarshal(got, &gotJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatalf("adapted request = %s, want %s", got, want)
	}
	// An unsupported modality must fail visibly, never silently disappear.
	_, err = fixtureRequest([]byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`))
	if err == nil {
		t.Fatal("image input was silently flattened to text")
	}
}
