// Real-browser acceptance smoke for the Issue #156 review-only tempo audition surface.
//
// The Node VM harness (app_behavior.mjs) runs app.js against a stubbed DOM and the seam-1 Go
// test streams the candidate over HTTP, so neither covers what the operator actually gets: a
// real client resolving the two run-scoped media URLs the panel builds, decoding and playing
// both waveforms, and showing no way to select the alternative. This script drives a real
// Chromium-family browser over CDP against a live RuntimeHost and walks that path:
//
//   select run → open exceptions → select the tempo review item → the panel binds the retained
//   natural waveform and one eligible alternative → both decode, play and hash to the exact
//   candidate artifacts → nothing in the surface selects or approves the alternative
//
// Usage: node tempo_audition_browser_smoke.mjs <operator-ui-url> <run-id> <review-item-id> <natural-sha256> <transformed-sha256>
// Exit codes: 0 = pass, 1 = failure, 3 = no Chromium-family browser available (caller skips).

import { rmSync } from "node:fs";

import {
  SKIP_EXIT_CODE,
  sleep,
  findBrowser,
  CDP,
  launchBrowser,
  createChecklist,
  attachPage,
} from "./cdp_client.mjs";

const [uiUrl, runID, itemID, naturalSHA, transformedSHA] = process.argv.slice(2);
if (!uiUrl || !runID || !itemID || !naturalSHA || !transformedSHA) {
  console.error(
    "usage: node tempo_audition_browser_smoke.mjs <operator-ui-url> <run-id> <review-item-id> <natural-sha256> <transformed-sha256>",
  );
  process.exit(1);
}

const { check, failures, checks } = createChecklist();
const expectedAbsentArtifacts = [];
const consoleErrors = [];

const browserPath = findBrowser();
if (!browserPath) {
  console.log("SKIP no Chromium-family browser found (set DOUYINIE_CHROME_BIN to pin one)");
  process.exit(SKIP_EXIT_CODE);
}
console.log(`using browser: ${browserPath}`);

// Filled in once a page session exists: a failed smoke must show what the page actually saw.
let pageDiagnostics = async () => [];

const { child, endpoint, profileDir } = await launchBrowser(browserPath);
let cdp = null;

