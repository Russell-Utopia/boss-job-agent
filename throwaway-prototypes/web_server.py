#!/usr/bin/env python3
"""THROWAWAY PROTOTYPE: local-only Web server backed by the shared memory engine."""

from __future__ import annotations

import argparse
import json
import mimetypes
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from engine import PrototypeState


ROOT = Path(__file__).resolve().parent
WEB_ROOT = ROOT / "web"
STATE = PrototypeState()


class PrototypeHandler(BaseHTTPRequestHandler):
    server_version = "BossJobAgentThrowawayPrototype/1.0"

    def log_message(self, fmt: str, *args: Any) -> None:
        print(f"[prototype-web] {self.address_string()} {fmt % args}")

    def send_json(self, payload: Any, status: int = 200) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802 - required by BaseHTTPRequestHandler
        path = urlparse(self.path).path
        if path == "/api/state":
            self.send_json(STATE.snapshot())
            return
        if path == "/api/health":
            self.send_json({"ok": True, "prototype": True, "external_actions": False})
            return
        static = {
            "/app.js": WEB_ROOT / "app.js",
            "/styles.css": WEB_ROOT / "styles.css",
        }.get(path)
        if static:
            self.send_file(static)
            return
        if path in {"/", "/prototype/workbench"}:
            self.send_file(WEB_ROOT / "index.html")
            return
        self.send_error(404)

    def send_file(self, path: Path) -> None:
        body = path.read_bytes()
        content_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
        self.send_response(200)
        self.send_header("Content-Type", f"{content_type}; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:  # noqa: N802 - required by BaseHTTPRequestHandler
        if urlparse(self.path).path != "/api/action":
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            payload = json.loads(self.rfile.read(length) or b"{}")
            result = self.dispatch(payload)
            self.send_json({"ok": True, "result": result})
        except (ValueError, KeyError, TypeError, json.JSONDecodeError) as error:
            self.send_json({"ok": False, "error": str(error)}, 400)

    @staticmethod
    def dispatch(payload: dict[str, Any]) -> Any:
        action = payload.get("action")
        if action == "reset":
            return STATE.reset()
        if action == "refresh_online_resume":
            return STATE.refresh_online_resume(payload.get("outcome", "unchanged"))
        if action == "simulate_first_use":
            return STATE.simulate_first_use()
        if action == "continue_discovery":
            return STATE.continue_discovery()
        if action == "start_discovery":
            return STATE.start_discovery()
        if action == "end_discovery_early":
            return STATE.end_discovery_early(payload.get("reason", ""))
        if action == "pause_discovery":
            return STATE.pause_discovery()
        if action == "tick":
            return STATE.tick()
        if action == "queue_assessment":
            return STATE.queue_assessment([int(value) for value in payload.get("job_ids", [])])
        if action == "queue_outreach":
            return STATE.queue_outreach(
                [int(value) for value in payload.get("job_ids", [])], payload["mode"]
            )
        if action == "review_job":
            return STATE.review_job(int(payload["job_id"]), payload["verdict"])
        if action == "generate_policy_suggestion":
            return STATE.generate_policy_suggestion()
        if action == "activate_policy_version":
            return STATE.activate_policy_version(
                int(payload["base_version"]),
                payload.get("full_text", ""),
            )
        if action == "configure_assessment":
            return STATE.configure_assessment(bool(payload["enabled"]), int(payload["limit"]))
        if action == "preview_outreach":
            return STATE.preview_outreach_change(bool(payload["enabled"]), payload["mode"])
        if action == "configure_outreach":
            return STATE.configure_outreach(
                bool(payload["enabled"]),
                payload["mode"],
                payload.get("greeting_text", ""),
                payload.get("time_windows", []),
            )
        raise ValueError(f"未知原型动作：{action}")


def main() -> None:
    parser = argparse.ArgumentParser(description="BOSS Job Agent throwaway Web prototype")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=4177)
    args = parser.parse_args()
    server = ThreadingHTTPServer((args.host, args.port), PrototypeHandler)
    print("THROWAWAY PROTOTYPE — 只使用模拟数据，不连接 BOSS，不执行真实沟通。")
    print("策略建议只返回当前页面会话；后台不保存、不恢复临时候选稿。")
    print(f"打开：http://{args.host}:{args.port}/prototype/workbench?view=jobs")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
