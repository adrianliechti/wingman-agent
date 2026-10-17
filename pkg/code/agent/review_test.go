package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

type reviewUI struct {
	mu       sync.Mutex
	confirms []string
}

func (u *reviewUI) Elicit(context.Context, tool.ElicitRequest) (tool.ElicitResult, error) {
	return tool.ElicitResult{Action: tool.ElicitCancel}, nil
}

func (u *reviewUI) Confirm(_ context.Context, message string) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.confirms = append(u.confirms, message)
	return true, nil
}

// newReviewSession serves verdict for every review request; an empty verdict
// fails the request so the agent falls back to the user.
func newReviewSession(t *testing.T, verdict string) (*Agent, *reviewUI, context.Context, *[]string) {
	t.Helper()
	ws := newOptionsTestWorkspace(t)
	var mu sync.Mutex
	var inputs []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		var body struct{ Input string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		inputs = append(inputs, body.Input)
		mu.Unlock()
		if verdict == "" {
			http.Error(w, `{"error":{"message":"no reviewer"}}`, http.StatusBadRequest)
			return
		}
		text, _ := json.Marshal(verdict)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%s}]}]}`, text)
	}))
	t.Cleanup(provider.Close)
	t.Setenv("WINGMAN_URL", provider.URL)
	cfg, err := harness.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	ui := &reviewUI{}
	a := New(ws, cfg, ui)
	t.Cleanup(func() { a.Close() })
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return a, ui, code.WithSessionID(t.Context(), id), &inputs
}

func TestReviewDeniesWithoutAskingAndTripsBreaker(t *testing.T) {
	a, ui, ctx, inputs := newReviewSession(t, `{"risk_level":"high","user_authorization":"unknown","outcome":"deny","rationale":"Deletes the home directory."}`)
	approval := tool.Approval{Message: "❯ rm -rf ~", Action: map[string]any{"type": "shell_command", "text": "rm -rf ~"}}
	var verdict tool.Verdict
	for i := range reviewConsecutiveDenials {
		var err error
		if verdict, err = a.review(ctx, approval); err != nil {
			t.Fatal(err)
		}
		if verdict.Allowed || !strings.Contains(verdict.Reason, "Deletes the home directory.") {
			t.Fatalf("review %d = %+v", i, verdict)
		}
	}
	if !strings.Contains(verdict.Reason, "turn was interrupted") {
		t.Fatalf("breaker did not trip: %q", verdict.Reason)
	}
	if len(ui.confirms) != 0 {
		t.Fatalf("user was asked: %v", ui.confirms)
	}
	if !strings.Contains((*inputs)[0], a.workspace.RootPath) {
		t.Fatalf("planned action lacks workspace:\n%s", (*inputs)[0])
	}
}

func TestReviewAllowsAndResetsConsecutiveDenials(t *testing.T) {
	a, _, ctx, _ := newReviewSession(t, `{"risk_level":"medium","user_authorization":"medium","outcome":"allow","rationale":"Removes a build directory."}`)
	verdict, err := a.review(ctx, tool.Approval{Message: "❯ rm -rf build", Action: map[string]any{"type": "shell_command", "text": "rm -rf build"}})
	if err != nil || !verdict.Allowed {
		t.Fatalf("verdict = %+v, %v", verdict, err)
	}
}

func TestReviewFallsBackToUserWhenReviewFails(t *testing.T) {
	a, ui, ctx, _ := newReviewSession(t, "")
	verdict, err := a.review(ctx, tool.Approval{Message: "❯ git push --force", Action: map[string]any{"type": "shell_command", "text": "git push --force"}})
	if err != nil || !verdict.Allowed {
		t.Fatalf("verdict = %+v, %v", verdict, err)
	}
	if len(ui.confirms) != 1 || !strings.Contains(ui.confirms[0], "git push --force") || !strings.Contains(ui.confirms[0], "Automatic review failed") {
		t.Fatalf("confirms = %v", ui.confirms)
	}
}

func TestUnattendedSkipsReview(t *testing.T) {
	a, ui, ctx, inputs := newReviewSession(t, "")
	if err := a.SetUnattended(ctx, code.SessionIDFromContext(ctx), true); err != nil {
		t.Fatal(err)
	}
	verdict, err := a.review(ctx, tool.Approval{Message: "❯ rm -rf ~"})
	if err != nil || !verdict.Allowed || len(*inputs) != 0 || len(ui.confirms) != 0 {
		t.Fatalf("verdict = %+v, %v; reviews %d, confirms %v", verdict, err, len(*inputs), ui.confirms)
	}
}

func TestReviewBreakerCountsRecentDenials(t *testing.T) {
	var b reviewBreaker
	tripped := false
	for i := range reviewRecentDenials {
		_, _, tripped = b.record(true)
		if i < reviewConsecutiveDenials-1 && tripped {
			t.Fatalf("tripped after %d denials", i+1)
		}
		b.record(false)
	}
	if _, recent, tripped := b.record(true); !tripped && recent < reviewRecentDenials {
		t.Fatalf("recent denials = %d, breaker did not trip", recent)
	}
	b.reset()
	if _, _, tripped := b.record(true); tripped {
		t.Fatal("reset breaker tripped on the first denial")
	}
}
