package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	acp "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

// Run starts the ACP stdio server and blocks until the peer disconnects or
// ctx is cancelled. All live sessions are closed before returning.
//
// in/out are parameters (not os.Stdin/os.Stdout) so callers can wire pipes
// for tests; the binary's main passes the real stdio handles.
func Run(ctx context.Context, in io.Reader, out io.Writer) error {
	s := &Server{sessions: map[acp.SessionId]*code.Agent{}}
	s.conn = acp.NewAgentSideConnection(s, out, in)
	s.conn.SetLogger(slog.Default())

	select {
	case <-s.conn.Done():
	case <-ctx.Done():
	}

	s.mu.Lock()
	for _, a := range s.sessions {
		a.Close()
	}
	s.mu.Unlock()

	return nil
}

type Server struct {
	conn *acp.AgentSideConnection

	mu       sync.Mutex
	sessions map[acp.SessionId]*code.Agent
}

func (s *Server) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
	}, nil
}

func (s *Server) Authenticate(_ context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (s *Server) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a, err := code.New(params.Cwd, noopUI{})
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	// Synchronous warmup: an immediately-following Prompt would otherwise
	// race ahead of LSP/MCP tool registration.
	a.WarmUp()
	if err := a.InitMCP(ctx); err != nil {
		a.Close()
		return acp.NewSessionResponse{}, err
	}

	sid := acp.SessionId(uuid.NewString())

	s.mu.Lock()
	s.sessions[sid] = a
	s.mu.Unlock()

	resp := acp.NewSessionResponse{SessionId: sid}
	if available := availableModels(); len(available) > 0 {
		current := string(available[0].ModelId)
		setAgentModel(a, current)
		resp.Models = &acp.SessionModelState{
			AvailableModels: available,
			CurrentModelId:  acp.ModelId(current),
		}
	}
	resp.ConfigOptions = sessionConfigOptions(a)
	return resp, nil
}

// UnstableSetSessionModel handles the (experimental) session/set_model
// request. Picked up by the SDK via a structural type assertion, so we
// don't need to implement the full acp.AgentExperimental interface.
func (s *Server) UnstableSetSessionModel(_ context.Context, params acp.UnstableSetSessionModelRequest) (acp.UnstableSetSessionModelResponse, error) {
	s.mu.Lock()
	a := s.sessions[params.SessionId]
	s.mu.Unlock()
	if a == nil {
		return acp.UnstableSetSessionModelResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}

	setAgentModel(a, string(params.ModelId))
	return acp.UnstableSetSessionModelResponse{}, nil
}

func (s *Server) CloseSession(_ context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	s.mu.Lock()
	a := s.sessions[params.SessionId]
	delete(s.sessions, params.SessionId)
	s.mu.Unlock()

	if a != nil {
		a.Close()
	}
	return acp.CloseSessionResponse{}, nil
}

// Cancel is a no-op: the SDK already cancels the in-flight Prompt's context
// before invoking us. Our Prompt loop observes that via context.Canceled.
func (s *Server) Cancel(_ context.Context, _ acp.CancelNotification) error { return nil }

