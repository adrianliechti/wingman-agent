package mcp

import (
	"context"
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

func TestExtractTextIncludesEmbeddedResourceTextAndNilGuards(t *testing.T) {
	if tools, err := Tools(t.Context(), nil, nil); err != nil || len(tools) != 0 {
		t.Fatalf("nil manager: tools=%v err=%v", tools, err)
	}
	got := extractText([]sdkmcp.Content{
		(*sdkmcp.TextContent)(nil),
		&sdkmcp.TextContent{Text: "line 1"},
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{Text: "resource text"}},
	})
	if got != "line 1\nresource text" {
		t.Fatalf("extractText = %q", got)
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
	if result, err := byName["db_delete"].Execute(t.Context(), map[string]any{"id": "42"}); err != nil || result.Content != "ok" {
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
