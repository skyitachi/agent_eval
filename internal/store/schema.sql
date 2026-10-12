-- Minimal schema for a design-doc-scoped evaluation harness.
--
-- Two tables, one job each: a run is one invocation of `aeval run` and holds the
-- factor labels a sweep is attributed by; an attempt is one (task, sample) cell
-- and holds everything measured about it.
--
-- Baselines are NOT here. A baseline is an input, not a result: it lives beside
-- the dataset it belongs to, and a run records only the comparison it made
-- against one. That keeps `aeval report` a function of this file alone, which is
-- what makes a two-week-old database render the same document it rendered then.

CREATE TABLE IF NOT EXISTS schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id          TEXT PRIMARY KEY,
    name        TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL DEFAULT 'running',
    started_at  TEXT    NOT NULL,
    ended_at    TEXT,
    duration_ms INTEGER NOT NULL DEFAULT 0,

    dataset_path TEXT NOT NULL DEFAULT '',
    dataset_name TEXT NOT NULL DEFAULT '',
    dataset_hash TEXT NOT NULL DEFAULT '',

    agent_kind  TEXT NOT NULL DEFAULT '',
    agent_model TEXT NOT NULL DEFAULT '',
    agent_bin   TEXT NOT NULL DEFAULT '',
    backend     TEXT NOT NULL DEFAULT '',

    samples     INTEGER NOT NULL DEFAULT 1,
    concurrency INTEGER NOT NULL DEFAULT 1,

    -- Factor labels. A sweep varies one axis and fixes the other two, so the
    -- axis and its level are what turn a pile of runs into an attribution.
    -- Empty on a plain run, which is not the same as a run whose axis is "".
    factor_axis  TEXT NOT NULL DEFAULT '',
    factor_level TEXT NOT NULL DEFAULT '',
    sweep_id     TEXT NOT NULL DEFAULT '',

    notes        TEXT NOT NULL DEFAULT '',
    tool_version TEXT NOT NULL DEFAULT '',
    config_json  TEXT NOT NULL DEFAULT '{}',
    error        TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_runs_started ON runs(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_sweep   ON runs(sweep_id, factor_axis);

CREATE TABLE IF NOT EXISTS attempts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id       TEXT    NOT NULL,
    task_id      TEXT    NOT NULL,
    sample_index INTEGER NOT NULL DEFAULT 0,

    category  TEXT NOT NULL DEFAULT '',
    tags_json TEXT NOT NULL DEFAULT '[]',

    -- status is about the harness: did the cell run to a verdict.
    status      TEXT NOT NULL,
    -- graded separates "measured and failed" from "measured, nothing to score".
    -- A collected session task has a baseline but no ground truth, so it is
    -- measurable and ungraded; counting it as a failure would invent a verdict.
    graded      INTEGER NOT NULL DEFAULT 0,
    passed      INTEGER NOT NULL DEFAULT 0,
    score       REAL    NOT NULL DEFAULT 0,
    threshold   REAL    NOT NULL DEFAULT 0,
    stop_reason TEXT    NOT NULL DEFAULT '',

    duration_ms INTEGER NOT NULL DEFAULT 0,
    cost_usd    REAL    NOT NULL DEFAULT 0,

    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_create_tokens INTEGER NOT NULL DEFAULT 0,

    turns       INTEGER NOT NULL DEFAULT 0,
    tool_calls  INTEGER NOT NULL DEFAULT 0,
    tool_errors INTEGER NOT NULL DEFAULT 0,

    diff        TEXT NOT NULL DEFAULT '',
    answer      TEXT NOT NULL DEFAULT '',
    grades_json TEXT NOT NULL DEFAULT '',
    trace_json  TEXT NOT NULL DEFAULT '',

    -- The comparison against the recorded baseline, resolved at run time.
    -- Storing the numbers rather than a reference keeps the report readable
    -- from the database after the dataset has moved or been edited.
    has_baseline     INTEGER NOT NULL DEFAULT 0,
    baseline_id      TEXT    NOT NULL DEFAULT '',
    baseline_ref     TEXT    NOT NULL DEFAULT '',
    baseline_tokens  INTEGER NOT NULL DEFAULT 0,
    baseline_ms      INTEGER NOT NULL DEFAULT 0,
    baseline_cost    REAL    NOT NULL DEFAULT 0,
    baseline_turns   INTEGER NOT NULL DEFAULT 0,
    -- The recorded session's final answer, so a reader can set the new
    -- attempt's conclusion beside the recorded one and not only compare what
    -- each cost. Stored rather than re-read for the same reason as the numbers
    -- above: the report is a function of this file.
    baseline_output  TEXT    NOT NULL DEFAULT '',

    error         TEXT NOT NULL DEFAULT '',
    workspace_dir TEXT NOT NULL DEFAULT '',

    UNIQUE(run_id, task_id, sample_index),
    FOREIGN KEY(run_id) REFERENCES runs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_attempts_run  ON attempts(run_id, task_id);
CREATE INDEX IF NOT EXISTS idx_attempts_task ON attempts(task_id);
