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

有七个决定塑造了其余一切。之所以要写出来，是因为它们解释了一些否则会显得奇怪的行为。

**报告是数据库的函数，不是进程的函数。** 两周之后，在另一台机器上，从一份拷贝来的 `.db`  
执行 `aeval report`，必须复现出当初运行期间写下的那份文档。因此渲染器只读取持久化状态，  
并且有测试断言：从数据库重新生成的报告，与运行期写下的那份**逐字节相同**。

**登记的是请求，不是答案的第二份拷贝。** 登记「渲染过一份报告」这件事，存的是当初要的是**哪个  
视图**、以及渲染出来的东西的哈希 —— 从不存 HTML，也从不存载荷。载荷是数据库的函数，而一份存下来的  
载荷是那个函数的**旧值**：行一变动的瞬间它就不再更新，却仍然看起来像一个答案。哈希是服务端在**每次  
读取时**重新核对的主张，正因如此，被提供出去的页面才说得出「它是不是当初登记的那份文档」，说不是的  
时候也说得出它现在是什么。

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

# 或者把上面这些命令登记下来的报告跑成一个服务：http://127.0.0.1:8080
./bin/aeval serve
```

把 demo 换成真东西：

```sh
export ANTHROPIC_API_KEY=...
./bin/aeval run --dataset datasets/demo --model sonnet --concurrency 4
```

## 子命令

| 命令               | 做什么                                       |
| ---------------- | ----------------------------------------- |
| `aeval collect`  | 把一段录制下来的 Claude Code 会话变成带 baseline 的数据集。 |
| `aeval validate` | 检查数据集，不运行任何东西。                            |
| `aeval run`      | 评测一份数据集并记录结果。                             |
| `aeval matrix`   | 对一条因子轴跑一次 sweep，或对已跑过的 sweep 做归因。         |
| `aeval report`   | 把一次 run、一次对比、一次 sweep，或整个索引渲染成 HTML，并登记它渲染了什么。 |
| `aeval serve`    | 把登记下来的报告跑成一个 HTTP 服务。                       |
| `aeval list`     | 列出已记录的 run，可按 sweep 或轴过滤。                 |
| `aeval version`  | 打印构建、schema、payload 版本与已注册的 agent。        |

退出码：`0` 成功，`1` 操作失败，`2` 命令行本身有错，`130` 被中断。

## 概念

| 术语              | 含义                                                        |
| --------------- | --------------------------------------------------------- |
| **Task（任务）**    | 数据集里的一条：一段 prompt、一个初始工作区、一个产出契约、若干评分器。                   |
| **Cell（格子）**    | 一个任务 × 一个 sample。工作单元，也是存储单元。                             |
| **Sample（样本）**  | 一次重复。`--samples k` 给 pass@k。每个 sample 拿到全新沙箱。             |
| **Grader（评分器）** | 给一个格子打分。若干评分器合成一个加权分数。                                    |
| **因子（Factor）**  | sweep 允许变动的三样之一：**harness**（实现 + 配置）、**model**、**skill**。 |
| **level**       | 被 sweep 的因子的一个取值：control，或某个备选。一个 level 就是一次 run。         |
| **control（对照）** | 其余每个 level 都被它减去的那个。它不是特殊记录，只是被当作参照的那一个。                  |
| **baseline**    | 一个任务用来对照的录制尝试，通常由 `aeval collect` 从真实会话导出。                |
| **Run（运行）**     | 一次 `aeval run` 调用，由 UUID 标识，连同数据集哈希一起存储。                  |

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
origin: session, prompt_index   # 任务出自哪份录制，以及它是其中的第几条 prompt
meta:   metadata: {...}          # 自由来源信息，引擎从不解释
```

`session` 与 `prompt_index` 是**引擎真的会读**的两个字段，而不是仅仅被存下来：它们的  
存在正是因为 `metadata` 契约上是不透明的 —— 引擎要解释的值不能放在它从不解释的块里。  
`collect` 两个都会写；手写的任务可以都不写。

`workspace.type: git` 接受 `repo`、`ref`（写 commit SHA 可复现，写分支名不可复现）、  
`subdir`；上游 `.git` 会被剥离并重建 baseline，所以 agent 没法从历史里读走修复。  
`setup` 在 agent 之前运行，之后会重新 baseline —— 装依赖不算 agent 的改动。  
`run --in-place` 在 `repo` 本身里跑而不是在副本里，这也是它拒绝 `subdir`、`setup` 和显式  
`keep_git_history: false` 的原因 —— 见上文那一节。

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

