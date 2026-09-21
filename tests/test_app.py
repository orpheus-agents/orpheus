import asyncio
import json
from collections.abc import AsyncIterator, Iterator
from pathlib import Path
from unittest.mock import AsyncMock

import pytest
from fastapi.testclient import TestClient
from sqlalchemy.exc import OperationalError
from sqlalchemy.ext.asyncio import AsyncSession

from orpheus.app import create_app
from orpheus.database import get_session
from orpheus.settings import Settings


@pytest.fixture
def session() -> AsyncMock:
    return AsyncMock(spec=AsyncSession)


@pytest.fixture
def client(session: AsyncMock, settings: Settings) -> Iterator[TestClient]:
    app = create_app(settings.model_copy(update={"readiness_timeout": 0.02}))

    async def override_session() -> AsyncIterator[AsyncSession]:
        yield session

    app.dependency_overrides[get_session] = override_session
    with TestClient(app) as client:
        yield client


def test_health_does_not_query_database(client: TestClient, session: AsyncMock) -> None:
    response = client.get("/health")
    assert response.status_code == 200
    assert response.json() == {"status": "ok"}
    session.execute.assert_not_awaited()


def test_readiness_recovers_after_database_failure(
    client: TestClient, session: AsyncMock
) -> None:
    session.execute.side_effect = OperationalError(
        None, None, Exception("DB unavailable")
    )
    response = client.get("/ready")
    assert response.status_code == 503
    assert response.json() == {"status": "unavailable"}
    assert client.get("/health").status_code == 200

    session.execute.side_effect = None
    response = client.get("/ready")
    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


def test_readiness_times_out(client: TestClient, session: AsyncMock) -> None:
    async def slow_query(*args: object, **kwargs: object) -> None:
        await asyncio.sleep(10)

    session.execute.side_effect = slow_query
    assert client.get("/ready").status_code == 503


@pytest.mark.parametrize("path", ["/docs", "/redoc", "/docs/oauth2-redirect"])
def test_documentation_pages_are_disabled(client: TestClient, path: str) -> None:
    assert client.get(path).status_code == 404


def test_openapi_matches_export(client: TestClient, session: AsyncMock) -> None:
    schema_path = Path(__file__).resolve().parents[1] / "openapi.json"
    response = client.get("/openapi.json")
    assert response.status_code == 200
    assert response.json() == json.loads(schema_path.read_text(encoding="utf-8"))
    session.execute.assert_not_awaited()
