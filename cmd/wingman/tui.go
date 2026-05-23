package main

import (
	"context"
	"os"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	coder "github.com/adrianliechti/wingman-agent/pkg/code/agent"
	codetui "github.com/adrianliechti/wingman-agent/pkg/tui/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func runTUI(ctx context.Context, sessionID string) {
	theme.Auto()

	wd, err := os.Getwd()
	if err != nil {
		fatal(err)
	}

	cfg, err := agent.DefaultConfig()
	if err != nil {
		fatal(err)
	}

	ws, err := code.NewWorkspace(wd)
	if err != nil {
		fatal(err)
	}
	// The coder.Agent's Close() doesn't tear down the workspace
	// (server shares the workspace across sessions); the TUI owns it
	// here so closing happens in the right order on shutdown.
	defer ws.Close()

	wa := coder.New(ws, cfg, nil)

	// Resolve "latest" / explicit id to a loaded session, or fresh one.
	if sessionID == "latest" {
		sessions, err := wa.ListSessions(ctx)
		if err != nil {
			fatal(err)
		}
		if len(sessions) > 0 {
			sessionID = sessions[0].ID
		} else {
			sessionID = ""
		}
	}
	if sessionID != "" {
		if err := wa.LoadSession(ctx, sessionID); err != nil {
			fatal(err)
		}
	} else {
		sessionID, err = wa.NewSession(ctx)
		if err != nil {
			fatal(err)
		}
	}

	if err := codetui.New(ctx, wa, sessionID).Run(); err != nil {
		fatal(err)
	}
}
