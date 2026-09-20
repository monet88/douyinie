# Issue #119 — Douyin discovery/provider strategy

Date: 2026-09-19  
Parent: [#118 — Douyin Discovery & Media Workstation Expansion](https://github.com/monet88/douyinie/issues/118)  
Question: [#119 — Choose Douyin discovery and channel-feed provider strategy](https://github.com/monet88/douyinie/issues/119)

## Decision

Use a **small, separate discovery boundary backed primarily by the current `jiji262/douyin-downloader` Douyin Web implementation**. Keep keyword search and creator-profile lookup on Jiji's direct Web lane while those endpoints remain reachable, but treat creator-feed polling as a **page/browser-backed operation** because current Douyin `ArgusSecurityPlugin` deterministically rejects direct `aweme/post` requests. Discovery stops at normalized metadata and stable identities. A selected result is handed to the existing `AcquisitionService` as its canonical Douyin video URL; discovery never becomes a second download path.

The minimum provider surface is:

1. `SearchVideos(query, cursor, authRef)`
2. `ResolveCreator(locator, authRef)`
3. `ListCreatorVideos(creatorID, cursor, authRef)`

The cursor is provider-owned and opaque. A fallback provider must **restart the operation from its first page** rather than consume another provider's cursor.

Fallback policy:

- **Creator/profile:** Jiji direct Web calls remain primary while the upstream endpoint stays reachable.
- **Creator feed:** Jiji family remains primary, but the viable lane is an operator-authorized **real Douyin page/browser context** for `aweme/post`, not the CLI direct-HTTP path. F2 remains an evidence-gated fallback only: its source implements creator posts, but it must not be assumed to bypass the same platform Argus gate without a fresh live smoke.
- **Keyword search:** Jiji primary -> **DouK/TikTokDownloader as a cold/operator-enabled fallback only if a second search implementation is actually needed**. Current F2 does not qualify here: its own current feature table marks Douyin video/user search as future implementation. DouK currently implements Douyin search, but adding it permanently would mean another session/signing implementation and a GPL-3.0 dependency surface. Do not vendor or enable it by default just to have a theoretical fallback.
- **Browser/page assistance:** use it both to establish/refresh an operator-authorized session and to execute operations that now require page-generated security context. This is an execution lane under the same discovery semantics, not a second identity model or independent discovery provider.
- **Official Douyin Open Platform:** keep as a future conditional provider, not a present fallback. Its API shape is attractive, but current official pages are internally inconsistent about availability (the capability page says the experimental search capability is not externally open, while a product page advertises application for beta access), and the published search-use rules prohibit crawling/storage/cache/download/mirroring of platform resources. That is not a safe assumption for a workstation whose selected results intentionally flow into download/storage. Arbitrary creator monitoring is also not supplied by the official author-list API: that API requires the author to have an authorization/binding relationship with the caller's mini-app.

This decision is based on current upstream state as of 2026-09-19, not the older August reference snapshot.

## Existing Douyinie boundary that must remain authoritative

### Repo observations

- `internal/service/acquisition.go` already owns the production download boundary: policy eligibility -> provider router -> probe -> acquire -> integrity validation -> immutable `SourceAsset`.
- `internal/provider/acquisition_cli.go` normalizes a canonical Douyin post to `douyin:aweme:<aweme_id>` and `https://www.douyin.com/video/<aweme_id>` before acquisition.
- RuntimeHost already registers Jiji, F2 and browser-assisted acquisition adapters. Credentials are resolved through `CredentialService` at call time and raw session secrets are not persisted.
- Existing acquisition failure classification already distinguishes auth/session/captcha/rate-control-like failures from content unavailable and integrity failure.
- Prior live evidence in `douyin-ingestion-reference-smoke-test.md` showed that an unauthenticated Jiji detail request could be rejected with an empty HTTP 200 response, while the documented operator-authorized Playwright cookie flow succeeded for the same real Douyin item. That evidence is historical, not proof of the current direct-download path: upstream now documents that, since 2026-09-14, direct `aweme/detail` and `aweme/post` requests are deterministically Argus-gated even with cookies. An authorized session remains necessary, but cookies alone are no longer sufficient for those gated endpoints.

### Architectural implication

Do **not** add search/feed methods to `AcquisitionProvider`. Search and monitoring have different pagination, durability and failure semantics from media acquisition. Add one discovery service/seam in front of acquisition and reuse only the already-settled infrastructure that is actually shared: provider runtime location, `CredentialRef`/`CredentialService`, structured provider errors, canonical Douyin identity, and the final `AcquisitionService` handoff.

The data flow should remain:

```text
Douyin discovery provider
  -> normalized discovery candidates / creator feed
  -> operator selection or later follow-channel policy
  -> canonical https://www.douyin.com/video/<aweme_id>
  -> existing policy-gated AcquisitionService
  -> immutable SourceAsset
```

Discovery must not materialize media bytes into CAS, create `SourceAsset`, or bypass acquisition rights attestation.

## Current provider evidence

### 1. Douyin Open Platform — high-fidelity search API, but not a deployable primary for this product today

**Verified official facts**

- The current video-search API is documented as `GET https://open.douyin.com/dy_open_api/v2/search/video/` with scope `aweme.dy.video_search_v2` and a required application capability.
- It supports exactly the search controls Douyinie needs at provider level: keyword, `sort_type` (`0` relevance, `1` most liked, `2` newest), `publish_time` (`0`, 1 day, 7 days, 180 days), cursor/`has_more`, and a `search_id` that must be replayed on load-more.
- Its response example contains `item_id`, title, signed cover URL, create time, nickname/avatar, `statistics.digg_count`, canonical Douyin link, cursor and `search_id`.
- The capability page publishes a baseline quota of 1,000 calls/day, with higher quota by application.
- The same capability page currently says the search capability is experimental and not externally open. A separate product page says "officially open" but still tells developers to apply for beta access. Therefore availability must be treated as permission-gated, not assumed from API documentation alone.
- The capability's published use rules prohibit crawling, storage, caching, downloading or mirroring of platform resources and use to build similar products. The same page describes the intended flow as search in the third-party app followed by opening Douyin for content consumption.
- The official "get author video list" API is for mini-program developers/agents and requires the queried video author to have authorized/bound the app's "query author video list" capability. It is therefore not an arbitrary public creator-feed API for Douyinie's followed-channel list.
- Official `oauth/userinfo` returns public info for an **authorized** user identified by that application's `open_id`; it is not arbitrary creator lookup by public Douyin URL.

**Decision implication**

If Douyinie later receives explicit search capability approval **and** Douyin confirms the workstation's intended search -> optional download/storage flow is allowed, an official provider would be the preferred search implementation because it removes Web signing/risk-control maintenance. That is a future capability gate, not something issue #119 can assume.

### 2. jiji262/douyin-downloader — primary

**Verified upstream state**

- Current `main` was pushed on 2026-09-17 (commit `47f4eef87b34042a7862d36d9bc10f749fcb888d`) and is actively maintained.
- `core/discovery.py` now explicitly implements **metadata-only discovery**: the module states that it collects hot-board/search data and does not download media; selected links can be sent to the downloader afterwards. This matches Douyinie's desired boundary unusually well.
- `search_and_dump` calls `DouyinAPIClient.search_aweme`, deduplicates by `aweme_id`, walks `has_more`, advances by the returned cursor/offset, can accept the existing rate limiter, and supports maximum-result bounds.
- `search_aweme` calls the current Douyin Web general-search endpoint and accepts:
  - `sort_type`: relevance / most liked / newest;
  - `publish_time`: unlimited / 1 day / 7 days / half-year;
  - `offset` + `count`, returning normalized `has_more` and next offset.
- Search results are flattened from Douyin's raw `aweme_info`, rather than reduced to a downloader-specific filename record.
- `get_user_info(sec_uid)` calls the Douyin Web profile endpoint. `get_user_post(sec_uid, max_cursor, count)` still exists and normalizes `aweme_list`, `has_more`, and the next cursor, but current upstream explicitly marks direct `aweme/post` as Argus-gated; source availability must not be confused with a viable direct runtime path.
- `core/url_parser.py` recognizes canonical `/user/<sec_uid>` URLs. Current metadata helpers explicitly extract `author.sec_uid` and build `https://www.douyin.com/user/<sec_uid>`; they also extract static video covers preferring original/high-quality variants.
- Current request code documents operational risk control rather than hiding it: HTTP 403/429 are treated as transient Web WAF/rate-control outcomes; login expiry is distinguished from those; deterministic `ArgusSecurityPlugin` rejection is not treated as something a retry can solve. The bounded retry schedule is 1/2/5 seconds.

**Auth/session**

Jiji can expose some public Web data without login, and upstream still reports search, user profile, following list, comments and live-room endpoints reachable through the direct Web lane as of 2026-09-14. A fresh 2026-09-20 anonymous keyword-search smoke on the pinned `47f4eef` reference reached `/aweme/v1/web/general/search/single/` but returned `status_code=2483` (login required), not an Argus rejection; the automatic interactive relogin could not complete in the non-interactive worker terminal. Treat direct search/profile as session-sensitive rather than anonymous-by-contract. For `aweme/post` and other known gated endpoints, however, the credible operating contract is stronger: use an operator-authorized real Douyin page/browser context so the page SDK supplies the required security context. Surface `AUTH_REQUIRED`, `SESSION_EXPIRED`, `CAPTCHA_REQUIRED`, and deterministic Argus/risk-control states, and never implement cookie farms, captcha bypass, signature reverse-engineering or anonymous identity rotation.

**Why it is the primary**

It is still the strongest primary provider family because it satisfies keyword discovery, creator identity/profile, rich raw metadata, active maintenance, and direct reuse of code and operating knowledge Douyinie already carries. The current Argus change means the implementation must split Jiji by execution lane: direct Web requests for reachable discovery endpoints and a page/browser-backed lane for creator feed. Upstream still separates discovery output from download execution, which reduces the amount of new Douyin-specific domain code Douyinie needs.

### 3. Johnserf-Seed/f2 — creator/profile/feed fallback, not search fallback

**Verified upstream state**

- Current `main` was pushed on 2026-09-12 (commit `7dab3e2ffffaa2535834d28fca99dbc2e89fa9d3`), Apache-2.0.
- Its current feature table marks Douyin `fetch_user_profile` and `fetch_user_post_videos` implemented.
- `DouyinHandler.fetch_user_post_videos` exists and the docs describe paginated interfaces as async generators carrying pagination data from the prior response.
- The same current feature table explicitly defines blue status as **future implementation** and marks Douyin `fetch_search_videos`, `fetch_search_users`, and `fetch_search_lives` blue. Search implementations found in current source are for TikTok, not a usable Douyin search provider.

**Decision implication**

F2 is a useful fallback candidate for creator/profile/feed operations because Douyinie already has an F2 acquisition runtime seam. However, its creator-post implementation also targets Douyin Web behavior and must not be treated as an Argus workaround without fresh live evidence for the exact operation. It must not be advertised or coded as the keyword-search fallback until upstream actually ships that Douyin capability and it passes a live smoke test.

### 4. JoeanAmier/TikTokDownloader (DouK) — viable cold keyword-search fallback

**Verified upstream state**

- Current `master` was pushed on 2026-09-16 (commit `473c90ff70c663cfb69310fff2b8d5192f200661`), GPL-3.0.
- Current source implements Douyin search across general/video/user/live channels. Video search has `sort_type`, `publish_time`, duration and other filters; pagination uses offset/count and retains the returned `search_id` for later pages.
- Current account code implements `/aweme/v1/web/aweme/post/` by `sec_user_id`, `max_cursor` and `has_more`, including earliest/latest date stopping for account posts.
- The README advertises detailed Douyin data collection, dynamic/static covers, publication-time filtering, incremental account collection, statistics and Douyin search data.

**Tradeoff**

This is technically a stronger keyword-search fallback than current F2, but it creates another Web-signing/session implementation to operate and carries GPL-3.0 integration/distribution considerations. It should therefore be a **cold** fallback: prove and enable it only when Jiji search has a demonstrated outage or incompatible upstream change that requires an independent implementation.

### 5. Evil0ctal/Douyin_TikTok_Download_API v5 — active and sophisticated, but too large and no wired search surface for this boundary

**Verified upstream state**

- Current `main` was pushed on 2026-09-15 (commit `9fa3e5406694c80ec0a97399f410948f130d0f9e`), Apache-2.0.
- v5 provides a self-hosted REST/MCP/console system with an identity pool, scheduler, PostgreSQL/TimescaleDB, Redis and a browser sidecar.
- Its Douyin source defines constants for general/video/user search, but the current registered P0 endpoint table wires content detail, author profile/posts, comments, likes, collections and session check — not keyword search.
- Its author model explicitly treats Douyin `sec_user_id` as the stable primary key rather than mutable display identity.
- Its own source comments record a measured 2026-09-08 behavior where nonzero `max_cursor` for a Douyin author-post request could return only `{"status_code":0}` with no list, demonstrating that cursor behavior remains an active Web-compatibility concern even in a mature provider.

**Decision implication**

It is useful as a reference for normalized IDs, opaque cursors, failure classification and provider operations, but adopting an identity-pool service plus Postgres/Redis/browser sidecar to solve three discovery calls would violate the smallest-credible-boundary goal. Do not add it to Douyinie's runtime for #119.

## Comparison

| Candidate | Keyword search | Creator URL/profile | Creator feed | Pagination | Metadata fidelity | Auth / anti-bot exposure | Maintenance / Douyinie reuse | Decision |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Official Open Platform | Yes if capability granted | Authorized users, not arbitrary public creator URL | Only authorization-bound author list | Search cursor + `search_id`; official author list `max_cursor` | Search example has item id, title, cover, time, nickname/avatar, likes, link | App token/capability/quota; avoids Web signing | Lowest technical maintenance, but permission/use-policy mismatch for current workstation | Future conditional only |
| Jiji | **Yes, direct and current** | **Yes by `sec_uid` / canonical user URL** | **Yes via page/browser-backed lane; direct `aweme/post` is Argus-gated** | Search offset/has_more; feed max_cursor/has_more | Raw aweme data; helpers for stable author sec_uid and high-quality cover; title/time/stats available in upstream payloads | Direct search/profile still reachable; gated feed requires real page security context | **Already primary acquisition provider family; active upstream** | **Primary, split by execution lane** |
| F2 | Current upstream says future | Yes | Source implements creator posts; current Argus viability unproven | Async-generator page state / cursors | Rich filter/raw data model | Cookie/signing exposure; do not assume Argus bypass | Already existing acquisition fallback | Evidence-gated creator/profile/feed fallback only |
| DouK/TikTokDownloader | **Yes, current** | Yes | Yes | Search offset + search_id; feed max_cursor | Rich data, covers, stats, dates | Cookie/signing exposure | New dependency; GPL-3.0; active upstream | Cold search fallback |
| DTK v5 | Search constants exist but not wired in current P0 API | Yes | Yes | Opaque cursor abstraction; current upstream has documented Web cursor edge cases | Strong normalized content model | Identity pool/risk-control system | Heavy new service stack, duplicates much of Douyinie | Reference only |

## Provider contract and normalized data

The interface should normalize only fields the product currently needs. Avoid mirroring the entire Douyin Web payload.

```text
DiscoveryProvider
  SearchVideos(query, opaqueCursor, credentialRef) -> DiscoveryPage<VideoCandidate>
  ResolveCreator(locator, credentialRef)            -> CreatorDescriptor
  ListCreatorVideos(creatorID, opaqueCursor, credentialRef) -> DiscoveryPage<VideoCandidate>

DiscoveryPage
  items
  next_cursor   // opaque and provider-bound
  has_more

VideoCandidate
  aweme_id              // stable video identity
  canonical_url         // https://www.douyin.com/video/<aweme_id>
  author_sec_uid        // stable creator identity for Web providers
  author_nickname       // display snapshot
  author_avatar_url?    // display snapshot, ephemeral URL
  title_or_desc
  published_at
  like_count?           // observation snapshot, not identity
  cover_url?            // ephemeral preview material
  preview_url?          // optional/ephemeral; never the acquisition source identity
  duration_ms?
  observed_at

CreatorDescriptor
  sec_uid
  canonical_url         // https://www.douyin.com/user/<sec_uid>
  nickname?
  avatar_url?
```

`aweme_id` and `author_sec_uid` are the durable identity keys. Nickname, avatar, likes, title text, covers and preview URLs are observations and may change or expire.

Do not expose provider-specific `offset`, `max_cursor`, `search_id` or signing fields above the adapter. The adapter can serialize whatever combination it needs into `next_cursor`. This is necessary because current candidates already disagree: official search requires cursor + search_id, Jiji keyword search uses offset, and creator feeds use max_cursor-like cursors.

## Search filters

The Web/API providers expose a useful but coarse common subset:

- sort: relevance / most liked / newest;
- publish window: unrestricted / last day / last week / roughly half-year;
- page size / maximum items.

Douyinie's UI requirement also mentions date and like filters. Keep those as application-level filters when they are more precise than the provider can express. Push down only the provider-supported coarse filters, then apply exact `published_at` and `like_count` predicates to normalized candidates. Do not claim that a locally post-filtered top-N search is exhaustive if the provider stopped after a bounded result count.

## Follow-channel polling semantics

For followed channels, the durable checkpoint should be **known `aweme_id`s plus an observation watermark**, not a saved remote cursor.

Each poll should start from the creator's newest page and stop when it reaches already-known items / an older observation boundary. Reasons:

- feed cursors are provider-specific and can change semantics;
- fallback providers cannot consume each other's cursor;
- newly published posts appear before a previously stored page position;
- starting at newest makes detect/list-only behavior deterministic and naturally supports later opt-in auto-download policies.

This also keeps fallback simple: if Jiji feed polling fails structurally and F2 is healthy, restart F2 from its first page, dedupe on `aweme_id`, and stop at the same stored known-item boundary.

## Fallback and failure rules

1. **Never switch provider mid-pagination with the old cursor.** Restart from page one, dedupe by `aweme_id`, and surface which provider supplied the page.
2. **Do not treat an empty page as proof of "no results" after an auth/risk-control failure.** Providers must distinguish valid empty results from session expiry, captcha, 403/429/risk control, invalid creator/content, and upstream-shape drift.
3. **No anti-bot bypass.** Bounded retry/backoff and operator-authorized session refresh are acceptable; cookie pools, captcha solving and identity rotation are outside this product boundary.
4. **Preview is best-effort.** Cover/preview URLs are ephemeral observations. The UI must tolerate expiry and refresh metadata or fall back to the cover/placeholder. A signed CDN URL is never a durable media identity.
5. **Download remains entirely inside existing acquisition.** Discovery fallback decides only how candidates are listed. Once an item is selected, all providers converge on the same canonical Douyin video URL and hand it to the existing acquisition boundary. The current acquisition execution strategy must be updated for the same September Argus change: direct `aweme/detail` can no longer be assumed viable merely because a cookie is present; the page/browser-backed lane owns known gated requests.

## Exact implications for Douyinie

- Add one discovery service/provider seam; do not modify the meaning of `AcquisitionProvider`.
- Reuse the existing Jiji runtime and `CredentialService` reference rather than introducing a new scraper service for the primary path. Add a page/browser-backed execution lane for known Argus-gated operations instead of reverse-engineering signatures.
- Normalize and persist `aweme_id` and creator `sec_uid` as identity. Persist mutable display metadata with `observed_at` so refreshes can be reasoned about.
- Treat search pagination state as opaque and short-lived. For creator monitoring, persist known-item/watermark state rather than remote cursors.
- Search result selection / multi-select / bulk download should convert each selected candidate to its canonical Douyin URL and call the existing acquisition API independently. This preserves existing policy, rights attestation, dedup and SourceAsset integrity behavior.
- Follow-channel default detect/list does not create download jobs. Later B/C automation tickets can consume the same newly-discovered candidate stream and explicitly call acquisition/job creation when enabled.
- Do not add F2 search code based on names/docs that are not implemented upstream today.
- Do not install DTK v5 or its database/identity-pool stack for this feature.
- Keep official Open Platform support behind an explicit future capability/policy decision; do not silently swap to it because an API endpoint exists in documentation.

## Open risks / validation required before implementation acceptance

1. **Fresh split-lane live smoke:** a 2026-09-20 direct Jiji keyword-search attempt on the pinned source returned truthful `status_code=2483` login-required before results; no operator login was fabricated or imported. Acceptance still needs an authorized-session rerun for (a) keyword search, (b) creator profile/identity resolution, and (c) multi-page creator-feed retrieval through the page/browser-backed lane, confirming normalized IDs, pagination and truthful failure states.
2. **Creator short/share URLs:** current Jiji parser clearly handles canonical `/user/<sec_uid>` URLs. Implementation should verify the exact redirect/canonicalization behavior for the creator share-link forms the operator actually pastes and add bounded redirect normalization if needed.
3. **Search result completeness:** Web search is ranked and coarse-filtered; local `min_likes` or arbitrary date filtering can reduce a bounded page set. Product copy/spec must not imply exhaustive corpus search.
4. **Web endpoint drift:** Jiji, F2 and DouK all ultimately depend on non-contractual Douyin Web behavior. Keep the provider seam small so one adapter can be repaired or replaced without changing followed-channel state or media acquisition.
5. **Official API permission/terms:** official pages currently present conflicting availability language. Re-evaluate only if Douyin grants the exact capability to the intended application and confirms the intended search-to-download/storage workflow is allowed.
6. **DouK licensing:** if the cold fallback is ever moved from a developer/operator tool into a distributed Douyinie build, review GPL-3.0 obligations before integrating or redistributing it.

## Sources

### Official Douyin

- Search capability and use rules: https://developer.open-douyin.com/docs/resource/zh-CN/dop/ability/search-management/item-search
- Video-search API: https://developer.open-douyin.com/docs/resource/zh-CN/dop/develop/openapi/douyin-search-capability/aweme-dy-video-search
- Search product/application page: https://developer.open-douyin.com/product/search
- Author video list (authorization-bound mini-app API): https://developer.open-douyin.com/docs/resource/zh-CN/mini-app/develop/server/reach-marketing/mount/self_mount/get-user-video-list
- Authorized user public info: https://developer.open-douyin.com/docs/resource/zh-CN/dop/develop/openapi/account-permission/get-account-open-info

### Current upstream provider sources

- Jiji repository: https://github.com/jiji262/douyin-downloader
- Jiji discovery module: https://github.com/jiji262/douyin-downloader/blob/47f4eef87b34042a7862d36d9bc10f749fcb888d/core/discovery.py
- Jiji Web API client: https://github.com/jiji262/douyin-downloader/blob/47f4eef87b34042a7862d36d9bc10f749fcb888d/core/api_client.py
- Jiji URL parser: https://github.com/jiji262/douyin-downloader/blob/47f4eef87b34042a7862d36d9bc10f749fcb888d/core/url_parser.py
- Jiji normalized metadata helpers: https://github.com/jiji262/douyin-downloader/blob/47f4eef87b34042a7862d36d9bc10f749fcb888d/core/metadata.py
- F2 repository/current feature table: https://github.com/Johnserf-Seed/f2/blob/7dab3e2ffffaa2535834d28fca99dbc2e89fa9d3/README.en.md
- F2 Douyin handler: https://github.com/Johnserf-Seed/f2/blob/7dab3e2ffffaa2535834d28fca99dbc2e89fa9d3/f2/apps/douyin/handler.py
- DouK/TikTokDownloader search: https://github.com/JoeanAmier/TikTokDownloader/blob/473c90ff70c663cfb69310fff2b8d5192f200661/src/interface/search.py
- DouK/TikTokDownloader account feed: https://github.com/JoeanAmier/TikTokDownloader/blob/473c90ff70c663cfb69310fff2b8d5192f200661/src/interface/account.py
- DTK v5 repository: https://github.com/Evil0ctal/Douyin_TikTok_Download_API
- DTK v5 Douyin endpoint table: https://github.com/Evil0ctal/Douyin_TikTok_Download_API/blob/9fa3e5406694c80ec0a97399f410948f130d0f9e/src/dtk/platforms/douyin/endpoints.py
- DTK v5 normalized content model: https://github.com/Evil0ctal/Douyin_TikTok_Download_API/blob/9fa3e5406694c80ec0a97399f410948f130d0f9e/src/dtk/models/content.py

### Douyinie local evidence

- `internal/service/acquisition.go`
- `internal/provider/acquisition.go`
- `internal/provider/acquisition_cli.go`
- `cmd/runtimehost/main.go`
- `docs/research/douyin-ingestion-reference-smoke-test.md`
- `docs/research/issue56-douyin-acquisition-corpus-2026-09-03.md`
