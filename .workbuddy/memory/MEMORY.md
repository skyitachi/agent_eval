# agenteval — 项目长期记忆

Go 的 agent 评测框架。`/Users/skyitachi/lab/agent_eval`，module
`github.com/skyitachi/agenteval`，Go 1.25.1。链路：数据集 → 可插拔 grader → SQLite → 自包含
HTML 报告。只有 Claude Code 一种 agent（`mock` 是测试替身），model 自由指定（透传 CLI）。
七条子命令（`cmd/aeval/main.go`）：`collect`/`validate`/`run`/`matrix`/`report`/`list`/`version`。
回答「做得怎么样 / 差异归因给谁 / 真实会话花了多少」，**拒绝**回答「这个 agent 好不好」。

## 三轴归因（OFAT）

- harness = 实现+配置；`model` 一轴；`skill` 一轴。一次 sweep 只动一条轴，**加载期强制**：level
  改了本轴不拥有的键 → 拒绝（不是警告）。轴的所有权靠**反射读 `agent.Spec` 的 yaml tag** 推导
  （`internal/attrib/plan.go` 的 `specKeys`/`Axis.Owned()`）——写死会漏字段，用
  `yaml.Marshal(零值)` 会因 `omitempty` 退化成空集（踩过）。
- 每条 level 必须记录**改了哪些键**（`attrib.Meta.Changed` → run 的 `config_json`）：缺了它，读者
  分不清「这个 level 没改东西」和「差异没被记录」。
- 分离度用**精确符号检验**（log 空间求和，`0.5^n` 在 n=1074 处下溢）；**非平局配对 < 6 对时无论
  多偏都到不了 p<0.05**（写进每份报告的 caveat）。方向由 **win/loss 分裂**定，不由均值。

## claude 的 skill 闸门（2.1.296 实测，帮助文本误导过我）

- `--bare` **和** `--disable-slash-commands` **各自独立**都能抹掉 skill（后者连 `--plugin-dir`
  注入的也抹）。要 skill 必须两个都关：`-bare=false -disable-slash-commands=false`（保留
  `--strict-mcp-config`），实际命令行只剩 `--strict-mcp-config`。claude **不接受**
  `--disable-slash-commands=false`，aeval 是靠**不传这个 flag** 实现的。`--strict-mcp-config`
  不碰 skill，只清用户 MCP。
- `--plugin-dir` 只吃**插件**目录（`.claude-plugin/plugin.json` + 同级 `skills/`）；给裸 skill
  目录是**静默无效**。`skills` 做成**单个软链**指向 `~/.claude/skills` 可自动跟随新增；包成插件
  后 skill 名变 `<plugin>:<skill>`。
- **零成本探针**：`claude -p --output-format stream-json --verbose --model <不存在的模型>` ——
  `system/init` 在 API 调用**之前**发出，可白拿 `tools`/`slash_commands`/`mcp_servers`，随后 400。
  比读帮助文本可靠。
- 用户本机要求加载 `~/.claude/skills/`（做法见上）；代价：自己的 hooks、settings.json 的 env
  （覆盖 `ANTHROPIC_BASE_URL` → siliconflow）、auto-memory/CLAUDE.md 发现都回来。run 头打
  `bare=false skills=on` 是唯一记录（报告里看不到，见缺口）。

## collect

从 Claude Code 转录生成**普通数据集**（prompt + cwd + baseline，**没有 grader** —— 转录没有
ground truth，判失败是造判决）。**墙钟 ≠ 忙碌时间**（实测 4416s vs 533s）→ 用 `Baseline.BusyMs`。
按 `message.id` 去重（176 条 assistant 记录 / 63 个 id，不去重放大 3 倍）。

## 设计约定（改代码前先读）

1. **报告是「数据库的函数」**：渲染层只读持久化状态（`report/model.go`+`load.go` 把行投影成视图
   模型，**借用** `runner.Summary`/`attrib.Attribution` 而不镜像）。e2e 断言运行期报告与从 db
   重生**逐字节相同**（比对时省掉 `generated`）。
