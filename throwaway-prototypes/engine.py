"""THROWAWAY PROTOTYPE: simulated state for the consolidated Web demo.

This intentionally does not implement production Go business logic or any BOSS
integration. All state is process-local mock data. A generated policy suggestion
is returned to the current Web page and is never retained by this engine.
"""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parent


ASSESSMENT_LABELS = {
    "not_required": "无需鉴定",
    "not_queued": "尚未安排",
    "pending": "待鉴定",
    "processing": "鉴定中",
    "suitable": "适合",
    "unsuitable": "不适合",
    "needs_user_confirmation": "待用户确认",
    "failed": "鉴定失败",
}

OUTREACH_LABELS = {
    "not_queued": "尚未安排",
    "pending": "等待开始",
    "processing": "沟通中",
    "contacted": "已沟通",
    "simulated": "模拟完成",
    "possibly_contacted": "可能已沟通",
    "failed": "首次沟通失败",
}

VERDICT_LABELS = {
    None: "尚无当前判断",
    "suitable": "适合",
    "unsuitable": "不适合",
    "needs_user_confirmation": "需要人工确认",
    "human_review_stale": "人工结论待复核",
}

RUN_LABELS = {
    "not_started": "尚未创建",
    "preparing": "准备中",
    "running": "运行中",
    "paused": "暂停",
    "failed": "失败",
    "completed": "发现完成",
    "ended_early": "提前结束",
}


