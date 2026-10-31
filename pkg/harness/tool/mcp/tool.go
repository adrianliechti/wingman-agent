package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
	"github.com/adrianliechti/wingman-agent/pkg/text"
)

const (
	listToolsTimeout = 30 * time.Second

	callToolTimeout = 5 * time.Minute

	// maxDescriptionBytes bounds what a single MCP tool can permanently add to
	// every request; verbose servers otherwise inflate the prompt unbounded.
	maxDescriptionBytes = 2 * 1024

	maxApprovalArgumentBytes = 4 * 1024
)

func Tools(ctx context.Context, m *mcp.Manager, elicit *tool.Elicitation) ([]tool.Tool, error) {
	if m == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var tools []tool.Tool

	sessions := m.Sessions()

	for _, serverName := range slices.Sorted(maps.Keys(sessions)) {
		if err := ctx.Err(); err != nil {
			return tools, err
		}
		session := sessions[serverName]
		if session == nil {
			continue
		}
		serverTools, err := ToolsForServer(ctx, serverName, session, elicit)
		if err != nil {
			if ctx.Err() != nil {
				return tools, ctx.Err()
			}
			fmt.Fprintf(os.Stderr, "warning: failed to list tools from MCP server %s: %v\n", serverName, err)
			continue
		}
		tools = append(tools, serverTools...)
	}

	return tools, nil
}

func ToolsForServer(ctx context.Context, serverName string, session *sdkmcp.ClientSession, elicit *tool.Elicitation) ([]tool.Tool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, listToolsTimeout)
	defer cancel()

	var listed []*sdkmcp.Tool
	for mcpTool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		if mcpTool == nil {
			continue
		}
		listed = append(listed, mcpTool)
	}

	slices.SortFunc(listed, func(a, b *sdkmcp.Tool) int {
		return cmp.Compare(a.Name, b.Name)
	})

	tools := make([]tool.Tool, 0, len(listed))
	for _, mcpTool := range listed {
		tools = append(tools, convertTool(serverName, session, *mcpTool, elicit))
	}
	return tools, nil
}

func convertTool(serverName string, session *sdkmcp.ClientSession, mcpTool sdkmcp.Tool, elicit *tool.Elicitation) tool.Tool {
	approve := requiresApproval(mcpTool.Annotations)
	effect := tool.EffectMutates
	if !approve && mcpTool.Annotations.ReadOnlyHint {
		effect = tool.EffectReadOnly
	}
	protocolVersion := ""
	if initialized := session.InitializeResult(); initialized != nil {
		protocolVersion = initialized.ProtocolVersion
	}

	return tool.Tool{
		Name:        fmt.Sprintf("%s_%s", serverName, mcpTool.Name),
		Description: text.TruncateHead(mcpTool.Description, maxDescriptionBytes),
		Effect:      tool.StaticEffect(effect),
		Parameters:  schemaToParams(serverName, mcpTool),
		Telemetry: tool.TelemetryMetadata{
			ToolType:           "extension",
			MCPMethod:          "tools/call",
			MCPProtocolVersion: protocolVersion,
		},
		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			if approve {
				if err := approveCall(ctx, elicit, serverName, mcpTool, args); err != nil {
					return tool.Result{}, err
				}
			}
			return callTool(ctx, session, mcpTool.Name, args)
		},
	}
}

// Unannotated MCP tools default to destructive and open-world.
func requiresApproval(a *sdkmcp.ToolAnnotations) bool {
	if a == nil || (a.DestructiveHint != nil && *a.DestructiveHint) {
		return true
	}
	if a.ReadOnlyHint {
		return false
	}
	return a.DestructiveHint == nil || a.OpenWorldHint == nil || *a.OpenWorldHint
}

