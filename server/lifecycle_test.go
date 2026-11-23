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
	"github.com/adrianliechti/wingman-agent/server/ahp"
	sdk "github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// hostTestRest waits until no client follows the host and its project has no work, then closes it.
func hostTestRest(t *testing.T, host *Host) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); host.connected() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	for _, project := range host.loaded() {
		for deadline := time.Now().Add(5 * time.Second); project.busy() && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}
	host.closeIdle(0)
	if n := len(host.loaded()); n != 0 {
		t.Fatalf("%d projects stayed open", n)
	}
}

func TestHostReopensSessionsAndChatsThatNeverRan(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, uri := hostTestProject(t, root, "project")
	host, endpoint := hostTestStart(t, root, "")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := protocolTestClient(t, endpoint, "editor")
	unused, used := "ahp-session:/unused", "ahp-session:/used"
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: unused, Provider: new("wingman"), WorkingDirectories: []string{uri}}, nil); err != nil {
		t.Fatal(err)
	}
	created := hostTestSession(t, client, used, uri)
	state, sub := subscribeChat(t, client, *created.DefaultChat)
	send(t, client, state.Resource, "kept-turn", 1)
	until(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	side := "ahp-chat:/used-side"
	if err := client.Request(ctx, "createChat", types.CreateChatParams{Channel: used, Chat: side}, nil); err != nil {
		t.Fatal(err)
	}
	_ = client.Shutdown(ctx)
	hostTestRest(t, host)
	for _, channel := range []string{unused, ahp.ChatURI(unused), side} {
		if _, _, err := host.ensureSession(channel); err != nil {
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
	s, _, _, endpoint := protocolTestServer(t)
	agent := &heldCreateAgent{protocolAgent: &protocolAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	installProtocolAgent(s, agent)
	client := protocolTestClient(t, endpoint, "dispose-race")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, side := "ahp-session:/race", "ahp-chat:/race-side"
	create(t, client, session)
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
	h := s.Project
	h.host.mu.Lock()
	alias, leaked := h.host.chatAliases[side]
	h.host.mu.Unlock()
	if leaked {
		t.Fatalf("the disposed session left its chat's alias %+v", alias)
	}
	create(t, client, session)
	h.host.mu.Lock()
	chats := len(h.host.sessions[session].chats)
	h.host.mu.Unlock()
	if chats != 1 {
		t.Fatalf("a new session at the URI has %d chats", chats)
	}
}

func TestAHPFailedCreationLeavesNoSession(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	installProtocolAgent(s, &lifecycleAgent{protocolAgent: &protocolAgent{}, setModel: func(_ context.Context, _, model string) error {
		if model == "broken" {
			return errors.New("model unavailable")
		}
		return nil
	}})
	client := protocolTestClient(t, endpoint, "creator")
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

// A chat whose first message fails, before or after its native session exists, is not created.
func TestAHPChatWhoseFirstMessageFailsIsNotCreated(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	client := protocolTestClient(t, endpoint, "first-message")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, chat := "ahp-session:/first-message", "ahp-chat:/first-message-side"
	create(t, client, session)
	user := types.MessageOrigin{Kind: types.MessageKindUser}
	archive := types.MessageAttachment{Value: &types.MessageEmbeddedResourceAttachment{Type: "embeddedResource", ContentType: "application/zip", Label: "archive", Data: "AA=="}}
	for name, message := range map[string]types.Message{
		"unsupported attachment": {Text: "unpack this", Origin: user, Attachments: []types.MessageAttachment{archive}},
		"empty message":          {Origin: user},
	} {
		if err := client.Request(ctx, "createChat", types.CreateChatParams{Channel: session, Chat: chat, InitialMessage: &message}, nil); err == nil {
			t.Fatalf("%s: the chat was created", name)
		}
		h := s.Project.host
		h.mu.Lock()
		_, saved := h.chatAliases[chat]
		live := h.chats[chat] != nil
		h.mu.Unlock()
		if saved || live {
			t.Fatalf("%s: the failed creation left its chat (saved %v, live %v)", name, saved, live)
		}
	}
	first := types.Message{Text: "hello", Origin: user}
	if err := client.Request(ctx, "createChat", types.CreateChatParams{Channel: session, Chat: chat, InitialMessage: &first}, nil); err != nil {
		t.Fatalf("a corrected chat was rejected: %v", err)
	}
}

type settingsProtocolAgent struct{ *protocolAgent }

func (settingsProtocolAgent) SetEffort(context.Context, string, string) error { return nil }
func (settingsProtocolAgent) SetMode(context.Context, string, string) error   { return nil }

func TestAHPDisposedURIsCanBeUsedAgain(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	installProtocolAgent(s, settingsProtocolAgent{&protocolAgent{}})
	client := protocolTestClient(t, endpoint, "recreate")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session := "ahp-session:/recreate"
	create(t, client, session)
	oldSession, oldChat, err := s.Project.host.ensureSession(ahp.ChatURI(session))
	if err != nil {
		t.Fatal(err)
	}
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
	replacement, _, err := s.Project.host.ensureSession(session)
	if err != nil {
		t.Fatal(err)
	}
	actor := &connection{host: s.Project.host, id: "recreate"}
	if err := actor.executeAction(oldSession, nil, types.DispatchActionParams{Channel: session, Action: types.StateAction{Value: &types.SessionTitleChangedAction{Type: types.ActionTypeSessionTitleChanged, Title: "stale"}}}); err == nil {
		t.Fatal("a stale session accepted an action after its URI was reused")
	}
	oldChat.controller.mu.Lock()
	oldChat.syncLocked(oldChat.controller, nil)
	oldChat.controller.mu.Unlock()
	s.Project.host.mu.Lock()
	registered := s.Project.host.sessions[session] == replacement && s.Project.host.chats[oldChat.chat.Resource] == replacement.chats[0]
	s.Project.host.mu.Unlock()
	if !registered {
		t.Fatal("a late deletion removed the replacement session or chat")
	}
	if dir, err := s.Project.aliasDirectory(); err == nil {
		if _, err := os.Stat(dir); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostIdleCloseUpdatesRootSubscribers(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, uri := hostTestProject(t, root, "project")
	host, endpoint := hostTestStart(t, root, "")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	watcher := protocolTestClient(t, endpoint, "")
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
				if action, ok := event.(sdk.SubscriptionEventAction); ok {
					sdk.ApplyActionToRoot(&state, action.Envelope.Action)
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
	for _, project := range host.loaded() {
		for deadline := time.Now().Add(5 * time.Second); project.busy() && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}
	host.closeIdle(0)
	if n := len(host.loaded()); n != 0 {
		t.Fatalf("%d projects stayed open", n)
	}
	drain(nil, 500*time.Millisecond)
	fresh := protocolTestClient(t, endpoint, "")
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
	protocolTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, uri := hostTestProject(t, root, "project")
	host, endpoint := hostTestStart(t, root, "")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := protocolTestClient(t, endpoint, "editor")
	const session = "ahp-session:/kept"
	created := hostTestSession(t, client, session, uri)
	time.Sleep(20 * time.Millisecond)
	state, sub := subscribeChat(t, client, *created.DefaultChat)
	send(t, client, state.Resource, "turn", 1)
	until(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	archive := types.DispatchActionParams{Channel: session, ClientSeq: 2, Action: types.StateAction{Value: &types.SessionIsArchivedChangedAction{Type: types.ActionTypeSessionIsArchivedChanged, IsArchived: true}}}
	if err := client.Notify(ctx, "dispatchAction", archive); err != nil {
		t.Fatal(err)
	}
	listed := func() types.SessionSummary {
		for _, item := range reviewHostCatalog(t, host) {
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
	if _, _, err := host.ensureSession(session); err != nil {
		t.Fatal(err)
	}
	reopened := listed()
	// JSON timestamps may omit trailing fractional zeros after persistence.
	sameTime := func(a, b string) bool {
		first, firstErr := time.Parse(time.RFC3339Nano, a)
		second, secondErr := time.Parse(time.RFC3339Nano, b)
		return firstErr == nil && secondErr == nil && first.Equal(second)
	}
	for name, item := range map[string]types.SessionSummary{"closed": closed, "reopened": reopened} {
		if !item.Status.Has(types.SessionStatusIsArchived) || !sameTime(item.CreatedAt, open.CreatedAt) || !sameTime(item.ModifiedAt, open.ModifiedAt) {
			t.Errorf("%s: status %d created %s modified %s, open: created %s modified %s", name, item.Status, item.CreatedAt, item.ModifiedAt, open.CreatedAt, open.ModifiedAt)
		}
	}
}

func TestServerListsSavedSessionsOfAgentsItHasNotStarted(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	dir := canonicalPath(t.TempDir())
	first, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Project.storeAlias(sessionAlias{Resource: "ahp-session:/external", Backend: "external", Session: "native", Created: time.Now(), Title: "External session"}); err != nil {
		t.Fatal(err)
	}
	first.Close()
	restarted, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if listed := reviewHostCatalog(t, restarted.Project.host); len(listed) != 1 {
		t.Fatalf("the dormant agent's session is not listed: %+v", listed)
	}
}

func TestHostListsSessionsWhenOneProjectsAgentFails(t *testing.T) {
	testenv.WingmanHome(t)
	h := newHost(context.Background())
	h.open = func(string) (*Project, error) { panic("unexpected open") }
	project := func(dir string, list func(context.Context) ([]code.SessionInfo, error)) {
		s := &Project{ctx: t.Context(), scope: workspaceScope(dir), workspace: &code.Workspace{RootPath: dir}, backends: map[string]*backendEntry{}}
		h.attach(s)
		s.backends["one"] = &backendEntry{runtime: &backendRuntime{Project: s, id: "one", agent: &lifecycleAgent{protocolAgent: &protocolAgent{}, list: list}}}
		h.projects[s.dir] = s
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
	if s.Project.busy() {
		t.Fatal("a saved queue keeps its project open")
	}
}
