package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func statusListed(t *testing.T, h *Host, session string) types.SessionSummary {
	t.Helper()
	for _, item := range reviewHostCatalog(t, h) {
		if item.Resource == session {
			return item
		}
	}
	t.Fatalf("%s is not listed", session)
	return types.SessionSummary{}
}

func TestSessionReadFollowsTurnsAndClients(t *testing.T) {
	s, _, agent, endpoint := protocolTestServer(t)
	agent.gate = make(chan struct{})
	h := s.Project.host
	client := protocolTestClient(t, endpoint, "reader")
	const session = "ahp-session:/read"
	chat := create(t, client, session)
	read := func() bool { return statusListed(t, h, session).Status.Has(types.SessionStatusIsRead) }
	if !read() {
		t.Fatal("a new session lists as unread")
	}
	state, sub := subscribeChat(t, client, chat)
	send(t, client, chat, "turn", 1)
	waitProtocol(t, func() bool { return !read() })
	// The client that shows the chat reads the running turn.
	reviewSessionsDispatch(t, client, chat, 2, &types.ChatIsReadChangedAction{Type: types.ActionTypeChatIsReadChanged, IsRead: true})
	waitProtocol(t, read)
	close(agent.gate)
	until(t, state, sub, func(e types.ActionEnvelope) bool { _, ok := e.Action.Value.(*types.ChatTurnCompleteAction); return ok })
	// The ending is new again; reducers do not clear the flag for it, so the host announces it.
	until(t, state, sub, func(e types.ActionEnvelope) bool {
		action, ok := e.Action.Value.(*types.ChatIsReadChangedAction)
		return ok && !action.IsRead
	})
	if read() {
		t.Fatal("a session whose turn ended unseen lists as read")
	}
	h.mu.Lock()
	unread := h.aliases[session].Unread
	h.mu.Unlock()
	if !unread {
		t.Fatal("the unread session is not saved as unread")
	}

	reviewSessionsDispatch(t, client, chat, 3, &types.ChatIsReadChangedAction{Type: types.ActionTypeChatIsReadChanged, IsRead: true})
	waitProtocol(t, read)

	reviewSessionsDispatch(t, client, session, 4, &types.SessionIsReadChangedAction{Type: types.ActionTypeSessionIsReadChanged, IsRead: false})
	waitProtocol(t, func() bool {
		summary := statusListed(t, h, session)
		return !summary.Status.Has(types.SessionStatusIsRead) && len(summary.Chats) == 1 && summary.Chats[0].Status != nil && !summary.Chats[0].Status.Has(types.SessionStatusIsRead)
	})
	reviewSessionsDispatch(t, client, session, 5, &types.SessionIsReadChangedAction{Type: types.ActionTypeSessionIsReadChanged, IsRead: true})
	waitProtocol(t, func() bool {
		summary := statusListed(t, h, session)
		return summary.Status.Has(types.SessionStatusIsRead) && summary.Chats[0].Status.Has(types.SessionStatusIsRead)
	})
}

func TestSavedSessionsListReadUnlessLeftUnread(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	dir := canonicalPath(t.TempDir())
	first, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	changes := &types.ChangesSummary{Additions: new(int64(4)), Deletions: new(int64(1)), Files: new(int64(2))}
	// Sessions saved before the host tracked reading carry no flags at all.
	for _, alias := range []sessionAlias{
		{Resource: "ahp-session:/old", Backend: "external", Session: "old", Created: time.Now()},
		{Resource: "ahp-session:/archived", Backend: "external", Session: "archived", Created: time.Now(), Status: types.SessionStatusIsArchived},
		{Resource: "ahp-session:/new", Backend: "external", Session: "new", Created: time.Now(), Unread: true, Changes: changes},
	} {
		if err := first.Project.storeAlias(alias); err != nil {
			t.Fatal(err)
		}
	}
	first.Close()
	restarted, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	h := restarted.Project.host
	if old := statusListed(t, h, "ahp-session:/old"); !old.Status.Has(types.SessionStatusIsRead) || old.Status.Has(types.SessionStatusIsArchived) {
		t.Errorf("an old session lists with status %d", old.Status)
	}
	if archived := statusListed(t, h, "ahp-session:/archived"); !archived.Status.Has(types.SessionStatusIsRead | types.SessionStatusIsArchived) {
		t.Errorf("an archived session lists with status %d", archived.Status)
	}
	unseen := statusListed(t, h, "ahp-session:/new")
	if unseen.Status.Has(types.SessionStatusIsRead) {
		t.Errorf("a session saved unread lists as read")
	}
	if unseen.Changes == nil || *unseen.Changes.Additions != 4 || *unseen.Changes.Deletions != 1 || *unseen.Changes.Files != 2 {
		t.Errorf("a closed project's session lists changes %+v", unseen.Changes)
	}
}

