package mcp

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestToolsForServerCollectsAllPages(t *testing.T) {
	server := sdkmcp.NewServer(
		&sdkmcp.Implementation{Name: "test", Version: "1.0.0"},
		&sdkmcp.ServerOptions{PageSize: 1},
	)
	for _, name := range []string{"third", "first", "second"} {
		sdkmcp.AddTool(server, &sdkmcp.Tool{Name: name},
			func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, any, error) {
				return &sdkmcp.CallToolResult{}, nil, nil
			})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	tools, err := ToolsForServer(ctx, "remote", clientSession, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
		if tool.Telemetry.ToolType != "extension" || tool.Telemetry.MCPMethod != "tools/call" || tool.Telemetry.MCPProtocolVersion == "" {
			t.Fatalf("tool %q telemetry = %#v", tool.Name, tool.Telemetry)
		}
	}
	if want := []string{"remote_first", "remote_second", "remote_third"}; !slices.Equal(names, want) {
		t.Fatalf("tool names = %v, want %v", names, want)
	}
}

func TestExtractContentIncludesEmbeddedResourceTextAndNilGuards(t *testing.T) {
	if tools, err := Tools(t.Context(), nil, nil); err != nil || len(tools) != 0 {
		t.Fatalf("nil manager: tools=%v err=%v", tools, err)
	}
	content, _ := extractContent(t.Context(), []sdkmcp.Content{
		(*sdkmcp.TextContent)(nil),
		&sdkmcp.TextContent{Text: "line 1"},
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{Text: "resource text"}},
	})
	if got := content.Text(); got != "line 1\nresource text" {
		t.Fatalf("content text = %q", got)
	}
}

func TestRequiresApprovalFollowsAnnotationDefaults(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name        string
		annotations *sdkmcp.ToolAnnotations
		want        bool
	}{
		{"unannotated", nil, true},
		{"read only", &sdkmcp.ToolAnnotations{ReadOnlyHint: true}, false},
		{"read only but destructive", &sdkmcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &yes}, true},
		{"additive defaults to open world", &sdkmcp.ToolAnnotations{DestructiveHint: &no}, true},
		{"additive open world", &sdkmcp.ToolAnnotations{DestructiveHint: &no, OpenWorldHint: &yes}, true},
		{"additive closed world", &sdkmcp.ToolAnnotations{DestructiveHint: &no, OpenWorldHint: &no}, false},
		{"closed world defaults to destructive", &sdkmcp.ToolAnnotations{OpenWorldHint: &no}, true},
	} {
		if got := requiresApproval(tc.annotations); got != tc.want {
			t.Errorf("%s: requiresApproval = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMutatingToolsRequireApprovalBeforeCalling(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	var calls []string
	for _, mcpTool := range []*sdkmcp.Tool{
		{Name: "lookup", Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true}},
		{Name: "delete", Description: "Deletes a record."},
	} {
		sdkmcp.AddTool(server, mcpTool,
			func(context.Context, *sdkmcp.CallToolRequest, struct {
				ID string `json:"id,omitempty"`
			}) (*sdkmcp.CallToolResult, any, error) {
				calls = append(calls, mcpTool.Name)
				return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "ok"}}}, nil, nil
			})
	}
	clientSession := connect(t, server)

	var approvals []tool.Approval
	allow := false
	elicit := &tool.Elicitation{Review: func(_ context.Context, req tool.Approval) (tool.Verdict, error) {
		approvals = append(approvals, req)
		if allow {
			return tool.Verdict{Allowed: true}, nil
		}
		return tool.Verdict{Reason: "rejected by review"}, nil
	}}
	tools, err := ToolsForServer(t.Context(), "db", clientSession, elicit)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]tool.Tool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	if effect := byName["db_lookup"].Effect(nil); effect != tool.EffectReadOnly {
		t.Fatalf("lookup effect = %s", effect)
	}

	if _, err := byName["db_lookup"].Execute(t.Context(), nil); err != nil || len(approvals) != 0 {
		t.Fatalf("read-only call: err=%v approvals=%d", err, len(approvals))
	}
	_, err = byName["db_delete"].Execute(t.Context(), map[string]any{"id": "42"})
	if err == nil || !strings.Contains(err.Error(), "rejected by review") {
		t.Fatalf("denied call error = %v", err)
	}
	if !slices.Equal(calls, []string{"lookup"}) {
		t.Fatalf("server calls after denial = %v", calls)
	}
	action := approvals[0].Action
	if action["type"] != "mcp_tool_call" || action["server"] != "db" || action["tool_name"] != "delete" || action["tool_description"] != "Deletes a record." {
		t.Fatalf("approval action = %#v", action)
	}
	if _, ok := action["annotations"]; ok {
		t.Fatalf("unannotated tool sent annotations: %#v", action)
	}
	if !strings.Contains(approvals[0].Message, "delete") || !strings.Contains(approvals[0].Message, `"42"`) {
		t.Fatalf("approval message = %q", approvals[0].Message)
	}

	allow = true
	if result, err := byName["db_delete"].Execute(t.Context(), map[string]any{"id": "42"}); err != nil || result.Content.Text() != "ok" {
		t.Fatalf("approved call: result=%#v err=%v", result, err)
	}
	if !slices.Equal(calls, []string{"lookup", "delete"}) {
		t.Fatalf("server calls after approval = %v", calls)
	}

	unapproved, err := ToolsForServer(t.Context(), "db", clientSession, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range unapproved {
		if tl.Name != "db_delete" {
			continue
		}
		if _, err := tl.Execute(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "requires approval") {
			t.Fatalf("call without an approver: err=%v", err)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("server calls without an approver = %v", calls)
	}
}

func connect(t *testing.T, server *sdkmcp.Server) *sdkmcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clientSession.Close() })
	return clientSession
}

