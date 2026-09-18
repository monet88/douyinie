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
    this.videoWidth = 0;
    this.videoHeight = 0;
    this.capturedPointers = [];
    this.rect = { left: 0, top: 0, width: 0, height: 0 };
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
    // A real element clears the reflected IDL property rather than deleting it:
    // `video.removeAttribute("src")` leaves `.src === ""`, not undefined.
    if (name === "src") this.src = "";
    else delete this[name];
  }
  setPointerCapture(pointerId) {
    this.capturedPointers.push(pointerId);
  }
  getBoundingClientRect() {
    return this.rect;
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
    videoWrapper: register("#video-wrapper"),
    regionOverlay: register("#region-overlay", { classes: ["hidden"] }),
    regionGhost: register("#region-ghost"),
    regionBox: register("#region-box"),
    regionBoxTag: register("#region-box-tag"),
    regionOverlayHint: register("#region-overlay-hint"),
    regionDiff: register("#region-diff"),
    regionError: register("#region-error", { classes: ["hidden"] }),
    regionResult: register("#region-result", { classes: ["hidden"] }),
    regionDX: register("#region-dx", { value: "0" }),
    regionDY: register("#region-dy", { value: "0" }),
    regionDW: register("#region-dw", { value: "0" }),
    regionDH: register("#region-dh", { value: "0" }),
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
    runTitle: register("#run-title"),
    runStatus: register("#run-status"),
    runDetails: register("#run-details"),
    runStageList: register("#stage-list"),
    runPause: register("[data-run-action=pause]", { dataset: { runAction: "pause" } }),
    runResume: register("[data-run-action=resume]", { dataset: { runAction: "resume" } }),
    runCancel: register("[data-run-action=cancel]", { dataset: { runAction: "cancel" } }),
  };

  lists.set("[data-inspector-tab]", [
    register("[data-inspector-tab=exceptions]", { dataset: { inspectorTab: "exceptions" } }),
    register("[data-inspector-tab=transcript]", { dataset: { inspectorTab: "transcript" } }),
    register("[data-inspector-tab=regions]", { dataset: { inspectorTab: "regions" } }),
  ]);
  lists.get("[data-inspector-tab]")[0].classList.add("is-active");
  lists.set("[data-inspector-panel]", [els.panelExceptions, els.panelTranscript, els.panelRegions]);
  lists.set("[data-editor-panel]", [els.acceptForm, els.textForm, els.voiceForm, els.regionForm]);
  lists.set("[data-run-action]", [els.runPause, els.runResume, els.runCancel]);

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
    "globalThis.__operatorUI = { state, loadSelectedRun, renderSpeakers, renderInspector, renderSelectedRun };",
    "})();",
  ].join("\n");
  vm.runInContext(moduleScope, context, { filename: "app.js" });
  vm.runInContext("Object.assign(globalThis, globalThis.__operatorUI); delete globalThis.__operatorUI;", context);
  const evalIn = (code) => vm.runInContext(code, context);
  const click = (node, mapping) => node.dispatch("click", { target: { closest: (selector) => mapping[selector] ?? null } });
  // Pointer gesture on a container: `closest` mirrors what a real event target
  // resolves to (e.g. a resize handle inside #region-box).
  const point = (node, type, { clientX = 0, clientY = 0, closest = {}, pointerId = 1, button = 0, buttons = 1 } = {}) =>
    node.dispatch(type, {
      clientX,
      clientY,
      pointerId,
      button,
      buttons,
      preventDefault: () => {},
      target: { closest: (selector) => closest[selector] ?? null },
    });
  const submit = (form) =>
    form.dispatch("submit", {
      preventDefault: () => {},
      currentTarget: { querySelector: (selector) => (selector === "button[type=submit]" ? { textContent: "apply", dataset: {} } : null) },
    });
  const settle = () => new Promise((resolve) => setImmediate(resolve));
  // app.js boots itself on load; wait for that async chain and drop its
  // requests/toasts so each test only observes its own actions.
  const ready = async () => {
    await settle();
    await settle();
    requests.length = 0;
    toastRegion.children.length = 0;
  };

  return { els, requests, evalIn, click, point, submit, settle, ready, setResponder: (fn) => (respond = fn) };
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

