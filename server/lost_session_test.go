package server

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	sdk "github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// A saved session its agent no longer has, such as a Claude session never
// prompted, fails to open as unknown and leaves the list and the disk.
func TestSavedSessionTheAgentLostLeavesTheList(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	alias := sessionAlias{Resource: "ahp-session:/lost", Project: s.Project.dir, Backend: "one", Session: "missing", Created: time.Now()}
	if err := s.Project.saveAlias(alias); err != nil {
		t.Fatal(err)
	}
	s.Project.host.mu.Lock()
	s.Project.host.rememberAliasLocked(alias)
	s.Project.host.mu.Unlock()
	listed := func() bool {
		result, err := s.Project.host.listSessions(json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(result.(types.ListSessionsResult).Items, func(item types.SessionSummary) bool { return item.Resource == alias.Resource })
	}
	if !listed() {
		t.Fatal("the saved session is not listed")
	}

	client := protocolTestClient(t, endpoint, "probe")
	_, _, err := client.Subscribe(t.Context(), alias.Resource)
	var rpc *sdk.RPCError
	if !errors.As(err, &rpc) || rpc.Code != types.ErrorCodeSessionNotFound {
		t.Fatalf("expected SessionNotFound, got %v", err)
	}
	if listed() {
		t.Fatal("a session its agent no longer has is still listed")
	}
	dir, err := s.Project.aliasDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(aliasPath(dir, alias)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("its saved alias remains: %v", err)
	}
}
