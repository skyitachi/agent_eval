# 报告：它必须能证明的事（模型 / skills / chip）+ app.js 客户端

## 通用规则：chip 缺了值也必须画出来

**「已知为未知」是一个断言，不是空格。** 加 chip 时先问：这个值缺了的时候，页面是不是就变成了沉默。
（`model` 曾经只在标签非空时画 → 没传 `--model` 的 run 头部**什么模型都不说**；skills 的 `not recorded` 是
正确先例。）

## 模型（报告要说得比标签多）

**两个来源，两种主张**：`runs.agent_model` 是**命令行要什么**（可选 → **没传就是空**）；`attempts.trace_json`
里 init 的 `model` 是 CLI **宣告**打算用什么（在任何请求之前）；`result.modelUsage` 的**键**才是**真正计了
费**的模型。`trace.Trace.Models()` **优先后者**、空则回落前者，并**排序**（报告必须是记录的函数）；空 key 不
是模型。

- **不需要新列**：trace 本来就按 attempt 落库 → 在这之前录下的 run 也答得出来。`report/load.go` 的 `tasks()`
  本来就把 trace 解成 `trace.Trace`，只是以前只取 tools/events。
- 四态文案在 Go 里写（照 `Skills.Text` 的先例）：`not recorded` / `X (the stream named no model)` /
  `default (the CLI chose it): X`（**真实 run 最常见的一档**）/ `X, but the stream billed Y`（标红）。
- **别名不是替换**：`trace.SameModel` = 小写后**互相包含**。`--model sonnet` 会被自称
  `claude-sonnet-4-5-20250929` 的流回答，直接比字符串会让**每次普通 run 都报警**，而每次都报的警告 = 没人读。
  两边都空 = **不是**同一模型（两个方向都是编的）。
- **每个格子各报**（`Task.Model`，文案永远非空）：一次 run 未必只有一个模型，中途回落只出现在某条格的流里。
  头部的 `Observed` **收全部格子**的名字 —— 只取第一个有答案的格子会把「跑一半换了模型」恰好藏起来。格子上
  的 `(asked for X)` 是读者找到**是哪一格**的唯一线索。
- **比较也要比实际值**：声明的模型相同（或**都空**）而流里报的不同 → 点名「which model served the
  requests」。**声明不同时不打这句**（同一列两句 = 两句都不被读）。只有一侧流报了模型、且两边都没声明 → 另说
  一句「未知」并**点名沉默的那一侧**。
- **`mock` 答不了这个问题**：`synthesizeStream` **自己合成流并照抄被告知的模型**（空则写 `"mock"`），夹具的
  stdout 只被当答案文本。所以断言「页面上的模型 = 流里报的模型」的 e2e **必须走默认 `claude_code` 路径**
  （`--bin <夹具>`，夹具只读 stdin、忽略 argv）。

## skills：声明 ≠ 加载

`Spec.SkillsActive()` = 有 `plugin_dirs` **且** `!Bare && !DisableSlashCommands`（`EffectiveIsolation()`：nil
isolation 回落 `DefaultIsolation()`，而默认是 `{Bare:true, StrictMCPConfig:true, DisableSlashCommands:true}`
→ **普通 run 什么都不加载**）。所以「声明了一个目录、flags 没动」= inert，报告要写
`N plugin dir(s), not loaded (--bare …)`。

- `loadedSkills()` 只返回**活下来**的目录：两个 run 的声明只差 inert 项 → 是同一份 agent 配置，报成差异反而
  把唯一那个真差异淹掉。
- `comparisonWarnings` 一侧知道、另一侧不知道（老行）→ 报「run X recorded no agent configuration … so
  whether the two runs loaded the same skills is unknown」，**且要点名是哪一侧**（"one of these runs" 会让
  读者猜，而会猜错的那个方向正是「沉默的那侧没加载」）。
- **不是打常驻警告，是「有差异才点名」**；**零差异时不打任何话**（沉默会被读成「两边测的是同一件事」，但常驻
  警告更糟）。
- `sameAgent` 要**折叠命名**：capture 写 `claude-code`、runner 注册 `claude_code`，字面比会把每一次「采集的
  Claude Code 基线被自己回放」都标成跨 agent。空值 = 「没记录」不算「不同」。

### claude 的 skill 闸门（2.1.296 实测，帮助文本误导过我）

