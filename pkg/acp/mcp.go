package acp

import (
	"fmt"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"
)

// ValidateMCPServers checks the transport union and the fields needed to hand
// an ACP-provided server to an underlying agent. Stdio is part of baseline ACP;
// optional transports must be advertised through capabilities.
func ValidateMCPServers(servers []acpsdk.McpServer, capabilities acpsdk.McpCapabilities) error {
	if err := validateMCPServers(servers, capabilities); err != nil {
		return acpsdk.NewInvalidParams(err.Error())
	}
	return nil
}

func validateMCPServers(servers []acpsdk.McpServer, capabilities acpsdk.McpCapabilities) error {
	seen := make(map[string]bool, len(servers))
	seenACPIDs := make(map[acpsdk.McpServerAcpId]bool, len(servers))
	for i, server := range servers {
		count := 0
		for _, present := range []bool{server.Stdio != nil, server.Http != nil, server.Sse != nil, server.Acp != nil} {
			if present {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("MCP server %d must specify exactly one transport", i+1)
		}
		name := ""
		switch {
		case server.Stdio != nil:
			name = strings.TrimSpace(server.Stdio.Name)
			if strings.TrimSpace(server.Stdio.Command) == "" {
				return fmt.Errorf("MCP server %d (%s): stdio command is required", i+1, name)
			}
			if err := validateEnvironment(server.Stdio.Env); err != nil {
				return fmt.Errorf("MCP server %d (%s): %w", i+1, name, err)
			}
		case server.Http != nil:
			name = strings.TrimSpace(server.Http.Name)
			if !capabilities.Http {
				return fmt.Errorf("MCP server %d (%s): HTTP transport is not supported", i+1, name)
			}
			if strings.TrimSpace(server.Http.Url) == "" {
				return fmt.Errorf("MCP server %d (%s): HTTP URL is required", i+1, name)
			}
			if err := validateHeaders(server.Http.Headers); err != nil {
				return fmt.Errorf("MCP server %d (%s): %w", i+1, name, err)
			}
		case server.Sse != nil:
			name = strings.TrimSpace(server.Sse.Name)
			if !capabilities.Sse {
				return fmt.Errorf("MCP server %d (%s): SSE transport is not supported", i+1, name)
			}
			if strings.TrimSpace(server.Sse.Url) == "" {
				return fmt.Errorf("MCP server %d (%s): SSE URL is required", i+1, name)
			}
			if err := validateHeaders(server.Sse.Headers); err != nil {
				return fmt.Errorf("MCP server %d (%s): %w", i+1, name, err)
			}
		case server.Acp != nil:
			name = strings.TrimSpace(server.Acp.Name)
			if !capabilities.Acp {
				return fmt.Errorf("MCP server %d (%s): ACP transport is not supported", i+1, name)
			}
			if server.Acp.ServerId == "" {
				return fmt.Errorf("MCP server %d (%s): ACP id is required", i+1, name)
			}
			if seenACPIDs[server.Acp.ServerId] {
				return fmt.Errorf("duplicate ACP MCP server id %q", server.Acp.ServerId)
			}
			seenACPIDs[server.Acp.ServerId] = true
		}
		if name == "" {
			return fmt.Errorf("MCP server %d has no name", i+1)
		}
		if seen[name] {
			return fmt.Errorf("duplicate MCP server name %q", name)
		}
		seen[name] = true
	}
	return nil
}

func validateEnvironment(variables []acpsdk.EnvVariable) error {
	seen := make(map[string]bool, len(variables))
	for _, variable := range variables {
		name := strings.TrimSpace(variable.Name)
		if name == "" {
			return fmt.Errorf("environment variable name is required")
		}
		if seen[name] {
			return fmt.Errorf("duplicate environment variable %q", name)
		}
		seen[name] = true
	}
	return nil
}

func validateHeaders(headers []acpsdk.HttpHeader) error {
	seen := make(map[string]bool, len(headers))
	for _, header := range headers {
		name := strings.TrimSpace(header.Name)
		if name == "" {
			return fmt.Errorf("HTTP header name is required")
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("duplicate HTTP header %q", name)
		}
		seen[key] = true
	}
	return nil
}
