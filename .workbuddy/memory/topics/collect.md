# collect / 链式 / 工作区绑定（细节）

> 属于 `MEMORY.md` 的展开。改 `internal/session`、`internal/dataset`、`runner.plan` 前读本文件。

## collect 输出的数据集

从转录生成**普通数据集**（prompt + cwd + baseline，**没有 grader**——转录没有 ground truth，判失败即造
判决）。**墙钟 ≠ 忙碌时间**（实测 4416s vs 533s）→ 用 `Baseline.BusyMs`。按 `message.id` 去重（176 条
assistant 记录 / 63 个 id，不去重放大 3 倍）。采集器自己的疑点走 `Baseline.Notes`。manifest 头按来源写，
别硬编码。

- **`--out` 已有数据集默认追加**，`--overwrite` 才替换（`PlanAppend`/`AppendDataset`，dry-run 预览）。
  去重键是 **`recordingKey` = `source/session_id#prompt_index`**，不是 id；id 被占用则改后缀 `-2`。
  既有 `tasks.jsonl`/`dataset.yaml` **逐字节保留**（鼓励手改）；内联 `tasks:` 的 manifest 拒绝追加。
- **两个来源**：`--source claude`（默认，`~/.claude/projects`）与 `--source cursor`（`state.vscdb` 的
  `cursorDiskKV`，**不是** `composerHeaders`）。`--cursor-db` 只配 cursor、`--root` 只配 claude，给错直接
  usage 报错。`--list` 先看有什么再收集。
- **`SplitSession` ≠「整段会话」**：任务是「第一个 prompt + 回答它的那些轮」；只有切片 == 整段会话
  （单 prompt 会话）时才用会话级 `Usage/BusyMs/DurationMs/CostUSD`，否则按切片求和并附「per-turn
  baseline inside a longer session」注记。**e2e 里别假设「基线 = 会话总量」**，除非夹具只有 1 个 prompt。
- **默认 `SplitSession` 会丢掉多 prompt 会话的后几个 prompt**（设计如此：整段轨迹在回答这个任务没问的
  问题，拿它比会让回放永远显得更便宜），但**必须说出来**：baseline 注记（`SplitSession &&
  len(s.Prompts)>1`）+ CLI `warnDroppedPrompts`。实例：cursor 会话 `1f2114cd` 80 msgs / 3 prompts → 默认
  1 个任务，`--split turn` 3 个。警告口径：按 metadata `session_id` 分组、比 `session_prompts` 与
  **实收 `items`（不是 `plan.Added`）**，否则每次重跑误报。
  **推论（2026-10-13）**：默认切片下**文件写入也会丢**——同一会话默认采集 0 个 Edit，`--split turn` 才有
  3 个。要重放文件改动必须配 `--split turn`。
- **`session`/`prompt_index` 是一等任务字段**（`dataset.TaskSpec`，在 `Baseline` 之后），**不走
  `metadata`**（metadata 契约上「引擎从不解释」，而这两个引擎必须读）。手写任务两个都不写 = 自成一条链
  ——这正是「不带 flag 时数据集行为不变」的机制。

## `--chain-sessions`：同一会话的轮次串成一条链

`--split turn` + 默认规划 = 每条 prompt 各自一个空沙箱；对「人在一直追问」的录制是错的。

- **规划**：按 `{chain, sample}` 分组（`chainKey` = 开链式且有 `session` 时取 `pathName(session)`，否则
  `pathName(task.ID)`），保留**首次出现的组序**，组内 `sortChain`，标 `first`/`last`；目录
  `root/<runID>/<chain>/s<sample>`。`sortChain` 只在**每一个** cell 都有 `PromptIndex` 时排序，否则
  **退回数据集自身顺序**（半个键不是键）。
- **物化/清理各一次**：只有 `first` 物化，只有 `last` 删。**物化失败污染整条链**（`broken[chain]`）：
  后面 cell 记 `fail(...)` 而**不跑**，否则会写出「agent 答得不好」的行而真相是工作区没建起来。
- **`Config.Validate()` 是独立纯方法**（Run 与 CLI 各调一次）：链式 + `--concurrency > 1` → 报错；CLI
  调它才能把 flag 错误报成 **usage error（exit 2）**。**`--resume` 拒绝半条链**（`chainsIntact`，点名链名
  与 `N of its M`）。
- **`pathName` 是安全边界**：白名单 `[A-Za-z0-9._-]`，其余换 `-`，trim `-.`，空则 `task`。
- **披露三件**：`runs.chain_sessions`（v5）→ 报告头部 chip → `comparisonWarnings` 里「链式的与普通的不是
  同一种测量」。
- **变异验证 14 项全部确认变红**（明细见 2026-10-12 日志）：丢 `sortChain`／`chainKey` 忽略 session／
  `pathName` 用原始 id／`Validate` 恒 nil／`first` 恒 false／`last` 恒 true／`chainsIntact` 永不拒绝／删
  baseline 注记／删 `warnDroppedPrompts` 调用／`boolInt(r.ChainSessions)` 恒 false／`scanRun` 恒 false／
  `load.go` 恒 false／`comparisonWarnings` 链式分支删掉／app.js chip 条件改 `false`——最后这条**必须跑
  Chrome 渲染测试**，静态 grep HTML 只证明源码被装进去。

