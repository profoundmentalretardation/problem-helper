import time

import pytest
from fastapi.testclient import TestClient

from problem_helper.api import create_app
from problem_helper.config import Settings
from problem_helper.schemas import ErrorCode, Outcome, SessionStatus, SolveRequest

PAYLOAD = {
    "task": "Sum of two numbers",
    "code": "print(1)",
    "tests": [{"input": "3 4\n", "expected_output": "7"}],
}


def make_client(tmp_path, processor, resumer=None):
    settings = Settings(
        llm_api_key="test",
        db_path=str(tmp_path / "api.db"),
        checkpoint_db_path=str(tmp_path / "checkpoints.db"),
        tracing_enabled=False,
    )
    app = create_app(settings, processor=processor, resumer=resumer)
    return TestClient(app), app


def wait_for(client, session_id: str, status: str, timeout: float = 3.0) -> dict:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        body = client.get(f"/v1/sessions/{session_id}").json()
        if body["status"] == status:
            return body
        time.sleep(0.02)
    pytest.fail(f"session never reached status {status}: {body}")


@pytest.fixture
def holder():
    return {}


def test_health(tmp_path, holder):
    client, _ = make_client(tmp_path, lambda *a: _noop())
    with client:
        assert client.get("/health").json() == {"status": "ok"}


def test_index_page_is_served(tmp_path, holder):
    client, _ = make_client(tmp_path, lambda *a: _noop())
    with client:
        response = client.get("/")

        assert response.status_code == 200
        assert response.headers["content-type"].startswith("text/html")
        assert "problem-helper" in response.text
        assert "/v1/sessions" in response.text


def test_create_session_returns_202_and_persists_request(tmp_path, holder):
    async def processor(session_id, request, trace):
        holder["seen"] = request

    client, _ = make_client(tmp_path, processor)
    with client:
        response = client.post("/v1/sessions", json=PAYLOAD)

        assert response.status_code == 202
        body = response.json()
        assert body["status"] == SessionStatus.pending
        session_id = body["session_id"]

        stored = client.get(f"/v1/sessions/{session_id}")
        assert stored.status_code == 200
        assert stored.json()["stage"] == "queued"

    assert isinstance(holder["seen"], SolveRequest)
    assert holder["seen"].task == "Sum of two numbers"


def test_background_result_becomes_visible(tmp_path, holder):
    async def processor(session_id, request, trace):
        await holder["app"].state.db.finish_success(
            session_id,
            {
                "outcome": Outcome.hint_ready,
                "hint": "compare the operator with the statement",
                "mistakes": [{"title": "sign", "detail": "minus instead of plus", "line": 1}],
                "tests_total": 1,
                "tests_passed_before": 0,
            },
            {"diff": "-a\n+b", "fixed_code": "print(7)"},
        )
        await holder["app"].state.db.add_attempt(session_id, "fix", 1, {"passed": True})

    client, app = make_client(tmp_path, processor)
    holder["app"] = app
    with client:
        session_id = client.post("/v1/sessions", json=PAYLOAD).json()["session_id"]

        body = wait_for(client, session_id, SessionStatus.succeeded)
        assert body["result"]["hint"] == "compare the operator with the statement"
        assert body["result"]["mistakes"][0]["title"] == "sign"
        assert body["error"] is None

        debug = client.get(f"/v1/sessions/{session_id}/debug").json()
        assert debug["internals"]["fixed_code"] == "print(7)"
        assert debug["request"]["task"] == "Sum of two numbers"
        assert debug["attempts"][0]["kind"] == "fix"


def test_failed_session_exposes_error(tmp_path, holder):
    async def processor(session_id, request, trace):
        await holder["app"].state.db.finish_failure(
            session_id, ErrorCode.fix_failed, "could not repair the code"
        )

    client, app = make_client(tmp_path, processor)
    holder["app"] = app
    with client:
        session_id = client.post("/v1/sessions", json=PAYLOAD).json()["session_id"]

        body = wait_for(client, session_id, SessionStatus.failed)
        assert body["error"] == {
            "code": ErrorCode.fix_failed,
            "message": "could not repair the code",
        }
        assert body["result"] is None


