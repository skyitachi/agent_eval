# 三轴归因（OFAT）

> `aeval matrix` 的语义与数学。改 `internal/attrib/` 之前读这里。

- harness = 实现 + 配置；`model`、`skill` 各一轴。**一次 sweep 只动一条轴**，加载期**拒绝**（不是警告）。
- 轴的所有权靠**反射读 `agent.Spec` 的 yaml tag**（`Axis.Owned()`）推导 —— 手写会漏字段，而
  `yaml.Marshal(零值)` 会因为 `omitempty` 退化成空集。
  归属：model = `{model, fallback_model, effort}`，skill = `{plugin_dirs}`，**其余全归 harness**
  （新字段默认落 harness，不会凭空变成第二个自由轴）。
- 每条 level 必须记录**改了哪些键**（`Changed` → `runs.config_json`）：缺了它，读者分不清
  「这个 level 没改东西」和「差异没被记录」。
- 分离度用**精确符号检验**（log 空间求和；`0.5^n` 在 n=1074 下溢）。**非平局配对 < 6 对时无论多偏都
  到不了 p<0.05** —— 这句话写进每份报告的 caveat。方向由 **win/loss 分裂**定，不由均值。
- 只警告**值得说**的：控制 level 一个字都没改是本职，警告它是噪音。
- **链式只覆盖 `run`，`matrix` 还没有这条路**；**inplace 同样未接 `matrix`**。要做是接线同一套 `plan`，
  不是重写。
