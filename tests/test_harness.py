import asyncio
import json
import shlex
import subprocess
from types import SimpleNamespace
from typing import Any, cast
from unittest.mock import AsyncMock

import pytest
from agentbox import AsyncSandbox

from orpheus.errors import UncertainError
from orpheus.harnesses.codex.journal import READER
from orpheus.harnesses.codex.offline import OFFLINE
from orpheus.harnesses.codex.rpc import RPC, RPCError
from orpheus.harnesses.output import bounded_result


@pytest.mark.parametrize(
    "value", ["я" * 1000, {"stdout": "x" * 1000, "stderr": "y" * 1000}]
)
def test_tool_budget_combined_utf8_and_json(value):
    result, complete, reason = bounded_result(value, 101)
    assert result["type"] == "truncated_text"
    assert len(result["head"].encode()) + len(result["tail"].encode()) <= 101
    assert complete == "truncated" and reason == "orpheus_limit"
    assert result["original_bytes"] > 101
    json.dumps(result).encode("utf8")


def test_short_and_native_truncated_result():
    assert bounded_result("ok", 10, 0) == (
        {"type": "text", "text": "ok", "exit_code": 0, "original_bytes": 2},
        "complete",
        None,
    )
    result, _complete, reason = bounded_result("tail", 10, incomplete=True)
    assert result["original_bytes"] is None and reason == "harness_limit"


async def test_rpc_fragmentation_and_opaque_errors():
    commands = SimpleNamespace(send_stdin=AsyncMock())
    rpc = RPC(cast(AsyncSandbox, SimpleNamespace(commands=commands)), 0.1)
    rpc.handle = SimpleNamespace(pid=1)

    async def reply(pid, data, **kwargs):
        value = json.loads(data)
        if value.get("method") == "read":
            raw = json.dumps({"id": value["id"], "result": {"value": "я"}}) + "\n"
            await rpc.stdout(raw[:5])
            await rpc.stdout(raw[5:])
        else:
            await rpc.stdout(
                json.dumps(
                    {"id": value["id"], "error": {"code": 1, "message": "secret"}}
                )
                + "\n"
            )

    commands.send_stdin.side_effect = reply
    assert await rpc.call("read", {}) == {"value": "я"}
    with pytest.raises(RPCError) as exc:
        await rpc.call("fail", {})
    assert "secret" not in str(exc.value)


async def test_lost_rpc_response_is_not_retried():
    commands = SimpleNamespace(send_stdin=AsyncMock())
    rpc = RPC(cast(AsyncSandbox, SimpleNamespace(commands=commands)), 0.01)
    rpc.handle = SimpleNamespace(pid=1)
    with pytest.raises(UncertainError):
        await rpc.call("turn/start", {"input": []})
    commands.send_stdin.assert_awaited_once()
    assert not rpc.pending


async def test_unexpected_approval_is_rejected():
    commands = SimpleNamespace(send_stdin=AsyncMock())
    rpc = RPC(cast(AsyncSandbox, SimpleNamespace(commands=commands)), 1)
    rpc.handle = SimpleNamespace(pid=1)
    await rpc.stdout('{"id":12,"method":"item/commandExecution/requestApproval"}\n')
    response = json.loads(commands.send_stdin.call_args.args[1])
    assert response["id"] == 12 and "error" in response


def test_native_readers_preserve_full_result_and_incomplete_line(tmp_path):
    records = [
        {"type": "event_msg", "payload": {"type": "task_started", "turn_id": "t"}},
        {
            "type": "event_msg",
            "payload": {
                "type": "item_completed",
                "turn_id": "t",
                "item": {
                    "id": "u",
                    "type": "UserMessage",
                    "content": [{"text": "task"}],
                },
            },
        },
        {
            "type": "event_msg",
            "payload": {
                "type": "item_completed",
                "turn_id": "t",
                "item": {
                    "id": "tool",
                    "type": "CommandExecution",
                    "command": ["echo", "x"],
                    "status": "completed",
                    "aggregated_output": "head-" + "я" * 200 + "-tail",
                    "exit_code": 0,
                },
            },
        },
        {
            "type": "response_item",
            "payload": {
                "type": "function_call_output",
                "call_id": "unrelated-model-call",
                "output": "must not replace the native command output",
            },
        },
        {
            "type": "event_msg",
            "payload": {
                "type": "item_completed",
                "turn_id": "t",
                "item": {
                    "id": "a",
                    "type": "AgentMessage",
                    "phase": "final_answer",
                    "content": [{"text": "answer"}],
                },
            },
        },
        {"type": "event_msg", "payload": {"type": "task_complete", "turn_id": "t"}},
    ]
    path = tmp_path / "history.jsonl"
    valid = "".join(json.dumps(r) + "\n" for r in records).encode()
    path.write_bytes(valid + b'{"unfinished"')
    for script in (READER, OFFLINE):
        out = subprocess.check_output(["python", "-c", script, str(path), "0", "100"])
        data = json.loads(out)
        assert data["offset"] == len(valid)
        if script == READER:
            output = next(i for i in data["items"] if i["type"] == "output")
            assert output["result"]["head"].startswith("head-")
            assert output["result"]["tail"].endswith("-tail")
        else:
            from orpheus.harnesses.codex.items import normalize_thread

            snapshot = normalize_thread(
                data, set(data["completed"]), data["outputs"], 100
            )
            turn = snapshot.turns[0]
            assert turn.status == "completed"
            assert turn.items[-1].text == "answer"
            assert turn.items[1].result is not None
            assert turn.items[1].result["type"] == "truncated_text"


