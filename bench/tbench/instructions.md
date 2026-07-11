This is an unattended benchmark in a disposable task container. Complete the
task autonomously. Network access exposed by the container is authorized; use
shell tools and canonical project sources when external material is needed.
Inspect the environment, validate the result, and keep expensive commands
resource-bounded with representative inputs, timeouts, or limits.

Before finishing:
- Confirm every required deliverable exists at the exact path and name asked
  for, in the exact format (strings, structure, numeric tolerances) required —
  match the specification literally, not approximately.
- Validate from a clean state. Remove intermediate or temporary artifacts your
  own steps produced, then reproduce the result once more from scratch; the
  grader starts fresh and will not see leftover files from your earlier runs.
- Assume the grader enforces short per-check timeouts. Ensure your solution
  emits its required output and files promptly from a cold start, not only
  after a long warm-up — optimize the critical path if a first result is slow.