| 类型            | 判什么                                                                            |
| ------------- | ------------------------------------------------------------------------------ |
| `pytest`      | Python 测试套件。`paths`（跑哪些文件）、`suite`（只在判分时注入的隐藏套件）、`min_passed`/`max_failed` 阈值。 |
| `patch`       | 补丁本身：`require_changed_paths`、`forbid_changed_paths`，以及规模上下界。                   |
| `exact_match` | stdout 里的答案，先经过一串声明的归一化。                                                       |
| `command`     | 任意命令的退出状态与输出。                                                                  |
| `llm_judge`   | 模型按 rubric 给出的判断。需要可达端点，所以不在离线测试套件的覆盖内。                                        |

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

回放会在自己的 workspace 里运行 agent，Claude Code 也会在那里写一份转录。那份转录是回放的  
产物，不是录制：`collect` 不会在 `--list` 里列出它，`--latest` 也不会选中它，用 `--session`  
显式指定会被拒绝。

它产出的任务带一段 prompt、一个工作目录、一个 baseline（tokens、忙碌时间、轮次、工具调用），  
以及**没有评分器**：转录里没有标准答案，所以这个任务是可测量但不可评分的。把它判成失败是  
造一个判决，判成通过是造另一个。若你确实有评分依据，用 `--grader` 显式挂上去。

每个 baseline 还记录采集它的机器（`machine`：平台机器标识的哈希 —— macOS 取 `IOPlatformUUID`、  
Linux 取 `/etc/machine-id`、Windows 取 `MachineGuid` —— 外加给人看的主机名、OS、架构）。  
`run` 与 `matrix` 会拿它和当前机器比对，**不一致就直接拒绝启动**（退出码 2，不写任何 run 行）：  
baseline 里的目录、仓库、工具都属于那台机器，换台机器回放就是拿一个没在其中运行的环境作对照。  
请回到采集的机器上回放，或在本机重新采集。没有 `machine` 的 baseline（手写的、或此前采集的）不做检查。

`--out` 里已有数据集时，`collect` 会追加写入：同一份录制（同一会话、同一条 prompt）已在其中的  
任务会跳过；id 被另一份录制占用的任务加 `-2` 后缀。已有的任务和 manifest 原样保留。  
`--dry-run` 可预览会追加什么，`--overwrite` 则整体替换。

### 一个任务覆盖会话的多少

`--split` 决定这件事，默认是 `session`：一个任务，装着**开头那条请求**，对照回答它的那些轮次  
来测量。

如果人在这个会话里一直在追问，那它就只是**第一条 prompt**。baseline 停在第二条 prompt 处 ——  
整条轨迹回答的是这个任务根本没问的问题，拿它作对照，重跑会被报成比一段并不对应的录制更便宜。  
第一条之后的 prompt 因此**完全没有被采集**：这是一件「一个单任务数据集里别的任何地方都不记  
录」的事实，所以 `collect` 会把它讲出来，既写进任务的 baseline 注记，也打到 stderr：

```
warning: 1 session(s) hold more prompts than this dataset collected: 1f2114cd (1 of 3).
         Re-run with --split turn to collect each prompt as its own task.
```

`--split turn` 按 prompt 一条一个任务，各自对照自己的切片测量；`--min-prompt-runes` 在这个  
split 下丢掉「ok」「再试一次」这类对话噪声 —— 它丢过什么，上面那条警告会告诉你。

### 一次会话的轮次要连起来回放

`--split turn` 把每条 prompt 规划成一个独立的 cell：自己的空沙箱，按 `--concurrency` 恰好  
排到的顺序跑。对「这个 agent 在这条 prompt 上表现如何」这是对的测量；对一段**人在一直往下  
追问**的录制则是错的 —— 第二条 prompt 问的是第一条写下的那个文件，于是在新目录里回放，等于  
在问一个从来没存在过的文件。`--chain-sessions` 让采集来的来源信息真正发挥作用：

```sh
aeval run -d datasets/mine --chain-sessions
```

