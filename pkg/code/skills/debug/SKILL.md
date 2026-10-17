---
name: debug
description: Diagnose a failure or unexpected behavior and fix its cause when requested. Use when the cause is unknown.
---
# Debug

Follow the evidence from the symptom to the cause. A diagnosis request calls for an explanation; a fix request includes remediation and verification.

Observed failure: $ARGUMENTS

## Reproduce and narrow

Establish expected versus actual behavior, the smallest known trigger, affected versions or environments, and the last known good state. Preserve exact errors and commands. Read relevant project instructions, logs, configuration, and tests.

Run the smallest reproduction within the authorized environment. If it cannot run, use the available evidence and state what remains unobserved. Compare working and failing cases, then trace the execution path, inputs, and error transformations. A recent change is a lead, not proof of causation.

## Test explanations

When the cause is unclear, rank competing explanations and choose the cheapest observation that distinguishes them: a focused test, boundary input, intermediate value, or comparison with a working version. Change one variable at a time. Do not manufacture alternatives once the failure mechanism is established. Remove temporary instrumentation before finishing unless it belongs in the fix.

A root cause must explain the trigger, the path through the code, and the observed failure. If the evidence supports only a hypothesis, label it as such and name the next discriminating check. Do not repeatedly run an unchanged failing command without a reason to expect new evidence.

## Fix and verify when requested

Correct the cause at the appropriate boundary. Preserve valid behavior and avoid broad fallback logic that hides the failure. Add a regression test when it meaningfully captures the bug; establish that it fails for the original reason and passes with the fix when practical.

Rerun the original reproduction and the relevant checks. For timing or environment issues, distinguish a passing run from evidence that the failure mechanism is removed.

Report the cause and supporting evidence, the fix if made, exact verification results, and unresolved uncertainty. Do not present a plausible explanation as a proven diagnosis.
