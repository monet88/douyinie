# Issue #95: ZeroTTS Vietnamese Production Routing — Verification & Provenance Record

> **Superseded pins (2026-09-17):** §1's ZeroTTS identity table below — and the
> `requirements-zerotts-0.1.2.txt` / `venvs/tts-zerotts-0.1.2-9d85578` references in §"Reproducibility" —
> record the state verified on 2026-09-15. The runtime pack and model revision have since moved to
> package `0.1.5`, source `47e466d7a1a36517cfd240de536523d17c00adac`, model `c2bfbd67dc648cac455077333f7cf5c18a2e3bb4`,
> venv `tts-zerotts-0.1.5-47e466d` and manifest `requirements-zerotts-0.1.5.txt`; see
> `docs/research/tts-runtime-upgrade-2026-09-17.md` §1 and §3 for the current values. Everything else
> in this record (routing, escalation, gate results at `31c7a20`) stands as the evidence of that revision.

**Date:** 2026-09-15
**Verdict:** PASS — #90 (ZeroTTS production integration) is implementation-ready for closure.
**Tested revision:** `31c7a205d744ef33b04a4efccce9e7a79e6743e5` (branch `main`), working tree unmodified (`git status --porcelain` empty before and after every gate).
**Scope:** verification/integration only. No production code was changed by this ticket. No third service-level seam was introduced.

**Environment:** Windows 11 Pro · Intel Core i5-12400F · NVIDIA GeForce RTX 2060 SUPER 8 GB · Go 1.25.5 windows/amd64.

---

## 1. Verified production identity

| Item | Pinned value | Bound at |
|---|---|---|
| ZeroTTS provider ID | `zerotts_tts_vi` | `internal/provider/tts.go:12` |
| ZeroTTS model | `zeroweight-ai/ZeroTTS` @ `8a0c3c29f6f047011f5cae02d0b14475a690be86` | `internal/domain/snapshot.go:202-203` |
| ZeroTTS package | `0.1.2` | `internal/domain/snapshot.go:205` |
| ZeroTTS source revision | `9d85578bee9321d6ef8305a4d454baf33e3fe861` | `internal/domain/snapshot.go:204` |
| Adapter revision | `cmd/stageworker/adapters/tts_engine.py@zerotts-0.1.2` | `internal/domain/snapshot.go:206` |
| Model snapshot (local, out of git) | `zeroweight-ai/ZeroTTS` local snapshot @ `8a0c3c29f6f047011f5cae02d0b14475a690be86` | `test/seam2/zerotts_seam2_test.go` |
| Runtime pack (local, out of git) | versioned local venv `tts-zerotts-0.1.2-9d85578` | `test/seam2/zerotts_seam2_test.go` |

Unattended VI rotation = the leading verified presets in frozen catalog order: `quangminh`, then `maichi`
(`provider.UnattendedZeroTTSVoices()` = `provider.ZeroTTSPresetVoices()[:provider.DefaultVIUnattendedVoiceCount]`,
which is derived from `domain.FrozenZeroTTSVoiceOrder`). The remaining six packaged presets stay selectable/auditionable but are never auto-assigned.

---

## 2. Gate results

| Gate | Command (CWD: repository root) | Result |
|---|---|---|
| Formatting | `gofmt -l cmd internal test` | empty output, exit 0 |
| Vet | `go vet ./...` | empty output, exit 0 |
| Whitespace | `git diff --check` | empty output, exit 0 |
| Full suite | `go test ./... -count=1 -timeout 3600s` | **19 packages `ok`, 0 FAIL**, exit 0 (seam1 195.9 s, seam2 126.7 s) |
| Seam 1 (verbose) | `go test ./test/seam1/ -count=1 -v -timeout 1800s` | **224 PASS / 2 SKIP / 0 FAIL**, 170.1 s |
| Seam 2 (verbose) | `go test ./test/seam2/ -count=1 -v -timeout 1800s` | **63 PASS / 4 SKIP / 0 FAIL**, 99.0 s |
| Focused Seam 2 ZeroTTS | `go test ./test/seam2/ -count=1 -v -run 'TestSeam2_ZeroTTS\|TestSeam2_WorkerTTSProvider_CPUOnlyResourceContract'` | **5 PASS / 0 FAIL**, 35.6 s |

