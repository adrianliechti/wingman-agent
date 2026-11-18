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
	"github.com/adrianliechti/wingman-agent/server/ahp"
	sdk "github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/microsoft/agent-host-protocol/clients/go/ahpws"
)

// hostTestStart serves a host whose clients start choosing a folder in root,
// as a user's host does in the home directory.
func hostTestStart(t *testing.T, root, token string) (*Host, string) {
	t.Helper()
	host, err := NewHost(t.Context(), hostTestOptions(t, root, token))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(host)
	t.Cleanup(func() { server.Close(); host.Close() })
	return host, "ws" + strings.TrimPrefix(server.URL, "http") + "/ahp"
}

// hostTestOptions are those of a host whose user's home directory is root.
func hostTestOptions(t *testing.T, root, token string) *HostOptions {
	t.Helper()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	return &HostOptions{Token: token, disableManagedTools: true}
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
	var rpc *sdk.RPCError
	if !errors.As(err, &rpc) || rpc.Code != want {
		t.Fatalf("want error %d, got %v", want, err)
	}
}

func hostTestSession(t *testing.T, client *sdk.Client, uri, directory string) *types.SessionState {
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

func TestHostRejectsAMissingFolder(t *testing.T) {
	if _, err := NewHost(t.Context(), &HostOptions{folder: filepath.Join(t.TempDir(), "missing"), disableManagedTools: true}); err == nil {
		t.Fatal("a host started for a folder that does not exist")
	}
}

// A cut-off creation retried in another directory is discarded by the project that began it.
func TestHostRetriedCreationDiscardsWhatAnotherProjectSaved(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	root := canonicalPath(t.TempDir())
	dirA, _ := hostTestProject(t, root, "a")
	_, uriB := hostTestProject(t, root, "b")
	host, endpoint := hostTestStart(t, root, "")
	a, err := host.projectAt(dirA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := a.backend("wingman")
	if err != nil {
		t.Fatal(err)
	}
	record := b.createSession(Command{ID: "cut-off", Type: "create"})
	if record.err != nil {
		t.Fatal(record.err)
	}
	uri, native := "ahp-session:/retried", record.receipt.Ref.SessionID
	if err := a.storeAlias(sessionAlias{Resource: uri, Backend: "wingman", Session: native, Created: time.Now().UTC(), CreationError: "session configuration is incomplete"}); err != nil {
		t.Fatal(err)
	}
	created := hostTestSession(t, protocolTestClient(t, endpoint, "retry"), uri, uriB)
	if created.Project == nil || created.Project.Uri != uriB {
		t.Fatalf("project: %+v", created.Project)
	}
	dir, err := a.aliasDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(readAliases(dir), func(alias sessionAlias) bool { return alias.Resource == uri }) {
		t.Fatal("the cut-off creation stays saved in the project that began it")
	}
	if err := b.agent.LoadSession(t.Context(), native); err == nil {
		t.Fatal("the cut-off native session was kept")
	}
}

func TestHostServesSeveralProjects(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, endpoint := hostTestStart(t, root, "")
	dirA, uriA := hostTestProject(t, root, "a")
	dirB, uriB := hostTestProject(t, root, "b")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	client := protocolTestClient(t, endpoint, "")
	hello, err := client.Initialize(ctx, "editor", []string{"1.1.0"}, []string{types.RootResourceURI})
	if err != nil {
		t.Fatal(err)
	}
	// A client starts choosing a directory in the home directory.
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
	// Every folder can be chosen, also one outside the home directory.
	if err := client.Request(ctx, "resourceList", types.ResourceListParams{Channel: types.RootResourceURI, Uri: fileuri.FromPath(filepath.Dir(root))}, &listing); err != nil {
		t.Fatalf("browse outside the home directory: %v", err)
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
	if len(directories) != 3 || directories["ahp-session:/in-a"] != uriA || directories["ahp-session:/in-b"] != uriB || directories["ahp-session:/through-link"] != uriB {
		t.Fatalf("session list: %v", directories)
	}

	// Turns run in their own project, and its files become writable.
	state, sub := subscribeChat(t, client, *a.DefaultChat)
	send(t, client, state.Resource, "turn-in-a", 1)
	until(t, state, sub, func(e types.ActionEnvelope) bool {
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
	protocolTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, uri := hostTestProject(t, root, "project")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const session = "ahp-session:/remembered"

	first, endpoint := hostTestStart(t, root, "")
	client := protocolTestClient(t, endpoint, "editor")
	created := hostTestSession(t, client, session, uri)
	state, sub := subscribeChat(t, client, *created.DefaultChat)
	send(t, client, state.Resource, "first-turn", 1)
	until(t, state, sub, func(e types.ActionEnvelope) bool {
		_, complete := e.Action.Value.(*types.ChatTurnCompleteAction)
		return complete
	})
	_ = client.Shutdown(ctx)
	first.Close()

	// A restarted host lists the session without opening its project.
	second, endpoint := hostTestStart(t, root, "")
	client = protocolTestClient(t, endpoint, "editor")
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
	if loaded := second.loaded(); len(loaded) != 0 {
		t.Fatalf("listing opened %d projects", len(loaded))
	}
	// Opening it loads the project and its history.
	state, _ = subscribeChat(t, client, ahp.ChatURI(session))
	if len(state.Turns) != 1 || state.Turns[0].Id != "first-turn" {
		t.Fatalf("history was not restored: %+v", state.Turns)
	}
	if loaded := second.loaded(); len(loaded) != 1 {
		t.Fatalf("subscribing opened %d projects", len(loaded))
	}
}

func TestHostClosesIdleProjects(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	root := canonicalPath(t.TempDir())
	_, uri := hostTestProject(t, root, "project")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const session = "ahp-session:/resting"

	host, endpoint := hostTestStart(t, root, "")
	client := protocolTestClient(t, endpoint, "editor")
	created := hostTestSession(t, client, session, uri)
	state, sub := subscribeChat(t, client, *created.DefaultChat)
	send(t, client, state.Resource, "only-turn", 1)
	until(t, state, sub, func(e types.ActionEnvelope) bool {
		_, complete := e.Action.Value.(*types.ChatTurnCompleteAction)
		return complete
	})

	// A running terminal is work that closing would cut off.
	project := host.loaded()[0]
	// The turn's last action reaches clients just before its session settles.
	for settled := time.Now().Add(5 * time.Second); project.busy(); time.Sleep(10 * time.Millisecond) {
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
		if !project.busy() {
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
		if items := reviewHostCatalog(t, host); len(items) == 1 && items[0].Title == "Renamed session" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rename did not take")
		}
	}

	// A client that follows the session keeps its project open.
	host.closeIdle(0)
	if loaded := host.loaded(); len(loaded) != 1 {
		t.Fatalf("closed a project a client follows: %d open", len(loaded))
	}
	_ = client.Shutdown(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for host.connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Recent use keeps it open too; after the idle time it closes.
	host.closeIdle(time.Hour)
	if loaded := host.loaded(); len(loaded) != 1 {
		t.Fatalf("closed a project that was just used: %d open", len(loaded))
	}
	host.closeIdle(0)
	if loaded := host.loaded(); len(loaded) != 0 {
		t.Fatalf("idle project stayed open: %d", len(loaded))
	}

	// Its session stays listed and reopens the project with its history.
	client = protocolTestClient(t, endpoint, "editor")
	var sessions types.ListSessionsResult
	if err := client.Request(ctx, "listSessions", types.ListSessionsParams{Channel: types.RootResourceURI}, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions.Items) != 1 || sessions.Items[0].Resource != session || sessions.Items[0].Title != "Renamed session" {
		t.Fatalf("sessions of a closed project: %+v", sessions.Items)
	}
	state, _ = subscribeChat(t, client, ahp.ChatURI(session))
	if len(state.Turns) != 1 || state.Turns[0].Id != "only-turn" {
		t.Fatalf("history after reopening: %+v", state.Turns)
	}
	if items := reviewHostCatalog(t, host); len(items) != 1 || items[0].Title != "Renamed session" {
		t.Fatalf("reopening lost the rename: %+v", items)
	}
	if loaded := host.loaded(); len(loaded) != 1 {
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
	client := protocolTestClient(t, endpoint+"?tkn=secret", "editor")
	var sessions types.ListSessionsResult
	if err := client.Request(ctx, "listSessions", types.ListSessionsParams{Channel: types.RootResourceURI}, &sessions); err != nil {
		t.Fatal(err)
	}
}
