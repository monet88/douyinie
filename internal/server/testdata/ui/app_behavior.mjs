// Behavior regression coverage for the Douyinie Operator UI.
//
// Runs the real internal/server/ui/app.js inside a vm context backed by a
// minimal DOM stub, then drives the real event listeners and inspects the
// observable effects: video seek, selection state, inspector tab, editor
// content, audition request payload, and surfaced artifact-load failures.
//
// Usage: node app_behavior.mjs <path-to-app.js>

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const appPath = process.argv[2];
if (!appPath) {
  console.error("usage: node app_behavior.mjs <path-to-app.js>");
  process.exit(2);
}
const source = readFileSync(appPath, "utf8");

class ClassList {
  constructor() {
    this.items = new Set();
  }
  add(...names) {
    names.filter(Boolean).forEach((name) => this.items.add(name));
  }
  remove(...names) {
    names.forEach((name) => this.items.delete(name));
  }
  contains(name) {
    return this.items.has(name);
  }
  toggle(name, force) {
    const on = force === undefined ? !this.items.has(name) : Boolean(force);
    if (on) this.items.add(name);
    else this.items.delete(name);
    return on;
  }
}

class StubElement {
  constructor(tag = "div") {
    this.tagName = String(tag).toUpperCase();
    this.dataset = {};
    this.classList = new ClassList();
    this.style = {};
    this.children = [];
    this.listeners = new Map();
    this.innerHTML = "";
    this.textContent = "";
    this.value = "";
    this.checked = false;
    this.disabled = false;
    this.scrolledIntoView = false;
    this.currentTime = 0;
    this.duration = 0;
    this.src = "";
  }
  querySelector(selector) {
    return StubElement.lookup(selector);
  }
  querySelectorAll() {
    return [];
  }
  addEventListener(type, handler) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(handler);
  }
  dispatch(type, event) {
    for (const handler of this.listeners.get(type) || []) handler(event);
  }
  append(node) {
    this.children.push(node);
  }
  remove() {
    this.removed = true;
  }
  removeAttribute(name) {
    delete this[name];
  }
  play() {
    return Promise.resolve();
  }
  pause() {}
  scrollIntoView() {
    this.scrolledIntoView = true;
  }
}

function elementText(node) {
  let text = node.textContent || "";
  for (const child of node.children) text += ` ${elementText(child)}`;
  return text.trim();
}

