package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/code/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code/agents"
	"github.com/adrianliechti/wingman-agent/pkg/telemetry"
)

// agentHost owns the CLI workspace and telemetry; telemetry flushes after the agent and workspace close.
type agentHost struct {
	agent     code.Agent
	workspace *code.Workspace
	telemetry *telemetry.Telemetry
	closeOnce sync.Once
	closeErr  error
}

func newAgentHost(ctx context.Context, workDir, name string) (*agentHost, error) {
	ws, err := code.NewWorkspace(workDir)
	if err != nil {
		return nil, err
	}
	host := &agentHost{workspace: ws}
	if id := agents.ID(name); id == "" || id == code.BuiltinAgentName {
		var cfg agent.Config
		if cfg, err = agent.DefaultConfig(); err == nil {
			host.telemetry = cfg.Telemetry
			host.agent = agent.New(ws, cfg, nil)
		}
	} else {
		host.agent, err = agents.NewExternal(ctx, ws, id)
	}
	if err != nil {
		_ = host.Close()
		return nil, err
	}
	return host, nil
}

func (h *agentHost) Close() error {
	h.closeOnce.Do(func() {
		if h.agent != nil {
			h.closeErr = h.agent.Close()
		}
		h.workspace.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.closeErr = errors.Join(h.closeErr, h.telemetry.Shutdown(ctx))
	})
	return h.closeErr
}
