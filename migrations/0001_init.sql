-- Initial schema: help_requests pipeline state, submissions snapshot,
-- shield audit trail, LLM usage/cost accounting, hints (incl. cross-user
-- cache), raw/curated mistakes, and the append-only events log.

CREATE TABLE help_requests (
    id                  UUID PRIMARY KEY,
    user_id             TEXT NOT NULL,
    problem_id          TEXT NOT NULL,
    platform            TEXT NOT NULL,
    n_submissions_taken INT NOT NULL DEFAULT 0,
    status              TEXT NOT NULL DEFAULT 'pending',
    failure_reason      TEXT,
    best_submission_id  UUID,
    hint_id             UUID,
    useless             BOOLEAN NOT NULL DEFAULT FALSE,
    error               TEXT,
    claimed_by          TEXT,
    heartbeat_at        TIMESTAMPTZ,
    resume_step         TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX help_requests_status_idx ON help_requests (status);

CREATE TABLE submissions (
    id                      UUID PRIMARY KEY,
    request_id              UUID NOT NULL REFERENCES help_requests (id),
    platform_submission_id  TEXT NOT NULL,
    code                    TEXT NOT NULL,
    language                TEXT NOT NULL,
    tests_passed            INT NOT NULL,
    tests_total             INT NOT NULL,
    submitted_at            TIMESTAMPTZ NOT NULL,
    is_best                 BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX submissions_request_id_idx ON submissions (request_id);

CREATE TABLE shield_records (
    id           UUID PRIMARY KEY,
    request_id   UUID NOT NULL REFERENCES help_requests (id),
    code_before  TEXT NOT NULL,
    code_after   TEXT NOT NULL,
    diff         TEXT NOT NULL,
    removed      JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX shield_records_request_id_idx ON shield_records (request_id);

CREATE TABLE llm_calls (
    id                   UUID PRIMARY KEY,
    request_id           UUID NOT NULL REFERENCES help_requests (id),
    agent                TEXT NOT NULL,
    model                TEXT NOT NULL,
    input_tokens         INT NOT NULL,
    cached_input_tokens  INT NOT NULL DEFAULT 0,
    output_tokens        INT NOT NULL,
    cost                 NUMERIC NOT NULL,
    latency_ms           INT NOT NULL,
    attempt              INT NOT NULL,
    prompt               TEXT,
    response             TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX llm_calls_request_id_idx ON llm_calls (request_id);

CREATE TABLE hints (
    id          UUID PRIMARY KEY,
    request_id  UUID NOT NULL REFERENCES help_requests (id),
    problem_id  TEXT NOT NULL,
    code_hash   TEXT NOT NULL,
    text        TEXT NOT NULL,
    approved    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX hints_problem_code_hash_approved_idx ON hints (problem_id, code_hash) WHERE approved;

CREATE TABLE raw_mistakes (
    id          UUID PRIMARY KEY,
    request_id  UUID NOT NULL REFERENCES help_requests (id),
    user_id     TEXT NOT NULL,
    text        TEXT NOT NULL,
    processed   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX raw_mistakes_unprocessed_idx ON raw_mistakes (user_id) WHERE NOT processed;

CREATE TABLE mistakes (
    id           UUID PRIMARY KEY,
    user_id      TEXT NOT NULL,
    title        TEXT NOT NULL,
    description  TEXT NOT NULL,
    count        INT NOT NULL DEFAULT 1,
    first_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX mistakes_user_id_idx ON mistakes (user_id);

CREATE TABLE events (
    id          UUID PRIMARY KEY,
    request_id  UUID NOT NULL REFERENCES help_requests (id),
    kind        TEXT NOT NULL,
    payload     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX events_request_id_created_at_idx ON events (request_id, created_at);