test("an interrupted run offers resume and refuses pause/cancel", async () => {
  const h = createHarness();
  await h.ready();
  h.evalIn(`state.selectedRunId = "run-1"; state.selectedRun = { id: "run-1", job_id: "job-1", status: "interrupted", config_snapshot_json: "" }; state.selectedJob = { id: "job-1", source_asset_id: "asset-1", target_language: "vi" };`);
  h.evalIn("renderSelectedRun();");
  // The RuntimeHost accepts POST /runs/{id}/resume for an interrupted run and re-drains the
  // queue from the incomplete stage, so a console that greys Resume out leaves a real operator
  // with no way to recover a fail-closed run.
  assert.equal(h.els.runResume.disabled, false, "resume must be offered for an interrupted run");
  assert.equal(h.els.runPause.disabled, true, "an interrupted run has nothing to pause");
  assert.equal(h.els.runCancel.disabled, true, "an interrupted run is already stopped");
});

test("a running run offers pause/cancel and not resume", async () => {
  const h = createHarness();
  await h.ready();
  h.evalIn(`state.selectedRunId = "run-1"; state.selectedRun = { id: "run-1", job_id: "job-1", status: "running", config_snapshot_json: "" }; state.selectedJob = { id: "job-1", source_asset_id: "asset-1", target_language: "vi" };`);
  h.evalIn("renderSelectedRun();");
  assert.equal(h.els.runResume.disabled, true, "a running run must not be resumable");
  assert.equal(h.els.runPause.disabled, false, "a running run can be paused");
  assert.equal(h.els.runCancel.disabled, false, "a running run can be cancelled");
});

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

// Canonical TextRegionPlan geometry: 1080x1920 media, tracked box at
// canonical (100,200,300,80) from 1000-3000ms and (120,210,300,80) at 2000ms.
// The rendered player is 800x960, so the video is pillarboxed: scale is exactly
// 0.5 with a 130px horizontal letterbox offset that must be honoured.
const regionOverlayFixture = `
state.selectedRunId = "run-1";
state.selectedRun = { id: "run-1", job_id: "job-1", status: "running", config_snapshot_json: "" };
state.selectedJob = { id: "job-1", source_asset_id: "asset-1", target_language: "vi" };
state.preview = { cas_hash: "preview-cas" };
state.translation = null;
state.transcript = null;
state.voiceAssignment = null;
state.textRegionPlan = {
  frame_width: 1080,
  frame_height: 1920,
  regions: [
    { id: "reg-box", role: "semantic_text", text: "Bấm để tải", first_seen_ms: 1000, last_seen_ms: 3000,
      keyframes: [
        { frame_index: 0, timestamp_ms: 1000, box: { x: 100, y: 200, width: 300, height: 80 }, observed: true },
        { frame_index: 30, timestamp_ms: 2000, box: { x: 120, y: 210, width: 300, height: 80 }, observed: true }
      ] },
    { id: "reg-none", role: "uncertain", text: "Không keyframe", first_seen_ms: 5000, last_seen_ms: 6000, keyframes: [] }
  ]
};
state.reviewItems = [{ id: "rev-region", type: "uncertain_role", severity: "warning", status: "pending", region_id: "reg-box", start_ms: 1000, end_ms: 3000 }];
state.selectedSegmentIndex = null;
state.selectedRegionId = null;
state.selectedReviewItem = null;
renderInspector();
`;

// Rendered player (800x960) inside the same-sized wrapper: 1080x1920 media is
// pillarboxed to a 540x960 content rect at x=130.
// Only the region override calls, so a submit test observes its own action and
// not the artifact reloads that follow a successful apply.
function overrideRequests(h) {
  return h.requests.filter((request) => request.url.includes("/inspector/override-region"));
}

