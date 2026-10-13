# agenteval — 项目长期记忆

> 只记「改代码前必须知道的约束」。实测数据、案例、复盘在 `.workbuddy/memory/2026-*.md`。

## 是什么

Go 的 agent 评测框架，`/Users/skyitachi/lab/agent_eval`，module `github.com/skyitachi/agenteval`。
数据集 → 可插拔 grader → SQLite → 自包含 HTML 报告。只有 Claude Code 一种 agent（`mock` 是测试替身），
model 自由指定（透传 CLI）。七条子命令（`cmd/aeval/main.go`）：`collect`/`validate`/`run`/`matrix`/
`report`/`list`/`version`。回答「做得怎么样 / 差异归因给谁 / 真实会话花了多少」，**拒绝**回答「这个
agent 好不好」。退出码 `0/1/2/130`；**有任务失败 ≠ 命令失败**。

## 三轴归因（OFAT）

- harness = 实现+配置；`model`、`skill` 各一轴。一次 sweep 只动一条轴，**加载期拒绝**（不是警告）。
  所有权靠**反射读 `agent.Spec` 的 yaml tag** 推导（`Axis.Owned()`）——手写会漏字段，
  `yaml.Marshal(零值)` 会因 `omitempty` 退化成空集。model={model,fallback_model,effort}，
  skill={plugin_dirs}，**其余全归 harness**（新字段默认落 harness，不会变成第二个自由轴）。
- 每条 level 必须记录**改了哪些键**（`Changed` → `config_json`）：缺了它，读者分不清「这个 level 没改
  东西」和「差异没被记录」。
- 分离度用**精确符号检验**（log 空间求和，`0.5^n` 在 n=1074 下溢）；**非平局配对 < 6 对时无论多偏都
  到不了 p<0.05**（写进每份报告 caveat）。方向由 **win/loss 分裂**定，不由均值。
- 只警告**值得说**的：控制 level 一个字都没改是本职，警告它是噪音。

## claude 的 skill 闸门（2.1.296 实测，帮助文本误导过我）

- `--bare` **和** `--disable-slash-commands` **各自独立**都能抹掉 skill（后者连 `--plugin-dir` 注入的
  也抹）。要 skill 必须两个都关（claude **不接受** `--disable-slash-commands=false`，aeval 靠**不传**
  实现）。`--strict-mcp-config` 不碰 skill，只清用户 MCP。clean-room 默认
  `{Bare:true, StrictMCPConfig:true, DisableSlashCommands:true}` = **什么都不加载**。
- **声明 ≠ 加载**：`Spec.SkillsActive()` = 有 `plugin_dirs` **且** `!Bare && !DisableSlashCommands`；
  `SkillsReason()` 说清是哪个 flag 抹的；`loadedSkills()` 只返回活下来的目录。报告 chip：
  `none declared` / `N plugin dir(s)` / `N plugin dir(s), not loaded (--bare…)`。
- `--plugin-dir` 只吃**插件**目录（`.claude-plugin/plugin.json` + 同级 `skills/`），给裸 skill 目录
  **静默无效**；包成插件后 skill 名变 `<plugin>:<skill>`。**一个 plugin dir 带的是它 `skills/` 下全部
  skill，flag 层无法只选其中一个** → 要比单个 skill 得造只含它的插件目录（软链即可）。
- **零成本探针**：`claude -p --output-format stream-json --verbose --model <不存在的模型>` ——
  `system/init` 在 API 调用**之前**发出，可白拿 `tools`/`slash_commands`/`mcp_servers`，随后 400。
- 用户本机 `bare=false` 时 `~/.claude/skills/`（`edu-analytic-geometry`/`edu-solid-geometry`）与插件
  `~/lab/edulab` 会一起进来；自己的 hooks / settings.json 的 env / auto-memory 也回来。

## collect

从 Claude Code 转录生成**普通数据集**（prompt + cwd + baseline，**没有 grader** —— 转录没有 ground
truth，判失败是造判决）。**墙钟 ≠ 忙碌时间**（实测 4416s vs 533s）→ 用 `Baseline.BusyMs`。按
`message.id` 去重（176 条 assistant 记录 / 63 个 id，不去重放大 3 倍）。采集器自己的疑点走
`Baseline.Notes`。manifest 头按来源写，别硬编码。

