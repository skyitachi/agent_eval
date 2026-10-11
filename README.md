# agenteval

**English** | [简体中文](README.zh-CN.md)

A small, opinionated harness for evaluating coding agents. It runs a dataset of
tasks against an agent, grades the result with pluggable graders, persists every
cell to SQLite, and renders self-contained HTML reports.

Today the only agent implementation is **Claude Code**, but the model is
free-form and forwarded untouched, so any endpoint the CLI can reach works. The
grading layer, the metric layer, and the sandbox backend are all registries —
adding one is a new file, not a fork.

```
aeval run  ->  SQLite  ->  aeval report   (one self-contained .html)
                       ->  aeval serve     (browsable UI)
                       ->  aeval compare   (two runs side by side)
```

## Design stance

Three decisions shape everything else, and they are worth stating because they
explain some otherwise-surprising behaviour.

**A report is a function of the database, not of the process.** Two weeks later,
from a copied `.db` on another machine, `aeval report` must reproduce the
document that was written during the run. The renderer therefore reads only
persisted state. The JSON embedded in the `.html` file and the JSON served by
`/api/runs/{id}` are produced by the same code path from the same rows, and a
test asserts they are byte-identical.

**Measurements are never retried away.** A timeout, a non-zero exit, an error
result — those are data. `retries` governs only the case where the agent process
could not be started at all, and it defaults to 0.

**An evaluation must not silently inherit the operator's machine.** The default
agent preset is a clean room: no user hooks, no auto-memory, no stray MCP
servers, no `CLAUDE.md` discovery outside the task. An evaluation that picks up
your `~/.claude` is not reproducible anywhere else.

One more decision only becomes visible when the same harness is used as an RL
environment, but it was made just as early: **a reward is a projection, not the
grade itself.** A grade says whether the attempt satisfied the task; a reward
says what the trainer is paid. Merging them costs nothing today and makes it
impossible to tell a failed task from a deliberately sparse reward forever
after. And a reward can only be as trustworthy as the task, which is why
`aeval audit` proves that the reference solution passes, that doing nothing
fails, and that repeats agree — and insists on reporting "could not be measured"
separately from "measured, and broken".

## Quickstart

Requires Go 1.25+. The demo dataset needs a Python with `pytest` available.

```sh
go build -o bin/aeval ./cmd/aeval

# See what is registered.
./bin/aeval registry

# Check a dataset without running anything.
./bin/aeval validate --dataset datasets/demo

# Evaluate. The demo dataset is self-contained and the mock agent runs a
# script; good.sh patches the workspace correctly, so a healthy pipeline
# reports "resolved 3/3 (100.0%)".
#
# AGENTEVAL_PYTHON must name an interpreter that can import pytest -- the
# pytest grader uses it, and nothing else in the pipeline needs Python at
# all. The command below fails loudly rather than silently scoring 0/3.
AGENTEVAL_PYTHON="$(python3 -c 'import pytest, sys; print(sys.executable)')" \
  ./bin/aeval run --dataset datasets/demo \
                  --agent mock --bin tests/testdata/agents/good.sh \
                  --report reports/demo.html

# Browse every recorded run.
./bin/aeval serve            # http://127.0.0.1:8787

# Inspect a dataset: can the reference solution pass? does doing nothing fail?
# do repeats agree? Reference solutions live in a task's `oracle` field, are
# never shown to the agent, and never take part in grading.
./bin/aeval audit --dataset datasets/demo --repeats 2
```

To evaluate the real thing instead of the demo:

```sh
export ANTHROPIC_API_KEY=...
./bin/aeval run --dataset datasets/demo --model sonnet --concurrency 4
```

To serve the same dataset to a training loop as RL environments:

```sh
./bin/aeval serve --dataset datasets/demo --rl --rl-allow-exec
```

Then `POST /api/rl/envs` to open an environment, `PUT .../files` to submit an
answer, and `POST .../verify` to collect the reward. See
[`docs/rl-environment.md`](docs/rl-environment.md).

## Concepts

