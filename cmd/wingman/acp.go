package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	acpclaude "github.com/adrianliechti/wingman-agent/pkg/acp/claude"
	acpcodex "github.com/adrianliechti/wingman-agent/pkg/acp/codex"
	acpserver "github.com/adrianliechti/wingman-agent/pkg/acp/server"
	"github.com/adrianliechti/wingman-agent/pkg/external/claude"
	"github.com/adrianliechti/wingman-agent/pkg/external/codex"
)

func runACP(ctx context.Context) {
	// stdout is reserved for the ACP JSON-RPC stream — redirect any default
	// slog writers (used by transitive deps like the openai and mcp SDKs)
	// to stderr so a stray log line can't corrupt the protocol.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if len(os.Args) >= 3 {
		switch os.Args[2] {
		case "claude":
			runACPClaude(ctx)
			return
		case "codex":
			runACPCodex(ctx)
			return
		}
	}

	if err := acpserver.Run(ctx, os.Stdin, os.Stdout); err != nil {
		fatal(err)
	}
}

func runACPClaude(ctx context.Context) {
	fs := flag.NewFlagSet("acp claude", flag.ExitOnError)
	model := fs.String("model", "default", "default model id for new sessions")
	effort := fs.String("effort", "", "default effort level (low|medium|high|xhigh|max)")
	debug := fs.Bool("debug", false, "log JSON-RPC traffic to stderr")
	fs.Parse(os.Args[3:])

	cwd, err := os.Getwd()
	if err != nil {
		fatal(err)
	}

	cfg, err := claude.NewConfig(ctx, nil)
	if err != nil {
		fatal(err)
	}

	path, err := claude.FindPath()
	if err != nil {
		fatal(err)
	}

	opts := acpclaude.Options{
		Model:  *model,
		Effort: *effort,
		Cwd:    cwd,
		Env:    claude.BuildEnv(os.Environ(), cfg),
		Path:   path,
	}

	if err := acpclaude.Run(ctx, opts, os.Stdin, os.Stdout, acpLogger(*debug)); err != nil {
		fatal(err)
	}
}

func runACPCodex(ctx context.Context) {
	fs := flag.NewFlagSet("acp codex", flag.ExitOnError)
	codexPath := fs.String("codex", "codex", "path to the codex binary")
	model := fs.String("model", "default", "default model id for new sessions")
	effort := fs.String("effort", "", "default reasoning effort (minimal|low|medium|high|xhigh)")
	debug := fs.Bool("debug", false, "log JSON-RPC traffic to stderr")
	fs.Parse(os.Args[3:])

	cfg, err := codex.NewConfig(ctx, nil)
	if err != nil {
		fatal(err)
	}

	opts := acpcodex.Options{
		Model:     *model,
		Effort:    *effort,
		Env:       codex.BuildEnv(os.Environ(), cfg),
		ExtraArgs: codex.BuildArgs(cfg),
	}

	if err := acpcodex.Run(ctx, *codexPath, opts, os.Stdin, os.Stdout, acpLogger(*debug)); err != nil {
		fatal(err)
	}
}

func acpLogger(debug bool) *slog.Logger {
	level := slog.LevelWarn
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