- **`--out` 已有数据集默认追加**，`--overwrite` 才替换（`PlanAppend`/`AppendDataset`，dry-run 预览）。
  去重键是 **`recordingKey` = `source/session_id#prompt_index`**，不是 id；id 被占用则改后缀 `-2`。
  既有 `tasks.jsonl`/`dataset.yaml` **逐字节保留**（鼓励手改），内联 `tasks:` 的 manifest 拒绝追加。
- **两个来源**：`--source claude`（默认，`~/.claude/projects`）与 `--source cursor`（`state.vscdb` 的
  `cursorDiskKV`，**不是** `composerHeaders`）。`--cursor-db` 只配 cursor、`--root` 只配 claude，给错
  直接 usage 报错。`--list` 先看有什么再收集。
- **`SplitSession` ≠「整段会话」**：任务是「第一个 prompt + 回答它的那些轮」；只有切片 == 整段会话
  （单 prompt 会话）时才用会话级 `Usage/BusyMs/DurationMs/CostUSD`，否则按切片求和并附「per-turn
  baseline inside a longer session」注记。**e2e 里别假设「基线 = 会话总量」**，除非夹具只有一个 prompt。
- **默认 `SplitSession` 会丢掉多 prompt 会话的后几个 prompt**（设计如此：整段轨迹在回答这个任务没问的
  问题，拿它比会让回放永远显得更便宜），但**必须说出来**：baseline 注记（`SplitSession &&
  len(s.Prompts)>1`）+ CLI `warnDroppedPrompts`。实例：cursor 会话 `1f2114cd` 80 msgs / 3 prompts →
  默认 1 个任务，`--split turn` 3 个。警告口径：按 metadata `session_id` 分组、比 `session_prompts` 与
  **实收 `items`（不是 `plan.Added`）**，否则每次重跑误报。
  **推论（2026-10-13）**：默认切片下**文件写入也会丢**——同一会话默认采集 0 个 Edit，`--split turn`
  才有 3 个。要重放文件改动必须配 `--split turn`。
- **`session`/`prompt_index` 是一等任务字段**（`dataset.TaskSpec`，在 `Baseline` 之后），**不走
  `metadata`**（metadata 契约上「引擎从不解释」，而这两个引擎必须读）。手写任务两个都不写 = 自成一条链，
  这正是「不带 flag 时数据集行为不变」的机制。

### `--chain-sessions`：同一会话的轮次串成一条链

`--split turn` + 默认规划 = 每条 prompt 各自一个空沙箱；对「人在一直追问」的录制是错的。

- **规划**：按 `{chain, sample}` 分组（`chainKey` = 开链式且有 `session` 时取 `pathName(session)`，否则
  `pathName(task.ID)`），保留**首次出现的组序**，组内 `sortChain`，标 `first`/`last`；目录
  `root/<runID>/<chain>/s<sample>`。`sortChain` 只在**每一个** cell 都有 `PromptIndex` 时排序，否则
  **退回数据集自身顺序**（半个键不是键）。
- **物化/清理各一次**：只有 `first` 物化，只有 `last` 删。**物化失败污染整条链**（`broken[chain]`）：
  后面 cell 记 `fail(...)` 而**不跑**，否则会写出「agent 答得不好」的行而真相是工作区没建起来。
- **`Config.Validate()` 是独立纯方法**（Run 和 CLI 都调）：链式 + `--concurrency > 1` → 报错；CLI 调它
  才能把 flag 错误报成 **usage error（exit 2）**。**`--resume` 拒绝半条链**（`chainsIntact`，点名链名与
  `N of its M`）。
- **`pathName` 是安全边界**：白名单 `[A-Za-z0-9._-]`，其余换 `-`，trim `-.`，空则 `task`。
- **披露三件**：`runs.chain_sessions`（v5）→ 报告头部 chip → `comparisonWarnings` 里「链式的与普通的
  不是同一种测量」。
