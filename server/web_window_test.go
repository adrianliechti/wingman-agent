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
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
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
	s, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	start := windowOpen(t, s, dir)
	if start.InstanceID != s.InstanceID() || start.Host != s.Project.host.id || start.Directory != fileuri.FromPath(dir) {
		t.Fatalf("the start folder's window: %+v", start)
	}
	// A window opens only a project the user already used.
	if recorder := windowRequest(t, s, http.MethodGet, "/api/workspace?project="+url.QueryEscape(other), "", ""); recorder.Code != http.StatusForbidden {
		t.Fatalf("a folder nobody opened: %d %s", recorder.Code, recorder.Body.String())
	}
	if err := s.Open(other); err != nil {
		t.Fatal(err)
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
	s.Project.host.closeIdle(0)
	if recorder := windowRequest(t, s, http.MethodGet, "/api/workspace", window.InstanceID, ""); recorder.Code != http.StatusConflict {
		t.Fatalf("a closed project's window still answers: %d", recorder.Code)
	}
	if reopened := windowOpen(t, s, other); reopened.InstanceID == window.InstanceID {
		t.Fatal("the reopened project kept the closed one's instance")
	}
	if recorder := windowRequest(t, s, http.MethodGet, "/api/workspace", start.InstanceID, ""); recorder.Code != http.StatusOK {
		t.Fatalf("the start folder's window closed with another: %d", recorder.Code)
	}
}

func TestServerWindowsOpenOnlyProjectsTheUserUsed(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	dir := canonicalPath(t.TempDir())
	outside := canonicalPath(t.TempDir())
	used := filepath.Join(dir, "used")
	if err := os.Mkdir(used, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	refused := func(project string, code int) {
		t.Helper()
		if recorder := windowRequest(t, s, http.MethodGet, "/api/workspace?project="+url.QueryEscape(project), "", ""); recorder.Code != code {
			t.Fatalf("%s: %d %s", project, recorder.Code, recorder.Body.String())
		}
	}
	// Any website can send a browser to a window's address; it opens no folder of its choice.
	refused(outside, http.StatusForbidden)
	refused(filepath.Join(dir, "missing"), http.StatusForbidden)
	refused("relative", http.StatusBadRequest)
	// A project with sessions is one the user used.
	if err := s.Project.storeAlias(sessionAlias{Resource: "ahp-session:/used", Backend: "wingman", Session: "used", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s.Project.host.mu.Lock()
	alias := s.Project.host.aliases["ahp-session:/used"]
	alias.Project = used
	s.Project.host.rememberAliasLocked(alias)
	s.Project.host.mu.Unlock()
	if window := windowOpen(t, s, used); window.Path != used {
		t.Fatalf("the used project's window: %+v", window)
	}
	// A folder the user picked opens, even outside the roots.
	if err := s.Open(outside); err != nil {
		t.Fatal(err)
	}
	if window := windowOpen(t, s, outside); window.Path != outside {
		t.Fatalf("the picked folder's window: %+v", window)
	}
}
