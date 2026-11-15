package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/microsoft/agent-host-protocol/clients/go/ahpws"
)

// hostTestStart serves a host for the projects below root.
func hostTestStart(t *testing.T, root, token string) (*Host, string) {
	t.Helper()
	host, err := NewHost(t.Context(), &HostOptions{Roots: []string{root}, Token: token, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(host)
	t.Cleanup(func() { server.Close(); host.Close() })
	return host, "ws" + strings.TrimPrefix(server.URL, "http") + "/ahp"
}

func hostTestProject(t *testing.T, root, name string) (dir, uri string) {
	t.Helper()
	dir = filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(name+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir, fileuri.FromPath(dir)
}

func hostTestCode(t *testing.T, err error, want int32) {
	t.Helper()
	var rpc *ahp.RPCError
	if !errors.As(err, &rpc) || rpc.Code != want {
		t.Fatalf("want error %d, got %v", want, err)
	}
}

func hostTestSession(t *testing.T, client *ahp.Client, uri, directory string) *types.SessionState {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: uri, Provider: new("wingman"), WorkingDirectories: []string{directory}}, nil); err != nil {
		t.Fatal(err)
	}
	result, sub, err := client.Subscribe(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	if result.Snapshot == nil || result.Snapshot.State.Session == nil {
		t.Fatalf("bad session: %+v", result)
	}
	return result.Snapshot.State.Session
}

func TestHostServesSeveralProjects(t *testing.T) {
	testenv.WingmanHome(t)
	ahpTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, endpoint := hostTestStart(t, root, "")
	dirA, uriA := hostTestProject(t, root, "a")
	dirB, uriB := hostTestProject(t, root, "b")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	client := ahpTestClient(t, endpoint, "")
	hello, err := client.Initialize(ctx, "editor", []string{"1.1.0"}, []string{types.RootResourceURI})
	if err != nil {
		t.Fatal(err)
	}
	// A client starts choosing a directory where the host may serve one.
	if hello.DefaultDirectory == nil || *hello.DefaultDirectory != fileuri.FromPath(root) {
		t.Fatalf("default directory: %v", hello.DefaultDirectory)
	}
	if _, pinned := hello.Snapshots[0].State.Root.Meta["wingman.workspace"]; pinned {
		t.Fatal("a host for several projects announced one workspace")
	}

	// Before any session, directories can be browsed and read, never changed.
	var listing types.ResourceListResult
	if err := client.Request(ctx, "resourceList", types.ResourceListParams{Channel: types.RootResourceURI, Uri: fileuri.FromPath(root)}, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 2 || listing.Entries[0].Name != "a" || listing.Entries[1].Name != "b" {
		t.Fatalf("browse: %+v", listing.Entries)
	}
	readme := fileuri.FromPath(filepath.Join(dirA, "README.md"))
	var content types.ResourceReadResult
	if err := client.Request(ctx, "resourceRead", types.ResourceReadParams{Channel: types.RootResourceURI, Uri: readme}, &content); err != nil || content.Data != "a\n" {
		t.Fatalf("read before a session: %v %+v", err, content)
	}
	err = client.Request(ctx, "resourceWrite", types.ResourceWriteParams{Channel: types.RootResourceURI, Uri: readme, Data: "changed", Encoding: types.ContentEncodingUtf8}, nil)
	hostTestCode(t, err, types.ErrorCodeInvalidParams)
	err = client.Request(ctx, "resourceList", types.ResourceListParams{Channel: types.RootResourceURI, Uri: fileuri.FromPath(filepath.Dir(root))}, &listing)
	hostTestCode(t, err, types.ErrorCodePermissionDenied)

	// A session's directory opens its project; other directories stay closed.
	err = client.Request(ctx, "createSession", types.CreateSessionParams{Channel: "ahp-session:/outside", Provider: new("wingman"), WorkingDirectories: []string{fileuri.FromPath(filepath.Dir(root))}}, nil)
	hostTestCode(t, err, types.ErrorCodePermissionDenied)
	// The protocol makes the directory optional; the host's default one is used.
	if err := client.Request(ctx, "createSession", types.CreateSessionParams{Channel: "ahp-session:/default", Provider: new("wingman")}, nil); err != nil {
		t.Fatal(err)
	}

	var config types.ResolveSessionConfigResult
	if err := client.Request(ctx, "resolveSessionConfig", types.ResolveSessionConfigParams{Channel: types.RootResourceURI, Provider: new("wingman"), WorkingDirectory: &uriA}, &config); err != nil {
		t.Fatal(err)
	}
	if string(config.Values["model"]) != `"gpt-6-sol"` {
		t.Fatalf("model configuration missing: %+v", config)
	}
	a := hostTestSession(t, client, "ahp-session:/in-a", uriA)
	b := hostTestSession(t, client, "ahp-session:/in-b", uriB)
	if !slices.Equal(a.WorkingDirectories, []string{uriA}) || !slices.Equal(b.WorkingDirectories, []string{uriB}) {
		t.Fatalf("working directories: %v %v", a.WorkingDirectories, b.WorkingDirectories)
	}
	if a.Project == nil || a.Project.Uri != uriA || a.Project.DisplayName != "a" || b.Project == nil || b.Project.Uri != uriB {
		t.Fatalf("projects: %+v %+v", a.Project, b.Project)
	}
	// A directory reached through a link is the project it leads to.
	link := filepath.Join(root, "link")
	if err := os.Symlink(dirB, link); err != nil {
		t.Skip(err)
	}
	linked := hostTestSession(t, client, "ahp-session:/through-link", fileuri.FromPath(link))
	if !slices.Equal(linked.WorkingDirectories, []string{uriB}) {
		t.Fatalf("linked directory opened another project: %v", linked.WorkingDirectories)
	}
	if err := client.Request(ctx, "resourceRead", types.ResourceReadParams{Channel: types.RootResourceURI, Uri: fileuri.FromPath(filepath.Join(link, "README.md"))}, &content); err != nil || content.Data != "b\n" {
		t.Fatalf("read through the link: %v %+v", err, content)
	}
	// Each project names its own changes, so both can be watched at once.
	if len(a.Changesets) != 3 || len(b.Changesets) != 3 || a.Changesets[0].UriTemplate == b.Changesets[0].UriTemplate {
		t.Fatalf("changesets: %+v %+v", a.Changesets, b.Changesets)
	}
	for _, changeset := range []string{a.Changesets[0].UriTemplate, b.Changesets[0].UriTemplate} {
		result, sub, err := client.Subscribe(ctx, changeset)
		if err != nil || result.Snapshot == nil || result.Snapshot.State.Changeset == nil {
			t.Fatalf("changeset %s: %v %+v", changeset, err, result)
		}
		sub.Close()
	}

	var sessions types.ListSessionsResult
	if err := client.Request(ctx, "listSessions", types.ListSessionsParams{Channel: types.RootResourceURI}, &sessions); err != nil {
		t.Fatal(err)
	}
	directories := map[string]string{}
	for _, session := range sessions.Items {
		directories[session.Resource] = strings.Join(session.WorkingDirectories, ",")
		if session.Project == nil || session.Project.Uri != directories[session.Resource] {
			t.Fatalf("listed session without its project: %+v", session)
		}
	}
	if len(directories) != 4 || directories["ahp-session:/in-a"] != uriA || directories["ahp-session:/in-b"] != uriB || directories["ahp-session:/through-link"] != uriB || directories["ahp-session:/default"] != fileuri.FromPath(root) {
		t.Fatalf("session list: %v", directories)
	}

	// Turns run in their own project, and its files become writable.
	state, sub := ahpSubscribeChat(t, client, *a.DefaultChat)
	ahpSend(t, client, state.Resource, "turn-in-a", 1)
	ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool {
		_, complete := e.Action.Value.(*types.ChatTurnCompleteAction)
		return complete
	})
	if len(state.Turns) != 1 {
		t.Fatalf("turn did not complete: %+v", state)
	}
	if err := client.Request(ctx, "resourceWrite", types.ResourceWriteParams{Channel: types.RootResourceURI, Uri: readme, Data: "changed", Encoding: types.ContentEncodingUtf8}, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dirA, "README.md")); string(data) != "changed" {
		t.Fatalf("write did not reach the project: %q", data)
	}

	// A terminal starts in the project its directory belongs to.
	terminal := "agenthost-terminal:/in-b"
	claim := types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient, ClientId: "editor"}}
	if err := client.Request(ctx, "createTerminal", types.CreateTerminalParams{Channel: terminal, Cwd: &uriB, Claim: claim}, nil); err != nil {
		t.Fatal(err)
	}
	shell, terminalSub, err := subscribeTerminal(ctx, client, terminal)
	if err != nil || shell.Cwd == nil || *shell.Cwd != uriB {
		t.Fatalf("terminal: %v %+v", err, shell)
	}
	terminalSub.Close()
	if err := client.Request(ctx, "disposeTerminal", types.DisposeTerminalParams{Channel: terminal}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHostRestoresSavedSessions(t *testing.T) {
	testenv.WingmanHome(t)
	ahpTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, uri := hostTestProject(t, root, "project")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const session = "ahp-session:/remembered"

	first, endpoint := hostTestStart(t, root, "")
	client := ahpTestClient(t, endpoint, "editor")
	created := hostTestSession(t, client, session, uri)
	state, sub := ahpSubscribeChat(t, client, *created.DefaultChat)
	ahpSend(t, client, state.Resource, "first-turn", 1)
	ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool {
		_, complete := e.Action.Value.(*types.ChatTurnCompleteAction)
		return complete
	})
	_ = client.Shutdown(ctx)
	first.Close()

	// A restarted host lists the session without opening its project.
	second, endpoint := hostTestStart(t, root, "")
	client = ahpTestClient(t, endpoint, "editor")
	var sessions types.ListSessionsResult
	if err := client.Request(ctx, "listSessions", types.ListSessionsParams{Channel: types.RootResourceURI}, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions.Items) != 1 || sessions.Items[0].Resource != session || !slices.Equal(sessions.Items[0].WorkingDirectories, []string{uri}) || sessions.Items[0].Title == "Session" {
		t.Fatalf("saved sessions: %+v", sessions.Items)
	}
	if project := sessions.Items[0].Project; project == nil || project.Uri != uri || project.DisplayName != "project" {
		t.Fatalf("saved session lost its project: %+v", project)
	}
	if loaded := second.ahp.loaded(); len(loaded) != 0 {
		t.Fatalf("listing opened %d projects", len(loaded))
	}
	// Opening it loads the project and its history.
	state, _ = ahpSubscribeChat(t, client, ahpChatURI(session))
	if len(state.Turns) != 1 || state.Turns[0].Id != "first-turn" {
		t.Fatalf("history was not restored: %+v", state.Turns)
	}
	if loaded := second.ahp.loaded(); len(loaded) != 1 {
		t.Fatalf("subscribing opened %d projects", len(loaded))
	}
}

