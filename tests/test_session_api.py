import asyncio
import json
from uuid import UUID, uuid4

import pytest
from conftest import session_body
from sqlalchemy import func, select

from orpheus.errors import Error
from orpheus.models import (
    EventRecord,
    IdempotencyRecord,
    MessageRecord,
    RunRecord,
    SessionRecord,
)
from orpheus.store import finish, get_run, get_session, publish_message, publish_run


async def create(api, body=None, key=None):
    return await api.post(
        "/api/v1/sessions",
        json=body or session_body(),
        headers={"Idempotency-Key": str(key or uuid4())},
    )


async def test_create_poll_and_secret_redaction(api, database):
    response = await create(api, session_body(env={"TOKEN": "sensitive"}))
    assert response.status_code == 202, response.text
    ids = response.json()
    session = (await api.get(f"/api/v1/sessions/{ids['session_id']}")).json()
    assert session["status"] == "accepted" and session["final_message"] is None
    assert session["configuration"]["sandbox"]["env_names"] == ["TOKEN"]
    assert "sensitive" not in json.dumps(session)
    history = (await api.get(f"/api/v1/sessions/{ids['session_id']}/history")).json()
    assert history["items"][0]["type"] == "message"
    assert history["items"][0]["message"]["delivery_status"] == "pending"
    async with database() as db:
        row = await db.get(SessionRecord, UUID(ids["session_id"]))
        assert "sensitive" not in row.env_ciphertext
        assert row.slot_reserved


async def test_concurrent_idempotency_and_secret_comparison(api, database, settings):
    key = uuid4()
    responses = await asyncio.gather(
        *(create(api, session_body(env={"TOKEN": "a"}), key) for _ in range(5))
    )
    assert {r.status_code for r in responses} == {202}
    assert len({r.text for r in responses}) == 1
    settings.max_concurrent_sessions = 1
    settings.orpheus_config_file.write_text("broken TOML")
    assert (await create(api, session_body(env={"TOKEN": "a"}), key)).status_code == 202
    assert (await create(api, session_body(env={"TOKEN": "b"}), key)).status_code == 409
    async with database() as db:
        assert await db.scalar(select(func.count()).select_from(RunRecord)) == 1
        assert await db.scalar(select(func.count()).select_from(IdempotencyRecord)) == 1


async def test_capacity_admission_is_atomic(api, settings, database):
    settings.max_concurrent_sessions = 1
    results = await asyncio.gather(create(api), create(api))
    assert sorted(r.status_code for r in results) == [202, 503]
    async with database() as db:
        assert await db.scalar(select(func.count()).select_from(SessionRecord)) == 1


async def test_busy_steer_and_cancel_contract(api, database):
    ids = (await create(api)).json()
    sid, rid = ids["session_id"], ids["run_id"]
    root = f"/api/v1/sessions/{sid}/runs"
    headers = {"Idempotency-Key": str(uuid4())}
    assert (
        await api.post(root, json={"message": {"text": "next"}}, headers=headers)
    ).status_code == 409
    assert (
        await api.post(
            f"{root}/{rid}/messages",
            json={"message": {"text": "steer"}},
            headers=headers,
        )
    ).status_code == 409
    async with database.begin() as db:
        session = await get_session(db, UUID(sid), lock=True)
        run = await get_run(db, UUID(sid), UUID(rid))
        run.status = "running"
        await publish_run(db, session, run)
    sent = await api.post(
        f"{root}/{rid}/messages", json={"message": {"text": "steer"}}, headers=headers
    )
    assert sent.status_code == 202
    assert (await api.post(f"{root}/{rid}/cancel")).status_code == 202
    first = (await api.get(f"{root}/{rid}")).json()["cancel_requested_at"]
    assert (await api.post(f"{root}/{rid}/cancel")).status_code == 202
    assert (await api.get(f"{root}/{rid}")).json()["cancel_requested_at"] == first
    assert (
        await api.post(
            f"{root}/{rid}/messages",
            json={"message": {"text": "steer"}},
            headers=headers,
        )
    ).json() == sent.json()
    assert (
        await api.post(
            f"{root}/{rid}/messages",
            json={"message": {"text": "another"}},
            headers={"Idempotency-Key": str(uuid4())},
        )
    ).status_code == 409
    assert (await api.get(f"{root}/{uuid4()}")).status_code == 404