| Term | Meaning |
| --- | --- |
| **Task** | One entry in a dataset: a prompt, an initial workspace, an output contract, and graders. |
| **Cell** | One task × one sample. The unit of work, and the unit of storage. |
| **Sample** | A repeat of a task. `--samples k` gives pass@k. Each sample gets a fresh sandbox. |
| **Grader** | Scores one cell. Several graders combine into a weighted score. |
| **Metric** | Aggregates all cells into a run-level number. Metrics never touch a cell's verdict. |
| **Reward** | The scalar a trainer is paid for one attempt, projected from the grade by the task's **reward contract**. Equal to the composite score by default; can be declared sparse (0/1). |
| **Oracle** | A reference solution for a task, used only by `aeval audit`. It is never shown to the agent and never takes part in grading. |
| **Run** | One invocation of `aeval run`, identified by a UUID, stored with its dataset hash. |

A cell **passes** when its composite grader score reaches `grader_threshold`
(1.0 by default, i.e. every grader must pass). `resolve_rate` is the fraction of
cells that passed — the same reading as SWE-bench's resolved rate.

**Grades and rewards answer different questions.** A grade says whether the
attempt satisfied the task: it has weights, partial credit and a threshold, and
it is a *judgement*. A reward says what the trainer is paid: it is a *scalar*,
and it goes straight into an advantage estimate. Keeping them apart costs one
field and buys the ability to tell "the task was judged a failure" from "the
reward was designed to be sparse" — two things with entirely different
consequences for training. The evaluation path and the RL environment service
share one projection (`internal/reward`); two implementations would mean two
reward scales.

## Dataset format

A dataset is a directory with a `dataset.yaml` and, optionally, JSONL tasks,
prompt files, workspace assets, and grade-time test suites.

```
datasets/demo/
├── dataset.yaml                  # manifest: defaults, filter, tasks file
├── tasks.jsonl                   # one task per line
├── prompts/fix-average.md        # referenced by prompt_file
├── workspaces/fix-average/       # copied into the sandbox as the initial state
└── grades/implement-slugify/     # injected only at grade time (hidden suite)
```

`dataset.yaml` carries dataset-wide defaults that each task inherits; a task's
own value wins.

```yaml
name: demo-basic
version: 1.0.0
tasks_file: tasks.jsonl

defaults:
  limits: {max_turns: 30, timeout_s: 600}
  output: {type: patch, ignore_paths: ["*.pyc", "__pycache__/**"]}

# Narrow the dataset without editing the tasks.
filter:
  categories: [bugfix]
  tags: [python]
  limit: 50
```

A task in `tasks.jsonl`:

```json
{
  "id": "implement-slugify",
  "category": "feature",
  "difficulty": "medium",
  "tags": ["python"],
  "prompt_file": "prompts/implement-slugify.md",
  "workspace": {"type": "files", "assets": "workspaces/implement-slugify"},
  "output": {"type": "patch", "paths": ["slugify.py"]},
  "graders": [
    {"type": "pytest", "name": "hidden-suite", "weight": 0.8,
     "config": {"suite": "grades/implement-slugify", "min_passed": 6}},
    {"type": "patch", "name": "touched-slugify-only", "weight": 0.2,
     "config": {"require_changed_paths": ["slugify.py"]}}
  ],
  "limits": {"max_turns": 30, "timeout_s": 600},
  "reward": {"type": "score", "notes": "dense: pay the weighted score"},
  "oracle": {"patch_file": "oracles/implement-slugify.patch"},
  "restore_paths": ["test_slugify.py"],
  "forbid_changed_paths": ["pyproject.toml"]
}
```

**Workspace** — where the agent starts. `type: files` copies `assets` into the
sandbox (optionally with `files` written verbatim). `type: git` clones `repo` at
`ref`, optionally restricted to `subdir`. `setup` commands run after the
workspace exists and before the agent starts.

**Output contract** — what the agent is expected to produce, and therefore what
graders see: `patch` (working-tree diff), `files` (a set of paths), `stdout` (a
textual answer), or `none`. `paths` / `ignore_paths` narrow the diff, which
matters because build artefacts otherwise pollute every patch.

**Reward contract** — `reward.type` is `score` (the default: dense, the
composite score clamped to `[0,1]`) or `pass` (sparse 0/1). Omitting `reward` is
identical to `score`, so adding the feature cannot silently change an existing
dataset. An attempt rejected by an anti-gaming guard pays **zero**, in the dense
case too — otherwise rewriting the assertions becomes the cheapest shortcut in
the environment.

