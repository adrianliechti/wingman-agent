package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// temporaryTestDir gives a test its own folder of temporary chats.
func temporaryTestDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, tmp)
	}
	return temporaryRoot()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestTemporaryChatIsRemovedOnceNoWindowShowsIt(t *testing.T) {
	// The start page opens a temporary chat on the running host, or starts the
	// host with it as its first project.
	for _, first := range []bool{false, true} {
		t.Run(map[bool]string{false: "another project", true: "first project"}[first], func(t *testing.T) {
			testenv.WingmanHome(t)
			protocolTestGateway(t)
			root := temporaryTestDir(t)
			chat, err := NewTemporaryFolder()
			if err != nil {
				t.Fatal(err)
			}
			dir := chat
			if !first {
				dir = canonicalPath(t.TempDir())
			}
			s, err := New(t.Context(), dir, &ServerOptions{NoBrowser: true, Projects: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			h := s.Project.host

			window := windowOpen(t, s, chat)
			if filepath.Dir(chat) != root || first != (window.InstanceID == s.InstanceID()) {
				t.Fatalf("temporary chat %q below %q in window %+v", chat, root, window)
			}
			if _, err := claimTemporary(chat); err == nil {
				t.Fatal("another Wingman could claim an open temporary chat")
			}
			var shown struct {
				Name      string `json:"name"`
				Temporary bool   `json:"temporary"`
			}
			if recorder := windowRequest(t, s, http.MethodGet, "/api/workspace", window.InstanceID, ""); json.Unmarshal(recorder.Body.Bytes(), &shown) != nil || !shown.Temporary || shown.Name != temporaryName {
				t.Fatalf("temporary window: %s", recorder.Body.String())
			}
			if info := projectInfo(chat); info.DisplayName != temporaryName {
				t.Fatalf("temporary project named %q", info.DisplayName)
			}

			state := code.ProjectStateDir(chat)
			if !exists(state) {
				t.Fatalf("the project kept no state at %s", state)
			}
			saved := sessionAlias{Resource: canonicalSessionURI(code.BuiltinAgentName, "s1", chat), Project: chat, Backend: code.BuiltinAgentName, Session: "s1"}
			if err := writeAlias(saved); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			h.rememberAliasLocked(saved)
			h.mu.Unlock()
			aliases, _ := aliasDirectory(chat)

			web := httptest.NewServer(s)
			t.Cleanup(web.Close)
			client := protocolTestClient(t, "ws"+strings.TrimPrefix(web.URL, "http")+"/ahp?instance="+window.InstanceID, "window")
			h.closeIdle(0)
			if !exists(chat) {
				t.Fatal("a shown temporary chat was removed")
			}
			_ = client.Shutdown(t.Context())
			temporary := s.Project
			if !first {
				temporary = s.windowOf(window.InstanceID).Project
			}
			waitProtocol(t, func() bool {
				h.mu.Lock()
				defer h.mu.Unlock()
				return temporary.windowRequests == 0
			})
			h.closeIdle(0)
			if first {
				// The host's first project lasts as long as the host.
				if !exists(chat) {
					t.Fatal("the host's first project was removed while the host runs")
				}
				if recorder := windowRequest(t, s, http.MethodGet, "/", "", ""); recorder.Code != http.StatusOK {
					t.Fatalf("pages no longer load: %d", recorder.Code)
				}
				s.Close()
			}

			for _, path := range []string{chat, chat + ".lock", state, aliases} {
				if exists(path) {
					t.Errorf("%s outlived its temporary chat", path)
				}
			}
			h.mu.Lock()
			_, remembered := h.aliases[saved.Resource]
			h.mu.Unlock()
			if remembered {
				t.Fatal("the host still lists the temporary chat's session")
			}
			if !first && windowRequest(t, s, http.MethodGet, "/api/workspace", s.InstanceID(), "").Code != http.StatusOK {
				t.Fatal("the host's first project closed with the temporary chat")
			}
		})
	}
}

func TestTemporaryChatsAreRemovedWhenTheHostStops(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	temporaryTestDir(t)
	h, err := NewHost(t.Context(), &HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := NewTemporaryFolder()
	if err != nil {
		h.Close()
		t.Fatal(err)
	}
	if _, err := h.projectAt(chat); err != nil {
		h.Close()
		t.Fatal(err)
	}
	h.Close()
	if exists(chat) || exists(chat+".lock") || exists(code.ProjectStateDir(chat)) {
		t.Fatal("a temporary chat outlived its host")
	}
}

// A temporary chat waits for a command that runs, not for a shell at its prompt.
func TestTemporaryChatWaitsForRunningWorkOnly(t *testing.T) {
	if runtime.GOOS == "windows" || !terminal.Supported() {
		t.Skip("uses /bin/sh")
	}
	testenv.UserHome(t)
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	temporaryTestDir(t)
	h, err := NewHost(t.Context(), &HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	chat, err := NewTemporaryFolder()
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.projectAt(chat)
	if err != nil {
		t.Fatal(err)
	}

	command, err := p.terminals.CreateCommand(terminal.CommandSpec{Path: "/bin/sh", Args: []string{"-c", "sleep 30"}}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	h.closeIdle(0)
	if !exists(chat) {
		t.Fatal("a temporary chat was removed while a command ran")
	}
	_ = command.Close()

	if _, err := p.terminals.Create("", 80, 24); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); p.working() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if !p.busy() {
		t.Fatal("a shell at its prompt does not keep a project open")
	}
	h.closeIdle(0)
	if exists(chat) {
		t.Fatal("a shell at its prompt kept a temporary chat")
	}
}

// A temporary chat inside a repository keeps its state with the
// repository's, which outlives the chat.
func TestTemporaryChatKeepsTheStateOfARepositoryAroundIt(t *testing.T) {
	testenv.WingmanHome(t)
	repo := canonicalPath(t.TempDir())
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, repo)
	}
	chat, err := NewTemporaryFolder()
	if err != nil {
		t.Fatal(err)
	}
	state := code.ProjectStateDir(repo)
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if code.ProjectStateDir(chat) != state {
		t.Fatal("the chat does not share the repository's state")
	}
	remove, err := claimTemporary(chat)
	if err != nil {
		t.Fatal(err)
	}
	remove()
	if exists(chat) || !exists(state) {
		t.Fatalf("chat removed %v, the repository's state kept %v", !exists(chat), exists(state))
	}
}

// Temporary chats are not made in a folder other users could write to.
func TestTemporaryChatsNeedAPrivateRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps a temporary directory per user")
	}
	root := temporaryTestDir(t)
	if err := os.Mkdir(root, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	if dir, err := NewTemporaryFolder(); err == nil {
		t.Fatalf("made %s in a shared root", dir)
	}
}

func TestSweepRemovesTemporaryChatsNoWingmanHasOpen(t *testing.T) {
	testenv.WingmanHome(t)
	root := temporaryTestDir(t)
	folder := func(age time.Duration) string {
		dir, err := NewTemporaryFolder()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(code.ProjectStateDir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		then := time.Now().Add(-age)
		if err := os.Chtimes(dir, then, then); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	stale, fresh, open := folder(time.Hour), folder(0), folder(time.Hour)
	remove, err := claimTemporary(open)
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(root, old, old); err != nil {
		t.Fatal(err)
	}

	sweepTemporary()

	if exists(stale) || exists(stale+".lock") || exists(code.ProjectStateDir(stale)) {
		t.Fatal("the sweep left a temporary chat no Wingman has open")
	}
	if !exists(fresh) || !exists(open) || !exists(code.ProjectStateDir(open)) || !exists(other) {
		t.Fatal("the sweep removed a temporary chat that is new or open, or what is not a chat")
	}
}

// A session deleted while its flag waits to be saved, as a temporary chat's
// are as it closes, is not saved again.
func TestFlagDoesNotSaveASessionDeletedMeanwhile(t *testing.T) {
	testenv.WingmanHome(t)
	protocolTestGateway(t)
	h, err := NewHost(t.Context(), &HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	dir := canonicalPath(t.TempDir())
	saved := sessionAlias{Resource: "ahp-session:/flagged", Project: dir, Backend: code.BuiltinAgentName, Session: "s1"}
	h.mu.Lock()
	h.rememberAliasLocked(saved)
	h.mu.Unlock()

	h.projectMu.Lock()
	flagged := make(chan bool)
	go func() {
		flagged <- h.flagUnopened(types.DispatchActionParams{Channel: saved.Resource, Action: types.StateAction{Value: &types.SessionIsReadChangedAction{Type: types.ActionTypeSessionIsReadChanged, IsRead: true}}}, types.ActionOrigin{})
	}()
	waitProtocol(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.creating[saved.Resource] != nil
	})
	time.Sleep(50 * time.Millisecond) // past its look at what the host saved
	h.mu.Lock()
	h.forgetSessionLocked(saved.Resource)
	h.mu.Unlock()
	h.projectMu.Unlock()

	if <-flagged {
		t.Fatal("the flag of a deleted session was saved")
	}
	h.mu.Lock()
	_, remembered := h.aliases[saved.Resource]
	h.mu.Unlock()
	if remembered {
		t.Fatal("a deleted session came back")
	}
}
