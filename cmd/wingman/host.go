package main

import (
	"context"
	"fmt"
	"os"

	"github.com/adrianliechti/wingman-agent/server"
)

func runHost(ctx context.Context, args []string) {
	var port int
	var root string
	token := os.Getenv("WINGMAN_HOST_TOKEN")

	fs := newFlags("wingman host")
	fs.Int(&port, "--port N", fmt.Sprintf("port to listen on (default: %d, falls back to random if taken)", server.DefaultHostPort))
	fs.String(&root, "--root PATH", "directory projects may be opened in (default: home directory)")
	fs.String(&token, "--token TOKEN", "token clients must connect with (or WINGMAN_HOST_TOKEN)")

	if err := fs.Parse(args); err != nil {
		fatal(err)
	}

	opts := &server.HostOptions{Token: token}
	if root != "" {
		opts.Roots = []string{root}
	}
	host, err := server.NewHost(ctx, opts)
	if err != nil {
		fatal(err)
	}
	defer host.Close()

	if err := host.Run(ctx, port); err != nil {
		fatal(err)
	}
}
