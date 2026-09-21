"""Single-owner, concurrent session executor. External I/O never holds DB locks."""

import asyncio
import contextlib
import json
import logging
import shlex
import signal
import time
from collections.abc import Awaitable, Callable
from datetime import timedelta
from typing import Any
from uuid import UUID, uuid4

from agentbox import AsyncSandbox
from agentbox.exceptions import (
    AuthenticationException,
    FileNotFoundException,
    InvalidArgumentException,
    NotEnoughSpaceException,
    SandboxException,
    SandboxNotFoundException,
    TemplateException,
)
from agentbox.sandbox.commands.command_handle import CommandExitException
from agentbox.sandbox.sandbox_api import SandboxQuery
from cryptography.fernet import InvalidToken
from sqlalchemy import or_, select, text
from sqlalchemy.ext.asyncio import async_sessionmaker, create_async_engine

from orpheus.configuration import ResolvedConfiguration
from orpheus.errors import Error, ExecutionError, UncertainError
from orpheus.harnesses import create_harness
from orpheus.harnesses.base import (
    CredentialSync,
    Harness,
    HarnessRejectedError,
    Snapshot,
)
from orpheus.models import (
    ACTIVE,
    TERMINAL,
    MessageRecord,
    OperationRecord,
    RunRecord,
    SessionRecord,
    now,
)
from orpheus.reconciliation import reconcile
from orpheus.settings import Settings
from orpheus.store import (
    CAPACITY_LOCK,
    WORKER_LOCK,
    active_run,
    advisory,
    emit,
    finish,
    get_run,
    get_session,
    publish_message,
    publish_run,
    sandbox_view,
)

logger = logging.getLogger(__name__)


def rejected_environment(error: Exception) -> bool:
    if isinstance(
        error,
        (
            AuthenticationException,
            InvalidArgumentException,
            CommandExitException,
            FileNotFoundException,
            NotEnoughSpaceException,
            TemplateException,
        ),
    ):
        return True
    # abox-sdk 0.1.7 exposes most HTTP statuses only in SandboxException text.
    if isinstance(error, SandboxException):
        code, separator, _ = str(error).partition(":")
        return bool(
            separator
            and code.isdecimal()
            and 400 <= int(code) < 500
            and int(code) not in (408, 429)
        )
    return False