- 共享同一个 `session` 的任务在**同一个工作区**里跑，按 `prompt_index` 排序 —— 是 prompt  
  被问出来的顺序，不是任务文件碰巧列出的顺序。
- 这条链只在**第一个 cell 之前**物化一次工作区，只在**最后一个 cell 之后**删掉它。后面的  
  cell 从前面留下的东西起步，因为中间没有任何人把目录放回去过。
- 不带 `session` 的任务自成一条链，这正是这个 flag 不会改变一份普通数据集的原因。
- k 个样本之间保持独立：一条链每个样本各回放一次，样本 1 绝不会从样本 0 的残留起步。
- 链式回放要求 `--concurrency 1`，并且以**用法错误**的方式说出来，而不是悄悄串行化：同一条  
  链的两个 cell 同时在跑会互相覆盖，而行上没有任何一列会记录这件事发生过。
- `--resume` **拒绝**只录了一半的链，并点出是哪条链、还剩多少。链式 cell 的前驱的工作区已随  
  它那个 cell 一起消失，所以剩下的会从空目录起步，写下的行描述的是那些缺失的文件，不是 agent。

也正因为如此，链式 run 的各个 cell 是**一个序列，不是重复**：`k=1` 是常见设置，一条链上的  
`pass_rate` 回答的是「整段对话最后走通了吗」。run 行会记下它是链式的，报告头部会写出来，把  
链式 run 与普通 run 作比较会**报出这个差异**，而不是把两个 delta 并排当成同类测量。

### 把任务绑回它真正跑过的目录

收集来的任务默认在**空**工作区里回放，因为转录记下了 prompt 和答案，却没记下当时检出的是哪些  
文件。这是诚实的，也几乎没用：录制是对着一棵真实目录树跑的，于是重跑其实在测量另一个任务。  
`--workspace-cwd` 把每个任务绑回它自己那份录制的目录：

```sh
aeval collect --session <id>.jsonl --out datasets/mine --workspace-cwd
```

- 目录是 git 仓库**且有提交** → 变成 `type: git`，钉在录制那次会话**开始时所在的提交**上。这个  
  提交号来自新版 Claude Code 写在转录开头的那段 session-context 块 —— 它的 `Recent commits`  
  第一行就是 HEAD；那里是十位缩写，这里会解析成完整的对象名。  
  - 转录没有记提交号时（老版本、或会话开始的地方不是仓库）→ 退回钉在录制当时所在的**分支**上，  
    注记里写明回放从该分支**现在**的样子起步。  
  - 转录记了提交号、但仓库里已经没有了时（rebase、force-push、`gc`）→ 同样退回分支，但注记写的  
    是**这一条**：对一次对比来说，「转录没记」和「仓库已经往前走了」是两件不同的事。  
  - 录制开始时工作区是脏的时 → 注记写明有多少个路径是提交号无法复现的。回放从该提交的**干净检出**  
    起步，那不是会话开始的地方；只告诉读者一个提交号，他会把它当成起点。  
  - 钉到提交号会让物化走**全量** clone 而不是浅 clone，因为任意一个提交不在 depth-1 的克隆里。  
    这次改动之前收集的数据集钉的是分支、走浅 clone；重新收集时它们的 `dataset_hash` 会变，  
    因为 ref 本身变了。
- 其他情况 —— 根本不是仓库，或仓库没有任何提交 → **拒绝**，点出目录并指向 `--workspace-repo`。  
  不拷贝任何东西：拷贝出来的是**录制结束之后**的目录，如果那次会话正是去修那里面的某个东西，  
  回放就从答案开始，baseline 对比量到的是那份拷贝而不是 agent。
- 目录已经不存在 → 报错并点出这个目录，而不是悄悄给一个空任务。

`type: git` 工作区的**杀伤力正发生在「把它物化出来」这一步**：对没有提交的仓库 `git clone`  
会**成功**但什么都不带，于是这次 run 会在不知不觉中测量一个空目录。它现在被**拒绝**，并点名原因  
（该分支上没有提交，还是 ref 根本不存在 —— 后者不怪仓库）。`--workspace-cwd` 不会走到这条路：  
它在更早一步就拒绝没有提交的仓库。

不带这个 flag 时，空回放会带一条注记点出录制所在的目录 —— 让这个缺口在报告里可见，而不是靠一  
张空白页去暗示；`collect` 也会把同样的话打到 stderr。

