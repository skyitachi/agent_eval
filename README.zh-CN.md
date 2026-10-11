# agenteval

[English](README.md) | **简体中文**

一个精简、立场明确的编码 agent 评测框架。它把一份数据集里的任务跑在 agent 上，用可插拔的评分器给结果打分，把每一个单元持久化进 SQLite，并渲染成自包含的 HTML 报告。

目前唯一实现的 agent 是 **Claude Code**，但 model 自由填写、原样透传，因此 CLI 能访问到的任何端点都能用。评分层、指标层和沙箱后端都是注册表（registry）——新增一个实现是加一个文件，而不是 fork 一份代码。

```
aeval run  ->  SQLite  ->  aeval report    写出一份自包含 .html
                       ->  aeval serve     可浏览的 Web UI
                       ->  aeval compare   两份运行并排对比
```

## 设计立场

有三个决定塑造了其余全部设计，值得明写出来，因为它们解释了一些乍看反直觉的行为。

**报告是数据库的函数，不是进程的函数。** 两周之后，在另一台机器上，从一份拷贝来的 `.db` 执行 `aeval report`，必须复现出当初运行期间写下的那份文档。因此渲染器只读取持久化状态。嵌在 `.html` 文件里的 JSON 与 `/api/runs/{id}` 提供的 JSON 出自同一条代码路径、同一批数据行，并且有测试断言两者逐字节相同。

**测量值绝不用重试抹掉。** 超时、非零退出、error result —— 这些都是数据。`retries` 只管辖「agent 进程根本起不来」这一种情况，且默认为 0。

**评测不能悄悄继承操作者的机器。** 默认的 agent 预设是一个 clean room：没有用户 hooks、没有 auto-memory、没有游离的 MCP server、不发现任务之外的 `CLAUDE.md`。一个会捡起你 `~/.claude` 的评测，在别的任何地方都不可复现。

还有一个决定只在把评测端当 RL 环境用时才浮现，但同样早就定下了：**奖励是投影，不是评分本身。** 评分回答「这次尝试是否满足任务」，奖励回答「训练器拿到什么数」。两者分开，是因为一旦合并就再也没法区分「任务被判负」和「奖励被设计成稀疏」。而奖励要可信，任务本身就必须先可信 —— 所以 `aeval audit` 会去证明参考解能过、什么都不做会失败、重跑结论一致，并且坚持把「测不出来」和「测出来是坏的」分开报告。

## 快速开始

需要 Go 1.25+。演示数据集需要一个装有 `pytest` 的 Python。

```sh
go build -o bin/aeval ./cmd/aeval

# 看看注册了什么。
./bin/aeval registry

# 检查数据集，不运行任何东西。
./bin/aeval validate --dataset datasets/demo

# 开始评测。演示数据集自包含，mock agent 跑一个脚本；
# good.sh 会正确地打补丁，所以健康的流水线会报 "resolved 3/3 (100.0%)"。
#
# AGENTEVAL_PYTHON 必须指向一个能 import pytest 的解释器 —— pytest 评分器
# 用它，而流水线其它部分完全不需要 Python。下面这条命令会显式报错，
# 而不是静默地打出 0/3。
AGENTEVAL_PYTHON="$(python3 -c 'import pytest, sys; print(sys.executable)')" \
  ./bin/aeval run --dataset datasets/demo \
                  --agent mock --bin tests/testdata/agents/good.sh \
                  --report reports/demo.html

# 浏览所有已记录的运行。
./bin/aeval serve            # http://127.0.0.1:8787

# 体检数据集：参考解能通过吗？什么都不做会失败吗？重跑结论一致吗？
# 参考解写在任务的 oracle 字段里，永不交给 agent，也不参与评分。
./bin/aeval audit --dataset datasets/demo --repeats 2
```

若要评测真实对象：

```sh
export ANTHROPIC_API_KEY=...
./bin/aeval run --dataset datasets/demo --model sonnet --concurrency 4
```

