package main

import (
	"context"
	"flag"
	"os"

	"github.com/adrianliechti/wingman-agent/pkg/tui/proxy"
)

func runProxy(ctx context.Context) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	port := fs.Int("port", 4242, "port to listen on")
	fs.Parse(os.Args[2:])

	if err := proxy.Run(ctx, proxy.Options{Port: *port}); err != nil {
		fatal(err)
	}
}