func approveCall(ctx context.Context, elicit *tool.Elicitation, serverName string, mcpTool sdkmcp.Tool, args map[string]any) error {
	if elicit == nil || (elicit.Confirm == nil && elicit.Review == nil) {
		return fmt.Errorf("MCP tool %s requires approval, which is unavailable here", mcpTool.Name)
	}

	action := map[string]any{"type": "mcp_tool_call", "server": serverName, "tool_name": mcpTool.Name}
	if mcpTool.Description != "" {
		action["tool_description"] = text.TruncateHead(mcpTool.Description, maxDescriptionBytes)
	}
	if hints := annotationHints(mcpTool.Annotations); len(hints) > 0 {
		action["annotations"] = hints
	}

	message := fmt.Sprintf("Call %s on MCP server %s?", mcpTool.Name, serverName)
	if len(args) > 0 {
		if preview, err := json.MarshalIndent(args, "", "  "); err == nil {
			message += "\n" + text.TruncateHead(string(preview), maxApprovalArgumentBytes)
		}
	}

	verdict, err := elicit.Approve(ctx, tool.Approval{Message: message, Action: action})
	if err != nil {
		return fmt.Errorf("failed to get approval: %w", err)
	}
	return verdict.Err("the user declined calling MCP tool " + mcpTool.Name)
}

func annotationHints(a *sdkmcp.ToolAnnotations) map[string]bool {
	hints := map[string]bool{}
	if a == nil {
		return hints
	}
	if a.ReadOnlyHint {
		hints["readOnlyHint"] = true
	}
	if a.DestructiveHint != nil {
		hints["destructiveHint"] = *a.DestructiveHint
	}
	if a.OpenWorldHint != nil {
		hints["openWorldHint"] = *a.OpenWorldHint
	}
	return hints
}

func schemaToParams(serverName string, mcpTool sdkmcp.Tool) map[string]any {
	empty := map[string]any{"type": "object", "properties": map[string]any{}}

	if mcpTool.InputSchema == nil {
		return empty
	}
	if schema, ok := mcpTool.InputSchema.(map[string]any); ok {
		if schema == nil {
			return empty
		}
		return schema
	}

	schemaBytes, err := json.Marshal(mcpTool.InputSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: marshal schema for %s_%s: %v\n", serverName, mcpTool.Name, err)
		return empty
	}
	var params map[string]any
	if err := json.Unmarshal(schemaBytes, &params); err != nil || params == nil {
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: unmarshal schema for %s_%s: %v\n", serverName, mcpTool.Name, err)
		}
		return empty
	}
	return params
}

func callTool(ctx context.Context, session *sdkmcp.ClientSession, name string, args map[string]any) (tool.Result, error) {
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, callToolTimeout)
	defer cancel()

	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})

	if err != nil {
		return tool.Result{}, fmt.Errorf("MCP tool call failed: %w", err)
	}
	if result == nil {
		return tool.Text(""), nil
	}

	content := extractText(result.Content)

	// The provider codec transmits only the content string, so the failure
	// marker must live in the text for the model to see it.
	if result.IsError {
		if content == "" {
			content = "(no error details)"
		}
		return tool.Error("error: MCP tool returned error: " + content), nil
	}

	return tool.Text(content), nil
}

// CallText runs a tool for callers that need only its text; a tool-reported failure is an error.
func CallText(ctx context.Context, session *sdkmcp.ClientSession, name string, args map[string]any) (string, error) {
	result, err := callTool(ctx, session, name, args)
	if err == nil && result.IsError {
		err = errors.New(result.Content)
	}
	return result.Content, err
}

func extractText(content []sdkmcp.Content) string {
	var parts []string

	for _, c := range content {
		switch v := c.(type) {
		case *sdkmcp.TextContent:
			if v != nil {
				parts = append(parts, v.Text)
			}
		case *sdkmcp.EmbeddedResource:
			if v != nil && v.Resource != nil && v.Resource.Text != "" {
				parts = append(parts, v.Resource.Text)
			}
		}
	}

	return strings.Join(parts, "\n")
}
