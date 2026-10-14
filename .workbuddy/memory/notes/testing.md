# 测试与运行 / 沙箱与工具陷阱 / gitignore

## 套件

- `go test ./...` 全过（13 包）。e2e 在 `tests/e2e_test.go`，用真实二进制/子进程/SQLite/HTML 驱动。覆盖率
  attrib 91.8% / report 89.6% / trace 87.5% / store 82.5% / session 79.7% / **runner 29.2%**（链式规划
  `chain_test.go` 把它从 8.9% 抬上来的）—— 编排行为仍主要靠 e2e，这是诚实的现状（编排器的正确层级就是 e2e）。
- **链式的可观察性只在 e2e 成立**：`tests/testdata/agents/chain.sh`（读 stdin 的 prompt，`*WRITE*` 写
  `handoff.txt` 并答 `wrote handoff.txt`，`*READ*` 答 `found: $(cat handoff.txt)` 或 `missing`）——「agent
  看见了上一步的文件」是单元测试看不见的事。不链式 → 第二格答 `missing` 判负，链式 →
  `found: the-first-turn-wrote-this` 判通过。
- e2e 的 payload 结构**刻意不复用** `report.Data`（否则字段改名测试会点头），也**刻意不声明** `generated`。解
  payload 找**第一个 `;</script>`**（后面紧跟内联 app.js）。
- fixture：`tests/testdata/agents/good.sh`（正确补丁）/`bad.sh`（各任务错法不同）；
  `examples/matrix-harness.yaml` 是可离线跑的 sweep 示例。

## pytest grader 的解释器

`AGENTEVAL_PYTHON`。managed python 3.13.12 与系统 python **都没有 pytest**。**本机专用 venv：
`~/.workbuddy/binaries/python/envs/aeval-pytest/bin/python`**（pytest 9.1.1）。早先记的 `envs/default` **已不
含 pytest** —— 那个 env 装了 pillow/pymupdf/pyobjc，是 WorkBuddy 自己的工具环境，别往上装。

跑法：`AGENTEVAL_PYTHON=…/aeval-pytest/bin/python go test ./...` —— 带上之后 test 包从 ~8s 变 ~60s，说明
pytest grader 真的在判分。`runCLI` **只在探测到带 pytest 时才导出**它（导出空值 → 回落 python3 → 报「没有
pytest」被误读成 agent 挂了）。**推论：任何断言「demo 三题全对」的 e2e 必须 `requirePytest(t)`**
（`TestCLIMatrixAttributesASweep` 曾漏，demo 2/3 靠 pytest 判分）。这个 env 会被 WorkBuddy 复用/清理，**缺了
症状是 e2e 静默 SKIP 而不是 FAIL —— 别把 SKIP 当通过**（`-v` 里看 `--- SKIP`）。

## 环境假象与沙箱

- **无头 Chrome 里 CSS 动画永不推进**（`document.timeline` 走但动画 `currentTime` 恒 0），`.drawer` 的 160ms
  淡入永远停在 `opacity:.4`，看起来像「半透明抽屉」—— 环境假象，**截图复核前先注入
  `.drawer{animation:none}`**。
- **沙箱内 `go test ./...` 必然红两条**：`tests` 包的 `TestReportDrawer*` 报 `Google Chrome: signal:
  trace/BPT trap`。根因是沙箱拒绝 Chrome 清理自己的临时 profile
  （`~/Library/Application Support/Google/Chrome-headless/scoped_dir*/…`、`code_sign_clone/…`）。**沙箱外**
  （`go test ./tests -run TestReportDrawer -count=1`）→ `ok`。别去查代码。
- **后台 shell 不继承这个放行**：变异验证脚本一旦 `run_in_background` 跑，全部报 `trace/BPT trap`，看起来像
  「全红」，其实一项都没被断言抓到。变异验证必须**前台**跑（放大 timeout，必要时
  `dangerouslyDisableSandbox`）。

## 变异验证的方法论

- **「红了」≠「被抓到了」**：要**按输出归因** —— 命中自己写的那句断言文案才算抓到，否则只能算环境红了。曾把
  「浏览器被 context 超时 kill」计成 catch 混进 4/4。脚本里做这层分类，别只看退出码。
- **要用「不破坏编译」的形状**：曾把 `return fold(a)==fold(b)` 改成 `return a == b`，`fold` 立刻成了未使用变
  量 → **build failed**，看起来「红了」其实是编译错，等于什么都没验。正确做法是让 `fold` 变恒等
  （`func(s string) string { return s }`）。同理：删 `AgentName: b.AgentName` 这种**字段赋值**是安全的变异，
  删**唯一使用点**会把变量变成未使用。

## 工具陷阱

- `--dump-dom` 输出**含注入脚本自身**，探针结果要 base64 回传，匹配字面量要运行时拼（`'[' + 'object '`），否
  则命中探针源码。
- **本机 grep 空结果先怀疑表达式**：BSD grep 不支持 `\|`，ERE 也不支持 `\s`/`\d`/`\b`（会**静默返回空**）。
  一律 `-E` 并避开这些转义；优先用 Grep 工具而不是 shell grep。

## gitignore 约定

根锚定的 `/workspaces/` 是跑评测的默认落盘处（`<db dir>/workspaces`，`--db` 默认时就是仓库根），每个 cell 是
目标仓库的完整拷贝（**内含嵌套 `.git`** → `git add -A` 会把它们当 gitlink 收进去并警告）。
`datasets/demo/workspaces/` 是数据集资产**必须保持被跟踪**，所以规则必须锚定根。
`datasets/local_claude_code/` 是 `collect` 产物（含绝对路径 + 真实会话文本），也忽略。不忽略 `datasets/mine/`
之类的任意目录 —— 那是猜命名，每加一个 `--out` 就补一行。
