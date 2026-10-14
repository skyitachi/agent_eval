# agenteval — 项目长期记忆

> 只记「改代码前必须知道的约束」。**细节在 `topics/`（按需读）与 `2026-*.md` 日志**：
> `topics/isolation.md`（就地回放 / hook / 起点记录 / 回滚）、`topics/collect.md`（collect / 链式 /
> 工作区绑定 / 还原现场素材）、`topics/report.md`（模型归属 / token 记账 / app.js）、
> `topics/testing.md`（测试与运行）、`topics/claude-cli.md`（claude skill 闸门 / skill 轴静默失效）。

## 是什么

Go agent 评测框架，`/Users/skyitachi/lab/agent_eval`，module `github.com/skyitachi/agenteval`。
数据集 → 可插拔 grader → SQLite → 自包含 HTML 报告。只有 Claude Code 一种 agent（`mock` 是测试替身），
model 透传 CLI。子命令：`collect`/`hook`/`validate`/`run`/`matrix`/`report`/`list`/`version`。
回答「做得怎么样 / 差异归因给谁 / 真实会话花了多少」，**拒绝**回答「这个 agent 好不好」。
退出码 `0/1/2/130`；**有任务失败 ≠ 命令失败**。

## 三轴归因（OFAT）

harness = 实现+配置；`model`、`skill` 各一轴。一次 sweep 只动一条轴，**加载期拒绝**。所有权靠**反射读
`agent.Spec` 的 yaml tag** 推导，**其余全归 harness**。分离度用**精确符号检验**，**非平局配对 < 6 对时
无论多偏都到不了 p<0.05**（写进每份报告）。**详见 `topics/attribution.md`。**

## 就地回放（`workspace.type: inplace`）—— 2026-10-14 新增

依据 `docs/isolation.md`（用户给的需求文档，现已改成实现记录）。**细节全在 `topics/isolation.md`**，
改这块之前必读。此处只留会咬人的四条：

- **用户拍板的语义不要自行改动**：非 git「**会话开始前**记录内容」；commit 取「会话开始前记的」；
  **不 clone、在原目录真正就地跑**；判据「**干净 + HEAD 一致**」；不符**拒绝并列出差异**；跑完
  「**存下 patch 后回滚**」；**锁死 `concurrency=1, samples=1`**（`ValidateDataset` 独立纯方法，
  报成 usage error）；记录形式 = **SessionStart hook 自动记**；回滚 = **重置 + 隔离到 `~/.aeval`**。
- **回滚必须由 `ran` 守卫**（⚠️ 血案）：只在 `drive.Run` **之前**置 true。没碰过目录的尝试回滚它不是
  no-op —— 它会拿记录的 commit 覆盖用户此刻在写的东西（曾真的 stash 走用户 6 个未提交改动）。
  `runCell` 的返回值是**具名**的，否则 defer 里的回滚结果会被 `return att, broke` 拷贝覆盖掉。
- **回滚不用 `git stash`**：`git reset --quiet`（清 ITA 条目）→ agent 新建文件搬到
  `~/.aeval/discarded/<sid>/`（**移不是删**）→ `git reset --hard <记录的 commit>`。stash 会堆进
  用户的栈，且被 `git add -A -N` 的 ITA 条目挡住过（静默回滚失败）。`GitDir` 非空时 git 走
  `GIT_DIR`/`GIT_WORK_TREE`，让 git 对非仓库目录做 diff 而**不写入 `.git`**。
- **必须披露**：patch 长得和「干净检出」里的一模一样、目录事后被还原 → 任何 cell 行都推不出来。
  `runs.inplace_tasks`（**v6**，计数不是 flag）→ 报告头部 chip → `comparisonWarnings`；
  `run` 还在动手**之前**打 `inplaceNotice`。**已知取舍别当 bug 修**：空目录留下、ignored 文件不动、
  mtime 不还原（都写进了代码注释）。

## 工作区绑定的三种来源（优先级从高到低）

1. **起点记录 → `type: inplace`**（上节）：`collect --workspace-cwd` 时若 `~/.aeval/starts/<sid>.json`
   存在，**优先**走它。
2. **`--workspace-cwd` 兜底 → `type: git`（钉 branch）**：必须是 git 仓库**且有提交**，否则报错（点名
   目录 + 建议 `--workspace-repo`）。**没有任何东西被拷进数据集**（当下状态在会话之后，拷进去等于把答案
   交给回放）。与 `--workspace-repo`/`--assets` **互斥**（usage 层拒绝）。
