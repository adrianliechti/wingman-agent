package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/remote"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func reviewWorkspaceApp(t *testing.T, files map[string]string) (*Server, string) {
	t.Helper()
	app, root := newLSPTestApp(t, nil)
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return app, root
}

func reviewWorkspaceRequest(app *Server, method, target, host, instance string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost"+target, body)
	req.Host = host
	if instance != "" {
		req.Header.Set(instanceHeader, instance)
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

func reviewWorkspaceWaitSearches(app *Server, want int32, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if app.searches.Load() == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return app.searches.Load() == want
}

// Matches are padded with U+2028, which JSON escapes to six bytes, so the
// stream is far larger than loopback socket buffers.
func reviewWorkspaceLargeSearchFiles() map[string]string {
	line := strings.Repeat(strings.Repeat(" ", 80)+"x", 50) + "\n"
	content := strings.Repeat(line, 10)
	files := map[string]string{}
	for i := range 40 {
		files[fmt.Sprintf("big/%02d.txt", i)] = content
	}
	return files
}

// reviewWorkspaceStalledSearch sends a search over a raw connection that never
// reads the response.
func reviewWorkspaceStalledSearch(t *testing.T, address, instance string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	body := `{"query":"x"}`
	fmt.Fprintf(conn, "POST /api/workspace/search HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\n%s: %s\r\nContent-Length: %d\r\n\r\n%s", address, instanceHeader, instance, len(body), body)
	return conn
}

func TestReviewWorkspaceEntryPointsRejectHostileHost(t *testing.T) {
	app, _ := reviewWorkspaceApp(t, map[string]string{"a.txt": "a", "index.html": "<p>x</p>"})
	for _, route := range []string{
		"GET /ahp", "GET /lsp", "GET /dap", "GET /content?path=a.txt", "GET /preview?path=index.html",
		"GET /api/workspace", "GET /api/workspace/git", "GET /api/workspace/files?query=a", "POST /api/workspace/search",
		"GET /api/v2/backends", "GET /api/workspace/terminals/x/status",
	} {
		for _, host := range []string{"attacker.example:9000", "localhost.attacker.example", "0.0.0.0:9000", "127.0.0.1.nip.io:9000", "", "[::ffff:7f00:1]x:1"} {
			method, target, _ := strings.Cut(route, " ")
			rec := reviewWorkspaceRequest(app, method, target, host, app.scope.InstanceID, strings.NewReader(`{"query":"a"}`))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s with Host %q: %d %s", route, host, rec.Code, rec.Body)
			}
		}
	}
}

func TestReviewWorkspaceProtectedRoutesCheckInstance(t *testing.T) {
	app, _ := reviewWorkspaceApp(t, map[string]string{"a.txt": "a", "index.html": "<p>x</p>"})
	chat := chatAPIPath("wingman", "missing")
	routes := []string{
		"GET /lsp", "GET /dap", "GET /content?path=a.txt", "GET /preview?path=index.html",
		"GET /api/workspace/git", "GET /api/workspace/files?query=a", "POST /api/workspace/search",
		"POST /api/workspace/reveal", "GET /api/workspace/terminals/x/status", "GET " + chat + "/reviews",
		"POST /api/workspace/git/init", "GET /api/v2/backends", "GET /%61pi/workspace/files",
	}
	for _, route := range routes {
		method, target, _ := strings.Cut(route, " ")
		for name, instance := range map[string]string{"missing": "", "stale": "previous-instance"} {
			body := `{"path":"../never-revealed"}`
			if rec := reviewWorkspaceRequest(app, method, target, "localhost:9000", instance, strings.NewReader(body)); rec.Code != http.StatusConflict {
				t.Fatalf("%s with %s instance: %d %s", route, name, rec.Code, rec.Body)
			}
		}
		// The header wins over the query, so a stale page cannot borrow a fresh query value.
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		if rec := reviewWorkspaceRequest(app, method, target+sep+"instance="+app.scope.InstanceID, "localhost:9000", "previous-instance", strings.NewReader(`{}`)); rec.Code != http.StatusConflict {
			t.Fatalf("%s with stale header and fresh query: %d", route, rec.Code)
		}
	}
	if rec := reviewWorkspaceRequest(app, "GET", "/api/workspace", "localhost:9000", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("unscoped discovery: %d", rec.Code)
	}
	if rec := reviewWorkspaceRequest(app, "GET", "/api/workspace", "localhost:9000", "previous-instance", nil); rec.Code != http.StatusConflict {
		t.Fatalf("stale discovery: %d", rec.Code)
	}
	if rec := reviewWorkspaceRequest(app, "GET", "/api/workspace/files?query=a&instance="+app.scope.InstanceID, "localhost:9000", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("query instance: %d %s", rec.Code, rec.Body)
	}
	app.Close()
	if rec := reviewWorkspaceRequest(app, "GET", "/api/workspace/files?query=a", "localhost:9000", app.scope.InstanceID, nil); rec.Code != http.StatusConflict {
		t.Fatalf("closed workspace served: %d", rec.Code)
	}
}

func TestReviewWorkspaceAHPNeedsNoInstanceAndRejectsForeignPages(t *testing.T) {
	app, _ := reviewWorkspaceApp(t, nil)
	web := httptest.NewServer(app)
	t.Cleanup(web.Close)
	endpoint := "ws" + strings.TrimPrefix(web.URL, "http")
	host := strings.TrimPrefix(web.URL, "http://")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	dial := func(path, origin string) error {
		headers := http.Header{}
		if origin != "" {
			headers.Set("Origin", origin)
		}
		conn, _, err := websocket.Dial(ctx, endpoint+path, &websocket.DialOptions{HTTPHeader: headers})
		if conn != nil {
			conn.CloseNow()
		}
		return err
	}
	if err := dial("/ahp", ""); err != nil {
		t.Fatalf("native AHP client without an instance: %v", err)
	}
	if err := dial("/ahp", "http://"+host); err != nil {
		t.Fatalf("same-origin AHP: %v", err)
	}
	// Ports are part of the origin: another local server (such as the preview
	// origin) is a different page.
	previewHost := strings.TrimPrefix(app.preview.URL(), "http://")
	for _, origin := range []string{"http://" + previewHost, "http://localhost", "https://" + host + ".attacker.example", "null", "http://" + host + "@attacker.example"} {
		if err := dial("/ahp", origin); err == nil {
			t.Fatalf("AHP accepted origin %s", origin)
		}
		if err := dial("/lsp?instance="+app.scope.InstanceID, origin); err == nil {
			t.Fatalf("LSP accepted origin %s", origin)
		}
	}
	if err := dial("/lsp", "http://"+host); err == nil {
		t.Fatal("LSP accepted a socket without an instance")
	}
}

func TestReviewWorkspaceSearchAbortReleasesSlot(t *testing.T) {
	app, _ := reviewWorkspaceApp(t, reviewWorkspaceLargeSearchFiles())
	web := httptest.NewServer(scopedTestHandler(app))
	t.Cleanup(web.Close)
	for range 3 {
		ctx, cancel := context.WithCancel(t.Context())
		req, _ := http.NewRequestWithContext(ctx, "POST", web.URL+"/api/workspace/search", strings.NewReader(`{"query":"x"}`))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(res.Body).ReadString('\n')
		if err != nil || !strings.Contains(line, `"type":"file"`) {
			t.Fatalf("first event: %q %v", line[:min(len(line), 80)], err)
		}
		cancel()
		res.Body.Close()
		if !reviewWorkspaceWaitSearches(app, 0, 5*time.Second) {
			t.Fatalf("aborted search kept its slot: %d", app.searches.Load())
		}
	}
}

func TestReviewWorkspaceSearchConcurrencyCap(t *testing.T) {
	app, _ := reviewWorkspaceApp(t, reviewWorkspaceLargeSearchFiles())
	web := httptest.NewServer(app)
	t.Cleanup(web.Close)
	address := strings.TrimPrefix(web.URL, "http://")
	var stalled []net.Conn
	for range maxWorkspaceSearches {
		stalled = append(stalled, reviewWorkspaceStalledSearch(t, address, app.scope.InstanceID))
	}
	if !reviewWorkspaceWaitSearches(app, maxWorkspaceSearches, 10*time.Second) {
		t.Fatalf("stalled searches = %d", app.searches.Load())
	}
	search := func() *http.Response {
		req, _ := http.NewRequest("POST", web.URL+"/api/workspace/search", strings.NewReader(`{"query":"nothing-matches-this"}`))
		req.Header.Set(instanceHeader, app.scope.InstanceID)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res
	}
	if res := search(); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("ninth search: %d", res.StatusCode)
	}
	if app.searches.Load() != maxWorkspaceSearches {
		t.Fatalf("rejected search leaked a slot: %d", app.searches.Load())
	}
	for _, conn := range stalled {
		conn.Close()
	}
	if !reviewWorkspaceWaitSearches(app, 0, 10*time.Second) {
		t.Fatalf("closed searches kept slots: %d", app.searches.Load())
	}
	if res := search(); res.StatusCode != http.StatusOK {
		t.Fatalf("search after release: %d", res.StatusCode)
	}
}

