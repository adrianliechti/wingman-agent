package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// hostTestRest waits until no client follows the host and its project has no work, then closes it.
func hostTestRest(t *testing.T, host *Host) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); host.ahp.connected() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	for _, project := range host.ahp.loaded() {
		for deadline := time.Now().Add(5 * time.Second); project.server.busy() && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}
	host.ahp.closeIdle(0)
	if n := len(host.ahp.loaded()); n != 0 {
		t.Fatalf("%d projects stayed open", n)
	}
}

func TestHostReopensSessionsAndChatsThatNeverRan(t *testing.T) {
	testenv.WingmanHome(t)
	ahpTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, uri := hostTestProject(t, root, "project")
	host, endpoint := hostTestStart(t, root, "")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := ahpTestClient(t, endpoint, "editor")
	unused, used := "ahp-session:/unused", "ahp-session:/used"
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: unused, Provider: new("wingman"), WorkingDirectories: []string{uri}}, nil); err != nil {
		t.Fatal(err)
	}
	created := hostTestSession(t, client, used, uri)
	state, sub := ahpSubscribeChat(t, client, *created.DefaultChat)
	ahpSend(t, client, state.Resource, "kept-turn", 1)
	ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	side := "ahp-chat:/used-side"
	if err := client.Request(ctx, "createChat", types.CreateChatParams{Channel: used, Chat: side}, nil); err != nil {
		t.Fatal(err)
	}
	_ = client.Shutdown(ctx)
	hostTestRest(t, host)
	for _, channel := range []string{unused, ahpChatURI(unused), side} {
		if _, _, err := host.ahp.ensureSession(channel); err != nil {
			t.Errorf("%s does not reopen: %v", channel, err)
		}
	}
}

// heldCreateAgent holds native creation while armed, once it has started.
type heldCreateAgent struct {
	*protocolAgent
	armed            atomic.Bool
	entered, release chan struct{}
}

func (a *heldCreateAgent) NewSession(ctx context.Context) (string, error) {
	if a.armed.Load() {
		a.entered <- struct{}{}
		<-a.release
	}
	return a.protocolAgent.NewSession(ctx)
}

func TestAHPChatCreatedDuringDisposeGoesWithItsSession(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	agent := &heldCreateAgent{protocolAgent: &protocolAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	installProtocolAgent(s, agent)
	client := ahpTestClient(t, endpoint, "dispose-race")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, side := "ahp-session:/race", "ahp-chat:/race-side"
	ahpCreate(t, client, session)
	agent.armed.Store(true)
	created := make(chan error, 1)
	go func() {
		created <- client.Request(ctx, "createChat", types.CreateChatParams{Channel: session, Chat: side}, nil)
	}()
	<-agent.entered
	agent.armed.Store(false)
	if err := client.Request(ctx, "disposeSession", types.DisposeSessionParams{Channel: session}, nil); err != nil {
		t.Fatal(err)
	}
	close(agent.release)
	if err := <-created; err == nil {
		t.Fatal("a chat joined a disposed session")
	}
	h := s.ahpProject()
	h.mu.Lock()
	alias, leaked := h.chatAliases[side]
	h.mu.Unlock()
	if leaked {
		t.Fatalf("the disposed session left its chat's alias %+v", alias)
	}
	ahpCreate(t, client, session)
	h.mu.Lock()
	chats := len(h.sessions[session].chats)
	h.mu.Unlock()
	if chats != 1 {
		t.Fatalf("a new session at the URI has %d chats", chats)
	}
}

func TestAHPFailedCreationLeavesNoSession(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	installProtocolAgent(s, &lifecycleAgent{protocolAgent: &protocolAgent{}, setModel: func(_ context.Context, _, model string) error {
		if model == "broken" {
			return errors.New("model unavailable")
		}
		return nil
	}})
	client := ahpTestClient(t, endpoint, "creator")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	uri := "ahp-session:/half"
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: uri, Provider: new("one"), Config: map[string]json.RawMessage{"model": json.RawMessage(`"broken"`)}}, nil); err == nil {
		t.Fatal("creation did not fail")
	}
	var listed types.ListSessionsResult
	if err := client.Request(ctx, "listSessions", types.ListSessionsParams{Channel: types.RootResourceURI}, &listed); err != nil {
		t.Fatal(err)
	}
	for _, item := range listed.Items {
		if item.Resource == uri {
			t.Fatalf("a failed creation is listed: %+v", item)
		}
	}
	if _, sub, err := client.Subscribe(ctx, uri); err == nil {
		sub.Close()
		t.Fatal("a failed creation is subscribable")
	}
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: uri, Provider: new("one")}, nil); err != nil {
		t.Fatalf("a corrected creation was rejected: %v", err)
	}
}

