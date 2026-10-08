// Shared real-browser plumbing for the Operator UI acceptance smokes.
//
// Both smokes drive a real Chromium-family browser over CDP against a live RuntimeHost, so the
// launch/attach/console/click machinery lives here once: a duplicated copy is how the two
// flows would drift apart on browser flags or console-error rules.
//
// Exit codes: 0 = pass, 1 = failure, 3 = no Chromium-family browser available (caller skips).

import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

export const SKIP_EXIT_CODE = 3;
export const FLOW_TIMEOUT_MS = 20000;

export const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

export function findBrowser() {
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

// The Operator UI reads these artifacts speculatively (app.js loadSelectedRun): a 404 means "not
// written yet" and must never fail a smoke. Every other network 404 is a real failure and stays a
// console error, so a genuinely broken endpoint cannot hide behind this protocol.
const OPTIONAL_ARTIFACT_PATHS = [
  /^\/api\/v1\/assets\/[^/]+\/transcript$/,
  /^\/api\/v1\/assets\/[^/]+\/translation-variant$/,
  /^\/api\/v1\/assets\/[^/]+\/voice-assignment$/,
  /^\/api\/v1\/assets\/[^/]+\/text-region-plan$/,
  /^\/api\/v1\/assets\/[^/]+\/render\/(preview|final)$/,
];

function isOptionalArtifactURL(rawURL) {
  if (!rawURL) return false;
  let pathname = rawURL;
  try {
    pathname = new URL(rawURL).pathname;
  } catch {
    // A relative URL keeps its raw value; the patterns still match its path.
  }
  return OPTIONAL_ARTIFACT_PATHS.some((pattern) => pattern.test(pathname));
}

export class CDP {
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
      // Every pending command is bounded: a response that never arrives must fail the flow, not
      // hang the smoke until the harness kills it. The timer is cleared on every settle path -
      // resolve, reject and socket close - so it can never fire after the promise is settled.
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`CDP ${method} timed out after ${FLOW_TIMEOUT_MS}ms`));
      }, FLOW_TIMEOUT_MS);
      const settle = (fn) => (value) => {
        clearTimeout(timer);
        fn(value);
      };
      this.pending.set(id, { resolve: settle(resolve), reject: settle(reject) });
      const payload = { id, method, params };
      if (sessionId) payload.sessionId = sessionId;
      try {
        this.socket.send(JSON.stringify(payload));
      } catch (error) {
        this.pending.delete(id);
        clearTimeout(timer);
        reject(error);
      }
    });
  }

  close() {
    this.socket.close();
  }
}

export async function launchBrowser(browserPath) {
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

export function createChecklist() {
  const failures = [];
  const checks = [];
  const check = (label, condition, detail = "") => {
    if (condition) {
      checks.push(label);
      console.log(`ok   ${label}`);
      return true;
    }
    failures.push(`${label}${detail ? ` (${detail})` : ""}`);
    console.log(`FAIL ${label}${detail ? ` — ${detail}` : ""}`);
    return false;
  };
  return { check, failures, checks };
}

// attachPage exposes the page session every flow drives: evaluation, polling waits, trusted
// clicks and the console-error accounting. Only a 404 from one of the UI's documented optional
// artifact reads counts as that read's "not written yet" protocol; every other network error,
// including an unrelated 404, stays a console error.
export async function attachPage(cdp, { consoleErrors = [], expectedAbsentArtifacts = [] } = {}) {
  const { targetId } = await cdp.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await cdp.send("Target.attachToTarget", { targetId, flatten: true });
  const session = (method, params = {}) => cdp.send(method, params, sessionId);
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
    if (entry.source === "network" && /404/.test(entry.text) && isOptionalArtifactURL(entry.url)) {
      expectedAbsentArtifacts.push(entry.url || entry.text);
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

  async function clickElement(selector) {
    // A real operator scrolls the target into view; without this a control below the fold has
    // viewport coordinates outside the browser window and the synthesized click hits nothing.
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

  return {
    session,
    sessionId,
    evaluate,
    waitFor,
    rectOf,
    textOf,
    valueOf,
    mouse,
    clickElement,
    consoleErrors,
    expectedAbsentArtifacts,
  };
}
