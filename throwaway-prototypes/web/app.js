/* THROWAWAY WEB PROTOTYPE — in-memory mock data only; no BOSS actions. */

const VIEWS = ["jobs", "assessment", "outreach", "resume"];
const VIEW_LABELS = { jobs: "岗位", assessment: "岗位鉴定", outreach: "首次沟通", resume: "在线简历" };
const JOB_PAGE_SIZES = [10, 20, 50, 100];
const DEFAULT_JOB_PAGE_SIZE = 10;
const TIME_WINDOW_PRESETS = [
  { id: "morning", label: "上午", start: "09:00", end: "12:00" },
  { id: "afternoon", label: "下午", start: "14:00", end: "18:00" },
  { id: "evening", label: "晚间", start: "19:00", end: "21:00" },
];
const ACTION_LABELS = {
  reassessment: "批量重新鉴定",
  review: "逐个人工复核",
  outreach_simulation: "安排模拟沟通",
  outreach_real: "安排真实发送",
};

let state = null;
let view = normalizedView(new URLSearchParams(location.search).get("view"));
let filters = { search: "", platform: "all", assessment: "all", human: "all", outreach: "all" };
let jobPage = normalizedPage(new URLSearchParams(location.search).get("jobPage"));
let jobPageSize = normalizedPageSize(new URLSearchParams(location.search).get("jobPageSize"));
let batchAction = null;
let directAction = false;
let selected = new Set();
let reviewIds = [];
let reviewIndex = 0;
let reviewResults = [];
let settingsDraft = null;
let pendingSettings = null;
let sessionPolicySuggestion = null;
let pendingPolicySuggestionLoss = null;
let timeWindowEditingIndex = null;
let toastTimer = null;

function normalizedView(value) {
  if (value === "execution") return "jobs";
  if (value === "settings") return "assessment";
  return VIEWS.includes(value) ? value : "jobs";
}

function normalizedPage(value) {
  const page = Number(value);
  return Number.isInteger(page) && page > 0 ? page : 1;
}

function normalizedPageSize(value) {
  const size = Number(value);
  return JOB_PAGE_SIZES.includes(size) ? size : DEFAULT_JOB_PAGE_SIZE;
}

function escapeHtml(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

function helpIcon(message, label = "查看说明") {
  return `<span class="help-icon" tabindex="0" role="note" aria-label="${escapeHtml(label)}" data-tooltip="${escapeHtml(message)}">?</span>`;
}

async function api(payload = null) {
  const response = await fetch(payload ? "/api/action" : "/api/state", {
    method: payload ? "POST" : "GET",
    headers: payload ? { "Content-Type": "application/json" } : {},
    body: payload ? JSON.stringify(payload) : undefined,
  });
  const body = await response.json();
  if (!response.ok || body.ok === false) throw new Error(body.error || "原型请求失败");
  return payload ? body.result : body;
}

async function refresh() {
  state = await api();
  render();
}

function showToast(message) {
  const toast = document.querySelector("#toast");
  toast.textContent = message;
  toast.classList.add("visible");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toast.classList.remove("visible"), 3000);
}

function statusChip(label, status, mode = null) {
  const modeChip = mode ? `<span class="chip mode-chip">${escapeHtml(mode)}</span>` : "";
  return `<span class="chip status-${escapeHtml(status)}">${escapeHtml(label)}</span>${modeChip}`;
}

function assessmentCell(job) {
  return statusChip(job.assessment_label, job.assessment_status);
}

function humanReviewCell(job) {
  if (!job.human_verdict) {
    return `<div class="human-review-cell unreviewed"><span>未复核</span><strong>—</strong></div>`;
  }
  const verdict = job.human_verdict === "suitable" ? "适合" : "不适合";
  if (!job.human_review_current) {
    return `<div class="human-review-cell stale"><span>待重新复核</span><strong>上次：${verdict}</strong></div>`;
  }
  return `<div class="human-review-cell reviewed ${job.human_verdict}"><span>已复核</span><strong>${verdict}</strong></div>`;
}

function appSidebar() {
  return `<div class="prototype-banner">THROWAWAY WEB PROTOTYPE · 内存模拟数据 · 不连接 BOSS · 不执行真实沟通</div>
  <aside class="app-sidebar">
    <div class="sidebar-brand"><strong>BOSS Job Agent</strong><span>本地个人实例</span></div>
    <nav aria-label="主导航">
      <span class="nav-group-label">工作台</span>
      <button class="sidebar-nav-item ${view === "jobs" ? "active" : ""}" data-view="jobs"><span class="nav-symbol">01</span><span><strong>岗位</strong><small>进度、筛选与批量处理</small></span><b>›</b></button>
      <span class="nav-group-label">自动化</span>
      <button class="sidebar-nav-item ${view === "assessment" ? "active" : ""}" data-view="assessment"><span class="nav-symbol">02</span><span><strong>岗位鉴定</strong><small>当前策略与自动鉴定</small></span><b>›</b></button>
      <button class="sidebar-nav-item ${view === "outreach" ? "active" : ""}" data-view="outreach"><span class="nav-symbol">03</span><span><strong>首次沟通</strong><small>模式、招呼语与时间</small></span><b>›</b></button>
      <span class="nav-group-label">资料</span>
      <button class="sidebar-nav-item ${view === "resume" ? "active" : ""}" data-view="resume"><span class="nav-symbol">04</span><span><strong>在线简历</strong><small>唯一手工刷新入口</small></span><b>›</b></button>
    </nav>
    <button class="sidebar-reset" data-action="reset">重置模拟数据</button>
  </aside>`;
}

function navigate(nextView, nextFilters = null, skipSuggestionGuard = false) {
  if (sessionPolicySuggestion && view === "assessment" && nextView !== "assessment" && !skipSuggestionGuard) {
    requestPolicySuggestionLoss({ kind: "leave", nextView, nextFilters });
    return;
  }
  view = normalizedView(nextView);
  if (nextFilters) filters = { search: "", platform: "all", assessment: "all", human: "all", outreach: "all", ...nextFilters };
  if (view !== "jobs") settingsDraft = draftFromState();
  if (view === "jobs") jobPage = 1;
  batchAction = null;
  directAction = false;
  selected.clear();
  const url = new URL(location.href);
  url.searchParams.set("view", view);
  if (view === "jobs") {
    url.searchParams.set("jobPage", jobPage);
    url.searchParams.set("jobPageSize", jobPageSize);
  } else {
    url.searchParams.delete("jobPage");
    url.searchParams.delete("jobPageSize");
  }
  url.searchParams.delete("assessmentPage");
  url.searchParams.delete("outreachPage");
  history.replaceState({}, "", url);
  render();
}

function countJobs(predicate) {
  return state.jobs.filter(predicate).length;
}

function eligibilityFor(job, action) {
  if (action === "review") return job.review_eligibility;
  return job.batch_eligibility[action];
}

function getEligibility(job) {
  return eligibilityFor(job, batchAction);
}

