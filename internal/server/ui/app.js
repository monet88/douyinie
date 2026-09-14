const state = {
  sourceMode: "douyin_url",
  view: "new",
  health: null,
  jobs: [],
  queue: [],
  selectedRunId: localStorage.getItem("douyinie:selectedRun") || "",
  selectedRun: null,
  selectedJob: null,
  stages: [],
  reviewItems: [],
  selectedReviewItem: null,
  transcript: null,
  translation: null,
  voiceAssignment: null,
  textRegionPlan: null,
  preview: null,
  final: null,
  handoff: null,
  auditionAudio: null,
  timelineDurationMs: 0,
  inspectorTab: "exceptions",
  selectedSegmentIndex: null,
  selectedRegionId: null,
};

let pollTimer = null;

const titles = {
  new: "Tạo localization job",
  jobs: "Jobs & Queue",
  inspector: "Review Workspace",
  result: "Kết quả",
};

const SPEAKER_COLORS = [
  "#4f8cff", // Blue
  "#2dd4bf", // Teal
  "#fbbf24", // Amber
  "#c084fc", // Purple
  "#fb7185", // Rose
  "#34d399", // Emerald
  "#f472b6", // Pink
  "#38bdf8", // Sky
];

function speakerColor(speakerId) {
  if (!speakerId) return "#8e99a4";
  let hash = 0;
  for (let i = 0; i < speakerId.length; i++) {
    hash = (hash * 31 + speakerId.charCodeAt(i)) & 0xffffffff;
  }
  return SPEAKER_COLORS[Math.abs(hash) % SPEAKER_COLORS.length];
}

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

async function api(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body && !(options.body instanceof FormData) && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  const response = await fetch(path, { ...options, headers });
  const text = await response.text();
  let data = {};
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { message: text };
    }
  }
  if (!response.ok) {
    const error = new Error(data.error || data.message || `${response.status} ${response.statusText}`);
    error.status = response.status;
    throw error;
  }
  return data;
}

async function apiOptional(path) {
  try {
    return await api(path);
  } catch (error) {
    if (error.status === 404) return null;
    throw error;
  }
}

function jsonBody(value) {
  return JSON.stringify(value);
}

function shortID(value, length = 10) {
  if (!value) return "—";
  const text = String(value);
  return text.length > length ? `${text.slice(0, length)}…` : text;
}

function formatDate(value) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString("vi-VN");
}

function formatMs(ms) {
  if (ms == null || isNaN(ms) || ms < 0) return "00:00.0";
  const totalSeconds = ms / 1000;
  const mins = Math.floor(totalSeconds / 60);
  const secs = (totalSeconds % 60).toFixed(1);
  return `${String(mins).padStart(2, "0")}:${secs < 10 ? "0" : ""}${secs}`;
}

function operatorName() {
  return $("#operator-input")?.value?.trim() || "local-operator";
}

