"""Paths and one connection helper. The only file more than one slice touches.

Everything else in `memory/` owns its own tables and its own files; this holds the two things
they cannot each decide separately — where `state/` is, and how a connection is opened. Every
path is read through a function rather than bound at import, so a test can point `STATE` at a
temporary directory and the whole package follows.
"""

import os
import sqlite3
import time
from pathlib import Path

STATE = Path(os.getenv("HELPER_STATE", "state"))
RULES_DIR = Path(os.getenv("HELPER_RULES", "rules"))


def now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def db_path():
    return STATE / "helper.db"


def facts_path():
    return STATE / "facts.json"


def events_path():
    return STATE / "events.jsonl"


def tutor_rules_path():
    return RULES_DIR / "tutor_rules.md"


def connect():
    """A connection with `state/` guaranteed to exist.

    Callers run their own `CREATE TABLE IF NOT EXISTS` — no shared migration, so two people
    adding a table on the same afternoon never touch the same lines.
    """
    STATE.mkdir(parents=True, exist_ok=True)
    con = sqlite3.connect(db_path())
    con.row_factory = sqlite3.Row
    return con
