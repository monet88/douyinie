// Real-browser acceptance smoke for the Operator UI direct region-correction flow.
//
// The Node VM harness (app_behavior.mjs) executes app.js against a stubbed DOM, so it can
// never cover what a real client does: layout metrics for the letterboxed video, trusted
// pointer gestures through pointer capture, media loading, the run-scoped artifact reloads
// after an apply, and the browser console. This script drives a real Chromium-family browser
// over CDP against a live RuntimeHost and walks the operator path end to end:
//
//   select run → select region (seek + overlay) → drag → resize → reclassify → apply
//   → targeted rerun → the player refetches a preview rendered from the corrected plan
//
// Usage: node region_correction_browser_smoke.mjs <operator-ui-url>
// Exit codes: 0 = pass, 1 = failure, 3 = no Chromium-family browser available (caller skips).

import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const SKIP_EXIT_CODE = 3;
const FLOW_TIMEOUT_MS = 20000;

const uiUrl = process.argv[2];
if (!uiUrl) {
  console.error("usage: node region_correction_browser_smoke.mjs <operator-ui-url>");
  process.exit(1);
}

function findBrowser() {
  const candidates = [
    process.env.DOUYINIE_CHROME_BIN,
    "C:/Program Files/Google/Chrome/Application/chrome.exe",
    "C:/Program Files (x86)/Google/Chrome/Application/chrome.exe",
    "C:/Program Files/Microsoft/Edge/Application/msedge.exe",
    "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe",
    "/usr/bin/google-chrome",
    "/usr/bin/chromium",
    "/usr/bin/chromium-browser",
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  ].filter(Boolean);

  for (const candidate of candidates) {
    if (candidate.includes("/") || candidate.includes("\\")) {
      if (existsSync(candidate)) return candidate;
      continue;
    }
    const probe = spawnSync(candidate, ["--version"], { encoding: "utf8" });
    if (probe.status === 0) return candidate;
  }
  return null;
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

class CDP {
  constructor(socket) {
    this.socket = socket;
    this.nextId = 1;
    this.pending = new Map();
    this.listeners = new Map();
    socket.addEventListener("message", (event) => this.#onMessage(event.data));
    socket.addEventListener("close", () => {
      for (const { reject } of this.pending.values()) reject(new Error("CDP connection closed"));
      this.pending.clear();
    });
  }

  static async connect(url) {
    const socket = new WebSocket(url);
    await new Promise((resolve, reject) => {
      socket.addEventListener("open", resolve, { once: true });
      socket.addEventListener("error", () => reject(new Error(`cannot connect to ${url}`)), { once: true });
    });
    return new CDP(socket);
  }

  #onMessage(raw) {
    const message = JSON.parse(typeof raw === "string" ? raw : raw.toString());
    if (message.id && this.pending.has(message.id)) {
      const { resolve, reject } = this.pending.get(message.id);
      this.pending.delete(message.id);
      if (message.error) reject(new Error(`${message.error.message}${message.error.data ? `: ${message.error.data}` : ""}`));
      else resolve(message.result);
      return;
    }
    if (message.method) {
      for (const listener of this.listeners.get(message.method) || []) {
        listener(message.params, message.sessionId);
      }
    }
  }

  on(method, listener) {
    if (!this.listeners.has(method)) this.listeners.set(method, []);
    this.listeners.get(method).push(listener);
  }

  send(method, params = {}, sessionId) {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      const payload = { id, method, params };
      if (sessionId) payload.sessionId = sessionId;
      this.socket.send(JSON.stringify(payload));
    });
  }

  close() {
    this.socket.close();
  }
}

async function launchBrowser(browserPath) {
  const profileDir = mkdtempSync(join(tmpdir(), "douyinie-browser-smoke-"));
  const child = spawn(
    browserPath,
    [
      "--headless=new",
      "--remote-debugging-port=0",
      `--user-data-dir=${profileDir}`,
      "--no-first-run",
      "--no-default-browser-check",
      "--disable-extensions",
      "--disable-background-networking",
      "--disable-features=Translate,MediaRouter",
      "--autoplay-policy=no-user-gesture-required",
      "--hide-scrollbars",
      "--window-size=1440,1000",
      "about:blank",
    ],
    { stdio: ["ignore", "pipe", "pipe"] },
  );

  let endpoint = "";
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (chunk) => {
    const match = /DevTools listening on (ws:\/\/\S+)/.exec(chunk);
    if (match && !endpoint) endpoint = match[1];
  });

  const deadline = Date.now() + 20000;
  while (!endpoint && Date.now() < deadline && child.exitCode === null) {
    await sleep(50);
  }
  if (!endpoint) {
    child.kill();
    throw new Error("the browser never reported a DevTools endpoint");
  }
  return { child, endpoint, profileDir };
}

