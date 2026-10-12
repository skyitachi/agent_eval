# agenteval

[English](README.md) | **简体中文**

一个小而立场明确的编码 agent 评测工具。它把一份任务数据集跑在 agent 上，用可插拔的评分器
打分，把每个格子持久化到 SQLite，再渲染成自包含的 HTML 报告。

它回答三个问题：

- **一个 agent 在某份数据集上做得怎么样？** —— `aeval run`
- **一次差异该归因给 harness、大模型还是 skill？** —— `aeval matrix`
- **一次真实会话花了多少，好让重跑有个对照？** —— `aeval collect`

它**不**回答「这个 agent 好不好」。那个问题需要一份有标准答案的数据集，以及一个知道这些数字
是拿来干什么的读者 —— 没有任何命令能提供这两者。

```
aeval collect  ──> 数据集 + baseline
                          │
aeval run      ──┐        ▼
aeval matrix   ──┴──> SQLite ──> aeval report    （一份自包含 .html）
                                 aeval list      （已经记录了什么）
```

目前唯一的 agent 实现是 **Claude Code**，但 model 是自由字段、原样透传，所以只要 CLI 能连上
的端点都能用。agent 驱动、评分器、沙箱后端都是注册表：新增一个是加一个文件，不是 fork。

## 设计立场

有六个决定塑造了其余一切。之所以要写出来，是因为它们解释了一些否则会显得奇怪的行为。

**报告是数据库的函数，不是进程的函数。** 两周之后，在另一台机器上，从一份拷贝来的 `.db`
执行 `aeval report`，必须复现出当初运行期间写下的那份文档。因此渲染器只读取持久化状态，
并且有测试断言：从数据库重新生成的报告，与运行期写下的那份**逐字节相同**。

**一次只动一条轴。** 一次 sweep 要么动 harness、要么动 model、要么动 skill，绝不同时动两个。
这条在**加载期**强制：某个 level 若改了它这条轴不拥有的键，是**拒绝**而不是警告。代价是真实的，
且写进每一份报告：单轴 sweep 看不见交互效应，只在组合中才出现的差异不会在这里显示。

**测到的值不靠重试抹掉。** 超时、非零退出、error 结果 —— 这些都是数据，测量本身不重试。
「测到了」和「打了分」也是两件事：一个格子可以是**已测量但未评分**的（从真实会话采集来的任务
没有标准答案），把它算成失败等于凭空造出一个数据里不存在的判决。

**评测不能悄悄继承操作者的机器。** 默认的 agent 预设是一个 clean room：没有用户 hooks、
没有 auto-memory、没有任务之外的 MCP、不发现 `CLAUDE.md`。一个会读到你的 `~/.claude`
的评测，在别的机器上不可复现。

**baseline 是输入，不是结果。** baseline 存放在它所属的数据集旁边；一次 run 只记录它做过的
那次比较。这正是「报告是数据库的函数」得以成立的原因。

**有任务失败的 run 仍然是一次成功的命令。** resolve rate 小于 1 是评测的正常结果，不是故障。
只有**根本没能跑起来**的 run 才返回非零退出码。

## 快速开始

需要 Go 1.25+。演示数据集里的两个 pytest 任务需要一个能 import pytest 的解释器；
管道里其余部分完全不需要 Python。

```sh
go build -o bin/aeval ./cmd/aeval

# 只检查数据集，不跑任何东西。
./bin/aeval validate --dataset datasets/demo

# 评测。good.sh 会正确修补工作区，所以健康时会打印 "passed 3 (100.0%)"。
#
# AGENTEVAL_PYTHON 必须指向一个能 import pytest 的解释器。下面这条命令会
# 显式报错，而不是静默地给出一个看起来像 agent 全挂的 0/3。
AGENTEVAL_PYTHON="$(python3 -c 'import pytest, sys; print(sys.executable)')" \
  ./bin/aeval run --dataset datasets/demo \
                  --agent mock --bin tests/testdata/agents/good.sh \
                  --report reports/demo.html

# 可浏览的索引，加每次 run 一页，全部能从 file:// 打开
./bin/aeval report --index --out-dir reports/site
```

