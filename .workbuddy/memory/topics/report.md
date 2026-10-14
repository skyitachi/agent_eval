# 报告：模型归属 / token 记账 / app.js（细节）

> 属于 `MEMORY.md` 的展开。改 `internal/trace`、`internal/report`、`app.js` 前读本文件。

## 「用的是哪个模型」（报告必须说得比标签多）

- **两个来源，两种主张**：`runs.agent_model` 是**命令行要什么**；trace 里 init 的 `model` 是 CLI **宣告**
  打算用什么，`result.modelUsage` 的**键**才是**真正计了费**的模型。`trace.Trace.Models()` **优先后者**、
  空则回落前者，并**排序**；空 key 不是模型。
- chip **永远画**，四态文案在 Go 里写：`not recorded` / `X (the stream named no model)` / `default (the
  CLI chose it): X`（最常见）/ `X, but the stream billed Y`（标红）。
- **别名不是替换**：`trace.SameModel` = 小写后**互相包含**（`--model sonnet` 会被自称 `claude-sonnet-4-5-…`
  的流回答）。两边都空 = **不是**同一模型。**每个格子各报**，头部 `Observed` 收全部；格子上的
  `(asked for X)` 是找到是哪一格的唯一线索。
- **比较也要比实际值**：声明相同（或**都空**）而流里报的不同 → 点名「which model served the requests」；
  **声明不同时不打这句**。只有一侧流报了模型 → 另说「未知」并**点名沉默的那一侧**。
- **`mock` 答不了这个问题**：断言「页面上的模型 = 流里报的模型」的 e2e **必须走默认 `claude_code` 路径**
  （`--bin <夹具>`，夹具只读 stdin、忽略 argv）。

## token 记账（最容易错的地方）

**一次 API 调用的用量 ≠ 一次 run 的用量。** `result` 的 `usage` 是**那一次调用**；**会话累计在
`modelUsage`**（**驼峰**！fixture 写成 `model_usage` → 空 map → 测试静默空转）。单 result 时两者**有时
恰好相等**，所以这个错能潜伏很久（报告 18,540 vs 真值 164,419，**少 89%**）。

- `trace.Trace.Usage()` **优先求和 `modelUsage`**，空则回落 `usage`；`Attempt.Usage()` 只许走它。
- **多个 result 事件是同一会话的累计快照**：按「总量最大」合并（幂等）；逐字段 last-wins 会让更小的快照
  抹掉更大的。但**描述答案的字段相反** → 取最后一个。
- `result.num_turns` **不是会话轮数**；`Trace.Turns()` 优先用流推导的 `TurnCount`（按 assistant message
  id 去重）。`trace_json` **落库不截断** → 历史行真值可事后重算，但**没有 backfill 命令**。
- **子代理造成两种不平衡**：`s.Usage`（含 sidechain）≠ `sumTurns(s.Turns)`（不含）；两侧都**没记录「跑过
  子代理」**这件事。
- 护栏：`internal/trace/testdata/multi_result_stream.jsonl` 是**真实击穿我们的那条流**。

## 报告客户端（app.js，唯一没单测的一层）

1. 插进模板的值必须已转义或包 `raw()`；裸 `tpl` 结果再插值 → **二次转义**。**已犯四次，最近两次都在
   任务表格里**：`Tools` 列整格被转义（页面印出 `<span class="v fail" …>!` 字面量）与排序后的表头
   （`Task <span class="arrow">▲</span>`，**只有读者点过表头才出现**）。共同点：载荷全对、退出码 0、
   印出来的也不是 `[object `，既有护栏一条都不响。正确形状是**整格一个 `raw(tpl…)`**。定位法：
   `grep '+ raw('`。
2. **节点只能 append，不能插值**（`tpl` 会 `String()` 它）——抽屉六个标签页全画成
   `[object HTMLDivElement]`；读代码看不出来，必须真渲染。
3. `raw()` 自带 `toString()`。兜底：`tpl` 对节点/普通对象输出 `⚠ unrendered <Ctor>` 的 `.bad` 标记
   ——**错误必须不像数据**。
4. overview 的 `Final answer` 是**两栏**（this attempt / recorded (baseline)）；录制没留答案时写一句话，
   不留空框。
5. **chip 通用规则：「已知为未知」也必须画出来**（`not recorded` 是一个断言，不是一个空格）。加 chip 先
   问：这个值缺了的时候页面是不是就沉默了。

护栏：`tests/render_test.go`，**唯一需要浏览器的测试**（找不到 skip，`AGENTEVAL_CHROME` 可覆盖）。探针
注入 → **读运行头部**（`.card.runhead` 的 `textContent`）→ 点每个 tab → 读回 DOM。**为什么头部也要读回
来**：chip 是 JS 运行时画的，**在 HTML 里 grep 到那个字符串只证明源码被装进去了**。