// Bug: a search whose client stops reading blocks forever in a response write, so it outlives its one-minute deadline and keeps one of the eight search slots; the deadline should also bound writes.
func TestReviewWorkspaceSearchDeadlineBoundsStalledClient(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the one-minute search deadline")
	}
	app, _ := reviewWorkspaceApp(t, reviewWorkspaceLargeSearchFiles())
	web := httptest.NewServer(app)
	t.Cleanup(web.Close)
	reviewWorkspaceStalledSearch(t, strings.TrimPrefix(web.URL, "http://"), app.scope.InstanceID)
	if !reviewWorkspaceWaitSearches(app, 1, 10*time.Second) {
		t.Fatalf("search did not start: %d", app.searches.Load())
	}
	if !reviewWorkspaceWaitSearches(app, 0, time.Minute+15*time.Second) {
		t.Fatalf("search held its slot past its deadline: %d active", app.searches.Load())
	}
}

func reviewWorkspaceSearchEvents(t *testing.T, app *Server, body string) []map[string]any {
	t.Helper()
	rec := workspaceAPI(t, app, http.MethodPost, "/api/workspace/search", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("search %s: %d %s", body, rec.Code, rec.Body)
	}
	var events []map[string]any
	scanner := bufio.NewScanner(rec.Body)
	scanner.Buffer(nil, 64<<20)
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) == 0 || events[len(events)-1]["type"] != "done" {
		t.Fatalf("search %s ended without summary: %v", body, events)
	}
	return events
}

