# Ideas

Backlog from comparing against Claude Code 2.1.330 (CC), Codex CLI 0.171 (Codex),
Antigravity App 2.34 / CLI 1.5.12 (AG) and Pi 1.6 (Pi), plus VS Code and Cursor
for the editor. Last refreshed 2026-11-21.

## Next

1. **Auto-review follow-ups** — read-only investigation tools for the reviewer (inspect an `rm` target, git remotes, search the conversation, list and search files); `/approve` one retry of a recent denial; reviewer status in the UI (Codex, AG); a setup that drafts `review.extraPolicy` from the project and recent sessions (CC `/auto-mode-setup`).
2. **Permission rules** — declarative allow/ask/deny with `tool(pattern)`, deny > ask > allow; grants double as sandbox config; a grant for one path lasts the conversation or is saved, and covers its subagents (AG).
3. **Sandbox follow-ups** — plan mode and read-only subagents use a read-only sandbox (CC, Codex, AG); set aside unexpected entries a sandboxed command leaves in `.git`, which could run outside the sandbox as hooks (CC); hide more credential stores by default, such as `~/.config`, `~/.docker`, `~/.kube` and `.git-credentials`, while tool settings stay readable (AG).
4. **Worktree isolation** — per subagent and per session, `.worktreeinclude`, merge-back (CC, Codex, AG); refuse removing a worktree that still has running work or unsaved changes (CC).
5. **`/goal`** — persisted objective, independent evaluator decides "done", token/time budget (CC, Codex, AG).
6. **Session ergonomics** — edit-earlier-message-and-fork, `/btw` side question, rename + content search (CC, Codex, AG); `/btw` gets read-only tools and its own pane, and its answer can go into the main chat (AG, Codex); fork a session whose folder is gone into the current one (Pi).
7. **MCP context cost** — tool search / deferred schemas (CC, Codex, Pi); exposure per server: direct, deferred or code mode only (Pi); a call to a tool not loaded yet returns its schema (CC). Code mode ([docs](docs/code-mode.md)) keeps intermediate results out of context; next, drop MCP schemas from the tool list and let scripts look signatures up, as Pi's `searchTools()` and `describeNamespace()` do.
8. **Commentable artifacts** — inline comments on plans, diffs, screenshots → "Proceed" or send all as one review (AG).
9. **Browser agent** — screenshots, console, DOM, recordings (CC, Codex, AG); downloads wait for approval, and cross-site frames load signed out and stay out of the agent's view (AG).
10. **Workflows** — code mode scripts that fan out to subagents and combine their results (AG `run_workflow`, also Python; CC workflows); a script can already call `agent`, not yet measured. AG `/swarm` instead coordinates workers over a shared message board.

## Agent & CLI

