package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// everywhereAgent keeps sessions of many folders, as Claude Code and Codex do.
type everywhereAgent struct {
	*protocolAgent
	sessions   []code.SessionInfo
	everywhere atomic.Int32
	here       atomic.Int32
}

func (a *everywhereAgent) ListAllSessions(context.Context) ([]code.SessionInfo, error) {
	a.everywhere.Add(1)
	return a.sessions, nil
}

func (a *everywhereAgent) ListSessions(context.Context) ([]code.SessionInfo, error) {
	a.here.Add(1)
	return nil, nil
}

// An agent's sessions of every folder are listed, each in its own folder, so
// the list holds what the agent's CLI would resume; a window may show their
// folders. Folders the host cannot open are left out.
func TestAgentSessionsOfEveryFolderAreListedByFolder(t *testing.T) {
	t.Setenv("WINGMAN_HOME", t.TempDir())
	s, _, _ := newProtocolServer(t)
	h := s.Project.host
	root := canonicalPath(t.TempDir())
	other := filepath.Join(root, "other")
	h.mu.Lock()
	h.roots = []string{root, s.Project.dir}
	h.mu.Unlock()
	agent := &everywhereAgent{protocolAgent: &protocolAgent{}, sessions: []code.SessionInfo{
		{ID: "here", Title: "In this project", Dir: s.Project.dir},
		{ID: "there", Title: "In another folder", Dir: other},
		{ID: "away", Title: "Outside the roots", Dir: canonicalPath(t.TempDir())},
		{ID: "nowhere", Title: "Without a folder"},
	}}
	b := s.Project.bindBackend("two", agent)
	s.Project.backends[b.id] = &backendEntry{runtime: b}

	list := func() map[string]types.SessionSummary {
		result, err := h.listSessions(json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		items := map[string]types.SessionSummary{}
		for _, item := range result.(types.ListSessionsResult).Items {
			items[item.Title] = item
		}
		return items
	}
	items := list()
	if item, ok := items["In this project"]; !ok || item.Resource != s.Project.canonicalURI("two", "here") {
		t.Fatalf("the project's own session: %+v", item)
	}
	there, ok := items["In another folder"]
	if !ok || there.Resource != canonicalSessionURI("two", "there", other) || there.Project == nil || there.Project.Uri != fileuri.FromPath(other) {
		t.Fatalf("a session of another folder: %+v", there)
	}
	for _, title := range []string{"Outside the roots", "Without a folder"} {
		if _, ok := items[title]; ok {
			t.Fatalf("%q is listed", title)
		}
	}
	if !h.known(other) {
		t.Fatal("a window may not show the folder of a listed session")
	}
	if agent.here.Load() != 0 {
		t.Fatal("the agent's sessions were listed by project as well")
	}
	// Listing again soon reuses the agent's list; a deleted session drops it.
	list()
	if got := agent.everywhere.Load(); got != 1 {
		t.Fatalf("the agent listed every folder %d times", got)
	}
	h.forgetEverywhere()
	list()
	if got := agent.everywhere.Load(); got != 2 {
		t.Fatalf("after a deletion the agent listed every folder %d times", got)
	}
}
