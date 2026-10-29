package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/remote"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func connectTestRemote(t *testing.T, s *Server, relayURL string, credentials remote.Credentials) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(s.ctx)
	ready, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.background.Go(func() {
		defer close(done)
		err := remote.Serve(ctx, remote.ClientOptions{Relay: relayURL, Token: "test-token", Credentials: credentials, OnStatus: func(connected bool, _ error) {
			if connected {
				once.Do(func() { close(ready) })
			}
		}}, s)
		if err != nil && ctx.Err() == nil {
			t.Errorf("remote transport: %v", err)
		}
	})
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("remote transport did not stop")
		}
	}
	t.Cleanup(stop)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("remote transport did not connect")
	}
	return stop
}

func pairTestRemote(t *testing.T, client *http.Client, relayURL string, credentials remote.Credentials) {
	t.Helper()
	body, _ := json.Marshal(credentials)
	response, err := client.Post(relayURL+"/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("pairing: %d", response.StatusCode)
	}
}

type remoteAHPFrame struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Params json.RawMessage `json:"params"`
	Error  *ahpRPCError    `json:"error"`
	Method string          `json:"method"`
}

func remoteAHP(t *testing.T, client *http.Client, relayURL string) *websocket.Conn {
	t.Helper()
	origin, _ := url.Parse(relayURL)
	headers := http.Header{"Origin": {relayURL}}
	for _, cookie := range client.Jar.Cookies(origin) {
		headers.Add("Cookie", cookie.String())
	}
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(relayURL, "http")+"/ahp", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func remoteRPC(t *testing.T, conn *websocket.Conn, method string, params any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}); err != nil {
		t.Fatal(err)
	}
	for {
		var frame remoteAHPFrame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.ID == 1 {
			if frame.Error != nil {
				t.Fatal(frame.Error)
			}
			return frame.Result
		}
	}
}

