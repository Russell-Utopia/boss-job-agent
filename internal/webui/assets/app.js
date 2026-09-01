(function () {
  "use strict";

  const outreachForm = document.querySelector("#outreach-form");
  outreachForm?.addEventListener("submit", (event) => {
    const selected = Array.from(outreachForm.querySelectorAll("input[name='jobId']:checked"));
    if (!selected.length) {
      event.preventDefault();
      window.alert("至少选择一个当前可入队岗位。");
      return;
    }
    const greeting = outreachForm.dataset.greeting || "";
    const timeDescription = outreachForm.dataset.timeDescription || "";
    const message = "本批将授权 " + selected.length + " 个岗位。\n\n完整招呼语：" + greeting + "\n当前时间规则：" + timeDescription + "\n\n确认后只加入本批真实打招呼队列，不会开启自动打招呼。";
    if (!window.confirm(message)) {
      event.preventDefault();
      return;
    }
    outreachForm.querySelector("input[name='jobCount']").value = String(selected.length);
    outreachForm.querySelector("input[name='greetingText']").value = greeting;
    outreachForm.querySelector("input[name='timeDescription']").value = timeDescription;
    outreachForm.querySelector("input[name='confirmed']").value = "true";
  });

  const workbench = document.querySelector("#job-workbench");
  if (workbench) {
    const batchForm = workbench.querySelector("#job-batch-form");
    const actionSelect = workbench.querySelector("#job-batch-action");
    const verdictLabel = workbench.querySelector("#job-batch-verdict-label");
    const verdictSelect = workbench.querySelector("#job-batch-verdict");
    const submitButton = workbench.querySelector("#job-batch-submit");
    const selectAll = workbench.querySelector("#job-select-all");
    const selectionText = workbench.querySelector("#job-batch-selection");
    const resultPanel = workbench.querySelector("#job-batch-result");
    const boxes = () => Array.from(workbench.querySelectorAll(".job-select"));

    function selectedBoxes() {
      return boxes().filter((box) => box.checked && !box.disabled);
    }

    function allowed(box, action) {
      return action !== "" && box.dataset[action + "Allowed"] === "true";
    }

    function updateBatchSelection() {
      const action = actionSelect.value;
      const enabledBoxes = boxes().filter((box) => allowed(box, action));
      boxes().forEach((box) => {
        box.disabled = !allowed(box, action);
        if (box.disabled) box.checked = false;
      });
      selectAll.disabled = !action || !enabledBoxes.length;
      selectAll.checked = enabledBoxes.length > 0 && enabledBoxes.every((box) => box.checked);
      const selected = selectedBoxes();
      submitButton.disabled = !action || !selected.length;
      selectionText.textContent = action ? ("已选择 " + selected.length + " 个岗位") : "请先选择动作";
      const review = action === "review";
      verdictLabel.hidden = !review;
      verdictSelect.hidden = !review;
    }

    actionSelect.addEventListener("change", () => {
      boxes().forEach((box) => { box.checked = false; });
      updateBatchSelection();
    });
    selectAll.addEventListener("change", () => {
      boxes().forEach((box) => {
        if (!box.disabled) box.checked = selectAll.checked;
      });
      updateBatchSelection();
    });
    boxes().forEach((box) => box.addEventListener("change", updateBatchSelection));

    workbench.querySelectorAll(".quick-action[data-quick-action]").forEach((button) => {
      button.addEventListener("click", () => {
        actionSelect.value = button.dataset.quickAction;
        updateBatchSelection();
        const box = boxes().find((item) => item.dataset.jobId === button.dataset.jobId);
        if (!box || box.disabled) return;
        box.checked = true;
        updateBatchSelection();
        batchForm.requestSubmit();
      });
    });

    function showBatchResult(payload) {
      resultPanel.replaceChildren();
      resultPanel.hidden = false;
      const summary = document.createElement("p");
      summary.textContent = "本次成功 " + (payload.succeeded || 0) + " 个，跳过 " + ((payload.skipped || []).length) + " 个。";
      resultPanel.appendChild(summary);
      if (payload.skipped?.length) {
        const list = document.createElement("ul");
        payload.skipped.forEach((item) => {
          const row = document.createElement("li");
          row.textContent = "岗位 #" + item.jobId + "：" + item.reason;
          list.appendChild(row);
        });
        resultPanel.appendChild(list);
      }
    }

    batchForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const action = actionSelect.value;
      const selected = selectedBoxes();
      if (!action || !selected.length) {
        window.alert("请先选择一个动作和至少一个可执行岗位。");
        return;
      }
      const body = {
        action,
        jobIds: selected.map((box) => Number(box.dataset.jobId)),
      };
      if (action === "review") {
        body.decisions = selected.map((box) => ({
          jobId: Number(box.dataset.jobId),
          expectedJdHash: box.dataset.jdHash,
          verdict: verdictSelect.value,
        }));
        if (!window.confirm("本批将人工复核 " + selected.length + " 个岗位为“" + (verdictSelect.value === "suitable" ? "适合" : "不适合") + "”。提交时后台仍会逐项检查陈旧 JD。继续吗？")) return;
      }
      if (action === "outreach") {
        const greeting = batchForm.dataset.greeting || "";
        const timeDescription = batchForm.dataset.timeDescription || "";
        body.confirmation = {
          jobCount: selected.length,
          greetingText: greeting,
          timeDescription,
          confirmed: window.confirm("本批将授权 " + selected.length + " 个岗位。\n\n完整招呼语：" + greeting + "\n当前时间规则：" + timeDescription + "\n\n确认后只加入本批真实打招呼队列，不会开启自动打招呼。"),
        };
        if (!body.confirmation.confirmed) return;
      }
      submitButton.disabled = true;
      try {
        const response = await fetch(batchForm.action, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        });
        const payload = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(payload.reason || "批量操作失败，请稍后重试");
        showBatchResult(payload);
        selected.forEach((box) => { box.checked = false; });
        updateBatchSelection();
      } catch (error) {
        resultPanel.hidden = false;
        resultPanel.textContent = error.message;
      } finally {
        updateBatchSelection();
      }
    });

    workbench.querySelectorAll(".job-filters input, .job-filters select").forEach((control) => {
      control.addEventListener("change", () => {
        workbench.querySelector(".job-filters input[name='page']").value = "1";
        boxes().forEach((box) => { box.checked = false; });
        updateBatchSelection();
      });
    });
    updateBatchSelection();
  }

  const root = document.querySelector("#policy-optimization");
  if (!root) return;

  const session = {
    draft: null,
    report: null,
    validationEnabled: false,
    sampleSelectionDirty: false,
  };
  const generateButton = root.querySelector("#policy-generate");
  const validateButton = root.querySelector("#policy-validate");
  const draftPanel = root.querySelector("#policy-draft-panel");
  const draftText = root.querySelector("#policy-draft-text");
  const diff = root.querySelector("#policy-diff");
  const report = root.querySelector("#policy-validation-report");
  const message = document.createElement("p");
  message.className = "reason";
  message.id = "policy-session-message";
  message.setAttribute("role", "status");
  root.querySelector(".policy-session")?.prepend(message);

  const checkboxes = () => Array.from(root.querySelectorAll(".policy-sample-checkbox"));
  const selectedIDs = () => checkboxes().filter((box) => box.checked).map((box) => Number(box.value));
  const allIDs = () => checkboxes().map((box) => Number(box.value));
  const initialSampleSelection = () => selectedIDs().join(",");
  const initialSelection = initialSampleSelection();

  function updateSampleSelectionDirty() {
    session.sampleSelectionDirty = initialSampleSelection() !== initialSelection;
  }

  function showMessage(text) {
    message.textContent = text || "";
  }

  function confirmDiscard(action) {
    return window.confirm("当前候选稿、生成样本选择和验收报告只存在本页面；" + action + "后未采用内容会永久丢失，无法恢复，再次取得需要重新调用模型。");
  }

  function lines(text) {
    return text.replace(/\r\n/g, "\n").split("\n");
  }

  // A small deterministic LCS diff keeps the browser the sole owner of
  // presentation changes and never asks the model for a change summary.
  function diffLines(before, after) {
    const oldLines = lines(before);
    const newLines = lines(after);
    const table = Array.from({ length: oldLines.length + 1 }, () => Array(newLines.length + 1).fill(0));
    for (let i = oldLines.length - 1; i >= 0; i -= 1) {
      for (let j = newLines.length - 1; j >= 0; j -= 1) {
        table[i][j] = oldLines[i] === newLines[j] ? table[i + 1][j + 1] + 1 : Math.max(table[i + 1][j], table[i][j + 1]);
      }
    }
    const result = [];
    let i = 0;
    let j = 0;
    while (i < oldLines.length || j < newLines.length) {
      if (i < oldLines.length && j < newLines.length && oldLines[i] === newLines[j]) {
        result.push(["unchanged", "  " + oldLines[i]]);
        i += 1;
        j += 1;
      } else if (j < newLines.length && (i === oldLines.length || table[i][j + 1] >= table[i + 1][j])) {
        result.push(["added", "+ " + newLines[j]]);
        j += 1;
      } else {
        result.push(["deleted", "- " + oldLines[i]]);
        i += 1;
      }
    }
    return result;
  }

  function renderDiff() {
    diff.replaceChildren();
    if (!session.draft) return;
    const before = (session.draft.policy.rules || []).join("\n");
    diffLines(before, draftText.value).forEach(([kind, text]) => {
      const line = document.createElement("span");
      line.className = "policy-diff-line " + kind;
      line.textContent = text;
      diff.appendChild(line);
    });
  }

  function renderReport() {
    report.replaceChildren();
    if (!session.report) {
      report.hidden = true;
      return;
    }
    report.hidden = false;
    const heading = document.createElement("h4");
    heading.textContent = "策略验收：" + validationStatusText(session.report.status);
    heading.className = "policy-report-status " + session.report.status;
    report.appendChild(heading);
    const summary = document.createElement("p");
    summary.textContent = session.report.summary;
    report.appendChild(summary);
    const metrics = document.createElement("p");
    metrics.textContent = "当前：误把不适合判为适合 " + session.report.currentMetrics.falsePositive + "，误把适合判为不适合 " + session.report.currentMetrics.falseNegative + "，需人工确认 " + session.report.currentMetrics.needsUserConfirmation + "；候选：误把不适合判为适合 " + session.report.candidateMetrics.falsePositive + "，误把适合判为不适合 " + session.report.candidateMetrics.falseNegative + "，需人工确认 " + session.report.candidateMetrics.needsUserConfirmation + "。";
    report.appendChild(metrics);
    appendResults(report, "全量结果", session.report.fullResults || []);
    appendResults(report, "未参与生成的样本", session.report.ungeneratedResults || []);
  }

  function appendResults(parent, title, results) {
    const heading = document.createElement("h4");
    heading.textContent = title + "（" + results.length + "）";
    parent.appendChild(heading);
    if (!results.length) return;
    const table = document.createElement("table");
    table.className = "policy-report-table";
    const head = document.createElement("tr");
    ["岗位", "人工结论", "当前策略", "候选策略"].forEach((label) => {
      const cell = document.createElement("th");
      cell.textContent = label;
      head.appendChild(cell);
    });
    table.appendChild(head);
    results.forEach((item) => {
      const row = document.createElement("tr");
      [item.jobTitle, verdictText(item.humanVerdict), statusText(item.currentStatus), statusText(item.candidateStatus)].forEach((value) => {
        const cell = document.createElement("td");
        cell.textContent = value;
        row.appendChild(cell);
      });
      table.appendChild(row);
    });
    parent.appendChild(table);
  }

  function validationStatusText(status) {
    return { passed: "通过", failed: "未通过", tradeoff: "结果有取舍", insufficient: "证据不足" }[status] || "未知";
  }

  function verdictText(status) {
    return status === "suitable" ? "适合" : "不适合";
  }

  function statusText(status) {
    return { suitable: "适合", unsuitable: "不适合", needs_user_confirmation: "需要人工确认" }[status] || status;
  }

  async function requestJSON(path, body) {
    const response = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(payload.reason || "操作失败，请稍后重试");
    return payload;
  }

  function randomHalf() {
    const boxes = checkboxes();
    const shuffled = boxes.slice().sort(() => Math.random() - 0.5);
    const count = Math.max(1, Math.ceil(shuffled.length / 2));
    boxes.forEach((box) => { box.checked = false; });
    shuffled.slice(0, count).forEach((box) => { box.checked = true; });
  }

  function showDraft(draft) {
    session.draft = draft;
    session.report = null;
    session.validationEnabled = Boolean(draft.validationEnabled);
    draftPanel.hidden = false;
    draftText.value = draft.text;
    root.querySelector("#policy-draft-generated-at").textContent = new Date(draft.generatedAt).toLocaleString();
    root.querySelector("#policy-draft-resume-version").textContent = draft.resumeVersion;
    root.querySelector("#policy-draft-policy-version").textContent = draft.policyVersion;
    root.querySelector("#policy-draft-sample-count").textContent = draft.generationSampleCount;
    root.querySelector("#policy-draft-validation-state").textContent = session.validationEnabled ? "待验收" : "未经独立验收";
    validateButton.disabled = !session.validationEnabled;
    renderDiff();
    renderReport();
    showMessage("候选稿已生成；它尚未成为正式策略，也不会改变任何岗位。");
  }

  function discardDraft() {
    session.draft = null;
    session.report = null;
    draftPanel.hidden = true;
    draftText.value = "";
    diff.replaceChildren();
    renderReport();
  }

  async function generate() {
    if (session.draft && !confirmDiscard("重新生成")) return;
    if (session.draft) discardDraft();
    const enabled = root.querySelector("#policy-validation-enabled").checked;
    const ids = enabled ? selectedIDs() : allIDs();
    if (!ids.length) {
      showMessage("至少选择一条有效人工复核作为生成样本。");
      return;
    }
    generateButton.disabled = true;
    try {
      const draft = await requestJSON("/api/policy/draft", { jobIds: ids, validationEnabled: enabled });
      showDraft(draft);
    } catch (error) {
      showMessage(error.message);
    } finally {
      generateButton.disabled = false;
    }
  }

  generateButton?.addEventListener("click", generate);
  root.querySelector("#policy-validation-enabled")?.addEventListener("change", (event) => {
    if (session.draft) {
      event.target.checked = session.validationEnabled;
      showMessage("候选稿已经生成，切换验收模式需要重新生成。");
      return;
    }
    if (event.target.checked) randomHalf();
    updateSampleSelectionDirty();
  });
  checkboxes().forEach((box) => box.addEventListener("change", updateSampleSelectionDirty));
  draftText?.addEventListener("input", () => {
    if (!session.draft) return;
    session.report = null;
    renderDiff();
    renderReport();
    showMessage("候选稿已编辑，旧验收报告立即失效。");
  });

  validateButton?.addEventListener("click", async () => {
    if (!session.draft || !session.validationEnabled) return;
    validateButton.disabled = true;
    const requestedDraft = session.draft;
    const requestedText = draftText.value;
    try {
      requestedDraft.text = requestedText;
      const nextReport = await requestJSON("/api/policy/validate", requestedDraft);
      if (session.draft === requestedDraft && draftText.value === requestedText) {
        session.report = nextReport;
        renderReport();
        showMessage("验收只产生当前页面报告，没有写入岗位状态。");
      }
    } catch (error) {
      showMessage(error.message);
    } finally {
      validateButton.disabled = false;
    }
  });

  let adopting = false;
  root.querySelector("#policy-adopt")?.addEventListener("click", async () => {
    if (adopting) return;
    if (!session.draft) return;
    if (!window.confirm("采用当前完整候选文本后，会新增并启用下一版正式策略；旧策略保持不可变，历史岗位不会重新鉴定。继续吗？")) return;
    adopting = true;
    root.querySelector("#policy-adopt").disabled = true;
    try {
      await requestJSON("/api/policy/adopt", { text: draftText.value, changeNote: "用户采用策略候选稿", policyVersionId: session.draft.policyVersionId });
      discardDraft();
      session.sampleSelectionDirty = false;
      window.location.href = "/assessments";
    } catch (error) {
      adopting = false;
      root.querySelector("#policy-adopt").disabled = false;
      showMessage(error.message);
    }
  });

  root.querySelector("#policy-discard")?.addEventListener("click", () => {
    if (!session.draft || !confirmDiscard("取消")) return;
    discardDraft();
    session.sampleSelectionDirty = false;
    window.location.reload();
  });

  root.querySelector("#policy-regenerate")?.addEventListener("click", generate);
  window.addEventListener("beforeunload", (event) => {
    if (!session.draft && !session.sampleSelectionDirty) return;
    event.preventDefault();
    event.returnValue = "候选稿、样本选择和验收报告会永久丢失。";
  });
})();
