# 就地回放：让 agent 跑在录制时那个目录里

## 设计要点（原始）

### 设计目标

- 让 agent 在执行采集到的 case 的时候，运行期间的环境保持和 baseline 相同
- 评测 skill 的维度的时候，仅 skill 不一样

### 如何定义环境相同

**本地，原运行目录是 git**

保证 commit 相同即可。如果在 baseline 会话开始之前，已有改动（dirty），暂时先不用管，
但是要在备注里加上提醒。

**本地，原运行目录不是 git**

记住会话开始前的目录内容即可。

### 运行约束

本地 git 仓库的场景，要切换到 baseline 对应的目录，但是对应目录如果是 dirty 的情况下不能
运行，需要提醒用户提交已有改动。

---

## 实现

一句话概括：**会话开始前记一次，回放时就地跑，跑完存下 patch 再还原**。

前两步的难点是「会话开始前」—— 等 `aeval collect` 去读转录时，目录早就是会话跑完之后的
样子了。所以起点记录由一个 hook 在会话启动的那一刻写下。

### 自动记录：`aeval hook start`

```sh
aeval hook start [--session <id>] [--dir <path>] [--max-snapshot-mb 512] [--verbose] [--strict]
```

作为 Claude Code 的 `SessionStart` hook：

```json
{
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "aeval hook start" } ] }
    ]
  }
}
```

会话 id 与目录按 `--session` / `--dir` → stdin payload（`session_id` / `cwd`）→
`$CLAUDE_SESSION_ID` / `$CLAUDE_PROJECT_DIR` → 当前工作目录 的顺序取。

- 默认**什么都不打印**：hook 跑在用户敲第一个键之前，聊天客户端启动过程不是打进度条的地方。
  `--verbose` 才输出记了什么（目录、kind、commit、branch、已有改动数、快照文件数、存储位置）。
- 默认**绝不失败**：记录出问题只往 stderr 写一行，不打断会用会话 —— 记录是给以后的，会话是
  现在的，顺序不能反。`--strict` 才返回非零退出码（给 CI 用）。
- 没有 session id 就**不记**：记不了就没有键可以查回来，那是一个文件而不是一条记录。

### 记录存在哪

`$AEVAL_HOME`，默认 `~/.aeval`：

| 路径 | 内容 |
| --- | --- |
| `starts/<session>.json` | 起点记录本身 |
| `snapshots/<session>/tree/` | 非 git 目录的内容快照（跳过符号链接） |
| `baselines/<session>/` | 非 git 目录的起点仓库（git 指向它，目录里不写 `.git`） |
| `discarded/<session>/` | 回滚时 agent 新建的文件被**移到**这里，而不是删掉 |

记录的关键字段：

```json
{
  "session_id": "…", "cwd": "/abs/path", "kind": "git",
  "commit": "…", "branch": "main", "dirty": [" M calc.py", "?? notes.md"],
  "snapshot": "…/snapshots/<sid>/tree", "files": 3, "bytes": 34,
  "recorded_at": "…", "tool_version": "…"
}
```

- `kind: git` → `commit` 就是「环境相同」的全部含义；`dirty` 里是**会话开始前就已经存在**
  的改动（带状态码，读者能分清「被改过」和「本来就没有被跟踪」）。按设计要点，这些改动
  不阻断录像，但会跟着记录走并在报告里点名。
- `kind: dir` → 内容快照是唯一的环境描述。
- session id 会变成路径组件，所以过白名单 `[A-Za-z0-9._-]`，其余换 `-`，归零则用 `session`。

### 数据集侧：`workspace.type: inplace`

`aeval collect --workspace-cwd` 遇到起点记录时写出：

```json
{"id":"…","session":"<sid>",
 "workspace":{"type":"inplace","dir":"/abs/path","ref":"<commit>"},
 "output":{"type":"patch"}}
```

- **不是 clone**，这是全部要点。clone 只带已提交的内容，会丢掉 agent 当时能看到的未跟踪
  文件；而且它把回放放进一个 harness 自己造的目录，那不可能等于录制时的环境。
- `dir` 必须是**绝对路径**（`validate` 会拒相对路径：一个相对路径只有在 collect 恰好从那个
  目录跑时才成立）。
- `inplace` 与 `repo` / `assets` / `setup` **互斥**，加载期就拒绝，不是警告。
- 数据集里**什么都没被拷进来**：目录的当下状态在会话之后，拷进去等于把答案交给回放。
- 有起点记录 → `inplace`；没有记录才退回 `type: git` 绑当前分支（并打 note 说「这是那个分支
  现在的样子，转录里只记了分支没记 commit」）。两种绑法是两种测量，`collect` 分开告诉你。

### 运行期：校验 → 就地跑 → 存 patch → 回滚

`aeval run` 对每个 inplace cell：

1. **前置校验**（`startstate.Verify`）—— 目录还在吗？`HEAD` 等于记录的 commit 吗？工作区
   干净吗？非 git 就快照逐文件比（内容而非 mtime）。
