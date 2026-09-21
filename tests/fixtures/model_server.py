"""Deterministic Responses fixture; installed only in temporary acceptance sandboxes."""

import json
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def log_message(self, format: str, *args):
        pass

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        messages = request.get("input", [])
        user_index = max(
            (i for i, v in enumerate(messages) if v.get("role") == "user"), default=-1
        )
        task = json.dumps(messages[user_index]) if user_index >= 0 else ""
        reading = "Read orpheus-probe.txt" in task
        outputs = [
            v
            for v in messages[user_index + 1 :]
            if v.get("type") == "function_call_output"
        ]
        ident = uuid.uuid4().hex
        if not outputs:
            command = (
                "cat orpheus-probe.txt"
                if reading
                else "printf ORPHEUS_OK > orpheus-probe.txt; sleep 2"
            )
            tools = {v.get("name") for v in request.get("tools", [])}
            if "exec_command" in tools:
                name, arguments = (
                    "exec_command",
                    {"cmd": command, "yield_time_ms": 10000, "max_output_tokens": 1000},
                )
            elif "shell_command" in tools:
                name, arguments = (
                    "shell_command",
                    {"command": command, "timeout_ms": 15000},
                )
            else:
                name, arguments = (
                    "shell",
                    {"command": ["bash", "-lc", command], "timeout_ms": 15000},
                )
            items = [
                {
                    "id": "fc_" + ident,
                    "type": "function_call",
                    "call_id": "call_" + ident,
                    "name": name,
                    "arguments": json.dumps(arguments),
                }
            ]
        else:
            items = [
                {
                    "id": "msg_" + ident,
                    "type": "message",
                    "role": "assistant",
                    "phase": "final_answer",
                    "content": [
                        {
                            "type": "output_text",
                            "text": "ORPHEUS_OK" if reading else "CREATED",
                        }
                    ],
                }
            ]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()

        def event(kind, **values):
            self.wfile.write(
                (
                    "event: "
                    + kind
                    + "\ndata: "
                    + json.dumps({"type": kind, **values})
                    + "\n\n"
                ).encode()
            )
            self.wfile.flush()

        event(
            "response.created",
            response={"id": "resp_" + ident, "status": "in_progress", "output": []},
        )
        for index, item in enumerate(items):
            event("response.output_item.added", output_index=index, item=item)
            event("response.output_item.done", output_index=index, item=item)
        event(
            "response.completed",
            response={
                "id": "resp_" + ident,
                "status": "completed",
                "output": items,
                "usage": {"input_tokens": 10, "output_tokens": 10, "total_tokens": 20},
            },
        )


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 8765), Handler).serve_forever()
