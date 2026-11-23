package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
	"github.com/adrianliechti/wingman-agent/server/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func reviewSaveHostAlias(t *testing.T, dir, resource, provider string) sessionAlias {
	t.Helper()
	a := sessionAlias{Resource: resource, Project: dir, Backend: provider, Session: "saved", Created: time.Now(), Title: "Private project title"}
	if err := writeAlias(a); err != nil {
		t.Fatal(err)
	}
	return a
}

func reviewHostCatalog(t *testing.T, h *Host) []types.SessionSummary {
	t.Helper()
	raw, _ := json.Marshal(types.ListSessionsParams{Channel: types.RootResourceURI})
	result, err := h.listSessions(raw)
	if err != nil {
		t.Fatal(err)
	}
	return result.(types.ListSessionsResult).Items
}

func TestReviewHostOfOneFolderFiltersSavedSessions(t *testing.T) {
	testenv.WingmanHome(t)
	allowed, excluded := canonicalPath(t.TempDir()), canonicalPath(t.TempDir())
	reviewSaveHostAlias(t, excluded, "ahp-session:/excluded", "wingman")
	h, err := NewHost(t.Context(), &HostOptions{folder: allowed})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if items := reviewHostCatalog(t, h); len(items) != 0 {
		t.Fatalf("the host of one folder lists the sessions of another: %+v", items)
	}
}

func TestReviewHostRetainsDormantProviderSessions(t *testing.T) {
	testenv.WingmanHome(t)
	dir := canonicalPath(t.TempDir())
	reviewSaveHostAlias(t, dir, "ahp-session:/external", "external")
	h, err := NewHost(t.Context(), hostTestOptions(t, dir, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if items := reviewHostCatalog(t, h); len(items) != 1 {
		t.Fatalf("initial catalog: %+v", items)
	}
	// Opening the project starts the builtin backend, leaving external providers dormant.
	protocolTestGateway(t)
	if _, err := h.projectAt(dir); err != nil {
		t.Fatal(err)
	}
	if items := reviewHostCatalog(t, h); len(items) != 1 {
		t.Fatalf("opening the project hid saved external sessions: %+v", items)
	}
}

func TestReviewHostScopesNativeSessionIDs(t *testing.T) {
	testenv.WingmanHome(t)
	h := newHost(context.Background())
	h.open = func(string) (*Project, error) { panic("unexpected open") }
	makeProject := func(dir string) *Project {
		s := &Project{ctx: t.Context(), scope: workspaceScope(dir), workspace: &code.Workspace{RootPath: dir}, backends: map[string]*backendEntry{}}
		p := h.attach(s)
		agent := &lifecycleAgent{protocolAgent: &protocolAgent{}, list: func(context.Context) ([]code.SessionInfo, error) {
			return []code.SessionInfo{{ID: "saved", Title: "Local session", UpdatedAt: time.Now()}}, nil
		}}
		s.backends["one"] = &backendEntry{runtime: &backendRuntime{Project: s, id: "one", agent: agent}}
		return p
	}
	a, b := makeProject(canonicalPath(t.TempDir())), makeProject(canonicalPath(t.TempDir()))
	h.aliases["ahp-session:/a"] = sessionAlias{Resource: "ahp-session:/a", Project: a.dir, Backend: "one", Session: "saved"}
	listed := b.nativeSessions([]types.AgentInfo{{Provider: "one"}}, nil, map[string]bool{})
	if len(listed) != 1 {
		t.Fatalf("project B listed %+v", listed)
	}
	h.mu.Lock()
	item, ok := h.listedSummaryLocked(listed[0], h.savedIndexLocked(), map[string]bool{})
	h.mu.Unlock()
	if !ok || item.Resource != b.canonicalURI("one", "saved") {
		t.Fatalf("project B adopted project A's alias: %+v", item)
	}
}

func TestReviewHostConcurrentOpening(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	root := canonicalPath(t.TempDir())
	h, err := NewHost(t.Context(), hostTestOptions(t, root, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = h.root()
		}
	})
	defer func() { close(stop); wg.Wait() }()
	for i := range 5 {
		dir := filepath.Join(root, fmt.Sprint(i))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := h.projectAt(dir); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReviewHostCreatesSessionViaSymlink(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	root := canonicalPath(t.TempDir())
	dir, _ := hostTestProject(t, root, "real")
	link := filepath.Join(root, "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	h, err := NewHost(t.Context(), hostTestOptions(t, root, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c := &connection{host: h, id: "review"}
	raw, _ := json.Marshal(types.CreateSessionParams{Channel: "ahp-session:/symlink", WorkingDirectories: []string{fileuri.FromPath(link)}})
	if _, err := c.request(ahp.Request{Method: "createSession", Params: raw}); err != nil {
		t.Fatalf("valid symlink working directory rejected: %v", err)
	}
}

func TestReviewHostTerminalNamesAreAtomic(t *testing.T) {
	if !terminal.Supported() {
		t.Skip("PTY terminals are unsupported")
	}
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	root := canonicalPath(t.TempDir())
	h, err := NewHost(t.Context(), hostTestOptions(t, root, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var dirs []string
	for _, name := range []string{"a", "b"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := h.projectAt(dir); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, fileuri.FromPath(dir))
	}
	c := &connection{host: h, id: "review"}
	claim := types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient, ClientId: c.id}}
	for i := range 5 {
		start, errs := make(chan struct{}), make(chan error, 2)
		channel := fmt.Sprintf("ahp-terminal:/review-%d", i)
		for _, dir := range dirs {
			raw, _ := json.Marshal(types.CreateTerminalParams{Channel: channel, Cwd: &dir, Claim: claim})
			go func() { <-start; _, err := c.createTerminal(raw); errs <- err }()
		}
		close(start)
		e1, e2 := <-errs, <-errs
		if e1 == nil && e2 == nil {
			t.Fatalf("both projects created the same terminal channel %s", channel)
		}
		if e1 != nil && e2 != nil {
			t.Fatalf("both attempts failed: %v / %v", e1, e2)
		}
	}
}
