# Live RuntimeHost E2E on real media

How to stand up the production RuntimeHost + StageWorker stack on this workstation, drive the Operator UI
like a real operator, and read the run back. Use it before claiming any acceptance criterion that says
"on real media" / "production path".

Authoritative behaviour still lives in [phase1-architecture.md](../architecture/phase1-architecture.md) and
the resolved specs on the issue tracker; this file is the runbook, not a source of truth.

## 0. Fast path: re-run one video end to end

The whole loop, in order, when someone says "test a video again". Sections 1-9 are the reference; this is
the recipe. Budget ~25 min for a 27 s clip: ~6 min of stage work, the rest startup, provisioning, human
review and the browser checks.

```bash
# 1. Build (rebuild after any Go change; adapters are Python, nothing to build there)
cd F:/CodeBase/douyinie
go build -o F:/douyinie-rt-scratch/stageworker.exe ./cmd/stageworker
go build -o F:/douyinie-rt-scratch/runtimehost.exe ./cmd/runtimehost

# 2. Start the daemon through the supervisor (the .cmd applies the whole §2 matrix)
#    hub start name=douyinie-live application=cmd.exe args=["/c","F:\\douyinie-rt-scratch\\start-daemon.integration.cmd"]
#    ready: log matches "API daemon listening" AND port 18099 answers
curl -s http://127.0.0.1:18099/api/v1/providers | jq '.providers|length'

# 3. Provision EVERY restart (in-memory snapshot bindings, §3)
python F:/douyinie-rt-scratch/provision/provision.py      # expect "14/14 lanes verified"

# 4. Drive the run — UI path in §4 (upload → Tạo job & đưa vào queue), or headless:
curl -s -X POST -F "file=@<clip>.mp4" -F "declared_by=<name>" -F "terms_accepted=true" \
     http://127.0.0.1:18099/api/v1/assets/upload                       # -> asset_id
curl -s -X POST -H "Content-Type: application/json" \
     -d '{"source_asset_id":"<asset>","target_language":"vi"}' \
     http://127.0.0.1:18099/api/v1/jobs                                # -> job_id
curl -s -X POST -H "Content-Type: application/json" -d '{}' \
     http://127.0.0.1:18099/api/v1/jobs/<job>/runs                     # -> run_id (queued)
curl -s http://127.0.0.1:18099/api/v1/queue | jq -c '.queue[]|{run_id,position,status}'
curl -s http://127.0.0.1:18099/api/v1/runs/<run>/stages | jq -r '.stages[]|"\(.stage) \(.status)"'
curl -s -X POST -H "Content-Type: application/json" -d '{}' \
     http://127.0.0.1:18099/api/v1/runs/<run>/resume                   # re-drain after an interrupt

# 5. Clear the exception queue (UI path in §4); headless form:
curl -s http://127.0.0.1:18099/api/v1/runs/<run>/review-items | jq -r '.review_items[]|"\(.id) \(.reason)"'
curl -s -X POST -H "Content-Type: application/json" \
     -d '{"review_item_id":"<item_id>","asset_id":"<asset>","reason":"<why it is acceptable>","operator":"<name>"}' \
     http://127.0.0.1:18099/api/v1/runs/<run>/review/override

# 6. Freeze and render
curl -s -X POST -H "Content-Type: application/json" -d '{"run_id":"<run>","target_language":"vi"}' \
     http://127.0.0.1:18099/api/v1/assets/<asset>/render-plan
curl -s -X POST -H "Content-Type: application/json" -d '{"run_id":"<run>","target_language":"vi"}' \
     http://127.0.0.1:18099/api/v1/assets/<asset>/render/final
curl -s http://127.0.0.1:18099/api/v1/assets/<asset>/render/final \
  | jq -r '.final_render|"\(.overall_status) \(.output_cas_path)"'
```

Test media that has actually been through the loop (the uploaded bytes live in the CAS, so a re-upload can
use the CAS path directly):

