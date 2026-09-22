# Douyinie — Coding Standards

Practical, repo-specific engineering standards for Douyinie Phase 1. These rules are grounded in the current codebase, `AGENTS.md`, `CONTEXT.md`, `PRODUCT.md`, and the canonical Phase 1 architecture. Follow them unless an authoritative issue/spec explicitly changes the contract.

## 1. Source of truth

When requirements disagree, use this order:

1. [Wayfinder Issue #1](https://github.com/monet88/douyinie/issues/1) — resolved product/architecture decisions.
2. [Implementation Spec #18](https://github.com/monet88/douyinie/issues/18) — authoritative Phase 1 implementation contract, including normative amendments.
3. [`docs/diagrams/douyinie-architecture.json`](docs/diagrams/douyinie-architecture.json) — repo-local materialization of the locked architecture (generated diagram set; read the JSON source, not the HTML render).
4. [`PRODUCT.md`](PRODUCT.md) and [`CONTEXT.md`](CONTEXT.md) — product charter and domain vocabulary/invariants.
5. [`docs/reference-repositories.md`](docs/reference-repositories.md) — reference/mining inventory only; never treat it as a dependency lockfile or architecture authority.

Do not silently reinterpret an accepted architecture decision because a local implementation would be easier another way. If implementation evidence exposes a genuine contradiction, surface it before redesigning the contract.

## 2. Language, formatting, and dependencies

- Production control-plane code is **Go 1.25.5** (`go.mod`).
- All Go changes must be `gofmt` clean. `gofmt -l` over touched files must print nothing before completion.
- Prefer the Go standard library and existing dependencies before adding a new dependency.
- Add a dependency only when it removes meaningful complexity that cannot be handled cleanly with the standard library or an existing package.
- Keep provider/model SDK specifics behind the existing provider or worker boundary. Domain and orchestration code must not become SDK-shaped.
- Do not commit generated executables, temporary media, model checkpoints, local credentials, test output, or machine-local paths unless an explicit repository contract requires them.

## 3. Architectural boundaries

These are load-bearing Phase 1 constraints.

### Control plane

- `RuntimeHost` is the authoritative control-plane process.
- `RuntimeHost` owns the localhost API, Job/Run lifecycle, orchestration, review projection, scheduling, and authoritative SQLite writes.
- The browser/local UI talks to the versioned RuntimeHost API. UI code must not bypass the API to manipulate model processes, SQLite, or CAS files directly.
- **RuntimeHost is the sole SQLite writer.** StageWorkers report results; they do not mutate orchestration SQLite directly.

### Execution plane

- Heavy ML/media execution belongs in isolated **StageWorker** families.
- RuntimeHost ↔ StageWorker communication uses the versioned metadata-only worker contract (Phase 1 baseline: NDJSON over stdin/stdout).
- Media bytes move through artifact references/paths, not inline IPC payloads.
- Worker-family/provider implementation details must not leak into stable domain contracts.
- Phase 1 assumes a single active GPU lease. Family switching must release ownership deterministically rather than relying on incidental process-local locks.
- Cancellation/crash handling must terminate the entire worker descendant tree on Windows; never leave FFmpeg/Python/CUDA descendants orphaned.

### Architectural test seams

Maintain exactly two architectural integration/acceptance seams:

1. **Seam 1 — RuntimeHost localhost API contract.**
2. **Seam 2 — StageWorker/runtime contract.**

Pure unit tests for helpers/parsers/math are encouraged and do not create a third architectural seam. Do not introduce another public testing seam merely to make implementation easier to test.

## 4. Domain and artifact integrity

### Source truth is immutable

- `SourceLocator` is an input locator, not media identity.
- `SourceAsset` is immutable normalized media identified by content fingerprint after integrity validation and rights attestation.
- Source timing anchors are immutable. Do not shift later speech, stretch video, or mutate source cuts to rescue a target-language fit problem.
- Use integer millisecond source coordinates at the domain boundary where the current model uses `start_ms` / `end_ms`.

### Source-derived vs. target-derived artifacts

Source-derived work must remain reusable across VI/EN when dependency/config hashes still match. Examples include transcript/alignment, speaker assignment, audio stems, and text-region tracking.

Target-language edits invalidate only their declared descendants. Preserve the targeted invalidation model:

```text
Target text edit
  -> TTS attempt -> DubSegment -> DubMix -> LocalizedSubtitleTrack -> Render descendants

Voice assignment edit
  -> affected speaker TTS -> affected DubSegment -> DubMix -> Render descendants

Text region / geometry edit
  -> TextRegionPlan -> LocalizedVisualTrack -> Render descendants
```

Do not rerun source acquisition, ASR, forced alignment, diarization, separation, or OCR merely because target text, a voice, or a region override changed.

### CAS and provenance

- Media and serialized artifact payloads are immutable CAS objects addressed by content hash.
- Persist canonical references only after the artifact is completely written and validated.
- CAS writes must preserve same-volume staging/atomic publication semantics; do not introduce a Windows symlink dependency.
- Cache identity must be deterministic over semantic inputs: stage + input hashes + semantic config + provider/model/version + target language where applicable + schema/pipeline version.
- Cache identity must **not** depend on paths, mtimes, JobID, or RunID.
- A retry or rerun creates new immutable evidence; it does not rewrite historical evidence in place.
- Changing what a stage emits or persists requires bumping that stage's schema version (or its semantic-config token) in the same change; otherwise already-cached rows replay the old behavior and the new behavior is silently disabled.

## 5. Append-only evidence and review semantics

The following are evidence, not mutable status slots:

- `ProviderAttempt`
- `SelectionDecision`
- `QualityResult`
- review/manual-override audit records

Preserve append-only semantics.

`QualityResult` is separate from execution state: a technically `SUCCEEDED` stage may still be `REVIEW_REQUIRED` or `FAIL`.

Review approval semantics are:

- `auto_pass` — automated evidence passed.
- `auto_resolved` — a targeted rerun produced a passing replacement and the exception leaves the queue.
- `manual_override` — a human accepts a still-flagged candidate; the original quality evidence remains intact and is never rewritten to `PASS`.

The operator queue is exception-only. Passing, auto-resolved, and manually overridden items must not remain actionable queue entries.

## 6. Audio and timing invariants

- Dub **dialogue/narration only**.
- Preserve BGM, ambience, Foley/SFX, singing/music vocals, and instrumental showcases as the source plan permits.
- Never replace the complete source soundtrack with a TTS-only track.
- Inside speech windows, suppress source dialogue and mix localized speech against the accepted preserved/background candidate.
- Outside speech windows, preserve the original soundtrack as the accepted mix plan permits.

### Measured-media truth

- Predicted/WPM duration is planning evidence only.
- A selectable TTS candidate must be checked against **actual synthesized audio duration**.
- Keep immutable source start/end anchors separate from playback acceptance. A selected dub may extend only to its persisted `DubPlaybackEndMs`, derived from proven source silence before the next canonical speech/vocal boundary with a frozen reserve.
- Validate the decoded waveform/sample extent against that playback end and against neighboring localized clip placement; metadata duration alone is not acceptance proof.
- An overlong candidate must not reach the final mixer.
- Resolve overrun through the approved adaptation loop: rewrite/resynthesize/rate/mild stretch/local regroup within the same speaker turn.
- Never fix one overrun by shifting later source speech.
- Preserve perceptible inter-turn breathing room; fitting arithmetic alone is not sufficient quality evidence.

### Mixer purity

The mixer executes an accepted timing/mix plan. It must not become a hidden timing-policy engine that truncates words, shifts anchors, or invents rescue behavior.

## 7. Visual-text invariants

`TextRegionPlan` roles are first-class domain meaning:

- `speech_subtitle`
- `semantic_text`
- `instructional_ui_text`
- `brand_keep`
- `ignore/noise`

Rules:

- Deterministic cover/overlay is the default localization path.
- Inpainting is a non-default fallback, not the common path.
- `brand_keep` content remains untouched unless an authoritative requirement says otherwise.
- Subtitle/overlay backgrounds must be compact and fit-content; never introduce a destructive full-width rectangle as a shortcut.
- Preserve scene-aware non-occlusion: UI buttons, timelines, sliders, tap targets, and active demonstration regions remain usable/visible.
- Fixture measurements may guide a fixture test, but do not turn one observed video-specific number into a global threshold without an explicit spec decision.

## 8. Provider, policy, licensing, and secrets

Provider routing order is fixed:

```text
policy eligibility
-> declared capability
-> runtime health
-> execution profile / quality / cost
```

A healthy provider that is policy-blocked is not routable.

Keep obligation layers separate:

- `CODE_LICENSE`
- `MODEL_LICENSE`
- `DATA_LICENSE`
- `SERVICE_TERMS`

Reimplementing code does not erase model/data/service obligations.

- Unmanifested checkpoints fail closed.
- Policy/auth/license rejection fails closed; do not retry past policy.
- Transient provider failures may retry only through the configured retry/circuit-breaker policy.
- Quality failure creates evidence first, then may select an allowed alternate for the affected scope.
- Credentials remain in OS/provider credential storage. Persist/log only safe references.
- Run snapshots, bundle manifests, logs, provenance, and tests must not contain API keys, tokens, cookies, session secrets, signed URLs, or machine-local credential material.
- External telemetry is off by default.

## 9. Go module responsibilities and design

Keep responsibilities local and explicit.

- `internal/domain` — stable domain vocabulary, value types, enums, invariant errors, artifact contracts.
- `internal/service` — orchestration/business behavior across domain + storage/provider seams.
- `internal/provider` — provider-neutral interfaces, routing-compatible adapters, deterministic fakes.
- `internal/storage` — SQLite migrations/index persistence and storage contracts.
- `internal/cas` — immutable content-addressed artifact storage.
- `internal/server` — HTTP transport/validation/status mapping; keep business policy in services/domain rather than handlers.
- `internal/worker` — worker/runtime protocol and execution-boundary concerns.
- `internal/scheduler` — resource scheduling/lease ownership.
- `internal/queue` — persisted queue behavior.
- `test/seam1` — RuntimeHost acceptance behavior.
- `test/seam2` — StageWorker/runtime contract behavior.

### Design rules

- Prefer the smallest stable public seam that proves the behavior.
- Reuse existing domain/service boundaries before inventing a new interface.
- Do not create an interface, factory, registry, configuration layer, or generic framework for one concrete need unless the architecture requires the variation.
- Keep HTTP handlers thin: parse/resolve request context → call service → map typed/sentinel errors → serialize response.
- Keep pure transformations pure where practical; persistence/provider side effects should remain visible at orchestration boundaries.
- Keep source/target separation explicit rather than encoding it as comments or naming conventions only.
- Prefer deletion and simplification over parallel paths. There should be one canonical production path for a behavior.

## 10. Error handling

- Return errors; do not panic for expected runtime/input/provider conditions.
- Add useful operation context when propagating errors, using `%w` when callers need `errors.Is` / `errors.As` semantics.
- Use domain sentinel/typed errors for conditions that transport layers need to classify.
- HTTP handlers map known domain/storage errors to stable status codes; do not string-match arbitrary error text when a typed/sentinel error exists.
- Fail closed when required evidence, policy authorization, manifests, artifacts, or services are missing.
- Do not swallow persistence/CAS errors that can leave canonical metadata inconsistent with artifact state.
- Do not hide a failing quality or policy condition just to keep the pipeline moving.

## 11. SQLite and migrations

- Schema changes use explicit, monotonic migrations.
- Preserve older immutable history; migrate metadata/schema explicitly rather than silently rewriting historical artifact payloads.
- Add indexes for actual lookup patterns, not speculative future queries.
- Keep SQLite metadata and CAS publication ordered so a canonical database reference never points to a partial/nonexistent artifact.
- Tests for migrations must prove upgrade behavior from the relevant previous schema state when the change is non-trivial.

## 12. Testing standards

### Behavior first

Tests should verify externally meaningful contracts, not private call graphs.

A useful regression test must fail if the real production behavior regresses. A mock-only test that bypasses the production branch it claims to protect is insufficient.

### Seam 1

Use the real RuntimeHost + SQLite + CAS + scheduler/orchestrator path with deterministic fake providers and tiny synthetic media. Use Seam 1 for observable business/architecture behavior such as:

- policy-before-health routing
- measured TTS overflow refusal
- no adjacent-speech collision / no anchor drift
- source-artifact reuse and targeted invalidation
- retry/selection provenance
- preview/final render-plan parity
- exception-only review semantics
- queue-zero Auto vs Review final-render handoff

### Seam 2

Use Seam 2 for worker/runtime behavior that the HTTP API cannot prove directly:

- NDJSON/schema version compatibility
- structured worker error envelopes
- heartbeat/lifecycle
- cooperative cancel and escalation
- Windows process-tree cleanup
- GPU lease acquire/release/family handoff

### Unit tests

Use ordinary unit tests for deterministic helpers, parsing, layout math, provenance hashing, role transforms, and other local pure behavior.

### Determinism

- Default test suites must not require live network services, paid providers, authenticated browser state, or a real GPU unless the ticket explicitly defines a real smoke/acceptance gate.
- Provider fakes must expose the behavior under test rather than shortcut around it.
- Keep expected values independent from the implementation calculation where possible.
- When a real smoke is required, report it separately from deterministic test success.

## 13. Change workflow

Before editing production symbols:

1. Read `AGENTS.md` and the authoritative issue/spec for the task.
2. Inspect `git status`, branch, HEAD, upstream, and unrelated WIP.
3. Use GitNexus `impact` on each symbol being changed (`direction: upstream`, repo `douyinie`). If the index is stale — `gitnexus status` reports an indexed commit behind HEAD — refresh it first with `gitnexus analyze --index-only` (never `--force`; see `AGENTS.md`) and then run `impact`.
4. If impact is HIGH or CRITICAL, surface the blast radius before proceeding.
5. Map each acceptance criterion to an observable test/check.

During implementation:

1. Keep one editing owner for overlapping mutable scope.
2. Work in small vertical changes; run the narrowest relevant test after each material behavior change.
3. Inspect `git diff` regularly; do not allow unrelated formatting or cleanup to enter the task.
4. Preserve existing WIP and immutable architecture unless the task explicitly supersedes it.
5. Use a fresh independent reviewer for important changes.

Before committing:

1. Run focused tests for changed behavior.
2. Run the full deterministic suite.
3. Run required real smoke/acceptance checks when the ticket calls for them.
4. Run `go vet ./...`.
5. Run `gofmt -l` over touched Go files; output must be empty.
6. Run `git diff --check`.
7. Run GitNexus `detect_changes()` / CLI equivalent for repo `douyinie` and inspect the affected processes/risk.
8. Inspect the final diff for secrets, machine-local paths, generated binaries/media, debug output, and unrelated changes.
9. Stage exact intended paths; do not use broad staging when unrelated WIP may exist.

## 14. Completion gates

A normal Go behavior change is not complete until the applicable gates are green. Run `powershell -File scripts/gate.ps1` or run the steps individually:

```powershell
gofmt -l <touched-go-files>
go vet ./...
go test ./...
git diff --check
gitnexus detect-changes --scope all
```

For documentation-only changes, the minimum gate is:

```powershell
git diff --check
```

Add targeted acceptance commands required by the issue; these minimums do not replace ticket-specific gates.

Do not report a command as PASS unless it actually ran successfully on the relevant tree/source state.

## 15. Code-review standard

Review the change against two independent axes:

1. **Repository standards:** architecture boundaries, code quality, error handling, security, persistence, testing, change hygiene.
2. **Originating requirement:** every issue/spec acceptance criterion is implemented and proven at the correct seam.

A green test suite does not override a spec mismatch. A reviewer approval does not replace final repository verification.

Review findings should identify:

- severity
- concrete file/symbol/behavior
- violated contract or acceptance criterion
- evidence/reproduction path
- smallest correct remediation

## 16. Code taste

Use this decision ladder before adding machinery:

1. Does this need to exist?
2. Can the existing path already express it?
3. Can the standard library solve it clearly?
4. Can an existing dependency or platform primitive solve it?
5. What is the smallest implementation that preserves the locked invariants?

Additional rules:

- YAGNI over speculative extensibility.
- Boring, explicit code over clever indirection.
- One canonical path over duplicated "temporary" paths.
- Delete obsolete code instead of preserving dead compatibility layers without a requirement.
- Comments explain **why / invariant / ceiling**, not what an obvious statement does.
- Avoid magic thresholds. Name and document policy/quality values that are genuinely configurable or normative.
- Do not generalize from one acceptance fixture into a universal rule without evidence.

## 17. Commit conventions

Current history uses concise Conventional-Commit-style subjects. Continue that pattern:

```text
feat(review): complete exception inspector workflow (#41)
docs(gitnexus): refresh index stats
test: seed audio role plan in full-dub integration fixture
```

Guidelines:

- Use `type(scope): subject` when a useful scope exists; `type: subject` is fine when it does not.
- Keep the subject concise and behavior-oriented.
- Reference the issue when the commit completes or materially implements that ticket.
- Do not mix unrelated documentation/generated artifacts/WIP into the implementation commit.
- Never commit credentials, auth material, local model paths, or machine-specific secrets.