async def test_final_message_atomic_projection_and_next_run(api, database):
    ids = (await create(api)).json()
    sid, rid = UUID(ids["session_id"]), UUID(ids["run_id"])
    async with database.begin() as db:
        session = await get_session(db, sid, lock=True)
        run = await get_run(db, sid, rid)
        message = MessageRecord(
            session_id=sid,
            run_id=rid,
            role="assistant",
            kind="answer",
            text="Done",
            registered_sequence=0,
            position={"run_number": 1, "item_index": 1},
        )
        db.add(message)
        await publish_message(db, session, message)
        await finish(db, session, run, "completed")
    root = f"/api/v1/sessions/{sid}"
    view = (await api.get(root)).json()
    assert view["status"] == "completed" and view["final_message"]["text"] == "Done"
    assert (await api.get(root + "/runs")).json()["items"][0]["final_message"] == view[
        "final_message"
    ]
    assert (await api.post(root + f"/runs/{rid}/cancel")).status_code == 200
    assert (
        await api.post(
            root + "/runs",
            json={"message": {"text": "next"}},
            headers={"Idempotency-Key": str(uuid4())},
        )
    ).status_code == 202
    view = (await api.get(root)).json()
    assert view["status"] == "accepted" and view["final_message"] is None
    assert (await api.get(root + f"/runs/{rid}")).json()["final_message"][
        "text"
    ] == "Done"


async def test_snapshot_pagination_and_event_backfill(api, database):
    ids = (await create(api)).json()
    sid, rid = UUID(ids["session_id"]), UUID(ids["run_id"])
    async with database.begin() as db:
        session = await get_session(db, sid, lock=True)
        for n in (3, 5):
            message = MessageRecord(
                session_id=sid,
                run_id=rid,
                role="assistant",
                kind="progress",
                text=str(n),
                registered_sequence=0,
                position={"run_number": 1, "item_index": n},
            )
            db.add(message)
            await publish_message(db, session, message)
    root = f"/api/v1/sessions/{sid}"
    first = (await api.get(root + "/history", params={"limit": 1})).json()
    async with database.begin() as db:
        session = await get_session(db, sid, lock=True)
        message = MessageRecord(
            session_id=sid,
            run_id=rid,
            role="assistant",
            kind="progress",
            text="backfill",
            registered_sequence=0,
            position={"run_number": 1, "item_index": 0},
        )
        db.add(message)
        await publish_message(db, session, message)
    second = (
        await api.get(
            root + "/history", params={"limit": 1, "cursor": first["next_cursor"]}
        )
    ).json()
    assert second["event_cursor"] == first["event_cursor"]
    assert second["items"][0]["message"]["text"] == "5"
    events = (
        await api.get(root + "/events", params={"after": first["event_cursor"]})
    ).json()
    assert events["items"][0]["data"]["text"] == "backfill"
    assert (
        await api.get(root + "/history", params={"cursor": "bad"})
    ).status_code == 422
    assert (
        await api.get(root + "/events/stream", headers={"Last-Event-ID": "99999"})
    ).status_code == 422


@pytest.mark.parametrize(
    "body",
    [
        {
            "configuration": {
                "agent": {"profile": "default"},
                "sandbox": {"template": "codex", "workdir": "/tmp"},
            },
            "message": {"text": "x"},
        },
        {
            "configuration": {
                "agent": {"profile": "default"},
                "sandbox": {"template": "codex"},
                "limits": {"run_timeout_seconds": "1"},
            },
            "message": {"text": "x"},
        },
        session_body(text="  "),
    ],
)
async def test_validation_is_strict_and_safe(api, body):
    response = await create(api, body)
    assert response.status_code == 422
    assert all(set(d) == {"path", "code"} for d in response.json()["error"]["details"])


async def test_request_boundary_and_auth(api, settings):
    headers = {"Idempotency-Key": str(uuid4()), "Content-Type": "application/json"}
    assert (
        await api.post(
            "/api/v1/sessions", content='{"message":1,"message":2}', headers=headers
        )
    ).status_code == 400
    assert (
        await api.post(
            "/api/v1/sessions", content="{}", headers={"Content-Type": "text/plain"}
        )
    ).status_code == 415
    assert (await api.post("/api/v1/sessions", json=session_body())).status_code == 422
    assert (
        await api.get("/api/v1/sessions", headers={"Authorization": "Bearer wrong"})
    ).status_code == 401
    assert (await api.get("/health", headers={"Authorization": ""})).status_code == 200
    settings.max_request_bytes = 8

    async def chunks():
        yield b'{"large":'
        yield b'"content"}'

    assert (
        await api.post("/api/v1/sessions", content=chunks(), headers=headers)
    ).status_code == 413