3. **空工作区**（默认）：带注记点名 `s.Cwd`，`collect` 也打同样的话到 stderr。

**空仓库陷阱**：对没有提交的仓库 `git clone` **成功但什么都不带** → 静默得到空工作区。所以
`materializeGit` checkout 后跑 `rev-parse --verify HEAD`，失败**拒绝落地**并说清是「没有提交」还是
「ref 不存在」；探针 `hasCommit` 同法。

**绑定只在 collect 期决定，run 期没有 flag**；改绑定形式只能手改 `tasks.jsonl` 或重新 collect —— 而
**往已有 `--out` 重新 collect 会被 `recordingKey` 判重复跳过**，必须 `--overwrite`。
`--workspace-cwd` 命中起点记录时 `collect` 打 `bindNotice`：`note: N task(s) replay in place, pinned to
the commit the session saw`（**别**再写死「binds at the branch it ran on」）。详见 `topics/collect.md`。

## 设计约定（改代码前先读）

1. **报告是「数据库的函数」**：渲染层只读持久化状态（`report/model.go`+`load.go` 投影成视图模型，
   **借用** `runner.Summary`/`attrib.Attribution` 而不镜像）。e2e 断言运行期报告与从 db 重生**逐字节
   相同**（比对时省掉 `generated`）。
2. **XSS 靠编码器不变量**：`render.go` 的 `neutralize()` 把 `<` `>` `&` 重写成 `\u003c` 等，一次封死三种
   逃逸；客户端默认 `esc()`，只有 `raw()` 例外。
3. `html/template` 上下文敏感：`<style>` 用 `template.CSS`、`<script>` 用 `template.JS`；用
   `template.HTML` 会被当文本**重新转义**。
4. **`hidden` 必须由样式表显式兑现**：UA 的 `display:none` 被任何作者 `display` 覆盖
   （`.drawer{display:flex}` 直接作废它）→ 加 hidden 元素必须同步加 `.cls[hidden]{display:none}`；
   `TestHiddenElementsCanActuallyBeHidden` 守它。
5. `MaxInlineBytes`（默认 8 MiB）超了按优先级丢 detail（失败/出错留最久，skip 最先丢），降级记入
   `Warnings`；轨迹截断留**头尾 + gap marker**。
6. 数据集/matrix 文档**就地解码**，未知键是**错误**（拼错的 `plugin_dirs` 会产出「skill 没用」的
   sweep）。时长必须带单位。
7. 路径相对**声明它的文档所在目录**解析；经 CLI 传的（`--bin`/`--work-root`/`--session`/`--file`）相对
   **调用者 cwd**。「裸命令名 vs 路径」：值里没有 `/` → 当 PATH 上的命令原样保留，否则绝对化（唯一实现
   `agent.ResolvePath`）。
8. **测量值不重试**：timeout/非零退出/error result 都是**数据**。`retries` 只管进程起不来，默认 0。
9. agent 默认 **clean room**（无 user hooks / auto-memory / 任务外 MCP / CLAUDE.md）。**但 `hook start`
   正好相反**：它跑在用户真实会话里，必须**永不让用户会话失败**（默认不吃错误）。
10. **位置参数可写在 flag 之后**：Go 的 `flag` 遇第一个非 flag 就停；`positionals()` 从那个词起重新解析，
    **但必须在 `openStore` 之前调**——曾因顺序错让 `report --compare a b --db X` 的 `--db` 来得太晚，两个
    run 都被报成「not found in **aeval.db**」。护栏 `TestCLIComparesTwoRunsHoweverTheFlagsAreOrdered`。
11. **工具打印的命令必须能跑**：`Next:` 提示漏 `--db` 就是必然失败的命令；`printSummary` 只在 `--db` 非
    默认时补它、**按用户输入原样回显**，`hintArg` 给带空格的值加引号。**找不到记录必须点名文件**：
    `store.NotFoundError{Kind,ID,Path}`。