async def test_interrupted_snapshot_keeps_observed_tool_start():
    from orpheus.harnesses.codex.driver import Codex

    rpc = SimpleNamespace(
        history_dirty=False,
        call=AsyncMock(
            return_value={
                "thread": {"turns": [{"id": "t", "status": "interrupted", "items": []}]}
            }
        ),
        notifications=[
            {
                "method": "item/started",
                "params": {
                    "turnId": "t",
                    "item": {
                        "id": "tool",
                        "type": "commandExecution",
                        "status": "inProgress",
                        "command": "sleep 30",
                    },
                },
            }
        ],
    )
    snapshot = await Codex(cast(RPC, rpc), 100).snapshot("thread", None, 0)
    tool = snapshot.turns[0].items[0]
    assert tool.native_id == "tool" and tool.status == "unknown"
    assert tool.result is None and not rpc.notifications


async def test_non_json_stdout_does_not_break_rpc_or_log_contents(caplog):
    commands = SimpleNamespace(send_stdin=AsyncMock())
    rpc = RPC(cast(AsyncSandbox, SimpleNamespace(commands=commands)), 0.1)
    rpc.handle = SimpleNamespace(pid=1)

    async def reply(pid, data, **kwargs):
        ident = json.loads(data)["id"]
        await rpc.stdout('private banner\n[]\n{"unfinished":\n')
        await rpc.stdout(json.dumps({"id": ident, "result": {"ok": True}}) + "\n")

    commands.send_stdin.side_effect = reply
    assert await rpc.call("read", {}) == {"ok": True}
    assert rpc.history_dirty and "private banner" not in caplog.text


@pytest.mark.parametrize("status", ["completed", "interrupted"])
async def test_live_and_offline_history_have_identical_items_and_positions(
    tmp_path, status
):
    from dataclasses import asdict

    from orpheus.harnesses.codex.driver import Codex
    from orpheus.harnesses.codex.items import normalize_thread

    items: list[dict[str, Any]] = [
        {"id": "u", "type": "userMessage", "content": [{"text": "task"}]},
        {"id": "r", "type": "reasoning"},
        {"id": "p", "type": "plan"},
        {
            "id": "tool",
            "type": "commandExecution",
            "command": "echo hi",
            "status": "completed",
            "aggregatedOutput": "hi",
            "exitCode": 0,
        },
        {
            "id": "progress",
            "type": "agentMessage",
            "text": "working",
            "phase": "commentary",
        },
        {"id": "c", "type": "contextCompaction"},
        {
            "id": "answer",
            "type": "agentMessage",
            "text": "done",
            "phase": "final_answer",
        },
    ]
    records = [
        {"type": "event_msg", "payload": {"type": "task_started", "turn_id": "t"}}
    ]
    for item in items:
        native = {**item, "type": item["type"][0].upper() + item["type"][1:]}
        if "aggregatedOutput" in native:
            native["aggregated_output"] = native.pop("aggregatedOutput")
        if "exitCode" in native:
            native["exit_code"] = native.pop("exitCode")
        if "text" in native:
            native["content"] = [{"text": native.pop("text")}]
        # Started + completed must occupy one native index, including ignored kinds.
        for event in ("item_started", "item_completed"):
            records.append(
                {
                    "type": "event_msg",
                    "payload": {"type": event, "turn_id": "t", "item": native},
                }
            )
    records.append(
        {
            "type": "event_msg",
            "payload": {
                "type": "task_complete" if status == "completed" else "turn_aborted",
                "turn_id": "t",
            },
        }
    )
    path = tmp_path / "history.jsonl"
    path.write_text("".join(json.dumps(r) + "\n" for r in records))
    data = json.loads(
        await asyncio.to_thread(
            subprocess.check_output, ["python", "-c", OFFLINE, str(path), "0", "100"]
        )
    )
    offline = normalize_thread(data, set(data["completed"]), data["outputs"], 100)
    rpc = SimpleNamespace(
        history_dirty=False,
        notifications=[],
        call=AsyncMock(
            return_value={
                "thread": {"turns": [{"id": "t", "status": status, "items": items}]}
            }
        ),
    )
    harness = Codex(cast(RPC, rpc), 100)
    harness.outputs = data["outputs"]
    live = await harness.snapshot("thread", None, 0)
    assert asdict(live) == asdict(offline)
    assert [i.index for i in live.turns[0].items] == [0, 3, 4, 6]
    assert [i.type for i in live.turns[0].items] == [
        "user",
        "tool",
        "assistant",
        "assistant",
    ]