function createHarness() {
  const registry = new Map();
  const lists = new Map();
  const requests = [];
  // Benign default so the real boot() sequence completes quietly; individual
  // tests override this responder.
  let respond = () => ({ status: 200, payload: {} });

  const register = (selector, init = {}) => {
    const node = new StubElement(init.tag || "div");
    Object.assign(node.dataset, init.dataset || {});
    node.classList.add(...(init.classes || []));
    if (init.value !== undefined) node.value = init.value;
    registry.set(selector, node);
    return node;
  };

  // Descendant queries resolve through the same registry so code that scopes a
  // lookup to a container (e.g. scrolling the selected row) reaches the node
  // under test.
  StubElement.lookup = (selector) => registry.get(selector) || new StubElement();

  const video = register("#preview-player", { tag: "video" });
  const toastRegion = register("#toast-region");

  const els = {
    video,
    toastRegion,
    timelineViewport: register("#timeline-viewport"),
    playhead: register("#timeline-playhead"),
    timelineTime: register("#timeline-time-display"),
    timelineRuler: register("#timeline-ruler"),
    laneDialogue: register("#lane-dialogue"),
    laneVisual: register("#lane-visual"),
    laneReview: register("#lane-review"),
    transcriptList: register("#transcript-list"),
    regionsList: register("#regions-list"),
    exceptionList: register("#exception-list"),
    speakerList: register("#speaker-list"),
    speakerBadge: register("#speaker-count-badge"),
    inspectorEditor: register("#inspector-editor", { classes: ["hidden"] }),
    inspectorEmpty: register("#inspector-empty"),
    textSegmentIndex: register("#text-segment-index"),
    textTarget: register("#text-target"),
    regionID: register("#region-id"),
    regionRole: register("#region-role"),
    regionText: register("#region-text"),
    inspectTitle: register("#inspect-title"),
    textForm: register("#text-form", { dataset: { editorPanel: "text" }, classes: ["hidden"] }),
    acceptForm: register("#accept-form", { dataset: { editorPanel: "accept" } }),
    voiceForm: register("#voice-form", { dataset: { editorPanel: "voice" }, classes: ["hidden"] }),
    regionForm: register("#region-form", { dataset: { editorPanel: "region" }, classes: ["hidden"] }),
    panelExceptions: register("#panel-exceptions", { dataset: { inspectorPanel: "exceptions" } }),
    panelTranscript: register("#panel-transcript", { dataset: { inspectorPanel: "transcript" }, classes: ["hidden"] }),
    panelRegions: register("#panel-regions", { dataset: { inspectorPanel: "regions" }, classes: ["hidden"] }),
    segmentRow1: register('[data-segment-index="1"]'),
    regionRow: register('[data-region-id="reg-1"]'),
    reviewRow: register('[data-review-id="rev-1"]'),
  };

  lists.set("[data-inspector-tab]", [
    register("[data-inspector-tab=exceptions]", { dataset: { inspectorTab: "exceptions" } }),
    register("[data-inspector-tab=transcript]", { dataset: { inspectorTab: "transcript" } }),
    register("[data-inspector-tab=regions]", { dataset: { inspectorTab: "regions" } }),
  ]);
  lists.get("[data-inspector-tab]")[0].classList.add("is-active");
  lists.set("[data-inspector-panel]", [els.panelExceptions, els.panelTranscript, els.panelRegions]);
  lists.set("[data-editor-panel]", [els.acceptForm, els.textForm, els.voiceForm, els.regionForm]);

  class Headers {
    constructor(init) {
      this.map = new Map(Object.entries(init || {}));
    }
    set(name, value) {
      this.map.set(name, value);
    }
    has(name) {
      return this.map.has(name);
    }
  }

  const fetchStub = async (url, options = {}) => {
    const body = options.body ? JSON.parse(options.body) : null;
    requests.push({ url: String(url), method: options.method || "GET", body });
    const { status, payload } = respond(String(url), options);
    return {
      ok: status >= 200 && status < 300,
      status,
      statusText: `stub ${status}`,
      text: async () => JSON.stringify(payload ?? {}),
    };
  };

  const windowStub = {
    location: { origin: "http://localhost:8080" },
    setInterval: () => 1,
    clearInterval: () => {},
    setTimeout: () => 0,
  };

  const sandbox = {
    document: {
      querySelector: (selector) => registry.get(selector) || new StubElement(),
      querySelectorAll: (selector) => lists.get(selector) || [],
      createElement: (tag) => new StubElement(tag),
      addEventListener: () => {},
    },
    window: windowStub,
    localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    fetch: fetchStub,
    Headers,
    FormData: class FormData {},
    Audio: class Audio {
      constructor(url) {
        this.src = url;
        this.currentTime = 0;
        this.listeners = new Map();
      }
      addEventListener(type, handler) {
        this.listeners.set(type, handler);
      }
      play() {
        return Promise.resolve();
      }
      pause() {}
    },
    CSS: { escape: (value) => String(value) },
    console,
  };

  const context = vm.createContext(sandbox);
  // index.html loads app.js as `type="module"`, so the real script runs in
  // strict module scope: its top-level bindings are NOT globals. Wrap it the
  // same way and re-export only the bindings the assertions drive.
  const moduleScope = [
    "(function () {",
    '"use strict";',
    source,
    "globalThis.__operatorUI = { state, loadSelectedRun, renderSpeakers, renderInspector };",
    "})();",
  ].join("\n");
  vm.runInContext(moduleScope, context, { filename: "app.js" });
  vm.runInContext("Object.assign(globalThis, globalThis.__operatorUI); delete globalThis.__operatorUI;", context);
  const evalIn = (code) => vm.runInContext(code, context);
  const click = (node, mapping) => node.dispatch("click", { target: { closest: (selector) => mapping[selector] ?? null } });
  const settle = () => new Promise((resolve) => setImmediate(resolve));
  // app.js boots itself on load; wait for that async chain and drop its
  // requests/toasts so each test only observes its own actions.
  const ready = async () => {
    await settle();
    await settle();
    requests.length = 0;
    toastRegion.children.length = 0;
  };

  return { els, requests, evalIn, click, settle, ready, setResponder: (fn) => (respond = fn) };
}

function buttonTag(html, action) {
  const marker = html.indexOf(`data-speaker-action="${action}"`);
  if (marker < 0) return "";
  const start = html.lastIndexOf("<button", marker);
  const end = html.indexOf(">", marker);
  return start < 0 || end < 0 ? "" : html.slice(start, end + 1);
}

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

