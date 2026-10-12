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

**`--out` 已有数据集默认追加**，`--overwrite` 才替换（`PlanAppend`/`AppendDataset`，dry-run 预览）。
去重键是 **`recordingKey` = `source/session_id#prompt_index`**，不是 id —— 于是「同会话换 `--split`
再收」会把已收录的 prompt 判重复跳过，报的 skipped 名字是**没写进过数据集的生成 id**。
id 被**别的录制**占用则改后缀 `-2`。既有 `tasks.jsonl`/`dataset.yaml` **逐字节保留**（鼓励手改），
内联 `tasks:` 的 manifest 拒绝追加。顺序：先 baseline 后 task 行，旧行解析失败即报错不硬追加。

**两个来源**：`--source claude`（默认，`~/.claude/projects`）与 `--source cursor`
（`state.vscdb` 的 `cursorDiskKV`，**不是** `composerHeaders` —— 它是不完整索引）。`--cursor-db`
只配 cursor、`--root` 只配 claude，给错直接 usage 报错。`--list` 先看有什么再收集。

- **`SplitSession` ≠ 「整段会话」**。任务是「第一个 prompt + 回答它的那些轮」
  （`turnSlice(s, 0)`）；只有**切片 == 整段会话**（也就是单 prompt 会话）时才用会话级
  `Usage/BusyMs/DurationMs/CostUSD`，否则按切片求和并附「per-turn baseline inside a longer
  session」注记（多 prompt 会话的整段轨迹在回答一个任务没问的问题，拿它比会让回放永远显得更便宜）。
  **推论：e2e 里别假设「基线 = 会话总量」，除非那个夹具只有一个 prompt。**
- 采集器自己的疑点走 `Session.BaselineNotes` → `dataset.Baseline.Notes`（"no cost recorded" /
  "token total is a lower bound" / "assistant turns are counted per bubble"）。
- manifest 头按来源写，别硬编码「Claude Code sessions」。

### 一个任务覆盖会话的多少（`--split`）

默认 **`SplitSession`**：每会话一个任务，prompt = **第一个** prompt，baseline = **回答它的那段切片**
（`turnSlice` 停在下一个 prompt 处）。**多 prompt 会话的后几个 prompt 根本没被收集** —— 这是设计
（整段轨迹在回答这个任务没问的问题，拿它对照会让回放显得更便宜），但**必须说出来**：baseline 注记
（`buildItem` 里 `SplitSession && len(s.Prompts)>1`）+ CLI 的 `warnDroppedPrompts`。`SplitTurn` 才每
prompt 一个任务。实例：cursor 会话 `1f2114cd` 80 msgs / 3 prompts → 默认 1 个任务（18 轮切片），
`--split turn` 3 个（18/55/7 轮）。

**警告的口径**：按 metadata 的 `session_id` 分组、比 `session_prompts` 与实收数，**统计用 `items` 而非
`plan.Added`**（skip 掉的任务仍在数据集里，只数追加的会在每次重跑误报）。建议文案随 split 变
（session → `--split turn`；turn → `--min-prompt-runes`）。

**`session` 与 `prompt_index` 是一等任务字段**（`dataset.TaskSpec`，在 `Baseline` 之后），**不走
`metadata`**：`metadata` 契约上「引擎从不解释」，而这两个值引擎必须读（分组 + 排序）。两个 split 都写。
手写任务两个都可以不写 —— 没有 `session` = 自成一条链，这正是「不带 flag 时数据集行为不变」的机制。

### `--chain-sessions`：把同一会话的轮次串成一条链

`--split turn` + 默认规划 = 每条 prompt 各自一个空沙箱、按 `--concurrency` 排到的顺序跑。对「人在
一直追问」的录制这是错的：第二条 prompt 问的是第一条写下的文件，在新目录里回放等于在问一个从没
存在过的文件。`--chain-sessions` 让采集来的 `session`/`prompt_index` 真正发挥作用：

