# 第 10 章：多 Agent 编排——planner/worker/critic 的分解艺术

> 对应阶段：阶段三（深入）· 项目 3 `stage-03-multi-agent/`
> 代码位置：`stage-03-multi-agent/internal/orchestrator/`（本章精讲）、`mini-agent/api/api.go`、`mini-agent/internal/agent/agent.go`
> 前置：第 3 章（ReAct 循环与 Agent 内核）、第 8 章（pool 并发底座）、第 9 章（任务状态机与 checkpoint）
> 学完后你能讲清：什么时候该拆多 agent（以及什么时候不该）、四种编排模式怎么选、planner 的 LLM 输出如何把关、critic 循环的双重熔断与降级、成本怎么核算到每个子任务——这些是多 Agent 岗位面试的核心题库。

---

## 本章地图

- 单 agent + 多工具的三堵墙：为什么一个大脑管不了所有事
- "能单 agent 就别多 agent"：拆是 trade-off 不是升级，三个该拆的信号
- 编排模式四选对比：planner-worker / critic / handoff / swarm（题眼：能否并行）
- planner 输出的工程化：结构化输出 → schema 校验 → 带错重试 → 耗尽 failed
- critic 循环：生成-评审-打回，双重熔断缺一不可
- 模型分级与子任务级成本核算：`Usage()` 接口是怎么来的
- 上下文共享两范式：黑板（checkpoint）vs 消息传递（handoff）
- 复用而非重写：`mini-agent/api` 门面包的由来
- 进阶：生成-校验-重试循环、预算哨兵错误、critic prompt 设计、同步 vs 事件驱动

---

## 一、概念详解

### 1.1 单 agent + 多工具的三堵墙

阶段一的 ReAct 循环是"一个 agent 挂 N 个工具"。工具一多、任务一复杂，会撞上三堵墙：

1. **Context 膨胀**：每轮循环都要把全部工具 schema + 累积的对话历史发给模型。20 个工具的 schema 就是几千 token；长任务里工具返回的中间结果还在不断堆。token 成本线性涨，模型的注意力反而被稀释——无关工具的描述干扰了它对当前子任务的判断。第 3 章的上下文压缩能缓解，但救不了根本：一份 messages 里什么都有，噪声和信号注定混在一起。
2. **工具选择准确率下降**：工具数量上去后，模型选错工具、填错参数的概率明显上升，schema 相似的工具（如 `search_docs` 与 `search_web`）之间尤其严重。经验值：超过 10~15 个就要开始分组或拆分，**几十个是明显拐点**——模型在一大页说明书里挑对那一件工具，本身就成了一道难题。
3. **职责混杂**：让一个 system prompt 同时规定"如何规划、如何执行、如何评审、如何汇报"，结果是每条规则互相稀释，哪条都调不优。规划需要全局视角和克制，执行需要聚焦和工具熟练，评审需要挑剔——三种"人设"拧在一份 prompt 里，模型每种都做不到最好。

多 Agent 的思路就是把"一个大脑什么都要管"拆成"多个专职 agent 协作"：每个 agent 有独立的 prompt、工具子集和上下文窗口，之间用明确接口（子任务描述、结构化产出）通信。

### 1.2 "能单 agent 就别多 agent"：这是 trade-off，不是升级

先泼冷水。多 agent 换来职责隔离与可控 context，付出的同样真实：

| 代价 | 具体表现 |
| --- | --- |
| 状态一致性 | 多个 agent 各自有 context，共享中间结果靠什么？出 bug 时状态散落各处 |
| 成本倍增 | planner 一次调用 + N 个 worker 各若干轮调用，token 是单 agent 的数倍（Anthropic 复盘其多 agent 研究系统，实测约 4 倍于单 agent） |
| 调试困难 | 单 agent 失败看一条线性 log；多 agent 失败要还原"谁在什么时间把什么传给了谁"，必须上树状 trace（第 12 章） |

所以判断框架按顺序问（来自 Anthropic "Building effective agents" 的原则：find the simplest solution possible）：

1. **加 prompt / 改工具描述能解决吗？** 能，就别拆。大多数"需要多 agent"的直觉，其实是 prompt 没写好。
2. **加工具、上 workflow（状态机）能解决吗？** 任务是"串行流程长"而不是"职责真的不同"时，确定性流程比多 agent 稳得多。
3. **真的撞墙了吗？** 三个该拆的信号：① context 膨胀到压缩都救不回来；② 工具几十个、模型明显选不准；③ 职责混杂到一个 prompt 写不下（又规划又执行又评审）。
4. **能接受 3~5 倍 token 成本和更复杂的调试吗？** 不能，回去优化单 agent。

一句话版本（可直接背）：**先 prompt，再工具，再 workflow，最后才 multi-agent**。面试被问"你怎么设计多 agent 系统"，先讲这句再讲架构——这是区分"真做过"和"背概念"的点。

### 1.3 编排模式四选

| 模式 | 结构 | 视角 | 适用场景 |
| --- | --- | --- | --- |
| planner-worker | planner 分解任务 → worker 并行执行 → 汇总 | 管理者：分而治之 | 任务可分解、子任务相对独立（"调研三个方案并对比"）；本章主线 |
| critic/reviewer | generator 产出 → critic 评审 → 打回重做，循环到通过或熔断 | 质检员：提案-反馈 | 产出质量可被另一个模型大致判断（写代码、写报告）；可叠加在任何模式上 |
| handoff | 一个 agent 判断"这活不归我管"，把对话连同上下文整体转交 | 流水线：路由-接力 | 分诊/串行流程（客服：售前→售后→技术）；任一时刻只有一个 agent 持有对话 |
| 群聊/swarm | 多 agent 共享消息流轮流发言 | 研讨会 | 开放性研讨；成本最高、终止条件难设计，生产可控性差，了解即可 |

两组关键辨析：

- **planner-worker vs handoff**：前者"拆任务"，子任务结果回流汇总；后者"转责任"，控制权一去不复返。**判断题眼：子任务能否并行**——能并行就是 planner-worker，必须串行接力就是 handoff。
- **critic 不是独立拓扑，是叠加层**：它寄生在生成者之后。本项目把它叠加在 planner-worker 的 worker 产出之后（练习 4）。