12. **schema 版本：往后拒绝、往前升级**（更低 = **就地升级**，发生在**任何一次 open** 含 `report`）。
    `migrations[i]` = v(i+1)→v(i+2)，只追加；每步**必须能重跑**（`ADD COLUMN` 不幂等，在 DDL 与版本号之间
    被杀掉会报 "duplicate column name"）。**每列都必须落库**：基线是**输入**，渲染期**不读数据集**；缺了
    列报告只能沉默。**v2** `attempts.baseline_output`（=「录制里没留下答案」）；**v3**
    `attempts.baseline_agent_*`（空 =「没记录过来源」≠「录制没有 agent」）；**v4** `runs.agent_spec_json`
    （标签列识别不出「只差 skills」的 run）；**v5** `runs.chain_sessions`（attempt 行上没有任何一列能事后
    推出「这些 cell 是一条续写」）；**v6** `runs.inplace_tasks`（计数：`--workspace-cwd` 只绑有起点记录的
    任务，所以一次 run 可以「一半就地」）。v5/v6 这类「**默认值就是答案**」的列不需要 `unknown` 分支：
    写它们的那版代码根本做不到这件事。**夹具构造法**：从今天的 schema **删掉新加的那些列**再改
    `schema_meta` 版本号（v1 夹具要连带 v3 四列 + v4/v5/v6 的列一起删；`tests/e2e_test.go::downgradeToV1`
    同理 —— 只删标题里那一列 = 夹具漂移，测试会永远绿）。

## 数据集格式

`dataset.yaml` + `tasks.jsonl`；`workspace` 四型（`files`/`git`/**`inplace`**/空）、`output.type` 四型。
**`session`/`prompt_index` 是一等字段，不走 `metadata`**；`graders[].weight` 是**指针**（区分未设=1 与
显式 0）。**详见 `topics/dataset.md`。**

## 测试与运行（要害）

- e2e 在 `tests/e2e_test.go`，真实二进制/子进程/SQLite/HTML 驱动；**runner 覆盖率只有 29.2%**，编排行为
  主要靠 e2e。
- **pytest grader 要 `AGENTEVAL_PYTHON`**，本机用
  `~/.workbuddy/binaries/python/envs/aeval-pytest/bin/python`（`envs/default` **没有** pytest）。
  断言「demo 三题全对」的 e2e 必须 `requirePytest(t)`。
- **沙箱内 `go test ./...` 必然红两条**（`TestReportDrawer*`，Chrome 信号）；**沙箱外 ok**，别去查代码。
  **背景 shell 不继承放行 → 变异验证必须前台跑**。
- **e2e 的 SKIP 要当没跑**（缺 pytest / 缺浏览器都 skip）；`ok` 不代表这两类执行过。
- **涉及 hook / inplace 的测试务必把 `AEVAL_HOME` 指到临时目录**：否则写的是用户真实的 `~/.aeval/`，而且
  真实工作区可能被就地回放碰到。
- 其余（fixture 位置、无头 Chrome 动画不推进、BSD grep 静默返回空、变异测试要「不破坏编译」）见
  `topics/testing.md`。

## 已知缺口（勿当已完成）

- **容器后端整体不可达**：`internal/sandbox/docker.go` **无调用方、无测试**；runner 里 `Backend` 写死
  local。保留是刻意的，注释已指向二选一：接上并补 e2e，或删掉。`DockerConfig.Network` **没有默认值** →
  空值 = docker 默认（联网）。
- **`sandbox.Baseline.RestorePaths` 写了但没人用**：「判分前还原测试文件」的原语，**比「没有」更危险**
  （看起来做完了）。要完成是**接线**。
- **`Attempt.CommandLine` 仍无消费方**。`llm_judge` 不在 e2e 覆盖内（e2e 必须离线）。
- **`EditedFiles` 只在会话级切片输出**（`editedFiles()` 有 `len(turns) != len(s.Turns) → nil` 早退，实测
  `--split turn` 每个任务都是 `edited_files: null`），且**不落报告**（`internal/report/` 与 `cmd/` 里 0
  引用）。文件信息只能从 tool calls 的 `file_path` 拿。
- **cursor 把「文件引用块」塞进 user bubble 正文**：`--split turn` 于是把机器注入的上下文当「prompt」的
  一部分（会话 `1f2114cd` 的 p0 正文里就带着一块）。**已报给用户、未修**——剥离规则写错会把真人写的内容
  也删掉。
- **链式只覆盖 `run`，`matrix` 还没有这条路**（要做是接线同一套 `plan`，不是重写）。**inplace 同样未接
  `matrix`**，且 `--resume` 对 inplace 语义未定义（没有「半条链」，但工作区已被改过）。
- **`checkSkillLevel` 只查 `disable_slash_commands`，漏了 `bare`** → 见 `topics/claude-cli.md`。

## gitignore 约定

根锚定的 `/workspaces/` 是默认落盘处。规则**必须锚定根**：`datasets/demo/workspaces/` 是数据集资产，
需要保持被跟踪。详见 `topics/testing.md`。
