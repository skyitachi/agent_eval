# 真实仓库任务评测：设计方案

目标场景：**手上有一个真实的代码仓库（含提交历史），加上一段描述任务的自然语言 prompt，
要在这个仓库上评测 agent。**

这与 `datasets/demo` 那种「三个小文件 + 一段提示词」的形态不是同一个问题。差别不在规模，
在**判分证据从哪来**：demo 的判分依据是随数据集一起写好的小测试；真实仓库的判分依据是
这个仓库自己的测试套件，而那个套件既庞大、又 flaky、还摆在 agent 眼前。

本文档给出设计，不给出实现。

---

## 1. 现状盘点

先明确哪些能直接用，避免重复造。以下均以代码为准。

| 能力 | 现状 | 结论 |
| --- | --- | --- |
| 真实仓库作为 workspace | `workspace.type: git` + `repo` + `ref` + `subdir`，`internal/sandbox/workspace.go` 的 `materializeGit` | **可用** |
| 剥离上游历史防抄答案 | 默认删除克隆来的 `.git` 再重建 baseline（`Materialize` 里的 `os.RemoveAll(root/.git)`），注释明写「否则 agent 能定位到上游的修复提交并读走答案」 | **已经做对，且是最关键的一条** |
| 依赖安装不算 agent 的改动 | `runSetup` 之后自动 `Rebaseline`（同在 `Materialize` 内） | **可用** |
| diff 采集 | baseline → `git diff`，含未跟踪文件（`ChangedPaths` 用 `git add -N`） | **可用** |
| 输出收窄 | `output.paths` / `ignore_paths` | **可用** |
| 隐藏测试套件 | `pytest.suite` 在评测期才拷进沙箱（`pytest.go` 的 `CopyInto`），agent 全程看不到 | **可用，等价于 SWE-bench 的 `test_patch`** |
| 保护测试文件 | `patch.forbid_changed_paths` | **可用，但语义见 §4.2** |
| 规模控制 | `max_diff_lines` / `max_changed_files` / `min_diff_lines` | **可用** |
| 容器隔离 | `internal/sandbox/docker.go` 写好了 `--pids-limit`、`--memory`、`--cpus`、`--network` | ⚠️ **不可达**：`internal/runner` 硬编码 `backendLocal`，CLI 与数据集都没有选中 docker 的入口。见 §3.1 |
| 断点续跑 | `--resume` 按 cell key 跳过已完成 | **可用** |
| **指定跑哪些测试用例** | pytest grader 只有 `paths`（文件级）+ `min_passed`/`max_failed`（计数级） | ❌ **最大缺口** |
| **"必须由失败转通过"的语义** | 无 | ❌ **缺口** |
| **"不许回归"的语义** | 无 | ❌ **缺口** |
| **评测前还原测试文件** | ✅ 原语已经写好：`sandbox.Baseline.RestorePaths(ctx, globs)`。但它**没有调用方、没有测试、数据集里也没有对应字段** —— 看起来做完了，实际没接上 | ⚠️ **半成品**：比"没有"更危险 |
| **参考解落位** | 无显式字段，只能塞 `metadata`（引擎从不解释） | ❌ **缺口** |
| **per-task 环境** | 镜像配置是 `DockerConfig` 的一个字段，无 per-task 覆盖；且 docker 后端整体不可达 | ❌ **缺口** |
| **clone / 依赖缓存** | 每个 cell 独立 clone + 独立 setup（`internal/runner/runner.go` 每个 cell 调一次 `sandbox.Materialize`） | ❌ **缺口** |

一句话：**链路是通的，判分语义是浅的。**

---

## 2. 数据集设计

仓库坐标 + 任务描述 + 判分证据，三者分离。关键改动是**把「判分证据」提升为一等公民**，
而不是塞进 `metadata` 当死数据。

```
datasets/<name>/
├── dataset.yaml
├── tasks.jsonl                 # 每行一条：仓库坐标 + prompt 引用 + graders
├── prompts/<id>.md             # 给 agent 看的任务描述（人写）
├── evidence/<id>/              # 判分证据，agent 全程不可见
│   ├── tests/                  #   隐藏测试（= test_patch），评测期注入
│   ├── fail_to_pass.txt        #   必须由失败转为通过的用例
│   ├── pass_to_pass.txt        #   必须保持通过的用例（防回归）
│   └── solution.patch          #   参考解：只用于验证任务可解，永不参与评分
└── env/<id>/                   # 可选：per-task 环境（Dockerfile 或 setup.sh）
```

