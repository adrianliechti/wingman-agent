package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/adrianliechti/wingman-agent/server"
)

func runServer(ctx context.Context) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	port := fs.Int("port", 0, fmt.Sprintf("port to listen on (default: %d, falls back to random if taken)", server.DefaultPort))
	noBrowser := fs.Bool("no-browser", false, "do not open browser on startup")
	fs.Parse(os.Args[2:])

	wd, err := os.Getwd()
	if err != nil {
		fatal(err)
	}

	srv, err := server.New(ctx, wd, &server.ServerOptions{
		NoBrowser: *noBrowser,
	})
	if err != nil {
		fatal(err)
	}
	defer srv.Close()

	if err := srv.Run(ctx, *port); err != nil {
		fatal(err)
	}
}