### 1.4 planner 输出的工程化：模型负责生成，代码负责把关

planner 是 LLM，LLM 输出是概率生成的（第 1 章 1.1 节）——它可能输出废话、裹 markdown 围栏、分解出 30 个子任务、给两个子任务同一个 ID。而未校验的计划一旦驱动状态机，错误就被 checkpoint 固化、被并发放大。**任何要进状态机的 LLM 输出，都必须先过确定性校验**。四道防线：

1. **结构化输出约束**：system prompt 要求"只输出 JSON"并给 schema 样例；更强的是 JSON mode 或 function calling 伪工具（回看第 1 章进阶 3.3 的三种做法）。这道防线降低出错率，但不保证不出错。
2. **代码侧 schema 校验**：字段非空、类型正确、子任务数上限、ID 唯一、（有依赖时）依赖不成环——纯函数，不碰 LLM/IO，写严不写松。
3. **校验失败带错误信息重试（限次）**：把"模型的原始输出 + 具体校验错误"追加进 messages 重发，模型知道上次错在哪，比从零重发成功率高得多。
4. **重试耗尽 → 任务 failed**：进人工或告警，绝不"再试一次说不定行"地无限烧。

原则一句话：**模型负责生成，代码负责把关**。进阶 3.1 给出可运行的教学实现，练习 3 要求你在项目里落地它。

### 1.5 critic 循环：生成-评审-打回，直到通过或熔断

critic 模式是 ReAct 的"双 agent 版"：worker 产出一版，critic 评审一次，不通过则把评审意见拼进 worker 下一轮的 prompt 重做——直到通过，或触发熔断。

熔断必须是**双重**的，缺一不可：

- **轮次熔断**（管深度/单点失控）：单个子任务最多执行 N 轮（首轮 + 打回重做）。没有它，一个"怎么改都不过"的子任务会让 worker 和 critic 无限拉锯。
- **token 预算熔断**（管广度/总量失控）：整个任务累计 token（worker + critic，含崩溃恢复前已烧的——从 checkpoint 续算）超预算，任务直接 failed。没有它，10 个子任务每个都在轮次限内拉锯，加起来照样烧穿预算——这是 agent 系统特有的熔断语义：防的不是下游故障，是**成本失控**。

反直觉的设计：两个熔断检查不到对方管的事。轮次熔断发现不了"总量在多个子任务上缓慢失血"，预算熔断发现不了"单个子任务深度死循环"（预算没到时一直烧）。一个管深度，一个管广度。

### 1.6 模型分级与子任务级成本核算

不是所有 agent 都该用同一个模型：

- **planner 用强模型**：分解错了全盘皆输，这是杠杆最高的环节；
- **critic 用强模型**：它是质量闸门，评审能力弱等于没有闸门；
- **执行型 worker 可用便宜模型**：子任务 prompt 已被 planner 写得自包含、边界清晰，执行难度低。

模型分级要落地为"换实现不换编排器"——这就是本章接口注入设计（代码精讲 2.3/2.4/2.5）的存在理由之一。

成本控制还要有数据：**每个子任务花了多少 token** 必须可计量。数据源头是每次 LLM 响应的 `usage`（第 1 章），经内核累计后由 `Agent.Usage()` 暴露（代码精讲 2.8），worker 执行完读出来随 checkpoint 落盘——预算熔断（练习 4）和"哪个子任务最贵"分析（第 12 章）都消费这笔账。**没有计量就没有成本控制。**

### 1.7 上下文共享两范式：黑板 vs 消息传递

多 agent 之间怎么共享信息，只有两种基本范式：

- **黑板（blackboard）**：共享存储，各 agent 读写公共状态。本项目的 checkpoint/SQLite 就是黑板——子任务产出写进去，汇总环节（以及后续子任务）读出来。优点：持久、可恢复、可查询（看板直接读）；缺点：要设计 schema，要处理并发写。
- **消息传递**：agent 之间直接传消息，handoff 的"整个对话上下文转交"就是典型。优点：边界清晰、无共享状态；缺点：状态散在消息流里，进程一死就难以恢复。

生产系统常混用：**黑板存事实，消息传指令**。本项目的选择是黑板为主（状态外置是崩溃恢复和 HITL 的地基，第 9 章），worker 的输入则走"消息"风格——一份自包含的子任务 prompt。

### 1.8 复用而非重写：阶段一内核作为 worker 执行体

阶段三最大的架构决策：**不重写 agent 内核**。worker 的执行体就是阶段一的 ReAct 循环——每个子任务 new 一个 `api.Agent`，跑完即弃。理由：

- 编排层的职责是任务分解、并发、状态、恢复；ReAct 循环本身不需要第二份实现；
- 每个子任务一个独立 Agent = 天然的 context 隔离：worker 只背自己的 system prompt + 自包含 prompt，不背其他子任务的历史——这正是"多 agent 解决 context 膨胀"的落地方式；
- 它倒逼阶段一的内核接口设计得可被外部消费——**能被你未来的项目复用，是阶段一产出的最好检验**。

工程障碍：mini-agent 的实现在 `internal/` 下，Go 的 internal 可见性规则让它无法被 module 外引用。解法就是 `mini-agent/api/` 门面包（代码精讲 2.7 逐段讲）。

---

## 二、代码精讲

本章精讲的代码全部是**练习骨架**：类型、接口、构造函数已就绪，核心逻辑是 `TODO(练习3/4)` 留给你。教程讲清"为什么这么设计、要实现什么"，不替你做——这是本教程的设计，不是遗漏。

### 2.1 包总览：orchestrator 坐在两根支柱上

`stage-03-multi-agent/internal/orchestrator/orchestrator.go:1-18` 的包注释就是全章架构图：

```go
// Package orchestrator 是多 agent 系统的编排层：
// planner 分解 → pool 并发执行 worker → （可选）critic 评审打回 → 汇总。
//
// 在整个 agent 链路中的位置：它坐在两根支柱之上——
// internal/pool（并发底座，练习1）与 internal/task（状态机 + checkpoint，练习2），
// 并把 mini-agent 内核（单 agent ReAct 循环）当作 worker 执行体复用（api.Agent）。
```

