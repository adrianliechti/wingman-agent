package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/settings"
)

type windowResponse struct {
	WorkspaceID string `json:"workspaceId"`
	InstanceID  string `json:"instanceId"`
	Namespace   string `json:"namespace"`
	Host        string `json:"host"`
	Directory   string `json:"directory"`
	Path        string `json:"path"`
	Terminal    string `json:"window.terminal.position"`
}

func windowRequest(t *testing.T, s *Server, method, target, instance, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://localhost"+target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if instance != "" {
		request.Header.Set(instanceHeader, instance)
	}
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	return recorder
}

func windowOpen(t *testing.T, s *Server, project string) windowResponse {
	t.Helper()
	recorder := windowRequest(t, s, http.MethodGet, "/api/workspace?project="+url.QueryEscape(project), "", "")
	var window windowResponse
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &window) != nil {
		t.Fatalf("open %s: %d %s", project, recorder.Code, recorder.Body.String())
	}
	return window
}

func TestServerShowsOtherProjectsInWindowsOfOneHost(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	dir := canonicalPath(t.TempDir())
	other := filepath.Join(dir, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true, Projects: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	start := windowOpen(t, s, dir)
	if start.InstanceID != s.InstanceID() || start.Host != s.Project.host.id || start.Directory != fileuri.FromPath(dir) {
		t.Fatalf("the start folder's window: %+v", start)
	}
	window := windowOpen(t, s, other)
	if window.Path != other || window.InstanceID == s.InstanceID() || window.Host != start.Host || window.Directory != fileuri.FromPath(other) || window.Namespace == start.Namespace {
		t.Fatalf("another folder's window: %+v", window)
	}
	// A file URI names the same project and its window.
	if again := windowOpen(t, s, fileuri.FromPath(other)); again.InstanceID != window.InstanceID {
		t.Fatalf("the file URI opened another window: %+v", again)
	}
	// Requests carry their window's instance and reach its project.
	var shown windowResponse
	if recorder := windowRequest(t, s, http.MethodGet, "/api/workspace", window.InstanceID, ""); recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &shown) != nil || shown.Path != other {
		t.Fatalf("the window's request reached %q: %d %s", shown.Path, recorder.Code, recorder.Body.String())
	}
	if recorder := windowRequest(t, s, http.MethodGet, "/api/workspace", "unknown", ""); recorder.Code != http.StatusConflict {
		t.Fatalf("an unknown instance: %d", recorder.Code)
	}

	// Browser preferences are the host's, whichever window changes them.
	if recorder := windowRequest(t, s, http.MethodPost, "/api/settings/window.terminal.position", window.InstanceID, `{"window.terminal.position":"bottom"}`); recorder.Code != http.StatusOK {
		t.Fatalf("a window's preference: %d %s", recorder.Code, recorder.Body.String())
	}
	if s.windowTerminalPosition() != settings.WindowTerminalPositionBottom {
		t.Fatal("the start folder's window kept the old preference")
	}

	// Its project's events name the workspace, so that windows find their own.
	observer := reviewSessionsObserver(t, s.Project, "1.1.0", "ahp-root://")
	s.windowOf(window.InstanceID).Project.workspaceChanged(Frame{Type: EvtFilesChanged})
	select {
	case data := <-observer.writer.outbox:
		var frame struct {
			Method string `json:"method"`
			Params struct {
				Workspace string `json:"workspace"`
			} `json:"params"`
		}
		if json.Unmarshal(data, &frame) != nil || frame.Method != "x-wingman/workspaceChanged" || frame.Params.Workspace != window.WorkspaceID {
			t.Fatalf("workspace event: %s", data)
		}
	default:
		t.Fatal("a window's project announced no file change")
	}

	// A project that closes takes its window along; naming it opens a new one.
	web := httptest.NewServer(s)
	t.Cleanup(web.Close)
	client := protocolTestClient(t, "ws"+strings.TrimPrefix(web.URL, "http")+"/ahp?instance="+window.InstanceID, "window")
	s.Project.host.closeIdle(0)
	secondary := s.windowOf(window.InstanceID)
	if secondary == nil {
		t.Fatal("idle cleanup closed the project of a connected window")
	}
	_ = client.Shutdown(t.Context())
	waitProtocol(t, func() bool {
		s.Project.host.mu.Lock()
		defer s.Project.host.mu.Unlock()
		return secondary.Project.windowRequests == 0
	})
	s.Project.host.closeIdle(0)
	if recorder := windowRequest(t, s, http.MethodGet, "/api/workspace", window.InstanceID, ""); recorder.Code != http.StatusConflict {
		t.Fatalf("a closed project's window still answers: %d", recorder.Code)
	}
	if _, err := s.window(secondary.Project); err != errWorkspaceClosed {
		t.Fatalf("a closed project acquired a window: %v", err)
	}
	if reopened := windowOpen(t, s, other); reopened.InstanceID == window.InstanceID {
		t.Fatal("the reopened project kept the closed one's instance")
	}
	if recorder := windowRequest(t, s, http.MethodGet, "/api/workspace", start.InstanceID, ""); recorder.Code != http.StatusOK {
		t.Fatalf("the start folder's window closed with another: %d", recorder.Code)
	}
}

