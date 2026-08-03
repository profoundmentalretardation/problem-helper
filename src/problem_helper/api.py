"""HTTP layer: queues a session in the background and serves its result."""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any
from uuid import uuid4

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import FileResponse

from .config import Settings, get_settings
from .db import Database
from .llm import LLMClient
from .orchestrator import process_session
from .schemas import (
    SessionCreated,
    SessionDebugView,
    SessionStatus,
    SessionView,
    SolveRequest,
)

logger = logging.getLogger(__name__)

Processor = Callable[[str, SolveRequest], Awaitable[None]]

_INDEX_PAGE = Path(__file__).parent / "static" / "index.html"


def create_app(
    settings: Settings | None = None,
    *,
    db: Database | None = None,
    processor: Processor | None = None,
) -> FastAPI:
    """Application factory. `db`/`processor` are replaced in tests."""
    settings = settings or get_settings()

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        owns_db = db is None
        database = db or await Database(settings.db_path).connect()
        llm = LLMClient(settings) if processor is None else None

        async def default_processor(session_id: str, request: SolveRequest) -> None:
            assert llm is not None
            await process_session(
                session_id, request, db=database, llm=llm, settings=settings
            )

        app.state.settings = settings
        app.state.db = database
        app.state.processor = processor or default_processor
        app.state.tasks = set()
        try:
            yield
        finally:
            for task in list(app.state.tasks):
                task.cancel()
            if llm is not None:
                await llm.close()
            if owns_db:
                await database.close()

    app = FastAPI(title="problem-helper", version="0.1.0", lifespan=lifespan)

    @app.get("/", include_in_schema=False)
    async def index() -> FileResponse:
        """A no-auth playground page for driving the API by hand."""
        return FileResponse(_INDEX_PAGE, media_type="text/html")

    @app.get("/health")
    async def health() -> dict[str, str]:
        return {"status": "ok"}

    @app.post("/v1/sessions", status_code=202, response_model=SessionCreated)
    async def create_session(payload: SolveRequest, request: Request) -> SessionCreated:
        session_id = uuid4().hex
        await request.app.state.db.create_session(session_id, payload.model_dump())
        _spawn(request.app, request.app.state.processor(session_id, payload))
        logger.info("session %s queued", session_id)
        return SessionCreated(session_id=session_id, status=SessionStatus.pending)

    @app.get("/v1/sessions/{session_id}", response_model=SessionView)
    async def get_session(session_id: str, request: Request) -> SessionView:
        record = await _record(request, session_id)
        return SessionView(**_common(session_id, record))

    @app.get("/v1/sessions/{session_id}/debug", response_model=SessionDebugView)
    async def get_session_debug(session_id: str, request: Request) -> SessionDebugView:
        record = await _record(request, session_id)
        attempts = await request.app.state.db.get_attempts(session_id)
        return SessionDebugView(
            **_common(session_id, record),
            request=record["request"],
            internals=record["internals"],
            attempts=attempts,
        )

    return app


async def _record(request: Request, session_id: str) -> dict[str, Any]:
    record = await request.app.state.db.get_session(session_id)
    if record is None:
        raise HTTPException(status_code=404, detail="session not found")
    return record


def _common(session_id: str, record: dict[str, Any]) -> dict[str, Any]:
    error = None
    if record["error_code"]:
        error = {"code": record["error_code"], "message": record["error_message"]}
    return {
        "session_id": session_id,
        "status": record["status"],
        "stage": record["stage"],
        "created_at": record["created_at"],
        "updated_at": record["updated_at"],
        "result": record["result"],
        "error": error,
    }


def _spawn(app: FastAPI, coro: Awaitable[None]) -> None:
    task = asyncio.ensure_future(coro)
    app.state.tasks.add(task)
    task.add_done_callback(app.state.tasks.discard)


app = create_app()
