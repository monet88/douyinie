# FastCtx Discovery: Exact-First, Bounded-Fallback

How agents find steering and configuration files that live outside the repository root.

## The problem

FastCtx `glob` and `inspect_local_file` can reach any path the process can read. Unbounded discovery (e.g. `glob("C:/Users/**/*.md")`) traverses application data, credential stores, caches, and temporary trees — producing permission errors, stale hits, and leaked paths that waste context and risk exposing secrets.

## Exact-first lookup

Always inspect known locations by exact path before searching:

| Source | Exact path |
|--------|-----------|
| Global agent steering | `C:/Users/monet/.omp/agent/AGENTS.md` |
| Repo agent steering | `<repo-root>/AGENTS.md` |
| Repo context | `<repo-root>/CONTEXT.md` |
| Repo product | `<repo-root>/PRODUCT.md` |
| Repo architecture | `<repo-root>/docs/architecture/phase1-architecture.md` |
| Repo agent docs | `<repo-root>/docs/agents/*.md` |

Use `inspect_local_file` (or native `view_file`) with the exact path. A single batched call can check multiple exact paths:

```
inspect_local_file({ paths: [
  "C:/Users/monet/.omp/agent/AGENTS.md",
  "<repo-root>/AGENTS.md",
  "<repo-root>/CONTEXT.md"
]})
```

FastCtx returns `Complete` or `Partial` per file. Handle both:

- **Complete** — full content returned; use directly.
- **Partial** — file exceeded the response limit; re-read with offset/limit or use native `view_file` with line selectors.
- **Not found / permission error** — the file does not exist at that location; proceed to bounded fallback only if the source is expected to exist somewhere.

## Bounded fallback

When a steering source has been deliberately relocated (e.g. a monorepo with per-package AGENTS.md), search within approved roots only:

### Approved search roots

1. The repository working tree (`<repo-root>/`)
2. The global agent config directory (`C:/Users/monet/.omp/agent/`)
3. The notes directory for the project (`F:/CodeBase/douyinie-notes/`)

### Excluded trees (never search)

- `C:/Users/monet/AppData/` — application data, caches, credentials
- `C:/Users/monet/.cache/`, `C:/Users/monet/.config/` (if present) — XDG-style caches
- Any `node_modules/`, `.git/objects/`, `vendor/`, `__pycache__/` subtree
- Temporary directories (`%TEMP%`, `/tmp`)
- Any path containing `.credentials`, `.ssh`, `.gnupg`, `.aws`

### Fallback search pattern

```
glob({ pattern: "AGENTS.md", path: "<approved-root>" })
```

Scope the glob to one approved root at a time. If the file is not found in any approved root, report absence — do not widen the search.

## Smoke scenario

A repeatable check that the discovery workflow resolves all required steering:

1. `inspect_local_file` with exact paths for global and repo AGENTS.md → both return Complete or Partial.
2. `inspect_local_file` with exact path for `CONTEXT.md` and `PRODUCT.md` → both return Complete or Partial.
3. No `glob` call needed if all exact lookups succeed.
4. If a file is missing from its exact location, `glob` within the repo root only → find or confirm absence.
5. No permission errors, no credential-path diagnostics, no unrelated file hits.

## When to use this workflow

- At session start when loading agent steering.
- When a task references an out-of-repo configuration file.
- When delegating to a subagent that needs steering context (pass exact paths, not discovery instructions).
