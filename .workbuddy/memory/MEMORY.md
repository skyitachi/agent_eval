# agenteval — 项目长期记忆

Go 实现的 agent 评测框架与 pipeline。路径 `/Users/skyitachi/lab/agent_eval`，
module `github.com/skyitachi/agenteval`，Go 1.25.1。

## 定位

跑数据集 → 可插拔 grader 打分 → SQLite 持久化 → 自包含 HTML 报告 + Web UI。
当前只实现 Claude Code 作为 agent（**model 自由指定**，透传给 CLI）。
四条链路：`aeval run` → `aeval report` / `aeval serve` / `aeval compare`。

## 不可动摇的设计约定（改代码前先读）

1. **报告是「数据库的函数」，不是「进程的函数」**。
   两周后从拷贝的 `.db` 生成的 HTML 必须与当时一致 → 渲染层只读持久化状态，
   先把 storage 行投影成视图模型（`internal/report/model.go` + `load.go`），
   HTML / Web / 对比页全部基于该投影。HTML 内嵌 payload 与 `/api/runs/{id}` 的 JSON
   由同一路径产出，有测试断言**逐字节相同**。

2. **XSS 靠编码器不变量解决，不靠逐处转义**。
   `internal/report/render.go` 的 `neutralize()` 把 payload 中的 `<` `>` `&`
   重写为 `\u003c` `\u003e` `\u0026`（JSON 语法在字符串外不含这三个字符，重写不改文档结构），
   一次性封死提前闭合 `</script>`、开启 `<!--`、开新标签三种逃逸。
   客户端 JS 里插值默认走 `esc()`，只有 `raw()` 显式包装的片段例外。

3. **`html/template` 上下文敏感转义**：注入 `<style>` 要用 `template.CSS`，
   `<script>` 用 `template.JS`。用 `template.HTML` 会被当 CSS/JS 文本**重新转义**（踩过）。

4. **报告体积有预算（`MaxInlineBytes` 默认 8 MiB）**，超了按优先级丢 per-task detail：
   失败/出错的 cell 保留证据最久，通过的先丢，skip 最先丢；每次降级都记入 `Warnings`。
   轨迹截断保留**头尾 + 显式 gap marker**（头承载任务框定，尾承载答案）。

5. **退出码**：`0` 成功 / `1` 操作失败 / `2` 命令行错误 / `130` 中断。
   **有任务失败 ≠ 命令失败** —— 否则 CI 里没法用。

6. **配置就地解码进 `engine.Config`**，不做镜像结构（消除字段漂移）；
   `KnownFields(true)` 严格模式拒绝拼错的键；时长必须带单位；
   env 展开只作用于 `run.agent.env` / `run.backend.env`。

7. **所有路径相对「配置文件所在目录」解析**（dataset / database / report.dir /
   work_root / agent.bin），不跟 shell 的 cwd —— 否则同一份配置在不同目录下行为不同。
   经 CLI 传入的路径（`--bin` / `--work-root`）则相对**调用者 cwd** 解析。

8. **「裸命令名 vs 路径」的判别规则**：值里没有 `/`（或 `filepath.Separator`）→ 当作要在
   PATH 上找的命令（如 `claude`），**原样保留**；否则当作路径绝对化。
   见 `absFlagPath()`（cmd）与 `looksLikePath()`（config）。

9. **测量值不重试**：timeout、非零退出、error result 都是**数据**。
   `retries` 只管「agent 进程完全起不来」，且默认 **0**。

10. **agent 默认 clean room**：无 user hooks、无 auto-memory、无任务外的 MCP、
    不发现 `CLAUDE.md`。会继承操作者 `~/.claude` 的评测在别的机器上不可复现。

## Web 威胁模型

默认只绑 loopback；破坏性路由（删除 run）默认关闭，需 `--allow-delete`；
拒绝携带外部 `Origin` 的请求。本地单用户工具，但会渲染模型生成文本且能删数据。

## 数据集格式要点

