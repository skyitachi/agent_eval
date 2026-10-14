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
    -- The agent configuration this run actually drove, as agent.Spec JSON.
    --
    -- The four columns above are the labels a run is compared by, and they are
    -- not the whole configuration: the skill set lives in plugin_dirs plus the
    -- isolation flags that decide whether it survives, and none of it was
    -- recorded. So `report --compare` could not tell a run with the user's
    -- skills loaded from one without them, and the page could not state either.
    --
    -- Empty means the row predates this column. That is not "no configuration":
    -- it is the absence of a record, and a reader has to be able to say which.
    agent_spec_json TEXT NOT NULL DEFAULT '',
    -- Whether the run replayed each session's tasks in one workspace, in order.
    --
    -- It changes what every number beside it means: a chained cell starts from
    -- the state its predecessor left, so its spend is not an independent sample
    -- and the cells of a chain cannot be averaged as though they were. A page
    -- that shows the cells without this says "k independent measurements" about
    -- what is really one continuation.
    --
    -- 0 means the row predates this column, which is the same as not chained --
    -- no earlier build could chain, so the two readings agree here and there is
    -- no third state to keep apart.
    chain_sessions INTEGER NOT NULL DEFAULT 0,
    -- How many of the run's tasks were replayed in the recording's own
    -- directory instead of one the harness built.
    --
    -- This is the strongest of the three "how was it driven" columns, because
    -- it is the one that changes what the agent could see: an inplace cell ran
    -- where the transcript ran, with whatever was already there -- the user's
    -- uncommitted edits, ignored build output, untracked scratch files -- and
    -- the directory was rolled back afterwards. A page that omits it shows a
    -- patch against a real tree as a patch against a fresh checkout.
    --
    -- A count, not a flag: --workspace-cwd binds a task only when its
    -- start-of-session record survived, so one run can be part in place.
    --
    -- 0 means the row predates this column, which is the same as not in place:
    -- no earlier build could replay in place.
    inplace_tasks INTEGER NOT NULL DEFAULT 0,
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

    -- Who made the recording, and what the recording says it does not cover.
    --
    -- A comparison is only readable if the reader can see what the two sides
    -- are. Comparing a replay against a recording made by a *different* agent
    -- is a legitimate thing to do -- it is what "replay a collected session"
    -- means -- but presenting its delta without saying so lets a reader take a
    -- cross-agent difference for a same-agent one.
    --
    -- These are the baseline's own claims about itself, carried verbatim:
    -- notes is what the capture declared (a field it never recorded, an
    -- estimate standing in for a measurement), and it replaces the report's
    -- generic sentence rather than sitting beside it.
    baseline_agent_name  TEXT NOT NULL DEFAULT '',
    baseline_agent_model TEXT NOT NULL DEFAULT '',
    baseline_source      TEXT NOT NULL DEFAULT '',
    baseline_notes       TEXT NOT NULL DEFAULT '',

    error         TEXT NOT NULL DEFAULT '',
    workspace_dir TEXT NOT NULL DEFAULT '',

    UNIQUE(run_id, task_id, sample_index),
    FOREIGN KEY(run_id) REFERENCES runs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_attempts_run  ON attempts(run_id, task_id);
CREATE INDEX IF NOT EXISTS idx_attempts_task ON attempts(task_id);