2. **XSS 靠编码器不变量**：`render.go` 的 `neutralize()` 把 payload 的 `<` `>` `&` 重写成
   `\u003c`/`\u003e`/`\u0026`（JSON 字符串外不含这三个字符，重写不改结构），一次封死三种逃逸。
   客户端默认 `esc()`，只有 `raw()` 例外。
3. `html/template` 上下文敏感：注入 `<style>` 用 `template.CSS`、`<script>` 用 `template.JS`；用
   `template.HTML` 会被当文本**重新转义**（踩过）。
4. **`hidden` 必须由样式表显式兑现**：UA 的 `display:none` 被任何作者 `display` 覆盖
   （`.drawer{display:flex}` 直接作废它），空抽屉不报错、只是**不透明盖住右边 1000px**，看起来像
   「布局坏了」。加 hidden 元素必须同步加 `.cls[hidden]{display:none}`；
   `TestHiddenElementsCanActuallyBeHidden` 守它。
5. `MaxInlineBytes`（默认 8 MiB）超了按优先级丢 detail（失败/出错留最久，skip 最先丢），降级记入
   `Warnings`。轨迹截断留**头尾 + gap marker**。
6. 退出码 `0/1/2/130`；**有任务失败 ≠ 命令失败**（否则 CI 没法用）。
7. 数据集/matrix 文档**就地解码**，未知键是**错误**（拼错的 `plugin_dirs` 会产出「skill 没用」的
   sweep）。时长必须带单位。
8. 路径相对**声明它的文档所在目录**解析；经 CLI 传的（`--bin`/`--work-root`/`--session`/`--file`）
   相对**调用者 cwd**。
9. 「裸命令名 vs 路径」：值里没有 `/` → 当 PATH 上的命令原样保留，否则绝对化。唯一实现在
   `agent.ResolvePath`。
10. **测量值不重试**：timeout/非零退出/error result 都是**数据**。`retries` 只管进程起不来，默认 0。
11. agent 默认 **clean room**（无 user hooks / auto-memory / 任务外 MCP / CLAUDE.md）。
12. **位置参数可写在 flag 之后**：Go 的 `flag` 遇第一个非 flag 就停，`report --compare a b -o x` 会
    把 `-o x` 当位置参数并报「got 4」；`positionals()` 从那个词起重新解析。
13. **工具打印的命令必须能跑**：`Next:` 提示漏 `--db` 就会给出必然失败的命令；`printSummary` 只在
    `--db` 非默认时补它、**按用户输入原样回显**，`hintArg` 给带空格的值加引号。**找不到记录必须点
    名文件**：`store.NotFoundError{Kind,ID,Path}` 保持 `errors.Is(err, ErrNotFound)` 同时打印
    `store: run X not found in aeval.db`（「not found」像工具坏了，「not found in aeval.db」才指向
    `--db` 给错）。
14. **schema 版本：往后拒绝、往前升级**。更高 = 拒绝（列含义不明）；更低 = **就地升级**（另一条路
    是扔掉付过钱的 run），发生在**任何一次 open**（含 `report`）。`migrations[i]` = v(i+1)→v(i+2)，
    只追加；每步**必须能重跑**（`ADD COLUMN` 不幂等，在 DDL 与版本号之间被杀掉会报
    "duplicate column name"）。**v2 = `attempts.baseline_output`**（`dataset.Baseline.FinalOutput`
    → `store.BaselineComparison.Output` → `report.BaselineView.Output`）；v1 库升上来读作空串，
    语义是**「录制里没留下答案」**（omitempty，客户端写一句话而不是空框）。e2e：
    `TestCLIReadsADatabaseFromBeforeTheRecordedAnswer`（由「删掉 v2 加的东西」构造，不抄 DDL）。

## token 记账（最容易错的地方）

**一次 API 调用的用量 ≠ 一次 run 的用量。** `result` 事件的 `usage` 是**那一次调用**；**会话累计
在 `modelUsage`**（**驼峰**！fixture 写成 `model_usage` → 空 map → 测试静默空转）。单 result 事件
时两者**有时恰好相等**，所以这个错能潜伏很久；一旦流出两个 result 事件就露馅（报告 18,540 vs
真值 164,419，**少 89%**）。

- 规则：`trace.Trace.Usage()` **优先求和 `modelUsage`**，空则回落 `usage`；`agent.Attempt.Usage()`
  只许走它（collect 侧一直是对的，**只有 run 侧没用**）。