| Clip | Dimensions / duration | Asset | Final render |
|---|---|---|---|
| `5.mp4` - phone-stand tips, burned-in captions and product labels | 1080x1440, 27.17 s | `41fb83c0-1e07-43f3-afaf-837cb8070ece` | `0f5cf282…` (PASS) |
| `4.mp4` - CapCut opacity tutorial, screen recording | 1080x1920, 33.11 s | `f6e0436f-e92b-4f24-9f86-3b572abc2541` | `26bbc77f…` (PASS) |
| the same clips as files | `F:/douyinie-rt-scratch/live-20260918/4-upload.mp4`, `…/data-head/cas/23/9a/239a21539a…` | | |

Measured stage cost for the 27 s clip (run `4f86657f`, stages with real work only):

| Stage | Seconds |
|---|---|
| `audio_role_plan` | 24 |
| `speech_understand` (ASR + align + diarize) | 241 |
| `translation` | 5 |
| `dub_synthesize` (ZeroTTS) | 170 |
| `visual_text_localize` (OCR is the slow part; a cached plan returns in ~0 s) | 64 |
| `render_preview` / `render_final` | 3 / 11 |

- **Accept review items through the RUN-scoped endpoint.** `/api/v1/review-items/{id}/override` resolves the
  item against the asset/language pending queue and rejects a run-scoped item with
  `not found in pending review queue for asset …`; `/api/v1/runs/{run}/review/override` takes the item id in the
  body, records the run binding, and clears the handoff gate.
- **`POST /runs/{id}/resume` can drop the connection** (`ConnectionResetError`) while it starts draining. That is
  not a failure: the daemon stays up and the run goes `running` (observed, run `3adede59`). Confirm with
  `GET /runs/{id}/stages` instead of retrying the resume.

Reading traps in that ledger:

- A **cache hit writes a `visual_text` row whose `started_at` is the cached plan's creation time**
  (`4f86657f` shows `visual_text` at 39057 s). Read the row's `completed_at`, not the delta, and do not
  treat the first line as the stage's cost.
- `translation`, `dub_script` and `audio_mix` legitimately log `0.0 s` rows: they are cache hits of an
  artifact the earlier attempt produced.
- `GET /api/v1/queue` lists **every** entry the data dir ever held, so stale `interrupted` / `completed` /
  `cancelled` rows sit at `position: 0` beside the live run. Read the `status` column: a run created through
  `POST /api/v1/jobs/{id}/runs` lands `queued` and the queue service picks it up on its own (observed: the new
  run jumped to `running` at `position: 1` within seconds while five old entries stayed at 0). `resume` is for
  a run that stopped, not for a run that has not started.
- Re-freezing does **not** re-encode: `render_plans` / `render_artifacts` are keyed by plan provenance, so
  delete their rows in the scratch data dir when you need a fresh encode to inspect.

## 1. Build and start

```bash
cd F:/CodeBase/douyinie
go build -o F:/douyinie-rt-scratch/stageworker.exe ./cmd/stageworker
go build -o F:/douyinie-rt-scratch/runtimehost.exe ./cmd/runtimehost
```

Start it through the process supervisor with the §2 environment applied (readiness = log match
`API daemon listening`, port 18099):

```
runtimehost.exe -port 18099 -data-dir F:\douyinie-rt-scratch\live-20260918\data-head
```

Health check: `curl -s http://127.0.0.1:18099/api/v1/providers | jq '.providers|length'`.

## 2. Runtime matrix (per-family interpreter + binaries)

The StageWorker resolves **one** `DOUYINIE_PYTHON_BIN` shared by asr / aligner, and a separate
per-family variable for every other family (diarizer included). Each family needs the venv that actually
carries its dependencies.

