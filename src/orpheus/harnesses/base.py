from dataclasses import dataclass, field
from typing import Any, Protocol

from orpheus.configuration import AgentConfiguration, Credentials


class HarnessRejectedError(Exception):
    """The harness confirmed rejection; delivery is not uncertain."""


class CredentialSync(Protocol):
    async def seed(self) -> None: ...
    async def watch(self) -> None: ...
    async def sync(self, *, force: bool = False) -> None: ...
    async def close(self) -> None: ...


@dataclass
class Context:
    native_id: str
    history_path: str | None = None


@dataclass
class Item:
    native_id: str
    type: str
    index: int
    text: str = ""
    kind: str | None = None
    name: str = ""
    input: Any = None
    status: str = "unknown"
    result: dict[str, Any] | None = None
    completeness: str = "unknown"
    truncation_reason: str | None = None


@dataclass
class Turn:
    native_id: str
    status: str
    items: list[Item] = field(default_factory=list)
    error_code: str | None = None


@dataclass
class Snapshot:
    turns: list[Turn]
    path: str | None = None
    offset: int = 0


class Harness(Protocol):
    """Native process, protocol, and history stay behind this boundary.

    The worker owns durable operations and process identity. Implementations
    report confirmed rejections with HarnessRejectedError and ambiguous delivery
    with UncertainError; recover() must work without a live harness process.
    Initialization and reopening a known context may be repeated after a lost
    acknowledgement, before the worker dispatches any task to that process.
    """

    def credentials(self, home: str, source: Credentials) -> CredentialSync | None: ...
    async def prepare(self, home: str, source: Credentials) -> dict[str, str]: ...
    async def launch(self, env: dict[str, str], cwd: str) -> int: ...
    async def attach(self, pid: int) -> None: ...
    async def initialize(self, source: Credentials, *, login: bool) -> None: ...
    async def open_context(
        self, agent: AgentConfiguration, cwd: str, context_id: str | None = None
    ) -> Context: ...
    async def recover(
        self, context_id: str | None, history_path: str | None, home: str | None
    ) -> Snapshot: ...

    @property
    def has_updates(self) -> bool: ...

    @property
    def needs_reconnect(self) -> bool: ...

    def committed(self) -> None: ...

    async def start(self, thread_id: str, text: str) -> str: ...
    async def steer(self, thread_id: str, turn_id: str, text: str) -> None: ...
    async def interrupt(self, thread_id: str, turn_id: str) -> None: ...
    async def snapshot(
        self, thread_id: str, history_path: str | None, offset: int
    ) -> Snapshot: ...
    async def close(self) -> None: ...