### Skips — every one is an opt-in real-model lane, not a failure

| Skipped test | Reason |
|---|---|
| `TestSeam1_TTS_RealModelSynthesis_FitGateAndZeroOverrun` | `real_tts_seam1_test.go:59` — isolated TTS python environment not configured (`DOUYINIE_TTS_PYTHON_BIN`) |
| `TestSeam1_WorkerTTSProvider_RealStageWorkerSynthesis` | `real_tts_seam1_test.go:165` — same |
| `TestSeam2_TTSStage_RealVieNeuSynthesisAndDurationProbe` | `real_tts_seam2_test.go:29` — same |
| `TestSeam2_TTSStage_RealCosyVoice3SynthesisSpeedFitAndDurationProbe` | `real_tts_seam2_test.go:109` — same |
| real separator smoke | `seam2_test.go:2289` — `AUDIO_SEPARATOR_MODEL_DIR` not configured |
| real separator checkpoint smoke | `snapshot_seam2_test.go:1290` — same |

The real **ZeroTTS** smoke is *not* in this skip list: `TestSeam2_ZeroTTSRealPinnedRuntime` runs by default on Windows because both pinned local paths resolve, and it passed in 22.5 s.

---

## 3. Acceptance criteria — evidence per row

| # | Acceptance criterion | Evidence |
|---|---|---|
| 1 | Real local ZeroTTS StageWorker smoke against the pinned snapshot, offline, truthful WAV/media/provenance | `TestSeam2_ZeroTTSRealPinnedRuntime` PASS (22.47 s). Drives the real StageWorker subprocess over the real pinned snapshot + real `tts-zerotts-0.1.2-9d85578` runtime for both `quangminh` and `maichi`. Asserts: 48 kHz mono `wav`; `MeasuredDurationMs == media.ProbeWAVBytes(AudioData)`; audio SHA-256 matches the returned digest; `provider/model/version` equal the pinned identities; `SnapshotService` runtime identity observed at exactly source `9d85578…`, `zerotts==0.1.2`, adapter `@zerotts-0.1.2`. Offline is enforced in-test (`HF_HUB_OFFLINE`/`TRANSFORMERS_OFFLINE`/`HF_DATASETS_OFFLINE=1` plus `HTTP(S)_PROXY` pointed at a dead port). |
| 2 | Focused Seam 2 ZeroTTS runtime/snapshot/resource/capability tests, incl. CPU-only and no fake speed-fit | `TestSeam2_ZeroTTSRealPinnedRuntime` (also asserts the competing `asr` GPU lease is **still held** after ZeroTTS synthesis, and that `speed=1.1` returns `domain.ErrTTSSpeedUnsupported` **before** synthesis without touching the lease), `TestSeam2_ZeroTTSAbsentSnapshotAndMissingRuntimeFailClosed` (absent snapshot → `ErrSnapshotUnverified`; dependency-less venv → `runtime probe` failure), `TestSeam2_ZeroTTSInvalidEntrypointAndEmptyAudioFailClosed` (`TTS_VOICE_ASSET_MISSING`, `TTS_NO_AUDIO`), `TestSeam2_ZeroTTSExactModelIdentityFailsClosed` (`TTS_MODEL_UNSUPPORTED`), `TestSeam2_WorkerTTSProvider_CPUOnlyResourceContract`. |
| 3 | Focused Seam 1 default routing, audition, actual duration, historical VieNeu, zero-overrun, whole-speaker CosyVoice3 fallback | Default routing `TestSeam1_VI_DefaultVoiceAssignment_ZeroTTSUnattendedRotation`, `TestSeam1_EN_DefaultVoiceAssignment_KeepsKokoroLane`; audition `TestSeam1_ZeroTTS_AuditionOutsideUnattendedRotation` (asserts `data:audio/wav;base64,` payload, non-empty `measured_duration_ms`, and **no** filesystem path field); actual duration `TestSeam1_ZeroTTS_ActualWAVDurationIsAuthoritative`; VieNeu history `TestSeam1_HistoricalVieNeuAssignment_RemainsRoutableAndUnrewritten`; overrun `TestSeam1_ZeroTTS_OverrunRejectedWithoutSpeedResynthesis` + `TestSeam1_TTSOverflowGate_OverlongCandidateFlaggedForReview` + `TestSeam1_TTS_NoAdjacentCollisionAndNoAnchorDrift`; escalation `TestSeam1_ZeroTTS_UnresolvedOverrunEscalatesWholeSpeakerToCosyVoice3`, `…EscalationUsesFallbackMeasuredSpeedFit`, `…EscalationRetryIsDeterministic`, `…EscalationStillOverrunningProjectsReviewWithoutSecondHop`, `…EscalationRequiresEligibleFallbackLane`, `…SharedVoiceEscalationAuditsEveryChangedSpeaker`, `…StaleBaseEscalationCannotClobberNewerOperatorAssignment`, `…PreIssue94CachedVariantCannotSatisfyTheEscalationContract`, `…LegacySchemaPriorVariantIsNotReusedBySupersedingSynthesis`, `…ReviewCorrectionReportsTheFinalAssignmentAfterEscalation`, plus unit guards `TestCurrentRunAssignmentFailsSafeOnUnreadableCurrentAssignment`, `TestIsEscalationOfBaseRejectsDifferentSharedVoiceScope`. All PASS. |
| 4 | Existing TTS, audition, reassignment, snapshot, routing and fit-controller regressions | All PASS in the seam1/seam2 verbose runs: `TestSeam1_TTS_*` (18 tests incl. measured-duration speed-fit lane, regroup, policy block, frozen-per-run assignment, cross-run isolation, 409 on conflicting same-run reassignment), `TestSeam1_TTS_VoiceAudition_Contextual_*` (6), `TestSeam1_MultiSpeaker_*` (2), `TestSeam1_VoiceChange_*` (3), `TestSeam1_VoiceReassign_*` (2), `TestSeam1_Snapshot_*` (8), `TestSeam1_ProductionRegistry_FailsClosedWhenSnapshotAbsent`, `TestSeam1_Translation_PolicyFallbackRouting`, plus `internal/provider` (router, policy, fit) and `internal/service` suites in the full run. |
| 5 | #84 RuntimeHost/operator audition and run-bound reassignment contracts intact, scoped invalidation, no local-path leakage | No new RuntimeHost route was added (`internal/server/server.go` route table unchanged for this change set); ZeroTTS uses the existing `/voice-assignment`, `/voice-assignment/reassign`, `/voice-audition` endpoints. Run-bound reassignment and scoped invalidation: `TestSeam1_VoiceChange_InvalidationScope_And_SpeakerScopedRegeneration`, `TestSeam1_VoiceReassign_TargetedInvalidation_And_DownstreamRerun`, `TestSeam1_VoiceReassign_AssetScopedWithoutRunID`, `TestSeam1_TTS_CrossRunVoiceAssignmentIsolation`, `TestSeam1_TTS_VoiceAssignment_SameRun_ConflictingReassignmentReturns409`, `TestSeam1_TTS_VoiceAssignmentFrozenPerRun`. Local-path non-leakage: `TestSeam1_ZeroTTS_AuditionOutsideUnattendedRotation`. Real-browser operator UI: `TestSeam1_OperatorUIRegionCorrectionBrowserSmoke` PASS — `21/21 region correction browser smoke checks passed`. |
| 6 | Full repository tests and static checks per gate policy | §2 table: `gofmt`/`go vet`/`git diff --check` clean; `go test ./...` 19 packages ok / 0 FAIL. |
| 7 | Post-change GitNexus impact refreshed/rechecked for the CRITICAL provider/catalog/voice-verification surfaces, graph claims verified against source | §4. |
| 8 | Architecture/context/governance/provenance docs consistently identify ZeroTTS as VI default, CosyVoice3 as duration-controlled speaker-scoped fallback, VieNeu as compatibility/history, EN/Kokoro unchanged | §5. |
| 9 | No third seam, no voice-cloning claim, no sentence-level engine hopping, no implicit model download, no in-place active-runtime patching | §6. |
| 10 | Phase 3 blind scorecard not rerun | No benchmark was executed by this ticket. The operator waiver recorded in #90 stands; the repo-internal `internal/benchmark` package suite passed as part of the ordinary full run, which is the repo's own suite and not the Phase 3 blind scorecard. |

