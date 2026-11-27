#!/usr/bin/env python3
"""Repeatable NextBot P1 Linux acceptance runner over real REST/SSE + Control + Runner + rootless containers."""

from __future__ import annotations

import argparse
import hashlib
import http.client
import json
import os
import re
import shutil
import socket
import sqlite3
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import uuid
from datetime import datetime, timezone
from http.cookies import SimpleCookie
from pathlib import Path


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


class ApiResponse:
    def __init__(self, status: int, headers: dict[str, str], raw: bytes) -> None:
        self.status = status
        self.headers = {k.lower(): v for k, v in headers.items()}
        self.raw = raw
        try:
            self.json = json.loads(raw.decode("utf-8")) if raw else None
        except Exception:
            self.json = None

    def error_code(self) -> str:
        if isinstance(self.json, dict):
            err = self.json.get("error")
            if isinstance(err, dict):
                return str(err.get("code", ""))
        return ""


class NextBotClient:
    def __init__(self, base_url: str, origin: str, ca_file: str | None = None) -> None:
        parsed = urllib.parse.urlparse(base_url)
        self.scheme = parsed.scheme
        self.host = parsed.hostname or "127.0.0.1"
        self.port = parsed.port or (443 if self.scheme == "https" else 80)
        self.origin = origin
        self.cookies: dict[str, str] = {}
        self.csrf_token = ""
        self.device_id = ""
        self.ssl_ctx = None
        if self.scheme == "https":
            self.ssl_ctx = ssl.create_default_context(cafile=ca_file) if ca_file else ssl.create_default_context()

    def _connect(self, timeout: float = 15.0) -> http.client.HTTPConnection:
        if self.scheme == "https":
            return http.client.HTTPSConnection(self.host, self.port, timeout=timeout, context=self.ssl_ctx)
        return http.client.HTTPConnection(self.host, self.port, timeout=timeout)

    def _cookie_header(self) -> str:
        return "; ".join(f"{k}={v}" for k, v in self.cookies.items())

    def _store_cookies(self, msg_headers) -> None:
        for val in msg_headers.get_all("Set-Cookie", []):
            c = SimpleCookie()
            c.load(val)
            for k, morsel in c.items():
                self.cookies[k] = morsel.value

    def request(
        self,
        method: str,
        path: str,
        body: dict | None = None,
        idem_key: str | None = None,
        include_origin: bool = True,
        include_csrf: bool = True,
    ) -> ApiResponse:
        conn = self._connect()
        try:
            headers: dict[str, str] = {}
            if self.cookies:
                headers["Cookie"] = self._cookie_header()
            if include_origin and method in ("POST", "PATCH", "PUT", "DELETE"):
                headers["Origin"] = self.origin
            if include_csrf and self.csrf_token and method in ("POST", "PATCH", "PUT", "DELETE"):
                headers["X-CSRF-Token"] = self.csrf_token
            if method in ("POST", "PATCH", "PUT", "DELETE"):
                headers["Idempotency-Key"] = idem_key or uuid.uuid4().hex
            raw_body = None
            if body is not None:
                headers["Content-Type"] = "application/json"
                raw_body = json.dumps(body).encode("utf-8")
            conn.request(method, "/api/v1/" + path.lstrip("/"), body=raw_body, headers=headers)
            resp = conn.getresponse()
            raw = resp.read()
            self._store_cookies(resp.headers)
            hdrs = {k: v for k, v in resp.getheaders()}
            return ApiResponse(resp.status, hdrs, raw)
        finally:
            conn.close()

    def must(self, method: str, path: str, expected_status: int | tuple[int, ...], body: dict | None = None, idem_key: str | None = None) -> ApiResponse:
        resp = self.request(method, path, body=body, idem_key=idem_key)
        allowed = (expected_status,) if isinstance(expected_status, int) else expected_status
        if resp.status not in allowed:
            raise AssertionError(f"{method} {path} returned {resp.status} (expected {allowed}): {resp.raw.decode('utf-8', 'replace')}")
        return resp

    def load_session(self) -> dict:
        resp = self.must("GET", "session", 200)
        self.csrf_token = resp.json["csrfToken"]
        self.device_id = resp.json["deviceId"]
        return resp.json

    def open_sse(self, stream_id: str, after_seq: str, last_event_id: str | None = None, timeout: float = 10.0) -> tuple[http.client.HTTPConnection, http.client.HTTPResponse]:
        conn = self._connect(timeout=timeout)
        headers: dict[str, str] = {}
        if self.cookies:
            headers["Cookie"] = self._cookie_header()
        if last_event_id:
            headers["Last-Event-ID"] = last_event_id
        q = urllib.parse.urlencode({"streamId": stream_id, "afterSeq": after_seq})
        conn.request("GET", f"/api/v1/events?{q}", headers=headers)
        resp = conn.getresponse()
        return conn, resp


def read_sse_until(resp: http.client.HTTPResponse, stop_fn, timeout_s: float = 10.0) -> tuple[list[dict], bool]:
    """Read SSE frames until stop_fn(events) returns True, EOF, or timeout."""
    events: list[dict] = []
    deadline = time.monotonic() + timeout_s
    buf = ""
    closed = False
    while time.monotonic() < deadline:
        try:
            chunk = resp.readline()
        except (TimeoutError, socket.timeout):
            break
        if not chunk:
            closed = True
            break
        line = chunk.decode("utf-8", "replace").replace("\r\n", "\n")
        buf += line
        if buf.endswith("\n\n"):
            frame = buf[:-2]
            buf = ""
            ev_type = ""
            ev_id = ""
            data_str = ""
            for fline in frame.split("\n"):
                if fline.startswith("event: "):
                    ev_type = fline[7:].strip()
                elif fline.startswith("id: "):
                    ev_id = fline[4:].strip()
                elif fline.startswith("data: "):
                    data_str = fline[6:]
            if ev_type == "reset_required":
                events.append({"event": "reset_required", "id": ev_id, "data": {}})
                break
            if data_str:
                parsed = json.loads(data_str)
                events.append({"event": ev_type, "id": ev_id, "data": parsed})
                if stop_fn(events):
                    break
    return events, closed