func TestLoadingAnUnreadSessionKeepsItUnread(t *testing.T) {
	// The saved flag would otherwise reach later servers of the same project.
	t.Setenv("WINGMAN_HOME", t.TempDir())
	s, b, _ := newProtocolServer(t)
	resource := s.Project.canonicalURI("one", "saved")
	s.Project.host.mu.Lock()
	s.Project.host.rememberAliasLocked(sessionAlias{Resource: resource, Project: s.Project.dir, Backend: "one", Session: "saved", Unread: true})
	s.Project.host.mu.Unlock()
	c := b.session("saved")
	c.load()
	ch, err := s.Project.attachSession(c, resource, "Test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h := s.Project.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch.session.state.Status.Has(types.SessionStatusIsRead) || ch.chat.Status.Has(types.SessionStatusIsRead) {
		t.Fatalf("an unread session loaded as read: session %d chat %d", ch.session.state.Status, ch.chat.Status)
	}
}

func TestArchivingNativeSessionSurvivesRestart(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	resource := s.Project.canonicalURI("one", "native")
	client := protocolTestClient(t, endpoint, "archive")
	reviewSessionsDispatch(t, client, resource, 1, &types.SessionIsArchivedChangedAction{Type: types.ActionTypeSessionIsArchivedChanged, IsArchived: true})
	waitProtocol(t, func() bool {
		h := s.Project.host
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.aliases[resource].Status.Has(types.SessionStatusIsArchived)
	})
	s.Close()
	restarted, _, _ := newProtocolServer(t)
	if !statusListed(t, restarted.Project.host, resource).Status.Has(types.SessionStatusIsArchived) {
		t.Fatal("a native session lost its archive flag on restart")
	}
}

// Archiving or marking a session no window has open changes what the host
// saved of it. Loading it would open its project and resume its agent's
// thread for every session a user tidies away, and its folder may be gone.
func TestMarkingASessionNoWindowOpenedLoadsNothing(t *testing.T) {
	s, _, agent, endpoint := protocolTestServer(t)
	h := s.Project.host
	gone := filepath.Join(t.TempDir(), "gone")
	alias := sessionAlias{Resource: "ahp-session:/elsewhere", Project: gone, Backend: "one", Session: "native", Title: "Elsewhere", Created: time.Now()}
	file := saveTestAlias(t, s, alias)
	client := protocolTestClient(t, endpoint, "tidy")
	saved := func() sessionAlias {
		var stored sessionAlias
		data, err := os.ReadFile(file)
		if err != nil || json.Unmarshal(data, &stored) != nil {
			t.Fatalf("the saved session: %v", err)
		}
		return stored
	}

	reviewSessionsDispatch(t, client, alias.Resource, 1, &types.SessionIsArchivedChangedAction{Type: types.ActionTypeSessionIsArchivedChanged, IsArchived: true})
	waitProtocol(t, func() bool { return statusListed(t, h, alias.Resource).Status.Has(types.SessionStatusIsArchived) })
	if !saved().Status.Has(types.SessionStatusIsArchived) {
		t.Fatal("the archive flag was not saved")
	}
	reviewSessionsDispatch(t, client, alias.Resource, 2, &types.SessionIsReadChangedAction{Type: types.ActionTypeSessionIsReadChanged, IsRead: false})
	waitProtocol(t, func() bool { return !statusListed(t, h, alias.Resource).Status.Has(types.SessionStatusIsRead) })
	if stored := saved(); !stored.Unread || !stored.Status.Has(types.SessionStatusIsArchived) {
		t.Fatalf("the saved flags: %+v", stored)
	}
	reviewSessionsDispatch(t, client, alias.Resource, 3, &types.SessionIsReadChangedAction{Type: types.ActionTypeSessionIsReadChanged, IsRead: true})
	waitProtocol(t, func() bool { return statusListed(t, h, alias.Resource).Status.Has(types.SessionStatusIsRead) })

	if agent.loaded.Load() != 0 {
		t.Fatal("marking loaded the session")
	}
	h.mu.Lock()
	_, open := h.sessions[alias.Resource]
	_, project := h.projects[gone]
	h.mu.Unlock()
	if open || project {
		t.Fatalf("marking opened the session (%v) or its project (%v)", open, project)
	}
}