三条设计核心也写在注释里：状态机驱动每次迁移落盘（第 9 章）；planner 的 LLM 输出必须过 `ValidatePlan` 才能进状态机（本章 1.4）；失败处理四件套——幂等键、部分失败汇总、critic 降级放行、双重熔断。

### 2.2 数据结构：SubtaskSpec 与 Plan（planner.go）

`SubtaskSpec`（`planner.go:22-29`）是 planner 与 worker 之间的契约：

```go
type SubtaskSpec struct {
	ID    string // 任务内唯一标识（如 "s1"），幂等键 = taskID + ":" + ID
	Title string // 一句话标题，给看板和汇总报告用
	Prompt string // 喂给 worker agent 的子任务指令（自包含）
	RequiresApproval bool // 高风险子任务标记（练习5 HITL 用）
}
```

最值得停下想的是 `Prompt` 的"自包含"要求（`planner.go:19-21` 注释）：**worker 是独立 agent，看不到用户原始目标，也看不到其他子任务——它唯一的输入就是这份 Prompt**。这是 1.1 节"砍 context"的代价转移：context 是省了，但信息量也随之切断，所以 planner 生成 Prompt 时必须把完成该子任务所需的上下文都写进去。这条要求会直接写进 planner 的 system prompt（练习 3）。

`Plan`（`planner.go:34-36`）目前只是一组相互独立、可并行的子任务；带依赖的 DAG 分发是练习 3 的进阶方向。`MaxSubtasks = 8`（`planner.go:41`）是成本控制阀：LLM 可能分解出几十个子任务，每个子任务至少一轮 LLM 调用，**不上限等于把成本控制权交给模型的发挥**。

### 2.3 Planner 接口与 LLMPlanner 骨架（planner.go）

```go
// Planner 把用户目标分解为结构化计划。（planner.go:49-53）
type Planner interface {
	// Plan 分解 goal，返回校验通过的 Plan。
	// 实现方负责：LLM 输出解析 + ValidatePlan 校验 + 校验失败带错误信息重试（限次）。
	Plan(ctx context.Context, goal string) (Plan, error)
}
```

为什么是接口？两个真实理由，背下来（面试话术）：① **可测**——编排器的核心是状态机与 checkpoint 时序，这些逻辑必须不烧 token、不依赖网络地测，注入假 Planner 即可；② **模型分级**——接口让"换强/弱模型"甚至"换固定模板 planner"（如周报场景固定三段式）只是换一个实现，编排器一行不改。

`LLMPlanner`（`planner.go:60-69`）的 TODO 字段建议里藏着一个可测试性技巧：把"发一次 LLM 问答"抽成可注入的 `chat` 函数字段——因为 mini-agent 的 `llm.Client` 无法指向 httptest 假服务器（baseURL 私有、无 WithBaseURL），没有这层注入，"校验失败重试"这条核心路径就无法离线测试。构造函数装配真实实现，测试替换字段。`var _ Planner = (*LLMPlanner)(nil)`（`planner.go:79`）是编译期断言：实现必须始终满足接口，改签名立刻编译报错。

**TODO(练习3) 在这里要实现什么**（`planner.go:81-137`）：`ValidatePlan(p Plan) error`——纯函数校验四条（非空、不超上限、ID 唯一、字段去空白后非空），错误信息要带定位（如 `subtasks[2]`），否则喂回 planner 的错误没有修复价值；`LLMPlanner.Plan`——组 messages → chat → 容错解析 JSON → 校验 → 失败带错误反馈重试（限次）。进阶 3.1 的教学实现把这条控制流完整演示一遍。

### 2.4 Worker 接口与 AgentWorker 骨架（worker.go）

```go
// Worker 执行单个子任务。（worker.go:25-29）
type Worker interface {
	// Execute 执行一个子任务，返回产出文本与本次消耗的 token 数。
	// 约定：必须响应 ctx 取消（pool 会为每个 job 派生超时预算）。
	Execute(ctx context.Context, spec SubtaskSpec) (output string, tokens int, err error)
}
```

注意签名里的 `tokens`：返回值带 token 数，是为了让编排器把成本累加进子任务 checkpoint 与任务总账——1.6 节"成本核算到子任务"在接口层的体现。

`AgentWorker`（`worker.go:38-42`）的注释回答了本章最重要的设计问题之一：**为什么每个子任务 new 一个 Agent 而不是共享一个**——共享意味着所有子任务的工具返回、中间推理堆进同一份 messages，context 膨胀 + 噪声稀释，多 agent 白拆了。每子任务新 Agent：system prompt + 自包含 prompt，跑完即弃。代价是每个子任务重新付一份 system prompt 的 token——这正是 1.2 节说的"用成本换可控 context"。

**TODO(练习3)**（`worker.go:54-80`）：`Execute` 的流程——开工前查 `ctx.Err()` → 用 worker 专用 system prompt new 一个 `api.Agent` → `Run(spec.Prompt)` → 返回产出与 `agent.Usage().TotalTokens`。worker 的 system prompt 要点：明确"你只负责这一个子任务"、要求最终输出自包含结论文本（它会进汇总报告，读者看不到执行过程）。

### 2.5 Critic 接口与 Verdict（critic.go）

`Verdict`（`critic.go:19-26`）只有两值：`VerdictPass` / `VerdictReject`——评审结论本质上是个布尔，用枚举让语义显式。`Critic` 接口（`critic.go:33-37`）：

```go
type Critic interface {
	// Review 评审 spec 的产出 output。
	// 返回评审结论、不通过时的反馈意见、本次评审消耗的 token 数。
	Review(ctx context.Context, spec SubtaskSpec, output string) (verdict Verdict, feedback string, tokens int, err error)
}
```

三个返回值各有去向：`verdict` 驱动循环，`feedback` 拼进 worker 下一轮 prompt（空反馈等于让 worker 盲改），`tokens` 计入预算熔断——**评审也花钱，必须上账**。接口注入的理由与 Planner 同构：可测（假 Critic 构造"先 reject 后 pass / 永远 reject / 出错降级"等场景）+ 模型分级（critic 值得用比 worker 更强的模型）。

**TODO(练习4)**（`critic.go:60-86`）：`LLMCritic.Review`——构造 messages（critic system prompt + 子任务要求 + worker 产出）→ chat → 解析结论。输出格式约定用裸文本（首行 PASS / REJECT + 意见）而非 JSON：结论只有两种，字符串解析够用且省 token。关键纪律：**既非 PASS 也非 REJECT → 返回 error 走降级路径**，而不是误判成 reject 让 worker 白重做——"模型说了句废话"和"模型说不合格"是两回事。

