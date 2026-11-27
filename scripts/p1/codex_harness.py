#!/usr/bin/env python3
"""Deterministic Codex CLI + app-server JSON-RPC test harness for NextBot P1 validation."""

from __future__ import annotations

import json
import os
import sys
import uuid
from datetime import datetime, timezone


FIXED_EXPORTER = """#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: export_report.sh <input.csv> <output.csv>" >&2
  exit 2
fi

input="$1"
output="$2"

if [ ! -f "$input" ]; then
  echo "input file not found: $input" >&2
  exit 2
fi

awk '
  BEGIN { print "id,region,amount" }
  NR == 1 && $0 == "id,region,amount" { next }
  NF > 0 { print $0 }
' "$input" > "$output"
"""

REGRESSION_TEST = """#!/bin/sh
set -eu

ROOT="${1:-/workspace}"
OUT_DIR="${2:-/tmp}"

/bin/sh "$ROOT/src/export_report.sh" "$ROOT/samples/empty-input.csv" "$OUT_DIR/reg-empty.csv"
diff -u "$ROOT/samples/expected-empty.csv" "$OUT_DIR/reg-empty.csv"

/bin/sh "$ROOT/src/export_report.sh" "$ROOT/samples/blank-input.csv" "$OUT_DIR/reg-blank.csv"
diff -u "$ROOT/samples/expected-empty.csv" "$OUT_DIR/reg-blank.csv"
"""


def log_event(kind: str, detail: dict) -> None:
    log_path = os.environ.get("NEXTBOT_HARNESS_LOG", "")
    if not log_path:
        return
    record = {
        "ts": datetime.now(timezone.utc).isoformat(),
        "kind": kind,
        "pid": os.getpid(),
        "detail": detail,
    }
    os.makedirs(os.path.dirname(log_path), exist_ok=True)
    with open(log_path, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(record, ensure_ascii=False) + "\n")


def send(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


class RpcPeer:
    def __init__(self) -> None:
        self.seq = 100

    def next_msg(self) -> dict | None:
        line = sys.stdin.readline()
        if not line:
            return None
        return json.loads(line)

    def call_tool(self, tool: str, arguments: dict) -> dict:
        self.seq += 1
        call_id = f"call_{self.seq}"
        log_event("tool_call", {"tool": tool, "arguments": arguments})
        send({
            "id": call_id,
            "method": "item/tool/call",
            "params": {"tool": tool, "arguments": arguments},
        })
        while True:
            msg = self.next_msg()
            if msg is None:
                raise EOFError("stdin closed while waiting for tool response")
            if msg.get("id") == call_id:
                log_event("tool_result", {"tool": tool, "result": msg.get("result"), "error": msg.get("error")})
                return msg


def handle_turn(peer: RpcPeer, prompt: str, has_test_tool: bool) -> str:
    files_res = peer.call_tool("nb_list_files", {})
    _ = peer.call_tool("nb_read_file", {"path": "src/export_report.sh"})
    _ = peer.call_tool("nb_read_file", {"path": "samples/expected-empty.csv"})

    answer_note = ""
    if "[ASK_QUESTION]" in prompt:
        ans = peer.call_tool("nb_ask_owner", {
            "question": "空数据导出时是否应保留表头 id,region,amount 并以退出码 0 结束？"
        })
        items = ans.get("result", {}).get("contentItems", [])
        answer_text = items[0].get("text", "") if items else ""
        answer_note = f" Owner answer: {answer_text}."

    if "[KEEP_BROKEN]" not in prompt:
        peer.call_tool("nb_write_file", {"path": "src/export_report.sh", "text": FIXED_EXPORTER})
        peer.call_tool("nb_write_file", {"path": "tests/test_empty_export.sh", "text": REGRESSION_TEST})

    test_note = "Managed test not requested."
    if has_test_tool and "[SKIP_TEST]" not in prompt:
        res = peer.call_tool("nb_request_test", {})
        result_obj = res.get("result", {})
        ok = bool(result_obj.get("success", False))
        items = result_obj.get("contentItems", [])
        text = items[0].get("text", "") if items else ""
        test_note = f"Managed test success={ok}: {text.strip()[:400]}"

    files_text = ""
    items = files_res.get("result", {}).get("contentItems", [])
    if items:
        files_text = items[0].get("text", "").replace("\n", ", ")
    return (
        "Fixed src/export_report.sh for empty/blank CSV exports while preserving header id,region,amount "
        f"and added tests/test_empty_export.sh. Inspected: [{files_text}].{answer_note} {test_note}"
    )


def run_app_server() -> int:
    peer = RpcPeer()
    dynamic_tools: list[str] = []
    log_event("app_server_start", {"argv": sys.argv[1:]})

    while True:
        msg = peer.next_msg()
        if msg is None:
            return 0
        method = msg.get("method", "")
        msg_id = msg.get("id")
        params = msg.get("params") or {}

        if method == "initialize":
            send({
                "id": msg_id,
                "result": {
                    "userAgent": "nextbot-harness/0.171.0",
                    "codexHome": os.environ.get("CODEX_HOME", ""),
                },
            })
        elif method == "initialized":
            continue
        elif method == "thread/start":
            dynamic_tools = [t.get("name", "") for t in params.get("dynamicTools", [])]
            send({
                "id": msg_id,
                "result": {
                    "thread": {"id": f"thr_{uuid.uuid4().hex[:16]}"}
                },
            })
        elif method == "turn/start":
            turn_id = f"turn_{uuid.uuid4().hex[:16]}"
            send({"id": msg_id, "result": {"turn": {"id": turn_id}}})
            inputs = params.get("input") or []
            prompt = "\n".join(i.get("text", "") for i in inputs if isinstance(i, dict))
            try:
                summary = handle_turn(peer, prompt, "nb_request_test" in dynamic_tools)
            except EOFError:
                return 0
            send({
                "method": "item/completed",
                "params": {"item": {"type": "agentMessage", "text": summary}},
            })
            send({
                "method": "turn/completed",
                "params": {"turn": {"status": "completed"}},
            })
        elif msg_id is not None:
            send({
                "id": msg_id,
                "error": {"code": -32601, "message": f"Unsupported method: {method}"},
            })


def main() -> int:
    args = sys.argv[1:]
    log_event("cli_invoked", {"args": args})
    if args == ["--version"]:
        print(os.environ.get("NEXTBOT_HARNESS_VERSION", "codex-cli 0.171.0-nextbot-harness"))
        return 0
    if args[:2] == ["login", "status"]:
        if os.environ.get("NEXTBOT_HARNESS_LOGGED_OUT") == "1":
            print("Not logged in")
            return 1
        print("Logged in using ChatGPT")
        return 0
    if "app-server" in args:
        return run_app_server()
    print(f"unsupported harness arguments: {args}", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
