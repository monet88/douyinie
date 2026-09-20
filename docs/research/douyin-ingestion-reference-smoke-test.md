# Douyin ingestion reference smoke test

## Conclusion

For the current Phase 1 source-ingestion decision, `jiji262/douyin-downloader` is the strongest implementation reference to exercise against a real Douyin URL. Its core URL/parser/downloader/retry/storage tests pass on this Windows host. `f2` is a viable secondary parsing/CLI reference. `TikTokDownload` is effectively a thin wrapper around `f2`. `TikTokDownloader` imports and its bundled tester suite passes on Python 3.12. `Evil0ctal/Douyin_TikTok_Download_API` is useful for API/parsing patterns but has older dependency/runtime coupling and should not be the primary reference.

Live testing was performed on `https://v.douyin.com/t6U4nCrLIYc/` (resolved public URL: `https://www.douyin.com/video/7659396126545619322?previous_page=web_code_link`). Normal unauthenticated acquisition did **not** succeed: Jiji reached the video-detail API but received an empty HTTP 200 response classified by upstream as anti-bot; F2 resolved the aweme ID but failed earlier while generating `msToken` because `mssdk.bytedance.com` resolves to `0.0.0.0` on the tested network. No media file was produced.

## Local reference layout

All source-ingestion references are shallow-cloned under the repo-local `.ref/` directory, which is ignored by the repository root `.gitignore`.

- `douyin-downloader` — current repo-local reference `47f4eef87b34042a7862d36d9bc10f749fcb888d` (the live results below were captured on the older August pin and are retained as historical evidence)
- `Douyin_TikTok_Download_API` — `42784ffc83a72a516bfe952153ad7e2a3998d16c`
- `TikTokDownloader` — `d3806386b392da7341397e18522acdd5283f2c81`
- `f2` — `7dab3e2ffffaa2535834d28fca99dbc2e89fa9d3`
- `TikTokDownload` — `64acbfda8621563da97ed06f133330c3335b1e8d`

## Findings

### jiji262/douyin-downloader

- Documented Windows-compatible CLI: `python run.py -u <url> -p <path>`; optional REST server mode is also present.
- On this host, `PYTHONUTF8=1` is needed for CLI help in the current PowerShell environment. Without it, argparse output containing Chinese text raises `UnicodeEncodeError` under cp1252.
- Targeted parser/downloader/retry/storage/API tests: **123 passed**.
- After installing upstream test/optional dependencies (`hypothesis`, `fastapi`, `uvicorn`), full suite: **568 passed, 1 skipped**.
- This is the preferred live-URL candidate because its core concerns match Douyinie's needed `probe -> acquire -> integrity/cache` behavior most closely.

### Johnserf-Seed/f2

- Installed successfully in an isolated Python 3.13 environment.
- `f2 dy --help` renders correctly and exposes single-item mode, cookie input, retry/timeout/concurrency controls, and auto-cookie options.
- Targeted CLI/download/signature helper tests: **11 passed**.
- Suitable as a secondary parser/provider reference; it should remain behind Douyinie's provider boundary rather than become domain logic.

### JoeanAmier/TikTokDownloader

- Repository pins Python **3.12** via `.python-version`; tested with Python 3.12.13.
- Package imports successfully after installing its requirements.
- Bundled `src/testers` suite: **8 passed**.
- Useful as another operational/download UX reference, but current ticket does not need it ahead of Jiji/F2 for live testing.

### Evil0ctal/Douyin_TikTok_Download_API

- Python 3.13 setup fails at pinned `lz4==4.3.3` because no compatible wheel was available and local MSVC Build Tools were not present.
- Python 3.12.13 setup succeeds; importing the app creates a FastAPI application with **71 routes**.
- Importing the app also immediately attempts remote msToken/network work and falls back after DNS/network failure. That side effect makes the app package a poor direct library dependency for Douyinie.
- Keep it as a reference for endpoint shapes, URL/ID extraction and provider implementation ideas; do not make it the primary adapter.

### Johnserf-Seed/TikTokDownload