### 在录制目录里原地跑（`--in-place`）

即使钉到了提交号，回放仍然发生在**副本**里：树被 clone 到 `--work-root` 下的工作区，上游 `.git`  
被删掉，agent 看到的是一个只有一条提交的仓库。`--in-place` 让 agent 直接在录制目录里跑：

```sh
aeval run --dataset datasets/mine/dataset.yaml --in-place
```

agent 被检出到任务指定的提交号，在真实的树里干活，跑完这次 run 把目录放回去 —— 它来时所在的分支、  
提交号和被跟踪的文件。这个模式要的就是保真：agent 拿到的是当时的树，diff 是相对录制起点的那个  
提交号量的，而不是相对一份拷贝。

它是唯一会碰「harness 不拥有的东西」的模式，所以它的构造方式与工具其余部分相反：**所有它撤不回来  
的事，都在第一个 cell 之前拒绝掉。**

- 工作区**有未提交改动**的仓库被拒绝，并列出路径。提交号能回来，压在它上面的改动回不来。先 commit  
  或 stash。
- `--concurrency` 必须是 1。两个 cell 可能指向同一个目录，调度器里没有任何东西把它们隔开；一次  
  只跑一个，才是「cell 放回去的状态就是它来时看到的状态」唯一成立的安排。
- `--keep` 被拒绝，因为这次 run 不创建工作区，没有东西需要它保护。
- 任务没有仓库、repo 是远端而不是目录、repo 路径相对数据集、有 `subdir`、有 `setup` 命令、或显式  
  写了 `keep_git_history: false` —— 逐条拒绝并点出是哪个任务。最后一条是**测量完整性**层面的拒绝：  
  这个模式把仓库整个交给 agent，所以一个声明「历史不可读」的任务在这里无法被兑现。
- 目录是 linked worktree、是 submodule 检出、是 bare 仓库、或已被另一个 run 持有时，拒绝。

恢复**不覆盖**什么，开跑前的 banner 会写清楚：

- agent 创建或移动的分支、tag、stash。它们只被写进该 cell 的 restore notes，不会被删 —— harness  
  无从知道哪些是 agent 有意留下的。
- 被 `.gitignore` 隐藏的文件。删掉它们会连你的 `.env`、`node_modules` 一起删掉，所以新增的留在原地  
  并写进 notes。
- 任何 push 到远端的东西。整个 run 期间 agent 可以读该仓库的全部历史，而 push 不是一次 run 能放回去的。

在把它指向你在意的仓库之前，还有两件事值得知道：harness 会为那个目录写下自己的 Claude Code 转录，  
所以之后 `aeval collect --cwd <dir>` 会把这些回放会话当成你的录制收进来；另外 agent 是以  
`bypassPermissions` 在一个它能任意写入的目录里跑的。

每个仓库持有一把锁（`.git/agenteval/inplace.lock`），里面写着要把它还回哪个提交号和分支。第二个 run  
会**拒绝**而不是等待；如果一个 run 发现锁还在、而持有它的进程已经不在了，它会打印修复该目录的命令：

```
git -C <repo> update-ref refs/heads/<branch> <commit>
git -C <repo> checkout --force <branch>
git -C <repo> clean -ffd
```

最后一条会丢掉该目录里所有从未提交过的东西，所以只在确认里面没有你要的东西之后再跑。恢复失败的 cell  
会被记录下来、在报告里渲染成失败、并让退出码非零 —— 一个仓库停在错提交上的人，不该被一个 0 退出码  
告知一切正常。

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

run 的表头还会陈述这次 run 究竟驱动了哪份配置，包括它的 skills：声明了哪些插件目录、它们有没有  
真的到达 agent、没到达时被哪个 flag 摘掉。**声明一个目录不等于加载它** —— `--bare` 与  
`--disable-slash-commands` 各自都能独自抹掉用户 skills，而 clean-room 默认把两个都打开。于是  
「只差 skills」的两次 run 过去会比成完全相同；现在比较会把这个差异点名，而当一侧压根没记录配置  
时，比较会**说这件事本身**，而不是安静下来变成「什么都没变」。整份 `agent.Spec` 按 run 落库  
（`runs.agent_spec_json`，schema v4），因为那四个标签列 —— agent、model、bin、backend ——  
识别不出「只有 skills 不同」的一次 run。v4 之前的行对这个问题的回答是沉默，页面上沉默与  
「none」被分开表述。

