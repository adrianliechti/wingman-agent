# Wingman Agent

An AI coding assistant for your terminal and browser. Ask Wingman to explain a
codebase, fix bugs, write tests, or implement features. It can read and edit files,
run commands, and help you review changes.

- Work in a terminal UI or a browser with an editor, Git diffs, and terminals.
- Use an OpenAI-compatible API, OpenRouter, Ollama, or a Wingman gateway.
- Connect installed coding agents such as Claude, Codex, Copilot, OpenCode, and Pi.
- Get code navigation, diagnostics, and integrated debugging.
- Extend your workflow with skills, plugins, and MCP servers.

## Install

### CLI

macOS and Linux, with a current Homebrew version:

```bash
brew install --cask adrianliechti/tap/wingman-cli
```

Windows (x64), with Scoop:

```powershell
scoop bucket add adrianliechti https://github.com/adrianliechti/scoop-bucket
scoop install wingman
```

Or install from source with Go 1.26.3 or later:

```bash
go install github.com/adrianliechti/wingman-agent/cmd/wingman@latest
```

### Desktop app

macOS (Apple Silicon):

```bash
brew install --cask adrianliechti/tap/wingman-app
```

Windows (x64):

```powershell
scoop bucket add adrianliechti https://github.com/adrianliechti/scoop-bucket
scoop install wingman-app
```

## Get started

Set up a model provider, then run Wingman in your project directory:

```bash
export OPENAI_API_KEY="your-api-key"
# For another OpenAI-compatible provider, also set its endpoint:
# export OPENAI_BASE_URL="https://your-provider.example/v1"

cd your-project
wingman
```

Try asking: “Explain how this project works”, “Fix the failing tests”, or
“Add input validation to the signup form”. Wingman asks for approval when needed.
Use Plan mode to explore an approach before making changes.

To use an installed agent with its existing login and configuration:

```bash
wingman --agent codex
wingman --agent claude
wingman --agent pi
```

Sign in through that agent's own CLI first. These agents use their own provider
settings, including inherited environment variables.

Sessions are saved automatically:

```bash
wingman --continue                 # Resume the latest session
wingman --resume <session-id>       # Resume a specific session
wingman --agent codex --continue    # Resume a native agent session
```

## Use the browser UI

```bash
wingman server
```

Opens the web UI at `http://localhost:9000`, or another available port. Use
`--port 9001` to choose a port. The UI includes chat, an agent picker, a file
editor, changes, and terminals. Select code to chat about it or preview an AI
edit. Use **Run | Debug** above a supported entry point to launch a debugger.

Wingman manages language servers and offers to install missing debuggers when
you launch them. Your project's runtimes and build tools must be installed
separately. See the [debugging examples](examples/debug/README.md) for supported
languages and sample projects.

For access from a phone or another computer, see [remote web access](docs/remote-control.md).

## Everyday controls

In the terminal UI, use `@` to attach files and `Ctrl+P` to find commands.

| Shortcut or command | Action |
|---------------------|--------|
| `Enter` | Send a message |
| `Shift+Enter` or `Ctrl+J` | Insert a new line |
| `Alt+Enter` | Queue a follow-up |
| `Tab` | Switch between Agent and Plan mode |
| `Escape` | Cancel the current response or close a menu |
| `Ctrl+C` twice | Exit |
| `/model` | Choose a model and reasoning effort |
| `/diff` | Review working tree changes |
| `/help` | Browse commands and skills |

See [sessions and recovery](docs/sessions.md) for queues and drafts, and
[reviewing changes](docs/reviewing-changes.md) for validation and undo.

Bundled skills include `/init` to create project instructions, `/debug` to
investigate a bug, `/test` to work on tests, and `/code-review` to review changes.

## Run a prompt from a script

```bash
wingman exec "Summarize this project"
git diff | wingman exec "Review this diff for bugs"
wingman exec "Generate release notes" > release-notes.md
```

`exec` runs unattended: it approves actions automatically and does not wait for
questions. Piped input becomes context for the prompt. The final response goes
to stdout; add `--debug` for progress and tool details on stderr.

```bash
wingman exec --json "Extract project metadata"
wingman exec --schema ./project.schema.json "Extract project metadata"
wingman exec resume --last "Implement the first suggestion"
```

See the [non-interactive examples](examples/non-interactive/README.md) for structured output.

## Configure your provider

The built-in Wingman agent reads provider settings from environment variables:

| Variable | Purpose |
|----------|---------|
| `OPENAI_API_KEY` | API key for OpenAI or a compatible provider |
| `OPENAI_BASE_URL` | API endpoint; defaults to `https://api.openai.com/v1` |
| `OPENROUTER_API_KEY` | Use OpenRouter |
| `OLLAMA_HOST` | Use a local or remote Ollama server |
| `OLLAMA_API_KEY` | Ollama API key; uses Ollama Cloud when no host is set |
| `WINGMAN_URL` | Wingman gateway URL |
| `WINGMAN_TOKEN` | Gateway authentication token, if required |
| `WINGMAN_MODEL` | Default model; also selectable with `/model` |
| `WINGMAN_EFFORT` | Reasoning effort; also selectable with `/model` |

Provider priority is `WINGMAN_URL`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, then
Ollama. With none set, Wingman uses `http://localhost:4242/v1`. A gateway must run
separately; `wingman server` starts the web UI.

To route another coding CLI through your Wingman gateway, use
`wingman run claude`, `wingman run codex`, or another supported target.
Run `wingman --help` for the full command list and `wingman <command> --help`
for command options.

## Customize your workflow

- **Project instructions:** add an `AGENTS.md` or `CLAUDE.md` in your project root
  with build commands, conventions, and other guidance.
- **Skills:** put reusable workflows in `.wingman/skills/<name>/SKILL.md`, or in
  `~/.wingman/skills/` to use them across projects. Invoke them with `/<name>`.
- **Plugins:** place a plugin directory in `.wingman/plugins/` or
  `~/.wingman/plugins/` to add its skills, MCP servers, and hooks.
- **Custom agents:** add specialist instructions in `.wingman/agents/*.md` or
  `~/.wingman/agents/`.

Connect an MCP server from the command line:

```bash
wingman mcp add remote --url https://mcp.example.com/mcp
wingman mcp list
wingman mcp remove remote
```

Add `--scope project` to save a server in the project's `mcp.json`. Personal
servers are stored in `~/.wingman/mcp.json`. Servers that require OAuth open a
browser for sign-in.

See [configuration examples](examples/README.md) for ready-to-copy skills,
plugins, custom agents, and MCP settings. Wingman stores its settings and sessions
in `~/.wingman`; set `WINGMAN_HOME` to choose another location.
