package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/terminal"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func TestAHPTerminalLifecycleAndOwnership(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	s.terminals = terminal.NewManager(t.TempDir())
	first := ahpTestClient(t, endpoint, "terminal-owner")
	uri := "ahp-terminal:/test"
	err := first.Request(t.Context(), "createTerminal", types.CreateTerminalParams{Channel: uri, Claim: types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient, ClientId: "terminal-owner"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, sub, err := subscribeTerminal(t.Context(), first, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if state == nil || state.Title == "" {
		t.Fatal("missing terminal state")
	}
	second := ahpTestClient(t, endpoint, "observer")
	_, other, err := subscribeTerminal(t.Context(), second, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	input := types.StateAction{Value: &types.TerminalInputAction{Type: types.ActionTypeTerminalInput, Data: "printf 'ahp-terminal-ok\\n'; exit 7\n"}}
	if err := second.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: uri, ClientSeq: 1, Action: input}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case e := <-other.Events():
			if action, ok := e.(ahp.SubscriptionEventAction); ok && action.Envelope.Origin != nil {
				if action.Envelope.RejectionReason == nil {
					t.Fatal("non-owner input accepted")
				}
				goto rejected
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing rejection")
		}
	}
rejected:
	p := types.DispatchActionParams{Channel: uri, ClientSeq: 1, Action: input}
	if err := first.Notify(t.Context(), "dispatchAction", p); err != nil {
		t.Fatal(err)
	}
	if err := first.Notify(t.Context(), "dispatchAction", p); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-sub.Events():
			if action, ok := e.(ahp.SubscriptionEventAction); ok && action.Envelope.RejectionReason == nil {
				ahp.ApplyActionToTerminal(state, action.Envelope.Action)
				if _, ok := action.Envelope.Action.Value.(*types.TerminalExitedAction); ok {
					goto exited
				}
			}
		case <-deadline:
			t.Fatal("terminal did not exit")
		}
	}
