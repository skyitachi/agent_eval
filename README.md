# agenteval

**English** | [简体中文](README.zh-CN.md)

A small, opinionated harness for measuring coding agents. It runs a dataset of
tasks against an agent, grades the result with pluggable graders, persists every
cell to SQLite, and renders self-contained HTML reports.

It answers three questions:

- **How well did an agent do on a dataset?** — `aeval run`
- **Which of harness, model and skill is responsible for a difference?** —
  `aeval matrix`
- **What did a real session cost, so a re-run can be measured against it?** —
  `aeval collect`

It does not answer *is this agent good*. That needs a dataset with ground truth
and a reader who knows what the numbers are for, and no command supplies either.

```
aeval collect  ──> dataset + baseline
                          │
aeval run      ──┐        ▼
aeval matrix   ──┴──> SQLite ──> aeval report    (one self-contained .html)
                                 aeval list      (what has been recorded)
```

The only agent implementation today is **Claude Code**, but the model is
free-form and forwarded untouched, so any endpoint the CLI can reach works. The
agent driver, the graders and the sandbox backend are all registries: adding one
is a new file, not a fork.

## Design stance

Seven decisions shape everything else. They are worth stating because they
explain some otherwise-surprising behaviour.

**A report is a function of the database, not of the process.** Two weeks later,
from a copied `.db` on another machine, `aeval report` must reproduce the
document that was written during the run. The renderer therefore reads only
persisted state, and a test asserts that a report regenerated from the database
is byte-identical to the one the run wrote.

**A registration is a request, not a copy of the answer.** Recording that a
report was rendered stores which view was asked for and the hash of what came
out — never the HTML, and never the payload. A payload is a function of the
database, and a stored payload is that function's *old* value: it stops being
updated the moment the rows move while still looking like an answer. The hash is
a claim the server re-checks on every read, which is why a served page can say
whether it still is the document that was registered, and can say what it is
when it is not.

**An attribution is one axis at a time.** A sweep varies *either* the harness
*or* the model *or* the skill, never two. This is enforced at load time — a level
that changes a key its axis does not own is a refusal, not a warning. The cost is
real and stated in every report: a one-axis sweep cannot see an interaction, so a
difference that only appears in combination will not show up.

**Measurements are never retried away.** A timeout, a non-zero exit, an error
result — those are data. There is no retry of a measurement. What is measured
and what is scored are also separate: a cell can be measured and *ungraded*
(a task built from a recorded session has no ground truth), and calling that a
failure would invent a verdict the data does not contain.

**An evaluation must not silently inherit the operator's machine.** The default
agent preset is a clean room: no user hooks, no auto-memory, no stray MCP
servers, no `CLAUDE.md` discovery outside the task. An evaluation that picks up
your `~/.claude` is not reproducible anywhere else.

**Baselines are inputs, not results.** A baseline lives beside the dataset it
belongs to; a run records only the comparison it made against one. That is what
keeps `aeval report` a function of the database alone.

**A run where tasks failed is still a successful command.** A resolve rate below
1 is the normal outcome of an evaluation, not a malfunction. Only a run that
could not be carried out at all exits non-zero.

## Quickstart

Requires Go 1.25+. The demo dataset's two pytest tasks need an interpreter with
`pytest`; nothing else in the pipeline needs Python at all.

```sh
go build -o bin/aeval ./cmd/aeval

# Check a dataset without running anything.
./bin/aeval validate --dataset datasets/demo

# Evaluate. good.sh patches the workspace correctly, so a healthy pipeline
# reports "passed 3 (100.0%)".
#
# AGENTEVAL_PYTHON must name an interpreter that can import pytest. The command
# below fails loudly rather than silently scoring 0/3.
AGENTEVAL_PYTHON="$(python3 -c 'import pytest, sys; print(sys.executable)')" \
  ./bin/aeval run --dataset datasets/demo \
                  --agent mock --bin tests/testdata/agents/good.sh \
                  --report reports/demo.html

# Browsable index plus one page per run, all openable from file://
./bin/aeval report --index --out-dir reports/site

# Or serve the reports the commands above recorded, at http://127.0.0.1:8080
./bin/aeval serve
```