async def test_codex_uses_notifications_between_periodic_history_reads(monkeypatch):
    from orpheus.harnesses.codex.driver import Codex

    journal = AsyncMock(return_value={"offset": 0, "items": [], "exhausted": True})
    monkeypatch.setattr("orpheus.harnesses.codex.driver.read_journal", journal)
    rpc = SimpleNamespace(
        history_dirty=False,
        notifications=[],
        sandbox=object(),
        call=AsyncMock(
            return_value={
                "thread": {
                    "path": "/history",
                    "turns": [{"id": "t", "status": "inProgress", "items": []}],
                }
            }
        ),
    )
    harness = Codex(cast(RPC, rpc), 100)
    await harness.snapshot("thread", None, 0)
    assert not harness.has_updates
    rpc.notifications.append(
        {
            "method": "item/started",
            "params": {
                "turnId": "t",
                "item": {
                    "id": "tool",
                    "type": "commandExecution",
                    "status": "inProgress",
                },
            },
        }
    )
    assert harness.has_updates
    snapshot = await harness.snapshot("thread", "/history", 0)
    assert snapshot.turns[0].items[0].status == "running"
    rpc.call.assert_awaited_once()
    journal.assert_awaited_once()
    rpc.notifications.append(
        {
            "method": "item/completed",
            "params": {
                "turnId": "t",
                "item": {
                    "id": "tool",
                    "type": "commandExecution",
                    "status": "completed",
                },
            },
        }
    )
    await harness.snapshot("thread", "/history", 0)
    rpc.call.assert_awaited_once()
    assert journal.await_count == 2
    harness.read_at = 0
    assert harness.has_updates
    await harness.snapshot("thread", "/history", 0)
    assert rpc.call.await_count == 2


@pytest.mark.parametrize("mode,expected", [("api_key", "api"), ("account", "chatgpt")])
async def test_codex_prepares_native_configuration(mode, expected):
    import tomllib

    from orpheus.configuration import Credentials
    from orpheus.harnesses import create_harness

    sandbox = SimpleNamespace(files=SimpleNamespace(write=AsyncMock()))
    harness = create_harness(
        "codex", cast(AsyncSandbox, sandbox), timeout=1, output_limit=100
    )
    env = await harness.prepare("/private/home", Credentials(mode=mode))
    assert env == {"CODEX_HOME": "/private/home"}
    path, raw = sandbox.files.write.call_args.args
    assert path == "/private/home/config.toml"
    assert tomllib.loads(raw) == {
        "cli_auth_credentials_store": "file",
        "forced_login_method": expected,
        "approval_policy": "never",
        "sandbox_mode": "danger-full-access",
    }


async def test_codex_initializes_api_key_only_when_needed(monkeypatch):
    from orpheus.configuration import Credentials
    from orpheus.errors import ExecutionError
    from orpheus.harnesses.codex.driver import Codex

    rpc = SimpleNamespace(initialize=AsyncMock(), call=AsyncMock())
    harness = Codex(cast(RPC, rpc), 100)
    source = Credentials(mode="api_key", api_key_env="TEST_HARNESS_KEY")
    monkeypatch.delenv("TEST_HARNESS_KEY", raising=False)
    await harness.initialize(source, login=False)
    rpc.call.assert_not_awaited()
    with pytest.raises(ExecutionError, match="credentials_unavailable"):
        await harness.initialize(source, login=True)
    monkeypatch.setenv("TEST_HARNESS_KEY", "fixture-key")
    await harness.initialize(source, login=True)
    rpc.call.assert_awaited_once_with(
        "account/login/start", {"type": "apiKey", "apiKey": "fixture-key"}
    )