class PrototypeState:
    """Disposable state machine for the Web prototype."""

    def __init__(
        self,
        data_path: Path | None = None,
    ) -> None:
        path = data_path or ROOT / "mock-data.json"
        self._initial = json.loads(path.read_text(encoding="utf-8"))
        self.data: dict[str, Any] = copy.deepcopy(self._initial)
        self.data.setdefault(
            "assessment_policy_versions",
            [copy.deepcopy(self.data["assessment_policy"])],
        )
        stable_ids = [job["platform_job_id"] for job in self.data["jobs"] if job.get("platform_job_id")]
        if len(stable_ids) != len(set(stable_ids)):
            raise ValueError("模拟数据违反全局岗位去重：稳定 platform_job_id 必须唯一。")
        self.event_log: list[str] = ["已载入同源模拟数据；未连接 BOSS。"]

    def reset(self) -> dict[str, Any]:
        saved_settings = copy.deepcopy(self.data["settings"])
        saved_online_resume = copy.deepcopy(self.data["online_resume"])
        saved_policy = copy.deepcopy(self.data["assessment_policy"])
        saved_policy_versions = copy.deepcopy(self.data["assessment_policy_versions"])
        self.data = copy.deepcopy(self._initial)
        self.data["settings"] = saved_settings
        self.data["online_resume"] = saved_online_resume
        self.data["assessment_policy"] = saved_policy
        self.data["assessment_policy_versions"] = saved_policy_versions
        self.event_log = ["已重置岗位与发现运行模拟数据；保留已保存设置和在线简历版本。"]
        return self.snapshot()

    def _job(self, job_id: int) -> dict[str, Any]:
        for job in self.data["jobs"]:
            if job["id"] == job_id:
                return job
        raise ValueError(f"不存在的模拟岗位：{job_id}")

    def rediscover_existing_job(self, platform_job_id: str) -> dict[str, Any]:
        if not platform_job_id:
            raise ValueError("稳定 platform_job_id 缺失，不能进行全局岗位去重。")
        matches = [job for job in self.data["jobs"] if job.get("platform_job_id") == platform_job_id]
        if len(matches) != 1:
            raise ValueError("稳定 platform_job_id 在全局岗位池中必须且只能命中一条岗位。")
        job = matches[0]
        self.event_log.append(
            f"再次发现“{job['title']}”；复用同一全局岗位及其当前鉴定/沟通状态，未创建重复记录。"
        )
        return {
            "created": False,
            "job_id": job["id"],
            "assessment_status": job["assessment_status"],
            "outreach_status": job["outreach_status"],
            "state": self.snapshot(),
        }

    @staticmethod
    def effective_verdict(job: dict[str, Any]) -> str | None:
        if job.get("human_verdict") is not None:
            if not job.get("human_review_current", False):
                return "human_review_stale"
            return job["human_verdict"]
        status = job["assessment_status"]
        if status in {"suitable", "unsuitable", "needs_user_confirmation"}:
            return status
        return None

    @classmethod
    def why_not_contacted(cls, job: dict[str, Any]) -> str:
        outreach = job["outreach_status"]
        mode = job.get("outreach_mode")
        if outreach == "contacted":
            return "已确认完成真实首次沟通"
        if outreach == "possibly_contacted":
            return "可能已沟通：必须先与 BOSS 对账，禁止重试"
        if outreach == "pending":
            suffix = "模拟" if mode == "simulation" else "真实"
            return f"已安排{suffix}首次沟通，等待开始"
        if outreach == "processing":
            suffix = "模拟" if mode == "simulation" else "真实"
            return f"正在执行{suffix}轮次；本轮模式已冻结"
        if outreach == "simulated":
            return "仅完成模拟，尚未发生真实沟通"
        if outreach == "failed":
            return f"首次沟通失败：{job.get('outreach_last_error') or '等待处理'}"
        if job["platform_status"] == "closed":
            return f"未沟通：{job['platform_closed_reason']}"
        if not job.get("platform_job_id"):
            return "未沟通：稳定岗位 ID 缺失，不能安排后续处理"
        verdict = cls.effective_verdict(job)
        if verdict == "human_review_stale":
            return "未沟通：人工结论待复核，不能回退使用 AI 结论"
        if verdict == "needs_user_confirmation":
            return "未沟通：AI 待用户确认"
        if verdict == "unsuitable":
            return "未沟通：当前岗位判断为不适合"
        if verdict == "suitable":
            return "未沟通：当前适合，但尚未安排首次沟通"
        status = ASSESSMENT_LABELS[job["assessment_status"]]
        return f"未沟通：鉴定状态为“{status}”"

    @classmethod
    def outreach_eligible(cls, job: dict[str, Any], mode: str) -> bool:
        if job["platform_status"] != "open":
            return False
        if not job.get("platform_job_id"):
            return False
        if cls.effective_verdict(job) != "suitable":
            return False
        status = job["outreach_status"]
        if mode == "simulation":
            return status == "not_queued"
        return status in {"not_queued", "simulated"}

    @classmethod
    def reassessment_eligibility(cls, job: dict[str, Any]) -> dict[str, Any]:
        if job["outreach_status"] == "contacted":
            return {"eligible": False, "reason": "已真实沟通，无需重新鉴定"}
        if not job.get("platform_job_id"):
            return {"eligible": False, "reason": "稳定岗位 ID 缺失，不能安排鉴定"}
        if job["platform_status"] == "closed":
            return {"eligible": False, "reason": f"岗位已关闭：{job['platform_closed_reason']}"}
        if job["assessment_status"] == "pending":
            return {"eligible": False, "reason": "已安排鉴定，正在等待开始"}
        if job["assessment_status"] == "processing":
            return {"eligible": False, "reason": "岗位鉴定处理中"}
        return {"eligible": True, "reason": "可以重新鉴定"}

    @classmethod
    def outreach_eligibility(cls, job: dict[str, Any], mode: str) -> dict[str, Any]:
        outreach = job["outreach_status"]
        if outreach == "contacted":
            return {"eligible": False, "reason": "已真实沟通，不会重复打招呼"}
        if outreach == "pending":
            frozen = "模拟" if job.get("outreach_mode") == "simulation" else "真实"
            return {"eligible": False, "reason": f"已安排{frozen}首次沟通，正在等待开始"}
        if outreach == "processing":
            frozen = job.get("outreach_mode") or "未知"
            return {"eligible": False, "reason": f"首次沟通处理中（{frozen}，模式已冻结）"}
        if outreach == "possibly_contacted":
            return {"eligible": False, "reason": "可能已沟通，必须先与 BOSS 对账"}
        if outreach == "failed":
            return {"eligible": False, "reason": "首次沟通失败，应使用失败重试动作"}
        if not job.get("platform_job_id"):
            return {"eligible": False, "reason": "稳定岗位 ID 缺失，不能安排首次沟通"}
        if job["platform_status"] == "closed":
            return {"eligible": False, "reason": f"岗位已关闭：{job['platform_closed_reason']}"}
        verdict = cls.effective_verdict(job)
        if verdict == "human_review_stale":
            return {"eligible": False, "reason": "人工结论待复核"}
        if verdict == "needs_user_confirmation":
            return {"eligible": False, "reason": "当前判断待用户确认"}
        if verdict == "unsuitable":
            return {"eligible": False, "reason": "当前岗位判断不适合"}
        if verdict != "suitable":
            return {
                "eligible": False,
                "reason": f"尚无适合结论：{ASSESSMENT_LABELS[job['assessment_status']]}",
            }
        if mode == "simulation" and outreach == "simulated":
            return {"eligible": False, "reason": "已完成模拟，不会重复安排"}
        if mode == "real" and outreach not in {"not_queued", "simulated"}:
            return {"eligible": False, "reason": "当前状态不能安排真实发送"}
        if mode == "simulation" and outreach != "not_queued":
            return {"eligible": False, "reason": "当前状态不能安排模拟沟通"}
        action = "模拟沟通" if mode == "simulation" else "真实发送"
        return {"eligible": True, "reason": f"可以安排{action}"}

    @staticmethod
    def outreach_label(job: dict[str, Any]) -> str:
        status = job["outreach_status"]
        mode = job.get("outreach_mode")
        if status == "pending":
            return "等待模拟" if mode == "simulation" else "等待真实沟通"
        if status == "processing":
            return "模拟中" if mode == "simulation" else "真实沟通中"
        return OUTREACH_LABELS[status]

    def _view_job(self, job: dict[str, Any]) -> dict[str, Any]:
        view = copy.deepcopy(job)
        view["assessment_label"] = ASSESSMENT_LABELS[job["assessment_status"]]
        view["outreach_label"] = self.outreach_label(job)
        view["effective_verdict"] = self.effective_verdict(job)
        view["current_verdict_label"] = VERDICT_LABELS[view["effective_verdict"]]
        view["why_not_contacted"] = self.why_not_contacted(job)
        simulation_eligibility = self.outreach_eligibility(job, "simulation")
        real_eligibility = self.outreach_eligibility(job, "real")
        if not self.data["settings"].get("outreach_greeting_text"):
            if simulation_eligibility["eligible"]:
                simulation_eligibility = {"eligible": False, "reason": "固定招呼语尚未配置"}
            if real_eligibility["eligible"]:
                real_eligibility = {"eligible": False, "reason": "固定招呼语尚未配置"}
        view["batch_eligibility"] = {
            "reassessment": self.reassessment_eligibility(job),
            "outreach_simulation": simulation_eligibility,
            "outreach_real": real_eligibility,
        }
        view["review_eligibility"] = self.review_eligibility(job)
        view["review_attention"] = (
            view["effective_verdict"] == "human_review_stale"
            or (
                job["assessment_status"] == "needs_user_confirmation"
                and not (job.get("human_verdict") and job.get("human_review_current", False))
            )
        )
        if job["assessment_status"] == "pending":
            view["assessment_usage_note"] = (
                "已安排，尚未开始；此时不绑定在线简历或策略。真正开始时才使用数据库中最后一次手工保存的当前版本。"
            )
        elif job["assessment_status"] == "processing":
            view["assessment_usage_note"] = (
                f"本次实际使用：在线简历 v{job.get('assessment_actual_resume_version', '?')}、"
                f"策略 v{job.get('assessment_actual_policy_version', '?')}。"
            )
        elif job.get("assessment_actual_resume_version") and job.get("assessment_actual_policy_version"):
            view["assessment_usage_note"] = (
                f"本次实际使用：在线简历 v{job['assessment_actual_resume_version']}、"
                f"策略 v{job['assessment_actual_policy_version']}。"
            )
        else:
            view["assessment_usage_note"] = None
        view["assessment_agent_input_note"] = (
            "Agent 收到完整在线简历内容，不是内部编号。"
            if job.get("assessment_agent_received_full_resume")
            else None
        )
        if job["outreach_status"] == "pending" and job.get("outreach_mode") == "real":
            windows = ", ".join(
                f"{item['start']}-{item['end']}"
                for item in self.data["settings"]["outreach_time_windows"]
            )
            view["why_not_contacted"] = (
                f"已安排真实首次沟通，等待当前发送时间窗（{windows}）"
                if windows
                else "已安排真实首次沟通；未设置时间窗，全天可开始真实发送"
            )
        return view

    @classmethod
    def review_eligibility(cls, job: dict[str, Any]) -> dict[str, Any]:
        if job.get("human_verdict") is not None and job.get("human_review_current", False):
            return {"eligible": True, "reason": "已有人工结论，可以再次复核并覆盖"}
        if cls.effective_verdict(job) == "human_review_stale":
            return {"eligible": True, "reason": "当前 JD 已变化，需要重新复核"}
        if job["assessment_status"] == "suitable":
            return {"eligible": True, "reason": "可以核对 AI 的“适合”判断是否正确"}
        if job["assessment_status"] == "unsuitable":
            return {"eligible": True, "reason": "可以核对 AI 的“不适合”判断是否正确"}
        if job["assessment_status"] == "needs_user_confirmation":
            return {"eligible": True, "reason": "AI 需要人工确认"}
        return {"eligible": True, "reason": "可以根据当前 JD 直接给出人工结论"}

    def snapshot(self) -> dict[str, Any]:
        jobs = [self._view_job(job) for job in self.data["jobs"]]
        run = copy.deepcopy(self.data["discovery_run"])
        run["status_label"] = RUN_LABELS[run["status"]]
        settings = self.data["settings"]
        online_resume = self.data["online_resume"]
        discovery_notices: list[str] = []
        if online_resume.get("current_version") is None:
            discovery_notices.append(
                "尚未保存在线简历版本。请先到在线简历页手动点击“刷新在线简历”，成功后才能开始岗位发现。"
            )
        if not settings["automatic_assessment_enabled"]:
            discovery_notices.append(
                "自动岗位鉴定当前已关闭，新发现岗位将只保存，暂不进行 AI 鉴定。"
            )
        if not settings["automatic_outreach_enabled"]:
            discovery_notices.append(
                "自动首次沟通当前已关闭，新发现岗位不会自动安排首次沟通。"
            )
        if not settings.get("outreach_greeting_text"):
            discovery_notices.append(
                "固定招呼语尚未配置，不影响岗位发现；配置前不能安排首次沟通。"
            )
        counts = {
            "total": len(jobs),
            "contacted": sum(j["outreach_status"] == "contacted" for j in jobs),
            "not_contacted": sum(j["outreach_status"] != "contacted" for j in jobs),
            "assessment_not_queued": sum(j["assessment_status"] == "not_queued" for j in jobs),
            "assessment_pending": sum(j["assessment_status"] == "pending" for j in jobs),
            "assessment_processing": sum(j["assessment_status"] == "processing" for j in jobs),
            "outreach_pending": sum(j["outreach_status"] == "pending" for j in jobs),
            "outreach_processing": sum(j["outreach_status"] == "processing" for j in jobs),
            "simulation_inflight": sum(
                j["outreach_status"] in {"pending", "processing"}
                and j.get("outreach_mode") == "simulation"
                for j in jobs
            ),
        }
        return {
            "prototype_notice": self.data["prototype_notice"],
            "online_resume": copy.deepcopy(online_resume),
            "discovery_run": run,
            "assessment_policy": copy.deepcopy(self.data["assessment_policy"]),
            "assessment_policy_versions": copy.deepcopy(
                self.data["assessment_policy_versions"]
            ),
            "settings": copy.deepcopy(self.data["settings"]),
            "jobs": jobs,
            "counts": counts,
            "event_log": self.event_log[-6:],
            "discovery_can_start": online_resume.get("current_version") is not None,
            "discovery_notices": discovery_notices,
        }

    def refresh_online_resume(self, outcome: str) -> dict[str, Any]:
        """Simulate one explicit user-triggered read of BOSS; never called by workers."""
        if outcome not in {"changed", "unchanged", "failed"}:
            raise ValueError("模拟刷新结果必须是 changed、unchanged 或 failed。")
        resume = self.data["online_resume"]
        resume["last_refresh_attempt_at"] = "2026-08-27 11:05"
        previous_version = resume.get("current_version")
        if outcome == "failed":
            resume["last_refresh_error"] = "读取 BOSS 在线简历失败：模拟登录状态已失效。旧版本继续有效。"
            self.event_log.append("用户手动刷新在线简历失败；保留旧版本，发现和鉴定均未触发自动重试。")
            return {
                "ok": False,
                "changed": False,
                "version": previous_version,
                "message": resume["last_refresh_error"],
                "state": self.snapshot(),
            }

        resume["last_refresh_error"] = None
        if outcome == "unchanged" and previous_version is not None:
            self.event_log.append(
                f"用户手动刷新在线简历成功；内容未变化，继续使用在线简历 v{previous_version}。"
            )
            return {
                "ok": True,
                "changed": False,
                "version": previous_version,
                "message": f"读取成功，内容未变化，在线简历仍为 v{previous_version}。",
                "state": self.snapshot(),
            }

        next_version = int(previous_version or 0) + 1
        resume.update(
            {
                "current_version": next_version,
                "saved_at": f"2026-08-27 {9 + next_version:02d}:05",
                "content_hash": f"resume-content-v{next_version}",
                "content": {
                    "intended_roles": ["后端开发"],
                    "target_cities": ["北京", "上海", "杭州", "深圳", "广州", "成都"],
                    "summary": f"模拟的完整 BOSS 在线简历内容 v{next_version}，仅供原型演示。",
                },
            }
        )
        self.event_log.append(f"用户手动刷新在线简历成功；内容变化，保存为在线简历 v{next_version}。")
        return {
            "ok": True,
            "changed": True,
            "version": next_version,
            "message": f"读取成功，内容已变化，已保存为在线简历 v{next_version}。",
            "state": self.snapshot(),
        }

    def simulate_first_use(self) -> dict[str, Any]:
        """Prototype-only setup for checking the no-resume-version blocking state."""
        self.data["online_resume"] = {
            "current_version": None,
            "saved_at": None,
            "content_hash": None,
            "content": None,
            "last_refresh_attempt_at": None,
            "last_refresh_error": None,
        }
        self.data["discovery_run"] = {
            "id": 0,
            "name": "尚未创建岗位发现",
            "status": "not_started",
            "status_reason": "首次使用前，需要先手动刷新在线简历。",
            "current_role": "—",
            "current_city": "—",
            "next_page": 1,
            "attempt_no": 0,
            "last_progress_at": None,
            "scopes_done": 0,
            "scopes_total": 0,
            "jobs_observed": 0,
            "online_resume_version_used": None,
        }
        self.event_log.append("切换到首次使用模拟场景：尚无在线简历版本，岗位发现被禁用。")
        return self.snapshot()

    def continue_discovery(self) -> dict[str, Any]:
        run = self.data["discovery_run"]
        if run["status"] not in {"paused", "failed"}:
            raise ValueError("只有暂停或失败的岗位发现运行可以继续。")
        run["status"] = "running"
        run["status_reason"] = None
        run["attempt_no"] += 1
        self.event_log.append(f"继续岗位发现运行 #{run['id']}，从第 {run['next_page']} 页恢复。")
        return self.snapshot()

    def start_discovery(self) -> dict[str, Any]:
        run = self.data["discovery_run"]
        if run["status"] in {"preparing", "running", "paused", "failed"}:
            raise ValueError("当前仍有一个未结束的岗位发现运行；请继续或提前结束它。")
        resume_version = self.data["online_resume"].get("current_version")
        if resume_version is None:
            raise ValueError("尚未保存在线简历版本。请先到在线简历页手动点击“刷新在线简历”。")
        new_id = int(run["id"]) + 1
        self.data["discovery_run"] = {
            **copy.deepcopy(self._initial["discovery_run"]),
            "id": new_id,
            "name": "新建的模拟岗位发现运行",
            "status": "running",
            "status_reason": None,
            "next_page": 1,
            "attempt_no": 1,
            "last_progress_at": "刚刚",
            "scopes_done": 0,
            "jobs_observed": 0,
            "online_resume_version_used": resume_version,
        }
        self.event_log.append(
            f"创建并开始岗位发现运行 #{new_id}；使用已手工保存的在线简历 v{resume_version}，未读取 BOSS。"
        )
        return self.snapshot()

    def end_discovery_early(self, reason: str) -> dict[str, Any]:
        run = self.data["discovery_run"]
        if run["status"] not in {"preparing", "running", "paused", "failed"}:
            raise ValueError("当前没有可以提前结束的岗位发现运行。")
        run["status"] = "ended_early"
        run["status_reason"] = (reason or "求职者主动提前结束").strip()
        self.event_log.append(f"提前结束岗位发现运行 #{run['id']}；现在可以创建新运行。")
        return self.snapshot()

    def pause_discovery(self) -> dict[str, Any]:
        run = self.data["discovery_run"]
        if run["status"] != "running":
            raise ValueError("只有运行中的岗位发现运行可以暂停。")
        run["status"] = "paused"
        run["status_reason"] = "用户从原型界面主动暂停"
        self.event_log.append(f"暂停岗位发现运行 #{run['id']}，检查点保持在第 {run['next_page']} 页。")
        return self.snapshot()

    def queue_assessment(self, job_ids: list[int]) -> dict[str, Any]:
        processed: list[dict[str, Any]] = []
        skipped: list[dict[str, Any]] = []
        for job_id in job_ids:
            job = self._job(job_id)
            eligibility = self.reassessment_eligibility(job)
            if not eligibility["eligible"]:
                skipped.append({"id": job_id, "title": job["title"], "reason": eligibility["reason"]})
                continue
            job["assessment_status"] = "pending"
            job["assessment_reason"] = None
            for key in (
                "assessment_actual_resume_version",
                "assessment_actual_policy_version",
                "assessment_agent_received_full_resume",
                "assessment_agent_resume_content",
            ):
                job.pop(key, None)
            processed.append({"id": job_id, "title": job["title"]})
        admitted = len(processed)
        self.event_log.append(f"手工批量安排鉴定：{admitted} 个岗位。")
        return {
            "admitted": admitted,
            "processed": processed,
            "skipped": skipped,
            "summary": f"成功 {admitted} 个，另有 {len(skipped)} 个因状态变化未处理。",
            "state": self.snapshot(),
        }

    def queue_outreach(self, job_ids: list[int], mode: str) -> dict[str, Any]:
        if mode not in {"simulation", "real"}:
            raise ValueError("首次沟通模式必须是 simulation 或 real。")
        greeting = self.data["settings"].get("outreach_greeting_text")
        if not greeting:
            raise ValueError("尚未配置固定招呼语。")
        processed: list[dict[str, Any]] = []
        skipped: list[dict[str, Any]] = []
        for job_id in job_ids:
            job = self._job(job_id)
            eligibility = self.outreach_eligibility(job, mode)
            if not eligibility["eligible"]:
                skipped.append({"id": job_id, "title": job["title"], "reason": eligibility["reason"]})
                continue
            job["outreach_status"] = "pending"
            job["outreach_mode"] = mode
            job["outreach_greeting_text"] = greeting
            processed.append({"id": job_id, "title": job["title"]})
        admitted = len(processed)
        action = "模拟沟通" if mode == "simulation" else "真实发送"
        self.event_log.append(f"手工批量安排{action}：{admitted} 个岗位。")
        result = {
            "admitted": admitted,
            "processed": processed,
            "skipped": skipped,
            "summary": f"成功 {admitted} 个，另有 {len(skipped)} 个因状态变化未处理。",
            "state": self.snapshot(),
        }
        if mode == "real":
            result["real_authorization"] = {
                "selected_job_count": len(job_ids),
                "admitted_job_count": admitted,
                "greeting_text": greeting,
                "time_windows": copy.deepcopy(self.data["settings"]["outreach_time_windows"]),
                "automatic_outreach_unchanged": True,
            }
        return result

    def review_job(self, job_id: int, verdict: str) -> dict[str, Any]:
        if verdict not in {"suitable", "unsuitable"}:
            raise ValueError("人工结论必须是 suitable 或 unsuitable。")
        job = self._job(job_id)
        eligibility = self.review_eligibility(job)
        if not eligibility["eligible"]:
            return {
                "processed": False,
                "job_id": job_id,
                "title": job["title"],
                "reason": eligibility["reason"],
                "state": self.snapshot(),
            }
        job["human_verdict"] = verdict
        job["human_review_current"] = True
        self.event_log.append(
            f"人工复核“{job['title']}”为{'适合' if verdict == 'suitable' else '不适合'}。"
        )
        return {
            "processed": True,
            "job_id": job_id,
            "title": job["title"],
            "verdict": verdict,
            "state": self.snapshot(),
        }

    def generate_policy_suggestion(self) -> dict[str, Any]:
        """Simulate one model call and return a page-session-only suggestion."""
        labels = [
            job
            for job in self.data["jobs"]
            if job.get("human_verdict") is not None
            and job.get("human_review_current", False)
        ]
        current = self.data["assessment_policy"]
        disagreements = [
            job
            for job in labels
            if job["assessment_status"] in {"suitable", "unsuitable"}
            and job["assessment_status"] != job["human_verdict"]
        ]
        if not labels:
            return {
                "available": False,
                "base_version": current["version_no"],
                "source_label_count": 0,
                "positive_label_count": 0,
                "negative_label_count": 0,
                "message": "当前没有基于最新 JD 的有效人工标注；先到岗位页完成人工复核。",
                "input_scope_note": "只排除 JD 已变化、人工结论待复核的标注。",
                "generation_note": "本次只读取点击生成时已经存在的信息；未产生临时候选稿。",
                "current_policy_unchanged": True,
                "simulated_llm_call_count": 0,
            }
        if disagreements:
            learned_rule = (
                "当岗位存在明确的可迁移后端经验时，不因技术栈名称不同直接判为不适合；"
                "证据不足时仍交给人工确认。"
            )
        else:
            learned_rule = "继续保持保守三态；只有明确且重要的不匹配才判为不适合。"
        candidate_rules = [*current["rules"]]
        if learned_rule not in candidate_rules:
            candidate_rules.append(learned_rule)
        proposed_version = current["version_no"] + 1
        candidate_title = f"默认岗位鉴定策略 v{proposed_version}（候选稿）"
        candidate_full_text = "\n".join(
            [
                candidate_title,
                "",
                "目标：根据已保存的在线简历与岗位 JD，给出保守、可追溯的三态结论。",
                "",
                "完整判定规则：",
                *[f"{index}. {rule}" for index, rule in enumerate(candidate_rules, start=1)],
                "",
                "输出只能是：适合 / 不适合 / 需要人工确认，并附上支持结论的 JD 证据。",
            ]
        )
        generated = {
            "available": True,
            "base_version": current["version_no"],
            "proposed_version": proposed_version,
            "source_label_count": len(labels),
            "positive_label_count": sum(job["human_verdict"] == "suitable" for job in labels),
            "negative_label_count": sum(job["human_verdict"] == "unsuitable" for job in labels),
            "disagreement_count": len(disagreements),
            "input_scope_note": (
                "已输入所有仍对应当前 JD 的人工“适合”和“不适合”标注；不要求岗位已有 AI 结论，"
                "也不因 AI 使用旧策略、AI 与人一致或 AI 判为不适合而排除。"
            ),
            "generation_note": (
                "这份建议只依据生成当时启用的策略和生成当时已有的有效人工标注；"
                "以后再次生成会读取届时的信息，结果可能不同。"
            ),
            "candidate": {
                "title": candidate_title,
                "rules": candidate_rules,
                "full_text": candidate_full_text,
            },
            "current_policy_unchanged": True,
            "simulated_llm_call_count": 1,
        }
        self.event_log.append(
            f"用户显式生成默认策略 v{proposed_version} 临时建议；本次模拟 1 次模型调用，"
            "结果已返回当前页面且未由后台保存。"
        )
        return copy.deepcopy(generated)

    def activate_policy_version(
        self,
        base_version: int,
        full_text: str,
    ) -> dict[str, Any]:
        current = self.data["assessment_policy"]
        if current["version_no"] != base_version:
            raise ValueError("当前启用策略已经变化；请基于最新信息重新生成。")
        normalized_full_text = (full_text or "").strip()
        if not normalized_full_text:
            raise ValueError("完整策略候选稿不能为空。")
        normalized_rules = []
        for line in normalized_full_text.splitlines():
            prefix, separator, rule = line.strip().partition(". ")
            if separator and prefix.isdigit() and rule:
                normalized_rules.append(rule)
        if not normalized_rules:
            normalized_rules = [normalized_full_text]
        next_version = current["version_no"] + 1
        next_policy = {
            **copy.deepcopy(current),
            "id": next_version,
            "version_no": next_version,
            "name": f"默认岗位鉴定策略 v{next_version}",
            "display_name": f"默认策略 v{next_version}",
            "rules": normalized_rules,
            "full_text": normalized_full_text,
            "version_note": "由用户采用完整候选稿后创建；旧版本保持不可变并可追溯。",
            "origin": "用户采用完整策略候选稿",
        }
        self.data["assessment_policy"] = next_policy
        self.data["assessment_policy_versions"].append(copy.deepcopy(next_policy))
        self.event_log.append(
            f"用户采用当前页面中的完整编辑文本，创建并启用默认策略 v{next_version}。"
        )
        return {
            "accepted": True,
            "new_version": next_version,
            "previous_version_preserved": True,
            "additional_llm_call_count": 0,
            "state": self.snapshot(),
        }

    def configure_assessment(self, enabled: bool, limit: int) -> dict[str, Any]:
        if limit < 1:
            raise ValueError("AI 同时鉴定数必须为正整数。")
        settings = self.data["settings"]
        settings["automatic_assessment_enabled"] = enabled
        settings["assessment_processing_limit"] = limit
        admitted = 0
        if enabled:
            eligible = [
                job["id"]
                for job in self.data["jobs"]
                if job["platform_status"] == "open"
                and job.get("platform_job_id")
                and job["outreach_status"] != "contacted"
                and job["assessment_status"] == "not_queued"
            ]
            admitted = self.queue_assessment(eligible)["admitted"]
        self.event_log.append(f"自动岗位鉴定={'开启' if enabled else '关闭'}；同时鉴定上限={limit}。")
        return {"admitted": admitted, "state": self.snapshot()}

    def preview_outreach_change(self, enabled: bool, mode: str) -> dict[str, int | bool | str]:
        if mode not in {"simulation", "real"}:
            raise ValueError("首次沟通模式必须是 simulation 或 real。")
        real_queue_count = 0
        if enabled and mode == "real":
            real_queue_count = sum(
                self.outreach_eligible(job, "real") for job in self.data["jobs"]
            )
        simulation_inflight_count = sum(
            job["outreach_status"] in {"pending", "processing"}
            and job.get("outreach_mode") == "simulation"
            for job in self.data["jobs"]
        )
        return {
            "enabled": enabled,
            "mode": mode,
            "real_queue_count": real_queue_count,
            "simulation_inflight_count": simulation_inflight_count,
        }

    def configure_outreach(
        self,
        enabled: bool,
        mode: str,
        greeting_text: str,
        time_windows: list[dict[str, str]],
    ) -> dict[str, Any]:
        if mode not in {"simulation", "real"}:
            raise ValueError("首次沟通模式必须是 simulation 或 real。")
        normalized_greeting = (greeting_text or "").strip()
        if enabled and not normalized_greeting:
            raise ValueError("开启自动首次沟通前必须配置固定招呼语。")
        settings = self.data["settings"]
        settings["automatic_outreach_enabled"] = enabled
        settings["automatic_outreach_mode"] = mode
        settings["outreach_greeting_text"] = normalized_greeting or None
        settings["outreach_time_windows"] = copy.deepcopy(time_windows)
        admitted = 0
        if enabled:
            eligible = [
                job["id"] for job in self.data["jobs"] if self.outreach_eligible(job, mode)
            ]
            admitted = self.queue_outreach(eligible, mode)["admitted"]
        self.event_log.append(
            f"自动首次沟通={'开启' if enabled else '关闭'}，模式={mode}，新安排={admitted}。"
        )
        return {"admitted": admitted, "state": self.snapshot()}

    def tick(self) -> dict[str, Any]:
        """Advance one safe, simulated worker step. No external action exists."""
        run = self.data["discovery_run"]
        if run["status"] == "running":
            run["next_page"] += 1
            run["jobs_observed"] += 2
            run["last_progress_at"] = "刚刚"

        processing_assessments = sum(
            job["assessment_status"] == "processing" for job in self.data["jobs"]
        )
        free = max(
            0,
            self.data["settings"]["assessment_processing_limit"] - processing_assessments,
        )
        current_resume = self.data["online_resume"]
        for job in self.data["jobs"]:
            if free and job["assessment_status"] == "pending":
                if current_resume.get("current_version") is None:
                    continue
                job["assessment_status"] = "processing"
                job["assessment_actual_resume_version"] = current_resume["current_version"]
                job["assessment_actual_policy_version"] = self.data["assessment_policy"]["version_no"]
                job["assessment_agent_received_full_resume"] = True
                job["assessment_agent_resume_content"] = copy.deepcopy(current_resume["content"])
                free -= 1
            elif job["assessment_status"] == "processing":
                job["assessment_status"] = "suitable"
                job["assessment_reason"] = "原型模拟完成：服务端能力与岗位职责存在直接证据。"

        for job in self.data["jobs"]:
            if job["outreach_status"] == "processing":
                if job.get("outreach_mode") == "simulation":
                    job["outreach_status"] = "simulated"
                else:
                    job["outreach_status"] = "contacted"
            elif job["outreach_status"] == "pending":
                job["outreach_status"] = "processing"

        self.event_log.append("推进一个模拟 Worker 节拍；未发生任何 BOSS 外部动作。")
        return self.snapshot()


