# 把评测端当成 RL 环境：设计说明

目标场景：**训练循环需要一个 HTTP 环境服务**——给出一个任务，让策略在真实工作区里动手，
然后把一个可训练的标量交回去。

`aeval` 原本只回答一个问题：「这个模型在这个数据集上考了多少分」。本文档说明
在它之上再回答第二个问题需要哪些东西：「这一次尝试，训练器该拿到什么数」。

命令入口是 `aeval serve --rl`；协议在 `/api/rl` 下；任务体检是 `aeval audit`。

---

## 1. 三层契约

一个 RL 环境只有三件事，缺一件训练循环就跑不起来。

| 层 | 问题 | 谁回答 | 本文档章节 |
| --- | --- | --- | --- |
| **task** | 这次该做什么 | `GET /api/rl/tasks`，`POST /api/rl/envs` | §5 |
| **act** | 策略怎么动手 | `POST .../exec`、`PUT .../files` | §5、§6 |
| **reward** | 训练器拿到什么数 | `POST .../verify`、`POST /api/rl/rollouts` | §2、§3 |

关键设计决定：**reward 不是 grade 的别名**。见下一节。

---

## 2. 奖励契约（reward contract）

### 2.1 grade 与 reward 回答不同的问题

- **grade** 回答「这次尝试是否满足任务」。它有权重、有部分分、有阈值，是一个**判断**。
- **reward** 回答「训练器拿到什么数」。它是一个**标量**，会被直接塞进 advantage 计算。

把两者合成一个数是最省事的做法，也是最容易出事的地方：一旦合成，你无法再区分
「任务被判负」和「奖励被设计成稀疏」，而这两件事在训练里的后果完全不同。所以数据
集里分成两个字段：

```yaml
tasks:
  - id: implement-slugify
    reward:
      type: pass          # 稀疏：0 或 1
      notes: 全对才算数
    graders:
      - type: pytest      # 稠密：算出加权分
```

`internal/reward` 是唯一的投影实现，输入是 `(*grader.Grade, rejected bool)`，输出是
`Verdict{Value, Kind, Rejected, Reason}`。引擎里**只有一处**调用它
（`internal/engine/verify.go` 的 `Verify`），评测链路与环境服务共用——两份实现意味着
两种奖励口径，那样训练数据就失去了可比性。

### 2.2 两种密度

| `reward.type` | `Kind` | 取值 | 什么时候用 |
| --- | --- | --- | --- |
| 省略 / `score` | `score` | `clamp01(Grade.Score)`，稠密 | 有清晰的部分分梯度时。默认值 |
| `pass` | `pass` | `0` 或 `1`（`Grade.Passed`） | 任务只有「做到 / 没做到」，部分分是噪声时 |

`EffectiveType()` 对 nil receiver 返回 `score`，所以**没写 `reward` 字段的任务行为和
以前完全一样**——这是刻意的，否则加入这个特性会把所有旧数据集静默改成稀疏奖励。

### 2.3 被拒绝的尝试恒为 0

```go
// internal/reward/reward.go
if rejected {
    return Verdict{Value: 0, Kind: ..., Rejected: true, Reason: reason}
}
```

这条在**稠密档也成立**。理由：如果作弊拿到 0.8 分就付 0.8 的奖励，那么「改写测试
断言」就是环境里的一条合法捷径，而且是奖励最高的那条。奖励必须比正确解低，而不是比
它略低一点。

配套地，`Verdict.Rejected` 会被透传到 `metric.Sample`、`report.Task` 和
`rlenv.Result`，所以事后能区分「答错了」和「改了验证器」——只看 `reward = 0` 是分不出来的。

### 2.4 rejected 判定发生在什么位置

顺序是固定的，写在 `Verify` 里，不能换：

```
原始 changed set + guards  →  diff（经 output filter）  →  grade
   → guard 命中则翻 grade.Passed = false  →  投影 reward
```

把投影放在翻 `Passed` 之后是必须的：反过来的话，一次被拒绝的尝试会先算出稠密分，
再被翻成「未通过」，于是还能拿到部分分。

**guards 直接翻 `Grade.Passed` 而不是只动 reward**，这样 `resolve_rate`、`pass^k`、
`cost_per_resolved` 这些既有指标不需要各自学一遍 guard 语义——它们看的是同一个
`Passed`。

---

## 3. 两条防作弊路径：`restore_paths` 与 `forbid_changed_paths`

这两个字段长得很像，回答的问题相反。

| 字段 | 动作 | 回答 | 典型用法 |
| --- | --- | --- | --- |
| `forbid_changed_paths` | 事后否决 | 「**是否守规矩**」 | 不许动 CI 配置、不许改依赖清单 |
| `restore_paths` | **评测前还原** | 「**能不能干活**」 | 测试文件在判分前 `git checkout` 回 baseline |

