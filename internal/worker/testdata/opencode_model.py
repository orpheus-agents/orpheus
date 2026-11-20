"""Deterministic OpenAI-compatible fixture for the live OpenCode worker test."""

import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


class Model(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        history = json.dumps(req["messages"])
        call = None
        text = "FIRST" if "SAINT_PETERSBURG" not in history else "PETERSBURG_OK"
        if "SECOND_RUN" in history:
            text = "HISTORY_OK" if "SAINT_PETERSBURG" in history else "LOST_CONTEXT"
        user_messages = [
            str(m.get("content", "")) for m in req["messages"] if m["role"] == "user"
        ]
        latest = user_messages[-1] if user_messages else ""
        summary = "## Objective" in history and "<template>" in history
        if (
            not summary
            and any(tag in latest for tag in ("SHELL_GATE", "SECOND_RUN"))
            and req.get("reasoning_effort") != "high"
        ):
            self.send_error(400, "Expected the selected profile variant")
            return
        if "FAIL_AUTH_CASE" in latest and not summary and "BUDGET_CASE" not in latest:
            self.send_response(401)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"error":{"message":"invalid API key"}}')
            return
        if summary:
            Path("compacted").touch()
            text = "## Objective\n- Remember the Petersburg correction. Continue COMPACT_CASE."
        elif "COMPACT_CASE" in history or Path("compacted").exists():
            text = "COMPACT_OK"
        elif "LATE_CITY" in history or "UNSEEN_AFTER_CRASH" in history:
            text = "LEAKED_STALE_INPUT"
        commands = {
            "SHELL_GATE": "touch gate.started; sleep 20; touch gate.done",
            "LATE_CASE": "touch late.started; while ! test -e late.release; do sleep 0.1; done",
            "CRASH_CASE": "printf 1 >> crash.count; touch crash.started; sleep 40",
            "CANCEL_CASE": "touch cancel.started; sleep 40; touch cancel.finished",
            "BUDGET_CASE": "touch budget.started",
        }
        for marker, command in commands.items():
            called = Path(marker + ".called")
            if (
                marker in history
                and req.get("tools")
                and not called.exists()
                and not summary
            ):
                called.touch()
                call = {
                    "index": 0,
                    "id": "probe_" + marker,
                    "type": "function",
                    "function": {
                        "name": "bash",
                        "arguments": json.dumps(
                            {"command": command, "description": "wait for " + marker}
                        ),
                    },
                }
                break
        if "LATE_CASE" in latest and not call and Path("late.started").exists():
            Path("late.answered").touch()
        msg = {"role": "assistant", "reasoning_content": "PRIVATE_REASONING_FIXTURE"}
        if call:
            msg["tool_calls"] = [call]
        else:
            msg["content"] = text
        usage = {
            "prompt_tokens": 44,
            "completion_tokens": 12,
            "total_tokens": 56,
            "prompt_tokens_details": {"cached_tokens": 7},
            "completion_tokens_details": {"reasoning_tokens": 2},
        }
        if "BUDGET_CASE" in history:
            usage["prompt_tokens"] = 610000
            usage["total_tokens"] = 610012
            if not call:
                time.sleep(10)
        finish = "tool_calls" if call else "stop"
        self.send_response(200)
        self.send_header(
            "Content-Type",
            "text/event-stream" if req.get("stream") else "application/json",
        )
        self.end_headers()
        meta = {"id": "fixture", "model": "fixture", "created": 1}
        if req.get("stream"):
            reasoning = msg.pop("reasoning_content")
            for choices, stats in [
                (
                    [
                        {
                            "index": 0,
                            "delta": {"reasoning_content": reasoning},
                            "finish_reason": None,
                        }
                    ],
                    None,
                ),
                ([{"index": 0, "delta": msg, "finish_reason": None}], None),
                ([{"index": 0, "delta": {}, "finish_reason": finish}], None),
                ([], usage),
            ]:
                event = dict(meta, object="chat.completion.chunk", choices=choices)
                if stats:
                    event["usage"] = stats
                self.wfile.write(("data: " + json.dumps(event) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")
        else:
            self.wfile.write(
                json.dumps(
                    dict(
                        meta,
                        object="chat.completion",
                        usage=usage,
                        choices=[{"index": 0, "message": msg, "finish_reason": finish}],
                    )
                ).encode()
            )


server = ThreadingHTTPServer(("127.0.0.1", 0), Model)
Path("opencode.json").write_text(
    json.dumps(
        {
            "update": "disable",
            "compaction": {
                "auto": True,
                "background": False,
                "buffer": "40%",
                "keep": {"tokens": 100},
            },
            "permissions": [{"action": "*", "resource": "*", "effect": "allow"}],
            "providers": {
                "fixture": {
                    "package": "aisdk:@ai-sdk/openai-compatible",
                    "settings": {
                        "baseURL": "http://127.0.0.1:" + str(server.server_port) + "/v1"
                    },
                    "models": {
                        "fixture": {
                            "name": "Fixture",
                            "variants": [
                                {"id": "thorough", "body": {"reasoning_effort": "high"}}
                            ],
                            "capabilities": {
                                "tools": True,
                                "input": ["text"],
                                "output": ["text"],
                            },
                            "limit": {"context": 20000, "output": 1500},
                        }
                    },
                }
            },
        }
    )
)
server.serve_forever()
