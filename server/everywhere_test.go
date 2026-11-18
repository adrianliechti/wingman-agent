package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
	sessions []code.SessionInfo
	// ownFolderOnly makes it an agent whose list without a folder is not
	// every folder's, as any ACP agent the host does not know may be.
	ownFolderOnly bool
	everywhere    atomic.Int32
	here          atomic.Int32
	firstList     chan struct{}
	started       chan struct{}
}

func (a *everywhereAgent) ListsAllSessions() bool { return !a.ownFolderOnly }

func (a *everywhereAgent) ListAllSessions(context.Context) ([]code.SessionInfo, error) {
	if a.everywhere.Add(1) == 1 && a.firstList != nil {
		close(a.started)
		<-a.firstList
	}
	return a.sessions, nil
}

func TestConcurrentWindowsShareAndInvalidateAgentListing(t *testing.T) {
	s, _, _ := newProtocolServer(t)
	h := s.Project.host
	agent := &everywhereAgent{protocolAgent: &protocolAgent{}, firstList: make(chan struct{}), started: make(chan struct{})}
	b := s.Project.bindBackend("two", agent)
	s.Project.backends[b.id] = &backendEntry{runtime: b}
	list := func() bool {
		_, ok := h.listEverywhere([]types.AgentInfo{{Provider: b.id}})[b.id]
		return ok
	}
	stale := make(chan bool, 1)
	go func() { stale <- list() }()
	<-agent.started
	h.forgetEverywhere()
	const windows = 8
	fresh := make(chan bool, windows)
	for range windows {
		go func() { fresh <- list() }()
	}
	for range windows {
		if !<-fresh {
			t.Error("a window missed the fresh listing")
		}
	}
	close(agent.firstList)
	if <-stale || agent.everywhere.Load() != 2 {
		t.Fatalf("the old listing survived invalidation or the windows issued duplicate requests: %d calls", agent.everywhere.Load())
	}
}

func (a *everywhereAgent) ListSessions(context.Context) ([]code.SessionInfo, error) {
	a.here.Add(1)
	if a.ownFolderOnly {
		return a.sessions, nil
	}
	return nil, nil
}

// An agent that is not known to list every folder is asked project by
// project: its answer without a folder may hold only one folder's sessions,
// or none, and the sessions of the other projects would leave the list.
func TestAgentThatListsItsOwnFolderIsListedByProject(t *testing.T) {
	t.Setenv("WINGMAN_HOME", t.TempDir())
	s, _, _ := newProtocolServer(t)
	h := s.Project.host
	agent := &everywhereAgent{protocolAgent: &protocolAgent{}, ownFolderOnly: true, sessions: []code.SessionInfo{{ID: "here", Title: "In this project"}}}
	b := s.Project.bindBackend("two", agent)
	s.Project.backends[b.id] = &backendEntry{runtime: b}
	result, err := h.listSessions(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	items := result.(types.ListSessionsResult).Items
	if len(items) != 1 || items[0].Resource != s.Project.canonicalURI("two", "here") {
		t.Fatalf("the project's own session: %+v", items)
	}
	if agent.everywhere.Load() != 0 || agent.here.Load() != 1 {
		t.Fatalf("listed every folder %d times and the project %d times", agent.everywhere.Load(), agent.here.Load())
	}
}

// An agent's sessions of every folder are listed, each in its own folder, so
// the list holds what the agent's CLI would resume; a window may show their
// folders. A session that names no folder is left out.
func TestAgentSessionsOfEveryFolderAreListedByFolder(t *testing.T) {
	t.Setenv("WINGMAN_HOME", t.TempDir())
	s, _, _ := newProtocolServer(t)
	h := s.Project.host
	root := canonicalPath(t.TempDir())
	other := filepath.Join(root, "other")
	agent := &everywhereAgent{protocolAgent: &protocolAgent{}, sessions: []code.SessionInfo{
		{ID: "here", Title: "In this project", Dir: s.Project.dir},
		{ID: "there", Title: "In another folder", Dir: other},
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
	if _, ok := items["Without a folder"]; ok {
		t.Fatal("a session without a folder is listed")
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

// A session list starts each agent so that it lists its sessions. An agent
// that lists every folder's runs once for that, in the project that stays
// open: every window starting its own would leave an idle agent per window.
func TestListingStartsAnAgentOfEveryFolderOnce(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	h := s.Project.host
	root := canonicalPath(t.TempDir())
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := &everywhereAgent{protocolAgent: &protocolAgent{}}
	b := s.Project.bindBackend("two", agent)
	s.Project.backends[b.id] = &backendEntry{runtime: b}
	previous := listsAllFolders
	listsAllFolders = func(provider string) bool { return provider == "two" }
	t.Cleanup(func() { listsAllFolders = previous })
	var opened atomic.Int32
	h.open = func(string) (*Project, error) {
		opened.Add(1)
		return nil, errors.New("the window's project was opened")
	}

	client := protocolTestClient(t, endpoint, "window")
	directory := fileuri.FromPath(other)
	params := types.ResolveSessionConfigParams{Channel: types.RootResourceURI, Provider: new("two"), WorkingDirectory: &directory, Meta: map[string]json.RawMessage{listingMeta: json.RawMessage("true")}}
	if err := client.Request(t.Context(), "resolveSessionConfig", params, nil); err != nil || opened.Load() != 0 {
		t.Fatalf("listing opened the window's project %d times: %v", opened.Load(), err)
	}
	// A session is still configured in the folder it will work in.
	params.Meta = nil
	if err := client.Request(t.Context(), "resolveSessionConfig", params, nil); err == nil || opened.Load() != 1 {
		t.Fatalf("a session was configured outside its folder: %v", err)
	}
	// An agent that lists only its own folder starts in the window's project.
	listsAllFolders = func(string) bool { return false }
	params.Meta = map[string]json.RawMessage{listingMeta: json.RawMessage("true")}
	if err := client.Request(t.Context(), "resolveSessionConfig", params, nil); err == nil || opened.Load() != 2 {
		t.Fatalf("an agent of one folder was not started in the window's project: %v", err)
	}
}
