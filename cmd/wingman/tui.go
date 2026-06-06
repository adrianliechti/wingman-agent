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

	defer ws.Close()

	wa := coder.New(ws, cfg, nil)

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

	app := codetui.New(ctx, wa, sessionID)

	wa.SetUI(app)

	if sessionID != "" {
		if err := wa.LoadSession(ctx, sessionID); err != nil {
			fatal(err)
		}
	} else {
		sessionID, err = wa.NewSession(ctx)
		if err != nil {
			fatal(err)
		}
		app.SetSessionID(sessionID)
	}

	if err := app.Run(); err != nil {
		fatal(err)
	}
}
