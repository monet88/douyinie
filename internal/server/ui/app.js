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
  preview: null,
  final: null,
  handoff: null,
};

let pollTimer = null;

const titles = {
  new: "Tạo localization job",
  jobs: "Jobs & Queue",
  inspector: "Inspector",
  result: "Kết quả",
};

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

async function api(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body && !(options.body instanceof FormData) && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  const response = await fetch(path, { ...options, headers });
  const text = await response.text();
  let data = {};
  if (text) {
    try { data = JSON.parse(text); } catch { data = { message: text }; }
  }
  if (!response.ok) {
    const error = new Error(data.error || data.message || `${response.status} ${response.statusText}`);
    error.status = response.status;
    throw error;
  }
  return data;
}

async function apiOptional(path) {
  try { return await api(path); }
  catch (error) {
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

function operatorName() {
  return $("#operator-input").value.trim() || "local-operator";
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

function toast(title, message = "", type = "") {
  const region = $("#toast-region");
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
  $("#page-title").textContent = titles[view] || "Douyinie Operator";
  if (view === "inspector") renderInspector();
  if (view === "result") refreshResult().catch(showError);
}

function renderRuntime() {
  const node = $("#runtime-state");
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
  const strong = $("#run-context strong");
  strong.textContent = state.selectedRun ? shortID(state.selectedRun.id, 16) : "Chưa chọn";
  strong.title = state.selectedRun?.id || "";
}

function renderMetrics() {
  $("#metric-jobs").textContent = state.jobs.length;
  $("#metric-queue").textContent = state.queue.length;
  $("#metric-review").textContent = state.reviewItems.filter((item) => item.status === "pending").length;
}

function renderQueue() {
  const body = $("#queue-body");
  $("#queue-summary").textContent = state.queue.length ? `${state.queue.length} run` : "Không có run";
  if (!state.queue.length) {
    body.innerHTML = '<tr><td colspan="5" class="empty-cell">Chưa có dữ liệu queue.</td></tr>';
    return;
  }
  body.innerHTML = state.queue.map((entry) => `
    <tr>
      <td class="mono" title="${esc(entry.run_id)}">${esc(shortID(entry.run_id))}</td>
      <td class="mono" title="${esc(entry.job_id)}">${esc(shortID(entry.job_id))}</td>
      <td><span class="status-pill ${esc(statusClass(entry.status))}">${esc(entry.status)}</span></td>
      <td>${esc(entry.position)}</td>
      <td><button class="inline-select" type="button" data-select-run="${esc(entry.run_id)}">Mở</button></td>
    </tr>`).join("");
}

function renderJobs() {
  const root = $("#job-cards");
  if (!state.jobs.length) {
    root.innerHTML = '<div class="empty-state"><strong>Chưa có job.</strong><span>Tạo job đầu tiên ở màn hình Job mới.</span></div>';
    return;
  }
  root.innerHTML = state.jobs.map((job) => `
    <article class="job-card">
      <div class="job-card-top"><strong>${esc(shortID(job.id, 14))}</strong><span class="status-pill ${esc(statusClass(job.status))}">${esc(job.status || "pending")}</span></div>
      <dl>
        <div><dt>Target</dt><dd>${esc((job.target_language || "—").toUpperCase())}</dd></div>
        <div><dt>Asset</dt><dd title="${esc(job.source_asset_id)}">${esc(shortID(job.source_asset_id, 14))}</dd></div>
        <div><dt>Created</dt><dd>${esc(formatDate(job.created_at))}</dd></div>
      </dl>
    </article>`).join("");
}

function renderSelectedRun() {
  const run = state.selectedRun;
  const job = state.selectedJob;
  $("#run-title").textContent = run ? shortID(run.id, 18) : "Chưa chọn run";
  const pill = $("#run-status");
  pill.className = `status-pill ${statusClass(run?.status)}`;
  pill.textContent = run?.status || "—";
  $("#run-details").innerHTML = `
    <div><dt>Job</dt><dd title="${esc(job?.id)}">${esc(shortID(job?.id, 16))}</dd></div>
    <div><dt>Target</dt><dd>${esc((job?.target_language || "—").toUpperCase())}</dd></div>
    <div><dt>Created</dt><dd>${esc(formatDate(run?.created_at))}</dd></div>`;

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
}

function renderStages() {
  const root = $("#stage-list");
  if (!state.selectedRun) {
    root.innerHTML = '<div class="empty-state"><strong>Chưa có run được chọn.</strong><span>Chọn một run từ bảng bên trái.</span></div>';
    return;
  }
  if (!state.stages.length) {
    root.innerHTML = '<div class="empty-state"><strong>Chưa có stage execution.</strong><span>Run đang chờ worker hoặc chưa bắt đầu.</span></div>';
    return;
  }
  root.innerHTML = state.stages.map((stage) => `
    <div class="stage-row">
      <strong>${esc(stage.stage)}</strong>
      <span class="status-pill ${esc(statusClass(stage.status))}">${esc(stage.status)}</span>
      ${stage.error_message ? `<small>${esc(stage.error_message)}</small>` : ""}
    </div>`).join("");
}

function renderInspector() {
  const list = $("#exception-list");
  const pending = state.reviewItems.filter((item) => item.status === "pending");
  $("#exception-count").textContent = `${pending.length} pending`;
  renderMetrics();

  if (!state.selectedRun) {
    list.innerHTML = '<div class="empty-state"><strong>Chưa chọn run.</strong><span>Mở một run từ Jobs & Queue.</span></div>';
    clearInspectorEditor();
    return;
  }
  if (!state.reviewItems.length) {
    list.innerHTML = '<div class="empty-state"><strong>Queue review đang sạch.</strong><span>Không có exception cần operator xử lý.</span></div>';
    clearInspectorEditor();
    return;
  }

  list.innerHTML = state.reviewItems.map((item) => `
    <button class="exception-item ${item.id === state.selectedReviewItem?.id ? "is-active" : ""}" type="button" data-review-id="${esc(item.id)}">
      <span class="exception-top"><strong>${esc(item.type)}</strong><span class="status-pill ${esc(statusClass(item.severity))}">${esc(item.severity)}</span></span>
      <p>${esc(item.reason || "Không có mô tả")}</p>
      <small>${esc(item.stage || "stage?")} · ${esc(item.status || "pending")}</small>
    </button>`).join("");

  if (state.selectedReviewItem && !state.reviewItems.some((item) => item.id === state.selectedReviewItem.id)) {
    state.selectedReviewItem = null;
  }
  if (!state.selectedReviewItem) clearInspectorEditor();
  else renderInspectorEditor();
}

function clearInspectorEditor() {
  state.selectedReviewItem = null;
  $("#inspector-empty").classList.remove("hidden");
  $("#inspector-editor").classList.add("hidden");
}

function renderInspectorEditor() {
  const item = state.selectedReviewItem;
  if (!item) return clearInspectorEditor();
  $("#inspector-empty").classList.add("hidden");
  $("#inspector-editor").classList.remove("hidden");
  const severity = $("#inspect-severity");
  severity.className = `status-pill ${statusClass(item.severity)}`;
  severity.textContent = item.severity || "warning";
  $("#inspect-title").textContent = item.type || "Exception";
  $("#inspect-reason").textContent = item.reason || "Không có mô tả";
  $("#inspect-id").textContent = shortID(item.id, 16);
  $("#inspect-id").title = item.id || "";
  $("#inspect-meta").innerHTML = `
    <div><dt>Stage</dt><dd>${esc(item.stage || "—")}</dd></div>
    <div><dt>Segment / region</dt><dd>${esc(item.segment_id || item.region_id || (item.item_index ?? "—"))}</dd></div>
    <div><dt>Time</dt><dd>${esc(item.start_ms ?? "—")}–${esc(item.end_ms ?? "—")} ms</dd></div>`;
  if (Number.isInteger(item.item_index) && item.item_index >= 0) $("#text-segment-index").value = item.item_index;
  if (item.speaker_id) $("#voice-speaker").value = item.speaker_id;
  if (item.region_id) $("#region-id").value = item.region_id;
  if (state.selectedJob?.target_language) $("#voice-language").value = state.selectedJob.target_language;
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

function renderMediaURL(kind) {
  if (!state.selectedJob) return "";
  const asset = encodeURIComponent(state.selectedJob.source_asset_id);
  const target = encodeURIComponent(state.selectedJob.target_language);
  const runParam = state.selectedRun ? `&run_id=${encodeURIComponent(state.selectedRun.id)}` : "";
  return `/api/v1/assets/${asset}/render/${kind}/media?target_language=${target}${runParam}`;
}

function renderResult() {
  const previewBox = $("#media-placeholder");
  const previewDetails = $("#preview-details");
  const finalDetails = $("#final-details");
  previewDetails.innerHTML = artifactRows(state.preview);
  finalDetails.innerHTML = state.final
    ? `<a class="secondary-button full artifact-open" href="${esc(renderMediaURL("final"))}" target="_blank" rel="noopener">Mở final video</a>${artifactRows(state.final)}`
    : "";

  if (state.preview) {
    previewBox.innerHTML = `<video class="render-video" controls playsinline preload="metadata" src="${esc(renderMediaURL("preview"))}"></video>`;
  } else {
    previewBox.innerHTML = '<div class="media-icon">▶</div><strong>Preview render</strong><span>Chưa có preview artifact cho run đang chọn.</span>';
  }

  const handoff = state.handoff;
  const pill = $("#handoff-status");
  const box = $("#handoff-box");
  const check = $("#check-handoff");
  const start = $("#start-final-render");
  check.disabled = !state.selectedRun;
  start.disabled = !handoff?.can_start_final_render;

  if (!state.selectedRun) {
    pill.className = "status-pill muted";
    pill.textContent = "Chưa kiểm tra";
    box.innerHTML = "<strong>Chọn run trước.</strong><span>Douyinie sẽ kiểm tra pending review items trước khi cho phép render.</span>";
  } else if (!handoff) {
    pill.className = "status-pill muted";
    pill.textContent = "Chưa kiểm tra";
    box.innerHTML = "<strong>Handoff chưa được đánh giá.</strong><span>Nhấn kiểm tra để xác nhận exception queue đã về zero.</span>";
  } else {
    pill.className = `status-pill ${handoff.can_start_final_render ? "pass" : "review_required"}`;
    pill.textContent = handoff.can_start_final_render ? "Ready" : "Review required";
    box.innerHTML = `<strong>${handoff.can_start_final_render ? "Có thể final render." : `${esc(handoff.pending_review_count ?? 0)} exception còn pending.`}</strong><span>${esc(handoff.message || handoff.action || "")}</span>`;
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

async function loadSelectedRun() {
  if (!state.selectedRunId) {
    state.selectedRun = null;
    state.selectedJob = null;
    state.stages = [];
    state.reviewItems = [];
    state.selectedReviewItem = null;
    renderSelectedRun();
    renderInspector();
    renderResult();
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
  await loadReviewItems();
  renderSelectedRun();
  renderInspector();
  syncPolling();
}

function syncPolling() {
  if (pollTimer !== null) {
    window.clearInterval(pollTimer);
    pollTimer = null;
  }
  if (state.selectedRun?.status === "running" || state.selectedRun?.status === "queued") {
    pollTimer = window.setInterval(() => refreshAll({ quiet: true }), 8000);
  }
}

async function loadReviewItems() {
  if (!state.selectedRun) {
    state.reviewItems = [];
    return;
  }
  const suffix = $("#include-resolved").checked ? "?include_resolved=true" : "";
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
      body: jsonBody({ config_snapshot_json: JSON.stringify({ profile: "hybrid", source: "operator-ui" }) }),
    });
    toast("Localization đã vào queue", `Run ${shortID(runData.run.id, 16)} · ${target.toUpperCase()}`, "success");
    await loadBaseData();
    await selectRun(runData.run.id, "jobs");
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
    state.selectedReviewItem = null;
    await loadReviewItems();
    renderInspector();
  } catch (error) { showError(error); }
  finally { setBusy(button, false); }
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
    toast("Text đã được sửa", "Douyinie đã invalidated đúng descendants và chạy lại targeted stages.", "success");
    await loadReviewItems();
    renderInspector();
  } catch (error) { showError(error); }
  finally { setBusy(button, false); }
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
    await loadReviewItems();
    renderInspector();
  } catch (error) { showError(error); }
  finally { setBusy(button, false); }
}

async function submitRegionCorrection(event) {
  event.preventDefault();
  if (!state.selectedRun || !state.selectedJob) return;
  const button = $("button[type=submit]", event.currentTarget);
  setBusy(button, true, "Đang cập nhật region…");
  try {
    const override = {
      region_id: $("#region-id").value.trim(),
      box_delta_x: Number($("#region-dx").value || 0),
      box_delta_y: Number($("#region-dy").value || 0),
      box_delta_w: Number($("#region-dw").value || 0),
      box_delta_h: Number($("#region-dh").value || 0),
      notes: $("#region-reason").value.trim(),
    };
    const role = $("#region-role").value;
    const text = $("#region-text").value.trim();
    if (role) override.new_role = role;
    if (text) override.new_text = text;
    await api(`/api/v1/runs/${encodeURIComponent(state.selectedRun.id)}/inspector/override-region`, {
      method: "POST",
      body: jsonBody({
        run_id: state.selectedRun.id,
        job_id: state.selectedJob.id,
        asset_id: state.selectedJob.source_asset_id,
        target_language: state.selectedJob.target_language,
        overrides: [override],
        scene_protected_regions: [],
        reason: $("#region-reason").value.trim(),
        operator: operatorName(),
      }),
    });
    toast("Text region đã cập nhật", "Chỉ visual/render descendants được targeted rerun.", "success");
    await loadReviewItems();
    renderInspector();
  } catch (error) { showError(error); }
  finally { setBusy(button, false); }
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
  const target = encodeURIComponent(state.selectedJob.target_language);
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
  setBusy(button, true, "Đang kiểm tra…");
  try {
    const data = await api(`/api/v1/runs/${encodeURIComponent(state.selectedRun.id)}/render/handoff`, {
      method: "POST",
      body: jsonBody({ posture: "review" }),
    });
    state.handoff = data.handoff || data;
    renderResult();
    if (state.handoff.can_start_final_render) toast("Final render ready", "Exception queue đã về zero.", "success");
    else toast("Cần review", `${state.handoff.pending_review_count ?? 0} exception còn pending.`);
  } catch (error) { showError(error); }
  finally { setBusy(button, false); renderResult(); }
}

async function startFinalRender(button) {
  if (!state.selectedRun || !state.selectedJob || !state.handoff?.can_start_final_render) return;
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
  } catch (error) { showError(error); }
  finally { setBusy(button, false); renderResult(); }
}

function setSourceMode(mode) {
  state.sourceMode = mode;
  $$("[data-source-mode]").forEach((button) => button.classList.toggle("is-active", button.dataset.sourceMode === mode));
  const input = $("#source-input");
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

function showError(error) {
  console.error(error);
  toast("Có lỗi", error?.message || String(error), "error");
}

function bindEvents() {
  $$("[data-view-target]").forEach((button) => button.addEventListener("click", () => navigate(button.dataset.viewTarget)));
  $$("[data-source-mode]").forEach((button) => button.addEventListener("click", () => setSourceMode(button.dataset.sourceMode)));
  $$("input[name=target_language]").forEach((radio) => radio.addEventListener("change", () => {
    $$(".language-option").forEach((label) => label.classList.toggle("is-selected", $("input", label).checked));
  }));
  $("#new-job-form").addEventListener("submit", handleNewJob);
  $("#refresh-all").addEventListener("click", () => refreshAll());
  $("#jobs-refresh").addEventListener("click", () => refreshAll());
  $("#result-refresh").addEventListener("click", () => refreshResult().catch(showError));
  $("#queue-body").addEventListener("click", (event) => {
    const button = event.target.closest("[data-select-run]");
    if (button) selectRun(button.dataset.selectRun).catch(showError);
  });
  $$("[data-run-action]").forEach((button) => button.addEventListener("click", () => runAction(button.dataset.runAction, button)));
  $("#include-resolved").addEventListener("change", async () => {
    try { await loadReviewItems(); renderInspector(); } catch (error) { showError(error); }
  });
  $("#exception-list").addEventListener("click", (event) => {
    const button = event.target.closest("[data-review-id]");
    if (!button) return;
    state.selectedReviewItem = state.reviewItems.find((item) => item.id === button.dataset.reviewId) || null;
    renderInspector();
  });
  $$("[data-editor-tab]").forEach((button) => button.addEventListener("click", () => setEditorTab(button.dataset.editorTab)));
  $("#accept-form").addEventListener("submit", submitManualOverride);
  $("#text-form").addEventListener("submit", submitTextCorrection);
  $("#voice-form").addEventListener("submit", submitVoiceCorrection);
  $("#region-form").addEventListener("submit", submitRegionCorrection);
  $("#check-handoff").addEventListener("click", (event) => checkHandoff(event.currentTarget));
  $("#start-final-render").addEventListener("click", (event) => startFinalRender(event.currentTarget));
}

async function boot() {
  bindEvents();
  setSourceMode(state.sourceMode);
  setEditorTab("accept");
  renderSelectedRun();
  renderInspector();
  renderResult();
  await refreshAll({ quiet: true });
  syncPolling();
}

boot().catch(showError);