const failures = [];
const checks = [];
const expectedAbsentArtifacts = [];
const consoleErrors = [];

function check(label, condition, detail = "") {
  if (condition) {
    checks.push(label);
    console.log(`ok   ${label}`);
    return true;
  }
  failures.push(`${label}${detail ? ` (${detail})` : ""}`);
  console.log(`FAIL ${label}${detail ? ` — ${detail}` : ""}`);
  return false;
}

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
  const { targetId } = await cdp.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await cdp.send("Target.attachToTarget", { targetId, flatten: true });
  const session = (method, params = {}) => cdp.send(method, params, sessionId);

  // The console is the acceptance criterion's other half: a flow that "works" while the
  // client throws is not a clean flow.
  cdp.on("Runtime.exceptionThrown", (params, sid) => {
    if (sid !== sessionId) return;
    const details = params.exceptionDetails || {};
    consoleErrors.push(`exception: ${details.exception?.description || details.text}`);
  });
  cdp.on("Runtime.consoleAPICalled", (params, sid) => {
    if (sid !== sessionId || params.type !== "error") return;
    consoleErrors.push(`console.error: ${params.args.map((arg) => arg.value ?? arg.description ?? "").join(" ")}`);
  });
  cdp.on("Log.entryAdded", (params, sid) => {
    if (sid !== sessionId) return;
    const entry = params.entry || {};
    if (entry.level !== "error") return;
    // 404 is the UI's documented "artifact not written yet" protocol (an optional panel it
    // renders as empty); Chrome still reports the fetch in the network log. Everything else
    // — and every JS error — is a real failure.
    if (entry.source === "network" && /404/.test(entry.text)) {
      expectedAbsentArtifacts.push(entry.text);
      return;
    }
    consoleErrors.push(`log(${entry.source}): ${entry.text}`);
  });

  await session("Page.enable");
  await session("Runtime.enable");
  await session("Log.enable");

  async function evaluate(expression, { awaitPromise = false } = {}) {
    const result = await session("Runtime.evaluate", { expression, returnByValue: true, awaitPromise });
    if (result.exceptionDetails) {
      const details = result.exceptionDetails;
      throw new Error(`page evaluation failed: ${details.exception?.description || details.text}\n${expression}`);
    }
    return result.result?.value;
  }

  pageDiagnostics = async () => {
    const lines = [`page url: ${await evaluate("window.location.href").catch(() => "?")}`];
    const bodyText = await evaluate("document.body ? document.body.innerText.slice(0, 600) : ''").catch(() => "");
    lines.push(`body text: ${JSON.stringify(bodyText)}`);
    const queue = await evaluate(`fetch("/api/v1/queue").then((r) => r.status + " " + r.statusText).catch((e) => String(e))`, { awaitPromise: true }).catch(() => "?");
    lines.push(`GET /api/v1/queue: ${queue}`);
    const jobs = await evaluate(`fetch("/api/v1/jobs").then((r) => r.text()).catch((e) => String(e))`, { awaitPromise: true }).catch(() => "?");
    lines.push(`GET /api/v1/jobs: ${String(jobs).slice(0, 300)}`);
    const regionState = await evaluate(`JSON.stringify({
      regionID: document.querySelector("#region-id")?.value ?? null,
      regionError: document.querySelector("#region-error")?.textContent ?? null,
      regionResult: document.querySelector("#region-result")?.textContent ?? null,
      regionFormHidden: document.querySelector("#region-form")?.classList.contains("hidden") ?? null,
      deltas: ["#region-dx", "#region-dy", "#region-dw", "#region-dh"].map((s) => document.querySelector(s)?.value ?? null),
      role: document.querySelector("#region-role")?.value ?? null,
    })`).catch(() => "?");
    lines.push(`region editor: ${regionState}`);
    lines.push(`console errors: ${consoleErrors.length ? consoleErrors.join(" | ") : "none"}`);
    return lines;
  };

  async function waitFor(label, probe, timeoutMs = FLOW_TIMEOUT_MS) {
    const deadline = Date.now() + timeoutMs;
    let last = null;
    while (Date.now() < deadline) {
      last = await probe();
      if (last) return last;
      await sleep(100);
    }
    throw new Error(`timed out waiting for ${label} (last probe value: ${JSON.stringify(last)})`);
  }

  const rectOf = (selector) =>
    evaluate(`(() => {
      const el = document.querySelector(${JSON.stringify(selector)});
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { x: r.x, y: r.y, width: r.width, height: r.height };
    })()`);

  const textOf = (selector) =>
    evaluate(`(() => {
      const el = document.querySelector(${JSON.stringify(selector)});
      return el ? el.textContent : null;
    })()`);

  const valueOf = (selector) =>
    evaluate(`(() => {
      const el = document.querySelector(${JSON.stringify(selector)});
      return el ? el.value : null;
    })()`);

  const previewArtifactParam = () =>
    evaluate(`(() => {
      const src = document.querySelector("#preview-player")?.src || "";
      if (!src) return "";
      try { return new URL(src, window.location.href).searchParams.get("artifact") || ""; } catch { return ""; }
    })()`);

  // The run the preview panel belongs to, read from the media URL the app itself built.
  const previewRunID = () =>
    evaluate(`(() => {
      const src = document.querySelector("#preview-player")?.src || "";
      if (!src) return "";
      try { return new URL(src, window.location.href).searchParams.get("run_id") || ""; } catch { return ""; }
    })()`);

  const fetchPreview = async (runID) =>
    evaluate(
      `(async () => {
        const src = document.querySelector("#preview-player")?.src || "";
        const asset = /\\/api\\/v1\\/assets\\/([^/]+)\\/render\\/preview\\/media/.exec(src)?.[1] || "";
        if (!asset) return null;
        const response = await fetch("/api/v1/assets/" + encodeURIComponent(asset) + "/render/preview?target_language=vi&run_id=" + encodeURIComponent(${JSON.stringify(runID)}));
        if (!response.ok) return null;
        const body = await response.json();
        return body.preview_render || null;
      })()`,
      { awaitPromise: true },
    );

  async function mouse(type, x, y, extra = {}) {
    await session("Input.dispatchMouseEvent", {
      type,
      x,
      y,
      button: "left",
      buttons: type === "mouseReleased" ? 0 : 1,
      clickCount: 1,
      ...extra,
    });
  }

  // Trusted pointer gestures: pressed → several moves → released, exactly what a drag on the
  // box emits. The app listens for pointer events, which mouse input synthesizes in Blink.
  async function drag(rect, dx, dy) {
    const x = rect.x + rect.width / 2;
    const y = rect.y + rect.height / 2;
    await mouse("mouseMoved", x, y, { buttons: 0 });
    await mouse("mousePressed", x, y);
    for (let step = 1; step <= 4; step++) {
      await mouse("mouseMoved", x + (dx * step) / 4, y + (dy * step) / 4, { buttons: 1 });
      await sleep(20);
    }
    await mouse("mouseReleased", x + dx, y + dy);
    await sleep(50);
  }

  async function clickElement(selector) {
    // A real operator scrolls the target into view; without this a control below the fold
    // has viewport coordinates outside the browser window and the synthesized click hits
    // nothing.
    await evaluate(`(() => {
      const el = document.querySelector(${JSON.stringify(selector)});
      if (el) el.scrollIntoView({ block: "center", inline: "nearest" });
    })()`);
    await sleep(120);
    const rect = await rectOf(selector);
    if (!rect || rect.width === 0 || rect.height === 0) throw new Error(`${selector} is not visible`);
    const x = rect.x + rect.width / 2;
    const y = rect.y + rect.height / 2;
    await mouse("mouseMoved", x, y, { buttons: 0 });
    await mouse("mousePressed", x, y);
    await mouse("mouseReleased", x, y);
    return rect;
  }

  await session("Page.navigate", { url: uiUrl });
  await waitFor("the operator UI shell to boot", async () => (await evaluate(`!!document.querySelector("[data-view-target=jobs]")`)));

  // 1. Select the run the way an operator does: open the queue and pick it.
  await clickElement('[data-view-target="jobs"]');
  await waitFor("the queue to list the run", async () => (await evaluate(`document.querySelectorAll("[data-select-run]").length`)) > 0);
  await clickElement("[data-select-run]");
  await waitFor("the run selection to load the asset artifacts", async () => (await evaluate(`
    (() => {
      const rows = document.querySelectorAll("#regions-list [data-region-id]");
      const player = document.querySelector("#preview-player");
      return rows.length >= 2 && !!player?.src && player.src.includes("/render/preview/media");
    })()
  `)));

  // The preview must be visible in the inspector workspace before the overlay can be projected.
  await clickElement('[data-view-target="inspector"]');
  await sleep(200);

  // 2. Select the region: seek, selection, inspector tab and editor must move together.
  await clickElement('[data-inspector-tab="regions"]');
  const regionRow = await rectOf('#regions-list [data-region-id="region-smoke-drag"]');
  check("the region row is projected in the inspector", Boolean(regionRow && regionRow.height > 0), JSON.stringify(regionRow));
  await clickElement('#regions-list [data-region-id="region-smoke-drag"]');

  const seeked = await waitFor("the preview to seek to the region keyframe", async () => {
    const time = await evaluate(`document.querySelector("#preview-player")?.currentTime ?? 0`);
    return time > 0.6 ? time : 0;
  });
  check("selecting a region seeks the preview to its keyframe", seeked > 0.6, `currentTime=${seeked}`);
  check("selecting a region loads it into the region editor", (await valueOf("#region-id")) === "region-smoke-drag", `region-id=${await valueOf("#region-id")}`);

  // Keep the video centred: the overlay is projected in viewport coordinates, so the gesture
  // below must be measured against the same scroll position it is dispatched at.
  await evaluate(`document.querySelector("#video-wrapper")?.scrollIntoView({ block: "center", inline: "nearest" })`);
  await sleep(150);

  const overlayRect = await waitFor("the canonical region overlay to project", async () => {
    const rect = await rectOf("#region-box");
    const regionId = await evaluate(`document.querySelector("#region-box")?.dataset?.regionId || ""`);
    return rect && rect.width > 4 && rect.height > 4 && regionId === "region-smoke-drag" ? rect : null;
  });
  check("the overlay projects the canonical geometry over the video", overlayRect.width > 4 && overlayRect.height > 4, JSON.stringify(overlayRect));

  const playerRect = await rectOf("#preview-player");
  check(
    "the preview player has a real layout box",
    Boolean(playerRect && playerRect.width > 100 && playerRect.height > 100),
    JSON.stringify(playerRect),
  );
  check(
    "the projected overlay stays inside the player content box",
    overlayRect.x >= playerRect.x - 1 && overlayRect.y >= playerRect.y - 1 &&
      overlayRect.x + overlayRect.width <= playerRect.x + playerRect.width + 1 &&
      overlayRect.y + overlayRect.height <= playerRect.y + playerRect.height + 1,
    `overlay=${JSON.stringify(overlayRect)} player=${JSON.stringify(playerRect)}`,
  );

  // 3. Drag: display pixels must be converted into canonical deltas by the one letterbox scale.
  await drag(overlayRect, 48, 24);
  const movedDeltas = await evaluate(`({
    dx: document.querySelector("#region-dx")?.value,
    dy: document.querySelector("#region-dy")?.value,
    dw: document.querySelector("#region-dw")?.value,
    dh: document.querySelector("#region-dh")?.value,
  })`);
  check(
    "dragging the region box writes canonical move deltas",
    Number(movedDeltas.dx) >= 20 && Number(movedDeltas.dy) >= 10 && movedDeltas.dw === "0" && movedDeltas.dh === "0",
    JSON.stringify(movedDeltas),
  );

  // 4. Resize from the south-east handle: the dragged edge grows, the anchor stays.
  const handleRect = await rectOf('[data-region-handle="se"]');
  check("the resize handle is projected", Boolean(handleRect && handleRect.width > 0), JSON.stringify(handleRect));
  await drag(handleRect, 40, 40);
  const resizedDeltas = await evaluate(`({
    dx: document.querySelector("#region-dx")?.value,
    dy: document.querySelector("#region-dy")?.value,
    dw: document.querySelector("#region-dw")?.value,
    dh: document.querySelector("#region-dh")?.value,
  })`);
  check(
    "resizing from the SE handle grows the box without moving its anchor",
    Number(resizedDeltas.dw) >= 20 && Number(resizedDeltas.dh) >= 20 &&
      resizedDeltas.dx === movedDeltas.dx && resizedDeltas.dy === movedDeltas.dy,
    JSON.stringify(resizedDeltas),
  );

  // 5. Reclassify through the real select + change event.
  await evaluate(`(() => {
    const select = document.querySelector("#region-role");
    select.value = "instructional_ui_text";
    select.dispatchEvent(new Event("change", { bubbles: true }));
  })()`);
  const diff = await textOf("#region-diff");
  check("reclassifying surfaces the role transition in the diff", Boolean(diff && diff.includes("instructional_ui_text")), diff);

  const previewBefore = await previewArtifactParam();
  check("the player is bound to a preview artifact before the apply", previewBefore.length > 0, previewBefore);
  const runID = await previewRunID();
  check("the preview panel is bound to the selected run", runID.length > 0, runID);
  const previewBeforeDetail = await fetchPreview(runID).catch(() => null);

  // 6. Apply through the real form submit: one correction carrying the move, the resize and
  // the reclassification the operator just made.
  await clickElement("#region-form button[type=submit]");
  const applyResult = await waitFor("the correction result panel", async () => {
    const text = await textOf("#region-result");
    return text && text.includes("Đã áp dụng") ? text : null;
  });
  const regionError = await textOf("#region-error");
  check("the RuntimeHost accepted the correction", Boolean(applyResult), regionError || "");

  const planCASInPanel = await evaluate(`(() => {
    const text = document.querySelector("#region-result")?.textContent || "";
    const match = /RenderPlan:\\s*([0-9a-f]+)/.exec(text);
    return match ? match[1] : "";
  })()`);
  const previewCASInPanel = await evaluate(`(() => {
    const text = document.querySelector("#region-result")?.textContent || "";
    const match = /Preview render:\\s*([0-9a-f]+)/.exec(text);
    return match ? match[1] : "";
  })()`);
  check("the correction reports the preview it rendered", previewCASInPanel.length > 0, previewCASInPanel);

  // 7. The targeted rerun must land as an updated preview the player actually refetches.
  const previewAfter = await waitFor("the player to refetch the corrected preview", async () => {
    const param = await previewArtifactParam();
    return param && param !== previewBefore ? param : "";
  });
  check("the player refetches a new preview artifact after the apply", previewAfter !== previewBefore, `${previewBefore} → ${previewAfter}`);

  const loaded = await waitFor("the new preview media to load", async () => {
    const state = await evaluate(`(() => {
      const player = document.querySelector("#preview-player");
      return { readyState: player?.readyState ?? 0, duration: player?.duration ?? 0 };
    })()`);
    return state.readyState >= 1 && state.duration > 0 ? state : null;
  });
  check("the new preview media loads and reports a duration", loaded.duration > 0, JSON.stringify(loaded));

  // What the UI reloaded must be the preview the RuntimeHost serves for this run, and it must
  // have consumed the plan the correction just reported.
  const servedPreview = await fetchPreview(runID);
  check("the RuntimeHost serves the preview the player loaded", servedPreview?.cas_hash === previewAfter, `${servedPreview?.cas_hash} vs ${previewAfter}`);
  check(
    "the reloaded preview was rendered from the reported correction plan",
    Boolean(planCASInPanel) && String(servedPreview?.consumed_plan?.plan_cas_hash || "").startsWith(planCASInPanel),
    `panel=${planCASInPanel} consumed=${servedPreview?.consumed_plan?.plan_cas_hash}`,
  );
  if (previewBeforeDetail?.consumed_plan?.plan_provenance_hash) {
    check(
      "the corrected preview consumes a different plan than the pre-correction preview",
      servedPreview?.consumed_plan?.plan_provenance_hash !== previewBeforeDetail.consumed_plan.plan_provenance_hash,
      `${previewBeforeDetail.consumed_plan.plan_provenance_hash} → ${servedPreview?.consumed_plan?.plan_provenance_hash}`,
    );
  }

  // 8. The editor returns to the canonical state of the applied geometry.
  const afterApplyDeltas = await evaluate(`({
    dx: document.querySelector("#region-dx")?.value,
    dy: document.querySelector("#region-dy")?.value,
    dw: document.querySelector("#region-dw")?.value,
    dh: document.querySelector("#region-dh")?.value,
  })`);
  check(
    "the applied deltas are spent and the editor returns to canonical state",
    afterApplyDeltas.dx === "0" && afterApplyDeltas.dy === "0" && afterApplyDeltas.dw === "0" && afterApplyDeltas.dh === "0",
    JSON.stringify(afterApplyDeltas),
  );

  await sleep(300);
  check("the flow produces no console errors", consoleErrors.length === 0, consoleErrors.join(" | "));

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
console.log(`\n${checks.length}/${checks.length} region correction browser smoke checks passed`);