若要把同一份数据集当成 RL 环境交给训练循环：

```sh
./bin/aeval serve --dataset datasets/demo --rl --rl-allow-exec
```

之后 `POST /api/rl/envs` 开环境、`PUT .../files` 提交答案、`POST .../verify` 拿 reward。
详见 [`docs/rl-environment.md`](docs/rl-environment.md)。

## 核心概念

| 术语 | 含义 |
| --- | --- |
| **Task（任务）** | 数据集中的一条：一段提示词、一份初始工作区、一份输出契约，以及若干评分器。 |
| **Cell（单元）** | 一个任务 × 一次采样。是工作单位，也是存储单位。 |
| **Sample（采样）** | 对同一任务的一次重复。`--samples k` 给出 pass@k。每次采样都拿到全新的沙箱。 |
| **Grader（评分器）** | 给一个单元打分。多个评分器加权合成为一个总分。 |
| **Metric（指标）** | 把所有单元聚合成一个运行级别的数字。指标从不影响单元的判定。 |
| **Reward（奖励）** | 训练器为一次尝试拿到的标量，由任务的**奖励契约**从评分结果投影而来。默认等于合成评分，也可以声明成稀疏的 0/1。 |
| **Oracle（参考解）** | 任务的一个正确解，只用于 `aeval audit` 的体检。它永不交给 agent，也不参与评分。 |
| **Run（运行）** | 一次 `aeval run` 调用，由一个 UUID 标识，连同数据集哈希一起存储。 |

当单元的合成评分达到 `grader_threshold`（默认 1.0，即每个评分器都必须通过）时，该单元**通过**。`resolve_rate` 是通过单元所占的比例 —— 与 SWE-bench 的 resolved rate 同义。

**评分和奖励回答的是两个不同的问题**。评分说「这次尝试是否满足任务」，它有权重、有部分分、有阈值，是一个判断。奖励说「训练器拿到什么数」，它会被直接塞进 advantage 计算。二者分开，是因为一旦合并就没法再区分「任务被判负」和「奖励被设计成稀疏」—— 而这两件事在训练里的后果完全不同。评测链路与 RL 环境服务共用同一份投影实现（`internal/reward`），两份实现意味着两种奖励口径。

## 数据集格式

一个数据集是一个目录，含一个 `dataset.yaml`，以及可选的 JSONL 任务、提示词文件、工作区资产和评测期测试套件。

```
datasets/demo/
├── dataset.yaml                  # 清单：defaults、filter、tasks 文件
├── tasks.jsonl                   # 每行一个任务
├── prompts/fix-average.md        # 由 prompt_file 引用
├── workspaces/fix-average/       # 作为初始状态复制进沙箱
└── grades/implement-slugify/     # 只在评测期注入（隐藏测试套件）
```

`dataset.yaml` 携带数据集级别的 defaults，每个任务继承之；任务自己的值优先。

```yaml
name: demo-basic
version: 1.0.0
tasks_file: tasks.jsonl

defaults:
  limits: {max_turns: 30, timeout_s: 600}
  output: {type: patch, ignore_paths: ["*.pyc", "__pycache__/**"]}

# 不编辑任务本身，也能收窄数据集。
filter:
  categories: [bugfix]
  tags: [python]
  limit: 50
```

