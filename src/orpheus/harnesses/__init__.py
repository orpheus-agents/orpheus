"""Harness implementations. Public API never depends on native protocols."""

from agentbox import AsyncSandbox

from orpheus.harnesses.base import Harness


def create_harness(
    name: str, sandbox: AsyncSandbox, *, timeout: float, output_limit: int
) -> Harness:
    if name == "codex":
        from orpheus.harnesses.codex.driver import Codex
        from orpheus.harnesses.codex.rpc import RPC

        return Codex(RPC(sandbox, timeout), output_limit)
    raise ValueError(f"Unsupported harness: {name}")
