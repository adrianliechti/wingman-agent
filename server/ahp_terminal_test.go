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
	if err != nil { t.Fatal(err) }
	result, sub, err := first.Subscribe(t.Context(), uri)
	if err != nil { t.Fatal(err) }; defer sub.Close()
	state := result.Snapshot.State.Terminal
	if state == nil || state.Title == "" { t.Fatal("missing terminal state") }
	second := ahpTestClient(t, endpoint, "observer")
	_, other, err := second.Subscribe(t.Context(), uri)
	if err != nil { t.Fatal(err) }; defer other.Close()
	input := types.StateAction{Value: &types.TerminalInputAction{Type: types.ActionTypeTerminalInput, Data: "printf 'ahp-terminal-ok\\n'; exit 7\n"}}
	if err := second.Notify(t.Context(), "dispatchAction", types.DispatchActionParams{Channel: uri, ClientSeq: 1, Action: input}); err != nil { t.Fatal(err) }
	for {
		select {
		case e := <-other.Events():
			if action, ok := e.(ahp.SubscriptionEventAction); ok && action.Envelope.Origin != nil {
				if action.Envelope.RejectionReason == nil { t.Fatal("non-owner input accepted") }
				goto rejected
			}
		case <-time.After(5*time.Second): t.Fatal("missing rejection")
		}
	}
rejected:
	p := types.DispatchActionParams{Channel: uri, ClientSeq: 1, Action: input}
	if err := first.Notify(t.Context(), "dispatchAction", p); err != nil { t.Fatal(err) }
	if err := first.Notify(t.Context(), "dispatchAction", p); err != nil { t.Fatal(err) }
	deadline := time.After(5*time.Second)
	for {
		select {
		case e := <-sub.Events():
			if action, ok := e.(ahp.SubscriptionEventAction); ok && action.Envelope.RejectionReason == nil {
				ahp.ApplyActionToTerminal(state, action.Envelope.Action)
				if _, ok := action.Envelope.Action.Value.(*types.TerminalExitedAction); ok { goto exited }
			}
		case <-deadline: t.Fatal("terminal did not exit")
		}
	}
exited:
	exit, ok := state.Lifecycle.Value.(*types.TerminalExitedLifecycleState)
	if !ok || exit.ExitCode == nil || *exit.ExitCode != 7 { t.Fatalf("wrong exit state: %+v", state.Lifecycle) }
	var output strings.Builder
	for _, p := range state.Content { if part, ok := p.Value.(*types.TerminalUnclassifiedPart); ok { output.WriteString(part.Value) } }
	if !strings.Contains(output.String(), "ahp-terminal-ok") { t.Fatal("terminal output missing") }
	late, lateSub, err := second.Subscribe(t.Context(), uri)
	if err != nil { t.Fatal(err) }; defer lateSub.Close()
	if _, ok := late.Snapshot.State.Terminal.Lifecycle.Value.(*types.TerminalExitedLifecycleState); !ok { t.Fatal("late subscription lost exit state") }
	if len(late.Snapshot.State.Terminal.Content) == 0 { t.Fatal("late subscription lost output") }
	if err := first.Request(t.Context(), "disposeTerminal", types.DisposeTerminalParams{Channel: uri}, nil); err != nil { t.Fatal(err) }
	if len(s.terminals.List()) != 0 { t.Fatal("terminal was not disposed") }
	ctx, cancel := context.WithTimeout(t.Context(), time.Second); defer cancel()
	if _, _, err := first.Subscribe(ctx, uri); err == nil { t.Fatal("disposed terminal subscribable") }
}

func TestAHPTerminalOutputFilterBoundaries(t *testing.T) {
	input := "before🌍\x1b]633;A\x07middle\x1b]133;D;0\x1b\\\x1b]0;title\x07after"
	want := "before🌍middle\x1b]0;title\x07after"
	for split := 0; split <= len(input); split++ {
		var filter terminalOutputFilter
		got := filter.feed([]byte(input[:split])) + filter.feed([]byte(input[split:]))
		if got != want { t.Fatalf("split %d: %q", split, got) }
	}
}