表头还会点出**用的是哪个模型**，而且它说的比标签列能说的多。`--model` 是可选的，所以凡是没有传它  
的 run，那一列都是空的 —— 而空标签过去渲染成**根本没有模型徽章**，于是三件不同的事看起来一模一样：  
没指定模型、用了 CLI 的默认、还是压根没人记。**这件事由流决定**：init 事件宣告 CLI 打算用什么，  
而按模型分账的那块（`modelUsage`）的键是**真正计了费**的模型。页面读后者、回落前者，并把四种状态  
说清楚：

| 记录里有什么 | 表头写什么 |
| --- | --- |
| 两边都没有 | `model not recorded` |
| 只有 `--model sonnet` | `model sonnet (the stream named no model)` |
| 只有流报了一个 | `model default (the CLI chose it): claude-opus-4-7` |
| 两者都有且不一致 | `model sonnet, but the stream billed claude-haiku-4` |

第三行是真实 run 最常见的样子，第四行才是需要标红的那种：那一刻之后页面上的每个数字，测的都是  
**一个这次 run 并没有选的模型**。别名不算替换 —— `--model sonnet` 会被一个自称  
`claude-sonnet-4-5-20250929` 的流回答，直接比字符串会让每一次普通 run 都报警，而**每次都报的警告  
就等于没人读**，所以一个名字包含另一个就算一致。每个格子也会各自报出答案，因为一次 run 未必只有  
一个模型：中途回落会只出现在某一条格的流里，别处看不见。而当两次 run 声明了同样的东西（或者都  
没声明）却被服务成了不同的模型时，比较会**把这件事点名**，而不是让相等的标签冒充相等的测量。

这一切**没有增加任何新列**：轨迹本来就按 attempt 落库，所以在这之前录下的 run 也照样答得出来。

### 登记渲染了什么，并把它提供出去

`aeval report` 还会登记**它渲染过这么一份东西**，而这是报告里唯一无法事后重推的部分。那一行存的是  
**请求** —— 哪个视图、关于谁、带了哪些参数、以及**已经 resolve 过**的选项 —— 加上载荷的 SHA-256。  
它**不存 HTML，也不存载荷**。载荷是数据库的函数；把载荷存在数据库旁边，等于对同一个问题存了第二份  
答案，而那份答案会在行一变动的瞬间停止更新。

这些行是**日志，不是状态**。同一个视图渲染两次就写两行、两个 id，而那一对哈希才是漂移的证据。  
id 用随机值而非序号（带编号的日志会被读成版本号），也不用内容寻址（内容恰恰是两次登记可以共享的  
东西）。

`aeval serve` 把它们列出来，并按需重建：

```sh
aeval serve --db runs.db --addr 127.0.0.1:8080
```

`GET /` 列出已登记的报告，`GET /r/<id>` 从数据库重建其中一份并渲染，`GET /r/<id>.json` 返回同一份  
文档的 JSON。每个页面上都有一条横幅，说明「它刚刚重建出来的这份文档」与「当初登记的那份」是什么  
关系：

| 横幅写什么 | 什么时候 |
| --- | --- |
| `Recorded report.` | 重建出来的就是当初登记的那串字节 |
| `Recorded in an older payload format.` | 该行早于一次格式变更，两个哈希本就不可比，它们不一致说明不了数据的事 |
| `This page is not the recorded document.` | 行动了，或者投影它们的代码变了；服务端把两种可能都点名，不猜是哪一种 |

哈希覆盖的字节**恰好就是** `/r/<id>.json` 返回的字节，所以脚本可以不经解析页面就核对这个主张 ——  
响应头里有四个字段写着它：`X-Aeval-Reported-Hash` 与 `X-Aeval-Current-Hash`（当初登记的那个 vs 这次  
读取算出来的那个）、`X-Aeval-Hash-Matches`，以及 `X-Aeval-Data-Version`。当某次登记对应的 run 已被  
删掉，它照样答得出来：它说得出那是哪次 run，并说明对象已经不在了。

**只有 `aeval report` 与回放按钮登记。** `run` 与 `matrix` 顺手写出的那份报告是便利产物、不是登记 ——  
那个视图没人要过，把它记进去只会让日志塞满从没被请求过的视图。