| Env var | Value on this workstation |
|---|---|
| `DOUYINIE_STAGEWORKER_BIN` | `F:\douyinie-rt-scratch\stageworker.exe` |
| `DOUYINIE_PYTHON_BIN` | `D:\douyinie-ref\phase1.1-runtime\venvs\asr\Scripts\python.exe` (asr + aligner) |
| `DOUYINIE_DIARIZER_PYTHON_BIN` | `…\venvs\diarizer\Scripts\python.exe` |
| `DOUYINIE_AUDIO_ROLE_PYTHON_BIN` | `…\venvs\audio_role\Scripts\python.exe` |
| `DOUYINIE_OCR_PYTHON_BIN` | `…\venvs\ocr\Scripts\python.exe` |
| `DOUYINIE_SEPARATOR_PYTHON_BIN` | `…\venvs\separator\Scripts\python.exe` |
| `DOUYINIE_TRANSLATION_PYTHON_BIN` | `…\venvs\translation\Scripts\python.exe` |
| `DOUYINIE_TTS_PYTHON_BIN` | `…\venvs\tts-zerotts-0.1.5-47e466d\Scripts\python.exe` |
| `DOUYINIE_GATEWAY_URL`, `DOUYINIE_GATEWAY_API_KEY` | operator-supplied authorized gateway. `DOUYINIE_GATEWAY_URL` must already carry the API prefix (`https://<host>/v1`): `GatewayTranslationProvider.resolveEndpointURL` appends `/chat/completions` verbatim, so a bare host yields HTTP 404 (recorded as `quality_failed` on both gateway lanes, surfacing as `all provider candidates failed for stage translation`) |
| `DOUYINIE_SERVICE_BASELINE_GEMINI`, `DOUYINIE_SERVICE_BASELINE_DEEPSEEK` | operator-supplied baseline ids |

Notes that were learned the hard way:

- **Every stem lane is normalized to the pipeline contract rate.** The UVR lane asks audio-separator for
  `sample_rate=16000`, but Demucs always writes its model's native 44.1 kHz stereo, and the audio-role
  analyzer rejects anything but 16 kHz mono — so `separate_audio_stems` passes both lanes through
  `enforce_contract_rate`, which resamples each stem with ffmpeg (file output, not a pipe: a streamed WAV
  cannot backfill its RIFF sizes) and replaces the reported rate/channels with what it measured. Without it a
  default-rate separator dead-ends `audio_role_plan`, the first stage of every real run, while the persisted
  stem metadata claims the contract rate. Caveat: an already-persisted stems artifact keeps the rate it was
  produced at — audio-role reuse serves it as-is and the analyzer still fails closed on it, so a lane fix only
  takes effect for stems separated after it.
- **The diarizer runs its own venv.** `venvs\asr` has no `modelscope`/`torchaudio` while
  `venvs\diarizer` has both, so set `DOUYINIE_DIARIZER_PYTHON_BIN` to the diarizer venv's interpreter
  (`resolveDiarizerPythonBinary`); asr and aligner keep resolving the shared `DOUYINIE_PYTHON_BIN`, and the
  two can differ in one session. The variable is fail-closed: a value that is not a usable interpreter
  aborts the stage with `DIARIZER_RUNTIME_MISSING` naming the variable and the path — it never falls back
  to another interpreter, which is what produced the opaque `DIARIZER_EXEC_FAILED`. No runner shim is
  needed any more, so drop every `DOUYINIE_{ASR,ALIGNER,DIARIZER}_BIN` override; a stale
  `DOUYINIE_DIARIZER_BIN` still wins over the new variable.
- `DOUYINIE_<FAMILY>_BIN` is a **binary** override: the worker invokes it with the JSON request on stdin and no
  adapter argument. Do not point it at a bare interpreter.
- Snapshot-required routes (separator, audio_role, tts) reject arbitrary binaries/adapters that cannot
  self-attest RC provenance — build from the repo, do not hand-roll those two.

## 3. Provisioning (once per data dir, and after **every** daemon restart)

14 model lanes must be verified + licensed + allowed before any stage runs. All three registries are HTTP;
a reference script lives at `F:/douyinie-rt-scratch/provision/provision.py`.