- **变异验证 14 项全部确认变红**（丢 `sortChain`／`chainKey` 忽略 session／`pathName` 用原始 id／
  `Validate` 恒 nil／`first` 恒 false／`last` 恒 true／`chainsIntact` 永不拒绝／删 baseline 注记／删
  `warnDroppedPrompts` 调用／`boolInt(r.ChainSessions)` 恒 false／`scanRun` 恒 false／`load.go` 恒
  false／`comparisonWarnings` 链式分支删掉／app.js chip 条件改 `false`——最后这条**必须跑 Chrome 渲染
  测试**，静态 grep HTML 只证明源码被装进去）。

### `--workspace-cwd`：把任务绑回录制目录

默认回放是**空工作区**（转录没记下检出过哪些文件）→ 空工作区带注记点名 `s.Cwd`，`collect` 也打同样的
话到 stderr。`--workspace-cwd` 才真的绑，**只有一条出路**：目录是 git 仓库**且有提交** →
`{type:git, repo:cwd, ref:s.GitBranch}`（`output.type` 随之从 `stdout` 变 `patch`）；**不是仓库/没有提交
→ 报错**（点名目录 + 「a replay would start from the answer」+ 建议 `--workspace-repo`）；目录不存在/
不是目录 → 报错。**没有任何东西被拷进数据集**（`bindCwd` 注释即是理由：目录的当下状态在会话之后，拷
进去等于把答案交给回放）——**早期那版 `{type:files, assets:…}` + `snapshotTree` 快照的分支已删除**；
`files` 类型现在**只**由 `--assets` 或手写 `workspace.assets` 产生。与 `--workspace-repo`/`--assets`
**互斥**（usage 层拒绝）。
路径基准的不对称：`--assets` 经 CLI 传 → `absFlagPath` **绝对化后写进 tasks.jsonl**（数据集不可移植），
手写的 `workspace.assets` 相对**数据集文档**解析（`materializeFiles`）。run 期把 assets 目录**拷进每个
cell 的根**（`assets_target` 可改落点），随后一律 `InitBaseline` 建单提交基线。

**空仓库陷阱**：对没有提交的仓库 `git clone` **成功但什么都不带** → 静默得到空工作区。所以
`materializeGit` checkout 后跑 `rev-parse --verify HEAD`，失败**拒绝落地**并说清是「没有提交」还是
「ref 不存在」；探针 `hasCommit` 同法。

**绑定只在 collect 期决定，run 期没有 flag**：`cwd` 平时只落在 `metadata.cwd`（引擎从不解释）与
`Baseline.Cwd`；run 期只读数据集的 `workspace`。把已采集的空工作区任务改成绑定形式，只能手改
`tasks.jsonl` 或重新 collect —— 而**往已有 `--out` 重新 collect 会被 `recordingKey` 判重复跳过**（实测
`appended 0, skipped 1`），必须 `--overwrite`。`--workspace-cwd` 绑的是**分支**不是提交（transcript 的
`git_head` 为空），CLI 会明知地打 note「that is the branch as it stands now」；cell 里的 `.git` 是**
新建的单提交仓库**（upstream 历史被剥离）。

### 「还原现场」的可用素材（2026-10-13 实测，多数未接入）

「现场」= **起点**（会话开始时 agent 看到什么）+ **过程**（改了什么）+ **环境**。现在 aeval 只把起点
记成 cwd+branch，过程只记 `EditedFiles`，环境零记录。素材四类（本机 80 个转录实测）：

1. **`session_context.gitStatus` attachment**（转录内）：会话开始的 git 现场 —— branch、`Recent
   commits`、**脏文件清单**（`M path`）。**0 引用**。**哪些文件脏**可得，**内容**不可得（只有状态码）。
2. **Write/Edit/MultiEdit/NotebookEdit 的完整 input**（转录内）：`file_path` + `content` /
   `old_string`+`new_string`，**无截断**，且 **aeval 已经把它写进 `BaselineToolCall.Input`**
   （json.RawMessage）→「按序 apply 重建会话结束时的树」是**离线、零额外采集**能做到的。`old_string`
   是天然**校验锚点**：apply 不上 = 起点不是那个样（= 脏起点），是证据而非静默损坏。盲区：**Bash 的
   副作用**（`cat >`/`mv`/`rm`/包管理器）与 **Read 过的文件**（`tool_use.input` 只有调用参数，
   result 不在其中）。
