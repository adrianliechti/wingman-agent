package server

import (
	"context"
	"errors"

	"github.com/adrianliechti/wingman-agent/pkg/dap"
	"github.com/adrianliechti/wingman-agent/pkg/terminal"
)

// LaunchTerminal implements dap.TerminalLauncher with Wingman's existing PTY
// service. DAP owns the reviewed command; the browser terminal API remains
// limited to the user's configured shells.
func (s *Server) LaunchTerminal(_ context.Context, launch dap.TerminalLaunch) (dap.TerminalProcess, error) {
	if s.terminals == nil || !terminal.Supported() {
		return nil, errors.New("terminals are not supported on this host")
	}
	// Each debug run replaces earlier finished debuggee terminals so they do
	// not accumulate up to the terminal limit.
	for _, session := range s.terminals.List() {
		if session.IsCommand() && session.Exited() {
			s.terminals.Remove(session.ID())
		}
	}
	process, err := s.terminals.CreateCommand(terminal.CommandSpec{
		Path:  launch.Path,
		Args:  launch.Args,
		Dir:   launch.Dir,
		Env:   launch.Env,
		Title: launch.Title,
	}, terminal.DefaultCols, terminal.DefaultRows)
	if err != nil {
		return nil, err
	}
	return process, nil
}