async def test_codex_context_create_resume_and_missing_context():
    from orpheus.configuration import AgentConfiguration
    from orpheus.errors import ExecutionError
    from orpheus.harnesses.base import Context
    from orpheus.harnesses.codex.driver import Codex

    rpc = SimpleNamespace(
        call=AsyncMock(return_value={"thread": {"id": "t", "path": "/history"}})
    )
    harness = Codex(cast(RPC, rpc), 100)
    agent = AgentConfiguration(
        profile="test", model="test-model", instructions="Instructions"
    )
    assert await harness.open_context(agent, "/workspace") == Context("t", "/history")
    params = {
        "model": "test-model",
        "cwd": "/workspace",
        "approvalPolicy": "never",
        "sandbox": "danger-full-access",
        "baseInstructions": "Instructions",
    }
    rpc.call.assert_awaited_with("thread/start", params)
    assert await harness.open_context(agent, "/workspace", "t") == Context("t")
    rpc.call.assert_awaited_with("thread/resume", {**params, "threadId": "t"})
    rpc.call.side_effect = RPCError({"message": "private native error"})
    with pytest.raises(ExecutionError, match="context_lost") as exc:
        await harness.open_context(agent, "/workspace", "t")
    assert "private" not in str(exc.value)


@pytest.mark.parametrize("paths", [[], ["/one", "/two"], ["/only"]])
async def test_codex_recovery_discovers_only_unambiguous_native_history(
    paths, monkeypatch
):
    import shlex

    from orpheus.errors import ExecutionError
    from orpheus.harnesses.base import Snapshot
    from orpheus.harnesses.codex import offline

    sandbox = SimpleNamespace(
        commands=SimpleNamespace(
            run=AsyncMock(return_value=SimpleNamespace(stdout=json.dumps(paths)))
        )
    )
    reader = AsyncMock(return_value=Snapshot([], "/only", 42))
    monkeypatch.setattr(offline, "offline_snapshot", reader)
    if len(paths) == 1:
        snapshot = await offline.recover(
            cast(AsyncSandbox, sandbox), "ctx", None, "/native home", 123
        )
        assert snapshot.path == "/only" and snapshot.offset == 42
        reader.assert_awaited_once_with(sandbox, "/only", 123)
    else:
        with pytest.raises(ExecutionError, match="context_lost"):
            await offline.recover(
                cast(AsyncSandbox, sandbox), "ctx", None, "/native home", 123
            )
        reader.assert_not_awaited()
    assert shlex.split(sandbox.commands.run.call_args.args[0])[-2:] == [
        "/native home",
        "ctx",
    ]


@pytest.mark.parametrize("restart", [False, True])
@pytest.mark.parametrize("terminal", [False, True])
async def test_journal_output_arriving_after_thread_read_is_preserved(
    monkeypatch, restart, terminal
):
    from orpheus.harnesses.codex.driver import Codex

    item = {
        "id": "tool",
        "type": "commandExecution",
        "status": "completed",
        "command": "echo full",
        "aggregatedOutput": "abbreviated",
    }
    status = "completed" if terminal else "inProgress"
    rpc = SimpleNamespace(
        history_dirty=False,
        notifications=[],
        sandbox=object(),
        call=AsyncMock(
            side_effect=[
                {
                    "thread": {
                        "path": "/history",
                        "turns": [{"id": "turn", "status": "inProgress", "items": []}],
                    }
                },
                {
                    "thread": {
                        "path": "/history",
                        "turns": [{"id": "turn", "status": status, "items": [item]}],
                    }
                },
            ]
        ),
    )
    if terminal:
        rpc.notifications.append(
            {
                "method": "turn/completed",
                "params": {"turn": {"id": "turn", "status": "completed"}},
            }
        )

    async def journal(sandbox, path, offset, limit):
        if offset == 0:
            # Tool finishes after thread/read and notification snapshot, before
            # the read of the journal. This output has no item in this snapshot.
            return {
                "offset": 100,
                "exhausted": True,
                "items": [
                    {
                        "type": "output",
                        "id": "tool",
                        "result": {
                            "type": "text",
                            "text": "FULL OUTPUT",
                            "original_bytes": 11,
                        },
                        "completeness": "complete",
                        "reason": None,
                    }
                ],
            }
        return {"offset": offset, "exhausted": True, "items": []}

    monkeypatch.setattr("orpheus.harnesses.codex.driver.read_journal", journal)
    harness = Codex(cast(RPC, rpc), 1000)
    first = await harness.snapshot("thread", None, 0)
    assert first.turns[0].items == [] and first.turns[0].status == "running"
    assert first.offset == 0  # Safe to persist even if the worker now disappears.
    harness.committed()
    if restart:
        harness = Codex(cast(RPC, rpc), 1000)
    assert harness.has_updates
    second = await harness.snapshot("thread", first.path, first.offset)
    result = second.turns[0].items[0].result
    assert result is not None and result["text"] == "FULL OUTPUT"
    assert second.offset == 100
    assert second.turns[0].status == ("completed" if terminal else "running")


