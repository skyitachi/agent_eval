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

    -- in_place is whether this run worked in the directories the tasks record
    -- rather than in copies of them.
    --
    -- It is on the row because it changes what the cells are evidence *of*. A
    -- run that worked in someone's repository put an agent in a tree with the
    -- repository's whole history in it, and put the tree back afterwards; a
    -- copy-based run gave the agent neither. Two runs whose rows are identical
    -- apart from this were not the same experiment, and a page that does not
    -- say so invites a reader to compare them as though they were.
    --
    -- 0 means the row predates this column, which is the same as not in place
    -- -- no earlier build could work in place, so the two readings agree.
    in_place     INTEGER NOT NULL DEFAULT 0,
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

    -- restore_notes is what happened to the recorded directory when the run
    -- gave it back: where it was put back, what was deliberately left in it,
    -- and -- the case that matters -- whether it could not be put back at all.
    --
    -- It is stored rather than derived because it is the only record of a
    -- directory the harness does not own. The next run over the same dataset
    -- will not see this state; the person whose repository it is has to, and
    -- the report is where they will look.
    restore_notes TEXT NOT NULL DEFAULT '',

    UNIQUE(run_id, task_id, sample_index),
    FOREIGN KEY(run_id) REFERENCES runs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_attempts_run  ON attempts(run_id, task_id);
CREATE INDEX IF NOT EXISTS idx_attempts_task ON attempts(task_id);

-- One report that was actually produced, recorded so a server can list it and
-- hand it back under a stable link.
--
-- The rendered payload is deliberately NOT here. A report is a function of this
-- database, and a second copy of it would be a second answer to the same
-- question: it would stop tracking the rows the moment the database moved, and
-- a renderer fix would never reach it. What is stored is the part that cannot
-- be re-derived -- which view was asked for, with which options, at which
-- moment -- plus the hash of what came out.
--
-- The hash is what makes the row a *claim* rather than a cache. Serving this id
-- rebuilds the payload, hashes it the same way and compares: a page whose data
-- has moved says so on its face instead of quietly showing a different document
-- under the same link. That is also why `payload_bytes` is kept -- it is the
-- size of the document that was hashed, which is a measurement of the record
-- and not a forecast of anything.
--
-- One row per registration, append-only. Re-rendering a view writes a second
-- row rather than updating the first, because the two hashes side by side are
-- the evidence of drift -- collapsing them to the latest would erase exactly
-- the history this table exists to keep.
CREATE TABLE IF NOT EXISTS reports (
    id            TEXT PRIMARY KEY,
    -- Which view: run | compare | sweep | index.
    kind          TEXT NOT NULL DEFAULT '',
    -- The document title, as rendered.
    title         TEXT NOT NULL DEFAULT '',
    -- A human label for what the report was about ("run name / axis=level",
    -- "a vs b"). Stored rather than derived because the thing it names can be
    -- deleted: a row whose subject is gone still has to be able to say what it
    -- was about, and no lookup can answer that afterwards.
    subject       TEXT NOT NULL DEFAULT '',
    -- The ids the view was asked for, as JSON. Together with kind this is the
    -- whole of the request; an unknown or missing key is an error rather than a
    -- default, because guessing would rebuild a different document and then
    -- report it as drift.
    params_json   TEXT NOT NULL DEFAULT '{}',
    -- The resolved report.Options that shaped the payload (field budgets, the
    -- summary/detail switch, the generation stamp). Resolved and not as-typed:
    -- the zero values become real numbers before they are stored, so replaying
    -- them cannot pick up a different default than the one that applied.
    options_json  TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL,
    -- sha256 over the exact bytes the JSON endpoint returns. Hex, lower case.
    payload_hash  TEXT NOT NULL DEFAULT '',
    payload_bytes INTEGER NOT NULL DEFAULT 0,
    -- report.DataVersion at registration. A payload whose format has changed
    -- since is a different document for a reason that has nothing to do with
    -- the data, and a reader comparing hashes has to be able to tell the two
    -- apart.
    data_version  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_reports_created ON reports(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_reports_kind    ON reports(kind, created_at DESC);

-- Replays started from the web page: one row per press of the replay button.
--
-- A replay is a session collected into a dataset and run under the harness, the
-- skill set and the model the person picked. The run it produced is an ordinary
-- row in runs and the report is an ordinary row in reports; this table is what
-- ties the three to the recording they came from, which neither of the others
-- can say -- a run knows its dataset directory, not which transcript the
-- dataset was collected from, and the session page has to list its replays.
--
-- The choices are stored as the names that were picked, beside the spec they
-- resolved to. The names are what the person reads back; the spec is what ran,
-- and it is kept because a preset file can be edited between two replays that
-- carry the same name.
CREATE TABLE IF NOT EXISTS replays (
    id            TEXT PRIMARY KEY,
    -- claude | cursor, and the session id inside that source.
    source        TEXT NOT NULL DEFAULT '',
    session_id    TEXT NOT NULL DEFAULT '',
    -- The session's title at the time, kept because the transcript can be
    -- deleted and the listing still has to say what was replayed.
    session_title TEXT NOT NULL DEFAULT '',
    harness       TEXT NOT NULL DEFAULT '',
    skill         TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    agent_spec_json TEXT NOT NULL DEFAULT '',
    -- queued | running | completed | failed. A row the server finds as queued
    -- or running when it starts belonged to a process that is gone, and is
    -- closed as failed rather than left to look as though it is still going.
    status        TEXT NOT NULL DEFAULT 'queued',
    created_at    TEXT NOT NULL,
    started_at    TEXT,
    ended_at      TEXT,
    dataset_dir   TEXT NOT NULL DEFAULT '',
    -- Empty until the run row exists, and on a replay refused before it did.
    run_id        TEXT NOT NULL DEFAULT '',
    report_id     TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT '',
    -- What the job printed while it ran: collection warnings, one line per
    -- finished cell. It is the only progress a page can show for a replay
    -- whose run row does not exist yet.
    log           TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_replays_session ON replays(source, session_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_replays_created ON replays(created_at DESC);