### 2.6 Orchestrator：接口注入、Option 模式与哨兵错误（orchestrator.go）

`Orchestrator` 结构体（`orchestrator.go:31-47`）：

```go
type Orchestrator struct {
	store   *task.Store  // checkpoint 唯一读写入口：每次状态迁移都落盘
	pool    *pool.Pool   // 并发底座：worker 子任务的并行执行与限流
	planner Planner      // 任务分解（接口注入：测试用假 Planner，不依赖真实 LLM）
	worker  Worker       // 子任务执行体（接口注入，同上）
	critic  Critic       // 产出评审；nil 表示不评审（练习3 的形态）
	tracer  trace.Tracer // 可观测后端；默认 Noop，接 Langfuse 只换实现（练习6）

	maxCriticRounds int // 熔断一：单个子任务的评审轮次上限（练习4）
	tokenBudget     int // 熔断二：整个任务的 token 预算，0 表示不限（练习4）
}
```

整个 struct 是一幅"依赖地图"：两根支柱（store/pool）是具体类型——它们是本项目的内部组件；三个 agent 角色（planner/worker/critic）全是接口——它们背后是 LLM，必须可替换。注释里还预告了练习 4 要补的运行时状态（critic 降级计数器），并点名并发安全：pool 并发执行多个子任务，共享计数器必须用 `atomic`，`go test -race` 会抓。

**Option 模式**（`orchestrator.go:51-77`）：`WithCritic` / `WithTokenBudget` / `WithTracer` 三个函数式选项，让练习 4/6 给编排器加能力时不动 `New` 的签名，调用方按需叠加——`New(store, pool, planner, worker)` 拿到的就是练习 3 的形态（无评审），`New(..., WithCritic(c, 3), WithTokenBudget(100000))` 叠加成练习 4 的形态。`New`（`orchestrator.go:81-93`）里 tracer 默认 Noop："不接观测后端"是显式选择，而不是 nil 判断散落各处。

**哨兵错误**（`orchestrator.go:171-184`）：`ErrWaitingHuman` 是"任务暂停等待人工审批"的哨兵错误（练习 5 用，契约先定死）。注释讲了为什么用哨兵错误而非返回值标志——"等待审批"不是失败，调用方用 `errors.Is` 把它和真实错误分开处理；它能穿透 pool → dispatch → Run 多层调用自然冒泡，不用给每层签名加返回值。练习 4 的 `ErrBudgetExceeded` 与此同构，进阶 3.2 用教学代码把这个模式讲透。

**TODO(练习3)**（`orchestrator.go:95-169`）：`Run`——CreateTask(pending) → planning → planner.Plan → 计划落盘（幂等键 = taskID+":"+子任务ID）→ running → pool 并发分发（每个 job 内迁移子任务状态、执行、落盘）→ 汇总 → done/failed；`Resume`——LoadTask 读出状态，已 done 的跳过、停在 running 的迁回 pending 重跑、failed 的重排队。**TODO(练习4)**（`orchestrator.go:228-261`）：在子任务执行路径上叠加评审循环 + 双重熔断 + critic 降级。两处的 TODO 注释把步骤、提示、验收都写全了，做练习时逐条对照。

### 2.7 门面模式逐段讲：mini-agent/api/api.go

`api` 包（`mini-agent/api/api.go`）只有 111 行、零逻辑，却是阶段三的地基。包注释（`api.go:1-17`）讲清了为什么需要它：

- mini-agent 的实现全在 `internal/` 下，**Go 的 internal 可见性按目录树位置判定，不是按 module 名**——所以即使 stage-03 的 module 路径叫 `mini-agent/xxx` 也没用，代码必须物理位于 mini-agent/ 目录下才能 import 它的 internal 包（`api.go:12-14`，反直觉点，面试可能问）；
- stage-03 是独立 module，要复用内核就需要一条"导出通道"；stage-03 侧用 `go.mod` 的 `replace mini-agent => ../mini-agent` 指过来。

实现手法是**类型别名 + 函数转发**，不搬动 internal 下任何文件（阶段一/二代码零影响）：

```go
type Agent = agent.Agent   // api.go:59 —— 别名而非新类型
var NewAgent = agent.New   // api.go:61 —— 函数转发
```

两个细节值得记住：

- **用别名（`=`）而非定义新类型**：保证外部拿到的 `*api.Agent` 就是 `*agent.Agent`，方法集全部保留，且能直接与内核里其他类型的签名互操作；定义新类型则要在每个方法上写转发、还破坏类型同一性；
- **命名让位**（`api.go:37-41`）：llm 包的线缆结构导出为 `ToolSchema` 而非 `Tool`——把短名字 `Tool` 让给工具接口（`api.go:67`），因为外部代码打交道更多的是"可执行的工具接口"，不是线缆结构。导出门面时的命名权分配，本身就是一种 API 设计。

`api.go:43-46` 的注释还埋了伏笔：`Usage` 在阶段三特别重要——成本核算到每个子任务靠它。下一节就看它从哪来。

### 2.8 Usage()：成本核算的数据源（agent.go）

内核里，`Agent` 结构体有一个私有字段（`mini-agent/internal/agent/agent.go:30-33`）：

```go
	// usage 累计本次任务所有 LLM 调用的 token 用量。
	// 阶段三的编排器需要按子任务核算成本（预算熔断、模型分级的收益量化），
	// 所以内核必须把用量暴露出来——否则上层只能瞎估。
	usage llm.Usage
```

`Run` 循环每轮累计（`agent.go:71-73`）：`a.usage.PromptTokens += resp.Usage.PromptTokens` 等三行。访问器（`agent.go:123-128`）：

```go
// Usage 返回本次任务累计的 token 用量（所有 ReAct 轮次之和）。
func (a *Agent) Usage() llm.Usage {
	return a.usage
}
```

这段代码是一段"接口设计要前瞻"的活教材：阶段一设计 `Run` 签名（`(string, error)`）时没留 Usage 返回值；阶段三要做成本核算时发现必须补。补救有两条路——改签名（破坏所有既有调用方）或加访问器（`Usage()`，向后兼容），内核选了后者。面试被问"加能力时怎么不动既有接口"，这就是答案。