- Current repository is largely a compatibility/wrapper shell around `f2`; its `requirements.txt` contains only `f2`.
- In Python 3.12.13, `f2 0.0.1.7` imports and `f2 dy --help` works.
- There is little unique ingestion logic here worth carrying separately once `f2` itself is already mined.

## Implications for this repo

- Implement the `SourceAdapter` around a small, testable provider seam; mine Jiji first, then F2 for fallback patterns.
- Preserve UTF-8 process I/O explicitly for any Windows subprocess integration.
- Do not import large third-party downloader apps wholesale when their module import performs network initialization.
- Treat URL parsing, metadata normalization, acquisition, integrity validation and cache materialization as separate observable steps so live failures can be classified.
- Test real URLs only through documented normal paths. Authentication/captcha requirements should surface as explicit provider failures; do not build anti-bot bypass behavior into Douyinie.

## Open questions

- The 2026-08-19 cookie-assisted Jiji success below is historical pre-Argus evidence. Current upstream at `47f4eef87b34042a7862d36d9bc10f749fcb888d` documents direct `aweme/detail` as deterministically Argus-gated since 2026-09-14 even with cookies, so it is no longer evidence that the direct CLI acquisition lane is viable.
- Jiji CLI `--browser-fallback` is a profile-post recovery path, not the desktop `page_bridge`; current single-video acquisition requires an operator-authorized real-page execution lane or must fail closed.
- If normal acquisition fails because Douyin requires authorization/captcha, the product policy decision remains whether the Phase 1 default should require a user-supplied local source artifact.

## Sources

- jiji262/douyin-downloader — https://github.com/jiji262/douyin-downloader
- Evil0ctal/Douyin_TikTok_Download_API — https://github.com/Evil0ctal/Douyin_TikTok_Download_API
- JoeanAmier/TikTokDownloader — https://github.com/JoeanAmier/TikTokDownloader
- Johnserf-Seed/f2 — https://github.com/Johnserf-Seed/f2
- Johnserf-Seed/TikTokDownload — https://github.com/Johnserf-Seed/TikTokDownload
- Douyinie Wayfinder map — https://github.com/monet88/douyinie/issues/1
- Source acquisition policy ticket — https://github.com/monet88/douyinie/issues/17


## Live URL test — 2026-08-19

Input share text contained `https://v.douyin.com/t6U4nCrLIYc/`.

- Plain HTTP redirect resolution succeeded with status 200 and canonicalized to aweme ID `7659396126545619322`.
- `jiji262/douyin-downloader` resolved the same aweme ID, then `/aweme/v1/web/aweme/detail/` exhausted retries on an empty HTTP 200 body. Upstream explicitly treats this shape as a likely anti-bot signal and retries with a fresh signature. Result: **0 success / 1 failed**, no media or JSON artifact created.
- `f2` `AwemeIdFetcher.get_aweme_id()` independently resolved the short URL to `7659396126545619322`.
- Full `f2 dy -M one` did not reach media parsing. During module/model initialization it attempted to generate `msToken` through `mssdk.bytedance.com`; DNS returned `0.0.0.0`, causing `getaddrinfo failed` and `APIConnectionError`.
- `Evil0ctal/Douyin_TikTok_Download_API` `AwemeIdFetcher.get_aweme_id()` also resolved the same aweme ID under Python 3.12.
- Direct GET of the canonical Douyin page returned a generic ~72 KB HTML shell with no embedded aweme ID/title/description in the static response, so static HTML parsing is not a useful metadata fallback for this sample.

### What this proves

- Short-link normalization / canonical source ID extraction works across three independent references.
- The tested public/no-cookie media-acquisition path is currently unreliable for this real URL; a successful URL parser must not be conflated with an authorized/reliable acquisition provider.
- Douyinie should model `probe` and `acquire` separately and persist structured failure classes such as `ANTI_BOT_OR_EMPTY_RESPONSE`, `AUTH_REQUIRED`, and `TOKEN_SERVICE_UNAVAILABLE` instead of reporting a generic parse failure.
- No anti-bot/captcha bypass was attempted. A later explicit user-authorized Playwright login/cookie capture produced an authenticated session and successful media acquisition; Jiji browser-fallback pagination itself remains untested.
## README authentication requirements — 2026-08-19