func TestHostClosesIdleProjects(t *testing.T) {
	testenv.WingmanHome(t)
	ahpTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, uri := hostTestProject(t, root, "project")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const session = "ahp-session:/resting"

	host, endpoint := hostTestStart(t, root, "")
	client := ahpTestClient(t, endpoint, "editor")
	created := hostTestSession(t, client, session, uri)
	state, sub := ahpSubscribeChat(t, client, *created.DefaultChat)
	ahpSend(t, client, state.Resource, "only-turn", 1)
	ahpUntil(t, state, sub, func(e types.ActionEnvelope) bool {
		_, complete := e.Action.Value.(*types.ChatTurnCompleteAction)
		return complete
	})

	// A running terminal is work that closing would cut off.
	project := host.ahp.loaded()[0]
	// The turn's last action reaches clients just before its session settles.
	for settled := time.Now().Add(5 * time.Second); project.server.busy(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(settled) {
			t.Fatal("project stayed busy after its turn completed")
		}
	}
	if terminal.Supported() {
		shell := "agenthost-terminal:/resting"
		claim := types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient, ClientId: "editor"}}
		if err := client.Request(ctx, "createTerminal", types.CreateTerminalParams{Channel: shell, Cwd: &uri, Claim: claim}, nil); err != nil {
			t.Fatal(err)
		}
		if !project.server.busy() {
			t.Fatal("a running terminal did not keep its project busy")
		}
		if err := client.Request(ctx, "disposeTerminal", types.DisposeTerminalParams{Channel: shell}, nil); err != nil {
			t.Fatal(err)
		}
	}

	// A rename outlives the project's closing.
	rename := types.DispatchActionParams{Channel: session, ClientSeq: 2, Action: types.StateAction{Value: &types.SessionTitleChangedAction{Type: types.ActionTypeSessionTitleChanged, Title: "Renamed session"}}}
	if err := client.Notify(ctx, "dispatchAction", rename); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if items := reviewHostCatalog(t, host.ahp); len(items) == 1 && items[0].Title == "Renamed session" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rename did not take")
		}
	}

	// A client that follows the session keeps its project open.
	host.ahp.closeIdle(0)
	if loaded := host.ahp.loaded(); len(loaded) != 1 {
		t.Fatalf("closed a project a client follows: %d open", len(loaded))
	}
	_ = client.Shutdown(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for host.ahp.connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Recent use keeps it open too; after the idle time it closes.
	host.ahp.closeIdle(time.Hour)
	if loaded := host.ahp.loaded(); len(loaded) != 1 {
		t.Fatalf("closed a project that was just used: %d open", len(loaded))
	}
	host.ahp.closeIdle(0)
	if loaded := host.ahp.loaded(); len(loaded) != 0 {
		t.Fatalf("idle project stayed open: %d", len(loaded))
	}

	// Its session stays listed and reopens the project with its history.
	client = ahpTestClient(t, endpoint, "editor")
	var sessions types.ListSessionsResult
	if err := client.Request(ctx, "listSessions", types.ListSessionsParams{Channel: types.RootResourceURI}, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions.Items) != 1 || sessions.Items[0].Resource != session || sessions.Items[0].Title != "Renamed session" {
		t.Fatalf("sessions of a closed project: %+v", sessions.Items)
	}
	state, _ = ahpSubscribeChat(t, client, ahpChatURI(session))
	if len(state.Turns) != 1 || state.Turns[0].Id != "only-turn" {
		t.Fatalf("history after reopening: %+v", state.Turns)
	}
	if items := reviewHostCatalog(t, host.ahp); len(items) != 1 || items[0].Title != "Renamed session" {
		t.Fatalf("reopening lost the rename: %+v", items)
	}
	if loaded := host.ahp.loaded(); len(loaded) != 1 {
		t.Fatalf("reopening left %d projects open", len(loaded))
	}
}

func TestHostRequiresItsToken(t *testing.T) {
	testenv.WingmanHome(t)
	_, endpoint := hostTestStart(t, canonicalPath(t.TempDir()), "secret")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if transport, err := ahpws.Connect(ctx, endpoint); err == nil {
		_ = transport.Close(ctx)
		t.Fatal("connected without the token")
	}
	if transport, err := ahpws.Connect(ctx, endpoint+"?tkn=wrong"); err == nil {
		_ = transport.Close(ctx)
		t.Fatal("connected with a wrong token")
	}
	client := ahpTestClient(t, endpoint+"?tkn=secret", "editor")
	var sessions types.ListSessionsResult
	if err := client.Request(ctx, "listSessions", types.ListSessionsParams{Channel: types.RootResourceURI}, &sessions); err != nil {
		t.Fatal(err)
	}
}
