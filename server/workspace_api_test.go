package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func workspaceAPI(t *testing.T, app *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	req.Header.Set(instanceHeader, app.scope.InstanceID)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

func TestWorkspaceSearchStreamsFilesThenSummary(t *testing.T) {
	app, _ := newLSPTestApp(t, map[string]string{"a.txt": "hello world\n", "b.txt": "nothing here\n"})
	rec := workspaceAPI(t, app, http.MethodPost, "/api/workspace/search", `{"query":"hello"}`)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("search: %d %s", rec.Code, rec.Body)
	}
	var events []map[string]any
	scanner := bufio.NewScanner(rec.Body)
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) != 2 || events[0]["type"] != "file" || events[0]["file"].(map[string]any)["path"] != "a.txt" || events[1]["type"] != "done" || events[1]["files"] != float64(1) || events[1]["matches"] != float64(1) {
		t.Fatalf("events = %v", events)
	}
	for _, body := range []string{`{"query":""}`, `{"query":"x","channel":"ahp-root://"}`, `{"query":"(","regex":true}`} {
		if rec := workspaceAPI(t, app, http.MethodPost, "/api/workspace/search", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
}

func TestWorkspaceFilesRevealAndTerminalStatus(t *testing.T) {
	app, _ := newLSPTestApp(t, map[string]string{"alpha.go": "package a\n", "beta.go": "package b\n"})
	rec := workspaceAPI(t, app, http.MethodGet, "/api/workspace/files?query=ALP", "")
	var hits []fileSearchHit
	if err := json.Unmarshal(rec.Body.Bytes(), &hits); err != nil || len(hits) != 1 || hits[0].Path != "alpha.go" {
		t.Fatalf("files: %d %s", rec.Code, rec.Body)
	}
	// Only invalid targets are exercised; a valid one would open the file manager.
	for body, want := range map[string]int{`{"path":"../outside"}`: http.StatusBadRequest, `{"path":"missing.go"}`: http.StatusNotFound, `{"uri":"file:///x"}`: http.StatusBadRequest} {
		if rec := workspaceAPI(t, app, http.MethodPost, "/api/workspace/reveal", body); rec.Code != want {
			t.Fatalf("reveal %s: %d", body, rec.Code)
		}
	}
	status := func(uri string) *httptest.ResponseRecorder {
		return workspaceAPI(t, app, http.MethodGet, "/api/workspace/terminals/"+url.PathEscape(uri)+"/status", "")
	}
	if rec := status("ahp-terminal:/missing"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown terminal: %d", rec.Code)
	}
	session, err := app.terminals.Create("", 80, 24)
	if err != nil {
		t.Skipf("no PTY: %v", err)
	}
	uri := "ahp-terminal:/wingman/" + session.ID()
	deadline := time.Now().Add(2 * time.Second)
	rec = status(uri)
	for rec.Code == http.StatusNotFound && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		rec = status(uri)
	}
	var result struct {
		Busy *bool `json:"busy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK || result.Busy == nil {
		t.Fatalf("terminal status: %d %s", rec.Code, rec.Body)
	}
}
