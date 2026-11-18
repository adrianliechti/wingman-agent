package acp

import (
	"encoding/json"
	"errors"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

// An agent's "resource not found" on loading a session tells callers the
// session is gone; other failures keep their own error.
func TestLoadingASessionTheAgentLacksIsNotFound(t *testing.T) {
	if err := notFound(acp.SessionNotFound("lost"), "lost"); !errors.Is(err, code.ErrSessionNotFound) {
		t.Fatalf("resource not found: %v", err)
	}
	other := acpsdk.NewInternalError(map[string]any{"error": "disk full"})
	if err := notFound(other, "kept"); err != other || errors.Is(err, code.ErrSessionNotFound) {
		t.Fatalf("an internal error: %v", err)
	}
	if err := notFound(nil, "loaded"); err != nil {
		t.Fatalf("a load that succeeded: %v", err)
	}
}

// Closing an agent that never started does nothing.
func TestClosingAnAgentThatNeverStarted(t *testing.T) {
	var a *Agent
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

// An agent that wants a login answers "authentication required"; the user
// reads how to log in, from the methods it offered, as Copilot's.
func TestALoginErrorSaysHowToLogIn(t *testing.T) {
	var initialized acpsdk.InitializeResponse
	if err := json.Unmarshal([]byte(`{"protocolVersion":1,"agentInfo":{"name":"Copilot","title":"Copilot","version":"1"},"authMethods":[{"id":"copilot-login","name":"Log in with Copilot CLI","description":"Run `+"`copilot login`"+` in the terminal"}]}`), &initialized); err != nil {
		t.Fatal(err)
	}
	login, method := loginHint(initialized.AuthMethods)
	if method != "copilot-login" {
		t.Fatalf("login method = %q", method)
	}
	a := &Agent{def: code.AgentDef{Name: "copilot"}, login: login, loginMethod: method, title: agentTitle(initialized.AgentInfo)}
	err := a.loginError(&acpsdk.RequestError{Code: authRequired, Message: "Authentication required"})
	if want := "Copilot needs you to log in: Run `copilot login` in the terminal"; err == nil || err.Error() != want {
		t.Fatalf("login error = %v, want %q", err, want)
	}
	other := errors.New("disk full")
	if a.loginError(other) != other {
		t.Fatal("another error changed")
	}
}
