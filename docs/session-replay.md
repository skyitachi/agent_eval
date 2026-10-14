
# 网页浏览本机会话与回放（设计稿，未落码）

## 目标

在 `aeval serve` 的网页里：

1. 列出本机的 Claude Code 与 Cursor 会话，每个会话显示名字。
2. 查看单个会话的明细。
3. 点「回放」，选 harness、skill，填 model，然后在本机重跑这个会话。
4. 每次回放自动生成 report，登记进 `reports` 表，可以直接在 `/r/{id}` 在线打开。

## 已拍板的决定

- **选项来源：预设文件，model 自由填。**
  - harness 和 skill 只能从 `serve --replay-config <yaml>` 里的命名预设中选。
  - 没给文件时，内置两种 harness（`clean-room`、`host`）和一种 skill（`none`）。
  - model 是自由文本，预设文件可以给一份建议列表。
  - 网页上**不能**填 bin、extra_args、插件路径、MCP 配置。
- **开放方式：默认开启。** `serve` 一启动就能浏览会话、触发回放，不需要额外 flag。

## 路由

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/sessions` | 会话列表，Claude 与 Cursor 合并，按最近修改时间倒序，分页 |
| GET | `/sessions/{source}/{id}` | 会话明细和回放表单（`source` = `claude` 或 `cursor`） |
| POST | `/sessions/{source}/{id}/replay` | 提交回放，返回 303 跳到 `/replays/{rid}` |
| GET | `/replays` | 回放任务列表 |
| GET | `/replays/{rid}` | 单个任务的状态。运行中用 meta refresh 跟进；完成后链接到 `/r/{reportId}` 和对应 run；失败时显示原因 |

已有的 `/`、`/r/{id}`、`/static/*` 保持不变，导航栏加上「Sessions」和「Replays」。

## 会话列表与明细

- **Claude**：用 `session.Sources(DefaultRoot())` 列出会话，只读 stat 信息。名字取第一条人类 prompt 的前 N 个字，只读文件头部，并按 mtime 和 size 缓存。完整解析（`ParseFile`）只在打开明细页时做。
- **Cursor**：用 `session.CursorChats(CursorDefaultDB())` 列出会话，名字取 `CursorChat.Name`，为空时退回第一条 prompt。明细用 `ParseCursorChat`。
- **明细页内容**：
  - 来源、cwd、分支和 HEAD、是否 dirty
  - 时间、models、token、cost、busy 时长
  - 每条 prompt，以及回答它的轮次。轮次包括文本、thinking（折叠）和工具调用，工具调用显示名字和 input。
  - 正文一律经过 `html/template` 转义。payload 进 `<script>` 时沿用 `neutralize()`。
- 找不到数据源（目录不存在、Cursor 库读不到）时，显示一句提示，不报 500。

## 回放预设文件（`--replay-config`）

词汇沿用 matrix level 的部分 `agent` 块。路径相对于预设文件所在目录解析（设计约定 8）。

```yaml
harnesses:
  - name: clean-room
    agent:
      isolation: { bare: true, strict_mcp_config: true, disable_slash_commands: true }
  - name: host
    agent:
      isolation: { bare: false, strict_mcp_config: false, disable_slash_commands: false }
skills:
  - name: none
    agent: { plugin_dirs: [] }
  - name: my-skill
    agent: { plugin_dirs: [./skills/my-skill] }
models: [sonnet, opus]   # 只是表单里的建议，仍然可以自由填写
```

加载时的校验，失败就让 `serve` 启动失败（exit 2）：

- 未知键报错（`KnownFields(true)`），和 matrix 一样。
- harness 不能改 `model`、`fallback_model`、`effort`、`plugin_dirs`。skill 只能改 `plugin_dirs`。复用 `attrib.Axis.Owned()`。
- 提交时，skill 带了 `plugin_dirs` 而所选 harness 的 `disable_slash_commands=true`，直接拒绝，原因同 `checkSkillLevel`：skill 会被静默丢掉。
- 预设名必须唯一。表单只提交预设名，服务端按名字查，不接受表单直接传 spec 字段。

## 回放流程（服务端后台任务）

1. 校验请求：Host、CSRF、Origin、预设名、model 非空、会话存在。
2. 生成回放 id（随机 hex），建一行任务，状态为 `queued`。
3. 后台执行，同一时间只跑一个回放（`concurrency 1`，避免同时抢本机资源）：
   1. 解析会话，调用 `session.Collect`，`Machine` 填 `machine.Current()`，split 和是否 `--workspace-cwd` 由表单决定。
   2. 写数据集到 `<db 所在目录>/replays/<rid>/dataset`。
   3. 依次调用 `dataset.Load`、`runner.CheckMachine`、`runner.Preflight`。
   4. `runner.Run`：用副本，不用 in-place；`RunName` = `replay <会话短 id>`；Notes 记下预设名和 model。
   5. `report.Load` 之后调用 `report.Save`（kind `run`，params `{run: <id>}`），把 report id 写回任务行。
4. 任务状态：`queued → running → done | failed`。失败时记下错误原文。
5. `serve` 收到 SIGINT 时，取消正在跑的回放（ctx 传进 `runner.Run`），任务标记为 `interrupted`。

## 存储

- 新增 `replays` 表：id、source、session_id、harness、skill、model、split、workspace_cwd、status、error、run_id、report_id、created_at、finished_at。
- schema 升到 **v8**。表的 DDL 只写在 `schema.sql`（`CREATE TABLE IF NOT EXISTS`），`upgradeV7ToV8` 保持空函数但必须存在（同 v5→v6 的约定）。
- 重启后，仍处于 `queued` 或 `running` 的行改记为 `interrupted`，不自动续跑。

## 安全

回放会在本机以 `bypassPermissions` 启动 `claude`。默认开启意味着任何能发 HTTP 请求到这个端口的人都能触发它，所以以下几条必须做：

- **Host 校验**：只接受 `127.0.0.1`、`localhost`、`[::1]`（带端口）作为 Host，防 DNS rebinding。范围覆盖所有 `/sessions*` 和 `/replays*` 路由。
- **CSRF**：进程启动时生成随机 token，放进表单隐藏字段，POST 时校验。同时校验 `Origin` 或 `Referer` 与本服务一致。
- **不接受任意参数**：网页只能选预设名和填 model，bin、extra_args 和各种路径都只能来自本机的预设文件。
- **非 loopback 绑定**：待确认，见下。
- 无认证这一点不变，README 已知缺口里要写清楚：能连上端口就能读到本机所有会话内容。

## 待确认

- 绑定非 loopback 地址时：是直接禁用回放和会话浏览，还是只打警告？建议禁用。
- 回放数据集和工作区是否保留：默认保留数据集（体积小、可复现），工作区跑完即删。
- model 是否限制字符集：建议限制为 `[A-Za-z0-9._:/-]`，因为它会经过 shellQuote 进命令行。

## 测试计划

- server 单测：列表和明细的渲染（用 `tests/testdata/session/basic.jsonl` 和 Cursor 夹具库）；Host 不对返回 403；CSRF 缺失或错误返回 403；未知预设返回 400；skill 与 disable_slash 冲突返回 400。
- 预设加载单测：未知键、越权键、重名、相对路径的解析。
- 回放流程单测：用 mock agent（`--bin /bin/true`）完整跑一遍，断言生成了 run 行和 report 行，`/r/{id}` 能打开且 hash 匹配；再断言失败任务记录了原因、没有生成 report。
- store：v7 夹具升级到 v8（删 `replays` 表，版本写 7）。
- e2e：启动 `serve --addr 127.0.0.1:0`，按 GET 表单、POST、轮询状态、打开报告的顺序走一遍。
- 变异验证：去掉 Host 校验、去掉 CSRF 校验、跳过 `report.Save`、跳过 `CheckMachine`，每一项都应该让对应测试变红。

## 已知风险

- 本机 `~/.claude/projects` 下会话的数量和大小还没确认（统计命令被权限拦截），列表页从一开始就按分页和只读文件头来设计。
- Cursor 库被 Cursor 进程占用时会退回读副本（已有逻辑），列表可能比实际落后几秒。