-- Schema for the evaluation store.
--
-- Design notes:
--
--  * Every run is self-describing. The dataset hash, config hash, agent, model
--    and sandbox backend are recorded on the run row, so a stored result can be
--    interpreted months later without the original files.
--
--  * Denormalised columns (turns, cost_usd, passed, ...) exist alongside the
--    serialised JSON blobs so that aggregate queries and the web UI do not have
--    to parse JSON per row.
--
--  * Detailed per-grader rows and per-tool-call rows are kept in their own
--    tables: "which grader fails most often" and "which tool errored" are the
--    questions that actually drive debugging, and both are painful to answer
--    from a blob.
--
--  * Raw streams are the only thing deliberately not stored here; they are large
--    and live as files next to the database, referenced by raw_stream_path.

CREATE TABLE IF NOT EXISTS runs (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL,          -- running | completed | failed | cancelled
    created_at          TEXT NOT NULL,          -- RFC3339 UTC
    finished_at         TEXT,
    duration_ms         INTEGER NOT NULL DEFAULT 0,

    dataset_name        TEXT NOT NULL DEFAULT '',
    dataset_version     TEXT NOT NULL DEFAULT '',
    dataset_hash        TEXT NOT NULL DEFAULT '',
    dataset_path        TEXT NOT NULL DEFAULT '',

    agent_kind          TEXT NOT NULL DEFAULT '',
    agent_model         TEXT NOT NULL DEFAULT '',
    sandbox_backend     TEXT NOT NULL DEFAULT '',
    sandbox_image       TEXT NOT NULL DEFAULT '',
    -- Which question the run asked: "agent" (can the agent do it), "oracle"
    -- (does the task's own reference solution pass -- a solvability check) or
    -- "baseline" (does the untouched workspace already pass -- a necessity
    -- check). Stored per run because the three produce the same shape of result
    -- and an unlabelled oracle run reads like a spectacular agent score.
    mode                TEXT NOT NULL DEFAULT 'agent',

    samples_per_task    INTEGER NOT NULL DEFAULT 1,
    concurrency         INTEGER NOT NULL DEFAULT 1,
    seed                INTEGER NOT NULL DEFAULT 0,

    config_json         TEXT NOT NULL DEFAULT '',
    config_hash         TEXT NOT NULL DEFAULT '',
    notes               TEXT NOT NULL DEFAULT '',
    tool_version        TEXT NOT NULL DEFAULT '',
    host_arch           TEXT NOT NULL DEFAULT '',
    metrics_json        TEXT NOT NULL DEFAULT '',
    error               TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_runs_created  ON runs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_dataset  ON runs(dataset_name, dataset_version);
CREATE INDEX IF NOT EXISTS idx_runs_model    ON runs(agent_model);

-- One row per (task, sample). `passed` and `score` are the graded verdict; the
-- attempt and trace blobs make the row fully replayable by the report generator.
CREATE TABLE IF NOT EXISTS task_runs (
    id                  TEXT PRIMARY KEY,
    run_id              TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    task_id             TEXT NOT NULL,
    sample_index        INTEGER NOT NULL DEFAULT 0,
    category            TEXT NOT NULL DEFAULT '',
    difficulty          TEXT NOT NULL DEFAULT '',
    tags_json           TEXT NOT NULL DEFAULT '[]',

    status              TEXT NOT NULL DEFAULT '',   -- completed | failed | skipped
    stop_reason         TEXT NOT NULL DEFAULT '',
    passed              INTEGER NOT NULL DEFAULT 0,
    score               REAL NOT NULL DEFAULT 0,
    threshold           REAL NOT NULL DEFAULT 0,
    -- reward is the scalar training signal, derived from score through the
    -- task's declared reward contract (see internal/reward). It is denormalised
    -- rather than recomputed at read time because the contract is part of the
    -- dataset: a reader must be able to see what the trainer would have seen
    -- without re-deriving it from a dataset version that may no longer exist.
    reward              REAL NOT NULL DEFAULT 0,
    -- reward_kind is the density that produced it ("score" or "pass"). Without
    -- it a stored 0 is ambiguous: a sparse contract paying nothing for a partial
    -- fix and a wrong answer paying nothing look identical, and the report
    -- cannot tell a reader which one it is looking at.
    reward_kind         TEXT NOT NULL DEFAULT '',

    session_id          TEXT NOT NULL DEFAULT '',
    model               TEXT NOT NULL DEFAULT '',
    started_at          TEXT,
    ended_at            TEXT,
    duration_ms         INTEGER NOT NULL DEFAULT 0,

    cost_usd            REAL NOT NULL DEFAULT 0,
    cost_reliable       INTEGER NOT NULL DEFAULT 0,
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_create_tokens INTEGER NOT NULL DEFAULT 0,
    turns               INTEGER NOT NULL DEFAULT 0,
    tool_calls          INTEGER NOT NULL DEFAULT 0,
    tool_errors         INTEGER NOT NULL DEFAULT 0,
    ttft_ms             INTEGER NOT NULL DEFAULT 0,
    api_duration_ms     INTEGER NOT NULL DEFAULT 0,

    diff                TEXT NOT NULL DEFAULT '',
    answer              TEXT NOT NULL DEFAULT '',
    attempt_json        TEXT NOT NULL DEFAULT '',
    trace_json          TEXT NOT NULL DEFAULT '',
    grades_json         TEXT NOT NULL DEFAULT '',
    -- JSON array of anti-gaming violations (see dataset.TaskSpec.ForbidChangedPaths)
    -- and of the paths reset to baseline before grading. Kept apart from `error`
    -- because a violated guard is a verdict about the attempt, not a harness
    -- fault, and the two are counted separately.
    guard_json          TEXT NOT NULL DEFAULT '',
    error               TEXT NOT NULL DEFAULT '',
    workspace_dir       TEXT NOT NULL DEFAULT '',
    raw_stream_path     TEXT NOT NULL DEFAULT '',

    UNIQUE (run_id, task_id, sample_index)
);

CREATE INDEX IF NOT EXISTS idx_task_runs_run      ON task_runs(run_id);
CREATE INDEX IF NOT EXISTS idx_task_runs_run_task ON task_runs(run_id, task_id);
CREATE INDEX IF NOT EXISTS idx_task_runs_passed   ON task_runs(run_id, passed);
CREATE INDEX IF NOT EXISTS idx_task_runs_category ON task_runs(run_id, category);
CREATE INDEX IF NOT EXISTS idx_task_runs_stop     ON task_runs(run_id, stop_reason);

-- Flattened per-grader verdicts.
--
-- `seq` preserves the order the graders ran in, which is the order they appear
-- in the attempt's grades blob. Reports and the web UI list graders in run
-- order, and without an explicit column that order would depend on storage.
CREATE TABLE IF NOT EXISTS grader_results (
    run_id          TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    task_run_id     TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
    task_id         TEXT NOT NULL,
    sample_index    INTEGER NOT NULL DEFAULT 0,
    seq             INTEGER NOT NULL DEFAULT 0,
    grader          TEXT NOT NULL,
    type            TEXT NOT NULL,
    passed          INTEGER NOT NULL DEFAULT 0,
    score           REAL NOT NULL DEFAULT 0,
    weight          REAL NOT NULL DEFAULT 0,
    required        INTEGER NOT NULL DEFAULT 0,
    reason          TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    details_json    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_grader_results_run    ON grader_results(run_id, grader);
CREATE INDEX IF NOT EXISTS idx_grader_results_failed ON grader_results(run_id, passed);

-- Flattened tool invocations, for trajectory analysis.
CREATE TABLE IF NOT EXISTS tool_calls (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    task_run_id     TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
    task_id         TEXT NOT NULL,
    sample_index    INTEGER NOT NULL DEFAULT 0,
    seq             INTEGER NOT NULL DEFAULT 0,
    tool_use_id     TEXT NOT NULL DEFAULT '',
    name            TEXT NOT NULL DEFAULT '',
    is_error        INTEGER NOT NULL DEFAULT 0,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    input_json      TEXT NOT NULL DEFAULT '',
    output          TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_tool_calls_task      ON tool_calls(task_run_id, seq);
CREATE INDEX IF NOT EXISTS idx_tool_calls_run_tool  ON tool_calls(run_id, name);

-- Computed metrics, including custom ones, keyed by name.
CREATE TABLE IF NOT EXISTS run_metrics (
    run_id      TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    value       REAL NOT NULL DEFAULT 0,
    unit        TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT '',
    label       TEXT NOT NULL DEFAULT '',
    extra_json  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, name)
);

-- Arbitrary key/value annotations, e.g. git revision of the harness.
CREATE TABLE IF NOT EXISTS run_tags (
    run_id  TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    key     TEXT NOT NULL,
    value   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, key)
);
