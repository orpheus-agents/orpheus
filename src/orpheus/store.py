"""Transactional admission, publication and snapshot reads on PostgreSQL 16."""

import base64
import hashlib
import json
from datetime import datetime
from typing import Any
from uuid import UUID, uuid4

from cryptography.fernet import InvalidToken
from sqlalchemy import func, literal, select, text, tuple_
from sqlalchemy.ext.asyncio import AsyncSession

from orpheus import schemas
from orpheus.configuration import EnvironmentCipher, Profiles, resolve
from orpheus.errors import APIError, Error
from orpheus.models import (
    ACTIVE,
    TERMINAL,
    EventRecord,
    IdempotencyRecord,
    MessageRecord,
    RunRecord,
    SessionRecord,
    ToolCallRecord,
    now,
)
from orpheus.settings import Settings

CAPACITY_LOCK = 4857668146151
WORKER_LOCK = 4857668146152


def lock_key(value: str) -> int:
    return int.from_bytes(
        hashlib.sha256(value.encode()).digest()[:8], "big", signed=True
    )


async def advisory(db: AsyncSession, key: int) -> None:
    await db.execute(text("SELECT pg_advisory_xact_lock(:key)"), {"key": key})


def message_view(record: MessageRecord) -> schemas.Message:
    return schemas.Message(
        **{
            key: getattr(record, key)
            for key in schemas.Message.model_fields
            if key != "registered_sequence"
        },
        registered_sequence=str(record.registered_sequence),
    )


def tool_view(record: ToolCallRecord) -> schemas.ToolCall:
    return schemas.ToolCall(
        **{
            key: getattr(record, key)
            for key in schemas.ToolCall.model_fields
            if key != "registered_sequence"
        },
        registered_sequence=str(record.registered_sequence),
    )


async def run_view(db: AsyncSession, record: RunRecord) -> schemas.Run:
    final = (
        await db.get(MessageRecord, record.final_message_id)
        if record.final_message_id
        else None
    )
    return schemas.Run(
        **{
            key: getattr(record, key)
            for key in schemas.Run.model_fields
            if key != "final_message"
        },
        final_message=message_view(final)
        if final and record.status in TERMINAL
        else None,
    )


def sandbox_view(record: SessionRecord) -> schemas.SandboxState:
    return schemas.SandboxState(
        state=record.sandbox_state,  # ty: ignore[invalid-argument-type] - DB CHECK matches the API enum
        last_known_state=record.sandbox_last_known_state,
        error=record.sandbox_error,
    )


async def session_view(db: AsyncSession, record: SessionRecord) -> schemas.Session:
    run = (
        await db.scalars(
            select(RunRecord)
            .where(RunRecord.session_id == record.id)
            .order_by(RunRecord.number.desc())
            .limit(1)
        )
    ).one()
    view = await run_view(db, run)
    return schemas.Session(
        id=record.id,
        created_at=record.created_at,
        configuration=record.configuration["public"],
        sandbox=sandbox_view(record),
        active_run_id=run.id if run.status in ACTIVE else None,
        last_run_id=run.id,
        status=view.status,
        final_message=view.final_message,
        error=view.error,
    )


async def emit(
    db: AsyncSession,
    session: SessionRecord,
    kind: str,
    data: dict[str, Any],
    *,
    flush: bool = True,
) -> None:
    db.add(
        EventRecord(
            session_id=session.id,
            sequence=session.next_event_sequence,
            type=kind,
            data=data,
        )
    )
    session.next_event_sequence += 1
    if flush:
        await db.flush()


async def publish_message(
    db: AsyncSession, session: SessionRecord, message: MessageRecord
) -> None:
    if not message.registered_sequence:
        message.registered_sequence = session.next_event_sequence
    await db.flush()
    await emit(
        db, session, "message.updated", message_view(message).model_dump(mode="json")
    )


async def publish_run(db: AsyncSession, session: SessionRecord, run: RunRecord) -> None:
    await db.flush()
    await emit(
        db, session, "run.updated", (await run_view(db, run)).model_dump(mode="json")
    )


async def get_session(
    db: AsyncSession, sid: UUID, *, lock: bool = False
) -> SessionRecord:
    query = select(SessionRecord).where(SessionRecord.id == sid)
    if lock:
        query = query.with_for_update()
    record = (await db.scalars(query)).one_or_none()
    if record is None:
        raise APIError(404, "session_not_found", "Session not found.")
    return record


