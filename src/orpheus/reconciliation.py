"""Merge harness snapshots by native identity under the parent-session lock."""

from uuid import uuid4

from sqlalchemy import String, Uuid, any_, literal, or_, select
from sqlalchemy.dialects.postgresql import ARRAY
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy.orm import defer, undefer

from orpheus.errors import Error
from orpheus.harnesses.base import Item, Snapshot
from orpheus.models import (
    ACTIVE,
    MessageRecord,
    OperationRecord,
    RunRecord,
    SessionRecord,
    ToolCallRecord,
    now,
)
from orpheus.store import (
    emit,
    finish,
    message_view,
    publish_run,
    tool_view,
)


def replaces_result(item: Item, tool: ToolCallRecord | None, has_result: bool) -> bool:
    # A byte cap can truncate an incomplete source too. Only fully read sources
    # supply original_bytes; abbreviated snapshots must not degrade saved output.
    return (
        not has_result
        or item.completeness == "complete"
        or (
            item.truncation_reason == "orpheus_limit"
            and item.result is not None
            and item.result.get("original_bytes") is not None
            and tool is not None
            and tool.output_completeness != "complete"
        )
    )


def message_state(message: MessageRecord) -> tuple:
    return (
        message.native_key,
        message.position,
        message.text,
        message.kind,
        message.delivery_status,
        message.error,
    )


def tool_state(tool: ToolCallRecord) -> tuple:
    return (tool.position, tool.name, tool.input, tool.status)


