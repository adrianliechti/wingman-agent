package claude

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/internal/testenv"
)

func TestStoreSessionClosesReplacedSession(t *testing.T) {
	a := New(Options{})
	old := a.newSession("session-1", t.TempDir(), "default", "", nil)
	oldCtx, cancelOld := context.WithCancel(context.Background())
	old.mu.Lock()
	old.cancel = cancelOld
	old.mu.Unlock()
	a.storeSession(old)

	replacement := a.newSession(old.id, old.cwd, "default", "", nil)
	a.storeSession(replacement)
	select {
	case <-oldCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("replaced session was not cancelled")
	}
	if !old.isClosed() || a.lookup(old.id) != replacement {
		t.Fatal("replacement session was not installed cleanly")
	}

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if !replacement.isClosed() || a.lookup(old.id) != nil {
		t.Fatal("agent close did not remove and close its sessions")
	}
}

func TestBypassModeHonorsHostOptOutAndSettings(t *testing.T) {
	testenv.UserHome(t)
	a := New(Options{Path: filepath.Join(t.TempDir(), "missing-claude"), Stderr: io.Discard})
	modeIDs := func(state *acp.SessionModeState) []string {
		var ids []string
		for _, m := range state.AvailableModes {
			ids = append(ids, string(m.Id))
		}
		return ids
	}
	checkBypassFlag := func(id acp.SessionId, want bool) {
		t.Helper()
		s := a.lookup(id)
		s.mu.Lock()
		args := s.cliArgsLocked()
		s.mu.Unlock()
		if got := slices.Contains(args, "--allow-dangerously-skip-permissions"); got != want {
			t.Fatalf("CLI bypass capability = %v, want %v: %v", got, want, args)
		}
	}

	open := t.TempDir()
	resp, err := a.NewSession(context.Background(), acp.NewSessionRequest{Cwd: open, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(modeIDs(resp.Modes), bypassModeID) {
		t.Fatalf("modes = %v, want bypass offered by default", modeIDs(resp.Modes))
	}
	checkBypassFlag(resp.SessionId, true)

	optOut := map[string]any{"claudeCode": map[string]any{"options": map[string]any{"allowDangerouslySkipPermissions": false}}}
	resp, err = a.NewSession(context.Background(), acp.NewSessionRequest{Cwd: open, McpServers: []acp.McpServer{}, Meta: optOut})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(modeIDs(resp.Modes), bypassModeID) {
		t.Fatalf("modes = %v, want bypass removed by host opt-out", modeIDs(resp.Modes))
	}
	checkBypassFlag(resp.SessionId, false)
	if _, err := a.SetSessionMode(context.Background(), acp.SetSessionModeRequest{SessionId: resp.SessionId, ModeId: bypassModeID}); err == nil {
		t.Fatal("bypass mode was accepted after host opt-out")
	}

	disabled := t.TempDir()
	if err := os.MkdirAll(filepath.Join(disabled, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(disabled, ".claude", "settings.json"), []byte(`{"permissions":{"disableBypassPermissionsMode":"disable"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err = a.NewSession(context.Background(), acp.NewSessionRequest{Cwd: disabled, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(modeIDs(resp.Modes), bypassModeID) {
		t.Fatalf("modes = %v, want bypass removed by disableBypassPermissionsMode", modeIDs(resp.Modes))
	}
	checkBypassFlag(resp.SessionId, false)
}

func TestPlanModeRemembersPreviousMode(t *testing.T) {
	s := New(Options{}).newSession("s", t.TempDir(), "default", "", nil)
	s.setModeLocked(bypassModeID)
	s.setModeLocked(planModeID)
	s.setModeLocked(planModeID)
	if s.prePlanMode != bypassModeID {
		t.Fatalf("pre-plan mode = %q, want %q", s.prePlanMode, bypassModeID)
	}
	s.setModeLocked(defaultModeID)
	if s.prePlanMode != "" {
		t.Fatalf("pre-plan mode = %q after leaving plan", s.prePlanMode)
	}
}

func TestResumeKeepsUnpromptedLiveSession(t *testing.T) {
	testenv.UserHome(t)
	ctx := context.Background()
	a := New(Options{Path: filepath.Join(t.TempDir(), "missing-claude"), Stderr: io.Discard})
	cwd, added := t.TempDir(), t.TempDir()
	created, err := a.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: created.SessionId, ModeId: planModeID}); err != nil {
		t.Fatal(err)
	}
	resumed, err := a.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: created.SessionId, Cwd: cwd, AdditionalDirectories: []string{added}})
	if err != nil {
		t.Fatalf("resume of a session that was never prompted: %v", err)
	}
	if resumed.Modes == nil || resumed.Modes.CurrentModeId != planModeID {
		t.Fatalf("modes = %+v, want the live session's plan mode", resumed.Modes)
	}
	s := a.lookup(created.SessionId)
	s.mu.Lock()
	args := strings.Join(s.cliArgsLocked(), " ")
	s.mu.Unlock()
	if strings.Contains(args, "--resume") || !strings.Contains(args, "--session-id "+string(created.SessionId)) || !strings.Contains(args, "--add-dir "+added) {
		t.Fatalf("args = %s, want a fresh start under the same id with the new directory", args)
	}
	if _, err := a.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: acp.SessionId(uuid.NewString()), Cwd: cwd}); err == nil {
		t.Fatal("resume of an unknown session succeeded")
	}
}

func TestLoadOfUnpromptedForkReplaysItsSource(t *testing.T) {
	testenv.UserHome(t)
	ctx := context.Background()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	wire := &bytes.Buffer{}
	a := New(Options{Path: filepath.Join(t.TempDir(), "missing-claude"), Stderr: io.Discard})
	a.SetAgentConnection(acp.NewAgentSideConnection(a, wire, reader))
	cwd := t.TempDir()
	source := uuid.NewString()
	dir := projectDirsFor(cwd, nil)[0]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","message":{"role":"user","content":"remember PELICAN"}}` + "\n" +
		`{"type":"assistant","message":{"model":"claude-sonnet-5","content":[{"type":"text","text":"OK"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, source+".jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	fork, err := a.UnstableForkSession(ctx, acp.UnstableForkSessionRequest{SessionId: acp.SessionId(source), Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.LoadSession(ctx, acp.LoadSessionRequest{SessionId: fork.SessionId, Cwd: cwd, McpServers: []acp.McpServer{}}); err != nil {
		t.Fatalf("load of a fork that was never prompted: %v", err)
	}
	var user, agent string
	for _, update := range readTranscriptUpdates(t, wire.Bytes()) {
		if chunk := update.UserMessageChunk; chunk != nil && chunk.Content.Text != nil {
			user += chunk.Content.Text.Text
		}
		if chunk := update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
			agent += chunk.Content.Text.Text
		}
	}
	if user != "remember PELICAN" || agent != "OK" {
		t.Fatalf("replay = user %q, agent %q, want the source's history", user, agent)
	}
	s := a.lookup(fork.SessionId)
	s.mu.Lock()
	args := strings.Join(s.cliArgsLocked(), " ")
	s.mu.Unlock()
	if !strings.Contains(args, "--resume "+source+" --session-id "+string(fork.SessionId)+" --fork-session") {
		t.Fatalf("args = %s, want the fork still pending", args)
	}
}

func TestPromptRacingCloseEndsCancelled(t *testing.T) {
	a := New(Options{Stderr: io.Discard})
	s := a.newSession("s", t.TempDir(), "default", "", nil)
	a.storeSession(s)
	s.close()
	resp, err := a.Prompt(context.Background(), acp.PromptRequest{SessionId: s.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
	if err != nil || resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("prompt = %+v, %v, want cancelled", resp, err)
	}
}

func TestDeleteSessionTouchesOnlySessionTranscripts(t *testing.T) {
	home := testenv.UserHome(t)
	a := New(Options{Stderr: io.Discard})
	project := filepath.Join(home, ".claude", "projects", "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, ".claude", "outside.jsonl")
	id := uuid.NewString()
	transcript := filepath.Join(project, id+".jsonl")
	for _, path := range []string{outside, transcript} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, sid := range []string{"../../outside", uuid.NewString(), id} {
		if _, err := a.UnstableDeleteSession(context.Background(), acp.UnstableDeleteSessionRequest{SessionId: acp.SessionId(sid)}); err != nil {
			t.Fatalf("delete %q: %v", sid, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("delete reached a file outside the session store: %v", err)
	}
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("transcript survived its delete: %v", err)
	}
}
