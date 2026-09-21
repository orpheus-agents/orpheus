import json
import os
import time
from typing import Any

from orpheus.configuration import AgentConfiguration, Credentials
from orpheus.errors import ExecutionError
from orpheus.harnesses.base import Context, Snapshot
from orpheus.harnesses.codex import offline
from orpheus.harnesses.codex.credentials import AccountCredentials
from orpheus.harnesses.codex.items import normalize_thread
from orpheus.harnesses.codex.journal import read_index, read_journal
from orpheus.harnesses.codex.rpc import RPC, RPCError


class Codex:
    def __init__(self, rpc: RPC, limit: int):
        self.rpc, self.limit = rpc, limit
        self.completed: set[str] = set()
        self.outputs: dict[str, dict[str, Any]] = {}
        self.thread: dict[str, Any] | None = None
        self.read_at = 0.0
        self.orders: dict[str, int] = {}
        self.commands: dict[str, Any] = {}
        self.index_loaded = False
        self.journal_ids: set[str] = set()
        self.divergence_reads = 0
        self.recovered: dict[str, dict[str, Any]] = {}

    def credentials(self, home: str, source: Credentials) -> AccountCredentials | None:
        if source.mode == "account":
            return AccountCredentials(self.rpc.sandbox, home, source)
        return None

    async def prepare(self, home: str, source: Credentials) -> dict[str, str]:
        await self.rpc.sandbox.files.write(
            f"{home}/config.toml",
            'cli_auth_credentials_store = "file"\nforced_login_method = '
            + json.dumps("chatgpt" if source.mode == "account" else "api")
            + '\napproval_policy = "never"\nsandbox_mode = "danger-full-access"\n',
            user="user",
        )
        return {"CODEX_HOME": home}

    async def launch(self, env: dict[str, str], cwd: str) -> int:
        return await self.rpc.launch(env, cwd)

    async def attach(self, pid: int) -> None:
        await self.rpc.attach(pid)

    async def initialize(self, source: Credentials, *, login: bool) -> None:
        await self.rpc.initialize()
        if login and source.mode == "api_key":
            key = os.environ.get(source.api_key_env or "")
            if not key:
                raise ExecutionError(
                    "credentials_unavailable", "Provider API key is unavailable."
                )
            await self.rpc.call(
                "account/login/start", {"type": "apiKey", "apiKey": key}
            )
        if source.mode == "account":
            account = await self.rpc.call("account/read", {"refreshToken": False})
            if (account.get("account") or {}).get("type") != "chatgpt":
                raise ExecutionError(
                    "authentication_failed", "Account credentials are invalid."
                )

    async def open_context(
        self, agent: AgentConfiguration, cwd: str, context_id: str | None = None
    ) -> Context:
        params = {
            "model": agent.model,
            "cwd": cwd,
            "approvalPolicy": "never",
            "sandbox": "danger-full-access",
            "baseInstructions": agent.instructions or None,
        }
        if context_id is not None:
            try:
                await self.rpc.call("thread/resume", {**params, "threadId": context_id})
            except RPCError:
                raise ExecutionError(
                    "context_lost", "Harness context cannot be restored."
                ) from None
            return Context(context_id)
        thread = (await self.rpc.call("thread/start", params))["thread"]
        return Context(thread["id"], thread.get("path"))

    async def recover(
        self, context_id: str | None, history_path: str | None, home: str | None
    ) -> Snapshot:
        return await offline.recover(
            self.rpc.sandbox, context_id, history_path, home, self.limit
        )

    @property
    def has_updates(self) -> bool:
        return bool(
            self.rpc.notifications
            or self.rpc.history_dirty
            or self.thread is None
            or time.monotonic() >= self.read_at
        )

    @property
    def needs_reconnect(self) -> bool:
        return self.rpc.bytes_seen > 4 * 1024 * 1024

    def committed(self) -> None:
        # Full results are durable now; do not retain past tool output per session.
        self.outputs.clear()

    async def start(self, thread_id: str, text: str) -> str:
        result = await self.rpc.call(
            "turn/start",
            {"threadId": thread_id, "input": [{"type": "text", "text": text}]},
        )
        if self.thread is not None:
            self.thread.setdefault("turns", []).append(result["turn"])
        return result["turn"]["id"]

    async def steer(self, thread_id: str, turn_id: str, text: str) -> None:
        await self.rpc.call(
            "turn/steer",
            {
                "threadId": thread_id,
                "expectedTurnId": turn_id,
                "input": [{"type": "text", "text": text}],
            },
        )

    async def interrupt(self, thread_id: str, turn_id: str) -> None:
        await self.rpc.call(
            "turn/interrupt", {"threadId": thread_id, "turnId": turn_id}
        )

    async def snapshot(
        self, thread_id: str, history_path: str | None, offset: int
    ) -> Snapshot:
        start_offset = offset
        journal_completed = self.journal_ids.copy()
        full_read = (
            self.thread is None
            or self.rpc.history_dirty
            or time.monotonic() >= self.read_at
            or any(n["method"] == "turn/completed" for n in self.rpc.notifications)
        )
        if full_read:
            self.rpc.history_dirty = False
            try:
                self.thread = (
                    await self.rpc.call(
                        "thread/read", {"threadId": thread_id, "includeTurns": True}
                    )
                )["thread"]
            except RPCError as error:
                if "not found" in error.native_message.lower():
                    raise ExecutionError(
                        "context_lost", "Harness context is unavailable."
                    ) from None
                raise
            self.read_at = time.monotonic() + 30
        assert self.thread is not None
        thread = self.thread
        notifications, self.rpc.notifications = self.rpc.notifications, []
        path = thread.get("path") or history_path
        if path and not self.index_loaded:
            if offset:
                index = await read_index(self.rpc.sandbox, path, offset)
                self.orders.update(index["orders"])
                self.commands.update(index["commands"])
                self.completed.update(index["completed"])
                journal_completed.update(index["completed"])
                self.journal_ids.update(index["completed"])
            self.index_loaded = True
        # Read all completed journal batches before exposing a terminal turn.
        if path and (
            full_read
            or any(
                n["method"] in ("item/completed", "turn/completed")
                for n in notifications
            )
        ):
            while True:
                batch = await read_journal(self.rpc.sandbox, path, offset, self.limit)
                previous, offset = offset, batch["offset"]
                for item in batch["items"]:
                    if item["type"] == "completed":
                        self.completed.add(item["id"])
                        journal_completed.add(item["id"])
                        self.journal_ids.add(item["id"])
                    elif item["type"] == "output":
                        self.outputs[item["id"]] = item
                    elif item["type"] == "order":
                        self.orders[item["id"]] = item["started_at_ms"]
                    elif item["type"] == "command":
                        self.commands[item["id"]] = item["argv"]
                if offset == previous or batch.get("exhausted", False):
                    break
        native_turns = {turn["id"]: turn for turn in thread.get("turns", [])}
        for notification in notifications:
            if notification["method"] in ("turn/started", "turn/completed"):
                native = notification["params"]["turn"]
                existing_turn = native_turns.get(native["id"])
                if existing_turn is None:
                    native_turns[native["id"]] = native
                    thread.setdefault("turns", []).append(native)
                else:
                    existing_turn.update(
                        {k: v for k, v in native.items() if k != "items"}
                    )
                    # Completion notifications can omit items; retain observed items.
                    for item in native.get("items", []):
                        items = existing_turn.setdefault("items", [])
                        index = next(
                            (
                                i
                                for i, old in enumerate(items)
                                if old["id"] == item["id"]
                            ),
                            None,
                        )
                        if index is None:
                            items.append(item)
                        else:
                            items[index] = item
            elif notification["method"] in ("item/started", "item/completed"):
                params = notification["params"]
                item = params["item"]
                if notification["method"] == "item/completed":
                    self.completed.add(item["id"])
                turn = native_turns.get(params.get("turnId"))
                if turn is None:
                    self.rpc.history_dirty = True
                else:
                    items = turn.setdefault("items", [])
                    existing = next(
                        (
                            index
                            for index, known in enumerate(items)
                            if known["id"] == item["id"]
                        ),
                        None,
                    )
                    if existing is None:
                        items.append(item)
                    elif notification["method"] == "item/completed":
                        items[existing] = item
        self.merge_recovered(thread)
        snapshot = self.normalize(thread)
        native_items = {
            item["id"]
            for turn in thread.get("turns", [])
            for item in turn.get("items", [])
        }
        tool_items = {
            item.native_id
            for turn in snapshot.turns
            for item in turn.items
            if item.type == "tool"
        }
        if journal_completed - native_items or self.outputs.keys() - tool_items:
            self.divergence_reads += 1
            if self.divergence_reads >= 3 and path:
                # A terminal thread/read can permanently omit native items (for
                # example after interruption). Recover them instead of waiting
                # until the run deadline or discarding their durable output.
                history = await offline.read_history(self.rpc.sandbox, path, self.limit)
                for turn in history["turns"]:
                    previous = {
                        i["id"]
                        for i in self.recovered.get(turn["id"], {}).get("items", [])
                    }
                    self.recovered[turn["id"]] = {
                        **turn,
                        "items": [
                            i
                            for i in turn["items"]
                            if i["id"] not in native_items or i["id"] in previous
                        ],
                    }
                self.completed.update(history["completed"])
                self.outputs.update(history["outputs"])
                self.merge_recovered(thread)
                snapshot = self.normalize(thread)
                represented = {i.native_id for t in snapshot.turns for i in t.items}
                if (journal_completed | self.outputs.keys()) - represented:
                    raise ExecutionError(
                        "harness_failed",
                        "Native history cannot recover the missing items.",
                    )
                self.divergence_reads = 0
                # Recovery may observe newer records, but commit only the prefix
                # read by the incremental reader, including its order metadata.
                snapshot.path, snapshot.offset = path, offset
                return snapshot
            # The journal can advance while thread/read is in flight. Never
            # commit a cursor past results not represented by this snapshot:
            # a fresh worker must be able to read them again after a crash.
            offset = start_offset
            self.rpc.history_dirty = True
            # Do not pause a terminal run before its remaining history is saved.
            for turn in snapshot.turns:
                if turn.status in ("completed", "failed", "cancelled"):
                    turn.status = "running"
        else:
            self.divergence_reads = 0
        snapshot.path, snapshot.offset = path, offset
        return snapshot

    def normalize(self, thread: dict[str, Any]) -> Snapshot:
        return normalize_thread(
            thread, self.completed, self.outputs, self.limit, self.orders, self.commands
        )

    def merge_recovered(self, thread: dict[str, Any]) -> None:
        turns = {t["id"]: t for t in thread.get("turns", [])}
        for ident, recovered in self.recovered.items():
            if ident not in turns:
                thread.setdefault("turns", []).append(
                    {**recovered, "items": list(recovered["items"])}
                )
                continue
            turn = turns[ident]
            known = {i["id"] for i in turn["items"]}
            turn["items"].extend(i for i in recovered["items"] if i["id"] not in known)

    async def close(self) -> None:
        await self.rpc.close()