两个使用纪律：① `Usage()` 是**整趟 Run 的累计量**，必须在 `Run` 返回后读；② 每子任务 new 一个 Agent（2.4 节），天然不串账。worker 读完这笔账，经 `CompleteSubtask` 落进 checkpoint，练习 4 的预算熔断与第 12 章的成本观测都消费它。

---

## 三、进阶拓展（带代码）

> 以下两段完整教学代码已在临时 module 中通过 `go vet` / `go build` / `go run` 验证。
> 它们是**自包含的模式演示**（LLM 用假实现，不依赖仓库）：讲清控制流，但刻意不同于项目骨架的类型与校验规则——项目版（更严的校验、checkpoint 落盘）留给你在练习 3/4 完成。

### 3.1 planner 的"生成-校验-重试"循环（可运行教学实现）

**为什么**：1.4 节讲了四道防线，这道"生成 → 解析 → 校验 → 带错重试"是其中第二、三道的合体，也是练习 3 的 `LLMPlanner.Plan` 要落地的核心控制流。要点有三个：校验是纯函数、重试把"原始输出 + 错误原因"喂回（保留上下文）、LLM 调用失败与校验失败走不同路径（传输层重试已在 client 内部做过，外层只管"内容不合法"）。

```go
// 教学示例：planner 的"生成-校验-重试"循环（自包含可运行，LLM 用假实现）。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxSubtasks = 8

type subtask struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
}

type plan struct {
	Subtasks []subtask `json:"subtasks"`
}

// validatePlan 是纯函数：不碰 LLM/IO，它存在的意义就是"不相信模型"。
// 教学版只演示三类规则；项目里的 ValidatePlan 更严（含 ID 查重等），练习 3 完成。
func validatePlan(p plan) error {
	if len(p.Subtasks) == 0 {
		return errors.New("子任务列表为空")
	}
	if len(p.Subtasks) > maxSubtasks {
		return fmt.Errorf("子任务数 %d 超过上限 %d", len(p.Subtasks), maxSubtasks)
	}
	for i, s := range p.Subtasks {
		if strings.TrimSpace(s.ID) == "" {
			return fmt.Errorf("subtasks[%d] 的 id 为空", i)
		}
		if strings.TrimSpace(s.Prompt) == "" {
			return fmt.Errorf("subtasks[%d] (%s) 的 prompt 为空", i, s.ID)
		}
	}
	return nil
}

// parsePlan 容错解析：模型可能裹 ```json 围栏或加前言，
// 截取第一个 '{' 到最后一个 '}' 再 Unmarshal 最稳。
func parsePlan(raw string) (plan, error) {
	s := strings.TrimSpace(raw)
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return plan{}, errors.New("输出中找不到 JSON 对象")
	}
	var p plan
	if err := json.Unmarshal([]byte(s[start:end+1]), &p); err != nil {
		return plan{}, fmt.Errorf("JSON 解析失败: %w", err)
	}
	return p, nil
}

type message struct {
	Role    string
	Content string
}

// planWithRetry：生成 → 解析 → 校验；不合法则把"原始输出 + 错误原因"
// 追加进 messages 重试（保留上下文，比从零重发成功率高）。
func planWithRetry(goal string, chat func([]message) (string, error), maxRetries int) (plan, error) {
	messages := []message{
		{Role: "system", Content: "你是任务分解器。只输出 JSON：{\"subtasks\":[{\"id\":\"s1\",\"title\":\"...\",\"prompt\":\"...\"}]}"},
		{Role: "user", Content: goal},
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		raw, err := chat(messages)
		if err != nil {
			// 网络/限流错误：传输层的退避重试已在 chat 内部做过，这里不叠加。
			return plan{}, err
		}
		p, err := parsePlan(raw)
		if err == nil {
			err = validatePlan(p)
		}
		if err == nil {
			return p, nil
		}
		lastErr = err
		messages = append(messages,
			message{Role: "assistant", Content: raw},
			message{Role: "user", Content: fmt.Sprintf("计划未通过校验：%v。请修正后重新只输出 JSON。", err)},
		)
	}
	return plan{}, fmt.Errorf("重试 %d 次后计划仍不合法: %w", maxRetries, lastErr)
}

func main() {
	calls := 0
	fakeChat := func(ms []message) (string, error) {
		calls++
		if calls == 1 {
			return "我觉得可以这样分：先调研，再写稿……", nil // 第一次输出垃圾
		}
		fmt.Printf("（第 %d 次请求带了 %d 条消息，末条是错误反馈）\n", calls, len(ms))
		return "```json\n{\"subtasks\":[{\"id\":\"s1\",\"title\":\"调研\",\"prompt\":\"调研竞品A的定价\"},{\"id\":\"s2\",\"title\":\"写稿\",\"prompt\":\"根据调研写分析稿\"}]}\n```", nil
	}
	p, err := planWithRetry("写一份竞品分析", fakeChat, 2)
	if err != nil {
		fmt.Println("分解失败:", err)
		return
	}
	fmt.Printf("分解成功：%d 个子任务（共调用 LLM %d 次）\n", len(p.Subtasks), calls)
}
```

运行输出：

```text
（第 2 次请求带了 4 条消息，末条是错误反馈）
分解成功：2 个子任务（共调用 LLM 2 次）
```

**取舍与生产注意**：① 校验规则写严不写松，但**校验保格式不保语义**——"分解得合理"是模型能力问题，靠 prompt、强模型和事后 eval，不在校验层解决；② 重试限次（项目默认 2 次），每次重试都是一轮 token，无限重试等于把成本控制权交给模型；③ 错误信息要带定位（`subtasks[2]`），模型才知道修哪里；④ 真实项目里 `chat` 内部用 `ChatWithRetry`（阶段一的 429/5xx 退避），两层重试职责不同：内层管传输，外层管内容——混在一层会双重退避，还把 401 这种永远失败的错误也重试。

### 3.2 预算哨兵错误 ErrBudgetExceeded 与双重熔断的配合

**为什么**：1.5 节讲了双重熔断缺一不可；落地时还要回答"预算耗尽这个信号怎么穿过 worker → pool → 编排器这么多层"。答案是**哨兵错误**：一个包级 `var`，各层原样向上返回（或 `%w` 包装），顶层用 `errors.Is` 识别。为什么是哨兵错误而不是布尔返回值——它能穿透多层调用自然冒泡，不用给每层签名加返回值；为什么不是字符串匹配——`errors.Is` 能穿透 `%w` 包装，且让调用方程序化区分"烧钱烧停的"（加预算续跑）和"真失败"（排查 bug），两种失败在看板上的处置完全不同。

```go
// 教学示例：评审循环的双重熔断（自包含可运行，worker/critic 用假实现）。
package main

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// ErrBudgetExceeded 是预算熔断的哨兵错误：
// 调用方用 errors.Is 把它和真实失败区分开——两者的处置完全不同
// （加预算续跑 vs 排查 bug），字符串匹配做不到这种程序化区分。
var ErrBudgetExceeded = errors.New("orchestrator: token 预算耗尽")

