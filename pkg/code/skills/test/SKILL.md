---
name: test
description: Plan, add, repair, or run tests for a specified behavior. Use for test strategy, regression coverage, and test maintenance.
---
# Test

Target: $ARGUMENTS

Infer the mode from the request: a strategy is read-only, writing or repairing tests includes execution, and running tests does not by itself authorize changing production behavior. With no target, use the current change and conversation.

## Select meaningful coverage

Read the implementation and nearby tests, fixtures, helpers, and CI commands. Choose the lowest layer that exercises the real contract. Use integration tests where the risk lies between components; mocking that boundary would miss the point.

For each proposed test, identify the observable behavior and a plausible regression it would catch. Prioritize the requested behavior, the exact bug trigger, boundaries, and consequential failure or state-transition paths. Consider cancellation, cleanup, retries, persistence, concurrency, and compatibility only where relevant.

Avoid assertions about private structure, compiler-enforced properties, or framework behavior. Do not substitute a coverage percentage for an account of what is protected.

## Write or repair

Use the existing framework and fixtures. Keep setup minimal and assertions specific to the contract. Control time, randomness, and external dependencies; use synchronization rather than sleeps for concurrency tests. Verify that failures occur for the intended reason, especially in negative tests. For a regression, demonstrate failure against the original behavior when practical, using an isolated copy if testing an older revision would disturb current edits.

When repairing a failing test, determine whether the implementation, expected behavior, or test environment is wrong. Do not update snapshots blindly, weaken assertions to match a bug, or skip a flaky test to obtain a pass. Fix production code when resolving that behavior is part of the user's request; otherwise report the defect.

## Run and report

Reuse existing coverage when it already exercises the risk. Run the smallest relevant command and broaden for shared behavior or integration risk. After a change, rerun checks it could invalidate; do not add duplicate tests or repeat successful suites merely to complete a checklist.

Report the behaviors covered, commands and outcomes, and important gaps. Distinguish a failed assertion from a check that could not run because of missing dependencies, services, or permissions.