3. **`~/.claude/file-history/<session-uuid>/<sha256(absPath)[:16]>@vN`**：**未文档化**的逐版完整快照。
   key = `sha256(绝对路径)[:16]`（反推验证过）、`@v1` = **编辑前原文**、逐版递增；实测三次 Edit 对应
   `@v1→@v4` 且与转录的 old/new **逐步吻合** → 第 2 类的**独立校验**。aeval 现在只读了
   `TrackedFileBackups` 的 **keys**，**values 指向的备份内容没读**。局限：本机只剩 1 个会话（有
   `.last-cleanup`），只有 Write/Edit 触发的文件有 → 只能当补充。
4. **`~/.claude/shell-snapshots/snapshot-<shell>-<ts>-<rand>.sh`**：会话开始注入的完整 shell 环境
   （227KB），时间戳与会话对得上。**0 引用**；另有 `environment` attachment 给
   cwd/isWorktree/isGitRepo/platform/shell。

**忠实度排序**：显式给予（`--assets` / `workspace.git` 钉 commit）> baseline 重放（覆盖 Write/Edit，
缺 Bash）> `--workspace-cwd` 绑 branch 现状 > 空工作区。推荐实现：`workspace: {type: replay}` + 报告
披露覆盖率（`replayed N of M edits; K bash calls not replayed`）。**未动手。**

## 设计约定（改代码前先读）

1. **报告是「数据库的函数」**：渲染层只读持久化状态（`report/model.go`+`load.go` 投影成视图模型，
   **借用** `runner.Summary`/`attrib.Attribution` 而不镜像）。e2e 断言运行期报告与从 db 重生**逐字节
   相同**（比对时省掉 `generated`）。
2. **XSS 靠编码器不变量**：`render.go` 的 `neutralize()` 把 `<` `>` `&` 重写成 `\u003c` 等，一次封死
   三种逃逸；客户端默认 `esc()`，只有 `raw()` 例外。
3. `html/template` 上下文敏感：`<style>` 用 `template.CSS`、`<script>` 用 `template.JS`；用
   `template.HTML` 会被当文本**重新转义**。
4. **`hidden` 必须由样式表显式兑现**：UA 的 `display:none` 被任何作者 `display` 覆盖
   （`.drawer{display:flex}` 直接作废它）→ 加 hidden 元素必须同步加 `.cls[hidden]{display:none}`；
   `TestHiddenElementsCanActuallyBeHidden` 守它。
5. `MaxInlineBytes`（默认 8 MiB）超了按优先级丢 detail（失败/出错留最久，skip 最先丢），降级记入
   `Warnings`；轨迹截断留**头尾 + gap marker**。
6. 数据集/matrix 文档**就地解码**，未知键是**错误**（拼错的 `plugin_dirs` 会产出「skill 没用」的
   sweep）。时长必须带单位。
7. 路径相对**声明它的文档所在目录**解析；经 CLI 传的（`--bin`/`--work-root`/`--session`/`--file`）
   相对**调用者 cwd**。「裸命令名 vs 路径」：值里没有 `/` → 当 PATH 上的命令原样保留，否则绝对化
   （唯一实现 `agent.ResolvePath`）。
8. **测量值不重试**：timeout/非零退出/error result 都是**数据**。`retries` 只管进程起不来，默认 0。
9. agent 默认 **clean room**（无 user hooks / auto-memory / 任务外 MCP / CLAUDE.md）。
10. **位置参数可写在 flag 之后**：Go 的 `flag` 遇第一个非 flag 就停；`positionals()` 从那个词起重新
    解析，**但必须在 `openStore` 之前调**——曾因顺序错让 `report --compare a b --db X` 的 `--db` 来得
    太晚，两个 run 都被报成「not found in **aeval.db**」。护栏
    `TestCLIComparesTwoRunsHoweverTheFlagsAreOrdered`。
11. **工具打印的命令必须能跑**：`Next:` 提示漏 `--db` 就是必然失败的命令；`printSummary` 只在 `--db`
    非默认时补它、**按用户输入原样回显**，`hintArg` 给带空格的值加引号。**找不到记录必须点名文件**：
    `store.NotFoundError{Kind,ID,Path}`。
