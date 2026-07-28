"""The append-only event log, and the table the two agents coordinate through.

Two stores, one job: make a run reconstructable from the outside, after it is over, by
somebody neither agent talked to.

* `events.jsonl` — one immutable line per thing that happened, each carrying the hash of the
  line before it. Editing or removing a record breaks every hash after it, and `verify_chain`
  says exactly where. This is the monitor's only input: a log the judged system can quietly
  tidy up is not evidence of anything.
* `handoffs` — one row per `{status, result, needs_approval}` passed between agents.

Shared memory is the least predictable way to coordinate agents, and the reason is not that it
is complicated — it is that the write and the read are separated in time, and neither side
shows up in the other's transcript. So a hand-off is written to *both* stores under one
`run_id`: the table answers "what was passed", the log answers "in what order, among what
else", and neither agent has to be alive to answer either.

Self-test: `python -m memory.events`
"""

import hashlib
import json

from .db import connect, events_path, now

KINDS = ["run_started", "repair_verified", "hint_delivered", "handoff", "fact_saved",
         "rule_added", "note_written", "notes_read", "run_finished"]

SCHEMA = """
CREATE TABLE IF NOT EXISTS handoffs (
    handoff_id     INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id         TEXT NOT NULL,
    from_agent     TEXT NOT NULL,
    to_agent       TEXT NOT NULL,
    status         TEXT NOT NULL,
    result         TEXT NOT NULL,
    needs_approval INTEGER NOT NULL,
    at             TEXT NOT NULL
);
"""


# ---------------------------------------------------------------- the chain

def _tail():
    """(seq, hash) of the last record. Reads line by line — the log only grows."""
    path = events_path()
    if not path.exists():
        return 0, "genesis"
    last = None
    with path.open() as fh:
        for line in fh:
            if line.strip():
                last = line
    if last is None:
        return 0, "genesis"
    rec = json.loads(last)
    return rec["seq"], rec["hash"]


def _digest(record):
    return hashlib.sha256(
        json.dumps(record, sort_keys=True, ensure_ascii=False).encode()).hexdigest()


def append_event(kind, run_id, payload):
    """Write one record. Returns it, so a caller can quote the `seq` in a trace."""
    if kind not in KINDS:
        # A typo'd kind is a record the monitor will never sample. Fail loudly here rather
        # than write something that silently never gets judged.
        raise ValueError(f"unknown event kind {kind!r}; add it to KINDS first")
    path = events_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    seq, prev = _tail()
    record = {"seq": seq + 1, "at": now(), "kind": kind, "run_id": run_id,
              "payload": payload, "prev": prev}
    record["hash"] = _digest(record)
    with path.open("a") as fh:
        fh.write(json.dumps(record, ensure_ascii=False) + "\n")
    return record


def read_events(since_seq=0, kind=None, run_id=None):
    path = events_path()
    if not path.exists():
        return []
    out = []
    with path.open() as fh:
        for line in fh:
            if not line.strip():
                continue
            rec = json.loads(line)
            if rec["seq"] <= since_seq:
                continue
            if kind and rec["kind"] != kind:
                continue
            if run_id and rec["run_id"] != run_id:
                continue
            out.append(rec)
    return out


def run_ids(since_seq=0):
    """Distinct runs in log order — what the monitor samples from."""
    seen = []
    for rec in read_events(since_seq):
        if rec["run_id"] and rec["run_id"] not in seen:
            seen.append(rec["run_id"])
    return seen


def verify_chain():
    """Recompute every hash. Returns the first `seq` that does not line up.

    Known limit, and it is a real one: this catches an edited or deleted record in the middle,
    because everything after it is chained to it. It does not catch truncation of the *tail* —
    for that the last hash has to be anchored somewhere the writer cannot reach. Out of scope
    here; written down so nobody reads more into a green check than it says.
    """
    prev, seq = "genesis", 0
    for rec in read_events():
        body = {k: v for k, v in rec.items() if k != "hash"}
        if rec["prev"] != prev or rec["seq"] != seq + 1 or _digest(body) != rec["hash"]:
            return {"ok": False, "broken_at": rec["seq"]}
        prev, seq = rec["hash"], rec["seq"]
    return {"ok": True, "records": seq}


# ---------------------------------------------------------------- hand-offs