func reviewWorkspaceSearchPaths(events []map[string]any) []string {
	var paths []string
	for _, event := range events {
		if file, ok := event["file"].(map[string]any); ok {
			paths = append(paths, file["path"].(string))
		}
	}
	return paths
}

func TestReviewWorkspaceSearchStaysInsideWorkspace(t *testing.T) {
	app, root := reviewWorkspaceApp(t, map[string]string{
		"inside.txt":          "needle\n",
		"ignored/hidden.txt":  "needle\n",
		".gitignore":          "ignored/\n*.log\n",
		"trace.log":           "needle\n",
		"node_modules/m.js":   "needle\n",
		".git/config":         "needle\n",
		"nested/.gitignore":   "!keep.log\n",
		"nested/keep.log":     "needle\n",
		"binary.bin":          "needle\x00\n",
		"latin1.txt":          "needle \xff\n",
		"dir with space/a.md": "needle\n",
	})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("needle secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "huge.txt"), append(bytes.Repeat([]byte("a"), maxWorkspaceSearchFileSize), []byte("needle\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := reviewWorkspaceSearchPaths(reviewWorkspaceSearchEvents(t, app, `{"query":"needle"}`))
	got := strings.Join(paths, ",")
	if got != "dir with space/a.md,inside.txt,nested/keep.log" {
		t.Fatalf("searched %s", got)
	}
	for _, body := range []string{`{"query":"needle","include":"../**"}`, `{"query":"needle","include":"/**/secret.txt"}`, `{"query":"secret","include":"**"}`} {
		for _, path := range reviewWorkspaceSearchPaths(reviewWorkspaceSearchEvents(t, app, body)) {
			if strings.Contains(path, "secret") || strings.Contains(path, "linked") {
				t.Fatalf("%s reached %s", body, path)
			}
		}
	}
}

func TestReviewWorkspaceSearchMatchesUnicodeAndReplacements(t *testing.T) {
	app, _ := reviewWorkspaceApp(t, map[string]string{
		"a.txt": "😀 foo_bar foo émoji fooé\r\nFOO(1) foo(22)\n",
	})
	type match struct {
		Line, Column, EndColumn int
		Text, Replacement       string
	}
	search := func(body string) []match {
		events := reviewWorkspaceSearchEvents(t, app, body)
		var matches []match
		for _, event := range events {
			file, ok := event["file"].(map[string]any)
			if !ok {
				continue
			}
			for _, raw := range file["matches"].([]any) {
				m := raw.(map[string]any)
				matches = append(matches, match{int(m["line"].(float64)), int(m["column"].(float64)), int(m["end_column"].(float64)), m["text"].(string), m["replacement"].(string)})
			}
		}
		return matches
	}
	// The emoji is two UTF-16 units; "fooé" is not a whole word.
	whole := search(`{"query":"foo","whole_word":true}`)
	if len(whole) != 3 || whole[0] != (match{1, 12, 15, "foo", ""}) || whole[1].Line != 2 || whole[1].Text != "FOO" {
		t.Fatalf("whole word: %+v", whole)
	}
	sensitive := search(`{"query":"FOO","case_sensitive":true}`)
	if len(sensitive) != 1 || sensitive[0].Line != 2 {
		t.Fatalf("case sensitive: %+v", sensitive)
	}
	replaced := search(`{"query":"foo\\((\\d+)\\)","regex":true,"replacement":"bar[$1]"}`)
	if len(replaced) != 2 || replaced[0].Replacement != "bar[1]" || replaced[1].Replacement != "bar[22]" || replaced[1].Column != 8 {
		t.Fatalf("regex replacement: %+v", replaced)
	}
	literal := search(`{"query":"foo(1)","replacement":"$1"}`)
	if len(literal) != 1 || literal[0].Replacement != "$1" {
		t.Fatalf("literal replacement: %+v", literal)
	}
	limited := reviewWorkspaceSearchEvents(t, app, `{"query":"o","limit":2}`)
	if done := limited[len(limited)-1]; done["matches"] != float64(2) || done["truncated"] != true {
		t.Fatalf("limit: %v", done)
	}
}

func TestReviewWorkspaceFilePickerHidesAndCaps(t *testing.T) {
	files := map[string]string{".env": "x", ".hidden/a.go": "x", "node_modules/a.go": "x", "build/a.go": "x"}
	for i := range 60 {
		files[fmt.Sprintf("src/file%02d.go", i)] = "x"
	}
	app, root := reviewWorkspaceApp(t, files)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{"": 50, "FILE0": 10, "a.go": 0, "outside": 0, "env": 0} {
		rec := workspaceAPI(t, app, http.MethodGet, "/api/workspace/files?query="+url.QueryEscape(query), "")
		var hits []fileSearchHit
		if err := json.Unmarshal(rec.Body.Bytes(), &hits); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%q: %d %s", query, rec.Code, rec.Body)
		}
		if len(hits) != want {
			t.Fatalf("%q: %d hits %v", query, len(hits), hits)
		}
		for _, hit := range hits {
			if strings.HasPrefix(hit.Path, ".") || strings.Contains(hit.Path, "node_modules") || strings.HasPrefix(hit.Path, "linked/") {
				t.Fatalf("%q listed %s", query, hit.Path)
			}
		}
	}
}