**Oracle** — `patch`, `patch_file` or `files`, plus an optional `apply` list of
commands. It is read only by `aeval audit`, to check that the task is actually
solvable. The engine never reads it during evaluation and never hands it to the
agent.

**Guards** — `forbid_changed_paths` rejects after the fact (it answers "did the
agent obey the rules"); `restore_paths` checks paths back to the baseline
*before grading* (it answers "can the agent do the work", and it is what
SWE-bench does by default). Both are needed. Paths in `restore_paths` are also
added to the diff's ignore list, or the reset itself would look like an agent
change.

**Graders** — `weight` is a pointer so that "unset" (1) differs from an explicit
`0` ("run and record, but do not contribute to the score"). `required: true`
fails the whole task when that grader fails, whatever the composite says.

**Limits** — `max_turns`, `timeout_s`, `max_cost_usd`, `max_output_tokens`.
`max_turns` is enforced by this harness by watching the event stream, because
the CLI exposes no equivalent flag.

## Graders

| Type | What it checks | Useful config |
| --- | --- | --- |
| `patch` | Shape of the diff: which paths changed, how much, whether anything changed at all. | `require_changed_paths`, `forbid_changed_paths`, `min_changed_files`, `max_diff_lines`, `expect_changed` |
| `pytest` | Runs a test suite and scores from the pass/fail counts. | `suite` (hidden suite injected at grade time), `paths`, `min_passed`, `max_failed`, `python_candidates`, `extra_args` |
| `exact_match` | Compares a stdout answer, after normalisation, against an expected value. | `expected`, `expected_any`, `regex`, `normalize` (`trim`, `strip_code_fence`, …), `case_sensitive` |
| `llm_judge` | Scores free-form output against a rubric using a model. | `rubric`, `criteria`, `model`, `base_url`, `input`, `reference` |
| `command` | Runs an arbitrary command inside the sandbox and maps its exit code to a verdict. | `command`, `pass_exit_codes`, `parse`, `copy`, `env` |

`llm_judge` is the escape hatch for anything the other four cannot express. It
needs a reachable endpoint; configure `base_url` and `api_key_env` rather than
hard-coding a key.

Registering a grader in Go is one `init()` call:

```go
func init() {
    Register("my_grader", func() Grader { return MyGrader{} })
}
```

## Metrics

Metrics are computed after grading and never change a cell's verdict. There are
34 registered run-level metrics; the default set is a curated subset. Run
`aeval registry` for the authoritative list with descriptions.

The headline one is `resolve_rate`. The others fall into groups the report
renders separately: **outcome** (`pass_at_k`, `pass_hat_k`, `mean_score`),
**cost**, **tokens**, **effort** (`mean_turns`, `mean_tool_calls`), **latency**
(`p50`/`p95`), and **health** (`timeout_rate`, `harness_error_rate`,
`tool_call_error_rate`). The health group exists because a run in which the
harness itself failed to measure anything is not a run in which the agent did
badly, and the two must not be averaged together.

Three metrics face a training loop:

| Metric | Meaning |
| --- | --- |
| `mean_reward` | Mean reward per attempt. **Includes never-started and rejected cells**, at their zero — excluding them would report the reward of the well-behaved subsets, which is precisely the number a reward-hacking policy would prefer to be judged on. |
| `reward_signal_rate` | Fraction of attempts with a non-zero reward. Near zero means the batch carries no learning signal, however good its score looks. |
| `guard_violation_rate` | Fraction of attempts rejected by an anti-gaming guard, with a `by_kind` breakdown. Non-zero means part of the reward is measuring compliance rather than capability. |

### Custom metrics

Three forms, declared under `run.custom_metrics` and evaluated in declaration
order (so a later expression may reference an earlier custom metric).

```yaml
custom_metrics:
  # 1 -- aggregate one numeric field over the run's cells, optionally filtered.
  - name: bugfix_resolve_rate
    unit: ratio
    group: outcome
    direction: up
    where: "category == 'bugfix'"
    field: passed
    agg: mean

  # 2 -- a ratio of two aggregates.
  - name: tokens_per_resolved
    unit: tokens
    group: token
    direction: down
    numerator:   {field: total_tokens, agg: sum}
    denominator: {field: passed,       agg: sum}
    zero_denominator: 0

  # 3 -- arithmetic over built-in and previously declared metrics.
  - name: cost_per_resolved_guarded
    unit: usd
    group: cost
    direction: down
    expr: "total_cost_usd / max(resolved_count, 1)"
```

Filterable strings are `task_id`, `category`, `difficulty`, `stop_reason`, and
`tags contains`. Numeric fields include `passed`, `score`, `turns`,
`tool_calls`, `total_tokens`, `latency_ms`, `cost_usd`, and the error flags.

`direction` and `group` are presentation hints, and they are not decoration. A
built-in name was curated by whoever wrote it; `slow_cells` carries no such
guarantee, and a report that colours an increase green is worse than one with no
colour at all. `group` must be one of `outcome`/`cost`/`token`/`effort`/
`latency`/`health` to join an existing section, else it lands in "other".

## Configuration

Everything above can live in `aeval.yaml`, which `aeval` picks up automatically
(also `.aeval.yaml`, `agenteval.yaml`). See
[`examples/aeval.yaml`](examples/aeval.yaml) for a fully commented file.

```sh
aeval run --config examples/aeval.yaml
aeval validate --config examples/aeval.yaml   # check it without running
```

Two rules worth knowing:

- **Unknown keys are errors.** A typo in a config file is otherwise
  indistinguishable from an option that had no effect.
- **Relative paths resolve against the config file's directory**, not the shell's
  working directory. A config is a portable artefact; the directory you happen
  to run it from is not.

Command-line flags override the file. Only flags that were actually given
override anything — a flag left at its zero value cannot be told apart from one
that was not passed, so it does not clobber the file.

## CLI

```
aeval run        Evaluate a dataset and record the results
aeval validate   Check a dataset and a config file without running anything
aeval audit      Inspect a dataset: solvability, necessity, stability
aeval list       List recorded runs
aeval report     Write an HTML report for a recorded run
aeval compare    Write an HTML comparison of two recorded runs
aeval serve      Serve recorded runs over HTTP; with `--rl`, also serve RL environments
aeval registry   List registered agents, graders and metrics
aeval version    Print build and environment information
```

Exit codes: `0` success, `1` operation failed, `2` command-line error, `130`
interrupted. **A run in which some tasks failed still exits 0** — that is a
measurement, not a crash. Without that rule the command would be useless in CI,
where a failing benchmark is exactly what you are trying to observe.

Useful flags:

```sh
# k samples per task, four cells at once, report at the end
aeval run --dataset ds --samples 3 --concurrency 4 --report out.html

# resume an interrupted run, reusing the same run id
aeval run --dataset ds --resume

# stop at the first hard failure
aeval run --dataset ds --fail-fast

# leave workspaces on disk for inspection
aeval run --dataset ds --keep-workspaces --work-root ./work

# grade only the reference solution, without starting the agent
aeval run --dataset ds --mode oracle

# inspect a dataset: can the oracle pass, does nothing fail, do repeats agree
aeval audit --dataset ds --repeats 3 --strict

# serve a dataset as RL environments
aeval serve --dataset ds --rl --rl-allow-exec --rl-max-envs 4
```

**`--mode`** selects what each cell measures: `agent` (the default, runs the
agent), `oracle` (applies the reference solution instead) or `baseline` (grades
the untouched workspace). All three produce results of exactly the same shape,
which is why `runs.mode` cannot be omitted — otherwise "the reference solution
passes" gets read as "the model passes". Reports mark non-agent runs with a
distinctly coloured mode badge.

## Inspecting a dataset: `aeval audit`

A reward you can trust requires a task you can trust. `aeval audit` runs three
checks per task:

| Check | Question | How it decides |
| --- | --- | --- |
| **Solvability** | Can the reference solution pass? | one `oracle` run must pass |
| **Necessity** | Does doing nothing fail? | one baseline run must **not** pass |
| **Stability** | Do repeats agree? | `--repeats` oracle runs, compared grader by grader |

Necessity is what catches **free reward**. A task the baseline already passes
pays a reward for doing nothing: a mediocre task in a benchmark, and a
policy-destroying shortcut under RL.

A check has three states, and the third is the one that must exist: `ok`
(measured, fine), `failed` (measured, broken) and `unknown` (**could not be
measured**). A task with no oracle — free-text answers are not patches — lands
in `unknown`; calling that `failed` slanders the task, and calling it `ok` is a
lie. For the same reason, an oracle that fails to apply is recorded as
`oracle_error`, not `harness_error`: **the dataset's own reference solution
being unusable is not an environment fault**.

`--strict` exits 1 when something is unhealthy; the default exits 0, for the
same reason a failing benchmark exits 0: finding a defect is not the audit
command failing.

## Reports and the web UI

`aeval report` writes one self-contained `.html` file — CSS and JS inlined. It
opens from `file://`, survives being mailed as an attachment, and needs no
server. `aeval serve` renders the same documents with the assets linked
instead, plus a live index.

The report contains the metric summary, per-group breakdowns (resolve rate by
category and difficulty, stop-reason mix, tool usage, score distribution), a
filterable and sortable task table, and a per-task drawer with the graders'
verdicts and reasons, the agent's answer, the diff, and the event timeline.

**The file has a size budget.** A 500-cell run with full traces would produce a
document too large to open. When the payload approaches `max_inline_bytes`
(8 MiB), per-cell *detail* is dropped in priority order — failing and errored
cells keep their evidence longest, passing cells go first — and every reduction
is recorded in the report's own warnings. A truncated trajectory keeps its head
and tail (the task framing and the answer) with an explicit gap marker, because
a truncated tail misleads.

The web server binds loopback by default, the deletion route is off unless
`--allow-delete` is passed, and requests carrying a foreign `Origin` are
refused. It renders model-generated text, so this is not paranoia.

## The RL environment service

With `--rl`, `aeval serve` also exposes the dataset as an environment a training
loop can drive:

```sh
aeval serve --dataset datasets/demo --rl --rl-allow-exec
```

```
GET  /api/rl/tasks                     what can be trained on
POST /api/rl/envs                      open an environment        -> 201
POST /api/rl/envs/{id}/exec            run a command in it
PUT  /api/rl/envs/{id}/files           submit a file
POST /api/rl/envs/{id}/verify          grade, project the reward, destroy -> terminal
POST /api/rl/envs/{id}/delete          destroy without grading
GET  /api/rl/envs/{id}/archive         the evidence behind a verdict
POST /api/rl/rollouts                  run the configured agent once, return its reward
```

The verify response is the thing a trainer needs:

```json
{"task_id":"subtract","reward":1,"reward_kind":"score","score":1,"passed":true,
 "rejected":false,"stop_reason":"verified",
 "run_id":"rl-8b1e04a7-...","task_run_id":"rl-8b1e04a7-.../subtract#0"}
```

Four properties are worth calling out.

**Every reward points at stored evidence.** `run_id` is a real run with
`mode = 'rl'`, so `aeval report <run_id>` renders the evidence rather than asking
the trainer to take the number on trust.

**`error` means the harness failed, not the policy.** Such a rollout carries no
training signal and must be discarded, never learned from as a zero.

**`rejected` means an anti-gaming guard fired.** The reward is already zero; the
flag lets a trainer tell "wrong" from "tampered".

**Verification is terminal.** Re-verifying returns `409` with the original
verdict, and `exec` afterwards returns `409` too. A policy that can act after
seeing the reward would make the verifier an oracle to probe.

This is the only part of the tool that *acts* rather than renders — it starts
sandboxes, runs the agent, and with `--rl-allow-exec` runs arbitrary commands as
the harness's user. Hence four gates rather than one: `--rl` must be passed, the
request must be same-origin, exec has its own flag, and every dimension has a
cap (`--rl-max-envs`, `--rl-ttl`, `--rl-max-execs`, `--rl-max-rollouts`,
`--rl-max-cost-usd`). Reaching a cap returns `429`, never a fabricated task
failure — "you asked too fast" and "the policy did badly" must not be confused
in a training signal.

Every switch and budget can also be written into the `serve.rl` section of
`aeval.yaml` (`enabled`, `allow_exec`, `max_envs`, `ttl`, `max_execs`,
`max_rollouts`, `max_cost_usd`); `examples/aeval.yaml` annotates each one. A
command-line flag overrides the file **field by field**, not section by
section: a config that sets only `allow_exec: true` plus `--rl-max-envs 4` means
both.

See [`docs/rl-environment.md`](docs/rl-environment.md) for the training-loop
walkthrough and the honest limits (turn-level trajectories only, because the
Claude Code CLI emits no token ids or logprobs).

## Sandbox backends

| Backend | Boundary | Use |
| --- | --- | --- |
| `local` | A directory and a controlled environment. No filesystem or network isolation. | Fast iteration, CI on trusted input. |
| `docker` | A real container: mount, network, and process isolation. | Untrusted tasks, reproducible baselines. |

```sh
aeval run --dataset ds --backend docker --image ghcr.io/example/agent:latest
```

Credentials reach a container via `agent.env_from_host: [ANTHROPIC_API_KEY]` —
copied from the harness environment into the sandbox, never written to a file.

## Repository layout

```
cmd/aeval/         CLI: flag parsing, config resolution, exit codes
internal/dataset/  manifest + task spec, JSONL loading, hashing
internal/sandbox/  backend interface, local and docker implementations
internal/agent/    agent runners (claude_code, mock) and stream parsing
internal/trace/    Claude Code event-stream parser
internal/grader/   grader registry and the five built-in graders
internal/reward/   grade -> reward projection (dense or sparse, rejected = 0)
internal/metric/   metric registry, built-in metrics, custom-metric DSL
internal/engine/   orchestration: scheduling, timeouts, retries, recording,
                   plus Verify (guards -> diff -> grade -> reward) shared with rlenv
internal/audit/    the three task checks behind `aeval audit`
internal/rlenv/    RL environment service: tasks, environments, rewards
internal/storage/  SQLite schema and queries
internal/report/   storage rows -> view model -> self-contained HTML
internal/web/      HTTP server over the same view model, plus the /api/rl routes
internal/config/   aeval.yaml loading and validation
internal/llm/      minimal client for the Anthropic Messages API (used by llm_judge)
tests/             end-to-end tests driving the real binary
docs/              design notes (RL environment service, real-repo evaluation)
```

## Testing

```sh
go test ./...                              # everything
go test ./internal/report/ -run XSS        # one concern

# The end-to-end suite needs an interpreter with pytest.
AGENTEVAL_PYTHON="$(python3 -c 'import pytest, sys; print(sys.executable)')" \
  go test ./tests/ -v
```

`tests/e2e_test.go` drives the compiled binary end to end: it builds a run with
a scripted agent that patches code correctly, a second one that patches it
wrongly, and asserts that the verdicts differ, that the report regenerated from
a copied database is identical to the one written during the run, and that every
HTTP route answers as documented. `AGENTEVAL_PYTHON` points the `pytest` grader
at an interpreter that has `pytest` installed.

## Status

Working and tested: dataset loading, both sandbox backends (local exercised
end-to-end; docker implemented but only unit-tested), the Claude Code runner and
stream parser, all five graders, the metric registry and custom-metric DSL,
SQLite persistence, the orchestration engine, HTML reports, the web UI, the
config layer, and the CLI.

The RL-facing parts: the reward contract (dense/sparse, rejected always zero),
the three run modes (agent / oracle / baseline), the two anti-gaming guards
(`forbid_changed_paths` rejects after the fact, `restore_paths` repairs before
grading), `aeval audit` with its three checks, and the `internal/rlenv`
environment service with its HTTP routes. The end-to-end suite covers `audit`
and a full RL environment lifecycle.

Known gaps:

- The docker backend has not been exercised end to end on a host with a running
  daemon; its contract is covered by unit tests only. **The RL environment
  service is likewise unverified on docker** — `EnvInfo.WorkspaceDir` is a host
  path and is meaningful for the local backend only.
- `llm_judge` needs a reachable model endpoint and is not covered by the
  end-to-end suite, which must stay offline.
- Only Claude Code is implemented as an agent. The registry seam exists, but a
  second implementation would be the first real test of it.
- **Trajectories are turn-level only.** The Claude Code CLI emits no token ids
  and no logprobs, so token-level importance ratios are unavailable; PPO against
  this service gets a sequence- or turn-level signal.
- **An oracle proves a task is solvable, not that it is unique.** A task with
  several correct implementations only shows that one path exists; whether the
  graders accept the others needs per-case assertions.
- The reaper runs every `TTL/4`, so an environment's real maximum lifetime is
  between `TTL` and `1.25 × TTL`.

## License

Apache-2.0.
