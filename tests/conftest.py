import os
from collections.abc import AsyncIterator
from pathlib import Path
from uuid import uuid4

import httpx2
import pytest
from cryptography.fernet import Fernet
from pydantic import SecretStr
from sqlalchemy import text
from sqlalchemy.ext.asyncio import async_sessionmaker, create_async_engine

import orpheus.models  # noqa: F401
from orpheus.app import create_app
from orpheus.database import Base
from orpheus.settings import Settings


@pytest.fixture
def config_file(tmp_path: Path) -> Path:
    path = tmp_path / "orpheus.toml"
    path.write_text(
        '[profiles.default]\nharness="codex"\nmodel="fixture-model"\n[profiles.default.auth]\nmode="api_key"\napi_key_env="CODEX_API_KEY"\n'
    )
    return path


@pytest.fixture
def settings(config_file: Path) -> Settings:
    return Settings(
        database_url="postgresql+psycopg://test:test@127.0.0.1:1/test",
        public_api_keys=[SecretStr("test-key")],
        env_encryption_key=SecretStr(Fernet.generate_key().decode()),
        orpheus_config_file=config_file,
        harness_env_allowlist=["GITHUB_TOKEN"],
        worker_poll_seconds=0.01,
        rpc_timeout_seconds=1,
        cancel_grace_seconds=0.05,
    )


@pytest.fixture
async def database(settings: Settings):
    url = os.environ.get("TEST_DATABASE_URL")
    if not url:
        pytest.skip("TEST_DATABASE_URL is required (make test supplies PostgreSQL 16)")
    admin = create_async_engine(url)
    schema = "test_" + uuid4().hex
    async with admin.begin() as connection:
        await connection.execute(text(f'CREATE SCHEMA "{schema}"'))
    settings.database_url = url + f"?options=-csearch_path%3D{schema}"
    engine = create_async_engine(settings.database_url)
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.create_all)
    sessions = async_sessionmaker(engine, expire_on_commit=False)
    try:
        yield sessions
    finally:
        await engine.dispose()
        async with admin.begin() as connection:
            await connection.execute(text(f'DROP SCHEMA "{schema}" CASCADE'))
        await admin.dispose()


@pytest.fixture
async def api(settings: Settings, database) -> AsyncIterator[httpx2.AsyncClient]:
    app = create_app(settings)
    async with (
        app.router.lifespan_context(app),
        httpx2.AsyncClient(
            transport=httpx2.ASGITransport(app=app),
            base_url="http://test",
            headers={"Authorization": "Bearer test-key"},
        ) as client,
    ):
        yield client


def session_body(text: str = "Perform a task", env: dict | None = None) -> dict:
    return {
        "configuration": {
            "agent": {"profile": "default"},
            "sandbox": {"template": "codex", "env": env or {}},
        },
        "message": {"text": text},
    }


@pytest.fixture
def required_env():
    def read(name: str) -> str:
        value = os.environ.get(name)
        if not value:
            pytest.fail(f"{name} is required for this test", pytrace=False)
        return value

    return read