`tasks.jsonl` 里的一个任务：

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
  "reward": {"type": "score", "notes": "稠密：按加权分付奖励"},
  "oracle": {"patch_file": "oracles/implement-slugify.patch"},
  "restore_paths": ["test_slugify.py"],
  "forbid_changed_paths": ["pyproject.toml"],
  "limits": {"max_turns": 30, "timeout_s": 600}
}
```

**Workspace（工作区）** —— agent 的起点。`type: files` 把 `assets` 复制进沙箱（可选地把 `files` 逐字写入）。`type: git` 克隆 `ref` 处的 `repo`，可选地限制在 `subdir` 内。`setup` 命令在工作区就绪之后、agent 启动之前执行。

**Output contract（输出契约）** —— agent 被期望产出什么，因而也就是评分器能看到什么：`patch`（工作区 diff）、`files`（一组路径）、`stdout`（文本答案）或 `none`。`paths` / `ignore_paths` 收窄 diff —— 这一点很重要，因为构建产物否则会污染每一个补丁。

**Reward contract（奖励契约）** —— `reward.type` 取 `score`（默认，稠密，等于合成评分并裁剪到 `[0,1]`）或 `pass`（稀疏 0/1）。省略 `reward` 字段等价于 `score`，所以加入这个特性不会静默改变既有数据集。被反作弊护栏拒绝的尝试**恒为 0**，稀疏档也不例外 —— 否则「改写测试断言」就成了环境里最划算的捷径。

**Oracle（参考解）** —— `patch`、`patch_file` 或 `files`，可选 `apply`（一串命令）。它只被 `aeval audit` 使用：体检「这个任务真的能被解出来吗」。引擎在评测期从不读它，也从不交给 agent。

**Guards（反作弊）** —— `forbid_changed_paths` 事后否决（回答「是否守规矩」），`restore_paths` 在**评分前**把路径 checkout 回 baseline（回答「能不能干活」，即 SWE-bench 的默认做法）。两者用途不同，都要有。`restore_paths` 里的路径也会自动进入 diff 的忽略列表，否则还原动作本身会被当成 agent 的改动。

**Graders（评分器）** —— `weight` 是一个指针，因此「未设置」（=1）与显式 `0`（「运行并记录，但不参与计分」）是不同的。`required: true` 时，无论合成结果如何，该评分器失败即整个任务失败。

**Limits（限制）** —— `max_turns`、`timeout_s`、`max_cost_usd`、`max_output_tokens`。`max_turns` 由本框架通过监听事件流强制执行，因为 CLI 没有对应的 flag。

## 评分器

| 类型 | 检查什么 | 常用配置 |
| --- | --- | --- |
| `patch` | 补丁的形状：哪些路径变了、变了多少、是否有任何改动。 | `require_changed_paths`、`forbid_changed_paths`、`min_changed_files`、`max_diff_lines`、`expect_changed` |
| `pytest` | 跑一个测试套件，按通过/失败的计数打分。 | `suite`（评测期注入的隐藏套件）、`paths`、`min_passed`、`max_failed`、`python_candidates`、`extra_args` |
| `exact_match` | 归一化之后，把 stdout 答案与期望值比较。 | `expected`、`expected_any`、`regex`、`normalize`（`trim`、`strip_code_fence` 等）、`case_sensitive` |
| `llm_judge` | 用模型按评分标准为自由形式的输出打分。 | `rubric`、`criteria`、`model`、`base_url`、`input`、`reference` |
| `command` | 在沙箱里跑一条任意命令，把退出码映射为判定。 | `command`、`pass_exit_codes`、`parse`、`copy`、`env` |

`llm_judge` 是其它四个表达不了时的逃生舱。它需要一个可达的端点；请配置 `base_url` 与 `api_key_env`，不要把密钥硬编码。

在 Go 里注册一个评分器就是一次 `init()` 调用：

```go
func init() {
    Register("my_grader", func() Grader { return MyGrader{} })
}
```

## 指标

指标在评分之后计算，从不改变单元的判定。共注册了 34 个运行级指标；默认集是精选出的一个子集。运行 `aeval registry` 获取权威列表及其描述。

头号指标是 `resolve_rate`。其余指标落在报告分开渲染的若干组里：**outcome**（`pass_at_k`、`pass_hat_k`、`mean_score`）、**cost**、**tokens**、**effort**（`mean_turns`、`mean_tool_calls`）、**latency**（`p50`/`p95`）以及 **health**（`timeout_rate`、`harness_error_rate`、`tool_call_error_rate`）。health 组之所以单独存在，是因为「框架本身什么都没测到」的运行，与「agent 做得差」的运行不是一回事，二者绝不能被平均到一起。

面向训练的三个指标：

| 指标 | 含义 |
| --- | --- |
| `mean_reward` | 每次尝试的平均奖励。**含未启动和被拒绝的单元**（以它们的 0 计入）—— 排除它们等于只报告表现好的子集，那正是奖励作弊策略最希望被拿来评判的数字。 |
| `reward_signal_rate` | 非零奖励的占比。接近 0 意味着这一批数据不含学习信号，无论它的评分看起来多好。 |
| `guard_violation_rate` | 触发反作弊护栏的比例，附 `by_kind` 拆分。非零说明有一部分奖励在测量服从性而非能力。 |

### 自定义指标

三种形式，声明在 `run.custom_metrics` 下，按声明顺序求值（因此靠后的表达式可以引用前面已声明的自定义指标）。

```yaml
custom_metrics:
  # 1 —— 在一个过滤条件内，对某个数值字段做聚合。
  - name: bugfix_resolve_rate
    unit: ratio
    group: outcome
    direction: up
    where: "category == 'bugfix'"
    field: passed
    agg: mean

  # 2 —— 两个聚合量之比。
  - name: tokens_per_resolved
    unit: tokens
    group: token
    direction: down
    numerator:   {field: total_tokens, agg: sum}
    denominator: {field: passed,       agg: sum}
    zero_denominator: 0

  # 3 —— 对内置指标与已声明的指标做算术。
  - name: cost_per_resolved_guarded
    unit: usd
    group: cost
    direction: down
    expr: "total_cost_usd / max(resolved_count, 1)"