def smoke_check() -> dict[str, Any]:
    state = PrototypeState()
    prototype_greeting = "您好，我对这个岗位很感兴趣，希望进一步沟通。"
    initial_state = state.snapshot()
    assert initial_state["discovery_can_start"] is True
    assert initial_state["discovery_notices"][0] == "自动岗位鉴定当前已关闭，新发现岗位将只保存，暂不进行 AI 鉴定。"
    assert initial_state["online_resume"]["current_version"] == 1
    assert state._job(112)["assessment_status"] == "not_queued"
    assert "policy_version" not in initial_state["discovery_run"]
    assert state._job(112).get("assessment_actual_policy_version") is None
    assert state._job(112).get("assessment_actual_resume_version") is None
    assert initial_state["assessment_policy"]["name"] == "默认岗位鉴定策略 v1"
    assert initial_state["assessment_policy"]["display_name"] == "默认策略 v1"
    assert initial_state["assessment_policy"]["is_active"] is True
    assert all(job["jd"]["responsibilities"] and job["jd"]["requirements"] for job in initial_state["jobs"])
    before_rediscovery_count = len(state.data["jobs"])
    rediscovered = state.rediscover_existing_job("boss-go-101")
    assert rediscovered["created"] is False
    assert rediscovered["job_id"] == 101
    assert rediscovered["assessment_status"] == "suitable"
    assert rediscovered["outreach_status"] == "contacted"
    assert len(state.data["jobs"]) == before_rediscovery_count
    assert state.reassessment_eligibility(state._job(101))["eligible"] is False
    assert state.outreach_eligibility(state._job(101), "real")["eligible"] is False
    assert state._job(101)["assessment_reason"]
    assert state.reassessment_eligibility(state._job(108))["reason"] == "稳定岗位 ID 缺失，不能安排鉴定"
    assert state.outreach_eligibility(state._job(108), "simulation")["reason"] == "稳定岗位 ID 缺失，不能安排首次沟通"
    preview = state.preview_outreach_change(True, "real")
    assert preview["real_queue_count"] == 2
    assert preview["simulation_inflight_count"] == 2
    assessment_state = PrototypeState()
    queued = assessment_state.queue_assessment([112])
    assert queued["admitted"] == 1
    assert assessment_state._job(112).get("assessment_actual_resume_version") is None
    assert assessment_state._job(112).get("assessment_actual_policy_version") is None
    changed_refresh = assessment_state.refresh_online_resume("changed")
    assert changed_refresh["changed"] is True
    assert assessment_state.data["online_resume"]["current_version"] == 2
    assert assessment_state.data["discovery_run"]["online_resume_version_used"] == 1
    saved_at_v2 = assessment_state.data["online_resume"]["saved_at"]
    unchanged_refresh = assessment_state.refresh_online_resume("unchanged")
    assert unchanged_refresh["changed"] is False
    assert assessment_state.data["online_resume"]["current_version"] == 2
    assert assessment_state.data["online_resume"]["saved_at"] == saved_at_v2
    failed_refresh = assessment_state.refresh_online_resume("failed")
    assert failed_refresh["ok"] is False
    assert assessment_state.data["online_resume"]["current_version"] == 2
    assert assessment_state.data["online_resume"]["saved_at"] == saved_at_v2
    assessment_state.data["assessment_policy"] = {
        **assessment_state.data["assessment_policy"],
        "id": 2,
        "version_no": 2,
        "name": "默认岗位鉴定策略 v2",
        "display_name": "默认策略 v2",
    }
    assessment_state.continue_discovery()
    assessment_state.tick()
    assert assessment_state.data["discovery_run"]["online_resume_version_used"] == 1
    assert assessment_state.data["online_resume"]["current_version"] == 2
    assert assessment_state._job(112)["assessment_status"] == "processing"
    assert assessment_state._job(112)["assessment_actual_resume_version"] == 2
    assert assessment_state._job(112)["assessment_actual_policy_version"] == 2
    assert assessment_state._job(112)["assessment_agent_resume_content"] == assessment_state.data["online_resume"]["content"]
    assert assessment_state._job(113)["assessment_actual_resume_version"] == 1
    assert assessment_state._job(113)["assessment_actual_policy_version"] == 1
    configured = state.configure_outreach(
        True,
        "real",
        prototype_greeting,
        [{"start": "10:00", "end": "12:00"}, {"start": "14:00", "end": "17:30"}],
    )
    assert configured["admitted"] == 2
    assert state._job(104)["outreach_mode"] == "simulation"
    assert state._job(105)["outreach_mode"] == "simulation"
    assert state._job(102)["outreach_mode"] == "real"
    assert state._job(103)["outreach_mode"] == "real"
    race = PrototypeState().queue_assessment([112, 109])
    assert race["admitted"] == 1
    assert len(race["skipped"]) == 1
    assert race["skipped"][0]["reason"] == "已安排鉴定，正在等待开始"
    outreach_guard_state = PrototypeState()
    outreach_guard_state.data["settings"]["outreach_greeting_text"] = prototype_greeting
    outreach_guard = outreach_guard_state.queue_outreach([103, 104, 107, 110], "simulation")
    assert outreach_guard["admitted"] == 1
    assert len(outreach_guard["skipped"]) == 3
    manual_real_state = PrototypeState()
    manual_real_state.data["settings"]["automatic_outreach_enabled"] = False
    manual_real_state.data["settings"]["outreach_greeting_text"] = prototype_greeting
    manual_real = manual_real_state.queue_outreach([102, 103], "real")
    assert manual_real["admitted"] == 2
    assert manual_real_state.data["settings"]["automatic_outreach_enabled"] is False
    assert manual_real["real_authorization"]["automatic_outreach_unchanged"] is True
    full_day_state = PrototypeState()
    full_day_result = full_day_state.configure_outreach(
        True,
        "real",
        prototype_greeting,
        [],
    )
    assert full_day_result["admitted"] == 2
    assert full_day_state.data["settings"]["outreach_time_windows"] == []
    historical_verdict_state = PrototypeState()
    historical_job = historical_verdict_state._job(103)
    historical_job["assessment_actual_resume_version"] = 1
    historical_job["assessment_actual_policy_version"] = 1
    historical_verdict_state.refresh_online_resume("changed")
    historical_verdict_state.data["assessment_policy"] = {
        **historical_verdict_state.data["assessment_policy"],
        "id": 2,
        "version_no": 2,
        "name": "默认岗位鉴定策略 v2",
        "display_name": "默认策略 v2",
    }
    historical_result = historical_verdict_state.configure_outreach(
        True,
        "real",
        prototype_greeting,
        [],
    )
    assert historical_result["admitted"] == 2
    assert historical_job["assessment_status"] == "suitable"
    assert historical_job["outreach_status"] == "pending"
    assert historical_job["assessment_actual_resume_version"] == 1
    assert historical_job["assessment_actual_policy_version"] == 1
    rediscovery_state = PrototypeState()
    rediscovered_job = rediscovery_state._job(101)
    rediscovery_state.refresh_online_resume("changed")
    rediscovery_state.end_discovery_early("切换到新版本搜索")
    rediscovery_state.start_discovery()
    assert rediscovery_state.data["discovery_run"]["online_resume_version_used"] == 2
    assert rediscovered_job["assessment_status"] == "suitable"
    assert rediscovered_job["assessment_actual_resume_version"] == 1
    assert rediscovered_job["assessment_actual_policy_version"] == 1
    saved_settings_state = PrototypeState()
    saved_settings_state.configure_assessment(True, 2)
    saved_settings_state.configure_outreach(False, "simulation", prototype_greeting, [])
    saved_settings_state.refresh_online_resume("changed")
    saved_settings_state.reset()
    assert saved_settings_state.data["settings"]["automatic_assessment_enabled"] is True
    assert saved_settings_state.data["settings"]["assessment_processing_limit"] == 2
    assert saved_settings_state.data["settings"]["outreach_greeting_text"] == prototype_greeting
    assert saved_settings_state.data["online_resume"]["current_version"] == 2
    positive_limit_state = PrototypeState()
    positive_limit_state.configure_assessment(False, 12)
    assert positive_limit_state.data["settings"]["assessment_processing_limit"] == 12
    review_state = PrototypeState()
    assert review_state.snapshot()["jobs"][6]["review_eligibility"]["eligible"] is True
    assert review_state.snapshot()["jobs"][6]["assessment_status"] == "unsuitable"
    assert review_state.snapshot()["jobs"][9]["review_eligibility"]["eligible"] is True
    for reviewed_job in review_state.data["jobs"]:
        if reviewed_job.get("human_review_current"):
            reviewed_job["human_verdict"] = None
            reviewed_job["human_review_current"] = False
            reviewed_job.pop("human_review_note", None)
    no_labels = review_state.generate_policy_suggestion()
    assert no_labels["available"] is False
    assert no_labels["current_policy_unchanged"] is True
    assert no_labels["simulated_llm_call_count"] == 0
    assert review_state.review_job(107, "suitable")["processed"] is True
    assert review_state._job(107)["assessment_status"] == "unsuitable"
    assert review_state.effective_verdict(review_state._job(107)) == "suitable"
    assert review_state.review_job(112, "unsuitable")["processed"] is True
    assert review_state._job(112)["assessment_status"] == "not_queued"
    assert review_state.review_job(101, "suitable")["processed"] is True
    candidate = review_state.generate_policy_suggestion()
    assert candidate["available"] is True
    assert candidate["source_label_count"] == 3
    assert candidate["positive_label_count"] == 2
    assert candidate["negative_label_count"] == 1
    assert candidate["disagreement_count"] == 1
    assert candidate["current_policy_unchanged"] is True
    assert candidate["simulated_llm_call_count"] == 1
    assert "完整判定规则" in candidate["candidate"]["full_text"]
    assert review_state.data["assessment_policy"]["version_no"] == 1
    reopened_state = PrototypeState()
    assert reopened_state.data["assessment_policy"]["version_no"] == 1
    assert review_state.review_job(106, "suitable")["processed"] is True
    regenerated = review_state.generate_policy_suggestion()
    assert regenerated["simulated_llm_call_count"] == 1
    assert regenerated["source_label_count"] == candidate["source_label_count"] + 1
    assert "结果可能不同" in regenerated["generation_note"]
    edited_full_text = candidate["candidate"]["full_text"] + "\n\n人工编辑：优先检查可迁移的后端工程经验。"
    accepted_candidate = review_state.activate_policy_version(
        candidate["base_version"],
        edited_full_text,
    )
    assert accepted_candidate["new_version"] == 2
    assert accepted_candidate["additional_llm_call_count"] == 0
    assert review_state.data["assessment_policy"]["version_no"] == 2
    assert review_state.data["assessment_policy"]["full_text"] == edited_full_text
    assert len(review_state.data["assessment_policy_versions"]) == 2
    assert review_state.data["assessment_policy_versions"][0]["version_no"] == 1
    assert review_state.snapshot()["jobs"][5]["review_eligibility"]["eligible"] is True
    discovery_state = PrototypeState()
    discovery_state.end_discovery_early("原型验证")
    discovery_state.start_discovery()
    assert discovery_state.snapshot()["discovery_run"]["status"] == "running"
    assert discovery_state.snapshot()["discovery_run"]["online_resume_version_used"] == 1
    assert "policy_version" not in discovery_state.snapshot()["discovery_run"]
    first_use_state = PrototypeState()
    first_use_state.simulate_first_use()
    assert first_use_state.snapshot()["discovery_can_start"] is False
    try:
        first_use_state.start_discovery()
        raise AssertionError("无在线简历版本时不应允许开始岗位发现")
    except ValueError as error:
        assert "刷新在线简历" in str(error)
    first_use_state.refresh_online_resume("changed")
    first_use_state.start_discovery()
    assert first_use_state.snapshot()["discovery_run"]["online_resume_version_used"] == 1
    return {
        "preview": preview,
        "assessment_admitted": queued["admitted"],
        "real_admitted": configured["admitted"],
        "inflight_modes_preserved": True,
        "submit_revalidation": race["summary"],
        "outreach_disabled_reasons": [item["reason"] for item in outreach_guard["skipped"]],
        "manual_real_does_not_enable_automatic_outreach": not manual_real_state.data["settings"]["automatic_outreach_enabled"],
        "empty_time_windows_mean_full_day": full_day_state.data["settings"]["outreach_time_windows"] == [],
        "historical_valid_verdict_survives_version_changes": True,
        "unchanged_rediscovered_job_reuses_prior_verdict": True,
        "mock_reset_preserves_saved_settings": True,
        "assessment_limit_accepts_positive_integer": True,
        "discovery_not_blocked_by_initial_settings": initial_state["discovery_can_start"],
        "new_job_without_assessment_stays": "not_queued",
        "discovery_does_not_bind_assessment_policy": True,
        "assessment_basis_selected_when_worker_starts": True,
        "resume_refresh_only_by_explicit_user_action": True,
        "unchanged_resume_does_not_increment_version": True,
        "failed_resume_refresh_preserves_old_version": True,
        "first_use_without_resume_blocks_discovery": True,
        "agent_receives_full_resume_content": True,
        "discovery_run_keeps_start_resume_version": True,
        "pending_assessment_uses_current_resume_independently": True,
        "manual_review_flow": True,
        "ai_unsuitable_is_human_reviewable": True,
        "policy_suggestion_uses_generation_time_policy_and_labels": True,
        "each_generation_simulates_exactly_one_llm_call": True,
        "policy_suggestion_is_not_in_backend_snapshot": True,
        "edited_full_text_can_activate_new_policy_version": True,
        "later_generation_sees_new_human_labels": True,
        "stable_platform_job_id_deduplicates_global_job_state": True,
        "contacted_jobs_keep_verdicts_but_cannot_reenter_processing": True,
        "missing_stable_id_is_processing_ineligibility_not_a_verdict": True,
        "discovery_end_then_create": True,
    }


if __name__ == "__main__":
    print(json.dumps(smoke_check(), ensure_ascii=False, indent=2))