def wait_until(desc: str, fn, timeout_s: float = 90.0, interval_s: float = 0.4):
    deadline = time.monotonic() + timeout_s
    last_err = None
    while time.monotonic() < deadline:
        try:
            val = fn()
            if val:
                return val
        except Exception as exc:
            last_err = exc
        time.sleep(interval_s)
    raise TimeoutError(f"Timed out waiting for {desc} after {timeout_s}s (last_err={last_err})")


def query_runner_db(runner_dir: Path, attempt_id: str | None = None) -> dict:
    db_path = runner_dir / "runner.db"
    if not db_path.exists():
        return {"jobs": [], "processes": [], "outbox_count": 0, "test_process_starts": 0}
    conn = sqlite3.connect(str(db_path))
    try:
        rows = conn.execute("SELECT id, phase, body FROM jobs ORDER BY rowid").fetchall()
        jobs = []
        cmd_meta = {}
        for cid, phase, body_blob in rows:
            body = json.loads(body_blob.decode("utf-8") if isinstance(body_blob, bytes) else body_blob)
            item = {
                "commandId": cid,
                "phase": phase,
                "type": body.get("type"),
                "taskId": body.get("taskId"),
                "attemptId": body.get("attemptId"),
            }
            if attempt_id is None or item["attemptId"] == attempt_id:
                jobs.append(item)
            cmd_meta[cid] = item

        procs = []
        for cid, pid, boot, birth, observed, stopped in conn.execute(
            "SELECT command, pid, boot, birth, observed, stopped FROM processes ORDER BY rowid"
        ).fetchall():
            meta = cmd_meta.get(cid, {})
            if attempt_id is not None and meta.get("attemptId") != attempt_id:
                continue
            procs.append({
                "commandId": cid,
                "type": meta.get("type"),
                "taskId": meta.get("taskId"),
                "attemptId": meta.get("attemptId"),
                "pid": pid,
                "boot": boot,
                "birth": birth,
                "observed": observed,
                "stopped": stopped,
            })
        outbox_count = conn.execute("SELECT count(*) FROM outbox").fetchone()[0]
        test_starts = sum(1 for p in procs if p["type"] == "run_registered_test")
        return {
            "jobs": jobs,
            "processes": procs,
            "outbox_count": outbox_count,
            "test_process_starts": test_starts,
        }
    finally:
        conn.close()


