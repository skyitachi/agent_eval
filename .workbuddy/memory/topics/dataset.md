# 数据集与 manifest 格式

> 改 `internal/dataset/` 之前读这里。

- `dataset.yaml`（`tasks_file` 或内联 `tasks`、`defaults` 合并、`filter`）+ `tasks.jsonl`。
- **文档就地解码，未知键是错误** —— 拼错的 `plugin_dirs` 会产出一个「skill 没用」的 sweep。
- 时长必须带单位。
- 路径相对**声明它的文档所在目录**解析；经 CLI 传的（`--bin`/`--work-root`/`--session`/`--file`）
  相对**调用者 cwd**。「裸命令名 vs 路径」：值里没有 `/` → 当 PATH 上的命令原样保留，否则绝对化
  （唯一实现 `agent.ResolvePath`）。
- `workspace`：`files`（`assets` + 字面量）/ `git`（repo/ref/subdir）/ **`inplace`**；`setup` 在 agent
  之前跑。`output.type`：`patch` / `files` / `stdout` / `none`。
- `graders[].weight` 是**指针**：区分「没设 = 1」与「显式 0 = 只跑不计分」。`required: true` 判负整个
  task。内建：`patch` / `pytest` / `exact_match` / `llm_judge` / `command`。
- **`session` / `prompt_index` 是一等任务字段**（`dataset.TaskSpec`，紧跟 `Baseline`），**不走
  `metadata`** —— metadata 的契约是「引擎从不解释」，而这两个引擎必须读。手写任务两个都不写 = 自成一条
  链，这正是「不带 flag 时数据集行为不变」的机制。
- 指标两层：run 头部展示值 + 归因七个配对指标（`pass_rate` 为 `Primary`），各带 `LowerIsBetter`。
  **timeouts 与 harness_errors 恒显（即使为 0）**。旧的 34 指标注册表 + 自定义 DSL **已删**。