const runFixture = `
state.selectedRunId = "run-1";
state.selectedRun = { id: "run-1", job_id: "job-1", status: "running", config_snapshot_json: "" };
state.selectedJob = { id: "job-1", source_asset_id: "asset-1", target_language: "vi" };
state.translation = { segments: [
  { index: 0, start_ms: 0, end_ms: 900, speaker_id: "spk_a", target_text: "Câu không" },
  { index: 1, start_ms: 1000, end_ms: 2400, speaker_id: "spk_b", target_text: "Câu một" }
] };
state.textRegionPlan = { regions: [{ id: "reg-1", start_ms: 2500, end_ms: 3000, role: "subtitle", text: "Chữ một" }] };
state.reviewItems = [{ id: "rev-1", type: "low_confidence", severity: "warning", status: "pending", start_ms: 2600, end_ms: 2700 }];
state.voiceAssignment = { assignments: { spk_a: { voice_id: "voice-1", provider_id: "fake-tts", name: "Voice 1", language: "vi" } } };
state.selectedSegmentIndex = null;
state.selectedRegionId = null;
state.selectedReviewItem = null;
renderInspector();
`;

test("transcript row click syncs seek, selection, inspector tab and editor", async () => {
  const h = createHarness();
  await h.ready();
  h.evalIn(runFixture);
  // Pre-select a different target so mutual exclusion is observable.
  h.evalIn('state.selectedRegionId = "reg-1"; state.selectedReviewItem = state.reviewItems[0];');

  // Mirrors a rendered transcript row: seek target plus segment identity.
  const row = { dataset: { transcriptSeek: "1000", segmentIndex: "1" } };
  h.click(h.els.transcriptList, { "[data-transcript-seek]": row });

  assert.equal(h.els.video.currentTime, 1, "clicking a transcript row must seek the preview video to the segment start");
  assert.equal(h.evalIn("state.selectedSegmentIndex"), 1, "segment selection must be stored");
  assert.equal(h.evalIn("state.selectedRegionId"), null, "region selection must be cleared (single active target)");
  assert.equal(h.evalIn("state.selectedReviewItem"), null, "review selection must be cleared (single active target)");
  assert.equal(h.els.panelTranscript.classList.contains("hidden"), false, "inspector must switch to the transcript tab");
  assert.equal(h.els.panelExceptions.classList.contains("hidden"), true, "exception panel must hide");
  assert.equal(h.els.panelRegions.classList.contains("hidden"), true, "region panel must hide");
  assert.equal(h.els.textSegmentIndex.value, "1", "editor must target the selected segment index");
  assert.equal(h.els.textTarget.value, "Câu một", "editor must show the selected segment text");
  assert.match(
    h.els.transcriptList.innerHTML,
    /class="transcript-row is-selected" data-transcript-seek="1000" data-segment-index="1"/,
    "the selected transcript row must render as selected"
  );
  assert.equal(h.els.segmentRow1.scrolledIntoView, true, "the selected row must be scrolled into view");
  assert.equal(h.els.textForm.classList.contains("hidden"), false, "the drawer must show the form matching the selection");
});

test("region row click syncs seek, selection, inspector tab and editor", async () => {
  const h = createHarness();
  await h.ready();
  h.evalIn(runFixture);
  h.evalIn("state.selectedSegmentIndex = 1;");

  h.click(h.els.regionsList, { "[data-region-seek]": { dataset: { regionSeek: "2500", regionId: "reg-1" } } });

  assert.equal(h.els.video.currentTime, 2.5, "clicking a region row must seek to the region start");
  assert.equal(h.evalIn("state.selectedRegionId"), "reg-1", "region selection must be stored");
  assert.equal(h.evalIn("state.selectedSegmentIndex"), null, "segment selection must be cleared (single active target)");
  assert.equal(h.els.panelRegions.classList.contains("hidden"), false, "inspector must switch to the regions tab");
  assert.equal(h.els.regionID.value, "reg-1", "editor must target the selected region");
  assert.equal(h.els.regionRole.value, "subtitle", "editor must show the selected region role");
  assert.equal(h.els.regionText.value, "Chữ một", "editor must show the selected region text");
  assert.equal(h.els.regionRow.scrolledIntoView, true, "the selected region row must be scrolled into view");
  assert.equal(h.els.regionForm.classList.contains("hidden"), false, "the drawer must show the region form for a region selection");
  assert.equal(h.els.textForm.classList.contains("hidden"), true, "stale editor forms must stay hidden");
});