```

可过滤的字符串字段有 `task_id`、`category`、`difficulty`、`stop_reason` 以及 `tags contains`。数值字段包括 `passed`、`score`、`turns`、`tool_calls`、`total_tokens`、`latency_ms`、`cost_usd` 以及各个错误标记。

`direction` 与 `group` 是呈现提示，而且它们不是装饰。一个内置名字是由写下它的人把关过的；`slow_cells` 则没有这层保证，而一份把增长标成绿色的报告，比压根不上色的报告更糟。`group` 必须是 `outcome`/`cost`/`token`/`effort`/`latency`/`health` 之一，才能并入已有区块，否则会落到 "other"。

## 配置

上述一切都可以写进 `aeval.yaml`，`aeval` 会自动识别它（也包括 `.aeval.yaml`、`agenteval.yaml`）。完整注释版见 [`examples/aeval.yaml`](examples/aeval.yaml)。

```sh
aeval run --config examples/aeval.yaml
aeval validate --config examples/aeval.yaml   # 不运行，只检查
```

两条值得知道的规则：

- **未知键是错误。** 否则配置文件里的一个拼写错误，与一个「没起任何作用」的选项，就区分不开了。
- **相对路径相对配置文件所在目录解析**，而不是 shell 的工作目录。配置文件是可移植的产物；你碰巧从哪个目录运行它，不是。

命令行 flag 覆盖文件。只有真正给过的 flag 才会覆盖 —— 一个停留在零值的 flag 与一个没传的 flag 无法区分，所以它不会覆盖文件。

## CLI

```
aeval run        评测一份数据集并记录结果
aeval validate   只检查数据集与配置文件，不运行任何东西
aeval audit      体检数据集：可解性 / 必要性 / 稳定性
aeval list       列出已记录的运行
aeval report     为某次已记录的运行输出 HTML 报告
aeval compare    输出两次已记录运行的 HTML 对比
aeval serve      通过 HTTP 提供已记录运行的浏览服务；`--rl` 时兼作 RL 环境服务
aeval registry   列出已注册的 agent、评分器和指标
aeval version    打印构建与环境信息
```

退出码：`0` 成功，`1` 操作失败，`2` 命令行错误，`130` 被中断。**一次有部分任务失败的运行仍然退出 0** —— 那是一次测量，不是一次崩溃。没有这条规则，这个命令在 CI 里就没法用，而在 CI 里，一个失败的 benchmark 恰恰是你要观察的对象。

常用 flag：

```sh
# 每个任务 k 次采样，4 个单元并发，结束时出报告
aeval run --dataset ds --samples 3 --concurrency 4 --report out.html