async def get_run(db: AsyncSession, sid: UUID, rid: UUID) -> RunRecord:
    run = (
        await db.scalars(
            select(RunRecord).where(RunRecord.session_id == sid, RunRecord.id == rid)
        )
    ).one_or_none()
    if run is None:
        raise APIError(404, "run_not_found", "Run not found.")
    return run


async def active_run(db: AsyncSession, sid: UUID) -> RunRecord | None:
    return (
        await db.scalars(
            select(RunRecord).where(
                RunRecord.session_id == sid, RunRecord.status.in_(ACTIVE)
            )
        )
    ).one_or_none()


def fingerprint(request: schemas.CreateSession | schemas.SendMessage) -> str:
    value = request.model_dump(exclude_unset=True)
    if isinstance(request, schemas.CreateSession):
        # Defaults are canonical; absent overrides remain distinguishable from explicit ones.
        value = request.model_dump()
        agent = value["configuration"]["agent"]
        for name in ("model", "instructions"):
            if name not in request.configuration.agent.model_fields_set:
                agent.pop(name)
        value["configuration"]["sandbox"]["env"] = sorted(
            request.configuration.sandbox.env
        )
    return hashlib.sha256(
        json.dumps(
            value, sort_keys=True, separators=(",", ":"), ensure_ascii=False
        ).encode()
    ).hexdigest()


async def accept(
    db: AsyncSession,
    settings: Settings,
    profiles: Profiles,
    cipher: EnvironmentCipher,
    request: schemas.CreateSession | schemas.SendMessage,
    key: UUID,
    *,
    sid: UUID | None = None,
    rid: UUID | None = None,
) -> schemas.Accepted:
    operation = "create" if sid is None else "steer" if rid else "run"
    resource = str(rid or sid or "sessions")
    digest = fingerprint(request)
    async with db.begin():
        await advisory(db, lock_key(f"{operation}:{resource}:{key}"))
        prior = await db.get(IdempotencyRecord, (operation, resource, key))
        if prior:
            same = prior.fingerprint == digest
            if same and isinstance(request, schemas.CreateSession):
                record = await get_session(db, prior.session_id)
                try:
                    same = (
                        cipher.decrypt(record.id, record.env_ciphertext)
                        == request.configuration.sandbox.env
                    )
                except InvalidToken, ValueError:
                    raise APIError(
                        503,
                        "storage_unavailable",
                        "Stored environment cannot be decrypted.",
                    ) from None
            if not same:
                raise APIError(
                    409,
                    "idempotency_conflict",
                    "Idempotency key was used for another request.",
                )
            return schemas.Accepted(
                session_id=prior.session_id,
                run_id=prior.run_id,
                message_id=prior.message_id,
            )
        if rid is None:
            await advisory(db, CAPACITY_LOCK)
        if sid is None:
            assert isinstance(request, schemas.CreateSession)
            configuration = resolve(
                request.configuration, profiles, settings.harness_env_allowlist
            )
            sid = uuid4()
            session = SessionRecord(
                id=sid,
                configuration=configuration.model_dump(mode="json"),
                env_ciphertext=cipher.encrypt(sid, request.configuration.sandbox.env),
                slot_reserved=False,
                next_run_number=1,
                next_event_sequence=1,
            )
            db.add(session)
            await db.flush()
        else:
            session = await get_session(db, sid, lock=True)
        if rid:
            run = await get_run(db, sid, rid)
            if run.status != "running" or run.cancel_requested_at:
                raise APIError(
                    409, "run_not_accepting_messages", "Run is not accepting messages."
                )
        else:
            if session.sandbox_state == "unavailable":
                raise APIError(
                    409, "session_unavailable", "Session environment is unavailable."
                )
            if await active_run(db, sid):
                raise APIError(
                    409, "session_busy", "Session already has an unfinished run."
                )
            if not session.slot_reserved:
                count = await db.scalar(
                    select(func.count())
                    .select_from(SessionRecord)
                    .where(SessionRecord.slot_reserved)
                )
                if (count or 0) >= settings.max_concurrent_sessions:
                    raise APIError(
                        503, "capacity_exhausted", "Concurrent session limit reached."
                    )
                session.slot_reserved = True
            run = RunRecord(
                id=uuid4(),
                session_id=sid,
                number=session.next_run_number,
                status="accepted",
                next_delivery_number=1,
            )
            session.next_run_number += 1
            db.add(run)
            await db.flush()
        message = MessageRecord(
            id=uuid4(),
            session_id=sid,
            run_id=run.id,
            role="user",
            text=request.message.text,
            delivery_status="pending",
            delivery_number=run.next_delivery_number,
            registered_sequence=0,
        )
        run.next_delivery_number += 1
        db.add(message)
        await publish_message(db, session, message)
        if rid is None:
            await publish_run(db, session, run)
        result = schemas.Accepted(session_id=sid, run_id=run.id, message_id=message.id)
        db.add(
            IdempotencyRecord(
                operation=operation,
                resource=resource,
                key=key,
                fingerprint=digest,
                **result.model_dump(),
            )
        )
    return result