func TestReviewWorkspaceRevealRejectsEscapes(t *testing.T) {
	app, root := reviewWorkspaceApp(t, nil)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	// Every target is invalid or missing; a valid one would open the file manager.
	for body, want := range map[string]int{
		`{"path":""}`:                           http.StatusBadRequest,
		`{"path":"."}`:                          http.StatusBadRequest,
		`{"path":"/"}`:                          http.StatusBadRequest,
		`{"path":".."}`:                         http.StatusBadRequest,
		`{"path":"a/../../x"}`:                  http.StatusBadRequest,
		`{"path":"` + outside + `/secret"}`:     http.StatusBadRequest,
		`{"path":"escape/secret"}`:              http.StatusNotFound,
		`{"path":"escape/../../x"}`:             http.StatusBadRequest,
		`{"path":"..\\..\\secret"}`:             http.StatusNotFound,
		`{"path":"%2e%2e/secret"}`:              http.StatusNotFound,
		`{"path":"missing\u0000"}`:              http.StatusNotFound,
		`{"path":"missing","extra":true}`:       http.StatusBadRequest,
		`{"path":["escape"]}`:                   http.StatusBadRequest,
		strings.Repeat(" ", 70<<10) + `{"p":1}`: http.StatusBadRequest,
	} {
		if rec := workspaceAPI(t, app, http.MethodPost, "/api/workspace/reveal", body); rec.Code != want {
			t.Fatalf("reveal %.60s: %d, want %d", body, rec.Code, want)
		}
	}
}

