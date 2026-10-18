package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook"
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
	a, ui, sessionCtx, inputs := newReviewSession(t, `{"risk_level":"high","user_authorization":"unknown","outcome":"deny","rationale":"Deletes the home directory."}`)
	approval := tool.Approval{Message: "❯ rm -rf ~", Action: map[string]any{"type": "shell_command", "text": "rm -rf ~"}}
	for range reviewConsecutiveDenials {
		if verdict, err := a.review(sessionCtx, approval); err != nil || strings.Contains(verdict.Reason, "turn was interrupted") {
			t.Fatalf("denial outside a turn tripped the breaker: %+v, %v", verdict, err)
		}
	}
	ctx := hook.WithRuntime(sessionCtx, hook.Runtime{TurnID: "turn-1"})
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
	// The planned action is JSON-encoded, so Windows path separators are escaped.
	workspace, _ := json.Marshal(a.workspace.RootPath)
	if !strings.Contains((*inputs)[0], strings.Trim(string(workspace), `"`)) {
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

func TestShellApprovalDoesNotSurviveLeavingUnattendedMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script")
	}
	a, ui, ctx, inputs := newReviewSession(t, `{"risk_level":"high","user_authorization":"unknown","outcome":"deny","rationale":"Preserve new user work."}`)
	id := code.SessionIDFromContext(ctx)
	var exec tool.Tool
	for _, candidate := range a.session(id).toolSet.Slice() {
		if candidate.Name == "exec" {
			exec = candidate
			break
		}
	}
	if exec.Execute == nil {
		t.Fatal("session has no exec tool")
	}
	if err := a.SetUnattended(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"command": "rm -rf fixture", "wait": 10}
	result, err := exec.Execute(ctx, args)
	if err != nil || result.IsError || len(*inputs) != 0 || len(ui.confirms) != 0 {
		t.Fatalf("unattended execution: %+v, %v; reviews=%d", result, err, len(*inputs))
	}
	marker := filepath.Join(a.workspace.RootPath, "fixture")
	if err := os.WriteFile(marker, []byte("new user work"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.SetUnattended(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Execute(ctx, args); err == nil || !strings.Contains(err.Error(), "Preserve new user work") {
		t.Fatalf("unattended approval bypassed review: %v", err)
	}
	if len(*inputs) != 1 || len(ui.confirms) != 0 {
		t.Fatalf("reviews=%d, confirms=%v", len(*inputs), ui.confirms)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "new user work" {
		t.Fatalf("denied command changed user work: %q, %v", data, err)
	}
}

func TestReviewBreakerCountsRecentDenials(t *testing.T) {
	var b reviewBreaker
	tripped := false
	for i := range reviewRecentDenials {
		_, _, tripped = b.record("turn-1", true)
		if i < reviewConsecutiveDenials-1 && tripped {
			t.Fatalf("tripped after %d denials", i+1)
		}
		b.record("turn-1", false)
	}
	if _, recent, tripped := b.record("turn-1", true); !tripped && recent < reviewRecentDenials {
		t.Fatalf("recent denials = %d, breaker did not trip", recent)
	}
	if _, _, tripped := b.record("turn-2", true); tripped {
		t.Fatal("a new turn inherited the previous turn's denials")
	}
	var interleaved reviewBreaker
	interleaved.record("turn-main", true)
	interleaved.record("turn-main", true)
	interleaved.record("bg:turn-sub", false)
	if _, _, tripped := interleaved.record("turn-main", true); !tripped {
		t.Fatal("interleaved background review reset the main turn's consecutive denials")
	}
	for range reviewConsecutiveDenials {
		if _, _, tripped := b.record("", true); tripped {
			t.Fatal("denials outside a turn tripped the breaker")
		}
	}
}

type recordingTrust map[string]bool

func (r recordingTrust) Trusted(fingerprint string) bool { return r[fingerprint] }

func (r recordingTrust) Trust(fingerprint string) error {
	r[fingerprint] = true
	return nil
}

func TestUnattendedHookApprovalIsNotRemembered(t *testing.T) {
	store := recordingTrust{}
	s := &sessionState{}
	trust := attendedTrust{TrustStore: store, session: s}
	s.unattended.Store(true)
	if err := trust.Trust("sha256:auto"); err != nil || store["sha256:auto"] {
		t.Fatalf("unattended approval persisted: %v %v", store, err)
	}
	s.unattended.Store(false)
	if err := trust.Trust("sha256:asked"); err != nil || !store["sha256:asked"] {
		t.Fatalf("attended approval not persisted: %v %v", store, err)
	}
}