func TestCallToolPreservesImageAttachments(t *testing.T) {
	bitmap := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, bitmap); err != nil {
		t.Fatal(err)
	}

	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "server", Version: "1.0.0"}, nil)
	server.AddTool(&sdkmcp.Tool{
		Name:        "screenshot",
		Description: "Capture screenshot",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{
				&sdkmcp.ImageContent{MIMEType: "image/png", Data: pngBytes.Bytes()},
			},
		}, nil
	})
	server.AddTool(&sdkmcp.Tool{
		Name:        "annotated_screenshot",
		Description: "Capture screenshot with caption",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{
				&sdkmcp.TextContent{Text: "Captured viewport at 1280x720"},
				&sdkmcp.ImageContent{MIMEType: "image/png", Data: pngBytes.Bytes()},
			},
		}, nil
	})
	session := connect(t, server)

	single, err := callTool(t.Context(), session, "screenshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if single.IsError || len(single.Content.ImageURLs()) != 1 {
		t.Fatalf("expected direct image data URL for image-only MCP result, got %+v", single)
	}

	mixed, err := callTool(t.Context(), session, "annotated_screenshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if mixed.IsError || mixed.Content.Text() != "Captured viewport at 1280x720" {
		t.Fatalf("unexpected mixed text result: %+v", mixed)
	}
	if len(mixed.Content.ImageURLs()) != 1 {
		t.Fatalf("expected 1 valid image attachment in mixed MCP result, got %+v", mixed.Content)
	}
}

func TestMCPErrorPreservesOrderedContent(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "server", Version: "1"}, nil)
	server.AddTool(&sdkmcp.Tool{Name: "failure", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{IsError: true, Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "before"}, &sdkmcp.ImageContent{MIMEType: "image/png", Data: buf.Bytes()}, &sdkmcp.TextContent{Text: "after"}}}, nil
	})
	result, err := callTool(t.Context(), connect(t, server), "failure", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || len(result.Content) != 4 || *result.Content[1].Text != "before" || result.Content[2].Image == nil || *result.Content[3].Text != "after" {
		t.Fatalf("error lost blocks: %+v", result)
	}
}