12. **schema 版本：往后拒绝、往前升级**（更低 = **就地升级**，发生在**任何一次 open** 含 `report`）。
    `migrations[i]` = v(i+1)→v(i+2)，只追加；每步**必须能重跑**（`ADD COLUMN` 不幂等，在 DDL 与版本号
    之间被杀掉会报 "duplicate column name"）。
    - **v2** `attempts.baseline_output`：语义是**「录制里没留下答案」**（omitempty，客户端写一句话）。
    - **v3** `attempts.baseline_agent_name/agent_model/source/notes`：四列而非 JSON blob。**为什么非要
      落库**：基线是**输入**，渲染期**不读数据集**。空 =「没记录过来源」≠「录制没有 agent」。
    - **v4** `runs.agent_spec_json`（整份 `agent.Spec`）：标签列**识别不出「只差 skills」的 run**。
      空 =「没记录配置」；`ok=false` → `RunInfo.Skills=nil`（页面写 `not recorded`）。
    - **v5** `runs.chain_sessions`：attempt 行上**没有任何一列**能事后推出「这些 cell 是一条续写」。
    - **夹具构造法**：从今天的 schema **删掉新加的那些列**再改 `schema_meta` 版本号。**v1 夹具要连带
      v3 的四列与 v4/v5 的列一起删**。

## 「用的是哪个模型」（报告必须说得比标签多）

- **两个来源，两种主张**：`runs.agent_model` 是**命令行要什么**；trace 里 init 的 `model` 是 CLI
  **宣告**打算用什么，`result.modelUsage` 的**键**才是**真正计了费**的模型。`trace.Trace.Models()`
  **优先后者**、空则回落前者，并**排序**；空 key 不是模型。
- chip **永远画**，四态文案在 Go 里写：`not recorded` / `X (the stream named no model)` /
  `default (the CLI chose it): X`（最常见）/ `X, but the stream billed Y`（标红）。
- **别名不是替换**：`trace.SameModel` = 小写后**互相包含**（`--model sonnet` 会被自称
  `claude-sonnet-4-5-…` 的流回答）。两边都空 = **不是**同一模型。**每个格子各报**，头部 `Observed` 收
  全部；格子上的 `(asked for X)` 是找到**是哪一格**的唯一线索。
- **比较也要比实际值**：声明相同（或**都空**）而流里报的不同 → 点名「which model served the
  requests」；**声明不同时不打这句**。只有一侧流报了模型 → 另说「未知」并**点名沉默的那一侧**。
- **`mock` 答不了这个问题**：断言「页面上的模型 = 流里报的模型」的 e2e **必须走默认 `claude_code`
  路径**（`--bin <夹具>`，夹具只读 stdin、忽略 argv）。

## token 记账（最容易错的地方）

**一次 API 调用的用量 ≠ 一次 run 的用量。** `result` 的 `usage` 是**那一次调用**；**会话累计在
`modelUsage`**（**驼峰**！fixture 写成 `model_usage` → 空 map → 测试静默空转）。单 result 时两者**有时
恰好相等**，所以这个错能潜伏很久（报告 18,540 vs 真值 164,419，**少 89%**）。

- `trace.Trace.Usage()` **优先求和 `modelUsage`**，空则回落 `usage`；`Attempt.Usage()` 只许走它。
- **多个 result 事件是同一会话的累计快照**：按「总量最大」合并（幂等）；逐字段 last-wins 会让更小的
  快照抹掉更大的。但**描述答案的字段相反** → 取最后一个。
- `result.num_turns` **不是会话轮数**；`Trace.Turns()` 优先用流推导的 `TurnCount`（按 assistant
  message id 去重）。`trace_json` **落库不截断** → 历史行真值可事后重算，但**没有 backfill 命令**。
- **子代理造成两种不平衡**：`s.Usage`（含 sidechain）≠ `sumTurns(s.Turns)`（不含）；两侧都**没记录
  「跑过子代理」**这件事。
- 护栏：`internal/trace/testdata/multi_result_stream.jsonl` 是**真实击穿我们的那条流**。

## 数据集格式

