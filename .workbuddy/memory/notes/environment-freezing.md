# 真实仓库评测 / 环境固化（设计，未落码）

## 现状与缺口

设计文档 `docs/real-repo-eval.md`（含 §1 现状表）—— **细节以该文档为准**。结论：链路通了，判分语义浅。

- **P0** ① pytest 无用例级期望（缺 FAIL_TO_PASS/PASS_TO_PASS 清单 + JUnit XML 逐用例解析）；②
  `restore_paths` 只做了一半；③ 无 `oracle` 字段/任务自检 → `resolve_rate` 不可解释。
- **已做对**：`workspace.git` 默认**剥离上游 `.git` 再重建 baseline**（防 agent 从历史读修复），`setup` 后自
  动 re-baseline。
- **瓶颈**：每 cell clone 一次、无 mirror 缓存。
- **最大风险不是跑不起来**，而是**答案泄漏**（prompt 带 issue/PR 号）、**作弊空间**（改测试/conftest）、
  **flaky 测试被当成 agent 失败**。

## 环境固化设计：`workspace.type: snapshot`

**前提事实**：`dataset_hash` 哈希的是 resolved task 内容（`loader.go:339` 把 `Workspace` 整个 marshal 进
去），**不含磁盘文件** → 只要快照**按内容命名**，环境就被现有哈希覆盖 → **不需要新 schema 列**。

- 环境就是一个文件：`snapshot: snapshots/<hash>.tar.zst`，**文件名即内容哈希**。
- `--workspace-cwd` 的语义从「引用活目录」变为「**冻结**」；`--snapshot-mode freeze|reference`（freeze 默
  认）。
- **两层划分（支点）**：`base` —— git 用 `git archive HEAD`（会话前干净树；会话的活儿在 worktree 不在提交
  里）；普通目录整棵树（**必然是后态**，没有更早版本可退）。`dirty` —— 已修改 + 未跟踪 + 被忽略文件；git 场景
  **默认不带**（那恰是会话自己的改动 = 答案），普通目录**只能带**并如实声明。
- **打包规则（可测）**：路径排序、`uid/gid/mode/mtime` 归一（否则内容寻址失效）、保留 symlink 与可执行位、
  **永远排除 `.git`**、体积上限（超限明确报错，不是静默截断）。
- **无 `.git` 用例**解包后走 `InitBaseline` 建仓 → 两种目录在回放侧收敛到同一条路径。
- **固化解决漂移、不解决泄漏**：采集发生在会话结束后 ⇒ 快照必然是后态。分三档可信度（git 干净 / git 有改动 /
  普通目录）；普通目录给 `--snapshot-pre-session` 用 `EditedFiles` 反向剥离，剥不动**必须说出来**。
- **顺带意义**：`--snapshot-layer post-setup` 把 `.venv`/`node_modules` 冻进快照 → local 后端也能
  hermetic（`real-repo-eval.md` §3.2 的张力），代价是平台绑定（记 OS/arch，不一致就拒绝）。
- **接口（窄）**：`internal/sandbox/snapshot.go` 新增 `CaptureOptions{Layer, Exclude, MaxBytes}`、
  `Capture(ctx, sb, opts) (*Snapshot, error)`、`(*Snapshot).Restore(ctx, sb) error`，二者都吃 `Sandbox`
  接口 → 天然双后端通用（`Materialize` 四个分支**都只碰 `sb.Root()`**）。
- **落地顺序**：P0 `WorkspaceSnapshot` + `materializeSnapshot` + collect freeze + validate（**校验文件存在
  与内容哈希** —— 因为 `dataset_hash` 只哈希路径串，**不校验文件本身**）+ notes 声明 + **e2e「collect 后删掉
  原目录再 run」**；P1 dirty/post-setup 两层 + 反向剥离 + 平台指纹 + 报告 chip（要 schema v6 加
  `runs.workspace_*` 承载泄漏声明）；P2 容器内采集（`docker exec tar`）+ `--backend local|docker` + per-task
  `image`。

**为什么 `dirty` 的默认是「不带」**：agent 直接改源文件，**磁盘上不存在独立 patch** —— 改动就长在目录里，所以
会话结束时的目录状态 = 初始环境 + 答案，二者不可分离。少收 → 起点偏早，是个**可测量、可披露**的偏差；多收 →
静默把答案塞进去，测量直接失效。**宁可少收。**

`dirty` 其实有两类，而 collect **分不开**它们：会话**开始前**就存在的未提交改动 = 任务的前提（该收）；会话
**自己造成**的改动 = 答案（绝不能收）。转录只记**分支名**不记 commit，也没有文件系统历史，采集时只有一个后
态。`Baseline.EditedFiles` 也不能承担剥离责任：它来自转录的 `file-history-snapshot` →
`Snapshot.TrackedFileBackups`（`parse.go` / `cursor.go`），即 **Edit/Write 工具碰过的被跟踪文件**，**Bash 写
入**（`echo > f`、脚本产物、`git apply`）全不在里面，且它是**累计集合**不是逐轮 delta。

## 采集若跑在 docker sandbox 里会更简单吗

