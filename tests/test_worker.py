import asyncio
import copy
import json
import os
from datetime import timedelta
from pathlib import Path
from types import SimpleNamespace
from typing import Any, ClassVar, cast
from unittest.mock import AsyncMock, Mock
from uuid import UUID, uuid4

import pytest
from agentbox import AsyncSandbox
from agentbox.sandbox.sandbox_api import SandboxQuery
from agentbox.sandbox_async.filesystem.filesystem import Filesystem
from conftest import session_body
from sqlalchemy import func, select, text

from orpheus import schemas
from orpheus.errors import UncertainError
from orpheus.harnesses.base import Item, Snapshot, Turn
from orpheus.harnesses.codex.driver import Codex
from orpheus.harnesses.codex.rpc import RPC
from orpheus.models import TERMINAL, MessageRecord, now
from orpheus.reconciliation import reconcile
from orpheus.store import WORKER_LOCK, accept, cancel, get_run, get_session
from orpheus.worker import SessionExecutor


class FakeRPC:
    def __init__(self, sandbox, timeout):
        self.sandbox = sandbox
        self.bytes_seen = 0

    async def launch(self, env, cwd):
        self.sandbox.pid += 1
        self.sandbox.processes.append(SimpleNamespace(pid=self.sandbox.pid, envs=env))
        return self.sandbox.pid

    async def attach(self, pid):
        pass

    async def initialize(self):
        pass

    async def call(self, method, params):
        if method == "thread/start":
            return {"thread": {"id": "thread", "path": "/history"}}
        return {}

    async def close(self):
        pass


class FakeHarness(Codex):
    rpc: Any
    has_updates = True
    needs_reconnect = False

    def committed(self):
        pass

    def __init__(self, rpc, limit):
        super().__init__(rpc, limit)

    async def start(self, thread_id, text):
        sb = self.rpc.sandbox
        sb.starts += 1
        turn = Turn(
            f"turn-{sb.starts}",
            "running",
            [Item(f"user-{sb.starts}", "user", 0, text=text)],
        )
        sb.turns.append(turn)
        if sb.lose_send:
            sb.lose_send = False
            raise UncertainError
        return turn.native_id

    async def steer(self, thread_id, turn_id, text):
        turn = self.rpc.sandbox.turns[-1]
        turn.items.append(
            Item(f"steer-{len(turn.items)}", "user", len(turn.items), text=text)
        )

    async def snapshot(self, thread_id, history_path, offset):
        return Snapshot(copy.deepcopy(self.rpc.sandbox.turns), "/history", 0)

    async def interrupt(self, thread_id, turn_id):
        if not self.rpc.sandbox.ignore_cancel:
            self.rpc.sandbox.turns[-1].status = "cancelled"

    async def close(self):
        pass


@pytest.fixture
def remote(monkeypatch):
    class Sandbox:
        objects: ClassVar[dict] = {}
        creates = 0
        lose_create = False

        def __init__(self):
            self.metadata = {}
            self.sandbox_id = str(uuid4())
            self.state = "running"
            self.processes = []
            self.pid = 100
            self.turns = []
            self.starts = 0
            self.lose_send = False
            self.ignore_cancel = False
            self.files = SimpleNamespace(write=AsyncMock())
            self.commands = SimpleNamespace(
                list=self.list_processes,
                kill=self.kill_process,
                run=AsyncMock(
                    return_value=SimpleNamespace(
                        stdout=json.dumps(
                            ["/home/user/workspace", "/home/user/.orpheus-codex"]
                        )
                    )
                ),
            )

        @classmethod
        async def create(cls, **kwargs):
            cls.creates += 1
            sb = cls()
            sb.metadata = kwargs["metadata"]
            cls.objects[sb.sandbox_id] = sb
            if cls.lose_create:
                cls.lose_create = False
                raise TimeoutError
            return sb

        @classmethod
        async def connect(cls, sid, **kwargs):
            sb = cls.objects[sid]
            sb.state = "running"
            return sb

        @classmethod
        async def get_info(cls, sid):
            return SimpleNamespace(state=SimpleNamespace(value=cls.objects[sid].state))

        @classmethod
        def list(cls, query):
            found = [
                SimpleNamespace(sandbox_id=sb.sandbox_id)
                for sb in cls.objects.values()
                if sb.metadata == query.metadata
            ]

            class Pages:
                has_next = True

                async def next_items(self):
                    self.has_next = False
                    return found

            return Pages()

        async def set_timeout(self, *args):
            pass

        async def pause(self, **kwargs):
            self.state = "paused"

        async def list_processes(self):
            return self.processes

        async def kill_process(self, pid):
            self.processes = [p for p in self.processes if p.pid != pid]

    monkeypatch.setattr("orpheus.worker.AsyncSandbox", Sandbox)
    monkeypatch.setattr("orpheus.harnesses.codex.rpc.RPC", FakeRPC)
    monkeypatch.setattr("orpheus.harnesses.codex.driver.Codex", FakeHarness)
    monkeypatch.setenv("CODEX_API_KEY", "fixture-only")
    return Sandbox


async def admitted(database, settings):
    profiles, cipher = settings.runtime()
    async with database() as db:
        ids = await accept(
            db,
            settings,
            profiles,
            cipher,
            schemas.CreateSession.model_validate(session_body()),
            uuid4(),
        )
    return ids, SessionExecutor(ids.session_id, database, settings, cipher)


async def test_worker_full_cycle_pause_resume_context(database, settings, remote):
    ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    assert (
        sb.starts == 1
        and sb.processes[0].envs["CODEX_HOME"] == "/home/user/.orpheus-codex"
    )
    sb.turns[-1].items.append(
        Item("answer-1", "assistant", 1, text="done", kind="answer")
    )
    sb.turns[-1].status = "completed"
    await worker.tick()
    await worker.tick()
    async with database() as db:
        session = await get_session(db, ids.session_id)
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.status == "completed" and run.final_message_id
        assert session.sandbox_state == "paused" and not session.slot_reserved
    profiles, cipher = settings.runtime()
    async with database() as db:
        new = await accept(
            db,
            settings,
            profiles,
            cipher,
            schemas.SendMessage(message=schemas.TextMessage(text="next")),
            uuid4(),
            sid=ids.session_id,
        )
    replacement = SessionExecutor(ids.session_id, database, settings, cipher)
    await replacement.tick()
    assert remote.creates == 1 and sb.starts == 2 and len(sb.processes) == 1
    _, run = await replacement.read()
    assert run is not None and run.id == new.run_id
    await replacement.disconnect()