// Drives the real playhead path: the preview player emits timeupdate, exactly as
// it does during playback.
function playTo(h, timeMs) {
  h.els.video.currentTime = timeMs / 1000;
  h.els.video.dispatch("timeupdate", {});
}

// Selects a region through the real list click path (seek + selection + editor).
function selectRegionRow(h, regionId, seekMs) {
  h.click(h.els.regionsList, { "[data-region-seek]": { dataset: { regionSeek: seekMs, regionId } } });
}

function stageRegionOverlay(h) {
  h.els.video.rect = { left: 0, top: 0, width: 800, height: 960 };
  h.els.videoWrapper.rect = { left: 0, top: 0, width: 800, height: 960 };
}

test("selecting a visual review item seeks and projects its canonical geometry", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);

  h.click(h.els.exceptionList, { "[data-review-id]": { dataset: { reviewId: "rev-region" } } });

  assert.equal(h.els.video.currentTime, 1, "selecting the review item must seek to its timestamp");
  assert.equal(h.els.regionOverlay.classList.contains("hidden"), false, "the tracked region must be projected over the video");
  assert.equal(h.els.regionBox.dataset.regionId, "reg-box", "the overlay must project the review item's own region");
  // The letterbox origin lives on the overlay; the box is canonical px * scale.
  assert.equal(h.els.regionOverlay.style.left, "130px", "the overlay must be placed on the letterboxed content rect");
  assert.equal(h.els.regionOverlay.style.top, "0px", "the overlay must be placed on the letterboxed content rect");
  assert.equal(h.els.regionOverlay.style.width, "540px", "the overlay must be sized to the rendered media, not the element");
  assert.equal(h.els.regionOverlay.style.height, "960px", "the overlay must be sized to the rendered media, not the element");
  // canonical (100,200,300,80) at scale 0.5.
  assert.equal(h.els.regionBox.style.left, "50px", "canonical x must be scaled into the overlay");
  assert.equal(h.els.regionBox.style.top, "100px", "canonical y must be scaled into the overlay");
  assert.equal(h.els.regionBox.style.width, "150px", "canonical width must be scaled");
  assert.equal(h.els.regionBox.style.height, "40px", "canonical height must be scaled");
  assert.equal(h.els.regionGhost.classList.contains("hidden"), true, "no pending edit means no before-state ghost");

  // Opening the region editor for that same projected region must show the
  // canonical before-state the operator is about to correct.
  h.click(h.els.regionsList, { "[data-edit-region]": { dataset: { editRegion: "reg-box" } } });
  assert.equal(h.els.regionDiff.classList.contains("hidden"), false, "the inspector must show the canonical before state");
  assert.match(h.els.regionDiff.innerHTML, /x=100 y=200 w=300 h=80/, "the before state must be the canonical geometry");
  assert.match(h.els.regionDiff.innerHTML, /semantic_text/, "the before state must include the current role");
});

test("the projected geometry follows the playhead keyframe", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);
  selectRegionRow(h, "reg-box", "1000");
  playTo(h, 2500);

  // Keyframe at 2000ms is canonical (120,210) => (60,105) at scale 0.5.
  assert.equal(h.els.regionBox.style.left, "60px", "the overlay must use the keyframe at the playhead");
  assert.equal(h.els.regionBox.style.top, "105px", "the overlay must use the keyframe at the playhead");
});