把 demo 换成真东西：

```sh
export ANTHROPIC_API_KEY=...
./bin/aeval run --dataset datasets/demo --model sonnet --concurrency 4
```

## 子命令

| 命令 | 做什么 |
| --- | --- |
| `aeval collect` | 把一段录制下来的 Claude Code 会话变成带 baseline 的数据集。 |
| `aeval validate` | 检查数据集，不运行任何东西。 |
| `aeval run` | 评测一份数据集并记录结果。 |
| `aeval matrix` | 对一条因子轴跑一次 sweep，或对已跑过的 sweep 做归因。 |
| `aeval report` | 把一次 run、一次对比、一次 sweep，或整个索引渲染成 HTML。 |
| `aeval list` | 列出已记录的 run，可按 sweep 或轴过滤。 |
| `aeval version` | 打印构建、schema、payload 版本与已注册的 agent。 |

退出码：`0` 成功，`1` 操作失败，`2` 命令行本身有错，`130` 被中断。

## 概念

| 术语 | 含义 |
| --- | --- |
| **Task（任务）** | 数据集里的一条：一段 prompt、一个初始工作区、一个产出契约、若干评分器。 |
| **Cell（格子）** | 一个任务 × 一个 sample。工作单元，也是存储单元。 |
| **Sample（样本）** | 一次重复。`--samples k` 给 pass@k。每个 sample 拿到全新沙箱。 |
| **Grader（评分器）** | 给一个格子打分。若干评分器合成一个加权分数。 |
| **因子（Factor）** | sweep 允许变动的三样之一：**harness**（实现 + 配置）、**model**、**skill**。 |
| **level** | 被 sweep 的因子的一个取值：control，或某个备选。一个 level 就是一次 run。 |
| **control（对照）** | 其余每个 level 都被它减去的那个。它不是特殊记录，只是被当作参照的那一个。 |
| **baseline** | 一个任务用来对照的录制尝试，通常由 `aeval collect` 从真实会话导出。 |
| **Run（运行）** | 一次 `aeval run` 调用，由 UUID 标识，连同数据集哈希一起存储。 |

当一个格子的评分器加权分达到阈值（`--threshold`，默认 1.0，即每个加权评分器都必须通过）时，
它**通过**。

## 数据集格式

一份数据集是一个目录，含一个 `dataset.yaml`，以及可选的 JSONL 任务、prompt 文件、
工作区素材、判分时才注入的测试套件。

```
datasets/demo/
├── dataset.yaml                  # manifest：defaults、filter、tasks 文件
├── tasks.jsonl                   # 一行一个任务
├── prompts/fix-average.md        # 由 prompt_file 引用
├── workspaces/fix-average/       # 作为初始状态拷进沙箱
├── grades/implement-slugify/     # 只在判分时注入（隐藏套件）
└── baselines/<id>.json           # 录制下来的参照尝试
```

数据集里的每一条路径 —— `prompt_file`、`assets`、`suite`、`baseline.file`、
`workspace.repo` —— 都相对**数据集文件所在目录**解析，而不是相对 shell 的当前目录。
因此同一份文档在哪儿调用都表现一致。

### 任务字段

```yaml
task:   id, version, category, difficulty, tags
prompt: prompt | prompt_file, system_prompt
init:   workspace: {type: empty|files|git, ...}
output: type: patch|files|stdout|none, paths, ignore_paths, expect_answer
score:  graders: [...], limits: {max_turns, timeout_s}
base:   baseline: {file, notes}
meta:   metadata: {...}          # 自由来源信息，引擎从不解释
```

`workspace.type: git` 接受 `repo`、`ref`（写 commit SHA 可复现，写分支名不可复现）、
`subdir`；上游 `.git` 会被剥离并重建 baseline，所以 agent 没法从历史里读走修复。
`setup` 在 agent 之前运行，之后会重新 baseline —— 装依赖不算 agent 的改动。

`graders[].weight` 是个指针，这样 `weight: 0`（「跑它，但不计分」）与「未设置」（= 1）
能区分开。`required: true` 会让整个任务直接判负，与其它评分器无关。