class SessionExecutor:
    def __init__(
        self, sid: UUID, sessions: async_sessionmaker, settings: Settings, cipher
    ):
        self.sid, self.sessions, self.settings, self.cipher = (
            sid,
            sessions,
            settings,
            cipher,
        )
        self.sandbox: AsyncSandbox | None = None
        self.harness: Harness | None = None
        self.account: CredentialSync | None = None
        self.snapshot = Snapshot([])
        self.pause_retry_at = 0.0
        self.pause_retry_seconds = 5.0
        self.pause_auth_synced = False
        self.timeout_renew_at = 0.0
        self.timeout_run_id: UUID | None = None

    async def read(self) -> tuple[SessionRecord, RunRecord | None]:
        async with self.sessions() as db:
            return await get_session(db, self.sid), await active_run(db, self.sid)

    async def state(self, state: str, error: Error | None = None) -> None:
        async with self.sessions.begin() as db:
            session = await get_session(db, self.sid, lock=True)
            if session.sandbox_state == "unavailable" and state != "unavailable":
                return  # Physical pause must not restore a lost logical context.
            before = sandbox_view(session)
            session.sandbox_state = state
            if state in ("ready", "paused", "unavailable"):
                session.sandbox_last_known_state = state
            session.sandbox_error = error.model_dump() if error else None
            if before != sandbox_view(session):
                await emit(
                    db,
                    session,
                    "sandbox.updated",
                    sandbox_view(session).model_dump(mode="json"),
                )

    async def observation(self, value: str) -> None:
        async with self.sessions.begin() as db:
            session = await get_session(db, self.sid, lock=True)
            run = await active_run(db, self.sid)
            if run and run.observation != value:
                run.observation = value
                await publish_run(db, session, run)

    async def operation(
        self,
        kind: str,
        run_id: UUID | None = None,
        message_id: UUID | None = None,
        parameters: dict[str, Any] | None = None,
    ) -> OperationRecord:
        async with self.sessions.begin() as db:
            await get_session(db, self.sid, lock=True)
            query = (
                select(OperationRecord)
                .where(
                    OperationRecord.session_id == self.sid,
                    OperationRecord.kind == kind,
                    OperationRecord.run_id == run_id,
                    OperationRecord.message_id == message_id,
                )
                .order_by(OperationRecord.created_at.desc())
                .limit(1)
            )
            prior = (await db.scalars(query)).one_or_none()
            if prior and prior.status != "failed":
                return prior
            operation = OperationRecord(
                id=uuid4(),
                session_id=self.sid,
                run_id=run_id,
                message_id=message_id,
                kind=kind,
                status="pending",
                parameters=parameters or {},
            )
            db.add(operation)
            await db.flush()
            return operation

    async def operation_status(
        self, ident: UUID, status: str, result: dict[str, Any] | None = None
    ) -> None:
        async with self.sessions.begin() as db:
            await get_session(db, self.sid, lock=True)
            operation = await db.get(OperationRecord, ident)
            operation.status = status
            if status == "sending" and operation.attempted_at is None:
                operation.attempted_at = now()
            if result is not None:
                operation.result = result

    async def invoke(
        self, operation: OperationRecord, call: Callable[[], Awaitable[dict[str, Any]]]
    ) -> dict[str, Any]:
        if operation.status == "confirmed":
            return operation.result or {}
        if operation.status != "pending":
            raise UncertainError
        await self.operation_status(operation.id, "sending")
        try:
            result = await call()
        except HarnessRejectedError, SandboxNotFoundException:
            await self.operation_status(operation.id, "failed")
            raise
        except Exception as error:  # noqa: BLE001 - classify confirmed rejection vs unknown delivery
            if rejected_environment(error):
                await self.operation_status(operation.id, "failed")
                raise ExecutionError(
                    "environment_unavailable", "Sandbox operation was rejected."
                ) from None
            await self.operation_status(operation.id, "uncertain")
            raise UncertainError from None
        await self.operation_status(operation.id, "confirmed", result)
        return result

    async def ensure_sandbox(self, record: SessionRecord, run: RunRecord) -> None:
        config = ResolvedConfiguration.model_validate(record.configuration)
        timeout = (
            config.public.limits.run_timeout_seconds
            + int(self.settings.cancel_grace_seconds)
            + 300
        )
        if not record.sandbox_id:
            await self.state("provisioning")
            operation = await self.operation("sandbox_create")
            if operation.status in ("sending", "uncertain"):
                paginator = AsyncSandbox.list(
                    query=SandboxQuery(
                        metadata={
                            "orpheus_session_id": str(self.sid),
                            "orpheus_operation_id": str(operation.id),
                        }
                    )
                )
                found = []
                while paginator.has_next:
                    found.extend(await paginator.next_items())
                if len(found) != 1:
                    raise UncertainError
                result = {"sandbox_id": found[0].sandbox_id}
                await self.operation_status(operation.id, "confirmed", result)
            else:

                async def create() -> dict[str, Any]:
                    self.sandbox = await AsyncSandbox.create(
                        template=config.public.sandbox.template,
                        timeout=timeout,
                        metadata={
                            "orpheus_session_id": str(self.sid),
                            "orpheus_operation_id": str(operation.id),
                        },
                        lifecycle={
                            "on_timeout": {"action": "pause", "keep_memory": True},
                            "auto_resume": False,
                        },
                    )
                    return {"sandbox_id": self.sandbox.sandbox_id}

                result = await self.invoke(operation, create)
            async with self.sessions.begin() as db:
                session = await get_session(db, self.sid, lock=True)
                session.sandbox_id = result["sandbox_id"]
            record.sandbox_id = result["sandbox_id"]
        assert record.sandbox_id is not None
        if self.sandbox is None:
            # get_info does not wake a paused sandbox; connect is an explicit wake.
            info = await AsyncSandbox.get_info(record.sandbox_id)
            if info.state.value == "paused":
                await self.state("resuming")
            operation = await self.operation("resume", run.id)
            await self.operation_status(operation.id, "sending")
            self.sandbox = await AsyncSandbox.connect(
                record.sandbox_id, timeout=timeout
            )
            await self.operation_status(operation.id, "confirmed")
        elif self.timeout_run_id != run.id or time.monotonic() >= self.timeout_renew_at:
            await self.sandbox.set_timeout(timeout)
        else:
            return
        self.timeout_run_id = run.id
        self.timeout_renew_at = time.monotonic() + min(60, timeout / 2)
        self.pause_auth_synced = False
        self.pause_retry_at = 0
        self.pause_retry_seconds = 5
        await self.state("ready")

    async def prepare_paths(self, record: SessionRecord) -> None:
        assert self.sandbox
        if record.workspace and record.harness_home:
            return
        config = ResolvedConfiguration.model_validate(record.configuration)
        script = "import pwd,pathlib,json,sys; h=pathlib.Path(pwd.getpwnam('user').pw_dir); w=h/'workspace'; w.mkdir(exist_ok=True); c=h/sys.argv[1]; c.mkdir(mode=0o700,exist_ok=True); c.chmod(0o700); print(json.dumps([str(w),str(c)]))"
        result = await self.sandbox.commands.run(
            "python3 -c "
            + shlex.quote(script)
            + " "
            + shlex.quote(f".orpheus-{config.harness}"),
            user="user",
            timeout=30,
        )
        workspace, home = json.loads(result.stdout)
        async with self.sessions.begin() as db:
            session = await get_session(db, self.sid, lock=True)
            session.workspace, session.harness_home = workspace, home
        record.workspace, record.harness_home = workspace, home

    async def auth_failed_before(
        self, run: RunRecord, *, include_current: bool = False
    ) -> bool:
        async with self.sessions() as db:
            previous = (
                await db.scalars(
                    select(RunRecord)
                    .where(
                        RunRecord.session_id == self.sid,
                        RunRecord.number < run.number + int(include_current),
                        or_(
                            RunRecord.execution_started_at.is_not(None),
                            RunRecord.error["code"].astext == "authentication_failed",
                        ),
                    )
                    .order_by(RunRecord.number.desc())
                    .limit(1)
                )
            ).one_or_none()
            return bool(
                previous
                and previous.error
                and previous.error["code"] == "authentication_failed"
            )

    async def ensure_harness(self, record: SessionRecord, run: RunRecord) -> None:
        assert self.sandbox
        config = ResolvedConfiguration.model_validate(record.configuration)
        repair = (
            config.credentials.mode == "account"
            and run.execution_started_at is None
            and await self.auth_failed_before(run)
        )
        if self.harness and not repair:
            return
        if self.harness:
            await self.harness.close()
            self.harness = None
        if repair and self.account:
            await self.account.close()
            self.account = None
        await self.prepare_paths(record)
        assert record.harness_home and record.workspace
        processes = await self.sandbox.commands.list()
        process = next(
            (
                p
                for p in processes
                if p.pid == record.process_id
                and p.envs.get("ORPHEUS_LAUNCH_ID") == str(record.launch_id)
            ),
            None,
        )
        if process is None:
            async with self.sessions() as db:
                outstanding = (
                    await db.scalars(
                        select(OperationRecord)
                        .where(
                            OperationRecord.session_id == self.sid,
                            OperationRecord.kind == "launch",
                            OperationRecord.status.in_(
                                ("sending", "uncertain", "confirmed")
                            ),
                        )
                        .order_by(OperationRecord.created_at.desc())
                        .limit(1)
                    )
                ).one_or_none()
            if outstanding and outstanding.id != record.launch_id:
                matches = [
                    p
                    for p in processes
                    if p.envs.get("ORPHEUS_LAUNCH_ID") == str(outstanding.id)
                ]
                if len(matches) > 1 or (
                    not matches and outstanding.status != "confirmed"
                ):
                    raise UncertainError
                if matches:
                    process = matches[0]
                    await self.operation_status(
                        outstanding.id, "confirmed", {"pid": process.pid}
                    )
                    async with self.sessions.begin() as db:
                        session = await get_session(db, self.sid, lock=True)
                        session.process_id, session.launch_id = (
                            process.pid,
                            outstanding.id,
                        )
                    record.process_id, record.launch_id = process.pid, outstanding.id
        async with self.sessions() as db:
            launch_record = (
                await db.get(OperationRecord, record.launch_id)
                if record.launch_id
                else None
            )
        # A process already launched for this run received repaired credentials.
        # Reconnect to it after interrupted initialization instead of killing it.
        repair = repair and (launch_record is None or launch_record.run_id != run.id)
        if repair and process:
            await self.sandbox.commands.kill(process.pid)
            return  # Next tick confirms death before replacing credentials/process.
        harness = create_harness(
            config.harness,
            self.sandbox,
            timeout=self.settings.rpc_timeout_seconds,
            output_limit=self.settings.max_tool_result_bytes,
        )
        if self.account is None:
            self.account = harness.credentials(record.harness_home, config.credentials)
        is_new = process is None
        if is_new:
            if run.execution_started_at is not None:
                # Never launch a replacement while the outcome of a run is unknown.
                await self.recover_dead_process(record, run)
                return
            operation = await self.operation("launch", run.id)
            if operation.status in ("sending", "uncertain", "confirmed"):
                matches = [
                    p
                    for p in processes
                    if p.envs.get("ORPHEUS_LAUNCH_ID") == str(operation.id)
                ]
                if not matches and operation.status == "confirmed":
                    raise ExecutionError(
                        "harness_failed", "Harness exited during preparation."
                    )
                if len(matches) != 1:
                    raise UncertainError
                pid = matches[0].pid
                await harness.attach(pid)
                await self.operation_status(operation.id, "confirmed", {"pid": pid})
            else:
                try:
                    env = self.cipher.environment(
                        self.sid,
                        record.env_ciphertext,
                        config.public,
                        self.settings.harness_env_allowlist,
                    )
                except KeyError as error:
                    raise ExecutionError(
                        "environment_unavailable",
                        f"Environment variable {error.args[0]} is unavailable.",
                    ) from None
                except ValueError, InvalidToken:
                    raise ExecutionError(
                        "environment_unavailable", "Harness environment is unavailable."
                    ) from None
                if self.account and (not record.process_id or repair):
                    await self.account.seed()
                env.update(
                    await harness.prepare(record.harness_home, config.credentials)
                )
                proxy = self.settings.sandbox_proxy_url.get_secret_value()
                if proxy:
                    env.update({"ALL_PROXY": proxy, "NO_PROXY": "localhost,127.0.0.1"})
                env["ORPHEUS_LAUNCH_ID"] = str(operation.id)

                async def launch() -> dict[str, Any]:
                    assert record.workspace is not None
                    return {"pid": await harness.launch(env, record.workspace)}

                try:
                    pid = (await self.invoke(operation, launch))["pid"]
                except BaseException:
                    await harness.close()
                    raise
            async with self.sessions.begin() as db:
                session = await get_session(db, self.sid, lock=True)
                session.process_id, session.launch_id = pid, operation.id
            record.process_id, record.launch_id = pid, operation.id
        else:
            await harness.attach(process.pid)
        try:
            assert record.launch_id is not None
            async with self.sessions() as db:
                launch_record = await db.get(OperationRecord, record.launch_id)
            initialized = bool(
                launch_record and (launch_record.result or {}).get("initialized")
            )
            await harness.initialize(config.credentials, login=not initialized)
            if record.thread_id:
                if not initialized:
                    await harness.open_context(
                        config.public.agent, record.workspace, record.thread_id
                    )
            else:
                operation = await self.operation("thread", run.id)
                # No turn has been sent yet; an empty context has no external work.
                if operation.status == "confirmed":
                    thread = operation.result
                    assert thread is not None
                else:
                    await self.operation_status(operation.id, "sending")
                    context = await harness.open_context(
                        config.public.agent, record.workspace
                    )
                    thread = {"id": context.native_id, "path": context.history_path}
                    await self.operation_status(
                        operation.id,
                        "confirmed",
                        {"id": thread["id"], "path": thread.get("path")},
                    )
                async with self.sessions.begin() as db:
                    session = await get_session(db, self.sid, lock=True)
                    session.thread_id, session.history_path = (
                        thread["id"],
                        thread.get("path"),
                    )
                record.thread_id = thread["id"]
            if not initialized:
                await self.operation_status(
                    record.launch_id,
                    "confirmed",
                    {"pid": record.process_id, "initialized": True},
                )
            self.harness = harness
            if self.account:
                await self.account.watch()
                # Check the actual run outcome before any upload on reconnect.
        except BaseException:
            await harness.close()
            raise

    async def recover_dead_process(self, record: SessionRecord, run: RunRecord) -> None:
        assert self.sandbox is not None
        config = ResolvedConfiguration.model_validate(record.configuration)
        harness = create_harness(
            config.harness,
            self.sandbox,
            timeout=self.settings.rpc_timeout_seconds,
            output_limit=self.settings.max_tool_result_bytes,
        )
        try:
            snapshot = await harness.recover(
                record.thread_id, record.history_path, record.harness_home
            )
        finally:
            await harness.close()
        async with self.sessions.begin() as db:
            session = await get_session(db, self.sid, lock=True)
            await reconcile(db, session, snapshot)
            current = await get_run(db, self.sid, run.id)
            if current.status in ACTIVE:
                if current.cancel_requested_at:
                    await finish(
                        db, session, current, "cancelled", stop_method="forced"
                    )
                else:
                    await finish(
                        db,
                        session,
                        current,
                        "failed",
                        Error(
                            code="harness_failed",
                            message="Harness exited before completion.",
                            phase="execution",
                        ),
                    )

    async def refresh(self, record: SessionRecord) -> None:
        assert self.harness and record.thread_id
        if not self.harness.has_updates:
            return
        self.snapshot = await self.harness.snapshot(
            record.thread_id, record.history_path, record.history_offset
        )
        async with self.sessions.begin() as db:
            session = await get_session(db, self.sid, lock=True)
            await reconcile(db, session, self.snapshot)
        # SDK 0.1.7 retains stdout chunks. Bound each connection's lifetime;
        # reconnect has no token replay requirement and durable history fills gaps.
        self.harness.committed()
        if self.harness.needs_reconnect:
            await self.harness.close()
            self.harness = None

    async def send(self, record: SessionRecord, run: RunRecord) -> None:
        assert self.harness and record.thread_id
        async with self.sessions() as db:
            message = (
                await db.scalars(
                    select(MessageRecord)
                    .where(
                        MessageRecord.run_id == run.id,
                        MessageRecord.role == "user",
                        MessageRecord.delivery_status.in_(
                            ("pending", "sending", "uncertain")
                        ),
                    )
                    .order_by(MessageRecord.delivery_number)
                    .limit(1)
                )
            ).one_or_none()
        if message is None:
            return
        kind = "start" if message.delivery_number == 1 else "steer"
        native = next(
            (
                turn
                for turn in self.snapshot.turns
                if turn.native_id == run.native_turn_id
            ),
            None,
        )
        operation = await self.operation(
            kind,
            run.id,
            message.id,
            {
                "thread_id": record.thread_id,
                "turn_id": run.native_turn_id,
                "previous_turns": [t.native_id for t in self.snapshot.turns],
                "previous_items": [i.native_id for i in native.items] if native else [],
            },
        )
        if operation.status in ("sending", "uncertain"):
            await self.observation("uncertain")
            return
        if operation.status == "confirmed":
            return
        # Persist the delivery attempt and original deadline before external I/O.
        async with self.sessions.begin() as db:
            session = await get_session(db, self.sid, lock=True)
            current = await get_run(db, self.sid, run.id)
            if current.cancel_requested_at or current.status in TERMINAL:
                return
            current_message = await db.get(MessageRecord, message.id)
            current_message.delivery_status = "sending"
            await publish_message(db, session, current_message)
            if kind == "start" and current.execution_started_at is None:
                current.execution_started_at = now()
                current.deadline_at = now() + timedelta(
                    seconds=record.configuration["public"]["limits"][
                        "run_timeout_seconds"
                    ]
                )
                await publish_run(db, session, current)

        async def deliver() -> dict[str, Any]:
            assert self.harness and record.thread_id
            if kind == "start":
                return {
                    "turn_id": await self.harness.start(record.thread_id, message.text)
                }
            assert run.native_turn_id is not None
            await self.harness.steer(record.thread_id, run.native_turn_id, message.text)
            return {}

        try:
            result = await self.invoke(operation, deliver)
        except (HarnessRejectedError, ExecutionError) as error:
            rejection = (
                Error(code=error.code, message=error.message)
                if isinstance(error, ExecutionError)
                else Error(
                    code="harness_failed"
                    if kind == "start"
                    else "run_finished_before_delivery",
                    message="Harness rejected the assignment."
                    if kind == "start"
                    else "Harness no longer accepts this message.",
                )
            )
            async with self.sessions.begin() as db:
                session = await get_session(db, self.sid, lock=True)
                current_message = await db.get(MessageRecord, message.id)
                current_message.delivery_status = "rejected"
                current_message.error = rejection.model_dump()
                await publish_message(db, session, current_message)
            if kind == "start":
                raise ExecutionError(rejection.code, rejection.message) from None
            return
        except UncertainError:
            async with self.sessions.begin() as db:
                session = await get_session(db, self.sid, lock=True)
                current_message = await db.get(MessageRecord, message.id)
                current_message.delivery_status = "uncertain"
                await publish_message(db, session, current_message)
            raise
        async with self.sessions.begin() as db:
            session = await get_session(db, self.sid, lock=True)
            current = await get_run(db, self.sid, run.id)
            current_message = await db.get(MessageRecord, message.id)
            current_message.delivery_status = "delivered"
            await publish_message(db, session, current_message)
            if kind == "start":
                current.native_turn_id = result["turn_id"]
                if not current.cancel_requested_at:
                    current.status = "running"
                current.observation = "attached"
                await publish_run(db, session, current)

    async def cancel(self, record: SessionRecord, run: RunRecord) -> None:
        if run.execution_started_at is None:
            async with self.sessions.begin() as db:
                session = await get_session(db, self.sid, lock=True)
                current = await get_run(db, self.sid, run.id)
                await finish(db, session, current, "cancelled")
            return
        operation = await self.operation("cancel", run.id)
        if run.cancel_attempted_at is None:
            async with self.sessions.begin() as db:
                session = await get_session(db, self.sid, lock=True)
                current = await get_run(db, self.sid, run.id)
                current.cancel_attempted_at = now()
                await publish_run(db, session, current)
            run.cancel_attempted_at = now()
        if operation.status != "pending":
            return
        if not self.harness or not record.thread_id or not run.native_turn_id:
            return
        await self.operation_status(operation.id, "sending")
        try:
            await self.harness.interrupt(record.thread_id, run.native_turn_id)
            await self.operation_status(operation.id, "confirmed")
        except HarnessRejectedError:
            await self.operation_status(operation.id, "confirmed", {"accepted": False})
        except Exception:
            await self.operation_status(operation.id, "uncertain")
            raise

    async def force_stop(self, record: SessionRecord, run: RunRecord) -> None:
        assert self.sandbox
        processes = await self.sandbox.commands.list()
        matching = [
            p
            for p in processes
            if p.pid == record.process_id
            and p.envs.get("ORPHEUS_LAUNCH_ID") == str(record.launch_id)
        ]
        if matching:
            operation = await self.operation("kill", run.id)
            await self.operation_status(operation.id, "sending")
            await self.sandbox.commands.kill(matching[0].pid)
            await self.operation_status(operation.id, "confirmed")
        # Do not infer death from a signal response; a subsequent list confirms it.
        if self.harness:
            await self.harness.close()
            self.harness = None

    async def pause(self, record: SessionRecord) -> None:
        if time.monotonic() < self.pause_retry_at:
            return
        try:
            await self._pause(record)
            self.pause_retry_at = 0
            self.pause_retry_seconds = 5
        except SandboxNotFoundException:
            raise  # tick confirms loss before releasing the reservation.
        except Exception as error:  # noqa: BLE001 - pause failure must not alter the run result
            await self.state(
                "pausing",
                Error(
                    code="environment_unavailable",
                    message="Sandbox pause failed; retrying.",
                    phase="recovery",
                ),
            )
            self.pause_retry_at = time.monotonic() + self.pause_retry_seconds
            self.pause_retry_seconds = min(60, self.pause_retry_seconds * 2)
            logger.warning(
                "Session %s pause failed (%s)", self.sid, type(error).__name__
            )
            await self.disconnect()

    async def _pause(self, record: SessionRecord) -> None:
        if record.sandbox_id:
            if record.sandbox_state != "pausing":
                await self.state("pausing")
            if self.sandbox is None:
                info = await AsyncSandbox.get_info(record.sandbox_id)
                if info.state.value != "paused":
                    self.sandbox = await AsyncSandbox.connect(record.sandbox_id)
            if self.sandbox:
                async with self.sessions() as db:
                    last = (
                        await db.scalars(
                            select(RunRecord)
                            .where(
                                RunRecord.session_id == self.sid,
                                RunRecord.status.in_(TERMINAL),
                            )
                            .order_by(RunRecord.number.desc())
                            .limit(1)
                        )
                    ).one()
                if not self.pause_auth_synced:
                    if not await self.auth_failed_before(last, include_current=True):
                        if self.account is None and record.harness_home:
                            config = ResolvedConfiguration.model_validate(
                                record.configuration
                            )
                            harness = create_harness(
                                config.harness,
                                self.sandbox,
                                timeout=self.settings.rpc_timeout_seconds,
                                output_limit=self.settings.max_tool_result_bytes,
                            )
                            self.account = harness.credentials(
                                record.harness_home, config.credentials
                            )
                            await harness.close()
                        if self.account:
                            await self.account.sync(force=True)
                    self.pause_auth_synced = True
                operation = await self.operation("pause", last.id)
                await self.operation_status(operation.id, "sending")
                await self.sandbox.pause(keep_memory=True)
                await self.operation_status(operation.id, "confirmed")
                await self.disconnect()
            if record.sandbox_state != "unavailable":
                await self.state("paused")
        async with self.sessions.begin() as db:
            await advisory(db, CAPACITY_LOCK)
            session = await get_session(db, self.sid, lock=True)
            if await active_run(db, self.sid) is None:
                session.slot_reserved = False

    async def failure(self, error: ExecutionError) -> None:
        async with self.sessions.begin() as db:
            session = await get_session(db, self.sid, lock=True)
            run = await active_run(db, self.sid)
            if run:
                await finish(
                    db,
                    session,
                    run,
                    "failed",
                    Error(
                        code=error.code,
                        message=error.message,
                        phase="execution"
                        if run.execution_started_at
                        else "preparation",
                    ),
                )
        if error.code in ("sandbox_lost", "context_lost"):
            await self.state(
                "unavailable",
                Error(code=error.code, message=error.message, phase="recovery"),
            )

    async def tick(self) -> None:
        try:
            await self._tick()
        except SandboxNotFoundException:
            record, _ = await self.read()
            # Confirm platform loss; a missing process/file must not be confused
            # with a sandbox deleted by the provider.
            if record.sandbox_id:
                try:
                    await AsyncSandbox.get_info(record.sandbox_id)
                except SandboxNotFoundException:
                    await self.failure(
                        ExecutionError(
                            "sandbox_lost", "Sandbox is no longer available."
                        )
                    )
                    async with self.sessions.begin() as db:
                        await advisory(db, CAPACITY_LOCK)
                        session = await get_session(db, self.sid, lock=True)
                        session.slot_reserved = False
                    return
            if not record.sandbox_id:
                raise ExecutionError(
                    "environment_unavailable", "Sandbox template is unavailable."
                ) from None
            raise
        except HarnessRejectedError:
            _, run = await self.read()
            if run and run.execution_started_at is not None:
                raise  # Observation failure is not evidence that execution stopped.
            raise ExecutionError(
                "harness_failed", "Harness rejected initialization."
            ) from None
        except Exception as error:
            if rejected_environment(error):
                _, run = await self.read()
                if run and run.execution_started_at is not None:
                    raise
                raise ExecutionError(
                    "environment_unavailable", "Sandbox environment is unavailable."
                ) from None
            raise

    async def _tick(self) -> None:
        record, run = await self.read()
        if run is None:
            await self.pause(record)
            return
        if run.status == "accepted":
            async with self.sessions.begin() as db:
                session = await get_session(db, self.sid, lock=True)
                current = await get_run(db, self.sid, run.id)
                if current.status == "accepted":
                    current.status = "starting"
                    await publish_run(db, session, current)
        record, run = await self.read()
        if run is None:
            await self.pause(record)
            return
        if run.cancel_requested_at and run.execution_started_at is None:
            if not record.sandbox_id:
                async with self.sessions() as db:
                    attempted = await db.scalar(
                        select(OperationRecord.id)
                        .where(
                            OperationRecord.session_id == self.sid,
                            OperationRecord.kind == "sandbox_create",
                            OperationRecord.status.in_(
                                ("sending", "uncertain", "confirmed")
                            ),
                        )
                        .limit(1)
                    )
                if attempted:
                    await self.ensure_sandbox(record, run)
            await self.cancel(record, run)
            return
        await self.ensure_sandbox(record, run)
        if run.cancel_requested_at and run.execution_started_at is None:
            await self.cancel(record, run)
            return
        if run.deadline_at and now() >= run.deadline_at and not run.cancel_requested_at:
            async with self.sessions.begin() as db:
                session = await get_session(db, self.sid, lock=True)
                current = await get_run(db, self.sid, run.id)
                current.cancel_requested_at, current.stop_reason, current.status = (
                    now(),
                    "run_timeout",
                    "cancelling",
                )
                await publish_run(db, session, current)
            record, run = await self.read()
        if run is None:
            return
        if run.cancel_requested_at:
            if run.cancel_attempted_at is None:
                async with self.sessions.begin() as db:
                    session = await get_session(db, self.sid, lock=True)
                    current = await get_run(db, self.sid, run.id)
                    current.cancel_attempted_at = now()
                    await publish_run(db, session, current)
                run.cancel_attempted_at = current.cancel_attempted_at
            assert run.cancel_attempted_at is not None
            if now() >= run.cancel_attempted_at + timedelta(
                seconds=self.settings.cancel_grace_seconds
            ):
                await self.force_stop(record, run)
                await self.ensure_harness(record, run)
                return
        await self.ensure_harness(record, run)
        if self.harness is None:
            return
        record, run = await self.read()
        if run is None:
            return
        if run.execution_started_at is not None or run.number > 1:
            await self.refresh(record)
        record, run = await self.read()
        if run is None:
            return
        if run.cancel_requested_at:
            await self.cancel(record, run)
        elif self.harness:
            await self.send(record, run)
        if self.account:
            await self.account.sync()

    async def disconnect(self) -> None:
        if self.harness:
            await self.harness.close()
            self.harness = None
        if self.account:
            with contextlib.suppress(Exception):
                await self.account.close()
            self.account = None
        self.sandbox = None

    async def run(self) -> None:
        try:
            while True:
                try:
                    await self.tick()
                    record, _ = await self.read()
                    if not record.slot_reserved:
                        return
                except ExecutionError as error:
                    await self.failure(error)
                except Exception as error:  # noqa: BLE001 - isolate sessions and reconcile SDK failures
                    await self.observation(
                        "uncertain"
                        if isinstance(error, UncertainError)
                        else "reconnecting"
                    )
                    logger.warning(
                        "Session %s reconnecting (%s)", self.sid, type(error).__name__
                    )
                    await self.disconnect()
                await asyncio.sleep(self.settings.worker_poll_seconds)
        finally:
            await self.disconnect()