test("dragging stores canonical deltas, never display pixels", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);
  selectRegionRow(h, "reg-box", "1000");

  const onBox = { "#region-box": h.els.regionBox };
  h.point(h.els.videoWrapper, "pointerdown", { clientX: 180, clientY: 100, closest: onBox });
  h.point(h.els.videoWrapper, "pointermove", { clientX: 205, clientY: 110, closest: onBox });
  h.point(h.els.videoWrapper, "pointerup", { clientX: 205, clientY: 110, closest: onBox });

  // +25 rendered px / 0.5 scale = +50 canonical px (not +25).
  assert.equal(h.els.regionDX.value, "50", "drag must convert rendered pixels to canonical media pixels");
  assert.equal(h.els.regionDY.value, "20", "drag must convert rendered pixels to canonical media pixels");
  assert.equal(h.els.regionDW.value, "0", "a move must not change the box size");
  assert.equal(h.els.regionDH.value, "0", "a move must not change the box size");
  // canonical (150,220) at scale 0.5 inside the 130px pillarbox.
  assert.equal(h.els.regionBox.style.left, "75px", "the box must render the pending (after) geometry");
  assert.equal(h.els.regionGhost.classList.contains("hidden"), false, "a pending edit must show the before-state ghost");
  assert.match(h.els.regionDiff.innerHTML, /Δx \+50/, "the inspector must show the applied delta");
});

test("dragging a handle resizes from the dragged edge in canonical space", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);
  selectRegionRow(h, "reg-box", "1000");

  const onHandle = { "[data-region-handle]": { dataset: { regionHandle: "se" } }, "#region-box": h.els.regionBox };
  h.point(h.els.videoWrapper, "pointerdown", { clientX: 330, clientY: 140, closest: onHandle });
  h.point(h.els.videoWrapper, "pointermove", { clientX: 355, clientY: 150, closest: onHandle });
  h.point(h.els.videoWrapper, "pointerup", { clientX: 355, clientY: 150, closest: onHandle });

  assert.equal(h.els.regionDX.value, "0", "a south-east resize must not move the box origin");
  assert.equal(h.els.regionDW.value, "50", "resize must convert rendered pixels to canonical width");
  assert.equal(h.els.regionDH.value, "20", "resize must convert rendered pixels to canonical height");
});

test("a drag beyond the frame is bounded visibly and submits the bounded geometry", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);
  h.setResponder(() => ({
    status: 200,
    payload: { result: { status: "auto_resolved", message: "ok", localized_visual_track_cas: "v", localized_subtitle_cas: "s", render_plan_cas: "r" } },
  }));
  selectRegionRow(h, "reg-box", "1000");

  const onBox = { "#region-box": h.els.regionBox };
  h.point(h.els.videoWrapper, "pointerdown", { clientX: 180, clientY: 100, closest: onBox });
  h.point(h.els.videoWrapper, "pointermove", { clientX: 3000, clientY: 100, closest: onBox });
  // The bound is surfaced while the operator is still dragging, so the gesture is
  // never silently absorbed.
  assert.equal(h.els.regionBox.classList.contains("is-clamped"), true, "the bound must be visible, not silent");
  assert.match(h.els.regionOverlayHint.textContent, /biên video/, "the bound must be explained to the operator");
  h.point(h.els.videoWrapper, "pointerup", { clientX: 3000, clientY: 100, closest: onBox });

  // Keyframes union is canonical (100,200)-(420,290), so the largest legal shift
  // in a 1080px frame is 1080 - 320 - 100 = 660 - and the same bound keeps the
  // second keyframe inside the frame, which the RuntimeHost would otherwise reject.
  assert.equal(h.els.regionDX.value, "660", "an out-of-frame drag must be bounded to the canonical frame");
  assert.match(h.els.regionDiff.innerHTML, /Δx \+660/, "the committed delta must be the bounded canonical delta");

  h.submit(h.els.regionForm);
  await h.settle();

  const applied = overrideRequests(h);
  assert.equal(applied.length, 1, "bounded geometry must still be applicable");
  assert.equal(applied[0].body.overrides[0].box_delta_x, 660, "only canonical deltas reach RuntimeHost");
  assert.equal(h.els.regionError.classList.contains("hidden"), true, "bounded geometry is valid, not an error");
});