- **规划**（`plan`）：先按 `{chain, sample}` 分组（`chainKey` = 开链式且有 `session` 时取
  `pathName(session)`，否则取 `pathName(task.ID)`），保留**首次出现的组序**，组内 `sortChain`，再标
  `first`/`last`。目录 = `root/<runID>/<chain>/s<sample>`。
- **同一会话一个工作区、按 `prompt_index` 排序**。`sortChain` 只在**每一个** cell 都有 `PromptIndex`
  时才排序，否则**退回数据集自身顺序**（半个键不是键：按它排会把没有索引的散到「零落在哪」）。
- **物化/清理各一次**（`runCell`）：只有 `first` 物化，只有 `last` 删目录。链上后面的 cell 从前面留
  下的东西起步，**因为中间没人把目录放回去**。k 个样本各是一条独立链（样本 1 不从样本 0 的残留起步）。
- **物化失败要污染整条链**：`execute` 维护 `broken[chain]`，链上一旦有一个 cell 失败，后面所有 cell
  记 `fail(...)` 而**不跑**（否则会写出「agent 答得不好」的行，而真相是工作区没建起来）。
- **`Config.Validate()` 单独一个纯方法**（Run 和 CLI 都调）：链式 + `--concurrency > 1` → 报错。CLI 调
  它才能把 flag 错误报成 **usage error（exit 2）**，而不是「开跑之后失败」。
- **`--resume` 拒绝半条链**（`chainsIntact`，点名链名与 `N of its M`）：链式 cell 的前驱工作区已随它
  那个 cell 消失，剩下的会从空目录起步；全录完的链可以整个跳过，全没录的可以整条跑，**只有分裂的那种
  不可测量**。
- **`pathName` 是安全边界**：session id 来自录制（实践上是 UUID），但任务文件是给人手改的，带分隔符
  的名字会把工作区放到 run 目录之外。白名单 `[A-Za-z0-9._-]`，其余换 `-`，再 trim `-.`，空则 `task`。
- **披露三件**：`runs.chain_sessions`（v5）→ 报告头部 chip（`runs.chips` 里 `k=` 旁边，`raw()`）→
  `comparisonWarnings` 里「链式的与普通的不是同一种测量」。

**变异验证（14 项，全部确认变红）**：丢 `sortChain`；`chainKey` 忽略 session；`pathName` 用原始 id；
`Validate` 恒 nil；`first` 恒 false；`last` 恒 true；`chainsIntact` 永不拒绝；删 baseline 注记；删
`warnDroppedPrompts` 调用处；`boolInt(r.ChainSessions)` 写死 false；`scanRun` 读回恒 false；
`load.go` 的 `ChainSessions` 写死 false；`comparisonWarnings` 的链式分支删掉；app.js 的 chip 条件改
`false`（这条要跑 Chrome 渲染测试才看得到，静态 grep HTML 只证明源码被装进去了）。

### `--workspace-cwd`：把任务绑回录制目录

默认回放是**空工作区**（转录没记下检出过哪些文件）→ 空工作区带一条注记点名 `s.Cwd`，`collect` 也
把同样的话打到 stderr。`--workspace-cwd` 才真的绑：

- 目录
  是 git 仓库**且有提交** → `{type:git, repo:cwd, ref:s.GitBranch}` + 注记「记了分支、没记提交」。
- 否则（非仓库 / 仓库无提交）→ `{type:files, assets:assets/<sanitized id>}` + **snapshot**：
  其实是**录制结束后**的目录树（`snapshotTree`，跳过任何 `.git`，上限 `maxSnapshotBytes=256MiB`），
  注记写「as it stands now」。
- 目录不存在 → **报错并点名该目录**（不是悄悄给空任务）。
- `--workspace-cwd` 与 `--workspace-repo`/`--assets` **互斥**（usage 层拒绝）。