async def test_lost_create_and_send_reconcile_without_repeating(
    database, settings, remote
):
    ids, worker = await admitted(database, settings)
    remote.lose_create = True
    with pytest.raises(UncertainError):
        await worker.tick()
    await worker.disconnect()
    worker = SessionExecutor(ids.session_id, database, settings, settings.runtime()[1])
    sb = next(iter(remote.objects.values()))
    sb.lose_send = True
    with pytest.raises(UncertainError):
        await worker.tick()
    await worker.disconnect()
    worker = SessionExecutor(ids.session_id, database, settings, settings.runtime()[1])
    await worker.tick()
    assert remote.creates == 1 and sb.starts == 1
    async with database() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        message = await db.get(MessageRecord, ids.message_id)
        assert run.status == "running" and run.native_turn_id == "turn-1"
        assert message.delivery_status == "delivered"
    await worker.disconnect()


async def test_restart_after_harness_finished_imports_answer_once(
    database, settings, remote
):
    ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    await worker.disconnect()
    sb.turns[-1].items.append(
        Item("answer", "assistant", 1, text="result", kind="answer")
    )
    sb.turns[-1].status = "completed"
    fresh = SessionExecutor(ids.session_id, database, settings, settings.runtime()[1])
    await fresh.tick()
    async with database.begin() as db:
        session = await get_session(db, ids.session_id, lock=True)
        await reconcile(db, session, Snapshot(sb.turns, "/history", 0))
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.status == "completed" and run.final_message_id
        assert (
            await db.scalar(
                select(func.count())
                .select_from(MessageRecord)
                .where(MessageRecord.role == "assistant")
            )
            == 1
        )
    await fresh.disconnect()


async def test_cancel_before_execution_and_running_cancel(database, settings, remote):
    ids, worker = await admitted(database, settings)
    async with database() as db:
        result = await cancel(db, ids.session_id, ids.run_id)
    assert result.status == "cancelled"
    await worker.tick()
    assert remote.creates == 0
    ids, worker = await admitted(database, settings)
    await worker.tick()
    async with database() as db:
        await cancel(db, ids.session_id, ids.run_id)
    await worker.tick()
    await worker.tick()
    await worker.tick()
    async with database() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.status == "cancelled" and run.stop_reason == "user_request"
    await worker.disconnect()


async def test_deadline_survives_restart(database, settings, remote):
    ids, worker = await admitted(database, settings)
    await worker.tick()
    async with database.begin() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        run.deadline_at = now() - timedelta(seconds=1)
    await worker.disconnect()
    fresh = SessionExecutor(ids.session_id, database, settings, settings.runtime()[1])
    await fresh.tick()
    await fresh.tick()
    async with database() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.status == "cancelled" and run.stop_reason == "run_timeout"
    await fresh.disconnect()


async def test_reserve_is_kept_for_next_run_during_pause(database, settings, remote):
    ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    sb.turns[-1].status = "completed"
    await worker.tick()
    profiles, cipher = settings.runtime()
    async with database() as db:
        await accept(
            db,
            settings,
            profiles,
            cipher,
            schemas.SendMessage(message=schemas.TextMessage(text="next")),
            uuid4(),
            sid=ids.session_id,
        )
    record, _ = await worker.read()
    await worker.pause(record)
    assert (await worker.read())[0].slot_reserved


async def test_second_worker_cannot_own_database_lock(database):
    engine = database.kw["bind"]
    async with engine.connect() as one, engine.connect() as two:
        assert await one.scalar(
            text("SELECT pg_try_advisory_lock(:k)"), {"k": WORKER_LOCK}
        )
        assert not await two.scalar(
            text("SELECT pg_try_advisory_lock(:k)"), {"k": WORKER_LOCK}
        )
        await one.execute(text("SELECT pg_advisory_unlock(:k)"), {"k": WORKER_LOCK})


async def test_same_text_steers_stay_distinct(database, settings, remote):
    ids, worker = await admitted(database, settings)
    await worker.tick()
    profiles, cipher = settings.runtime()
    for _ in range(2):
        async with database() as db:
            await accept(
                db,
                settings,
                profiles,
                cipher,
                schemas.SendMessage(message=schemas.TextMessage(text="same")),
                uuid4(),
                sid=ids.session_id,
                rid=ids.run_id,
            )
        await worker.tick()
    await worker.tick()
    async with database() as db:
        rows = list(
            await db.scalars(select(MessageRecord).where(MessageRecord.text == "same"))
        )
        assert len(rows) == 2 and len({m.native_key for m in rows}) == 2
        assert all(m.delivery_status == "delivered" for m in rows)
    await worker.disconnect()


async def test_empty_proxy_disables_injection(database, settings, remote):
    from pydantic import SecretStr

    settings.sandbox_proxy_url = SecretStr("")
    _ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    assert "ALL_PROXY" not in sb.processes[0].envs
    assert "NO_PROXY" not in sb.processes[0].envs
    await worker.disconnect()


async def test_custom_proxy_is_common_process_environment(database, settings, remote):
    from pydantic import SecretStr

    settings.sandbox_proxy_url = SecretStr("socks5h://proxy.example:1080")
    _ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    assert sb.processes[0].envs["ALL_PROXY"] == "socks5h://proxy.example:1080"
    await worker.disconnect()


async def test_force_stop_only_targets_harness_and_next_run_restores_context(
    database, settings, remote, monkeypatch
):
    ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    sb.ignore_cancel = True
    sb.processes.append(SimpleNamespace(pid=999, envs={"BACKGROUND": "1"}))
    async with database() as db:
        await cancel(db, ids.session_id, ids.run_id)
    await worker.tick()
    async with database.begin() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        run.cancel_attempted_at = now() - timedelta(seconds=1)
    monkeypatch.setattr(
        "orpheus.harnesses.codex.offline.offline_snapshot",
        AsyncMock(return_value=Snapshot(copy.deepcopy(sb.turns), "/history", 0)),
    )
    await worker.tick()
    assert [p.pid for p in sb.processes] == [999]
    await worker.tick()
    await worker.tick()
    async with database() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.status == "cancelled" and run.stop_method == "forced"
    profiles, cipher = settings.runtime()
    async with database() as db:
        await accept(
            db,
            settings,
            profiles,
            cipher,
            schemas.SendMessage(message=schemas.TextMessage(text="continue")),
            uuid4(),
            sid=ids.session_id,
        )
    await worker.tick()
    assert len(sb.processes) == 2 and sb.processes[0].pid == 999
    await worker.disconnect()