async def test_error_without_answer_is_null(api, database):
    ids = (await create(api)).json()
    sid, rid = UUID(ids["session_id"]), UUID(ids["run_id"])
    async with database.begin() as db:
        session = await get_session(db, sid, lock=True)
        await finish(
            db,
            session,
            await get_run(db, sid, rid),
            "failed",
            Error(code="harness_failed", message="Failed", phase="execution"),
        )
    view = (await api.get(f"/api/v1/sessions/{sid}")).json()
    assert view["final_message"] is None and view["error"]["code"] == "harness_failed"
    async with database() as db:
        sequences = list(
            await db.scalars(
                select(EventRecord.sequence)
                .where(EventRecord.session_id == sid)
                .order_by(EventRecord.sequence)
            )
        )
        assert sequences == list(range(1, len(sequences) + 1))


async def test_sse_reads_same_journal_and_last_event_id_wins(api, database):
    from types import SimpleNamespace

    from starlette.requests import Request

    from orpheus.api import stream_events
    from orpheus.database import Database

    ids = (await create(api)).json()
    sid = UUID(ids["session_id"])
    page = (await api.get(f"/api/v1/sessions/{sid}/events")).json()
    app = SimpleNamespace(
        state=SimpleNamespace(
            database=Database(engine=database.kw["bind"], sessions=database)
        )
    )

    async def receive():
        await asyncio.sleep(60)
        return {"type": "http.disconnect"}

    request = Request({"type": "http", "app": app, "headers": []}, receive)
    response = await stream_events(sid, request, after="9999", last_event_id="0")
    from typing import Any, cast

    iterator = cast(Any, aiter(response.body_iterator))
    first = await anext(iterator)
    assert first.id == page["items"][0]["id"]
    assert json.loads(first.data) == page["items"][0]
    await iterator.aclose()


async def test_get_endpoints_do_not_wait_for_worker_row_lock(api, database):
    import asyncio

    from conftest import session_body

    from orpheus.store import get_session

    response = await api.post(
        "/api/v1/sessions",
        json=session_body(),
        headers={"Idempotency-Key": str(uuid4())},
    )
    ids = response.json()
    sid, rid = ids["session_id"], ids["run_id"]
    async with database.begin() as db:
        await get_session(db, UUID(sid), lock=True)
        for path in (
            "/sessions",
            f"/sessions/{sid}",
            f"/sessions/{sid}/runs",
            f"/sessions/{sid}/runs/{rid}",
            f"/sessions/{sid}/history",
            f"/sessions/{sid}/events",
        ):
            async with asyncio.timeout(2):
                response = await api.get("/api/v1" + path)
            assert response.status_code == 200


async def test_session_get_uses_one_read_only_snapshot(api, database, monkeypatch):
    import asyncio

    from conftest import session_body
    from sqlalchemy import text

    from orpheus import store

    response = await api.post(
        "/api/v1/sessions",
        json=session_body(),
        headers={"Idempotency-Key": str(uuid4())},
    )
    ids = response.json()
    sid, rid = UUID(ids["session_id"]), UUID(ids["run_id"])
    entered, release = asyncio.Event(), asyncio.Event()
    original = store.session_view

    async def gated(db, record):
        assert await db.scalar(text("SHOW transaction_isolation")) == "repeatable read"
        assert await db.scalar(text("SHOW transaction_read_only")) == "on"
        entered.set()
        await release.wait()
        return await original(db, record)

    monkeypatch.setattr(store, "session_view", gated)
    reading = asyncio.create_task(api.get(f"/api/v1/sessions/{sid}"))
    try:
        async with asyncio.timeout(2):
            await entered.wait()
            async with database.begin() as db:
                session = await store.get_session(db, sid, lock=True)
                run = await store.get_run(db, sid, rid)
                await store.finish(db, session, run, "completed")
        release.set()
        assert (await reading).json()["status"] == "accepted"
        assert (await api.get(f"/api/v1/sessions/{sid}")).json()[
            "status"
        ] == "completed"
    finally:
        release.set()
        await reading