try {
  cdp = await CDP.connect(endpoint);
  const { session, evaluate, waitFor, rectOf, clickElement } = await attachPage(cdp, {
    consoleErrors,
    expectedAbsentArtifacts,
  });

  pageDiagnostics = async () => {
    const lines = [`page url: ${await evaluate("window.location.href").catch(() => "?")}`];
    const panel = await evaluate(`JSON.stringify({
      panelHidden: document.querySelector("#inspect-tempo")?.classList.contains("hidden") ?? null,
      pill: document.querySelector("#inspect-tempo-selectable")?.textContent ?? null,
      note: document.querySelector("#inspect-tempo-note")?.textContent ?? null,
      naturalSrc: document.querySelector("#inspect-tempo-natural")?.getAttribute("src") ?? null,
      transformedSrc: document.querySelector("#inspect-tempo-transformed")?.getAttribute("src") ?? null,
      exceptionRows: [...document.querySelectorAll("#exception-list [data-review-id]")].map((el) => el.dataset.reviewId),
      selectedReviewID: document.querySelector(".exception-item.is-active")?.dataset?.reviewId ?? null,
    })`).catch(() => "?");
    lines.push(`tempo panel: ${panel}`);
    lines.push(`console errors: ${consoleErrors.length ? consoleErrors.join(" | ") : "none"}`);
    return lines;
  };

  const itemSelector = `#exception-list [data-review-id="${itemID}"]`;

  await session("Page.navigate", { url: uiUrl });
  await waitFor("the operator UI shell to boot", async () => await evaluate(`!!document.querySelector("[data-view-target=jobs]")`));

  // 1. Select the run the way an operator does: open the queue and pick it.
  await clickElement('[data-view-target="jobs"]');
  await waitFor("the queue to list the run", async () => (await evaluate(`document.querySelectorAll("[data-select-run]").length`)) > 0);
  await clickElement("[data-select-run]");
  await clickElement('[data-view-target="inspector"]');
  await sleep(200);

  // 2. The tempo review item must be projected in the exception queue.
  await clickElement('[data-inspector-tab="exceptions"]');
  const row = await waitFor("the tempo review item to be listed", async () => {
    const rect = await rectOf(itemSelector);
    return rect && rect.width > 0 && rect.height > 0 ? rect : null;
  });
  check("the tempo review item is projected in the exception queue", row.width > 0 && row.height > 0, JSON.stringify(row));
  await clickElement(itemSelector);

  // 3. Selecting it must expose BOTH waveforms, bound to the candidate's run-scoped artifacts.
  const bound = await waitFor("the tempo panel to bind the candidate", async () =>
    evaluate(`(() => {
      const panel = document.querySelector("#inspect-tempo");
      if (!panel || panel.classList.contains("hidden")) return null;
      const natural = document.querySelector("#inspect-tempo-natural");
      const transformed = document.querySelector("#inspect-tempo-transformed");
      const naturalSrc = natural?.getAttribute("src") || "";
      const transformedSrc = transformed?.getAttribute("src") || "";
      if (!naturalSrc || !transformedSrc) return null;
      return {
        naturalSrc,
        transformedSrc,
        visible: !natural.classList.contains("hidden") && !transformed.classList.contains("hidden"),
      };
    })()`),
  );

  const expectedNatural = `/api/v1/runs/${encodeURIComponent(runID)}/dub-media/${naturalSHA}`;
  const expectedTransformed = `/api/v1/runs/${encodeURIComponent(runID)}/dub-media/${transformedSHA}`;
  check("the panel binds the retained natural waveform of the run", bound.naturalSrc === expectedNatural, bound.naturalSrc);
  check("the panel binds the one eligible tempo alternative", bound.transformedSrc === expectedTransformed, bound.transformedSrc);
  check("both audition waveforms are visible to the operator", bound.visible, JSON.stringify(bound));

  // 4. The surface must stay review-only: the alternative is audible, never choosable.
  const surface = await evaluate(`(() => {
    const rows = [...document.querySelectorAll("#inspect-tempo-meta div")].map((node) => [
      node.querySelector("dt")?.textContent ?? "",
      node.querySelector("dd")?.textContent ?? "",
    ]);
    return {
      meta: Object.fromEntries(rows),
      pill: (document.querySelector("#inspect-tempo-selectable")?.textContent || "").trim(),
      note: document.querySelector("#inspect-tempo-note")?.textContent || "",
      controls: document.querySelectorAll("#inspect-tempo button, #inspect-tempo input, #inspect-tempo select, #inspect-tempo form").length,
      selectionMarks: document.querySelectorAll("[data-tempo-select], [data-tempo-approve], [data-tempo-selected], #inspect-tempo .is-selected").length,
      itemStatus: document.querySelector(${JSON.stringify(`${itemSelector} small`)})?.textContent || "",
    };
  })()`);
  const naturalMs = parseFloat(String(surface.meta.Natural || ""));
  const transformedMs = parseFloat(String(surface.meta.Tempo || ""));
  const playbackMs = parseFloat(String(surface.meta["Cửa sổ phát"] || ""));
  check("the panel reports the measured tempo factor", /^1\.\d{4}$/.test(String(surface.meta.Factor || "")), String(surface.meta.Factor));
  check("the candidate is announced as in-window", surface.pill === "trong cửa sổ phát", surface.pill);
  check(
    "the surface states the alternative is unselected and approval is the next ticket",
    surface.note.includes("chưa được chọn") && surface.note.includes("ticket kế tiếp"),
    surface.note,
  );
  check("the panel offers no control that selects, approves or publishes the alternative", surface.controls === 0, String(surface.controls));
  check("no audition element marks the candidate as selected", surface.selectionMarks === 0, String(surface.selectionMarks));
  check("the review item stays pending while it is auditioned", surface.itemStatus.includes("pending"), surface.itemStatus);

  // 5. The served bytes must be the candidate's exact artifacts, not a re-synthesis.
  const fetchBytes = async (url) =>
    evaluate(
      `fetch(${JSON.stringify(url)}).then(async (response) => {
        const buffer = await response.arrayBuffer();
        const digest = await crypto.subtle.digest("SHA-256", buffer);
        const hash = [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
        return { status: response.status, contentType: response.headers.get("content-type"), length: buffer.byteLength, hash };
      })`,
      { awaitPromise: true },
    );
  const naturalBytes = await fetchBytes(bound.naturalSrc);
  const transformedBytes = await fetchBytes(bound.transformedSrc);
  check(
    "the run-scoped endpoint serves the retained natural waveform",
    naturalBytes.status === 200 && naturalBytes.hash === naturalSHA,
    JSON.stringify(naturalBytes),
  );
  check(
    "the run-scoped endpoint serves exactly the transformed candidate artifact",
    transformedBytes.status === 200 && transformedBytes.hash === transformedSHA,
    JSON.stringify(transformedBytes),
  );
  check(
    "both audition waveforms are served as playable wav audio",
    naturalBytes.contentType === "audio/wav" && transformedBytes.contentType === "audio/wav",
    `${naturalBytes.contentType} / ${transformedBytes.contentType}`,
  );
  check(
    "the alternative is a distinct artifact from the retained waveform",
    naturalBytes.hash !== transformedBytes.hash && transformedBytes.length > 0,
    `${naturalBytes.hash} vs ${transformedBytes.hash}`,
  );

  // 6. Both must actually decode and play through the browser's media stack.
  const durationOf = async (selector, label) => {
    await evaluate(`(() => { const a = document.querySelector(${JSON.stringify(selector)}); a.muted = true; a.load(); })()`);
    return waitFor(`${label} to decode`, async () => {
      const value = await evaluate(
        `(() => { const a = document.querySelector(${JSON.stringify(selector)}); return a && a.readyState >= 2 && a.duration > 0 ? a.duration * 1000 : 0; })()`,
      );
      return value > 0 ? value : 0;
    });
  };
  const playOf = async (selector, label) => {
    const started = await evaluate(`document.querySelector(${JSON.stringify(selector)}).play().then(() => true).catch(() => false)`, {
      awaitPromise: true,
    });
    if (!started) return 0;
    return waitFor(`${label} to advance`, async () => {
      const time = await evaluate(`document.querySelector(${JSON.stringify(selector)}).currentTime`);
      return time > 0.1 ? time : 0;
    });
  };

  const naturalDecodedMs = await durationOf("#inspect-tempo-natural", "the natural waveform");
  const transformedDecodedMs = await durationOf("#inspect-tempo-transformed", "the tempo alternative");
  const naturalPlayed = await playOf("#inspect-tempo-natural", "the natural waveform");
  const transformedPlayed = await playOf("#inspect-tempo-transformed", "the tempo alternative");

  check(
    "the natural waveform decodes to the retained duration",
    Number.isFinite(naturalMs) && Math.abs(naturalDecodedMs - naturalMs) <= 30,
    `decoded=${naturalDecodedMs} reported=${naturalMs}`,
  );
  check(
    "the alternative decodes to the accepted playback window",
    Number.isFinite(playbackMs) && Math.abs(transformedDecodedMs - playbackMs) <= 30 && transformedDecodedMs < naturalDecodedMs,
    `decoded=${transformedDecodedMs} window=${playbackMs} natural=${naturalDecodedMs}`,
  );
  check(
    "the alternative is the shorter waveform for the same content",
    Number.isFinite(transformedMs) && Math.abs(transformedDecodedMs - transformedMs) <= 30,
    `decoded=${transformedDecodedMs} reported=${transformedMs}`,
  );
  check(
    "both waveforms really play in the browser",
    naturalPlayed > 0.1 && transformedPlayed > 0.1,
    `natural=${naturalPlayed}s transformed=${transformedPlayed}s`,
  );

  await sleep(300);
  check("the audition flow produces no console errors", consoleErrors.length === 0, consoleErrors.join(" | "));

  if (expectedAbsentArtifacts.length > 0) {
    console.log(`note: ${expectedAbsentArtifacts.length} optional artifact read(s) returned 404 (the UI's not-yet-written protocol)`);
  }
} catch (error) {
  failures.push(`unexpected failure: ${error?.message || String(error)}`);
  console.log(`FAIL unexpected failure — ${error?.stack || error}`);
  for (const line of await pageDiagnostics()) console.log(line);
} finally {
  try {
    cdp?.close();
  } catch {
    // Best-effort: closing an already-broken socket must not mask the result.
  }
  child.kill();
  try {
    rmSync(profileDir, { recursive: true, force: true });
  } catch {
    // The temp profile is disposable; a locked file on Windows must not fail the smoke.
  }
}

if (failures.length > 0) {
  console.log(`\n${failures.length} browser smoke failure(s):`);
  for (const failure of failures) console.log(` - ${failure}`);
  process.exit(1);
}
console.log(`\n${checks.length}/${checks.length} tempo audition browser smoke checks passed`);