**先纠正前提：今天的 docker 后端不是「环境在容器里」，而是「执行在容器、环境在宿主」** ——
`DockerConfig.Root` 是 bind-mount 的宿主目录，`Image` 只负责「含 agent CLI 和依赖」，`start()` 即
`docker run -v host:container … image sleep infinity`。所以**沿用今天的模型，采集跑在容器里一点都不解决问
题**（会话改的仍是宿主目录，三条毛病原封不动）。

**有用的那种模型**：环境 = 镜像层（不是 bind-mount），起点锚在 `docker run` 那一刻，会话写入落在可写层。于是
「会话前」= 镜像（digest 内容寻址）、「会话改动」= 可写层 —— **两者物理分层**，snapshot 设计里最复杂的三块
（dirty 层、`EditedFiles` 反向剥离、三档可信度）**直接不需要**。真正让回放简单的**不是 docker，而是「起点在
会话开始前就被固化」**（这个约束不用 docker 也能达到，docker 让它**结构性**成立而非靠纪律）。

**不会自动变简单的**：① 转录里**没有**「会话是从 `image X@digest` 起的」这条锚点 —— 采集侧要新增「记录并证
明起点」的职责；② 只有跑在**新起的**容器里才干净（长期运行的开发容器可写层混着全部历史 → 又回到分不开）；
③ volume / env / home 配置（`~/.claude/skills`、`.gitconfig`）在镜像之外；④ 平台绑定（amd64/arm64）与体积。
落点：`type: snapshot` 保留为**降级路径**，新增 `type: image`（或让 docker 后端直接以 image 为根）；采集契约
从「打包文件」变成「**记录锚点 + 证明**」。

## 业界怎么设置大规模评测的回放环境（2026-10 联网核实）

**结论几乎统一：环境 = 「一个预构建容器镜像 + 一个固定 commit」，回放 = 拉镜像 → 起容器 → checkout。**

- **SWE-bench**：每实例一个预构建镜像 `sweb.eval.x86_64.<id>`，repo 已在 `/testbed` @ `base_commit`，conda
  env 装好。**三级镜像 key 做分层缓存**：`base_image_key`（语言基础）→ `env_image_key`（按环境**哈希**，可命
  中缓存）→ `instance_image_key`（实例专属）—— 几千实例不必几千个完整镜像，这是可规模化的关键。实例字段区分
  **`environment_setup_commit`（装依赖用）与 `base_commit`（评测起点）**。2024-06 全面容器化。patch 应用三级
  宽容度：`git apply` → `git apply --reject` → `patch --fuzz=5 -p1`。
- **Terminal-Bench**（Stanford × Laude）：每任务 `task.yaml` + `Dockerfile` + `tests/` + `solution.sh`，工作
  目录 `/app`，**端状态验证**（不看 transcript）。随 2.0 发布 **Harbor** harness，支持 Docker / Modal /
  Daytona / E2B / Runloop 五 provider，可并行数千容器；每任务跑 5 次报 95% CI。
- **SWE-bench-Live / REPOLAUNCH**（微软）：**环境由 agent 自动构建**（读 README/CI → 选基础镜像 → ReAct 迭代
  装依赖 → 固化成镜像）；「Time Machine」强制包管理器只能看见 bug report 之前发布的版本，防依赖漂移。
- **SWE-rebench**：标准化脚手架（所有模型同一最小 ReAct 框架 + 相同 prompt/超参，跑 5 次报 SEM + pass@5）——
  专治 **scaffold inflation**。
- **PawBench**（阿里通义）：量化出 **harness 分差最高 6.4 分 ≈ 一次重大模型版本升级**（与 agenteval「harness
  是一等变量」同一立论）。

**真实教训（比结论更有用）**：

1. **tag 会漂移**：E2B 报告 SWE-bench `:latest` 镜像现在带的 setuptools/pytest 比老测试能忍的新（astropy 把
   deprecation warning 当 error）→ **假失败**。教训：**钉 digest，不用 tag**。
2. **文件系统顺序也是环境**：部分实例跑整个套件，顺序依赖 `scandir` 顺序 → 换文件系统就冒 PASS_TO_PASS 假失
   败；他们专门分类为 `ordering_artifact`。教训：环境差异会产出「看起来像 agent 失败」的噪声，要能**分类**。
3. **镜像里的 HEAD ≠ `base_commit`**：SWE-bench 镜像在 `base_commit` 之上加了自己的一个提交（改构建文件），
   所以**正确的提取方式是 `git -C /testbed diff`（相对 HEAD）** —— 「环境 = 镜像层 + 一个基线提交」的活样
   本。
4. **联网不可协商**：`network_mode="none"`，否则 agent 会 `pip install` 去「修」导入错误，评测变成测连通性。
   轻量替代：bwrap / user namespaces（冷启动 340ms → 3.4ms）。
5. **答案会藏在镜像里**：Terminal-Bench 有 `Dockerfile reference check` 强制保证镜像不含测试或 oracle
   solution。

**对 agenteval 的映射（缺口）**：① 业界用**镜像**表达环境，我们 `workspace.type` 四分支（`type: image` 正是
业界默认答案）；② 业界起点是「commit + 预构建镜像」，我们 `bindCwd` 只记**分支名**（最弱一档）；③ 业界
`network_mode=none`，而 `DockerConfig.Network` **无默认值（空 = 不传 = docker 默认 = 联网）**；④ 业界「每任务
每容器」，我们的 `--chain-sessions` 是刻意的例外，必须在报告里声明。
