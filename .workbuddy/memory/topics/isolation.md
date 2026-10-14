# 就地回放（inplace）—— 细节

> `MEMORY.md` 只留约束与血案；这里放实现细节。设计来源 `docs/isolation.md`（已改成实现记录）。

## 用户拍板的语义（**不要自行改动**）

依据用户给的 `docs/isolation.md`：非 git 目录「**会话开始前**记录内容」；commit 取「**会话开始前
记录当前 commit**」；**不 clone、在原目录真正就地跑**；判据 =「**干净 + HEAD 一致**」；不符
**拒绝并列出差异**；跑完「**存下 patch 后回滚**」；**锁死 `concurrency=1, samples=1`**；记录形式 =
「**SessionStart hook 自动记**」；回滚方式 =「**重置 + 隔离到 `~/.aeval`**」。

## `internal/startstate/`（新包）

`~/.aeval/`（`AEVAL_HOME` 可覆盖）下四棵树：

| 路径 | 内容 |
| --- | --- |
| `starts/<sid>.json` | 起点记录 |
| `snapshots/<sid>/tree/` | 非 git 目录的完整快照 |
| `baselines/<sid>/` | 非 git 目录的起点仓库（`GIT_DIR` 指这里，目录里不写 `.git`） |
| `discarded/<sid>/` | 回滚时 agent 新建的文件被**移动**到这里 |

- `Record{SessionID,Cwd,Kind,Commit,Branch,Dirty,Snapshot,Files,Bytes,RecordedAt,ToolVersion}`；
  `Kind` ∈ `git`|`dir`（`insideWorkTree()` 经 `git rev-parse --is-inside-work-tree`）。
- `Capture` 不写盘、`Save` 才写（临时文件 + rename 原子写）；`safeName(id)` 白名单 `[A-Za-z0-9._-]`，
  归零则 `session`。
- `snapshotTree` 跳 `.git` / 符号链接 / 非普通文件；`maxBytes`（默认 512 MiB）是**硬停止**不是截断
  （不完整的快照是会撒谎的起点）。
- `compareTrees` 按 sha256 **内容**比对（不比 mtime），非 git 场景忽略 `.git`。
- `Restore`：Changed/Missing 从快照拷回，Added 走 `QuarantinePath` → `discarded/<sid>/`（**可逆**）。
- `Verify` 对 `KindGit` **先查 `Commit == ""` 就报错**（Capture 读不到 HEAD 时的空值 =「不知道」，不是
  「不匹配」，两者补救办法不同）。
- `Lines()`/`Error()` 把 drift 渲染成可打印的多行：`HEAD is … the record says …` /
  `N uncommitted change(s):` + 逐条 `git status --porcelain` 行 / 非 git 的 changed|missing|added 三组。

## Claude Code hook

`aeval hook start [--session|--dir|--max-snapshot-mb|--verbose|--strict]`，stdin 收
`{session_id, cwd, hook_event_name, source}`。取值顺序：flag → payload → `$CLAUDE_SESSION_ID` /
`$CLAUDE_PROJECT_DIR` → 当前 cwd。没有 id 就不记（记不了就没有键可以查回来）。

**默认静默、默认不吃错误**（`hookTrouble` 只写 stderr，`--strict` 才非零退出）：hook 跑在用户敲第一个
键之前，一条记录问题不该拦住正在干活的人。

## 数据集字段

`Workspace{Type:"inplace", Dir, Ref, Snapshot, Session}`；`Dir` **必须绝对路径**；禁止与
`repo`/`assets`/`setup` 同时出现（setup 会改一个不属于 harness 的目录）。`output.type` 由 collect
推导成 `patch`。**`session` 是一等任务字段**（`dataset.TaskSpec`，紧跟 `Baseline`），**不走
`metadata`**（metadata 契约上「引擎从不解释」）。

## 物化与运行

- `sandbox.materializeInplace`：目录存在且是目录、**sandbox root 必须 == `ws.Dir`**、
  `startstate.Verify` 拒绝 drift（`InplaceDriftError`）、非 git 时 `buildDirBaseline` 在
  `~/.aeval/baselines/<sid>/` 建仓库并用 `GIT_WORK_TREE=快照` 提交一次。
- `buildDirBaseline` 用 `baselineCommitsTheSnapshot()` 判定（`GIT_DIR`+`GIT_WORK_TREE` 跑
  `status --porcelain` 为空）—— 防止快照被**重新记录**后基线变陈旧。
- `Baseline.GitDir` 非空 → git 走 `GIT_DIR`/`GIT_WORK_TREE`（让 git 对非仓库目录做 diff 而**不写入**
  `.git`）；`ChangedPaths`/`Diff`/`DiffStat`/`RestorePaths`/`listBaseline` **全部走 `(*Baseline).git()`**。