**空仓库陷阱**：对没有提交的仓库 `git clone` **成功但什么都不带** → 静默得到空工作区。所以
`materializeGit` 在 checkout 后跑 `rev-parse --verify HEAD`，失败就**拒绝落地**并说清是「没有提交」
还是「ref 不存在」两类（后者不怪仓库）。探针 `hasCommit` 同样用 `rev-parse --verify HEAD`（带
sanitized env）。

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
    把 `-o x` 当位置参数并报「got 4」；`positionals()` 从那个词起重新解析。**但重新解析必须在
    `openStore` 之前做**：曾经 `cmdReport` 先 `openStore(&common)`、到 `--compare` 分支里才
    `positionals()`，于是 `report --compare a b --db X` 的 `--db X` **来得太晚** —— store 已经开在默认
    路径上，两个 run 都被报成「not found in **aeval.db**」（错误信息点名的文件正是线索）。
    `--db` 写在 `--compare` 前面的形状一直是好的，而**本文件里所有既有 e2e 都是那个形状**，所以没人
    发现。现在 `trailing, code := positionals(fs, stderr)` 在 `openStore` 之前调一次，分支复用结果。
    护栏 `TestCLIComparesTwoRunsHoweverTheFlagsAreOrdered`（两种顺序渲染出的载荷必须逐字段相同；
    真的找错库时错误必须点名**调用者传的那个路径**而不是 `aeval.db`）。
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
    **v3 = `attempts.baseline_agent_name/agent_model/source/notes`**：录制方的身份与它自己声明的缺口。
    四列而非一个 JSON blob（各自独立被读，blob 会把「空还是坏」的问题推给每个读者）。**为什么非要落库**：
    基线是**输入**，live beside dataset，渲染期**不读数据集** —— 不落库就说不出一句「录制来自 cursor /
    本次是 claude-code」，跨 agent 回放会被读成同口径比较。空 = **「没记录过来源」≠「录制没有 agent」**。
    单测构造仍走「从今天的 schema 删掉新加的四列 + version=2」；**v1 夹具要连 v3 的四列一起删**，
    只删 `baseline_output` 得到的是「v2 再删一列」，不是 v1。
    **v4 = `runs.agent_spec_json`**（整份 `agent.Spec` 的 JSON）。落它的理由：四个标签列（agent_kind/
    model/bin/backend）**识别不出「只差 skills」的 run**，而 skill 集恰恰是 `plugin_dirs` + 那两个决定
    目录生死的 isolation flag。空 = **「没记录配置」**（v1–v3 的行），与「配置里什么都没声明」分开表述；
    `runs.AgentSpec()` 返回 `(spec, ok)`，`ok=false` 时报告 `RunInfo.Skills = nil`（页面写 `not recorded`）。
    升级路径 `upgradeV3ToV4` 就是一句 `ADD COLUMN ... DEFAULT ''`；store 夹具 drop 掉这一列再
    `UPDATE schema_meta SET value='3'`；**v1 夹具要连带把 v4 这列也削掉**（否则是「v3 再降一版」）。
    **v5 = `runs.chain_sessions`**（这次 run 有没有把同一会话的任务串成一条链）。**为什么非要落库**：
    attempt 行上**没有任何一列**能事后推出「这些 cell 是一条续写而不是独立采样」—— 链上第二个 cell
    读到第一个 cell 留下的文件，但行只记结果。空/false = **v1–v4 的普通 run**，语义正确不是缺口。
    `upgradeV4ToV5` 同理一句 `ADD COLUMN ... DEFAULT 0`；夹具把三份 v1/v2/v3 都补 drop 这一列。

## 「用的是哪个模型」（报告必须说得比标签多）

- **两个来源，两种主张**：`runs.agent_model` 是**命令行要什么**（`--model`，可选 → **没传就是空**）；
  `attempts.trace_json` 里 init 的 `model` 是 CLI **宣告**打算用什么（在任何请求之前），
  `result.modelUsage` 的**键**才是**真正计了费**的模型。`trace.Trace.Models()` **优先后者**、空则回落
  前者，并**排序**（报告必须是记录的函数）；空 key 不是模型。