- **多个 result 事件是同一会话的累计快照**：按「总量最大」合并（幂等）；逐字段 last-wins 会让更小
  的快照抹掉更大的，`total_cost_usd` 同理。但**描述答案的字段相反** → 取最后一个。
- `result.num_turns` **不是会话轮数**；`Trace.Turns()` 优先用流推导的 `TurnCount`（按 assistant
  message id 去重），runner 也写它。`trace_json` **落库不截断** → 历史行真值可事后重算，但**没有
  backfill 命令**（旧行仍错）。
- **子代理造成两种不平衡**：`s.Usage`（含 sidechain）≠ `sumTurns(s.Turns)`（不含）；两侧都**没记录
  「跑过子代理」**这件事。
- `metadata` 是引擎**从不解释**的 provenance 块；`baseline_tokens`/`baseline_ms` 曾读**会话**总量
  → `--split turn` 下与其旁的 baseline 文件自相矛盾（528,296 vs 108,935），已改为取切片值。分轮切片
  的**嵌套语义是故意的**（有测试钉住 `want the whole session`），别当 bug。
- 护栏：`internal/trace/testdata/multi_result_stream.jsonl` 是**真实击穿我们的那条流**（50 行 /
  2 result / 11 轮），断言它仍能区分「一次调用」与「一段会话」。

## 数据集格式

`dataset.yaml`（`tasks_file`/内联 `tasks`、`defaults` 合并、`filter`）+ `tasks.jsonl`。
`workspace`：`files`（assets + 字面量）或 `git`（repo/ref/subdir），`setup` 在 agent 前跑。
`output.type`：`patch`/`files`/`stdout`/`none`。`graders[].weight` 是**指针**（区分未设=1 与显式 0=
只跑不计分）；`required:true` 判负整个 task。内建 grader：`patch`/`pytest`/`exact_match`/
`llm_judge`/`command`。指标两层：run 头部展示值；归因七个配对指标（pass_rate 为 `Primary`）各带
`LowerIsBetter`。**timeouts 与 harness_errors 恒显（即使为 0）** —— 只列「它有的问题」会让不存在
的问题不可见（前者关于 agent「跑超预算」，后者关于 run「压根没测到」）。旧 34 指标注册表 + 自定义
DSL **已删**（`internal/metric` 不存在）。

## 报告客户端（app.js，唯一没单测的一层）

同类错踩三次，规则写进了文件头注释：

1. 插进模板的值必须已转义或包 `raw()`；裸 `tpl` 结果再插值 → **二次转义**（diff 视图犯过）。
2. **节点只能 append，不能插值**（`tpl` 会 `String()` 它）—— 抽屉六个标签页全因此画成
   `[object HTMLDivElement]`；读代码看不出来，必须真渲染。
3. `raw()` 自带 `toString()`，否则 `'a' + raw(x)` 得到 `[object Object]`（baseline 行犯过）。
4. 兜底：`tpl` 对节点/普通对象输出 `⚠ unrendered <Ctor>` 的 `.bad` 标记 —— **错误必须不像数据**。
5. overview 的 `Final answer` 是**两栏**（this attempt / recorded (baseline)）—— 数字说不出两次
   尝试是否得出同一结论。录制没留答案时写一句话，不留空框。

护栏：`tests/render_test.go`，**唯一需要浏览器的测试**（找不到 skip，`AGENTEVAL_CHROME` 可覆盖）。
探针注入 → 点每个 tab → 读回 DOM，断言字节数下限、无 `.bad`、无 `[object `、diff 有
`<span class="dl `、baseline 卡两个 delta 含录制总量、两栏答案都在。三处变异验过会红。

## 测试与运行

- `go test ./...` 全过（13 包）。e2e 在 `tests/e2e_test.go`，用真实二进制/子进程/SQLite/HTML 驱动。
  覆盖率 attrib 91.8% / report 86.6% / store 82.6%，**runner 只有 8.9%** —— 编排行为靠 e2e，这是
  诚实的现状（编排器的正确层级就是 e2e）。
- e2e 的 payload 结构**刻意不复用** `report.Data`（否则字段改名测试会点头），也**刻意不声明
  `generated`**。解 payload 找**第一个 `;</script>`**（后面紧跟内联 app.js）。
