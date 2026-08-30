# Douyinie Agent Context & Guidelines

When working on Douyinie architecture, provider selection, benchmarks, implementation planning, or code changes:

1. **Hierarchy of Truth**:
   - **[Wayfinder Issue #1](https://github.com/monet88/douyinie/issues/1)**: Canonical source of truth for resolved decisions.
   - **[Implementation Spec #18](https://github.com/monet88/douyinie/issues/18)**: Authoritative Phase 1 implementation spec (body + normative revalidation amendments).
   - **[`docs/architecture/phase1-architecture.md`](docs/architecture/phase1-architecture.md)**: Canonical repo-local Phase 1 architecture synthesis; it materializes the locked #16 architecture plus authoritative #18 amendments. If it drifts from #1/#18, GitHub issues win.
   - **[`PRODUCT.md`](PRODUCT.md)** & **[`CONTEXT.md`](CONTEXT.md)**: Repo-local product charter and domain invariant glossary.
   - **[`docs/reference-repositories.md`](docs/reference-repositories.md)**: Mining and reference inventory (**not** a dependency lockfile).

2. **Core Pipeline Invariants**:
   - **Dialogue/Narration Dubbing Only**: Preserve BGM, sound effects (Foley/ambient), singing/music-vocals, and instrumental outros perceptually and semantically. Never replace full audio tracks with TTS.
   - **Immutable Source Timing**: Video cuts are locked; shorten/rewrite text concise first; probe actual duration; enforce zero overrun (`tts_finish <= source_end`) with natural inter-turn breathing room.
   - **Multi-Role `TextRegionPlan`**: Classify text as `speech_subtitle`, `semantic_text`, `instructional_ui_text`, `brand_keep`, or `ignore`. In-place cover/overlay is the default; inpainting is non-default.
   - **Compact Fit-Content Subtitle Box**: Background box must hug rendered text (1–2 lines max, compact padding), never a full-width rectangle. Keep UI buttons, timeline tracks, and finger tap targets unobstructed.
   - **Pre-Dub Voice Audition**: AI recommended default, 5s standalone or 10s contextual audition mixed with video BGM, 1 stable voice per speaker per run.
   - **Target Languages**: Vietnamese AND English (VI/EN).
   - **No Facial Lip-Sync**: Timeline-constrained speech alignment defines V1; facial lip sync is out of scope.

3. **Obligation Layers**:
   - Keep `CODE_LICENSE`, `MODEL_LICENSE`, `DATA_LICENSE`, and `SERVICE_TERMS` separate. Rewriting source code does not remove model/data obligations.

4. **Testing Seams**:
   - Maintain exactly two approved architectural integration/acceptance seams: **Seam 1** (localhost RuntimeHost API) and **Seam 2** (StageWorker runtime contract). Ordinary in-memory unit tests for pure helpers/parsers/math do not create another architectural seam.

## Agent Skills

### Issue Tracker
Issues tracked in GitHub Issues via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage Labels
Five default triage labels, strings equal to role names. See `docs/agents/triage-labels.md`.

### Domain Docs
Single-context layout — root `CONTEXT.md` + `docs/adr/`; Phase 1 architecture is materialized in `docs/architecture/phase1-architecture.md`. See `docs/agents/domain.md`.


## Agent Orchestration

When coordinating multiple coding agents:

- Honor the exact supervisor, implementation-worker, and reviewer families selected by the user/coordinator. Never substitute another agent family without explicit authorization.
- A supervisor is control-plane only unless explicitly assigned implementation or review work.
- Explicit workflow Skill invocation must be the first characters of the actual agent-visible prompt; do not prepend lifecycle prose or transport markers.
- Before launching AGY, OpenCode, Pi, or another agent with a documented transport workaround, read [`docs/agents/orca-orchestration.md`](docs/agents/orca-orchestration.md) and follow its current repo-local launch policy.
- Preserve one editing owner for overlapping mutable scope and use fresh, read-only reviewer sessions.
- Agent completion claims are evidence, not proof. Verify repository state, required tests/checks, and acceptance criteria before declaring PASS.

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