`prompts/` 与 `evidence/` 的分离是刻意的：前者是 agent 的输入，后者是判分的底牌。
混在一起，早晚会有一次 `assets` 目录整包拷进沙箱把答案一起送进去。

一条任务：

```json
{
  "id": "django-11099",
  "category": "bugfix",
  "difficulty": "medium",
  "tags": ["python", "django"],
  "prompt_file": "prompts/django-11099.md",
  "workspace": {
    "type": "git",
    "repo": "https://github.com/django/django",
    "ref": "a1b2c3d4e5f6...",
    "setup": [
      {"run": "python -m venv .venv && .venv/bin/pip install -e '.[tests]'", "timeout_s": 900}
    ]
  },
  "oracle": {
    "solution_patch": "evidence/django-11099/solution.patch",
    "fail_to_pass": "evidence/django-11099/fail_to_pass.txt",
    "pass_to_pass": "evidence/django-11099/pass_to_pass.txt"
  },
  "output": {"type": "patch", "ignore_paths": ["*.pyc", "__pycache__/**", ".venv/**"]},
  "graders": [
    {"type": "pytest", "name": "fail-to-pass", "weight": 0.7,
     "config": {"suite": "evidence/django-11099/tests",
                "restore_paths": ["tests/**"],
                "require_pass_file": "evidence/django-11099/fail_to_pass.txt"}},
    {"type": "pytest", "name": "no-regression", "weight": 0.3,
     "config": {"require_pass_file": "evidence/django-11099/pass_to_pass.txt"}},
    {"type": "patch", "name": "stayed-in-bounds", "required": true, "weight": 0,
     "config": {"forbid_changed_paths": ["tests/**", "*.cfg", "*.toml"]}}
  ],
  "limits": {"max_turns": 60, "timeout_s": 1800}
}
```

### 2.1 `ref` 必须是 commit SHA，不能是分支名

`internal/sandbox/workspace.go` 的 `materializeGit` 对分支/tag 走 `--depth 1 --branch`，对 HEX SHA 走全量克隆
再 checkout。**两者都能工作，但只有 SHA 可复现。** 用 `main` 意味着今天和下周跑的不是同一份代码，
报告之间不可比 —— 而"两次运行可比"正是这套框架的核心承诺。

### 2.2 `oracle` 是新增字段，语义要严格限定

`oracle` 的三个字段**只在 `aeval verify` 时被读取，评分路径永远不碰**。它存在的唯一理由，
是让「这个任务本身是否有效」变成一个可检查的事实，而不是一个信念（见 §5.1）。

塞进 `metadata` 也能存，但 `metadata` 的契约是「引擎从不解释」，
一份谁都不读的数据和一份不存在的数据没有区别。

---

## 3. 环境设计

### 3.1 后端必须是 docker —— 而当前选不出 docker

`local` 后端的能力边界写在 `sandbox/local.go` 的包注释里：只有独立工作目录和一个受控环境，
**没有文件系统与网络边界**。真实仓库评测里这是不可接受的：

- agent 能读到操作者机器上的任何东西，包括那个仓库的另一份克隆；
- agent 能上网搜到 issue 的修复 PR；
- `--pids-limit` / `--memory` / `--cpus` 只在 docker 下生效。

麻烦在于：`internal/sandbox/docker.go` **已经实现了**上面这一整套（364 行，
`DockerConfig` 带 `Image` / `Network` / `Memory` / `CPUs` / `PidsLimit` / `User` / `Env`），
但 `internal/runner` 里 `Backend: backendLocal` 是**写死的**，CLI 没有 `--backend`，
数据集里也没有对应字段。结果是这份实现**没有任何非测试路径能选中它** —— 它没有集成测试，
也没有调用方，而 `runner` 的注释却写着容器后端「已被丢弃」。

所以这一条是前置工作，而且要先做一个二选一的决定：

- **接上它**：后端做成 run 级可选项，补一条在有 daemon 的机器上真跑的端到端测试；
- **删掉它**：承认这个 build 只有 local 后端。

保留一份谁也调不到的 364 行实现，代价是每个读代码的人都会以为容器隔离已经生效 ——
比明确地没有更糟。

### 3.2 网络默认值不是 hermetic；而 hermetic 与「setup 装依赖」确实冲突

先纠正一个容易记错的点：`DockerConfig.Network` 的注释写着 `"none"` 是
「recommended default for hermetic evaluation」，但**代码里没有任何地方设置它** ——
没有 `Normalize()`，没有别的默认值注入，`DockerConfig` 就是个普通结构体。
空值意味着不传 `--network`，也就是 docker 自己的默认值：**联网**。