function filteredJobs() {
  const query = filters.search.trim().toLowerCase();
  return state.jobs.filter((job) => {
    if (query && !`${job.title} ${job.company} ${job.city}`.toLowerCase().includes(query)) return false;
    if (filters.platform !== "all" && job.platform_status !== filters.platform) return false;
    if (filters.assessment === "unfinished") {
      if (!["not_queued", "pending", "processing", "failed"].includes(job.assessment_status)) return false;
    } else if (filters.assessment === "review") {
      if (!job.review_attention) return false;
    } else if (filters.assessment === "active") {
      if (!["pending", "processing"].includes(job.assessment_status)) return false;
    } else if (filters.assessment !== "all" && job.assessment_status !== filters.assessment) return false;
    if (filters.human === "reviewed") {
      if (!job.human_verdict || !job.human_review_current) return false;
    } else if (filters.human === "unreviewed") {
      if (job.human_verdict) return false;
    } else if (filters.human === "stale") {
      if (!job.human_verdict || job.human_review_current) return false;
    } else if (filters.human === "suitable") {
      if (job.human_verdict !== "suitable" || !job.human_review_current) return false;
    } else if (filters.human === "unsuitable") {
      if (job.human_verdict !== "unsuitable" || !job.human_review_current) return false;
    }
    if (filters.outreach === "uncontacted") {
      if (job.outreach_status === "contacted") return false;
    } else if (filters.outreach === "active") {
      if (!["pending", "processing", "failed", "possibly_contacted"].includes(job.outreach_status)) return false;
    } else if (filters.outreach !== "all" && job.outreach_status !== filters.outreach) return false;
    return true;
  });
}

function paginatedJobs(jobs = filteredJobs()) {
  const totalPages = Math.max(1, Math.ceil(jobs.length / jobPageSize));
  jobPage = Math.min(jobPage, totalPages);
  const start = (jobPage - 1) * jobPageSize;
  return { items: jobs.slice(start, start + jobPageSize), start, totalPages, totalItems: jobs.length };
}

function syncJobPaginationUrl() {
  const url = new URL(location.href);
  url.searchParams.set("view", "jobs");
  url.searchParams.set("jobPage", jobPage);
  url.searchParams.set("jobPageSize", jobPageSize);
  url.searchParams.delete("assessmentPage");
  url.searchParams.delete("outreachPage");
  history.replaceState({}, "", url);
}

function setJobPage(nextPage) {
  jobPage = normalizedPage(nextPage);
  syncJobPaginationUrl();
  render();
  document.querySelector(".jobs-table")?.scrollIntoView({ behavior: "smooth", block: "start" });
}

function setJobPageSize(nextSize) {
  jobPageSize = normalizedPageSize(nextSize);
  jobPage = 1;
  selected.clear();
  syncJobPaginationUrl();
  render();
}

function jobPagination(page) {
  const pageButtons = Array.from({ length: page.totalPages }, (_, index) => index + 1)
    .map((number) => `<button class="page-number ${number === jobPage ? "active" : ""}" data-job-page="${number}" ${number === jobPage ? 'aria-current="page"' : ""}>${number}</button>`)
    .join("");
  return `<div class="job-pagination"><div class="pagination-meta"><span>共 ${page.totalItems} 个岗位</span><label class="page-size-control"><span>每页</span><select id="job-page-size" aria-label="每页岗位数量">${JOB_PAGE_SIZES.map((size) => `<option value="${size}" ${size === jobPageSize ? "selected" : ""}>${size}</option>`).join("")}</select><span>条</span></label></div><div class="page-controls"><button class="page-nav" data-job-page="${jobPage - 1}" ${jobPage === 1 ? "disabled" : ""}>上一页</button>${pageButtons}<button class="page-nav" data-job-page="${jobPage + 1}" ${jobPage === page.totalPages ? "disabled" : ""}>下一页</button></div></div>`;
}

function selectOptions(options, current) {
  return options.map(([value, label]) => `<option value="${value}" ${value === current ? "selected" : ""}>${label}</option>`).join("");
}

function filterBar() {
  return `<div class="filter-bar">
    <label class="search-field"><span>搜索岗位或公司</span><input id="job-search" value="${escapeHtml(filters.search)}" placeholder="例如：Go、平台后端" /></label>
    <label><span>平台状态</span><select id="platform-filter">${selectOptions([["all","全部"],["open","可沟通"],["closed","已关闭"]], filters.platform)}</select></label>
    <label><span>AI 鉴定状态</span><select id="assessment-filter">${selectOptions([
      ["all","全部"],["unfinished","未完成鉴定"],["not_queued","尚未安排"],["active","等待或处理中"],["pending","待鉴定"],["processing","鉴定中"],["review","建议人工确认"],["suitable","AI 适合"],["unsuitable","AI 不适合"],["failed","鉴定失败"]
    ], filters.assessment)}</select></label>
    <label><span>人工结论</span><select id="human-filter">${selectOptions([
      ["all","全部"],["reviewed","已复核"],["unreviewed","未复核"],["suitable","人工适合"],["unsuitable","人工不适合"],["stale","待重新复核"]
    ], filters.human)}</select></label>
    <label><span>首次沟通状态</span><select id="outreach-filter">${selectOptions([
      ["all","全部"],["uncontacted","尚未真实沟通"],["not_queued","尚未安排"],["active","等待或处理中"],["simulated","模拟完成"],["contacted","已真实沟通"],["failed","沟通失败"],["possibly_contacted","可能已沟通"]
    ], filters.outreach)}</select></label>
    <button class="button ghost compact" data-action="clear-filters">清除筛选</button>
  </div>`;
}

function batchToolbar() {
  if (batchAction) {
    return `<div class="batch-workspace active">
      <div><span class="batch-step">批量模式</span><strong>${ACTION_LABELS[batchAction]}</strong>${helpIcon("不可处理的岗位会直接禁用；提交时仍会重新校验状态。")}</div>
      <div class="action-row"><button class="button ghost compact" data-action="select-all-eligible">全选本页可选岗位</button><button class="text-button" data-action="cancel-batch">退出操作</button><button class="button primary compact" data-action="submit-batch" ${selected.size ? "" : "disabled"}>继续处理 ${selected.size} 个</button></div>
    </div>`;
  }
  return `<div class="batch-workspace">
    <div><span class="batch-step">批量操作</span><strong>处理多个岗位</strong></div>
    <div class="batch-action-buttons">
      <button data-batch-action="review">人工复核</button>
      <button data-batch-action="reassessment">重新鉴定</button>
      <button data-batch-action="outreach_simulation">模拟沟通</button>
      <button data-batch-action="outreach_real">真实发送</button>
    </div>
  </div>`;
}

function directJobActions(job) {
  return [
    ["review", "人工复核"],
    ["reassessment", "重新鉴定"],
    ["outreach_simulation", "模拟沟通"],
    ["outreach_real", "真实发送"],
  ].map(([action, label]) => {
    const eligibility = eligibilityFor(job, action);
    return `<button class="row-action-button" data-direct-job-action="${action}" data-direct-job-id="${job.id}" ${eligibility.eligible ? "" : "disabled"} title="${escapeHtml(eligibility.reason)}">${label}</button>`;
  }).join("");
}