async def test_account_repair_survives_intervening_preparation_failure(
    database, settings
):
    from orpheus.errors import Error
    from orpheus.store import finish

    ids, worker = await admitted(database, settings)
    async with database.begin() as db:
        session = await get_session(db, ids.session_id, lock=True)
        run = await get_run(db, ids.session_id, ids.run_id)
        run.execution_started_at = now()
        await finish(
            db,
            session,
            run,
            "failed",
            Error(code="authentication_failed", message="Auth failed"),
        )
    profiles, cipher = settings.runtime()
    async with database() as db:
        second = await accept(
            db,
            settings,
            profiles,
            cipher,
            schemas.SendMessage(message=schemas.TextMessage(text="repair")),
            uuid4(),
            sid=ids.session_id,
        )
    async with database.begin() as db:
        session = await get_session(db, ids.session_id, lock=True)
        run = await get_run(db, ids.session_id, second.run_id)
        await finish(
            db,
            session,
            run,
            "failed",
            Error(code="credentials_unavailable", message="S3 down"),
        )
    assert await worker.auth_failed_before(run, include_current=True)
    async with database() as db:
        third = await accept(
            db,
            settings,
            profiles,
            cipher,
            schemas.SendMessage(message=schemas.TextMessage(text="repair again")),
            uuid4(),
            sid=ids.session_id,
        )
        current = await get_run(db, ids.session_id, third.run_id)
    assert await worker.auth_failed_before(current)


async def test_cancel_uncertain_turn_forces_known_harness(
    database, settings, remote, monkeypatch
):
    ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    sb.turns.clear()  # Neither response nor native history confirms delivery.
    async with database.begin() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        run.native_turn_id = None
    async with database() as db:
        await cancel(db, ids.session_id, ids.run_id)
    await worker.tick()
    async with database.begin() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.cancel_attempted_at
        run.cancel_attempted_at = now() - timedelta(seconds=1)
    monkeypatch.setattr(
        "orpheus.harnesses.codex.offline.offline_snapshot",
        AsyncMock(return_value=Snapshot([], "/history", 0)),
    )
    await worker.tick()
    assert not sb.processes
    await worker.disconnect()


async def test_missing_sandbox_before_pause_releases_capacity(
    database, settings, remote, monkeypatch
):
    from agentbox import SandboxNotFoundException

    _ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    sb.turns[-1].status = "completed"
    await worker.tick()
    await worker.disconnect()
    monkeypatch.setattr(
        remote, "get_info", AsyncMock(side_effect=SandboxNotFoundException("gone"))
    )
    await worker.tick()
    record, run = await worker.read()
    assert run is None and record.sandbox_state == "unavailable"
    assert record.sandbox_error["code"] == "sandbox_lost"
    assert not record.slot_reserved


async def test_unrecognized_account_never_starts_task_or_uploads_credentials(
    database, settings, remote, config_file, monkeypatch
):
    from unittest.mock import Mock

    from orpheus.errors import ExecutionError

    config_file.write_text("""
[credential_stores.main]
type = "s3"
bucket = "fixture"
[profiles.default]
harness = "codex"
model = "fixture-model"
[profiles.default.auth]
mode = "account"
store = "main"
key = "auth.json"
""")
    account = SimpleNamespace(
        seed=AsyncMock(), watch=AsyncMock(), sync=AsyncMock(), close=AsyncMock()
    )
    monkeypatch.setattr(
        "orpheus.harnesses.codex.driver.AccountCredentials", Mock(return_value=account)
    )
    ids, worker = await admitted(database, settings)
    with pytest.raises(ExecutionError) as exc:
        await worker.tick()
    assert exc.value.code == "authentication_failed"
    await worker.failure(exc.value)
    await worker.tick()
    account.seed.assert_awaited_once()
    account.sync.assert_not_awaited()
    sb = next(iter(remote.objects.values()))
    assert sb.starts == 0 and sb.state == "paused"
    async with database() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.error and run.error["code"] == "authentication_failed"


