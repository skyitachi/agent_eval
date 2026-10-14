# collect：从转录到数据集（细节）

从转录生成**普通数据集**（prompt + cwd + baseline，**没有 grader**：转录没有 ground truth，判失败是造判决）。
**墙钟 ≠ 忙碌时间**（实测 4416s vs 533s）→ 用 `Baseline.BusyMs`。按 `message.id` 去重（176 条 assistant 记录
只 63 个 id，不去重放大 3 倍）。

## 来源

`--source claude`（默认，`~/.claude/projects`）/ `--source cursor`（`state.vscdb` 的 `cursorDiskKV`，**不是**
`composerHeaders` —— 那是不完整索引）。`--cursor-db` 只配 cursor、`--root` 只配 claude，给错直接 usage 报错。

## 挑选器：`--session` / `--cwd` / `--latest`

- 优先级 **session > cwd > latest**（`collectSources` 的 switch，**不是互斥报错**）。
- `--cwd <dir>` = **「这个工作目录录过哪些会话，全都要」**，**只选不绑**。把 cwd 按 Claude Code 的编码
  （分隔符 → `-`，`session.ProjectDir`）映射到 `<root>/<encoded>`，再列该目录所有 `.jsonl`。该编码**有损**
  （名字带 `-` 的目录与嵌套路径无法区分）→ 只用来**找**候选；所有 cwd/branch 事实都从转录正文读回、不从目录
  名推。cursor 侧对应 `CursorChatsForCwd`（比 `WorkspaceDir`，先 `filepath.Clean`）。
- cwd 先按**调用者目录**绝对化（`absFlagPath`），所以数据集里记的路径从哪跑都指同一个地方。
- **`--list` 时 `--cwd` 被静默忽略**（`cmdCollectList` 根本没有这个参数）。
- 绑定工作区是另一个 flag（`--workspace-cwd`），与挑选无关。

## 追加语义

`--out` 已有数据集**默认追加**，`--overwrite` 才替换（`PlanAppend`/`AppendDataset`，dry-run 预览）。去重键是
**`recordingKey` = `source/session_id#prompt_index`**，不是 id —— 于是「同会话换 `--split` 再收」会把已收录的
prompt 判重复跳过，报的 skipped 名字是**没写进过数据集的生成 id**。id 被**别的录制**占用则改后缀 `-2`。既有
`tasks.jsonl`/`dataset.yaml` **逐字节保留**（鼓励手改），内联 `tasks:` 的 manifest 拒绝追加。顺序：先 baseline
后 task 行，旧行解析失败即报错、不硬追加。

## `--split`：一个任务覆盖会话的多少

默认 **`SplitSession`** = 每会话一个任务，prompt 取**第一个**，baseline = **回答它的那段切片**（`turnSlice`
停在下一个 prompt 处）。**多 prompt 会话的后几个 prompt 根本没被收集** —— 这是设计（整段轨迹在回答这个任务没
问的问题，拿它对照会让回放显得更便宜），但**必须说出来**：baseline 注记（`buildItem` 里
`SplitSession && len(s.Prompts)>1`）+ CLI 的 `warnDroppedPrompts`。`SplitTurn` 才每 prompt 一个任务。实例：
cursor 会话 `1f2114cd` 80 msgs / 3 prompts → 默认 1 个任务（18 轮切片），`--split turn` 3 个（18/55/7 轮）。
**推论：e2e 里别假设「基线 = 会话总量」，除非那个夹具只有一个 prompt。**

**警告的口径**：按 metadata 的 `session_id` 分组、比 `session_prompts` 与实收数，**统计用 `items` 而非
`plan.Added`**（skip 掉的任务仍在数据集里，只数追加的会在每次重跑误报）。建议文案随 split 变（session →
`--split turn`；turn → `--min-prompt-runes`）。

**`session` 与 `prompt_index` 是一等任务字段**（`dataset.TaskSpec`，在 `Baseline` 之后），**不走
`metadata`**：`metadata` 契约上「引擎从不解释」，而这两个值引擎必须读（分组 + 排序）。两个 split 都写。手写任务
两个都可以不写 —— 没有 `session` = 自成一条链，这正是「不带 flag 时数据集行为不变」的机制。

采集器自己的疑点走 `Session.BaselineNotes` → `dataset.Baseline.Notes`（"no cost recorded" / "token total is
a lower bound" / "assistant turns are counted per bubble"）。manifest 头按来源写，别硬编码「Claude Code
sessions」。

## `--chain-sessions`：把同一会话的轮次串成一条链

`--split turn` + 默认规划 = 每条 prompt 各自一个空沙箱、按 `--concurrency` 排到的顺序跑。对「人在一直追问」的
录制这是错的：第二条 prompt 问的是第一条写下的文件，在新目录里回放等于在问一个从没存在过的文件。