// A session made in an agent's own CLI, which the host never saved, is kept
// by the host once it is archived, under the title its agent lists.
func TestArchivingAListedSessionOfAnotherFolderLoadsNothing(t *testing.T) {
	s, _, _, endpoint := protocolTestServer(t)
	h := s.Project.host
	root := canonicalPath(t.TempDir())
	h.mu.Lock()
	h.roots = []string{root, s.Project.dir}
	h.mu.Unlock()
	dir := filepath.Join(root, "other")
	updated := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	fleet := &deletingFleetAgent{&deletingAgent{protocolAgent: &protocolAgent{}}, []code.SessionInfo{{ID: "cli", Title: "Made in the CLI", Dir: dir, UpdatedAt: updated}}}
	b := s.Project.bindBackend("two", fleet)
	s.Project.backends[b.id] = &backendEntry{runtime: b}
	resource := canonicalSessionURI("two", "cli", dir)
	client := protocolTestClient(t, endpoint, "tidy")

	reviewSessionsDispatch(t, client, resource, 1, &types.SessionIsArchivedChangedAction{Type: types.ActionTypeSessionIsArchivedChanged, IsArchived: true})
	waitProtocol(t, func() bool { return statusListed(t, h, resource).Status.Has(types.SessionStatusIsArchived) })
	item := statusListed(t, h, resource)
	if item.Title != "Made in the CLI" || !item.Status.Has(types.SessionStatusIsRead) || item.ModifiedAt != updated.Format(time.RFC3339Nano) {
		t.Fatalf("the archived session: %+v", item)
	}
	if fleet.loaded.Load() != 0 {
		t.Fatal("archiving loaded the session")
	}
	h.mu.Lock()
	_, project := h.projects[dir]
	h.mu.Unlock()
	if project {
		t.Fatal("archiving opened the session's project")
	}
}

func TestSessionActivityFollowsTheRunningTurn(t *testing.T) {
	s, _, _, c, ch := reviewSessionsAttach(t)
	h := s.Project.host
	activity := func() string {
		h.mu.Lock()
		defer h.mu.Unlock()
		if value := ch.session.summary().Activity; value != nil {
			return *value
		}
		return ""
	}
	read := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return ch.session.state.Status.Has(types.SessionStatusIsRead)
	}
	if !read() {
		t.Fatal("a loaded session that was never saved lists as unread")
	}
	reviewSessionsInput(c, "t1", "run the tests", "user", code.TurnInputActive)
	c.apply(Frame{Type: EvtToolCall, ID: "run", Name: "shell", Kind: "execute", Args: `{"command":"go test ./..."}`, Hint: "go test ./..."})
	waitProtocol(t, func() bool { return strings.HasSuffix(activity(), ": go test ./...") })
	c.apply(Frame{Type: EvtToolResult, ID: "run", Name: "shell", Kind: "execute", Args: `{"command":"go test ./..."}`, Hint: "go test ./...", Content: "ok"})
	reviewSessionsInput(c, "t1", "run the tests", "user", code.TurnInputCompleted)
	waitProtocol(t, func() bool { return activity() == "" })
	if read() {
		t.Fatal("a turn that ended unseen left the session read")
	}
}

func TestSessionActivityNamesWhatItWaitsFor(t *testing.T) {
	s, b, _, c, ch := reviewSessionsAttach(t)
	h := s.Project.host
	activity := func() string {
		h.mu.Lock()
		defer h.mu.Unlock()
		if value := ch.session.summary().Activity; value != nil {
			return *value
		}
		return ""
	}
	id, reply := reviewSessionsPrompt(t, b, c, PromptKindConfirm)
	waitProtocol(t, func() bool { return activity() == "Proceed?" })
	if err := c.resolvePrompt(Command{PromptID: id, Action: "accept"}); err != nil {
		t.Fatal(err)
	}
	<-reply
	waitProtocol(t, func() bool { return activity() == "" })
}

func TestCurrentActivityPrefersTheWaitThenTheChatThenItsTool(t *testing.T) {
	a := &Session{}
	ch := &Chat{session: a}
	a.chats = []*Chat{ch}
	if got := a.currentActivity(); got != "" {
		t.Fatalf("an idle session does %q", got)
	}
	running := &types.ToolCallRunningState{ToolCallId: "1", DisplayName: "Edit file", Content: []types.ToolResultContent{{Value: &types.ToolResultResourceContent{Type: types.ToolResultContentTypeResource, Uri: fileuri.FromPath("/work/src/App.tsx") + "#L3"}}}}
	ch.chat.ActiveTurn = &types.ActiveTurn{Id: "t", ResponseParts: []types.ResponsePart{{Value: &types.ToolCallResponsePart{Kind: types.ResponsePartKindToolCall, ToolCall: types.ToolCallState{Value: running}}}}}
	if got := a.currentActivity(); got != "Edit file: App.tsx" {
		t.Errorf("a tool without an intention names its file: %q", got)
	}
	running.Intention = new("go test ./...\nsecond line")
	if got := a.currentActivity(); got != "Edit file: go test ./..." {
		t.Errorf("a tool names its intention's first line: %q", got)
	}
	ch.chat.Activity = new("Compacting context…")
	if got := a.currentActivity(); got != "Compacting context…" {
		t.Errorf("the chat's own activity wins over its tool: %q", got)
	}
	a.state.InputNeeded = []types.SessionInputRequest{{Value: &types.SessionToolConfirmationRequest{Id: "approval:1", Chat: "ahp-chat:/c", Kind: types.SessionInputRequestKindToolConfirmation, TurnId: "t", ToolCall: types.ToolCallConfirmationState{Value: &types.ToolCallPendingConfirmationState{ToolCallId: "approval:1", DisplayName: "Approval", InvocationMessage: types.NewStringOrMarkdownPlain("\nRun `git push`?\n\nRisk: high")}}}}}
	if got := a.currentActivity(); got != "Run `git push`?" {
		t.Errorf("an approval names its first line: %q", got)
	}
	a.state.InputNeeded = []types.SessionInputRequest{{Value: &types.SessionChatInputRequest{Id: "q", Chat: "ahp-chat:/c", Kind: types.SessionInputRequestKindChatInput, Request: types.ChatInputRequest{Id: "q"}}}}
	if got := a.currentActivity(); got != "Waiting for an answer" {
		t.Errorf("a question without a message: %q", got)
	}
	long := strings.Repeat("é", activityLimit+5)
	if got := clip(long, ""); len([]rune(got)) != activityLimit || !strings.HasSuffix(got, "…") {
		t.Errorf("long activity is not clipped: %d runes", len([]rune(got)))
	}
}