- `--bare` **和** `--disable-slash-commands` **各自独立**都能抹掉 skill（后者连 `--plugin-dir` 注入的也抹）。
  要 skill 必须两个都关，实际命令行**只剩 `--strict-mcp-config`** —— claude **不接受**
  `--disable-slash-commands=false`，aeval 是靠**不传这个 flag** 实现的。`--strict-mcp-config` 不碰 skill，只清
  用户 MCP。
- `--plugin-dir` 只吃**插件**目录（`.claude-plugin/plugin.json` + 同级 `skills/`）；给裸 skill 目录是**静默无
  效**。`skills` 做成**单个软链**指向 `~/.claude/skills` 可自动跟随新增；包成插件后 skill 名变
  `<plugin>:<skill>`。
- **零成本探针**：`claude -p --output-format stream-json --verbose --model <不存在的模型>` —— `system/init`
  在 API 调用**之前**发出，可白拿 `tools`/`slash_commands`/`mcp_servers`，随后 400。比读帮助文本可靠。
- 用户本机要求加载 `~/.claude/skills/`；代价：自己的 hooks、settings.json 的 env（覆盖 `ANTHROPIC_BASE_URL`
  → siliconflow）、auto-memory/CLAUDE.md 发现都回来。run 头打 `bare=false skills=on` 是唯一记录（报告里看
  不到 —— 缺口）。

## app.js（唯一没单测的一层）

同类错踩三次，规则写进了文件头注释：

1. **插进模板的值必须已转义或包 `raw()`**；裸 `tpl` 结果再插值 → **二次转义**。**已犯四次，最近两次都在任务
   表格里**：`Tools` 列（`int(n) + raw(badge)` → 整格被转义，页面印出 `6<span class="v fail" …>!</span>`）
   与**排序后的表头**（`arrow = tpl…` 没包 `raw`，`sortKey` 初值是 null，所以**只有读者点过表头才出现**）。
   共同点：**载荷全对、退出码 0、印出来的也不是 `[object `** —— 既有护栏一条都不响。正确形状是**整格一个
   `raw(tpl…)`**（见 `toolCell`）。定位法：`grep '+ raw('`（曾经只有一处）。
2. **节点只能 append，不能插值**（`tpl` 会 `String()` 它）—— 抽屉六个标签页全因此画成
   `[object HTMLDivElement]`；读代码看不出来，必须真渲染。
3. `raw()` 自带 `toString()`，否则 `'a' + raw(x)` 得到 `[object Object]`（baseline 行犯过）。
4. 兜底：`tpl` 对节点/普通对象输出 `⚠ unrendered <Ctor>` 的 `.bad` 标记 —— **错误必须不像数据**。
5. overview 的 `Final answer` 是**两栏**（this attempt / recorded (baseline)）—— 数字说不出两次尝试是否得出
   同一结论。录制没留答案时写一句话，不留空框。

### 护栏

`tests/render_test.go`，**唯一需要浏览器的测试**（找不到 skip，`AGENTEVAL_CHROME` 可覆盖）。探针注入 → **读
运行头部**（`.card.runhead` 的 `textContent`，`out.header`）→ 点每个 tab → 读回 DOM，断言字节数下限、无
`.bad`、无 `[object `、diff 有 `<span class="dl `、baseline 卡两个 delta 含录制总量、两栏答案都在。**表格也要
读回来**（`out.toolsCell`/`toolsBadges`/`tableMarkup`，以及 `sortedHead`/`sortedMarkup` —— 表头要**点一下再
读**，箭头只在排序后才存在）：`TestReportTaskTableShowsValuesNotMarkup` 断言 `toolsCell == "2!"`、徽章是元
素、表格与排序后的表头里都没有字面标签。夹具 `tool-errors.sh` 是**全套唯一会调用工具的那个**（2 calls / 1
error）—— 工具的计数列在没有工具调用的 cell 上只画 `-`，那个坏掉的格子永远不会被走到。

**为什么头部也要读回来**：chip 是 JS 运行时画的，**在 HTML 里 grep 到那个字符串只证明源码被装进去了**，证明
不了它被画出来 —— 这正是 `[object HTMLDivElement]` 当年潜伏的方式（文件/载荷/退出码全对、套件全绿）。同形状
的两条护栏：`TestReportHeaderSaysTheCellsWereChained`（链式徽章）与
`TestReportHeaderNamesTheModelTheStreamNamed`（**模型徽章**，跑一份不传 `--model` 的 run，断言头部含
`default (the CLI chose it): stand-in`）。删掉 app.js 里对应的那行 → 各自变红（已验证）。
