"""Shared projection for app-server history and native JSONL recovery."""

import shlex
from typing import Any

from orpheus.harnesses.base import Item, Snapshot, Turn
from orpheus.harnesses.codex.native_reader import ITEM_TYPES, TOOL_TYPES
from orpheus.harnesses.output import bounded_result

ITEM_KINDS = {t[0].lower() + t[1:] for t in ITEM_TYPES}
TOOL_KINDS = {t[0].lower() + t[1:] for t in TOOL_TYPES}


def normalize_thread(
    thread: dict[str, Any],
    completed: set[str],
    outputs: dict[str, dict[str, Any]],
    limit: int,
    orders: dict[str, int] | None = None,
    commands: dict[str, Any] | None = None,
) -> Snapshot:
    turns = []
    for native in thread.get("turns", []):
        status = {
            "inProgress": "running",
            "completed": "completed",
            "failed": "failed",
            "interrupted": "cancelled",
        }.get(native["status"], "unknown")
        turn = Turn(native_id=native["id"], status=status)
        if native.get("error"):
            detail = str(native["error"]).lower()
            turn.error_code = (
                "authentication_failed"
                if any(
                    s in detail
                    for s in (
                        "unauthorized",
                        "authentication",
                        "401",
                        "token_expired",
                        "refresh_token",
                    )
                )
                else "harness_failed"
            )
        items = [i for i in native.get("items", []) if i["type"] in ITEM_KINDS]
        if orders:
            # thread/read can return completion order while live notifications
            # use start order. Persisted start timestamps survive reconnects.
            items.sort(
                key=lambda item: (
                    orders.get(item["id"], float("inf")),
                    item["id"] if item["id"] in orders else "",
                )
            )
        for index, item in enumerate(items):
            ident, kind = item["id"], item["type"]
            if kind == "userMessage":
                turn.items.append(
                    Item(
                        ident,
                        "user",
                        index,
                        text="".join(
                            v.get("text", "") for v in item.get("content", [])
                        ),
                    )
                )
            elif kind == "agentMessage":
                if ident in completed or status in (
                    "completed",
                    "failed",
                    "cancelled",
                ):
                    turn.items.append(
                        Item(
                            ident,
                            "assistant",
                            index,
                            text=item.get("text", ""),
                            kind="progress"
                            if item.get("phase") == "commentary"
                            else "answer",
                        )
                    )
            elif kind in TOOL_KINDS:
                state = {
                    "inProgress": "running",
                    "completed": "completed",
                    "failed": "failed",
                    "declined": "cancelled",
                }.get(
                    item.get("status"),
                    "completed" if ident in completed else "unknown",
                )
                if state == "running" and status in (
                    "completed",
                    "failed",
                    "cancelled",
                ):
                    state = "unknown"
                output = outputs.get(ident)
                if output:
                    result, completeness, reason = (
                        output["result"],
                        output["completeness"],
                        output["reason"],
                    )
                    if result["type"] != "json":
                        result = {**result, "exit_code": item.get("exitCode")}
                elif (
                    "aggregatedOutput" in item and item["aggregatedOutput"] is not None
                ):
                    result, completeness, reason = bounded_result(
                        item["aggregatedOutput"],
                        limit,
                        item.get("exitCode"),
                        incomplete=True,
                    )
                elif item.get("result") is not None:
                    result, completeness, reason = bounded_result(item["result"], limit)
                else:
                    result, completeness, reason = None, "unknown", None
                tool_input = item.get(
                    "arguments", item.get("command", item.get("changes", {}))
                )
                if kind == "commandExecution":
                    if commands and ident in commands:
                        tool_input = commands[ident]
                    if isinstance(tool_input, str):
                        try:
                            tool_input = shlex.split(tool_input)
                        except ValueError:
                            # A display string is not necessarily valid shell syntax.
                            # Preserve it until native argv arrives in the journal.
                            pass
                elif kind == "fileChange":
                    tool_input = sorted(tool_input, key=lambda change: change["path"])
                turn.items.append(
                    Item(
                        ident,
                        "tool",
                        index,
                        name=item.get("tool") or kind,
                        input=tool_input,
                        status=state,
                        result=result,
                        completeness=completeness,
                        truncation_reason=reason,
                    )
                )
        turns.append(turn)
    return Snapshot(turns=turns)