def record_handoff(run_id, from_agent, to_agent, envelope):
    """Persist one hand-off to both stores, under one `run_id`."""
    for field in ("status", "needs_approval"):
        if field not in envelope:
            return {"ok": False, "error": f"envelope is missing {field!r}"}
    with connect() as con:
        con.executescript(SCHEMA)
        con.execute("INSERT INTO handoffs (run_id, from_agent, to_agent, status, result, "
                    "needs_approval, at) VALUES (?,?,?,?,?,?,?)",
                    (run_id, from_agent, to_agent, envelope["status"],
                     json.dumps(envelope.get("result"), ensure_ascii=False),
                     int(bool(envelope["needs_approval"])), now()))
    append_event("handoff", run_id, {"from": from_agent, "to": to_agent,
                                     "status": envelope["status"],
                                     "needs_approval": bool(envelope["needs_approval"])})
    return {"ok": True}


def handoffs_for(run_id):
    with connect() as con:
        con.executescript(SCHEMA)
        rows = con.execute("SELECT * FROM handoffs WHERE run_id = ? ORDER BY handoff_id",
                           (run_id,)).fetchall()
    return [dict(r) for r in rows]


# ---------------------------------------------------------------- tests

if __name__ == "__main__":
    import shutil
    import tempfile

    from . import db

    db.STATE = tmp = __import__("pathlib").Path(tempfile.mkdtemp(prefix="events-"))
    passed = failed = 0

    def check(label, condition, detail=""):
        global passed, failed
        passed, failed = passed + bool(condition), failed + (not condition)
        print(("  PASS  " if condition else "  FAIL  ") + label, "" if condition else detail)

    print("1. the chain")
    for i in range(3):
        append_event("run_started", f"run-{i}", {"i": i})
    check("records are numbered from one", [r["seq"] for r in read_events()] == [1, 2, 3])
    check("the first record chains to genesis", read_events()[0]["prev"] == "genesis")
    check("each record chains to the one before",
          read_events()[1]["prev"] == read_events()[0]["hash"])
    check("a fresh log verifies", verify_chain() == {"ok": True, "records": 3}, verify_chain())

    print("\n2. tampering")
    lines = events_path().read_text().splitlines()
    edited = json.loads(lines[1])
    edited["payload"] = {"i": 99}
    lines[1] = json.dumps(edited, ensure_ascii=False)
    events_path().write_text("\n".join(lines) + "\n")
    check("editing a record is caught, at its seq", verify_chain() == {"ok": False, "broken_at": 2},
          verify_chain())

    lines = events_path().read_text().splitlines()
    del lines[1]
    events_path().write_text("\n".join(lines) + "\n")
    check("deleting a record from the middle is caught", verify_chain()["ok"] is False)

    events_path().unlink()
    for i in range(3):
        append_event("run_started", f"run-{i}", {"i": i})
    events_path().write_text("\n".join(events_path().read_text().splitlines()[:2]) + "\n")
    check("dropping the tail is NOT caught — the documented limit", verify_chain()["ok"] is True)

    print("\n3. filters and hand-offs")
    events_path().unlink()
    append_event("run_started", "r1", {})
    append_event("hint_delivered", "r1", {"concept": "loop_bounds"})
    append_event("run_started", "r2", {})
    check("filtering by kind works", len(read_events(kind="run_started")) == 2)
    check("filtering by run works", len(read_events(run_id="r1")) == 2)
    check("since_seq skips what a monitor already judged", len(read_events(since_seq=2)) == 1)
    check("runs come back in order", run_ids() == ["r1", "r2"])

    env = {"status": "fixed", "result": {"tag": "off_by_one"}, "needs_approval": False}
    check("a hand-off is accepted", record_handoff("r1", "executor", "curator", env)["ok"])
    check("...and is queryable afterwards",
          handoffs_for("r1")[0]["to_agent"] == "curator")
    check("...and appears in the log under the same run_id",
          len(read_events(kind="handoff", run_id="r1")) == 1)
    check("an envelope missing a field is refused, not half-written",
          record_handoff("r1", "a", "b", {"status": "x"})["ok"] is False
          and len(handoffs_for("r1")) == 1)
    try:
        append_event("whatever", "r1", {})
        check("an unknown event kind raises", False, "it did not raise")
    except ValueError:
        check("an unknown event kind raises rather than writing an unjudgeable record", True)

    check("the chain still verifies after all of that", verify_chain()["ok"], verify_chain())

    shutil.rmtree(tmp)
    print(f"\n{passed} passed, {failed} failed")