@pytest.mark.live
async def test_real_codex_pause_resume_and_worker_reconnect(
    api, database, settings, monkeypatch, required_env, tmp_path
):
    required_env("AGENTBOX_API_KEY")
    fixture_model = os.environ.get("ORPHEUS_LIVE_FIXTURE") == "1"
    monkeypatch.setenv(
        "CODEX_API_KEY",
        "fixture-key" if fixture_model else required_env("OPENAI_API_KEY"),
    )
    if fixture_model:
        original_prepare = SessionExecutor.prepare_paths
        original_write = Filesystem.write

        async def prepare(self, record):
            await original_prepare(self, record)
            found = await self.sandbox.commands.run(
                "test -f /tmp/orpheus-fixture.py && echo present || true", user="user"
            )
            if not found.stdout.strip():
                await self.sandbox.files.write(
                    "/tmp/orpheus-fixture.py",
                    Path("tests/fixtures/model_server.py").read_text(),
                    user="user",
                )
                handle = await self.sandbox.commands.run(
                    "python3 /tmp/orpheus-fixture.py",
                    user="user",
                    background=True,
                    timeout=0,
                )
                await handle.disconnect()

        async def write(self, path, data, *args, **kwargs):
            if path.endswith("/config.toml"):
                data += '\nmodel_provider = "fixture"\nweb_search = "disabled"\n[model_providers.fixture]\nname="Protocol fixture"\nbase_url="http://127.0.0.1:8765/v1"\nwire_api="responses"\nrequires_openai_auth=false\n'
            return await original_write(self, path, data, *args, **kwargs)

        monkeypatch.setattr(SessionExecutor, "prepare_paths", prepare)
        monkeypatch.setattr(Filesystem, "write", write)
    if os.environ.get("ORPHEUS_LIVE_FAULTS") == "1":
        # Drop each acknowledgement once, after the real external action succeeded.
        original_create, original_launch, original_start = (
            AsyncSandbox.create,
            RPC.launch,
            Codex.start,
        )
        dropped = set()

        async def create(*args, **kwargs):
            result = await original_create(*args, **kwargs)
            if "create" not in dropped:
                dropped.add("create")
                raise TimeoutError("Injected lost create response")
            return result

        async def launch(self, *args, **kwargs):
            result = await original_launch(self, *args, **kwargs)
            if "launch" not in dropped:
                dropped.add("launch")
                raise UncertainError
            return result

        async def start(self, *args, **kwargs):
            result = await original_start(self, *args, **kwargs)
            if "start" not in dropped:
                dropped.add("start")
                raise UncertainError
            return result

        monkeypatch.setattr(AsyncSandbox, "create", create)
        monkeypatch.setattr(RPC, "launch", launch)
        monkeypatch.setattr(Codex, "start", start)
    settings.rpc_timeout_seconds = 30
    settings.worker_poll_seconds = 1
    body = session_body(
        "Create a file named orpheus-probe.txt in the current directory containing exactly ORPHEUS_OK. Then reply with exactly CREATED. Do not access other directories or networks."
    )
    body["configuration"]["agent"]["model"] = os.environ.get(
        "ORPHEUS_TEST_MODEL", "gpt-5.4"
    )
    body["configuration"]["limits"] = {"run_timeout_seconds": 120}
    response = await api.post(
        "/api/v1/sessions", json=body, headers={"Idempotency-Key": str(uuid4())}
    )
    assert response.status_code == 202, response.text
    ids = response.json()
    sid = UUID(ids["session_id"])
    worker = SessionExecutor(sid, database, settings, settings.runtime()[1])
    known = set()

    async def tick():
        try:
            await worker.tick()
        except UncertainError:
            await worker.disconnect()

    async def until_terminal():
        async with asyncio.timeout(180):
            while True:
                await tick()
                view = (await api.get(f"/api/v1/sessions/{sid}")).json()
                if view["status"] in TERMINAL:
                    if view["status"] != "completed" and isinstance(
                        worker.harness, Codex
                    ):
                        native = (
                            await worker.harness.rpc.call(
                                "thread/read",
                                {
                                    "threadId": (await worker.read())[0].thread_id,
                                    "includeTurns": True,
                                },
                            )
                        )["thread"]
                        diagnostic = json.dumps(native["turns"][-1].get("error"))
                        for key in ("OPENAI_API_KEY", "AGENTBOX_API_KEY"):
                            diagnostic = diagnostic.replace(
                                os.environ.get(key, "unused-secret"), "[redacted]"
                            )
                        print("Native failure:", diagnostic)
                    assert view["status"] == "completed", view.get("error")
                    assert view["final_message"] is not None
                    return view
                await asyncio.sleep(1)

    try:
        await tick()
        async with database() as db:
            record = await get_session(db, sid)
            known.add(record.sandbox_id)
        # Drop all local transport/harness state while Codex continues remotely.
        await worker.disconnect()
        worker = SessionExecutor(sid, database, settings, settings.runtime()[1])
        first = await until_terminal()
        assert "CREATED" in first["final_message"]["text"]
        from orpheus.harnesses.codex.offline import offline_snapshot

        record, _ = await worker.read()
        assert worker.sandbox is not None and record.history_path
        offline = await offline_snapshot(
            worker.sandbox, record.history_path, settings.max_tool_result_bytes
        )

        def positions(snapshot):
            return [
                (
                    turn.native_id,
                    [
                        (item.native_id, item.type, item.index, item.text, item.kind)
                        for item in turn.items
                    ],
                )
                for turn in snapshot.turns
            ]

        from test_harness import assert_native_pair

        assert isinstance(worker.harness, Codex)
        thread = (
            await worker.harness.rpc.call(
                "thread/read", {"threadId": record.thread_id, "includeTurns": True}
            )
        )["thread"]
        raw = await worker.sandbox.files.read(record.history_path, user="user")
        pair = {
            "thread": thread,
            "records": [json.loads(line) for line in raw.splitlines()],
        }
        assert_native_pair(pair, tmp_path)
        assert positions(offline) == positions(worker.snapshot)
        await worker.tick()
        async with database() as db:
            record = await get_session(db, sid)
            assert record.sandbox_state == "paused" and not record.slot_reserved
            first_pid = record.process_id
            known.add(record.sandbox_id)
        response = await api.post(
            f"/api/v1/sessions/{sid}/runs",
            json={
                "message": {
                    "text": "Read orpheus-probe.txt from the current directory and reply with its exact contents. Do not access other directories or networks."
                }
            },
            headers={"Idempotency-Key": str(uuid4())},
        )
        assert response.status_code == 202
        second = await until_terminal()
        assert "ORPHEUS_OK" in second["final_message"]["text"]
        await worker.tick()
        async with database() as db:
            record = await get_session(db, sid)
            assert record.sandbox_id in known and record.process_id == first_pid
        history = (await api.get(f"/api/v1/sessions/{sid}/history")).json()
        tools = [
            item["tool_call"]
            for item in history["items"]
            if item["type"] == "tool_call"
        ]
        assert tools
        assert all(item["output_completeness"] == "complete" for item in tools)
        assert any("ORPHEUS_OK" in json.dumps(item["result"]) for item in tools)
    finally:
        await worker.disconnect()
        paginator = AsyncSandbox.list(
            query=SandboxQuery(metadata={"orpheus_session_id": str(sid)})
        )
        while paginator.has_next:
            for sandbox in await paginator.next_items():
                known.add(sandbox.sandbox_id)
        for sandbox_id in known:
            if sandbox_id:
                await AsyncSandbox.kill(sandbox_id)


@pytest.mark.parametrize(
    "failure", ["missing_template", "billing", "missing_python", "missing_user"]
)
async def test_confirmed_preparation_failure_finishes_and_releases_slot(
    database, settings, remote, monkeypatch, failure
):
    from agentbox.exceptions import InvalidArgumentException, SandboxException
    from agentbox.sandbox.commands.command_handle import CommandExitException

    if failure in ("missing_template", "billing"):
        error = SandboxException(
            "404: private template"
            if failure == "missing_template"
            else "403: billing:missing_payment_method"
        )
        monkeypatch.setattr(remote, "create", AsyncMock(side_effect=error))
    ids, worker = await admitted(database, settings)
    if failure not in ("missing_template", "billing"):
        sb = await remote.create(metadata={})
        monkeypatch.setattr(remote, "create", AsyncMock(return_value=sb))
        error = (
            CommandExitException(
                stderr="private details", stdout="", exit_code=127, error=None
            )
            if failure == "missing_python"
            else InvalidArgumentException("user absent")
        )
        sb.commands.run.side_effect = error
    async with asyncio.timeout(3):
        await worker.run()
    async with database() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        session = await get_session(db, ids.session_id)
        assert run.error is not None
        assert run.status == "failed" and run.error["phase"] == "preparation"
        assert run.error["code"] == "environment_unavailable"
        assert not session.slot_reserved
        assert run.execution_started_at is None


async def test_context_loss_survives_pause_and_rejects_new_run(
    database, settings, remote
):
    from orpheus.errors import APIError, ExecutionError

    ids, worker = await admitted(database, settings)
    await worker.tick()
    worker.harness.snapshot = AsyncMock(
        side_effect=ExecutionError("context_lost", "Context missing")
    )
    async with asyncio.timeout(3):
        await worker.run()
    record, run = await worker.read()
    assert run is None and record.sandbox_state == "unavailable"
    assert record.sandbox_error["code"] == "context_lost" and not record.slot_reserved
    assert next(iter(remote.objects.values())).state == "paused"
    profiles, cipher = settings.runtime()
    with pytest.raises(APIError) as exc:
        async with database() as db:
            await accept(
                db,
                settings,
                profiles,
                cipher,
                schemas.SendMessage(message=schemas.TextMessage(text="next")),
                uuid4(),
                sid=ids.session_id,
            )
    assert exc.value.error.code == "session_unavailable"