class NextBotStack:
    def __init__(
        self,
        repo_root: Path,
        state_dir: Path,
        binary: Path,
        engine: str,
        cli_mode: str,
        codex_home: Path,
        event_window: int = 10000,
    ) -> None:
        self.repo_root = repo_root
        self.state_dir = state_dir
        self.binary = binary
        self.engine = engine
        self.cli_mode = cli_mode
        self.codex_home = codex_home
        self.event_window = event_window
        self.port = free_port()
        self.origin = f"http://127.0.0.1:{self.port}"
        self.control_dir = state_dir / "control"
        self.runner_dir = state_dir / "runner"
        self.socket_path = state_dir / "runner.sock"
        self.harness_dir = state_dir / "harness-bin"
        self.harness_log = state_dir / "evidence" / "codex-harness.jsonl"
        self.control_log = state_dir / "evidence" / "control.log"
        self.runner_log = state_dir / "evidence" / "runner.log"
        self.control_proc: subprocess.Popen | None = None
        self.runner_proc: subprocess.Popen | None = None
        self.codex_version = ""

    def setup(self, image_digest: str | None = None) -> dict:
        setup_script = self.repo_root / "scripts" / "p1" / "setup-clean-instance.sh"
        cmd = [
            "bash",
            str(setup_script),
            "--state-dir", str(self.state_dir),
            "--binary", str(self.binary),
            "--engine", self.engine,
            "--origin", self.origin,
            "--skip-build",
        ]
        if image_digest:
            cmd.extend(["--image-digest", image_digest])
        res = subprocess.run(cmd, capture_output=True, text=True)
        if res.returncode:
            raise RuntimeError(f"Acceptance setup failed ({res.returncode}): {res.stderr.strip()}")
        env_map = {}
        for line in (self.state_dir / "instance.env").read_text(encoding="utf-8").splitlines():
            if "=" in line:
                k, v = line.split("=", 1)
                env_map[k] = v
        if self.cli_mode == "harness":
            self.harness_dir.mkdir(parents=True, exist_ok=True)
            wrapper = self.harness_dir / "codex"
            harness_py = self.repo_root / "scripts" / "p1" / "codex_harness.py"
            wrapper.write_text(f"#!/usr/bin/env bash\nexec python3 {json.dumps(str(harness_py))} \"$@\"\n", encoding="utf-8")
            wrapper.chmod(0o755)
            self.codex_version = "codex-cli 0.171.0-nextbot-harness"
        else:
            out = subprocess.check_output(["codex", "--version"], text=True).strip()
            self.codex_version = out
        env_map["SETUP_STDOUT"] = res.stdout.strip()
        env_map["CODEX_VERSION"] = self.codex_version
        return env_map

    def start_control(self) -> None:
        self.control_log.parent.mkdir(parents=True, exist_ok=True)
        fh = open(self.control_log, "ab")
        self.control_proc = subprocess.Popen(
            [
                str(self.binary),
                "control",
                "--data", str(self.control_dir),
                "--socket", str(self.socket_path),
                "--listen", f"127.0.0.1:{self.port}",
                "--origin", self.origin,
                "--event-window", str(self.event_window),
                "--dev-http",
            ],
            stdout=fh,
            stderr=subprocess.STDOUT,
        )
        wait_until("control /healthz", lambda: self._healthz_ok(), timeout_s=15.0, interval_s=0.2)

    def _healthz_ok(self) -> bool:
        try:
            conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=2.0)
            conn.request("GET", "/healthz")
            r = conn.getresponse()
            r.read()
            conn.close()
            return r.status == 200
        except Exception:
            return False

    def stop_control(self) -> None:
        if self.control_proc and self.control_proc.poll() is None:
            self.control_proc.terminate()
            try:
                self.control_proc.wait(timeout=8)
            except subprocess.TimeoutExpired:
                self.control_proc.kill()
                self.control_proc.wait()
        self.control_proc = None

    def start_runner(self) -> None:
        self.runner_log.parent.mkdir(parents=True, exist_ok=True)
        fh = open(self.runner_log, "ab")
        env = os.environ.copy()
        if self.cli_mode == "harness":
            env["PATH"] = f"{self.harness_dir}:{env.get('PATH', '')}"
            env["NEXTBOT_HARNESS_VERSION"] = self.codex_version
            env["NEXTBOT_HARNESS_LOG"] = str(self.harness_log)
            codex_home = str(self.state_dir / "codex-home")
        else:
            codex_home = str(self.codex_home)
        self.runner_proc = subprocess.Popen(
            [
                str(self.binary),
                "runner",
                "--data", str(self.runner_dir),
                "--socket", str(self.socket_path),
                "--codex-home", codex_home,
                "--verified-codex-version", self.codex_version,
                "--container-engine", self.engine,
                "--enable-managed-tests",
            ],
            env=env,
            stdout=fh,
            stderr=subprocess.STDOUT,
        )

    def stop_runner(self) -> None:
        if self.runner_proc and self.runner_proc.poll() is None:
            self.runner_proc.terminate()
            try:
                self.runner_proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.runner_proc.kill()
                self.runner_proc.wait()
        self.runner_proc = None

    def stop_all(self) -> None:
        self.stop_runner()
        self.stop_control()

    def claim_ticket(self) -> str:
        out = subprocess.check_output(
            [str(self.binary), "claim", "--data", str(self.control_dir), "--origin", self.origin],
            text=True,
        ).strip()
        _, _, ticket = out.partition("#claim=")
        if len(ticket) != 64:
            raise RuntimeError(f"unexpected claim output: {out}")
        return ticket


def create_and_submit_task(
    client: NextBotClient,
    project_id: str,
    title: str,
    goal: str,
    scope: str,
    criteria: str,
    test_id: str = "export-check",
    require_test: bool = True,
) -> dict:
    res = client.must("POST", f"projects/{project_id}/base-resolutions", 202, {"refName": "HEAD"}).json
    resolved = wait_until(
        "base resolution",
        lambda: (
            r := client.must("GET", f"projects/{project_id}/base-resolutions/{res['resolutionId']}", 200).json
        )
        and (r if r["status"] == "resolved" else None),
        timeout_s=20.0,
    )
    fields = {
        "projectId": project_id,
        "title": title,
        "goal": goal,
        "scope": scope,
        "acceptanceCriteria": criteria,
        "refName": "HEAD",
        "executionAgent": "codex",
        "reviewAgent": "",
        "testId": test_id,
        "requireTest": require_test,
    }
    draft = client.must("POST", "tasks", 201, fields).json
    submit_body = {
        **fields,
        "expectedRevision": draft["revision"],
        "resolutionId": resolved["resolutionId"],
        "baseCommit": resolved["commitId"],
    }
    return client.must("POST", f"tasks/{draft['taskId']}/submit", 200, submit_body).json


def wait_node_ready(client: NextBotClient) -> dict:
    def check():
        nodes = client.must("GET", "nodes", 200).json["items"]
        if not nodes:
            return None
        n = nodes[0]
        c = n.get("adapterCapabilities", {}).get("codex", {})
        if n.get("availability") == "online" and c.get("execution") and c.get("managedTests"):
            return n
        return None

    return wait_until("online node with codex execution & managedTests", check, timeout_s=30.0)