func (s *Server) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	s.mu.Lock()
	a := s.sessions[params.SessionId]
	s.mu.Unlock()
	if a == nil {
		return acp.PromptResponse{}, fmt.Errorf("session %s not found", params.SessionId)
	}

	notify := func(u acp.SessionUpdate) {
		_ = s.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: params.SessionId,
			Update:    u,
		})
	}

	for msg, err := range a.Send(ctx, promptToContent(params.Prompt)) {
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
			}
			// Surface as a final assistant message rather than failing the
			// whole connection — some ACP clients close on Prompt error.
			notify(acp.UpdateAgentMessageText("error: " + err.Error()))
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		for _, c := range msg.Content {
			switch {
			case c.ToolCall != nil:
				notify(acp.StartToolCall(
					acp.ToolCallId(c.ToolCall.ID),
					c.ToolCall.Name,
					acp.WithStartKind(mapKind(c.ToolCall.Name)),
					acp.WithStartStatus(acp.ToolCallStatusInProgress),
					acp.WithStartRawInput(parseRawInput(c.ToolCall.Args)),
				))
			case c.ToolResult != nil:
				notify(acp.UpdateToolCall(
					acp.ToolCallId(c.ToolResult.ID),
					acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
					acp.WithUpdateContent([]acp.ToolCallContent{
						acp.ToolContent(acp.TextBlock(c.ToolResult.Content)),
					}),
				))
			case c.Reasoning != nil && c.Reasoning.Summary != "":
				notify(acp.UpdateAgentThoughtText(c.Reasoning.Summary))
			case c.Text != "":
				notify(acp.UpdateAgentMessageText(c.Text))
			}
		}
	}

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

// Stub methods required by the acp.Agent interface.

func (s *Server) ListSessions(_ context.Context, _ acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
}

func (s *Server) ResumeSession(_ context.Context, _ acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}

// SetSessionConfigOption handles the stable session/set_config_option
// request. We expose a single option: "effort" (reasoning effort), as a
// select with values auto/low/medium/high.
func (s *Server) SetSessionConfigOption(_ context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	if params.ValueId == nil {
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("expected select value")
	}
	p := params.ValueId

	s.mu.Lock()
	a := s.sessions[p.SessionId]
	s.mu.Unlock()
	if a == nil {
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("session %s not found", p.SessionId)
	}

	switch string(p.ConfigId) {
	case "model":
		setAgentModel(a, string(p.Value))
	case "effort":
		if err := setAgentEffort(a, string(p.Value)); err != nil {
			return acp.SetSessionConfigOptionResponse{}, err
		}
	default:
		return acp.SetSessionConfigOptionResponse{}, fmt.Errorf("unknown config id: %s", p.ConfigId)
	}
	// The spec requires the full updated option list on every response —
	// strict clients (Zed) treat a missing/null configOptions as "session
	// has no options" and wipe the entire picker UI.
	return acp.SetSessionConfigOptionResponse{
		ConfigOptions: sessionConfigOptions(a),
	}, nil
}

