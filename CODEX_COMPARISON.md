# Wingman vs. OpenAI Codex (`codex-rs`) — Architectural Comparison & Roadmap

Date: 2026-11-12  
Compared repositories:
- **Wingman**: `/Users/adrian/Projects/wingman-agent`
- **Codex**: `/Users/adrian/Projects/scratch/codex` (`codex-rs`, HEAD `c0cc9fc820`)

---

## Executive Summary

Wingman (`wingman-agent`) is already near frontier parity—and in several core code-editing and multi-model areas, it **outperforms `codex-rs`**.

The two systems optimize from different foundations:
- **Codex (`codex-rs`)** is co-designed around OpenAI's Responses API and GPT-5.x/6.x models: shell-first file reading (`rg`/`sed` via `exec_command`), freeform Lark-grammar `apply_patch`, strict append-only `WorldState` prompt-cache invariance, deferred MCP `tool_search`, sandboxed V8 `code-mode` tool chaining, a domain-filtering `network-proxy`, a two-phase background memory consolidation pipeline, and V2 hierarchical multi-agent fan-out with worktree isolation.
- **Wingman** is a genuinely **model-agnostic** (OpenAI, Anthropic, Gemini) software-engineering harness with **stronger static code intelligence and file safety**: structured `read`/`edit`/`glob`/`grep` with per-agent `Freshness` tracking, **built-in LSP + tree-sitter navigation**, **automatic post-edit LSP diagnostics** (`WithEditDiagnostics`), **lossless `<persisted-output>` scratch spill** (instead of Codex's lossy middle-out truncation), microcompaction before LLM summarization, and validation-run tracking (`validation: true`).

To make Wingman a **top-notch coding agent**, do **not** copy Codex's shell-only file reading or drop LSP. Keep Wingman's code-intelligence core, close the high-leverage harness gaps where Codex pulls ahead, and unlock the latent runtime debugger (`pkg/dap` + `pkg/debugadapter`) already built in this repo.

---

## Side-by-Side Architecture Matrix

| Dimension | Wingman (`wingman-agent`) | OpenAI Codex (`codex-rs`) | Edge |
| :--- | :--- | :--- | :--- |
| **Model Layer** | Native multi-provider (OpenAI Responses, Anthropic, Gemini) + per-model prompt variants (`pkg/code/prompt/models/`) + Claude `finish_turn` phase gate (`pkg/harness/loop.go:223-238`) | Primarily OpenAI Responses API + live `/models` ETag catalog (`codex-rs/models-manager/`) | **Wingman** (multi-model), **Codex** (OpenAI-native features) |
| **File Reading & Search** | Dedicated `read` (text/images/PDF/Office), `glob`, `grep` (RE2) + `Freshness` external-edit watcher (`pkg/code/agent/agent.go:765-843`) | Removed `read_file`/`grep_files`/`list_dir` (`codex-rs/core/src/tools/spec_plan.rs:1200+`); relies on shell `rg`/`sed` + `view_image` | **Wingman** |
| **Code Editing** | JSON `edit` (`old_string`/`new_string`, batched, 5-tier fuzzy/gutter/indent/quote tolerance, `pkg/harness/tool/fs/fs_edit.go:56-118`). **No delete or move/rename** | Freeform Lark-grammar `apply_patch` (`Add`/`Delete`/`Update` + `Move to`, 4-pass `seek_sequence`, `codex-rs/apply-patch/src/seek_sequence.rs`) | **Tie / Complementary** |
| **Code Intelligence** | Built-in `lsp` tool + tree-sitter fallback + **auto post-edit LSP diagnostics** (`pkg/code/workspace.go:1119-1177`) + unused `pkg/dap` | **None** in core agent loop (relies on shell grep/compilers) | **Wingman** (by a wide margin) |
| **Tool Concurrency** | Streaming tool dispatch with effect-based read/write DAG barrier (`pkg/harness/stream_tools.go:29-99`); shell AST classifies read-only `exec` calls (`pkg/harness/tool/shell/shell_classifier.go:42-67`) | Streaming `FuturesOrdered` dispatch + coarse `RwLock<()>` (`codex-rs/core/src/tools/parallel.rs:49-56`) | **Wingman** (finer shell effect safety) |
| **Mid-Turn Steering** | Queued in `pendingInput`, injected only **between** model/tool steps (`pkg/harness/loop.go:70-76`) | **`RealtimeSteer` stream preemption** cancels/flushes mid-SSE stream before tool dispatch (`codex-rs/core/src/session/turn.rs:3091-3235`) | **Codex** |
| **Prompt Cache Invariance** | System base + append-only `[Session context]`, **but** Plan-mode toggle mutates `instructions` & `tools` (`pkg/code/agent/agent.go:850-904`), and any context change re-emits full `[Session context]` (`pkg/harness/context_instructions.go:33-51`) | Strict append-only `WorldState` incremental delta fragments (`<collaboration_mode>`, `<environment_context>`, `codex-rs/core/src/context/world_state/mod.rs`) | **Codex** |
| **Tool Output & Compaction** | **Lossless scratch spill** (`<persisted-output>` w/ 4 KB head + 8 KB tail + readable file path, `pkg/harness/hook/truncation/truncation.go:46-108`) -> `compressContext` microcompaction -> cache-preserving `summarizeInContext` | Lossy 50/50 head+tail token truncation (`10k` tokens, `codex-rs/utils/output-truncation/src/lib.rs:34-68`) -> `/responses/compact` encrypted latent compaction + background pre-compaction (`compact_remote_v2.rs`) | **Wingman** (tool output), **Codex** (remote compaction) |
| **MCP & Tool Scaling** | Dumps all MCP schemas inline every turn; drops `ImageContent` (`pkg/harness/tool/mcp/tool.go:183-270`) | Schema byte caps + deferred **`tool_search`** (BM25, `mcp_tool_exposure.rs`) + **`code-mode`** V8 JS isolate for multi-tool chaining (`code-mode-runtime/`) | **Codex** (major gap) |
| **Multi-Agent** | 8 strong specialist roles (`explore`, `verification`, `security`, `code-reviewer`, etc.), JSON schema reports, `agent_send` (`pkg/harness/tool/subagent/subagent.go:47-99`). Shared worktree, no `fork_context`, no `wait_any` | V1/V2 hierarchical agent tree (`agent-graph-store`), **`fork_context`**, **`wait_agent`** multi-ID fan-in, **`agent-message-board`**, **`codex-worktree`** isolation | **Wingman** (role prompts/schemas), **Codex** (orchestration & isolation) |
| **Sandbox & Approvals** | Seatbelt / bwrap+seccomp / Windows AppContainer, tree-sitter destructive classifier, `ReviewAction` LLM judge, auto-rerun on denial. Binary network (`loopback` vs all IP); no persistent allow rules | Seatbelt / vendored bwrap+seccomp / Windows MXC, **Starlark `.codexpolicy`** ("always allow prefix"), **domain-allowlist HTTP/SOCKS `network-proxy`**, pooled **Guardian** w/ escalation re-review | **Codex** |
| **Memory** | Explicit/manual file edits to `ws.MemoryPath` (`pkg/code/prompt/section_memory.txt`) | **Automated 2-phase background pipeline**: Phase 1 parallel rollout extraction + secret scrubbing -> Phase 2 git-diff gated consolidation subagent (`codex-rs/memories/`) | **Codex** |

---

## Where Wingman Already Beats Codex (Keep & Protect These)

1. **LSP + Tree-Sitter + Automatic Post-Edit Diagnostics (`pkg/code/workspace.go:1119-1177`, `pkg/harness/tool/lsp/tool.go`)**
   - Every `edit` call in Wingman automatically queries active language servers concurrently (up to 2s per file) and appends type/syntax diagnostics straight onto the `edit` tool result. Codex has to spend an extra model turn + shell compiler invocation just to catch a missing import or broken signature.
2. **Lossless Scratch Persistence + Two-Tier Context Compression (`pkg/harness/hook/truncation/truncation.go:46-108`, `pkg/harness/context_compression.go`, `pkg/harness/loop.go:306-325`)**
   - When `exec` or any tool produces >48 KB, Wingman writes the full output to `scratchDir` and returns a 4 KB head + 8 KB tail preview with `Full output saved to: <path>` so the model can `read`/`grep` exact lines (`truncation.go:90-107`). Codex truncates middle tokens in-place, permanently losing middle test failures unless the command is re-run.
   - Wingman keeps tool outputs intact in context until `compactionOvershoot > 0` (`loop.go:310-322`), preserving prompt cache hits as long as possible before microcompacting stale outputs (`compressContext`) and falling back to cache-aligned `summarizeInContext` (`pkg/harness/context_summary.go:91-97`).
3. **File `Freshness` Guard & Live External Modification Notices (`pkg/harness/tool/fs/freshness.go`, `pkg/code/agent/agent.go:765-843`)**
   - Tracks per-agent read baselines (including isolated background subagent baselines in `freshness.go:59-71` and LSP rename preview SHA-256 snapshots in `freshness.go:76-112`) and polls every 2s during active turns to notify the model when a formatter, linter, or user changes a tracked file.
4. **Built-in Specialist Subagent Prompts & Structured Output Schemas (`pkg/harness/tool/subagent/subagent.go:31-99`, `172-179`)**
   - Wingman's `verification` ("probe for failure, not confirmation"), `code-reviewer` (confidence >=80 scoring), `security`, and `explore` roles—plus optional JSON Schema validation via a synthetic `report` tool—are much sharper out of the box than Codex's generic `default`/`explorer`/`worker` roles.
5. **First-Class Validation Tracking (`pkg/harness/turn_review.go`, `exec` `validation: true`)**
   - Recording actual exit statuses of validation commands separately from ordinary shell exploration gives UIs and stop/review gates ground-truth verification evidence.

---

## Where Codex Pulls Ahead — Concrete Gaps Holding Wingman Back

### 1. Prompt Cache Busting on Mode Switches & Context Updates
- **Wingman today (`pkg/code/agent/agent.go:850-904`, `pkg/harness/context_instructions.go:33-51`)**:
  - Switching between **Plan Mode** and **Agent Mode** mutates both `req.instructions` (`s.instructions()` appends `variant.Plan`) **and** `req.tools` (`s.filterTools()` strips non-read-only tools from the tool catalog). After exploring 60k–120k tokens in Plan Mode, switching to Agent Mode to implement the plan **invalidates 100% of the provider prompt cache** on the next turn.
  - Whenever anything in `instructionsData()` changes (e.g., reading a subdirectory triggers nested `AGENTS.md` discovery, or saving a small memory), `contextInstructionsUpdate` appends a complete `[Session context]` snapshot (`Project Guidelines` + `<available_skills>` + `Memory` + `Environment`), duplicating thousands of tokens in history.
- **Codex (`codex-rs/core/src/context/world_state/mod.rs`, `codex-rs/context-fragments/`)**:
  - `base_instructions` and core tool schemas remain **byte-identical** for the entire session. Mode switches, cwd changes, permission changes, and newly discovered `AGENTS.md` scopes emit small incremental delta fragments (`<collaboration_mode>`, `<environment_context>`, `<permissions instructions>`) immediately before the new turn.

### 2. Editing Ergonomics: JSON Escaping Tax + Missing Delete/Move
- **Wingman today (`pkg/harness/tool/fs/fs_edit.go`, `pkg/harness/tool/fs/undo.go:15-51`)**:
  - **No file delete or move/rename in `edit`**: Deleting or moving files requires `exec` (`rm` / `git mv`). Because `UndoFileChanges` and `turn_review.go` only track `tool.FileChange` metadata emitted by `edit`, **deletions, moves, and codegen/formatter changes executed via `exec` are invisible to Wingman's undo and turn-change ledger**.
  - **JSON string-escaping overhead on GPT/Codex models**: OpenAI models are heavily RL-trained on `*** Begin Patch` hunks. Serializing large `old_string` and `new_string` blocks inside JSON costs ~15–25% extra output tokens on quote/backslash-heavy files compared to Codex's freeform Lark-grammar `apply_patch` (`codex-rs/apply-patch/src/parser.rs`, `codex-rs/core/src/tools/handlers/apply_patch_spec.rs`).

### 3. MCP Bloat & Multi-Tool Chaining
- **Wingman today (`pkg/harness/tool/mcp/tool.go:60-115, 253-270`)**:
  - Injects every tool schema from every connected MCP server directly into `AvailableTools()` on turn 1 with zero schema byte budget, and `extractText` (`mcp/tool.go:253-270`) drops `sdkmcp.ImageContent` (breaking browser/Playwright screenshot MCP tools). Connecting 3–4 standard MCP servers injects 40k–80k tokens of JSON Schema on every request.
- **Codex (`codex-rs/core/src/mcp_tool_exposure.rs`, `handlers/tool_search.rs`, `codex-rs/code-mode-runtime/`)**:
  - Enforces `MAX_AGENT_PLUGIN_MCP_SPEC_BYTES` (8 KB) / `64 KB` total budgets and marks large MCP catalogs `Deferred`. The model sees lightweight server summaries and calls BM25 **`tool_search`** to activate only the 2–3 schemas it needs.
  - **`code-mode`** embeds an out-of-process sandboxed V8 isolate allowing the model to write async JS that loops/filters/composes built-in and MCP tools in a single turn without round-tripping intermediate JSON payloads through the LLM context.

### 4. Subagent Coordination & Worktree Isolation
- **Wingman today (`pkg/harness/tool/subagent/subagent.go`, `tasks.go`)**:
  1. **No `fork_context`**: Every subagent starts from an empty message history (`subagent.go:181-185`). In Codex (`codex-rs/core/src/tools/handlers/multi_agents/spawn.rs`), `fork_context: true` clones the parent's conversation history so the child inherits (and prompt-cache-shares) files already inspected.
  2. **No `wait_agent` multi-task barrier**: Wingman's `agent` tool is either synchronous single-child (`background: false`) or fire-and-forget (`background: true`), and `agent_output` warns against polling loops. When launching 3 parallel background subagents whose outputs are needed before proceeding, the parent lacks an `agent_wait(ids, mode: "all"|"any", timeout)` barrier (`codex-rs/core/src/tools/handlers/multi_agents/wait.rs`).
  3. **No Git worktree isolation**: Parallel editing subagents share one worktree. Codex (`codex-rs/worktree/src/lib.rs`) supports isolated git worktrees so parallel workers can edit/build/test without colliding.

### 5. Sandbox Network Proxy, Persistent `execpolicy`, and Escalation Re-Review
- **All-or-nothing network (`pkg/harness/tool/shell/sandbox.go`, `permissions.go:36`)**: Wingman's sandbox offers loopback-only or full `network: true` IP access. Codex's host-side loopback **`network-proxy`** (`codex-rs/network-proxy/`) enforces domain allowlists/denylists over HTTP CONNECT / SOCKS5 while keeping raw IP egress blocked inside Seatbelt/netns.
- **No persistent command-prefix policy (`codex-rs/execpolicy/`)**: Wingman has a strong tree-sitter shell classifier (`shell_classifier.go`) and `ReviewAction` (`pkg/harness/review.go`), plus prose `ReviewExtraPolicy`, but lacks deterministic Starlark/TOML command-prefix rules (`allow` / `prompt` / `forbid`) and "Always allow this command prefix" persistence.
- **Escalation re-review (`pkg/harness/tool/shell/permissions.go:101-108, 150-184`)**: Wingman's `approveWithRerun` (`approveCoveredRerun`) lets a single approval cover an automatic unsandboxed retry if the sandbox denies the command. Codex (`codex-rs/core/src/tools/orchestrator.rs:491-528`) forces a **fresh** Guardian review whenever authority escalates from sandboxed to unsandboxed.

### 6. Real-Time Stream Preemption on User Steering
- Wingman (`pkg/harness/loop.go:70-76`) drains `pendingInput` only at step boundaries after the current model stream and tool round finish. Codex (`codex-rs/core/src/session/turn.rs:3091-3235`) monitors the steering queue inside `tokio::select!` during SSE streaming and preempts in-flight generation immediately when no mutating tool has started.

### 7. Manual-Only Memory vs. Automated Background Consolidation
- Wingman's memory (`ws.MemoryPath`) relies on the model explicitly calling `edit` during an active task. Codex (`codex-rs/memories/write/src/phase1.rs`, `phase2.rs`) runs an idle background pipeline: Phase 1 extracts structured per-session lessons with secret redaction; Phase 2 runs a git-diff-gated, networkless subagent to consolidate `MEMORY.md` and reusable skills.

---

## Wingman's Latent Superpower: Agentic Runtime Debugging (`pkg/dap` + `pkg/debugadapter`)

Neither `codex-rs` nor Claude Code exposes a stateful runtime debugger to the agent—forcing agents into `printf` edit/run/revert loops on hard bugs.

Wingman **already implements a full multi-language DAP stack** in `pkg/dap/` and `pkg/debugadapter/` (Delve for Go, `debugpy` for Python, `codelldb`/LLDB for Rust/C/C++, `js-debug` for Node/TS, Java, .NET) with session lifecycle, breakpoints, child-process attachment, and expression evaluation—currently wired **only** to `server/handler_debug.go` for UI clients.

Exposing a compact, stateful `debug` tool to the agent (set breakpoint / conditional breakpoint, run failing test under debugger, inspect stack frames/locals/expressions on stop or panic, tear down) gives Wingman a true capability advantage over Codex on concurrency, state-machine, and subtle test failures.

---

## Prioritized Roadmap

### Tier 1 — Immediate High-ROI Harness Wins (Speed, Cache Hit Rate, & Core Reliability)
1. **Make Plan <-> Agent Mode & Session Context 100% Cache-Invariant**
   - Keep `AvailableTools()` schemas constant across Plan/Agent mode (enforce read-only in `Execute` / `PreToolUse` rather than removing tool definitions from the request) and move `PlanMode` / `UnattendedMode` out of `s.instructions()` into incremental `<mode_change>` / `<session_context_delta>` user messages (`pkg/code/agent/agent.go:850-904`, `pkg/harness/context_instructions.go:33-51`).
2. **Upgrade File Mutations & Workspace-Wide Turn Snapshots**
   - Add `delete` and `move_to` (rename) support to `edit` (`pkg/harness/tool/fs/fs_edit.go`)—and optionally expose model-routed `apply_patch` for GPT-5.x/Codex models while keeping `edit` for Claude/Gemini.
   - Back turn diffing and `/undo` with a lightweight detached Git index baseline so mutations from `exec` (`gofmt`, codegen, `rm`, `mv`) are tracked and reversible alongside `edit` (`pkg/harness/tool/fs/undo.go`).
3. **Add `agent_wait` + `fork_context` + Worktree Isolation to Subagents**
   - Add `agent_wait(ids, mode: "all"|"any", timeout)` to `pkg/harness/tool/subagent/tasks.go` so the lead agent can fan out parallel `explore`/`verification` runs and join them cleanly within a turn.
   - Add `fork_context: bool` (sharing the parent's cached message prefix) and `isolation: "worktree"` for parallel mutating subagents.
4. **Fix MCP Scaling & Multimodal Tool Results**
   - Preserve `sdkmcp.ImageContent` in `pkg/harness/tool/mcp/tool.go:253-270` using Wingman's existing `tool.IsImageResult` pipeline so Playwright/browser MCP tools retain vision.
   - Add deferred MCP exposure + a `tool_search` tool whenever total MCP schema bytes exceed ~16 KB.

### Tier 2 — Verification & Autonomy Multipliers
5. **Pre-Completion Self-Review & Verification Gate**
   - Build a first-class `/review` workflow + optional automatic `Stop`-hook gate on non-trivial diffs: if files were mutated in the turn and zero `validation: true` checks ran (or `PostEditDiagnostics` still reports errors), inject a continuation prompt before allowing turn completion.
6. **Wire `pkg/dap` into an Agent `debug` Tool**
   - Expose a single high-level `debug` tool (`run_to_breakpoint` / `inspect` / `step` / `stop`) backed by `pkg/dap` + `pkg/debugadapter` so Wingman can inspect live stack frames and variables on failing tests without `println` churn.
7. **Mid-Turn Stream Preemption on User Steering**
   - Cancel and flush in-flight SSE generation in `pkg/harness/agent.go:551+` when user steering arrives before any mutating tool has started, matching Codex's `RealtimeSteer` responsiveness.

### Tier 3 — Enterprise Hardening & Long-Horizon Polish
8. **Host Loopback Domain-Allowlist HTTP/HTTPS Proxy for Sandboxed `exec`**
   - Allow `go`, `cargo`, `npm`, `uv`, and `pip` package registries in sandboxed runs via `HTTP_PROXY`/`HTTPS_PROXY` pointed at a host-side domain-filtering loopback proxy without granting raw `network: true` IP access, and require a fresh `ReviewAction` whenever a command escalates out of the sandbox.
9. **Deterministic Command Policy Rules + Background Memory Consolidation**
   - Pair `ReviewAction` with fast deterministic allow/prompt/deny command-prefix rules, and add an opt-in idle background job to distill completed session journals (`pkg/session`) into `ws.MemoryPath` + project skills.
10. **Continuous Head-to-Head Eval Loop (`bench/acpflow` & `bench/tbench`)**
    - Expand `bench/acpflow` into a 20–30 task regression suite (multi-file refactors, subtle bug diagnosis, Plan->Build cache hit verification, MCP-heavy tasks) to measure token efficiency, cache-read ratio, and pass rate across harness changes.
