import asyncio
import logging
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Annotated, Literal, cast

from fastapi import Depends, FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse
from pydantic import BaseModel
from sqlalchemy import text
from sqlalchemy.exc import SQLAlchemyError
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker, create_async_engine

from orpheus import __version__
from orpheus.api import RequestBoundary, router
from orpheus.database import Database, get_session
from orpheus.errors import APIError, Error, ErrorDetail
from orpheus.settings import Settings

logger = logging.getLogger(__name__)


class HealthResponse(BaseModel):
    status: Literal["ok"] = "ok"


class UnavailableResponse(BaseModel):
    status: Literal["unavailable"] = "unavailable"


def create_app(settings: Settings | None = None) -> FastAPI:
    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        config = settings if settings is not None else Settings()
        profiles, cipher = config.runtime(api=True)
        app.state.profiles = profiles
        app.state.cipher = cipher
        engine = create_async_engine(config.database_url, pool_pre_ping=True)
        app.state.settings = config
        app.state.database = Database(
            engine=engine,
            sessions=async_sessionmaker(engine, expire_on_commit=False),
        )
        try:
            yield
        finally:
            await engine.dispose()

    app = FastAPI(
        title="Orpheus",
        version=__version__,
        lifespan=lifespan,
        docs_url=None,
        redoc_url=None,
    )

    @app.get("/health", operation_id="health", response_model=HealthResponse)
    async def health() -> HealthResponse:
        return HealthResponse()

    @app.get(
        "/ready",
        operation_id="ready",
        response_model=HealthResponse,
        responses={503: {"model": UnavailableResponse}},
    )
    async def ready(
        request: Request,
        session: Annotated[AsyncSession, Depends(get_session)],
    ) -> HealthResponse | JSONResponse:
        config = cast(Settings, request.app.state.settings)
        try:
            async with asyncio.timeout(config.readiness_timeout):
                await session.execute(text("SELECT 1"))
        except SQLAlchemyError, OSError, TimeoutError:
            logger.warning("Database readiness check failed")
            return JSONResponse(
                status_code=503,
                content=UnavailableResponse().model_dump(),
            )
        return HealthResponse()

    app.include_router(router)
    app.add_middleware(RequestBoundary)

    @app.exception_handler(APIError)
    async def api_error(request: Request, error: APIError) -> JSONResponse:
        return JSONResponse(
            status_code=error.status,
            content={"error": error.error.model_dump()},
            headers={"WWW-Authenticate": "Bearer"} if error.status == 401 else None,
        )

    @app.exception_handler(RequestValidationError)
    async def validation_error(
        request: Request, error: RequestValidationError
    ) -> JSONResponse:
        details = [
            ErrorDetail(path=list(item["loc"]), code=item["type"])
            for item in error.errors()
        ]
        return JSONResponse(
            status_code=422,
            content={
                "error": Error(
                    code="validation_error",
                    message="Request validation failed.",
                    details=details,
                ).model_dump()
            },
        )

    @app.exception_handler(SQLAlchemyError)
    async def storage_error(request: Request, error: SQLAlchemyError) -> JSONResponse:
        logger.warning("Storage operation failed (%s)", type(error).__name__)
        return JSONResponse(
            status_code=503,
            content={
                "error": Error(
                    code="storage_unavailable",
                    message="Storage is temporarily unavailable.",
                ).model_dump()
            },
        )

    return app
