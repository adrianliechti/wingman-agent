package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/devtools"
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
	ws, wa := host.workspace, host.agent

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
	toolUpdate := ws.StartManagedToolsUpdate(ctx, code.ManagedLSPTools, func(progress devtools.Progress) {
		label := progress.Label
		if label == "" {
			label = progress.Tool
		}
		message := "Installing " + label
		if progress.Phase == devtools.ProgressUpdating {
			message = "Updating " + label
		}
		if progress.Total > 0 {
			message = fmt.Sprintf("%s (%d/%d)", message, progress.Current, progress.Total)
		}
		app.SetBackgroundStatus(message, false)
	})
	defer toolUpdate.Cancel()
	go func() {
		_, updateErr := toolUpdate.Wait()
		if devtools.IsUnavailable(updateErr) {
			tools := devtools.ToolLabels(devtools.UnavailableTools(updateErr))
			message := "Automatic development-tool setup could not finish"
			if len(tools) > 0 {
				message = "Could not install " + strings.Join(tools, ", ")
			}
			app.SetBackgroundStatus(message+"; project and system tools still work", true)
			return
		}
		app.SetBackgroundStatus("", false)
	}()

	return app.Run()
}