async def run_worker(settings: Settings) -> None:
    _, cipher = settings.runtime()
    engine = create_async_engine(settings.database_url, pool_pre_ping=True)
    sessions = async_sessionmaker(engine, expire_on_commit=False)
    stopping = asyncio.Event()
    loop = asyncio.get_running_loop()
    for signum in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(signum, stopping.set)
    tasks: dict[UUID, asyncio.Task] = {}
    try:
        async with engine.connect() as owner:
            owned = await owner.scalar(
                text("SELECT pg_try_advisory_lock(:key)"), {"key": WORKER_LOCK}
            )
            if not owned:
                raise RuntimeError("Another worker owns the executor lock")
            await owner.commit()
            while not stopping.is_set():
                # No pool reconnect can replace this dedicated ownership connection.
                async with asyncio.timeout(settings.readiness_timeout):
                    await owner.execute(text("SELECT 1"))
                    await owner.commit()
                async with sessions() as db:
                    ids = list(
                        await db.scalars(
                            select(SessionRecord.id).where(SessionRecord.slot_reserved)
                        )
                    )
                for sid in list(tasks):
                    if tasks[sid].done():
                        task = tasks.pop(sid)
                        if not task.cancelled() and task.exception():
                            logger.warning(
                                "Session executor stopped (%s)",
                                type(task.exception()).__name__,
                            )
                for sid in ids:
                    if sid not in tasks:
                        tasks[sid] = asyncio.create_task(
                            SessionExecutor(sid, sessions, settings, cipher).run()
                        )
                try:
                    await asyncio.wait_for(
                        stopping.wait(), timeout=settings.worker_poll_seconds
                    )
                except TimeoutError:
                    pass
    finally:
        for task in tasks.values():
            task.cancel()
        await asyncio.gather(*tasks.values(), return_exceptions=True)
        for signum in (signal.SIGTERM, signal.SIGINT):
            loop.remove_signal_handler(signum)
        await engine.dispose()
