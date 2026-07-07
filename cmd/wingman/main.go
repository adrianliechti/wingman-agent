package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
)

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if len(os.Args) < 2 {
		runTUI(ctx, "")
		return
	}

	switch os.Args[1] {
	case "--help", "-h", "help":
		printHelp(os.Stdout)
		return
	case "server":
		runServer(ctx)
		return
	case "acp":
		runACP(ctx)
		return
	case "claw":
		runClaw(ctx)
		return
	case "proxy":
		if os.Getenv("WINGMAN_URL") != "" {
			runProxy(ctx)
			return
		}
	case "run":
		runRun(ctx)
		return
	case "--resume":
		sessionID := "latest"
		if len(os.Args) > 2 {
			sessionID = os.Args[2]
		}
		runTUI(ctx, sessionID)
		return
	}

	runTUI(ctx, "")
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `wingman — AI coding agent

Usage:
  wingman [--resume [id]]      Launch the agent TUI
  wingman server [-port N] [-no-browser]  Run the web UI server
  wingman acp [target]         Run as an ACP stdio server (wingman | claude | codex | pi)
  wingman claw [--plain]      Run the claw multi-agent runner (TUI; plain REPL when piped or with --plain)
  wingman proxy [-port N]      Run the API proxy + dashboard (requires WINGMAN_URL)
  wingman run <target> [args]  Run an external agent through wingman

Run targets:
  claude, claude-desktop, codex, copilot, gemini, goose, junie, opencode, pi

Flags:
  --resume [id]   Resume the latest (or specified) saved session
  --help, -h      Show this help
`)
}