function jobRows(jobs) {
  if (!jobs.length) return `<tr><td colspan="${batchAction ? 8 : 6}"><div class="empty-state"><strong>当前筛选下没有岗位</strong><span>请调整筛选条件。</span></div></td></tr>`;
  return jobs.map((job) => {
    const eligibility = batchAction ? getEligibility(job) : null;
    const checkbox = batchAction
      ? `<input type="checkbox" data-job-id="${job.id}" aria-label="选择 ${escapeHtml(job.title)}" ${selected.has(job.id) ? "checked" : ""} ${eligibility.eligible ? "" : "disabled"} />`
      : "";
    return `<tr>
      ${batchAction ? `<td>${checkbox}</td>` : ""}
      <td><button class="job-title" data-job-detail="${job.id}">${escapeHtml(job.title)}</button><span class="job-subline">${escapeHtml(job.company)} · ${escapeHtml(job.city)} · ${escapeHtml(job.salary)}</span></td>
      <td>${statusChip(job.platform_status === "open" ? "可沟通" : "已关闭", job.platform_status)}</td>
      <td>${assessmentCell(job)}</td>
      <td>${humanReviewCell(job)}</td>
      <td>${statusChip(job.outreach_label, job.outreach_status)}</td>
      <td><div class="row-actions">${directJobActions(job)}</div></td>
      ${batchAction ? `<td class="reason-cell"><span class="eligibility ${eligibility.eligible ? "yes" : "no"}">${eligibility.eligible ? "可选" : "不可选"} · ${escapeHtml(eligibility.reason)}</span></td>` : ""}
    </tr>`;
  }).join("");
}

function renderJobs() {
  const jobs = filteredJobs();
  const page = paginatedJobs(jobs);
  const firstVisible = page.totalItems ? page.start + 1 : 0;
  const lastVisible = page.start + page.items.length;
  return `${appSidebar()}<main class="page jobs-page">
    <div class="page-title-row">
      <div><span class="eyebrow">工作台</span><h1>岗位</h1></div>
    </div>
    ${jobOverview()}
    ${filterBar()}
    ${batchToolbar()}
    <div class="list-summary"><strong>第 ${firstVisible}–${lastVisible} 个，共 ${page.totalItems} 个岗位</strong><span>第 ${jobPage} / ${page.totalPages} 页</span></div>
    <div class="table-wrap"><table class="jobs-table ${batchAction ? "batching" : ""}"><colgroup>${batchAction ? '<col style="width:38px" />' : ""}<col style="width:23%" /><col style="width:8%" /><col style="width:11%" /><col style="width:11%" /><col style="width:11%" /><col style="width:24%" />${batchAction ? "<col style=\"width:18%\" />" : ""}</colgroup><thead><tr>${batchAction ? "<th></th>" : ""}<th>岗位</th><th>平台</th><th>AI 鉴定</th><th>人工结论</th><th>首次沟通</th><th>操作</th>${batchAction ? "<th>本次操作可选性</th>" : ""}</tr></thead><tbody>${jobRows(page.items)}</tbody></table></div>
    ${jobPagination(page)}
  </main>`;
}

function moduleStatus(label, tone = "neutral") {
  return `<span class="module-status ${tone}">${escapeHtml(label)}</span>`;
}

function jobOverview() {
  const run = state.discovery_run;
  const activeRun = ["preparing", "running", "paused", "failed"].includes(run.status);
  const savedResumeVersion = state.online_resume.current_version;
  const discoveryDone = run.status === "not_started" ? 0 : run.scopes_done;
  const discoveryTotal = run.status === "not_started" ? 0 : run.scopes_total;
  const discoveryRemaining = Math.max(0, discoveryTotal - discoveryDone);
  const assessmentUnfinishedStatuses = ["not_queued", "pending", "processing", "failed"];
  const assessmentUnfinished = state.jobs.filter((job) => assessmentUnfinishedStatuses.includes(job.assessment_status));
  const assessmentDone = state.jobs.length - assessmentUnfinished.length;
  const assessmentProcessing = state.jobs.filter((job) => job.assessment_status === "processing");
  const contacted = countJobs((job) => job.outreach_status === "contacted");
  const uncontacted = state.jobs.length - contacted;
  const outreachActive = countJobs((job) => ["pending", "processing"].includes(job.outreach_status));
  const discoveryVersionWarning = activeRun && savedResumeVersion !== run.online_resume_version_used
    ? `<p class="version-boundary-note">当前已保存在线简历 v${savedResumeVersion}，但本轮发现仍使用 v${run.online_resume_version_used}；v${savedResumeVersion} 从下一轮发现开始使用。</p>`
    : "";
  const discoveryAction = activeRun
    ? `<div class="action-row">${run.status === "running" ? `<button class="button ghost compact" data-action="pause-discovery">暂停</button>` : `<button class="button primary compact" data-action="continue-discovery">继续发现</button>`}<button class="text-button danger" data-action="open-end-discovery">提前结束</button></div>`
    : state.discovery_can_start
      ? `<button class="button primary compact" data-action="open-start-discovery">创建岗位发现</button>`
      : `<button class="button primary compact" disabled>创建岗位发现</button><span class="disabled-explanation">先手动刷新在线简历</span><button class="text-button" data-view="resume">去在线简历</button>`;
  const runTitle = run.status === "not_started" ? "尚未创建岗位发现" : `运行 #${run.id} · ${escapeHtml(run.name)}`;
  const runProgress = run.status === "not_started"
    ? `<div class="empty-progress"><strong>还没有发现任务</strong><span>创建后，这里只显示当前位置和剩余范围。</span></div>`
    : `<div class="progress-summary">
        <div class="progress-number"><strong>${discoveryDone}/${discoveryTotal}</strong><span>搜索范围已完成</span></div>
        <div class="progress-content"><div class="progress-track"><span style="width:${discoveryTotal ? Math.round((discoveryDone / discoveryTotal) * 100) : 0}%"></span></div><p>正在检查：<strong>${escapeHtml(run.current_role)} · ${escapeHtml(run.current_city)} · 第 ${run.next_page} 页</strong></p><span>剩余 ${discoveryRemaining} 个搜索范围 · 已发现 ${run.jobs_observed} 个岗位</span></div>
      </div>`;
  const currentAssessment = assessmentProcessing.length
    ? `正在鉴定：${assessmentProcessing.map((job) => escapeHtml(job.title)).join("、")}`
    : assessmentUnfinished.length
      ? "当前没有岗位正在鉴定，剩余岗位尚未安排或正在等待。"
      : "所有岗位都已有鉴定结果。";
  return `<section class="job-overview" aria-label="运行概览">
    <div class="overview-heading"><h2>运行概览</h2></div>
    <div class="overview-grid">
      <article class="overview-card discovery-card">
        <div class="overview-card-head"><div><span>01</span><h3>岗位发现</h3></div>${moduleStatus(run.status_label, run.status === "failed" ? "danger" : run.status === "running" ? "good" : "warning")}</div>
        <strong class="overview-title">${runTitle}</strong>
        ${runProgress}
        ${discoveryVersionWarning}
        <div class="overview-actions">${discoveryAction}</div>
      </article>
      <article class="overview-card">
        <div class="overview-card-head"><div><span>02</span><h3>岗位鉴定</h3></div><span class="overview-count">还剩 ${assessmentUnfinished.length} 个</span></div>
        <div class="overview-ratio"><strong>${assessmentDone}</strong><span>/ ${state.jobs.length} 已完成</span></div>
        <div class="progress-track"><span style="width:${state.jobs.length ? Math.round((assessmentDone / state.jobs.length) * 100) : 0}%"></span></div>
        <p>${currentAssessment}</p>
        <button class="button ghost compact" data-job-preset="assessment-unfinished">筛出未完成鉴定的岗位</button>
      </article>
      <article class="overview-card">
        <div class="overview-card-head"><div><span>03</span><h3>首次沟通</h3>${helpIcon("进度只统计真实沟通；Simulation 完成不计入已沟通。")}</div><span class="overview-count">还剩 ${uncontacted} 个</span></div>
        <div class="overview-ratio"><strong>${contacted}</strong><span>/ ${state.jobs.length} 已真实沟通</span></div>
        <div class="progress-track"><span style="width:${state.jobs.length ? Math.round((contacted / state.jobs.length) * 100) : 0}%"></span></div>
        <p>${outreachActive ? `${outreachActive} 个正在等待或处理` : "当前没有正在处理的岗位"}</p>
        <div class="action-row"><button class="button ghost compact" data-job-preset="outreach-uncontacted">筛出未沟通</button><button class="text-button" data-job-preset="outreach-contacted">查看已沟通</button></div>
      </article>
    </div>
  </section>`;
}