test("exception click syncs seek, selection, inspector tab and review editor", async () => {
  const h = createHarness();
  await h.ready();
  h.evalIn(runFixture);
  h.evalIn("state.selectedSegmentIndex = 1;");

  h.click(h.els.exceptionList, { "[data-review-id]": { dataset: { reviewId: "rev-1" } } });

  assert.equal(h.els.video.currentTime, 2.6, "clicking an exception must seek to its start");
  assert.equal(h.evalIn("state.selectedReviewItem.id"), "rev-1", "review selection must be stored");
  assert.equal(h.evalIn("state.selectedSegmentIndex"), null, "segment selection must be cleared (single active target)");
  assert.equal(h.els.panelExceptions.classList.contains("hidden"), false, "inspector must switch to the exceptions tab");
  assert.equal(h.els.inspectTitle.textContent, "low_confidence", "editor must describe the selected exception");
});

test("row edit shortcut syncs selection and editor content", async () => {
  const h = createHarness();
  await h.ready();
  h.evalIn(runFixture);

  h.click(h.els.transcriptList, { "[data-edit-segment]": { dataset: { editSegment: "1" } } });

  assert.equal(h.evalIn("state.selectedSegmentIndex"), 1, "the edit shortcut must select the row it belongs to");
  assert.equal(h.els.textSegmentIndex.value, "1", "the edit shortcut must target that segment");
  assert.equal(h.els.textTarget.value, "Câu một", "the edit shortcut must prefill that segment's translated text");
  assert.equal(h.els.textForm.classList.contains("hidden"), false, "the edit shortcut must open the text form");

  h.click(h.els.regionsList, { "[data-edit-region]": { dataset: { editRegion: "reg-1" } } });

  assert.equal(h.evalIn("state.selectedRegionId"), "reg-1", "the region shortcut must select its region");
  assert.equal(h.evalIn("state.selectedSegmentIndex"), null, "the region shortcut must clear the previous target");
  assert.equal(h.els.regionRole.value, "subtitle", "the region shortcut must prefill the region role");
  assert.equal(h.els.regionText.value, "Chữ một", "the region shortcut must prefill the region text");
});

test("timeline block click reuses the same synchronized selection path", async () => {
  const h = createHarness();
  await h.ready();
  h.evalIn(runFixture);

  h.click(h.els.timelineViewport, { "[data-timeline-seek]": { dataset: { segIndex: "1", timelineSeek: "1000" } } });

  assert.equal(h.els.video.currentTime, 1, "timeline block click must seek");
  assert.equal(h.evalIn("state.selectedSegmentIndex"), 1, "timeline block click must select the segment");
  assert.equal(h.els.panelTranscript.classList.contains("hidden"), false, "timeline block click must switch inspector tab");
  assert.equal(h.els.textTarget.value, "Câu một", "timeline block click must sync the editor content");
});

test("contextual audition is tied to the selected segment and run", async () => {
  const h = createHarness();
  h.setResponder(() => ({
    status: 200,
    payload: { audition_result: { measured_duration_ms: 9800, is_contextual: true }, audio_data_url: "data:audio/wav;base64,AA==" },
  }));
  await h.ready();
  h.evalIn(runFixture);
  h.evalIn("state.selectedSegmentIndex = 1; renderSpeakers();");

  const tag = buttonTag(h.els.speakerList.innerHTML, "audition-contextual");
  assert.notEqual(tag, "", "speaker cards must expose a contextual audition control");
  assert.doesNotMatch(tag, /disabled/, "contextual audition must be enabled while a segment is selected");

  h.click(h.els.speakerList, {
    "[data-speaker-action]": {
      dataset: { speakerAction: "audition-contextual", speaker: "spk_a", voice: "voice-1", provider: "fake-tts", name: "Voice 1" },
    },
  });
  await h.settle();

  assert.equal(h.requests.length, 1, "contextual audition must call the existing voice-audition endpoint");
  const request = h.requests[0];
  assert.match(request.url, /\/api\/v1\/assets\/asset-1\/voice-audition$/);
  assert.equal(request.method, "POST");
  assert.equal(request.body.is_contextual, true, "contextual audition must request real BGM/SFX mixing");
  assert.equal(request.body.segment_index, 1, "contextual audition must target the selected segment, not a default");
  assert.equal(request.body.run_id, "run-1", "contextual audition must be bound to the selected run");
});

test("contextual audition stays unavailable without a selected segment", async () => {
  const h = createHarness();
  await h.ready();
  h.evalIn(runFixture);
  h.evalIn("state.selectedSegmentIndex = null; renderSpeakers();");

  const tag = buttonTag(h.els.speakerList.innerHTML, "audition-contextual");
  assert.notEqual(tag, "", "speaker cards must expose a contextual audition control");
  assert.match(tag, /disabled/, "contextual audition must be disabled until a segment is selected");

  h.click(h.els.speakerList, {
    "[data-speaker-action]": {
      dataset: { speakerAction: "audition-contextual", speaker: "spk_a", voice: "voice-1", provider: "fake-tts", name: "Voice 1" },
    },
  });
  await h.settle();
  assert.equal(h.requests.length, 0, "contextual audition must never fall back to an implicit default segment");
});

