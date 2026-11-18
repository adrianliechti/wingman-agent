package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

// deletingAgent records the sessions it deletes and loads none.
type deletingAgent struct {
	*protocolAgent
	mu      sync.Mutex
	deleted []string
}

type deletingFleetAgent struct {
	*deletingAgent
	sessions []code.SessionInfo
}

func (a *deletingFleetAgent) ListsAllSessions() bool { return true }

func (a *deletingFleetAgent) ListAllSessions(context.Context) ([]code.SessionInfo, error) {
	return a.sessions, nil
}

func TestDisposeUsesListedProjectAndSessionIdentity(t *testing.T) {
	t.Setenv("WINGMAN_HOME", t.TempDir())
	s, _, _ := newProtocolServer(t)
	h := everyProject(s)
	dir := filepath.Join(canonicalPath(t.TempDir()), "listed")
	agent := &deletingFleetAgent{&deletingAgent{protocolAgent: &protocolAgent{}}, []code.SessionInfo{{ID: "saved", Dir: dir}}}
	b := s.Project.bindBackend("two", agent)
	s.Project.backends[b.id] = &backendEntry{runtime: b}
	if _, _, err := h.ensureSession(s.Project.canonicalURI("two", "saved")); !errors.Is(err, errUnknownSession) || agent.loaded.Load() != 0 {
		t.Fatalf("resuming a session under another project: %v, %d loads", err, agent.loaded.Load())
	}
	for _, path := range []string{canonicalPath(t.TempDir()), filepath.Join(filepath.Dir(dir), "other")} {
		if _, err := h.disposeUnopened(canonicalSessionURI("two", "saved", path)); err == nil {
			t.Fatalf("accepted a session identity from %s", path)
		}
	}
	resource := canonicalSessionURI("two", "saved", dir)
	alias := sessionAlias{Resource: "ahp-session:/named", Project: dir, Session: "saved", Backend: "two"}
	saveTestAlias(t, s, alias)
	if _, err := h.disposeUnopened(resource); err == nil || len(agent.deleted) != 0 {
		t.Fatal("a canonical URI disposed a session owned by an alias")
	}
	if _, err := h.disposeUnopened(alias.Resource); err != nil || !slices.Equal(agent.deleted, []string{"saved"}) {
		t.Fatalf("disposing by the session's alias: %v, %v", agent.deleted, err)
	}
}

func (a *deletingAgent) DeleteSession(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deleted = append(a.deleted, id)
	return nil
}

func listed(t *testing.T, h *Host, resource string) bool {
	t.Helper()
	return slices.ContainsFunc(reviewHostCatalog(t, h), func(item types.SessionSummary) bool { return item.Resource == resource })
}

// saveTestAlias saves a session as its folder's project would.
func saveTestAlias(t *testing.T, s *Server, alias sessionAlias) string {
	t.Helper()
	if err := writeAlias(alias); err != nil {
		t.Fatal(err)
	}
	s.Project.host.mu.Lock()
	s.Project.host.rememberAliasLocked(alias)
	s.Project.host.mu.Unlock()
	dir, err := aliasDirectory(alias.Project)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := sessionAlias{Resource: "ahp-session:/elsewhere", Project: gone, Backend: "two", Session: "native", Created: time.Now()}
	file := saveTestAlias(t, s, alias)
	if !listed(t, s.Project.host, alias.Resource) {
		t.Fatal("the saved session is not listed")
	}
	// A session whose folder is gone leaves the list, and is deleted all the same.
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if listed(t, s.Project.host, alias.Resource) {
		t.Fatal("a session whose folder is gone is listed")
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
	// Old worktree aliases did not record the shared repository store.
	writeJournal := func() string {
		store := code.SessionsDir(t.TempDir())
		if err := os.MkdirAll(store, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(store, alias.Session+".jsonl")
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	journal, duplicate := writeJournal(), writeJournal()
	if _, err := builtinStore(alias); err == nil {
		t.Fatal("selected a store for an ambiguous session ID")
	}
	if err := os.Remove(duplicate); err != nil {
		t.Fatal(err)
	}
	client := protocolTestClient(t, endpoint, "probe")
	if err := client.Request(t.Context(), "disposeSession", types.DisposeSessionParams{Channel: alias.Resource}, nil); err != nil {
		t.Fatalf("deleting a session of a folder that is gone: %v", err)
	}
	if listed(t, s.Project.host, alias.Resource) {
		t.Fatal("the deleted session is still listed")
	}
	for _, path := range []string{file, journal} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remains after deletion: %v", path, err)
		}
	}
}

func TestBuiltinSessionKeepsItsWorktreeThroughListingAndDeletion(t *testing.T) {
	t.Setenv("WINGMAN_HOME", t.TempDir())
	protocolTestGateway(t)
	root := canonicalPath(t.TempDir())
	repo, checkout := filepath.Join(root, "main"), filepath.Join(root, "worktree")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "initial"},
		{"-C", repo, "worktree", "add", "-q", "--detach", checkout},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	h, endpoint := hostTestStart(t, root, "")
	client := protocolTestClient(t, endpoint, "worktree")
	resource := "ahp-session:/worktree"
	state := hostTestSession(t, client, resource, fileuri.FromPath(checkout))
	chat, sub := subscribeChat(t, client, *state.DefaultChat)
	send(t, client, chat.Resource, "turn", 1)
	until(t, chat, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	if _, err := h.projectAt(repo); err != nil {
		t.Fatal(err)
	}
	items := reviewHostCatalog(t, h)
	if len(items) != 1 || items[0].Resource != resource || items[0].Project.Uri != fileuri.FromPath(checkout) {
		t.Fatalf("shared journal listed under multiple worktrees: %+v", items)
	}
	h.mu.Lock()
	id := h.aliases[resource].Session
	h.mu.Unlock()
	journals := code.SessionsDir(repo)
	if _, err := session.Load(journals, id); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if err := os.Rename(checkout, checkout+"-moved"); err != nil {
		t.Fatal(err)
	}
	_, endpoint = hostTestStart(t, root, "")
	client = protocolTestClient(t, endpoint, "cleanup")
	if err := client.Request(t.Context(), "disposeSession", types.DisposeSessionParams{Channel: resource}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Load(journals, id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the removed worktree's journal remained: %v", err)
	}
}