- `jiji262/douyin-downloader`: README requires Python 3.8+ and dependencies; for reliable acquisition it recommends valid Douyin cookies. Its documented path is `python -m tools.cookie_fetcher --config config.yml`, which opens the login flow and writes cookies into config. Browser fallback additionally requires Playwright + Chromium.
- `Johnserf-Seed/f2`: README links to cookie configuration docs. The docs recommend a complete logged-in Douyin Web cookie, support manual `-k ... --update-config`, and offer `--auto-cookie`; they warn Chromium V20 cookie encryption can break automatic browser-cookie extraction. The FAQ says empty responses are generally a cookie configuration problem and recommends a normal logged-in account rather than a guest cookie.
- `Evil0ctal/Douyin_TikTok_Download_API`: deployment README explicitly says self-hosters need to handle crawler-cookie risk control and recommends cookies from an account already logged in on Douyin Web, replacing the Douyin web cookie in its config.
- `JoeanAmier/TikTokDownloader` / DouK-Downloader: README requires Python >=3.12 for source mode and tells the user to put Cookie information into config. Manual/clipboard cookie entry is current; browser-cookie reading is marked deprecated and QR-login cookie acquisition is marked failed/disabled. README notes logged-in cookies can improve data access and video resolution.
- `Johnserf-Seed/TikTokDownload`: its README advertises QR-code login and generated cookie values, but the checked current branch is mostly a wrapper around `f2`; for current behavior, follow the `f2` cookie/session requirements rather than treating the older README flow as authoritative.

### Implication for the live sample

The controlled Jiji retest used its documented `tools.cookie_fetcher` Playwright login flow. Raw cookie values were not printed; cookie material stayed under the repo-local ignored `.ref/_live-tests/` area. The authenticated request succeeded and produced a durable media artifact.

## Authenticated Jiji retest — 2026-08-19

- User explicitly authorized the Jiji README login flow and completed Douyin login in the Playwright Chromium window opened by `python -m tools.cookie_fetcher --config ...`.
- Cookie capture completed successfully and saved 24 Douyin cookies inside ignored `.ref/_live-tests/t6U4nCrLIYc/`; raw cookie values were not printed. The captured set included `ttwid`, `odin_tt`, `passport_csrf_token`, and `sid_guard`; `msToken` was absent and Jiji attempted its normal automatic generation path.
- The same URL `https://v.douyin.com/t6U4nCrLIYc/` then succeeded through Jiji: `/aweme/v1/web/aweme/detail/` returned HTTP 200, 115049 bytes, `status_code=0`, and an `aweme_detail` payload.
- Jiji reported **1 total / 1 success / 0 failed / 100% success rate** and wrote both media plus JSON metadata.
- Downloaded media: aweme `7659396126545619322`, title `10款有袖实穿的通勤连衣裙分享#通勤连衣裙`, author folder `九九衣橱`.
- Media integrity snapshot: 33,215,327 bytes; SHA-256 `CA961831B0DE6B74DEF3BA61251FE53864160489E3A29BFAF2008CCBE46F6F90`; HEVC 2560x1440 at 30 fps; AAC 44.1 kHz stereo; duration 445.100998 s; 5-second ffmpeg decode check passed.
- Jiji still logged failure to generate a real `msToken` because the environment could not resolve the token service, but the authenticated cookie set was sufficient for this sample to return metadata and media successfully.

### Updated implication

For this sample, Jiji demonstrated a `probe -> authenticated acquire -> integrity check` path on 2026-08-19. That result does not describe the post-2026-09-14 platform state. Current upstream routes `aweme/detail` through a gated request path and states that the CLI's direct HTTP lane receives deterministic Argus 403 responses; only a real logged-in page context can supply the required page SDK security material. Douyinie therefore keeps Jiji's direct lane for still-reachable operations/probing, while gated single-video acquisition must execute through an operator-authorized page-backed provider or fail closed.
