package main

import (
	"context"
	"os"
	"slices"

	"golang.org/x/term"

	"github.com/adrianliechti/wingman-agent/pkg/claw"
	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
	"github.com/adrianliechti/wingman-agent/pkg/claw/channel/cli"
	clawtui "github.com/adrianliechti/wingman-agent/pkg/tui/claw"
)

func runClaw(ctx context.Context) {
	cfg, cleanup, err := claw.DefaultConfig()
	if err != nil {
		fatal(err)
	}
	defer cleanup()

	c := claw.New(cfg)

	if err := c.Init(); err != nil {
		fatal(err)
	}
	defer c.Close()

	if clawPlainMode() {
		cfg.Channels = []channel.Channel{cli.New()}
	} else {
		cfg.Channels = []channel.Channel{clawtui.New(c)}
	}

	if err := c.Run(ctx); err != nil {
		fatal(err)
	}
}

func clawPlainMode() bool {
	if len(os.Args) > 2 && slices.ContainsFunc(os.Args[2:], func(a string) bool {
		return a == "-plain" || a == "--plain"
	}) {
		return true
	}

	return !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd()))
}
