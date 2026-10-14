# 测试与运行（细节）

> 属于 `MEMORY.md` 的展开。写/跑测试前读本文件。

- e2e 在 `tests/e2e_test.go`，用真实二进制/子进程/SQLite/HTML 驱动。覆盖率 attrib 91.8% / report 89.6% /
  trace 87.5% / store 82.5% / session 79.7% / **runner 29.2%** ——编排行为主要靠 e2e。
- e2e 的 payload 结构**刻意不复用** `report.Data`，也**刻意不声明** `generated`；解 payload 找**第一个
  `;</script>`**。
- **pytest grader 需要带 pytest 的解释器**：`AGENTEVAL_PYTHON`。本机
  `~/.workbuddy/binaries/python/envs/aeval-pytest/bin/python`（`envs/default` **没有** pytest）。跑法
  `AGENTEVAL_PYTHON=… go test ./...`（带上后 test 包 ~8s → ~60s）。`runCLI` **只在探测到带 pytest 时
  才导出**它。**任何断言「demo 三题全对」的 e2e 必须 `requirePytest(t)`**。
- fixture：`tests/testdata/agents/good.sh`/`bad.sh`/`chain.sh`；`examples/matrix-harness.yaml` 可离线跑。
- **无头 Chrome 里 CSS 动画永不推进**（`.drawer` 淡入停在 `opacity:.4`）→ **截图复核前先注入
  `.drawer{animation:none}`**。
- **沙箱内 `go test ./...` 必然红两条**：`TestReportDrawer*` 报 `Google Chrome: signal: trace/BPT trap`。
  **沙箱外** → `ok`。别去查代码。**推论：背景 shell 不继承这个放行** → 变异验证必须**前台**跑。
- **「红了」≠「被抓到了」**：变异验证要**按输出归因**——命中自己写的断言文案才算抓到。
- `--dump-dom` 输出**含注入脚本自身**，探针结果要 base64 回传，匹配字面量要运行时拼。
- **grep 空结果先怀疑表达式**：BSD grep 不支持 `\|`，ERE 也不支持 `\s`/`\d`/`\b`（**静默返回空**）。
- **变异测试要用「不破坏编译」的形状**：把 `return fold(a)==fold(b)` 改成 `return a == b` 会让 `fold` 变成
  未使用 → build failed，看起来红了其实什么都没验；正确做法是让 `fold` 变恒等。删**字段赋值**安全，删
  **唯一使用点**会把变量变未使用。
- **e2e 的 SKIP 要当没跑**（缺 pytest / 缺浏览器都会 skip）；`go test ./...` 的 `ok` 不代表这两类执行过。
- **涉及 hook / inplace 的测试务必把 `AEVAL_HOME` 指到临时目录**：否则写的是用户真实的 `~/.aeval/`，而且
  真实工作区可能被就地回放碰到。`runCLIEnv(t, env, args...)` 就是为此加的（`runCLIEnv` 会照旧补
  `AGENTEVAL_PYTHON`）。
- **需要真 git 仓库的 e2e 用 `gitIn`/`gitOut`（`tests/e2e_test.go`）**：前者把 `commit.gpgsign=false`、
  `init.defaultBranch=main`、`user.name/email` 都 `-c` 死 —— 用户全局 git 配置不该决定测试能不能建夹具。
- **渲染测试要跑 `browserRender(t, path)` 读 `.card.runhead`**，只 grep HTML 源码证明不了客户端画出来了。
  验证方式：把 app.js 里那个条件改成 `&& false`，确认变红，再改回来。
- **`--json` 时人类可读输出改走 stderr**（`chatter` 分流）：否则 `run --json | jq` 会撞上横幅文本。
  `matrix --json` 本来就这么做。

## gitignore 约定

- 根锚定的 `/workspaces/` 是默认落盘处（`<db dir>/workspaces`），每个 cell 是目标仓库的**完整拷贝**
  （内含嵌套 `.git` → `git add -A` 会把它们当 gitlink 收进去并警告）。
- `datasets/demo/workspaces/` 是数据集资产**必须保持被跟踪** → 规则必须锚定根。
- `datasets/local_claude_code/` 是 `collect` 产物（含绝对路径 + 真实会话文本），也忽略。
- **不忽略 `datasets/mine/` 之类的任意目录** —— 那是猜命名。