async def test_start_rejection_rejects_initial_message(
    database, settings, remote, monkeypatch
):
    from orpheus.harnesses.codex.rpc import RPCError

    monkeypatch.setattr(
        FakeHarness,
        "start",
        AsyncMock(side_effect=RPCError({"message": "private failure"})),
    )
    ids, worker = await admitted(database, settings)
    async with asyncio.timeout(3):
        await worker.run()
    async with database() as db:
        message = await db.get(MessageRecord, ids.message_id)
        run = await get_run(db, ids.session_id, ids.run_id)
        assert message.delivery_status == "rejected"
        assert run.error is not None
        assert run.status == "failed" and run.error["code"] == "harness_failed"


async def test_uncertain_steer_does_not_emit_alternating_observation(
    database, settings, remote
):
    from orpheus.models import EventRecord

    ids, worker = await admitted(database, settings)
    await worker.tick()
    await worker.tick()
    profiles, cipher = settings.runtime()
    async with database() as db:
        await accept(
            db,
            settings,
            profiles,
            cipher,
            schemas.SendMessage(message=schemas.TextMessage(text="clarify")),
            uuid4(),
            sid=ids.session_id,
            rid=ids.run_id,
        )
    worker.harness.steer = AsyncMock(side_effect=UncertainError)
    with pytest.raises(UncertainError):
        await worker.tick()
    await worker.observation("uncertain")
    async with database() as db:
        before = await db.scalar(select(func.count()).select_from(EventRecord))
    for _ in range(3):
        await worker.tick()
    async with database() as db:
        assert await db.scalar(select(func.count()).select_from(EventRecord)) == before
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.observation == "uncertain"
    await worker.disconnect()


async def test_pause_failure_preserves_result_reports_error_and_backs_off(
    database, settings, remote
):
    ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    sb.turns[-1].status = "completed"
    await worker.tick()
    account = SimpleNamespace(sync=AsyncMock(), close=AsyncMock())
    worker.account = account
    original_pause = sb.pause
    sb.pause = AsyncMock(side_effect=OSError("private network error"))
    await worker.tick()
    record, _ = await worker.read()
    assert record.sandbox_state == "pausing" and record.slot_reserved
    assert record.sandbox_error["phase"] == "recovery"
    for _ in range(3):
        await worker.tick()
    sb.pause.assert_awaited_once()
    account.sync.assert_awaited_once()
    worker.pause_retry_at = 0
    await worker.tick()
    assert sb.pause.await_count == 2
    account.sync.assert_awaited_once()
    worker.pause_retry_at = 0
    sb.pause = original_pause
    await worker.tick()
    record, _ = await worker.read()
    assert record.sandbox_state == "paused" and record.sandbox_error is None
    assert not record.slot_reserved
    async with database() as db:
        assert (await get_run(db, ids.session_id, ids.run_id)).status == "completed"


async def test_cancel_is_sent_once_across_ticks_and_restart(
    database, settings, remote, monkeypatch
):
    interrupt = AsyncMock()
    monkeypatch.setattr(FakeHarness, "interrupt", interrupt)
    settings.cancel_grace_seconds = 30
    ids, worker = await admitted(database, settings)
    await worker.tick()
    async with database() as db:
        await cancel(db, ids.session_id, ids.run_id)
    await worker.tick()
    await worker.tick()
    await worker.disconnect()
    worker = SessionExecutor(ids.session_id, database, settings, settings.runtime()[1])
    await worker.tick()
    interrupt.assert_awaited_once()
    sb = next(iter(remote.objects.values()))
    # A signal may not remove the process immediately; do not send twice per tick.
    sb.commands.kill = AsyncMock()
    async with database.begin() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        run.cancel_attempted_at = now() - timedelta(seconds=60)
    await worker.tick()
    sb.commands.kill.assert_awaited_once()
    await worker.disconnect()


async def test_timeout_lease_is_not_renewed_each_tick(database, settings, remote):
    _ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    sb.set_timeout = AsyncMock()
    for _ in range(3):
        await worker.tick()
    sb.set_timeout.assert_not_awaited()
    worker.timeout_renew_at = 0
    await worker.tick()
    sb.set_timeout.assert_awaited_once()
    await worker.disconnect()


async def test_harness_exit_during_initialization_is_preparation_failure(
    database, settings, remote, monkeypatch
):
    monkeypatch.setattr(FakeRPC, "initialize", AsyncMock(side_effect=UncertainError))
    ids, worker = await admitted(database, settings)
    with pytest.raises(UncertainError):
        await worker.tick()
    sb = next(iter(remote.objects.values()))
    sb.processes.clear()
    await worker.disconnect()
    async with asyncio.timeout(3):
        await worker.run()
    async with database() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.status == "failed" and run.error
        assert (
            run.error["phase"] == "preparation"
            and run.error["code"] == "harness_failed"
        )
        assert not (await get_session(db, ids.session_id)).slot_reserved


async def test_worker_uses_harness_contract_for_launch_and_dead_process_recovery(
    database, settings, remote, monkeypatch
):
    from unittest.mock import Mock

    from orpheus.harnesses.base import Context, Harness

    harness = Mock(spec=Harness)
    harness.has_updates = True
    harness.needs_reconnect = False
    harness.credentials.return_value = None
    harness.prepare.return_value = {"HARNESS_ROOT": "/native/home"}
    harness.open_context.return_value = Context("opaque-context", "/native/history")
    sandbox = None
    turns = []

    def factory(name, sb, **kwargs):
        nonlocal sandbox
        sandbox = sb
        return harness

    async def launch(env, cwd):
        assert sandbox is not None
        assert cwd == "/home/user/workspace"
        assert "CODEX_HOME" not in env
        assert env["HARNESS_ROOT"] == "/native/home"
        assert env["ALL_PROXY"] == settings.sandbox_proxy_url.get_secret_value()
        sandbox.processes.append(SimpleNamespace(pid=123, envs=env))
        return 123

    async def start(context_id, text):
        assert context_id == "opaque-context"
        turns.append(Turn("opaque-turn", "running", [Item("u", "user", 0, text=text)]))
        return "opaque-turn"

    harness.launch.side_effect = launch
    harness.start.side_effect = start
    harness.snapshot.side_effect = lambda *args: Snapshot(copy.deepcopy(turns))
    monkeypatch.setattr("orpheus.worker.create_harness", factory)
    ids, worker = await admitted(database, settings)
    await worker.tick()
    harness.initialize.assert_awaited_once()
    harness.open_context.assert_awaited_once()
    assert sandbox is not None
    # The next worker sees a dead process, and must ask the implementation for
    # history without assuming RPC, JSONL, or the Codex directory structure.
    await worker.disconnect()
    sandbox.processes.clear()
    sandbox.commands.run.reset_mock()
    turns[-1].status = "completed"
    turns[-1].items.append(
        Item("answer", "assistant", 1, text="recovered", kind="answer")
    )
    harness.recover.return_value = Snapshot(turns, "/native/history", 100)
    replacement = SessionExecutor(ids.session_id, database, settings, worker.cipher)
    await replacement.tick()
    harness.recover.assert_awaited_once_with(
        "opaque-context", "/native/history", "/home/user/.orpheus-codex"
    )
    sandbox.commands.run.assert_not_awaited()
    harness.launch.assert_awaited_once()
    async with database() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.status == "completed" and run.final_message_id
    await replacement.disconnect()


