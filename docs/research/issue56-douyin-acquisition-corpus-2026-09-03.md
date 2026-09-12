# Issue #56 — 100-URL Douyin Acquisition Corpus Validity & Drift Protocol

Date: 2026-09-03
Status: RESOLVED DECISION (research/design only; implementation-ready)
Parent: https://github.com/monet88/douyinie/issues/53
Issue: https://github.com/monet88/douyinie/issues/56
Authoritative references:
- Wayfinder Issue #1: https://github.com/monet88/douyinie/issues/1
- Issue #9 (Acceptance metrics & benchmark corpus): https://github.com/monet88/douyinie/issues/9
- Issue #17 (Source acquisition policy & governance): https://github.com/monet88/douyinie/issues/17
- Issue #18 (Phase 1 implementation spec): https://github.com/monet88/douyinie/issues/18
- Issue #53 (Phase 1.1 release hardening map): https://github.com/monet88/douyinie/issues/53
- Issue #54 (Release benchmark harness audit): https://github.com/monet88/douyinie/issues/54
- Issue #61 (Model snapshot & license enforcement): https://github.com/monet88/douyinie/issues/61
- Issue #62 (Production adapter compatibility & RC pinning): https://github.com/monet88/douyinie/issues/62
- Canonical architecture: `docs/architecture/phase1-architecture.md`
- Codebase truth: `internal/domain/acquisition.go`, `internal/provider/acquisition_cli.go`, `internal/service/acquisition.go`, `internal/service/ingest.go`, `internal/media/probe.go`, `test/seam1/acquisition_test.go`

---

## 1. Executive Summary & Problem Definition

