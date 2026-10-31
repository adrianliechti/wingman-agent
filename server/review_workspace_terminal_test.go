package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/dap"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func reviewWorkspaceTerminalServer(t *testing.T) (*Server, string) {
	t.Helper()
	if !terminal.Supported() {
		t.Skip("no PTY support")
	}
	s, _, _, endpoint := ahpTestServer(t)
	s.terminals = terminal.NewManager(t.TempDir())
	// Closing sessions (not the manager) first releases any PTY write a test left blocked.
	t.Cleanup(func() {
		for _, session := range s.terminals.List() {
			_ = session.Close()
		}
	})
	return s, endpoint
}

type reviewWorkspaceTerminalView struct {
	content string
	owner   string
	title   string
	exit    *types.TerminalExitedLifecycleState
	session *terminal.Session
}

func reviewWorkspaceTerminalLookup(s *Server, uri string) (reviewWorkspaceTerminalView, bool) {
	h := s.ahpHost()
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.terminals[uri]
	if a == nil {
		return reviewWorkspaceTerminalView{}, false
	}
	view := reviewWorkspaceTerminalView{content: terminalContent(&a.state), title: a.state.Title, session: a.session}
	if claim, ok := a.state.Claim.Value.(*types.TerminalClientClaim); ok {
		view.owner = claim.ClientId
	}
	if exit, ok := a.state.Lifecycle.Value.(*types.TerminalExitedLifecycleState); ok {
		copied := *exit
		view.exit = &copied
	}
	return view, true
}

func reviewWorkspaceTerminalEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func reviewWorkspaceTerminalClaim(clientID string) types.StateAction {
	return types.StateAction{Value: &types.TerminalClaimedAction{Type: types.ActionTypeTerminalClaimed, Claim: types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient, ClientId: clientID}}}}
}

func reviewWorkspaceTerminalInput(data string) types.StateAction {
	return types.StateAction{Value: &types.TerminalInputAction{Type: types.ActionTypeTerminalInput, Data: data}}
}

// reviewWorkspaceTerminalDispatch sends an action and returns its rejection
// reason, or nil when the host accepted it.
func reviewWorkspaceTerminalDispatch(t *testing.T, client *ahp.Client, sub *ahp.Subscription, clientID, uri string, seq int64, action types.StateAction) *string {
	t.Helper()
	if err := client.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: uri, ClientSeq: seq, Action: action}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed")
			}
			if action, ok := e.(ahp.SubscriptionEventAction); ok && action.Envelope.Origin != nil && action.Envelope.Origin.ClientId == clientID && action.Envelope.Origin.ClientSeq == seq {
				return action.Envelope.RejectionReason
			}
		case <-deadline:
			t.Fatalf("no acknowledgement for %s #%d", clientID, seq)
		}
	}
}

func reviewWorkspaceTerminalClientGone(s *Server, id string) func() bool {
	return func() bool {
		h := s.ahpHost()
		h.mu.Lock()
		defer h.mu.Unlock()
		for c := range h.clients {
			if c.id == id {
				return false
			}
		}
		return true
	}
}

