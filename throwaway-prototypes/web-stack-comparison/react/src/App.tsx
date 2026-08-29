import { FormEvent, MouseEvent, useEffect, useMemo, useRef, useState } from "react";

type Filters = {
  query: string;
  platformStatus: string;
  aiConclusion: string;
  humanConclusion: string;
};

type Availability = { allowed: boolean; reason?: string };

type Job = {
  id: string;
  title: string;
  company: string;
  city: string;
  platformStatusText: string;
  aiConclusionText: string;
  humanConclusionText: string;
  currentJudgementText: string;
  outreachStatusText: string;
  queueSimulation: Availability;
  becameUnavailableOnSubmit: boolean;
};

type JobListView = { filters: Filters; jobs: Job[]; total: number };
type BatchItem = { id: string; title: string; accepted: boolean; reason?: string };
type BatchResult = { acceptedCount: number; skippedCount: number; items: BatchItem[] };
type Policy = { name: string; rules: string[]; text: string; humanReviewCount: number };
type PolicyDraft = { text: string; basedOnPolicy: string; humanReviewCount: number };
type DiffEntry = { type: "same" | "added" | "removed"; text: string };

const API = "/prototype/api";

export function App() {
  const page = new URLSearchParams(window.location.search).get("page") === "assessments" ? "assessments" : "jobs";
  const [draft, setDraft] = useState<PolicyDraft | null>(null);
  const allowUnload = useRef(false);

  useEffect(() => {
    const warn = (event: BeforeUnloadEvent) => {
      if (!draft || allowUnload.current) return;
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [draft]);

  const guardNavigation = (event: MouseEvent<HTMLAnchorElement>) => {
    if (!draft) return;
    event.preventDefault();
    const target = event.currentTarget.href;
    if (!window.confirm("离开会丢弃当前策略候选稿。确定继续吗？")) return;
    allowUnload.current = true;
    window.location.assign(target);
  };

  const reset = async () => {
    if (draft && !window.confirm("重置会丢弃当前策略候选稿。确定继续吗？")) return;
    await fetch(`${API}/reset`, { method: "POST" });
    allowUnload.current = true;
    setDraft(null);
    window.location.reload();
  };

  return (
    <>
      <aside className="sidebar">
        <a className="brand" href="?page=jobs" onClick={guardNavigation}><span className="brand-mark">B</span><span>BOSS Job Agent</span></a>
        <p className="prototype-label">技术栈对比原型</p>
        <nav aria-label="原型导航">
          <a href="?page=jobs" onClick={guardNavigation} className={page === "jobs" ? "active" : ""} aria-current={page === "jobs" ? "page" : undefined}>岗位</a>
          <a href="?page=assessments" onClick={guardNavigation} className={page === "assessments" ? "active" : ""} aria-current={page === "assessments" ? "page" : undefined}>岗位鉴定</a>
        </nav>
        <div className="runtime"><span className="status-dot" />共享内存场景</div>
      </aside>

      <main>
        <header className="page-header">
          <div>
            <p className="eyebrow">React + TypeScript + Vite</p>
            <h1>{page === "jobs" ? "岗位筛选与批量选择" : "策略候选稿编辑"}</h1>
            <p className="lede">同一场景、同一业务结果，只改变表现层实现。</p>
          </div>
          <span className="implementation-badge">React 客户端</span>
        </header>

        {page === "jobs" ? <JobsPage /> : <AssessmentsPage draft={draft} setDraft={setDraft} />}
      </main>

      <div className="comparison-bar" aria-label="技术栈切换">
        <span><strong>当前：</strong>React 客户端</span>
        <a href={`/prototype/go/${page}`} onClick={guardNavigation}>切换到 Go</a>
        <button type="button" className="secondary" onClick={reset}>重置共享场景</button>
      </div>
    </>
  );
}

function JobsPage() {
  const initial = filtersFromURL();
  const [formFilters, setFormFilters] = useState<Filters>(initial);
  const [filters, setFilters] = useState<Filters>(initial);
  const [view, setView] = useState<JobListView | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [result, setResult] = useState<BatchResult | null>(null);
  const [loading, setLoading] = useState(true);

  const load = async (next: Filters) => {
    setLoading(true);
    const response = await fetch(`${API}/jobs?${filtersToQuery(next)}`);
    setView(await response.json() as JobListView);
    setLoading(false);
  };

  useEffect(() => { void load(filters); }, [filters]);

  const applyFilters = (event: FormEvent) => {
    event.preventDefault();
    setSelected(new Set());
    setResult(null);
    setFilters(formFilters);
    const params = new URLSearchParams(filtersToQuery(formFilters));
    params.set("page", "jobs");
    window.history.replaceState(null, "", `?${params}`);
  };

  const eligible = view?.jobs.filter((job) => job.queueSimulation.allowed) ?? [];
  const allSelected = eligible.length > 0 && eligible.every((job) => selected.has(job.id));

  const toggleAll = () => {
    setSelected(allSelected ? new Set() : new Set(eligible.map((job) => job.id)));
  };

  const toggle = (id: string) => {
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id); else next.add(id);
      return next;
    });
  };

  const submit = async () => {
    const response = await fetch(`${API}/simulation`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ jobIds: [...selected] }),
    });
    setResult(await response.json() as BatchResult);
    setSelected(new Set());
    await load(filters);
  };

  return (
    <>
      <section className="card filter-card">
        <div className="section-heading"><div><p className="eyebrow">代表性记录</p><h2>筛选平台岗位</h2></div><span className="count">{view?.total ?? 0} 条</span></div>
        <form className="filters" onSubmit={applyFilters}>
          <label>岗位或公司<input type="search" value={formFilters.query} placeholder="例如：Go" onChange={(event) => setFormFilters({ ...formFilters, query: event.target.value })} /></label>
          <FilterSelect label="平台状态" value={formFilters.platformStatus} onChange={(value) => setFormFilters({ ...formFilters, platformStatus: value })} options={[["", "全部"], ["open", "可沟通"], ["closed", "已关闭"]]} />
          <FilterSelect label="AI 鉴定" value={formFilters.aiConclusion} onChange={(value) => setFormFilters({ ...formFilters, aiConclusion: value })} options={[["", "全部"], ["suitable", "适合"], ["unsuitable", "不适合"], ["needs_user_confirmation", "需要人工确认"]]} />
          <FilterSelect label="人工结论" value={formFilters.humanConclusion} onChange={(value) => setFormFilters({ ...formFilters, humanConclusion: value })} options={[["", "全部"], ["suitable", "已复核·适合"], ["unsuitable", "已复核·不适合"], ["unreviewed", "未复核"], ["needs_rereview", "待重新复核"]]} />
          <button type="submit">应用筛选</button>
        </form>
      </section>

      {result && (
        <section className="result-panel" aria-live="polite">
          <strong>本次提交：{result.acceptedCount} 条已加入，{result.skippedCount} 条跳过</strong>
          <ul>{result.items.map((item) => <li key={item.id} className={item.accepted ? "accepted" : "skipped"}>{item.title || item.id}：{item.accepted ? "已加入模拟队列" : item.reason}</li>)}</ul>
        </section>
      )}

      <section className="card table-card">
        <div className="batch-toolbar">
          <div>
            <label className="select-all"><input type="checkbox" checked={allSelected} onChange={toggleAll} /> 全选当前可处理记录</label>
            <span className="selection-count">已选择 {selected.size} 条</span>
          </div>
          <button type="button" disabled={selected.size === 0} onClick={submit}>加入模拟队列</button>
        </div>
        <div className="table-wrap">
          <table>
            <thead><tr><th>选择</th><th>岗位</th><th>平台</th><th>AI 鉴定</th><th>人工结论</th><th>当前判断</th><th>首次沟通</th></tr></thead>
            <tbody>
              {loading && <tr><td colSpan={7} className="empty">正在读取共享场景…</td></tr>}
              {!loading && view?.jobs.map((job) => (
                <tr key={job.id} className={job.queueSimulation.allowed ? "" : "unavailable"}>
                  <td><input type="checkbox" checked={selected.has(job.id)} disabled={!job.queueSimulation.allowed} onChange={() => toggle(job.id)} aria-label={`选择 ${job.title}`} /></td>
                  <td><strong>{job.title}</strong><span>{job.company} · {job.city}</span><small>{job.id}</small></td>
                  <td>{job.platformStatusText}</td><td>{job.aiConclusionText}</td><td>{job.humanConclusionText}</td><td>{job.currentJudgementText}</td>
                  <td>{job.outreachStatusText}{!job.queueSimulation.allowed ? <p className="reason">{job.queueSimulation.reason}</p> : job.becameUnavailableOnSubmit ? <p className="hint">提交时会模拟一次状态变化</p> : null}</td>
                </tr>
              ))}
              {!loading && view?.jobs.length === 0 && <tr><td colSpan={7} className="empty">当前筛选条件下没有平台岗位</td></tr>}
            </tbody>
          </table>
        </div>
      </section>
    </>
  );
}

