"""Storage on plain aiosqlite: a single connection driven by the asyncio loop."""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any, Self

import aiosqlite

from .schemas import ErrorCode, SessionStage, SessionStatus

_SCHEMA = """
CREATE TABLE IF NOT EXISTS sessions (
    id             TEXT PRIMARY KEY,
    status         TEXT NOT NULL,
    stage          TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    request_json   TEXT NOT NULL,
    result_json    TEXT,
    internals_json TEXT,
    error_code     TEXT,
    error_message  TEXT
);

CREATE TABLE IF NOT EXISTS attempts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT NOT NULL,
    kind         TEXT NOT NULL,
    attempt_no   INTEGER NOT NULL,
    payload_json TEXT NOT NULL,
    created_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_attempts_session ON attempts (session_id, id);
"""


def _now() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds")


class Database:
    def __init__(self, path: str) -> None:
        self._path = path
        self._conn: aiosqlite.Connection | None = None

    async def connect(self) -> Self:
        self._conn = await aiosqlite.connect(self._path)
        self._conn.row_factory = aiosqlite.Row
        await self._conn.execute("PRAGMA journal_mode=WAL")
        await self._conn.execute("PRAGMA busy_timeout=5000")
        await self._conn.executescript(_SCHEMA)
        await self._conn.commit()
        return self

    async def close(self) -> None:
        if self._conn is not None:
            await self._conn.close()
            self._conn = None

    @property
    def _db(self) -> aiosqlite.Connection:
        if self._conn is None:
            raise RuntimeError("Database.connect() was never called")
        return self._conn

    async def _write(self, sql: str, params: tuple) -> None:
        await self._db.execute(sql, params)
        await self._db.commit()

    # ------------------------------------------------------------------ #

    async def create_session(self, session_id: str, request: dict) -> None:
        now = _now()
        await self._write(
            "INSERT INTO sessions (id, status, stage, created_at, updated_at, request_json)"
            " VALUES (?, ?, ?, ?, ?, ?)",
            (
                session_id,
                SessionStatus.pending,
                SessionStage.queued,
                now,
                now,
                json.dumps(request, ensure_ascii=False),
            ),
        )

    async def set_stage(
        self, session_id: str, status: SessionStatus, stage: SessionStage
    ) -> None:
        await self._write(
            "UPDATE sessions SET status = ?, stage = ?, updated_at = ? WHERE id = ?",
            (status, stage, _now(), session_id),
        )

    async def finish_success(
        self, session_id: str, result: dict, internals: dict | None = None
    ) -> None:
        await self._write(
            "UPDATE sessions SET status = ?, stage = ?, updated_at = ?, result_json = ?,"
            " internals_json = ? WHERE id = ?",
            (
                SessionStatus.succeeded,
                SessionStage.done,
                _now(),
                json.dumps(result, ensure_ascii=False),
                json.dumps(internals or {}, ensure_ascii=False),
                session_id,
            ),
        )

    async def finish_failure(
        self,
        session_id: str,
        code: ErrorCode,
        message: str,
        internals: dict | None = None,
    ) -> None:
        await self._write(
            "UPDATE sessions SET status = ?, stage = ?, updated_at = ?, error_code = ?,"
            " error_message = ?, internals_json = ? WHERE id = ?",
            (
                SessionStatus.failed,
                SessionStage.done,
                _now(),
                code,
                message,
                json.dumps(internals or {}, ensure_ascii=False),
                session_id,
            ),
        )

    async def add_attempt(
        self, session_id: str, kind: str, attempt_no: int, payload: dict
    ) -> None:
        await self._write(
            "INSERT INTO attempts (session_id, kind, attempt_no, payload_json, created_at)"
            " VALUES (?, ?, ?, ?, ?)",
            (session_id, kind, attempt_no, json.dumps(payload, ensure_ascii=False), _now()),
        )

    async def get_session(self, session_id: str) -> dict[str, Any] | None:
        async with self._db.execute(
            "SELECT * FROM sessions WHERE id = ?", (session_id,)
        ) as cursor:
            row = await cursor.fetchone()
        if row is None:
            return None
        record = dict(row)
        for field in ("request_json", "result_json", "internals_json"):
            raw = record.pop(field)
            record[field.removesuffix("_json")] = json.loads(raw) if raw else None
        return record

    async def get_attempts(self, session_id: str) -> list[dict[str, Any]]:
        async with self._db.execute(
            "SELECT kind, attempt_no, payload_json, created_at FROM attempts"
            " WHERE session_id = ? ORDER BY id",
            (session_id,),
        ) as cursor:
            rows = await cursor.fetchall()
        return [
            {
                "kind": row["kind"],
                "attempt_no": row["attempt_no"],
                "created_at": row["created_at"],
                "payload": json.loads(row["payload_json"]),
            }
            for row in rows
        ]
