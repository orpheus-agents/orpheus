"""Standalone stdlib projection sent as a one-shot command to the sandbox.

Native events define public items. Model-facing results enrich command output
only with an exact ID or explicit process binding; completion events alone can
contain just the last stdout chunk. Keep this script compatible with Python 3.10.
"""

import datetime
import json
import re
import sys

TOOL_TYPES = {
    "CommandExecution",
    "FileChange",
    "McpToolCall",
    "DynamicToolCall",
    "CollabAgentToolCall",
    "WebSearch",
    "ImageView",
    "ImageGeneration",
}
ITEM_TYPES = TOOL_TYPES | {
    "UserMessage",
    "AgentMessage",
    "Reasoning",
    "Plan",
    "ContextCompaction",
    "EnteredReviewMode",
    "ExitedReviewMode",
}
VISIBLE_TYPES = TOOL_TYPES | {"UserMessage", "AgentMessage"}


def bounded(value, limit):
    raw = (
        value
        if isinstance(value, str)
        else json.dumps(value, ensure_ascii=False, separators=(",", ":"))
    ).encode()
    incomplete = isinstance(value, str) and (
        "tokens truncated" in value or "Warning: truncated output" in value
    )
    size = None if incomplete else len(raw)
    if len(raw) > limit:
        return (
            {
                "type": "truncated_text",
                "head": raw[: limit // 2].decode("utf-8", "ignore"),
                "tail": raw[-(limit - limit // 2) :].decode("utf-8", "ignore"),
                "original_bytes": size,
                "source_type": "text" if isinstance(value, str) else "json",
            },
            "truncated",
            "orpheus_limit",
        )
    result = (
        {"type": "text", "text": value, "original_bytes": size}
        if isinstance(value, str)
        else {"type": "json", "value": value, "original_bytes": size}
    )
    return (
        result,
        "truncated" if incomplete else "complete",
        "harness_limit" if incomplete else None,
    )


def output(item, limit):
    if item["type"] not in TOOL_TYPES:
        return None
    value = item.get("aggregated_output")
    if value is None:
        value = item.get("result")
    if value is None and item["type"] == "FileChange" and "stdout" in item:
        value = {"stdout": item["stdout"], "stderr": item.get("stderr", "")}
    if value is None:
        return None
    result, completeness, reason = bounded(value, limit)
    if item["type"] == "CommandExecution":
        # Native completion records may contain only the final stdout chunk.
        # Only correlated model-facing results prove the complete output.
        if reason != "orpheus_limit":
            completeness, reason = "truncated", "harness_limit"
        if result["type"] != "json":
            result["original_bytes"] = None
    return {
        "type": "output",
        "id": item["id"],
        "result": result,
        "completeness": completeness,
        "reason": reason,
    }


def records_before(path, stop):
    with open(path, "rb") as stream:
        while stream.tell() < stop:
            line = stream.readline()
            if not line or not line.endswith(b"\n"):
                break
            yield json.loads(line)


def command_outputs(path, stop, changed, limit):
    """Recover selected completed commands, including earlier/polled chunks.

    The scan stays inside the sandbox and returns bounded outputs only. No
    assumed equivalence between model call IDs and native execution IDs:
    exact identity or an explicit process/session ID establishes a binding.
    Unrelated control calls are ignored, never cursor barriers.
    """
    commands, processes, polls = set(), {}, {}
    current = None
    for record in records_before(path, stop):
        p = record.get("payload", {})
        if record.get("type") == "event_msg":
            if p.get("type") == "task_started":
                current = p.get("turn_id")
            item = p.get("item", {})
            if (
                p.get("type") == "item_completed"
                and item.get("type") == "CommandExecution"
            ):
                commands.add(item["id"])
                if item.get("process_id") is not None:
                    processes[
                        (p.get("turn_id") or current, str(item["process_id"]))
                    ] = item["id"]
        elif (
            record.get("type") == "response_item"
            and p.get("type") == "function_call"
            and p.get("name", "").split(".")[-1] == "write_stdin"
        ):
            try:
                args = json.loads(p["arguments"])
                polls[p["call_id"]] = (current, str(args["session_id"]))
            except (KeyError, ValueError, TypeError) as _error:
                continue
        elif (
            record.get("type") == "response_item"
            and p.get("type") in ("function_call_output", "custom_tool_call_output")
            and isinstance(p.get("output"), str)
        ):
            header = p["output"].split("\nOutput:\n", 1)[0]
            match = re.search(r"Process running with session ID (\d+)", header)
            if match:
                polls[p["call_id"]] = (current, match.group(1))
    bindings = {ident: ident for ident in commands}
    bindings.update(
        {call: processes[key] for call, key in polls.items() if key in processes}
    )
    targets = {bindings[ident] for ident in changed if ident in bindings}
    if not targets:
        return {}
    buffers = {}
    for record in records_before(path, stop):
        p = record.get("payload", {})
        if record.get("type") != "response_item" or p.get("type") not in (
            "function_call_output",
            "custom_tool_call_output",
        ):
            continue
        ident = bindings.get(p.get("call_id"))
        if ident not in targets:
            continue
        value = p.get("output")
        if not isinstance(value, str):
            continue
        incomplete = "tokens truncated" in value or "Warning: truncated output" in value
        running = "Process running with session ID" in value.split("\nOutput:\n", 1)[0]
        if "\nOutput:\n" in value:
            value = value.split("\nOutput:\n", 1)[1]
        else:
            try:
                decoded = json.loads(value)
                if isinstance(decoded, dict) and isinstance(decoded.get("output"), str):
                    value = decoded["output"]
            except ValueError:
                pass
        raw = value.encode()
        state = buffers.setdefault(
            ident, {"head": b"", "tail": b"", "size": 0, "incomplete": False}
        )
        state["head"] = (state["head"] + raw)[:limit]
        state["tail"] = (state["tail"] + raw)[-limit:]
        state["size"] += len(raw)
        state["incomplete"] |= incomplete
        state["running"] = running
    results = {}
    for ident, state in buffers.items():
        incomplete = state["incomplete"] or state["running"]
        size = None if incomplete else state["size"]
        if state["size"] > limit:
            result = {
                "type": "truncated_text",
                "head": state["head"][: limit // 2].decode("utf-8", "ignore"),
                "tail": state["tail"][-(limit - limit // 2) :].decode(
                    "utf-8", "ignore"
                ),
                "original_bytes": size,
                "source_type": "text",
            }
            completeness, reason = "truncated", "orpheus_limit"
        else:
            result = {
                "type": "text",
                "text": state["head"].decode(),
                "original_bytes": size,
            }
            completeness, reason = (
                ("truncated", "harness_limit") if incomplete else ("complete", None)
            )
        results[ident] = {
            "type": "output",
            "id": ident,
            "result": result,
            "completeness": completeness,
            "reason": reason,
        }
    return results


def project(item):
    typ = item["type"]
    out = {"id": item["id"], "type": typ[0].lower() + typ[1:]}
    for key, value in item.items():
        if key in (
            "id",
            "type",
            "stdout",
            "stderr",
            "aggregated_output",
            "formatted_output",
            "result",
            "raw_content",
        ):
            continue
        parts = key.split("_")
        out[parts[0] + "".join(p.title() for p in parts[1:])] = value
    if typ == "AgentMessage":
        out["text"] = "".join(x.get("text", "") for x in item.get("content", []))
    elif typ == "FileChange":
        out["changes"] = [
            {
                "path": path,
                "kind": (
                    {"type": "update", "move_path": change.get("move_path")}
                    if change["type"] == "update"
                    else {"type": change["type"]}
                ),
                "diff": change.get("unified_diff", change.get("content", "")),
            }
            for path, change in sorted(item.get("changes", {}).items())
        ]
    return out


def journal():
    path, offset, limit = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
    items = []
    exhausted = False
    size = 0
    changed = set()
    with open(path, "rb") as f:
        f.seek(offset)
        for _ in range(128):
            line = f.readline()
            if not line or not line.endswith(b"\n"):
                exhausted = True
                break
            record = json.loads(line)
            p = record.get("payload", {})
            if record.get("type") == "event_msg" and p.get("type") == "item_completed":
                item = p["item"]
                if item["type"] in ITEM_TYPES and p.get("started_at_ms") is not None:
                    items.append(
                        {
                            "type": "order",
                            "id": item["id"],
                            "started_at_ms": p["started_at_ms"],
                        }
                    )
                if item["type"] in VISIBLE_TYPES:
                    if item["type"] == "CommandExecution":
                        changed.add(item["id"])
                        items.append(
                            {
                                "type": "command",
                                "id": item["id"],
                                "argv": item["command"],
                            }
                        )
                    items.append(
                        {
                            "type": "completed",
                            "turn_id": p.get("turn_id"),
                            "id": item["id"],
                        }
                    )
                    result = output(item, limit)
                    if result:
                        items.append(result)
                        size += len(json.dumps(result))
            elif record.get("type") == "response_item" and p.get("type") in (
                "function_call_output",
                "custom_tool_call_output",
            ):
                changed.add(p["call_id"])
                size += min(limit, len(json.dumps(p.get("output", ""))))
            offset = f.tell()
            if size > 2 * 1024 * 1024:
                break
    if changed:
        items.extend(command_outputs(path, offset, changed, limit).values())
    print(json.dumps({"offset": offset, "items": items, "exhausted": exhausted}))


def index():
    """Read committed identity/order/argv once, without replaying tool results."""
    path, stop = sys.argv[1], int(sys.argv[2])
    orders, commands, completed = {}, {}, []
    for record in records_before(path, stop):
        payload = record.get("payload", {})
        if record.get("type") != "event_msg" or payload.get("type") != "item_completed":
            continue
        item = payload["item"]
        if item["type"] in ITEM_TYPES and payload.get("started_at_ms") is not None:
            orders[item["id"]] = payload["started_at_ms"]
        if item["type"] in VISIBLE_TYPES:
            completed.append(item["id"])
        if item["type"] == "CommandExecution":
            commands[item["id"]] = item["command"]
    print(json.dumps({"orders": orders, "commands": commands, "completed": completed}))


def offline():
    path, offset, limit = sys.argv[1], 0, int(sys.argv[3])
    turns, orders, outputs, completed = {}, {}, {}, set()
    current = None
    with open(path, "rb") as f:
        for sequence, line in enumerate(iter(f.readline, b"")):
            if not line.endswith(b"\n"):
                break
            record = json.loads(line)
            p = record.get("payload", {})
            if record.get("type") == "event_msg":
                tid = p.get("turn_id") or current
                kind = p.get("type")
                if kind == "task_started":
                    current = tid
                    turns.setdefault(
                        tid, {"id": tid, "status": "inProgress", "items": {}}
                    )
                elif kind in ("item_started", "item_completed") and tid in turns:
                    item = p["item"]
                    if item["type"] in ITEM_TYPES:
                        ident = item["id"]
                        key = (tid, ident)
                        started = p.get("started_at_ms")
                        if started is None:
                            timestamp = record.get("timestamp")
                            started = (
                                datetime.datetime.fromisoformat(
                                    timestamp.replace(
                                        "Z", "+00:00"
                                    )  # Python 3.10 requires an explicit UTC offset.
                                ).timestamp()
                                * 1000
                                if timestamp
                                else sequence
                            )
                        orders.setdefault(key, (started, ident))
                        turns[tid]["items"][ident] = project(item)
                        if kind == "item_completed":
                            completed.add(ident)
                            result = output(item, limit)
                            if result:
                                outputs[ident] = result
                elif kind == "error" and tid in turns:
                    turns[tid]["error"] = p
                    turns[tid]["status"] = "failed"
                elif kind in ("task_complete", "turn_aborted") and tid in turns:
                    turns[tid]["status"] = (
                        "completed" if kind == "task_complete" else "interrupted"
                    )
            offset = f.tell()
    for tid, turn in turns.items():
        turn["items"] = sorted(
            turn["items"].values(), key=lambda i: orders[(tid, i["id"])]
        )
    outputs.update(command_outputs(path, offset, completed, limit))
    print(
        json.dumps(
            {
                "turns": list(turns.values()),
                "completed": sorted(completed),
                "outputs": outputs,
                "offset": offset,
            }
        )
    )
