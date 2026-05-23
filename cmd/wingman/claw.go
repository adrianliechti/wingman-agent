package main

import (
	"context"

	"github.com/adrianliechti/wingman-agent/pkg/claw"
	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
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

	cfg.Channels = []channel.Channel{clawtui.New(c)}

	if err := c.Run(ctx); err != nil {
		fatal(err)
	}
}
