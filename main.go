package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/adrianliechti/wingman-agent/server"
	clawtui "github.com/adrianliechti/wingman-agent/tui/claw"
	codetui "github.com/adrianliechti/wingman-agent/tui/code"

	"github.com/adrianliechti/wingman-agent/pkg/acp"
	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/claw"
	"github.com/adrianliechti/wingman-agent/pkg/claw/channel"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	"github.com/adrianliechti/wingman-agent/tui/proxy"

	"github.com/adrianliechti/wingman-agent/tui/run/claude"
	claudedesktop "github.com/adrianliechti/wingman-agent/tui/run/claude-desktop"
	"github.com/adrianliechti/wingman-agent/tui/run/codex"
	"github.com/adrianliechti/wingman-agent/tui/run/copilot"
	"github.com/adrianliechti/wingman-agent/tui/run/gemini"
	"github.com/adrianliechti/wingman-agent/tui/run/opencode"

	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
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

func runServer(ctx context.Context) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	port := fs.Int("port", 9000, "port to listen on")
	noBrowser := fs.Bool("no-browser", false, "do not open browser on startup")
	fs.Parse(os.Args[2:])

	wd, err := os.Getwd()
	if err != nil {
		fatal(err)
	}

	srv, err := server.New(ctx, wd, &server.ServerOptions{
		Port:      *port,
		NoBrowser: *noBrowser,
	})
	if err != nil {
		fatal(err)
	}
	defer srv.Close()

	if err := srv.Run(ctx); err != nil {
		fatal(err)
	}
}

func runACP(ctx context.Context) {
	// stdout is reserved for the ACP JSON-RPC stream — redirect any default
	// slog writers (used by transitive deps like the openai and mcp SDKs)
	// to stderr so a stray log line can't corrupt the protocol.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := acp.Run(ctx, os.Stdin, os.Stdout); err != nil {
		fatal(err)
	}
}

func runProxy(ctx context.Context) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	port := fs.Int("port", 4242, "port to listen on")
	fs.Parse(os.Args[2:])

	if err := proxy.Run(ctx, proxy.Options{Port: *port}); err != nil {
		fatal(err)
	}
}

func runRun(ctx context.Context) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Error: missing or unknown run target")
		fmt.Fprintln(os.Stderr)
		printHelp(os.Stderr)
		os.Exit(1)
	}

	var err error

	switch os.Args[2] {
	case "claude":
		err = claude.Run(ctx, os.Args[3:], nil)
	case "claude-desktop":
		err = claudedesktop.Run(ctx, os.Args[3:], nil)
	case "codex":
		err = codex.Run(ctx, os.Args[3:], nil)
	case "copilot":
		err = copilot.Run(ctx, os.Args[3:], nil)
	case "gemini":
		err = gemini.Run(ctx, os.Args[3:], nil)
	case "opencode":
		err = opencode.Run(ctx, os.Args[3:], nil)
	default:
		fmt.Fprintln(os.Stderr, "Error: missing or unknown run target")
		fmt.Fprintln(os.Stderr)
		printHelp(os.Stderr)
		os.Exit(1)
	}

	if err != nil {
		fatal(err)
	}
}

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

	c := ws.NewAgent(cfg, nil)
	sessionsDir := filepath.Join(filepath.Dir(ws.MemoryPath), "sessions")

	if sessionID == "latest" {
		sessions, err := session.List(sessionsDir)
		if err != nil {
			fatal(err)
		}
		if len(sessions) > 0 {
			sessionID = sessions[0].ID
		} else {
			sessionID = ""
		}
	}

	if sessionID != "" {
		s, err := session.Load(sessionsDir, sessionID)
		if err != nil {
			fatal(err)
		}
		c.Messages = s.State.Messages
		c.Usage = s.State.Usage
	}

	if err := codetui.New(ctx, c, sessionID).Run(); err != nil {
		fatal(err)
	}
}

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

func printHelp(w io.Writer) {
	fmt.Fprint(w, `wingman — AI coding agent

Usage:
  wingman [--resume [id]]      Launch the agent TUI
  wingman server [-port N] [-no-browser]  Run the web UI server
  wingman acp                  Run as an ACP stdio server
  wingman claw                 Run the claw multi-agent runner
  wingman proxy [-port N]      Run the API proxy + dashboard (requires WINGMAN_URL)
  wingman run <target> [args]  Run an external agent through wingman

Run targets:
  claude, claude-desktop, codex, copilot, gemini, opencode

Flags:
  --resume [id]   Resume the latest (or specified) saved session
  --help, -h      Show this help
`)
}