- **规划**（`plan`）：先按 `{chain, sample}` 分组（`chainKey` = 开链式且有 `session` 时取 `pathName(session)`，
  否则取 `pathName(task.ID)`），保留**首次出现的组序**，组内 `sortChain`，再标 `first`/`last`。目录 =
  `root/<runID>/<chain>/s<sample>`。
- **同一会话一个工作区、按 `prompt_index` 排序**。`sortChain` 只在**每一个** cell 都有 `PromptIndex` 时才排序，
  否则**退回数据集自身顺序**（半个键不是键：按它排会把没有索引的散到「零落在哪」）。
- **物化/清理各一次**（`runCell`）：只有 `first` 物化，只有 `last` 删目录。链上后面的 cell 从前面留下的东西起
  步，**因为中间没人把目录放回去**。k 个样本各是一条独立链（样本 1 不从样本 0 的残留起步）。
- **物化失败要污染整条链**：`execute` 维护 `broken[chain]`，链上一旦有一个 cell 失败，后面所有 cell 记
  `fail(...)` 而**不跑**（否则会写出「agent 答得不好」的行，而真相是工作区没建起来）。
- **`Config.Validate()` 单独一个纯方法**（Run 和 CLI 都调）：链式 + `--concurrency > 1` → 报错。CLI 调它才能
  把 flag 错误报成 **usage error（exit 2）**，而不是「开跑之后失败」。
- **`--resume` 拒绝半条链**（`chainsIntact`，点名链名与 `N of its M`）：链式 cell 的前驱工作区已随它那个 cell
  消失，剩下的会从空目录起步；全录完的链可以整个跳过，全没录的可以整条跑，**只有分裂的那种不可测量**。
- **`pathName` 是安全边界**：session id 来自录制（实践上是 UUID），但任务文件是给人手改的，带分隔符的名字会把
  工作区放到 run 目录之外。白名单 `[A-Za-z0-9._-]`，其余换 `-`，再 trim `-.`，空则 `task`。
- **披露三件**：`runs.chain_sessions`（v5）→ 报告头部 chip（`runs.chips` 里 `k=` 旁边，`raw()`）→
  `comparisonWarnings` 里「链式的与普通的不是同一种测量」。
- 变异验证 14 项全部确认变红（丢 `sortChain` / `chainKey` 忽略 session / `pathName` 用原始 id / `Validate` 恒
  nil / `first` 恒 false / `last` 恒 true / `chainsIntact` 永不拒绝 / 删 baseline 注记 / 删
  `warnDroppedPrompts` 调用处 / `boolInt(r.ChainSessions)` 写死 false / `scanRun` 读回恒 false / `load.go`
  的 `ChainSessions` 写死 false / 删 `comparisonWarnings` 链式分支 / app.js 的 chip 条件改 `false`）；**最后
  那条要跑 Chrome 渲染测试才看得到**（静态 grep HTML 只证明源码被装进去了）。

## `--workspace-cwd`：把任务绑回录制目录

默认回放是**空工作区**（转录没记下检出过哪些文件）→ 空工作区带一条注记点名 `s.Cwd`，`collect` 也把同样的话打
到 stderr（`warnUnboundReplays`）。`--workspace-cwd` 才真的绑：

- 目录是 git 仓库**且有提交** → `{type:git, repo:cwd, ref:s.GitBranch}` + 注记「记了分支、没记提交」。
- 否则（非仓库 / 仓库无提交）→ `{type:files, assets:assets/<sanitized id>}` + **snapshot**：其实是**录制结束
  后**的目录树（`snapshotTree`，跳过任何 `.git`，上限 `maxSnapshotBytes=256MiB`），注记写「as it stands
  now」。
- 目录不存在 → **报错并点名该目录**（不是悄悄给空任务）。
- 与 `--workspace-repo`/`--assets` **互斥**（usage 层拒绝）。

**起点会漂移**：转录只记**分支名**、不记 commit，而 collect 是事后跑的 → 现场任何快照都是**后态**。
**空仓库陷阱**：对没有提交的仓库 `git clone` **成功但什么都不带** → 静默得到空工作区。所以 `materializeGit`
在 checkout 后跑 `rev-parse --verify HEAD`，失败就**拒绝落地**并说清是「没有提交」还是「ref 不存在」两类
（后者不怪仓库）。探针 `hasCommit` 同理（带 sanitized env）。

## 本地 baseline 数据集：起点怎么锚

`Baseline.StartedAt` 已落库 → `git log --before='<started_at>' -n1 --format=%H` 反推出**会话开始前那一个
commit**（会话中途自己 commit 不影响）。这条今天是**手工**的，建议做成 `collect --record-start`（顺手
`git status --porcelain` 检测会话前 dirty 并写进 notes）—— 两条缺口一起堵：起点漂移、以及**今天 `bindCwd`
完全不检测 dirty**（读者不知道丢了一块起点，这是最该补的静默缺口）。参考设计见
`notes/environment-freezing.md`。