The Phase 1.1 release hardening contract (Issue #9, refined by Issues #53 and #54) mandates an acquisition benchmark:
> **$\ge 98\%$ acquisition success** across a **100 valid public Douyin URL acquisition corpus** after configured retries and fallbacks.

The fundamental tension in operating this benchmark is distinguishing **platform/system acquisition reliability** from **extrinsic web rot**:
1. **Source churn**: public Douyin creators delete videos, set accounts to private, or geoblock content. If a video is deleted 48 hours after corpus selection, failing the benchmark because the video no longer exists measures upstream content attrition, not Douyinie pipeline health.
2. **Upstream page/anti-bot drift**: ByteDance continuously alters edge web protection (anti-bot WAF challenges, short-link redirect behaviors, `msToken` / `a_bogus` / `x-bogus` signature scripts, and HTML shell rendering).
3. **False successes & corrupted streams**: an adapter returning a 200 OK HTML challenge page, a truncated MP4 missing the `moov` atom, an encrypted CENC trial chunk, or an unrelated trending video must be caught and classified as failure.
4. **Phase 1 policy boundary (non-negotiable)**: Douyinie explicitly forbids CAPTCHA cracking, automated anti-scraping bypass, and unauthorized account creation/session automation. Furthermore, while local user-supplied files (`local_file`) remain an authorized product fallback for operators, **local file imports do not count as URL acquisition success for this 100-URL benchmark**.

This decision defines the exact **validity oracle**, **expected-source identity contract**, **refresh/rotation lifecycle**, **retry/fallback accounting**, **truncation/wrong-video detection gates**, and the **triage protocol distinguishing upstream drift from genuinely dead sources**.

---

## 2. Analysis of Shipped Acquisition Architecture & Reference Implementations

### 2.1 Current Implementation State in Douyinie
Douyinie's codebase implements a clean, policy-gated acquisition architecture centered around `Seam 1` (`RuntimeHost` API) and strict domain modeling:

1. **Domain Failure States (`internal/domain/acquisition.go`)**:
   - Hard blocks (fail-closed, never retried or fallen through):
     - `INVALID_URL`: malformed URL or unsupported locator type (e.g. non-Douyin domain). Returns HTTP 400.
     - `UNSUPPORTED_MEDIA_TYPE`: V1 only accepts standard video posts. Gallery/photo-notes (`/note/`) and live streams (`live.douyin.com`) are rejected at the `Probe` phase. Returns HTTP 422.
     - `CONTENT_UNAVAILABLE`: upstream returns HTTP 404, 410 Gone, 403 Forbidden on the canonical URL, or adapter classifies explicit "removed / private / not found". Returns HTTP 422.
   - Fallback-eligible soft failures (advance through the adapter ladder):
     - `ANTI_BOT_OR_EMPTY_RESPONSE`: empty HTTP 200 or redirect to verification interstitial without an `aweme_id`.
     - `AUTH_REQUIRED`: endpoint requires logged-in session cookies.
     - `SESSION_EXPIRED`: configured session cookie rejected or expired.
     - `CAPTCHA_REQUIRED`: explicit verification challenge detected.
     - `DOWNLOAD_FAILED`: transient network reset, timeout, or subprocess exit.
     - `INTEGRITY_FAILED`: media downloaded but failed container integrity (`ContainerValid == false`) or content fingerprinting.

2. **The Acquisition Ladder (`internal/service/acquisition.go` & `internal/provider/acquisition_cli.go`)**:
   - Order:
     1. `jiji_douyin` (quality 0.95, method `api`): drives `jiji262/douyin-downloader` CLI (`run.py -u <url> -p <dest> --cookie-file <temp>`).
     2. `f2_douyin` (quality 0.60, method `api`): fallback driving `f2` CLI (`f2 dy -M one -u <url> -p <dest> -k <temp>`).
     3. `douyin_browser_assist` (quality 0.50, method `browser`): last-resort driving Jiji with `--browser-fallback` using pre-authorized operator session cookies.
   - **Probe vs. Acquire separation**: `Probe` resolves short links (`v.douyin.com/...`) via HTTP redirect to canonical aweme IDs (`douyin:aweme:<id>`) without downloading media bytes or persisting durable assets.
   - **Per-candidate integrity gate**: each adapter downloads into an isolated candidate directory. The media file is ingested and probed immediately by `media.Prober` (running `ffprobe`). Corrupt media (`moov` atom missing, zero duration, stream missing) triggers `ErrQualityRejected`, recording a `quality_failed` attempt and advancing to the next ladder candidate. `INTEGRITY_FAILED` surfaces only when all candidates fail.
   - **CAS & SQLite Dedup**: successful media commits into CAS by SHA-256. If a canonical `aweme_id` or identical content SHA was previously acquired, reacquisition dedups directly to the existing `SourceAsset`.
   - **Safe Session Credentials**: raw session secrets (cookies, `msToken`) are passed transiently via temporary files with strict cleanup and zero persistent logging; only safe `CredentialRef` IDs are stored in `AcquisitionProvenance`.

### 2.2 Ground Truth from Ingestion Reference Mining (`.ref/`)
Empirical smoke tests (`docs/research/douyin-ingestion-reference-smoke-test.md`) and code inspection of `.ref/douyin-downloader` and `.ref/f2` reveal critical live Douyin behaviors:

1. **Unauthenticated Public Access is Heavily Rate-Limited & WAF-Gated**:
   - Douyin Web edge WAF frequently responds with empty HTTP 200 bodies, status -1, or redirects to web security check pages (`/security-check`) when accessed without valid browser cookies.
   - The primary API endpoint `/aweme/v1/web/aweme/detail/` requires valid Web cookies (`ttwid`, `odin_tt`, `passport_csrf_token`, `sid_guard`) and query parameters (`aid=1128` or `6383`, `a_bogus` / `x-bogus`).
   - `jiji262/douyin-downloader` uses `tools.cookie_fetcher` (Playwright Chromium session capture) to obtain an operator-authorized cookie bundle. Douyinie's live smoke evidence proves **1/1 authenticated sample succeeded** after capture; it does not establish corpus-wide reliability, which is exactly what the 100-URL benchmark must measure.
2. **URL Shapes & Normalization**:
   - Share short-links: `https://v.douyin.com/<token>/` (must follow 1 redirect hop to extract `aweme_id`).
   - Canonical web video URLs: `https://www.douyin.com/video/<aweme_id>` (numeric, 15–20 digits).
   - Modal/share query URLs: `https://www.douyin.com/?modal_id=<aweme_id>` or `...&aweme_id=<aweme_id>`.
   - Notes/galleries: `https://www.douyin.com/note/<aweme_id>` (explicitly rejected by V1 media policy).
3. **Upstream Asset Traps**:
   - Paid/preview content: paid videos have a plaintext `play_addr` containing a 10–30s preview, while `download_addr` references a CENC-encrypted full-length track. Downloading from `download_addr` yields unplayable DRM ciphertext.
   - Watermarked vs. Clean: `video.bit_rate` entries contain clean H.264/H.265 streams with `watermark=0` when requested with proper API signatures.

---

## 3. The Validity Oracle Contract

To ensure the $\ge 98\%$ benchmark evaluates the code rather than external link death, every URL entering the benchmark run must be verified against an unambiguous **Validity Oracle** at $T_0$ (benchmark execution start).

### 3.1 Definition of a "Valid Public Douyin URL"
A URL is defined as a **Valid Public Douyin URL** if and only if it satisfies all four criteria:
1. **Syntactically Valid Locator**: belongs to the Douyin domain space (`v.douyin.com`, `www.douyin.com`, `douyin.com`) and resolves to a stable 15–20 digit numeric `aweme_id`.
2. **Supported Media Type (V1 Policy)**: points to a standard public video post (`MediaType == "video"`). Gallery/slideshow posts (`/note/`) and live streams (`live.douyin.com`) are invalid for this corpus.
3. **Publicly Accessible & Unrestricted**:
   - The video is not deleted, hidden, or set to private (`status != 404`, `status != 410`, no creator-private restriction).
   - The video is not paid / CENC-encrypted media.
   - The video does not require specific account-following or age-gating permissions.
4. **Verifiable Upstream Media Asset**: upstream servers currently serve a complete, uncorrupted video stream corresponding to that `aweme_id`.

### 3.2 Out-of-Band Validity Oracle
The Validity Oracle is **benchmark precondition tooling, not a third architectural testing seam**. It must not rely on the production acquisition ladder as its sole source of truth, otherwise adapter breakage could incorrectly classify a valid URL as dead and remove it from the denominator. The oracle runs immediately before the benchmark through an out-of-band validation path:

```
                  ┌──────────────────────────────────────────────┐
                  │          Benchmark Manifest (100 URLs)       │
                  └──────────────────────┬───────────────────────┘
                                         │
                                  T_0 Oracle Gate
                                         │
                  ┌──────────────────────▼───────────────────────┐
                  │         Out-of-Band Oracle Validator         │
                  │   - HTTP HEAD/GET redirect resolution        │
                  │   - Browser/secondary reference evidence  │
                  │   - Public Video State Verification          │
                  └───────┬──────────────────────────────┬───────┘
                          │                              │
                    Source Alive?                  Source Dead?
                          │ (YES)                        │ (NO)
            ┌─────────────▼────────────┐   ┌─────────────▼────────────┐
            │ Confirmed Valid for Run  │   │ Corpus Rotation Trigger: │
            │ Handed to Seam 1 Suite   │   │ Auto-replace from Reserve│
            └──────────────────────────┘   └──────────────────────────┘
```

#### Oracle Verification Procedure:
1. **Redirect & Canonicalization**:
   - Issue an HTTP `GET` with `CheckRedirect` capping at 1 hop.
   - Extract canonical `aweme_id` via regex `awemeIDRe`.
   - If HTTP status is 404, 410, or redirected to an account-login/account-private interstitial, mark `ORACLE_CONTENT_UNAVAILABLE`.
2. **Metadata & Payload Probe**:
   - Use operator-authorized browser evidence or secondary tooling that does **not** call the same production adapter code path being scored. F2 may be corroborating evidence, but because `f2_douyin` is itself a scored fallback provider it cannot be the sole oracle.
   - Verify `status_code == 0`, `aweme_detail` is non-null.
   - Check `aweme_detail.is_copy` / `aweme_detail.is_prohibited` / `aweme_detail.status.is_delete`. If any indicate removed content, mark `ORACLE_CONTENT_UNAVAILABLE`.
   - Check `aweme_detail.images` / `aweme_detail.aweme_type`. If it is a note/gallery (type 68/0x44) or live replay, mark `ORACLE_UNSUPPORTED_TYPE`.
3. **Stream Availability Check**:
   - Prefer a bounded range `GET` on the candidate `play_addr` CDN URL; `HEAD` may be used only when that edge supports it reliably.
   - If CDN returns 403, 404, or 410, mark `ORACLE_STREAM_UNAVAILABLE`.
4. **Outcome**:
   - Only URLs receiving an unambiguous `ORACLE_PASS` enter the active 100-URL benchmark pool.

---

## 4. Expected-Source Identity Metadata & Anti-Drift Verification

A major risk in web scrapers is **silent wrong-video acquisition**: Douyin redirecting to a trending feed or home page, causing the downloader to acquire a random video and report success.

Every corpus entry must have an immutable **Expected-Source Identity Contract** recorded in the benchmark manifest:

### 4.1 Corpus Manifest Schema (`corpus_manifest.json`)
Each entry in the 100-URL corpus is defined by:
```json
{
  "corpus_id": "DY-ACQ-042",
  "original_url": "https://v.douyin.com/t6U4nCrLIYc/",
  "expected_aweme_id": "7659396126545619322",
  "expected_canonical_url": "https://www.douyin.com/video/7659396126545619322",
  "expected_author_sec_uid": "MS4wLjABAAAAm...",
  "expected_author_nickname": "九九衣橱",
  "expected_title_prefix": "10款有袖实穿的通勤连衣裙分享",
  "expected_duration_ms": 445100,
  "duration_tolerance_ms": "<pinned integer established during curation>",
  "expected_has_audio": true,
  "baseline_sha256": "ca961831b0de6b74def3ba61251fe53864160489e3a29bfaf2008ccbe46f6f90",
  "oracle_checked_at": "2026-09-03T00:00:00Z",
  "oracle_method": "authorized_browser_plus_secondary_probe",
  "oracle_evidence_sha256": "<sha256 of sanitized oracle evidence>",
  "curation_date": "2026-08-19T00:00:00Z",
  "category": "fashion_single_speaker"
}
```

The example above is a manifest **shape**, not a literal ready-to-run JSON record. `duration_tolerance_ms` must be materialized as an integer before the corpus is frozen. Author/title metadata may be retained as audit context, but acquisition scoring must not depend on mutable display text.

### 4.2 Acquisition Acceptance Identity Gates
For an acquisition to be scored as a **PASS** in the $\ge 98\%$ gate, the produced `SourceAsset` and `PreflightReport` must satisfy all verification rules:
1. **Strict Aweme ID Match**:
   - The acquired `SourceDescriptor.SourceID` must match `douyin:aweme:<expected_aweme_id>`.
   - Matching a different video ID is a hard **WRONG_VIDEO_ERROR** (`FAIL`).
2. **Duration Boundary Check**:
   - `|PreflightReport.DurationMs - ExpectedDurationMs| <= duration_tolerance_ms`.
   - `duration_tolerance_ms` is pinned **per corpus entry before the scored run** from curation/oracle evidence. Issue #56 intentionally does not invent one global millisecond constant; changing the tolerance after a failure is forbidden.
   - A discrepancy beyond the pinned tolerance is **DURATION_MISMATCH_ERROR** (`FAIL`) because it can indicate truncation, preview substitution, or wrong media.
3. **Container & Stream Integrity Check**:
   - `PreflightReport.ContainerValid == true`.
   - Video stream present (`Width > 0`, `Height > 0`, `FrameRate > 0`, valid video codec e.g. `h264`/`hevc`).
   - If the oracle snapshot says the source has audio, an audio stream must still be present. A legitimately silent public video is not invalid merely because it lacks an audio track.
   - Container has no unreadable moov atom or trailing byte truncation (`PreflightReport.Errors` is empty).
4. **Byte Identity / Baseline Fingerprint**:
   - If `baseline_sha256` is recorded, a bit-identical match confirms 100% fidelity.
   - If SHA-256 differs (due to Douyin re-encoding or CDN bitrate shifting), the video is still accepted if and only if:
     - `aweme_id` matches exactly;
     - Duration matches within tolerance;
     - Container is fully valid.

---

## 5. Distinguishing Upstream Page Drift vs. Dead Source vs. Pipeline Failure

A key requirement of Issue #56 is establishing a deterministic triage protocol to classify failures during benchmark execution:

| Failure Observation | Technical Classification | Attribution | Benchmark Accounting |
| :--- | :--- | :--- | :--- |
| **HTTP 404 / 410 / Gone** on canonical video URL | `CONTENT_UNAVAILABLE` | Dead source (creator deleted/privated) | Excluded from denominator via Oracle; rotated from Reserve pool |
| **Account Private / Deleted** detected by parser | `CONTENT_UNAVAILABLE` | Dead source | Excluded from denominator via Oracle; rotated from Reserve pool |
| **Short-link redirects to valid `aweme_id`, but empty HTTP 200 on API** | `ANTI_BOT_OR_EMPTY_RESPONSE` | Upstream anti-bot drift / session issue | **Pipeline Acquisition Failure** (counts against $\ge 98\%$ gate) |
| **Douyin signature rejected (`x-bogus`/`a_bogus` failure)** | `DOWNLOAD_FAILED` | Upstream signature drift | **Pipeline Acquisition Failure** (counts against $\ge 98\%$ gate) |
| **HTTP 403 / 429 WAF Edge Challenge** | `ANTI_BOT_OR_EMPTY_RESPONSE` / `CAPTCHA_REQUIRED` | Rate-limiting / anti-bot challenge | **Pipeline Acquisition Failure** (counts against $\ge 98\%$ gate) |
| **Downloaded file is truncated (< 100 KB, missing moov)** | `INTEGRITY_FAILED` | Downloader stream interruption / CDN cutoff | **Pipeline Acquisition Failure** (counts against $\ge 98\%$ gate) |
| **Downloaded media duration exceeds the entry's pinned tolerance** | `DURATION_MISMATCH` | Downloader truncation / wrong variant | **Pipeline Acquisition Failure** (counts against $\ge 98\%$ gate) |
| **Different video acquired (`aweme_id` mismatch)** | `WRONG_VIDEO` | Downloader redirect hijack | **Pipeline Acquisition Failure** (counts against $\ge 98\%$ gate) |
| **Credential invalid before the scored run starts** | `AUTH_REQUIRED` / `SESSION_EXPIRED` | Test harness setup defect | **Harness Invalidation** (do not start scoring) |
| **Credential/session expires after a valid pre-run check** | `SESSION_EXPIRED` | Runtime/session reliability | **Pipeline Acquisition Failure**, unless independent evidence proves explicit external revocation |

**Current implementation mismatch to carry into the later implementation spec:** `cliAcquisitionProvider.Probe` currently maps any HTTP 403 during redirect resolution to `CONTENT_UNAVAILABLE`. Under this benchmark contract, **403 alone is not sufficient evidence that the source is dead** because it may be a WAF/session challenge. A 403 may exclude a URL from the denominator only when the out-of-band oracle independently proves deleted/private/unavailable content; otherwise it remains a scored acquisition failure.

### 5.1 The Triage Protocol
When an acquisition run produces a failure:
1. **Step 1: Check HTTP & Canonical Response**.
   - Did the source return 404/410, or does Douyin explicitly report `status_code == 200` with `filter_reason == "item_deleted"`?
   - If **YES**: Source is dead. Trigger **Corpus Rotation Protocol** (§6).
2. **Step 2: Check Oracle Status at T_0**.
   - If the URL was verified alive at $T_0$ but failed during the run:
     - Was it an anti-bot challenge, empty 200, or signature error?
     - If **YES**: This is **Upstream Page Drift / Adapter Fragility**. It is attributed to the Douyinie acquisition pipeline and counts as a **FAIL**. The adapter ladder must handle this via its configured fallbacks (Jiji $\to$ F2 $\to$ browser-assist).
3. **Step 3: Check Media Payload**.
   - If bytes were produced but failed `media.Prober`:
     - This is **Pipeline Integrity Failure**. Attributed to the adapter.

---

## 6. Corpus Composition, Refresh, and Rotation Lifecycle

### 6.1 The 100-URL Corpus Structure
The scored corpus contains exactly **100 valid public Douyin URLs**. Maintain a separate **pre-curated reserve pool**, but Issue #56 does not make an arbitrary reserve count or content-category quota a release invariant.

The manifest must declare acquisition-relevant strata so rotation cannot silently make the corpus easier. At minimum preserve diversity across URL form (short-share vs canonical), creator/account identity, clip duration/size bands, and ordinary codec/container variants observed during curation. Exact quotas and reserve capacity are harness/corpus-maintenance parameters and must be frozen in the RC evidence before execution.

### 6.2 Rotation & Refresh Rules
Public Douyin URLs can be deleted, privatized, or otherwise become unavailable after curation, so corpus maintenance must be explicit and auditable.
1. **Pre-Benchmark Run Scrub (Oracle Gate)**:
   - Before executing a formal Release Candidate benchmark, the Oracle (§3) executes against the 100 active URLs.
   - Any URL verified as `CONTENT_UNAVAILABLE` is **retired** and replaced with an identical-category URL from the Reserve Pool.
   - The rotation is logged with an audit trail:
     `ROTATE: DY-ACQ-014 (765939...) -> DY-RES-003 (768841...) [Reason: CONTENT_UNAVAILABLE_404]`.
2. **Corpus Immutability During Benchmark Execution**:
   - Once the active pool passes the $T_0$ Oracle Gate, the 100-URL manifest is **frozen** for that benchmark run.
   - No rotation is permitted during the run.
   - If independent revalidation proves a frozen entry genuinely became deleted/private after $T_0$, the scored acquisition run is marked **CORPUS_INVALIDATED** and must be restarted after an audited rotation. Do not silently shrink the denominator or substitute a new URL mid-run.
3. **Periodic Maintenance**:
   - Keep enough reserve entries to preserve every declared acquisition stratum when rotation is needed; the exact reserve size is recorded as corpus metadata, not hard-coded by this decision.

---

## 7. Retry, Fallback, and Accounting Rules

### 7.1 Retry Policy Boundaries (Inside Seam 1)
All retries and adapter transitions must be governed strictly by the Router and `AcquisitionService` (`internal/service/acquisition.go`), adhering to the two approved testing seams:
1. **Fail-Closed on Policy/Governance Violations**:
   - If an adapter returns `CONTENT_UNAVAILABLE`, `INVALID_URL`, or `UNSUPPORTED_MEDIA_TYPE`, the Router halts immediately. **Zero retries, zero fallback**.
2. **Score the Frozen Production Retry Configuration, Not Benchmark-Only Retries**:
   - At `main@7bbac44969442444934338867db4abcb29a4d24b`, `AcquisitionService` calls `ExecuteRoutedWithRetry(..., 1, ...)`, so the Douyinie Router invokes each ladder candidate once.
   - Individual external CLIs may retry internally. Current upstream Jiji documents configurable retries with exponential backoff; therefore the RC evidence must pin each adapter's effective internal retry configuration/version and retain enough attempt/log evidence to distinguish Router fallback from adapter-internal retry.
   - The benchmark harness must not add hidden retries that production does not receive.
3. **Ladder Transition (Fallback)**:
   - On a fallback-eligible probe/acquire failure, the Router advances from `jiji_douyin` to `f2_douyin` and then `douyin_browser_assist` according to the shipped policy/routing contract.
   - Each shipped CLI adapter invocation is currently bounded by a 10-minute `context.WithTimeout`; there is no verified `_VIDEO_ITEM_DEADLINE_S` 15-minute corpus contract in the repo, so Issue #56 does not invent one.
   - Interactive CAPTCHA solving during a scored run is not a success path. Browser-assist may use an already authorized operator session, but a scored run requiring fresh human CAPTCHA completion is a `FAIL`/`CAPTCHA_REQUIRED` outcome rather than hidden manual recovery.

### 7.2 Metric Calculation & Benchmark Gate Formulation
The Acquisition Gate metric is calculated as:

$$\text{Acquisition Success Rate} = \frac{N_{\text{success}}}{N_{\text{total\_active}}} \times 100\%$$

Where:
- $N_{\text{total\_active}} = 100$ (the frozen valid active pool at $T_0$).
- $N_{\text{success}}$ is the count of URLs where:
  1. Status is `SUCCEEDED`;
  2. `SourceAsset` is committed to CAS with valid SHA-256;
  3. `PreflightReport.ContainerValid == true`;
  4. `ExpectedAwemeID == ProbedAwemeID`;
  5. `|PreflightDuration - ExpectedDuration| <= duration_tolerance_ms` using the value frozen in that entry's manifest;
  6. Acquisition was performed via authorized URL ladder (local file bypass disallowed).

#### Release Gate:
$$\text{Acquisition Success Rate} \ge 98.0\%$$
*(At most 2 failures permitted across the 100 active URLs).*

---

## 8. Preserving the Phase 1 Policy Boundary

To remain 100% compliant with the Phase 1 charter and legal/architectural guardrails:
1. **Strictly No Anti-Bot / CAPTCHA Bypass**:
   - Douyinie contains no OCR-based slider solvers, third-party captcha bypass APIs, or browser fingerprint spoofing farms.
   - If an automated request triggers an interactive safety challenge that cannot be satisfied by authorized cookies, it must report `CAPTCHA_REQUIRED` and fail closed or advance to authorized browser-assist.
2. **Authorized Session Management**:
   - Automated acquisition requires operator authorization (`PolicyRequiresAuthorization`).
   - Session cookies are provisioned exclusively by the operator via `CredentialRef` (e.g. captured via interactive login using `tools.cookie_fetcher`).
   - Raw secrets are never logged, persisted in SQLite, or exported in API responses.
3. **Disallow Local Media Bypass for Benchmark Scoring**:
   - The production pipeline supports ingesting user-supplied files via `/api/v1/sources/local` as an authorized fallback when online extraction fails.
   - **However, for the 100-URL Acquisition Benchmark, local file ingestion is strictly forbidden**. All 100 runs must be executed via `POST /api/v1/sources/acquire` with `locator.type = "douyin_url"`. Any local file fallback used for a benchmark entry is scored as a **FAIL**.

---

## 9. Recommended Contract Summary

1. **Active Corpus**: exactly 100 valid public Douyin URLs plus a pre-curated reserve pool that preserves declared acquisition-relevant strata; reserve size/quotas are frozen evidence, not a universal constant.
2. **Oracle Gate**: $T_0$ out-of-band probe confirming source existence, supported media type, stable `aweme_id`, and upstream media availability before benchmark execution. The oracle is benchmark precondition tooling, not a third architectural testing seam.
3. **Identity & Quality Assertion**: Every successful acquisition must match the expected `aweme_id`, pass `PreflightReport` container integrity, and stay within the entry's pre-frozen duration tolerance. Byte-identical SHA is supporting evidence, not required across valid upstream transcodes.
4. **Ladder Execution**: Managed via Seam 1 `AcquisitionService` using `jiji_douyin` $\to$ `f2_douyin` $\to$ `douyin_browser_assist`, with the exact RC Router + adapter-internal retry configuration frozen in evidence. No benchmark-only retry budget is added.
5. **Threshold**: $\ge 98\%$ success rate ($98/100$), zero anti-bot bypass mechanisms, zero local-file substitution.

---

## 10. Unresolved Evidence Gaps

The following empirical gaps remain before full automated benchmark execution:
1. **Live Douyin Session Longevity**: While Jiji cookie-assisted acquisition is verified on individual videos, the lifespan of captured web session cookies (`ttwid`, `odin_tt`) under sequential 100-request load without triggering intermediate WAF session expiration needs empirical measurement during dry runs.
2. **Out-of-Band Oracle Tooling**: Issue #59 must define how the harness stores sanitized oracle evidence without introducing a third architectural seam. A scored production provider such as F2 may corroborate, but cannot be the sole validity oracle.
3. **Per-entry Duration Tolerance Calibration**: The first dry-run/corpus-curation pass must pin a defensible tolerance for every URL before scoring; Issue #56 deliberately rejects an evidence-free global `3s` constant.
4. **Actual Corpus Manifest Assembly**: The concrete 100 active URLs plus reserve pool, strata, oracle snapshots, and frozen adapter/runtime configuration still need to be curated before the Phase 1.1 release gate executes.

---

## 11. Verified Evidence Used

- Resolved benchmark contract: GitHub Issue #9 resolution (`100 valid public Douyin URLs`, `>=98%`, wrong/truncated/false-success = failure).
- Current shipped acquisition implementation: `internal/service/acquisition.go`, `internal/provider/router.go`, `internal/provider/acquisition_cli.go`, `internal/domain/acquisition.go`, `internal/media/probe.go` at `main@7bbac44969442444934338867db4abcb29a4d24b`.
- Repo-local live Douyin evidence: `docs/research/douyin-ingestion-reference-smoke-test.md` (unauthenticated failure on the tested public URL; later authenticated Jiji **1/1** success; no claim of corpus-wide reliability).
- Current upstream Jiji README: https://github.com/jiji262/douyin-downloader/blob/main/README.md (short-link/video support, cookie capture, browser fallback, integrity checks, and configurable retry/backoff behavior).
