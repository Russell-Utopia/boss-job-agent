(() => {
  const batchForm = document.querySelector("[data-batch-form]");
  if (batchForm) {
    const selectAll = batchForm.querySelector("[data-select-all]");
    const checkboxes = [...batchForm.querySelectorAll("[data-job-checkbox]:not(:disabled)")];
    const count = batchForm.querySelector("[data-selection-count]");
    const submit = batchForm.querySelector("[data-submit-selection]");

    const updateSelection = () => {
      const selected = checkboxes.filter((checkbox) => checkbox.checked).length;
      count.textContent = `已选择 ${selected} 条`;
      submit.disabled = selected === 0;
      selectAll.checked = checkboxes.length > 0 && selected === checkboxes.length;
      selectAll.indeterminate = selected > 0 && selected < checkboxes.length;
    };

    selectAll?.addEventListener("change", () => {
      checkboxes.forEach((checkbox) => { checkbox.checked = selectAll.checked; });
      updateSelection();
    });
    checkboxes.forEach((checkbox) => checkbox.addEventListener("change", updateSelection));
    updateSelection();
  }

  const editor = document.querySelector("[data-candidate-editor]");
  const diffRoot = document.querySelector("[data-policy-diff]");
  const currentPolicy = document.querySelector("#current-policy-text");
  let draftActive = Boolean(editor);

  if (editor && diffRoot && currentPolicy) {
    const render = () => renderDiff(diffRoot, diffLines(currentPolicy.value, editor.value));
    const confirmDiscard = (message) => {
      if (!draftActive) return true;
      if (!window.confirm(message)) return false;
      draftActive = false;
      return true;
    };

    editor.addEventListener("input", render);
    render();

    document.querySelector("[data-discard-draft]")?.addEventListener("click", () => {
      if (!confirmDiscard("丢弃后本次策略候选稿永久消失，重新生成会再次调用模型。确定丢弃吗？")) return;
      window.location.assign("/prototype/go/assessments");
    });

    document.querySelectorAll("[data-draft-navigation]").forEach((link) => {
      link.addEventListener("click", (event) => {
        if (!draftActive) return;
        event.preventDefault();
        if (!confirmDiscard("离开会丢弃当前策略候选稿。确定继续吗？")) return;
        window.location.assign(link.href);
      });
    });

    const resetForm = document.querySelector("[data-reset-form]");
    resetForm?.addEventListener("submit", (event) => {
      if (!draftActive) return;
      event.preventDefault();
      if (!confirmDiscard("重置会丢弃当前策略候选稿。确定继续吗？")) return;
      resetForm.submit();
    });

    window.addEventListener("beforeunload", (event) => {
      if (!draftActive) return;
      event.preventDefault();
      event.returnValue = "";
    });
  }

  function diffLines(beforeText, afterText) {
    const before = beforeText.split("\n");
    const after = afterText.split("\n");
    const table = Array.from({ length: before.length + 1 }, () => Array(after.length + 1).fill(0));

    for (let i = before.length - 1; i >= 0; i -= 1) {
      for (let j = after.length - 1; j >= 0; j -= 1) {
        table[i][j] = before[i] === after[j]
          ? table[i + 1][j + 1] + 1
          : Math.max(table[i + 1][j], table[i][j + 1]);
      }
    }

    const result = [];
    let i = 0;
    let j = 0;
    while (i < before.length && j < after.length) {
      if (before[i] === after[j]) {
        result.push({ type: "same", text: before[i] });
        i += 1;
        j += 1;
      } else if (table[i + 1][j] >= table[i][j + 1]) {
        result.push({ type: "removed", text: before[i] });
        i += 1;
      } else {
        result.push({ type: "added", text: after[j] });
        j += 1;
      }
    }
    while (i < before.length) result.push({ type: "removed", text: before[i++] });
    while (j < after.length) result.push({ type: "added", text: after[j++] });
    return result;
  }

  function renderDiff(root, entries) {
    root.replaceChildren(...entries.map((entry) => {
      const line = document.createElement("div");
      line.className = `diff-line ${entry.type}`;
      line.textContent = `${entry.type === "added" ? "+" : entry.type === "removed" ? "−" : " "} ${entry.text}`;
      return line;
    }));
  }
})();