test("standalone audition stays non-contextual and segment-free", async () => {
  const h = createHarness();
  h.setResponder(() => ({
    status: 200,
    payload: { audition_result: { measured_duration_ms: 5000, is_contextual: false }, audio_data_url: "data:audio/wav;base64,AA==" },
  }));
  await h.ready();
  h.evalIn(runFixture);
  h.evalIn("state.selectedSegmentIndex = 1;");

  h.click(h.els.speakerList, {
    "[data-speaker-action]": { dataset: { speakerAction: "audition", speaker: "spk_a", voice: "voice-1", provider: "fake-tts", name: "Voice 1" } },
  });
  await h.settle();

  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].body.is_contextual, false, "standalone audition must remain standalone");
  assert.equal("segment_index" in h.requests[0].body, false, "standalone audition must not send a segment index");
});

function loadRunResponder(assetStatus) {
  return (url) => {
    const path = new URL(url, "http://localhost").pathname;
    if (path === "/api/v1/runs/run-1/stages") return { status: 200, payload: { stages: [] } };
    if (path === "/api/v1/runs/run-1/review-items") return { status: 200, payload: { review_items: [] } };
    if (path === "/api/v1/runs/run-1") return { status: 200, payload: { run: { id: "run-1", job_id: "job-1", status: "running" } } };
    if (path === "/api/v1/jobs/job-1") {
      return { status: 200, payload: { job: { id: "job-1", source_asset_id: "asset-1", target_language: "vi" } } };
    }
    if (path === "/api/v1/assets/asset-1/transcript") return assetStatus("transcript");
    if (path === "/api/v1/assets/asset-1/translation-variant") {
      return { status: 200, payload: { translation_variant: { segments: [{ index: 0, start_ms: 0, end_ms: 900, target_text: "Câu một" }] } } };
    }
    return { status: 200, payload: {} };
  };
}

test("non-404 artifact failures are surfaced while independent panels still load", async () => {
  const h = createHarness();
  h.setResponder(loadRunResponder(() => ({ status: 500, payload: { error: "transcript store unavailable" } })));
  await h.ready();
  h.evalIn(runFixture);
  h.evalIn('state.selectedRunId = "run-1"; state.jobs = []; state.translation = null; state.transcript = null;');

  await h.evalIn("loadSelectedRun()");

  assert.equal(h.evalIn("state.transcript"), null, "the failed artifact must not be invented");
  assert.notEqual(h.evalIn("state.translation"), null, "artifacts that did load must still populate their panel");
  assert.match(h.els.transcriptList.innerHTML, /Câu một/, "the translation panel must render despite the transcript failure");

  const toasts = h.els.toastRegion.children.map(elementText);
  assert.equal(toasts.length, 1, "exactly one surfaced failure notice is expected");
  assert.match(toasts[0], /transcript/, "the surfaced notice must name the failed panel");
  assert.match(toasts[0], /transcript store unavailable/, "the surfaced notice must carry the RuntimeHost error");
});

test("a missing artifact (404) stays silent and an identical failure does not spam", async () => {
  const h = createHarness();
  h.setResponder(loadRunResponder(() => ({ status: 404, payload: { error: "not found" } })));
  await h.ready();
  h.evalIn(runFixture);
  h.evalIn('state.selectedRunId = "run-1"; state.jobs = [];');
  await h.evalIn("loadSelectedRun()");
  assert.equal(h.els.toastRegion.children.length, 0, "404 means the artifact is absent, not that loading failed");

  const failing = createHarness();
  failing.setResponder(loadRunResponder(() => ({ status: 500, payload: { error: "boom" } })));
  await failing.ready();
  failing.evalIn(runFixture);
  failing.evalIn('state.selectedRunId = "run-1"; state.jobs = [];');
  await failing.evalIn("loadSelectedRun()");
  await failing.evalIn("loadSelectedRun()");
  assert.equal(failing.els.toastRegion.children.length, 1, "a persistent failure must be reported once, not on every poll");
});

let failed = 0;
for (const { name, fn } of tests) {
  try {
    await fn();
    console.log(`ok   ${name}`);
  } catch (error) {
    failed += 1;
    console.log(`FAIL ${name}`);
    console.log(`     ${error.message}`);
  }
}

console.log(`\n${tests.length - failed}/${tests.length} operator UI behavior checks passed`);
process.exit(failed === 0 ? 0 : 1);
