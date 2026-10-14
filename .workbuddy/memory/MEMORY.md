# agenteval — 项目长期记忆

Go 的 agent 评测框架。`/Users/skyitachi/lab/agent_eval`，module `github.com/skyitachi/agenteval`，Go 1.25.1。
链路：数据集 → 可插拔 grader → SQLite → 自包含 HTML 报告。只有 Claude Code 一种 agent（`mock` 是测试替身）
，model 自由指定（透传 CLI）。**八条子命令**（`cmd/aeval/main.go`）：`collect`/`validate`/`run`/`matrix`/
`report`/`serve`/`list`/`version`。回答「做得怎么样 / 差异归因给谁 / 真实会话花了多少」，**拒绝**回答
「这个 agent 好不好」。

**细节在 `notes/`，本文件只留「不带着上下文就会踩坑」的规则。**
`collect.md`（来源/挑选/切片/`--chain-sessions`/`--workspace-cwd`）·
`reporting.md`（模型与 skills 两种 chip、app.js 客户端、Chrome 渲染护栏）·
`testing.md`（套件、pytest 解释器、沙箱与变异验证方法论、gitignore）·
`trace-and-schema.md`（token 记账、schema 版本表、数据集格式、patch 从磁盘反算）·
`environment-freezing.md`（真实仓库评测设计，未落码）。

## 三轴归因（OFAT）

- harness = 实现+配置；`model` 一轴；`skill` 一轴。一次 sweep 只动一条轴，**加载期强制**：level 改了本轴
  不拥有的键 → 拒绝（不是警告）。轴的所有权靠**反射读 `agent.Spec` 的 yaml tag** 推导
  （`internal/attrib/plan.go` 的 `specKeys`/`Axis.Owned()`）——写死会漏字段，用 `yaml.Marshal(零值)` 会因
  `omitempty` 退化成空集（踩过）。
- 每条 level 必须记录**改了哪些键**（`attrib.Meta.Changed` → run 的 `config_json`）：缺了它，读者分不清
  「这个 level 没改东西」和「差异没被记录」。
- 分离度用**精确符号检验**（log 空间求和，`0.5^n` 在 n=1074 处下溢）；**非平局配对 < 6 对时无论多偏都到
  不了 p<0.05**（写进每份报告的 caveat）。方向由 **win/loss 分裂**定，不由均值。

## 设计约定（改代码前先读）

1. **报告是「数据库的函数」**：渲染层只读持久化状态（`report/model.go`+`load.go` 把行投影成视图模型，
   **借用** `runner.Summary`/`attrib.Attribution` 而不镜像）。e2e 断言运行期报告与从 db 重生**逐字节相
   同**（比对时省掉 `generated`）。
2. **XSS 靠编码器不变量**：`render.go` 的 `neutralize()` 把 payload 的 `<` `>` `&` 重写成 `\u003c`/
   `\u003e`/`\u0026`（JSON 字符串外不含这三个字符，重写不改结构），一次封死三种逃逸。客户端默认
   `esc()`，只有 `raw()` 例外。
3. `html/template` 上下文敏感：注入 `<style>` 用 `template.CSS`、`<script>` 用 `template.JS`；用
   `template.HTML` 会被当文本**重新转义**（踩过）。
4. **`hidden` 必须由样式表显式兑现**：UA 的 `display:none` 被任何作者 `display` 覆盖
   （`.drawer{display:flex}` 直接作废它），空抽屉不报错、只是**不透明盖住右边 1000px**，看起来像「布局
   坏了」。加 hidden 元素必须同步加 `.cls[hidden]{display:none}`；`TestHiddenElementsCanActuallyBeHidden`
   守它。
5. `MaxInlineBytes`（默认 8 MiB）超了按优先级丢 detail（失败/出错留最久，skip 最先丢），降级记入
   `Warnings`。轨迹截断留**头尾 + gap marker**。
6. 退出码 `0/1/2/130`；**有任务失败 ≠ 命令失败**（否则 CI 没法用）。
7. 数据集/matrix 文档**就地解码**，未知键是**错误**（拼错的 `plugin_dirs` 会产出「skill 没用」的 sweep）。
   时长必须带单位。
8. 路径相对**声明它的文档所在目录**解析；经 CLI 传的（`--bin`/`--work-root`/`--session`/`--file`）相对
   **调用者 cwd**。
9. 「裸命令名 vs 路径」：值里没有 `/` → 当 PATH 上的命令原样保留，否则绝对化。唯一实现在
   `agent.ResolvePath`。
