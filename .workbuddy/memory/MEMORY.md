# agenteval — 项目长期记忆

Go 的 agent 评测框架。module `github.com/skyitachi/agenteval`，Go 1.25.1。
链路：数据集 → 可插拔 grader → SQLite → 自包含 HTML 报告。只有 Claude Code 一种 agent（`mock` 是测试
替身），model 透传 CLI。八条子命令：`collect`/`validate`/`run`/`matrix`/`report`/`serve`/`list`/`version`。
回答「做得怎么样 / 差异归因给谁 / 真实会话花了多少」，**拒绝**回答「这个 agent 好不好」。

**本文件只留「不带着上下文就会踩坑」的规则，全部解释性细节在 `notes/`：**
`collect.md`（来源/挑选/切片/chain/workspace-cwd）· `reporting.md`（chip、app.js 客户端、Chrome 护栏）·
`testing.md`（套件、pytest 解释器、变异验证方法论）· `trace-and-schema.md`（token 记账、schema 版本表、
数据集格式）· `environment-freezing.md`（真实仓库评测，未落码）· `memory-archive-2026-11-02.md`
（本文件 2026-11-02 之前的完整长文，被压缩前原样存档，找不到细节先翻它）。

## 会话来源 / 归因（结论级）

- 三条来源：Claude Code（`~/.claude/projects` JSONL）、Cursor（`cursor.db`）、WorkBuddy
  （`~/.workbuddy/traces/*/trace_*.jsonl` OTel）。WorkBuddy trace 是**遥测非权威记录**：无 cwd、payload 可能
  截断、token 只有会话总量 —— 三处损失写进 `Session.BaselineNotes`；很多 trace 无开头 prompt 会直接被
  collect 跳过；解析只取开头一条 prompt，`--split turn` 也只产 1 个任务。
- **没有 `aeval replay` 子命令**。重放只经 `serve` 网页按钮，或两步 CLI：`collect --session ... --out DIR`
  再 `run --dataset DIR`。
- 三轴归因 OFAT：轴所有权靠**反射读 `agent.Spec` 的 yaml tag** 推导（写死会漏、`omitempty` 会退化成空集）；
  每条 level 必须记录 `attrib.Meta.Changed`；分离度用精确符号检验（log 空间），非平局配对 < 6 对到不了
  p<0.05；方向看 win/loss 分裂不看均值。

## 设计约定（改代码前先读）

1. **报告是数据库的函数**：渲染层只读持久化状态；e2e 断言运行期报告与从 db 重生逐字节相同（省 `generated`）。
2. **XSS 靠编码器不变量**：`render.go` 的 `neutralize()` 把 `<` `>` `&` 重写成 `\u003c` 等，一次封死三种
   逃逸；客户端默认 `esc()`，只有 `raw()` 例外。
3. `html/template` 上下文敏感：`<style>` 用 `template.CSS`、`<script>` 用 `template.JS`；`template.HTML`
   会被重新转义。
4. **`hidden` 必须由样式表显式兑现**：任何作者 `display` 都会作废 UA 的 `display:none`（`.drawer{display:flex}`
   就盖住了右边 1000px）。加 hidden 元素必须同步 `.cls[hidden]{display:none}`，`TestHiddenElementsCanActuallyBeHidden`
   守它。
5. `MaxInlineBytes`（默认 8 MiB）超了按优先级丢 detail，记入 `Warnings`；轨迹截断留头尾 + gap marker。
6. 退出码 `0/1/2/130`；**有任务失败 ≠ 命令失败**。
7. 数据集/matrix 文档就地解码，**未知键是错误**（拼错的 `plugin_dirs` 会产出「skill 没用」的 sweep）；时长
   必须带单位。
8. 路径相对**声明它的文档所在目录**；CLI 传的相对调用者 cwd。裸命令名 vs 路径见 `agent.ResolvePath`。
9. **测量值不重试**：timeout/非零/error 都是数据，`retries` 只管进程起不来。
10. agent 默认 clean room：**`--bare` 与 `--disable-slash-commands` 都默认 true**。改成 false 会同时打破
    `TestCLIReadsTheSkillFlags` 与 `TestASlashCommandPromptThatCannotResolveIsNotScoredAsARun`。
11. **位置参数可写在 flag 之后**，但 `positionals()` 必须在 `openStore` **之前**调一次（否则
    `report --compare a b --db X` 的 `--db` 来得太晚，store 开在默认路径）。护栏
    `TestCLIComparesTwoRunsHoweverTheFlagsAreOrdered`。
