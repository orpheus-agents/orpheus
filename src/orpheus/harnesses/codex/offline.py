"""Recover Codex JSONL history after app-server exits, using a one-shot reader."""

import json
import shlex
from typing import Any

from agentbox import AsyncSandbox

from orpheus.errors import ExecutionError
from orpheus.harnesses.base import Snapshot
from orpheus.harnesses.codex.items import normalize_thread
from orpheus.harnesses.codex.journal import SOURCE

OFFLINE = SOURCE + "\noffline()\n"


async def read_history(sandbox: AsyncSandbox, path: str, limit: int) -> dict[str, Any]:
    result = await sandbox.commands.run(
        "python3 -c " + shlex.quote(OFFLINE) + " " + shlex.quote(path) + f" 0 {limit}",
        user="user",
        timeout=30,
    )
    return json.loads(result.stdout)


async def offline_snapshot(sandbox: AsyncSandbox, path: str, limit: int) -> Snapshot:
    data = await read_history(sandbox, path, limit)
    snapshot = normalize_thread(data, set(data["completed"]), data["outputs"], limit)
    snapshot.path, snapshot.offset = path, data["offset"]
    return snapshot


async def recover(
    sandbox: AsyncSandbox,
    context_id: str | None,
    history_path: str | None,
    home: str | None,
    limit: int,
) -> Snapshot:
    if not history_path and context_id and home:
        script = "import pathlib,sys,json; print(json.dumps([str(p) for p in (pathlib.Path(sys.argv[1])/'sessions').rglob('*.jsonl') if sys.argv[2] in p.name]))"
        result = await sandbox.commands.run(
            "python3 -c "
            + shlex.quote(script)
            + " "
            + shlex.quote(home)
            + " "
            + shlex.quote(context_id),
            user="user",
            timeout=30,
        )
        paths = json.loads(result.stdout)
        if len(paths) == 1:
            history_path = paths[0]
    if not history_path:
        raise ExecutionError("context_lost", "Harness history is unavailable.")
    return await offline_snapshot(sandbox, history_path, limit)
