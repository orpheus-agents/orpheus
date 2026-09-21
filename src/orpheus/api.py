import asyncio
import json
import secrets
from collections.abc import AsyncIterator
from typing import Annotated
from uuid import UUID

from fastapi import APIRouter, Depends, Header, Query, Request, Response
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer
from sqlalchemy.ext.asyncio import AsyncSession
from sse_starlette import EventSourceResponse, ServerSentEvent

from orpheus import schemas, store
from orpheus.database import Database, get_read_session, get_session, read_snapshot
from orpheus.errors import APIError, ErrorResponse
from orpheus.models import TERMINAL

bearer = HTTPBearer(auto_error=False)


async def authorize(
    request: Request,
    credentials: Annotated[HTTPAuthorizationCredentials | None, Depends(bearer)],
) -> None:
    supplied = credentials.credentials.encode() if credentials else b""
    matches = [
        secrets.compare_digest(supplied, key.get_secret_value().encode())
        for key in request.app.state.settings.public_api_keys
    ]
    if not any(matches):
        raise APIError(401, "unauthorized", "A valid Bearer key is required.")


router = APIRouter(
    prefix="/api/v1",
    dependencies=[Depends(authorize)],
    responses={
        code: {"model": ErrorResponse}
        for code in (400, 401, 404, 409, 413, 415, 422, 503)
    },
)
DB = Annotated[AsyncSession, Depends(get_session)]
ReadDB = Annotated[AsyncSession, Depends(get_read_session)]
Limit = Annotated[int, Query(ge=1, le=200)]


def idempotency_key(
    value: Annotated[str | None, Header(alias="Idempotency-Key")] = None,
) -> UUID:
    try:
        return UUID(value) if value else _missing_key()
    except ValueError:
        return _missing_key()


def _missing_key() -> UUID:
    raise APIError(422, "idempotency_key_required", "Idempotency-Key must be a UUID.")


Key = Annotated[UUID, Depends(idempotency_key)]


def accepted_location(response: Response, result: schemas.Accepted) -> schemas.Accepted:
    response.headers["Location"] = (
        f"/api/v1/sessions/{result.session_id}/runs/{result.run_id}"
    )
    return result


@router.post("/sessions", status_code=202, operation_id="create_session")
async def create_session(
    body: schemas.CreateSession, request: Request, response: Response, db: DB, key: Key
) -> schemas.Accepted:
    state = request.app.state
    return accepted_location(
        response,
        await store.accept(db, state.settings, state.profiles, state.cipher, body, key),
    )


@router.post("/sessions/{sid}/runs", status_code=202, operation_id="create_run")
async def create_run(
    sid: UUID,
    body: schemas.SendMessage,
    request: Request,
    response: Response,
    db: DB,
    key: Key,
) -> schemas.Accepted:
    state = request.app.state
    return accepted_location(
        response,
        await store.accept(
            db, state.settings, state.profiles, state.cipher, body, key, sid=sid
        ),
    )


@router.post(
    "/sessions/{sid}/runs/{rid}/messages", status_code=202, operation_id="send_message"
)
async def send_message(
    sid: UUID,
    rid: UUID,
    body: schemas.SendMessage,
    request: Request,
    response: Response,
    db: DB,
    key: Key,
) -> schemas.Accepted:
    state = request.app.state
    return accepted_location(
        response,
        await store.accept(
            db,
            state.settings,
            state.profiles,
            state.cipher,
            body,
            key,
            sid=sid,
            rid=rid,
        ),
    )


@router.post(
    "/sessions/{sid}/runs/{rid}/cancel",
    status_code=202,
    operation_id="cancel_run",
    responses={200: {"model": schemas.Cancelled}},
)
async def cancel_run(
    sid: UUID, rid: UUID, response: Response, db: DB
) -> schemas.Cancelled:
    result = await store.cancel(db, sid, rid)
    response.status_code = 200 if result.status in TERMINAL else 202
    return result


@router.get("/sessions", operation_id="list_sessions")
async def list_sessions(
    db: ReadDB, limit: Limit = 50, cursor: str | None = None
) -> schemas.Page[schemas.Session]:
    return await store.list_sessions(db, limit, cursor)


@router.get("/sessions/{sid}", operation_id="get_session")
async def read_session(sid: UUID, db: ReadDB) -> schemas.Session:
    return await store.session_view(db, await store.get_session(db, sid))