2. 不符则**拒绝并列出差异**：
   ```
   materialize: /path/repo no longer matches the state this replay starts from:
     1 uncommitted change(s):
        M calc.py
   commit or stash those changes, then run again
   ```
   拒绝发生在 agent 启动**之前**，所以目录一个字节都没动 —— 一个「拒绝了还顺手还原」的检查
   比没有检查更糟，它毁掉的正是它据以拒绝的证据。这个 cell 记为 harness error，命令本身仍然
   exit 0（一次任务失败 ≠ 一次命令失败）。
3. **就地跑**：agent 的 cwd 就是那个目录，没有 `--work-dir` 之类的选择。
4. **存下 patch**，再回滚。顺序不能反 —— 先还原就什么都没得存了。

### 回滚：重置 + 隔离，不碰用户的 stash 栈

git 目录：

```sh
git reset --quiet                                       # 清掉 diff 留下的 intent-to-add 条目
<列出 agent 新建的文件> → 移到 ~/.aeval/discarded/<sid>/
git reset --hard --quiet <记录的 commit>
```

- **不用 `git stash`**：早期版本用 stash，结果是往用户的栈里堆他们没要求过的东西，还得靠
  `git reset` 先清掉 `git add -A -N` 留下的 ITA 条目才能成功。重置 + 隔离把这两件事一起
  解决了，而且用户的工作区从来没被「保存起来」过。
- 新建文件从 `git ls-files --others --exclude-standard` 加
  `git diff --name-only --no-renames --diff-filter=A <ref>` 得到：前者是没跟踪的，后者是
  agent 自己提交过的。
- 不删，只**移**。一个会丢弃产出的回滚没人敢跑第二次。

非 git 目录：回滚 = 按快照还原（新增的移走，改过的写回，缺了的补回），同一套隔离开关。

### 回滚必须由「agent 真的跑过」守卫

`runCell` 里那个 `ran` 标志不是防御性编程。教训是：物化失败时（沙箱根目录和任务目录对不上）
deferred 的回滚照跑，把当时正在被编辑的工作树重置了。**一个还没碰过目录的尝试，回滚它不是
no-op —— 它会拿记录的 commit 覆盖用户此刻写的东西。**

### 这一模式在报告里必须可见

inplace cell 的 patch 和「harness 自己造的干净检出」里的 patch **长得一模一样**，而目录事后
被还原了，所以任何一行 cell 都无法事后推出这件事。因此：

- `runs.inplace_tasks`（schema **v6**）记下这次 run 有几个任务就地跑过；
- 报告头部挂 chip「workspace **the recording's own directory**」；
- `report --compare` 把「是否就地跑」列进差异清单 —— 两个 run 标签全同，但一个把用户的真实
  工作树交给了 agent，另一个只给了检出，配对出来的 delta 不是同一个问题。

同时，`aeval run` 在动手**之前**就把这件事打在终端上（点名目录 + 点名隔离位置）。

### 并发与样本

inplace 数据集**锁死 `concurrency=1`、`samples=1`**，`run` 在 flag 层报 usage error
（exit 2）并点名是哪个任务：

- 两个 cell 同一个目录会互相覆盖，而且是在一个没人能撤销的地方；
- 第二个 sample 测的是第一个留下的状态，不是记录的起点。

链式（`--chain-sessions`）与 inplace 是两条独立的检查：前者是关于「哪些 cell 共享目录」的
flag 错误，后者是关于「这个数据集能不能这样跑」的，合在一起会报错报成另一个 flag。

### 已知取舍

- **空目录会留在原地**：agent 建的目录如果内容都被移走，目录本身留下 —— git 不跟踪目录，
  没有诚实的办法判断它是谁建的。
- **ignored 文件不动**：`status --porcelain` 说「干净」就意味着这里有 ignored 内容，
  而无从区分是 agent 写的还是本来就在的。宁可留下也不误删。
- **文件的 mtime 不还原**：判据是内容，不是时间戳。
- **非 git 快照有上限**（默认 512 MiB）：超了**报错**，而不是悄悄截断 —— 一份不完整的快照
  是一份会撒谎的起点。

### 测试

- `internal/startstate/`：记录、快照、比对、还原、路径逃逸。
- `internal/sandbox/inplace_test.go`：物化、基线、回滚（git 与非 git 两条路）。
- `internal/runner/inplace_test.go`：cell 目录来源、并发/样本拒绝。
- `internal/dataset/inplace_test.go`：`inplace` 的加载期约束。
- `tests/e2e_test.go::TestCLIReplaysInPlaceAndPutsTheDirectoryBack`：真 git 仓库 → hook →
  collect → run，断言 patch 落库、目录逐字节还原、新建文件进了 `discarded/`、**stash 栈没被
  碰过**、dirty 时拒绝且用户改动还在。
- `tests/render_test.go::TestReportHeaderSaysTheCellsRanInTheRecordingDirectory`：真浏览器
  里读报告头部 —— 载荷里有 `inplace_tasks` 不等于页面上画出了它。
