package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	codetui "github.com/adrianliechti/wingman-agent/pkg/tui/code"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

type tuiOptions struct {
	Agent     string
	SessionID string
	Fork      bool
}

func latestSessionID(sessions []code.SessionInfo) string {
	id := ""
	var updated time.Time
	for _, s := range sessions {
		if id == "" || s.UpdatedAt.After(updated) {
			id = s.ID
			updated = s.UpdatedAt
		}
	}
	return id
}

func runTUI(ctx context.Context, opts tuiOptions) (err error) {
	theme.Auto()

	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	host, err := newAgentHost(ctx, wd, opts.Agent)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, host.Close()) }()
	wa := host.agent

	sessionID := opts.SessionID
	if sessionID == "latest" {
		sessions, err := wa.ListSessions(ctx)
		if err != nil {
			return err
		}
		sessionID = latestSessionID(sessions)
	}

	if sessionID != "" && opts.Fork {
		forker, ok := wa.(code.SessionForker)
		if !ok {
			return fmt.Errorf("agent %q cannot fork sessions", opts.Agent)
		}
		sessionID, err = forker.ForkSession(ctx, sessionID, "")
		if err != nil {
			return err
		}
	} else if sessionID != "" {
		if err := wa.LoadSession(ctx, sessionID); err != nil {
			return err
		}
	} else {
		sessionID, err = wa.NewSession(ctx)
		if err != nil {
			return err
		}
	}

	app := codetui.New(ctx, wa, sessionID)
	return app.Run()
}