async def test_journal_completion_before_native_message_is_replayed_after_restart(
    monkeypatch,
):
    from orpheus.harnesses.codex.driver import Codex

    rpc = SimpleNamespace(
        history_dirty=False,
        notifications=[],
        sandbox=object(),
        call=AsyncMock(
            side_effect=[
                {
                    "thread": {
                        "path": "/history",
                        "turns": [{"id": "t", "status": "inProgress", "items": []}],
                    }
                },
                {
                    "thread": {
                        "path": "/history",
                        "turns": [
                            {
                                "id": "t",
                                "status": "inProgress",
                                "items": [
                                    {
                                        "id": "a",
                                        "type": "agentMessage",
                                        "text": "progress",
                                        "phase": "commentary",
                                    }
                                ],
                            }
                        ],
                    }
                },
            ]
        ),
    )

    async def journal(sandbox, path, offset, limit):
        return {
            "offset": 100,
            "exhausted": True,
            "items": [{"type": "completed", "id": "a", "turn_id": "t"}]
            if offset == 0
            else [],
        }

    monkeypatch.setattr("orpheus.harnesses.codex.driver.read_journal", journal)
    harness = Codex(cast(RPC, rpc), 1000)
    first = await harness.snapshot("thread", None, 0)
    assert first.offset == 0
    harness.committed()
    replacement = Codex(cast(RPC, rpc), 1000)
    second = await replacement.snapshot("thread", first.path, first.offset)
    assert second.offset == 100 and second.turns[0].items[0].text == "progress"


def assert_native_pair(pair, tmp_path):
    """Compare both projections of an actual app-server thread and its rollout."""
    from dataclasses import asdict

    from orpheus.harnesses.codex.items import normalize_thread

    path = tmp_path / "native.jsonl"
    raw = "".join(json.dumps(r) + "\n" for r in pair["records"])
    path.write_text(raw)
    journal = json.loads(
        subprocess.check_output(["python", "-c", READER, str(path), "0", "524288"])
    )
    assert journal["exhausted"] and journal["offset"] == len(raw.encode())
    outputs = {i["id"]: i for i in journal["items"] if i["type"] == "output"}
    completed = {i["id"] for i in journal["items"] if i["type"] == "completed"}
    data = json.loads(
        subprocess.check_output(["python", "-c", OFFLINE, str(path), "0", "524288"])
    )
    orders = {
        i["id"]: i["started_at_ms"] for i in journal["items"] if i["type"] == "order"
    }
    commands = {i["id"]: i["argv"] for i in journal["items"] if i["type"] == "command"}
    live = normalize_thread(
        pair["thread"], completed, outputs, 524288, orders, commands
    )
    offline = normalize_thread(data, set(data["completed"]), data["outputs"], 524288)
    if asdict(live) != asdict(offline):
        from pathlib import Path

        Path(".pytest_cache/native-pair-failure.json").write_text(json.dumps(pair))
        for observed_turn, recovered_turn in zip(
            live.turns, offline.turns, strict=True
        ):
            for observed, recovered in zip(
                observed_turn.items, recovered_turn.items, strict=True
            ):
                for field, value in asdict(observed).items():
                    assert value == asdict(recovered)[field], (
                        f"{observed.native_id}.{field}"
                    )
    assert asdict(live) == asdict(offline)
    return path, journal, live