所以现状既不是 hermetic，也谈不上「默认联网」这个有意选择；它只是没有默认值。
一旦按 §3.1 把 docker 接上，下面这个张力才会真正出现：

- 想把默认值定成 `network: none`（这才叫 hermetic），
- 但 `workspace.setup` 是在 sandbox **内部**执行的（`sandbox/workspace.go` 的 `runSetup` 调 `sb.Exec`），
- 而真实仓库的 setup 几乎必然要装依赖。

三条出路：

| 方案 | 做法 | 代价 |
| --- | --- | --- |
| **A. 依赖烘进镜像**（推荐） | 每个仓库/版本一个镜像，依赖预装好；setup 只做 `pip install -e . --no-deps` 这类离线操作 | 需要维护镜像，首次构建慢 |
| B. 全程开 `bridge` | 简单，一次搞定 | **agent 也能上网**，答案泄漏风险直接拉满 |
| C. 分阶段网络的容器 | setup 用能上网的容器，agent 用 `none` 的容器 | 工作区在两容器间搬迁，复杂度高 |

选 A 时，`network: none` 就从"装不了依赖"变成"网络被真正封死"，两个目标同时达成。

### 3.3 clone 与依赖需要缓存

现在每个 cell 都会走一遍 `materializeGit`：克隆到临时目录 → 拷进沙箱 → 删 `.git` → 重建 baseline。
django 这种仓库，配合 `--concurrency 8`，就是 8 次克隆加 8 次 `pip install`。

设计上应该在 run 级维护：

- **一个 bare mirror**（按 `repo@ref` 哈希命名），各 cell 用 `git clone --reference` 或
  `git --git-dir=<mirror> worktree add` 取工作树 —— 只下载一次；
- **依赖层**：烘进镜像（推荐），或挂一个共享的 `pip`/`uv` 缓存卷（`extra_mounts` 已经支持）。

注意 clone 发生在 **host** 上（`runHost`），不是容器内。这是有意的设计 —— 比 `docker cp` 快得多，
且两个后端行为一致 —— 但意味着**评测机必须有 git 和到仓库的网络可达性**，
而且这一步的失败会表现为"workspace 造不出来"，不是 agent 的问题。

### 3.4 per-task 环境

真实仓库对运行时版本极度敏感（Python 3.12 vs 3.13、Node 20 vs 22）。run 级单一镜像不够。
最小改动是给 `TaskSpec` 或 `Workspace` 加一个 `image` 字段，覆盖 run 级 `backend.image`；
再用 `env/<id>/Dockerfile` 描述构建方式。

---

## 4. 判分设计

### 4.1 三层判分，语义各不同

真实仓库的判分不该是一个分数，而是三个正交的问题：

1. **做对了吗** —— 指定测试用例是否由失败转为通过（FAIL_TO_PASS）
2. **弄坏别的了吗** —— 原本通过的用例是否仍然通过（PASS_TO_PASS / 回归）
3. **越界了吗** —— 补丁的形状是否可接受（改了不该改的文件、diff 过大）

三层分开加权，失败时报告能说清是哪种失败。合成成一个布尔值就把这些信息全丢了。

### 4.2 测试文件：还原，而不是否决

当前只有 `forbid_changed_paths`：agent 改了测试 → 整个任务判负。
但 SWE-bench 用的是另一个语义：**apply agent 的补丁之后，强制把测试文件 checkout 回官方版本，
然后用官方测试判分。** 差别很实质：

| | `forbid_changed_paths` | `restore_paths` |
| --- | --- | --- |
| 本质 | 事后惩罚 | 事前还原 |
| agent 改测试的后果 | 任务判负 | 改了也白改，按真实水平判分 |
| 测的是什么 | 服从性 | 能力 |
| 副作用 | 一个只想"确保测试跑过"的 agent 会被判负，即使代码是对的 | 无 |

**两者都要有，但用途不同**：`restore_paths` 是默认的、正确的做法；
`forbid_changed_paths` 保留给「我想单独测量 agent 是否有越界倾向」的场景。

实现上是评测前对列出的路径执行 `git checkout <baseline_sha> -- <paths>`，
`Baseline` 结构体里已经存了那个 SHA（`Baseline.SHA`），拿得到。