func remoteInitialize(t *testing.T, conn *websocket.Conn) string {
	result := remoteRPC(t, conn, "initialize", map[string]any{"channel": "ahp-root://", "clientId": "remote-test", "protocolVersions": []string{"1.1.0"}, "initialSubscriptions": []string{"ahp-root://"}})
	var initialized struct {
		Snapshots []struct {
			State struct {
				Meta struct {
					Wingman WorkspaceScope `json:"wingman"`
				} `json:"_meta"`
			} `json:"state"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(result, &initialized); err != nil {
		t.Fatal(err)
	}
	return initialized.Snapshots[0].State.Meta.Wingman.InstanceID
}

func remoteCommand(t *testing.T, client *http.Client, relayURL, instance string, command Command) (int, string) {
	t.Helper()
	conn := remoteAHP(t, client, relayURL)
	defer conn.CloseNow()
	// Like the browser, bind the initialized workspace before replaying any command.
	if remoteInitialize(t, conn) != instance {
		return 409, "workspace replaced"
	}
	resolved := remoteRPC(t, conn, "x-wingman/resolveSession", map[string]any{"channel": "ahp-root://", "backend": "one", "session": "saved"})
	var resource struct {
		Resource string `json:"resource"`
	}
	_ = json.Unmarshal(resolved, &resource)
	snapshot := remoteRPC(t, conn, "subscribe", map[string]string{"channel": resource.Resource})
	var session struct {
		Snapshot struct {
			State struct {
				DefaultChat string `json:"defaultChat"`
			} `json:"state"`
		} `json:"snapshot"`
	}
	_ = json.Unmarshal(snapshot, &session)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	params := map[string]any{"channel": session.Snapshot.State.DefaultChat, "clientSeq": 1, "action": map[string]any{"type": "chat/pendingMessageSet", "id": command.ID, "kind": "queued", "message": map[string]any{"text": command.Text, "origin": map[string]string{"kind": "user"}}}}
	if err := wsjson.Write(ctx, conn, map[string]any{"jsonrpc": "2.0", "method": "dispatchAction", "params": params}); err != nil {
		t.Fatal(err)
	}
	for {
		var frame remoteAHPFrame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Method != "action" {
			continue
		}
		var action struct {
			Origin *struct {
				ClientID string `json:"clientId"`
			} `json:"origin"`
			Rejection *string `json:"rejectionReason"`
		}
		_ = json.Unmarshal(frame.Params, &action)
		if action.Origin != nil && action.Origin.ClientID == "remote-test" {
			if action.Rejection != nil {
				return 409, *action.Rejection
			}
			return 200, string(frame.Params)
		}
	}
}

func remoteSnapshot(t *testing.T, client *http.Client, relayURL string, c *sessionController) json.RawMessage {
	t.Helper()
	conn := remoteAHP(t, client, relayURL)
	defer conn.CloseNow()
	remoteInitialize(t, conn)
	return remoteRPC(t, conn, "subscribe", map[string]string{"channel": ahpChatURI(canonicalAHPURI(c.ref.BackendID, c.ref.SessionID))})
}

func TestRemoteUsesExistingReceiptsAndRecoversLiveSessionAfterTunnelRestart(t *testing.T) {
	s, b, a := newProtocolServer(t)
	a.gate = make(chan struct{})
	c := b.session("saved")
	c.load()
	relay := remote.NewRelay("test-token")
	web := httptest.NewServer(relay)
	t.Cleanup(func() { relay.Close(); web.Close() })
	credentials := remote.NewCredentials()
	stop := connectTestRemote(t, s, web.URL, credentials)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	pairTestRemote(t, client, web.URL, credentials)
	command := Command{ID: "remote-input", Type: "send", Epoch: c.epoch, Text: "hello"}
	if status, _ := remoteCommand(t, client, web.URL, "old-instance", command); status != 409 {
		t.Fatalf("old instance accepted: %d", status)
	}
	status, first := remoteCommand(t, client, web.URL, s.scope.InstanceID, command)
	if status != 200 {
		t.Fatalf("send: %d %s", status, first)
	}
	status, duplicate := remoteCommand(t, client, web.URL, s.scope.InstanceID, command)
	if status != 200 || first != duplicate {
		t.Fatalf("receipt changed on retry: %d %s", status, duplicate)
	}
	waitProtocol(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.entries) > 1 && c.entries[len(c.entries)-1].Content == "live prefix"
	})
	before := remoteSnapshot(t, client, web.URL, c)
	stop()
	if s.ctx.Err() != nil {
		t.Fatal("losing the tunnel closed the workspace")
	}
	connectTestRemote(t, s, web.URL, credentials)
	after := remoteSnapshot(t, client, web.URL, c)
	if !strings.Contains(string(after), "live prefix") || !bytes.Equal(before, after) {
		t.Fatalf("reconnect did not recover the running session: %+v", after)
	}
	if a.sent.Load() != 1 {
		t.Fatalf("model invoked %d times", a.sent.Load())
	}
	close(a.gate)
}

func TestRemotePairingSwitchCannotRedirectAnOldWorkspaceCommand(t *testing.T) {
	first, _, _ := newProtocolServer(t)
	second, b, a := newProtocolServer(t)
	second.scope = WorkspaceScope{WorkspaceID: "second-workspace", InstanceID: "second-instance"}
	c := b.session("saved")
	c.load()
	relay := remote.NewRelay("test-token")
	web := httptest.NewServer(relay)
	t.Cleanup(func() { relay.Close(); web.Close() })
	firstCredentials, secondCredentials := remote.NewCredentials(), remote.NewCredentials()
	connectTestRemote(t, first, web.URL, firstCredentials)
	connectTestRemote(t, second, web.URL, secondCredentials)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	pairTestRemote(t, client, web.URL, firstCredentials)
	pairTestRemote(t, client, web.URL, secondCredentials)
	status, _ := remoteCommand(t, client, web.URL, first.scope.InstanceID, Command{ID: "old-page", Epoch: c.epoch, Type: "send", Text: "must not run here"})
	if status != 409 || a.sent.Load() != 0 {
		t.Fatalf("old browser mutated newly paired workspace: status=%d sends=%d", status, a.sent.Load())
	}
	first.Close()
	response, err := client.Get(web.URL + "/api/v2/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("closing another workspace broke this tunnel: %d", response.StatusCode)
	}
}