@pytest.mark.parametrize("rename_ids", [False, True])
async def test_captured_native_pair_recovers_order_output_and_ignores_polling(
    tmp_path, monkeypatch, rename_ids
):
    from pathlib import Path

    from orpheus.harnesses.codex.driver import Codex

    pair = json.loads(Path("tests/fixtures/codex-native-history.json").read_text())
    assert pair["version"] == "codex-cli 0.155.1"
    commands = [
        i
        for t in pair["thread"]["turns"]
        for i in t["items"]
        if i["type"] == "commandExecution"
    ]
    assert len(commands) == 2
    if rename_ids:
        # Model call IDs remain call_..., native execution IDs are independent.
        for index, command in enumerate(commands):
            old, new = command["id"], f"exec_{index}"
            command["id"] = new
            for record in pair["records"]:
                item = record["payload"].get("item", {})
                if item.get("id") == old:
                    item["id"] = new
    path, journal, live = assert_native_pair(pair, tmp_path)
    assert [i.input for i in live.turns[0].items if i.type == "tool"] == [
        shlex.split(i["command"]) for i in commands
    ]
    results = [i for i in live.turns[0].items if i.type == "tool"]
    if not rename_ids:
        starts = [
            n["params"]["item"]["id"]
            for n in pair["notifications"]
            if n["method"] == "item/started"
        ]
        assert [(i.native_id, i.index) for i in live.turns[0].items] == [
            (i.native_id, starts.index(i.native_id)) for i in live.turns[0].items
        ]
    if rename_ids:
        # The long command has an explicit process ID linking model responses
        # to exec_*. The short command has no such link; keep its native output
        # but report the unavailable completeness honestly.
        assert (
            results[0].completeness == "truncated"
            and results[0].truncation_reason == "harness_limit"
        )
        assert results[1].completeness == "complete"
    else:
        assert all(
            i.completeness == "complete" and i.truncation_reason is None
            for i in results
        )
    assert {i.result["text"] for i in results if i.result} == {
        "ORPHEUS_FAST\n",
        "ORPHEUS_SLOW\n",
    }
    rpc = SimpleNamespace(
        history_dirty=False,
        notifications=[],
        sandbox=object(),
        call=AsyncMock(return_value={"thread": {**pair["thread"], "path": str(path)}}),
    )

    async def read(sandbox, file, offset, limit):
        return (
            journal
            if offset == 0
            else {"offset": offset, "exhausted": True, "items": []}
        )

    monkeypatch.setattr("orpheus.harnesses.codex.driver.read_journal", read)
    harness = Codex(cast(RPC, rpc), 524288)
    snapshot = await harness.snapshot("thread", str(path), 0)
    assert (
        snapshot.turns[0].status == "completed" and snapshot.offset == journal["offset"]
    )
    harness.committed()
    assert not harness.has_updates


def test_native_internal_items_are_not_tool_calls_or_cursor_barriers(tmp_path):
    from pathlib import Path

    pair = json.loads(Path("tests/fixtures/codex-native-history.json").read_text())
    tid = pair["thread"]["turns"][0]["id"]
    for kind in ("Extension", "FunctionCallOutput"):
        pair["records"].append(
            {
                "type": "event_msg",
                "payload": {
                    "type": "item_completed",
                    "turn_id": tid,
                    "item": {
                        "id": kind,
                        "type": kind,
                        "result": {"secret": "internal"},
                    },
                },
            }
        )
    _path, journal, snapshot = assert_native_pair(pair, tmp_path)
    assert not any(
        i["id"] in ("Extension", "FunctionCallOutput") for i in journal["items"]
    )
    assert all(
        i.name not in ("extension", "functionCallOutput")
        for t in snapshot.turns
        for i in t.items
    )


def test_simultaneous_native_starts_have_a_stable_order(tmp_path):
    from pathlib import Path

    pair = json.loads(Path("tests/fixtures/codex-native-history.json").read_text())
    for record in pair["records"]:
        if record["payload"].get("type") == "item_completed":
            record["payload"]["started_at_ms"] = 1
    pair["thread"]["turns"][0]["items"].reverse()
    assert_native_pair(pair, tmp_path)


def test_captured_streamed_command_recovers_more_than_native_tail(tmp_path):
    from pathlib import Path

    pair = json.loads(Path("tests/fixtures/codex-streamed-output.json").read_text())
    _path, _journal, snapshot = assert_native_pair(pair, tmp_path)
    tool = next(i for i in snapshot.turns[0].items if i.type == "tool")
    assert (
        tool.result
        and "tool-start" in tool.result["text"]
        and "tool-end" in tool.result["text"]
    )
    assert tool.completeness == "complete"


def test_captured_file_change_preserves_input_and_output(tmp_path):
    from pathlib import Path

    pair = json.loads(Path("tests/fixtures/codex-file-change.json").read_text())
    _path, _journal, snapshot = assert_native_pair(pair, tmp_path)
    tool = next(i for i in snapshot.turns[0].items if i.type == "tool")
    assert tool.input == [
        {
            "path": "/home/user/workspace/orpheus-probe.txt",
            "kind": {"type": "add"},
            "diff": "ORPHEUS_OK\n",
        }
    ]
    assert tool.result and "Success" in tool.result["value"]["stdout"]
    assert tool.completeness == "complete"