---

## 4. GitNexus blast-radius recheck

Index refreshed to the tested revision (`gitnexus analyze --force` → `Indexed commit: 31c7a20`, `Status: ✅ up-to-date`; 8,134 nodes / 41,574 edges / 215 clusters / 537 flows).

`gitnexus impact <symbol> --direction upstream`:

| Symbol | Risk | impacted | direct | processes | modules |
|---|---|---|---|---|---|
| `provider.ResolveTTSVoiceEntrypoint` (`internal/domain/snapshot.go`) | CRITICAL | 20 | 1 | 9 | 2 |
| `provider.IsVerifiedTTSVoice` | CRITICAL | 17 | 4 | 12 | 5 |
| `provider.DefaultPresetVoices` | CRITICAL | 12 | 4 | 9 | 5 |
| `provider.UnattendedZeroTTSVoices` | CRITICAL | 9 | 1 | 5 | 4 |
| `provider.ZeroTTSPresetVoices` | HIGH | 7 | 3 | 2 | 3 |
| `provider.VieNeuPresetVoices` | LOW | 3 | 2 | 0 | 2 |

The pre-spec CRITICAL classification of the **default voice catalog and verified-voice checks** is therefore unchanged and still accurate after the change — which is the expected result, because the change *repoints* those shared symbols rather than narrowing them. The affected execution flows named by the graph are the operator-facing ones the spec predicted: `handleAssignVoices`, `handleReassignVoices`, `handleInspectorReassignVoice`, `handleRunDubSynthesize`, `synthesizeSegmentsPass`.