`dataset.yaml`（`tasks_file`/内联 `tasks`、`defaults` 合并、`filter`）+ `tasks.jsonl`。`workspace`：
`files`（assets + 字面量）或 `git`（repo/ref/subdir），`setup` 在 agent 前跑。`output.type`：
`patch`/`files`/`stdout`/`none`。`graders[].weight` 是**指针**（区分未设=1 与显式 0=只跑不计分）；
`required:true` 判负整个 task。内建 grader：`patch`/`pytest`/`exact_match`/`llm_judge`/`command`。
指标两层：run 头部展示值；归因七个配对指标（pass_rate 为 `Primary`）各带 `LowerIsBetter`。
**timeouts 与 harness_errors 恒显（即使为 0）**。旧 34 指标注册表 + 自定义 DSL **已删**。

## 报告客户端（app.js，唯一没单测的一层）

1. 插进模板的值必须已转义或包 `raw()`；裸 `tpl` 结果再插值 → **二次转义**。**已犯四次，最近两次都在
   任务表格里**：`Tools` 列整格被转义（页面印出 `<span class="v fail" …>!` 字面量）与排序后的表头
   （`Task <span class="arrow">▲</span>`，**只有读者点过表头才出现**）。共同点：载荷全对、退出码 0、
   印出来的也不是 `[object `，既有护栏一条都不响。正确形状是**整格一个 `raw(tpl…)`**。定位法：
   `grep '+ raw('`。
2. **节点只能 append，不能插值**（`tpl` 会 `String()` 它）——抽屉六个标签页全画成
   `[object HTMLDivElement]`；读代码看不出来，必须真渲染。
3. `raw()` 自带 `toString()`。兜底：`tpl` 对节点/普通对象输出 `⚠ unrendered <Ctor>` 的 `.bad` 标记
   ——**错误必须不像数据**。
4. overview 的 `Final answer` 是**两栏**（this attempt / recorded (baseline)）；录制没留答案时写一句
   话，不留空框。
5. **chip 的通用规则：「已知为未知」也必须画出来**（`not recorded` 是一个断言，不是一个空格）。加
   chip 先问：这个值缺了的时候页面是不是就沉默了。

护栏：`tests/render_test.go`，**唯一需要浏览器的测试**（找不到 skip，`AGENTEVAL_CHROME` 可覆盖）。
探针注入 → **读运行头部**（`.card.runhead` 的 `textContent`）→ 点每个 tab → 读回 DOM。**为什么头部也
要读回来**：chip 是 JS 运行时画的，**在 HTML 里 grep 到那个字符串只证明源码被装进去了**。

## 测试与运行

- e2e 在 `tests/e2e_test.go`，用真实二进制/子进程/SQLite/HTML 驱动。覆盖率 attrib 91.8% / report
  89.6% / trace 87.5% / store 82.5% / session 79.7% / **runner 29.2%** ——编排行为主要靠 e2e。
- e2e 的 payload 结构**刻意不复用** `report.Data`，也**刻意不声明** `generated`；解 payload 找**第一个
  `;</script>`**。
- **pytest grader 需要带 pytest 的解释器**：`AGENTEVAL_PYTHON`。本机
  `~/.workbuddy/binaries/python/envs/aeval-pytest/bin/python`；`envs/default` **没有** pytest。跑法
  `AGENTEVAL_PYTHON=… go test ./...`（带上后 test 包 ~8s → ~60s）。`runCLI` **只在探测到带 pytest 时
  才导出**它。**任何断言「demo 三题全对」的 e2e 必须 `requirePytest(t)`**。
- fixture：`tests/testdata/agents/good.sh`/`bad.sh`/`chain.sh`；`examples/matrix-harness.yaml` 可离线跑。
- **无头 Chrome 里 CSS 动画永不推进**（`.drawer` 淡入停在 `opacity:.4`）→ **截图复核前先注入
  `.drawer{animation:none}`**。
- **沙箱内 `go test ./...` 必然红两条**：`TestReportDrawer*` 报 `Google Chrome: signal: trace/BPT
  trap`。**沙箱外** → `ok`。别去查代码。**推论：背景 shell 不继承这个放行** → 变异验证必须**前台**跑。
