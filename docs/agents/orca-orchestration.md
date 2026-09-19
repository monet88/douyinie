# Douyinie Agent Coordination

This file contains only Douyinie-specific coordination policy. The installed Orca skill and the live, version-matched Orca guides are authoritative for Orca CLI syntax, agent launch/transport behavior, Run/Task/Dispatch lifecycle, readiness checks, recovery, and cleanup. Do not duplicate those mechanics here.

## Coordinator and Agent Roles

- ChatGPT is the default and final technical coordinator for Douyinie: it owns decomposition, coordination decisions, and final technical verification.
- Coding agents are execution workers or independent reviewers by default. Use a detached/local coding-agent supervisor only when the user explicitly requests one.
- AGY is the preferred implementation worker unless the user selects another worker.
- OpenCode and Pi may be used as independent reviewers/planners when available and appropriate.
- Honor any worker, reviewer, or supervisor family explicitly selected by the user. Do not silently substitute another agent family.
- Keep one editing owner per overlapping mutable scope. Independent reviewers use fresh sessions and remain read-only unless explicitly assigned fixes.

## Workflow Skills

- When a workflow skill such as `implement`, `to-spec`, `to-tickets`, or `code-review` is selected, let that skill own its procedure; do not replace it with a coordinator-authored checklist.
- Every delegated agent review must explicitly invoke `code-review` using the current agent-specific invocation syntax defined by the Orca skill.
- For delegated skill-driven work, follow the Orca skill's current prompt-shape and delivery rules rather than encoding transport-specific syntax in this repository.

## Source of Truth and Dependency Order

- GitHub Issues are the implementation tracker. Respect `blocked_by` and dependency ordering before starting implementation.
- Before implementation, implementation planning, or architecture-sensitive review, read `AGENTS.md`, `docs/diagrams/douyinie-architecture.json`, and the relevant `PRODUCT.md`, `CONTEXT.md`, spec, ADR, and ticket material.
- `docs/diagrams/douyinie-architecture.json` is the canonical repo-local architecture materialization (generated diagram set) and must not be silently contradicted during implementation.
- Wayfinder Issue #1 and Implementation Spec #18, including normative amendments, remain authoritative if repo-local architecture/product/context documents drift.

## Mutation and Verification Boundaries

- Preserve existing WIP and unrelated files. Do not reset, clean, stash, overwrite, broadly reformat, or otherwise simplify the tree by destroying user work.
- Do not commit, push, merge, close issues, publish tickets, or otherwise mutate GitHub unless the user has authorized that class of action.
- Treat worker and reviewer claims as evidence, not proof.
- Before declaring implementation or review complete, independently verify the relevant repository state, tracked and untracked diff, required tests/checks, acceptance criteria, and retained test evidence against the tested workspace state.
- If implementation evidence conflicts with locked architecture, surface the conflict explicitly instead of making a new architecture decision inside an implementation ticket.
- Treat explicitly documented residual implementation risks/tuning as validation work, not permission to reopen architecture.

## Continuity

- Live repository and GitHub state override stale project files, checkpoints, and prior chat context.
- For long conversations or handoffs, use the current compact/history workflow so exact identifiers, decisions, unresolved issues, and next actions remain recoverable.