type settingsProtocolAgent struct{ *protocolAgent }

func (settingsProtocolAgent) SetEffort(context.Context, string, string) error { return nil }
func (settingsProtocolAgent) SetMode(context.Context, string, string) error   { return nil }

func TestAHPDisposedURIsCanBeUsedAgain(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	installProtocolAgent(s, settingsProtocolAgent{&protocolAgent{}})
	client := ahpTestClient(t, endpoint, "recreate")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session := "ahp-session:/recreate"
	ahpCreate(t, client, session)
	side := types.CreateChatParams{Channel: session, Chat: "ahp-chat:/recreate-side"}
	for range 2 {
		if err := client.Request(ctx, "createChat", side, nil); err != nil {
			t.Fatalf("creating a disposed chat again: %v", err)
		}
		if err := client.Request(ctx, "disposeChat", types.DisposeChatParams{Channel: side.Chat}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Request(ctx, "disposeSession", types.DisposeSessionParams{Channel: session}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: session, Provider: new("one")}, nil); err != nil {
		t.Fatalf("creating a disposed session again: %v", err)
	}
	if dir, err := s.ahpProject().aliasDirectory(); err == nil {
		if _, err := os.Stat(dir); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostIdleCloseUpdatesRootSubscribers(t *testing.T) {
	testenv.WingmanHome(t)
	ahpTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, uri := hostTestProject(t, root, "project")
	host, endpoint := hostTestStart(t, root, "")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	watcher := ahpTestClient(t, endpoint, "")
	rootSub := watcher.AttachSubscription(types.RootResourceURI)
	hello, err := watcher.Initialize(ctx, "watcher", []string{"1.1.0"}, []string{types.RootResourceURI})
	if err != nil {
		t.Fatal(err)
	}
	state := *hello.Snapshots[0].State.Root
	models := func(root types.RootState) int {
		for _, agent := range root.Agents {
			if agent.Provider == "wingman" {
				return len(agent.Models)
			}
		}
		return -1
	}
	drain := func(until func() bool, wait time.Duration) {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		for until == nil || !until() {
			select {
			case event := <-rootSub.Events():
				if action, ok := event.(ahp.SubscriptionEventAction); ok {
					ahp.ApplyActionToRoot(&state, action.Envelope.Action)
				}
			case <-timer.C:
				return
			}
		}
	}
	if err := watcher.Request(ctx, "createSession", types.CreateSessionParams{Channel: "ahp-session:/resting", Provider: new("wingman"), WorkingDirectories: []string{uri}}, nil); err != nil {
		t.Fatal(err)
	}
	drain(func() bool { return models(state) > 0 }, 5*time.Second)
	_ = watcher.Request(ctx, "unsubscribe", map[string]string{"channel": "ahp-session:/resting"}, nil)
	for _, project := range host.ahp.loaded() {
		for deadline := time.Now().Add(5 * time.Second); project.server.busy() && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}
	host.ahp.closeIdle(0)
	if n := len(host.ahp.loaded()); n != 0 {
		t.Fatalf("%d projects stayed open", n)
	}
	drain(nil, 500*time.Millisecond)
	fresh := ahpTestClient(t, endpoint, "")
	again, err := fresh.Initialize(ctx, "fresh", []string{"1.1.0"}, []string{types.RootResourceURI})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := *again.Snapshots[0].State.Root; models(state) != models(snapshot) || len(state.Terminals) != len(snapshot.Terminals) {
		t.Fatalf("the root changed without an action: models %d/%d", models(state), models(snapshot))
	}
}

func TestHostIdleCloseKeepsSessionFlagsAndTimes(t *testing.T) {
	testenv.WingmanHome(t)
	ahpTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, uri := hostTestProject(t, root, "project")
	host, endpoint := hostTestStart(t, root, "")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := ahpTestClient(t, endpoint, "editor")
	const session = "ahp-session:/kept"
	created := hostTestSession(t, client, session, uri)
	time.Sleep(20 * time.Millisecond)
	state, sub := ahpSubscribeChat(t, client, *created.DefaultChat)
	ahpSend(t, client, state.Resource, "turn", 1)
	ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	archive := types.DispatchActionParams{Channel: session, ClientSeq: 2, Action: types.StateAction{Value: &types.SessionIsArchivedChangedAction{Type: types.ActionTypeSessionIsArchivedChanged, IsArchived: true}}}
	if err := client.Notify(ctx, "dispatchAction", archive); err != nil {
		t.Fatal(err)
	}
	listed := func() types.SessionSummary {
		for _, item := range reviewHostCatalog(t, host.ahp) {
			if item.Resource == session {
				return item
			}
		}
		t.Fatal("the session is not listed")
		return types.SessionSummary{}
	}
	for deadline := time.Now().Add(5 * time.Second); !listed().Status.Has(types.SessionStatusIsArchived); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the archive did not take")
		}
	}
	open := listed()
	_ = client.Shutdown(ctx)
	hostTestRest(t, host)
	closed := listed()
	if _, _, err := host.ahp.ensureSession(session); err != nil {
		t.Fatal(err)
	}
	reopened := listed()
	for name, item := range map[string]types.SessionSummary{"closed": closed, "reopened": reopened} {
		if !item.Status.Has(types.SessionStatusIsArchived) || item.CreatedAt != open.CreatedAt || item.ModifiedAt != open.ModifiedAt {
			t.Errorf("%s: status %d created %s modified %s, open: created %s modified %s", name, item.Status, item.CreatedAt, item.ModifiedAt, open.CreatedAt, open.ModifiedAt)
		}
	}
}

func TestServerListsSavedSessionsOfAgentsItHasNotStarted(t *testing.T) {
	testenv.WingmanHome(t)
	ahpTestGateway(t)
	dir := canonicalPath(t.TempDir())
	first, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ahpProject().storeAlias(ahpAlias{Resource: "ahp-session:/external", Backend: "external", Session: "native", Created: time.Now(), Title: "External session"}); err != nil {
		t.Fatal(err)
	}
	first.Close()
	restarted, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if listed := reviewHostCatalog(t, restarted.ahpProject().ahpHost); len(listed) != 1 {
		t.Fatalf("the dormant agent's session is not listed: %+v", listed)
	}
}

func TestHostListsSessionsWhenOneProjectsAgentFails(t *testing.T) {
	testenv.WingmanHome(t)
	h := newAHPHost()
	h.open = func(string) (*Server, error) { panic("unexpected open") }
	project := func(dir string, list func(context.Context) ([]code.SessionInfo, error)) {
		s := &Server{ctx: t.Context(), scope: workspaceScope(dir), workspace: &code.Workspace{RootPath: dir}, backends: map[string]*backendEntry{}}
		p := h.attach(s)
		s.ahp = p
		s.backends["one"] = &backendEntry{runtime: &backendRuntime{Server: s, id: "one", agent: &lifecycleAgent{protocolAgent: &protocolAgent{}, list: list}}}
		h.projects[p.dir] = p
	}
	project(canonicalPath(t.TempDir()), func(context.Context) ([]code.SessionInfo, error) { return nil, errors.New("agent process exited") })
	project(canonicalPath(t.TempDir()), func(context.Context) ([]code.SessionInfo, error) {
		return []code.SessionInfo{{ID: "healthy", Title: "Healthy session", UpdatedAt: time.Now()}}, nil
	})
	if listed := reviewHostCatalog(t, h); len(listed) != 1 {
		t.Fatalf("listed %+v", listed)
	}
}

func TestSavedPausedQueueLetsItsProjectClose(t *testing.T) {
	s, b, agent := newProtocolServer(t)
	agent.queue = code.TurnQueueState{Inputs: []code.TurnInput{{ID: "saved-input", Intent: code.TurnInputFollowUp, Content: []harness.Content{{Text: "later"}}}}}
	b.session("saved").load()
	if !b.turns.Snapshot("saved").Paused {
		t.Fatal("the recovered queue is not paused")
	}
	if s.busy() {
		t.Fatal("a saved queue keeps its project open")
	}
}
