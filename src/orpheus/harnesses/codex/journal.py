"""Read-only, one-shot journal projection executed through AgentBox commands.

No service is installed in the sandbox. Large native tool output is reduced
before transport; whole messages are preserved. Offsets stop at complete lines.
"""

import json
import shlex
from pathlib import Path
from typing import Any

from agentbox import AsyncSandbox

# The file uses only the sandbox's standard library; no package is installed.
SOURCE = Path(__file__).with_name("native_reader.py").read_text()
READER = SOURCE + "\njournal()\n"
INDEX = SOURCE + "\nindex()\n"


async def read_index(sandbox: AsyncSandbox, path: str, offset: int) -> dict[str, Any]:
    result = await sandbox.commands.run(
        "python3 -c " + shlex.quote(INDEX) + " " + shlex.quote(path) + f" {offset}",
        user="user",
        timeout=30,
    )
    return json.loads(result.stdout)


async def read_journal(
    sandbox: AsyncSandbox, path: str, offset: int, limit: int
) -> dict[str, Any]:
    command = (
        "python3 -c "
        + shlex.quote(READER)
        + " "
        + shlex.quote(path)
        + f" {offset} {limit}"
    )
    result = await sandbox.commands.run(command, user="user", timeout=30)
    return json.loads(result.stdout)