function esc(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

function statusClass(value) {
  return String(value || "muted").toLowerCase().replaceAll(" ", "_");
}

function emptyReviewQueueState() {
  const runStatus = String(state.selectedRun?.status || "").toLowerCase();
  if (runStatus === "completed") {
    return {
      badgeClass: "pass",
      badgeText: "0 pending",
      title: "Review queue sạch",
      message: "Không có exception nào cần xử lý. Run đã hoàn tất.",
      completed: true,
    };
  }

  const failedStage = [...state.stages]
    .reverse()
    .find((stage) => ["failed", "interrupted"].includes(String(stage.status || "").toLowerCase()));
  const stageSuffix = failedStage?.stage ? ` tại ${failedStage.stage}` : "";

  if (runStatus === "interrupted") {
    return {
      badgeClass: "interrupted",
      badgeText: "0 pending · interrupted",
      title: "Run bị gián đoạn",
      message: `Không có exception pending, nhưng run đã interrupted${stageSuffix}. Chưa thể xem là PASS.`,
      completed: false,
    };
  }
  if (runStatus === "cancelled") {
    return {
      badgeClass: "cancelled",
      badgeText: "0 pending · cancelled",
      title: "Run đã huỷ",
      message: "Không có exception pending, nhưng run đã bị huỷ trước khi hoàn tất.",
      completed: false,
    };
  }
  if (runStatus === "paused") {
    return {
      badgeClass: "paused",
      badgeText: "0 pending · paused",
      title: "Run đang tạm dừng",
      message: "Không có exception pending ở thời điểm này. Quality gates chưa hoàn tất.",
      completed: false,
    };
  }
  if (runStatus === "queued" || runStatus === "running") {
    return {
      badgeClass: runStatus,
      badgeText: `0 pending · ${runStatus}`,
      title: "Run chưa hoàn tất",
      message: `Run đang ${runStatus}; chưa có exception pending không đồng nghĩa với PASS.`,
      completed: false,
    };
  }
  return {
    badgeClass: "muted",
    badgeText: "0 pending",
    title: "Chưa có review pending",
    message: "Chưa có đủ trạng thái run để kết luận quality gates đã hoàn tất.",
    completed: false,
  };
}

function toast(title, message = "", type = "") {
  const region = $("#toast-region");
  if (!region) return;
  const item = document.createElement("div");
  item.className = `toast ${type}`.trim();
  const strong = document.createElement("strong");
  strong.textContent = title;
  item.append(strong);
  if (message) {
    const span = document.createElement("span");
    span.textContent = message;
    item.append(span);
  }
  region.append(item);
  window.setTimeout(() => item.remove(), 5200);
}

function setBusy(button, busy, label) {
  if (!button) return;
  if (busy) {
    button.dataset.originalLabel = button.textContent;
    button.disabled = true;
    button.textContent = label || "Đang xử lý…";
  } else {
    button.disabled = false;
    if (button.dataset.originalLabel) button.textContent = button.dataset.originalLabel;
  }
}

function navigate(view) {
  state.view = view;
  $$("[data-view]").forEach((node) => node.classList.toggle("is-active", node.dataset.view === view));
  $$("[data-view-target]").forEach((node) => node.classList.toggle("is-active", node.dataset.viewTarget === view));
  const titleNode = $("#page-title");
  if (titleNode) titleNode.textContent = titles[view] || "Douyinie Operator";
  if (view === "inspector") renderInspector();
  if (view === "result") refreshResult().catch(showError);
}

function getRunPosture(run) {
  if (!run?.config_snapshot_json) return "auto";
  try {
    const cfg = JSON.parse(run.config_snapshot_json);
    return cfg.posture || cfg.review_posture || "auto";
  } catch {
    return "auto";
  }
}

function renderRuntime() {
  const node = $("#runtime-state");
  if (!node) return;
  node.classList.remove("is-ok", "is-error");
  const status = $("span:last-child", node);
  if (state.health?.status === "ok") {
    node.classList.add("is-ok");
    status.textContent = `${state.health.version || "runtime"} · online`;
  } else {
    node.classList.add("is-error");
    status.textContent = "Không kết nối được";
  }
}

function renderRunContext() {
  const run = state.selectedRun;
  const job = state.selectedJob;
  const posture = getRunPosture(run);

  const topbarId = $("#topbar-run-id");
  if (topbarId) {
    topbarId.textContent = run ? shortID(run.id, 16) : "Chưa chọn";
    topbarId.title = run?.id || "";
  }
  const topbarStatus = $("#topbar-run-status");
  if (topbarStatus) {
    topbarStatus.className = `status-pill ${statusClass(run?.status)}`;
    topbarStatus.textContent = run?.status || "—";
  }
  const topbarTarget = $("#topbar-run-target");
  if (topbarTarget) {
    topbarTarget.textContent = (job?.target_language || "—").toUpperCase();
  }
  const topbarPosture = $("#topbar-run-posture");
  if (topbarPosture) {
    topbarPosture.textContent = posture === "review" ? "Review mode" : "Auto mode";
  }

  const inspectorRunPill = $("#inspector-run-pill");
  if (inspectorRunPill) {
    inspectorRunPill.className = `status-pill ${statusClass(run?.status)}`;
    inspectorRunPill.textContent = run ? `${run.status} · ${posture.toUpperCase()}` : "Chưa chọn run";
  }
}

function renderMetrics() {
  const jobsNode = $("#metric-jobs");
  if (jobsNode) jobsNode.textContent = state.jobs.length;
  const queueNode = $("#metric-queue");
  if (queueNode) queueNode.textContent = state.queue.length;
  const reviewNode = $("#metric-review");
  if (reviewNode) reviewNode.textContent = state.reviewItems.filter((item) => item.status === "pending").length;
}

function computeStepState(stepKey) {
  if (!state.selectedRun) return "waiting";

  const stepOrder = ["source", "analyze", "translate", "dub", "export"];
  const thisIdx = stepOrder.indexOf(stepKey);

  // 1. Source step is always completed once source_asset_id is present
  if (stepKey === "source") {
    return state.selectedJob?.source_asset_id ? "completed" : "waiting";
  }

  const stageDefs = {
    source: [],
    analyze: ["audio_role_plan", "speech_understand", "text_detection"],
    translate: ["translation", "dub_script", "visual_text_localize"],
    dub: ["voice_assignment", "dub_synthesize", "audio_mix"],
    export: ["render_plan", "render_preview", "final_render_handoff", "render_final"],
  };

  const stepStages = stageDefs[stepKey] || [];
  const runStages = state.stages.filter((st) => stepStages.includes(st.stage));
  const runStatus = state.selectedRun.status;

  // 2. Check if this step is already completed
  let isStepCompleted = false;
  if (stepKey === "export") {
    const finalStage = state.stages.find((st) => st.stage === "render_final");
    if (state.final || finalStage?.status === "succeeded" || finalStage?.status === "completed") {
      isStepCompleted = true;
    }
  } else {
    // Check if any downstream step has run or is running
    const downstreamHasRun = state.stages.some((st) => {
      for (let i = thisIdx + 1; i < stepOrder.length; i++) {
        if (stageDefs[stepOrder[i]].includes(st.stage)) return true;
      }
      return false;
    });
    if (downstreamHasRun) {
      isStepCompleted = true;
    } else {
      const succeededCount = runStages.filter((st) => st.status === "succeeded").length;
      if (succeededCount > 0 && succeededCount === stepStages.length) {
        isStepCompleted = true;
      }
    }
  }

  if (isStepCompleted) {
    return "completed";
  }

  // 3. Export requires review if pending review items exist
  if (stepKey === "export") {
    if (runStatus === "paused") return "paused";
    if (runStatus === "cancelled") return "cancelled";
    if (runStatus === "interrupted") return "interrupted";
    if (state.reviewItems.some((item) => item.status === "pending")) {
      return "review_required";
    }
    const reviewHandoff = state.stages.find((st) => st.stage === "final_render_handoff");
    if (
      getRunPosture(state.selectedRun) === "review" &&
      !state.final &&
      (reviewHandoff?.status === "succeeded" || reviewHandoff?.status === "completed")
    ) {
      return "review_required";
    }
    if (state.stages.some((st) => st.stage === "render_preview" && st.status === "succeeded")) {
      if (runStatus === "queued") return "queued";
      if (runStatus === "running") return "running";
      return "waiting";
    }
  }

  // 4. Check for failures / interruptions in this step
  if (runStages.some((st) => st.status === "failed" || st.status === "interrupted")) {
    return "interrupted";
  }

  // 5. Check if stages in this step are actively running
  const hasRunningStage = runStages.some((st) => st.status === "running");

  if (hasRunningStage) {
    if (runStatus === "paused") return "paused";
    if (runStatus === "cancelled") return "cancelled";
    if (runStatus === "interrupted") return "interrupted";
    return "running";
  }

  // 6. Check if all prior steps are completed
  const allPriorCompleted = stepOrder.slice(0, thisIdx).every((prevStep) => {
    if (prevStep === "source") return !!state.selectedJob?.source_asset_id;
    const prevDef = stageDefs[prevStep];
    const prevStages = state.stages.filter((st) => prevDef.includes(st.stage));
    const downstreamOfPrev = state.stages.some((st) => {
      for (let i = stepOrder.indexOf(prevStep) + 1; i < stepOrder.length; i++) {
        if (stageDefs[stepOrder[i]].includes(st.stage)) return true;
      }
      return false;
    });
    return downstreamOfPrev || (prevStages.length === prevDef.length && prevStages.every((st) => st.status === "succeeded"));
  });

  if (!allPriorCompleted) {
    return "waiting";
  }

  // Earliest incomplete step matches current run lifecycle
  if (runStatus === "queued") return "queued";
  if (runStatus === "paused") return "paused";
  if (runStatus === "cancelled") return "cancelled";
  if (runStatus === "interrupted") return "interrupted";
  if (runStatus === "running") return "running";

  return runStages.length > 0 ? "running" : "waiting";
}

function renderStepper() {
  const stepper = $("#workflow-stepper");
  if (!stepper) return;
  const steps = ["source", "analyze", "translate", "dub", "export"];
  steps.forEach((step) => {
    const node = $(`[data-step="${step}"]`, stepper);
    if (!node) return;
    const st = computeStepState(step);
    node.className = `stepper-step status-${st}`;
  });
}

function renderQueue() {
  const body = $("#queue-body");
  if (!body) return;
  const summary = $("#queue-summary");
  if (summary) summary.textContent = state.queue.length ? `${state.queue.length} run` : "Không có run";

  // Update active run banner
  const banner = $("#active-run-banner");
  const activeEntry = state.queue.find((e) => e.status === "running");
  const nextEntry = !activeEntry ? state.queue.find((e) => e.status === "queued" && e.position === 1) : null;
  if (banner) {
    if (activeEntry) {
      banner.classList.add("has-active");
      $(".banner-body strong", banner).textContent = `Slot active: Run ${shortID(activeEntry.run_id, 14)} (Job ${shortID(activeEntry.job_id, 10)})`;
      $(".banner-body span", banner).textContent = `Trạng thái: ${activeEntry.status.toUpperCase()} · Vị trí ${activeEntry.position}. Single active run invariant được giữ trên toàn RuntimeHost.`;
    } else if (nextEntry) {
      banner.classList.remove("has-active");
      $(".banner-body strong", banner).textContent = `Run kế tiếp: ${shortID(nextEntry.run_id, 14)} (Job ${shortID(nextEntry.job_id, 10)})`;
      $(".banner-body span", banner).textContent = `Trạng thái: QUEUED · Vị trí ${nextEntry.position}. Chưa có run nào chiếm active slot.`;
    } else {
      banner.classList.remove("has-active");
      $(".banner-body strong", banner).textContent = "RuntimeHost Phase 1 xử lý tuần tự 1 active run đồng thời (active_run_slots=1).";
      $(".banner-body span", banner).textContent = "Tài nguyên GPU/VRAM được bảo toàn cho các media stage (ASR, alignment, TTS, mix). Các run trong queue tự động thực thi khi slot trống.";
    }
  }

  if (!state.queue.length) {
    body.innerHTML = '<tr><td colspan="6" class="empty-cell">Chưa có dữ liệu queue.</td></tr>';
    return;
  }

  body.innerHTML = state.queue
    .map((entry) => {
      const isSelected = entry.run_id === state.selectedRunId;
      const isActiveSlot = entry.status === "running";
      const isNext = !activeEntry && entry.status === "queued" && entry.position === 1;
      const job = state.jobs.find((j) => j.id === entry.job_id);
      const target = (job?.target_language || "vi").toUpperCase();

      const slotBadge = isActiveSlot
        ? '<span class="slot-badge active-slot">ACTIVE SLOT</span>'
        : isNext
          ? '<span class="slot-badge queued-slot">NEXT</span>'
          : `<span class="slot-badge queued-slot">#${entry.position}</span>`;

      return `
      <tr class="${isSelected ? "is-selected-row" : ""}">
        <td class="mono" title="${esc(entry.run_id)}">${esc(shortID(entry.run_id, 12))}</td>
        <td class="mono" title="${esc(entry.job_id)}">${esc(shortID(entry.job_id, 10))}</td>
        <td>${slotBadge}</td>
        <td><strong>${esc(target)}</strong></td>
        <td><span class="status-pill ${esc(statusClass(entry.status))}">${esc(entry.status)}</span></td>
        <td>
          <div class="table-actions">
            <button class="inline-select" type="button" data-select-run="${esc(entry.run_id)}">Mở</button>
            ${entry.status === "running" || entry.status === "queued" ? `<button class="inline-action" type="button" data-inline-action="pause" data-inline-run="${esc(entry.run_id)}">Pause</button>` : ""}
            ${entry.status === "paused" ? `<button class="inline-action" type="button" data-inline-action="resume" data-inline-run="${esc(entry.run_id)}">Resume</button>` : ""}
            ${entry.status === "running" || entry.status === "queued" || entry.status === "paused" ? `<button class="inline-danger" type="button" data-inline-action="cancel" data-inline-run="${esc(entry.run_id)}">Cancel</button>` : ""}
          </div>
        </td>
      </tr>`;
    })
    .join("");
}

function renderJobs() {
  const root = $("#job-cards");
  if (!root) return;
  if (!state.jobs.length) {
    root.innerHTML = '<div class="empty-state"><strong>Chưa có job.</strong><span>Tạo job đầu tiên ở màn hình Job mới.</span></div>';
    return;
  }
  root.innerHTML = state.jobs
    .map(
      (job) => `
    <article class="job-card">
      <div class="job-card-top"><strong>${esc(shortID(job.id, 14))}</strong><span class="status-pill ${esc(statusClass(job.status))}">${esc(job.status || "pending")}</span></div>
      <dl>
        <div><dt>Target</dt><dd>${esc((job.target_language || "—").toUpperCase())}</dd></div>
        <div><dt>Asset</dt><dd title="${esc(job.source_asset_id)}">${esc(shortID(job.source_asset_id, 14))}</dd></div>
        <div><dt>Created</dt><dd>${esc(formatDate(job.created_at))}</dd></div>
      </dl>
    </article>`
    )
    .join("");
}

function renderSelectedRun() {
  const run = state.selectedRun;
  const job = state.selectedJob;
  const posture = getRunPosture(run);

  const titleNode = $("#run-title");
  if (titleNode) titleNode.textContent = run ? shortID(run.id, 18) : "Chưa chọn run";
  const pill = $("#run-status");
  if (pill) {
    pill.className = `status-pill ${statusClass(run?.status)}`;
    pill.textContent = run?.status || "—";
  }
  const details = $("#run-details");
  if (details) {
    details.innerHTML = `
      <div><dt>Job</dt><dd title="${esc(job?.id)}">${esc(shortID(job?.id, 16))}</dd></div>
      <div><dt>Target</dt><dd>${esc((job?.target_language || "—").toUpperCase())}</dd></div>
      <div><dt>Posture</dt><dd>${esc(posture === "review" ? "Review" : "Auto")}</dd></div>
      <div><dt>Created</dt><dd>${esc(formatDate(run?.created_at))}</dd></div>`;
  }

  const allowed = {
    pause: ["queued", "running"],
    resume: ["paused"],
    cancel: ["queued", "running", "paused"],
  };
  $$("[data-run-action]").forEach((button) => {
    button.disabled = !run || !allowed[button.dataset.runAction].includes(run.status);
  });

  renderStages();
  renderRunContext();
  renderStepper();
}

function renderStages() {
  const root = $("#stage-list");
  if (!root) return;
  if (!state.selectedRun) {
    root.innerHTML = '<div class="empty-state"><strong>Chưa có run được chọn.</strong><span>Chọn một run từ bảng bên trái.</span></div>';
    return;
  }
  if (!state.stages.length) {
    root.innerHTML = '<div class="empty-state"><strong>Chưa có stage execution.</strong><span>Run đang chờ worker hoặc chưa bắt đầu.</span></div>';
    return;
  }
  root.innerHTML = state.stages
    .map(
      (stage) => `
    <div class="stage-row">
      <strong>${esc(stage.stage)}</strong>
      <span class="status-pill ${esc(statusClass(stage.status))}">${esc(stage.status)}</span>
      ${stage.error_message ? `<small>${esc(stage.error_message)}</small>` : ""}
    </div>`
    )
    .join("");
}

function renderZoneContext() {
  const run = state.selectedRun;
  const job = state.selectedJob;
  const posture = getRunPosture(run);

  const assetIdEl = $("#context-asset-id");
  if (assetIdEl) {
    assetIdEl.textContent = job ? shortID(job.source_asset_id, 14) : "—";
    assetIdEl.title = job?.source_asset_id || "";
  }
  const runIdEl = $("#context-run-id");
  if (runIdEl) {
    runIdEl.textContent = run ? shortID(run.id, 14) : "—";
    runIdEl.title = run?.id || "";
  }
  const targetEl = $("#context-target-lang");
  if (targetEl) targetEl.textContent = (job?.target_language || "—").toUpperCase();
  const postureEl = $("#context-posture");
  if (postureEl) {
    postureEl.className = `status-pill ${posture === "review" ? "warning" : "pass"}`;
    postureEl.textContent = posture === "review" ? "Review mode" : "Auto mode";
  }

  // Calculate duration
  const durEl = $("#context-duration");
  if (durEl) {
    durEl.textContent = state.timelineDurationMs > 0 ? formatMs(state.timelineDurationMs) : "—";
  }

  renderSpeakers();
}

function renderSpeakers() {
  const list = $("#speaker-list");
  const badge = $("#speaker-count-badge");
  if (!list) return;

  const speakersMap = new Map();

  // 1. From persisted voice assignments (truth from RuntimeHost)
  if (state.voiceAssignment?.assignments) {
    for (const [spkId, profile] of Object.entries(state.voiceAssignment.assignments)) {
      const hasRealProfile = profile && (profile.voice_id || profile.id) && profile.provider_id;
      speakersMap.set(spkId, {
        id: spkId,
        assigned: !!hasRealProfile,
        name: profile?.name || profile?.id || "Chưa gán voice",
        voiceId: profile?.voice_id || profile?.id || "",
        providerId: profile?.provider_id || "",
        language: profile?.language || state.selectedJob?.target_language || "vi",
      });
    }
  }

  // 2. From transcript speech blocks and translation segments
  const allBlocks = [
    ...(state.transcript?.speech_blocks || []),
    ...(state.translation?.segments || []),
  ];
  for (const block of allBlocks) {
    if (block.speaker_id && !speakersMap.has(block.speaker_id)) {
      speakersMap.set(block.speaker_id, {
        id: block.speaker_id,
        assigned: false,
        name: "Chưa gán voice",
        voiceId: "",
        providerId: "",
        language: state.selectedJob?.target_language || "vi",
      });
    }
  }

  const speakers = [...speakersMap.values()];
  if (badge) badge.textContent = `${speakers.length} speaker`;

  // Contextual audition reads the real translated segment + preserved stems of
  // the selected target, so it stays unavailable until an operator picks one.
  const contextualSegmentIndex = Number.isInteger(state.selectedSegmentIndex) ? state.selectedSegmentIndex : null;

  if (!speakers.length) {
    list.innerHTML = '<div class="empty-state"><span>Chưa có speaker nào được phát hiện trong run này.</span></div>';
    return;
  }

  list.innerHTML = speakers
    .map((spk) => {
      const color = speakerColor(spk.id);
      const isAssigned = spk.assigned && spk.voiceId && spk.providerId;
      const metaHtml = isAssigned
        ? `${esc(spk.name)} · <span class="mono">${esc(spk.voiceId)}</span>`
        : `<span class="unassigned-badge">Chưa gán voice</span>`;

      return `
      <div class="speaker-card ${isAssigned ? "" : "is-unassigned"}" data-speaker-id="${esc(spk.id)}">
        <div class="speaker-card-top">
          <div class="speaker-avatar" style="--spk-color: ${color};" aria-hidden="true">${esc(spk.id.replace("spk_", "S"))}</div>
          <div class="speaker-meta">
            <strong>${esc(spk.id)}</strong>
            <small>${metaHtml}</small>
          </div>
        </div>
        <div class="speaker-actions">
          <button class="speaker-action-btn" type="button"
                  data-speaker-action="reassign"
                  data-speaker="${esc(spk.id)}"
                  data-voice="${esc(spk.voiceId)}"
                  data-provider="${esc(spk.providerId)}"
                  data-name="${esc(isAssigned ? spk.name : "")}">
            ${isAssigned ? "Đổi voice" : "Gán voice"}
          </button>
          <button class="speaker-action-btn secondary" type="button"
                  data-speaker-action="audition"
                  data-speaker="${esc(spk.id)}"
                  data-voice="${esc(spk.voiceId)}"
                  data-provider="${esc(spk.providerId)}"
                  data-name="${esc(isAssigned ? spk.name : "")}"
                  ${isAssigned ? "" : "disabled title=\"Chưa có voice profile để nghe thử\""}>
            Nghe thử
          </button>
          <button class="speaker-action-btn secondary" type="button"
                  data-speaker-action="audition-contextual"
                  data-speaker="${esc(spk.id)}"
                  data-voice="${esc(spk.voiceId)}"
                  data-provider="${esc(spk.providerId)}"
                  data-name="${esc(isAssigned ? spk.name : "")}"
                  ${isAssigned && contextualSegmentIndex != null ? `title="Audition ~10s cùng BGM/SFX thật của segment ${contextualSegmentIndex}"` : "disabled title=\"Chọn một segment trong timeline hoặc tab 'Transcript & Dịch' để nghe thử trong ngữ cảnh run\""}>
            Nghe + BGM
          </button>
        </div>
      </div>`;
    })
    .join("");
}

function renderMediaURL(kind) {
  if (!state.selectedJob) return "";
  const asset = encodeURIComponent(state.selectedJob.source_asset_id);
  const target = encodeURIComponent(state.selectedJob.target_language || "vi");
  const runParam = state.selectedRun ? `&run_id=${encodeURIComponent(state.selectedRun.id)}` : "";
  // The media route always serves the run's *latest* artifact for this kind, so the
  // URL alone cannot tell the player that a correction replaced the preview it is
  // still showing. Binding the artifact identity into the URL is what makes the
  // player refetch instead of keeping the previous geometry on screen.
  const artifact = kind === "preview" ? state.preview : state.final;
  const versionParam = artifact?.cas_hash ? `&artifact=${encodeURIComponent(artifact.cas_hash)}` : "";
  return `/api/v1/assets/${asset}/render/${kind}/media?target_language=${target}${runParam}${versionParam}`;
}

function renderZoneCenter() {
  const player = $("#preview-player");
  const placeholder = $("#video-overlay-placeholder");
  const placeholderText = $("#preview-placeholder-text");

  if (player && placeholder) {
    if (state.preview) {
      const mediaUrl = renderMediaURL("preview");
      if (player.src !== window.location.origin + mediaUrl && !player.src.endsWith(mediaUrl)) {
        player.src = mediaUrl;
      }
      placeholder.classList.add("hidden");
      player.classList.remove("hidden");
    } else {
      player.removeAttribute("src");
      player.classList.add("hidden");
      placeholder.classList.remove("hidden");
      if (placeholderText) {
        placeholderText.textContent = state.selectedRun
          ? `Run ${state.selectedRun.status} · Chờ preview render hoàn tất.`
          : "Chọn một run để xem preview.";
      }
    }
  }

  renderObservationalTimeline();
  // The timeline short-circuits when no duration is known, so the overlay is
  // refreshed explicitly to stay correct when the preview media disappears.
  renderRegionOverlay();
}

function calculateTotalDuration() {
  const player = $("#preview-player");
  let dur = 0;
  if (state.preview && player && player.duration && !isNaN(player.duration) && isFinite(player.duration) && player.duration > 0) {
    dur = player.duration * 1000;
  }
  if (!dur) {
    const timedItems = [
      ...(state.translation?.segments || []),
      ...(state.transcript?.speech_blocks || []),
      ...(state.textRegionPlan?.regions || []),
      ...(state.reviewItems || []),
    ];
    for (const item of timedItems) {
      const endMs = Number(item.end_ms ?? item.last_seen_ms);
      const startMs = Number(item.start_ms ?? item.first_seen_ms);
      if (Number.isFinite(endMs) && endMs > dur) dur = endMs;
      if (Number.isFinite(startMs) && startMs > dur) dur = startMs;
    }
  }
  state.timelineDurationMs = dur;
  return dur;
}

function renderObservationalTimeline() {
  const totalMs = calculateTotalDuration();
  const timeDisplay = $("#timeline-time-display");
  const player = $("#preview-player");
  const curMs = player && player.currentTime ? player.currentTime * 1000 : 0;
  if (timeDisplay) {
    timeDisplay.textContent = totalMs > 0 ? `${formatMs(curMs)} / ${formatMs(totalMs)}` : `${formatMs(curMs)} / —`;
  }

  const ruler = $("#timeline-ruler");
  const laneDialogue = $("#lane-dialogue");
  const laneVisual = $("#lane-visual");
  const laneReview = $("#lane-review");
  if (totalMs <= 0) {
    if (ruler) ruler.innerHTML = "";
    if (laneDialogue) laneDialogue.innerHTML = '<span class="lane-empty">Chờ persisted speech timing từ RuntimeHost</span>';
    if (laneVisual) laneVisual.innerHTML = '<span class="lane-empty">Chờ persisted text-region timing từ RuntimeHost</span>';
    if (laneReview) laneReview.innerHTML = '<span class="lane-empty">Chưa có persisted review timing</span>';
    const playhead = $("#timeline-playhead");
    if (playhead) playhead.style.left = "0%";
    return;
  }

  // Ruler ticks
  if (ruler) {
    const tickIntervalSec = totalMs > 60000 ? 10 : 5;
    const tickCount = Math.floor(totalMs / (tickIntervalSec * 1000));
    let rulerHtml = "";
    for (let i = 0; i <= tickCount; i++) {
      const sec = i * tickIntervalSec;
      const pct = (sec * 1000 / totalMs) * 100;
      if (pct <= 100) {
        rulerHtml += `<div class="ruler-tick" style="left: ${pct}%;"><span>${sec}s</span></div>`;
      }
    }
    ruler.innerHTML = rulerHtml;
  }

  // Dialogue track
  if (laneDialogue) {
    const segments = state.translation?.segments || state.transcript?.speech_blocks || [];
    if (!segments.length) {
      laneDialogue.innerHTML = '<span class="lane-empty">Chưa có speech segments</span>';
    } else {
      laneDialogue.innerHTML = segments
        .map((seg, idx) => {
          const startMs = Number.isFinite(Number(seg.start_ms)) ? Number(seg.start_ms) : 0;
          const persistedEndMs = Number(seg.end_ms);
          const endMs = Number.isFinite(persistedEndMs) && persistedEndMs >= startMs ? persistedEndMs : startMs;
          const leftPct = Math.max(0, Math.min(100, (startMs / totalMs) * 100));
          const widthPct = Math.min(100 - leftPct, Math.max(0.75, ((endMs - startMs) / totalMs) * 100));
          const spk = seg.speaker_id || "unassigned";
          const color = speakerColor(spk);
          const text = seg.target_text || seg.source_text || `Seg #${idx}`;
          const isSelected = state.selectedSegmentIndex === (seg.index ?? idx);

          return `
          <div class="timeline-block speech-block ${isSelected ? "is-selected" : ""}"
               data-timeline-seek="${startMs}"
               data-start-ms="${startMs}"
               data-end-ms="${endMs}"
               data-seg-index="${seg.index ?? idx}"
               style="left: ${leftPct}%; width: ${widthPct}%; --speaker-color: ${color};"
               title="[${esc(spk)}: ${formatMs(startMs)}–${formatMs(endMs)}] ${esc(text)}">
            <span class="block-label">${esc(spk)}: ${esc(text)}</span>
          </div>`;
        })
        .join("");
    }
  }

  // Visual text track
  if (laneVisual) {
    const regions = state.textRegionPlan?.regions || [];
    if (!regions.length) {
      laneVisual.innerHTML = '<span class="lane-empty">Chưa có text regions</span>';
    } else {
      laneVisual.innerHTML = regions
        .map((reg) => {
          const startMs = Number.isFinite(Number(reg.start_ms)) ? Number(reg.start_ms) : Number.isFinite(Number(reg.first_seen_ms)) ? Number(reg.first_seen_ms) : 0;
          const rawEndMs = Number(reg.end_ms);
          const persistedEndMs = Number.isFinite(rawEndMs) ? rawEndMs : Number(reg.last_seen_ms);
          const endMs = Number.isFinite(persistedEndMs) && persistedEndMs >= startMs ? persistedEndMs : startMs;
          const leftPct = Math.max(0, Math.min(100, (startMs / totalMs) * 100));
          const widthPct = Math.min(100 - leftPct, Math.max(0.75, ((endMs - startMs) / totalMs) * 100));
          const isSelected = state.selectedRegionId === reg.id;

          return `
          <div class="timeline-block visual-block ${isSelected ? "is-selected" : ""}"
               data-timeline-seek="${startMs}"
               data-start-ms="${startMs}"
               data-end-ms="${endMs}"
               data-region-id="${esc(reg.id)}"
               style="left: ${leftPct}%; width: ${widthPct}%;"
               title="[${esc(reg.role || "region")}: ${formatMs(startMs)}–${formatMs(endMs)}] ${esc(reg.text || reg.id)}">
            <span class="block-label">${esc(reg.role || "ocr")}</span>
          </div>`;
        })
        .join("");
    }
  }

  // Review markers track
  if (laneReview) {
    const items = state.reviewItems || [];
    if (!items.length) {
      laneReview.innerHTML = '<span class="lane-empty">Không có review markers</span>';
    } else {
      laneReview.innerHTML = items
        .map((item) => {
          const startMs = Number.isFinite(Number(item.start_ms)) ? Number(item.start_ms) : 0;
          const persistedEndMs = Number(item.end_ms);
          const endMs = Number.isFinite(persistedEndMs) && persistedEndMs >= startMs ? persistedEndMs : startMs;
          const leftPct = Math.max(0, Math.min(99.5, (startMs / totalMs) * 100));
          const isSelected = state.selectedReviewItem?.id === item.id;
          const isWarning = item.severity === "warning";

          return `
          <div class="timeline-marker ${isWarning ? "is-warning" : "is-blocker"} ${isSelected ? "is-selected" : ""}"
               data-timeline-seek="${startMs}"
               data-start-ms="${startMs}"
               data-end-ms="${endMs}"
               data-review-marker="${esc(item.id)}"
               style="left: ${leftPct}%;"
               title="[${esc(item.severity)}] ${esc(item.type || item.reason || "Exception")}">
            <span class="marker-pin">${isWarning ? "!" : "✕"}</span>
          </div>`;
        })
        .join("");
    }
  }

  updatePlayhead();
}

let lastActiveSegIndex = null;
let lastActiveRegionId = null;
let lastActiveReviewId = null;
let lastOptionalReadFailureKey = "";

function speechSegments() {
  return state.translation?.segments || state.transcript?.speech_blocks || [];
}

function segmentByIndex(segmentIndex) {
  const segments = speechSegments();
  const pos = segments.findIndex((seg, idx) => (seg.index ?? idx) === segmentIndex);
  return pos < 0 ? null : segments[pos];
}

function regionById(regionId) {
  return (state.textRegionPlan?.regions || []).find((region) => region.id === regionId) || null;
}

function startMsOf(value) {
  // TextRegionPlan regions carry source-truth timing as first_seen_ms/last_seen_ms;
  // speech segments and review items carry start_ms/end_ms.
  const startMs = Number(value?.start_ms ?? value?.first_seen_ms);
  return Number.isFinite(startMs) ? startMs : null;
}

function inspectorTabForSelection() {
  if (state.selectedSegmentIndex != null) return "transcript";
  if (state.selectedRegionId) return "regions";
  return "exceptions";
}

function scrollSelectionIntoView(tab) {
  let node = null;
  if (tab === "transcript" && state.selectedSegmentIndex != null) {
    node = $(`[data-segment-index="${state.selectedSegmentIndex}"]`, $("#transcript-list"));
  } else if (tab === "regions" && state.selectedRegionId) {
    node = $(`[data-region-id="${CSS.escape(state.selectedRegionId)}"]`, $("#regions-list"));
  } else if (tab === "exceptions" && state.selectedReviewItem) {
    node = $(`[data-review-id="${CSS.escape(state.selectedReviewItem.id)}"]`, $("#exception-list"));
  }
  if (node) node.scrollIntoView({ behavior: "smooth", block: "nearest" });
}

// An open editor drawer must always describe the current selection, never the
// previous target. Prefill is intentionally driven by user selection only, so
// background polling cannot overwrite in-progress operator input.
function syncEditorToSelection() {
  const editorBox = $("#inspector-editor");
  if (!editorBox || editorBox.classList.contains("hidden")) return;

  if (state.selectedSegmentIndex != null) {
    const segment = segmentByIndex(state.selectedSegmentIndex);
    const indexInput = $("#text-segment-index");
    if (indexInput) indexInput.value = String(state.selectedSegmentIndex);
    const targetInput = $("#text-target");
    if (targetInput) targetInput.value = segment?.target_text || "";
  }
  if (state.selectedRegionId) {
    const region = regionById(state.selectedRegionId);
    const idInput = $("#region-id");
    if (idInput) idInput.value = state.selectedRegionId;
    const roleInput = $("#region-role");
    if (roleInput && region?.role) roleInput.value = region.role;
    const textInput = $("#region-text");
    if (textInput && region?.text) textInput.value = region.text;
    // A pending rectangle edit belongs to exactly one region: switching the
    // active region must not carry the previous region's deltas onto it.
    if (lastSyncedRegionId !== state.selectedRegionId) {
      lastSyncedRegionId = state.selectedRegionId;
      resetRegionDeltaInputs();
      setRegionError("");
      renderRegionApplyResult(null);
    }
  }
}

// --- Direct manipulation of TextRegionPlan geometry over the video ----------
//
// Single source of truth: the existing Inspector region form. Pointer gestures
// write canonical-pixel deltas into the same Δ fields the form already submits,
// so the overlay never becomes a second geometry model. Canonical media
// coordinates are the only persisted truth; display pixels are derived.

let lastSyncedRegionId = null;
let regionDrag = null;

// The smallest canonical box the backend accepts: domain.MinTextRegionBoxPx, the floor behind
// ApplyRegionOverrides' minimum-dimension clamp. A stricter UI-only floor would reject edits the
// runtime accepts (a role-only edit on a small region) and disagree with the operator's own drag.
const REGION_MIN_CANONICAL_PX = 1;

function currentPlayheadMs() {
  const player = $("#preview-player");
  const seconds = Number(player?.currentTime);
  return Number.isFinite(seconds) && seconds > 0 ? seconds * 1000 : 0;
}

// Intrinsic media size of the canonical TextRegionPlan, falling back to the
// decoded preview only when the plan does not carry frame dimensions.
function regionFrameSize() {
  const plan = state.textRegionPlan;
  const player = $("#preview-player");
  return {
    frameWidth: Number(plan?.frame_width) || Number(player?.videoWidth) || 0,
    frameHeight: Number(plan?.frame_height) || Number(player?.videoHeight) || 0,
  };
}

// Maps the rendered video content rect (object-fit: contain, so letterboxed) into
// coordinates relative to the overlay host. One scale factor converts canonical
// media pixels to rendered pixels and back, so letterbox offsets are structural
// rather than duplicated per box.
function computeRegionViewport(playerRect, hostRect, frameWidth, frameHeight) {
  const fw = Number(frameWidth) || 0;
  const fh = Number(frameHeight) || 0;
  const pw = Number(playerRect?.width) || 0;
  const ph = Number(playerRect?.height) || 0;
  if (!(fw > 0) || !(fh > 0) || !(pw > 0) || !(ph > 0) || !hostRect) return null;
  const scale = Math.min(pw / fw, ph / fh);
  const width = fw * scale;
  const height = fh * scale;
  return {
    frameWidth: fw,
    frameHeight: fh,
    scale,
    width,
    height,
    left: playerRect.left + (pw - width) / 2 - hostRect.left,
    top: playerRect.top + (ph - height) / 2 - hostRect.top,
  };
}

function regionViewport() {
  const player = $("#preview-player");
  const host = $("#video-wrapper");
  if (!player || !host || typeof player.getBoundingClientRect !== "function" || typeof host.getBoundingClientRect !== "function") {
    return null;
  }
  const { frameWidth, frameHeight } = regionFrameSize();
  return computeRegionViewport(player.getBoundingClientRect(), host.getBoundingClientRect(), frameWidth, frameHeight);
}

// Canonical geometry of a tracked region at a playhead position. Uses the last
// sampled keyframe at or before the playhead and never invents coordinates: a
// region without keyframes has no canonical geometry to project.
function regionBoxAt(region, timeMs) {
  const frames = (region?.keyframes || []).filter((frame) => frame?.box);
  if (!frames.length) return null;
  const t = Number(timeMs) || 0;
  let before = null;
  let earliest = frames[0];
  for (const frame of frames) {
    const ts = Number(frame.timestamp_ms);
    if (Number.isFinite(ts) && ts < Number(earliest?.timestamp_ms ?? ts)) earliest = frame;
    if (!Number.isFinite(ts) || ts > t) continue;
    if (!before || ts >= Number(before.timestamp_ms ?? ts)) before = frame;
  }
  const chosen = before || earliest;
  const box = chosen.box;
  return {
    x: Math.round(Number(box.x) || 0),
    y: Math.round(Number(box.y) || 0),
    width: Math.max(1, Math.round(Number(box.width) || 0)),
    height: Math.max(1, Math.round(Number(box.height) || 0)),
    observed: Boolean(chosen.observed),
    timestampMs: Number(chosen.timestamp_ms) || 0,
  };
}

function regionDeltaInputs() {
  const read = (selector) => {
    const value = Number($(selector)?.value);
    return Number.isFinite(value) ? Math.round(value) : 0;
  };
  return { dx: read("#region-dx"), dy: read("#region-dy"), dw: read("#region-dw"), dh: read("#region-dh") };
}

function setRegionDeltaInputs(deltas) {
  const write = (selector, value) => {
    const node = $(selector);
    if (node) node.value = String(Math.round(value));
  };
  write("#region-dx", deltas.dx);
  write("#region-dy", deltas.dy);
  write("#region-dw", deltas.dw);
  write("#region-dh", deltas.dh);
}

function resetRegionDeltaInputs() {
  setRegionDeltaInputs({ dx: 0, dy: 0, dw: 0, dh: 0 });
}

function offsetRegionBox(box, deltas) {
  return {
    x: box.x + deltas.dx,
    y: box.y + deltas.dy,
    width: box.width + deltas.dw,
    height: box.height + deltas.dh,
  };
}

function regionBoxChanged(before, after) {
  return before.x !== after.x || before.y !== after.y || before.width !== after.width || before.height !== after.height;
}

function regionDeltaBetween(from, to) {
  return { dx: to.x - from.x, dy: to.y - from.y, dw: to.width - from.width, dh: to.height - from.height };
}

// Union rect of every tracked keyframe. Deltas apply to all keyframes at once, so
// the frame constraint must be computed on their union: a shift that fits the
// keyframe on screen but pushes a sibling keyframe off-frame would be refused by
// the RuntimeHost contract after the fact.
function regionUnionBox(region) {
  const frames = (region?.keyframes || []).filter((frame) => frame?.box);
  if (!frames.length) return null;
  let left = Infinity;
  let top = Infinity;
  let right = -Infinity;
  let bottom = -Infinity;
  for (const frame of frames) {
    const box = frame.box;
    const x = Math.round(Number(box.x) || 0);
    const y = Math.round(Number(box.y) || 0);
    left = Math.min(left, x);
    top = Math.min(top, y);
    right = Math.max(right, x + Math.max(1, Math.round(Number(box.width) || 0)));
    bottom = Math.max(bottom, y + Math.max(1, Math.round(Number(box.height) || 0)));
  }
  return { x: left, y: top, width: right - left, height: bottom - top };
}

function formatRegionBox(box) {
  return `x=${box.x} y=${box.y} w=${box.width} h=${box.height}`;
}

function formatRegionDelta(deltas) {
  const sign = (value) => (value > 0 ? `+${value}` : String(value));
  return `Δx ${sign(deltas.dx)} · Δy ${sign(deltas.dy)} · Δw ${sign(deltas.dw)} · Δh ${sign(deltas.dh)}`;
}

// Fail-closed geometry validation mirroring the RuntimeHost override contract:
// out-of-frame or collapsed rectangles are refused and surfaced, never silently
// clamped into range. Deltas apply to every keyframe of the region, so every
// keyframe is validated, not just the one currently rendered.
function validateRegionBox(box, frameWidth, frameHeight) {
  if (box.width < REGION_MIN_CANONICAL_PX || box.height < REGION_MIN_CANONICAL_PX) {
    return `quá nhỏ (tối thiểu ${REGION_MIN_CANONICAL_PX}px canonical): ${formatRegionBox(box)}`;
  }
  if (box.x < 0 || box.y < 0 || box.x + box.width > frameWidth || box.y + box.height > frameHeight) {
    return `vượt biên khung hình ${frameWidth}×${frameHeight}: ${formatRegionBox(box)}`;
  }
  return "";
}

function validateRegionDelta(region, deltas, frameWidth, frameHeight) {
  const frames = (region?.keyframes || []).filter((frame) => frame?.box);
  if (!frames.length) return "region không có keyframe hình học để chỉnh";
  if (!(frameWidth > 0) || !(frameHeight > 0)) return "TextRegionPlan không khai báo kích thước khung hình";
  for (const frame of frames) {
    const base = regionBoxAt({ keyframes: [frame] }, frame.timestamp_ms);
    const problem = validateRegionBox(offsetRegionBox(base, deltas), frameWidth, frameHeight);
    if (problem) return `keyframe ${Math.round(Number(frame.timestamp_ms) || 0)}ms ${problem}`;
  }
  return "";
}

// The region the overlay edits: an explicit selection wins, then the region of a
// selected review item, then whatever region is live under the playhead.
function regionOverlayTarget(timeMs) {
  if (state.selectedRegionId) {
    const selected = regionById(state.selectedRegionId);
    if (selected) return selected;
  }
  const reviewRegionId = state.selectedReviewItem?.region_id;
  if (reviewRegionId) {
    const reviewed = regionById(reviewRegionId);
    if (reviewed) return reviewed;
  }
  return (state.textRegionPlan?.regions || []).find((region) => {
    const start = Number(region?.first_seen_ms);
    const end = Number(region?.last_seen_ms);
    return timeMs >= (Number.isFinite(start) ? start : 0) && timeMs <= (Number.isFinite(end) ? end : Infinity);
  }) || null;
}

function boxToOverlay(box, viewport) {
  return {
    left: box.x * viewport.scale,
    top: box.y * viewport.scale,
    width: box.width * viewport.scale,
    height: box.height * viewport.scale,
  };
}

// Pointer distance in rendered pixels converted through the single letterbox
// scale factor into a canonical union rect; display pixels never leave here.
function regionDragUnionRect(drag, clientX, clientY) {
  const dx = (clientX - drag.startX) / drag.scale;
  const dy = (clientY - drag.startY) / drag.scale;
  const base = drag.unionStart;
  if (drag.mode !== "resize") {
    return { x: Math.round(base.x + dx), y: Math.round(base.y + dy), width: base.width, height: base.height };
  }

  let left = base.x;
  let top = base.y;
  let right = base.x + base.width;
  let bottom = base.y + base.height;
  const handle = drag.handle || "";
  if (handle.includes("w")) left = Math.min(Math.round(base.x + dx), right - REGION_MIN_CANONICAL_PX);
  if (handle.includes("e")) right = Math.max(Math.round(base.x + base.width + dx), left + REGION_MIN_CANONICAL_PX);
  if (handle.includes("n")) top = Math.min(Math.round(base.y + dy), bottom - REGION_MIN_CANONICAL_PX);
  if (handle.includes("s")) bottom = Math.max(Math.round(base.y + base.height + dy), top + REGION_MIN_CANONICAL_PX);
  return { x: left, y: top, width: right - left, height: bottom - top };
}

// Clamps one axis of a rectangle into the canonical frame. When an edge handle
// owns the axis, the dragged edge stops at the frame bound and the opposite edge
// stays anchored where it was: clamping the origin while keeping the requested
// size would grow the stationary edge instead (a west-edge drag past x=0 would
// expand the box to the right). Otherwise - a move, or typed deltas - the whole
// extent is clamped, which is what the form's Δ model means.
function constrainRegionAxis(origin, size, frame, movingStart, movingEnd) {
  const minimum = Math.min(REGION_MIN_CANONICAL_PX, frame);
  if (movingStart) {
    const start = Math.max(0, Math.min(origin, origin + size - minimum));
    return { origin: start, size: origin + size - start };
  }
  if (movingEnd) {
    const end = Math.min(frame, Math.max(origin + size, origin + minimum));
    return { origin, size: end - origin };
  }
  const finalSize = Math.min(Math.max(size, minimum), frame);
  return { origin: Math.min(Math.max(origin, 0), Math.max(0, frame - finalSize)), size: finalSize };
}

// Constrains a dragged rectangle to the canonical frame. The constraint is
// surfaced (bounded class + hint) rather than applied silently, and the value
// that reaches the form is the constrained one, so what the operator sees over
// the video is what gets submitted.
function constrainRegionBox(box, frameWidth, frameHeight, handle = "") {
  const horizontal = constrainRegionAxis(box.x, box.width, frameWidth, handle.includes("w"), handle.includes("e"));
  const vertical = constrainRegionAxis(box.y, box.height, frameHeight, handle.includes("n"), handle.includes("s"));
  const constrained = { x: horizontal.origin, y: vertical.origin, width: horizontal.size, height: vertical.size };
  return { box: constrained, bounded: regionBoxChanged(box, constrained) };
}

function renderRegionOverlay() {
  const overlay = $("#region-overlay");
  const boxEl = $("#region-box");
  const ghostEl = $("#region-ghost");
  const hintEl = $("#region-overlay-hint");
  const tagEl = $("#region-box-tag");
  if (!overlay || !boxEl) return;

  const timeMs = currentPlayheadMs();
  const region = state.preview ? regionOverlayTarget(timeMs) : null;
  const base = region ? regionBoxAt(region, timeMs) : null;
  const viewport = region ? regionViewport() : null;

  if (!region || !base || !viewport) {
    overlay.classList.add("hidden");
    boxEl.dataset.regionId = "";
    if (hintEl) hintEl.textContent = "";
    return;
  }

  const dragging = Boolean(regionDrag && regionDrag.regionId === region.id);
  const deltas = regionDeltaInputs();
  const after = dragging ? regionDrag.current : constrainRegionBox(offsetRegionBox(base, deltas), viewport.frameWidth, viewport.frameHeight).box;
  const bounded = dragging ? regionDrag.bounded : regionBoxChanged(offsetRegionBox(base, deltas), after);
  const changed = regionBoxChanged(base, after);

  overlay.classList.remove("hidden");
  overlay.style.left = `${viewport.left}px`;
  overlay.style.top = `${viewport.top}px`;
  overlay.style.width = `${viewport.width}px`;
  overlay.style.height = `${viewport.height}px`;

  if (ghostEl) {
    const ghost = boxToOverlay(base, viewport);
    ghostEl.classList.toggle("hidden", !changed);
    ghostEl.style.left = `${ghost.left}px`;
    ghostEl.style.top = `${ghost.top}px`;
    ghostEl.style.width = `${ghost.width}px`;
    ghostEl.style.height = `${ghost.height}px`;
  }

  const placed = boxToOverlay(after, viewport);
  boxEl.dataset.regionId = region.id;
  boxEl.dataset.regionRole = region.role || "";
  boxEl.classList.toggle("is-dragging", dragging);
  boxEl.classList.toggle("is-clamped", bounded);
  boxEl.style.left = `${placed.left}px`;
  boxEl.style.top = `${placed.top}px`;
  boxEl.style.width = `${placed.width}px`;
  boxEl.style.height = `${placed.height}px`;
  if (tagEl) {
    const role = region.role || "region";
    tagEl.textContent = changed ? `${role} · ${formatRegionDelta({ dx: after.x - base.x, dy: after.y - base.y, dw: after.width - base.width, dh: after.height - base.height })}` : role;
  }
  if (hintEl) {
    hintEl.textContent = bounded
      ? "Đã chạm biên video — kéo thả không thể đưa region ra ngoài khung."
      : base.observed
        ? `Keyframe gần nhất ${Math.round(base.timestampMs)}ms`
        : `Keyframe nội suy ${Math.round(base.timestampMs)}ms`;
  }
}

function renderRegionDiff() {
  const node = $("#region-diff");
  if (!node) return;
  const { frameWidth, frameHeight } = regionFrameSize();
  const region = state.selectedRegionId ? regionById(state.selectedRegionId) : null;
  const base = region ? regionBoxAt(region, currentPlayheadMs()) : null;
  if (!region || !base) {
    node.innerHTML = "";
    node.classList.add("hidden");
    return;
  }

  const deltas = regionDeltaInputs();
  const after = constrainRegionBox(offsetRegionBox(base, deltas), frameWidth, frameHeight).box;
  const changed = regionBoxChanged(base, after);
  const roleSelect = $("#region-role");
  const nextRole = roleSelect?.value || "";
  const roleChanged = Boolean(nextRole) && nextRole !== region.role;

  node.classList.remove("hidden");
  node.innerHTML = `
    <div class="region-diff-row"><span>Trước</span><code>${esc(formatRegionBox(base))} · ${esc(region.role || "—")}</code></div>
    <div class="region-diff-row ${changed ? "is-changed" : ""}"><span>Sau</span><code>${esc(formatRegionBox(after))}${changed ? ` · ${esc(formatRegionDelta({ dx: after.x - base.x, dy: after.y - base.y, dw: after.width - base.width, dh: after.height - base.height }))}` : " · chưa đổi"}</code></div>
    <div class="region-diff-row ${roleChanged ? "is-changed" : ""}"><span>Role</span><code>${esc(region.role || "—")} → ${esc(nextRole || region.role || "—")}</code></div>
    <div class="region-diff-row"><span>Khung</span><code>${frameWidth}×${frameHeight} canonical px</code></div>`;
}

function setRegionError(message) {
  const node = $("#region-error");
  if (!node) return;
  node.textContent = message || "";
  node.classList.toggle("hidden", !message);
}

function renderRegionApplyResult(result) {
  const node = $("#region-result");
  if (!node) return;
  if (!result) {
    node.innerHTML = "";
    node.classList.add("hidden");
    return;
  }
  const pending = result.status !== "auto_resolved";
  node.classList.remove("hidden");
  node.classList.toggle("is-pending", pending);
  node.innerHTML = `
    <strong>${esc(pending ? "Đã áp dụng — vẫn cần review" : "Đã áp dụng — auto-resolved")}</strong>
    <span>${esc(result.message || "")}</span>
    <span>LocalizedVisualTrack: ${esc(shortID(result.localized_visual_track_cas || "—", 18))}</span>
    <span>LocalizedSubtitle: ${esc(shortID(result.localized_subtitle_cas || "—", 18))}</span>
    <span>RenderPlan: ${esc(shortID(result.render_plan_cas || "—", 18))}</span>
    <span>Preview render: ${esc(shortID(result.preview_render_cas || "—", 18))}</span>`;
}

function beginRegionDrag(event) {
  const overlay = $("#region-overlay");
  if (!overlay || overlay.classList.contains("hidden")) return;
  // One gesture owns the drag: a second pointer (touch/pen) and non-primary
  // buttons must not hijack or restart an in-flight edit.
  if (event.button != null && event.button !== 0) return;
  if (regionDrag && regionDrag.pointerId != null && event.pointerId != null && regionDrag.pointerId !== event.pointerId) return;
  const target = event.target;
  const handleEl = typeof target?.closest === "function" ? target.closest("[data-region-handle]") : null;
  const boxEl = typeof target?.closest === "function" ? target.closest("#region-box") : null;
  if (!boxEl && !handleEl) return;

  const timeMs = currentPlayheadMs();
  const region = regionOverlayTarget(timeMs);
  const base = region ? regionBoxAt(region, timeMs) : null;
  const union = region ? regionUnionBox(region) : null;
  const viewport = regionViewport();
  if (!region || !base || !union || !viewport || !(viewport.scale > 0)) return;

  // Only the selected region owns the form's Δ fields, so a drag also selects it.
  if (state.selectedRegionId !== region.id) {
    selectTimelineTarget({ regionId: region.id });
  }

  const startUnion = constrainRegionBox(offsetRegionBox(union, regionDeltaInputs()), viewport.frameWidth, viewport.frameHeight).box;
  const startDelta = regionDeltaBetween(union, startUnion);
  regionDrag = {
    regionId: region.id,
    mode: handleEl ? "resize" : "move",
    handle: handleEl?.dataset?.regionHandle || "",
    pointerId: event.pointerId,
    startX: Number(event.clientX) || 0,
    startY: Number(event.clientY) || 0,
    scale: viewport.scale,
    frameWidth: viewport.frameWidth,
    frameHeight: viewport.frameHeight,
    union,
    unionStart: startUnion,
    renderBase: base,
    delta: startDelta,
    current: offsetRegionBox(base, startDelta),
    bounded: false,
  };
  if (boxEl && typeof boxEl.setPointerCapture === "function" && event.pointerId != null) {
    try {
      boxEl.setPointerCapture(event.pointerId);
    } catch {
      // Pointer capture is an enhancement; the wrapper listeners still track the gesture.
    }
  }
  event.preventDefault?.();
  renderRegionOverlay();
}

function moveRegionDrag(event) {
  if (!regionDrag) return;
  // A second pointer must never steer an in-flight gesture, and a move with no
  // button held means the release happened outside the window: end the gesture
  // there instead of leaving a drag that follows the bare cursor.
  if (event.pointerId != null && regionDrag.pointerId != null && event.pointerId !== regionDrag.pointerId) return;
  if (event.buttons === 0) {
    endRegionDrag(event);
    return;
  }
  const requested = regionDragUnionRect(regionDrag, Number(event.clientX) || 0, Number(event.clientY) || 0);
  const constrained = constrainRegionBox(requested, regionDrag.frameWidth, regionDrag.frameHeight, regionDrag.mode === "resize" ? regionDrag.handle : "");
  regionDrag.delta = regionDeltaBetween(regionDrag.union, constrained.box);
  regionDrag.current = offsetRegionBox(regionDrag.renderBase, regionDrag.delta);
  regionDrag.bounded = regionBoxChanged(requested, constrained.box);
  event.preventDefault?.();
  renderRegionOverlay();
}

function endRegionDrag(event) {
  if (!regionDrag) return;
  if (event?.pointerId != null && regionDrag.pointerId != null && event.pointerId !== regionDrag.pointerId) return;
  const drag = regionDrag;
  regionDrag = null;
  // Commit the gesture into the existing form fields: the same Δ model the
  // RuntimeHost override contract already consumes, expressed in canonical px.
  setRegionDeltaInputs(drag.delta);
  setRegionError("");
  renderRegionDiff();
  renderRegionOverlay();
}

// Single entry point for transcript / region / review selection so video seek,
// selection state, inspector tab, highlight and editor content always move
// together and exactly one target stays selected.
function selectTimelineTarget({ segmentIndex = null, regionId = null, reviewId = null } = {}) {
  state.selectedSegmentIndex = segmentIndex == null ? null : Number(segmentIndex);
  state.selectedRegionId = regionId || null;
  state.selectedReviewItem = reviewId ? state.reviewItems.find((item) => item.id === reviewId) || null : null;

  const tab = inspectorTabForSelection();
  let startMs = null;
  if (tab === "transcript") startMs = startMsOf(segmentByIndex(state.selectedSegmentIndex));
  else if (tab === "regions") startMs = startMsOf(regionById(state.selectedRegionId));
  else startMs = startMsOf(state.selectedReviewItem);

  setInspectorTab(tab);
  // A selected target opens the drawer on the form that describes it, so the
  // visible editor and the active selection never disagree.
  if (tab === "transcript") setEditorTab("text");
  else if (tab === "regions") setEditorTab("region");
  renderSpeakers();
  renderZoneInspector();
  syncEditorToSelection();
  renderRegionDiff();
  scrollSelectionIntoView(tab);
  if (startMs != null) seekVideoTo(startMs);
  // Keep the projected geometry and the selected review target in step even when
  // the target carries no start timestamp of its own.
  renderRegionOverlay();
}

function updatePlayhead() {
  const player = $("#preview-player");
  const playhead = $("#timeline-playhead");
  const timeDisplay = $("#timeline-time-display");
  if (!playhead) return;

  const totalMs = state.timelineDurationMs;
  const curSec = player && player.currentTime ? player.currentTime : 0;
  const curMs = curSec * 1000;

  // The projected text region tracks the playhead directly, so it stays correct
  // even when the observational timeline has no duration to draw yet.
  renderRegionOverlay();

  if (!(totalMs > 0)) {
    playhead.style.left = "0%";
    if (timeDisplay) timeDisplay.textContent = `${formatMs(curMs)} / —`;
    return;
  }

  const pct = Math.max(0, Math.min(100, (curMs / totalMs) * 100));
  playhead.style.left = `${pct}%`;
  if (timeDisplay) {
    timeDisplay.textContent = `${formatMs(curMs)} / ${formatMs(totalMs)}`;
  }

  // 1. Dialogue timeline blocks & transcript sync using real intervals
  let activeSegIndex = null;
  $$(".speech-block").forEach((blk) => {
    const start = Number(blk.dataset.startMs ?? blk.dataset.timelineSeek);
    const end = Number(blk.dataset.endMs ?? start);
    const isActive = curMs >= start && curMs <= end;
    blk.classList.toggle("is-active-playing", isActive);
    if (isActive && activeSegIndex === null) {
      activeSegIndex = Number(blk.dataset.segIndex);
    }
  });

  if (activeSegIndex !== lastActiveSegIndex) {
    lastActiveSegIndex = activeSegIndex;
    const transcriptList = $("#transcript-list");
    if (transcriptList) {
      $$(".transcript-row", transcriptList).forEach((row) => {
        const rowIdx = Number(row.dataset.segmentIndex);
        row.classList.toggle("is-active-playing", activeSegIndex !== null && rowIdx === activeSegIndex);
      });
      if (activeSegIndex !== null && state.inspectorTab === "transcript") {
        const activeRow = $(`[data-segment-index="${activeSegIndex}"]`, transcriptList);
        if (activeRow) {
          activeRow.scrollIntoView({ behavior: "smooth", block: "nearest" });
        }
      }
    }
  }

  // 2. Visual text timeline blocks & regions sync using real intervals
  let activeRegionId = null;
  $$(".visual-block").forEach((blk) => {
    const start = Number(blk.dataset.startMs ?? blk.dataset.timelineSeek);
    const end = Number(blk.dataset.endMs ?? start);
    const isActive = curMs >= start && curMs <= end;
    blk.classList.toggle("is-active-playing", isActive);
    if (isActive && !activeRegionId) {
      activeRegionId = blk.dataset.regionId;
    }
  });

  if (activeRegionId !== lastActiveRegionId) {
    lastActiveRegionId = activeRegionId;
    const regionsList = $("#regions-list");
    if (regionsList) {
      $$(".region-row", regionsList).forEach((row) => {
        row.classList.toggle("is-active-playing", Boolean(activeRegionId && row.dataset.regionId === activeRegionId));
      });
      if (activeRegionId && state.inspectorTab === "regions") {
        const activeRow = $(`[data-region-id="${activeRegionId}"]`, regionsList);
        if (activeRow) {
          activeRow.scrollIntoView({ behavior: "smooth", block: "nearest" });
        }
      }
    }
  }

  // 3. Review markers & exceptions sync using real intervals
  let activeReviewId = null;
  $$(".timeline-marker").forEach((marker) => {
    const start = Number(marker.dataset.startMs ?? marker.dataset.timelineSeek);
    const end = Number(marker.dataset.endMs ?? start);
    const isActive = curMs >= start && curMs <= end;
    marker.classList.toggle("is-active-playing", isActive);
    if (isActive && !activeReviewId) {
      activeReviewId = marker.dataset.reviewMarker;
    }
  });

  if (activeReviewId !== lastActiveReviewId) {
    lastActiveReviewId = activeReviewId;
    const exceptionList = $("#exception-list");
    if (exceptionList) {
      $$(".exception-item", exceptionList).forEach((item) => {
        item.classList.toggle("is-active-playing", Boolean(activeReviewId && item.dataset.reviewId === activeReviewId));
      });
      if (activeReviewId && state.inspectorTab === "exceptions") {
        const activeItem = $(`[data-review-id="${activeReviewId}"]`, exceptionList);
        if (activeItem) {
          activeItem.scrollIntoView({ behavior: "smooth", block: "nearest" });
        }
      }
    }
  }
}

function renderZoneInspector() {
  const tabExceptionsCount = $("#tab-exceptions-count");
  const inspectorPendingBadge = $("#inspector-pending-badge");
  const pending = state.reviewItems.filter((item) => item.status === "pending");
  const emptyState = emptyReviewQueueState();

  if (tabExceptionsCount) tabExceptionsCount.textContent = pending.length;
  if (inspectorPendingBadge) {
    inspectorPendingBadge.className = `status-pill ${pending.length > 0 ? "warning" : emptyState.badgeClass}`;
    inspectorPendingBadge.textContent = pending.length > 0 ? `${pending.length} pending` : emptyState.badgeText;
  }

  renderInspectorExceptions();
  renderInspectorTranscript();
  renderInspectorRegions();
  renderInspectorEditor();
}

function renderInspectorExceptions() {
  const list = $("#exception-list");
  if (!list) return;

  if (!state.selectedRun) {
    list.innerHTML = '<div class="empty-state"><strong>Chưa chọn run.</strong><span>Mở một run từ Jobs & Queue.</span></div>';
    return;
  }

  const pending = state.reviewItems.filter((item) => item.status === "pending");
  if (!state.reviewItems.length || (pending.length === 0 && !$("#include-resolved")?.checked)) {
    const emptyState = emptyReviewQueueState();
    if (!emptyState.completed) {
      list.innerHTML = `
        <div class="compact-empty">
          <div class="empty-glyph">!</div>
          <strong>${esc(emptyState.title)}</strong>
          <p>${esc(emptyState.message)}</p>
        </div>`;
      return;
    }
    list.innerHTML = `
      <div class="happy-path-card">
        <div class="happy-icon">✓</div>
        <strong>${esc(emptyState.title)}</strong>
        <p>${esc(emptyState.message)}</p>
      </div>`;
    return;
  }

  list.innerHTML = state.reviewItems
    .map(
      (item) => `
    <button class="exception-item ${item.id === state.selectedReviewItem?.id ? "is-active" : ""}" type="button" data-review-id="${esc(item.id)}">
      <span class="exception-top">
        <strong>${esc(item.type)}</strong>
        <span class="status-pill ${esc(statusClass(item.severity))}">${esc(item.severity)}</span>
      </span>
      <p>${esc(item.reason || "Không có mô tả")}</p>
      <small>${esc(item.stage || "stage?")} · ${esc(item.status || "pending")}</small>
    </button>`
    )
    .join("");
}

function renderInspectorTranscript() {
  const list = $("#transcript-list");
  if (!list) return;

  const segments = state.translation?.segments || state.transcript?.speech_blocks || [];
  if (!segments.length) {
    list.innerHTML = '<div class="empty-state"><span>Chưa có dữ liệu transcript hoặc bản dịch.</span></div>';
    return;
  }

  list.innerHTML = segments
    .map((seg, idx) => {
      const spk = seg.speaker_id || "unassigned";
      const color = speakerColor(spk);
      const startMs = Number.isFinite(Number(seg.start_ms)) ? Number(seg.start_ms) : 0;
      const persistedEndMs = Number(seg.end_ms);
      const endMs = Number.isFinite(persistedEndMs) && persistedEndMs >= startMs ? persistedEndMs : startMs;
      const isSelected = state.selectedSegmentIndex === (seg.index ?? idx);

      return `
      <div class="transcript-row ${isSelected ? "is-selected" : ""}" data-transcript-seek="${startMs}" data-segment-index="${seg.index ?? idx}">
        <div class="transcript-meta">
          <span class="speaker-tag" style="--spk-color: ${color};">${esc(spk)}</span>
          <span class="mono quiet-time">${formatMs(startMs)}–${formatMs(endMs)}</span>
          <button class="small-text-btn" type="button" data-edit-segment="${seg.index ?? idx}">Sửa</button>
        </div>
        ${seg.source_text ? `<p class="source-quote">${esc(seg.source_text)}</p>` : ""}
        <p class="target-text"><strong>${esc(seg.target_text || seg.source_text || "")}</strong></p>
      </div>`;
    })
    .join("");
}

function renderInspectorRegions() {
  const list = $("#regions-list");
  if (!list) return;

  const regions = state.textRegionPlan?.regions || [];
  if (!regions.length) {
    list.innerHTML = '<div class="empty-state"><span>Chưa có dữ liệu text region.</span></div>';
    return;
  }

  list.innerHTML = regions
    .map((reg) => {
      const startMs = Number.isFinite(Number(reg.start_ms)) ? Number(reg.start_ms) : Number.isFinite(Number(reg.first_seen_ms)) ? Number(reg.first_seen_ms) : 0;
      const rawEndMs = Number(reg.end_ms);
      const persistedEndMs = Number.isFinite(rawEndMs) ? rawEndMs : Number(reg.last_seen_ms);
      const endMs = Number.isFinite(persistedEndMs) && persistedEndMs >= startMs ? persistedEndMs : startMs;
      const isSelected = state.selectedRegionId === reg.id;

      return `
      <div class="region-row ${isSelected ? "is-selected" : ""}" data-region-seek="${startMs}" data-region-id="${esc(reg.id)}">
        <div class="region-meta">
          <span class="status-pill info">${esc(reg.role || "region")}</span>
          <span class="mono quiet-time">${formatMs(startMs)}–${formatMs(endMs)}</span>
          <button class="small-text-btn" type="button" data-edit-region="${esc(reg.id)}">Chỉnh</button>
        </div>
        <p class="region-text">${esc(reg.text || "—")}</p>
      </div>`;
    })
    .join("");
}

function renderInspector() {
  renderZoneContext();
  renderZoneCenter();
  renderZoneInspector();
  renderRunContext();
  renderMetrics();
}

function clearInspectorEditor() {
  state.selectedReviewItem = null;
  state.selectedSegmentIndex = null;
  state.selectedRegionId = null;
  const emptyBox = $("#inspector-empty");
  const editorBox = $("#inspector-editor");
  if (emptyBox) emptyBox.classList.remove("hidden");
  if (editorBox) editorBox.classList.add("hidden");
}

function renderInspectorEditor() {
  const item = state.selectedReviewItem;
  const emptyBox = $("#inspector-empty");
  const editorBox = $("#inspector-editor");

  if (!item && state.selectedSegmentIndex == null && !state.selectedRegionId) {
    clearInspectorEditor();
    return;
  }

  if (emptyBox) emptyBox.classList.add("hidden");
  if (editorBox) editorBox.classList.remove("hidden");

  if (item) {
    const severity = $("#inspect-severity");
    if (severity) {
      severity.className = `status-pill ${statusClass(item.severity)}`;
      severity.textContent = item.severity || "warning";
    }
    const titleNode = $("#inspect-title");
    if (titleNode) titleNode.textContent = item.type || "Exception";
    const reasonNode = $("#inspect-reason");
    if (reasonNode) reasonNode.textContent = item.reason || "Không có mô tả";
    const idNode = $("#inspect-id");
    if (idNode) {
      idNode.textContent = shortID(item.id, 16);
      idNode.title = item.id || "";
    }
    const metaNode = $("#inspect-meta");
    if (metaNode) {
      metaNode.innerHTML = `
        <div><dt>Stage</dt><dd>${esc(item.stage || "—")}</dd></div>
        <div><dt>Segment / region</dt><dd>${esc(item.segment_id || item.region_id || (item.item_index ?? "—"))}</dd></div>
        <div><dt>Time</dt><dd>${esc(item.start_ms ?? "—")}–${esc(item.end_ms ?? "—")} ms</dd></div>`;
    }

    if (Number.isInteger(item.item_index) && item.item_index >= 0) {
      const segInput = $("#text-segment-index");
      if (segInput) segInput.value = item.item_index;
    }
    if (item.speaker_id) {
      const spkInput = $("#voice-speaker");
      if (spkInput) spkInput.value = item.speaker_id;
    }
    if (item.region_id) {
      const regInput = $("#region-id");
      if (regInput) regInput.value = item.region_id;
    }
  }

  if (state.selectedJob?.target_language) {
    const voiceLang = $("#voice-language");
    if (voiceLang) voiceLang.value = state.selectedJob.target_language;
  }
}

function artifactRows(artifact) {
  if (!artifact) return "";
  const rows = [
    ["Media CAS", artifact.output_cas_hash],
    ["Artifact CAS", artifact.cas_hash],
    ["Provenance", artifact.provenance_hash || artifact.provenance],
    ["Created", artifact.created_at],
    ["Asset", artifact.asset_id],
  ].filter(([, value]) => value);
  return rows.map(([label, value]) => `<div class="artifact-row"><span>${esc(label)}</span><span>${esc(value)}</span></div>`).join("");
}

function renderResult() {
  const previewBox = $("#media-placeholder");
  const previewDetails = $("#preview-details");
  const finalDetails = $("#final-details");

  if (previewDetails) previewDetails.innerHTML = artifactRows(state.preview);
  if (finalDetails) {
    finalDetails.innerHTML = state.final
      ? `<video class="render-video" controls playsinline preload="metadata" src="${esc(renderMediaURL("final"))}"></video><a class="secondary-button full artifact-open" href="${esc(renderMediaURL("final"))}" target="_blank" rel="noopener">Mở final video (mp4)</a>${artifactRows(state.final)}`
      : "";
  }

  if (previewBox) {
    if (state.preview) {
      previewBox.innerHTML = `<video class="render-video" controls playsinline preload="metadata" src="${esc(renderMediaURL("preview"))}"></video>`;
    } else {
      previewBox.innerHTML = '<div class="media-icon">▶</div><strong>Preview render</strong><span>Chưa có preview artifact cho run đang chọn.</span>';
    }
  }

  const handoff = state.handoff;
  const pill = $("#handoff-status");
  const box = $("#handoff-box");
  const check = $("#check-handoff");
  const start = $("#start-final-render");
  const posture = getRunPosture(state.selectedRun);
  const canManuallyStartFinal =
    posture === "review" &&
    handoff?.can_start_final_render === true &&
    handoff?.action === "start_final_render" &&
    !state.final;

  // Auto posture owns final-render handoff inside RuntimeHost. Calling the
  // handoff POST from the browser in Auto mode can execute final render again,
  // so the operator check is intentionally Review-only.
  if (check) check.disabled = !state.selectedRun || posture !== "review" || !state.preview || !!state.final;
  if (start) start.disabled = !canManuallyStartFinal;

  if (!state.selectedRun) {
    if (pill) {
      pill.className = "status-pill muted";
      pill.textContent = "Chưa kiểm tra";
    }
    if (box) {
      box.innerHTML = "<strong>Chọn run trước.</strong><span>Douyinie sẽ kiểm tra pending review items trước khi cho phép render.</span>";
    }
  } else if (!handoff) {
    if (pill) {
      pill.className = `status-pill ${posture === "auto" && state.final ? "pass" : "muted"}`;
      pill.textContent = posture === "auto" && state.final ? "Completed" : "Chưa kiểm tra";
    }
    if (box) {
      if (posture === "auto") {
        box.innerHTML = state.final
          ? "<strong>Auto mode: final render đã hoàn tất.</strong><span>Artifact cuối đang hiển thị từ persisted RuntimeHost state.</span>"
          : "<strong>Auto mode do RuntimeHost điều phối.</strong><span>Final render sẽ tự chạy khi preview hoàn tất và exception queue về zero; UI không kích hoạt handoff lần hai.</span>";
      } else if (!state.preview) {
        box.innerHTML = "<strong>Review mode: Chưa tới final handoff.</strong><span>Đợi preview render hoàn tất trước khi kiểm tra exception queue.</span>";
      } else {
        box.innerHTML = "<strong>Review mode: Cần kiểm tra handoff.</strong><span>Nhấn kiểm tra để xác nhận exception queue đã về zero trước khi final render.</span>";
      }
    }
  } else {
    if (pill) {
      pill.className = `status-pill ${handoff.can_start_final_render ? "pass" : "review_required"}`;
      pill.textContent = handoff.can_start_final_render ? "Ready" : "Review required";
    }
    if (box) {
      if (handoff.can_start_final_render) {
        if (posture === "auto") {
          box.innerHTML = "<strong>Auto mode: Có thể final render.</strong><span>Final render tự động được kích hoạt khi queue sạch.</span>";
        } else {
          box.innerHTML = "<strong>Review mode: Đã sẵn sàng.</strong><span>Queue về zero; operator có thể nhấn 'Bắt đầu final render' để xuất bản.</span>";
        }
      } else {
        box.innerHTML = `<strong>${esc(handoff.pending_review_count ?? 0)} exception còn pending.</strong><span>${esc(handoff.message || handoff.action || "Cần xử lý hết exception trước khi final render.")}</span>`;
      }
    }
  }
}

async function loadHealth() {
  try {
    state.health = await api("/api/v1/health");
  } catch {
    state.health = null;
  }
  renderRuntime();
}

async function loadBaseData() {
  const [jobsData, queueData] = await Promise.all([api("/api/v1/jobs"), api("/api/v1/queue")]);
  state.jobs = Array.isArray(jobsData.jobs) ? jobsData.jobs : [];
  state.queue = Array.isArray(queueData.queue) ? queueData.queue : [];
  renderJobs();
  renderQueue();
  renderMetrics();
}

// 404 already means "artifact not written yet" and stays silent. Everything
// else is a real RuntimeHost failure: tell the operator once per distinct
// failure set so the 3.5s poll loop cannot spam a persistent error.
function reportOptionalReadFailures(failures) {
  if (!failures.length) {
    lastOptionalReadFailureKey = "";
    return;
  }
  const key = failures.join(" | ");
  if (key === lastOptionalReadFailureKey) return;
  lastOptionalReadFailureKey = key;
  toast("Không tải được một phần artifact của run", failures.join("; "), "error");
}

async function loadSelectedRun() {
  if (!state.selectedRunId) {
    lastOptionalReadFailureKey = "";
    state.selectedRun = null;
    state.selectedJob = null;
    state.stages = [];
    state.reviewItems = [];
    state.selectedReviewItem = null;
    state.transcript = null;
    state.translation = null;
    state.voiceAssignment = null;
    state.textRegionPlan = null;
    state.preview = null;
    state.final = null;
    state.timelineDurationMs = 0;
    renderSelectedRun();
    renderInspector();
    renderResult();
    renderStepper();
    return;
  }

  const [runData, stagesData] = await Promise.all([
    api(`/api/v1/runs/${encodeURIComponent(state.selectedRunId)}`),
    api(`/api/v1/runs/${encodeURIComponent(state.selectedRunId)}/stages`),
  ]);
  state.selectedRun = runData.run;
  state.stages = Array.isArray(stagesData.stages) ? stagesData.stages : [];

  state.selectedJob = state.jobs.find((job) => job.id === state.selectedRun?.job_id) || null;
  if (!state.selectedJob && state.selectedRun?.job_id) {
    const jobData = await api(`/api/v1/jobs/${encodeURIComponent(state.selectedRun.job_id)}`);
    state.selectedJob = jobData.job;
  }

  const assetID = state.selectedJob?.source_asset_id;
  const target = state.selectedJob?.target_language || "vi";
  const runID = state.selectedRun?.id;

  // Review state is control-plane truth, not an optional artifact. Clear the
  // prior run projection before loading so a failed request can never leave a
  // previous run's exceptions attached to the newly selected run.
  state.reviewItems = [];
  state.selectedReviewItem = null;
  await loadReviewItems();

  // Each artifact read is independent: a missing artifact (404) is a normal
  // empty panel, but any other RuntimeHost failure must reach the operator
  // instead of being flattened into "no data" alongside the panels that did
  // load.
  const optionalReads = [
    {
      label: "transcript",
      url: assetID ? `/api/v1/assets/${encodeURIComponent(assetID)}/transcript?run_id=${encodeURIComponent(runID)}` : "",
      apply: (value) => {
        state.transcript = value?.transcript_artifact || null;
      },
    },
    {
      label: "translation variant",
      url: assetID ? `/api/v1/assets/${encodeURIComponent(assetID)}/translation-variant?target_language=${target}&run_id=${encodeURIComponent(runID)}` : "",
      apply: (value) => {
        state.translation = value?.translation_variant || null;
      },
    },
    {
      label: "voice assignment",
      url: assetID ? `/api/v1/assets/${encodeURIComponent(assetID)}/voice-assignment?target_language=${target}&run_id=${encodeURIComponent(runID)}` : "",
      apply: (value) => {
        state.voiceAssignment = value?.voice_assignment || null;
      },
    },
    {
      label: "text region plan",
      url: assetID ? `/api/v1/assets/${encodeURIComponent(assetID)}/text-region-plan` : "",
      apply: (value) => {
        state.textRegionPlan = value?.text_region_plan || null;
      },
    },
    {
      label: "preview render",
      url: assetID ? `/api/v1/assets/${encodeURIComponent(assetID)}/render/preview?target_language=${target}&run_id=${encodeURIComponent(runID)}` : "",
      apply: (value) => {
        state.preview = value?.preview_render || null;
      },
    },
    {
      label: "final render",
      url: assetID ? `/api/v1/assets/${encodeURIComponent(assetID)}/render/final?target_language=${target}&run_id=${encodeURIComponent(runID)}` : "",
      apply: (value) => {
        state.final = value?.final_render || null;
      },
    },
  ];

  const settled = await Promise.allSettled(optionalReads.map((read) => (read.url ? apiOptional(read.url) : Promise.resolve(null))));
  const failedReads = [];
  settled.forEach((result, index) => {
    const read = optionalReads[index];
    if (result.status === "fulfilled") {
      read.apply(result.value);
      return;
    }
    read.apply(null);
    failedReads.push(`${read.label}: ${result.reason?.message || result.reason}`);
  });
  reportOptionalReadFailures(failedReads);

  renderSelectedRun();
  renderInspector();
  renderResult();
  renderStepper();
  syncPolling();
}

function syncPolling() {
  if (pollTimer !== null) {
    window.clearInterval(pollTimer);
    pollTimer = null;
  }
  if (state.selectedRun?.status === "running" || state.selectedRun?.status === "queued") {
    pollTimer = window.setInterval(() => refreshAll({ quiet: true }), 3500);
  }
}

async function loadReviewItems() {
  if (!state.selectedRun) {
    state.reviewItems = [];
    return;
  }
  const suffix = $("#include-resolved")?.checked ? "?include_resolved=true" : "";
  const data = await api(`/api/v1/runs/${encodeURIComponent(state.selectedRun.id)}/review-items${suffix}`);
  state.reviewItems = Array.isArray(data.review_items) ? data.review_items : [];
  if (state.selectedReviewItem) {
    state.selectedReviewItem = state.reviewItems.find((item) => item.id === state.selectedReviewItem.id) || null;
  }
}

async function refreshAll({ quiet = false } = {}) {
  try {
    await Promise.all([loadHealth(), loadBaseData()]);
    if (state.selectedRunId) await loadSelectedRun();
    if (state.view === "result") await refreshResult();
    if (!quiet) toast("Đã làm mới", "Runtime, jobs, queue và run state đã đồng bộ.", "success");
  } catch (error) {
    showError(error);
  }
}

async function selectRun(runID, targetView = "jobs") {
  state.selectedRunId = runID;
  localStorage.setItem("douyinie:selectedRun", runID);
  state.handoff = null;
  state.selectedReviewItem = null;
  state.selectedSegmentIndex = null;
  state.selectedRegionId = null;
  state.timelineDurationMs = 0;
  lastActiveSegIndex = null;
  lastActiveRegionId = null;
  lastActiveReviewId = null;
  await loadSelectedRun();
  await refreshResult();
  navigate(targetView);
}

async function createSource(source, operator) {
  const attestation = {
    attestation_type: "OPERATOR_EXPLICIT_CONFIRMATION",
    declared_by: operator,
    terms_accepted: true,
    notes: "Created from Douyinie Operator UI",
  };
  if (state.sourceMode === "local_file") {
    const form = new FormData();
    form.append("file", source);
    form.append("declared_by", operator);
    form.append("terms_accepted", "true");
    form.append("notes", "Created from Douyinie Operator UI");
    return api("/api/v1/assets/upload", {
      method: "POST",
      body: form,
    });
  }
  return api("/api/v1/sources/acquire", {
    method: "POST",
    body: jsonBody({
      locator: { type: "douyin_url", location: source },
      attestation,
      consent_granted: true,
    }),
  });
}

async function handleNewJob(event) {
  event.preventDefault();
  const button = $("#start-job");
  const sourceInput = $("#source-input");
  const source = state.sourceMode === "local_file" ? sourceInput.files?.[0] : sourceInput.value.trim();
  const target = $("input[name=target_language]:checked").value;
  const posture = $("input[name=review_posture]:checked")?.value || "auto";
  const operator = operatorName();
  if (!source || !$("#rights-confirm").checked) return;

  setBusy(button, true, "Đang tạo job…");
  try {
    const sourceResult = await createSource(source, operator);
    const asset = sourceResult.asset;
    if (!asset?.id) throw new Error("RuntimeHost không trả source asset ID.");
    const jobData = await api("/api/v1/jobs", {
      method: "POST",
      body: jsonBody({ source_asset_id: asset.id, target_language: target }),
    });
    const job = jobData.job;
    const runData = await api(`/api/v1/jobs/${encodeURIComponent(job.id)}/runs`, {
      method: "POST",
      body: jsonBody({
        posture,
        review_posture: posture,
        config_snapshot_json: JSON.stringify({ profile: "hybrid", source: "operator-ui", posture }),
      }),
    });
    toast(
      "Localization đã vào queue",
      `Run ${shortID(runData.run.id, 16)} · ${target.toUpperCase()} · ${posture.toUpperCase()}`,
      "success"
    );
    await loadBaseData();
    await selectRun(runData.run.id, "inspector");
  } catch (error) {
    showError(error);
  } finally {
    setBusy(button, false);
  }
}

async function runAction(action, button) {
  if (!state.selectedRun) return;
  setBusy(button, true, "Đang xử lý…");
  try {
    await api(`/api/v1/runs/${encodeURIComponent(state.selectedRun.id)}/${action}`, { method: "POST", body: "{}" });
    toast("Run đã cập nhật", `${action}: ${shortID(state.selectedRun.id, 16)}`, "success");
    await loadBaseData();
    await loadSelectedRun();
  } catch (error) {
    showError(error);
  } finally {
    setBusy(button, false);
    renderSelectedRun();
  }
}

async function runInlineAction(action, runId, button) {
  setBusy(button, true, "…");
  try {
    await api(`/api/v1/runs/${encodeURIComponent(runId)}/${action}`, { method: "POST", body: "{}" });
    toast("Run đã cập nhật", `${action}: ${shortID(runId, 16)}`, "success");
    await loadBaseData();
    if (state.selectedRunId === runId) await loadSelectedRun();
  } catch (error) {
    showError(error);
  } finally {
    setBusy(button, false);
  }
}

async function submitManualOverride(event) {
  event.preventDefault();
  const item = state.selectedReviewItem;
  if (!item || !state.selectedRun) return;
  const button = $("button[type=submit]", event.currentTarget);
  setBusy(button, true, "Đang ghi audit…");
  try {
    await api(`/api/v1/runs/${encodeURIComponent(state.selectedRun.id)}/review/override`, {
      method: "POST",
      body: jsonBody({
        review_item_id: item.id,
        item_type: item.type,
        stage: item.stage,
        item_index: item.item_index,
        segment_id: item.segment_id || "",
        region_id: item.region_id || "",
        reason: $("#accept-reason").value.trim(),
        operator: operatorName(),
      }),
    });
    toast("Đã manual override", "Quyết định đã được ghi vào audit trail.", "success");
    state.handoff = null;
    state.selectedReviewItem = null;
    await loadReviewItems();
    renderInspector();
  } catch (error) {
    showError(error);
  } finally {
    setBusy(button, false);
  }
}

async function submitTextCorrection(event) {
  event.preventDefault();
  if (!state.selectedRun || !state.selectedJob) return;
  const button = $("button[type=submit]", event.currentTarget);
  setBusy(button, true, "Đang rerun…");
  try {
    await api(`/api/v1/runs/${encodeURIComponent(state.selectedRun.id)}/inspector/correct-text`, {
      method: "POST",
      body: jsonBody({
        run_id: state.selectedRun.id,
        job_id: state.selectedJob.id,
        asset_id: state.selectedJob.source_asset_id,
        target_language: state.selectedJob.target_language,
        segment_index: Number($("#text-segment-index").value),
        new_target_text: $("#text-target").value.trim(),
        reason: $("#text-reason").value.trim(),
        operator: operatorName(),
        execution_profile: "hybrid",
      }),
    });
    toast("Text đã được sửa", "Douyinie đã invalidates đúng descendants và chạy lại targeted stages.", "success");
    state.handoff = null;
    await loadSelectedRun();
    renderInspector();
  } catch (error) {
    showError(error);
  } finally {
    setBusy(button, false);
  }
}

async function submitVoiceCorrection(event) {
  event.preventDefault();
  if (!state.selectedRun || !state.selectedJob) return;
  const button = $("button[type=submit]", event.currentTarget);
  const speaker = $("#voice-speaker").value.trim();
  setBusy(button, true, "Đang đổi voice…");
  try {
    const profile = {
      id: $("#voice-profile-id").value.trim(),
      provider_id: $("#voice-provider-id").value.trim(),
      voice_id: $("#voice-engine-id").value.trim(),
      name: $("#voice-name").value.trim(),
      language: $("#voice-language").value,
    };
    await api(`/api/v1/runs/${encodeURIComponent(state.selectedRun.id)}/inspector/reassign-voice`, {
      method: "POST",
      body: jsonBody({
        run_id: state.selectedRun.id,
        job_id: state.selectedJob.id,
        asset_id: state.selectedJob.source_asset_id,
        target_language: state.selectedJob.target_language,
        custom_assignments: { [speaker]: profile },
        use_same_voice_for_all: false,
        reason: $("#voice-reason").value.trim(),
        operator: operatorName(),
        execution_profile: "hybrid",
      }),
    });
    toast("Voice assignment đã cập nhật", "TTS descendants được targeted rerun.", "success");
    state.handoff = null;
    await loadSelectedRun();
    renderInspector();
  } catch (error) {
    showError(error);
  } finally {
    setBusy(button, false);
  }
}

async function submitRegionCorrection(event) {
  event.preventDefault();
  if (!state.selectedRun || !state.selectedJob) return;
  const button = $("button[type=submit]", event.currentTarget);
  const regionId = ($("#region-id")?.value || "").trim();
  const region = regionById(regionId);
  if (!region) {
    setRegionError(`Không tìm thấy text region "${regionId}" trong TextRegionPlan của asset này.`);
    return;
  }

  // Fail-closed pre-flight: the deltas apply to every keyframe of the region, so
  // an edit that would leave the frame or collapse the rectangle is refused with
  // an actionable Inspector error instead of being silently clamped downstream.
  const deltas = regionDeltaInputs();
  const { frameWidth, frameHeight } = regionFrameSize();
  const geometryProblem = validateRegionDelta(region, deltas, frameWidth, frameHeight);
  if (geometryProblem) {
    setRegionError(`Override bị từ chối (fail-closed): ${geometryProblem}.`);
    renderRegionDiff();
    return;
  }
  setRegionError("");

  setBusy(button, true, "Đang cập nhật region…");
  try {
    const override = {
      region_id: region.id,
      box_delta_x: deltas.dx,
      box_delta_y: deltas.dy,
      box_delta_w: deltas.dw,
      box_delta_h: deltas.dh,
      notes: $("#region-reason")?.value.trim() || "",
    };
    const role = $("#region-role")?.value || "";
    const text = $("#region-text")?.value.trim() || "";
    if (role) override.new_role = role;
    if (text) override.new_text = text;
    const data = await api(`/api/v1/runs/${encodeURIComponent(state.selectedRun.id)}/inspector/override-region`, {
      method: "POST",
      body: jsonBody({
        run_id: state.selectedRun.id,
        job_id: state.selectedJob.id,
        asset_id: state.selectedJob.source_asset_id,
        target_language: state.selectedJob.target_language,
        overrides: [override],
        scene_protected_regions: [],
        reason: $("#region-reason")?.value.trim() || "",
        operator: operatorName(),
      }),
    });
    const result = data?.result || null;
    renderRegionApplyResult(result);
    toast("Text region đã cập nhật", result?.message || "Chỉ visual/render descendants được targeted rerun.", "success");
    state.handoff = null;
    await loadSelectedRun();
    // The reloaded plan already contains the applied geometry: the pending deltas
    // are spent, so the before/after readout returns to the canonical state.
    resetRegionDeltaInputs();
    renderRegionDiff();
    renderRegionOverlay();
  } catch (error) {
    // The pending edit stays intact so the operator can correct the rejected
    // geometry, and the RuntimeHost rejection is surfaced where the edit is made.
    setRegionError(`RuntimeHost từ chối region override: ${error?.message || String(error)}`);
    showError(error);
  } finally {
    setBusy(button, false);
  }
}

async function auditionVoice(speakerId, voiceProfile, { contextual = false } = {}) {
  if (!state.selectedJob) return;

  // Contextual audition must be bound to a real selected segment: the artifact
  // is rendered from that segment's translated text plus the run's preserved
  // stems, never from an implicit default segment.
  const segmentIndex = contextual ? state.selectedSegmentIndex : null;
  if (contextual && !Number.isInteger(segmentIndex)) {
    toast("Chưa chọn segment", "Chọn một segment trong timeline hoặc tab 'Transcript & Dịch' trước khi nghe thử cùng BGM/SFX.", "error");
    return;
  }

  toast(
    contextual ? "Đang audition trong ngữ cảnh" : "Đang audition voice",
    contextual ? `Đang dựng mẫu ~10s cùng BGM/SFX cho ${speakerId} ở segment ${segmentIndex}…` : `Đang gửi yêu cầu mẫu voice cho ${speakerId}…`
  );
  try {
    const body = {
      run_id: state.selectedRun?.id || "",
      target_language: state.selectedJob.target_language,
      voice: voiceProfile,
      is_contextual: contextual,
    };
    if (contextual) body.segment_index = segmentIndex;

    const res = await api(`/api/v1/assets/${encodeURIComponent(state.selectedJob.source_asset_id)}/voice-audition`, {
      method: "POST",
      body: jsonBody(body),
    });
    if (!res.audio_data_url) throw new Error("RuntimeHost không trả audition audio có thể phát trong browser.");
    if (state.auditionAudio) {
      state.auditionAudio.pause();
      state.auditionAudio.currentTime = 0;
    }
    const audio = new Audio(res.audio_data_url);
    state.auditionAudio = audio;
    audio.addEventListener(
      "ended",
      () => {
        if (state.auditionAudio === audio) state.auditionAudio = null;
      },
      { once: true }
    );
    await audio.play();
    const durationSeconds = (Number(res.audition_result?.measured_duration_ms) || 0) / 1000;
    const suffix = contextual ? ` · segment ${segmentIndex} · có BGM/SFX` : "";
    toast(contextual ? "Đang phát audition ngữ cảnh" : "Đang phát audition", `${speakerId} · ${durationSeconds.toFixed(1)}s${suffix}`, "success");
  } catch (error) {
    showError(error);
  }
}

async function refreshResult() {
  if (!state.selectedRun || !state.selectedJob) {
    state.preview = null;
    state.final = null;
    state.handoff = null;
    renderResult();
    return;
  }
  const assetID = encodeURIComponent(state.selectedJob.source_asset_id);
  const target = encodeURIComponent(state.selectedJob.target_language || "vi");
  const runID = encodeURIComponent(state.selectedRun.id);
  const [previewData, finalData] = await Promise.all([
    apiOptional(`/api/v1/assets/${assetID}/render/preview?target_language=${target}&run_id=${runID}`),
    apiOptional(`/api/v1/assets/${assetID}/render/final?target_language=${target}&run_id=${runID}`),
  ]);
  state.preview = previewData?.preview_render || null;
  state.final = finalData?.final_render || null;
  renderResult();
}

async function checkHandoff(button) {
  if (!state.selectedRun) return;
  const posture = getRunPosture(state.selectedRun);
  if (posture !== "review" || !state.preview || state.final) return;
  setBusy(button, true, "Đang kiểm tra…");
  try {
    const data = await api(`/api/v1/runs/${encodeURIComponent(state.selectedRun.id)}/render/handoff`, {
      method: "POST",
      body: jsonBody({ posture }),
    });
    state.handoff = data.handoff || data;
    if (state.handoff.auto_render_started) {
      await refreshResult();
      toast("Final render hoàn tất", "Auto mode đã xuất final render qua RuntimeHost.", "success");
    } else if (state.handoff.can_start_final_render) {
      toast("Final render ready", "Exception queue đã về zero.", "success");
    } else {
      toast("Cần review", `${state.handoff.pending_review_count ?? 0} exception còn pending.`);
    }
  } catch (error) {
    showError(error);
  } finally {
    setBusy(button, false);
    renderResult();
  }
}

async function startFinalRender(button) {
  const posture = getRunPosture(state.selectedRun);
  if (
    !state.selectedRun ||
    !state.selectedJob ||
    posture !== "review" ||
    !state.handoff?.can_start_final_render ||
    state.handoff?.action !== "start_final_render" ||
    state.final
  ) return;
  setBusy(button, true, "Đang final render…");
  try {
    await api(`/api/v1/assets/${encodeURIComponent(state.selectedJob.source_asset_id)}/render/final`, {
      method: "POST",
      body: jsonBody({
        run_id: state.selectedRun.id,
        job_id: state.selectedJob.id,
        target_language: state.selectedJob.target_language,
      }),
    });
    toast("Final render hoàn tất", "Artifact cuối đã được ghi vào CAS.", "success");
    await refreshResult();
    await loadSelectedRun();
  } catch (error) {
    showError(error);
  } finally {
    setBusy(button, false);
    renderResult();
  }
}

function setSourceMode(mode) {
  state.sourceMode = mode;
  $$("[data-source-mode]").forEach((button) => button.classList.toggle("is-active", button.dataset.sourceMode === mode));
  const input = $("#source-input");
  if (!input) return;
  if (mode === "local_file") {
    $("#source-label").textContent = "Video trên máy";
    $("#source-help").textContent = "Chọn file trong browser. RuntimeHost nhận upload và tự ingest vào CAS; UI không gửi đường dẫn filesystem.";
    input.type = "file";
    input.accept = "video/*,audio/*";
    input.removeAttribute("placeholder");
    input.removeAttribute("autocomplete");
  } else {
    $("#source-label").textContent = "Douyin URL";
    $("#source-help").textContent = "Nguồn chỉ được acquire khi policy và quyền sử dụng hợp lệ.";
    input.type = "url";
    input.removeAttribute("accept");
    input.autocomplete = "off";
    input.placeholder = "https://www.douyin.com/video/…";
  }
}

function setEditorTab(tab) {
  $$("[data-editor-tab]").forEach((button) => button.classList.toggle("is-active", button.dataset.editorTab === tab));
  $$("[data-editor-panel]").forEach((panel) => panel.classList.toggle("hidden", panel.dataset.editorPanel !== tab));
}

function setInspectorTab(tab) {
  state.inspectorTab = tab;
  $$("[data-inspector-tab]").forEach((button) => button.classList.toggle("is-active", button.dataset.inspectorTab === tab));
  $$("[data-inspector-panel]").forEach((panel) => panel.classList.toggle("hidden", panel.dataset.inspectorPanel !== tab));
}

function seekVideoTo(timeMs) {
  const player = $("#preview-player");
  if (player && !isNaN(timeMs)) {
    player.currentTime = timeMs / 1000;
    updatePlayhead();
  }
}

function showError(error) {
  console.error(error);
  toast("Có lỗi", error?.message || String(error), "error");
}

function bindEvents() {
  $$("[data-view-target]").forEach((button) => button.addEventListener("click", () => navigate(button.dataset.viewTarget)));
  $$("[data-source-mode]").forEach((button) => button.addEventListener("click", () => setSourceMode(button.dataset.sourceMode)));

  $$("input[name=target_language]").forEach((radio) =>
    radio.addEventListener("change", () => {
      $$(".language-option").forEach((label) => label.classList.toggle("is-selected", $("input", label).checked));
    })
  );

  $$("input[name=review_posture]").forEach((radio) =>
    radio.addEventListener("change", () => {
      $$(".posture-option").forEach((label) => label.classList.toggle("is-selected", $("input", label).checked));
    })
  );

  $("#new-job-form")?.addEventListener("submit", handleNewJob);
  $("#refresh-all")?.addEventListener("click", () => refreshAll());
  $("#jobs-refresh")?.addEventListener("click", () => refreshAll());
  $("#result-refresh")?.addEventListener("click", () => refreshResult().catch(showError));

  // Queue table interactions
  $("#queue-body")?.addEventListener("click", (event) => {
    const selectBtn = event.target.closest("[data-select-run]");
    if (selectBtn) {
      selectRun(selectBtn.dataset.selectRun).catch(showError);
      return;
    }
    const actionBtn = event.target.closest("[data-inline-action]");
    if (actionBtn) {
      runInlineAction(actionBtn.dataset.inlineAction, actionBtn.dataset.inlineRun, actionBtn);
    }
  });

  // Selected run actions
  $$("[data-run-action]").forEach((button) =>
    button.addEventListener("click", () => runAction(button.dataset.runAction, button))
  );

  // Inspector tabs
  $$("[data-inspector-tab]").forEach((button) =>
    button.addEventListener("click", () => setInspectorTab(button.dataset.inspectorTab))
  );

  $("#include-resolved")?.addEventListener("change", async () => {
    try {
      await loadReviewItems();
      renderInspector();
    } catch (error) {
      showError(error);
    }
  });

  // Exceptions selection
  $("#exception-list")?.addEventListener("click", (event) => {
    const button = event.target.closest("[data-review-id]");
    if (!button) return;
    selectTimelineTarget({ reviewId: button.dataset.reviewId });
  });

  // Transcript row click & edit
  $("#transcript-list")?.addEventListener("click", (event) => {
    const row = event.target.closest("[data-transcript-seek]");
    if (row) {
      selectTimelineTarget({ segmentIndex: Number(row.dataset.segmentIndex) });
    }
    const editBtn = event.target.closest("[data-edit-segment]");
    if (editBtn) {
      // Row-scoped edit shortcut: select the row's target and let the shared
      // selector open the text form with that segment's content.
      selectTimelineTarget({ segmentIndex: Number(editBtn.dataset.editSegment) });
    }
  });

  // Visual regions row click & edit
  $("#regions-list")?.addEventListener("click", (event) => {
    const row = event.target.closest("[data-region-seek]");
    if (row) {
      selectTimelineTarget({ regionId: row.dataset.regionId });
    }
    const editBtn = event.target.closest("[data-edit-region]");
    if (editBtn) {
      selectTimelineTarget({ regionId: editBtn.dataset.editRegion });
    }
  });

  // Speaker actions (reassign, audition)
  $("#speaker-list")?.addEventListener("click", (event) => {
    const btn = event.target.closest("[data-speaker-action]");
    if (!btn) return;
    const action = btn.dataset.speakerAction;
    const speakerId = btn.dataset.speaker;
    const voiceId = btn.dataset.voice;
    const providerId = btn.dataset.provider;
    const name = btn.dataset.name;

    if (action === "reassign") {
      $("#voice-speaker").value = speakerId;
      $("#voice-engine-id").value = voiceId;
      $("#voice-profile-id").value = voiceId;
      $("#voice-provider-id").value = providerId;
      $("#voice-name").value = name;
      setEditorTab("voice");
      const emptyBox = $("#inspector-empty");
      const editorBox = $("#inspector-editor");
      if (emptyBox) emptyBox.classList.add("hidden");
      if (editorBox) editorBox.classList.remove("hidden");
    } else if (action === "audition" || action === "audition-contextual") {
      auditionVoice(
        speakerId,
        {
          id: voiceId,
          voice_id: voiceId,
          provider_id: providerId,
          name: name,
          language: state.selectedJob?.target_language || "vi",
        },
        { contextual: action === "audition-contextual" }
      );
    }
  });

  // Timeline seeking & clicking
  $("#timeline-viewport")?.addEventListener("click", (event) => {
    const seekEl = event.target.closest("[data-timeline-seek]");
    if (seekEl) {
      if (seekEl.dataset.segIndex != null) {
        selectTimelineTarget({ segmentIndex: Number(seekEl.dataset.segIndex) });
      } else if (seekEl.dataset.regionId) {
        selectTimelineTarget({ regionId: seekEl.dataset.regionId });
      } else if (seekEl.dataset.reviewMarker) {
        selectTimelineTarget({ reviewId: seekEl.dataset.reviewMarker });
      } else {
        seekVideoTo(Number(seekEl.dataset.timelineSeek));
      }
      return;
    }

    // Ruler/viewport background click seeking
    const rect = event.currentTarget.getBoundingClientRect();
    const clickX = event.clientX - rect.left;
    const pct = Math.max(0, Math.min(1, clickX / rect.width));
    const totalMs = state.timelineDurationMs;
    if (!(totalMs > 0)) return;
    seekVideoTo(pct * totalMs);
  });

  // Video player timeupdate
  const player = $("#preview-player");
  if (player) {
    player.addEventListener("timeupdate", updatePlayhead);
    player.addEventListener("loadedmetadata", () => {
      calculateTotalDuration();
      renderObservationalTimeline();
    });
  }

  // Editor drawer tabs
  $$("[data-editor-tab]").forEach((button) =>
    button.addEventListener("click", () => setEditorTab(button.dataset.editorTab))
  );

  // Forms
  $("#accept-form")?.addEventListener("submit", submitManualOverride);
  $("#text-form")?.addEventListener("submit", submitTextCorrection);
  $("#voice-form")?.addEventListener("submit", submitVoiceCorrection);
  $("#region-form")?.addEventListener("submit", submitRegionCorrection);

  // Direct manipulation of the projected text region over the video. Pointer
  // gestures only ever write canonical deltas into the region form above.
  const videoWrapper = $("#video-wrapper");
  if (videoWrapper) {
    videoWrapper.addEventListener("pointerdown", beginRegionDrag);
    videoWrapper.addEventListener("pointermove", moveRegionDrag);
    videoWrapper.addEventListener("pointerup", endRegionDrag);
    videoWrapper.addEventListener("pointercancel", endRegionDrag);
  }
  ["#region-dx", "#region-dy", "#region-dw", "#region-dh"].forEach((selector) => {
    $(selector)?.addEventListener("input", () => {
      setRegionError("");
      renderRegionDiff();
      renderRegionOverlay();
    });
  });
  $("#region-role")?.addEventListener("change", renderRegionDiff);

  // Result view buttons
  $("#check-handoff")?.addEventListener("click", (event) => checkHandoff(event.currentTarget));
  $("#start-final-render")?.addEventListener("click", (event) => startFinalRender(event.currentTarget));
}

async function boot() {
  bindEvents();
  setSourceMode(state.sourceMode);
  setEditorTab("accept");
  setInspectorTab("exceptions");
  renderSelectedRun();
  renderInspector();
  renderResult();
  renderStepper();
  await refreshAll({ quiet: true });
  syncPolling();
}

boot().catch(showError);