def test_tools_are_listed(tmp_path, holder):
    client, _ = make_client(tmp_path, lambda *a: _noop())
    with client:
        body = client.get("/v1/tools").json()

        names = [t["name"] for t in body["tools"]]
        assert "search_corpus" in names
        assert len(names) >= 2
        assert all(t["description"] for t in body["tools"])


def test_samples_are_listed_without_their_solutions(tmp_path, holder):
    client, _ = make_client(tmp_path, lambda *a: _noop())
    with client:
        body = client.get("/v1/samples").json()

        assert len(body["samples"]) >= 8
        first = body["samples"][0]
        assert first["task"] and first["code"] and first["tests"]
        # the reference solution stays on the server — this endpoint feeds a browser
        assert "solution" not in first
        assert "mistake" not in first


def test_a_sample_can_be_submitted_as_is(tmp_path, holder):
    client, _ = make_client(tmp_path, lambda *a: _noop())
    with client:
        sample = client.get("/v1/samples").json()["samples"][0]

        response = client.post(
            "/v1/sessions",
            json={"task": sample["task"], "code": sample["code"], "tests": sample["tests"]},
        )

        assert response.status_code == 202


def test_unfinished_session_can_be_resumed(tmp_path, holder):
    async def processor(session_id, request, trace):
        await holder["app"].state.db.finish_failure(
            session_id, ErrorCode.internal_error, "provider is down"
        )

    async def resumer(session_id, request, trace):
        holder["resumed"] = request
        await holder["app"].state.db.finish_success(
            session_id,
            {
                "outcome": Outcome.hint_ready,
                "hint": "look at line 2",
                "mistakes": [],
                "materials": [],
                "tests_total": 1,
                "tests_passed_before": 0,
            },
        )

    client, app = make_client(tmp_path, processor, resumer=resumer)
    holder["app"] = app
    with client:
        session_id = client.post("/v1/sessions", json=PAYLOAD).json()["session_id"]
        wait_for(client, session_id, SessionStatus.failed)

        response = client.post(f"/v1/sessions/{session_id}/resume")

        assert response.status_code == 202
        body = wait_for(client, session_id, SessionStatus.succeeded)
        assert body["result"]["hint"] == "look at line 2"

    assert holder["resumed"].task == "Sum of two numbers"


def test_resume_of_a_finished_session_is_rejected(tmp_path, holder):
    async def processor(session_id, request, trace):
        await holder["app"].state.db.finish_success(
            session_id,
            {
                "outcome": Outcome.already_correct,
                "mistakes": [],
                "materials": [],
                "tests_total": 1,
                "tests_passed_before": 1,
            },
        )

    client, app = make_client(tmp_path, processor)
    holder["app"] = app
    with client:
        session_id = client.post("/v1/sessions", json=PAYLOAD).json()["session_id"]
        wait_for(client, session_id, SessionStatus.succeeded)

        assert client.post(f"/v1/sessions/{session_id}/resume").status_code == 409
        assert client.post("/v1/sessions/missing/resume").status_code == 404


def test_unknown_session_is_404(tmp_path, holder):
    client, _ = make_client(tmp_path, lambda *a: _noop())
    with client:
        assert client.get("/v1/sessions/missing").status_code == 404
        assert client.get("/v1/sessions/missing/debug").status_code == 404


def test_request_validation(tmp_path, holder):
    client, _ = make_client(tmp_path, lambda *a: _noop())
    with client:
        assert client.post("/v1/sessions", json={**PAYLOAD, "tests": []}).status_code == 422
        assert client.post("/v1/sessions", json={**PAYLOAD, "code": ""}).status_code == 422
        assert (
            client.post("/v1/sessions", json={**PAYLOAD, "language": "go"}).status_code == 422
        )


async def _noop() -> None:
    return None