// Bug: disposeTerminal waits for the terminal's opMu, which an input write blocked by a process that never reads stdin holds forever; dispose must close the PTY instead of hanging behind the write.
func TestReviewWorkspaceTerminalDisposeNotBlockedByStalledInput(t *testing.T) {
	s, endpoint := reviewWorkspaceTerminalServer(t)
	client := ahpTestClient(t, endpoint, "owner")
	uri := "ahp-terminal:/stalled-input"
	createTestTerminal(t, client, uri, "owner")
	_, sub, err := subscribeTerminal(t.Context(), client, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	view, _ := reviewWorkspaceTerminalLookup(s, uri)
	if reason := reviewWorkspaceTerminalDispatch(t, client, sub, "owner", uri, 1, reviewWorkspaceTerminalInput("sleep 30\n")); reason != nil {
		t.Fatal(*reason)
	}
	reviewWorkspaceTerminalEventually(t, "sleep in the foreground", view.session.HasForegroundProcess)
	// Complete lines that sleep never reads fill the PTY input queue.
	paste := types.DispatchActionParams{Channel: uri, ClientSeq: 2, Action: reviewWorkspaceTerminalInput(strings.Repeat("x\n", 256<<10))}
	if err := client.Notify(t.Context(), "dispatchAction", paste); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := client.Request(ctx, "disposeTerminal", types.DisposeTerminalParams{Channel: uri}, nil); err != nil {
		t.Fatalf("dispose hung behind a stalled input write: %v", err)
	}
	if s.terminals.Get(view.session.ID()) != nil {
		t.Fatal("disposed terminal still runs")
	}
}

// Bug: when the process exits while the host's output subscriber is a whole scrollback behind, the closed stream is taken for a clean exit and the end of the output is lost; the host must recover the session snapshot before publishing the exit.
func TestReviewWorkspaceTerminalExitWhileBehindKeepsFinalOutput(t *testing.T) {
	s, endpoint := reviewWorkspaceTerminalServer(t)
	client := ahpTestClient(t, endpoint, "observer")
	gate := filepath.Join(t.TempDir(), "go")
	script := `while [ ! -e "$GATE" ]; do sleep 0.01; done; head -c 600000 /dev/zero | tr '\0' a; printf '\nfinal-marker\n'; exit 3`
	session, err := s.terminals.CreateCommand(terminal.CommandSpec{Path: "/bin/sh", Args: []string{"-c", script}, Env: map[string]*string{"GATE": &gate}}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	uri := "ahp-terminal:/wingman/" + session.ID()
	reviewWorkspaceTerminalEventually(t, "terminal attached", func() bool {
		_, ok := reviewWorkspaceTerminalLookup(s, uri)
		return ok
	})
	h := s.ahpHost()
	h.mu.Lock() // a busy host: the terminal's output pump cannot publish
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(10 * time.Second):
		h.mu.Unlock()
		t.Fatal("command did not exit")
	}
	h.mu.Unlock()
	reviewWorkspaceTerminalEventually(t, "exit published", func() bool {
		view, _ := reviewWorkspaceTerminalLookup(s, uri)
		return view.exit != nil
	})
	state, sub, err := subscribeTerminal(t.Context(), client, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	exit, ok := state.Lifecycle.Value.(*types.TerminalExitedLifecycleState)
	if !ok || exit.ExitCode == nil || *exit.ExitCode != 3 {
		t.Fatalf("exit state: %+v", state.Lifecycle)
	}
	content := terminalContent(state)
	if !strings.Contains(content, "final-marker") {
		t.Fatalf("exited terminal lost the end of its output: %d bytes retained, tail %q", len(content), content[max(0, len(content)-40):])
	}
}

// Bug: an unterminated OSC (e.g. printing a binary file) makes the filter withhold all later output until a BEL or ST arrives, then drop it as part of the OSC; like xterm.js, ESC must end an OSC string so later output still flows.
func TestReviewWorkspaceTerminalFilterEndsUnterminatedOSC(t *testing.T) {
	var binary terminalOutputFilter
	got := binary.feed([]byte("cat a.bin\r\n\x1b]0;garbage"))
	got += binary.feed([]byte("\x1b[0m% echo still-alive\r\nstill-alive\r\n"))
	if !strings.Contains(got, "still-alive") {
		t.Fatalf("output after an unterminated OSC was withheld: %q", got)
	}
	var marker terminalOutputFilter
	got = marker.feed([]byte("\x1b]633;P;Cwd=/x"))
	got += marker.feed([]byte("\x1b[32mhello\x1b[0m\r\n"))
	got += marker.feed([]byte("\x1b]0;title\x07"))
	if !strings.Contains(got, "hello") {
		t.Fatalf("output after a truncated shell marker was dropped with it: %q", got)
	}
}

func TestReviewWorkspaceTerminalFilterRandomSplitsMatchWholeFeed(t *testing.T) {
	pieces := []string{"plain ", "🌍", "é", "\x1b]633;A\x07", "\x1b]133;D;0\x1b\\", "\x1b]1337;SetMark\x07", "\x1b]0;tïtle\x07", "\x1b]2;other\x1b\\", "\x1b[31mred\x1b[0m", "\r\n", "\xff", "\xe2\x82", "\x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\", "\x1b"}
	rng := rand.New(rand.NewPCG(1, 2))
	for round := range 2000 {
		var input strings.Builder
		for range 1 + rng.IntN(20) {
			input.WriteString(pieces[rng.IntN(len(pieces))])
		}
		input.WriteString("end")
		data := []byte(input.String())
		var whole terminalOutputFilter
		want, wantTitle := whole.feed(data), whole.title
		var split terminalOutputFilter
		var got strings.Builder
		title := ""
		for rest := data; len(rest) > 0; {
			n := 1 + rng.IntN(min(len(rest), 7))
			got.WriteString(split.feed(rest[:n]))
			if split.title != "" {
				title, split.title = split.title, ""
			}
			rest = rest[n:]
		}
		if got.String() != want || title != wantTitle {
			t.Fatalf("round %d %q: split %q/%q, whole %q/%q", round, data, got.String(), title, want, wantTitle)
		}
		if !utf8.ValidString(want) {
			t.Fatalf("round %d: invalid UTF-8 %q", round, want)
		}
		for _, marker := range []string{"633;", "133;", "1337;"} {
			if strings.Contains(want, "\x1b]"+marker) {
				t.Fatalf("round %d: shell marker leaked: %q", round, want)
			}
		}
	}
}

func TestReviewWorkspaceTerminalOwnerReconnectAndTakeover(t *testing.T) {
	s, endpoint := reviewWorkspaceTerminalServer(t)
	first := ahpTestClient(t, endpoint, "owner")
	uri := "ahp-terminal:/reconnect"
	createTestTerminal(t, first, uri, "owner")
	// The same identity reconnects on a new socket before the old one closes.
	second := ahpTestClient(t, endpoint, "")
	var raw json.RawMessage
	if err := second.Request(t.Context(), "reconnect", types.ReconnectParams{Channel: types.RootResourceURI, ClientId: "owner", Subscriptions: []string{uri, "ahp-terminal:/missing"}}, &raw); err != nil {
		t.Fatal(err)
	}
	var reconnect struct {
		Type      string `json:"type"`
		Snapshots []struct {
			Resource string          `json:"resource"`
			State    json.RawMessage `json:"state"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(raw, &reconnect); err != nil || reconnect.Type != "snapshot" || len(reconnect.Snapshots) != 1 || reconnect.Snapshots[0].Resource != uri {
		t.Fatalf("reconnect must restore the live terminal and skip the missing one: %s", raw)
	}
	secondSub := second.AttachSubscription(uri)
	defer secondSub.Close()
	_ = first.Shutdown(t.Context())
	reviewWorkspaceTerminalEventually(t, "first socket closed", func() bool {
		h := s.ahpHost()
		h.mu.Lock()
		defer h.mu.Unlock()
		n := 0
		for c := range h.clients {
			if c.id == "owner" {
				n++
			}
		}
		return n == 1
	})
	if view, _ := reviewWorkspaceTerminalLookup(s, uri); view.owner != "owner" {
		t.Fatalf("an overlapping reconnect lost the claim to %q", view.owner)
	}
	if reason := reviewWorkspaceTerminalDispatch(t, second, secondSub, "owner", uri, 1, reviewWorkspaceTerminalInput("printf 'after-reconnect\\n'\n")); reason != nil {
		t.Fatalf("reconnected owner input rejected: %s", *reason)
	}
	observer := ahpTestClient(t, endpoint, "observer")
	_, observerSub, err := subscribeTerminal(t.Context(), observer, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer observerSub.Close()
	if reason := reviewWorkspaceTerminalDispatch(t, observer, observerSub, "observer", uri, 1, reviewWorkspaceTerminalClaim("observer")); reason == nil {
		t.Fatal("claimed a terminal whose owner is connected")
	}
	_ = second.Shutdown(t.Context())
	reviewWorkspaceTerminalEventually(t, "handover to the host", func() bool {
		view, _ := reviewWorkspaceTerminalLookup(s, uri)
		return view.owner == ahpHostClient
	})
	if reason := reviewWorkspaceTerminalDispatch(t, observer, observerSub, "observer", uri, 2, reviewWorkspaceTerminalClaim("observer")); reason != nil {
		t.Fatalf("takeover of a released terminal rejected: %s", *reason)
	}
	// The old owner returns but stays read-only while the new owner is connected.
	third := ahpTestClient(t, endpoint, "owner")
	_, thirdSub, err := subscribeTerminal(t.Context(), third, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer thirdSub.Close()
	if reason := reviewWorkspaceTerminalDispatch(t, third, thirdSub, "owner", uri, 1, reviewWorkspaceTerminalInput("echo hijack\n")); reason == nil {
		t.Fatal("former owner wrote to a terminal it no longer owns")
	}
	if reason := reviewWorkspaceTerminalDispatch(t, third, thirdSub, "owner", uri, 2, reviewWorkspaceTerminalClaim("owner")); reason == nil {
		t.Fatal("former owner reclaimed a terminal from a connected owner")
	}
	if err := third.Request(t.Context(), "disposeTerminal", types.DisposeTerminalParams{Channel: uri}, nil); err == nil {
		t.Fatal("non-owner disposed the terminal")
	}
	if err := observer.Request(t.Context(), "disposeTerminal", types.DisposeTerminalParams{Channel: uri}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestReviewWorkspaceTerminalSlowOwnerIsDisconnectedAndReleased(t *testing.T) {
	s, endpoint := reviewWorkspaceTerminalServer(t)
	conn, _, err := websocket.Dial(t.Context(), endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	uri := "ahp-terminal:/slow-owner"
	call := func(id int, method string, params any) {
		t.Helper()
		if err := wsjson.Write(t.Context(), conn, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		for {
			var reply struct {
				ID    *int            `json:"id"`
				Error json.RawMessage `json:"error"`
			}
			if err := wsjson.Read(t.Context(), conn, &reply); err != nil {
				t.Fatal(err)
			}
			if reply.ID != nil && *reply.ID == id {
				if reply.Error != nil {
					t.Fatalf("%s: %s", method, reply.Error)
				}
				return
			}
		}
	}
	call(1, "initialize", types.InitializeParams{Channel: types.RootResourceURI, ClientId: "slow", ProtocolVersions: []string{"1.0.0"}})
	call(2, "createTerminal", types.CreateTerminalParams{Channel: uri, Claim: types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient, ClientId: "slow"}}})
	call(3, "subscribe", types.SubscribeParams{Channel: uri})
	flood := types.DispatchActionParams{Channel: uri, ClientSeq: 1, Action: reviewWorkspaceTerminalInput("yes slow-consumer-0123456789\n")}
	if err := wsjson.Write(t.Context(), conn, map[string]any{"jsonrpc": "2.0", "method": "dispatchAction", "params": flood}); err != nil {
		t.Fatal(err)
	}
	// The owner stops reading; the host must drop it rather than buffer without bound.
	reviewWorkspaceTerminalEventually(t, "slow owner dropped", reviewWorkspaceTerminalClientGone(s, "slow"))
	reviewWorkspaceTerminalEventually(t, "terminal released", func() bool {
		view, _ := reviewWorkspaceTerminalLookup(s, uri)
		return view.owner == ahpHostClient
	})
	observer := ahpTestClient(t, endpoint, "observer")
	_, sub, err := subscribeTerminal(t.Context(), observer, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if reason := reviewWorkspaceTerminalDispatch(t, observer, sub, "observer", uri, 1, reviewWorkspaceTerminalClaim("observer")); reason != nil {
		t.Fatalf("takeover from a dropped owner rejected: %s", *reason)
	}
	if reason := reviewWorkspaceTerminalDispatch(t, observer, sub, "observer", uri, 2, reviewWorkspaceTerminalInput("\x03")); reason != nil {
		t.Fatalf("new owner cannot interrupt: %s", *reason)
	}
}

func TestReviewWorkspaceTerminalConcurrentClaimsHaveOneWinner(t *testing.T) {
	s, endpoint := reviewWorkspaceTerminalServer(t)
	_ = ahpTestClient(t, endpoint, "starter")
	session, err := s.terminals.CreateCommand(terminal.CommandSpec{Path: "/bin/sh", Args: []string{"-c", "sleep 30"}}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	uri := "ahp-terminal:/wingman/" + session.ID()
	reviewWorkspaceTerminalEventually(t, "terminal attached", func() bool {
		view, ok := reviewWorkspaceTerminalLookup(s, uri)
		return ok && view.owner == ahpHostClient
	})
	const clients = 8
	var mu sync.Mutex
	var winners []string
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range clients {
		id := fmt.Sprintf("claimer-%d", i)
		client := ahpTestClient(t, endpoint, id)
		_, sub, err := subscribeTerminal(t.Context(), client, uri)
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		wg.Go(func() {
			<-start
			if reviewWorkspaceTerminalDispatch(t, client, sub, id, uri, 1, reviewWorkspaceTerminalClaim(id)) == nil {
				mu.Lock()
				winners = append(winners, id)
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()
	view, _ := reviewWorkspaceTerminalLookup(s, uri)
	if len(winners) != 1 || view.owner != winners[0] {
		t.Fatalf("winners %v, owner %q", winners, view.owner)
	}
}

func TestReviewWorkspaceTerminalActionValidation(t *testing.T) {
	s, endpoint := reviewWorkspaceTerminalServer(t)
	owner := ahpTestClient(t, endpoint, "owner")
	uri := "ahp-terminal:/validation"
	createTestTerminal(t, owner, uri, "owner")
	_, ownerSub, err := subscribeTerminal(t.Context(), owner, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerSub.Close()
	observer := ahpTestClient(t, endpoint, "observer")
	_, observerSub, err := subscribeTerminal(t.Context(), observer, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer observerSub.Close()
	resize := func(cols, rows int64) types.StateAction {
		return types.StateAction{Value: &types.TerminalResizedAction{Type: types.ActionTypeTerminalResized, Cols: cols, Rows: rows}}
	}
	title := func(value string) types.StateAction {
		return types.StateAction{Value: &types.TerminalTitleChangedAction{Type: types.ActionTypeTerminalTitleChanged, Title: value}}
	}
	clear := types.StateAction{Value: &types.TerminalClearedAction{Type: types.ActionTypeTerminalCleared}}
	seq := int64(0)
	expect := func(client *ahp.Client, sub *ahp.Subscription, id string, action types.StateAction, accepted bool) {
		t.Helper()
		seq++
		reason := reviewWorkspaceTerminalDispatch(t, client, sub, id, uri, seq, action)
		if accepted != (reason == nil) {
			t.Fatalf("%s %T: accepted=%v, reason %v", id, action.Value, reason == nil, reason)
		}
	}
	for _, action := range []types.StateAction{reviewWorkspaceTerminalInput("echo x\n"), resize(100, 30), title("stolen"), clear} {
		expect(observer, observerSub, "observer", action, false)
	}
	for _, action := range []types.StateAction{
		resize(0, 24), resize(80, 0), resize(1001, 24), resize(-5, 24),
		title(""), title("   "), title(strings.Repeat("t", 1025)),
		reviewWorkspaceTerminalInput(strings.Repeat("x", 1<<20+1)),
		{Value: &types.TerminalDataAction{Type: types.ActionTypeTerminalData, Data: "forged output"}},
		{Value: &types.TerminalExitedAction{Type: types.ActionTypeTerminalExited, ExitCode: ahpPtr(int64(0))}},
		{Value: &types.TerminalCwdChangedAction{Type: types.ActionTypeTerminalCwdChanged, Cwd: "file:///etc"}},
		{Value: &types.TerminalClaimedAction{Type: types.ActionTypeTerminalClaimed, Claim: types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient}}}},
	} {
		expect(owner, ownerSub, "owner", action, false)
	}
	view, _ := reviewWorkspaceTerminalLookup(s, uri)
	if strings.Contains(view.content, "forged output") || view.exit != nil {
		t.Fatal("a rejected server-only action changed the terminal")
	}
	expect(owner, ownerSub, "owner", resize(132, 40), true)
	if cols, rows := view.session.Size(); cols != 132 || rows != 40 {
		t.Fatalf("PTY size %dx%d", cols, rows)
	}
	expect(owner, ownerSub, "owner", title("renamed"), true)
	h := s.ahpHost()
	h.mu.Lock()
	catalogue := h.terminalCatalogueLocked()
	h.mu.Unlock()
	if i := slices.IndexFunc(catalogue, func(info types.TerminalInfo) bool { return info.Resource == uri }); i < 0 || catalogue[i].Title != "renamed" {
		t.Fatalf("catalogue: %+v", catalogue)
	}
	expect(owner, ownerSub, "owner", reviewWorkspaceTerminalInput("exit 5\n"), true)
	reviewWorkspaceTerminalEventually(t, "exit", func() bool {
		view, _ := reviewWorkspaceTerminalLookup(s, uri)
		return view.exit != nil
	})
	if view, _ := reviewWorkspaceTerminalLookup(s, uri); view.exit.ExitCode == nil || *view.exit.ExitCode != 5 {
		t.Fatalf("exit code: %+v", view.exit)
	}
	expect(owner, ownerSub, "owner", reviewWorkspaceTerminalInput("echo late\n"), false)
	if err := owner.Request(t.Context(), "disposeTerminal", types.DisposeTerminalParams{Channel: uri}, nil); err != nil {
		t.Fatal(err)
	}
	expect(owner, ownerSub, "owner", reviewWorkspaceTerminalInput("echo gone\n"), false)
}

func TestReviewWorkspaceTerminalCreateValidation(t *testing.T) {
	s, endpoint := reviewWorkspaceTerminalServer(t)
	client := ahpTestClient(t, endpoint, "owner")
	claim := types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient, ClientId: "owner"}}
	outside := t.TempDir()
	for name, params := range map[string]types.CreateTerminalParams{
		"session scheme":  {Channel: "ahp-session:/x", Claim: claim},
		"empty path":      {Channel: "ahp-terminal:", Claim: claim},
		"host form":       {Channel: "ahp-terminal://host/x", Claim: claim},
		"query":           {Channel: "ahp-terminal:/x?y=1", Claim: claim},
		"foreign claim":   {Channel: "ahp-terminal:/x", Claim: types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient, ClientId: "someone-else"}}},
		"host claim":      {Channel: "ahp-terminal:/x", Claim: ahpHostTerminalClaim()},
		"zero cols":       {Channel: "ahp-terminal:/x", Claim: claim, Cols: ahpPtr(int64(0))},
		"huge rows":       {Channel: "ahp-terminal:/x", Claim: claim, Rows: ahpPtr(int64(5000))},
		"outside cwd":     {Channel: "ahp-terminal:/x", Claim: claim, Cwd: ahpPtr((&url.URL{Scheme: "file", Path: filepath.ToSlash(outside)}).String())},
		"unknown shell":   {Channel: "ahp-terminal:/x", Claim: claim, Meta: map[string]json.RawMessage{"wingman.shell": json.RawMessage(`"/bin/rm"`)}},
		"malformed shell": {Channel: "ahp-terminal:/x", Claim: claim, Meta: map[string]json.RawMessage{"wingman.shell": json.RawMessage(`42`)}},
	} {
		if err := client.Request(t.Context(), "createTerminal", params, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(s.terminals.List()) != 0 {
		t.Fatalf("rejected requests started %d terminals", len(s.terminals.List()))
	}
	uri := "ahp-terminal:/dup"
	createTestTerminal(t, client, uri, "owner")
	err := client.Request(t.Context(), "createTerminal", types.CreateTerminalParams{Channel: uri, Claim: claim}, nil)
	var rpc *ahp.RPCError
	if !errors.As(err, &rpc) || rpc.Code != types.ErrorCodeAlreadyExists {
		t.Fatalf("duplicate channel: %v", err)
	}
	if len(s.terminals.List()) != 1 {
		t.Fatal("duplicate create started another shell")
	}
}

func TestReviewWorkspaceTerminalTrimKeepsValidUTF8(t *testing.T) {
	s, endpoint := reviewWorkspaceTerminalServer(t)
	client := ahpTestClient(t, endpoint, "observer")
	gate := filepath.Join(t.TempDir(), "go")
	// 75,000 two-byte characters on one line: no newline or escape to trim at.
	script := `while [ ! -e "$GATE" ]; do sleep 0.01; done; yes é | tr -d '\n' | head -c 150000; printf 'utf8-end'; exit 0`
	session, err := s.terminals.CreateCommand(terminal.CommandSpec{Path: "/bin/sh", Args: []string{"-c", script}, Env: map[string]*string{"GATE": &gate}}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	uri := "ahp-terminal:/wingman/" + session.ID()
	reviewWorkspaceTerminalEventually(t, "terminal attached", func() bool {
		_, ok := reviewWorkspaceTerminalLookup(s, uri)
		return ok
	})
	_, sub, err := subscribeTerminal(t.Context(), client, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var live strings.Builder
	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case e := <-sub.Events():
			if action, ok := e.(ahp.SubscriptionEventAction); ok {
				switch value := action.Envelope.Action.Value.(type) {
				case *types.TerminalDataAction:
					if !utf8.ValidString(value.Data) || strings.ContainsRune(value.Data, utf8.RuneError) {
						t.Fatalf("data action split a character: %q", value.Data[:min(len(value.Data), 20)])
					}
					live.WriteString(value.Data)
				case *types.TerminalExitedAction:
					done = true
				}
			}
		case <-deadline:
			t.Fatalf("no exit after %d bytes", live.Len())
		}
	}
	if !strings.HasSuffix(live.String(), "utf8-end") {
		t.Fatalf("live stream tail %q", live.String()[max(0, live.Len()-20):])
	}
	view, _ := reviewWorkspaceTerminalLookup(s, uri)
	if len(view.content) > ahpTerminalMaxContent || !utf8.ValidString(view.content) || strings.ContainsRune(view.content, utf8.RuneError) || !strings.HasSuffix(view.content, "utf8-end") || !strings.HasPrefix(view.content, "é") {
		t.Fatalf("trimmed snapshot: %d bytes, head %q", len(view.content), view.content[:min(len(view.content), 12)])
	}
}

func TestReviewWorkspaceTerminalCatalogueRecoversFromOverflow(t *testing.T) {
	s, endpoint := reviewWorkspaceTerminalServer(t)
	_ = ahpTestClient(t, endpoint, "observer")
	h := s.ahpHost()
	// Stall the catalogue pump so the manager drops it as a slow observer.
	h.terminalMu.Lock()
	for range 40 {
		session, err := s.terminals.CreateCommand(terminal.CommandSpec{Path: "/bin/sh", Args: []string{"-c", "true"}}, 80, 24)
		if err != nil {
			h.terminalMu.Unlock()
			t.Fatal(err)
		}
		<-session.Done()
	}
	running, err := s.terminals.CreateCommand(terminal.CommandSpec{Path: "/bin/sh", Args: []string{"-c", "sleep 30"}}, 80, 24)
	h.terminalMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	want := func() []string {
		var ids []string
		for _, session := range s.terminals.List() {
			ids = append(ids, "ahp-terminal:/wingman/"+session.ID())
		}
		slices.Sort(ids)
		return ids
	}
	got := func() []string {
		h.mu.Lock()
		defer h.mu.Unlock()
		var ids []string
		for _, info := range h.terminalCatalogueLocked() {
			ids = append(ids, info.Resource)
		}
		return ids
	}
	reviewWorkspaceTerminalEventually(t, "catalogue to match the manager", func() bool {
		return slices.Equal(got(), want())
	})
	if !slices.Contains(got(), "ahp-terminal:/wingman/"+running.ID()) || len(got()) > 33 {
		t.Fatalf("catalogue %d entries", len(got()))
	}
}

func TestReviewWorkspaceTerminalDebugTerminalsReplaceExitedOnes(t *testing.T) {
	s, endpoint := reviewWorkspaceTerminalServer(t)
	client := ahpTestClient(t, endpoint, "observer")
	launch := func(script string) dap.TerminalProcess {
		t.Helper()
		process, err := s.LaunchTerminal(t.Context(), dap.TerminalLaunch{Title: "Debug", Path: "/bin/sh", Args: []string{"-c", script}})
		if err != nil {
			t.Fatal(err)
		}
		return process
	}
	first := launch("printf 'debuggee-out\\n'; exit 4")
	firstURI := "ahp-terminal:/wingman/" + first.ID()
	reviewWorkspaceTerminalEventually(t, "debuggee exit published", func() bool {
		view, _ := reviewWorkspaceTerminalLookup(s, firstURI)
		return view.exit != nil
	})
	view, _ := reviewWorkspaceTerminalLookup(s, firstURI)
	if view.owner != ahpHostClient || view.title != "Debug" || !strings.Contains(view.content, "debuggee-out") || view.exit.ExitCode == nil || *view.exit.ExitCode != 4 {
		t.Fatalf("debug terminal: %+v", view)
	}
	shell := ahpTestClient(t, endpoint, "user")
	createTestTerminal(t, shell, "ahp-terminal:/user-shell", "user")
	second := launch("sleep 30")
	reviewWorkspaceTerminalEventually(t, "exited debuggee removed", func() bool {
		_, old := reviewWorkspaceTerminalLookup(s, firstURI)
		_, current := reviewWorkspaceTerminalLookup(s, "ahp-terminal:/wingman/"+second.ID())
		return !old && current
	})
	if _, ok := reviewWorkspaceTerminalLookup(s, "ahp-terminal:/user-shell"); !ok {
		t.Fatal("a debug launch removed a user shell")
	}
	if _, _, err := client.Subscribe(t.Context(), firstURI); err == nil {
		t.Fatal("removed debug terminal still subscribable")
	}
}

func TestReviewWorkspaceTerminalStatusRoute(t *testing.T) {
	s, endpoint := reviewWorkspaceTerminalServer(t)
	client := ahpTestClient(t, endpoint, "owner")
	router := chi.NewRouter()
	router.Get("/api/workspace/terminals/{terminal}/status", s.handleTerminalStatus)
	handler := LocalHostOnly(s.checkInstance(router))
	status := func(uri, instance string) (int, bool) {
		req := httptest.NewRequest(http.MethodGet, "http://localhost/api/workspace/terminals/"+url.PathEscape(uri)+"/status", nil)
		if instance != "" {
			req.Header.Set(instanceHeader, instance)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var body struct {
			Busy bool `json:"busy"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Busy
	}
	uri := "ahp-terminal:/review/ü%41 x"
	createTestTerminal(t, client, uri, "owner")
	_, sub, err := subscribeTerminal(t.Context(), client, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if code, _ := status(uri, "stale-instance"); code != http.StatusConflict {
		t.Fatalf("stale instance: %d", code)
	}
	if code, _ := status("ahp-terminal:/review/üA x", s.scope.InstanceID); code != http.StatusNotFound {
		t.Fatalf("decoded twice: %d", code)
	}
	if code, busy := status(uri, s.scope.InstanceID); code != http.StatusOK || busy {
		t.Fatalf("idle shell: %d busy=%v", code, busy)
	}
	if reason := reviewWorkspaceTerminalDispatch(t, client, sub, "owner", uri, 1, reviewWorkspaceTerminalInput("sleep 30\n")); reason != nil {
		t.Fatal(*reason)
	}
	reviewWorkspaceTerminalEventually(t, "busy status", func() bool {
		code, busy := status(uri, s.scope.InstanceID)
		return code == http.StatusOK && busy
	})
	if err := client.Request(t.Context(), "disposeTerminal", types.DisposeTerminalParams{Channel: uri}, nil); err != nil {
		t.Fatal(err)
	}
	if code, _ := status(uri, s.scope.InstanceID); code != http.StatusNotFound {
		t.Fatalf("disposed terminal: %d", code)
	}
}