12. **工具打印的命令必须能跑**：`Next:` 提示漏 `--db` 就是必然失败的命令；找不到记录必须点名文件
    （`store.NotFoundError`，`«not found in aeval.db»` 才指向 `--db` 给错）。
13. **schema：往后拒绝、往前升级**。升级发生在任何一次 open（含 `report`/`serve`）；`migrations[i]` 只追加、
    每步必须能重跑；`upgradeV5ToV6` **故意为空**（DDL 只在 `schema.sql`），但仍必须存在否则链断。低版本夹具
    要把所有更高版本加的列一起删。
14. **verdict 可能事后才可达 → `aeval grade`（`internal/regrade`）**：只写 verdict 列（tokens/耗时是已退出
    进程的测量值，改它就是造假）。`grader.Sandboxed{RequiresSandbox()}` 让「要活工作区」由 grader 自己声明，
    离线重判**按名字拒绝**；新增这类 grader 必须实现它。重判成功要摘掉 runner 留下的 `; grade: ...` 段。
15. **阈值三层：默认 0.6 → task 声明 → `--threshold`（task 压过命令行；CLI 的 `--threshold` 压过默认）**。
    `grader.Threshold` 与 `runner.DefaultThreshold` 必须同时改（前者是重判无记录时的兜底，后者是 run 的默认）。
    task 级 `threshold` 进 `hashTask`（不同线 = 不同数据集），越界（<0 或 >1）是校验错误，0 = 未声明。
    重判**沿用行上记录的阈值**而不是今天的默认：行是「这份 verdict 按什么线量的」的记录，`--threshold` 才改线。
16. **一个 cell 两个阈值**：judge 卡片比的是它自己 config 的 cutoff（`judgeThreshold`=0.67），cell 比的是 run/task
    的线，两者可以同时一 green 一 red。`grader.SelfThresholded{OwnThreshold(config)}` + `DeclaredThreshold(spec)`
    是唯一来源：`collect` 把它写进 task.threshold，`verdictNotice` 用行里 `details.threshold` 把两个数字一起说出来
    （**只陈述 cutoff 与 verdict，不写「score≥cutoff 所以 pass」**——模型断言 `passed` 时会越过自己的算术，下限是
    cutoff 的一半；`details.model_passed` 记录那次断言）。

## 报告登记 + HTTP 服务

- 登记行**只存无法重新推导的东西**：`kind`/`title`/`subject`/`params_json`/`options_json`/`created_at` +
  `payload_hash`（= `sha256(DataJSON(payload))`）/`payload_bytes`/`data_version`。不存渲染好的 HTML，访问时
  按需重渲染并比对哈希 → 横幅声明。**哈希覆盖的字节 == `/r/{id}.json` 返回的字节**（`X-Aeval-Hash*`）。
- **append-only（log 而非 state）**：同一视图渲染两次写两行；id = 8 字节随机 hex（不是序号、不是内容寻址）。
  **只有 `report` 命令登记**，`run`/`matrix` 顺手写的报告不登记。
- `options` 存**已 resolve** 的值（`Options.Resolve()`，`GeneratedAt` 是文档的一部分）；`ParseOptions` 对坏
  JSON 或零 `GeneratedAt` 一律拒绝。
- 横幅在 shell 里、**不在 payload 里**（client 每次 render 清空重绘 `<main>`）。失败页是页面而非纯状态码，
  `error.html` **不带 app.js**；两个 view 都嵌 `Head shellHead`。
- `recordFor` 分支顺序 **Match 优先**：Match → info；否则 DataVersion 不同 → warn 旧格式；否则 warn
  "not the recorded document"（**不选因**）。
- `serve`：先 `net.Listen` 再打印；非 loopback 打警告；SIGINT 优雅停机返 0；登记失败 = `fail`。
- **静态资源 `Cache-Control: no-cache`（不是 max-age）**：页面是 `no-store`，资源若能免问缓存就会让「新页面
  + 旧客户端」共存 —— 修好的 grade 按钮因此看起来没生效 5 分钟。ETag 仍在，重验只是 304。守它的是
  `TestTheAssetsAreServedOnceAndRevalidated`。
- **一个写路径 `POST /r/{id}/grade`**（撤销只读承诺）：从该行已存 diff/answer 重判一个 cell。无认证/无登录；
  写路由三条守卫（Host 可答 / Origin 是本服务 / per-process form token），读路由不设防。