def run_suite(args: argparse.Namespace) -> dict:
    repo_root = Path(__file__).resolve().parents[2]
    binary = Path(args.binary).resolve()
    state_dir = Path(args.state_dir).resolve()
    codex_home = Path(args.codex_home).resolve() if args.codex_home else (state_dir / "codex-home")
    cli_label = f"real-codex" if args.cli_mode == "real-codex" else "harness-codex"

    report: dict = {
        "startedAt": utc_now(),
        "cliMode": args.cli_mode,
        "engine": args.engine,
        "stateDir": str(state_dir),
        "environment": {
            "uname": " ".join(os.uname()),
            "python": sys.version.split()[0],
        },
        "scenarios": [],
    }

    stack = NextBotStack(repo_root, state_dir, binary, args.engine, args.cli_mode, codex_home, event_window=10000)
    try:
        setup_meta = stack.setup(image_digest=args.image_digest)
        report["environment"]["imageDigest"] = setup_meta["IMAGE_DIGEST"]
        report["environment"]["codexVersion"] = setup_meta["CODEX_VERSION"]
        report["environment"]["baseCommit"] = setup_meta["BASE_COMMIT"]
        report["environment"]["projectId"] = setup_meta["PROJECT_ID"]
        project_id = setup_meta["PROJECT_ID"]
        base_commit = setup_meta["BASE_COMMIT"]
        demo_repo = Path(setup_meta["DEMO_REPO"])

        stack.start_control()
        stack.start_runner()

        desktop = NextBotClient(stack.origin, stack.origin)
        phone = NextBotClient(stack.origin, stack.origin)

        # Verify unauthenticated read is rejected
        unauth = desktop.request("GET", "tasks")
        assert unauth.status == 401, f"expected 401 before claim, got {unauth.status}"

        # Claim owner on desktop; verify ticket reuse rejected
        ticket = stack.claim_ticket()
        desktop.must("POST", "owner/claim", 201, {"ticket": ticket, "deviceName": "Desktop-Linux"})
        reuse_claim = phone.request("POST", "owner/claim", {"ticket": ticket, "deviceName": "Foreign"})
        assert reuse_claim.status == 403, f"expected 403 on ticket reuse, got {reuse_claim.status}"
        desktop.load_session()

        # Pair phone; verify token reuse rejected
        pair = desktop.must("POST", "pairings", 201, {}).json
        phone.must("POST", "pairings/claim", 201, {"token": pair["token"], "deviceName": "Mobile-Validation"})
        third = NextBotClient(stack.origin, stack.origin)
        reuse_pair = third.request("POST", "pairings/claim", {"token": pair["token"], "deviceName": "Intruder"})
        assert reuse_pair.status == 403, f"expected 403 on pairing token reuse, got {reuse_pair.status}"
        phone.load_session()

        node = wait_node_ready(desktop)
        report["environment"]["nodeCapabilities"] = node["adapterCapabilities"]

        # ------------------------------------------------------------------
        # Scenario 1: Happy path (A01 + A05 + A09 + A11 allow_once)
        # ------------------------------------------------------------------
        snap_before = desktop.must("GET", "snapshot", 200).json
        sse_conn_d, sse_resp_d = desktop.open_sse(snap_before["streamId"], snap_before["highWater"])
        assert sse_resp_d.status == 200

        task1 = create_and_submit_task(
            desktop,
            project_id,
            title="修复空数据导出失败并补回归测试",
            goal=(
                "修复 src/export_report.sh 在空数据（samples/empty-input.csv 与 samples/blank-input.csv）时导出失败并退出码为 1 的问题，"
                "保持表头 id,region,amount 与普通输入导出格式不变，补充回归测试并运行受管测试 export-check。"
            ),
            scope="仅修改 src/export_report.sh 和 tests/ 目录下的测试脚本，不修改 samples/ 对照样本。",
            criteria=(
                "1) samples/normal-input.csv 输出与 samples/expected-normal.csv 一致；"
                "2) samples/empty-input.csv 与 samples/blank-input.csv 均退出码 0 且输出与 samples/expected-empty.csv 一致；"
                "3) 受管测试 export-check 返回退出码 0。"
            ),
        )
        # Simulate desktop closing its page right after dispatch!
        sse_conn_d.close()

        # Phone waits for approval_requested
        req1 = wait_until(
            "approval_requested for task1",
            lambda: (
                d := phone.must("GET", f"tasks/{task1['taskId']}", 200).json
            )
            and next((r for r in d.get("requests", []) if r["kind"] == "action_approval" and r["status"] == "pending"), None),
            timeout_s=120.0,
        )
        assert req1["frozenInput"]["testId"] == "export-check"
        assert req1["frozenInput"]["inputDigest"].startswith("sha256:")

        # Phone approves once
        phone.must(
            "POST",
            f"requests/{req1['requestId']}/decide",
            200,
            {
                "taskId": task1["taskId"],
                "attemptId": req1["attemptId"],
                "expectedRevision": phone.must("GET", f"tasks/{task1['taskId']}", 200).json["revision"],
                "inputDigest": req1["frozenInput"]["inputDigest"],
                "outcome": "allow_once",
            },
        )

        # Wait for awaiting_acceptance on Phone
        t1_ready = wait_until(
            "task1 awaiting_acceptance",
            lambda: (
                d := phone.must("GET", f"tasks/{task1['taskId']}", 200).json
            )
            and (d if d["state"] == "awaiting_acceptance" else None),
            timeout_s=180.0,
        )
        art1_id = t1_ready["currentArtifactVersionId"]
        art1 = phone.must("GET", f"artifacts/{art1_id}", 200).json
        assert art1["hasRequiredEvidence"] is True, f"expected hasRequiredEvidence=true, got {art1}"
        assert art1["integrityStatus"] == "available"

        entry_ids = {e["entryId"] for e in art1["manifest"]}
        assert f"test-{req1['requestId']}.json" in entry_ids
        assert f"test-{req1['requestId']}.txt" in entry_ids
        run_json = json.loads(phone.must("GET", f"artifacts/{art1_id}/entries/test-{req1['requestId']}.json", 200).raw.decode("utf-8"))
        run_log = phone.must("GET", f"artifacts/{art1_id}/entries/test-{req1['requestId']}.txt", 200).raw.decode("utf-8")
        patch_text = phone.must("GET", f"artifacts/{art1_id}/entries/change.patch", 200).raw.decode("utf-8")
        assert run_json["exitCode"] == 0
        assert "[export-check] observed_run=true" in run_log
        assert "[export-check] all_checks=pass" in run_log
        assert "src/export_report.sh" in patch_text

        # Phone accepts current version
        phone.must(
            "POST",
            f"tasks/{task1['taskId']}/acceptance",
            200,
            {
                "artifactVersionId": art1_id,
                "expectedRevision": t1_ready["revision"],
                "outcome": "accept",
            },
        )
        t1_done_desktop = desktop.must("GET", f"tasks/{task1['taskId']}", 200).json
        assert t1_done_desktop["state"] == "completed"
        art1_done = desktop.must("GET", f"artifacts/{art1_id}", 200).json
        assert art1_done["acceptance"]["deviceId"] == phone.device_id

        # Verify node records + source repo untouched + restart + backup
        db_t1 = query_runner_db(stack.runner_dir, t1_ready["currentAttemptId"])
        assert db_t1["test_process_starts"] == 1, f"expected 1 container start, got {db_t1}"
        assert subprocess.check_output(["git", "-C", str(demo_repo), "status", "--porcelain"], text=True).strip() == ""
        assert subprocess.check_output(["git", "-C", str(demo_repo), "rev-parse", "HEAD"], text=True).strip() == base_commit

        # Restart Control and confirm historical readback + backup verification
        stack.stop_control()
        stack.start_control()
        t1_after_restart = desktop.must("GET", f"tasks/{task1['taskId']}", 200).json
        assert t1_after_restart["state"] == "completed"
        assert desktop.must("GET", f"artifacts/{art1_id}/entries/test-{req1['requestId']}.json", 200).status == 200
        backup_dir = state_dir / "backup-1"
        subprocess.run([str(binary), "backup", "--data", str(stack.control_dir), "--out", str(backup_dir)], check=True, stdout=subprocess.DEVNULL)
        verify_out = subprocess.check_output([str(binary), "verify-backup", "--from", str(backup_dir)], text=True)
        verify_json = json.loads(verify_out)
        assert len(verify_json["objects"]) == len(art1["manifest"])

        report["scenarios"].append({
            "id": "S1_happy_delivery_and_restart_backup",
            "cases": ["A01", "A05", "A09", "A11(allow_once)"],
            "status": "PASS",
            "cliUsed": cli_label,
            "clientUsed": "automated-rest-sse (desktop+phone sessions)",
            "taskId": task1["taskId"],
            "attemptId": t1_ready["currentAttemptId"],
            "artifactVersionId": art1_id,
            "versionNumber": art1["versionNumber"],
            "requestId": req1["requestId"],
            "actualTestStarts": db_t1["test_process_starts"],
            "acceptanceDeviceId": art1_done["acceptance"]["deviceId"],
            "backupVerifiedObjects": len(verify_json["objects"]),
        })

        # For deterministic fault/negative cases below, switch to harness if real-codex was used for S1
        if args.cli_mode != "harness":
            stack.stop_runner()
            stack.cli_mode = "harness"
            stack.harness_dir.mkdir(parents=True, exist_ok=True)
            wrapper = stack.harness_dir / "codex"
            harness_py = repo_root / "scripts" / "p1" / "codex_harness.py"
            wrapper.write_text(f"#!/usr/bin/env bash\nexec python3 {json.dumps(str(harness_py))} \"$@\"\n", encoding="utf-8")
            wrapper.chmod(0o755)
            stack.codex_version = "codex-cli 0.171.0-nextbot-harness"
            stack.start_runner()
            wait_node_ready(desktop)

        # ------------------------------------------------------------------
        # Scenario 2: Approval Deny (A11 deny) -> 0 container launches, 422 accept blocked
        # ------------------------------------------------------------------
        task2 = create_and_submit_task(
            desktop,
            project_id,
            title="修复空数据导出失败并补回归测试 [DENY]",
            goal="测试审批拒绝场景：修复空数据导出并申请测试，所有者将拒绝测试授权。",
            scope="src/export_report.sh",
            criteria="拒绝后不得运行受管测试，且缺少证据时阻止验收。",
        )
        req2 = wait_until(
            "approval_requested for task2",
            lambda: (
                d := phone.must("GET", f"tasks/{task2['taskId']}", 200).json
            )
            and next((r for r in d.get("requests", []) if r["kind"] == "action_approval" and r["status"] == "pending"), None),
            timeout_s=30.0,
        )
        rev_s2 = phone.must("GET", f"tasks/{task2['taskId']}", 200).json["revision"]
        tampered = phone.request(
            "POST",
            f"requests/{req2['requestId']}/decide",
            {
                "taskId": task2["taskId"],
                "attemptId": req2["attemptId"],
                "expectedRevision": rev_s2,
                "inputDigest": "sha256:" + ("0" * 64),
                "outcome": "allow_once",
            },
        )
        assert tampered.status == 409 and tampered.error_code() == "stale_target", (
            f"expected 409 stale_target on mismatched inputDigest, got {tampered.status} {tampered.raw}"
        )
        phone.must(
            "POST",
            f"requests/{req2['requestId']}/decide",
            200,
            {
                "taskId": task2["taskId"],
                "attemptId": req2["attemptId"],
                "expectedRevision": rev_s2,
                "inputDigest": req2["frozenInput"]["inputDigest"],
                "outcome": "deny",
            },
        )
        t2_ready = wait_until(
            "task2 awaiting_acceptance after deny",
            lambda: (
                d := phone.must("GET", f"tasks/{task2['taskId']}", 200).json
            )
            and (d if d["state"] == "awaiting_acceptance" else None),
            timeout_s=30.0,
        )
        db_t2 = query_runner_db(stack.runner_dir, t2_ready["currentAttemptId"])
        assert db_t2["test_process_starts"] == 0, f"expected 0 container starts on deny, got {db_t2}"
        art2 = phone.must("GET", f"artifacts/{t2_ready['currentArtifactVersionId']}", 200).json
        assert art2["hasRequiredEvidence"] is False
        blocked_accept = phone.request(
            "POST",
            f"tasks/{task2['taskId']}/acceptance",
            {
                "artifactVersionId": t2_ready["currentArtifactVersionId"],
                "expectedRevision": t2_ready["revision"],
                "outcome": "accept",
            },
        )
        assert blocked_accept.status == 422 and blocked_accept.error_code() == "evidence_missing", (
            f"expected 422 evidence_missing, got {blocked_accept.status} {blocked_accept.raw}"
        )
        report["scenarios"].append({
            "id": "S2_approval_deny",
            "cases": ["A11(deny)"],
            "status": "PASS",
            "cliUsed": "harness-codex",
            "clientUsed": "automated-rest-sse",
            "taskId": task2["taskId"],
            "attemptId": t2_ready["currentAttemptId"],
            "artifactVersionId": t2_ready["currentArtifactVersionId"],
            "requestId": req2["requestId"],
            "actualTestStarts": db_t2["test_process_starts"],
            "acceptWithoutEvidenceStatus": blocked_accept.status,
            "acceptWithoutEvidenceCode": blocked_accept.error_code(),
        })

        # ------------------------------------------------------------------
        # Scenario 3: Approval Expiry (A11 expired)
        # ------------------------------------------------------------------
        if args.wait_real_expiry:
            task3 = create_and_submit_task(
                desktop,
                project_id,
                title="修复空数据导出失败并补回归测试 [EXPIRY]",
                goal="测试审批超时过期场景：不操作等待 5 分钟到期。",
                scope="src/export_report.sh",
                criteria="过期后自动解除等待且实际测试启动次数为 0。",
            )
            req3 = wait_until(
                "approval_requested for task3",
                lambda: (
                    d := phone.must("GET", f"tasks/{task3['taskId']}", 200).json
                )
                and next((r for r in d.get("requests", []) if r["kind"] == "action_approval" and r["status"] == "pending"), None),
                timeout_s=30.0,
            )
            exp_dt = datetime.fromisoformat(req3["expiresAt"].replace("Z", "+00:00"))
            wait_sec = max(5.0, (exp_dt - datetime.now(timezone.utc)).total_seconds() + 4.0)
            print(f"[expiry] waiting {wait_sec:.1f}s for request {req3['requestId']} to expire at {req3['expiresAt']}", flush=True)
            time.sleep(wait_sec)

            req3_after = wait_until(
                "request3 expired",
                lambda: (
                    r := phone.must("GET", f"requests/{req3['requestId']}", 200).json
                )
                and (r if r["status"] == "expired" else None),
                timeout_s=20.0,
            )
            assert req3_after["decision"]["outcome"] == "expired"
            late = phone.request(
                "POST",
                f"requests/{req3['requestId']}/decide",
                {
                    "taskId": task3["taskId"],
                    "attemptId": req3["attemptId"],
                    "expectedRevision": phone.must("GET", f"tasks/{task3['taskId']}", 200).json["revision"],
                    "inputDigest": req3["frozenInput"]["inputDigest"],
                    "outcome": "allow_once",
                },
            )
            assert late.status == 409 and late.error_code() == "request_expired", f"expected 409 request_expired, got {late.status} {late.raw}"
            t3_ready = wait_until(
                "task3 settles after expiry",
                lambda: (
                    d := phone.must("GET", f"tasks/{task3['taskId']}", 200).json
                )
                and (d if d["state"] == "awaiting_acceptance" else None),
                timeout_s=30.0,
            )
            db_t3 = query_runner_db(stack.runner_dir, t3_ready["currentAttemptId"])
            assert db_t3["test_process_starts"] == 0, f"expected 0 container starts on expiry, got {db_t3}"
            report["scenarios"].append({
                "id": "S3_approval_expiry",
                "cases": ["A11(expired)"],
                "status": "PASS",
                "cliUsed": "harness-codex",
                "clientUsed": "automated-rest-sse",
                "taskId": task3["taskId"],
                "attemptId": t3_ready["currentAttemptId"],
                "requestId": req3["requestId"],
                "actualTestStarts": db_t3["test_process_starts"],
                "lateDecideStatus": late.status,
                "lateDecideCode": late.error_code(),
            })
        else:
            report["scenarios"].append({
                "id": "S3_approval_expiry",
                "cases": ["A11(expired)"],
                "status": "SKIPPED",
                "reason": "Run with --wait-real-expiry (~305s) to exercise unmodified 5m TTL expiration",
            })

        # ------------------------------------------------------------------
        # Scenario 4: Queue serialization, cross-device idempotency, & Phone SSE Reconnect (A06 + A12a)
        # ------------------------------------------------------------------
        task4a = create_and_submit_task(
            desktop,
            project_id,
            title="修复空数据导出失败并补回归测试 [QUEUE_HOLD]",
            goal="[ASK_QUESTION] [SKIP_TEST] 等待提问答复以验证队列与断线补读。",
            scope="src/export_report.sh",
            criteria="确认队列阻塞、跨端幂等和断线补读。",
            test_id="",
            require_test=False,
        )
        q4 = wait_until(
            "question_requested for task4a",
            lambda: (
                d := desktop.must("GET", f"tasks/{task4a['taskId']}", 200).json
            )
            and next((r for r in d.get("requests", []) if r["kind"] == "user_question" and r["status"] == "pending"), None),
            timeout_s=30.0,
        )

        # Phone syncs snapshot & records highWater BEFORE going offline
        phone_snap = phone.must("GET", "snapshot", 200).json
        offline_stream = phone_snap["streamId"]
        offline_seq = phone_snap["highWater"]

        # While Phone is offline: Desktop submits Task 4b, Phone replays same submit key (cross-device idempotency), then Desktop answers q4 & cancels Task 4b
        res4b = desktop.must("POST", f"projects/{project_id}/base-resolutions", 202, {"refName": "HEAD"}).json
        resolved4b = wait_until(
            "base resolution 4b",
            lambda: (
                r := desktop.must("GET", f"projects/{project_id}/base-resolutions/{res4b['resolutionId']}", 200).json
            )
            and (r if r["status"] == "resolved" else None),
            timeout_s=20.0,
        )
        draft4b_fields = {
            "projectId": project_id,
            "title": "排队与取消验证任务",
            "goal": "验证单节点写名额排队和取消。",
            "scope": "src/export_report.sh",
            "acceptanceCriteria": "排队期间不启动容器。",
            "refName": "HEAD",
            "executionAgent": "codex",
            "reviewAgent": "",
            "testId": "",
            "requireTest": False,
        }
        draft4b = desktop.must("POST", "tasks", 201, draft4b_fields).json
        submit_4b_key = uuid.uuid4().hex
        submit_4b_body = {
            **draft4b_fields,
            "expectedRevision": draft4b["revision"],
            "resolutionId": resolved4b["resolutionId"],
            "baseCommit": resolved4b["commitId"],
        }
        s1 = desktop.must("POST", f"tasks/{draft4b['taskId']}/submit", 200, submit_4b_body, idem_key=submit_4b_key)
        s2 = phone.must("POST", f"tasks/{draft4b['taskId']}/submit", 200, submit_4b_body, idem_key=submit_4b_key)
        assert s1.raw == s2.raw, "cross-device idempotent submit changed body"
        assert s2.headers.get("idempotency-replayed") == "true"

        time.sleep(1.5)
        t4b_queued = desktop.must("GET", f"tasks/{draft4b['taskId']}", 200).json
        assert t4b_queued["state"] == "queued" and t4b_queued["currentAttemptId"] is None
        desktop.must("POST", f"tasks/{draft4b['taskId']}/cancel", 200, {"expectedRevision": t4b_queued["revision"], "reason": "queue test"})

        # Desktop answers q4 while Phone is offline
        desktop.must(
            "POST",
            f"requests/{q4['requestId']}/decide",
            200,
            {
                "taskId": task4a["taskId"],
                "attemptId": q4["attemptId"],
                "expectedRevision": desktop.must("GET", f"tasks/{task4a['taskId']}", 200).json["revision"],
                "inputDigest": q4["frozenInput"]["inputDigest"],
                "outcome": "answer",
                "answerText": "保留 id,region,amount 列序并返回退出码 0",
            },
        )
        t4a_done = wait_until(
            "task4a awaiting_acceptance",
            lambda: (
                d := desktop.must("GET", f"tasks/{task4a['taskId']}", 200).json
            )
            and (d if d["state"] == "awaiting_acceptance" else None),
            timeout_s=30.0,
        )
        t4b_cancelled = desktop.must("GET", f"tasks/{draft4b['taskId']}", 200).json
        assert t4b_cancelled["state"] == "cancelled"
        assert len(desktop.must("GET", f"tasks/{draft4b['taskId']}/attempts", 200).json["items"]) == 0

        # Phone reconnects SSE from offline_seq using Last-Event-ID and verifies contiguous seq replay
        rc_conn, rc_resp = phone.open_sse(offline_stream, offline_seq, last_event_id=f"{offline_stream}:{offline_seq}")
        assert rc_resp.status == 200
        replayed_events, _ = read_sse_until(
            rc_resp,
            lambda evs: any(
                e["data"].get("type") == "task_changed"
                and e["data"].get("taskId") == task4a["taskId"]
                and (e["data"].get("data") or {}).get("state") == "awaiting_acceptance"
                for e in evs
            ),
            timeout_s=10.0,
        )
        rc_conn.close()
        seqs = [int(e["data"]["seq"]) for e in replayed_events]
        assert seqs[0] == int(offline_seq) + 1, f"expected replay start at {int(offline_seq)+1}, got {seqs[:5]}"
        assert seqs == list(range(seqs[0], seqs[-1] + 1)), f"non-contiguous SSE seqs: {seqs}"
        assert any(e["data"].get("type") == "request_decided" and e["data"]["data"]["requestId"] == q4["requestId"] for e in replayed_events)
        assert len(phone.must("GET", f"tasks/{task4a['taskId']}/attempts", 200).json["items"]) == 1

        report["scenarios"].append({
            "id": "S4a_queue_idempotency_and_sse_replay",
            "cases": ["A06", "A12(within_window)", "A28(single_answer)"],
            "status": "PASS",
            "cliUsed": "harness-codex",
            "clientUsed": "automated-rest-sse",
            "taskId": task4a["taskId"],
            "cancelledTaskId": draft4b["taskId"],
            "offlineAfterSeq": offline_seq,
            "replayedSeqRange": [seqs[0], seqs[-1]],
            "replayedCount": len(seqs),
        })

        # ------------------------------------------------------------------
        # Scenario 5: Device Revocation while Phone SSE is open (A02 revoke)
        # ------------------------------------------------------------------
        snap_pre_revoke = phone.must("GET", "snapshot", 200).json
        rev_conn, rev_resp = phone.open_sse(snap_pre_revoke["streamId"], snap_pre_revoke["highWater"], timeout=8.0)
        assert rev_resp.status == 200

        # Ensure SSE loop entered select() on server
        time.sleep(0.3)
        desktop.must("POST", f"devices/{phone.device_id}/revoke", 200, {})
        post_revoke_task = desktop.must("POST", "tasks", 201, {"projectId": project_id, "title": "撤权后新建任务"}).json

        rev_events, sse_closed = read_sse_until(rev_resp, lambda evs: False, timeout_s=4.0)
        rev_conn.close()
        assert sse_closed, "expected server to close revoked device SSE stream"
        assert not any(e["data"].get("taskId") == post_revoke_task["taskId"] for e in rev_events), (
            f"revoked phone SSE leaked post-revoke event: {rev_events}"
        )
        assert phone.request("GET", "session").status == 401
        assert phone.request("GET", f"tasks/{task1['taskId']}").status == 401
        assert phone.request("POST", "tasks", {"title": "should fail"}).status == 401
        c_sse, r_sse = phone.open_sse(snap_pre_revoke["streamId"], snap_pre_revoke["highWater"])
        c_sse.close()
        assert r_sse.status == 401

        devices_list = desktop.must("GET", "devices", 200).json["items"]
        revoked_phone = next(d for d in devices_list if d["deviceId"] == phone.device_id)
        assert revoked_phone.get("revokedAt") is not None

        report["scenarios"].append({
            "id": "S5_device_revocation",
            "cases": ["A02(revoke)"],
            "status": "PASS",
            "clientUsed": "automated-rest-sse",
            "revokedDeviceId": phone.device_id,
            "revokedAt": revoked_phone["revokedAt"],
            "sseClosedOnRevoke": sse_closed,
            "postRevokeStatuses": {"session": 401, "tasks": 401, "mutate": 401, "sse": r_sse.status},
        })

    finally:
        stack.stop_all()

    # ----------------------------------------------------------------------
    # Scenario 4b: Short event-window -> 410 cursor_expired -> snapshot reload
    # ----------------------------------------------------------------------
    short_dir = state_dir.parent / (state_dir.name + "-short-window")
    short_stack = NextBotStack(repo_root, short_dir, binary, args.engine, "harness", codex_home, event_window=5)
    try:
        short_meta = short_stack.setup(image_digest=report["environment"]["imageDigest"])
        short_stack.start_control()
        d2 = NextBotClient(short_stack.origin, short_stack.origin)
        p2 = NextBotClient(short_stack.origin, short_stack.origin)
        t_short = short_stack.claim_ticket()
        d2.must("POST", "owner/claim", 201, {"ticket": t_short, "deviceName": "Desktop-Short"})
        d2.load_session()
        pair2 = d2.must("POST", "pairings", 201, {}).json
        p2.must("POST", "pairings/claim", 201, {"token": pair2["token"], "deviceName": "Phone-Short"})
        p2.load_session()

        snap_old = p2.must("GET", "snapshot", 200).json
        old_hw = snap_old["highWater"]
        for idx in range(7):
            d2.must("POST", "tasks", 201, {"projectId": short_meta["PROJECT_ID"], "title": f"窗口推进草稿-{idx}"})
        time.sleep(1.5)  # allow Maintenance to advance floor = seq - 5

        c410, r410 = p2.open_sse(snap_old["streamId"], old_hw)
        c410.close()
        assert r410.status == 410, f"expected 410 cursor_expired, got {r410.status}"

        snap_new = p2.must("GET", "snapshot", 200).json
        assert int(snap_new["highWater"]) > int(old_hw)
        assert len(snap_new["tasks"]) == 7

        c_ok, r_ok = p2.open_sse(snap_new["streamId"], snap_new["highWater"])
        assert r_ok.status == 200
        live_task = d2.must("POST", "tasks", 201, {"projectId": short_meta["PROJECT_ID"], "title": "快照重同步后实时事件"}).json
        evs, _ = read_sse_until(
            r_ok,
            lambda items: any(e["data"].get("taskId") == live_task["taskId"] for e in items),
            timeout_s=5.0,
        )
        c_ok.close()
        assert any(e["data"].get("taskId") == live_task["taskId"] for e in evs)

        report["scenarios"].append({
            "id": "S4b_cursor_expired_snapshot_reload",
            "cases": ["A12(cursor_expired)"],
            "status": "PASS",
            "clientUsed": "automated-rest-sse",
            "eventWindow": 5,
            "expiredAfterSeq": old_hw,
            "reconnectStatus": r410.status,
            "reloadedHighWater": snap_new["highWater"],
            "resumedLiveEventTaskId": live_task["taskId"],
        })
    finally:
        short_stack.stop_all()

    report["finishedAt"] = utc_now()
    evidence_dir = state_dir / "evidence"
    evidence_dir.mkdir(parents=True, exist_ok=True)
    report_json = evidence_dir / "p1-acceptance-report.json"
    report_json.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description="Run NextBot P1 Linux acceptance suite.")
    parser.add_argument("--state-dir", required=True, help="Clean instance directory")
    parser.add_argument("--binary", default="bin/nextbot", help="Path to built Linux nextbot binary")
    parser.add_argument("--engine", default="podman", choices=["podman", "docker"], help="Rootless container engine")
    parser.add_argument("--cli-mode", default="harness", choices=["harness", "real-codex"], help="Codex mode for Scenario 1")
    parser.add_argument("--codex-home", default="", help="Dedicated CODEX_HOME when --cli-mode real-codex")
    parser.add_argument("--image-digest", default="", help="Pre-resolved digest-pinned container image")
    parser.add_argument("--wait-real-expiry", action="store_true", help="Wait full 5 minutes to test unmodified request TTL expiry")
    args = parser.parse_args()
    run_suite(args)
    return 0


if __name__ == "__main__":
    sys.exit(main())