```bash
POST /api/v1/snapshots/verify        # per dependency + version + manifest sha
POST /api/v1/licenses               # per dependency: CODE_LICENSE / MODEL_LICENSE / DATA_LICENSE / SERVICE_TERMS
PUT  /api/v1/policies/{provider_id}  # {"status":"ALLOWED"}
```

**Snapshot bindings live in process memory, not in SQLite.** Licenses and policies persist in the DB, but
the verified-snapshot bindings are rebuilt only by `POST /api/v1/snapshots/verify`, so a restarted daemon
rejects every snapshot-required provider with `SNAPSHOT_UNVERIFIED` — which surfaces as
`no eligible provider found satisfying policy, capability, and health` on the first stage (typically ASR),
within milliseconds of the run starting. Re-run the provisioning script after each restart.

Verify with `GET /api/v1/policies`, `GET /api/v1/licenses`, `GET /api/v1/snapshots`. A missing license or a
non-ALLOWED policy surfaces the same way; the per-candidate truth is in SQLite:

```bash
sqlite3 <data-dir>/douyinie.db \
 "select stage, decision_reason, candidates_evaluated_json from selection_decisions order by created_at desc limit 1"
```

## 4. Drive the Operator UI as a real operator

Browser: the Orca embedded Chromium tab (`orca tab list --json`) pointed at `http://127.0.0.1:18099/`.
Use `orca snapshot --json` for refs and `orca click/fill/upload --element <ref>`; the accessibility refs
are the only stable handle (the DOM ids change with the view).

1. `01 Job mới` → `File local` segmented button → `#source-input` becomes `type=file`.
2. Attach the media with `orca upload --files <path> --element <ref>`; confirm the input actually holds the
   file (`document.getElementById('source-input').files[0].name`).
3. Fill `Tên operator`, tick the rights attestation, click `Tạo job & đưa vào queue→`.
4. The UI posts `/api/v1/assets/upload` → `/api/v1/jobs` → `/api/v1/jobs/{id}/runs` and selects the new run.

Trap: **switching views clears the file input.** If you navigate to another tab between attach and submit,
the submit is a silent no-op (no toast, no job). Re-attach immediately before clicking.

### Working the exception queue (03 Review Workspace)

1. Each pending exception is one button in the Inspector's `Exceptions` tab; clicking it loads the detail
   card (stage, region/segment, and the fix-type tabs `Accept` / `Sửa text` / `Đổi voice` / `Chỉnh region`).
2. `Accept` shows the audit form: fill `Lý do chấp nhận` (`#accept-reason`) and submit
   `Chấp nhận có audit`. The item leaves the queue and the decision is appended to the audit trail with the
   operator name from `Tên operator`.

Traps:

- **The a11y click does not submit this form.** `orca click --element <ref>` on `Chấp nhận có audit`
  reports success but no request is sent (the pending count does not move). Dispatch the DOM click instead:
  `orca eval --expression "(()=>{[...document.querySelectorAll('button')].find(b=>b.textContent.includes('Chấp nhận có audit')).click();return 'ok'})()"`.
  Buttons that call a handler directly (list items, nav, `Làm mới dữ liệu`) work fine with `orca click`.
- **The view does not auto-refresh after an override.** Pending counts stay stale until `Làm mới dữ liệu`
  (or a reload); confirm the real state with
  `GET /api/v1/runs/<run>/review-items?include_resolved=true`.
- A run that is `interrupted` now offers `Resume` in the console (the RuntimeHost accepts
  `POST /api/v1/runs/<id>/resume` for it and re-drains from the incomplete stage); `Pause`/`Cancel` stay
  disabled. Reload the page after changing `app.js` — the data-refresh button re-fetches state, not the script.

- **A blocked dub reads as `tts_overrun` blockers (`severity: blocker`), not as OCR warnings.** When the
  dub lane cannot fit a slot, the exception queue carries one item per affected segment with
  `measured_duration_ms` / `slot_duration_ms`. Two ways out, both operator-visible: shorten the wording
  (`Sửa text` → `POST /api/v1/runs/<run>/inspector/correct-text` with `new_target_text` +
  `spoken_text_override`) or give the run a lane that can compress (below).