- **这不需要新列**：trace 本来就按 attempt 落库 → 在这之前录下的 run 也答得出来。`report/load.go`
  的 `tasks()` **本来就把 trace 解成 `trace.Trace` 了**，只是以前只取 tools/events。
- **旧缺口是「空标签的沉默」**：chip 以前只在 `agent_model` 非空时画 → 没传 `--model` 的 run 头部
  **根本没有模型**，读者分不清「没指定」「用了 CLI 默认」「没人记录」。现在**永远画**，四态文案在
  Go 里写（照 `Skills.Text` 的先例）：`not recorded` / `X (the stream named no model)` /
  `default (the CLI chose it): X`（**真实 run 最常见的一档**）/ `X, but the stream billed Y`（标红）。
- **别名不是替换**：`trace.SameModel` = 小写后**互相包含**。`--model sonnet` 会被自称
  `claude-sonnet-4-5-20250929` 的流回答，直接比字符串会让**每次普通 run 都报警**，而每次都报的警告
  = 没人读。两边都空 = **不是**同一模型（两个方向都是编的）。
- **每个格子各报**（`Task.Model`，文案永远非空）：一次 run 未必只有一个模型，中途回落只出现在某条
  格的流里。头部的 `Observed` **收全部格子**的名字 —— 只取第一个有答案的格子会把「跑一半换了模型」
  恰好藏起来。格子上的 `(asked for X)` 是读者找到**是哪一格**的唯一线索。
- **比较也要比实际值**：声明的模型相同（或**都空**）而流里报的不同 → 点名「which model served the
  requests」。**声明不同时不打这句**（同一列两句 = 两句都不被读）。只有一侧流报了模型、且两边都
  没声明 → 另说一句「未知」并**点名沉默的那一侧**（照 skills 先例：读者对沉默的错猜正是「那侧什么
  都没声明」）。
- **`mock` 答不了这个问题**：`synthesizeStream` **自己合成流并照抄被告知的模型**（空则写 `"mock"`），
  夹具的 stdout 只被当答案文本。所以断言「页面上的模型 = 流里报的模型」的 e2e **必须走默认
  `claude_code` 路径**（`--bin <夹具>`，夹具只读 stdin、忽略 argv）。

## skill 到底有没有开（报告必须能证明）

- **声明 ≠ 加载**：`Spec.SkillsActive()` = 有 `plugin_dirs` **且** `!Bare && !DisableSlashCommands`
  （`EffectiveIsolation()`：nil isolation 回落 `DefaultIsolation()`，而默认是
  `{Bare:true, StrictMCPConfig:true, DisableSlashCommands:true}` → **普通 run 什么都不加载**）。
  所以「声明了一个目录、flags 没动」= inert，报告要写 `N plugin dir(s), not loaded (--bare …)`。
- `loadedSkills()` 只返回**活下来**的目录：两个 run 的声明只差 inert 项 → 是同一份 agent 配置，
  报成差异反而把唯一那个真差异淹掉。
- `comparisonWarnings` 同理点名 skills：一侧知道、另一侧不知道（老行）→ 报
  「run X recorded no agent configuration … so whether the two runs loaded the same skills is unknown」，
  **且要点名是哪一侧**（"one of these runs" 会让读者猜，而会猜错的那个方向正是「沉默的那侧没加载」）。
  这条把旧记忆里「不为此打常驻警告」的盲区**关掉了**——不是打常驻警告，是**有差异才点名**。

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
   **已犯四次，最近两次都在任务表格里**：`Tools` 列（`int(n) + raw(badge)` → 整格被转义，页面印出
   `6<span class="v fail" title="2 tool error(s)">!</span>`）与**排序后的表头**（`arrow = tpl…` 没包
   `raw` → `Task <span class="arrow">▲</span>`，`sortKey` 初值是 null，所以**只有读者点过表头才会出现**）。
   共同点：**载荷全对、退出码 0、印出来的也不是 `[object `** —— 既有护栏一条都不响。正确形状是
   **整格一个 `raw(tpl…)`**（见 `toolCell`）。定位法：`grep '+ raw('`（曾经只有一处）。
