# Code mode

Code mode gives the model one more tool, `code`. Instead of calling tools one
at a time, the model writes a short Python script that calls them as
functions, and only what the script returns goes back into the conversation.
Loops, filters and sums over tool results then cost one model request rather
than one per call, and large intermediate results stay out of the context.

## What it is good for

Tasks that need several dependent or repetitive tool calls, or whose
intermediate results are large: paging through an MCP server's list and
aggregating it, reading many files and keeping one number from each, joining
the results of two tools.

On a tracker task that pages through 164 issues (the opt-in test
`TestLiveCodeMode`, three runs per model):

| Model | Input tokens, plain calls | Input tokens, code mode | Requests |
|-------|--------------------------:|------------------------:|---------:|
| gpt-6.2-sol | 18,463 | 1,521 | 3 → 2 |
| claude-sonnet-5-5 | 28,889 | 3,147 | 3 → 2 |
| claude-opus-5-5 | 28,896 | 3,079 | 3 → 2 |
| gemini-4-carbon | 19,673 | 1,966 | 3 → 2 |

All twelve code mode runs answered correctly with their first script. With
plain calls the model counts by reading, and two of the twelve runs miscounted.
Two smaller models, qwen3.8-27b and nemotron-3.5-lightning, answered correctly
in ten of ten code mode runs and in one of six with plain calls.

For a single call, or for calls whose output it wants to read in full, the
model is told to call the tools directly. On ordinary coding tasks (the
`invoice`, `refactor_move` and `file_ops` scenarios of `bench/acpflow`, five
runs each) offering the tool changed neither the pass rate nor, within noise,
the number of requests for gpt-6.2-sol, claude-opus-5-5 and qwen3.8-27b; the
models rarely reach for the tool there. nemotron-3.5-lightning passed equally
often either way, but its request counts vary too much between runs to tell.

## What a script can do

```python
pages = await asyncio.gather(*[tracker_list_issues(state="open", page=p) for p in (1, 2, 3)])
bugs = [i for page in pages for i in page["issues"] if "bug" in i["labels"]]
len(bugs)
```

- Every other tool is an `async` function taking the tool's parameters as
  keyword arguments. Names follow Python's rules: `-A` becomes `_A`, `from`
  becomes `from_`.
- A tool returns its text output. A tool that declares an output schema, as MCP
  servers can, returns the parsed value, and its signature is listed in the
  `code` tool's description.
- A failed tool call raises `RuntimeError`, which the script can catch.
- `asyncio.gather` runs calls together. Read-only calls overlap; calls that
  change something run one at a time, as the model's own parallel calls do.
- The script is checked against the tool signatures before it runs. A call
  that does not fit, an unknown name or a tool call that is never awaited
  rejects it: nothing has run, and the error shows the signatures of the tools
  it called.

The sandbox is [Monty](https://github.com/pydantic/monty), a Python subset
compiled to WebAssembly and embedded in the binary: no third-party packages, no
class inheritance, no generators. A script has no file or network access of
its own; it reaches the outside only through the tools. Each `code` call starts
fresh. A script gets one minute of interpreter time (time spent in tools does
not count), 256 MiB of memory and about a thousand tool calls, so a loop that
never ends stops early.

## Permissions

A tool called from a script is under the same rules as one the model calls
directly:

- **Hooks:** `PreToolUse`, `PostToolUse` and `PermissionRequest` hooks run for
  each call a script makes, and can block it.
- **Approvals:** a tool that needs approval asks for it from inside the script.
  The reviewer, or you, see that tool's name and arguments, not the script's.
  The script waits for the answer; a refusal reaches it as a `RuntimeError`.
- **MCP questions:** a question an MCP server asks during a call is shown as
  usual.
- **Plan mode:** the `code` tool stays available, and each call a script makes
  is checked on its own, so a script can read but not change anything.
- **Sandbox:** shell commands a script starts run in the same sandbox as any
  other.

Tools that are answered by someone watching the conversation cannot be called
from a script: the question tool, and tools provided by a connected editor.
The model calls those directly.

Calls made by a script are not listed one by one in the conversation. The
script itself is, and the tool's status line shows the call in progress. Files
a script edits and checks it runs are part of the turn's change review like
any others, and undoing the turn reverts them.

## Limits

- Images a tool returns do not reach the model through a script.
- Variables do not carry over from one `code` call to the next.
- The first use on a machine compiles the sandbox, which takes about six
  seconds; later starts take a fraction of a second.
- The sandbox adds about 28 MiB to the binary (9 MiB compressed).