- **`git status --porcelain` 在 `GIT_WORK_TREE` 下不可靠**（stat-fails 会误报 deleted）。
- `plan()`：inplace cell 的目录直接取 `t.Workspace.Dir`（不建 per-run/per-cell 目录）。
- `runCell()`：跳过 `MkdirAll` / `RemoveDir`（目录不是 harness 的）。
- `ValidateDataset(ds)` 是**独立纯方法**（与 `Validate()` 并列），`Run()` 与 CLI 各调一次 → inplace
  的 concurrency/samples 错误才能报成 **usage error（exit 2）**。

## 回滚：重置 + 隔离（**不再用 stash**）

`settleInplace()`：**先 diff 落库，再 rollback**（顺序反了就没有 patch 可存）。

git 目录 `rollbackGit`：

1. `git reset --quiet` —— 清掉 diff 留下的 intent-to-add 条目；
2. 找 agent 新建的文件：`git ls-files --others --exclude-standard` +
   `git diff --name-only --no-renames --diff-filter=A <ref>`（前者未跟踪，后者**已被 agent 提交过**）；
3. 逐条搬到 `~/.aeval/discarded/<sid>/`（`moveAside`：rename 失败则拷回退，跨设备安全）；
4. `git reset --hard --quiet <记录的 commit>`。

非 git 目录：`startstate.Restore` 按快照还原（新增的**移走**）。

**为什么弃用 stash**：早期版本 `git stash push --include-untracked`，两个后果 —— ① 往用户的栈里堆
他们没要求过的东西（跑完得提醒用户去 `stash pop`）；② 被 `git add -A -N` 留下的 ITA 条目挡住，报
`Entry '…' not uptodate. Cannot merge.`，回滚静默失败。重置 + 隔离把两件事一起解决，用户的栈从头到尾
没被碰过。

**已知取舍（写进代码注释，勿当 bug 修）**：

- **空目录会留在原地**（agent 造的目录，内容被移走后目录本身留下；git 不跟踪目录）。
- **ignored 文件不动**（`status --porcelain` 承认的干净意味着 ignored 内容无从区分是谁写的）。
- **mtime 不还原**（判据是内容）。

## ⚠️ 血案（已修，勿重犯）

**回滚必须由「agent 真的跑过」守卫**。曾因 `settleInplace` 只看 workspace 类型、不看 agent 是否启动，
在 materialize 失败后仍回滚，把**用户正在写代码的真实工作区** 6 个未提交改动 + 1 个未跟踪目录 stash
走。现加 `ran bool`，**只在 `drive.Run` 之前置 true**：
`if inplace { if ran { settleInplace(…) }; return }`。注释已写「That is not hypothetical: it is how this
code once ate the working tree it was being written in.」**改这块之前先读那段注释。**

`runCell` 的返回值是**具名**的（`(att store.Attempt, broke string)`）：`return att, broke` 会在 defer
之前先把值拷进返回槽，defer 里再改 `att` 就白改了 —— 回滚的结果正是这一行存在的理由。

## 披露三件（schema v6）

inplace cell 的 patch 和「干净检出」里的 patch **长得一模一样**，且目录事后被还原 → 任何 cell 行都
无法事后推出这件事。所以：

1. `runs.inplace_tasks`（**v6**，计数而非 flag：`--workspace-cwd` 只绑有起点记录的那些任务）；
2. 报告头部 chip `workspace the recording's own directory`（app.js，`mode-alert`）；
3. `report --compare` 的 `comparisonWarnings` 加一条「whether the cells ran in the recording's own
   directory (1 task vs 0 tasks)」。

另外 `aeval run` 在动手**之前**把 `inplaceNotice` 打在终端上（点名目录 + 点名 `discarded/<session>/`
落点）。它是**最需要披露的一种 run**：agent 能看见用户未提交的改动、ignored 的 `node_modules`，和沙箱
回放不是同一个 agent。

## 端到端

`tests/e2e_test.go::TestCLIReplaysInPlaceAndPutsTheDirectoryBack`（夹具
`newInplaceFixture`，被渲染测试复用）：真 git 仓库 → `hook start` → `collect --workspace-cwd` → `run`，
断言 patch 落库、目录逐字节还原、新建文件进 `discarded/`、**stash 栈为空**、dirty 时拒绝且用户改动还在
（cell 记 harness error，**命令仍 exit 0**）。

`tests/render_test.go::TestReportHeaderSaysTheCellsRanInTheRecordingDirectory`：**真 Chrome** 读
`.card.runhead` 的 textContent —— 载荷里有 `inplace_tasks` 不等于页面上画出来了它。

`bindCwd(s)` **已经没有 `opts` 参数**（`--assets` 那条分支删掉之后它就不用了），别再把它加回去。