### 浏览本机会话并回放

`aeval serve` 同时提供本机录下的会话：Claude Code（`~/.claude/projects`）与 Cursor（它的 `state.vscdb`）。

| 路由 | 内容 |
| --- | --- |
| `GET /sessions` | 所有录制下来的会话，最新在前，用工具自己的名字作标题（Claude Code 生成的标题、Cursor 的对话名，没有就取第一条 prompt）；可按来源或文本过滤。回放本身产生的会话不是录制，不在这里列出 |
| `GET /sessions/<source>/<id>` | 会话明细 —— 目录、分支、commit、模型、tokens、花费、每条 prompt / 回答 / 工具调用 —— 旁边是回放表单和它已有的回放 |
| `POST /sessions/<source>/<id>/replay` | 按所选 harness、skill、model 开始一次回放 |
| `POST /replays/<id>/cancel` | 终止排队中或正在运行的回放；运行中的会杀掉 agent 进程，并记为失败、原因 `cancelled` |
| `GET /replays`、`GET /replays/<id>` | 回放状态与日志；未结束时页面自动刷新，结束后给出报告链接 |

一次回放 = `collect --split turn --workspace-cwd` + `run --chain-sessions` + 登记 run 报告：每条 prompt 一个  
任务，按顺序在同一个工作区里跑，工作区从录制所在仓库检出（目录不是仓库时从空目录起步，日志里会写明）；报告  
像其他登记一样在 `/r/<id>` 打开。回放**一次只跑一个**，免得两个 agent 争抢机器、污染要对照的忙碌时间。服务  
停掉时没跑完的回放，在下次启动时被标记为失败。

harness 与 skill 是**命名预设**，页面只能提交名字、提交不了内容：

```sh
aeval serve --replay-config examples/replay-presets.yaml
```

harness 是 matrix 词汇里的部分 agent 块，**不许设 model 或 `plugin_dirs`**（那是另外两个选项，设了就会覆盖用  
户的选择）。skill 目录不存在 → 启动即报错。在带 `--disable-slash-commands` 的 harness 下选 skill → 表单直  
接拒绝（否则一个 skill 都加载不上）。model 是带建议的自由文本，按 model id 的字符规则校验后作为一个带引号  
的参数传给 `--model`。没给预设文件时提供 `clean-room` 与 `host`（用本机自己的配置跑 Claude Code）两个  
harness、`none` 一个 skill、CLI 默认模型。

这些页面能读本机每一份转录、并以跳过权限确认的方式启动 agent，所以防护比报告页更严：

- **只绑 loopback。** `--addr` 不是 loopback 就拒绝启动，除非加 `--no-sessions`（只提供报告）。
- **不认别的主机名。** 会话路由只接受 `Host` 为 `localhost` 或 IP 字面量 —— 这是挡住 DNS rebinding 页面读转  
  录的那一道。
- **不接受跨站表单。** 每个 POST 都要带服务启动时生成的 token；带了 `Origin` 的必须是本服务。别的网站发  
  不起回放。
- **没有登录。** 本机以你的身份运行、能开 socket 的任何东西都能用这些页面。

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
internal/report/   视图模型、加载器、HTML 渲染器、报告登记表
internal/server/   HTTP 服务：已登记报告、本机会话、回放表单
internal/replay/   回放预设与回放按钮背后的任务队列
internal/machine/  collect 盖上、回放核对的机器标识
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

- **会话页面没有登录。** 只绑 loopback、拒绝外来主机名与跨站表单，但本机以你身份运行的任何进程都能通过  
  它们读每一份转录、发起回放。不能接受时用 `--no-sessions`。

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
- **报告服务只读、且无认证。** `aeval serve` 从数据库重建已登记的报告并把它提供出去；没有登录、  
  没有写入路径、也没有按用户区分的视图。它默认绑 `127.0.0.1`，被要求监听别处时会打警告，  
  因为凡是能连上那个端口的人，就能读到所有已登记的报告。

`docs/real-repo-eval.md` 是「在真实仓库而不是三文件 demo 上评测 agent」的设计，  
它的 §1 表格保持与代码同步。`docs/design.md` 是本实现所回答的最初需求。

## 许可

Apache-2.0。