`restore_paths` 在 `internal/sandbox/workspace.go` 的 `(*Baseline).RestorePaths` 里实现：
tracked 文件走 `git checkout <SHA> -- <path>`，untracked 文件 `os.Remove` 之后再
`git rm -f --cached --ignore-unmatch` 把索引清干净。

实现上还有一处容易漏：`restore_paths` 也会被追加进 diff 的 `IgnorePaths`
（`internal/grader/grader.go` 的 `diffOptions()`）。否则还原动作本身会出现在 diff 里，
被 `patch` grader 当成 agent 的改动。

### 为什么还原比否决更重要

SWE-bench 的默认做法是**评测前**把测试文件 checkout 回 baseline。这不是宽容，而是
把评价对象从「服从性」换成「能力」：一个改了断言让自己通过的策略，和一个真的修好了
bug 的策略，在「不许改测试」这条规则下会得到同样的 0——但它们的训练信号本该天差地别。

两者都要有。只还原不否决，就看不到「agent 试图走捷径」这个信号；只否决不还原，
训练器就在给一个**服从性测试**付奖励。

---

## 4. 任务体检：`aeval audit`

奖励可信的前提是任务本身可信。`aeval audit` 对每个任务跑三项检查：

| 检查 | 问题 | 判定方式 |
| --- | --- | --- |
| **可解性** | 参考解能通过吗 | `oracle` 跑一次，`Grade.Passed` 必须为真 |
| **必要性** | 什么都不做会失败吗 | baseline 跑一次，必须**不**通过 |
| **稳定性** | 重跑结论一致吗 | `oracle` 连跑 `--repeats` 次，逐 grader 比对 |

```bash
aeval audit --dataset datasets/demo --repeats 3
```

**必要性检查抓的是「白拿奖励」**：一个 baseline 就已经通过的任务，会为「什么都不做」
支付奖励。在 benchmark 里这只是个平庸的任务，在 RL 里它是一条会摧毁策略的捷径——
策略会学会尽快结束回合。

### `unknown` 不等于 `failed`

`Check.Status` 有三个值，第三个是必须的：

- `ok` —— 测过了，没问题
- `failed` —— 测过了，有问题
- `unknown` —— **测不出来**

没有 `oracle` 的任务（比如自由文本问答，参考解不是 patch）落在 `unknown`。把它当成
`failed` 是在污蔑任务；当成 `ok` 是在撒谎。报告里两者分开列。

同理，oracle 模式失败时 `StopReason` 写的是 `oracle_error`，不是 `harness_error`：
**数据集自己的参考解不可用 ≠ 环境故障**。

`--strict` 在有不健康任务时退出码为 1，默认退出 0——理由和评测本身一致：审计发现了
问题不等于审计命令失败，CI 里要能自己决定怎么处理。

---

## 5. HTTP API

所有路由挂在 `/api/rl` 下。完整列表也可以从 `GET /api/rl` 的 `operations` 字段自取，
且该字段**只列出当前真正可用的路由**（exec 未启用时不列 exec 与 files）。

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| `GET` | `/api/rl` | 数据集、任务数、预算与开关状态 |
| `GET` | `/api/rl/tasks` | 任务列表 |
| `GET` | `/api/rl/tasks/{task}` | 单个任务 |
| `GET` | `/api/rl/envs` | 环境列表 + 统计 |
| `POST` | `/api/rl/envs` | 开环境（`201`） |
| `GET` | `/api/rl/envs/{id}` | 单个环境状态 |
| `POST` | `/api/rl/envs/{id}/exec` | 在工作区里跑命令 |
| `PUT` | `/api/rl/envs/{id}/files` | 往工作区写文件 |
| `POST` | `/api/rl/envs/{id}/verify` | **终态**：判分、投影 reward、销毁工作区 |
| `POST` | `/api/rl/envs/{id}/delete` | 不判分直接销毁 |
| `GET` | `/api/rl/envs/{id}/archive` | 一个环境的完整证据 |
| `POST` | `/api/rl/rollouts` | 让配置的 agent 跑一次，直接返回 reward |

### 5.1 一次完整的 rollout