# 续跑一次被中断的运行，复用同一个 run id
aeval run --dataset ds --resume

# 在第一个硬失败处停下
aeval run --dataset ds --fail-fast

# 把工作区留在磁盘上以便检查
aeval run --dataset ds --keep-workspaces --work-root ./work

# 只跑参考解（不启动 agent），用于验证任务的判分语义
aeval run --dataset ds --mode oracle

# 体检：参考解能过吗、什么都不做会失败吗、重跑一致吗
aeval audit --dataset ds --repeats 3 --strict

# 把数据集作为 RL 环境服务出去
aeval serve --dataset ds --rl --rl-allow-exec --rl-max-envs 4
```

**`--mode`** 有三条链路：`agent`（默认，跑 agent）、`oracle`（只应用参考解）、`baseline`（什么都不做）。它们产出的结果**形状完全相同**，所以 `runs.mode` 这一列不能省 —— 否则「参考解通过了」会被读成「模型通过了」。报告页上会用不同颜色的 mode 徽章标出非 agent 的 run。

## 体检：`aeval audit`

奖励可信的前提是任务本身可信。`aeval audit` 对每个任务跑三项检查：

| 检查 | 问题 | 判定 |
| --- | --- | --- |
| **可解性** | 参考解能通过吗 | `oracle` 跑一次，必须判通过 |
| **必要性** | 什么都不做会失败吗 | baseline 跑一次，必须**不**通过 |
| **稳定性** | 重跑结论一致吗 | `oracle` 连跑 `--repeats` 次，逐评分器比对 |

必要性检查抓的是「白拿奖励」。一个 baseline 就已经通过的任务，会为「什么都不做」支付奖励 —— 在 benchmark 里这只是个平庸的任务，在 RL 里它是一条会摧毁策略的捷径。

检查结果有三种状态，第三种是必须的：`ok`（测过了，没问题）、`failed`（测过了，有问题）、`unknown`（**测不出来**）。没有 `oracle` 的任务（比如自由文本问答）落在 `unknown` —— 当成 `failed` 是污蔑任务，当成 `ok` 是撒谎。同理，参考解模式失败时记录的 stop reason 是 `oracle_error` 而不是 `harness_error`：**数据集自己的参考解不可用，和环境故障是两回事**。

`--strict` 在有缺陷时退出 1，默认退出 0 —— 理由和评测本身一致：审计发现问题不等于审计命令失败。

## 报告与 Web UI

`aeval report` 写出一个自包含的 `.html` 文件 —— CSS 与 JS 全部内联。它能从 `file://` 打开，能作为邮件附件存活，且不需要服务器。`aeval serve` 渲染同样的文档，但资源改为外链，另外提供一个实时索引。

报告包含指标摘要、按组拆解（按 category 与 difficulty 的 resolve rate、stop-reason 分布、工具使用、分数分布）、一个可过滤可排序的任务表，以及一个逐任务抽屉：评分器的判定与理由、agent 的回答、diff，以及事件时间线。

**这个文件有体积预算。** 一次 500 单元、带完整轨迹的运行会产生一个大到打不开的文档。当载荷接近 `max_inline_bytes`（8 MiB）时，逐单元的*细节*会按优先级被丢弃 —— 失败与出错的单元保留证据最久，通过的单元先丢 —— 每一次降级都记录在报告自己的 warnings 里。被截断的轨迹保留头与尾（任务框定与答案）并带一个显式的 gap marker，因为被截断的尾巴会误导人。

Web 服务器默认只绑 loopback，除非传入 `--allow-delete` 否则删除路由是关闭的，携带外部 `Origin` 的请求会被拒绝。它渲染的是模型生成的文本，所以这不是多疑。