func TestReviewWorkspaceChatRoutesResolveOnlyKnownChats(t *testing.T) {
	app, _ := reviewWorkspaceApp(t, nil)
	web := httptest.NewServer(scopedTestHandler(app))
	t.Cleanup(web.Close)
	b := app.backends["wingman"].runtime
	record := b.createSession(Command{ID: "review-create", Type: "create"})
	if record.status != 0 {
		t.Fatal(record.message)
	}
	id := record.receipt.Ref.SessionID
	get := func(path string) int {
		t.Helper()
		res, err := http.Get(web.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	if code := get(chatAPIPath("wingman", id) + "/tasks"); code != http.StatusOK {
		t.Fatalf("known chat: %d", code)
	}
	session := "/api/v2/chats/" + url.PathEscape(canonicalAHPURI("wingman", id))
	for path, want := range map[string]int{
		chatAPIPath("wingman", "missing") + "/tasks":          http.StatusNotFound,
		chatAPIPath("review-unknown-backend", "x") + "/tasks": http.StatusNotFound,
		session + "/tasks": http.StatusNotFound,
		"/api/v2/chats/" + url.PathEscape("ahp-chat:/wingman/!!/!!") + "/tasks":                      http.StatusNotFound,
		"/api/v2/chats/" + url.PathEscape("file:///etc/passwd") + "/tasks":                           http.StatusNotFound,
		"/api/v2/chats/" + url.PathEscape("ahp-chat:/wingman/"+strings.Repeat("A", 4096)) + "/tasks": http.StatusNotFound,
	} {
		if code := get(path); code != want {
			t.Fatalf("%.120s: %d, want %d", path, code, want)
		}
	}
	b.mu.Lock()
	_, kept := b.sessions["missing"]
	b.mu.Unlock()
	app.backendsMu.Lock()
	_, started := app.backends["review-unknown-backend"]
	app.backendsMu.Unlock()
	if kept || started {
		t.Fatalf("lookups left state behind: controller=%v backend=%v", kept, started)
	}
	if code := get(chatAPIPath("wingman", id) + "/tasks"); code != http.StatusOK {
		t.Fatalf("known chat after probes: %d", code)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if code := get(chatAPIPath("wingman", id) + "/reviews"); code != http.StatusOK {
				t.Errorf("concurrent reviews: %d", code)
			}
		})
	}
	wg.Wait()
	reviewWorkspaceDeleteChat(t, app, id)
	if code := get(chatAPIPath("wingman", id) + "/tasks"); code != http.StatusNotFound {
		t.Fatalf("deleted chat: %d", code)
	}
}