func (s *Server) SetSessionMode(_ context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

// noopUI satisfies code.UI for the ACP mode: no in-tool Ask/Confirm prompts
// (the agent runs auto-allow), no status surface.
type noopUI struct{}

func (noopUI) Ask(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (noopUI) Confirm(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func (noopUI) StatusUpdate(_ string) {
}

// promptToContent converts ACP prompt blocks to the agent's input shape.
// We advertise no image/audio/embedded-context capabilities, so well-
// behaved clients only send Text and ResourceLink.
func promptToContent(blocks []acp.ContentBlock) []agent.Content {
	out := make([]agent.Content, 0, len(blocks))
	for _, b := range blocks {
		switch {
		case b.Text != nil:
			out = append(out, agent.Content{Text: b.Text.Text})
		case b.ResourceLink != nil:
			out = append(out, agent.Content{Text: fmt.Sprintf("[Resource: %s]", b.ResourceLink.Uri)})
		}
	}
	return out
}

// parseRawInput tries to expose tool args as a structured value to the
// client; falls back to the raw string if the agent emitted non-JSON args.
func parseRawInput(args string) any {
	if args == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err == nil {
		return v
	}
	return args
}

// availableModels exposes the curated model list (pkg/code.AvailableModels)
// to ACP clients so the IDE's model picker can render the same options the
// TUI/web UI shows.
func availableModels() []acp.ModelInfo {
	out := make([]acp.ModelInfo, 0, len(code.AvailableModels))
	for _, m := range code.AvailableModels {
		out = append(out, acp.ModelInfo{
			ModelId: acp.ModelId(m.ID),
			Name:    m.Name,
		})
	}
	return out
}

// setAgentModel installs a model selector on the agent. agent.Config.Model
// is a thunk so each turn picks up the latest value — replacing it here
// takes effect on the next Send iteration.
func setAgentModel(a *code.Agent, modelID string) {
	a.Config.Model = func() string { return modelID }
}

// sessionConfigOptions builds the full set of config options for a
// session, with each option's currentValue reflecting the agent's actual
// state. This is what every NewSession / SetSessionConfigOption response
// must carry — clients use it as the authoritative session config view.
//
// Once config_options is non-empty, strict clients (Zed) treat it as
// canonical and ignore the legacy Models/Modes fields entirely. So every
// selector wingman wants visible — model included — has to live here.
func sessionConfigOptions(a *code.Agent) []acp.SessionConfigOption {
	return []acp.SessionConfigOption{
		modelConfigOption(currentModel(a)),
		effortConfigOption(currentEffort(a)),
	}
}

// currentModel reads the agent's selected model id, or "" if unset.
func currentModel(a *code.Agent) string {
	if a.Config.Model == nil {
		return ""
	}
	return a.Config.Model()
}

// modelConfigOption advertises wingman's curated model list as a select.
func modelConfigOption(current string) acp.SessionConfigOption {
	opts := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(code.AvailableModels))
	for _, m := range code.AvailableModels {
		opts = append(opts, acp.SessionConfigSelectOption{
			Value: acp.SessionConfigValueId(m.ID),
			Name:  m.Name,
		})
	}
	return acp.SessionConfigOption{
		Select: &acp.SessionConfigOptionSelect{
			Id:           "model",
			Name:         "Model",
			CurrentValue: acp.SessionConfigValueId(current),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &opts},
		},
	}
}

// currentEffort reads the agent's reasoning-effort selection back as the
// option-value-id wingman exposes ("auto" when no override is set).
func currentEffort(a *code.Agent) string {
	if a.Config.Effort == nil {
		return "auto"
	}
	if v := a.Config.Effort(); v != "" {
		return v
	}
	return "auto"
}

// effortConfigOption advertises wingman's reasoning-effort knob to ACP
// clients as a select. v0.13+ of the SDK adds a named "thought_level"
// category constant; until then the option ships uncategorized.
func effortConfigOption(current string) acp.SessionConfigOption {
	opts := acp.SessionConfigSelectOptionsUngrouped{
		{Value: "auto", Name: "Auto"},
		{Value: "low", Name: "Low"},
		{Value: "medium", Name: "Medium"},
		{Value: "high", Name: "High"},
	}
	return acp.SessionConfigOption{
		Select: &acp.SessionConfigOptionSelect{
			Id:           "effort",
			Name:         "Effort",
			CurrentValue: acp.SessionConfigValueId(current),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &opts},
		},
	}
}

// setAgentEffort applies a reasoning-effort selection to the agent.
// Valid values mirror the web UI's POST /api/effort handler at
// server/server.go:496.
func setAgentEffort(a *code.Agent, effort string) error {
	switch effort {
	case "", "auto":
		a.Config.Effort = nil
	case "low", "medium", "high":
		v := effort
		a.Config.Effort = func() string { return v }
	default:
		return fmt.Errorf("effort must be auto, low, medium, or high (got %q)", effort)
	}
	return nil
}

// mapKind classifies wingman tools onto ACP's ToolKind enum so IDEs can
// pick appropriate icons / grouping. Cosmetic — mismatches don't affect
// behavior. Tool names match pkg/agent/tool/{fs,shell,fetch,search}/.
func mapKind(name string) acp.ToolKind {
	switch name {
	case "read", "ls", "find":
		return acp.ToolKindRead
	case "grep", "search_online":
		return acp.ToolKindSearch
	case "fetch":
		return acp.ToolKindFetch
	case "write", "edit":
		return acp.ToolKindEdit
	case "shell":
		return acp.ToolKindExecute
	default:
		return acp.ToolKindOther
	}
}