### manifest

```yaml
name: demo-basic
description: ...
tasks_file: tasks.jsonl     # 或者内联的 `tasks:` 列表
defaults: {...}             # 合并进每个任务
filter: {...}               # 收窄要加载哪些任务
```

## 评分器

| 类型 | 判什么 |
| --- | --- |
| `pytest` | Python 测试套件。`paths`（跑哪些文件）、`suite`（只在判分时注入的隐藏套件）、`min_passed`/`max_failed` 阈值。 |
| `patch` | 补丁本身：`require_changed_paths`、`forbid_changed_paths`，以及规模上下界。 |
| `exact_match` | stdout 里的答案，先经过一串声明的归一化。 |
| `command` | 任意命令的退出状态与输出。 |
| `llm_judge` | 模型按 rubric 给出的判断。需要可达端点，所以不在离线测试套件的覆盖内。 |

## 指标

一次 run 的头部展示一小组数字：cells、pass rate、tokens、cost、wall clock、tokens/cell、
timeouts、harness errors；当有任何格子带 baseline 时，还有相对它的 token 与时间增量。
timeouts 与 harness errors **恒显**，即使为零 —— 一个只列出「它有的问题」的表头，
会让不存在的问题变得不可见。`timeouts` 数的是跑超预算的格子（关于 agent 的事实），
`harness_errors` 数的是 harness 完全没能测到的格子（关于这次 run 的事实）。

归因则对七个指标做**配对**比较，正反两个方向都看 —— 一个没人能用上的 token 节省不算发现：

`pass rate`（标为 **primary** —— 变更就该拿正确性来判）、`mean score`、`tokens`、
`cost`、`time`、`turns`、`tool calls`。

`LowerIsBetter` 决定哪一边算赢。方向由 **win/loss 分裂**决定，而不是由均值决定 ——
一个离群点能拉动均值，但拉不动分裂。

## 归因：`aeval matrix`

一份 sweep 文档指定一条轴、一份共享配置，以及要试的若干 level：

```yaml
name: demo-harness-sweep
dataset: ../datasets/demo
axis: harness
control: stock

agent:                     # 每个 level 的起点配置
  kind: mock
  bin: ../tests/testdata/agents/good.sh

levels:
  - name: stock
  - name: terse-prompt
    agent:
      append_system_prompt: 只回答裸数字，别的都不要。
  - name: scripted-failure
    agent:
      bin: ../tests/testdata/agents/bad.sh
```

level 的 `agent` 块是**局部**的：只替换它点到的那些键。「这个 level 改了哪些键」会跟着这个
level 一起被记录下来 —— `terse-prompt` 改了一个键，control 一个都没改 —— 因为那正是读者
用来核对「只动了一条轴」这个主张的证据。`aeval matrix --dry-run` 只打印计划、不写任何东西；
`aeval matrix --sweep <id>` 对已经跑过的 sweep 做归因，不重跑任何东西。

运行：

```sh
aeval matrix --file examples/matrix-harness.yaml --db aeval.db
aeval matrix --sweep <id> --json          # 归因，JSON
aeval report --sweep <id> -o sweep.html   # 归因，页面
```

### 它不会声称什么

level 之间的分离度用**精确符号检验**（exact sign test）判定，只看非平局配对：
一个这么偏的分裂，可能来自一枚公平硬币吗？三条推论被写进每份报告，而不是留给读者猜：

- 它只是个**显著性**检验。它不说差异够不够大、值不值得在意。
- 非平局配对少于六对时，无论分裂多偏都到不了 p < 0.05。演示数据集只有三个任务，
  所以演示 sweep 会报告它发现的差异，并拒绝把它称作真的。这是**预期行为，不是缺陷**。
- 每条 finding 都把正确性结果带在旁边，所以一个 token 节省永远不能被单独读。

## 从会话采集

`aeval collect` 读取一段录制下来的 Claude Code 转录，写出一份**普通数据集** ——
这意味着重跑可以拿真实会话的开销作对照。