func reviewWorkspaceDeleteChat(t *testing.T, app *Server, id string) {
	t.Helper()
	_, ch, err := app.ahpHost().ensureSession(ahpChatURI(canonicalAHPURI("wingman", id)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.command(Command{ID: "review-delete", Epoch: ch.controller.epoch, Type: "delete"}); err != nil {
		t.Fatal(err)
	}
}

func reviewWorkspaceRawAHP(t *testing.T, ctx context.Context, endpoint, clientID string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	if clientID == "" {
		return conn
	}
	if err := wsjson.Write(ctx, conn, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"channel": "ahp-root://", "clientId": clientID, "protocolVersions": []string{"1.1.0"}, "initialSubscriptions": []string{"ahp-root://"}}}); err != nil {
		t.Fatal(err)
	}
	var reply remoteAHPFrame
	if err := wsjson.Read(ctx, conn, &reply); err != nil || reply.ID != 1 || reply.Error != nil {
		t.Fatalf("initialize: %+v %v", reply, err)
	}
	return conn
}

func TestReviewWorkspaceAHPDropsSlowConsumerWithoutBlockingHost(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	reviewWorkspaceRawAHP(t, ctx, endpoint, "never-reads")
	healthy := ahpTestClient(t, endpoint, "")
	if _, err := healthy.Initialize(ctx, "healthy", []string{"1.0.0"}, nil); err != nil {
		t.Fatal(err)
	}
	h := s.ahpHost()
	h.mu.Lock()
	connected := len(h.clients)
	h.mu.Unlock()
	if connected != 2 {
		t.Fatalf("clients = %d", connected)
	}
	text := strings.Repeat("x", 16<<10)
	start := time.Now()
	for range 4000 {
		s.send(Frame{Type: EvtWorkspaceChanged, Text: text})
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("publishing waited for a slow consumer: %v", elapsed)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		h.mu.Lock()
		remaining := []string{}
		for c := range h.clients {
			remaining = append(remaining, c.id)
		}
		h.mu.Unlock()
		if len(remaining) == 1 && remaining[0] == "healthy" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slow consumer was not dropped: %v", remaining)
		}
		time.Sleep(20 * time.Millisecond)
	}
	request, done := context.WithTimeout(t.Context(), 5*time.Second)
	defer done()
	if err := healthy.Request(request, "listSessions", map[string]any{"channel": "ahp-root://"}, nil); err != nil {
		t.Fatalf("healthy client after slow consumer: %v", err)
	}
}

func TestReviewWorkspaceAHPMalformedFramesKeepConnection(t *testing.T) {
	_, _, _, endpoint := ahpTestServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn := reviewWorkspaceRawAHP(t, ctx, endpoint, "")
	send := func(frame string) {
		t.Helper()
		if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]any {
		t.Helper()
		var frame map[string]any
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}
	code := func(frame map[string]any) float64 {
		e, _ := frame["error"].(map[string]any)
		c, _ := e["code"].(float64)
		return c
	}
	for _, check := range []struct {
		frame string
		want  float64
	}{
		{`{`, -32700},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, -32700},
		{`{"jsonrpc":"1.0","id":2,"method":"ping"}`, -32600},
		{`{"jsonrpc":"2.0","id":3}`, -32600},
		{`{"jsonrpc":"2.0","id":4,"method":"listSessions"}`, -32602},
		{`{"jsonrpc":"2.0","id":5,"method":"dispatchAction"}`, -32602},
		{`{"jsonrpc":"2.0","id":6,"method":"initialize","params":{"channel":"ahp-root://","clientId":"wingman-host","protocolVersions":["1.1.0"]}}`, -32602},
		{`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"channel":"other","clientId":"x","protocolVersions":["1.1.0"]}}`, -32602},
	} {
		send(check.frame)
		if got := code(read()); got != check.want {
			t.Fatalf("%s: error %v, want %v", check.frame, got, check.want)
		}
	}
	send(`{"jsonrpc":"2.0","method":"dispatchAction","params":{"channel":"ahp-root://"}}`)
	send(`{"jsonrpc":"2.0","method":"initialize","params":{}}`)
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{0xff, 0x00}); err != nil {
		t.Fatal(err)
	}
	read() // the binary frame is not JSON
	send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	if frame := read(); frame["id"] != "p" || frame["error"] != nil {
		t.Fatalf("ping after malformed frames: %v", frame)
	}
}