2. **节点只能 append，不能插值**（`tpl` 会 `String()` 它）—— 抽屉六个标签页全因此画成
   `[object HTMLDivElement]`；读代码看不出来，必须真渲染。
3. `raw()` 自带 `toString()`，否则 `'a' + raw(x)` 得到 `[object Object]`（baseline 行犯过）。
4. 兜底：`tpl` 对节点/普通对象输出 `⚠ unrendered <Ctor>` 的 `.bad` 标记 —— **错误必须不像数据**。
5. overview 的 `Final answer` 是**两栏**（this attempt / recorded (baseline)）—— 数字说不出两次
   尝试是否得出同一结论。录制没留答案时写一句话，不留空框。

护栏：`tests/render_test.go`，**唯一需要浏览器的测试**（找不到 skip，`AGENTEVAL_CHROME` 可覆盖）。
探针注入 → **读运行头部**（`.card.runhead` 的 `textContent`，`out.header`）→ 点每个 tab → 读回 DOM，
断言字节数下限、无 `.bad`、无 `[object `、diff 有 `<span class="dl `、baseline 卡两个 delta 含录制
总量、两栏答案都在。**表格也要读回来**（`out.toolsCell` / `toolsBadges` / `tableMarkup`，
以及 `sortedHead` / `sortedMarkup` —— 表头要**点一下再读**，箭头只在排序后才存在）：
`TestReportTaskTableShowsValuesNotMarkup` 断言 `toolsCell == "2!"`、徽章是元素、表格与排序后的表头
里都没有字面标签。夹具 `tool-errors.sh` 是**全套唯一会调用工具的那个**（2 calls / 1 error）——
工具的计数列在没有工具调用的 cell 上只画 `-`，那个坏掉的格子永远不会被走到。本轮这处新增的
**六项变异**（工具格子 4 + 表头箭头 2）全部确认变红。

**为什么头部也要读回来**：chip 是 JS 运行时画的，**在 HTML 里 grep 到那个字符串只证明源码被装进去了**，
证明不了它被画出来 —— 这正是 `[object HTMLDivElement]` 当年潜伏的方式（文件/载荷/退出码全对、套件
全绿）。两条同形状的护栏：`TestReportHeaderSaysTheCellsWereChained`（链式徽章）与
`TestReportHeaderNamesTheModelTheStreamNamed`（**模型徽章**，跑一份不传 `--model` 的 run，断言头部含
`default (the CLI chose it): stand-in`）。删掉 app.js 里对应的那行 → 各自变红（已验证）。

**chip 的通用规则**：**「已知为未知」也必须画出来**。skills 与 model 两个 chip 都遵循它（`not recorded`
是一个断言，不是一个空格），而 `model` 以前只在标签非空时画 —— 于是没传 `--model` 的 run 头部
**什么模型都不说**。加 chip 时先问：这个值缺了的时候，页面是不是就变成了沉默。

## 测试与运行

- `go test ./...` 全过（13 包）。e2e 在 `tests/e2e_test.go`，用真实二进制/子进程/SQLite/HTML 驱动。
  覆盖率 attrib 91.8% / report 89.6% / trace 87.5% / store 82.5% / session 79.7% / **runner 29.2%**
  （链式规划 `chain_test.go` 把它从 8.9% 抬上来的）—— 编排行为仍主要靠 e2e，这是诚实的现状
  （编排器的正确层级就是 e2e）。
- **链式的可观察性只在 e2e 成立**：`tests/testdata/agents/chain.sh`（读 stdin 的 prompt，`*WRITE*`
  写 `handoff.txt` 并答 `wrote handoff.txt`，`*READ*` 答 `found: $(cat handoff.txt)` 或 `missing`）
  —— 「agent 看见了上一步的文件」是单元测试看不见的事。不链式 → 第二格答 `missing` 判负，链式 →
  `found: the-first-turn-wrote-this` 判通过。
