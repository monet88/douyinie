# Douyinie Agent Context

When working on Douyinie architecture, provider selection, benchmarks, implementation planning, or code changes:

1. Read `docs/reference-repositories.md` before selecting or adding external components.
2. Treat the Wayfinder map as the source of truth for resolved product/architecture decisions: https://github.com/monet88/douyinie/issues/1
3. Treat `docs/reference-repositories.md` as a mining/reference inventory, **not** a dependency lockfile. Resolved Wayfinder decisions outrank generic repository examples.
4. Keep `CODE_LICENSE`, `MODEL_LICENSE`, and `DATA_LICENSE` separate. Rewriting code does not remove checkpoint/data/service obligations.
5. Core V1 dubbing is timeline-constrained speech alignment, not facial lip synchronization.
6. Preserve provider boundaries and fallbacks: CapCut/private APIs may accelerate the pipeline but must not be the only path required for core functionality.

If new external repositories become materially useful, update `docs/reference-repositories.md` rather than creating another competing list.

## Agent skills

### Issue tracker

Issues tracked in GitHub Issues via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Five default triage labels, strings equal to role names. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context layout — root `CONTEXT.md` + `docs/adr/`. See `docs/agents/domain.md`.