async def cancel(db: AsyncSession, sid: UUID, rid: UUID) -> schemas.Cancelled:
    async with db.begin():
        await advisory(db, CAPACITY_LOCK)
        session = await get_session(db, sid, lock=True)
        run = await get_run(db, sid, rid)
        if run.status not in TERMINAL and run.cancel_requested_at is None:
            run.cancel_requested_at = now()
            run.stop_reason = "user_request"
            if run.status == "accepted":
                await finish(db, session, run, "cancelled")
                if session.sandbox_state in ("not_created", "paused"):
                    session.slot_reserved = False
            else:
                run.status = "cancelling"
                await publish_run(db, session, run)
        return schemas.Cancelled.model_validate({"run_id": rid, "status": run.status})


async def finish(
    db: AsyncSession,
    session: SessionRecord,
    run: RunRecord,
    status: str,
    error: Error | None = None,
    stop_method: str | None = None,
) -> None:
    answers = list(
        await db.scalars(
            select(MessageRecord).where(
                MessageRecord.run_id == run.id,
                MessageRecord.role == "assistant",
                MessageRecord.kind == "answer",
            )
        )
    )
    answers.sort(
        key=lambda m: ((m.position or {}).get("item_index", -1), m.registered_sequence)
    )
    run.final_message_id = answers[-1].id if answers else None
    run.status, run.finished_at, run.observation = status, now(), "attached"
    run.error = error.model_dump(mode="json") if error else None
    if status == "cancelled":
        run.stop_method = stop_method or "graceful"
    for message in await db.scalars(
        select(MessageRecord).where(
            MessageRecord.run_id == run.id, MessageRecord.delivery_status == "pending"
        )
    ):
        message.delivery_status = "rejected"
        message.error = Error(
            code="run_finished_before_delivery", message="Run finished before delivery."
        ).model_dump()
        await publish_message(db, session, message)
    await publish_run(db, session, run)


def encode_cursor(scope: str, position: Any) -> str:
    return (
        base64.urlsafe_b64encode(
            json.dumps([scope, position], separators=(",", ":")).encode()
        )
        .decode()
        .rstrip("=")
    )


def decode_cursor(value: str, scope: str) -> Any:
    try:
        found, position = json.loads(
            base64.b64decode(
                value + "=" * (-len(value) % 4), altchars=b"-_", validate=True
            )
        )
        if found != scope:
            raise ValueError
        return position
    except ValueError, TypeError, UnicodeError:
        raise APIError(422, "invalid_cursor", "Invalid cursor.") from None


async def list_sessions(
    db: AsyncSession, limit: int, cursor: str | None
) -> schemas.Page[schemas.Session]:
    query = (
        select(SessionRecord)
        .order_by(SessionRecord.created_at, SessionRecord.id)
        .limit(limit + 1)
    )
    if cursor:
        try:
            date, ident = decode_cursor(cursor, "sessions")
            query = query.where(
                tuple_(SessionRecord.created_at, SessionRecord.id)
                > tuple_(literal(datetime.fromisoformat(date)), literal(UUID(ident)))
            )
        except TypeError, ValueError:
            raise APIError(422, "invalid_cursor", "Invalid cursor.") from None
    records = list(await db.scalars(query))
    items = [await session_view(db, record) for record in records[:limit]]
    next_cursor = (
        encode_cursor(
            "sessions",
            [records[limit - 1].created_at.isoformat(), str(records[limit - 1].id)],
        )
        if len(records) > limit
        else None
    )
    return schemas.Page(items=items, next_cursor=next_cursor)


async def list_runs(
    db: AsyncSession, sid: UUID, limit: int, cursor: str | None
) -> schemas.Page[schemas.Run]:
    await get_session(db, sid)
    position = decode_cursor(cursor, f"runs:{sid}") if cursor else 0
    if type(position) is not int or position < 0:
        raise APIError(422, "invalid_cursor", "Invalid cursor.")
    records = list(
        await db.scalars(
            select(RunRecord)
            .where(RunRecord.session_id == sid, RunRecord.number > position)
            .order_by(RunRecord.number)
            .limit(limit + 1)
        )
    )
    return schemas.Page(
        items=[await run_view(db, r) for r in records[:limit]],
        next_cursor=encode_cursor(f"runs:{sid}", records[limit - 1].number)
        if len(records) > limit
        else None,
    )


