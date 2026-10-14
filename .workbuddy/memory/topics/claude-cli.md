# claude CLI 闸门 与 skill 轴（细节）

> 属于 `MEMORY.md` 的展开。改 `internal/agent`、`internal/attrib` 前读本文件。

## claude 的 skill 闸门（2.1.296 实测，官方帮助文本误导过我）

- `--bare` **与** `--disable-slash-commands` **各自独立**都能抹掉 skill（后者连 `--plugin-dir` 注入的
  也抹）。要 skill 必须两个都关（claude **不接受** `--disable-slash-commands=false`，aeval 靠**不传**
  实现）。`--strict-mcp-config` 不碰 skill，只清用户 MCP。clean-room 默认
  `{Bare,StrictMCPConfig,DisableSlashCommands}` 全 true = **什么都不加载**。
- **声明 ≠ 加载**：`Spec.SkillsActive()` = 有 `plugin_dirs` **且** `!Bare && !DisableSlashCommands`；
  `SkillsReason()` 说清是哪个 flag 抹的；`loadedSkills()` 只返回活下来的目录。报告 chip：
  `none declared` / `N plugin dir(s)` / `N plugin dir(s), not loaded (--bare…)`。
- `--plugin-dir` 只吃**插件**目录（`.claude-plugin/plugin.json` + 同级 `skills/`），喂裸 skill 目录
  **静默无效**；包成插件后 skill 名变 `<plugin>:<skill>`。**一个 plugin dir 带的是它 `skills/` 下全部
  skill，flag 层选不了单个** → 要比单个 skill 得造只含它的插件目录（软链即可）。
- **零成本探针**：`claude -p --output-format stream-json --verbose --model <不存在的模型>` ——
  `system/init` 在 API 调用**之前**发出，可白拿 `tools`/`slash_commands`/`mcp_servers`，随后 400。
- 本机 `bare=false` 时 `~/.claude/skills/`（`edu-analytic-geometry`/`edu-solid-geometry`）与插件
  `~/lab/edulab` 会一起进来；用户的 hooks / settings.json 的 env / auto-memory 也回来。

## skill 轴比对：已知的静默失效（2026-10-12 复现）

`matrix --dry-run` 对以下形状**全部 exit 0、plan 表里都写 `varies plugin_dirs`**：

| base 的 isolation | 实际发生 |
| --- | --- |
| 完全不写（→ 默认 `bare:true`） | plugin_dirs **inert**，plan 无任何警告 |
| `bare: true, disable_slash_commands: false` | 同上，**guard 不响** |
| `bare: false, disable_slash_commands: false` | 唯一真的加载 skill 的形状 |
| `disable_slash_commands: true` | **被拒绝**（`checkSkillLevel` 命中） |

- 根因：`checkSkillLevel`（`internal/attrib/plan.go`）判的是 `l.Agent.Isolation != nil &&
  l.Agent.Isolation.DisableSlashCommands`，**既没看 `Bare`、也没用 `EffectiveIsolation()`**，而 `Bare`
  的默认值恰恰是 true。于是「本轴看起来改了东西、实际什么都没加载」的 sweep 能一路跑到付钱。
- 报告侧是诚实的（chip 写 `1 plugin dir(s), not loaded (--bare removed them)`），但那是**跑完之后**；
  plan 阶段与 run 头都没有。**gap 是「plan 沉默」不是「报告撒谎」。**
- 修法：guard 改用 `EffectiveIsolation()` 并同时检查两个 flag。
