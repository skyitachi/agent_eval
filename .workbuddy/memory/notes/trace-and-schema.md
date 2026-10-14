# token 记账 / 落库格式 / 数据集格式

## token 记账（最容易错的地方）

**一次 API 调用的用量 ≠ 一次 run 的用量。** `result` 事件的 `usage` 是**那一次调用**；**会话累计在
`modelUsage`**（**驼峰**！fixture 写成 `model_usage` → 空 map → 测试静默空转）。单 result 事件时两者**有时恰
好相等**，所以这个错能潜伏很久；一旦流出两个 result 事件就露馅（报告 18,540 vs 真值 164,419，**少 89%**）。

- 规则：`trace.Trace.Usage()` **优先求和 `modelUsage`**，空则回落 `usage`；`agent.Attempt.Usage()` 只许走它
  （collect 侧一直是对的，**只有 run 侧没用**）。
- **多个 result 事件是同一会话的累计快照**：按「总量最大」合并（幂等）；逐字段 last-wins 会让更小的快照抹掉更
  大的，`total_cost_usd` 同理。但**描述答案的字段相反** → 取最后一个。
- `result.num_turns` **不是会话轮数**；`Trace.Turns()` 优先用流推导的 `TurnCount`（按 assistant message id 去
  重），runner 也写它。`trace_json` **落库不截断** → 历史行真值可事后重算，但**没有 backfill 命令**（旧行仍
  错）。
- **子代理造成两种不平衡**：`s.Usage`（含 sidechain）≠ `sumTurns(s.Turns)`（不含）；两侧都**没记录「跑过子代
  理」**这件事。
- `metadata` 是引擎**从不解释**的 provenance 块；`baseline_tokens`/`baseline_ms` 曾读**会话**总量 →
  `--split turn` 下与其旁的 baseline 文件自相矛盾（528,296 vs 108,935），已改为取切片值。分轮切片的**嵌套语
  义是故意的**（有测试钉住 `want the whole session`），别当 bug。
- 护栏：`internal/trace/testdata/multi_result_stream.jsonl` 是**真实击穿我们的那条流**（50 行 / 2 result /
  11 轮），断言它仍能区分「一次调用」与「一段会话」。

## schema 版本（表）

往后拒绝、往前升级；更高 = 拒绝（列含义不明），更低 = **就地升级**（另一条路是扔掉付过钱的 run），发生在**任
何一次 open**（含 `report`）。`migrations[i]` = v(i+1)→v(i+2)，只追加；每步**必须能重跑**（`ADD COLUMN` 不幂
等，在 DDL 与版本号之间被杀掉会报 "duplicate column name"）。

| v | 列 | 为什么非要落库 | 空值语义 |
|---|---|---|---|
| v2 | `attempts.baseline_output` | 数字说不出两次尝试是否同一结论 | **「录制里没留下答案」**（omitempty，客户端写一句话而非空框） |
| v3 | `attempts.baseline_{agent_name,agent_model,source,notes}` | 基线是**输入**、live beside dataset，渲染期**不读数据集** → 不落库就说不出一句「录制来自 cursor / 本次 claude-code」，跨 agent 回放会被读成同口径比较 | **「没记录过来源」≠「录制没有 agent」** |
| v4 | `runs.agent_spec_json`（整份 `agent.Spec`） | 四个标签列（kind/model/bin/backend）**识别不出「只差 skills」的 run** | **「没记录配置」**，与「配置里什么都没声明」分开表述 |
| v5 | `runs.chain_sessions` | attempt 行上**没有任何一列**能事后推出「这些 cell 是一条续写而不是独立采样」 | false/空 = v1–v4 的普通 run，**语义正确不是缺口** |

v3 用**四列而非一个 JSON blob**（各自独立被读；blob 会把「空还是坏」的问题推给每个读者）。
`runs.AgentSpec()` 返回 `(spec, ok)`，`ok=false` 时报告 `RunInfo.Skills = nil`（页面写 `not recorded`）。
升级路径都是一句 `ADD COLUMN ... DEFAULT ...`；store 夹具 drop 掉那一列再
`UPDATE schema_meta SET value='<前一个版本>'`。

**夹具构造法**：从**今天的 schema 删掉新加的东西 + 降 version**，不抄 DDL。要点名的坑：**低版本夹具要把所有
更高版本加的列一起删** —— 只删 v2 那列得到的是「v2 再降一版」，不是 v1。e2e：
`TestCLIReadsADatabaseFromBeforeTheRecordedAnswer`。

## 数据集格式

`dataset.yaml`（`tasks_file`/内联 `tasks`、`defaults` 合并、`filter`）+ `tasks.jsonl`。`workspace`：`files`
（assets + 字面量）或 `git`（repo/ref/subdir），`setup` 在 agent 前跑。`output.type`：`patch`/`files`/
`stdout`/`none`。`graders[].weight` 是**指针**（区分未设=1 与显式 0=只跑不计分）；`required:true` 判负整个
task。内建 grader：`patch`/`pytest`/`exact_match`/`llm_judge`/`command`。指标两层：run 头部展示值；归因七个配
对指标（pass_rate 为 `Primary`）各带 `LowerIsBetter`。**timeouts 与 harness_errors 恒显（即使为 0）** —— 只
列「它有的问题」会让不存在的问题不可见（前者关于 agent「跑超预算」，后者关于 run「压根没测到」）。旧 34 指标
注册表 + 自定义 DSL **已删**（`internal/metric` 不存在）。

### patch 不是模型的产出

`patch` 是 harness 在 run 结束后**从磁盘反算**出来的：`ChangedPaths` 先 `git add -A -N`（让 untracked 也进
diff）再 `git diff --name-only --no-renames <baseline SHA>`；`Diff` 是 `git diff <SHA>` 的 unified diff 字符
串，落进 `agent.Attempt.Diff`；`grader/patch.go` 只调这两个、**不读任何文件**。

- `output.type` 描述的是**期望与判分口径**，不决定 agent 怎么干活。
- agent 自己 `git commit` **不影响** diff（baseline SHA 是物化时固定的）。
- 但 agent 改完又 `git checkout` 回去 → 后态里什么都看不出来（anti-gaming 盲区）；
  `sandbox.Baseline.RestorePaths` 正是为这个写的原语，**今天没有调用方**。
- 会话结束时的目录状态 = 初始环境 + 答案，**二者不可分离** —— 采集到的快照必然是**后态**，`dirty` 层（已修改
  + 未跟踪）语义上**就是答案本身**。见 `notes/environment-freezing.md`。