- **Why the automatic fit lane may never engage.** The VI default lane (ZeroTTS) is fixed-rate: it rejects any
  speed other than 1.0, so overrun can only go through rewrite/regroup and then the whole-speaker escalation
  to the duration-controlled lane (`cosyvoice3_tts`, feature `measured_duration_speed_fit`). That lane is
  route-eligible only with a license manifest **and** a verified snapshot in the data dir; in the
  `live-20260918` data dir it has neither, so the router refuses it, no escalation happens, and the overrun
  stays `REVIEW`. Provision that lane or shorten the text - do not expect the escalation to fire on a box
  where its weights were never registered.

## 5. Read the run back

```bash
curl -s http://127.0.0.1:18099/api/v1/runs/<run>/stages | jq -r '.stages[] | "\(.stage) \(.status) \(.error_message//"")"'
curl -s http://127.0.0.1:18099/api/v1/jobs/<job>           | jq -c '.job|{status}'
```

Per-attempt provider truth (which model ran, latency, why a candidate was rejected) lives in SQLite, not in
the API:

```bash
sqlite3 <data-dir>/douyinie.db \
 "select stage, provider_id, model_name, status, count(*), round(avg(latency_ms))
  from provider_attempts where run_id='<run>' group by 1,2,3,4"
```

Artifact payloads are content-addressed under `<data-dir>/cas/<aa>/<bb>/<sha256>`; the index rows
(`translation_variants`, `dub_segments_variants`, `voice_assignments`, `transcript_artifacts`, …) carry the
hash. Probe media with `ffprobe` before believing any metadata column.

Terminal states: `completed` (final render done), `review_required` on the **job** + `interrupted` on the
**run** when the final-render handoff is blocked by pending review exceptions (by design).

### What the finished video must show

`completed` means the file was written, not that it is right. These three checks are cheap and each one
names a defect that shipped in run `4f86657f` and is now pinned by tests, so a recurrence is a regression:

1. **No source caption left visible.** Covers are the tracked `speech_subtitle` boxes, so a black bar must
   sit exactly on the caption band for the window the source caption was up. `GET
   /api/v1/assets/<asset>/localized-visual-track` → `covers[]` must tile the timeline with no gap, and
   `ffmpeg -ss <t> -i <output> -frames:v 1` at each cover edge must show no source glyphs.
2. **One or two lines per Vietnamese cue, never a block.** `GET /api/v1/assets/<asset>/render/final` →
   `consumed_plan.subtitle_plan.cue_count`: a segment longer than two readable lines must appear as several
   cues (the splitter breaks at word boundaries, preferring sentence ends), and each cue's box stays in the
   caption band rather than growing to the frame width.
3. **The audio carries the dub.** The mix embeds the separated background stem plus the TTS clips, so a mix
   that equals the background stem byte-for-byte is a mute replacement: the source dialogue was suppressed and
   nothing was placed. Verify per artifact instead of trusting `overall_status`:
   ```bash
   ffmpeg -v error -i <dub_mix audio_cas_path> -ac 1 -ar 16000 -c:a pcm_s16le /tmp/mix.wav -y
   ffmpeg -v error -i <background stem audio_cas_path> -ac 1 -ar 16000 -c:a pcm_s16le /tmp/bg.wav -y
   python -c "import wave,numpy as np;r=lambda p:np.frombuffer(wave.open(p).readframes(-1),dtype=np.int16).astype(float);a,b=r('/tmp/mix.wav'),r('/tmp/bg.wav');n=min(len(a),len(b));print('corr',np.corrcoef(a[:n],b[:n])[0,1])"
   ```
   `corr ≈ 1.0` = no dub (the mixer now refuses that state instead of reporting PASS); `0.6-0.97` = dub present.
