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
“Add input validation to the signup form”. Risky actions are reviewed automatically before they run; you decide what the reviewer declines.
Use Plan mode to explore an approach before making changes.

The built-in agent runs shell commands in a sandbox with writes limited to the
workspace, project memory, and private session temporary storage. `TMPDIR`,
`TEMP`, and `TMP` point to that private directory. Tool cache settings are inherited,
including configuration in `go env -w` and `.npmrc`. Sandboxed commands run with
`GODEBUG=goindex=0`, so Go reads the host build cache it cannot write. The model
receives the actual writable roots and network policy, and redirects a cache into private
temp only when a command fails writing it (for example,
`npm ci --cache "$TMPDIR/npm-cache"`).
Private temporary files are reused within the session and removed on close.

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

The session list shows the sessions of every agent and of every project in
your home directory you used, with what needs you first. Opening a session of
another project moves the window there while all agents keep running; see
[sessions and recovery](docs/sessions.md#resume-a-conversation).

Wingman manages language servers and offers to install missing debuggers when
you launch them. Your project's runtimes and build tools must be installed
separately. See the [debugging examples](examples/debug/README.md) for supported
languages and sample projects.

For access from a phone or another computer, see [remote web access](docs/remote-control.md).

To use Wingman in VS Code's Agents window, connect to the server's
[Agent Host Protocol endpoint](docs/ahp.md) at `ws://localhost:9000/ahp`, or run
`wingman host` for a host without the web UI that opens any project in your home
directory, at `ws://localhost:9100/ahp`.

## Everyday controls

In the terminal UI, use `@` to attach files and `Ctrl+P` to find commands.

| Shortcut or command | Action |
|---------------------|--------|
| `Enter` | Send a message |
| `Shift+Enter` or `Ctrl+J` | Insert a new line |
| `Alt+Enter` | Queue a follow-up |
| `Tab` | Switch between Agent and Plan mode |
| `Shift+Tab` | Toggle unattended mode |
| `Ctrl+P` | Open or close the command picker |
| `Ctrl+O` | Open or close the transcript |
| `Ctrl+Q` | Manage queued follow-ups |
| `Ctrl+V` | Paste into the focused input |
| `Ctrl+Y` | Copy the last response |
| `Escape` | Cancel the current response or close a menu |
| `Ctrl+C` | Clear the draft, interrupt work, or close a menu |
| `Ctrl+C` twice with an empty draft | Exit |
| `/model` | Choose a model and reasoning effort |
| `/agent` or `/plan` | Switch to Agent or Plan mode |
| `/agents` | Watch agents and view scheduled tasks |
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

Built-in skills cover feature development, debugging, testing, simplification,
code review, architecture, security review, threat modeling, commits, pull
requests, project instructions, memory, and skill creation.
`/architecture` handles system design as well as existing-system analysis.
`/security-review` handles scanning, supplied findings, and remediation when
requested. These replace `/system-design`, `/vuln-scan`, `/triage`, and `/patch`.
For example, use `/security-review triage findings.json` to validate a report or
`/security-review fix the confirmed findings in findings.json` to remediate it.

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

## Test on Windows 11 Arm64

On an Apple Silicon Mac, install Homebrew QEMU (`brew install qemu`), Go, Task,
Python 3, and Node.js with npm. Supply an English (US) Windows 11 Arm64 ISO
containing the Pro edition and a VirtIO driver ISO containing `viostor` and `NetKVM` for Windows
11 Arm64. [Windows installation media](https://www.microsoft.com/en-us/software-download/windows11arm64) · [VirtIO driver downloads](https://github.com/virtio-win/virtio-win-pkg-scripts#downloads)

```bash
WINDOWS_ISO="$HOME/Downloads/Windows11-arm64.iso" \
VIRTIO_ISO="$HOME/Downloads/virtio-win.iso" \
task lab:windows:test
```

The first run installs Windows Pro unattended, accepts its setup license terms,
and uses Windows Update to install OpenSSH. The guest signs in its dedicated
test user at boot. The VM gets four CPUs, 8 GiB of RAM,
and an 80 GiB sparse disk. Later runs reuse the installation and rebuild Wingman
and its embedded web UI from the current checkout. The shell tests need a Windows
build with PSEC 1.0 and native denied paths. [Windows sandbox requirements](docs/sessions.md)

| Command | What it does |
| --- | --- |
| `task lab:windows` | Boot the VM and copy the Arm64 CLI and desktop app to `C:\wingman-lab`. |
| `task lab:windows:test` | Run CLI script, agent edit-and-undo, ConPTY, desktop launcher and sandboxed PowerShell tests in the guest. A failing guest test fails the task. |
| `task lab:windows:ssh` | Open a PowerShell session in the running VM. |
| `task lab:windows:stop` | Shut Windows down and keep the disk for next time. |

For a desktop check, run these commands from the repo on your Mac, then paste
the password into Screen Sharing.

```bash
pbcopy < .windows-lab/vm/vnc-password
open vnc://127.0.0.1:5916
```

If Windows asks you to sign in, use `wingman` and the password in
`.windows-lab/vm/password`. Launch `C:\wingman-lab\Wingman Agent.exe` from
Explorer. SSH and VNC listen on loopback.

The lab runs the sandbox tests in the signed-in user's desktop. On Windows build
26300.9457 the same PowerShell sandbox exits with `0xc0000142` when launched
from the SSH service session.

To try a real agent turn, open a project folder in PowerShell on the guest desktop,
configure your model provider there, and run `C:\wingman-lab\wingman.exe`.
For Ollama running on the Mac, set `$env:OLLAMA_HOST = 'http://10.0.2.2:11434'`
in that shell.