async def next_run(database, settings, sid):
    profiles, cipher = settings.runtime()
    async with database() as db:
        return await accept(
            db,
            settings,
            profiles,
            cipher,
            schemas.SendMessage(message=schemas.TextMessage(text="next")),
            uuid4(),
            sid=sid,
        )


@pytest.fixture
def account_remote(remote, config_file, monkeypatch):
    config_file.write_text("""[credential_stores.main]
bucket = "fixture"
[profiles.default]
harness = "codex"
model = "fixture-model"
[profiles.default.auth]
mode = "account"
store = "main"
key = "auth.json"
""")
    account = SimpleNamespace(
        seed=AsyncMock(), watch=AsyncMock(), sync=AsyncMock(), close=AsyncMock()
    )
    monkeypatch.setattr(
        "orpheus.harnesses.codex.driver.AccountCredentials", Mock(return_value=account)
    )
    original = FakeRPC.call

    async def call(self, method, params):
        if method == "account/read":
            return {"account": {"type": "chatgpt"}}
        return await original(self, method, params)

    monkeypatch.setattr(FakeRPC, "call", call)
    return account


@pytest.mark.parametrize("interrupted", [False, True])
async def test_account_repair_when_next_run_arrives_before_pause(
    database, settings, remote, account_remote, monkeypatch, interrupted
):
    account = account_remote
    ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    sb.turns[-1].status = "failed"
    sb.turns[-1].error_code = "authentication_failed"
    await worker.tick()
    _, run = await worker.read()
    assert run is None
    await next_run(database, settings, ids.session_id)
    await worker.tick()  # Stop the old process before replacing credentials.
    assert sb.starts == 1 and not sb.processes
    if interrupted:
        monkeypatch.setattr(
            FakeRPC, "initialize", AsyncMock(side_effect=UncertainError)
        )
        with pytest.raises(UncertainError):
            await worker.tick()
        pid = sb.processes[0].pid
        await worker.disconnect()
        monkeypatch.setattr(FakeRPC, "initialize", AsyncMock())
        worker = SessionExecutor(
            ids.session_id, database, settings, settings.runtime()[1]
        )
        await worker.tick()
        assert sb.processes[0].pid == pid  # Do not kill the already repaired process.
    else:
        await worker.tick()
    await worker.disconnect()
    assert account.seed.await_count == 2
    assert sb.starts == 2 and len(sb.processes) == 1


@pytest.mark.parametrize(
    "interruption", ["initialize", "login", "resume", "ready_commit"]
)
async def test_replacement_process_reconnect_still_resumes_thread(
    database, settings, remote, monkeypatch, interruption
):
    from orpheus.models import OperationRecord

    ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    sb.turns[-1].status = "completed"
    await worker.tick()
    await worker.tick()
    sb.processes.clear()  # Old harness exited between runs; context remains on disk.
    await next_run(database, settings, ids.session_id)
    original_call = FakeRPC.call
    original_status = worker.operation_status

    async def call(self, method, params):
        if method == {"login": "account/login/start", "resume": "thread/resume"}.get(
            interruption
        ):
            raise UncertainError
        return await original_call(self, method, params)

    async def operation_status(ident, status, result=None):
        if result and result.get("initialized") and interruption == "ready_commit":
            raise UncertainError
        await original_status(ident, status, result)

    monkeypatch.setattr(FakeRPC, "call", call)
    monkeypatch.setattr(worker, "operation_status", operation_status)
    if interruption == "initialize":
        monkeypatch.setattr(
            FakeRPC, "initialize", AsyncMock(side_effect=UncertainError)
        )
    with pytest.raises(UncertainError):
        await worker.tick()
    pid = sb.processes[0].pid
    record, _ = await worker.read()
    async with database() as db:
        launch = await db.get(OperationRecord, record.launch_id)
        assert launch and not (launch.result or {}).get("initialized")
    await worker.disconnect()
    # PID is durable, but preparation is incomplete; no task has been sent.
    assert sb.starts == 1
    monkeypatch.setattr(FakeRPC, "initialize", AsyncMock())
    calls = AsyncMock(return_value={})
    monkeypatch.setattr(FakeRPC, "call", calls)
    replacement = SessionExecutor(
        ids.session_id, database, settings, settings.runtime()[1]
    )
    await replacement.tick()
    assert [c.args[0] for c in calls.await_args_list] == [
        "account/login/start",
        "thread/resume",
    ]
    assert sb.starts == 2 and sb.processes[0].pid == pid
    async with database() as db:
        launch = await db.get(OperationRecord, record.launch_id)
        assert launch and launch.result and launch.result["initialized"]
    await replacement.disconnect()
    calls.reset_mock()
    # An initialized process running a task must not be logged in or resumed again.
    replacement = SessionExecutor(
        ids.session_id, database, settings, settings.runtime()[1]
    )
    await replacement.tick()
    calls.assert_not_awaited()
    assert sb.starts == 2 and sb.processes[0].pid == pid
    await replacement.disconnect()


@pytest.mark.parametrize("auth_failed", [False, True])
async def test_restart_between_completion_and_pause_syncs_account(
    database, settings, remote, account_remote, monkeypatch, auth_failed
):
    account = account_remote
    ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    sb.turns[-1].status = "failed" if auth_failed else "completed"
    sb.turns[-1].error_code = "authentication_failed" if auth_failed else None
    await worker.tick()
    await worker.disconnect()
    account.sync.reset_mock()
    account.seed.reset_mock()
    monkeypatch.setattr(
        FakeRPC,
        "launch",
        AsyncMock(side_effect=AssertionError("must not launch to pause")),
    )
    monkeypatch.setattr(
        FakeRPC,
        "initialize",
        AsyncMock(side_effect=AssertionError("must not initialize to pause")),
    )
    replacement = SessionExecutor(
        ids.session_id, database, settings, settings.runtime()[1]
    )
    await replacement.tick()
    assert sb.state == "paused"
    if auth_failed:
        account.sync.assert_not_awaited()
    else:
        account.sync.assert_awaited_once_with(force=True)
    account.seed.assert_not_awaited()