test("an out-of-frame typed delta fails closed with an actionable inspector error", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);
  selectRegionRow(h, "reg-box", "1000");

  h.els.regionDX.value = "-500";
  h.submit(h.els.regionForm);
  await h.settle();

  assert.equal(h.requests.length, 0, "invalid geometry must never be sent to RuntimeHost");
  assert.equal(h.els.regionError.classList.contains("hidden"), false, "the refusal must be surfaced in the Inspector");
  assert.match(h.els.regionError.textContent, /fail-closed/, "the error must name the fail-closed refusal");
  assert.match(h.els.regionError.textContent, /1080×1920/, "the error must name the violated frame bounds");
  assert.match(h.els.regionError.textContent, /reg-box|keyframe/, "the error must be actionable for the edited region");
});

test("a role-only edit on a small canonical region is accepted, not refused by a UI-only minimum", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(`
state.selectedRunId = "run-1";
state.selectedRun = { id: "run-1", job_id: "job-1", status: "running", config_snapshot_json: "" };
state.selectedJob = { id: "job-1", source_asset_id: "asset-1", target_language: "vi" };
state.preview = { cas_hash: "preview-cas" };
state.textRegionPlan = {
  frame_width: 1080,
  frame_height: 1920,
  regions: [
    { id: "reg-tiny", role: "uncertain", text: "i", first_seen_ms: 1000, last_seen_ms: 3000,
      keyframes: [
        { frame_index: 0, timestamp_ms: 1000, box: { x: 10, y: 10, width: 40, height: 4 }, observed: true }
      ] }
  ]
};
state.reviewItems = [];
state.selectedSegmentIndex = null;
state.selectedRegionId = null;
state.selectedReviewItem = null;
renderInspector();
`);
  h.setResponder(() => ({
    status: 200,
    payload: {
      result: { status: "auto_resolved", message: "ok", localized_visual_track_cas: "v", localized_subtitle_cas: "s", render_plan_cas: "r" },
    },
  }));
  selectRegionRow(h, "reg-tiny", "1000");
  h.els.regionRole.value = "semantic_text";
  h.submit(h.els.regionForm);
  await h.settle();

  // 4 canonical px tall is legal: the runtime floor is 1px, so a stricter UI-only
  // floor refused an edit the backend accepts.
  assert.equal(overrideRequests(h).length, 1, "a role-only edit on a 4px-tall region must reach RuntimeHost");
  assert.equal(h.els.regionError.classList.contains("hidden"), true, "the accepted edit must not be refused as too small");
});

test("a RuntimeHost region rejection is surfaced and keeps the pending edit", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);
  h.setResponder((url) => {
    if (String(url).includes("/inspector/override-region")) {
      return { status: 422, payload: { error: "subtitle or overlay overlaps protected region" } };
    }
    return loadRunResponder(() => ({ status: 404, payload: { error: "not found" } }))(url);
  });
  selectRegionRow(h, "reg-box", "1000");

  h.els.regionDX.value = "50";
  // The rejection path legitimately logs the RuntimeHost failure; the harness
  // asserts on the surfaced outcome instead of the console trace.
  const realConsoleError = console.error;
  console.error = () => {};
  try {
    h.submit(h.els.regionForm);
    await h.settle();
  } finally {
    console.error = realConsoleError;
  }

  assert.equal(overrideRequests(h).length, 1, "the edit must be attempted through the RuntimeHost override flow");
  assert.equal(h.els.regionError.classList.contains("hidden"), false, "the rejection must reach the Inspector");
  assert.match(h.els.regionError.textContent, /protected region/, "the Inspector error must carry the RuntimeHost reason");
  assert.equal(h.els.regionDX.value, "50", "a rejected edit must stay pending so the operator can correct it");
  assert.equal(h.els.regionResult.classList.contains("hidden"), true, "no success outcome may be shown for a rejected edit");
});

