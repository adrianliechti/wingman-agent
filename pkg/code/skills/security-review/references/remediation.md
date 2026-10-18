# Remediate verified findings

A request to fix scanner findings includes checking whether they are real. Validate raw reports using the review criteria or [triage.md](triage.md); do not stop solely because the user has not run a separate triage command. Patch supported findings and retain unresolved ones with the evidence needed to proceed.

Group issues by root cause and prioritize consequential exposure. Read callers and legitimate inputs before editing. Identify the control that should enforce the boundary and reuse an existing safe API when its contract fits. Prefer correcting the shared cause to patching each symptom; a larger coherent change may be necessary when the abstraction itself is unsafe.

Preserve legitimate behavior and explain any intentional compatibility change needed to close the vulnerability. Avoid unrelated cleanup. For dependency updates, use the project's package tooling, update the lockfile consistently, and check the relevant advisory and compatibility rather than assuming the newest release is the required fix.

Verify both sides of the boundary:

- The malicious or unauthorized input is rejected or made harmless for the stated reason.
- An authorized, legitimate input on the same path still works.
- Neighboring paths do not bypass the new control.
- Relevant tests and build checks pass, or their limitations are stated.

Use local regression tests or isolated reproductions within the authorized environment. A request to patch source does not authorize testing against production. A disappearing scanner warning alone does not prove remediation.

Inspect the final diff against each original finding. Report what was fixed, why the path is closed, checks performed, and what remains unresolved. Distinguish a code change from a verified fix and from a deployed fix.