To evaluate the real thing instead of the demo:

```sh
export ANTHROPIC_API_KEY=...
./bin/aeval run --dataset datasets/demo --model sonnet --concurrency 4
```

## Commands

| Command | What it does |
| --- | --- |
| `aeval collect` | Turn a recorded Claude Code session into a dataset with a baseline. |
| `aeval validate` | Check a dataset without running anything. |
| `aeval run` | Evaluate a dataset and record the result. |
| `aeval matrix` | Run a sweep over one factor axis, or attribute one that already ran. |
| `aeval report` | Render a run, a comparison, a sweep, or the whole index as HTML, and record what was rendered. |
| `aeval serve` | Serve the recorded reports over HTTP. |
| `aeval list` | List recorded runs, filtered by sweep or axis. |
| `aeval version` | Print the build, schema and payload versions and the registered agents. |

Exit codes: `0` success, `1` the operation failed, `2` the command line was
wrong, `130` interrupted.

## Concepts

| Term | Meaning |
| --- | --- |
| **Task** | One entry in a dataset: a prompt, an initial workspace, an output contract, and graders. |
| **Cell** | One task × one sample. The unit of work, and the unit of storage. |
| **Sample** | A repeat of a task. `--samples k` gives pass@k. Each sample gets a fresh sandbox. |
| **Grader** | Scores one cell. Several graders combine into a weighted score. |
| **Factor** | One of three things a sweep may vary: **harness** (the implementation and its configuration), **model**, or **skill**. |
| **Level** | One value of the swept factor: the control, or an alternative. One level is one run. |
| **Control** | The level every other level is subtracted from. Not a special record — just the one the others are measured against. |
| **Baseline** | A recorded attempt a task is compared to, usually exported from a real session by `aeval collect`. |
| **Run** | One invocation of `aeval run`, identified by a UUID, stored with its dataset hash. |

A cell **passes** when its composite grader score reaches the threshold
(`--threshold`, 1.0 by default, i.e. every weighted grader must pass).

## Dataset format

A dataset is a directory with a `dataset.yaml` and, optionally, JSONL tasks,
prompt files, workspace assets, and grade-time test suites.

```
datasets/demo/
├── dataset.yaml                  # manifest: defaults, filter, tasks file
├── tasks.jsonl                   # one task per line
├── prompts/fix-average.md        # referenced by prompt_file
├── workspaces/fix-average/       # copied into the sandbox as the initial state
├── grades/implement-slugify/     # injected only at grade time (hidden suite)
└── baselines/<id>.json           # recorded reference attempts
```

Every path inside a dataset — `prompt_file`, `assets`, `suite`, `baseline.file`,
`workspace.repo` — is resolved relative to the directory the dataset file is in,
not the shell's working directory. The same document therefore behaves the same
way wherever it is invoked from.

### Task fields

```yaml
task:   id, version, category, difficulty, tags
prompt: prompt | prompt_file, system_prompt
init:   workspace: {type: empty|files|git, ...}
output: type: patch|files|stdout|none, paths, ignore_paths, expect_answer
score:  graders: [...], limits: {max_turns, timeout_s}
base:   baseline: {file, notes}
origin: session, prompt_index   # the recording a task came from, and which prompt
meta:   metadata: {...}          # free-form provenance, never interpreted
```

`session` and `prompt_index` are the two fields the engine reads rather than
merely stores, and they exist because `metadata` is contractually opaque: a
value the engine interprets cannot live in the block it never interprets.
`collect` writes both; a hand-written task may leave them out.

`workspace.type: git` takes `repo`, `ref` (a commit SHA is reproducible, a branch
is not) and `subdir`; the upstream `.git` is dropped and the baseline rebuilt, so
an agent cannot read the fix out of the history. `setup` runs before the agent and
is re-baselined afterwards, so installing dependencies does not count as the
agent's change. `run --in-place` works in `repo` itself instead of a copy, which
is why it refuses `subdir`, `setup` and an explicit `keep_git_history: false` —
see the section on it above.