`gitnexus detect-changes --scope compare --base-ref a240053` (the commit before #92 landed): **29 files, 232 symbols, 53 affected processes, risk level: critical** — consistent with the pre-spec prediction that this is not a simple provider-ID replacement.

### Graph claims verified against source

- `UnattendedZeroTTSVoices` direct callers = 1 → confirmed: `internal/provider/tts.go:99` (`DefaultPresetVoices`). No other production caller.
- `IsVerifiedTTSVoice` direct callers = 4 → confirmed in source at `internal/provider/worker_adapters.go:1156` and `internal/service/dubbing.go:171,189,357,372` (plus `:1770` for the CosyVoice fallback target).
- `ResolveTTSVoiceEntrypoint` direct callers = 1 → confirmed: exactly one production call site, `internal/provider/worker_adapters.go:256`. **Not verified:** the graph attributes this symbol to the `defaultASRInvoke` / `defaultAlignerInvoke` execution flows; no source path from either function to `ResolveTTSVoiceEntrypoint` exists (the only direct caller is the TTS snapshot-binding branch). Treat that process attribution as graph over-reach, not as evidence about ASR/aligner behaviour.

> Note: the analyzer emitted caps/truncation warnings of its own (588 of 788 entry-point candidates unranked, 3,192 callees skipped at the branching cap, and a `Function.function_fts` build failure on the first run). The graph and embeddings were rebuilt successfully on `--force`; the transient full-text-index failure is recorded here because an *absent* flow is not evidence that a path does not exist.

---

## 5. Documentation / provenance reconciliation

| Artefact | Finding |
|---|---|
| `docs/architecture/phase1-architecture.md` §6.3 | Names the ZeroTTS unattended rotation (`quangminh`, then `maichi`) as the VI AI-default; EN keeps Kokoro. Correct. |
| `docs/architecture/phase1-architecture.md` §6.4 | All four claims present and correct: **VI default** ZeroTTS (CPU-only, offline, fixed rate → non-1.0 speed fails closed, overrun remediation is rewrite/regroup/review); **VI compatibility lane** VieNeu preserved and never rewritten in place; **duration-controlled fallback** CosyVoice3 with whole-speaker escalation to a superseding speaker-scoped `VoiceAssignment` and `REVIEW_REQUIRED` instead of a second hop; **EN baseline** Kokoro/Chatterbox unchanged. |
| `docs/architecture/phase1-architecture.md` §7 diagram | GPU lease annotated `VieNeu / CosyVoice3 (ZeroTTS is CPU-only)`. Correct. |
| `docs/architecture/phase1-architecture.md` §12 item 1 | **Reconciled by this ticket.** The residual-risk item still listed ZeroTTS model pins as outstanding; the ZeroTTS pins are now bound in production and verified by the real StageWorker smoke (§3 row 1). Amended in place. |
| `CONTEXT.md`, `PRODUCT.md`, `README.md`, `CODING_STANDARDS.md`, `docs/agents/*` | Contain no TTS-provider claim that competes with the architecture document — no edit needed. |
| `docs/research/issue62-…-2026-09-03.md` | Dated historical provider-compatibility note that names VieNeu v3 Turbo as the then-required VI lane. Deliberately **not** rewritten: it is a timestamped record of a superseded decision, and `docs/architecture/phase1-architecture.md` is the canonical authority. |
| Governance / runtime provenance | License obligations stay layered per the #18 rule; runtime identity (`source revision`, package version, adapter revision) is observed and persisted through `SnapshotService` (`internal/domain/snapshot.go:200-206`, `internal/provider/worker_adapters.go:1088-1155`) and asserted at the exact pins. This file is the repo-local verification/provenance record for the routing change. |

---

## 6. Prohibited-shape audit (all clear)

| Prohibition | Evidence |
|---|---|
| No third service-level integration seam | Test tree is `test/fixtures`, `test/seam1`, `test/seam2`, `test/steering` only. `test/steering` is a steering-document integrity test, not a service seam. The new work lives in `test/seam1/zerotts_default_voice_test.go`, `test/seam1/zerotts_escalation_test.go`, `test/seam2/zerotts_seam2_test.go`. |
| No voice-cloning claim | `provider.ZeroTTSPresetVoices()` attaches no `Timbre`/name claim of cloning; `provider.IsVerifiedTTSVoice` treats ZeroTTS as preset-voice only. The only `natural_clone` timbre in the catalog belongs to the pre-existing CosyVoice3 EN profile. The adapter loads packaged voice latents only and has no voice-encoder path. |
| No sentence-level engine hopping | `TestSeam1_TTS_NoSentenceBySentenceEngineHopping` and `TestSeam1_ZeroTTS_UnresolvedOverrunEscalatesWholeSpeakerToCosyVoice3` PASS; `docs/architecture` §6.4 states the invariant. |
| No implicit model download | `cmd/stageworker/adapters/tts_engine.py` raises `WORKER_SNAPSHOT_PATH_REQUIRED … Hub download is prohibited` when no verified snapshot path is supplied, and the real smoke runs with offline env vars and dead proxies set. |
| No in-place active-runtime patching | The runtime pack is a separate versioned venv (`venvs/tts-zerotts-0.1.2-9d85578`), resolved by path, and its exact dependency set is a tracked repo artefact — `cmd/stageworker/adapters/requirements-zerotts-0.1.2.txt`, which pins `zerotts @ git+https://github.com/zeroweight-ai/ZeroTTS.git@9d85578…` on `onnxruntime` with no torch, and whose header states **"Rebuild into a fresh venv; do not patch an installed worker environment in place."** The adapter contains no provisioning step (the only `pip install` strings are operator-facing error messages). |

---

## 7. Independent two-axis review of the #92–#94 change set

Two read-only reviewers were run in parallel over `git diff a240053...31c7a20` (the exact revision verified above), one on the Standards axis and one on the Spec axis.

### Standards axis — no hard violations

The reviewer found **no breach of a documented repo standard**: `DubSegmentsSchemaVersion` is bumped 1→2 with the cache-identity rationale (`internal/domain/tts.go:46-50`, CODING_STANDARDS.md §4); error paths wrap with `%w` and use sentinel errors (§10); no third test seam (§3/§12); no machine-local path or credential committed (§2). All remaining findings are baseline smells, i.e. judgement calls:

1. **Duplicated Code (strongest).** The canonical ZeroTTS required-asset list is written out five times: `internal/domain/snapshot.go:415-424`, `cmd/stageworker/main.go:695-703`, `cmd/stageworker/adapters/tts_engine.py:313-320`, `test/seam2/zerotts_seam2_test.go:65-72`, `cmd/stageworker/adapters/test_tts_engine.py:42-49`. Cross-language duplication is defensible (the adapter must fail closed independently of Go), but the three Go copies could share one exported `domain.ZeroTTSRequiredAssets`.
2. **Duplicated Code.** `resolvePinnedZeroTTSRunner` (`cmd/stageworker/main.go:1254-1296`) reimplements the runner discovery of `resolveTTSRunner` (`:1221-1246`); its only delta is ignoring `DOUYINIE_TTS_BIN`/PATH.
3. **Repeated Switches.** `invokeCommand` adds seven `strings.Contains(stderrStr, "TTS_*")` branches (`cmd/stageworker/main.go:943-955`) plus two `strings.Contains(stageTarget, "zerotts")` chains (`:969`, `:988`) — consistent with the pre-existing substring convention, but the cascade keeps widening.
4. **Feature Envy / Primitive Obsession (minor).** `ttsFitCapabilities` (`internal/service/dubbing.go:2327-2345`) scans capability feature strings; `FeatureFixedRateVoice` is a bare string const (`internal/provider/tts.go:37`). Matches the existing feature-string design.
5. **Hidden side effect (minor).** `WorkerTTSProvider.SynthesizeSpeech` (`internal/provider/worker_adapters.go:1155-1164`) can persist a `RuntimeIdentity` through `snapshotSvc.SetRuntimeIdentity` from inside a synthesize call. Once per binding.

### Spec axis — no gaps, no scope creep

The reviewer found **no spec requirement missing or partial and no unrequested behaviour**, and independently confirmed the load-bearing constraints in source: VI default rotation (`internal/provider/tts.go:85-88`); VieNeu preserved and routable (`internal/provider/worker_adapters.go:1069`); fixed-rate lane tagged `FeatureFixedRateVoice` with the non-1.0 speed rejection (`worker_adapters.go:1159`) and the matching fit-controller guard (`internal/service/fit_controller.go:156-160`); CPU-only (`worker_adapters.go:630`); no implicit download (`tts_engine.py:302`, `WORKER_SNAPSHOT_PATH_REQUIRED`); whole-speaker superseding assignment without mutating history (`internal/service/dubbing.go:1850-1860`); actual-duration truth from `media.ParseWAVHeader` (`worker_adapters.go:1203`); post-escalation review correction reporting the real assignment hash (`internal/service/review.go:663-665`); cache-identity bump preventing silent suppression of the new lane evidence (`internal/domain/tts.go:42`).

### Disposition

All Standards findings are **judgement-call refactors carried forward, not fixed here**: this ticket is a verification gate, and changing production code now would invalidate the revision the gates in §2/§3 were run against. They are recorded for a follow-up cleanup ticket; none of them is a correctness or safety defect. No Spec findings to disposition.

---

## 8. Environment boundary (what this run could not prove)

- Live **VieNeu** and live **CosyVoice3** synthesis were not exercised: both are gated behind `DOUYINIE_TTS_PYTHON_BIN` and were reported SKIP by the harness. VieNeu's "remains routable/reproducible for historical assignments" guarantee is therefore proven at the **seam** level (identity preservation, old-provider invocation, zero new-lane invocations, unchanged stored CAS) rather than by a live VieNeu synthesis.
- Real separator model smokes were skipped (`AUDIO_SEPARATOR_MODEL_DIR` unset); unrelated to this change.
- `gitnexus analyze --force` rewrote the machine-managed `<!-- gitnexus:start -->` block inside `AGENTS.md`: it replaced the repo-relative runner path with an absolute machine-specific runner path, relinked the skill table to `.claude/skills/…`, and injected hard-coded index statistics (`8134 symbols, 41574 relationships, 537 execution flows`). That last part would have failed the repository's own steering gate — `TestSteeringIntegrity_NoDynamicStats` (`test/steering/steering_integrity_test.go:145`) rejects `\b\d[\d,]*\s+(symbols?|relationships?|execution flows?|…)\b` in `AGENTS.md`. The side effect was reverted; `AGENTS.md` is byte-identical to `HEAD` and `go test ./test/steering/` passes 5/5. **Re-run `git checkout -- AGENTS.md` after any future `gitnexus analyze`.**

## 9. Evidence logs

Retained outside the repository in the operator scratch directory:

- `full-95.txt` — `go test ./...` (19 packages ok / 0 FAIL)
- `95-seam1-v.txt` — seam1 verbose (224 PASS / 2 SKIP / 0 FAIL)
- `95-seam2-v.txt` — seam2 verbose (63 PASS / 4 SKIP / 0 FAIL)
- `95-seam2-zerotts.txt` — focused ZeroTTS seam2 gate (5 PASS)
- `95-detect-changes.json` — GitNexus change-set analysis