- `dataset.yaml`（manifest：`tasks_file` 或内联 `tasks`、`defaults` 合并、`filter` 收窄）+ `tasks.jsonl`。
- `workspace`：`files`（`assets` 目录 + `files` 字面量）或 `git`（`repo`/`ref`/`subdir`）；`setup` 在 agent 前跑。
- `output.type`：`patch` / `files` / `stdout` / `none`；`paths` / `ignore_paths` 收窄 diff。
- `graders[].weight` 是**指针**，用于区分「未设（=1）」与「显式 0（只跑不计分）」；`required: true` 直接判负整个 task。
- 内建 grader：`patch` / `pytest` / `exact_match` / `llm_judge` / `command`。
- 指标：34 个 run-level，默认集是精选子集（`aeval registry` 为准）；
  分组 outcome/cost/token/effort/latency/health，**health 组单列**是因为「harness 没测到」和「agent 做得差」不能平均到一起。
- 自定义指标三形态：aggregate（`where` + `field` + `agg`）、ratio（`numerator`/`denominator`/`zero_denominator`）、
  expr（可引用已算出的指标）。`direction`/`group` 是**呈现提示**，不是装饰——自定义名字没有语义保证，
  把增长标成绿色比不标色更糟。

## 测试与运行

- `go test ./...` 全过（13 包）。e2e 在 `tests/e2e_test.go`，用真实二进制驱动。
- **pytest grader 需要带 pytest 的解释器**：`AGENTEVAL_PYTHON=<path>`。
  本机 managed python 3.13.12 **没有** pytest；可用的是
  `/Users/skyitachi/.workbuddy/binaries/python/envs/default/bin/python`（pytest 8.4.1）。
  README 用 `$(python3 -c 'import pytest, sys; print(sys.executable)')` 探测，失败即报错。
- 演示 fixture：`tests/testdata/agents/good.sh`（正确的补丁）与 `bad.sh`（错误的补丁）。
- **`aeval compare` 的位置参数必须放在所有 flag 之后**（Go `flag` 遇非 flag 停止解析）。

## 已知缺口（勿当成已完成）

- docker 后端只有单测，**未**在有 daemon 的机器上端到端跑过。
- `llm_judge` 不在 e2e 覆盖内（e2e 必须离线），需可达端点。
- 只有 Claude Code 一种 agent 实现。

## 真实仓库评测（设计已写，代码未实现）

场景：**真实代码仓库 + 一段任务 prompt**。设计文档：`docs/real-repo-eval.md`（2026-10-09）。

结论：**链路是通的，判分语义是浅的。** P0 三项缺口（均未实现）：

1. **pytest 无用例级期望** —— 只有 `paths`（文件级）+ `min_passed`/`max_failed`（计数级），
   缺 FAIL_TO_PASS / PASS_TO_PASS 的**用例清单**语义；真实仓库测试数会波动，计数阈值不可靠。
   另需 JUnit XML 解析逐用例结果（现在是正则抓汇总行）。
2. **无 `restore_paths`** —— 只有事后否决 `forbid_changed_paths`，没有评测前把测试文件
   checkout 回 baseline（SWE-bench 的默认做法：测「能力」而非「服从性」）。两者用途不同，都要有。
3. **无 `oracle` 字段、无任务自检** —— 参考解只能塞 `metadata`（引擎从不解释）。
   缺「可解性 / 必要性 / 稳定性」三检 → `resolve_rate` 无法解释（分不清任务难还是任务坏）。

已确认的现状（改之前先看 `docs/real-repo-eval.md` §1 的完整表）：

- `workspace.git` 默认**剥离上游 `.git` 再重建 baseline**，防 agent 从历史里读修复 —— **已做对**，
  注释里明写了这个意图。
- `setup` 之后自动 re-baseline → 依赖安装不算 agent 的改动。
- `backend.image` / `network` 是 **run 级**，无 per-task 覆盖；真实仓库需要版本隔离。
- docker 默认 `network: none`，但 `workspace.setup` 在沙箱内跑 → **装依赖与 hermetic 直接冲突**；
  推荐出路是「依赖烘进镜像」，让 `none` 真正成立。
- clone 每个 cell 一次（在 **host** 上跑 `runHost`，不在容器内），无 mirror 缓存 → 大规模的成本瓶颈。
- 真实仓库评测最大的风险不是跑不起来，而是**答案泄漏**（prompt 里带 issue 链接/PR 号）、
  **作弊空间**（改测试 / conftest / 配置文件）、**flaky 测试被当成 agent 失败**。
