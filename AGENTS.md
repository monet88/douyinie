# Douyinie Agent Steering

## Hierarchy of Truth

1. **[Wayfinder Issue #1](https://github.com/monet88/douyinie/issues/1)** → resolved decisions.
2. **[Implementation Spec #18](https://github.com/monet88/douyinie/issues/18)** → Phase 1 spec + normative amendments.
3. **[`docs/diagrams/douyinie-architecture.json`](docs/diagrams/douyinie-architecture.json)** → repo-local architecture materialization (generated diagram set: architecture, dataflow, sequence, workflow, lifecycle). Read the JSON source; the `.html` files are 15k-line human renders. GitHub issues win on drift.
4. **[`PRODUCT.md`](PRODUCT.md)** & **[`CONTEXT.md`](CONTEXT.md)** → product charter, domain invariants.
5. **[`docs/reference-repositories.md`](docs/reference-repositories.md)** → mining inventory (not a dependency lockfile).

## Load-Bearing Guardrails

- **Two testing seams only**: Seam 1 (RuntimeHost localhost API) and Seam 2 (StageWorker runtime contract). In-memory unit tests are not additional seams.
- **Production translation is remote-LLM only**: VI/EN speech and visual-text translation route through the authorized gateway in order **gemini-3.8-flash** -> **deepseek-v4.1-flash**. Local LLM translators are not production-eligible and are not fallbacks. If both remote lanes are unavailable or rejected by QA/policy, fail closed or surface review.
- **Reserve constrained local compute for specialized media stages**: local hardware is for ASR/alignment/diarization, TTS, audio separation, OCR/tracking, mixing, and render. Do not reintroduce a local general-purpose LLM merely to satisfy a local-only ideology when an authorized API lane is available.
- **Obligation layers**: `CODE_LICENSE`, `MODEL_LICENSE`, `DATA_LICENSE`, `SERVICE_TERMS` stay separate. Code changes do not remove model/data obligations.
- **Completion is evidence-gated**: agent claims are evidence, not proof. Verify repo state, tests, and acceptance criteria before declaring PASS.
- **GitNexus index refresh**: `gitnexus analyze --index-only` is safe. `gitnexus analyze --force` rewrites this file's machine-managed block with a machine-local runner path and live index statistics — run `git checkout -- AGENTS.md` before staging, or `test/steering` fails.

## Context by Task

| Task | Read before starting |
|------|---------------------|
| **Architecture / design** | `docs/diagrams/douyinie-architecture.json`, `CONTEXT.md`, relevant `docs/adr/` |
| **Implementation** | Above, plus the ticket's spec, `PRODUCT.md` §invariants, `docs/agents/domain.md`, and `CODING_STANDARDS.md` §13 (pre-edit and pre-commit checklists) |
| **Review** | Above, plus `docs/agents/orca-orchestration.md` §Mutation and Verification Boundaries |
| **Coordination** | `docs/agents/orca-orchestration.md` (roles, workflow skills, mutation policy, continuity) |
| **Issue tracker ops** | `docs/agents/issue-tracker.md` |
| **Triage** | `docs/agents/triage-labels.md` |
| **Domain vocabulary** | `docs/agents/domain.md` → `CONTEXT.md` + `docs/adr/` |
| **Out-of-repo file discovery** | `docs/agents/fastctx-discovery.md` (exact-first, bounded-fallback) |
| **Live E2E on real media** | `docs/agents/live-runtime-e2e.md` (runtime matrix, provisioning, UI drive, run read-back) |

## Agent Orchestration (summary)

Full policy: [`docs/agents/orca-orchestration.md`](docs/agents/orca-orchestration.md).

- Honor user-selected agent families; never substitute without authorization.
- One editing owner per overlapping mutable scope; reviewers are read-only.
- Workflow Skill invocation must lead the agent-visible prompt.

<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **douyinie**. For current index statistics (symbols, relationships, execution flows), read `gitnexus://repo/douyinie/context`.

> Index stale? Run `gitnexus analyze --index-only` from the project root. No `gitnexus` CLI yet? Bootstrap with `bunx` or `pnpm dlx` — e.g. `bunx gitnexus@latest analyze` (npm 11 npx crash; #1939).

## Always Do

- **MUST run impact analysis before editing.** Use `impact({target: "symbolName", direction: "upstream"})` (MCP) or `gitnexus impact "symbolName" --direction upstream` (CLI fallback); report callers, processes, and risk. Never substitute grep for graph analysis.
- **MUST analyze graph changes before committing.** Use `detect_changes({scope: "all"})` (MCP) or `gitnexus detect-changes --scope all` (CLI fallback). `partial: true` or `truncated: true` is not a clean check — a zero means unseen, not unaffected; re-run it. For regression review: `detect_changes({scope: "compare", base_ref: "main"})` or `gitnexus detect-changes --scope compare --base-ref "main"`.
- **MUST warn the user** if impact analysis returns HIGH or CRITICAL risk before proceeding with edits.
- **MUST treat `risk: UNKNOWN` as unresolved, not as low.** An empty caller set is not evidence the symbol is unused — it can also mean the callers are not resolvable by the index (plain-object property access, dynamic dispatch, cross-language calls). `impact` pairs `UNKNOWN` with a `riskNote` saying so. Confirm with a text search before treating the symbol as safe to change or delete; do not proceed on the strength of a zero.
- When exploring unfamiliar code, use `query({search_query: "concept"})` to find execution flows instead of grepping. It returns process-grouped results ranked by relevance.
- When you need full context on a specific symbol — callers, callees, which execution flows it participates in — use `context({name: "symbolName"})`.
- For security review, `explain({target: "fileOrSymbol"})` lists taint findings (source→sink flows; needs `analyze --pdg`).

## Never Do

- NEVER edit a function, class, or method before MCP/CLI impact analysis.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis, and never read `UNKNOWN` as an all-clear — it means the walk could not answer, which is the one verdict that requires confirming by other means.
- NEVER rename symbols with find-and-replace — use `rename` which understands the call graph.
- NEVER commit before MCP/CLI graph change analysis.

## Resources

| Resource | Use for |
| --- | --- |
| `gitnexus://repo/douyinie/context` | Codebase overview, check index freshness |
| `gitnexus://repo/douyinie/clusters` | All functional areas |
| `gitnexus://repo/douyinie/processes` | All execution flows |
| `gitnexus://repo/douyinie/process/{name}` | Step-by-step execution trace |

## CLI

| Task | Read this skill file |
| --- | --- |
| Understand architecture / "How does X work?" | `~/.agents/skills/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `~/.agents/skills/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `~/.agents/skills/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `~/.agents/skills/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `~/.agents/skills/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `~/.agents/skills/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->