10. **测量值不重试**：timeout/非零退出/error result 都是**数据**。`retries` 只管进程起不来，默认 0。
11. agent 默认 **clean room**（无 user hooks / auto-memory / 任务外 MCP / CLAUDE.md）。**这是两条 flag 的
    默认值**：`--bare` 与 `--disable-slash-commands` **都默认 true**。把它们改成 false 会同时打破
    `TestCLIReadsTheSkillFlags` 与 `TestASlashCommandPromptThatCannotResolveIsNotScoredAsARun`（后者靠
    「skills 没加载」才会打出那个 slash-command 警告）——**本轮踩过，是无关的顺手改动**。
12. **位置参数可写在 flag 之后**：Go 的 `flag` 遇第一个非 flag 就停，`report --compare a b -o x` 会把
    `-o x` 当位置参数并报「got 4」；`positionals()` 从那个词起重新解析。**但重新解析必须在 `openStore`
    之前做**：曾经 `cmdReport` 先 `openStore(&common)`、到 `--compare` 分支里才 `positionals()`，于是
    `report --compare a b --db X` 的 `--db X` **来得太晚** —— store 已经开在默认路径上，两个 run 都被报成
    「not found in **aeval.db**」（错误信息点名的文件正是线索）。`--db` 写在 `--compare` 前面的形状一直是好
    的，而**本文件里所有既有 e2e 都是那个形状**，所以没人发现。现在 `trailing, code := positionals(fs,
    stderr)` 在 `openStore` 之前调一次，分支复用结果。护栏
    `TestCLIComparesTwoRunsHoweverTheFlagsAreOrdered`。
13. **工具打印的命令必须能跑**：`Next:` 提示漏 `--db` 就会给出必然失败的命令；`printSummary` 只在 `--db`
    非默认时补它、**按用户输入原样回显**，`hintArg` 给带空格的值加引号。**找不到记录必须点名文件**：
    `store.NotFoundError{Kind,ID,Path}` 保持 `errors.Is(err, ErrNotFound)` 同时打印 `store: run X not
    found in aeval.db`（「not found」像工具坏了，「not found in aeval.db」才指向 `--db` 给错）。
14. **schema 版本：往后拒绝、往前升级**。更高 = 拒绝（列含义不明）；更低 = **就地升级**（另一条路是扔掉付
    过钱的 run），发生在**任何一次 open**（含 `report`/`serve`）。`migrations[i]` = v(i+1)→v(i+2)，只追加；
    每步**必须能重跑**（`ADD COLUMN` 不幂等，在 DDL 与版本号之间被杀掉会报 "duplicate column name"）。
    **v6 只加表**：`reports` 由 `schema.sql` 的 `CREATE TABLE IF NOT EXISTS` 建立，`upgradeV5ToV6` **故意
    为空**（在此重复 DDL 就会成为第二份定义并漂移），但仍必须存在，否则 upgrade 链会报 "no path from
    it"。版本→列的速查表见 `notes/trace-and-schema.md`；**低版本夹具要把所有更高版本加的列一起删**，只删
    一列得到的是「再降一版」而不是那个版本。

## 报告登记 + HTTP 服务（`report` 写、`serve` 读）

`report` 每次渲染都往 `reports` 表写一行。前提仍是约定 1：报告是 db 的函数，所以登记行**只存无法重新推
导的东西**，不存第二份答案。

- **存「视图描述 + 载荷哈希」，不存渲染好的 HTML**（用户拍板）：`kind`/`title`/`subject`/`params_json`/
  `options_json`/`created_at` + `payload_hash`（= `sha256(DataJSON(payload))`）/`payload_bytes`/
  `data_version`。访问 `/r/{id}` 时**按需重渲染**，重算哈希与登记值比 → 横幅声明。
- **哈希覆盖的字节 == `/r/{id}.json` 返回的字节**（关键不变量）：`X-Aeval-Hash` / `X-Aeval-Hash-Matches`
  让脚本不经解析页面就能核对这个主张。改渲染就不能偷偷改哈希的输入。
- **append-only（log 而非 state）**：同一视图渲染两次写两行、两个哈希并列才是漂移的证据。id = 8 字节随机
  hex（16 字符）——不用序号（会被读成版本号），不用内容寻址（内容恰恰是两次登记会共享的东西）。
- **`options` 必须存「已 resolve」的值**：`Options.Resolve()`（原 `withDefaults`）导出，因为 `GeneratedAt`
  是文档的一部分（重放要用录制时刻而非当前时刻）。`ParseOptions` 对坏 JSON 或零 `GeneratedAt` **一律拒
  绝**，不猜。
- **只有 `report` 命令登记**（用户拍板）：`run`/`matrix` 顺手写出的报告不登记。
- **横幅在 shell 里、不在 payload 里**：`Record` 放 `RenderOptions`/`pageView`，渲染在 `<main>` **之外**
  （client 每次 render 会清空并重绘 `<main>`）。