function draftFromState() {
  return {
    assessmentEnabled: state.settings.automatic_assessment_enabled,
    assessmentLimit: state.settings.assessment_processing_limit,
    outreachEnabled: state.settings.automatic_outreach_enabled,
    outreachMode: state.settings.automatic_outreach_mode,
    greetingText: state.settings.outreach_greeting_text || "",
    timeWindows: state.settings.outreach_time_windows.map((item) => ({ ...item })),
  };
}

function renderAssessmentSettings() {
  if (!settingsDraft) settingsDraft = draftFromState();
  const policy = state.assessment_policy;
  const validHumanLabels = state.jobs.filter((job) => job.human_verdict && job.human_review_current).length;
  return `${appSidebar()}<main class="page settings-page">
    <div class="page-title-row">
      <div><span class="eyebrow">自动化</span><h1>岗位鉴定</h1></div>
      <div class="page-primary-actions"><button class="button secondary" data-action="generate-policy-suggestion">生成新策略</button><button class="button primary" data-action="save-assessment-settings">保存鉴定设置</button></div>
    </div>
    <section class="settings-section">
      <div class="settings-intro"><div class="heading-with-help"><h2>当前策略</h2>${helpIcon(`“生成新策略”会使用当前策略和现有的 ${validHumanLabels} 条有效人工标注；人工复核本身不会调用模型。`)}</div></div>
      <div class="settings-form">
        <div class="policy-box"><span>当前启用</span><strong>${escapeHtml(policy.display_name)}</strong><p>${escapeHtml(policy.version_note)}</p><ul>${policy.rules.map((rule) => `<li>${escapeHtml(rule)}</li>`).join("")}</ul></div>
      </div>
    </section>
    <section class="settings-section">
      <div class="settings-intro"><h2>自动鉴定</h2></div>
      <div class="settings-form">
        <label class="switch-row"><span><strong>自动岗位鉴定</strong>${helpIcon("关闭不影响岗位发现；新岗位只保存为尚未安排。岗位真正开始鉴定时才读取当前在线简历和当前策略。")}</span><input id="automatic-assessment" type="checkbox" ${settingsDraft.assessmentEnabled ? "checked" : ""} /></label>
        <label class="field-stack concurrency-field"><span>AI 同时鉴定数</span><div class="number-with-unit"><input id="assessment-limit" type="number" inputmode="numeric" min="1" step="1" value="${settingsDraft.assessmentLimit}" /><span>个岗位</span></div></label>
      </div>
    </section>
  </main>`;
}

function renderOutreachSettings() {
  if (!settingsDraft) settingsDraft = draftFromState();
  const windows = settingsDraft.timeWindows.length
    ? settingsDraft.timeWindows.map((window, index) => `<div class="saved-time-window"><button data-action="edit-time-window" data-window-index="${index}"><strong>${escapeHtml(window.start)}–${escapeHtml(window.end)}</strong><span>编辑</span></button><button class="remove-time-window" data-action="remove-time-window" data-window-index="${index}" aria-label="删除 ${escapeHtml(window.start)} 至 ${escapeHtml(window.end)}">×</button></div>`).join("")
    : `<div class="all-day-window"><strong>全天可发送</strong><span>没有时间限制</span></div>`;
  return `${appSidebar()}<main class="page settings-page">
    <div class="page-title-row"><div><span class="eyebrow">自动化</span><h1>首次沟通</h1></div><button class="button primary" data-action="save-outreach-settings">保存首次沟通设置</button></div>
    <section class="settings-section">
      <div class="settings-intro"><h2>自动沟通</h2></div>
      <div class="settings-form">
        <label class="switch-row"><span><strong>自动首次沟通</strong>${helpIcon("开启后会持续处理当前判断为适合且仍可沟通的岗位；手工真实发送不会修改这个开关。")}</span><input id="automatic-outreach" type="checkbox" ${settingsDraft.outreachEnabled ? "checked" : ""} /></label>
        <fieldset class="mode-choice"><legend>自动沟通模式 ${helpIcon("Simulation 只演练，不联系招聘者；Real 会进入真实发送队列。")}</legend><label><input type="radio" name="outreach-mode" value="simulation" ${settingsDraft.outreachMode === "simulation" ? "checked" : ""} /> Simulation</label><label><input type="radio" name="outreach-mode" value="real" ${settingsDraft.outreachMode === "real" ? "checked" : ""} /> Real</label></fieldset>
        <label class="field-stack"><span>固定招呼语</span><textarea id="greeting-text" rows="4" placeholder="尚未配置">${escapeHtml(settingsDraft.greetingText)}</textarea></label>
      </div>
    </section>
    <section class="settings-section">
      <div class="settings-intro"><div class="heading-with-help"><h2>真实发送时间</h2>${helpIcon("时间窗只限制何时开始新的真实发送；不设置任何时间窗就是全天可发送。")}</div></div>
      <div class="settings-form">
        <div class="saved-time-window-list">${windows}</div>
        <button class="button ghost compact add-time-window" data-action="add-time-window">＋ 添加时间窗</button>
      </div>
    </section>
  </main>`;
}

