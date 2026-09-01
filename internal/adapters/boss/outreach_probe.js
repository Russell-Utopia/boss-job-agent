(() => {
  const input = __OUTREACH_INPUT__;
  const normalized = value => String(value || "").replace(/\s+/g, " ").trim();
  const rawText = value => String(value || "").replace(/\r\n?/g, "\n").trim();
  const fail = message => { throw new Error("BOSS_OUTREACH_UNRELIABLE:" + message); };
  const bodyText = () => document.body?.innerText || "";
  const countText = (text, wanted) => wanted ? text.split(wanted).length - 1 : 0;
  const preflight = () => {
    const text = bodyText();
    if (/login/i.test(location.pathname) || text.includes("登录/注册")) throw new Error("BOSS_AUTHENTICATION_REQUIRED");
    if (text.includes("安全验证") || text.includes("请输入验证码")) throw new Error("BOSS_VERIFICATION_REQUIRED");
    if (text.includes("访问过于频繁") || text.includes("操作过于频繁")) throw new Error("BOSS_PLATFORM_LIMITED");
  };
  const pageJobID = () => {
    const match = String(location.pathname).match(/\/job_detail\/([^/?#]+?)(?:\.html)?$/);
    return match ? decodeURIComponent(match[1]) : "";
  };
  const visible = element => {
    if (!element) return false;
    const style = window.getComputedStyle(element);
    return style.display !== "none" && style.visibility !== "hidden";
  };
  const allButtons = () => [...document.querySelectorAll("button, a, input[type='button'], input[type='submit']")].filter(visible);
  const buttonWithText = (wanted, exact = false) => allButtons().find(button => {
    const text = normalized(button.innerText || button.value);
    return exact ? text === wanted : text.includes(wanted);
  });
  const hasExistingContact = () => {
    const text = bodyText();
    return text.includes("已沟通过") || text.includes("已沟通") || Boolean(buttonWithText("继续沟通"));
  };
  const readOpenStatus = () => {
    const text = bodyText();
    if (text.includes("招聘中") || buttonWithText("立即沟通")) return true;
    if (text.includes("职位已关闭") || text.includes("已下线") || text.includes("停止招聘")) return false;
    fail("missing_reliable_open_status");
  };
  const evidence = (open, contacted, stage, extra = {}) => ({
    platformJobId: input.platformJobId, open, alreadyContacted: contacted, stage,
    pageURL: String(location.href).split("#")[0], ...extra
  });
  const waitFor = async (predicate, timeoutMs) => {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      preflight();
      const value = predicate();
      if (value) return value;
      await new Promise(resolve => setTimeout(resolve, 100));
    }
    fail("timeout_waiting_for_chat_ui");
  };
  const fillGreeting = (field, greeting) => {
    field.focus();
    if (field instanceof HTMLTextAreaElement || field instanceof HTMLInputElement) {
      const setter = Object.getOwnPropertyDescriptor(field.constructor.prototype, "value")?.set;
      if (setter) setter.call(field, greeting); else field.value = greeting;
    } else {
      field.textContent = greeting;
    }
    field.dispatchEvent(new InputEvent("input", {bubbles: true, inputType: "insertText", data: greeting}));
    field.dispatchEvent(new Event("change", {bubbles: true}));
  };
  const chatField = () => [...document.querySelectorAll("textarea, input[type='text'], [contenteditable='true']")]
    .filter(visible).find(field => !String(field.getAttribute("placeholder") || "").includes("搜索"));

  preflight();
  if (pageJobID() !== input.platformJobId) fail("detail_identity_mismatch");
  const contacted = hasExistingContact();
  const open = readOpenStatus();
  if (input.mode === "check") {
    return JSON.stringify({platformJobId: input.platformJobId, open, alreadyContacted: contacted, evidence: evidence(open, contacted, "contact_check")});
  }
  if (input.mode !== "send") fail("invalid_mode");
  if (contacted || !open) {
    return JSON.stringify({platformJobId: input.platformJobId, effect: "confirmed_no_effect", evidence: evidence(open, contacted, "send_preflight")});
  }
  const chatButton = buttonWithText("立即沟通");
  if (!chatButton) fail("missing_immediate_contact_button");
  const greetingCountBeforeSend = countText(bodyText(), input.greetingText);
  let sendClicked = false;
  try {
    chatButton.click();
    const field = await waitFor(chatField, 8000);
    fillGreeting(field, input.greetingText);
    const sendButton = await waitFor(() => buttonWithText("发送", true), 8000);
    sendClicked = true;
    sendButton.click();
    await new Promise(resolve => setTimeout(resolve, 800));
    preflight();
    const greetingCountAfterSend = countText(bodyText(), input.greetingText);
    if (greetingCountAfterSend > greetingCountBeforeSend) {
      return JSON.stringify({platformJobId: input.platformJobId, effect: "confirmed_sent", evidence: evidence(open, false, "send_confirmed", {greetingCountBeforeSend, greetingCountAfterSend})});
    }
    return JSON.stringify({platformJobId: input.platformJobId, effect: "possibly_effective", evidence: evidence(open, false, "send_unconfirmed")});
  } catch (error) {
    if (sendClicked) {
      return JSON.stringify({platformJobId: input.platformJobId, effect: "possibly_effective", evidence: evidence(open, false, "send_interrupted")});
    }
    throw error;
  }
})()