**这条已经有了一半**：`sandbox.Baseline.RestorePaths(ctx, globs)` 写好了，语义正是上面描述的
这件事。但它**没有调用方、没有测试，数据集里也没有 `restore_paths` 字段** ——
所以从外部看它不存在。要完成这一条，缺的是接线：把字段暴露到 `output` 或 grader 配置上、
在 grading 之前调用、并补上「agent 改了测试但仍然按真实水平判分」的测试。
注意不要重复实现一遍：这个函数已经在了。

### 4.3 pytest grader 需要的扩展

现在的 `pytestConfig`（`pytest.go`）只有计数阈值。计数阈值在真实仓库上很脆弱 ——
测试总数会随 collector 行为、依赖版本、跳过规则波动，`min_passed: 12` 这种阈值第二年就不成立了。

需要新增的是**用例级的期望**：

```yaml
config:
  tests:                      # 显式 node id，取代脆弱的计数阈值
    - "tests/test_queries.py::test_file_path_field_special_chars"
  require_pass: all           # 全部通过才算过（默认）
  restore_paths: ["tests/**"] # 评测前还原
  baseline_expect:            # 固化两组期望（推荐）：来自 evidence/ 的文件
    fail_to_pass_file: "evidence/<id>/fail_to_pass.txt"
    pass_to_pass_file: "evidence/<id>/pass_to_pass.txt"
  infer_expect: true          # 备选：先在 baseline 上跑一遍自动推导两组期望
```

`infer_expect` 很诱人（不用手写清单），但**不建议作为默认**：它会把 flaky 测试的噪音直接吸进来。
SWE-bench 是在数据集构造期算一次、人工复核后固化下来的，这个顺序不能省。

### 4.4 用例级结果需要单独解析

`ParsePytestOutput` 目前只从末尾汇总行抓计数，从 `FAILED`/`ERROR` 行抓失败用例名（`pytest.go`）。
要判定"指定的这 12 个用例是否都通过"，需要拿逐用例的结果 —— 最省事的做法是让 pytest 输出
JUnit XML（`--junit-xml`）再解析，比正则抓文本稳得多。

---

## 5. 有效性威胁

这一节是整套设计里最值钱的部分。**真实仓库评测最常见的失败不是"跑不起来"，而是"跑起来了，
但测的不是你以为的东西"。**

### 5.1 任务自检：没有它，你分不清"任务难"和"任务坏"

每条任务在进入正式评测前，必须做三个检查：

1. **可解性** —— 应用 `oracle.solution_patch`，`fail_to_pass` 全部通过。
   不通过 = 任务坏了（测试写错、环境不对、参考解过时）。
   Terminal-Bench 强制要求 `solution.sh` 就是为这个；SWE-bench 靠 PR 自带的 gold patch。
2. **必要性** —— 不打任何补丁时，`fail_to_pass` 必须**失败**。
   如果它本来就通过，这个任务什么也没测，通过是白送的。
3. **稳定性** —— baseline 连跑 N 次（3 次起步），结果不一致的用例要从两组清单里剔除。
   真实仓库的测试套件普遍 flaky，不筛就会把 flaky 当成 agent 的失败。

这三条建议做成 `aeval verify` 子命令，输出一份「任务体检报告」。没有它，
`resolve_rate` 这个数字没有意义 —— 你无法判断 65% 是模型的水平还是数据集的水平。

### 5.2 答案泄漏面

| 泄漏通道 | 现状 | 对策 |
| --- | --- | --- |
| git 提交历史里的修复 | ✅ 默认剥离 | 保持 |
| 远端仓库可访问 | ⚠️ 取决于 network | `network: none` + 依赖烘进镜像 |
| 测试文件（含期望值） | ⚠️ 若用可见套件 | 一律用 `suite` 隐藏注入 |
| **prompt 里的答案** | ❌ 无检查 | prompt 要脱敏：删掉 issue 链接、PR 号、"修复见 #123" |
| 依赖包拉到已修复版本 | ❌ 无检查 | 锁死版本；`pip install` 不要带 `-U` |
| `git log` / `CHANGELOG` 里的修复说明 | ⚠️ 部分 | 剥离历史已覆盖大部分；`CHANGELOG.md` 需要 `restore_paths` 或忽略 |

第 4 条最容易被忽略：真实 issue 的正文里经常写着"这个 bug 在 #12345 修了"，
或者干脆就是 PR 的描述。**prompt 必须当作可能泄漏答案的输入来审查。**

### 5.3 作弊面

真实仓库给 agent 的作弊空间比小数据集大得多：

- 删测试 / 改测试 / 加 `pytest.skip` / 改 `conftest.py` 劫持收集；
- 改 `setup.cfg`、`pyproject.toml` 把测试排除掉；
- 用 `monkeypatch` 让被测函数直接返回期望值；
- 在 `sitecustomize.py` 里做手脚。