// Every kind of MCP result content must reach the model as text or an image.
// Content that cannot be forwarded is named rather than dropped.
func TestCallToolMapsEveryContentKind(t *testing.T) {
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewNRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	size := int64(2048)
	results := map[string]*sdkmcp.CallToolResult{
		"everything": {Content: []sdkmcp.Content{
			&sdkmcp.TextContent{Text: "intro"},
			&sdkmcp.AudioContent{MIMEType: "audio/wav", Data: make([]byte, 3000)},
			&sdkmcp.ResourceLink{URI: "file:///repo/report.pdf", Name: "report.pdf", Title: "Quarterly report", Description: "Full report with charts.", MIMEType: "application/pdf", Size: &size},
			&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{URI: "file:///repo/notes.md", MIMEType: "text/markdown", Text: "# Notes"}},
			&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{URI: "file:///repo/archive.zip", MIMEType: "application/zip", Blob: []byte("PK\x03\x04 not an image")}},
			&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{URI: "file:///repo/shot.png", MIMEType: "image/png", Blob: pngBytes.Bytes()}},
			&sdkmcp.TextContent{Text: "outro"},
		}},
		"structured_only":       {StructuredContent: map[string]any{"temperature": 21.5, "unit": "C"}},
		"structured_with_copy":  {Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "{\n  \"unit\": \"C\",\n  \"temperature\": 21.5\n}"}}, StructuredContent: map[string]any{"temperature": 21.5, "unit": "C"}},
		"structured_with_prose": {Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "Lookup complete."}}, StructuredContent: map[string]any{"temperature": 21.5, "unit": "C"}},
		"structured_error":      {IsError: true, StructuredContent: map[string]any{"code": "E_LIMIT"}},
	}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "server", Version: "1"}, nil)
	for name, result := range results {
		server.AddTool(&sdkmcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return result, nil
		})
	}
	session := connect(t, server)
	call := func(name string) tool.Result {
		t.Helper()
		result, err := callTool(t.Context(), session, name, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return result
	}

	everything := call("everything")
	var kinds []string
	for _, block := range everything.Content {
		if block.Image != nil {
			kinds = append(kinds, "image")
		} else {
			kinds = append(kinds, "text")
		}
	}
	if want := []string{"text", "text", "text", "text", "text", "image", "text"}; !slices.Equal(kinds, want) {
		t.Fatalf("block kinds = %v, want %v\n%s", kinds, want, everything.Content.Text())
	}
	text := everything.Content.Text()
	for _, want := range []string{
		"intro",
		"[audio attachment not shown (audio/wav, 2.9 KB): only text and images are forwarded]",
		"[resource link: Quarterly report — file:///repo/report.pdf (application/pdf, 2.0 KB)]\nFull report with charts.",
		"# Notes",
		"[embedded resource not shown: file:///repo/archive.zip (application/zip, 17 B): only text and images are forwarded]",
		"outro",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("result lacks %q:\n%s", want, text)
		}
	}
	if everything.IsError || len(everything.Content.ImageURLs()) != 1 {
		t.Fatalf("embedded image lost: %+v", everything.Content)
	}

	if got := call("structured_only").Content.Text(); got != `{"temperature":21.5,"unit":"C"}` {
		t.Fatalf("structured-only result = %q", got)
	}
	if got := call("structured_with_copy").Content; len(got) != 1 {
		t.Fatalf("structured content duplicated its text copy: %q", got.Text())
	}
	if got := call("structured_with_prose").Content.Text(); got != "Lookup complete.\n"+`{"temperature":21.5,"unit":"C"}` {
		t.Fatalf("structured content dropped beside prose: %q", got)
	}
	failed := call("structured_error")
	if !failed.IsError || failed.Content.Text() != "error: MCP tool returned error\n"+`{"code":"E_LIMIT"}` {
		t.Fatalf("structured error = %+v %q", failed.IsError, failed.Content.Text())
	}
}