```bash
# 1. 看有哪些任务
curl -s localhost:8787/api/rl/tasks | jq '.tasks[] | {id, reward_kind, has_oracle, guarded}'

# 2. 开一个环境
ENV=$(curl -s -XPOST localhost:8787/api/rl/envs \
      -d '{"task_id":"subtract"}' | jq -r .id)

# 3. 策略动手（需要 --rl-allow-exec）
curl -s -XPOST localhost:8787/api/rl/envs/$ENV/exec -d '{"command":"ls -la"}'
curl -s -XPUT  localhost:8787/api/rl/envs/$ENV/files \
     -d '{"path":"thing.py","content":"def calc(a, b):\n    return a - b\n"}'

# 4. 交卷
curl -s -XPOST localhost:8787/api/rl/envs/$ENV/verify | jq
```

`verify` 的响应就是训练循环需要的那一份：

```json
{
  "env_id": "env-3f9c1d2e-...",
  "task_id": "subtract",
  "reward": 1,
  "reward_kind": "score",
  "score": 1,
  "passed": true,
  "rejected": false,
  "stop_reason": "verified",
  "run_id": "rl-8b1e04a7-...",
  "task_run_id": "rl-8b1e04a7-.../subtract#0",
  "cost_usd": 0,
  "turns": 0
}
```

几个字段值得单独说：

- **`run_id` / `task_run_id` 是回报，不是装饰**。每一条 reward 都对应数据库里的一行，
  所以 `aeval report <run_id>` 能把这份 reward 的证据渲染出来，而不是让训练器凭信任接受它。
- **`rejected` / `violations` / `restored`** 让训练循环能区别对待作弊。
- **`error` 非空表示 harness 失败**，不是策略失败。这种 rollout **必须丢弃**，
  不能当成 0 奖励学进去。

### 5.2 交给 aeval 自己跑：`rollouts`

如果训练循环不想手工驱动工作区：

```bash
curl -s -XPOST localhost:8787/api/rl/rollouts -d '{"task_id":"subtract"}' | jq
```

它用配置里的 agent（`agent.kind` / `agent.model` / `agent.bin`）跑一次完整评测并返回
同样的 `Result`。这是「拿 benchmark 当环境」的最短路径，也是接入已有训练框架时最省事
的形态。

### 5.3 `verify` 是终态

```go
// internal/rlenv/rlenv.go
// Verify 先抢占 state 再评分，成功后无论如何销毁工作区。
```

被验证过的环境不再接受任何动作，重复 `verify` 返回 `409` **并带上原结果**，`exec`
同样返回 `409`。原因只有一个：**策略见过 reward 之后不能再动**。允许「看一眼分再补一手」
等于把 verifier 变成一个可以反复试探的预言机，而这是 RL 里最经典的作弊面。

---

## 6. 威胁模型与四道闸门

这是整个工具里**唯一会让进程去「做」而不只是「渲染」**的部分：它会起沙箱、跑 agent，
在 `--rl-allow-exec` 下还会以 harness 用户的身份跑任意命令。所以闸门是四层，不是一层。

| # | 闸门 | 作用 |
| --- | --- | --- |
| 1 | 必须有 `--rl` | 否则所有路由 `404`，并**在错误信息里点名这个 flag** |
| 2 | 同源检查 | 拒绝带外部 `Origin` 的请求，同删除路由 |
| 3 | `--rl-allow-exec` | exec / files 路由单独开关 |
| 4 | 预算上限 | 见下表，触发返回 `429` |

### 第 2 道闸门为什么必要

服务默认只绑 loopback，但「只绑 loopback」**不等于「只有本机能访问」**：用户浏览器里
任何一个打开的页面都能向 `localhost` 发起请求。同源检查拦的是这条路径。

### 预算

| 配置 | 默认 | 含义 |
| --- | --- | --- |
| `--rl-max-envs` | 8 | 同时打开的环境数 |
| `--rl-ttl` | 15m | 一个环境最多活多久（到点由 reaper 回收） |
| `--rl-max-execs` | 200 | 每个环境最多跑多少条命令 |
| `--rl-max-rollouts` | 0（不限） | 本进程总共跑多少次 rollout |
| `--rl-max-cost-usd` | 0（不限） | agent 成本累计上限 |
| `--rl-allow-exec` | false | 命令与文件写入 |

上限触发一律返回 `429` + `ErrLimit`，**绝不伪造一个任务失败**。这两者的区别是：一个
是「你请求得太快」，另一个是「策略做得不好」——把前者伪装成后者会直接污染训练信号。

以上每一项都可以写进 `aeval.yaml` 的 `serve.rl` 段（`enabled`、`allow_exec`、
`max_envs`、`ttl`、`max_execs`、`max_rollouts`、`max_cost_usd`），逐项注释见
`examples/aeval.yaml`。命令行 flag 只覆盖**它自己那一个字段**：配置里写了
`allow_exec: true`，命令行只传 `--rl-max-envs 4`，两者都生效。