test("applying an edit posts canonical deltas to the run override flow and reports descendants", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);
  h.setResponder((url) => {
    if (String(url).includes("/inspector/override-region")) {
      return {
        status: 200,
        payload: {
          result: {
            status: "auto_resolved",
            message: "Region geometry/classification updated and visual track regenerated (auto-resolved)",
            localized_visual_track_cas: "vis-cas-1",
            localized_subtitle_cas: "sub-cas-1",
            render_plan_cas: "plan-cas-1",
          },
        },
      };
    }
    return loadRunResponder(() => ({ status: 404, payload: { error: "not found" } }))(url);
  });
  selectRegionRow(h, "reg-box", "1000");
  h.els.regionRole.value = "instructional_ui_text";

  h.els.regionDX.value = "50";
  h.els.regionDY.value = "20";
  h.submit(h.els.regionForm);
  await h.settle();

  const applied = overrideRequests(h);
  assert.equal(applied.length, 1, "apply must use the existing run-scoped region override flow");
  const request = applied[0];
  assert.match(request.url, /\/api\/v1\/runs\/run-1\/inspector\/override-region$/);
  assert.equal(request.method, "POST");
  assert.equal(request.body.run_id, "run-1");
  assert.equal(request.body.asset_id, "asset-1");
  assert.equal(request.body.target_language, "vi");
  assert.equal(request.body.operator, "local-operator");
  const override = request.body.overrides[0];
  assert.equal(override.region_id, "reg-box");
  assert.equal(override.box_delta_x, 50, "canonical deltas must be posted, not display pixels");
  assert.equal(override.box_delta_y, 20);
  assert.equal(override.new_role, "instructional_ui_text", "reclassification must ride along with the geometry edit");
  assert.equal(request.body.overrides.length, 1, "only the edited region may be targeted");

  const result = h.els.regionResult;
  assert.equal(result.classList.contains("hidden"), false, "the apply outcome must be confirmed in the Inspector");
  assert.match(result.innerHTML, /auto-resolved/, "the outcome must state the resolution status");
  assert.match(result.innerHTML, /LocalizedVisualTrack: vis-cas-1/, "the outcome must name the visual descendant that reran");
  assert.match(result.innerHTML, /RenderPlan: plan-cas-1/, "the outcome must name the render descendant that reran");
  assert.equal(h.els.regionDX.value, "0", "the applied deltas are spent once the canonical plan reloads");
});

test("a west-edge resize past the frame stops at the bound and keeps the east edge", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);
  selectRegionRow(h, "reg-box", "1000");

  // Keyframe union is canonical (100,200)-(420,290): the east edge is 420.
  const onHandle = { "[data-region-handle]": { dataset: { regionHandle: "w" } }, "#region-box": h.els.regionBox };
  h.point(h.els.videoWrapper, "pointerdown", { clientX: 50, clientY: 100, closest: onHandle });
  h.point(h.els.videoWrapper, "pointermove", { clientX: -5000, clientY: 100, closest: onHandle });
  // The bound is surfaced while the operator is still dragging, so the clamped
  // edge is never silently absorbed.
  assert.equal(h.els.regionBox.classList.contains("is-clamped"), true, "hitting the frame bound must be visible");
  assert.match(h.els.regionOverlayHint.textContent, /biên video/, "hitting the frame bound must be explained");
  h.point(h.els.videoWrapper, "pointerup", { clientX: -5000, clientY: 100, closest: onHandle });

  // Left edge reaches 0 => dx -100; the dragged edge must stop there, so the
  // width grows by exactly that much and the anchored east edge stays at 420
  // (420 - 0 width, not a box grown rightward to the 1080 frame bound).
  assert.equal(h.els.regionDX.value, "-100", "a west-edge resize must stop the dragged edge at the frame");
  assert.equal(h.els.regionDW.value, "100", "the width must take up exactly the clamped movement");
  assert.match(h.els.regionDiff.innerHTML, /x=100 y=200 w=300 h=80/, "the before state stays canonical");

  h.submit(h.els.regionForm);
  await h.settle();
  const applied = overrideRequests(h);
  assert.equal(applied.length, 1, "a bounded resize must be applicable");
  assert.equal(applied[0].body.overrides[0].box_delta_x, -100, "the clamped delta is what RuntimeHost receives");
  assert.equal(applied[0].body.overrides[0].box_delta_w, 100, "the clamped resize is what RuntimeHost receives");
});