- **Hooks** — `prompt`/`agent` handlers once Codex runs them; events PostToolUseFailure, PermissionDenied, Notification, FileChanged (CC); `onFailure: "block"` so a failing hook refuses the call (CC); a channel for `systemMessage`/`statusMessage`; deliver async output into subagents.
- **Skills** — enforce `allowed-tools` (parsed, unused); `paths` globs for activation; `` !`cmd` `` output injection (CC).
- **Instructions** — `@path` imports in AGENTS.md; glob-scoped `.agents/rules/*.md` (CC, AG).
- **Memory** — `/learn` proposes a memory/rule/skill diff the user approves (AG); list/delete UI.
- **Plan → implement** — start implementation in a fresh context with the full approved plan, show context usage first (Codex); optionally on a faster model, as AG's Orchestrator plans with one model and implements with another.
- **Advisor** — let the agent consult the `complex` model mid-turn (CC).
- **Monitor** — stream background command output as events instead of polling; long MCP calls go background as MCP tasks and report back (CC).
- **Subagents** — approvals bubble up to parent, and a grant covers the session's other agents (AG); user can `@`-message a running or finished subagent (CC, AG); model and effort per subagent, shown in `/agents` (CC, AG); spawn/concurrency caps (AG, CC).
- **Cache-aware context** — idle compaction before cache TTL (CC) or cost-aware cache warming during long tool runs and idle time (Pi); warn before resuming a long session whose cache expired (CC); a notice when the provider misses the cache or drops cached reasoning (Pi); cache hit stats in `/context` (CC).
- **Cost** — per-model pricing in footer and `/usage` (CC, Codex, AG, Pi).
- **exec** — `--output stream-json` events; stdin stream-json for multi-turn (CC, AG); a `finished_reason` and its own exit code when a run is cut short (AG); `--remote` to run on an existing `wingman server` (Codex).
- **Import** — `/import` from Claude Code/Codex: MCP, skills, agents, sessions (Codex, AG); other agents' memories (Codex).
- **Providers** — add or edit a provider and pick the default in the UI instead of environment variables, with device-code sign-in for gateways over SSH (Codex); `wingman auth login <provider>` (Pi).
- **Plugins** — `wingman plugin add <git|url>`; read Claude Code marketplace format (Codex does); `wingman plugin validate` for a folder, zip or GitHub URL (AG, CC); a supported Wingman version range in the manifest (CC).
- **MCP** — resources and prompts; `/mcp` status; URL-mode elicitation (CC); per-project overrides of user servers, OAuth Client ID Metadata Documents (Pi).
- **Doctor** — `/status` or `wingman doctor` lists broken skills, agents and plugins, hook matchers that never match, ignored settings and an unavailable sandbox (CC, Codex); published JSON schemas for `config.json`, `mcp.json` and `agents.json` (Pi).
- **TUI** — vim mode, keybindings file, status-line script, Ctrl+G `$EDITOR`, Ctrl+R prompt history, turn undo; composer undo/redo, OSC 9/777 notifications (Codex); OSC 7501 working/blocked/done status (CC, Pi); inline image previews in Kitty and Sixel terminals (Codex, CC, Pi).
- **Voice follow-ups** — answer question cards by voice and summarize the call on its transcript card (AG); WebRTC transport (Codex).
- **Code mode follow-ups** — variables that persist between `code` calls; show a script's running call and output (CC) and elapsed time (Pi); name the calls still running when a script fails (CC); pass images through.
- **Later** — daemon so TUI/web/phone attach to the same sessions (Codex); critic/auditor verify pipeline against test gaming (AG teamwork, could be a skill); extension API for status line, panes, tool renderers and commands (CC mods, Pi extensions, Codex MCP status line).

## IDE mode

- **Change review** — per-file and per-hunk accept/reject inside turn reviews; cumulative diff across turns (AG).
- **Inline HTML/chart artifacts** in chat (AG generative UI, CC charts); MCP Apps UI from tool results (AG).
- **Transcript navigation** — a rail with one mark per prompt, jump to the previous or next prompt, collapse finished turn details (AG, Codex).
- **Terminal ↔ chat** — send terminal output/selection to chat or a side question (AG), `@terminal` context, command generation.
- **Tab** — coordinated multi-file suggestions.
- **Editor** — persist open tabs, split layout, panel sizes; settings page, keybindings, theme picker.
- **Navigation** — outline, breadcrumbs, workspace symbol search (needs HTTP route; LSP tool already has it).
- **Test** — Test Explorer; coverage.
- **Git** — hunk/line staging, amend, file-tree status decorations; merge-conflict editor; stash; blame/timeline.
- **PRs** — PR/issue view; live status on PR links (AG); post `/code-review` findings as PR comments.
- **Notebooks** — `.ipynb` view and edit (AG has a viewer).

## Platform

- **Remote** — SSH workspaces in the launcher: install the server over OpenSSH from `~/.ssh/config`, agents keep running after the window closes (AG); WSL and devcontainers.
- **Notifications** — push "done / needs input" over the relay (AG, CC).
- **Marketplace UI** — install plugins and MCP servers from the web UI; plugin-provided side panels (AG UI Extensions); the agent suggests a plugin when one would help (AG).
- **Triggers** — GitHub, Slack, webhook-started runs (CC, Codex); schedules page with next/last run and pause (AG Automations).
- **Specs** — requirements/design/tasks as plain files, shown through the artifact comment view.
- **Enterprise** — managed settings; policies for models, tools, MCP; audit log.

## Decisions

- No automatic filesystem checkpoints or shadow repos; restore points and forks via Git refs only; non-Git folders stay checkpoint-free.
- Tab uses the utility role over the Responses API; no dedicated FIM model.
- Don't rebuild the VS Code extension ecosystem.