```sh
aeval collect --session ~/.claude/projects/<project>/<id>.jsonl --out datasets/mine
aeval collect --latest --out datasets/mine
aeval collect --cwd ~/code/proj --out datasets/mine
```

它产出的任务带一段 prompt、一个工作目录、一个 baseline（tokens、忙碌时间、轮次、工具调用），
以及**没有评分器**：转录里没有标准答案，所以这个任务是可测量但不可评分的。把它判成失败是
造一个判决，判成通过是造另一个。若你确实有评分依据，用 `--grader` 显式挂上去。

## 报告

`aeval report` 写出一份自包含的 `.html` —— CSS 与 JS 全部内联 —— 能从 `file://` 打开、
能作为邮件附件存活、不需要服务器。它能渲染四种东西：

```sh
aeval report --run <id>               # 一次 run
aeval report --compare <a> <b>        # 两次 run 并排
aeval report --sweep <id>             # 一次归因
aeval report --index --out-dir site/  # 索引，加每次 run 一页
```

报告有体积预算（默认内嵌 JSON 8 MiB）。超了之后按优先级丢弃：通过的格子先丢轨迹与 diff，
失败的格子保留证据最久，每次降级都记进报告自己的 warnings。被截断的轨迹保留**头尾两端** ——
开头的事件承载任务框定，最后的事件承载答案。

## 包结构

```
cmd/aeval/         命令行
internal/agent/    Agent 接口、Claude Code 驱动、mock、注册表
internal/sandbox/  工作区落地与命令执行
internal/dataset/  数据集与任务加载、baseline、路径解析
internal/grader/   五个评分器与注册表
internal/trace/    Claude Code stream-json -> 归一化轨迹
internal/session/  Claude Code 转录 -> 带 baseline 的任务
internal/runner/   调度、超时、工作区生命周期、记录
internal/store/    SQLite schema、runs 与 attempts
internal/attrib/   因子轴、sweep 计划、配对比较、符号检验
internal/report/   视图模型、加载器、HTML 渲染器
internal/llm/      llm_judge 用的最小客户端
tests/             驱动真实二进制的端到端测试
```

## 测试

```sh
go test ./...

# pytest 评分器需要一个能 import pytest 的解释器。演示数据集有两个 pytest
# 任务；不给这个变量时测试套件会 skip，而不是报出一个看起来像 agent 挂了的 0/3。
AGENTEVAL_PYTHON="$(python3 -c 'import pytest, sys; print(sys.executable)')" go test ./...
```

`tests/e2e_test.go` 按用户的方式驱动真实二进制：磁盘上的数据集、真实子进程、真实 SQLite
文件、真实 HTML 报告。它不需要模型、不需要网络、不需要凭据。

## 已知缺口

写在这里，好过让别人日后发现：

- **只有一种 agent 实现。** Claude Code 是唯一的真驱动；注册表是那道缝，第二个驱动是一个新文件。
- **容器后端不可达。** `internal/sandbox/docker.go` 已经写好（364 行，带 `--pids-limit`、
  `--memory`、`--cpus`），但 `internal/runner` 把后端写死成 local，没有 flag 也没有数据集
  字段能选中它，也没有任何测试会启动容器。要么把它接上并补端到端测试，要么删掉 ——
  一份没人调用的参考实现比没有更糟，因为它看起来像是「隔离只差一个开关」。
- **`llm_judge` 不在端到端覆盖内。** 它需要可达端点，而 e2e 必须离线跑。
- **没有 `oracle` 字段，也没有任务自检。** 参考解只能塞进 `metadata`，而引擎从不解释它，
  于是 `resolve_rate` 无法被解释：分不清是任务难还是任务坏。
- **`Baseline.RestorePaths` 写了但没人用。** 它正是「判分前把测试文件还原，让改了测试的
  agent 按真实水平计分」那条语义的原语 —— 而它没有调用方、没有测试、数据集里也没有字段。

`docs/real-repo-eval.md` 是「在真实仓库而不是三文件 demo 上评测 agent」的设计，
它的 §1 表格保持与代码同步。`docs/design.md` 是本实现所回答的最初需求。

## 许可

Apache-2.0。
