const BossVisiblePageProbe = (() => {
  const normalize = value => String(value || "").replace(/\s+/g, " ").trim();
  const rawText = value => String(value || "").replace(/\r\n?/g, "\n").trim();
  const jobIdFromHref = href => {
    const match = String(href || "").match(/\/job_detail\/([^/?#]+?)(?:\.html)?(?:[?#]|$)/);
    return match ? decodeURIComponent(match[1]) : "";
  };
  const fail = (kind, stage, detailOrdinal = 0) => {
    throw new Error(kind + "|stage=" + stage + "|detail_ordinal=" + detailOrdinal);
  };
  const preflight = () => {
    const pageText = document.body?.innerText || "";
    if (/login/i.test(location.pathname) || pageText.includes("登录/注册")) {
      fail("BOSS_AUTHENTICATION_REQUIRED", "page_preflight");
    }
    if (location.search.includes("_security_check=1") || pageText.includes("安全验证") || pageText.includes("请输入验证码")) {
      fail("BOSS_VERIFICATION_REQUIRED", "page_preflight");
    }
    if (pageText.includes("访问过于频繁") || pageText.includes("操作过于频繁")) {
      fail("BOSS_PLATFORM_LIMITED", "page_preflight");
    }
  };
  const renderedText = element => typeof element?.innerText === "string" ? element.innerText : "";
  const hasPrivateUseCharacters = value => /[\uE000-\uF8FF\u{F0000}-\u{FFFFD}\u{100000}-\u{10FFFD}]/u.test(value);
  const requiredText = (root, selector, label) => {
    const value = normalize(renderedText(root.querySelector(selector)));
    if (!value) throw new Error("missing_" + label);
    return value;
  };
  const requiredRawText = (root, selector, label) => {
    const value = rawText(renderedText(root.querySelector(selector)));
    if (!value) throw new Error("missing_" + label);
    return value;
  };
  const cardIdentity = root => {
    const card = root.matches?.(".job-card-box") ? root : root.querySelector(".job-card-box");
    if (!card) throw new Error("missing_job_card");
    const link = card.querySelector("a.job-name[href*='/job_detail/']");
    const platformJobId = jobIdFromHref(link?.getAttribute("href"));
    if (!platformJobId) throw new Error("missing_card_stable_id");
    return {card, link, platformJobId};
  };
  const readCard = root => {
    const {card, link, platformJobId} = cardIdentity(root);
    const renderedSalary = normalize(renderedText(card.querySelector(".salary, .job-salary")));
    const salaryReadable = renderedSalary !== "" && !hasPrivateUseCharacters(renderedSalary);
    return {
      platformJobId,
      canonicalUrl: new URL(link.getAttribute("href"), "https://www.zhipin.com").href,
      jobTitle: requiredText(card, ".job-name", "job_title"),
      companyName: requiredText(card, ".company-name, .boss-name", "company_name"),
      city: requiredText(card, ".job-area, .company-location", "city"),
      salary: salaryReadable ? renderedSalary : "",
      salaryEvidence: salaryReadable ? "readable" : "unavailable",
      link
    };
  };
  const detailRoots = root => [
    ...root.querySelectorAll(".job-detail-container, .job-detail-box, .job-detail-content")
  ];
  const detailIdentity = root => {
    const link = root.querySelector("a.more-job-btn[href*='/job_detail/'], .job-detail-header a.job-name[href*='/job_detail/'], a.job-name[href*='/job_detail/']");
    return jobIdFromHref(link?.getAttribute("href"));
  };
  const findMatchingDetail = (root, expectedJobId) => {
    const roots = detailRoots(root);
    const match = roots.find(candidate => detailIdentity(candidate) === expectedJobId);
    if (match) return match;
    const actual = roots.map(detailIdentity).find(Boolean);
    if (actual) throw new Error("detail_identity_mismatch:expected=" + expectedJobId + ":actual=" + actual);
    throw new Error("missing_detail_stable_id");
  };
  const readDetail = (root, expectedJobId) => {
    const detail = findMatchingDetail(root, expectedJobId);
    const explicitStatus = normalize(renderedText(detail.querySelector(".job-status, [class*='job-status']")));
    const chat = detail.querySelector(".op-btn-chat");
    const chatEnabled = chat && !chat.hasAttribute("disabled") &&
      chat.getAttribute("aria-disabled") !== "true" && !chat.classList.contains("disabled") &&
      normalize(renderedText(chat)).includes("立即沟通");
    if (explicitStatus !== "招聘中" && !chatEnabled) {
      throw new Error(explicitStatus ? "unreliable_open_status:" + explicitStatus : "missing_open_status");
    }
    const status = explicitStatus === "招聘中" ? explicitStatus : "招聘中";
    const fullJD = requiredRawText(detail, ".job-detail-body .desc", "full_jd");
    return {detailPlatformJobId: expectedJobId, platformStatusEvidence: status, fullJD};
  };
  const readObservation = root => {
    const card = readCard(root);
    const detail = readDetail(root, card.platformJobId);
    return {
      platformJobId: card.platformJobId,
      detailPlatformJobId: detail.detailPlatformJobId,
      platformStatusEvidence: detail.platformStatusEvidence,
      canonicalUrl: card.canonicalUrl,
      jobTitle: card.jobTitle,
      companyName: card.companyName,
      city: card.city,
      salary: card.salary,
      salaryEvidence: card.salaryEvidence,
      fullJD: detail.fullJD
    };
  };
  const waitForMatchingDetail = async (root, expectedJobId, timeoutMs) => {
    const deadline = Date.now() + timeoutMs;
    let lastError = new Error("missing_detail_stable_id");
    while (Date.now() < deadline) {
      preflight();
      try {
        return findMatchingDetail(root, expectedJobId);
      } catch (error) {
        lastError = error;
      }
      await new Promise(resolve => setTimeout(resolve, 100));
    }
    throw lastError;
  };
  const waitForStableCards = async (root, timeoutMs) => {
    const deadline = Date.now() + timeoutMs;
    let lastSignature = "";
    let stableSamples = 0;
    while (Date.now() < deadline) {
      preflight();
      const cards = [...root.querySelectorAll(".job-card-box")];
      const signature = cards.map(card => {
        try {
          return cardIdentity(card).platformJobId;
        } catch (_) {
          return "";
        }
      }).filter(Boolean).join("|");
      if (signature && signature === lastSignature) {
        stableSamples++;
        if (stableSamples >= 2) return cards;
      } else {
        lastSignature = signature;
        stableSamples = 0;
      }
      await new Promise(resolve => setTimeout(resolve, 200));
    }
    fail("BOSS_VISIBLE_PAGE_UNRELIABLE:missing_job_cards", "job_list_dom");
  };
  const run = async input => {
    preflight();
    const limit = Number(input?.limit);
    if (!Number.isInteger(limit) || limit < 1 || limit > 8) {
      fail("BOSS_VISIBLE_PAGE_UNRELIABLE:invalid_limit", "probe_input");
    }
    const cards = await waitForStableCards(document, 10000);

    const stableCards = [];
    const seen = new Set();
    for (const card of cards) {
      try {
        const snapshot = readCard(card);
        if (seen.has(snapshot.platformJobId)) continue;
        seen.add(snapshot.platformJobId);
        stableCards.push({card, snapshot});
      } catch (error) {
        fail("BOSS_VISIBLE_PAGE_UNRELIABLE:" + error.message, "job_list_dom");
      }
    }
    if (!stableCards.length) fail("BOSS_VISIBLE_PAGE_UNRELIABLE:missing_stable_ids", "job_list_dom");

    const jobs = [];
    const selected = stableCards.slice(0, limit);
    for (const [index, entry] of selected.entries()) {
      const detailOrdinal = index + 1;
      preflight();
      entry.card.scrollIntoView({block: "center", inline: "nearest"});
      entry.snapshot.link.click();
      try {
        await waitForMatchingDetail(document, entry.snapshot.platformJobId, 8000);
        const detail = readDetail(document, entry.snapshot.platformJobId);
        jobs.push({
          platformJobId: entry.snapshot.platformJobId,
          detailPlatformJobId: detail.detailPlatformJobId,
          platformStatusEvidence: detail.platformStatusEvidence,
          canonicalUrl: entry.snapshot.canonicalUrl,
          jobTitle: entry.snapshot.jobTitle,
          companyName: entry.snapshot.companyName,
          city: entry.snapshot.city,
          salary: entry.snapshot.salary,
          salaryEvidence: entry.snapshot.salaryEvidence,
          fullJD: detail.fullJD
        });
      } catch (error) {
        fail("BOSS_VISIBLE_PAGE_UNRELIABLE:" + error.message, "job_detail", detailOrdinal);
      }
    }
    return JSON.stringify({
      jobs,
      scannedCardCount: stableCards.length,
      truncated: stableCards.length > selected.length,
      exhaustionEvidence: "unavailable"
    });
  };
  return Object.freeze({jobIdFromHref, cardIdentity, readCard, readDetail, readObservation, run});
})();