4. **No doubled cover bars.** One bar per caption instant: covers of consecutive captions must not overlap in
   time. When a frame shows two offset black bars over one caption band, the padded cover windows were not
   split.
   A single bar is not enough either: the box the OCR lane reports must bound the glyphs it names. Live
   evidence (1080x1440 frame, caption 你就得到了同款上帝视角): PaddleOCR's polygon stopped at x=828 while the
   glyph ink ran to 850, so the cover (+6px padding) left the last glyph's right edge on screen. The adapter
   now grows every detection box to the ink it overlaps (`refine_box_to_ink`); a detection whose box stops
   inside its glyphs means that lane was bypassed or disabled (`DOUYINIE_OCR_INK_REFINE=0`).
3. **The subtitle text is the TRANSLATION, never the source reading.** A run that reused cached artifacts
   (nothing to re-run, everything a cache hit) owns no variant index row of its own, so the visual lane must
   still resolve the translation through the run's own stage executions. When it cannot, the fallback branch
   draws `TextRegionPlan` region text - the *source* captions - as the localized subtitle, which lands the
   Chinese on top of the covers that were hiding it. Check the frozen plan:
   `consumed_plan.cue_count` and the cue texts (`sub-*` ids come from the fallback branch; the translated
   branch ids are `cue-<segment>[-<part>]`). The failing shape was live run `3adede59`; the fix is
   `VisualTextService.stageArtifactHash` binding `translation` / `dub_script` stage artifacts to the run.
5. **No garbled label drawn on the video.** Overlays come from `semantic_text` / `instructional_ui_text`
   regions that cleared the classifier's evidence gates. When junk text is drawn anyway, read
   `GET /api/v1/assets/<asset>/text-region-plan` and the region's `review_reason` before blaming the OCR:
   `spatially_unstable_label_noise` (readings of one label wandering across the frame),
   `spatio_temporal_ocr_instability_noise` (readings of one box disagreeing with each other) and
   `unreadable_caption_low_confidence` (a caption held for review instead of dropped) are the three
   deliberate outcomes.

A plan is cached by provenance, so a classifier change alone will not re-run detection: clear the derived
rows (`text_region_plans`, `localized_visual_tracks`, `localized_subtitle_tracks`, `render_plans`,
`render_artifacts`) in the scratch data dir or bump `TextRegionClassifierVersion`.

## 6. Blocks and flags you will meet on real media

The meaning-first QA gate **reports, it does not block**: a violation still marks the provider attempt
`quality_failed` (so a clean lane is preferred), but when every lane carries the same violation the best
candidate is persisted with the offending segment flagged, and the run continues. What stops a run is
either a stage that cannot produce an artifact at all (sample rate, missing interpreter) or the
final-render handoff, which waits for the operator.

