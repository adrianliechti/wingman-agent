package acp

import (
	"context"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
)

// loginAgent wants a login, as Copilot does: the user logs in outside it,
// and it takes the login up when the client authenticates.
type loginAgent struct {
	*adapterProtocolAgent
	loggedIn bool // the user ran `copilot login`
	taken    bool // the agent read that login
	methods  []string
}

func (a *loginAgent) Initialize(ctx context.Context, req acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	resp, err := a.adapterProtocolAgent.Initialize(ctx, req)
	description := "Run `copilot login` in the terminal"
	resp.AuthMethods = []acpsdk.AuthMethod{{Agent: &acpsdk.AuthMethodAgent{Id: "copilot-login", Name: "Log in with Copilot CLI", Description: &description}}}
	resp.AgentInfo = &acpsdk.Implementation{Name: "Copilot"}
	return resp, err
}

func (a *loginAgent) Authenticate(_ context.Context, req acpsdk.AuthenticateRequest) (acpsdk.AuthenticateResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.methods = append(a.methods, string(req.MethodId))
	if !a.loggedIn {
		return acpsdk.AuthenticateResponse{}, acpsdk.NewAuthRequired(nil)
	}
	a.taken = true
	return acpsdk.AuthenticateResponse{}, nil
}

func (a *loginAgent) NewSession(ctx context.Context, req acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	a.mu.Lock()
	taken := a.taken
	a.mu.Unlock()
	if !taken {
		return acpsdk.NewSessionResponse{}, acpsdk.NewAuthRequired(nil)
	}
	return a.adapterProtocolAgent.NewSession(ctx, req)
}

// A login the user made after the agent started counts: the agent is asked
// to take it up, and the session starts.
func TestAnAgentTakesUpALoginMadeOutsideIt(t *testing.T) {
	remote := &loginAgent{adapterProtocolAgent: &adapterProtocolAgent{}}
	a := newAdapterForTest(t, remote)
	if _, err := a.NewSession(t.Context()); err == nil || !strings.Contains(err.Error(), "Copilot needs you to log in: Run `copilot login`") {
		t.Fatalf("before the login: %v", err)
	}
	remote.mu.Lock()
	remote.loggedIn = true
	remote.mu.Unlock()
	if _, err := a.NewSession(t.Context()); err != nil {
		t.Fatalf("after the login: %v", err)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if len(remote.methods) != 2 || remote.methods[1] != "copilot-login" {
		t.Fatalf("authenticate calls: %v", remote.methods)
	}
}