## RL 环境服务

加上 `--rl`，`aeval serve` 会同时把数据集暴露成训练循环可以驱动的环境：

```sh
aeval serve --dataset datasets/demo --rl --rl-allow-exec
```

```
GET  /api/rl/tasks                    可以拿来训什么
POST /api/rl/envs                     开一个环境                -> 201
POST /api/rl/envs/{id}/exec           在里面跑一条命令
PUT  /api/rl/envs/{id}/files          提交一个文件
POST /api/rl/envs/{id}/verify         判分、投影奖励、销毁工作区 -> 终态
POST /api/rl/envs/{id}/delete         不判分直接销毁
GET  /api/rl/envs/{id}/archive        一份判定的全部证据
POST /api/rl/rollouts                 让配置的 agent 跑一次，直接返回 reward
```

`verify` 的响应就是训练器需要的那份：

```json
{"task_id":"subtract","reward":1,"reward_kind":"score","score":1,"passed":true,
 "rejected":false,"stop_reason":"verified",
 "run_id":"rl-8b1e04a7-...","task_run_id":"rl-8b1e04a7-.../subtract#0"}
```

四个性质值得单独说：

**每一份 reward 都指向已落库的证据。** `run_id` 是一条 `mode = 'rl'` 的真实运行，所以 `aeval report <run_id>` 能把证据渲染出来，而不是让训练器凭信任接受这个数。

**`error` 非空表示 harness 失败，不是策略失败。** 这种 rollout 不含训练信号，必须丢弃，绝不能当成 0 奖励学进去。

**`rejected` 为真表示反作弊护栏命中了。** 奖励此时已经是 0；这个标志让训练器能区分「答错了」和「改了验证器」。

**验证是终态。** 重复 `verify` 返回 `409` 并带回原结果，之后的 `exec` 同样返回 `409`。一个能「看完分再补一手」的策略，会把 verifier 变成一个可以反复试探的预言机。

这是整个工具里**唯一会「做」而不只是「渲染」**的部分 —— 它会起沙箱、跑 agent，在 `--rl-allow-exec` 下还会以 harness 用户的身份跑任意命令。所以闸门是四层：必须传 `--rl`、请求必须同源、exec 有自己的开关、每个维度都有上限（`--rl-max-envs`、`--rl-ttl`、`--rl-max-execs`、`--rl-max-rollouts`、`--rl-max-cost-usd`）。触到上限返回 `429`，绝不伪造一个任务失败 —— 「你请求得太快」和「策略做得不好」在训练信号里不能混为一谈。

这些开关和上限都可以写进 `aeval.yaml` 的 `serve.rl` 段（`enabled`、`allow_exec`、`max_envs`、`ttl`、`max_execs`、`max_rollouts`、`max_cost_usd`），`examples/aeval.yaml` 里有逐项注释。命令行 flag 是**逐字段**覆盖配置的，不是整段替换：一个只写了 `allow_exec: true` 的配置文件加上一条 `--rl-max-envs 4`，两者都生效。

训练循环怎么接、以及诚实的边界（只有 turn 级轨迹，因为 Claude Code CLI 不吐 token id 也不吐 logprobs），见 [`docs/rl-environment.md`](docs/rl-environment.md)。

## 沙箱后端

| 后端 | 边界 | 适用 |
| --- | --- | --- |
| `local` | 一个目录和一个受控环境。没有文件系统或网络隔离。 | 快速迭代，在可信输入上跑 CI。 |
| `docker` | 真正的容器：挂载、网络和进程隔离。 | 不可信任务，可复现的基线。 |

```sh
aeval run --dataset ds --backend docker --image ghcr.io/example/agent:latest
```

凭证通过 `agent.env_from_host: [ANTHROPIC_API_KEY]` 进入容器 —— 从框架环境复制进沙箱，绝不写入文件。

## 仓库结构