function renderResumeSettings() {
  const resume = state.online_resume;
  const resumeSummary = resume.current_version == null
    ? `<div class="resume-box missing"><span>当前版本</span><strong>尚未保存在线简历</strong><p>开始岗位发现前必须由你手动刷新一次。</p></div>`
    : `<div class="resume-box"><span>当前版本</span><strong>在线简历 v${resume.current_version}</strong><p>保存时间：${escapeHtml(resume.saved_at)}</p></div>`;
  const resumeError = resume.last_refresh_error ? `<div class="resume-error"><strong>最近一次刷新失败</strong><span>${escapeHtml(resume.last_refresh_error)}</span></div>` : "";
  const activeDiscovery = ["preparing", "running", "paused", "failed"].includes(state.discovery_run.status);
  const resumeBoundary = activeDiscovery && resume.current_version !== state.discovery_run.online_resume_version_used
    ? `<div class="version-boundary-note"><strong>当前已保存 v${resume.current_version}；本轮发现仍使用 v${state.discovery_run.online_resume_version_used}</strong><span>v${resume.current_version} 将用于下一次发现和尚未开始的岗位鉴定。重复岗位 JD 判断内容未变时仍复用原结论。</span></div>`
    : activeDiscovery ? `<div class="version-boundary-note"><strong>本轮发现固定使用在线简历 v${state.discovery_run.online_resume_version_used}</strong><span>暂停恢复仍使用同一版本，运行中不提供切换入口。</span></div>` : "";
  return `${appSidebar()}<main class="page settings-page">
    <div class="page-title-row"><div><span class="eyebrow">资料</span><h1>在线简历</h1><p>这是唯一的手工刷新入口；发现和鉴定过程中不会自动读取 BOSS。</p></div></div>
    <section class="settings-section single-settings-section">
      <div class="settings-intro"><h2>当前保存版本</h2><p>内容变化才新增版本；失败时保留旧版本。</p></div>
      <div class="settings-form">
        ${resumeSummary}${resumeError}${resumeBoundary}
        <div class="resume-refresh-row"><label class="field-stack"><span>THROWAWAY：模拟本次 BOSS 读取结果</span><select id="resume-refresh-outcome"><option value="changed">成功，内容有变化</option><option value="unchanged">成功，内容未变化</option><option value="failed">读取失败</option></select></label><button class="button primary" data-action="refresh-online-resume">刷新在线简历</button></div>
        <p class="plain-note">只有点击上面的按钮才模拟读取一次。保存给 Agent 的是完整在线简历内容，不是内部编号。</p>
        <button class="text-button prototype-only-action" data-action="simulate-first-use">切换到“首次尚无版本”演示状态</button>
      </div>
    </section>
  </main>`;
}

function render() {
  if (!state) return;
  const renderers = { jobs: renderJobs, assessment: renderAssessmentSettings, outreach: renderOutreachSettings, resume: renderResumeSettings };
  document.querySelector("#app").innerHTML = renderers[view]();
}

function collectAssessmentSettings() {
  return {
    assessmentEnabled: document.querySelector("#automatic-assessment").checked,
    assessmentLimit: Number(document.querySelector("#assessment-limit").value),
  };
}

function collectOutreachSettings() {
  return {
    outreachEnabled: document.querySelector("#automatic-outreach").checked,
    outreachMode: document.querySelector('input[name="outreach-mode"]:checked').value,
    greetingText: document.querySelector("#greeting-text").value.trim(),
    timeWindows: settingsDraft.timeWindows.map((window) => ({ ...window })),
  };
}

function timeSelectOptions(selected) {
  const values = [];
  for (let hour = 0; hour < 24; hour += 1) {
    for (const minute of ["00", "30"]) values.push(`${String(hour).padStart(2, "0")}:${minute}`);
  }
  return values.map((value) => `<option value="${value}" ${value === selected ? "selected" : ""}>${value}</option>`).join("");
}

function openTimeWindowEditor(index = null) {
  timeWindowEditingIndex = index;
  const window = index == null ? TIME_WINDOW_PRESETS[0] : settingsDraft.timeWindows[index];
  document.querySelector("#time-window-editor-title").textContent = index == null ? "添加发送时间窗" : "编辑发送时间窗";
  document.querySelector("#time-window-start").innerHTML = timeSelectOptions(window.start);
  document.querySelector("#time-window-end").innerHTML = timeSelectOptions(window.end);
  document.querySelector("#time-window-editor-dialog").showModal();
}

function chooseTimeWindowPreset(presetId) {
  const preset = TIME_WINDOW_PRESETS.find((item) => item.id === presetId);
  if (!preset) return;
  document.querySelector("#time-window-start").value = preset.start;
  document.querySelector("#time-window-end").value = preset.end;
}

function saveTimeWindowEditor() {
  const start = document.querySelector("#time-window-start").value;
  const end = document.querySelector("#time-window-end").value;
  if (start >= end) {
    showToast("结束时间必须晚于开始时间。");
    return;
  }
  settingsDraft = { ...settingsDraft, ...collectOutreachSettings() };
  const nextWindow = { start, end };
  if (timeWindowEditingIndex == null) settingsDraft.timeWindows.push(nextWindow);
  else settingsDraft.timeWindows[timeWindowEditingIndex] = nextWindow;
  document.querySelector("#time-window-editor-dialog").close();
  timeWindowEditingIndex = null;
  render();
}

function removeTimeWindow(index) {
  settingsDraft = { ...settingsDraft, ...collectOutreachSettings() };
  settingsDraft.timeWindows.splice(index, 1);
  render();
}

function closeDialogs() {
  document.querySelectorAll("dialog[open]").forEach((dialog) => dialog.close());
}

function showBatchResult(result, wasDirect = false) {
  document.querySelector("#batch-result-title").textContent = wasDirect ? "岗位操作结果" : "批量操作结果";
  document.querySelector("#batch-result-summary").textContent = result.summary;
  document.querySelector("#batch-result-reasons").innerHTML = result.skipped.length
    ? result.skipped.map((item) => `<div class="result-reason"><strong>${escapeHtml(item.title)}</strong><span>${escapeHtml(item.reason)}</span></div>`).join("")
    : `<div class="result-success">全部选中岗位均处理成功。</div>`;
  document.querySelector("#batch-result-dialog").showModal();
}

async function executeBatch() {
  const action = batchAction;
  const ids = [...selected];
  const wasDirect = directAction;
  const result = action === "reassessment"
    ? await api({ action: "queue_assessment", job_ids: ids })
    : await api({ action: "queue_outreach", job_ids: ids, mode: action === "outreach_real" ? "real" : "simulation" });
  selected.clear();
  batchAction = null;
  directAction = false;
  await refresh();
  showBatchResult(result, wasDirect);
}

