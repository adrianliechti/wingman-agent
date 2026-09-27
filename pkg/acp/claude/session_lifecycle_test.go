package claude

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"

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
