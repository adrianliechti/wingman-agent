package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

func jsonString(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLauncherSmoke(t *testing.T) {
	testenv.UserHome(t)
	home := testenv.WingmanHome(t)
	t.Setenv("WINGMAN_URL", "https://example.com")
	t.Setenv("WINGMAN_TOKEN", "secret")

	app := &App{}
	app.launcher = app.newLauncher()

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://localhost"+path, nil)
		if srv := app.server.Load(); srv != nil {
			req.Header.Set("X-Wingman-Instance", srv.InstanceID())
		}
		app.ServeHTTP(rec, req)
		return rec
	}

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://localhost"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if srv := app.server.Load(); srv != nil {
			req.Header.Set("X-Wingman-Instance", srv.InstanceID())
		}
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		return rec
	}

	if rec := get("/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Wingman Agent") {
		t.Fatalf("start page: %d", rec.Code)
	} else if !strings.Contains(rec.Body.String(), "--shell-window-drag: drag") {
		t.Fatal("start page has no window drag region")
	} else if !strings.Contains(rec.Body.String(), `data-window-chrome="windows-overlay"`) {
		t.Fatal("start page has no app-owned Windows menu")
	} else if !strings.Contains(rec.Body.String(), "window.shell?.platform") {
		t.Fatal("start page does not consume the injected shell environment")
	} else if strings.Contains(rec.Body.String(), "shell_chrome") {
		t.Fatal("start page still depends on URL-based shell detection")
	}

	if rec := get("/app/workspaces"); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("workspaces: %d %q", rec.Code, rec.Body.String())
	}

	if rec := get("/app/settings"); rec.Code != http.StatusNotFound {
		t.Fatalf("removed settings endpoint: %d %q", rec.Code, rec.Body.String())
	}

	if rec := post("/app/workspaces/remove", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("remove without path: %d", rec.Code)
	}

	workspace := t.TempDir()

	if rec := post("/app/workspaces/open", `{"path":`+jsonString(t, workspace)+`}`); rec.Code != http.StatusNoContent {
		t.Fatalf("open workspace: %d %q", rec.Code, rec.Body.String())
	}
	config, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if strings.Contains(string(config), "url") || strings.Contains(string(config), "token") || strings.Contains(string(config), "secret") {
		t.Fatalf("config contains connection credentials: %s", config)
	}

	if rec := get("/app/workspaces"); rec.Code != http.StatusOK {
		t.Fatalf("app commands unavailable after open: %d", rec.Code)
	}

	if rec := get("/api/workspace"); rec.Code != http.StatusOK {
		t.Fatalf("workspace server not mounted: %d %q", rec.Code, rec.Body.String())
	}

	if rec := post("/app/workspaces/open", `{"path":`+jsonString(t, workspace)+`}`); rec.Code == http.StatusNoContent {
		t.Fatalf("second open should fail")
	}

	stale := httptest.NewRequest("POST", "http://localhost/app/workspaces/open", strings.NewReader(`{"path":`+jsonString(t, workspace)+`,"replace":true}`))
	stale.Header.Set("X-Wingman-Instance", "old-instance")
	rejected := httptest.NewRecorder()
	app.ServeHTTP(rejected, stale)
	if rejected.Code != http.StatusConflict {
		t.Fatalf("stale workspace replacement: %d", rejected.Code)
	}
	replacement := t.TempDir()
	if rec := post("/app/workspaces/open", `{"path":`+jsonString(t, replacement)+`,"replace":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("replace workspace: %d %q", rec.Code, rec.Body.String())
	}

	app.shutdown()
}

func TestLauncherRejectsRebindingAndCrossOriginCommands(t *testing.T) {
	app := &App{}
	handler := app.newLauncher()
	for _, path := range []string{"/", "/app/workspaces", "/app/workspaces/open"} {
		request := httptest.NewRequest("POST", "http://attacker.example"+path, nil)
		request.Header.Set("Origin", "http://attacker.example")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("host check allowed %s: %d", path, response.Code)
		}
	}
	request := httptest.NewRequest("POST", "http://localhost/app/workspaces/open", nil)
	request.Header.Set("Origin", "https://attacker.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin command allowed: %d", response.Code)
	}
}

func TestConcurrentWorkspaceReplacementsHaveOneWinner(t *testing.T) {
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer gateway.Close()
	t.Setenv("WINGMAN_URL", gateway.URL)
	t.Setenv("WINGMAN_TOKEN", "test")
	app := &App{}
	app.launcher = app.newLauncher()
	t.Cleanup(func() {
		if srv := app.server.Load(); srv != nil {
			srv.Close()
		}
	})
	open := httptest.NewRequest(http.MethodPost, "http://localhost/app/workspaces/open", strings.NewReader(`{"path":`+jsonString(t, t.TempDir())+`}`))
	opened := httptest.NewRecorder()
	app.ServeHTTP(opened, open)
	if opened.Code != http.StatusNoContent {
		t.Fatalf("open workspace: %d %s", opened.Code, opened.Body)
	}
	previous := app.server.Load()
	t.Cleanup(previous.Close)

	start := make(chan struct{})
	results := make(chan int, 2)
	var workers sync.WaitGroup
	for range cap(results) {
		request := httptest.NewRequest(http.MethodPost, "http://localhost/app/workspaces/open", strings.NewReader(`{"path":`+jsonString(t, t.TempDir())+`,"replace":true}`))
		request.Header.Set("X-Wingman-Instance", previous.InstanceID())
		workers.Go(func() {
			<-start
			response := httptest.NewRecorder()
			app.ServeHTTP(response, request)
			results <- response.Code
		})
	}
	workers.Go(func() {
		<-start
		for range 20 {
			app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://localhost/", nil))
		}
	})
	close(start)
	workers.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[http.StatusNoContent] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("replacement responses = %v", counts)
	}
	current := app.server.Load()
	if current == previous {
		t.Fatal("successful replacement did not mount the new workspace")
	}
	request := httptest.NewRequest(http.MethodGet, "http://localhost/api/workspace", nil)
	request.Header.Set("X-Wingman-Instance", current.InstanceID())
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("replacement is unavailable: %d %s", response.Code, response.Body)
	}
}