async function submitBatch() {
  if (!selected.size) return;
  if (batchAction === "reassessment") {
    document.querySelector("#assessment-confirm-count").textContent = `${selected.size} 个`;
    document.querySelector("#assessment-confirm-dialog").showModal();
    return;
  }
  if (batchAction === "review") {
    reviewIds = [...selected];
    reviewIndex = 0;
    reviewResults = [];
    renderReviewStep();
    document.querySelector("#review-dialog").showModal();
    return;
  }
  if (batchAction === "outreach_real") {
    document.querySelector("#manual-real-count").textContent = selected.size;
    document.querySelector("#manual-real-greeting").textContent = state.settings.outreach_greeting_text || "未配置";
    document.querySelector("#manual-real-windows").textContent = state.settings.outreach_time_windows.length
      ? state.settings.outreach_time_windows.map((item) => `${item.start}-${item.end}`).join("、")
      : "全天";
    document.querySelector("#manual-real-dialog").showModal();
    return;
  }
  await executeBatch();
}

async function submitDirectJobAction(action, jobId) {
  const job = state.jobs.find((item) => item.id === jobId);
  if (!job) return;
  const eligibility = eligibilityFor(job, action);
  if (!eligibility.eligible) {
    showToast(eligibility.reason);
    return;
  }
  directAction = true;
  batchAction = action;
  selected = new Set([jobId]);
  await submitBatch();
}

function renderReviewStep() {
  const job = state.jobs.find((item) => item.id === reviewIds[reviewIndex]);
  if (!job) return finishReview();
  document.querySelector("#review-progress").textContent = `第 ${reviewIndex + 1} / ${reviewIds.length} 个`;
  document.querySelector("#review-title").textContent = job.title;
  document.querySelector("#review-company").textContent = `${job.company} · ${job.city} · ${job.salary}`;
  document.querySelector("#review-platform-status").textContent = job.platform_status === "open" ? "可沟通" : "已关闭";
  document.querySelector("#review-platform-status").className = `module-status ${job.platform_status === "open" ? "good" : "danger"}`;
  document.querySelector("#review-responsibilities").innerHTML = job.jd.responsibilities.map((item) => `<li>${escapeHtml(item)}</li>`).join("");
  document.querySelector("#review-requirements").innerHTML = job.jd.requirements.map((item) => `<li>${escapeHtml(item)}</li>`).join("");
  document.querySelector("#review-ai-verdict").textContent = job.assessment_label;
  document.querySelector("#review-reason").textContent = job.assessment_reason || "当前没有可展示的 AI 理由。";
  document.querySelector("#review-assessment-usage").textContent = job.assessment_usage_note || "当前没有已完成 AI 鉴定的实际版本信息。";
  document.querySelector("#review-human-verdict").textContent = job.human_verdict
    ? `${job.human_verdict === "suitable" ? "适合" : "不适合"}${job.human_review_current ? "（当前有效，可再次覆盖）" : "（所依据 JD 已变化）"}`
    : "尚无人工结论";
}

function showPolicyCandidate(candidate) {
  if (!candidate.available) {
    sessionPolicySuggestion = null;
    showToast(candidate.message);
    return;
  }
  sessionPolicySuggestion = candidate;
  document.querySelector("#policy-candidate-count").textContent = `${candidate.source_label_count} 条`;
  document.querySelector("#policy-candidate-label-split").textContent = `${candidate.positive_label_count} 条适合 / ${candidate.negative_label_count} 条不适合`;
  document.querySelector("#policy-candidate-input-help").dataset.tooltip = candidate.input_scope_note;
  document.querySelector("#policy-candidate-full-text").value = candidate.candidate.full_text;
  document.querySelector(".policy-candidate-editor").open = false;
  renderPolicyTextDiff(candidate.candidate.full_text);
  const accept = document.querySelector("#activate-policy-version-button");
  accept.disabled = false;
  accept.textContent = "采用策略";
  document.querySelector("#policy-candidate-dialog").showModal();
  document.querySelector("#policy-text-diff").focus({ preventScroll: true });
}

function currentPolicyFullText() {
  return state.assessment_policy.full_text || [
    state.assessment_policy.name,
    "",
    "目标：根据已保存的在线简历与岗位 JD，给出保守、可追溯的三态结论。",
    "",
    "完整判定规则：",
    ...state.assessment_policy.rules.map((rule, index) => `${index + 1}. ${rule}`),
    "",
    "输出只能是：适合 / 不适合 / 需要人工确认，并附上支持结论的 JD 证据。",
  ].join("\n");
}

function diffPolicyLines(beforeText, afterText) {
  const before = beforeText.replaceAll("\r\n", "\n").split("\n");
  const after = afterText.replaceAll("\r\n", "\n").split("\n");
  const lengths = Array.from({ length: before.length + 1 }, () => Array(after.length + 1).fill(0));
  for (let left = before.length - 1; left >= 0; left -= 1) {
    for (let right = after.length - 1; right >= 0; right -= 1) {
      lengths[left][right] = before[left] === after[right]
        ? lengths[left + 1][right + 1] + 1
        : Math.max(lengths[left + 1][right], lengths[left][right + 1]);
    }
  }
  const result = [];
  let left = 0;
  let right = 0;
  while (left < before.length || right < after.length) {
    if (left < before.length && right < after.length && before[left] === after[right]) {
      result.push({ kind: "unchanged", marker: " ", text: before[left] });
      left += 1;
      right += 1;
    } else if (left < before.length && (right === after.length || lengths[left + 1][right] >= lengths[left][right + 1])) {
      result.push({ kind: "removed", marker: "−", text: before[left] });
      left += 1;
    } else {
      result.push({ kind: "added", marker: "+", text: after[right] });
      right += 1;
    }
  }
  return result;
}

function renderPolicyTextDiff(candidateText) {
  document.querySelector("#policy-diff-version").textContent = `${state.assessment_policy.display_name} → 候选策略 · 本地文本比较`;
  document.querySelector("#policy-text-diff").innerHTML = diffPolicyLines(currentPolicyFullText(), candidateText)
    .map((line) => `<div class="policy-diff-line ${line.kind}"><span>${line.marker}</span><code>${line.text ? escapeHtml(line.text) : "&nbsp;"}</code></div>`)
    .join("");
}

async function generatePolicySuggestion() {
  if (sessionPolicySuggestion) {
    requestPolicySuggestionLoss({ kind: "regenerate" });
    return;
  }
  const candidate = await api({ action: "generate_policy_suggestion" });
  showPolicyCandidate(candidate);
}

function capturePolicySuggestionEdit() {
  if (!sessionPolicySuggestion?.available) return;
  sessionPolicySuggestion.candidate.full_text = document.querySelector("#policy-candidate-full-text").value;
  renderPolicyTextDiff(sessionPolicySuggestion.candidate.full_text);
}

function requestPolicySuggestionLoss(request) {
  if (!sessionPolicySuggestion) return;
  capturePolicySuggestionEdit();
  pendingPolicySuggestionLoss = request;
  const copy = {
    close: ["确认关闭临时候选稿？", "关闭后，尚未采用的完整文本将永久丢失，重新打开页面也无法恢复。"],
    discard: ["确认取消并丢弃？", "取消后，当前编辑内容将永久丢失，无法恢复。"],
    regenerate: ["确认丢弃并重新生成？", "重新生成会永久丢弃当前编辑内容，并基于届时的当前策略和人工标注再模拟 1 次模型调用。"],
    leave: ["确认离开岗位鉴定？", "离开后，当前临时候选稿将永久丢失，返回岗位鉴定页也无法恢复。"],
  }[request.kind];
  document.querySelector("#policy-suggestion-loss-title").textContent = copy[0];
  document.querySelector("#policy-suggestion-loss-message").textContent = copy[1];
  document.querySelector("#policy-candidate-dialog").close();
  document.querySelector("#policy-suggestion-loss-dialog").showModal();
}