- **pytest grader 需要带 pytest 的解释器**：`AGENTEVAL_PYTHON`。本机 managed python 3.13.12
  **没有 pytest**，可用 `~/.workbuddy/binaries/python/envs/default/bin/python`（pytest 8.4.1）。
  `runCLI` **只在探测到带 pytest 时才导出**它（导出空值 → 回落 python3 → 报「没有 pytest」被误读
  成 agent 挂了）。**推论：任何断言「demo 三题全对」的 e2e 必须 `requirePytest(t)`**
  （`TestCLIMatrixAttributesASweep` 曾漏，demo 2/3 靠 pytest 判分）。
- fixture：`tests/testdata/agents/good.sh`（正确补丁）/`bad.sh`（各任务错法不同）；
  `examples/matrix-harness.yaml` 是可离线跑的 sweep 示例。
- **无头 Chrome 里 CSS 动画永不推进**（`document.timeline` 走但动画 `currentTime` 恒 0），
  `.drawer` 的 160ms 淡入永远停在 `opacity:.4`，看起来像「半透明抽屉」—— 环境假象，**截图复核前
  先注入 `.drawer{animation:none}`**。
- `--dump-dom` 输出**含注入脚本自身**，探针结果要 base64 回传，匹配字面量要运行时拼
  （`'[' + 'object '`），否则命中探针源码。
- **本机 grep 空结果先怀疑表达式**：BSD grep 不支持 `\|`，ERE 也不支持 `\s`/`\d`/`\b`（会**静默返回
  空**）。一律 `-E` 并避开这些转义。

## gitignore 约定

根锚定的 `/workspaces/` 是跑评测的默认落盘处（`<db dir>/workspaces`，`--db` 默认时就是仓库根），
每个 cell 是目标仓库的完整拷贝（**内含嵌套 `.git`** → `git add -A` 会把它们当 gitlink 收进去并警
告）。`datasets/demo/workspaces/` 是数据集资产**必须保持被跟踪**，所以规则必须锚定根。
`datasets/local_claude_code/` 是 `collect` 产物（含绝对路径 + 真实会话文本），也忽略。不忽略
`datasets/mine/` 之类的任意目录 —— 那是猜命名，每加一个 `--out` 就补一行。

## 已知缺口（勿当已完成）

- **容器后端整体不可达**：`internal/sandbox/docker.go` **无调用方、无测试**；runner 里 `Backend`
  写死 local，CLI/数据集都无字段。保留是刻意的（真实仓库评测依赖它），注释已陈述事实并指向二选一：
  接上并补 e2e，或删掉。`DockerConfig.Network` **没有默认值** → 空值 = 不传 = docker 默认（联网）。
- **`sandbox.Baseline.RestorePaths` 写了但没人用**（无调用方/测试/字段）：它正是「判分前还原测试
  文件」的原语，**比「没有」更危险**（看起来做完了）。要完成是接线，不是重新实现。
- **跑过的配置没落库**：`Attempt.CommandLine` 无消费方，`runs` 表只有 agent_kind/model/bin/backend，
  `config_json` 只有 `matrix` 会写 → **报告无法证明「skill 是开着的」**。修法是接线不是重写。
- `llm_judge` 不在 e2e 覆盖内（e2e 必须离线）。

## 真实仓库评测

设计文档 `docs/real-repo-eval.md`（含 §1 现状表）—— **细节以该文档为准**。结论：链路通了，判分语义
浅。P0：① pytest 无用例级期望（缺 FAIL_TO_PASS/PASS_TO_PASS 清单 + JUnit XML 逐用例解析）；②
`restore_paths` 只做了一半；③ 无 `oracle` 字段/任务自检 → `resolve_rate` 不可解释。已做对：
`workspace.git` 默认**剥离上游 `.git` 再重建 baseline**（防 agent 从历史读修复），`setup` 后自动
re-baseline。瓶颈：每 cell clone 一次、无 mirror 缓存。最大风险不是跑不起来，而是**答案泄漏**
（prompt 带 issue/PR 号）、**作弊空间**（改测试/conftest）、**flaky 测试被当成 agent 失败**。
