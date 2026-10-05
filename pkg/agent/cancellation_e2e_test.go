package agent_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
	"github.com/adrianliechti/wingman-agent/pkg/session"
)

func TestAlreadyCancelledHarnessSendE2E(t *testing.T) {
	p := newContextProvider(t, func(w http.ResponseWriter, _ contextRequest) {
		fmt.Fprint(w, completedText("accepted answer"))
	})
	var starts, prompts atomic.Int64
	cfg := e2eConfig(t)
	cfg.RequireFinish = func(string) bool { return false }
	cfg.CacheKey = "cancel-e2e"
	cfg.Hooks = hook.Hooks{
		SessionStart: []hook.SessionStart{func(context.Context, string) (hook.Outcome, error) {
			starts.Add(1)
			return hook.Outcome{AdditionalContext: []string{"startup context"}}, nil
		}},
		UserPromptSubmit: []hook.UserPromptSubmit{func(context.Context, string) (hook.Outcome, error) {
			prompts.Add(1)
			return hook.Outcome{}, nil
		}},
	}
	dir := t.TempDir()
	journal, err := session.OpenJournal(dir, cfg.CacheKey)
	if err != nil {
		t.Fatal(err)
	}
	a := &agent.Agent{Config: cfg, Recorder: journal}
	before := a.StateSnapshot()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stream, err := a.Send(ctx, []agent.Content{{Text: "already cancelled"}})
	if !errors.Is(err, context.Canceled) || stream != nil || starts.Load() != 0 || prompts.Load() != 0 || len(p.snapshot()) != 0 || !reflect.DeepEqual(before, a.StateSnapshot()) {
		t.Fatalf("error=%v starts=%d prompts=%d requests=%d messages=%d", err, starts.Load(), prompts.Load(), len(p.snapshot()), len(a.MessagesSnapshot()))
	}
	if err := drain(t, a, "accepted input"); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || prompts.Load() != 1 || len(p.snapshot()) != 1 {
		t.Fatalf("startup=%d prompts=%d requests=%d", starts.Load(), prompts.Load(), len(p.snapshot()))
	}
	loaded, err := session.Load(dir, cfg.CacheKey)
	if err != nil {
		t.Fatal(err)
	}
	restored := &agent.Agent{Config: cfg}
	if err := restored.Restore(loaded.State); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.MessagesSnapshot(), restored.MessagesSnapshot()) {
		t.Fatal("persisted transcript differs after reload")
	}
	if len(loaded.State.Messages) != 3 || loaded.State.Messages[1].Content[0].Text != "accepted input" || loaded.State.Messages[2].Content[0].Text != "accepted answer" {
		t.Fatalf("reloaded transcript includes rejected input: %+v", loaded.State.Messages)
	}
}