def test_file_change_projection_preserves_delete_update_and_move(tmp_path):
    from pathlib import Path

    pair = json.loads(Path("tests/fixtures/codex-file-change.json").read_text())
    changes = {
        "/workspace/deleted": {"type": "delete", "content": "old\n"},
        "/workspace/updated": {
            "type": "update",
            "unified_diff": "-old\n+new\n",
            "move_path": None,
        },
        "/workspace/moved": {
            "type": "update",
            "unified_diff": "-old\n+new\n",
            "move_path": "/workspace/new",
        },
    }
    expected = [
        {"path": "/workspace/deleted", "kind": {"type": "delete"}, "diff": "old\n"},
        {
            "path": "/workspace/updated",
            "kind": {"type": "update", "move_path": None},
            "diff": "-old\n+new\n",
        },
        {
            "path": "/workspace/moved",
            "kind": {"type": "update", "move_path": "/workspace/new"},
            "diff": "-old\n+new\n",
        },
    ]
    for item in pair["thread"]["turns"][0]["items"]:
        if item["type"] == "fileChange":
            item["changes"] = expected
    for record in pair["records"]:
        item = record["payload"].get("item", {})
        if item.get("type") == "FileChange":
            item["changes"] = changes
    _path, _journal, snapshot = assert_native_pair(pair, tmp_path)
    tool = next(i for i in snapshot.turns[0].items if i.type == "tool")
    assert tool.input == [expected[0], expected[2], expected[1]]


@pytest.mark.parametrize("limit", [524288, 15])
def test_poll_chunks_merge_with_initial_output_without_control_call_results(
    tmp_path, limit
):
    from pathlib import Path

    pair = json.loads(Path("tests/fixtures/codex-native-history.json").read_text())
    for r in pair["records"]:
        p = r["payload"]
        if p.get(
            "type"
        ) == "function_call_output" and "Process running with session ID" in p.get(
            "output", ""
        ):
            p["output"] += "BEGIN\n"
    path = tmp_path / "chunks.jsonl"
    path.write_text("".join(json.dumps(r) + "\n" for r in pair["records"]))
    data = json.loads(
        subprocess.check_output(["python", "-c", OFFLINE, str(path), "0", str(limit)])
    )
    command = next(
        i["id"]
        for t in pair["thread"]["turns"]
        for i in t["items"]
        if i.get("command", "").find("sleep 2") >= 0
    )
    result = data["outputs"][command]
    if limit == 524288:
        assert result["result"]["text"] == "BEGIN\nORPHEUS_SLOW\n"
        assert result["completeness"] == "complete"
    else:
        assert result["reason"] == "orpheus_limit"
        assert result["result"]["original_bytes"] == len(b"BEGIN\nORPHEUS_SLOW\n")
        assert (
            len(result["result"]["head"].encode())
            + len(result["result"]["tail"].encode())
            <= limit
        )


def test_sandbox_reader_does_not_require_orchestrator_python_version():
    import ast

    ast.parse(READER, feature_version=(3, 10))
    ast.parse(OFFLINE, feature_version=(3, 10))


@pytest.fixture
def local_native_rpc():
    import copy

    thread = {"turns": []}

    async def call(*args, **kwargs):
        return {"thread": copy.deepcopy(thread)}

    async def run(command, **kwargs):
        result = await asyncio.to_thread(
            subprocess.check_output, shlex.split(command), text=True
        )
        return SimpleNamespace(stdout=result)

    rpc = SimpleNamespace(
        history_dirty=False,
        notifications=[],
        call=AsyncMock(side_effect=call),
        sandbox=SimpleNamespace(
            commands=SimpleNamespace(run=AsyncMock(side_effect=run))
        ),
    )
    return rpc, thread


@pytest.mark.parametrize("status", ["completed", "interrupted"])
@pytest.mark.parametrize("omission", ["tool", "message", "turn"])
async def test_persistent_native_omission_recovers_without_waiting_for_deadline(
    tmp_path, local_native_rpc, status, omission
):
    from pathlib import Path

    from orpheus.harnesses.codex.driver import Codex

    pair = json.loads(Path("tests/fixtures/codex-native-history.json").read_text())
    pair["thread"]["turns"][0]["status"] = status
    if status == "interrupted":
        for record in pair["records"]:
            if record["payload"].get("type") == "task_complete":
                record["payload"]["type"] = "turn_aborted"
    path, _journal, expected = assert_native_pair(pair, tmp_path)
    rpc, thread = local_native_rpc
    thread.update(pair["thread"], path=str(path))
    if omission == "turn":
        thread["turns"] = []
    else:
        kind = "commandExecution" if omission == "tool" else "agentMessage"
        thread["turns"][0]["items"] = [
            i for i in thread["turns"][0]["items"] if i["type"] != kind
        ]
    harness = Codex(cast(RPC, rpc), 524288)
    for _ in range(2):
        snapshot = await harness.snapshot("thread", str(path), 0)
        assert snapshot.offset == 0
        assert all(t.status == "running" for t in snapshot.turns)
        harness.committed()
    snapshot = await harness.snapshot("thread", str(path), 0)
    assert snapshot.offset == path.stat().st_size
    assert snapshot.turns == expected.turns
    harness.committed()
    # A later app-server refresh still omits these elements. Keep the recovered
    # identities/order without another fallback scan or another cursor barrier.
    harness.read_at = 0
    later = await harness.snapshot("thread", str(path), snapshot.offset)
    assert later.offset == snapshot.offset
    assert [
        (t.native_id, t.status, [(i.native_id, i.index) for i in t.items])
        for t in later.turns
    ] == [
        (t.native_id, t.status, [(i.native_id, i.index) for i in t.items])
        for t in expected.turns
    ]
    assert (
        sum(
            "\noffline()\n" in c.args[0]
            for c in rpc.sandbox.commands.run.call_args_list
        )
        == 1
    )