- e2e 的 payload 结构**刻意不复用** `report.Data`（否则字段改名测试会点头），也**刻意不声明
  `generated`**。解 payload 找**第一个 `;</script>`**（后面紧跟内联 app.js）。
- **pytest grader 需要带 pytest 的解释器**：`AGENTEVAL_PYTHON`。managed python 3.13.12 与系统 python
  **都没有 pytest**。**本机专用 venv：`~/.workbuddy/binaries/python/envs/aeval-pytest/bin/python`**
  （pytest 9.1.1；早先记的 `envs/default` **已不含 pytest**，那个 env 装了 pillow/pymupdf/pyobjc，
  是 WorkBuddy 自己的工具环境，别往上装）。跑法：`AGENTEVAL_PYTHON=…/aeval-pytest/bin/python go test ./...`
  —— 带上之后 test 包从 ~8s 变 ~60s，说明 pytest grader 真的在判分。
  `runCLI` **只在探测到带 pytest 时才导出**它（导出空值 → 回落 python3 → 报「没有 pytest」被误读
  成 agent 挂了）。**推论：任何断言「demo 三题全对」的 e2e 必须 `requirePytest(t)`**
  （`TestCLIMatrixAttributesASweep` 曾漏，demo 2/3 靠 pytest 判分）。
- fixture：`tests/testdata/agents/good.sh`（正确补丁）/`bad.sh`（各任务错法不同）；
  `examples/matrix-harness.yaml` 是可离线跑的 sweep 示例。
- **无头 Chrome 里 CSS 动画永不推进**（`document.timeline` 走但动画 `currentTime` 恒 0），
  `.drawer` 的 160ms 淡入永远停在 `opacity:.4`，看起来像「半透明抽屉」—— 环境假象，**截图复核前
  先注入 `.drawer{animation:none}`**。
- **沙箱内 `go test ./...` 必然红两条**：`tests` 包的 `TestReportDrawer*` 报
  `Google Chrome: signal: trace/BPT trap`。根因是沙箱拒绝 Chrome 清理自己的临时 profile
  （`~/Library/Application Support/Google/Chrome-headless/scoped_dir*/…`、`code_sign_clone/…`）。
  **沙箱外**（`go test ./tests -run TestReportDrawer -count=1`）→ `ok`。别去查代码。
  **推论：后台 shell 不继承这个放行** —— 变异验证脚本一旦 `run_in_background` 跑，六项全部报
  `trace/BPT trap`，看起来像「全红」，其实一项都没被断言抓到。变异验证必须**前台**跑（放大 timeout）。
- **「红了」≠「被抓到了」**：变异验证要**按输出归因** —— 命中自己写的那句断言文案才算抓到，否则
  只能算环境红了。曾把「浏览器被 context 超时 kill」计成 catch 混进 4/4。脚本里做这层分类，
  别只看退出码。
- `--dump-dom` 输出**含注入脚本自身**，探针结果要 base64 回传，匹配字面量要运行时拼
  （`'[' + 'object '`），否则命中探针源码。
- **本机 grep 空结果先怀疑表达式**：BSD grep 不支持 `\|`，ERE 也不支持 `\s`/`\d`/`\b`（会**静默返回
  空**）。一律 `-E` 并避开这些转义。
- **变异测试（验断言真的会红）要用「不破坏编译」的形状**。曾把 `return fold(a)==fold(b)` 改成
  `return a == b`，`fold` 立刻成了未使用变量 → **build failed**，看起来「红了」其实是编译错，
  等于什么都没验。正确做法是让 `fold` 变恒等（`func(s string) string { return s }`）。
  同理：删 `AgentName: b.AgentName` 这种**字段赋值**是安全的变异，删**唯一使用点**会把变量变成未使用。