async def events(
    db: AsyncSession, sid: UUID, after: str, limit: int
) -> schemas.EventPage:
    session = await get_session(db, sid)
    if (
        not after.isascii()
        or not after.isdecimal()
        or len(after) > 19
        or int(after) >= session.next_event_sequence
    ):
        raise APIError(422, "invalid_cursor", "Invalid event cursor.")
    records = list(
        await db.scalars(
            select(EventRecord)
            .where(EventRecord.session_id == sid, EventRecord.sequence > int(after))
            .order_by(EventRecord.sequence)
            .limit(limit + 1)
        )
    )
    return schemas.EventPage(
        items=[
            schemas.Event(
                id=str(r.sequence),
                session_id=sid,
                type=r.type,  # ty: ignore[invalid-argument-type] - event names are validated on publication
                created_at=r.created_at,
                data=r.data,
            )
            for r in records[:limit]
        ],
        next_cursor=str(records[min(limit, len(records)) - 1].sequence)
        if records
        else after,
        has_more=len(records) > limit,
    )


async def history(
    db: AsyncSession, sid: UUID, run_id: UUID | None, limit: int, cursor: str | None
) -> schemas.HistoryPage:
    session = await get_session(db, sid)
    if run_id:
        await get_run(db, sid, run_id)
    scope = f"history:{sid}:{run_id}"
    watermark, last = session.next_event_sequence - 1, None
    if cursor:
        try:
            watermark, last = decode_cursor(cursor, scope)
            if (
                type(watermark) is not int
                or not 0 <= watermark < session.next_event_sequence
                or not isinstance(last, list)
                or len(last) != 4
                or any(type(v) is not int or v < 0 for v in last)
            ):
                raise ValueError
        except ValueError, TypeError:
            raise APIError(422, "invalid_cursor", "Invalid cursor.") from None
    # DISTINCT ON reconstructs each object at the committed watermark. The outer
    # query orders the resulting snapshot, not event registration order.
    statement = text("""
      WITH objects AS (
        SELECT DISTINCT ON (type, data->>'id') type, data
        FROM session_events
        WHERE session_id=:sid AND sequence<=:watermark
          AND type IN ('message.updated','tool_call.updated')
        ORDER BY type, data->>'id', sequence DESC
      ), positioned AS (
        SELECT objects.type, objects.data, runs.number AS rn,
          CASE WHEN objects.data->'position' = 'null'::jsonb THEN 1 ELSE 0 END AS unknown,
          COALESCE((objects.data->'position'->>'item_index')::bigint, 0) AS idx,
          (objects.data->>'registered_sequence')::bigint AS seq
        FROM objects JOIN runs ON runs.id=(objects.data->>'run_id')::uuid
        WHERE (CAST(:rid AS uuid) IS NULL OR runs.id=CAST(:rid AS uuid))
      ) SELECT type,data,rn,unknown,idx,seq FROM positioned
        WHERE (:has_last=false OR (rn,unknown,idx,seq) > (:rn,:unknown,:idx,:seq))
        ORDER BY rn,unknown,idx,seq LIMIT :lim
    """)
    rows = (
        (
            await db.execute(
                statement,
                {
                    "sid": sid,
                    "watermark": watermark,
                    "rid": str(run_id) if run_id else None,
                    "has_last": last is not None,
                    "rn": last[0] if last else 0,
                    "unknown": last[1] if last else 0,
                    "idx": last[2] if last else 0,
                    "seq": last[3] if last else 0,
                    "lim": limit + 1,
                },
            )
        )
        .mappings()
        .all()
    )
    items = [
        schemas.MessageItem(message=schemas.Message.model_validate(r["data"]))
        if r["type"] == "message.updated"
        else schemas.ToolItem(tool_call=schemas.ToolCall.model_validate(r["data"]))
        for r in rows[:limit]
    ]
    next_cursor = (
        encode_cursor(
            scope,
            [watermark, [rows[limit - 1][k] for k in ("rn", "unknown", "idx", "seq")]],
        )
        if len(rows) > limit
        else None
    )
    return schemas.HistoryPage(
        items=items, next_cursor=next_cursor, event_cursor=str(watermark)
    )