- **「红了」≠「被抓到了」**：变异验证要**按输出归因**——命中自己写的断言文案才算抓到。
- `--dump-dom` 输出**含注入脚本自身**，探针结果要 base64 回传，匹配字面量要运行时拼。
- **grep 空结果先怀疑表达式**：BSD grep 不支持 `\|`，ERE 也不支持 `\s`/`\d`/`\b`（**静默返回空**）。
- **变异测试要用「不破坏编译」的形状**：把 `return fold(a)==fold(b)` 改成 `return a == b` 会让 `fold`
  变成未使用 → build failed，看起来红了其实什么都没验；正确做法是让 `fold` 变恒等。删**字段赋值**
  安全，删**唯一使用点**会把变量变未使用。
- **e2e 的 SKIP 要当没跑**（缺 pytest / 缺浏览器都会 skip）；`go test ./...` 的 `ok` 不代表这两类执行过。

## gitignore 约定

根锚定的 `/workspaces/` 是默认落盘处（`<db dir>/workspaces`），每个 cell 是目标仓库的完整拷贝（**内含
嵌套 `.git`** → `git add -A` 会把它们当 gitlink 收进去并警告）。`datasets/demo/workspaces/` 是数据集
资产**必须保持被跟踪**，所以规则必须锚定根。`datasets/local_claude_code/` 是 `collect` 产物（含绝对
路径 + 真实会话文本），也忽略。不忽略 `datasets/mine/` 之类的任意目录——那是猜命名。

## 已知缺口（勿当已完成）

- **容器后端整体不可达**：`internal/sandbox/docker.go` **无调用方、无测试**；runner 里 `Backend` 写死
  local。保留是刻意的，注释已指向二选一：接上并补 e2e，或删掉。`DockerConfig.Network` **没有默认值**
  → 空值 = docker 默认（联网）。
- **`sandbox.Baseline.RestorePaths` 写了但没人用**：「判分前还原测试文件」的原语，**比「没有」更危险**
  （看起来做完了）。要完成是**接线**。
- **`Attempt.CommandLine` 仍无消费方**。`llm_judge` 不在 e2e 覆盖内（e2e 必须离线）。
- **`EditedFiles` 只在会话级切片输出**（`editedFiles()` 有 `len(turns) != len(s.Turns) → nil` 早退，
  实测 `--split turn` 每个任务都是 `edited_files: null`），且**不落报告**（`internal/report/` 与
  `cmd/` 里 0 引用）。文件信息只能从 tool calls 的 `file_path` 拿。
- **cursor 把「文件引用块」塞进 user bubble 正文**：`--split turn` 于是把机器注入的上下文当「prompt」
  的一部分（会话 `1f2114cd` 的 p0 正文里就带着一块）。**已报给用户、未修**——剥离规则写错会把真人写的
  内容也删掉。
- **链式只覆盖 `run`，`matrix` 还没有这条路**：要做是接线同一套 `plan`，不是重写。
- **`checkSkillLevel` 只查 `disable_slash_commands`，漏了 `bare`**（见下节）。

## skill 轴比对：已知的静默失效（2026-10-12 复现）

`matrix --dry-run` 对以下形状**全部 exit 0、plan 表里都写 `varies plugin_dirs`**：

| base 的 isolation | 实际发生 |
| --- | --- |
| 完全不写（→ 默认 `bare:true`） | plugin_dirs **inert**，plan 无任何警告 |
| `bare: true, disable_slash_commands: false` | 同上，**guard 不响** |
| `bare: false, disable_slash_commands: false` | 唯一真的加载 skill 的形状 |
| `disable_slash_commands: true` | **被拒绝**（`checkSkillLevel` 命中） |

- 根因：`checkSkillLevel`（`internal/attrib/plan.go`）判的是 `l.Agent.Isolation != nil &&
  l.Agent.Isolation.DisableSlashCommands`，**既没看 `Bare`、也没用 `EffectiveIsolation()`**，而 `Bare`
  的默认值恰恰是 true。于是「本轴看起来改了东西、实际什么都没加载」的 sweep 能一路跑到付钱。
- 报告侧是诚实的（chip 写 `1 plugin dir(s), not loaded (--bare removed them)`），但那是**跑完之后**；
  plan 阶段与 run 头都没有。**gap 是「plan 沉默」不是「报告撒谎」。**
- 修法：guard 改用 `EffectiveIsolation()` 并同时检查两个 flag。