- **e2e 的 SKIP 要当没跑**（缺 pytest / 缺浏览器都会 skip）。`go test ./...` 的 `ok` 不代表
  这两类测试执行过；断言 e2e 覆盖时先看 `-v` 里的 `--- SKIP`。

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
- ~~**跑过的配置没落库**~~ **已接线（schema v4）**：`runs.agent_spec_json` 存整份 `agent.Spec`
  （runner 在 `withDefaults` 里把 nil isolation 填成 `DefaultIsolation()` 后 `json.Marshal(cfg.Agent)`
  落库）→ 报告 run 头部的 `skills` chip 与 `comparisonWarnings` 的点名。**旧盲区已关**：只差
  `plugin_dirs` 的两条 run 现在会被点名，而不是比成一样。`Attempt.CommandLine` **仍无消费方**（未变）。
- `llm_judge` 不在 e2e 覆盖内（e2e 必须离线）。
- **cursor 把「文件引用块」塞进 user bubble 的正文**：`--split turn` 于是把机器注入的上下文当成
  「prompt」的一部分（会话 `1f2114cd` 的 p0 正文里就带着一块机器写的引用）。**已报给用户、未修**——
  修法是识别并剥离这类注入块，但剥离规则一旦写错就会把真人写的内容也删掉，不该顺手做。
- **链式只覆盖 `run`，`matrix` 还没有这条路**：`--chain-sessions` 是 `run` 的 flag，sweep 出来的
  每个 level 仍按普通方式规划（每任务一个空沙箱）。要做的话是接线同一套 `plan`，不是重写。
- **带 pytest 的解释器**：本机 `~/.workbuddy/binaries/python/envs/aeval-pytest/bin/python`（pytest 9.1.1）
  **有**；`.../envs/default/bin/python` **没有**。全量套件用
  `AGENTEVAL_PYTHON=$HOME/.workbuddy/binaries/python/envs/aeval-pytest/bin/python go test ./...`。
  这个 env 会被 WorkBuddy 复用/清理，**缺了症状是 e2e 静默 SKIP 而不是 FAIL——别把 SKIP 当通过**。
- ~~`dataset.Baseline.Notes` 从不落库~~ **已接线（schema v3）**：`compareToBaseline` 带走
  `AgentName/AgentModel/Source/Notes` → `store.BaselineComparison` 四列 → `report.BaselineView` →
  app.js 的 `recordedBy()`/`crossAgentNote()`（录制自己的注记**替换**那行硬编码，不是并排——一卡两行
  caveat = 两行都不被读）。`report --compare` 也补了 `comparisonWarnings`：比 AgentKind/Model/Bin/
  Backend/DatasetHash，一处差异＝点名它是唯一变量，多处＝明说「没有哪个 delta 能归给单个因素」。
  **零差异时不打任何话**（沉默会被读成「两边测的是同一件事」，但常驻警告更糟）。
  **注意 `sameAgent` 要折叠命名**：capture 写 `claude-code`、runner 注册 `claude_code`，字面比会把
  每一次「采集的 Claude Code 基线被自己回放」都标成跨 agent。空值＝「没记录」不算「不同」。

## 真实仓库评测

设计文档 `docs/real-repo-eval.md`（含 §1 现状表）—— **细节以该文档为准**。结论：链路通了，判分语义
浅。P0：① pytest 无用例级期望（缺 FAIL_TO_PASS/PASS_TO_PASS 清单 + JUnit XML 逐用例解析）；②
`restore_paths` 只做了一半；③ 无 `oracle` 字段/任务自检 → `resolve_rate` 不可解释。已做对：
`workspace.git` 默认**剥离上游 `.git` 再重建 baseline**（防 agent 从历史读修复），`setup` 后自动
re-baseline。瓶颈：每 cell clone 一次、无 mirror 缓存。最大风险不是跑不起来，而是**答案泄漏**
（prompt 带 issue/PR 号）、**作弊空间**（改测试/conftest）、**flaky 测试被当成 agent 失败**。
