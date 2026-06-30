package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	acpclaude "github.com/adrianliechti/wingman-agent/pkg/acp/claude"
	acpcodex "github.com/adrianliechti/wingman-agent/pkg/acp/codex"
	acppi "github.com/adrianliechti/wingman-agent/pkg/acp/pi"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/acp"
	"github.com/adrianliechti/wingman-agent/pkg/external/claude"
	"github.com/adrianliechti/wingman-agent/pkg/external/codex"
	extpi "github.com/adrianliechti/wingman-agent/pkg/external/pi"
)

type agentRegistration struct {
	ID          string
	Name        string
	Constructor func(ctx context.Context, ws *code.Workspace) (code.Agent, error)
}

func agentID(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), " ", "-")
}

func (s *Server) availableAgents() []agentRegistration {
	seen := map[string]bool{code.BuiltinAgentName: true}
	out := make([]agentRegistration, 0, 4)

	if !code.HasAgentsConfig() {
		for _, r := range detectAgents() {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
		}
		return out
	}

	for _, def := range code.LoadAgents() {
		id := agentID(def.Name)
		if seen[id] {
			continue
		}
		seen[id] = true
		name := def.Name
		def.Name = id
		out = append(out, agentRegistration{
			ID:   id,
			Name: name,
			Constructor: func(_ context.Context, ws *code.Workspace) (code.Agent, error) {
				return acp.New(ws, def)
			},
		})
	}
	return out
}

func detectAgents() []agentRegistration {
	var out []agentRegistration

	if _, err := claude.FindPath(); err == nil {
		out = append(out, agentRegistration{
			ID:          "claude",
			Name:        "Claude",
			Constructor: claudeBackend,
		})
	}
	if _, err := exec.LookPath("codex"); err == nil {
		out = append(out, agentRegistration{
			ID:          "codex",
			Name:        "Codex",
			Constructor: codexBackend,
		})
	}
	if path, err := exec.LookPath("copilot"); err == nil {
		out = append(out, agentRegistration{
			ID:   "copilot",
			Name: "Copilot",
			Constructor: func(_ context.Context, ws *code.Workspace) (code.Agent, error) {
				return acp.New(ws, code.AgentDef{
					Name:    "copilot",
					Command: path,
					Args:    []string{"--acp", "--stdio"},
				})
			},
		})
	}
	if path, err := exec.LookPath("opencode"); err == nil {
		out = append(out, agentRegistration{
			ID:   "opencode",
			Name: "OpenCode",
			Constructor: func(_ context.Context, ws *code.Workspace) (code.Agent, error) {
				return acp.New(ws, code.AgentDef{
					Name:    "opencode",
					Command: path,
					Args:    []string{"acp"},
				})
			},
		})
	}
	if _, err := exec.LookPath(extpi.BinPath()); err == nil {
		out = append(out, agentRegistration{
			ID:          "pi",
			Name:        "Pi",
			Constructor: piBackend,
		})
	}
	return out
}

func claudeBackend(ctx context.Context, ws *code.Workspace) (code.Agent, error) {
	cfg, err := claude.NewConfig(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claude config: %w", err)
	}
	srv := acpclaude.New(acpclaude.Options{
		Cwd: ws.RootPath,
		Env: claude.BuildEnv(os.Environ(), cfg),
	})
	return acp.NewInProcess(ws, "claude", srv, func(conn *acpsdk.AgentSideConnection) {
		srv.SetAgentConnection(conn)
	}, srv.Close)
}

func piBackend(ctx context.Context, ws *code.Workspace) (code.Agent, error) {
	cfg, err := extpi.NewConfig(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pi config: %w", err)
	}

	dir, err := extpi.ConfigDir()
	if err != nil {
		return nil, fmt.Errorf("pi config dir: %w", err)
	}
	if err := extpi.WriteModels(dir, cfg); err != nil {
		return nil, err
	}

	srv := acppi.New(acppi.Options{
		Path:        extpi.BinPath(),
		Dir:         ws.RootPath,
		Env:         extpi.BuildEnv(os.Environ(), dir),
		Args:        extpi.BuildArgs(cfg),
		SessionsDir: extpi.SessionsDir(dir),
	})

	return acp.NewInProcess(ws, "pi", srv, func(conn *acpsdk.AgentSideConnection) {
		srv.SetAgentConnection(conn)
	}, srv.Close)
}

func codexBackend(ctx context.Context, ws *code.Workspace) (code.Agent, error) {
	cfg, err := codex.NewConfig(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("codex config: %w", err)
	}
	srv, err := acpcodex.Spawn(ctx, acpcodex.Options{
		Dir:       ws.RootPath,
		Env:       codex.BuildEnv(os.Environ(), cfg),
		ExtraArgs: codex.BuildArgs(cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("codex spawn: %w", err)
	}
	return acp.NewInProcess(ws, "codex", srv, func(conn *acpsdk.AgentSideConnection) {
		srv.SetAgentConnection(conn)
	}, srv.Close)
}
