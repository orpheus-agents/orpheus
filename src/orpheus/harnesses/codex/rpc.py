"""JSON-RPC over AgentBox stdin/stdout. Requests are never retried here."""

import asyncio
import json
import logging
from typing import Any
from uuid import uuid4

from agentbox import AsyncSandbox

from orpheus.errors import UncertainError
from orpheus.harnesses.base import HarnessRejectedError

logger = logging.getLogger(__name__)


class RPCError(HarnessRejectedError):
    def __init__(self, error: dict[str, Any]):
        self.code = error.get("code")
        # Keep native errors private; callers classify without logging their text.
        self.native_message = str(error.get("message", ""))
        super().__init__("Harness rejected RPC request")


class RPC:
    def __init__(self, sandbox: AsyncSandbox, timeout: float):
        self.sandbox, self.timeout = sandbox, timeout
        self.handle: Any = None
        self.buffer = ""
        self.pending: dict[str, asyncio.Future] = {}
        self.notifications: list[dict[str, Any]] = []
        self.bytes_seen = 0
        self.disconnected = False
        self.history_dirty = False

    async def stdout(self, chunk: str) -> None:
        self.bytes_seen += len(chunk)
        self.buffer += chunk
        while "\n" in self.buffer:
            line, self.buffer = self.buffer.split("\n", 1)
            if not line.strip():
                continue
            try:
                value = json.loads(line)
                if not isinstance(value, dict):
                    raise TypeError
            except ValueError, TypeError, RecursionError:
                self.history_dirty = True
                logger.warning(
                    "Ignored malformed harness stdout; history reconciliation required"
                )
                continue
            ident = str(value.get("id", ""))
            if "method" in value:
                if "id" in value:
                    # Autonomous operation: no approval UI and no stalled server requests.
                    await self.sandbox.commands.send_stdin(
                        self.handle.pid,
                        json.dumps(
                            {
                                "id": value["id"],
                                "error": {
                                    "code": -32601,
                                    "message": "Client requests are not supported",
                                },
                            }
                        )
                        + "\n",
                    )
                elif value["method"] in {
                    "item/completed",
                    "item/started",
                    "turn/completed",
                    "turn/started",
                }:
                    self.notifications.append(value)
            elif ident in self.pending and not self.pending[ident].done():
                self.pending[ident].set_result(value)

    async def attach(self, pid: int) -> None:
        self.handle = await self.sandbox.commands.connect(
            pid, timeout=0, on_stdout=self.stdout
        )

    async def launch(self, env: dict[str, str], cwd: str) -> int:
        self.handle = await self.sandbox.commands.run(
            "exec codex app-server",
            background=True,
            stdin=True,
            timeout=0,
            envs=env,
            cwd=cwd,
            user="user",
            on_stdout=self.stdout,
        )
        return self.handle.pid

    async def call(self, method: str, params: dict[str, Any]) -> dict[str, Any]:
        ident = str(uuid4())
        future = asyncio.get_running_loop().create_future()
        self.pending[ident] = future
        try:
            async with asyncio.timeout(self.timeout):
                await self.sandbox.commands.send_stdin(
                    self.handle.pid,
                    json.dumps({"id": ident, "method": method, "params": params})
                    + "\n",
                    request_timeout=self.timeout,
                )
                value = await future
            if "error" in value:
                raise RPCError(value["error"])
            return value.get("result", {})
        except RPCError:
            raise
        except OSError, TimeoutError, ConnectionError:
            raise UncertainError from None
        finally:
            self.pending.pop(ident, None)

    async def initialize(self) -> None:
        try:
            await self.call(
                "initialize", {"clientInfo": {"name": "orpheus", "version": "0.1.0"}}
            )
            await self.sandbox.commands.send_stdin(
                self.handle.pid, '{"method":"initialized"}\n'
            )
        except RPCError as error:
            if "already initialized" not in error.native_message.lower():
                raise

    async def close(self) -> None:
        if self.handle is not None:
            await self.handle.disconnect()
        self.disconnected = True