// The desktop app's server opens any folder a window names, but only as the
// first request of a window on the host it was meant for.
func TestServerOfEveryProjectOpensAnyFolderAfterItsChecks(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	dir := canonicalPath(t.TempDir())
	outside := canonicalPath(t.TempDir())
	s, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true, Projects: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	target := "/api/workspace?project=" + url.QueryEscape(outside)
	request := httptest.NewRequest(http.MethodGet, "http://unrecognized.invalid"+target, nil)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || len(s.Project.host.loaded()) != 1 {
		t.Fatal("project discovery ran before the host check")
	}
	if rec := windowRequest(t, s, http.MethodGet, target, "stale", ""); rec.Code != http.StatusConflict || len(s.Project.host.loaded()) != 1 {
		t.Fatal("a stale discovery request opened a project")
	}
	if rec := windowRequest(t, s, http.MethodPost, target, "", ""); rec.Code != http.StatusMethodNotAllowed || len(s.Project.host.loaded()) != 1 {
		t.Fatal("a method other than GET opened a project")
	}
	for project, code := range map[string]int{"relative": http.StatusBadRequest, filepath.Join(dir, "missing"): http.StatusNotFound} {
		if rec := windowRequest(t, s, http.MethodGet, "/api/workspace?project="+url.QueryEscape(project), "", ""); rec.Code != code {
			t.Fatalf("%s: %d %s", project, rec.Code, rec.Body.String())
		}
	}
	// A folder outside the one the server started in opens too.
	if window := windowOpen(t, s, outside); window.Path != outside {
		t.Fatalf("the other folder's window: %+v", window)
	}
	// Once its folder is removed it shows in no window, though its project
	// is open still, and other folders open as before.
	if err := os.RemoveAll(outside); err != nil {
		t.Fatal(err)
	}
	if rec := windowRequest(t, s, http.MethodGet, target, "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a removed folder's window: %d %s", rec.Code, rec.Body.String())
	}
	if window := windowOpen(t, s, dir); window.InstanceID != s.InstanceID() {
		t.Fatalf("the start folder's window: %+v", window)
	}
}

// A server started for one folder, as `wingman server` is, shows that folder
// and no other: a browser, or a phone through the relay, reaches it, and any
// website can send a browser to its address.
func TestServerOfOneFolderShowsNoOtherProject(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	dir := canonicalPath(t.TempDir())
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	h := s.Project.host
	for _, project := range []string{nested, canonicalPath(t.TempDir()), filepath.Join(dir, "missing")} {
		if rec := windowRequest(t, s, http.MethodGet, "/api/workspace?project="+url.QueryEscape(project), "", ""); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s", project, rec.Code, rec.Body.String())
		}
	}
	if len(h.loaded()) != 1 {
		t.Fatal("a window opened another project")
	}
	// Its own folder is the window it shows anyway.
	if window := windowOpen(t, s, dir); window.InstanceID != s.InstanceID() {
		t.Fatalf("the start folder's window: %+v", window)
	}
	// Its list holds its own folder's sessions: an agent that keeps the
	// sessions of every folder is asked for this one alone.
	agent := &everywhereAgent{protocolAgent: &protocolAgent{}, sessions: []code.SessionInfo{{ID: "nested", Dir: nested}}}
	b := s.Project.bindBackend("two", agent)
	s.Project.backendsMu.Lock()
	s.Project.backends[b.id] = &backendEntry{runtime: b}
	s.Project.backendsMu.Unlock()
	for _, item := range reviewHostCatalog(t, h) {
		if item.Provider == "two" {
			t.Fatalf("a session of another folder is listed: %+v", item)
		}
	}
	if agent.everywhere.Load() != 0 || agent.here.Load() != 1 {
		t.Fatalf("listed every folder %d times and the project %d times", agent.everywhere.Load(), agent.here.Load())
	}
}
