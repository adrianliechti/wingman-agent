package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/mcp"
)

const (
	listToolsTimeout = 30 * time.Second

	callToolTimeout = 5 * time.Minute
)

func Tools(ctx context.Context, m *mcp.Manager) ([]tool.Tool, error) {
	var tools []tool.Tool

	ctx, cancel := context.WithTimeout(ctx, listToolsTimeout)
	defer cancel()

	sessions := m.Sessions()

	for _, serverName := range slices.Sorted(maps.Keys(sessions)) {
		session := sessions[serverName]
		result, err := session.ListTools(ctx, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to list tools from MCP server %s: %v\n", serverName, err)
			continue
		}

		slices.SortFunc(result.Tools, func(a, b *sdkmcp.Tool) int {
			return cmp.Compare(a.Name, b.Name)
		})

		for _, mcpTool := range result.Tools {
			tools = append(tools, convertTool(serverName, session, *mcpTool))
		}
	}

	return tools, nil
}

func convertTool(serverName string, session *sdkmcp.ClientSession, mcpTool sdkmcp.Tool) tool.Tool {
	return tool.Tool{
		Name:        fmt.Sprintf("%s_%s", serverName, mcpTool.Name),
		Description: mcpTool.Description,
		Effect:      tool.StaticEffect(tool.EffectMutates),
		Parameters:  schemaToParams(serverName, mcpTool),
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			return callTool(ctx, session, mcpTool.Name, args)
		},
	}
}

func schemaToParams(serverName string, mcpTool sdkmcp.Tool) map[string]any {
	empty := map[string]any{"type": "object", "properties": map[string]any{}}

	if mcpTool.InputSchema == nil {
		return empty
	}
	if schema, ok := mcpTool.InputSchema.(map[string]any); ok {
		return schema
	}

	schemaBytes, err := json.Marshal(mcpTool.InputSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: marshal schema for %s_%s: %v\n", serverName, mcpTool.Name, err)
		return empty
	}
	var params map[string]any
	if err := json.Unmarshal(schemaBytes, &params); err != nil {
		fmt.Fprintf(os.Stderr, "warning: unmarshal schema for %s_%s: %v\n", serverName, mcpTool.Name, err)
		return empty
	}
	return params
}

func callTool(ctx context.Context, session *sdkmcp.ClientSession, name string, args map[string]any) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, callToolTimeout)
	defer cancel()

	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})

	if err != nil {
		return "", fmt.Errorf("MCP tool call failed: %w", err)
	}

	if result.IsError {
		return "", fmt.Errorf("MCP tool returned error: %s", extractText(result.Content))
	}

	return extractText(result.Content), nil
}

func extractText(content []sdkmcp.Content) string {
	var parts []string

	for _, c := range content {
		if text, ok := c.(*sdkmcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}

	return strings.Join(parts, "\n")
}