@pytest.mark.parametrize("source", ["lease", "journal", "read", "reconnect"])
async def test_observation_rejection_does_not_fail_running_task(
    database, settings, remote, monkeypatch, source
):
    from agentbox.exceptions import AuthenticationException
    from agentbox.sandbox.commands.command_handle import CommandExitException

    from orpheus.errors import ExecutionError
    from orpheus.harnesses.codex.rpc import RPCError

    ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    error = AuthenticationException("private")
    if source == "lease":
        worker.timeout_renew_at = 0
        sb.set_timeout = AsyncMock(side_effect=error)
    elif source == "reconnect":
        await worker.disconnect()
        monkeypatch.setattr(remote, "get_info", AsyncMock(side_effect=error))
    else:
        error = (
            RPCError({"message": "private read failure"})
            if source == "read"
            else CommandExitException(
                stdout="", stderr="private OOM", exit_code=137, error="command failed"
            )
        )
        monkeypatch.setattr(FakeHarness, "snapshot", AsyncMock(side_effect=error))
    with pytest.raises(type(error)) as exc:
        await worker.tick()
    assert not isinstance(exc.value, ExecutionError)
    async with database() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.status == "running" and run.error is None
    assert sb.processes and sb.starts == 1
    await worker.disconnect()


@pytest.mark.parametrize("steer", [False, True])
async def test_platform_delivery_rejection_resolves_message_status(
    database, settings, remote, monkeypatch, steer
):
    from agentbox.exceptions import AuthenticationException

    from orpheus.errors import ExecutionError

    ids, worker = await admitted(database, settings)
    if steer:
        await worker.tick()
        profiles, cipher = settings.runtime()
        async with database() as db:
            message_ids = await accept(
                db,
                settings,
                profiles,
                cipher,
                schemas.SendMessage(message=schemas.TextMessage(text="clarify")),
                uuid4(),
                sid=ids.session_id,
                rid=ids.run_id,
            )
    else:
        message_ids = ids
    monkeypatch.setattr(
        FakeHarness,
        "steer" if steer else "start",
        AsyncMock(side_effect=AuthenticationException("private")),
    )
    if steer:
        await worker.tick()
    else:
        with pytest.raises(ExecutionError) as exc:
            await worker.tick()
        await worker.failure(exc.value)
    async with database() as db:
        message = await db.get(MessageRecord, message_ids.message_id)
        run = await get_run(db, ids.session_id, ids.run_id)
        assert message and message.delivery_status == "rejected"
        assert message.error and message.error["code"] == "environment_unavailable"
        assert "private" not in str(message.error) and "preparation" not in str(
            message.error
        )
        assert run.status == ("running" if steer else "failed")
    await worker.disconnect()


async def test_large_history_reconciliation_batches_queries_and_emits_only_changes(
    database, settings, remote
):
    from sqlalchemy import event

    from orpheus.models import EventRecord

    ids, worker = await admitted(database, settings)
    await worker.tick()
    sb = next(iter(remote.objects.values()))
    turn = copy.deepcopy(sb.turns[0])
    for i in range(400):
        turn.items.append(
            Item(
                f"item-{i}",
                "assistant" if i % 2 else "tool",
                i + 1,
                text="progress",
                kind="progress",
                name="commandExecution",
                input=["echo", "ok"],
                status="completed",
                result={
                    "type": "text",
                    "text": "ok",
                    "original_bytes": 2,
                    "exit_code": 0,
                },
                completeness="complete",
            )
        )
    snapshot = Snapshot([turn], "/history", 100)
    engine = database.kw["bind"].sync_engine
    statements = []

    def executed(conn, cursor, statement, params, context, executemany):
        statements.append(statement)

    event.listen(engine, "before_cursor_execute", executed)
    try:
        for iteration in range(3):
            statements.clear()
            if iteration == 2:
                turn.items[-1].text = "changed"
            async with database.begin() as db:
                record = await get_session(db, ids.session_id, lock=True)
                before = record.next_event_sequence
                await reconcile(db, record, snapshot)
                change = record.next_event_sequence - before
            assert sum(s.startswith("SELECT") for s in statements) <= 6
            assert len(statements) < 25
            if iteration == 1:
                assert change == 0
            if iteration == 2:
                assert change == 1
        async with database() as db:
            events = list(
                await db.scalars(
                    select(EventRecord)
                    .where(EventRecord.session_id == ids.session_id)
                    .order_by(EventRecord.sequence)
                )
            )
            assert [e.sequence for e in events] == list(range(1, len(events) + 1))
    finally:
        event.remove(engine, "before_cursor_execute", executed)
        await worker.disconnect()


@pytest.mark.parametrize("limit", [10, 30, 200])
@pytest.mark.parametrize("initial_fallback", [False, True])
async def test_later_snapshot_must_not_replace_full_source_with_native_tail(
    database, settings, remote, monkeypatch, limit, initial_fallback
):
    from orpheus.harnesses.codex.items import normalize_thread
    from orpheus.harnesses.output import bounded_result
    from orpheus.models import ToolCallRecord

    ids, worker = await admitted(database, settings)
    await worker.tick()
    await worker.disconnect()
    # Cover limits below the native tail, between tail and full output, and above
    # full output. Both complete and bounded journal results must survive rereads.
    full = "BEGIN" + "x" * 100 + "THE END"
    native_tail = full[-20:]
    result, completeness, reason = bounded_result(full, limit)
    expected = {**result, "exit_code": 0}
    rpc = SimpleNamespace(
        notifications=[],
        history_dirty=False,
        sandbox=object(),
        call=AsyncMock(
            return_value={
                "thread": {
                    "path": "/history",
                    "turns": [
                        {
                            "id": "turn-1",
                            "status": "inProgress",
                            "items": [
                                {
                                    "id": "tool",
                                    "type": "commandExecution",
                                    "status": "completed",
                                    "command": "echo output",
                                    "aggregatedOutput": native_tail,
                                    "exitCode": 0,
                                }
                            ],
                        }
                    ],
                }
            }
        ),
    )

    async def journal(sandbox, path, offset, output_limit):
        return {
            "offset": 100,
            "exhausted": True,
            "items": [
                {
                    "type": "output",
                    "id": "tool",
                    "result": result,
                    "completeness": completeness,
                    "reason": reason,
                }
            ]
            if offset == 0
            else [],
        }

    monkeypatch.setattr("orpheus.harnesses.codex.driver.read_journal", journal)
    if initial_fallback:
        initial = normalize_thread(rpc.call.return_value["thread"], set(), {}, limit)
        async with database.begin() as db:
            session = await get_session(db, ids.session_id, lock=True)
            await reconcile(db, session, initial)
    harness = Codex(cast(RPC, rpc), limit)
    first = await harness.snapshot("thread", "/history", 0)
    async with database.begin() as db:
        session = await get_session(db, ids.session_id, lock=True)
        await reconcile(db, session, first)
    harness.committed()
    # The normal periodic thread/read sees the same shortened native output.
    harness.read_at = 0
    second = await harness.snapshot("thread", "/history", first.offset)
    async with database.begin() as db:
        session = await get_session(db, ids.session_id, lock=True)
        before = session.next_event_sequence
        await reconcile(db, session, second)
        assert session.next_event_sequence == before
    async with database() as db:
        tool = (await db.scalars(select(ToolCallRecord))).one()
        assert tool.result == expected