async def reconcile(
    db: AsyncSession, session: SessionRecord, snapshot: Snapshot
) -> None:
    runs = list(
        await db.scalars(
            select(RunRecord)
            .where(
                RunRecord.session_id == session.id,
                or_(
                    RunRecord.status.in_(ACTIVE),
                    RunRecord.native_turn_id
                    == any_(
                        literal(
                            [t.native_id for t in snapshot.turns], type_=ARRAY(String)
                        )
                    ),
                ),
            )
            .order_by(RunRecord.number)
        )
    )
    run_numbers = {r.native_turn_id: r.number for r in runs}
    message_keys, tool_items = set(), {}
    for turn in snapshot.turns:
        for item in turn.items:
            key = f"{session.thread_id}:{turn.native_id}:{item.native_id}"
            if item.type in ("user", "assistant"):
                message_keys.add(key)
            else:
                tool_items[key] = (turn.native_id, item)
    messages = list(
        await db.scalars(
            select(MessageRecord).where(
                MessageRecord.session_id == session.id,
                or_(
                    MessageRecord.native_key
                    == any_(literal(list(message_keys), type_=ARRAY(String))),
                    MessageRecord.native_key.is_(None),
                ),
            )
        )
    )
    messages_by_id = {m.id: m for m in messages}
    messages_by_native = {m.native_key: m for m in messages if m.native_key}
    # Inspect lightweight metadata first. Reading an unchanged native tail must
    # not transfer every previously saved JSONB output back into the worker.
    rows = (
        await db.execute(
            select(ToolCallRecord, ToolCallRecord.result["type"].astext)
            .options(defer(ToolCallRecord.result, raiseload=True))
            .where(
                ToolCallRecord.session_id == session.id,
                ToolCallRecord.native_key
                == any_(literal(list(tool_items), type_=ARRAY(String))),
            )
        )
    ).all()
    tools_by_native = {tool.native_key: tool for tool, _ in rows}
    has_results = {tool.native_key: kind is not None for tool, kind in rows}
    load_results = []
    for key, (tid, item) in tool_items.items():
        tool = tools_by_native.get(key)
        if (
            tool is not None
            and has_results[key]
            and (
                replaces_result(item, tool, True)
                or tool_state(tool)
                != (
                    {"run_number": run_numbers.get(tid), "item_index": item.index},
                    item.name,
                    item.input,
                    item.status,
                )
            )
        ):
            load_results.append(tool.id)
    if load_results:
        await db.scalars(
            select(ToolCallRecord)
            .options(undefer(ToolCallRecord.result))
            .where(ToolCallRecord.id == any_(literal(load_results, type_=ARRAY(Uuid))))
        )
    operations = list(
        await db.scalars(
            select(OperationRecord)
            .where(
                OperationRecord.session_id == session.id,
                OperationRecord.kind.in_(("start", "steer")),
                OperationRecord.run_id
                == any_(literal([r.id for r in runs], type_=ARRAY(Uuid))),
            )
            .order_by(OperationRecord.created_at)
        )
    )
    operations_by_message = {o.message_id: o for o in operations}
    starts = {
        o.run_id: o
        for o in operations
        if o.kind == "start" and o.status in ("sending", "uncertain", "confirmed")
    }
    by_native = {run.native_turn_id: run for run in runs if run.native_turn_id}
    for run in runs:
        if run.native_turn_id or run.status not in ACTIVE:
            continue
        operation = starts.get(run.id)
        if operation is None:
            continue
        initial = messages_by_id.get(operation.message_id)
        assert initial is not None
        previous = operation.parameters.get("previous_turns", [])
        candidates = [
            turn
            for turn in snapshot.turns
            if turn.native_id not in previous
            and any(
                item.type == "user" and item.text == initial.text for item in turn.items
            )
        ]
        if len(candidates) == 1:
            run.native_turn_id = candidates[0].native_id
            operation.status, operation.result = (
                "confirmed",
                {"turn_id": run.native_turn_id},
            )
            by_native[candidates[0].native_id] = run
    for turn in snapshot.turns:
        run = by_native.get(turn.native_id)
        if run is None:
            continue
        old_run = (run.status, run.observation)
        for item in turn.items:
            native_key = f"{session.thread_id}:{turn.native_id}:{item.native_id}"
            position = {"run_number": run.number, "item_index": item.index}
            if item.type in ("user", "assistant"):
                message = messages_by_native.get(native_key)
                if message is None and item.type == "user":
                    # One in-flight delivery. Match only after the durable pre-send
                    # boundary, so equal texts from different requests stay distinct.
                    candidates = sorted(
                        (
                            m
                            for m in messages
                            if m.run_id == run.id
                            and m.role == "user"
                            and m.native_key is None
                            and m.delivery_status
                            in ("sending", "uncertain", "delivered")
                        ),
                        key=lambda m: m.delivery_number or 0,
                    )
                    if candidates and candidates[0].text == item.text:
                        candidate = candidates[0]
                        operation = operations_by_message.get(candidate.id)
                        if operation and item.native_id not in operation.parameters.get(
                            "previous_items", []
                        ):
                            message = candidate
                            operation.status = "confirmed"
                if message is None:
                    if item.type == "user":
                        continue
                    message = MessageRecord(
                        id=uuid4(),
                        created_at=now(),
                        session_id=session.id,
                        run_id=run.id,
                        role="assistant",
                        kind=item.kind,
                        text=item.text,
                        registered_sequence=0,
                    )
                    db.add(message)
                    before = None
                else:
                    before = message_state(message)
                message.native_key, message.position = native_key, position
                messages_by_native[native_key] = message
                if message.role == "user":
                    message.delivery_status = "delivered"
                    message.error = None
                else:
                    message.text, message.kind = item.text, item.kind
                if before is None or before != message_state(message):
                    if not message.registered_sequence:
                        message.registered_sequence = session.next_event_sequence
                    await emit(
                        db,
                        session,
                        "message.updated",
                        message_view(message).model_dump(mode="json"),
                        flush=False,
                    )
            else:
                tool = tools_by_native.get(native_key)
                before = tool_state(tool) if tool else None
                had_result = has_results.get(native_key, False)
                replace = replaces_result(item, tool, had_result)
                result_changed = False
                if tool is None:
                    tool = ToolCallRecord(
                        id=uuid4(),
                        created_at=now(),
                        session_id=session.id,
                        run_id=run.id,
                        native_key=native_key,
                        registered_sequence=session.next_event_sequence,
                    )
                    db.add(tool)
                    tools_by_native[native_key] = tool
                tool.position, tool.name, tool.input, tool.status = (
                    position,
                    item.name,
                    item.input,
                    item.status,
                )
                if replace:
                    previous = (
                        tool.result if had_result else None,
                        tool.output_completeness,
                        tool.truncation_reason,
                    )
                    tool.result, tool.output_completeness, tool.truncation_reason = (
                        item.result,
                        item.completeness,
                        item.truncation_reason,
                    )
                    result_changed = previous != (
                        tool.result,
                        tool.output_completeness,
                        tool.truncation_reason,
                    )
                    has_results[native_key] = tool.result is not None
                if before != tool_state(tool) or result_changed:
                    await emit(
                        db,
                        session,
                        "tool_call.updated",
                        tool_view(tool).model_dump(mode="json"),
                        flush=False,
                    )
        if run.status in ACTIVE:
            if turn.status in ("completed", "failed", "cancelled"):
                error = (
                    Error(
                        code=turn.error_code or "harness_failed",
                        message="Harness execution failed.",
                        phase="execution",
                    )
                    if turn.status == "failed"
                    else None
                )
                await finish(db, session, run, turn.status, error)
            else:
                if run.status != "cancelling":
                    run.status = "running"
                pending = any(
                    o.run_id == run.id and o.status in ("sending", "uncertain")
                    for o in operations
                )
                run.observation = "uncertain" if pending else "attached"
                if old_run != (run.status, run.observation):
                    await publish_run(db, session, run)
    session.history_path, session.history_offset = snapshot.path, snapshot.offset
    await db.flush()
