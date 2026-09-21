import json
from typing import Any


def bounded_result(
    value: Any, limit: int, exit_code: int | None = None, *, incomplete: bool = False
) -> tuple[dict[str, Any], str, str | None]:
    is_text = isinstance(value, str)
    raw = (
        value
        if is_text
        else json.dumps(value, ensure_ascii=False, separators=(",", ":"))
    ).encode("utf-8")
    original = None if incomplete else len(raw)
    if len(raw) > limit:
        head = raw[: limit // 2].decode("utf-8", errors="ignore")
        tail = raw[-(limit - limit // 2) :].decode("utf-8", errors="ignore")
        return (
            {
                "type": "truncated_text",
                "head": head,
                "tail": tail,
                "source_type": "text" if is_text else "json",
                "exit_code": exit_code,
                "original_bytes": original,
            },
            "truncated",
            "orpheus_limit",
        )
    if is_text:
        result = {
            "type": "text",
            "text": value,
            "exit_code": exit_code,
            "original_bytes": original,
        }
    else:
        result = {"type": "json", "value": value, "original_bytes": original}
    return (
        result,
        "truncated" if incomplete else "complete",
        "harness_limit" if incomplete else None,
    )