`0` 表示不限，是刻意的默认：这个服务是本地单用户工具，训练循环自己知道要跑多久；
默认设一个数会把「跑长一点」变成需要读文档才能发现的故障。

### 客户端也会被限制

- 请求体有上限：控制面 `64 KiB`，写文件 `8 MiB`。一个把数据集误当成 patch 发过来的
  训练脚本不能让我们把它分配出来。
- 未知 JSON 字段直接报错（`DisallowUnknownFields`），避免字段名拼错后静默变成默认值。

---

## 7. 训练循环怎么接

```python
import requests

BASE = "http://127.0.0.1:8787"

def rollout(policy, task_id):
    env = requests.post(f"{BASE}/api/rl/envs", json={"task_id": task_id}).json()["id"]
    try:
        for _ in range(MAX_STEPS):
            action = policy.act(observations)          # 你的策略
            if action.kind == "exec":
                requests.post(f"{BASE}/api/rl/envs/{env}/exec",
                              json={"command": action.command})
            else:
                requests.put(f"{BASE}/api/rl/envs/{env}/files",
                             json={"path": action.path, "content": action.content})
        res = requests.post(f"{BASE}/api/rl/envs/{env}/verify").json()
    finally:
        requests.post(f"{BASE}/api/rl/envs/{env}/delete")   # 幂等，已 terminated 也安全

    if res.get("error"):
        return None                 # harness 失败：丢弃，不要当成 0 奖励
    return res["reward"]
```

三条纪律：

1. **`error` 非空的 rollout 必须丢**，不要填 0。填 0 是在教策略「有时候做什么都是错」。
2. **`rejected` 为真的 rollout 可以考虑加权或单独处理**。它们已经是 0 奖励，但知道
   有多少尝试在作弊是很有用的诊断信号（见 §8 的 `guard_violation_rate`）。
3. **`--rl-max-envs 1` + 串行**是最容易调试的起点。并发是性能选项，不是正确性选项。

---

## 8. 落库与可审计

每一项都成为一次 run，`runs.mode = 'rl'`。这不是记账癖：oracle / baseline / rl 三种
模式产出的结果**形状完全相同**，只有 mode 这一列能阻止「参考解通过了」被读成
「模型通过了」。

由此得到三个 RL 面向的指标（`aeval registry` 里有）：

| 指标 | 含义 | 为什么单列 |
| --- | --- | --- |
| `mean_reward` | 平均 reward，**含未启动与被拒绝的 cell** | 只算成功的 cell 会系统性高估 |
| `reward_signal_rate` | 非零 reward 的占比 | 全 0 的批次不是「难」，是坏掉了 |
| `guard_violation_rate` | 触发 guard 的比例，带 `by_kind` 拆分 | 作弊率是任务设计质量的信号 |

报告页里任务表有独立的 **Reward** 列（挨着 Score），run 头部有 mode 徽章，delta 视图
里非 agent 的 mode 会高亮。`reward_kind` 为空时该列显示 `-` 而不是 `0`——**没有记录**
和**记录了一个 0** 是两回事。

---

## 9. 已知边界

诚实列出来，避免被当成已完成：

- **只有 turn 级轨迹**。Claude Code CLI 不吐 token id，也不吐 logprobs，所以拿不到
  token 级的 importance ratio。用这个服务做 PPO 只能拿到序列级（或 turn 级）的信号；
  要做细粒度的 credit assignment，得换一个能吐 logprobs 的 agent 实现。
- **docker 后端的环境服务没有端到端跑过**。`WorkspaceDir` 是宿主路径，只在 local 后端
  有意义；docker 环境必须全程通过 `exec` 驱动。这条路径只有单测。
- **reaper 的精度是 `TTL/4`**，所以实际最长存活时间在 `TTL` 到 `1.25 × TTL` 之间。
- **`oracle` 只能判「可解」，不能判「唯一」**。一个有多种正确实现的任务，oracle 过了
  只证明至少有一条路；另外那些路是否也被 grader 接受，需要额外的用例级断言。
- **`--rl-max-envs` 等 flag 的默认值不是 0，而是配置项的默认值。** 这与其他
  「0 = 未设置」的 flag 不同，所以「命令行传 `--rl-max-envs 8`」和「什么都不传」在
  配置文件写了 `max_envs: 3` 时结果不同：前者覆盖成 8，后者保留 3。想用配置值就别在
  命令行重复传同名 flag。

---

## 10. 相关文档

- `docs/real-repo-eval.md` —— 真实代码仓库任务的评测设计，以及它对奖励可信化提出的
  要求（用例级断言、`restore_paths`、per-task 镜像）。
- `README.zh-CN.md` —— 数据集格式、grader 与指标注册表。