`graders[].weight` is a pointer, which is how `weight: 0` ("run it, do not score
it") stays distinct from an unset weight (= 1). `required: true` fails the whole
task regardless of the other graders.

### Manifest

```yaml
name: demo-basic
description: ...
tasks_file: tasks.jsonl     # or an inline `tasks:` list
defaults: {...}             # merged into every task
filter: {...}               # narrow which tasks load
```

## Graders

| Type | Scores |
| --- | --- |
| `pytest` | A Python test suite. `paths` (files to run), `suite` (a hidden suite injected only at grade time), `min_passed`/`max_failed` thresholds. |
| `patch` | The diff: `require_changed_paths`, `forbid_changed_paths`, and size bounds. |
| `exact_match` | A stdout answer, after a declared list of normalizations. |
| `command` | An arbitrary command's exit status and output. |
| `llm_judge` | A model's verdict against a rubric. Needs a reachable endpoint, so it is absent from the offline test suite. |

## Metrics

A run headlines a small set: cells, pass rate, tokens, cost, wall clock,
tokens/cell, timeouts, harness errors, and — when any cell has a baseline — the
token and time deltas against it. Timeouts and harness errors are always shown,
even at zero: a header that lists only the problems it has makes an absent one
invisible. `timeouts` counts cells that ran out of budget (a fact about the
agent); `harness_errors` counts cells the harness could not measure at all (a
fact about the run).

Attribution compares seven metrics pairwise, in both directions, because a
token saving nobody could act on is not a finding:

`pass rate` (marked **primary** — correctness is what a change is judged on),
`mean score`, `tokens`, `cost`, `time`, `turns`, `tool calls`.

`LowerIsBetter` decides which side counts as winning. Direction is decided by the
win/loss split, not by the mean, because one outlier moves a mean and not a split.

## Attribution: `aeval matrix`

A sweep document names one axis, a shared configuration, and the levels to try:

```yaml
name: demo-harness-sweep
dataset: ../datasets/demo
axis: harness
control: stock

agent:                     # the configuration every level starts from
  kind: mock
  bin: ../tests/testdata/agents/good.sh

levels:
  - name: stock
  - name: terse-prompt
    agent:
      append_system_prompt: Answer with the bare number and nothing else.
  - name: scripted-failure
    agent:
      bin: ../tests/testdata/agents/bad.sh
```

A level's `agent` block is partial: it replaces only the keys it names. Which
keys a level varies is recorded with the level — `terse-prompt` varies one key,
the control varies none — because that is the evidence a reader needs to check
the one-axis claim. `aeval matrix --dry-run` prints the plan and writes nothing;
`aeval matrix --sweep <id>` attributes a sweep that already ran, without
re-running anything.

Run it:

```sh
aeval matrix --file examples/matrix-harness.yaml --db aeval.db
aeval matrix --sweep <id> --json          # the attribution as JSON
aeval report --sweep <id> -o sweep.html   # the attribution as a page
```

### What it will not claim

The separation between levels is tested with an **exact sign test** over the
non-tied pairs: could a split this lopsided come from a fair coin? Three
consequences are stated in every report rather than left implicit:

- It is a *significance* test and nothing else. It says nothing about whether a
  difference is large enough to care about.
- With fewer than six non-tied pairs it cannot reach p < 0.05 however lopsided
  the split. The demo dataset has three tasks, so the demo sweep reports what it
  found and declines to call it real. That is the intended behaviour, not a bug.
- Every finding carries the correctness result beside it, so a token saving can
  never be read on its own.

## Collecting from a session

`aeval collect` reads a recorded Claude Code transcript and writes an ordinary
dataset, which means a re-run can be measured against what the session actually
cost.

```sh
aeval collect --session ~/.claude/projects/<project>/<id>.jsonl --out datasets/mine
aeval collect --latest --out datasets/mine
aeval collect --cwd ~/code/proj --out datasets/mine
```

A replay runs the agent in a workspace of its own, and Claude Code records a
transcript there too. That transcript is the replay's output, not a recording:
`collect` leaves it out of `--list` and never picks it for `--latest`, and
naming one with `--session` is refused.

The tasks it emits have a prompt, a working directory, a baseline (tokens, busy
time, turns, tool calls) and **no grader**: a transcript carries no ground truth,
so the task is measurable and unscorable. Grading it as a failure would invent a
verdict; grading it as a pass would invent the opposite. Attach a grader
explicitly with `--grader` if you have one.

Every baseline also records the machine it was collected on (`machine`: a
hashed platform identifier — `IOPlatformUUID` on macOS, `/etc/machine-id` on
Linux, `MachineGuid` on Windows — plus host name, OS and arch for the reader).
`run` and `matrix` compare it with the machine they run on and **refuse to
start** (exit 2, before any run row is written) when they differ: the
baseline's directories, repository and tools belong to that machine, and a
replay elsewhere would be measured against an environment it is not running in.
Replay on the collecting machine, or re-collect there. Baselines with no
`machine` (hand-written, or collected before this) are not checked.

### How much of a session one task covers

`--split` decides that, and the default is `session`: one task holding the
opening request, measured against the turns that answered it.

On a session the human kept asking in, that is the **first prompt only**. The
baseline stops at the next prompt, because the whole trajectory answers
questions this task never asks and a replay measured against it would be
reported as cheaper than a recording it does not correspond to. The prompts
after the first are therefore not collected at all — which is a fact nothing
else in a one-task dataset records, so `collect` states it, in the task's
baseline notes and on stderr:

```
warning: 1 session(s) hold more prompts than this dataset collected: 1f2114cd (1 of 3).
         Re-run with --split turn to collect each prompt as its own task.
```

`--split turn` emits one task per prompt, each measured against its own slice,
and `--min-prompt-runes` drops conversational noise like "ok" or "try again"
under that split — the same warning tells you when it did.

### Replaying a session's turns as one sequence

`--split turn` plans each prompt as an independent cell: its own fresh sandbox,
run in whatever order `--concurrency` happens to schedule. That is the right
measurement for "how does this agent do on this prompt", and the wrong one for a
recording the human kept going — the second prompt asks about the file the first
one wrote, so a replay in a fresh directory asks a question about a file that was
never there. `--chain-sessions` makes the collected provenance load-bearing:

```sh
aeval run -d datasets/mine --chain-sessions
```

- Tasks sharing a `session` run in **one workspace**, in `prompt_index` order —
  the order the prompts were asked, not the order the tasks file lists them.
- The chain materialises that workspace once, before its first cell, and removes
  it after its last. The later cells start from what the earlier ones left
  because nothing put the directory back in between.
- A task with no `session` is its own chain, which is what keeps the flag from
  changing an ordinary dataset at all.
- Samples stay independent: a chain is replayed once per sample, so sample 1
  never starts from sample 0's leftovers.
- Chaining needs `--concurrency 1`, and says so as a usage error rather than
  quietly serialising: two cells of one chain in flight would write over each
  other with nothing on the row to record that it happened.
- `--resume` refuses a chain that is only half recorded, naming it and how much
  is left. A chained cell's predecessor's workspace went away with its cell, so
  the rest would start from nothing and their rows would describe the missing
  files rather than the agent.

Because of that, a chained run's cells are a **sequence, not repeats**: `k=1` is
the usual setting, and a `pass_rate` over a chain answers "did the whole
conversation get there". The run row records that it chained, the report header
says so, and comparing a chained run with a plain one reports the difference
instead of showing the deltas as like-for-like.

When `--out` already holds a dataset, `collect` appends to it. Tasks whose
recording (the same session and prompt) is already in it are skipped, and a
task whose id is taken by a different recording gets a `-2` suffix. The
existing tasks and manifest are left as they are. `--dry-run` shows what would
be appended, and `--overwrite` replaces the dataset instead.

### Binding a task to the directory it ran in

A collected task replays in an **empty** workspace, because a transcript records
the prompt and the answers but not the files that were checked out. That is
honest and nearly useless: the recording ran against a real tree, so the re-run
is measuring a different task. `--workspace-cwd` binds each task to its own
recording's directory instead:

```sh
aeval collect --session <id>.jsonl --out datasets/mine --workspace-cwd
```

- A directory that is a git repository **with a commit** becomes `type: git`,
  pinned to the commit the recording's session started from. That commit comes
  out of the session-context block a recent Claude Code writes at the top of a
  transcript, whose `Recent commits` list starts at HEAD; it is abbreviated to
  ten hex characters there and is resolved to a full object name here.
  - Where the capture named no commit — an older build, or a session that began
    outside a repository — the task is pinned to the recorded **branch**
    instead, and its notes say the replay starts from that branch as it stands
    now.
  - Where the capture named a commit the repository no longer holds — a rebase,
    a force-push, a `gc` — the same fallback happens, and the notes say *that*,
    because "the capture did not record it" and "the repository has moved on"
    are different facts about a comparison.
  - Where the recording started with uncommitted changes, the notes say how many
    paths a commit cannot reproduce. The replay starts from a clean checkout of
    the commit, which is not where the session started, and a reader who is told
    only the commit would take it for one.
  - A pinned commit makes the materialiser do a **full** clone rather than a
    shallow one, since an arbitrary commit is not in a depth-1 clone. Datasets
    collected before this change name a branch and clone shallowly; their
    `dataset_hash` changes when they are re-collected, because the ref did.
- Anything else — no repository, or a repository with no commits — is refused,
  naming the directory and pointing at `--workspace-repo`. Nothing is copied:
  a copy would be the directory *after* the recording finished, so if that
  session fixed something in there the replay would start from the answer, and
  the baseline comparison would measure the copy rather than the agent.
A directory that no longer exists is an error naming it, not a silently empty
task.

Materialising a `type: git` workspace is where an empty repository would do its
damage: `git clone` of a repository with no commits *succeeds* and carries
nothing, so the run would silently measure an empty directory. It is refused
instead, by name and with the reason (no commit at the branch, versus a ref that
does not exist — the second is not the repository's fault). `--workspace-cwd`
never reaches that path: it refuses a commit-less repository up front.

Without the flag, an empty replay carries a note naming the recorded directory —
so the gap is visible in the report rather than implied by an empty page, and
`collect` prints the same thing to stderr.

### Running in the recorded repository (`--in-place`)

Even pinned to a commit, a replay happens in a **copy**: the tree is cloned into
a workspace under `--work-root`, the upstream `.git` is dropped, and the agent
sees a repository with one commit in it. `--in-place` runs the agent in the
recorded directory itself instead:

```sh
aeval run --dataset datasets/mine/dataset.yaml --in-place
```

The agent is checked out at the commit the task names, works in the real tree,
and the run puts the directory back when it is done — the branch, the commit and
the tracked files it found. The point of the mode is fidelity: the agent gets
the tree as it was, and the diff is measured against the commit the recording
started from rather than against a copy of it.

It is the one mode that touches something the harness does not own, so it is
built the other way round from the rest of the tool: **everything it cannot undo
is refused before the first cell.**

- A repository with **uncommitted changes** is refused, naming the paths. The
  commit comes back; the changes on top of it cannot. Commit or stash first.
- `--concurrency` must be 1. Two cells can name the same directory, and nothing
  in the scheduler keeps them apart; one at a time is the only arrangement in
  which the state a cell puts back is the state it found.
- `--keep` is refused, because this run creates no workspaces to keep.
- A task with no repository, a remote instead of a directory, a repository path
  relative to the dataset, a `subdir`, `setup` commands, or an explicit
  `keep_git_history: false` is refused, each naming the task. The last is a
  measurement-integrity refusal: the mode hands the agent the repository, so a
  task that asked for the history to be hidden cannot be honoured here.
- A directory that is a linked worktree, a submodule checkout, bare, or already
  held by another run is refused.

What a restore does **not** cover, and says so in the banner before the run:

- Branches, tags and stashes the agent creates or moves. They are reported in
  the cell's restore notes rather than deleted, because the harness cannot know
  which of them the agent meant to keep.
- Files your `.gitignore` hides. Removing them would take your `.env` or
  `node_modules` with them, so new ones are left in place and named in the
  notes.
- Anything pushed to a remote. The repository's whole history is readable by the
  agent for the whole run, and a push is not something a run can put back.

Two more things worth knowing before pointing this at a repository you care
about. The harness writes its own Claude Code transcript for that directory, so
a later `aeval collect --cwd <dir>` will pick the replay sessions up as if they
were your recordings. And the agent runs with `bypassPermissions` in a directory
it can write anything into.

Each repository holds a lock (`.git/agenteval/inplace.lock`) naming the commit
and branch to return it to. A second run refuses rather than waiting, and a run
that finds a lock left by a process that is gone prints the commands that repair
the directory:

```
git -C <repo> update-ref refs/heads/<branch> <commit>
git -C <repo> checkout --force <branch>
git -C <repo> clean -ffd
```

The last command discards everything in the directory that was never committed,
so run it only once you are sure there is nothing there you want. A restore that
fails is recorded on the cell, rendered as a failure in the report, and returned
as a non-zero exit — a person whose repository is at the wrong commit must not
be told by a zero exit that everything is fine.

## Reports

`aeval report` writes a self-contained `.html` file — CSS and JS inlined — that
opens from `file://`, survives being an email attachment, and needs no server. It
can render four things:

```sh
aeval report --run <id>               # one run
aeval report --compare <a> <b>        # two runs side by side
aeval report --sweep <id>             # an attribution
aeval report --index --out-dir site/  # the listing, plus one page per run
```

Reports have a size budget (8 MiB of embedded JSON by default). When a run
exceeds it the builder sheds by priority: passing cells lose their trajectory and
diff first, failing cells keep their evidence longest, and every degradation is
recorded in the report's own warnings. A trajectory that is truncated keeps both
ends — the opening events frame the task, the last ones carry the answer.

A run's header also states the configuration the run drove, including its
skills: which plugin directories were declared, whether they reached the agent,
and which flag removed them if not. Declaring a directory is not loading it —
`--bare` and `--disable-slash-commands` each remove the user's skills on their
own, and the clean-room default turns both on. Two runs that differ only in
their skills therefore used to compare as identical; now the comparison names
that as the difference, and a comparison where one side recorded no
configuration says *that* instead of quieting down into "nothing changed". The
whole `agent.Spec` is stored per run (`runs.agent_spec_json`, schema v4), since
the four label columns — agent, model, binary, backend — cannot identify a run
that differs only in its skills. Rows written before v4 answer this question
with silence, and the page keeps silence apart from "none".

The header also names the model, and it says more than the label can. `--model`
is optional, so the label is empty on every run that did not pass it — and an
empty label used to render as no model chip at all, which made three different
facts look the same: no model was requested, the CLI's default was used, or
nobody recorded it. The stream settles it: its init event announces what the CLI
is about to use, and its per-model accounting is keyed by what was actually
billed. The page reads the second and falls back to the first, and states which
of the four cases it is in:

| the record says | the header says |
| --- | --- |
| nothing, either way | `model not recorded` |
| only `--model sonnet` | `model sonnet (the stream named no model)` |
| only the stream named one | `model default (the CLI chose it): claude-opus-4-7` |
| both, and they differ | `model sonnet, but the stream billed claude-haiku-4` |

The third row is the ordinary shape of a real run, and the fourth is the one
worth colouring: every number below it then measures a model the run did not
choose. An alias is not a substitution — `--model sonnet` is answered by a
stream naming `claude-sonnet-4-5-20250929`, and comparing those two strings
directly would warn on every ordinary run, which is how a warning stops being
read, so one name containing the other counts as agreement. Each cell carries
its own answer too, because a run is not necessarily one model: a fallback
mid-run shows up on a single cell's stream and nowhere else. And when two runs
declared the same thing — or nothing at all — and were served differently, the
comparison names that rather than letting equal labels stand in for an equal
measurement.

None of this needed a new column: the trajectory was already stored per attempt,
so runs recorded before the header said any of it are answered here too.

### Recording what was rendered, and serving it

`aeval report` also records *that* it rendered something, and this is the one
part of a report that cannot be re-derived. The row holds the request — which
view, of what subject, with which options, already resolved — and the SHA-256 of
the payload. It does not hold the HTML, and it does not hold the payload. A
payload is a function of the database; storing it beside the database would be a
second answer to the same question, one that stops being updated the moment the
rows move.

The rows are a log, not a state. Rendering the same view twice writes two rows
with two ids, and the pair of hashes is the evidence. Ids are random rather than
sequential, because a numbered log invites reading the number as a version, and
random rather than content-addressed, because the content is exactly what two
registrations can share while still being two events.

`aeval serve` lists them and rebuilds each one on demand:

```sh
aeval serve --db runs.db --addr 127.0.0.1:8080
```

`GET /` lists the recorded reports, `GET /r/<id>` rebuilds one from the database
and renders it, and `GET /r/<id>.json` returns the same document as JSON. Every
page carries a banner saying how the document it just rebuilt relates to the one
that was recorded:

| the banner says | when |
| --- | --- |
| `Recorded report.` | the rebuild produced the recorded bytes |
| `Recorded in an older payload format.` | the row predates a format change, so the two hashes are not comparable and their disagreement says nothing about the data |
| `This page is not the recorded document.` | the rows moved, or the code that projects them changed; the server names both causes rather than guessing which |

The hash covers exactly the bytes `/r/<id>.json` returns, so a script can check
the claim without parsing the page. The response carries it in four headers:
`X-Aeval-Reported-Hash` and `X-Aeval-Current-Hash` (what was registered, versus
what this read produced), `X-Aeval-Hash-Matches`, and `X-Aeval-Data-Version`. A
registration whose run has since been deleted still answers: it says which run
it was, and that the subject is gone.

Only `aeval report` and the replay button register. The report `run` and
`matrix` write alongside their own output is a convenience, not a registration —
nobody asked for that view, and recording it would fill the log with views that
were never requested.

### Browsing this machine's sessions and replaying them

`aeval serve` also serves the sessions recorded on this machine, by Claude Code
(`~/.claude/projects`) and by Cursor (its `state.vscdb`):

| route | what it is |
| --- | --- |
| `GET /sessions` | every recorded session, newest first, titled by the tool's own name for it (Claude Code's generated title, Cursor's conversation name, else the first prompt); filter by source or text. Transcripts a replay produced are not recordings, so they are left out |
| `GET /sessions/<source>/<id>` | the session in full — directory, branch, commit, models, tokens, cost, every prompt, answer and tool call — beside a replay form and the replays already made of it |
| `POST /sessions/<source>/<id>/replay` | starts a replay under the picked harness, skill set and model |
| `POST /replays/<id>/cancel` | terminates a queued or running replay; a running one has its agent process killed and is recorded as failed with `cancelled` |
| `GET /replays`, `GET /replays/<id>` | replay status and log; the page refreshes until it finishes, then links the report |

A replay is `collect --split turn --workspace-cwd` followed by `run
--chain-sessions`, then a registered run report: each prompt becomes a task,
the tasks run in order in one workspace checked out from the recording's
repository (an empty one when the directory is not a repository, which the log
says), and the report opens at `/r/<id>` like any other. Replays run one at a
time, so two agents never contend for the machine whose busy time they are
compared against. A replay left unfinished by a server that stopped is closed as
failed when the next one starts.

The harness and the skill set are **named presets**, not values the page sends:

```sh
aeval serve --replay-config examples/replay-presets.yaml
```

A harness is a partial agent block in the matrix vocabulary, and may not set
the model or `plugin_dirs` — those are the other two choices, and a harness
that set them would override the pick. A skill directory that does not exist is
an error at startup. Picking skills under a harness that passes
`--disable-slash-commands` is refused on the form, because the replay would load
none of them. The model is free text with suggestions, checked against a model
id pattern and forwarded to `--model` as one quoted word. Without a presets file
the page offers `clean-room` and `host` (Claude Code with this machine's own
settings), the skill set `none`, and the CLI's default model.

These pages read every transcript on this machine and launch an agent with
permission prompts bypassed, so they are guarded more strictly than the reports:

- **Loopback only.** `serve` refuses a non-loopback `--addr` unless
  `--no-sessions` is given, which serves the reports alone.
- **No other host names.** Every session route answers only `localhost` or an IP
  literal in `Host`, which is what stops a page whose DNS was rebound to
  127.0.0.1 from reading the transcripts.
- **No cross-site forms.** Every POST carries a token the server generated at
  startup and an `Origin`, when present, of the server itself. A page on another
  site cannot start a replay.
- **There is no login.** Anything running as you on this machine that can open a
  socket can use the pages.

## Package layout

```
cmd/aeval/         the command line
internal/agent/    Agent interface, the Claude Code driver, the mock, the registry
internal/sandbox/  workspace materialisation and command execution
internal/dataset/  dataset and task loading, baselines, path resolution
internal/grader/   the five graders and the registry
internal/trace/    Claude Code stream-json -> a normalized trajectory
internal/session/  Claude Code transcript -> tasks with baselines
internal/runner/   scheduling, timeouts, workspace lifecycle, recording
internal/store/    the SQLite schema, runs and attempts
internal/attrib/   factor axes, sweep plans, paired comparison, the sign test
internal/report/   the view model, the loader, the HTML renderer, the report registry
internal/server/   the HTTP server: recorded reports, this machine's sessions, the replay form
internal/replay/   replay presets and the job queue behind the replay button
internal/machine/  the machine identity collect stamps and replays check
internal/llm/      the minimal client used by llm_judge
tests/             end-to-end tests driving the built binary
```

## Testing

```sh
go test ./...

# The pytest grader needs an interpreter that can import pytest. The demo
# dataset has two pytest tasks; without this the suite skips rather than
# reporting a 0/3 that looks like a broken agent.
AGENTEVAL_PYTHON="$(python3 -c 'import pytest, sys; print(sys.executable)')" go test ./...
```

`tests/e2e_test.go` drives the built binary the way a user would: a dataset on
disk, a real subprocess, a real SQLite file, a real HTML report. It needs no
model, no network and no credentials.

## Known gaps

Stated here rather than discovered later:

- **The session pages have no login.** They are loopback-only and refuse
  foreign host names and cross-site forms, but any local process running as you
  can read every transcript through them and start a replay. Pass
  `--no-sessions` where that is not acceptable.

- **Only one agent implementation.** Claude Code is the only real driver; the
  registry is the seam, and a second driver is a new file.
- **The container backend is unreachable.** `internal/sandbox/docker.go` is
  written (364 lines, with `--pids-limit`, `--memory`, `--cpus`), but
  `internal/runner` hard-codes the local backend, there is no flag or dataset
  field to select it, and no test starts a container. Either wire it up with an
  end-to-end test or delete it — a reference implementation nobody calls is worse
  than not having one, because it looks like isolation is one flag away.
- **`llm_judge` is not covered end to end.** It needs a reachable endpoint, and
  the e2e suite must run offline.
- **No `oracle` field and no task self-check.** A reference solution can only be
  stashed in `metadata`, which the engine never interprets, so `resolve_rate`
  cannot be explained: there is no way to tell a hard task from a broken one.
- **`Baseline.RestorePaths` is written but unused.** It is the primitive behind
  "restore the test files before grading, so an agent that edits the tests is
  scored on its actual work" — and it has no caller, no test and no dataset
  field.
- **The report server is read-only and unauthenticated.** `aeval serve` rebuilds
  recorded reports from the database and serves them; there is no login, no
  write path and no per-user view. It binds to `127.0.0.1` by default and warns
  when told to listen anywhere else, because anyone who can reach the port can
  read every recorded report.

`docs/real-repo-eval.md` is the design for evaluating agents against a real
repository rather than a three-file demo, and its §1 tables are kept current.
`docs/design.md` is the original brief this implementation answers.

## License

Apache-2.0.