func TestReviewWorkspaceRelayKeepsOriginAndInstanceChecks(t *testing.T) {
	app, _ := reviewWorkspaceApp(t, map[string]string{"a.txt": "a"})
	relay := remote.NewRelay("test-token")
	web := httptest.NewServer(relay)
	t.Cleanup(func() { relay.Close(); web.Close() })
	credentials := remote.NewCredentials()
	connectTestRemote(t, app, web.URL, credentials)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	pairTestRemote(t, client, web.URL, credentials)
	get := func(path, instance string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", web.URL+path, nil)
		if instance != "" {
			req.Header.Set(instanceHeader, instance)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	if code := get("/api/workspace/files?query=a", app.scope.InstanceID); code != http.StatusOK {
		t.Fatalf("paired request: %d", code)
	}
	if code := get("/api/workspace/files?query=a", ""); code != http.StatusConflict {
		t.Fatalf("relay request without instance: %d", code)
	}
	if code := get("/content?path=a.txt", "stale"); code != http.StatusConflict {
		t.Fatalf("relay content with stale instance: %d", code)
	}
	relayURL, _ := url.Parse(web.URL)
	sibling := "http://" + relayURL.Hostname() + ":1"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	dial := func(path, origin string) error {
		headers := http.Header{"Origin": {origin}}
		for _, cookie := range jar.Cookies(relayURL) {
			headers.Add("Cookie", cookie.String())
		}
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(web.URL, "http")+path, &websocket.DialOptions{HTTPHeader: headers})
		if conn != nil {
			conn.CloseNow()
		}
		return err
	}
	if err := dial("/ahp", web.URL); err != nil {
		t.Fatalf("same-origin relay AHP: %v", err)
	}
	if err := dial("/lsp?instance="+app.scope.InstanceID, web.URL); err != nil {
		t.Fatalf("same-origin relay LSP: %v", err)
	}
	// A same-site page carries the pairing cookie but not the relay's origin.
	for _, path := range []string{"/ahp", "/lsp?instance=" + app.scope.InstanceID, "/dap?instance=" + app.scope.InstanceID} {
		if err := dial(path, sibling); err == nil {
			t.Fatalf("relay %s accepted sibling origin", path)
		}
	}
}

// Bug: a search the remote browser aborts (the search panel aborts on every new query) stays blocked writing into the half-closed relay stream until yamux's 5-minute close timeout, so eight aborted remote searches lock search out with 429; an abort should release the slot.
func TestReviewWorkspaceRelayAbortedSearchReleasesSlot(t *testing.T) {
	app, _ := reviewWorkspaceApp(t, reviewWorkspaceLargeSearchFiles())
	relay := remote.NewRelay("test-token")
	web := httptest.NewServer(relay)
	t.Cleanup(func() { relay.Close(); web.Close() })
	credentials := remote.NewCredentials()
	connectTestRemote(t, app, web.URL, credentials)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	pairTestRemote(t, client, web.URL, credentials)
	search := func(ctx context.Context, query string) *http.Response {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "POST", web.URL+"/api/workspace/search", strings.NewReader(`{"query":"`+query+`"}`))
		req.Header.Set(instanceHeader, app.scope.InstanceID)
		req.Header.Set("Origin", web.URL)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for range maxWorkspaceSearches {
		ctx, cancel := context.WithCancel(t.Context())
		res := search(ctx, "x")
		line, err := bufio.NewReader(res.Body).ReadString('\n')
		if err != nil || !strings.Contains(line, `"type":"file"`) {
			t.Fatalf("first relayed event: %v", err)
		}
		cancel()
		res.Body.Close()
	}
	reviewWorkspaceWaitSearches(app, 0, 5*time.Second)
	res := search(t.Context(), "nothing-matches-this")
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("search after aborted remote searches: %d, %d slots still held", res.StatusCode, app.searches.Load())
	}
}