function keepPolicySuggestion() {
  document.querySelector("#policy-suggestion-loss-dialog").close();
  pendingPolicySuggestionLoss = null;
  document.querySelector("#policy-candidate-full-text").value = sessionPolicySuggestion.candidate.full_text;
  document.querySelector("#policy-candidate-dialog").showModal();
}

async function confirmPolicySuggestionLoss() {
  const request = pendingPolicySuggestionLoss;
  document.querySelector("#policy-suggestion-loss-dialog").close();
  pendingPolicySuggestionLoss = null;
  sessionPolicySuggestion = null;
  if (request?.kind === "regenerate") {
    await generatePolicySuggestion();
    return;
  }
  if (request?.kind === "leave") {
    navigate(request.nextView, request.nextFilters, true);
    showToast("未采用的临时候选稿已永久丢失。");
    return;
  }
  render();
  showToast("未采用的临时候选稿已永久丢失，无法恢复。");
}

async function activatePolicyVersion() {
  if (!sessionPolicySuggestion?.available) return;
  capturePolicySuggestionEdit();
  await api({
    action: "activate_policy_version",
    base_version: sessionPolicySuggestion.base_version,
    full_text: sessionPolicySuggestion.candidate.full_text,
  });
  sessionPolicySuggestion = null;
  document.querySelector("#policy-candidate-dialog").close();
  await refresh();
  settingsDraft = draftFromState();
  render();
  showToast("新策略已采用并启用。");
}

async function applyReview(verdict) {
  const jobId = reviewIds[reviewIndex];
  const result = await api({ action: "review_job", job_id: jobId, verdict });
  reviewResults.push(result);
  reviewIndex += 1;
  await refresh();
  if (reviewIndex >= reviewIds.length) finishReview();
  else renderReviewStep();
}

function finishReview() {
  document.querySelector("#review-dialog").close();
  const success = reviewResults.filter((item) => item.processed).length;
  const skipped = reviewResults.length - success;
  selected.clear();
  batchAction = null;
  directAction = false;
  render();
  showToast(`人工复核完成 ${success} 个${skipped ? `，另有 ${skipped} 个因状态变化跳过` : ""}。`);
}

async function saveAssessmentSettings() {
  const config = collectAssessmentSettings();
  if (!Number.isInteger(config.assessmentLimit) || config.assessmentLimit < 1) {
    showToast("AI 同时鉴定数请输入正整数。");
    return;
  }
  await api({ action: "configure_assessment", enabled: config.assessmentEnabled, limit: config.assessmentLimit });
  await refresh();
  settingsDraft = draftFromState();
  render();
  showToast("岗位鉴定设置已保存；已经开始的鉴定保持原版本。");
}

async function saveOutreachSettings() {
  const config = collectOutreachSettings();
  if (config.outreachEnabled && !config.greetingText) {
    showToast("开启自动首次沟通前必须配置固定招呼语。");
    return;
  }
  if (state.settings.automatic_outreach_mode === "simulation" && config.outreachMode === "real") {
    pendingSettings = config;
    const impact = await api({ action: "preview_outreach", enabled: config.outreachEnabled, mode: "real" });
    document.querySelector("#impact-exact-copy").textContent = `当前 ${impact.real_queue_count} 个岗位会安排真实发送，另有 ${impact.simulation_inflight_count} 个仍在模拟、本次不安排真实发送。`;
    document.querySelector("#impact-dialog").showModal();
    return;
  }
  await applyOutreachSettings(config);
}

async function applyOutreachSettings(config) {
  await api({ action: "configure_outreach", enabled: config.outreachEnabled, mode: config.outreachMode, greeting_text: config.greetingText, time_windows: config.timeWindows });
  pendingSettings = null;
  closeDialogs();
  await refresh();
  settingsDraft = draftFromState();
  render();
  showToast("首次沟通设置已保存。");
}

function openStartDiscovery() {
  const resume = state.online_resume;
  document.querySelector("#start-resume-version").textContent = resume.current_version == null ? "尚未保存在线简历" : `在线简历 v${resume.current_version}`;
  document.querySelector("#start-resume-saved").textContent = resume.current_version == null ? "请先到在线简历页手动刷新。" : `保存于 ${resume.saved_at}；开始发现不会再次读取 BOSS。`;
  document.querySelector("#start-discovery-submit").disabled = !state.discovery_can_start;
  document.querySelector("#start-discovery-notices").innerHTML = state.discovery_notices.map((notice) => `<div>${escapeHtml(notice)}</div>`).join("");
  document.querySelector("#start-discovery-dialog").showModal();
}

function openJobDetail(jobId) {
  const job = state.jobs.find((item) => item.id === jobId);
  if (!job) return;
  document.querySelector("#job-detail-title").textContent = job.title;
  document.querySelector("#job-detail-company").textContent = `${job.company} · ${job.city} · ${job.salary}`;
  document.querySelector("#job-detail-assessment-status").textContent = job.assessment_label;
  document.querySelector("#job-detail-assessment-usage").textContent = job.assessment_usage_note || "当前状态没有本次实际使用信息。";
  document.querySelector("#job-detail-agent-input").textContent = job.assessment_agent_input_note || "尚未开始鉴定，当前没有 Agent 输入。";
  document.querySelector("#job-detail-outreach").textContent = `${job.outreach_label}；${job.why_not_contacted}`;
  document.querySelector("#job-detail-dialog").showModal();
}

