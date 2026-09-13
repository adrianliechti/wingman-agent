package codex

import (
	"context"
	"net"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/coder/acp-go-sdk"
)

type mcpPermissionClient struct {
	liveClient
	mu       sync.Mutex
	option   acp.PermissionOptionId
	requests []acp.RequestPermissionRequest
	updates  []acp.SessionUpdate
}

func (c *mcpPermissionClient) RequestPermission(_ context.Context, request acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	if c.option == "" {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: c.option}}}, nil
}

func (c *mcpPermissionClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, n.Update)
	return nil
}

func TestStandaloneMCPPermissionsCompleteWithDistinctIDs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agentIO, clientIO := net.Pipe()
		defer agentIO.Close()
		defer clientIO.Close()
		conn := acp.NewAgentSideConnection(&Agent{}, agentIO, agentIO)
		client := &mcpPermissionClient{}
		_ = acp.NewClientSideConnection(client, clientIO, clientIO)
		app := newApprover(context.Background(), conn, "s", acp.ClientCapabilities{})
		seen := map[acp.ToolCallId]bool{}
		for _, tt := range []struct {
			option acp.PermissionOptionId
			action string
		}{
			{optionAllowOnce, "accept"}, {optionAllowAlways, "accept"},
			{optionRejectOnce, "decline"}, {"", "cancel"}, {"unknown", "decline"},
		} {
			client.mu.Lock()
			client.option = tt.option
			client.mu.Unlock()
			result := app.handleElicitation(elicitationParams{Mode: "form", ServerName: "example", Message: "Proceed?"})
			if result.Action != tt.action {
				t.Fatalf("action=%q, want %q", result.Action, tt.action)
			}
			synctest.Wait()
			client.mu.Lock()
			requests, updates := client.requests, client.updates
			client.requests, client.updates = nil, nil
			client.mu.Unlock()
			if len(requests) != 1 || len(updates) != 1 {
				t.Fatalf("requests=%d updates=%d", len(requests), len(updates))
			}
			id := requests[0].ToolCall.ToolCallId
			if id == "" || seen[id] {
				t.Fatalf("reused or missing tool ID %q", id)
			}
			seen[id] = true
			update := updates[0].ToolCallUpdate
			if update == nil || update.ToolCallId != id || update.Status == nil || *update.Status != acp.ToolCallStatusCompleted {
				t.Fatalf("terminal update = %+v", update)
			}
			output, _ := update.RawOutput.(map[string]any)
			if output["action"] != tt.action {
				t.Fatalf("terminal output=%v, want action %q", output, tt.action)
			}
		}
	})
}
