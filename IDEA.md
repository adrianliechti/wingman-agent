# Ideas

Backlog from comparing against Claude Code 2.1.302 (CC), Codex 0.163 / app (Codex),
Antigravity App 2.24 / CLI 1.4 (AG), plus VS Code and Cursor for the editor.
Last refreshed 2026-10-17.

Unmerged branch: `feature/browser` (90 commits behind `main`).

## Next

1. **Auto-review follow-ups** — read-only investigation tools for the reviewer (inspect an `rm` target, git remotes); `/approve` one retry of a recent denial; reviewer status in the UI (Codex).
2. **Permission rules** — declarative allow/ask/deny with `tool(pattern)`, deny > ask > allow; grants double as sandbox config (AG).
3. **Sandbox follow-ups** — plan mode and read-only subagents use a read-only sandbox; the shell classifier shrinks to Codex size (all three).
4. **Worktree isolation** — per subagent and per session, `.worktreeinclude`, merge-back (all three).
5. **`/goal`** — persisted objective, independent evaluator decides "done", token/time budget (all three).
6. **Session ergonomics** — edit-earlier-message-and-fork, `/btw` side question, rename + content search (all three).
7. **MCP context cost** — tool search / deferred schemas; forward image content (dropped today) (CC, Codex).
8. **Commentable artifacts** — inline comments on plans, diffs, screenshots → "Proceed" or send all as one review (AG).
9. **Browser agent** — revive `feature/browser`: screenshots, console, DOM, recordings (all three).

## Agent & CLI

- **Hooks** — `prompt`/`agent` handlers once Codex runs them; events PostToolUseFailure, PermissionDenied, Notification, FileChanged (CC); a channel for `systemMessage`/`statusMessage`; deliver async output into subagents.
- **Skills** — enforce `allowed-tools` (parsed, unused); `paths` globs for activation; `` !`cmd` `` output injection (CC).
- **Instructions** — `@path` imports in AGENTS.md; glob-scoped `.agents/rules/*.md` (CC, AG).
- **Memory** — `/learn` proposes a memory/rule/skill diff the user approves (AG); list/delete UI.
- **Plan → implement** — start implementation in a fresh context, show context usage first (Codex).
- **Advisor** — let the agent consult the `complex` model mid-turn (CC).
- **Monitor** — stream background command output as events instead of polling; long MCP calls go background (CC).
- **Subagents** — approvals bubble up to parent; user can `@`-message a running subagent; spawn/concurrency caps (AG, CC).
- **Cache-aware context** — idle compaction before cache TTL; cache hit stats in `/context` (CC).
- **Cost** — per-model pricing in footer and `/usage` (all three).
- **exec** — `--output stream-json` events; stdin stream-json for multi-turn (CC, AG).
- **Import** — `/import` from Claude Code/Codex: MCP, skills, agents, sessions (Codex).
- **Plugins** — `wingman plugin add <git|url>`; read Claude Code marketplace format (Codex does).
- **MCP** — resources and prompts; `/mcp` status.
- **TUI** — vim mode, keybindings file, status-line script, Ctrl+G `$EDITOR`, Ctrl+R prompt history, OSC 9 notifications, turn undo.
- **Later** — model-written orchestration scripts for large fan-out (CC workflows, Codex code mode); daemon so TUI/web/phone attach to the same sessions (Codex); critic/auditor verify pipeline against test gaming (AG teamwork, could be a skill).

## IDE mode

- **Agent manager** — all sessions with status, "needs approval" inbox, jump to next (AG, Codex app).
- **Change review** — per-file and per-hunk accept/reject inside turn reviews; cumulative diff across turns (AG).
- **Inline HTML/chart artifacts** in chat (AG generative UI).
- **Terminal ↔ chat** — send terminal output/selection to chat, `@terminal` context, command generation.
- **Tab** — coordinated multi-file suggestions.
- **Editor** — persist open tabs, split layout, panel sizes; settings page, keybindings, theme picker.
- **Navigation** — outline, breadcrumbs, workspace symbol search (needs HTTP route; LSP tool already has it).
- **Debug/test** — watch expressions + debug console (backend already accepts `watch`/`repl`); Test Explorer; coverage.
- **Git** — hunk/line staging, amend, file-tree status decorations; merge-conflict editor; stash; blame/timeline.
- **PRs** — PR/issue view; post `/code-review` findings as PR comments.
- **Notebooks** — `.ipynb` view and edit.

## Platform

- **Remote** — SSH workspaces in the launcher; WSL and devcontainers.
- **Notifications** — push "done / needs input" over the relay (AG, CC).
- **Marketplace UI** — install plugins and MCP servers from the web UI; plugin-provided side panels (AG UI Extensions).
- **Triggers** — GitHub, Slack, webhook-started runs (CC, Codex).
- **Specs** — requirements/design/tasks as plain files, shown through the artifact comment view.
- **Enterprise** — managed settings; policies for models, tools, MCP; audit log.

## Decisions

- No automatic filesystem checkpoints or shadow repos; restore points and forks via Git refs only; non-Git folders stay checkpoint-free.
- Tab uses the utility role over the Responses API; no dedicated FIM model.
- Don't rebuild the VS Code extension ecosystem.