## 工作区绑定：三种来源，优先级从高到低（2026-10-14 更新）

1. **起点记录 → `type: inplace`**（见 MEMORY.md 同名节）：`collect --workspace-cwd` 时若
   `~/.aeval/starts/<sid>.json` 存在，**优先**走它。
2. **`--workspace-cwd` 兜底 → `type: git`（钉 branch）**：目录是 git 仓库**且有提交** →
   `{type:git, repo:cwd, ref:s.GitBranch}`（`output.type` 从 `stdout` 变 `patch`）；**不是仓库/没有提交
   → 报错**（点名目录 + 「a replay would start from the answer」+ 建议 `--workspace-repo`）；目录不存在/
   不是目录 → 报错。**没有任何东西被拷进数据集**（目录的当下状态在会话之后，拷进去等于把答案交给回放）
   ——早期 `{type:files, assets:…}` + `snapshotTree` 分支**已删除**；`files` 类型现在**只**由 `--assets`
   或手写 `workspace.assets` 产生。与 `--workspace-repo`/`--assets` **互斥**（usage 层拒绝）。绑的是
   **分支**不是提交（transcript 的 `git_head` 为空），CLI 明知地打 note「that is the branch as it stands
   now」；cell 里的 `.git` 是**新建的单提交仓库**（upstream 历史被剥离）。
3. **空工作区**（默认）：带注记点名 `s.Cwd`，`collect` 也打同样的话到 stderr。

**空仓库陷阱**：对没有提交的仓库 `git clone` **成功但什么都不带** → 静默得到空工作区。所以
`materializeGit` checkout 后跑 `rev-parse --verify HEAD`，失败**拒绝落地**并说清是「没有提交」还是
「ref 不存在」；探针 `hasCommit` 同法。

路径基准不对称：`--assets` 经 CLI 传 → `absFlagPath` **绝对化后写进 tasks.jsonl**（数据集不可移植），
手写的 `workspace.assets` 相对**数据集文档**解析（`materializeFiles`）。run 期把 assets 目录**拷进每个
cell 的根**（`assets_target` 可改落点），随后一律 `InitBaseline` 建单提交基线。

**绑定只在 collect 期决定，run 期没有 flag**：`cwd` 平时只落在 `metadata.cwd`（引擎从不解释）与
`Baseline.Cwd`；run 期只读数据集的 `workspace`。改绑定形式只能手改 `tasks.jsonl` 或重新 collect —— 而
**往已有 `--out` 重新 collect 会被 `recordingKey` 判重复跳过**（实测 `appended 0, skipped 1`），必须
`--overwrite`。

## 未接入的素材：「还原现场」（2026-10-13 勘察）

「现场」= 起点 + 过程 + 环境；目前只记了起点（cwd + commit/快照）与过程（patch）。四类素材按忠实度：

1. **写/编辑工具的完整 input**（`Write`/`Edit`/`MultiEdit`/`NotebookEdit`）：`file_path` + `content` /
   `old_string`+`new_string`，**无截断**，且 **aeval 已写进 `BaselineToolCall.Input`（json.RawMessage）**
   →「按序 apply 重建会话结束时的树」是**离线、零额外采集**能做到的。`old_string` 是天然**校验锚点**：
   apply 不上 = 起点不是那个样（脏起点），是证据而非静默损坏。盲区：**Bash 副作用**（`cat >`/`mv`/`rm`/
   包管理器）与 **Read 过的文件**（`tool_use.input` 只有参数，result 不在其中）。
2. **`~/.claude/file-history/<session-uuid>/<sha256(absPath)[:16]>@vN`**：**未文档化**的逐版完整快照。
   key = `sha256(绝对路径)[:16]`（反推验证过）、`@v1` = **编辑前原文**；实测三次 Edit 对应 `@v1→@v4`
   且与转录的 old/new **逐步吻合** → 第 1 类的**独立校验**。aeval 现在只读了 `TrackedFileBackups` 的
   **keys**，values 指向的备份内容没读。局限：本机只剩 1 个会话（有 `.last-cleanup`），只有 Write/Edit
   触发的文件有 → 只能当补充。
3. **`session_context.gitStatus` attachment**（转录内）：会话开始的 branch / `Recent commits` / **脏文件
   清单**（`M path`）。**0 引用**。哪些文件脏可得，**内容**不可得（只有状态码）。
4. **`~/.claude/shell-snapshots/snapshot-<shell>-<ts>-<rand>.sh`**：会话开始注入的完整 shell 环境
   （227KB），时间戳与会话对得上。**0 引用**；另有 `environment` attachment 给
   cwd/isWorktree/isGitRepo/platform/shell。

**忠实度排序**：显式给予（`--assets` / `workspace.git` 钉 commit）> baseline 重放（覆盖 Write/Edit，
缺 Bash）> `--workspace-cwd` 绑 branch 现状 > 空工作区。**未接的下一个候选**：`type: replay` + 报告披露
覆盖率（`replayed N of M edits; K bash calls not replayed`）。
