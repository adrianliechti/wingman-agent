package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	acpsdk "github.com/coder/acp-go-sdk"

	acpclaude "github.com/adrianliechti/wingman-agent/pkg/acp/claude"
	acpcodex "github.com/adrianliechti/wingman-agent/pkg/acp/codex"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/acp"
	"github.com/adrianliechti/wingman-agent/pkg/external/claude"
	"github.com/adrianliechti/wingman-agent/pkg/external/codex"
)

// agentRegistration is a single named code.Agent backend selectable from
// the Web UI. The Constructor is lazy: nothing is spawned until the user
// actually selects this entry.
type agentRegistration struct {
	Name        string
	Constructor func(ctx context.Context, ws *code.Workspace) (code.Agent, error)
}

// availableAgents merges built-in detection with the user's
// ~/.wingman/agents.json. User-defined entries override detected entries
// on Name collision so users can opt out of a baked-in wiring just by
// declaring their own.
func (s *Server) availableAgents() []agentRegistration {
	detected := detectAgents()

	userDefs := code.LoadAgents()
	override := make(map[string]bool, len(userDefs))
	for _, d := range userDefs {
		override[d.Name] = true
	}

	out := make([]agentRegistration, 0, len(detected)+len(userDefs))
	for _, r := range detected {
		if override[r.Name] {
			continue
		}
		out = append(out, r)
	}
	for _, d := range userDefs {
		def := d
		out = append(out, agentRegistration{
			Name: def.Name,
			Constructor: func(_ context.Context, ws *code.Workspace) (code.Agent, error) {
				return acp.New(ws, def)
			},
		})
	}
	return out
}

// detectAgents enumerates the built-in CLI wrappers the host can run
// today: claude / codex via in-process ACP, copilot via its own native
// ACP stdio. Missing CLIs are silently skipped.
func detectAgents() []agentRegistration {
	var out []agentRegistration

	if _, err := claude.FindPath(); err == nil {
		out = append(out, agentRegistration{
			Name:        "Claude",
			Constructor: claudeBackend,
		})
	}
	if _, err := exec.LookPath("codex"); err == nil {
		out = append(out, agentRegistration{
			Name:        "Codex",
			Constructor: codexBackend,
		})
	}
	if path, err := exec.LookPath("copilot"); err == nil {
		out = append(out, agentRegistration{
			Name: "Copilot",
			Constructor: func(_ context.Context, ws *code.Workspace) (code.Agent, error) {
				return acp.New(ws, code.AgentDef{
					Name:    "Copilot",
					Command: path,
					Args:    []string{"--acp", "--stdio"},
				})
			},
		})
	}
	if path, err := exec.LookPath("opencode"); err == nil {
		out = append(out, agentRegistration{
			Name: "OpenCode",
			Constructor: func(_ context.Context, ws *code.Workspace) (code.Agent, error) {
				return acp.New(ws, code.AgentDef{
					Name:    "OpenCode",
					Command: path,
					Args:    []string{"acp"},
				})
			},
		})
	}
	return out
}

func claudeBackend(ctx context.Context, ws *code.Workspace) (code.Agent, error) {
	cfg, err := claude.NewConfig(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claude config: %w", err)
	}
	path, err := claude.FindPath()
	if err != nil {
		return nil, fmt.Errorf("claude path: %w", err)
	}
	srv := acpclaude.New(acpclaude.Options{
		Cwd:  ws.RootPath,
		Env:  claude.BuildEnv(os.Environ(), cfg),
		Path: path,
	})
	return acp.NewInProcess(ws, "Claude", srv, func(conn *acpsdk.AgentSideConnection) {
		srv.SetAgentConnection(conn)
	}, srv.Close)
}

// codexBackend spawns `codex app-server` (managed by codex.Spawn) and
// wires it in-process. The returned code.Agent's Close terminates the
// codex subprocess via the cleanup callback.
func codexBackend(ctx context.Context, ws *code.Workspace) (code.Agent, error) {
	cfg, err := codex.NewConfig(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("codex config: %w", err)
	}
	srv, err := acpcodex.Spawn(ctx, "codex", acpcodex.Options{
		Env:       codex.BuildEnv(os.Environ(), cfg),
		ExtraArgs: codex.BuildArgs(cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("codex spawn: %w", err)
	}
	return acp.NewInProcess(ws, "Codex", srv, func(conn *acpsdk.AgentSideConnection) {
		srv.SetAgentConnection(conn)
	}, srv.Close)
}
