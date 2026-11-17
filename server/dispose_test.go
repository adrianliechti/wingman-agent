package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/layout"
)

// deletingAgent records the sessions it deletes and loads none.
type deletingAgent struct {
	*protocolAgent
	mu      sync.Mutex
	deleted []string
}

func (a *deletingAgent) DeleteSession(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deleted = append(a.deleted, id)
	return nil
}

func listed(t *testing.T, h *Host, resource string) bool {
	t.Helper()
	result, err := h.listSessions(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(result.(types.ListSessionsResult).Items, func(item types.SessionSummary) bool { return item.Resource == resource })
}

// saveTestAlias saves a session as its folder's project would.
func saveTestAlias(t *testing.T, s *Server, alias sessionAlias) string {
	t.Helper()
	dir, err := layout.WingmanPath("ahp", workspaceScope(alias.Project).WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aliasPath(dir, alias), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s.Project.host.mu.Lock()
	s.Project.host.rememberAliasLocked(alias)
	s.Project.host.mu.Unlock()
	return aliasPath(dir, alias)
}

// A session no window has open is deleted without loading it: its agent
// deletes it by its id, even from another folder, and its folder may be gone.
func TestDeletingASessionNoWindowOpenedLoadsNothing(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	agent := &deletingAgent{protocolAgent: &protocolAgent{}}
	b := s.Project.bindBackend("two", agent)
	s.Project.backends[b.id] = &backendEntry{runtime: b}
	gone := filepath.Join(t.TempDir(), "gone")
	alias := sessionAlias{Resource: "ahp-session:/elsewhere", Project: gone, Backend: "two", Session: "native", Created: time.Now()}
	file := saveTestAlias(t, s, alias)
	if !listed(t, s.Project.host, alias.Resource) {
		t.Fatal("the saved session is not listed")
	}

	client := protocolTestClient(t, endpoint, "probe")
	if err := client.Request(t.Context(), "disposeSession", types.DisposeSessionParams{Channel: alias.Resource}, nil); err != nil {
		t.Fatal(err)
	}
	if listed(t, s.Project.host, alias.Resource) {
		t.Fatal("the deleted session is still listed")
	}
	agent.mu.Lock()
	deleted := slices.Clone(agent.deleted)
	agent.mu.Unlock()
	if !slices.Equal(deleted, []string{"native"}) {
		t.Fatalf("the agent deleted %v", deleted)
	}
	if agent.loaded.Load() != 0 {
		t.Fatal("deleting loaded the session")
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("its saved alias remains: %v", err)
	}
}

// Wingman's own session of a folder that is gone is removed from where it
// was kept, since no project can open there.
func TestDeletingAWingmanSessionOfAFolderThatIsGone(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	gone := filepath.Join(t.TempDir(), "gone")
	alias := sessionAlias{Resource: "ahp-session:/" + uuid.NewString(), Project: gone, Backend: code.BuiltinAgentName, Session: uuid.NewString(), Created: time.Now()}
	file := saveTestAlias(t, s, alias)
	client := protocolTestClient(t, endpoint, "probe")
	if err := client.Request(t.Context(), "disposeSession", types.DisposeSessionParams{Channel: alias.Resource}, nil); err != nil {
		t.Fatalf("deleting a session of a folder that is gone: %v", err)
	}
	if listed(t, s.Project.host, alias.Resource) {
		t.Fatal("the deleted session is still listed")
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("its saved alias remains: %v", err)
	}
}
