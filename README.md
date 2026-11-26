# Wingman Agent

An AI coding assistant for your terminal and browser. Ask Wingman to explain a
codebase, fix bugs, write tests, or implement features. It can read and edit files,
run commands, and help you review changes.

- Work in a terminal UI or a browser with an editor, Git diffs, and terminals.
- Use an OpenAI-compatible API, OpenRouter, Ollama, or a Wingman gateway.
- Connect installed coding agents such as Claude, Codex, Copilot, OpenCode, and Pi.
- Get code navigation from the language servers you have installed.
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
edit.

The session list shows the sessions of every agent in the folder, with what
needs you first. In the desktop app it holds the sessions of every project, and
opening a session of another project moves the window there while all agents
keep running; see [sessions and recovery](docs/sessions.md#resume-a-conversation).

Code navigation in the editor, and for the agent's `lsp` tool, comes from the
[language servers](#language-servers) on your `PATH`.

For access from a phone or another computer, see [remote web access](docs/remote-control.md).

To use Wingman in VS Code's Agents window, connect to the server's
[Agent Host Protocol endpoint](docs/ahp.md) at `ws://localhost:9000/ahp`, or run
`wingman host` for a host without the web UI that opens any project in your home
directory, at `ws://localhost:9100/ahp`.

## Language servers

Wingman uses language servers for the editor's code intelligence and for the
agent's `lsp` tool: definitions, references, hover, symbols, implementations,
and call hierarchies. It does not install them. A server starts the first time
a file with one of its extensions is opened in the editor, queried, or edited
by the agent, and only if its command is on your `PATH` or in a usual tool
directory such as `~/go/bin`, `~/.cargo/bin`, or Homebrew's. After an agent
edit, the errors and warnings the server reports for the file within two
seconds are added to the edit result.

| Language | Command | Install, for example |
|----------|---------|----------------------|
| C/C++ | `clangd` | `brew install llvm` or `sudo apt install clangd` |
| C# | `csharp-ls` | `dotnet tool install --global csharp-ls` |
| Go | `gopls` | `go install golang.org/x/tools/gopls@latest` |
| Java | `jdtls` | `brew install jdtls` |
| Kotlin | `kotlin-lsp` | `brew install JetBrains/utils/kotlin-lsp` |
| Lua | `lua-language-server` | `brew install lua-language-server` |
| PHP | `intelephense` | `npm install -g intelephense` |
| Python | `pyright-langserver` | `npm install -g pyright` |
| Ruby | `ruby-lsp` | `gem install ruby-lsp` |
| Rust | `rust-analyzer` | `rustup component add rust-analyzer` |
| Swift | `sourcekit-lsp` | Included with Xcode and the Swift toolchain |
| TypeScript, JavaScript | `typescript-language-server` | `npm install -g typescript-language-server typescript` |

These definitions are those of Claude Code's code intelligence plugins. To add
or replace a server, put a plugin in `~/.wingman/plugins/` with a `.lsp.json`
file, or an `lspServers` entry in its `plugin.json` or
`.claude-plugin/plugin.json`, in Claude Code's format. A Claude Code plugin
directory works as it is:

```json
{
  "zls": {
    "command": "zls",
    "extensionToLanguage": { ".zig": "zig" }
  }
}
```

`command` and `extensionToLanguage` are required. `args`, `env`,
`initializationOptions`, `settings`, `workspaceFolder`, `startupTimeout`,
`shutdownTimeout`, `requestTimeout`, `restartOnCrash`, `maxRestarts`, and
`diagnostics` (`false` keeps them out of edit results) are optional;
`transport` is accepted and has no effect. Each
file extension has one server: a plugin's server takes it from a built-in one,
and a server with the name of a built-in one replaces it. Language servers of
a plugin inside a project are not started, so that a repository cannot choose
a command to run.

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
| `/btw <question>` | Ask a side question; the answer stays out of the conversation |
| `/diff` | Review working tree changes |
| `/mcp` | Show MCP servers, their state, and their tools |
| `/skills` | Show available skills and where each comes from |
| `/plugins` | Show installed plugins and what each adds |
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

`exec` connects your MCP servers before it starts. A server that cannot be
reached is reported on stderr and to the model, and the run goes on without
it. Servers from a project's `mcp.json` connect once you have opened the
project in `wingman` and approved them.

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
| `WINGMAN_MODEL_UTILITY` | Model for short internal requests, including completion checks when the decision model is empty |
| `WINGMAN_MODEL_DECISION` | Optional deployment for the OpenAI Decisions API, for example `gpt-6-luna` |
| `WINGMAN_EFFORT` | Reasoning effort; also selectable with `/model` |
| `WINGMAN_MODEL_TRANSCRIBE` | Optional model for microphone dictation |
| `WINGMAN_MODEL_REALTIME` | Optional Realtime API model for voice conversations |
| `WINGMAN_DISABLE_VOICE` | Disable dictation and voice conversations |

Provider priority is `WINGMAN_URL`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, then
Ollama. With none set, Wingman uses `http://localhost:4242/v1`. A gateway must run
separately; `wingman server` starts the web UI.

Voice conversations use a separate speech model while the selected coding model
continues handling files and tools. With an empty message, the send button in
the web or desktop composer starts voice; in a local terminal, run `/voice`.
Speak requests and corrections normally; corrections steer running work. Escape
ends voice, and coding work continues unless you ask to cancel the task.
Status questions use a read-only tool and do not add a coding-chat turn or steer work.
While voice is connected, completed coding turns trigger a spoken summary,
including work started in chat.

The provider must support the OpenAI Realtime API over WebSockets, including
server VAD with `create_response: false` and per-response `tool_choice` and
`instructions` overrides. Wingman
discovers models with `realtime` in their name, or you can set
`WINGMAN_MODEL_REALTIME=gpt-realtime-2.1`. GPT-Live uses a different protocol.
The web UI streams audio and supports speech interruptions and microphone mute.
Terminal playback buffers and queues spoken answers. It pauses microphone
forwarding during playback and briefly afterward to prevent speaker audio from
being treated as user input; listening resumes automatically. Use `/voice headphone`
with headphones to keep listening during playback and allow spoken interruptions.
Playback uses `afplay` on macOS, PowerShell on
Windows, or `pw-play`, `paplay`, or `aplay` on Linux. Microphone access is required;
remote SSH terminals do not capture the remote machine's microphone.

To route another coding CLI through your Wingman gateway, use
`wingman run claude`, `wingman run codex`, or another supported target.
Run `wingman --help` for the full command list and `wingman <command> --help`
for command options.

## Customize your workflow

- **Project instructions:** add an `AGENTS.md` or `CLAUDE.md` in your project root
  with build commands, conventions, and other guidance.
- **Skills:** put reusable workflows in `.wingman/skills/<name>/SKILL.md`, or in
  `~/.wingman/skills/` to use them across projects. Invoke them with `/<name>`.
  Without personal Wingman skills, `~/.agents/skills/` or `~/.claude/skills/` is
  used instead, except the skills the Claude apps sync into
  `~/.claude/skills/synced/`, which rely on those apps' own tools.
- **Plugins:** place a plugin directory in `.wingman/plugins/` or
  `~/.wingman/plugins/` to add its skills, MCP servers, and hooks. A plugin in
  `~/.wingman/plugins/` can also add [language servers](#language-servers).
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
wingman mcp add remote https://mcp.example.com/mcp
wingman mcp list
wingman mcp remove remote
```

Add `--scope project` to save a server in the project's `mcp.json`. Personal
servers are stored in `~/.wingman/mcp.json`. Servers that require OAuth open a
browser for sign-in.

To see which servers are connected and the tools each one offers, run `/mcp`
in the terminal UI or choose **Show connections** from the browser's command
palette. A server that failed to start is listed with its error. The browser
page also lists the installed plugins and the available skills; in the
terminal UI, `/plugins` and `/skills` show them.

The model can write a short Python script that calls its tools, MCP tools
included, and get back only the result. See [code mode](docs/code-mode.md).

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