func TestChangesSumNetEditsWithoutUndoneTurns(t *testing.T) {
	changes := changesOf([]harness.TurnReview{
		{ID: "one", Files: []tool.FileChange{
			{Path: "a.go", Before: "a\nb\n", After: "a\nB\nc\n", BeforeExists: true, AfterExists: true},
			{Path: "new.go", After: "x\ny\n", AfterExists: true},
			{Path: "reverted.go", Before: "keep\n", After: "changed\n", BeforeExists: true, AfterExists: true},
		}},
		{ID: "two", Files: []tool.FileChange{
			{Path: "reverted.go", Before: "changed\n", After: "keep\n", BeforeExists: true, AfterExists: true},
		}},
		{ID: "three", Undone: true, Files: []tool.FileChange{{Path: "undone.go", After: "gone\n", AfterExists: true}}},
	})
	if *changes.Additions != 4 || *changes.Deletions != 1 || *changes.Files != 2 {
		t.Fatalf("changes +%d -%d in %d files", *changes.Additions, *changes.Deletions, *changes.Files)
	}
}

// reviewedAgent records file edits like the builtin agent's turn reviews.
type reviewedAgent struct {
	*protocolAgent
	reviewMu sync.Mutex
	reviews  []harness.TurnReview
	counted  chan struct{}
}

func (a *reviewedAgent) TurnReviews(context.Context, string) ([]harness.TurnReview, error) {
	a.reviewMu.Lock()
	defer a.reviewMu.Unlock()
	if a.counted != nil {
		<-a.counted
	}
	return slices.Clone(a.reviews), nil
}

func (a *reviewedAgent) UndoTurn(context.Context, string, string) error { return nil }

func TestSessionCountsWhatItsTurnsChanged(t *testing.T) {
	s, _, _ := newProtocolServer(t)
	agent := &reviewedAgent{protocolAgent: &protocolAgent{}, reviews: []harness.TurnReview{
		{ID: "one", Files: []tool.FileChange{{Path: "a.go", Before: "a\nb\n", After: "a\nB\nc\n", BeforeExists: true, AfterExists: true}}},
	}}
	b := s.Project.bindBackend("reviewed", agent)
	s.Project.backends[b.id] = &backendEntry{runtime: b}
	c := b.session("saved")
	c.load()
	ch, err := s.Project.attachSession(c, s.Project.canonicalURI("reviewed", "saved"), "Test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h := s.Project.host
	counted := func(additions, deletions, files int64) func() bool {
		return func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			summary := ch.session.summary()
			got := summary.Changes
			return got != nil && *got.Additions == additions && *got.Deletions == deletions && *got.Files == files && summary.Chats[0].Changes == got
		}
	}
	waitProtocol(t, counted(2, 1, 1))

	agent.reviewMu.Lock()
	agent.reviews[0].Undone = true
	agent.reviewMu.Unlock()
	c.refreshChanges()
	waitProtocol(t, counted(0, 0, 0))

	// A count requested while one runs runs once more, with the later edits.
	agent.reviewMu.Lock()
	agent.counted = make(chan struct{})
	agent.reviews = append(agent.reviews, harness.TurnReview{ID: "two", Files: []tool.FileChange{{Path: "b.go", After: "1\n2\n3\n", AfterExists: true}}})
	agent.reviewMu.Unlock()
	c.refreshChanges()
	waitProtocol(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return ch.changesRunning })
	c.refreshChanges()
	close(agent.counted)
	waitProtocol(t, counted(3, 0, 1))
	waitProtocol(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return !ch.changesRunning })
}
