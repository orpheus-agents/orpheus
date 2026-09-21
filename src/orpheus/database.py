from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass
from typing import cast

from fastapi import Request
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncEngine, AsyncSession, async_sessionmaker
from sqlalchemy.orm import DeclarativeBase


class Base(DeclarativeBase):
    pass


@dataclass
class Database:
    engine: AsyncEngine
    sessions: async_sessionmaker[AsyncSession]


async def get_session(request: Request) -> AsyncIterator[AsyncSession]:
    database = cast(Database, request.app.state.database)
    async with database.sessions() as session:
        yield session


@asynccontextmanager
async def read_snapshot(database: Database) -> AsyncIterator[AsyncSession]:
    async with database.sessions() as session:
        await session.execute(
            text("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY")
        )
        yield session


async def get_read_session(request: Request) -> AsyncIterator[AsyncSession]:
    async with read_snapshot(request.app.state.database) as session:
        yield session