- **失败页是页面而非纯状态码**：`RenderError` + `error.html`，**不带 app.js**（否则 client 会把「还没记录
  任何报告」画在失败原因之上）。共享 `shell-head` 模板靠 `shellHead` 结构承接，两个 view 都嵌
  `Head shellHead`。
- 路由（`net/http` 方法限定 + `r.PathValue`）：`GET /{$}`、`GET /r/{id}`、`GET /static/app.css`、
  `GET /static/app.js`、`GET /` 兜底。`.json` 后缀**就地剥离**（mux 只匹配整段）；`Derive` 失败区分
  `ErrNotFound` → `gone` 页（标题 **"The subject of this report is gone."**，含 subject 与登记时间）。
- `recordFor` 的**分支顺序是 Match 优先**：Match → info；否则 `DataVersion` 不同 → warn "Recorded in an
  older payload format."；否则 warn "This page is not the recorded document."（且**不选因**——行动了和投影
  代码变了服务端分不出）。
- `serve`：`--addr`（默认 `127.0.0.1:8080`）/`--limit`（默认 200）/`--title`/`--db`。**先 `net.Listen` 再打
  印**（绑不上就 exit 1 且不打印 "serving"）；非 loopback 打警告；SIGINT → `srv.Shutdown` 优雅停机后返回
  0。登记失败 = `fail`（记账失败就是失败，不像渲染那样可以降级）。
- **变异验证 9/9 确认变红**：`Fingerprint` 换输入；`store.SaveReport` 被跳过（`TestARegistrationRebuildsTo
  ItsOwnHash` + CLI e2e 两条各自抓到）；`recordFor` 把 Match 条件加上 `DataVersion` 相等（顺序变异）；
  横幅挪成 `Record: nil`；`SaveReport` 不再要求 kind；`ListReports` 改 `ASC`；`upgradeV5ToV6` 从 migrations
  摘掉；`.json` 后缀不剥离。
  **两条方法论坑（本轮都踩了）**：① 我第一版「跳过 SaveReport」的变异写成
  `if err := st.SaveReport(...); err != nil && false` —— **init 语句照样执行**，是语义 no-op，两次
  MISSED 全是变异写错、不是测试弱；② 变异脚本必须区分 **CAUGHT / MISSED / BROKEN(编译不过) / NOCHANGE
  (perl 没匹配上) / NOTRUN(测试名不存在)**，只看退出码会把编译错算成抓到。

## 已知缺口（勿当已完成）

- **容器后端整体不可达**：`internal/sandbox/docker.go` **无调用方、无测试**；runner 里 `Backend` 写死
  local，CLI/数据集都无字段。保留是刻意的，注释已指向二选一：接上并补 e2e，或删掉。
  `DockerConfig.Network` **没有默认值** → 空值 = 不传 = docker 默认（联网）。
- **`sandbox.Baseline.RestorePaths` 写了但没人用**：正是「判分前还原测试文件」的原语，**比「没有」更危
  险**（看起来做完了）。要完成是接线，不是重新实现。
- `llm_judge` 不在 e2e 覆盖内（e2e 必须离线）。
- **报告服务只读、无认证**：无登录、无写入路径、无按用户视图。默认绑 loopback，被要求监听别处时只打警
  告 —— 能连上端口就能读到所有已登记报告。
- **cursor 把「文件引用块」塞进 user bubble 的正文**：`--split turn` 于是把机器注入的上下文当成「prompt」
  的一部分。**已报给用户、未修**——修法是识别并剥离注入块，但规则写错会删掉真人写的内容，不该顺手做。
- **链式只覆盖 `run`，`matrix` 还没有这条路**：sweep 出来的每个 level 仍按普通方式规划。要做的话是接线同
  一套 `plan`。
- `Attempt.CommandLine` **仍无消费方**（未变）。~~`Baseline.Notes`（v3）~~ 与 ~~`agent_spec_json`（v4）~~
  两条盲区**已关**。

## 真实仓库评测

设计文档 `docs/real-repo-eval.md`（**细节以它为准**）+ `notes/environment-freezing.md`。结论：链路通了，
判分语义浅。P0：① pytest 无用例级期望（缺 FAIL_TO_PASS/PASS_TO_PASS 清单 + JUnit XML 逐用例解析）；
② `restore_paths` 只做了一半；③ 无 `oracle` 字段/任务自检 → `resolve_rate` 不可解释。已做对：
`workspace.git` 默认**剥离上游 `.git` 再重建 baseline**。最大风险不是跑不起来，而是**答案泄漏**（prompt 带
issue/PR 号）、**作弊空间**（改测试/conftest）、**flaky 测试被当成 agent 失败**。