function AssessmentsPage({ draft, setDraft }: { draft: PolicyDraft | null; setDraft: (draft: PolicyDraft | null) => void }) {
  const [policy, setPolicy] = useState<Policy | null>(null);
  const [candidateText, setCandidateText] = useState("");

  useEffect(() => {
    void fetch(`${API}/policy`).then((response) => response.json()).then((value: Policy) => setPolicy(value));
  }, []);

  useEffect(() => { setCandidateText(draft?.text ?? ""); }, [draft]);

  const generate = async () => {
    if (draft && !window.confirm("重新生成会永久丢弃当前候选稿，并再次取得一份固定原型结果。确定继续吗？")) return;
    const response = await fetch(`${API}/policy-draft`, { method: "POST" });
    setDraft(await response.json() as PolicyDraft);
  };

  const discard = () => {
    if (!window.confirm("丢弃后本次策略候选稿永久消失，重新生成会再次调用模型。确定丢弃吗？")) return;
    setDraft(null);
  };

  const diff = useMemo(() => diffLines(policy?.text ?? "", candidateText), [policy?.text, candidateText]);

  if (!policy) return <section className="card"><p>正在读取共享场景…</p></section>;

  return (
    <>
      <section className="policy-grid">
        <article className="card policy-card">
          <p className="eyebrow">当前正式策略</p><h2>{policy.name}</h2>
          <ol>{policy.rules.map((rule) => <li key={rule}>{rule}</li>)}</ol>
          <p className="muted">已有 {policy.humanReviewCount} 条仍对应当前 JD 的人工复核记录。</p>
          <button type="button" onClick={generate}>生成策略候选稿</button>
        </article>

        <article className={`card candidate-card ${draft ? "" : "empty-candidate"}`}>
          {draft ? <>
            <div className="section-heading"><div><p className="eyebrow">页面会话内存</p><h2>策略候选稿</h2></div><span className="transient-badge">不会保存</span></div>
            <p className="muted">基于 {draft.basedOnPolicy} 和 {draft.humanReviewCount} 条有效人工复核生成。编辑不会调用模型。</p>
            <label className="editor-label">完整候选策略<textarea rows={9} value={candidateText} onChange={(event) => setCandidateText(event.target.value)} /></label>
            <div className="candidate-actions"><button type="button" className="secondary" onClick={discard}>丢弃候选稿</button></div>
          </> : <div className="empty-state"><p className="eyebrow">策略候选稿</p><h2>尚未生成</h2><p>生成后只保留在当前页面；刷新、离开或关闭都会丢失。</p></div>}
        </article>
      </section>

      {draft && <section className="card diff-card">
        <div className="section-heading"><div><p className="eyebrow">浏览器本地计算</p><h2>逐行实时 diff</h2></div><span className="local-badge">0 次额外模型调用</span></div>
        <div className="diff" aria-live="polite">{diff.map((entry, index) => <div key={`${entry.type}-${index}`} className={`diff-line ${entry.type}`}>{entry.type === "added" ? "+" : entry.type === "removed" ? "−" : " "} {entry.text}</div>)}</div>
      </section>}
    </>
  );
}