exited:
	exit, ok := state.Lifecycle.Value.(*types.TerminalExitedLifecycleState)
	if !ok || exit.ExitCode == nil || *exit.ExitCode != 7 {
		t.Fatalf("wrong exit state: %+v", state.Lifecycle)
	}
	var output strings.Builder
	for _, p := range state.Content {
		if part, ok := p.Value.(*types.TerminalUnclassifiedPart); ok {
			output.WriteString(part.Value)
		}
	}
	if !strings.Contains(output.String(), "ahp-terminal-ok") {
		t.Fatal("terminal output missing")
	}
	late, lateSub, err := subscribeTerminal(t.Context(), second, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer lateSub.Close()
	if _, ok := late.Lifecycle.Value.(*types.TerminalExitedLifecycleState); !ok {
		t.Fatal("late subscription lost exit state")
	}
	if len(late.Content) == 0 {
		t.Fatal("late subscription lost output")
	}
	if err := first.Request(t.Context(), "disposeTerminal", types.DisposeTerminalParams{Channel: uri}, nil); err != nil {
		t.Fatal(err)
	}
	if len(s.terminals.List()) != 0 {
		t.Fatal("terminal was not disposed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, err := first.Subscribe(ctx, uri); err == nil {
		t.Fatal("disposed terminal subscribable")
	}
}

func terminalContent(state *types.TerminalState) string {
	var output strings.Builder
	for _, p := range state.Content {
		if part, ok := p.Value.(*types.TerminalUnclassifiedPart); ok {
			output.WriteString(part.Value)
		}
	}
	return output.String()
}

func createTestTerminal(t *testing.T, client *ahp.Client, uri, owner string) {
	t.Helper()
	claim := types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient, ClientId: owner}}
	if err := client.Request(t.Context(), "createTerminal", types.CreateTerminalParams{Channel: uri, Claim: claim}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAHPTerminalTrimsWithoutResettingClients(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	s.terminals = terminal.NewManager(t.TempDir())
	client := ahpTestClient(t, endpoint, "terminal-owner")
	uri := "ahp-terminal:/long"
	createTestTerminal(t, client, uri, "terminal-owner")
	_, sub, err := subscribeTerminal(t.Context(), client, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	input := types.StateAction{Value: &types.TerminalInputAction{Type: types.ActionTypeTerminalInput, Data: "yes 0123456789 | head -c 300000; echo trimmed-$((1+1))\n"}}
	if err := client.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: uri, ClientSeq: 1, Action: input}); err != nil {
		t.Fatal(err)
	}
	received := ""
	deadline := time.After(20 * time.Second)
	for !strings.Contains(received, "trimmed-2") {
		select {
		case e := <-sub.Events():
			action, ok := e.(ahp.SubscriptionEventAction)
			if !ok {
				continue
			}
			switch value := action.Envelope.Action.Value.(type) {
			case *types.TerminalClearedAction:
				t.Fatal("trimming reset the client terminal")
			case *types.TerminalDataAction:
				received += value.Data
			}
		case <-deadline:
			t.Fatalf("output did not finish after %d bytes", len(received))
		}
	}
	if len(received) < 300000 {
		t.Fatalf("subscriber missed output: %d bytes", len(received))
	}
	late, lateSub, err := subscribeTerminal(t.Context(), client, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer lateSub.Close()
	content := terminalContent(late)
	if len(content) > ahpTerminalMaxContent || !strings.HasPrefix(content, "0123456789") || !strings.Contains(content, "trimmed-2") {
		t.Fatalf("snapshot of %d bytes must be bounded, start at a line and keep the newest output: %q", len(content), content[:min(len(content), 40)])
	}
}

func TestAHPTerminalOwnerDisconnectHandsOver(t *testing.T) {
	s, _, _, endpoint := ahpTestServer(t)
	s.terminals = terminal.NewManager(t.TempDir())
	owner := ahpTestClient(t, endpoint, "owner")
	uri := "ahp-terminal:/handover"
	createTestTerminal(t, owner, uri, "owner")
	observer := ahpTestClient(t, endpoint, "observer")
	state, sub, err := subscribeTerminal(t.Context(), observer, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	_ = owner.Shutdown(t.Context())
	h := s.ahpHost()
	for gone := false; !gone; {
		h.mu.Lock()
		gone = true
		for c := range h.clients {
			gone = gone && c.id != "owner"
		}
		if gone {
			h.releaseTerminalsLocked("owner") // handleAHP does this when the owner's last connection closes
		}
		h.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	claimedBy := func() string {
		claim, _ := state.Claim.Value.(*types.TerminalClientClaim)
		if claim == nil {
			return ""
		}
		return claim.ClientId
	}
	deadline := time.After(5 * time.Second)
	for claimedBy() != "wingman-host" {
		select {
		case e := <-sub.Events():
			if action, ok := e.(ahp.SubscriptionEventAction); ok && action.Envelope.RejectionReason == nil {
				ahp.ApplyActionToTerminal(state, action.Envelope.Action)
			}
		case <-deadline:
			t.Fatalf("terminal was not handed to the host: %+v", state.Claim)
		}
	}
	claim := types.StateAction{Value: &types.TerminalClaimedAction{Type: types.ActionTypeTerminalClaimed, Claim: types.TerminalClaim{Value: &types.TerminalClientClaim{Kind: types.TerminalClaimKindClient, ClientId: "observer"}}}}
	if err := observer.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: uri, ClientSeq: 1, Action: claim}); err != nil {
		t.Fatal(err)
	}
	for claimedBy() != "observer" {
		select {
		case e := <-sub.Events():
			if action, ok := e.(ahp.SubscriptionEventAction); ok {
				if action.Envelope.RejectionReason != nil {
					t.Fatalf("claim rejected: %s", *action.Envelope.RejectionReason)
				}
				ahp.ApplyActionToTerminal(state, action.Envelope.Action)
			}
		case <-deadline:
			t.Fatal("observer could not take over the terminal")
		}
	}
}

func TestAHPTerminalOutputFilterBoundaries(t *testing.T) {
	input := "before🌍\x1b]633;A\x07middle\x1b]133;D;0\x1b\\\x1b]0;title\x07after"
	want := "before🌍middle\x1b]0;title\x07after"
	for split := 0; split <= len(input); split++ {
		var filter terminalOutputFilter
		got := filter.feed([]byte(input[:split])) + filter.feed([]byte(input[split:]))
		if got != want {
			t.Fatalf("split %d: %q", split, got)
		}
	}
}

// The Go SDK 1.0.0 SnapshotState decoder checks lifecycle before content,
// mistaking a terminal lifecycle object for a session lifecycle string. Decode
// this known channel directly while still exercising its official RPC client.
func subscribeTerminal(ctx context.Context, client *ahp.Client, uri string) (*types.TerminalState, *ahp.Subscription, error) {
	sub := client.AttachSubscription(uri)
	var result struct {
		Snapshot struct {
			State types.TerminalState `json:"state"`
		} `json:"snapshot"`
	}
	err := client.Request(ctx, "subscribe", types.SubscribeParams{Channel: uri}, &result)
	return &result.Snapshot.State, sub, err
}
