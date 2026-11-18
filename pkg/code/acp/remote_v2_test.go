package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

// Subprocess fixture exercises negotiation, buffered stdio and shutdown through
// the same constructor used for Copilot, OpenCode and agents.json entries.
func TestExternalNegotiationHelper(t *testing.T) {
	mode := os.Getenv("WINGMAN_ACP_NEGOTIATION_FIXTURE")
	if mode == "" {
		return
	}
	enc := json.NewEncoder(os.Stdout)
	scan := bufio.NewScanner(os.Stdin)
	update := func(v any) {
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s", "update": v}})
	}
	calls := 0
	for scan.Scan() {
		var req struct {
			ID     json.RawMessage
			Method string
			Params map[string]any
		}
		if json.Unmarshal(scan.Bytes(), &req) != nil {
			os.Exit(2)
		}
		reply := func(res any) { _ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res}) }
		switch req.Method {
		case "initialize":
			calls++
			if calls == 1 && (req.Params["protocolVersion"] != 2.0 || req.Params["info"] == nil || req.Params["capabilities"] == nil) {
				os.Exit(3)
			}
			if mode == "retry-v1" {
				if calls == 1 {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "v1 required"}})
					continue
				}
				if req.Params["protocolVersion"] != 1.0 || req.Params["clientCapabilities"] == nil {
					os.Exit(4)
				}
				reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}})
				continue
			}
			reply(map[string]any{"protocolVersion": 2, "info": map[string]any{"name": "fixture", "version": "1"}, "capabilities": map[string]any{"session": map[string]any{}}})
		case "session/new":
			startup := map[string]any{"sessionUpdate": "notice", "severity": "info", "title": "Ready"}
			if mode == "retry-v1" {
				update(startup)
			}
			reply(map[string]any{"sessionId": "s", "availableCommands": []any{map[string]any{"type": "command", "name": "review", "description": "Review the changes"}, map[string]any{"type": "_external", "name": "future", "description": "Unknown invocation"}}})
			if mode != "retry-v1" {
				update(startup)
			}
		case "session/prompt":
			if mode == "retry-v1" {
				update(map[string]any{"sessionUpdate": "notice", "severity": "warning", "title": "Hook stopped", "description": "Workspace policy"})
				reply(map[string]any{"stopReason": "end_turn"})
				continue
			}
			if req.Params["idempotencyKey"] != "input" {
				os.Exit(5)
			}
			ack := func() { reply(map[string]any{"messageId": "inserted"}) }
			if mode == "early-ack" {
				ack()
			}
			update(map[string]any{"sessionUpdate": "user_message", "messageId": "inserted", "content": req.Params["prompt"]})
			update(map[string]any{"sessionUpdate": "state_update", "state": "running"})
			update(map[string]any{"sessionUpdate": "notice", "severity": "warning", "title": "Hook stopped", "description": "Workspace policy"})
			for _, v := range []struct{ id, text string }{{"a", "obsolete"}, {"b", "second"}, {"a", "first"}} {
				update(map[string]any{"sessionUpdate": "agent_message", "messageId": v.id, "content": []any{map[string]any{"type": "text", "text": v.text}}})
			}
			update(map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "a", "content": map[string]any{"type": "text", "text": " answer"}})
			update(map[string]any{"sessionUpdate": "state_update", "state": "idle", "stopReason": "end_turn"})
			if mode == "late-ack" {
				ack()
			}
		case "session/close":
			reply(map[string]any{})
		default:
			fmt.Fprintln(os.Stderr, req.Method)
			os.Exit(6)
		}
	}
	os.Exit(0)
}

type noticeUI struct {
	fakeUI
	notices chan string
}

func (ui *noticeUI) Notice(sessionID string, n code.Notice) {
	ui.notices <- sessionID + " " + n.String()
}

func TestExternalClientNegotiatesAndDrainsForeground(t *testing.T) {
	for _, mode := range []string{"early-ack", "late-ack", "retry-v1"} {
		t.Run(mode, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			a, err := New(ctx, &code.Workspace{RootPath: t.TempDir()}, code.AgentDef{Name: "fixture", Command: exe, Args: []string{"-test.run=^TestExternalNegotiationHelper$"}, Env: map[string]string{"WINGMAN_ACP_NEGOTIATION_FIXTURE": mode, "GORACE": "atexit_sleep_ms=0"}})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			ui := &noticeUI{notices: make(chan string, 2)}
			a.SetUI(ui)
			sid, err := a.NewSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := a.Send(harness.WithInputID(ctx, "input"), sid, []harness.Content{{Text: "hello"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, want := range []string{"Ready", "Warning: Hook stopped\nWorkspace policy"} {
				select {
				case got := <-ui.notices:
					if got != sid+" "+want {
						t.Fatalf("notice = %q, want %q", got, want)
					}
				case <-ctx.Done():
					t.Fatalf("missing live notice %q", want)
				}
			}
			if mode == "retry-v1" {
				return
			}
			if commands := a.Commands(sid); len(commands) != 1 || commands[0].Name != "review" {
				t.Fatalf("setup commands = %v", commands)
			}
			var got []string
			for _, m := range a.Messages(sid) {
				for _, c := range m.Content {
					got = append(got, c.Text)
				}
			}
			if strings.Join(got, "|") != "hello|first answer|second" {
				t.Fatalf("messages after idle = %q", got)
			}
		})
	}
}