- **判分是异步的：判定留在 attempt，请求落 `grade_jobs`**。`Check`（= `planFor`）同步拒判，`Start` 落请求，
  worker（2 个）跑，303 立刻返回（23.8 s → 29 ms）。「一个 cell 只一个活请求」靠部分唯一索引
  `WHERE status IN ('queued','running')` 兜住，应用层检查只是礼貌的一半。进程死掉的 running 行由下次
  `serve` 启动 `AbandonGradeJobs` 收成 failed（停机写库要 `context.WithoutCancel`）。进行中页面靠
  **`<meta http-equiv="refresh" content="3">`**（无脚本、无状态）+ 行上 `grading…` pill（由 `data-grade-task`
  壳层属性喂给 app.js，**不进 payload**：payload 是被哈希覆盖的字节）。

## 客户端（`internal/report/assets/app.js`）—— 交互 bug 的教训

- **`form.submit()` 要求表单在文档里**：分离的表单被 Blink 取消，只留一条 console warning
  （`Form submission canceled because the form is not connected`），页面毫无反应 —— 看起来就像按钮坏了。
  `submitGrade()` 因此先 `appendChild` 再提交。**这是 2026-11-02 修的真实 bug**。
- 渲染类 bug 在 artifact 里看不见（文件、payload、退出码全对）：`tests/render_test.go` 用 Chrome
  `--dump-dom` + 注入探针，**探针必须真的「按下去」**（拦截 `HTMLFormElement.prototype.submit` 记录
  `isConnected`/action/字段），只断言「按钮画出来了」就会漏掉这类。Chrome 启动必须带 `--no-sandbox
  --disable-dev-shm-usage`，找不到浏览器跳过不失败（`AGENTEVAL_CHROME` 可覆盖）。
- **`--dump-dom` 只能读会 settle 的页面**：探针页若带 `MetaRefresh`（如 grade 进行中页面），Chrome 每 3 秒
  重载、加载永不完成，2 分钟 context 到点直接 SIGKILL（报 `signal: killed`，极像 OOM，**别误判**）。
  自我刷新页在浏览器测试里只能「不测」或「测 markup」（`TestAPageCanAskTheBrowserToComeBack`）。

## 已知缺口（勿当已完成）

- **容器后端整体不可达**：`internal/sandbox/docker.go` 无调用方无测试；runner 里 `Backend` 写死 local。
  `DockerConfig.Network` 无默认值（空 = 联网）。
- **`sandbox.Baseline.RestorePaths` 写了没人用**：正是「判分前还原测试文件」的原语，**比没有更危险**。要接线。
- `llm_judge` 不在 e2e 覆盖内（e2e 必须离线）。**grade 与 replay 的 judge 声明必须是同一个**：
  `serve --grader-config-file` 现在同时决定页面 grade 用哪个 judge（`server.Options.Judge`）；以前只有
  replay 拿到它，页面永远打 api.anthropic.com → 无 `ANTHROPIC_*` 就 409。无凭证的拒绝文案会点名
  **不调模型的替代**（`grader.Modelled` / `NeedsModel` / `freeGraders`），serve 启动横幅也会先警告。
  **reasoning 模型的思考与回答共用 `max_tokens`**：默认 2048
  在 `deepseek-flash` 上会得到「只有 thinking、无 text」的空回复（实测 thinking 6.5k–25k 字符随机），
  `examples/judge.yaml` 因此用 `max_tokens: 16384`；空文本 + `stop_reason=max_tokens` 现在有一句点明预算的报错。
  该端点的模型只有 `deepseek-flash` / `deepseek-v4-pro`（`deepseek-chat` 是别名，`/models` 不列）。
- 报告服务：能连上端口就能读所有登记报告并给任何 run 报告的 cell 重判（要花钱）。
- cursor 把「文件引用块」塞进 user bubble 正文，`--split turn` 会把它当 prompt（**已报用户、未修**）。
- 链式只覆盖 `run`，`matrix` 还没有这条路。`Attempt.CommandLine` 仍无消费方。

## 真实仓库评测

设计以 `docs/real-repo-eval.md` 为准。链路通了，判分语义浅。P0：pytest 无用例级期望（缺
FAIL_TO_PASS/PASS_TO_PASS + JUnit 逐用例解析）、`restore_paths` 只做一半、无 `oracle` 字段 → `resolve_rate`
不可解释。已做对：`workspace.git` 默认剥离上游 `.git` 再重建 baseline。最大风险是**答案泄漏**（prompt 带
issue/PR 号）、**作弊空间**（改测试/conftest）、**flaky 测试当成 agent 失败**。