document.addEventListener("click", async (event) => {
  const viewTarget = event.target.closest("[data-view]");
  if (viewTarget) {
    navigate(viewTarget.dataset.view);
    return;
  }
  const pageTarget = event.target.closest("[data-job-page]");
  if (pageTarget && !pageTarget.disabled) {
    setJobPage(pageTarget.dataset.jobPage);
    return;
  }
  const detailTarget = event.target.closest("[data-job-detail]");
  if (detailTarget) {
    openJobDetail(Number(detailTarget.dataset.jobDetail));
    return;
  }
  const directJobTarget = event.target.closest("[data-direct-job-action]");
  if (directJobTarget && !directJobTarget.disabled) {
    try {
      await submitDirectJobAction(directJobTarget.dataset.directJobAction, Number(directJobTarget.dataset.directJobId));
    } catch (error) {
      showToast(error.message);
    }
    return;
  }
  const timePresetTarget = event.target.closest("[data-time-window-preset]");
  if (timePresetTarget) {
    chooseTimeWindowPreset(timePresetTarget.dataset.timeWindowPreset);
    return;
  }
  const batchTarget = event.target.closest("[data-batch-action]");
  if (batchTarget) {
    batchAction = batchTarget.dataset.batchAction;
    directAction = false;
    selected.clear();
    render();
    return;
  }
  const presetTarget = event.target.closest("[data-job-preset]");
  if (presetTarget) {
    const preset = presetTarget.dataset.jobPreset;
    if (preset === "assessment-unfinished") navigate("jobs", { assessment: "unfinished" });
    else if (preset === "assessment-unqueued") navigate("jobs", { assessment: "not_queued" });
    else if (preset === "assessment-review") navigate("jobs", { assessment: "review" });
    else if (preset === "assessment-active") navigate("jobs", { assessment: "active" });
    else if (preset === "outreach-ready") navigate("jobs", { platform: "open", assessment: "suitable", outreach: "not_queued" });
    else if (preset === "outreach-uncontacted") navigate("jobs", { outreach: "uncontacted" });
    else if (preset === "outreach-contacted") navigate("jobs", { outreach: "contacted" });
    else if (preset === "outreach-active") navigate("jobs", { outreach: "active" });
    return;
  }
  const target = event.target.closest("[data-action]");
  if (!target) return;
  const action = target.dataset.action;
  try {
    if (action === "clear-filters") {
      filters = { search: "", platform: "all", assessment: "all", human: "all", outreach: "all" };
      jobPage = 1;
      render();
    } else if (action === "cancel-batch") {
      batchAction = null; directAction = false; selected.clear(); render();
    } else if (action === "select-all-eligible") {
      selected = new Set(paginatedJobs(filteredJobs()).items.filter((job) => getEligibility(job).eligible).map((job) => job.id)); render();
    } else if (action === "submit-batch") await submitBatch();
    else if (action === "confirm-assessment-queue") { document.querySelector("#assessment-confirm-dialog").close(); await executeBatch(); }
    else if (action === "confirm-manual-real") { document.querySelector("#manual-real-dialog").close(); await executeBatch(); }
    else if (action === "review-suitable") await applyReview("suitable");
    else if (action === "review-unsuitable") await applyReview("unsuitable");
    else if (action === "review-skip") { reviewIndex += 1; if (reviewIndex >= reviewIds.length) finishReview(); else renderReviewStep(); }
    else if (action === "continue-discovery") { await api({ action: "continue_discovery" }); await refresh(); showToast("已从原检查点继续同一岗位发现运行。"); }
    else if (action === "pause-discovery") { await api({ action: "pause_discovery" }); await refresh(); showToast("已暂停岗位发现运行，检查点保留。"); }
    else if (action === "open-end-discovery") document.querySelector("#end-discovery-dialog").showModal();
    else if (action === "confirm-end-discovery") { await api({ action: "end_discovery_early", reason: document.querySelector("#end-reason").value }); closeDialogs(); await refresh(); showToast("当前运行已提前结束，现在可以创建新运行。"); }
    else if (action === "open-start-discovery") openStartDiscovery();
    else if (action === "confirm-start-discovery") { await api({ action: "start_discovery" }); closeDialogs(); await refresh(); showToast("已创建并开始新的岗位发现运行。"); }
    else if (action === "refresh-online-resume") { const result = await api({ action: "refresh_online_resume", outcome: document.querySelector("#resume-refresh-outcome").value }); await refresh(); showToast(result.message); }
    else if (action === "simulate-first-use") { await api({ action: "simulate_first_use" }); await refresh(); showToast("已切换到首次使用演示状态；当前没有在线简历版本。"); }
    else if (action === "generate-policy-suggestion") await generatePolicySuggestion();
    else if (action === "activate-policy-version") await activatePolicyVersion();
    else if (action === "request-discard-policy-suggestion") requestPolicySuggestionLoss({ kind: "discard" });
    else if (action === "request-regenerate-policy-suggestion") requestPolicySuggestionLoss({ kind: "regenerate" });
    else if (action === "keep-policy-suggestion") keepPolicySuggestion();
    else if (action === "confirm-policy-suggestion-loss") await confirmPolicySuggestionLoss();
    else if (action === "save-assessment-settings") await saveAssessmentSettings();
    else if (action === "save-outreach-settings") await saveOutreachSettings();
    else if (action === "add-time-window") openTimeWindowEditor();
    else if (action === "edit-time-window") openTimeWindowEditor(Number(target.dataset.windowIndex));
    else if (action === "remove-time-window") removeTimeWindow(Number(target.dataset.windowIndex));
    else if (action === "save-time-window") saveTimeWindowEditor();
    else if (action === "confirm-real-switch") await applyOutreachSettings(pendingSettings);
    else if (action === "cancel-real-switch") { document.querySelector("#impact-dialog").close(); pendingSettings = null; }
    else if (action === "close-dialog") { closeDialogs(); if (directAction) { directAction = false; batchAction = null; selected.clear(); render(); } }
    else if (action === "reset") { await api({ action: "reset" }); filters = { search: "", platform: "all", assessment: "all", human: "all", outreach: "all" }; jobPage = 1; selected.clear(); batchAction = null; directAction = false; await refresh(); showToast("已重置岗位与发现运行；已保存设置和在线简历版本仍保留。"); }
  } catch (error) {
    showToast(error.message);
  }
});

document.querySelector("#policy-candidate-dialog").addEventListener("cancel", (event) => {
  if (!sessionPolicySuggestion) return;
  event.preventDefault();
  requestPolicySuggestionLoss({ kind: "close" });
});

document.querySelector("#policy-suggestion-loss-dialog").addEventListener("cancel", (event) => {
  if (!sessionPolicySuggestion) return;
  event.preventDefault();
  keepPolicySuggestion();
});

window.addEventListener("beforeunload", (event) => {
  if (!sessionPolicySuggestion) return;
  event.preventDefault();
  event.returnValue = "";
});

document.addEventListener("change", (event) => {
  if (event.target.matches("[data-job-id]")) {
    const id = Number(event.target.dataset.jobId);
    if (event.target.checked) selected.add(id); else selected.delete(id);
    render();
  } else if (event.target.matches("#platform-filter")) { filters.platform = event.target.value; jobPage = 1; selected.clear(); render(); }
  else if (event.target.matches("#assessment-filter")) { filters.assessment = event.target.value; jobPage = 1; selected.clear(); render(); }
  else if (event.target.matches("#human-filter")) { filters.human = event.target.value; jobPage = 1; selected.clear(); render(); }
  else if (event.target.matches("#outreach-filter")) { filters.outreach = event.target.value; jobPage = 1; selected.clear(); render(); }
  else if (event.target.matches("#job-page-size")) setJobPageSize(event.target.value);
});

document.addEventListener("input", (event) => {
  if (event.target.matches("#policy-candidate-full-text")) {
    capturePolicySuggestionEdit();
  } else if (event.target.matches("#job-search")) {
    filters.search = event.target.value;
    jobPage = 1;
    selected.clear();
    render();
    const input = document.querySelector("#job-search");
    input.focus();
    input.setSelectionRange(filters.search.length, filters.search.length);
  }
});

refresh().catch((error) => {
  document.querySelector("#app").innerHTML = `<p style="padding:30px">原型加载失败：${escapeHtml(error.message)}</p>`;
});