| Symptom | Meaning |
|---|---|
| `AUDIO_ROLE_EXEC_FAILED … unsupported sample rate: 44100 Hz (expected 16000 Hz)` | separated stems are produced at the separator's native rate while the analyzer contract is 16 kHz |
| `translation QA gate flagged segment N: negation polarity inverted` in `provider_attempts` | Meaning-First gate polarity mismatch. The attempt is recorded as `quality_failed` so the ladder advances, but when no lane is clean the best candidate is still persisted with the segment flagged: **the run continues** and the exception waits in the review queue. Check whether the source `不` sits inside a lexical compound (`不透明度`) before treating it as a real inversion |
| `translation QA gate flagged segment N: name/brand 'XX' missing from target` in `provider_attempts` | entity gate vs OCR'd short tokens; same rule — the candidate is kept and flagged |
| `dub script QA gate flagged segment N` (pending `meaning_corrupted` item) | the spoken text drifted from the meaning text and the unshortened fallback did not clear it; correct the segment or accept it |
| `conditional diarization failed … DIARIZER_EXEC_FAILED` | wrong interpreter for the diarizer family (§2) |
| `visual_occlusion` pending exception (`overlay for … occludes protected region …`) | the tracked UI element sits next to another protected UI element (`GetProtectedBoxesForTimeWindow`) and the localized overlay cannot clear it. Since the occlusion-as-exception change the overlay is skipped (source text stays on screen) and `visual_text_localize` **continues**: resolve the item in `03 Review Workspace` → `Chỉnh region` (move the region so the overlay clears the protected box, or reclassify it to `brand_keep` / `ignore/noise`) and re-apply. A collision with a *scene-protected* region (face / tap target) still fails the stage, and a geometry the operator dragged onto a protected region is still rejected outright by `Chỉnh region` |
| `final render handoff blocked: N pending review exceptions` | correct gate: resolve the queue in `03 Review Workspace`, then re-check the handoff in `04 Kết quả` |
| `meaning preservation validation failed: dub script translation CAS "…" does not match canonical translation CAS "…"` | the run-scoped `TranslationVariant` index moved after the dub script was frozen. Inline overlay translations must be `Ephemeral` (`TranslationJobInput.Ephemeral`) so the visual lane cannot republish the canonical variant; if it appears anyway, compare the newest run-scoped `translation_variants` row against the dub script's embedded `TranslationVariantCAS` and re-run `translation` + `dub_script` together instead of resuming the visual stage alone |
| a region correction of an *overlay* region does not change the preview | `RenderPlan` currently freezes dub mix, audio and subtitle cues only — overlay geometry lives in the localized visual track (what the region inspector reads), so the preview artifact is legitimately identical. Freezing overlay references into `RenderPlan` (architecture §4) is an open gap |

Workaround to keep a run moving while `audio_role_plan` is blocked: seed an operator plan, then resume.

```bash
curl -X POST -H "Content-Type: application/json" \
  -d '{"segments":[{"start_ms":0,"end_ms":<asset ms>,"role":"narration/dialogue"}]}' \
  http://127.0.0.1:18099/api/v1/assets/<asset>/audio-role-plan
curl -X POST -H "Content-Type: application/json" -d '{}' \
  http://127.0.0.1:18099/api/v1/runs/<run>/resume
```

## 7. Evidence to capture

- `hub logs <daemon>` (spawn order, per-stage progress lines).
- `03 Review Workspace` and `04 Kết quả` screenshots (`orca screenshot --json`, base64 in `.result.data`).
- The review-item list: `GET /api/v1/runs/<run>/review-items` (type, severity, reason, stage).
- For gateway disputes: re-run the disputed segment against the authorized gateway with the shipped system
  prompt and compare against `detectNegation` semantics before filing anything
  (reference: `F:/douyinie-rt-scratch/live-20260918/probe_negation.py`).

## 8. Windows traps

- The agent shell cannot exec a relative `./thing.exe`; use an absolute path.
- `orca snapshot` intermittently returns no `result` on a busy SPA — retry, or read state with `orca eval`.
- Never pipe into `orca eval --stdin`; pass a one-line `--expression`.
- `orca snapshot` (and with it every `--element <ref>` command, including `upload`) can fail with
  `runtime_unavailable: The Orca runtime closed the connection` while `orca eval`, `orca find … --action click`,
  `orca mouse` and `orca tab` keep working. That is not a reason to abandon the UI run: read state with `eval`,
  click with `find --action click` or `mouse`, and when the file picker is needed attach a CDP-driven Chromium
  (`browser.open({app: {path: <chrome.exe>}})`, then `page.$('#source-input')` + `uploadFile`).
- Two minutes of `sleep` polling per stage is the normal cost of a real run; do not interpret a slow stage as
  a hang until `hub logs` shows no progress line for longer than the previous stage's worst case.

## 9. Reference evidence

- `docs/research/issue80-audio-role-plan-smoke-evidence-2026-09-10.md` — earlier audio-role smoke.
- `F:/douyinie-rt-scratch/live-20260918/EVIDENCE.md` and `REPORT-run-78ad6589.md` — the full live-run record
  (stage timeline, provider ledger, real artifacts, defect reproductions) behind the first E2E run that
  reached preview on a real Douyin video.