async def test_unchanged_history_does_not_load_or_serialize_saved_tool_outputs(
    database, settings, remote, monkeypatch
):
    from sqlalchemy import event

    from orpheus import reconciliation
    from orpheus.harnesses.output import bounded_result
    from orpheus.models import EventRecord

    ids, worker = await admitted(database, settings)
    await worker.tick()
    await worker.disconnect()
    full, complete, reason = bounded_result("BEGIN" + "x" * 65536 + "END", 131072)
    items = [
        Item(
            str(i),
            "tool",
            i,
            name="commandExecution",
            input=["echo", "output"],
            status="running",
            result=full,
            completeness=complete,
            truncation_reason=reason,
        )
        for i in range(40)
    ]
    snapshot = Snapshot([Turn("turn-1", "running", items)], "/history", 100)
    async with database.begin() as db:
        session = await get_session(db, ids.session_id, lock=True)
        await reconcile(db, session, snapshot)
    partial, complete, reason = bounded_result("native tail", 131072, incomplete=True)
    for item in items:
        item.result, item.completeness, item.truncation_reason = (
            partial,
            complete,
            reason,
        )
    view = Mock(wraps=reconciliation.tool_view)
    monkeypatch.setattr(reconciliation, "tool_view", view)
    statements = []
    engine = database.kw["bind"].sync_engine

    def executed(conn, cursor, statement, params, context, executemany):
        if statement.startswith("SELECT"):
            statements.append((statement, params))

    event.listen(engine, "before_cursor_execute", executed)
    try:
        async with database.begin() as db:
            session = await get_session(db, ids.session_id, lock=True)
            before = session.next_event_sequence
            await reconcile(db, session, snapshot)
            assert session.next_event_sequence == before
        view.assert_not_called()
        assert not any("tool_calls.result," in sql for sql, _ in statements)
        # Updating only one status needs the saved payload for that event, not
        # the payloads of all forty tools. Preserve the complete saved result.
        statements.clear()
        items[0].status = "completed"
        async with database.begin() as db:
            session = await get_session(db, ids.session_id, lock=True)
            before = session.next_event_sequence
            await reconcile(db, session, snapshot)
            assert session.next_event_sequence == before + 1
        view.assert_called_once()
        payload_reads = [
            (sql, params) for sql, params in statements if "tool_calls.result," in sql
        ]
        assert len(payload_reads) == 1 and len(payload_reads[0][1]) == 1
        async with database() as db:
            saved = await db.scalar(
                select(EventRecord)
                .where(EventRecord.session_id == ids.session_id)
                .order_by(EventRecord.sequence.desc())
                .limit(1)
            )
            assert (
                saved
                and saved.data["result"] == full
                and saved.data["status"] == "completed"
            )
    finally:
        event.remove(engine, "before_cursor_execute", executed)


async def test_reconciliation_binds_large_identity_sets_as_arrays(
    database, settings, remote
):
    from sqlalchemy import event

    ids, worker = await admitted(database, settings)
    await worker.tick()
    await worker.disconnect()
    # Exercise all history-key queries through PostgreSQL/psycopg, beyond its
    # bind-parameter limit. Only the current turn is tracked in this database;
    # archived native turns still participate in the lookup, but create no rows.
    snapshot = Snapshot(
        [
            Turn(
                "turn-1" if n == 0 else f"archived-{n}",
                "running",
                [
                    Item("message", "assistant", 0, text="saved", kind="progress"),
                    Item(
                        "tool",
                        "tool",
                        1,
                        name="commandExecution",
                        input=["echo", "ok"],
                        status="completed",
                        result={"type": "text", "text": "ok", "original_bytes": 2},
                        completeness="complete",
                    ),
                ],
            )
            for n in range(70_000)
        ],
        "/history",
        100,
    )
    observed = []
    engine = database.kw["bind"].sync_engine

    def executed(conn, cursor, statement, params, context, executemany):
        if statement.startswith("SELECT"):
            observed.append(
                (len(params), [len(v) for v in params.values() if isinstance(v, list)])
            )

    event.listen(engine, "before_cursor_execute", executed)
    try:
        for repeat in range(2):
            async with database.begin() as db:
                session = await get_session(db, ids.session_id, lock=True)
                before = session.next_event_sequence
                await reconcile(db, session, snapshot)
                assert session.next_event_sequence - before == (0 if repeat else 2)
        assert max(count for count, _ in observed) <= 6
        assert sum(70_000 in sizes for _, sizes in observed) == 6
    finally:
        event.remove(engine, "before_cursor_execute", executed)


async def test_projection_failure_preserves_sandbox_and_allows_next_run(
    database, settings, remote, monkeypatch
):
    ids, worker = await admitted(database, settings)
    await worker.tick()
    rpc = SimpleNamespace(
        sandbox=object(),
        notifications=[],
        history_dirty=False,
        call=AsyncMock(
            return_value={
                "thread": {
                    "path": "/history",
                    "turns": [{"id": "turn-1", "status": "completed", "items": []}],
                }
            }
        ),
    )
    projection = Codex(cast(RPC, rpc), settings.max_tool_result_bytes)
    monkeypatch.setattr(worker.harness, "snapshot", projection.snapshot)
    monkeypatch.setattr(
        "orpheus.harnesses.codex.driver.read_journal",
        AsyncMock(
            return_value={
                "offset": 100,
                "exhausted": True,
                "items": [{"type": "completed", "id": "missing"}],
            }
        ),
    )
    recovery = AsyncMock(return_value={"turns": [], "completed": [], "outputs": {}})
    monkeypatch.setattr("orpheus.harnesses.codex.offline.read_history", recovery)
    async with asyncio.timeout(3):
        await worker.run()
    recovery.assert_awaited_once()
    async with database() as db:
        run = await get_run(db, ids.session_id, ids.run_id)
        assert run.error is not None
        assert run.status == "failed" and run.error["code"] == "harness_failed"
        assert run.error["phase"] == "execution"
    record, run = await worker.read()
    assert run is None and record.sandbox_state == "paused"
    assert record.sandbox_error is None and not record.slot_reserved
    sb = next(iter(remote.objects.values()))
    assert sb.state == "paused" and sb.processes
    profiles, cipher = settings.runtime()
    async with database() as db:
        next_run = await accept(
            db,
            settings,
            profiles,
            cipher,
            schemas.SendMessage(message=schemas.TextMessage(text="next")),
            uuid4(),
            sid=ids.session_id,
        )
    assert next_run.session_id == ids.session_id and next_run.run_id != ids.run_id