type workerFn func(prompt string) (output string, tokens int)
type criticFn func(output string) (pass bool, feedback string, tokens int)

// runWithReview 是"生成-评审-打回"循环的最小骨架：
// 轮次熔断管单点失控（一个任务怎么改都不过），预算熔断管总量失控。
func runWithReview(task string, w workerFn, c criticFn, maxRounds, budget int) (string, error) {
	var consumed atomic.Int64
	feedback := ""
	for round := 1; round <= maxRounds; round++ {
		// 预算检查点在每次 LLM 调用之前：之后检查意味着已经多烧了一轮。
		if budget > 0 && consumed.Load() >= int64(budget) {
			return "", ErrBudgetExceeded
		}
		prompt := task
		if feedback != "" {
			prompt = task + "\n\n【上次产出未通过评审，评审意见】：" + feedback +
				"\n请针对意见修正后重新完成。"
		}
		output, wt := w(prompt)
		consumed.Add(int64(wt))

		if budget > 0 && consumed.Load() >= int64(budget) {
			return "", ErrBudgetExceeded
		}
		pass, fb, ct := c(output)
		consumed.Add(int64(ct))
		if pass {
			fmt.Printf("  （第 %d 轮通过，累计 %d token）\n", round, consumed.Load())
			return output, nil
		}
		feedback = fb
	}
	return "", fmt.Errorf("评审 %d 轮仍未通过（轮次熔断）", maxRounds)
}

func main() {
	// 场景一：预算 150，第 1 轮烧 160 → 第 2 轮开工前熔断。
	_, err := runWithReview("写纪要",
		func(string) (string, int) { return "草稿", 100 },
		func(string) (bool, string, int) { return false, "缺少数据", 60 },
		5, 150)
	fmt.Println("场景一 errors.Is(ErrBudgetExceeded):", errors.Is(err, ErrBudgetExceeded))

	// 场景二：预算不限（0），永远 reject → 撞轮次上限。
	_, err = runWithReview("写纪要",
		func(string) (string, int) { return "草稿", 10 },
		func(string) (bool, string, int) { return false, "再改改", 5 },
		3, 0)
	fmt.Println("场景二:", err)

	// 场景三：第一次 reject（带意见），第二次 pass；重做 prompt 必须含意见。
	n := 0
	out, err := runWithReview("写纪要",
		func(p string) (string, int) {
			if n > 0 && !strings.Contains(p, "缺少数据支撑") {
				fmt.Println("  （警告：重做 prompt 没带评审意见）")
			}
			return "纪要成稿", 10
		},
		func(string) (bool, string, int) {
			n++
			if n == 1 {
				return false, "缺少数据支撑", 5
			}
			return true, "", 5
		},
		3, 0)
	fmt.Println("场景三:", out, err)
}
```

运行输出：

```text
场景一 errors.Is(ErrBudgetExceeded): true
场景二: 评审 3 轮仍未通过（轮次熔断）
  （第 2 轮通过，累计 30 token）