对策是 `restore_paths`（还原测试与配置）+ 隐藏套件 + `patch` grader 的路径约束三者叠加。
单靠任何一条都不够。

### 5.4 不可复现面

`ref` 浮动、依赖不锁版本、镜像漂移、时区、随机种子、并发度、磁盘/内存不足。
其中**资源不足最阴险**：OOM 导致的失败看起来像 agent 的失败，应该归类到 harness 层。
框架里 `stop_reason` 已经区分了 `harness_error` 与 `timeout`，这个区分要在报告里被真正用起来。

---

## 6. 规模与成本

一条真实仓库任务的成本结构与小数据集完全不同：

| 阶段 | 典型耗时 | 能否缓存 |
| --- | --- | --- |
| clone | 秒级 ~ 分钟级 | ✅ run 级 mirror |
| 装依赖 | 分钟级 | ✅ 烘进镜像 |
| agent 跑 | 几分钟 ~ 半小时 | ❌ |
| 跑测试 | 秒级 ~ 分钟级 | ❌ |

**agent 那一段才是成本大头，其余的都该缓存掉。** 建议的推进节奏：

1. 小规模冒烟：3~5 条任务，`--concurrency 1`，`--keep-workspaces`，
   人肉检查 workspace 里到底发生了什么；
2. 中规模：20 条左右，`--concurrency 4`，看 `harness_error_rate` 和 `timeout_rate`；
3. 全量：只在 1、2 都干净之后，且必须先做完 §5.1 的任务自检。

`--resume` 在中大规模上是必需品而不是锦上添花 —— 它按 cell key 跳过已完成的格子。

---

## 7. 需要新增的能力（按优先级）

| 优先级 | 能力 | 位置 | 理由 |
| --- | --- | --- | --- |
| **P0** | `oracle` 字段 + `aeval verify` 任务自检 | `dataset/spec.go`、`cmd/aeval` | 没有它，`resolve_rate` 无法解释 |
| **P0** | pytest：用例级 `tests` 列表 + JUnit XML 解析 | `grader/pytest.go` | 计数阈值在真实仓库上不成立 |
| **P0** | `restore_paths`（评测前还原测试文件） | `grader/context.go` 或 grader 级动作 | 比 `forbid_changed_paths` 更接近业界做法 |
| **P1** | per-task `image` 覆盖 | `dataset/spec.go`、`internal/sandbox/docker.go` | 真实仓库需要版本隔离 |
| **P1** | 把 docker 后端接上（或明确删掉） | `internal/runner`、`cmd/aeval` | §3.1：代码已在，但没有 run 路径能选中它 |
| **P1** | run 级 clone mirror | `sandbox/workspace.go` | 直接决定大规模是否可行 |
| **P1** | `fail_to_pass` / `pass_to_pass` 显式清单 | `grader/pytest.go` | 分离"做对"与"没弄坏" |
| **P2** | `infer_expect`（baseline 自动推导期望） | `grader/pytest.go` | 省事但引入 flaky 噪音，仅作备选 |
| **P2** | flaky 筛选（baseline 连跑 N 次） | `aeval verify` | 数据质量，可先人工 |
| **P2** | 依赖缓存卷 | `internal/sandbox/docker.go`（`ExtraMounts` 已够） | 能用配置解决，不必改代码 |

P0 三项做完，这条链路才真正算「能用于真实仓库评测」。P1 决定规模上限，P2 是质量优化。

---

## 8. 与两个主流基准的对应关系

| 概念 | SWE-bench | Terminal-Bench | 本项目对应 |
| --- | --- | --- | --- |
| 初始状态 | `repo` + `base_commit` | Dockerfile | `workspace.git` + `ref` |
| 任务描述 | `problem_statement` | `task.yaml: instruction` | `prompt` / `prompt_file` |
| 判分套件 | `test_patch` | `tests/test_*.py` | `pytest.suite`（隐藏注入） |
| 期望清单 | `FAIL_TO_PASS` / `PASS_TO_PASS` | 隐含在断言里 | ❌ 待加 |
| 参考解 | `patch` | `solution.sh` | ❌ 待加（`oracle`） |
| 环境 | 每实例一个 env | 每任务一个镜像 | ⚠️ run 级镜像，待加 per-task |

本项目与 SWE-bench 的结构最接近，因为两者都是「仓库 + 补丁」形态；
Terminal-Bench 的「容器最终状态断言」则更适合用 `command` grader 表达。