@router.get("/sessions/{sid}/runs", operation_id="list_runs")
async def list_runs(
    sid: UUID, db: ReadDB, limit: Limit = 50, cursor: str | None = None
) -> schemas.Page[schemas.Run]:
    await store.get_session(db, sid)
    return await store.list_runs(db, sid, limit, cursor)


@router.get("/sessions/{sid}/runs/{rid}", operation_id="get_run")
async def read_run(sid: UUID, rid: UUID, db: ReadDB) -> schemas.Run:
    await store.get_session(db, sid)
    return await store.run_view(db, await store.get_run(db, sid, rid))


@router.get("/sessions/{sid}/history", operation_id="get_history")
async def history(
    sid: UUID,
    db: ReadDB,
    limit: Limit = 50,
    cursor: str | None = None,
    run_id: UUID | None = None,
) -> schemas.HistoryPage:
    return await store.history(db, sid, run_id, limit, cursor)


@router.get("/sessions/{sid}/events", operation_id="get_events")
async def events(
    sid: UUID, db: ReadDB, after: str = "0", limit: Limit = 50
) -> schemas.EventPage:
    return await store.events(db, sid, after, limit)


@router.get(
    "/sessions/{sid}/events/stream",
    operation_id="stream_events",
    response_class=EventSourceResponse,
    responses={200: {"content": {"text/event-stream": {"schema": {"type": "string"}}}}},
)
async def stream_events(
    sid: UUID,
    request: Request,
    after: str = "0",
    last_event_id: Annotated[str | None, Header()] = None,
) -> EventSourceResponse:
    database: Database = request.app.state.database
    cursor = last_event_id if last_event_id is not None else after
    async with read_snapshot(database) as db:
        await store.events(db, sid, cursor, 1)

    async def generate() -> AsyncIterator[ServerSentEvent]:
        nonlocal cursor
        while not await request.is_disconnected():
            async with read_snapshot(database) as db:
                page = await store.events(db, sid, cursor, 100)
            for event in page.items:
                yield ServerSentEvent(
                    id=event.id, event=event.type, data=event.model_dump_json()
                )
            cursor = page.next_cursor
            if not page.has_more:
                if not page.items:
                    yield ServerSentEvent(comment="keep-alive")
                await asyncio.sleep(1)

    return EventSourceResponse(
        generate(),
        media_type="text/event-stream",
        send_timeout=15,
        ping=15,
        headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
    )


class RequestBoundary:
    """Bound bodies before FastAPI buffers them; reject ambiguous JSON objects."""

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if (
            scope["type"] != "http"
            or not scope["path"].startswith("/api/v1")
            or scope["method"] != "POST"
        ):
            return await self.app(scope, receive, send)
        from fastapi.responses import JSONResponse

        from orpheus.errors import Error

        limit = scope["app"].state.settings.max_request_bytes
        body = bytearray()
        while True:
            message = await receive()
            if message["type"] == "http.disconnect":
                return
            body.extend(message.get("body", b""))
            if len(body) > limit:
                response = JSONResponse(
                    status_code=413,
                    content={
                        "error": Error(
                            code="request_too_large",
                            message="Request body is too large.",
                        ).model_dump()
                    },
                )
                return await response(scope, receive, send)
            if not message.get("more_body"):
                break
        if body:
            headers = dict(scope["headers"])
            media = headers.get(b"content-type", b"").split(b";")[0].strip().lower()
            status, code = 400, "invalid_json"
            try:
                if media != b"application/json":
                    status, code = 415, "unsupported_media_type"
                    raise ValueError
                if scope["path"].endswith("/cancel"):
                    raise ValueError

                def pairs(items):
                    result = {}
                    for key, value in items:
                        if key in result:
                            raise ValueError
                        result[key] = value
                    return result

                def constant(value):
                    raise ValueError

                json.loads(body, object_pairs_hook=pairs, parse_constant=constant)
            except ValueError, UnicodeError, RecursionError:
                response = JSONResponse(
                    status_code=status,
                    content={
                        "error": Error(
                            code=code, message="Invalid request body."
                        ).model_dump()
                    },
                )
                return await response(scope, receive, send)
        delivered = False

        async def replay():
            nonlocal delivered
            if not delivered:
                delivered = True
                return {"type": "http.request", "body": bytes(body), "more_body": False}
            return await receive()

        return await self.app(scope, replay, send)