场景三: 纪要成稿 <nil>
```

**取舍与生产注意**：① 检查点在每次 LLM 调用**之前**——之后检查意味着已经多烧一轮，熔断要拦在花钱的动作前面；② `consumed` 用 `atomic.Int64`，因为真实编排器里多个子任务并发共享同一个任务预算（`go test -race` 会抓普通字段）；③ 真实项目里 `consumed` 要从 checkpoint 的 `TotalTokens` **续算**——否则崩溃恢复成了预算重置漏洞；④ worker 和 critic 的 token 都要入账，只算 worker 会低估拉锯成本；⑤ 生产上还有第三重熔断：wall-clock 超时（第 8 章 pool 的 jobTimeout 已部分覆盖）。

### 3.3 critic prompt 设计要点与 feedback 回喂

**为什么**：critic 是 LLM，它的输出质量同样由 prompt 决定。三个设计要点：

1. **要求先列问题再给结论**：让模型先写分析、后写 PASS/REJECT，是利用"生成顺序即推理顺序"——先下结论再补理由的评审质量明显更差（与 chain-of-thought 同一原理）。
2. **输出格式强约定**：基础版用裸文本（首行 PASS / REJECT，第二行起意见）——结论只有两种，字符串解析够用且省 token；进阶方向是结构化输出 `{"pass","score","issues"}`（score 支撑质量趋势观测，issues 让 worker 逐条对着改），代价是 JSON 多一层解析失败面。
3. **只评不改**：评审 prompt 里写明"重写是执行 agent 的事"——critic 改稿会让两个 agent 的职责再次混杂，且打回重做的意义就消失了。

打回重做时，feedback 怎么回到 worker？`Worker.Execute` 的签名只有 `spec`，所以构造一个**带反馈的 spec 副本**，把意见拼进 Prompt（3.2 代码里的 `prompt = task + "【上次产出未通过评审……" + feedback` 就是这一行）。新 Agent 下一轮看到的仍是一份自包含 prompt——不动 Worker 接口、不需要 agent 间消息通道。两个纪律：**spec 是值类型，重做用副本，不要改共享的原始 spec**（并发下其他 job 可能在读）；**空反馈要兜底**——critic 给了 REJECT 却没写意见时，补一句"请对照子任务要求全面检查并改进"，空反馈等于让 worker 盲改。

### 3.4 讨论：同步编排 vs 事件驱动

本项目的编排器是**同步**的：`Run` 一个调用从头推进状态机到尾，pool.Run 阻塞等全部 job 结束。另一条路是**事件驱动**：状态变更发事件，各 agent 订阅响应（worker 完成 → 发事件 → 汇总器被触发）。

为什么学习项目选同步：

- **简单可调试**：一条调用栈走到底，日志按时间线读得懂；事件驱动的链路是隐式的——"谁触发了这个 handler"要翻订阅关系表才能回答；
- **与 checkpoint 天然契合**：同步推进意味着每个状态迁移点是显式的，落盘时机一目了然；事件驱动下"此刻全局处于什么状态"要从事件流 replay 出来；
- **规模匹配**：单进程、任务量小，同步的吞吐足够。

事件驱动的真正优势在**弹性与解耦**：worker 可以跨进程/跨机器（事件队列就是天然的任务队列），新增一类订阅者不改编排器；失败重试、优先级、背压都可以交给消息中间件。代价是链路隐式难追踪、状态一致性要自己设计（事件丢失/重复）、本地开发要起一堆基础设施。演进路径通常是：同步编排跑通 → 某个环节成为瓶颈或需要跨进程 → 只把那个环节改成事件驱动。**先同步，痛了就局部改**，和"能单 agent 就别多 agent"是同一个哲学。

---

## 四、面试视角

> 以下每题给"标准回答 → 常见追问 → 加分点"。自测方法：不看回答口述，再对照差距。

**Q1：什么时候该用多 agent，什么时候不该？**

标准回答：默认不该——单 agent + 工具能解决的，先优化 prompt、加 RAG、拆工具描述。该拆的三个信号：① context 膨胀到压缩都救不回来；② 工具数量多到模型选不准（几十个是拐点）；③ 职责混杂到一个 prompt 写不下（又规划又执行又评审）。

追问链：
- "多 agent 的代价是什么？" → 状态一致性（多份 context 怎么共享）、成本倍增（每个 agent 一份 system prompt + 历史，Anthropic 实测多 agent 约 4 倍 token）、调试困难（失败要还原谁把什么传给了谁，必须树状 trace）；
- "所以多 agent 是升级版单 agent？" → 不是，是 trade-off：用状态一致性 + 成本 + 调试复杂度，换职责隔离与可控 context。

加分点：主动给出决策顺序"先 prompt，再工具，再 workflow，最后才 multi-agent"，并指出出处（Anthropic: Building effective agents）。先讲"能单 agent 就别多 agent"再讲架构，是区分真做过与背概念的点。

**Q2：planner-worker 和 handoff 有什么区别？**

标准回答：planner-worker 是分解-并行-汇总（管理者视角）：planner 一次性拆成子任务，worker 并行跑，结果回流汇总。handoff 是路由-接力（流水线视角）：任一时刻只有一个 agent 持有对话，它判断"该换人了"就把控制权连同整个对话上下文转给下一个，一去不复返。

追问链：
- "判断用哪个的题眼？" → **子任务能否并行**：能并行拆解是 planner-worker，必须串行接力/分诊是 handoff；
- "handoff 转交的是什么？" → 不只是新任务，是整个对话上下文（否则用户得重说一遍）——这是它和 planner 的本质区别：状态随任务走；
- "critic 和这两个什么关系？" → critic 不是独立拓扑，是可叠加的质检层，寄生在生成者之后。

**Q3：多个 agent 之间怎么共享上下文？**

标准回答：两种范式。① 黑板：共享存储（DB/内存结构），各 agent 读写公共状态——本项目的 checkpoint/SQLite 就是黑板，优点是持久可恢复、可查询，缺点是要设计 schema 和并发写；② 消息传递：agent 间直接传消息（handoff 的上下文交接），优点是边界清晰，缺点是状态散在消息流里难恢复。生产混用：黑板存事实，消息传指令。

加分点：把黑板选择和崩溃恢复、HITL 串起来——状态外置（黑板）才能"暂停-恢复"和崩溃续跑；如果状态在内存 channel 里，进程一重启审批点就没了（第 9、11 章）。

**Q4：多 agent 系统成本怎么控制？**

标准回答：四招。① 模型分级——planner/critic 用强模型，执行型 worker 用便宜模型；② 预算熔断——任务级 token 预算，超了直接 failed，防 critic 循环烧钱；③ 缓存与复用——相同前缀命中 prompt 缓存（第 1 章进阶 3.2），重复检索结果复用；④ 砍 context——worker 只拿自包含的子任务描述，不背整个任务历史。

追问链：
- "成本数据从哪来？" → 每次 LLM 响应的 usage 是唯一真实数据源，内核累计后经 `Usage()` 暴露，worker 执行完落 checkpoint；没有计量就没有成本控制；
- "预算熔断了任务怎么办？" → 任务 failed 但现场在 checkpoint 里（状态外置），人工可以加预算后 Resume 续跑，已完成的子任务因幂等键跳过——熔断不是丢任务，是暂停烧钱。

**Q5：planner 输出的计划怎么保证可用？**

标准回答：四道防线——① 结构化输出约束（prompt 给 schema 样例 / JSON mode / 伪工具）；② 代码侧确定性校验（字段、类型、子任务数上限、ID 唯一、依赖不成环）；③ 校验失败把"原始输出 + 错误"喂回重试（限次）；④ 重试耗尽任务 failed 进人工。原则：模型负责生成，代码负责把关——任何进状态机的 LLM 输出都必须先过确定性校验。

追问链：
- "校验能保计划质量吗？" → 不能。校验保格式合法性，"分解得合理"是模型能力问题——靠强模型、prompt 里的分解原则、以及事后 eval/轨迹评估。别把两层混为一谈；
- "重试为什么是带错误反馈而不是从零重发？" → 保留上下文让模型知道错在哪，成功率显著更高；且反馈要带定位（哪个子任务哪个字段），否则等于没反馈。

**Q6：critic 自己也是 LLM，它出错了怎么办？**

标准回答：critic 是质量增强层，不是正确性依赖，所以它的故障绝不能转嫁成任务失败。三级处理：① 单次出错（含输出无法解析——"模型说废话"≠"模型说不合格"）→ **放行本次产出 + 记 log**；② 连续出错达阈值 → 判定 critic 服务性故障，整个任务跳过评审，不再浪费评审 token；③ 全过程在 trace/log 留痕，降级是可审计的，不是静默放水。

追问："为什么不能当成 reject？" → 那会把 critic 的故障放大成 worker 的无意义重做甚至熔断 failed——用一个故障组件的输出做破坏性决策，是最差的失败模式。降级是"有计划的牺牲质量保可用"。

加分点：补一句评审本身也要计 token、计入预算熔断——评审不是免费的。

**Q7：子任务部分失败时，编排器怎么汇总？**

标准回答：部分失败语义——单个 job 的失败收进结果与子任务 checkpoint（FailSubtask 落错误信息），不中断其他 job（第 8 章 errgroup 的"模式 B"：错误当结果值收，不传播）；分发结束后**重新 LoadTask 以 checkpoint 为准**汇总：done 的产出按序拼接，failed 的列出失败原因；全部失败才把任务迁 failed，否则 done。

追问链：
- "为什么汇总以 checkpoint 为准而不是内存里的结果？" → Resume 场景下部分产出是上一个进程写进 SQLite 的，本轮内存结果里根本没有——"状态在库里，进程无状态"要贯彻到最后一个环节；
- "为什么不用 LLM 做汇总？" → 确定性拼接（标题+产出）省一轮 token、无新失败点、格式可控；LLM 汇总是产品化阶段的锦上添花，不是编排器的职责底线。

---

## 五、常见坑

1. **未校验的 LLM 计划直接驱动状态机**：再强的 prompt 也不能保证每次输出合法。未校验的计划一旦落盘分发，错误就被 checkpoint 固化、被并发放大——空 ID 子任务撞主键、30 个子任务烧穿预算，都发生在"相信模型"的那一行。校验是纯函数，写严不写松。
2. **critic 循环无预算熔断**：只有轮次上限管不了"10 个子任务每个都在限内拉锯"的总量失血。真实事故形态：评审标准过严 + worker 能力不够 → 每个子任务拉锯 N 轮 → 一觉睡醒账单爆炸。双重熔断缺一不可，且预算要从 checkpoint 续算（否则崩溃恢复成了预算重置漏洞）。
3. **worker 背整个任务历史**：把用户原始目标 + 其他子任务的结果全塞给每个 worker，context 膨胀原样保留，多 agent 白拆——还多了 N 份 token 开销。worker 的输入就是一份自包含的 `SubtaskSpec.Prompt`，planner 负责把必要上下文写进去。
4. **所有 agent 用同一个模型**：全用强模型，worker 执行浪费钱；全用便宜模型，planner 分解和 critic 评审能力不足。模型分级落地靠接口注入——换实现不换编排器。
5. **LLM 调用失败与校验失败混在一个重试循环**：chat 返回 error（网络/限流）时外层再循环是双重退避（内层 `ChatWithRetry` 已退避过），还会把 401 这种永远失败的错误也重试。两条路径分开：传输错误直接上抛，内容不合法才带反馈重试。

---

## 六、动手练习

本章对应阶段三的练习 3 和练习 4，位置都在 `stage-03-multi-agent/internal/orchestrator/`。**先完成练习 1（pool）和练习 2（task Store）再做本章练习**——编排器的测试要跑真的并发池和真的 SQLite checkpoint。

**练习 3：Planner/Worker 编排器**（`TODO(练习3)` 共五处）：

- `planner.go`：`ValidatePlan`（四条校验，错误带定位）、`LLMPlanner.Plan`（生成-解析-校验-带错重试，进阶 3.1 的教学代码演示了控制流，但项目版的校验规则更严、要走 checkpoint——照抄教学版不算完成）；
- `worker.go`：`AgentWorker.Execute`（每子任务 new 一个 `api.Agent`，`Usage()` 记账）；
- `orchestrator.go`：`Run`（状态机驱动 + checkpoint + 并发分发 + 汇总）、`Resume`（崩溃恢复续跑）。
- 验收：`go test ./internal/orchestrator/` 通过——用假 Planner/Worker 覆盖完整生命周期、部分失败仍汇总、崩溃恢复跳过 done 子任务。
- 参考答案：`docs/solutions/stage-03/exercise-3-planner-worker.md`（完成后再看）。

**练习 4：Critic 评审循环**（`TODO(练习4)` 两处）：

- `critic.go`：`LLMCritic.Review`（PASS/REJECT 解析，无法解析返回 error 走降级，评审 token 入账）；
- `orchestrator.go`：评审循环 + 双重熔断（轮次上限 + token 预算，哨兵错误 `errors.Is` 判定）+ critic 降级（放行 + 记 log + 连续出错跳过评审，计数器用 `atomic`）。
- 验收：测试覆盖四条——先 reject 后 pass（第 2 次 prompt 含 feedback）、永远 reject 触发轮次熔断、预算熔断任务 failed、critic 连续出错降级放行；`go vet` 与 `go test -race -count=1` 全绿。
- 参考答案：`docs/solutions/stage-03/exercise-4-critic-loop.md`（完成后再看）。

---

## 本章小结

- 多 agent 是用状态一致性 + 成本 + 调试复杂度，换职责隔离与可控 context——**能单 agent 就别多 agent**，三个该拆的信号之外都是过度设计。
- 模式四选的题眼是"子任务能否并行"：planner-worker 分解-并行-汇总，handoff 路由-接力，critic 是可叠加的质检层，swarm 了解即可。
- planner 的输出是 LLM 的输出：结构化约束 → 确定性校验 → 带错重试 → 耗尽 failed，**模型负责生成，代码负责把关**。
- critic 循环要双重熔断（轮次管深度、预算管广度）+ 降级放行（critic 是质量增强，不是单点故障）。
- 模型分级落地靠接口注入；成本核算落地靠 `Usage()`——没有计量就没有成本控制。
- 阶段一的 ReAct 内核原样当了 worker 执行体：每子任务一个 Agent，跑完即弃，context 天然隔离。

下一章：[第 11 章：Human-in-the-loop 审批点](11-human-in-the-loop.md)——让高风险操作停下来等人：状态机进入 `waiting_human`、任务让出、审批后从断点恢复，以及 `ErrWaitingHuman` 哨兵错误的完整落地。
