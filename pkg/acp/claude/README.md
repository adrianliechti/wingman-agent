# claude

ACP bridge for the Claude Code CLI. The session spawns `claude` with
`--output-format stream-json --input-format stream-json` and translates the
newline-delimited envelopes into ACP session updates.

## Protocol reference

- https://code.claude.com/docs/en/headless — the stream-json protocol from a
  programmatic consumer's view: `system/init`, `system/api_retry`, subagent
  messages carrying `parent_tool_use_id`, and the terminating `result` message.
- https://code.claude.com/docs/en/cli-reference — the flags `cliArgsLocked`
  passes: `--output-format`, `--input-format`, `--resume`, `--session-id`,
  `--add-dir`, `--mcp-config`, `--model`, `--effort`.
- https://code.claude.com/docs/en/agent-sdk/typescript — the message type
  schemas (`SDKSystemMessage` and friends). This is the closest equivalent to
  Codex's app-server reference.

Note that `anthropic-sdk-typescript` documents the Anthropic **API** (Messages
API), not this stream — it is a different layer and not a reference for this
package.

## Upstream review

The 2026-10-08 review compared Claude ACP at `6a162ba`, including the folded
prompt fix in `390b0b9`. Results that explicitly name the active prompt still
settle it even when their origin is a background task. Autonomous results
without a matching prompt ID, and their following idle events, cannot settle
or fail the active user turn. An explicit UUID list takes precedence over the
legacy singular UUID; unknown origins retain compatibility behavior.
Trailing idle events are accounted for on every root result, including late
events from the preceding prompt, so they cannot fail a newly started prompt.
An idle without a preceding result still reports an abandoned turn.
`stream_protocol_test.go` covers the autonomous, folded-prompt, and delayed-idle
paths.

An isolated initialize handshake with installed Claude Code 2.1.294 confirmed
that the launcher overrides resolve `haiku` to `claude-haiku-5-5`, `sonnet` to
`claude-sonnet-5-5`, `opus` to `claude-opus-5-5`, and `fable` to
`claude-fable-5-1`. Haiku advertises `low`, `medium`, `high`, `xhigh`, and `max`
effort through the CLI. Native ACP consumes this metadata directly; the
Wingman backend exposes its additional API-level `none` setting separately.
This handshake submitted no user prompt or inference request.

The 2026-10-05 review compared Claude ACP at `0724b17`, including its cancellation
and task fixes. Session close, delete, and replacement now wait up to five seconds
after process shutdown for cancelled prompt cleanup. Cancellation wins when a
result or process exit arrives at the same time. Child result and idle events
cannot settle the root turn. Task progress and summaries only update tool calls
that are still open; Monitor tasks do not produce background-task updates.
`upstream_review_test.go` covers these cases.

Wingman emits tools from complete assistant messages or permission requests, so
the upstream abandoned partial-tool bug does not apply. Context limits come from
the CLI's model metadata rather than token-count probes. This ACP v1 bridge does
not implement native subagent sessions, async-task extensions, or terminal exit
metadata; the corresponding upstream v2 and extension fixes need no wire changes
here.

## Plans

This bridge does not emit ACP `plan_update`. Current Claude Code exposes no
plan-emitting tool: the `TodoWrite` and `TaskCreate`/`TaskUpdate`/`TaskList`
tools that earlier versions surfaced are gone, so there is nothing to translate.
Plan *mode* is unrelated and still supported — see the `plan` entry in
`sessionModes` and the `ExitPlanMode` handling in `approvals.go`.

## Live regression tests

`TestLiveExitPlanModeEnablesBypass` drives the installed Claude CLI through ACP,
selects bypass when approving a plan, and uses a `UserPromptSubmit` hook to check
the CLI's actual permission mode on the next prompt. It makes two short model
requests using the current CLI configuration and requires hooks and bypass to
be available.

Run from the repository root:

```sh
CLAUDE_ACP_LIVE=1 go test ./pkg/acp/claude -run '^TestLiveExitPlanModeEnablesBypass$' -count=1 -v -timeout=4m
```