test("a second pointer cannot hijack the active gesture and a missed release ends it", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);
  selectRegionRow(h, "reg-box", "1000");

  const onBox = { "#region-box": h.els.regionBox };
  h.point(h.els.videoWrapper, "pointerdown", { clientX: 180, clientY: 100, closest: onBox, pointerId: 1 });
  h.point(h.els.videoWrapper, "pointermove", { clientX: 205, clientY: 100, closest: onBox, pointerId: 1 });
  // A second finger touching the same region must not restart the drag.
  h.point(h.els.videoWrapper, "pointerdown", { clientX: 600, clientY: 600, closest: onBox, pointerId: 2 });
  h.point(h.els.videoWrapper, "pointermove", { clientX: 700, clientY: 700, closest: onBox, pointerId: 2 });
  assert.equal(h.els.regionDX.value, "0", "a foreign pointer must not move the box before the gesture ends");
  h.point(h.els.videoWrapper, "pointerup", { clientX: 700, clientY: 700, closest: onBox, pointerId: 2 });
  assert.equal(h.els.regionDX.value, "0", "a foreign pointerup must not commit the gesture");
  h.point(h.els.videoWrapper, "pointerup", { clientX: 205, clientY: 100, closest: onBox, pointerId: 1 });
  // +25 rendered px / 0.5 scale, from the owning pointer only.
  assert.equal(h.els.regionDX.value, "50", "the owning pointer must still commit its own gesture");

  // A move with no button held means the release was missed: the gesture ends at
  // the last position tracked under a press instead of following the bare cursor.
  h.point(h.els.videoWrapper, "pointerdown", { clientX: 200, clientY: 100, closest: onBox, pointerId: 1 });
  h.point(h.els.videoWrapper, "pointermove", { clientX: 400, clientY: 100, closest: onBox, pointerId: 1, buttons: 0 });
  assert.equal(h.els.regionDX.value, "50", "a missed release must not invent movement");
  h.point(h.els.videoWrapper, "pointermove", { clientX: 700, clientY: 400, closest: onBox, pointerId: 1, buttons: 0 });
  assert.equal(h.els.regionDX.value, "50", "a released pointer must not keep dragging on hover");

  // The next press starts a fresh gesture that accumulates onto the pending edit
  // (+50 canonical already in the form, +50 rendered = +100 canonical more).
  h.point(h.els.videoWrapper, "pointerdown", { clientX: 200, clientY: 100, closest: onBox, pointerId: 1 });
  h.point(h.els.videoWrapper, "pointermove", { clientX: 250, clientY: 100, closest: onBox, pointerId: 1 });
  h.point(h.els.videoWrapper, "pointerup", { clientX: 250, clientY: 100, closest: onBox, pointerId: 1 });
  assert.equal(h.els.regionDX.value, "150", "a fresh gesture must accumulate onto the pending edit");
});

test("a region without keyframes projects nothing instead of inventing geometry", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);

  selectRegionRow(h, "reg-none", "5000");

  assert.equal(h.els.regionOverlay.classList.contains("hidden"), true, "absent canonical geometry must not be invented");
  assert.equal(h.els.regionBox.dataset.regionId, "", "no region may stay armed for direct manipulation");
});

test("the overlay follows the playhead when no region is selected", async () => {
  const h = createHarness();
  await h.ready();
  stageRegionOverlay(h);
  h.evalIn(regionOverlayFixture);

  playTo(h, 1500);
  assert.equal(h.els.regionOverlay.classList.contains("hidden"), false, "the region live under the playhead must be projected");
  assert.equal(h.els.regionBox.dataset.regionId, "reg-box", "the projected region must be the one active at the playhead");

  playTo(h, 5500);
  assert.equal(h.els.regionOverlay.classList.contains("hidden"), true, "a keyframe-less active region must degrade to no overlay");

  playTo(h, 1500);
  assert.equal(h.els.regionOverlay.classList.contains("hidden"), false, "returning to a tracked region must restore the projection");
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