function FilterSelect({ label, value, onChange, options }: { label: string; value: string; onChange: (value: string) => void; options: [string, string][] }) {
  return <label>{label}<select value={value} onChange={(event) => onChange(event.target.value)}>{options.map(([key, text]) => <option key={key} value={key}>{text}</option>)}</select></label>;
}

function filtersFromURL(): Filters {
  const params = new URLSearchParams(window.location.search);
  return {
    query: params.get("query") ?? "",
    platformStatus: params.get("platformStatus") ?? "",
    aiConclusion: params.get("aiConclusion") ?? "",
    humanConclusion: params.get("humanConclusion") ?? "",
  };
}

function filtersToQuery(filters: Filters): string {
  const params = new URLSearchParams();
  Object.entries(filters).forEach(([key, value]) => { if (value) params.set(key, value); });
  return params.toString();
}

function diffLines(beforeText: string, afterText: string): DiffEntry[] {
  const before = beforeText.split("\n");
  const after = afterText.split("\n");
  const table = Array.from({ length: before.length + 1 }, () => Array<number>(after.length + 1).fill(0));
  for (let i = before.length - 1; i >= 0; i -= 1) {
    for (let j = after.length - 1; j >= 0; j -= 1) {
      table[i][j] = before[i] === after[j] ? table[i + 1][j + 1] + 1 : Math.max(table[i + 1][j], table[i][j + 1]);
    }
  }
  const result: DiffEntry[] = [];
  let i = 0;
  let j = 0;
  while (i < before.length && j < after.length) {
    if (before[i] === after[j]) { result.push({ type: "same", text: before[i] }); i += 1; j += 1; }
    else if (table[i + 1][j] >= table[i][j + 1]) { result.push({ type: "removed", text: before[i] }); i += 1; }
    else { result.push({ type: "added", text: after[j] }); j += 1; }
  }
  while (i < before.length) result.push({ type: "removed", text: before[i++] });
  while (j < after.length) result.push({ type: "added", text: after[j++] });
  return result;
}