```
cmd/aeval/         CLI：flag 解析、配置解析、退出码
internal/dataset/  清单 + 任务规格、JSONL 加载、哈希
internal/sandbox/  后端接口，local 与 docker 实现
internal/agent/    agent runner（claude_code、mock）与流解析
internal/trace/    Claude Code 事件流解析器
internal/grader/   评分器注册表与五个内置评分器
internal/reward/   评分 -> 奖励的投影（稠密或稀疏，被拒绝恒为 0）
internal/metric/   指标注册表、内置指标、自定义指标 DSL
internal/engine/   编排：调度、超时、重试、记录；以及 Verify
                   （guards -> diff -> 评分 -> 奖励），与 rlenv 共用
internal/audit/    `aeval audit` 背后的三项任务体检
internal/rlenv/    RL 环境服务：任务、环境、奖励
internal/storage/  SQLite schema 与查询
internal/report/   storage 行 -> 视图模型 -> 自包含 HTML
internal/web/      在同一个视图模型之上的 HTTP 服务器，外加 /api/rl 路由
internal/config/   aeval.yaml 加载与校验
internal/llm/      Anthropic Messages API 的最小客户端（供 llm_judge 使用）
tests/             驱动真实二进制文件的端到端测试
docs/              设计说明（RL 环境服务、真实仓库评测）
```

## 测试

```sh
go test ./...                              # 全部
go test ./internal/report/ -run XSS        # 单个关注点

# 端到端套件需要一个装有 pytest 的解释器。
AGENTEVAL_PYTHON="$(python3 -c 'import pytest, sys; print(sys.executable)')" \
  go test ./tests/ -v
```

`tests/e2e_test.go` 端到端驱动编译出的二进制：它用一个会正确修补代码的脚本化 agent 构建一次运行，再用一个会错误修补的构建第二次，并断言两者的判定不同、从拷贝的数据库重新生成的报告与运行期间写下的那份完全相同、每一条 HTTP 路由都按文档应答。`AGENTEVAL_PYTHON` 把 `pytest` 评分器指向一个装有 `pytest` 的解释器。

## 状态

已工作且经过测试：数据集加载、两个沙箱后端（local 已端到端跑过；docker 已实现但只有单测）、Claude Code runner 与流解析器、全部五个评分器、指标注册表与自定义指标 DSL、SQLite 持久化、编排引擎、HTML 报告、Web UI、配置层，以及 CLI。

面向 RL 的部分：奖励契约（稠密/稀疏，被拒绝恒为 0）、三条运行链路（agent / oracle / baseline）、两条防作弊路径（`forbid_changed_paths` 事后否决、`restore_paths` 事前还原）、`aeval audit` 三检、`internal/rlenv` 环境服务与其 HTTP 路由。端到端套件覆盖 audit 与一次完整的 RL 环境生命周期。

已知缺口：

- docker 后端尚未在一台有运行中 daemon 的主机上端到端跑过；它的契约只由单测覆盖。**RL 环境服务在 docker 后端下同样未端到端验证过** —— `EnvInfo.WorkspaceDir` 是宿主路径，只在 local 后端有意义。
- `llm_judge` 需要一个可达的模型端点，且不在端到端套件覆盖范围内 —— 该套件必须保持离线。
- 只有 Claude Code 一种 agent 实现。注册表的这个接缝已经存在，但第二份实现才会是它的第一次真正检验。
- **轨迹只有 turn 级。** Claude Code CLI 不吐 token id，也不吐 logprobs，所以拿不到 token 级 importance ratio；做 PPO 只能拿到序列级或 turn 级信号。
- **参考解只能证明「可解」，不能证明「唯一」。** 一个有多种正确实现的任务，oracle 通过只说明至少有一条路；其余的路是否也被评分器接受，需要用例级的断言。
- reaper 的检查间隔是 `TTL/4`，所以环境的实际最长存活时间在 `TTL` 到 `1.25 × TTL` 之间。

## 许可证

Apache-2.0。