async def test_reconnect_indexes_committed_history_without_replaying_results(
    tmp_path, local_native_rpc
):
    from orpheus.harnesses.codex.driver import Codex

    path = tmp_path / "long.jsonl"
    records = [
        {"type": "event_msg", "payload": {"type": "task_started", "turn_id": "t"}}
    ]
    native_items = []
    for n in range(400):
        records.append(
            {
                "type": "event_msg",
                "payload": {
                    "type": "item_completed",
                    "turn_id": "t",
                    "started_at_ms": n,
                    "item": {
                        "id": str(n),
                        "type": "CommandExecution",
                        "command": ["echo", "$HOME"],
                        "status": "completed",
                        "aggregated_output": "x" * 8192,
                    },
                },
            }
        )
        native_items.append(
            {
                "id": str(n),
                "type": "commandExecution",
                "command": 'echo "$HOME"',
                "status": "completed",
                "aggregatedOutput": "tail",
            }
        )
    records.append(
        {"type": "event_msg", "payload": {"type": "task_complete", "turn_id": "t"}}
    )
    path.write_text("".join(json.dumps(r) + "\n" for r in records))
    offset = path.stat().st_size
    rpc, thread = local_native_rpc
    thread.update(
        path=str(path),
        turns=[
            {"id": "t", "status": "completed", "items": list(reversed(native_items))}
        ],
    )
    harness = Codex(cast(RPC, rpc), 100)
    snapshot = await harness.snapshot("thread", str(path), offset)
    assert snapshot.offset == offset and snapshot.turns[0].status == "completed"
    assert [i.native_id for i in snapshot.turns[0].items] == [
        str(i) for i in range(400)
    ]
    assert all(i.input == ["echo", "$HOME"] for i in snapshot.turns[0].items)
    assert not harness.outputs
    calls = rpc.sandbox.commands.run.call_args_list
    assert len(calls) == 2  # One metadata pass, one incremental EOF check.
    assert shlex.split(calls[0].args[0])[-1] == str(offset)
    assert shlex.split(calls[1].args[0])[-2] == str(offset)
    harness.committed()
    harness.read_at = 0
    await harness.snapshot("thread", str(path), offset)
    assert rpc.sandbox.commands.run.await_count == 3  # No second metadata pass.


@pytest.mark.parametrize("display", ['bash -c "echo \\$HOME"', "bash -c 'unterminated"])
def test_command_input_uses_native_argv_when_display_is_lossy(tmp_path, display):
    from pathlib import Path

    pair = json.loads(Path("tests/fixtures/codex-native-history.json").read_text())
    commands = [
        i
        for i in pair["thread"]["turns"][0]["items"]
        if i["type"] == "commandExecution"
    ]
    ident = commands[0]["id"]
    commands[0]["command"] = display
    for record in pair["records"]:
        item = record["payload"].get("item", {})
        if item.get("id") == ident:
            item["command"] = ["bash", "-c", "echo $HOME"]
    _path, _journal, snapshot = assert_native_pair(pair, tmp_path)
    assert next(i for i in snapshot.turns[0].items if i.native_id == ident).input == [
        "bash",
        "-c",
        "echo $HOME",
    ]


def test_invalid_command_display_does_not_break_snapshot():
    from orpheus.harnesses.codex.items import normalize_thread

    command = "bash -c 'unterminated"
    snapshot = normalize_thread(
        {
            "turns": [
                {
                    "id": "t",
                    "status": "inProgress",
                    "items": [
                        {
                            "id": "c",
                            "type": "commandExecution",
                            "command": command,
                            "status": "inProgress",
                        }
                    ],
                }
            ]
        },
        set(),
        {},
        100,
    )
    assert snapshot.turns[0].items[0].input == command
