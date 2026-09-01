# Douyinie Agent Steering

## Hierarchy of Truth

1. **[Wayfinder Issue #1](https://github.com/monet88/douyinie/issues/1)** → resolved decisions.
2. **[Implementation Spec #18](https://github.com/monet88/douyinie/issues/18)** → Phase 1 spec + normative amendments.
3. **[`docs/architecture/phase1-architecture.md`](docs/architecture/phase1-architecture.md)** → canonical repo-local architecture. GitHub issues win on drift.
4. **[`PRODUCT.md`](PRODUCT.md)** & **[`CONTEXT.md`](CONTEXT.md)** → product charter, domain invariants.
5. **[`docs/reference-repositories.md`](docs/reference-repositories.md)** → mining inventory (not a dependency lockfile).

## Load-Bearing Guardrails

- **Two testing seams only**: Seam 1 (RuntimeHost localhost API) and Seam 2 (StageWorker runtime contract). In-memory unit tests are not additional seams.
- **Obligation layers**: `CODE_LICENSE`, `MODEL_LICENSE`, `DATA_LICENSE`, `SERVICE_TERMS` stay separate. Code changes do not remove model/data obligations.
- **Completion is evidence-gated**: agent claims are evidence, not proof. Verify repo state, tests, and acceptance criteria before declaring PASS.

## Context by Task

| Task | Read before starting |
|------|---------------------|
| **Architecture / design** | `docs/architecture/phase1-architecture.md`, `CONTEXT.md`, relevant `docs/adr/` |
| **Implementation** | Above, plus the ticket's spec, `PRODUCT.md` §invariants, `docs/agents/domain.md` |
| **Review** | Above, plus `docs/agents/orca-orchestration.md` §Mutation and Verification Boundaries |
| **Coordination** | `docs/agents/orca-orchestration.md` (roles, workflow skills, mutation policy, continuity) |
| **Issue tracker ops** | `docs/agents/issue-tracker.md` |
| **Triage** | `docs/agents/triage-labels.md` |
| **Domain vocabulary** | `docs/agents/domain.md` → `CONTEXT.md` + `docs/adr/` |
| **Out-of-repo file discovery** | `docs/agents/fastctx-discovery.md` (exact-first, bounded-fallback) |

## Agent Orchestration (summary)

Full policy: [`docs/agents/orca-orchestration.md`](docs/agents/orca-orchestration.md).

- Honor user-selected agent families; never substitute without authorization.
- One editing owner per overlapping mutable scope; reviewers are read-only.
- Workflow Skill invocation must lead the agent-visible prompt.

<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **douyinie** (4165 symbols, 18444 relationships, 260 execution flows). Use the GitNexus MCP tools to understand code, assess impact, and navigate safely.

> Index stale? Run `node .gitnexus/run.cjs analyze` from the project root — it auto-selects an available runner. No `.gitnexus/run.cjs` yet? `npx gitnexus analyze` (npm 11 crash → `npm i -g gitnexus`; #1939).

## Always Do

- **MUST run impact analysis before editing any symbol.** Before modifying a function, class, or method, run `impact({target: "symbolName", direction: "upstream"})` and report the blast radius (direct callers, affected processes, risk level) to the user.
- **MUST run `detect_changes()` before committing** to verify your changes only affect expected symbols and execution flows. For regression review, compare against the default branch: `detect_changes({scope: "compare", base_ref: "main"})`.
- **MUST warn the user** if impact analysis returns HIGH or CRITICAL risk before proceeding with edits.
- When exploring unfamiliar code, use `query({search_query: "concept"})` to find execution flows instead of grepping. It returns process-grouped results ranked by relevance.
- When you need full context on a specific symbol — callers, callees, which execution flows it participates in — use `context({name: "symbolName"})`.
- For security review, `explain({target: "fileOrSymbol"})` lists taint findings (source→sink flows; needs `analyze --pdg`).

## Never Do

- NEVER edit a function, class, or method without first running `impact` on it.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis.
- NEVER rename symbols with find-and-replace — use `rename` which understands the call graph.
- NEVER commit changes without running `detect_changes()` to check affected scope.

## Resources

| Resource | Use for |
|----------|---------|
| `gitnexus://repo/douyinie/context` | Codebase overview, check index freshness |
| `gitnexus://repo/douyinie/clusters` | All functional areas |
| `gitnexus://repo/douyinie/processes` | All execution flows |
| `gitnexus://repo/douyinie/process/{name}` | Step-by-step execution trace |

## CLI

| Task | Read this skill file |
|------|---------------------|
| Understand architecture / "How does X work?" | `.claude/skills/gitnexus/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `.claude/skills/gitnexus/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `.claude/skills/gitnexus/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `.claude/skills/gitnexus/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `.claude/skills/gitnexus/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `.claude/skills/gitnexus/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->
