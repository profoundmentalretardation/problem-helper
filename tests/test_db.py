import pytest

from problem_helper.db import Database
from problem_helper.schemas import ErrorCode, SessionStage, SessionStatus


@pytest.fixture
async def db(tmp_path):
    database = await Database(str(tmp_path / "test.db")).connect()
    yield database
    await database.close()


async def test_create_and_get_session(db):
    await db.create_session("s1", {"task": "sum of two numbers", "code": "print(1)"})

    record = await db.get_session("s1")

    assert record["status"] == SessionStatus.pending
    assert record["stage"] == SessionStage.queued
    assert record["request"]["task"] == "sum of two numbers"
    assert record["result"] is None
    assert record["created_at"] == record["updated_at"]


async def test_unknown_session_is_none(db):
    assert await db.get_session("nope") is None


async def test_stage_transition_and_success(db):
    await db.create_session("s1", {"task": "t"})
    await db.set_stage("s1", SessionStatus.running, SessionStage.fixing)

    running = await db.get_session("s1")
    assert running["status"] == SessionStatus.running
    assert running["stage"] == SessionStage.fixing

    await db.finish_success("s1", {"hint": "check the sign"}, internals={"diff": "-a\n+b"})

    done = await db.get_session("s1")
    assert done["status"] == SessionStatus.succeeded
    assert done["stage"] == SessionStage.done
    assert done["result"]["hint"] == "check the sign"
    assert done["internals"]["diff"] == "-a\n+b"
    assert done["error_code"] is None


async def test_non_ascii_payload_round_trips(db):
    await db.create_session("s1", {"task": "сумма чисел"})

    await db.finish_success("s1", {"hint": "проверь знак операции ✓"})

    record = await db.get_session("s1")
    assert record["request"]["task"] == "сумма чисел"
    assert record["result"]["hint"] == "проверь знак операции ✓"


async def test_failure_is_stored_with_code(db):
    await db.create_session("s1", {"task": "t"})

    await db.finish_failure("s1", ErrorCode.fix_failed, "could not repair in 3 attempts")

    record = await db.get_session("s1")
    assert record["status"] == SessionStatus.failed
    assert record["error_code"] == ErrorCode.fix_failed
    assert "3 attempts" in record["error_message"]


async def test_success_after_a_failure_clears_the_error(db):
    """A resumed session must not keep reporting the failure it was resumed from."""
    await db.create_session("s1", {"task": "t"})
    await db.finish_failure("s1", ErrorCode.internal_error, "provider is down")

    await db.finish_success("s1", {"outcome": "hint_ready", "hint": "look at line 2"})

    record = await db.get_session("s1")
    assert record["status"] == SessionStatus.succeeded
    assert record["error_code"] is None
    assert record["error_message"] is None


async def test_attempts_are_ordered(db):
    await db.create_session("s1", {"task": "t"})

    await db.add_attempt("s1", "fix", 1, {"passed": False})
    await db.add_attempt("s1", "fix", 2, {"passed": True})
    await db.add_attempt("s1", "hint", 1, {"approved": True})

    attempts = await db.get_attempts("s1")
    assert [(a["kind"], a["attempt_no"]) for a in attempts] == [
        ("fix", 1),
        ("fix", 2),
        ("hint", 1),
    ]
    assert attempts[1]["payload"] == {"passed": True}
    assert await db.get_attempts("other") == []
