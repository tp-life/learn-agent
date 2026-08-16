# 第 13 章：产品化集成——HTTP/SSE API 与实时看板

> 对应阶段：阶段三（深入）· 项目 3 `stage-03-multi-agent/`
> 代码位置：`stage-03-multi-agent/internal/server/`（HTTP/SSE 门面）、`stage-03-multi-agent/cmd/server/`（服务入口）、`stage-03-multi-agent/web/`（Next.js 看板）
> 前置：第 8-12 章（编排引擎的全部内层包）、第 3 章（SSE 客户端解析）、第 7 章（Next.js 全栈）
> 学完后你能讲清：为什么 HTTP 层只能"接单"不能"等单"、SSE / WebSocket / 轮询怎么选、poll-diff 推送为什么比事件回调稳、后台 goroutine 的 ctx 纪律，以及这套系统怎么固化成简历上的架构文档与故障演练报告。

---

## 本章地图

- 项目 3 总装图：第 8-12 章的零件在哪里合体
- 请求生命周期 vs 任务生命周期：HTTP 层"接单即返"的根因
- context 纪律：后台 goroutine 为什么不能用请求的 ctx
- 推送选型：SSE / WebSocket / 轮询三选一
- poll-diff：轮询 + diff + 心跳的推送设计
- 代码精讲：server 骨架五个路由、cmd/server 双模式、web 看板结构
- 进阶：最小 SSE 示例（协议第三次相遇）、背压与断线续传、生产化清单
- 面试：系统设计题"多 agent 任务执行平台"的 5 分钟架构版预演
- 练习 8（HTTP/SSE 引擎 + 看板）与练习 9（架构文档 + 故障演练报告）

---

## 一、概念详解

### 1.1 项目 3 总装图：所有零件的集成点

第 8-12 章把多 agent 系统的内层零件逐个造好了，但它们一直缺一个"产品形态"——用户不可能敲 CLI 提交任务、盯着日志看进度。本章把引擎包上 HTTP API、接上实时看板，项目 3 才成为一个能演示的系统：

```
浏览器看板（web/，Next.js）                   ← 第 7 章的全栈手艺
   │  POST /api/tasks                 提交任务
   │  GET  /api/tasks、/api/tasks/{id} 查列表 / 详情
   │  POST /api/tasks/{id}/approve    人工审批
   │  GET  /api/tasks/{id}/events     SSE 订阅实时进度
   ▼
internal/server（HTTP 门面，本章）             ← 只做协议翻译，零业务逻辑
   │  提交 / 审批 → 后台 goroutine 跑长任务
   ▼
orchestrator 编排器                            ← 第 10 章（planner / worker / critic）
   ├── pool       并发底座                     ← 第 8 章（errgroup + 限流 + 超时预算）
   ├── hitl       审批闸                       ← 第 11 章（waiting_human 暂停-恢复）
   └── trace      可观测                       ← 第 12 章（嵌套 span + 成本归因）
   │  每次状态迁移都落盘
   ▼
SQLite checkpoint（task.Store）                ← 第 9 章（状态机 + 崩溃恢复）
```

三条主线在 HTTP 这一层汇合，各对应一条前文埋下的伏笔：

1. **写路径（提交 / 审批）**：HTTP 请求只负责"接单"和"触发"，长任务在后台 goroutine 里跑——1.2 节展开；
2. **读路径（查询 / SSE）**：数据全部从 SQLite checkpoint 读，进程无状态——第 9 章"状态外置"的红利在这里兑现：看板随便刷新、服务随便重启；
3. **demo / 真实双模式**：`cmd/server` 按 `DEEPSEEK_API_KEY` 是否设置切换假实现与真 LLM——第 10 章 Planner/Worker 接口注入的系统级红利。

第 12 章的 MCP server（`cmd/mcp-server`）是支线（工具对外暴露），不在这条请求链路上。

### 1.2 请求生命周期 vs 任务生命周期：HTTP 层只"接单"

把两个时间尺度并排看，本章一半的设计决策都从这个矛盾里长出来：

- **一次 HTTP 请求的合理寿命是秒级**：浏览器 fetch 默认容忍几十秒，网关/反向代理普遍在 30-120s 掐掉空闲请求；
- **一个 agent 任务的寿命是分钟到小时级**：planner 分解十几秒，每个 worker 是几十秒的 LLM 调用，中间还可能停下来等人审批几天。

如果让提交任务的 handler 同步等任务跑完，等于把 HTTP 请求挂在那里几分钟：浏览器超时、网关 504、连接数被占满——全是事故。所以 HTTP 层只做三件事：**校验入参 → 生成任务 ID → 把长任务丢进后台 goroutine，立即返回 `202 Accepted`**。202 的语义是"请求已受理，处理进行中"，比 200 准确——这是 HTTP 协议专门为异步受理场景准备的状态码。

进度怎么拿？读路径与写路径彻底分离：任务状态每次迁移都落在 SQLite（第 9 章），客户端随时可以 `GET /api/tasks/{id}` 查当前状态，或用 SSE 订阅推送（1.4 / 1.5 节）。**HTTP 层不持有任何任务状态，它只是一个翻译器**——这个性质让服务进程可以随时重启：`cmd/server` 启动时会把未完成任务从 checkpoint 续上（`cmd/server/main.go:104`）。

**demo 模式与真实模式**。`cmd/server/main.go:84` 做了一个环境开关：未设置 `DEEPSEEK_API_KEY` 时用固定三步计划的假 Planner + 延时回显的假 Worker，设了才接真 LLM。demo 模式不是偷工减料，它有明确的产品价值：

- 演示零成本、零网络依赖——面试现场热点断网也能跑完全链路；
- 结果可预期——固定计划里必有一个高风险子任务，审批点必然出现，不会演示到一半 LLM 给你分解出八个野路子子任务；
- HTTP 层与看板的开发、测试不需要等 LLM 链路（练习 3）就绪。

能这么切换，是因为第 10 章把 Planner/Worker 设计成了接口——"换实现"从测试技巧变成了产品功能。

### 1.3 context 纪律：后台 goroutine 不能用请求的 ctx

本章最高频的新手 bug，值得单独一节。Go `net/http` 的约定：**handler 返回后，`r.Context()` 即被取消**。如果把 `r.Context()` 透传给后台 goroutine 里的编排调用——

```go
// 错误示范：响应一返回，r.Context() 就被取消，
// 分钟级的编排任务刚起步就被掐死。
go s.orch.Run(r.Context(), id, goal)
```

任务的表现会非常诡异：curl 提交成功，任务状态却永远停在 planning 或 running，日志里一条 `context canceled`。排查时很容易怀疑编排器，其实死在 HTTP 层。

正确做法是从 `context.Background()` 派生一个全新的 ctx（需要预算就再叠 `WithTimeout`，呼应第 8 章的超时预算分层）：

```go
// 任务的生命周期属于任务自己，不属于这次 HTTP 请求。
go s.orch.Run(context.Background(), id, goal)
```

反过来，**读路径用 `r.Context()` 恰恰是对的**：列表 / 详情查询、SSE 的单次读库都该挂在请求 ctx 上——客户端断开了，查询没必要继续；SSE 循环也靠 `r.Context().Done()` 退出（见 2.4）。一句话总结：**触发型写路径用 `context.Background()`，读路径用 `r.Context()`**。

### 1.4 SSE vs WebSocket vs 轮询：三选一怎么选

看板要"实时看到任务进度"，三个候选方案：

| 维度 | SSE | WebSocket | 轮询 |
| --- | --- | --- | --- |
| 方向 | 服务端 → 客户端单向 | 双向 | 客户端定期拉取 |
| 协议 | HTTP 之上的文本协议（`text/event-stream`），无升级握手 | 独立帧协议，握手要 Upgrade | 普通 HTTP 请求 |
| 浏览器端 | `EventSource` 原生 API：自动分帧、自动重连 | `WebSocket` 原生，但心跳/重连/背压自己管 | `setInterval` + fetch |
| 基础设施亲和性 | 就是一条 HTTP 长响应，代理/网关都认识 | 代理需显式放行 Upgrade | 最好，但空转请求多 |
| 适用场景 | 单向状态推送（进度、通知、行情） | 双向高频（协作编辑、聊天室、游戏） | 低频兜底、环境受限 |

任务进度推送是**天然单向**场景：看板不需要往这条连接里发任何东西（审批走独立的 POST）。选 SSE 就是选"协议最简单、浏览器帮你干活最多"的方案：分帧和重连浏览器全包，服务端只按格式写文本。

注意本项目其实**两种方案都用了**，正好是一次真实选型对照：列表页是低频总览，2s 轮询够用且简单（`web/app/page.tsx:32`）；详情页要逐子任务跟进状态流转，才上 SSE（`web/app/tasks/[id]/page.tsx:25` 的 TODO①）。"哪里都值得上 SSE"和"凡事用 WebSocket"一样，都是没想清楚需求的表现。

### 1.5 poll-diff：比"主动回调"简单可靠的推送设计

传输定了，下一个问题是：**服务端怎么知道"有进度可推"？** 两种思路：

- **事件 hook**：orchestrator 每次状态迁移时回调通知 SSE 层。毫秒级实时，但要改编排器契约（练习 3/5 已验收的 Run/Resume 都得插回调），还要处理回调的并发与 panic 隔离，复杂度上一个台阶；
- **poll-diff（本项目的选择）**：SSE 循环每 1s 从 SQLite 读一次任务快照，序列化后与上次推送的字节比较，**有变化才推**；每 15s 推一行注释行心跳保活。

poll-diff 胜出的理由有三层，面试都能讲：

1. **零侵入**：orchestrator / task / hitl 的契约一行不改。SQLite 本来就是真相源，读它不需要任何人配合；
2. **跨进程正确**：这是更本质的理由。approve 触发的状态变化发生在 HTTP 进程里；如果将来编排器拆到独立进程跑，事件 hook 只活在编排器进程，HTTP 进程发起的迁移它根本收不到——而 poll 读库天然跨进程正确。第 9 章"状态外置"红利的又一次兑现；
3. **断线自愈**：推送的是**全量快照**而不是增量事件，客户端断线重连后收到的第一帧就是最新全量状态，断线期间错过多少帧都无所谓。（事件流方案要做到这点就得引入序号与补发日志，进阶 3.2 会写这个模式。）

代价是 ≤1s 延迟和每连接每秒一次 SQLite 读——看板是"给人看"的场景，秒级完全无感。**事件 hook 是性能瓶颈真的出现后的优化方向，不是起点**。

心跳为什么用注释行（`: hb\n\n`）而不是 `data:` 帧：注释行不会触发前端的 `onmessage`，保活但不惊动应用层。nginx 这类反向代理默认会掐掉 60s 无字节的长连接，15s 心跳就是发给中间设备看的"我还活着"。

### 1.6 练习 9 的性质：表达输出就是面试素材

本章对应的第二个练习不写代码，写两份文档：`architecture.md`（架构文档）与 `failure-drills.md`（故障演练报告）。别把它当作文作业——它的定位是**把阶段三的全部知识固化成可复述的资产**：

- **讲得清的前提是写得清**。阶段验收标准要求"能讲清为什么这样拆 agent、失败如何处理"。按"问题 → 架构 → 量化结果"的口径落成文字，面试时就是脱稿底稿；
- **故障演练是把容错声明变成证据**。声明了崩溃恢复，就真的 kill 一次进程、贴出 sqlite3 查询结果；声明了预算熔断，就贴出测试断言。简历上"写了一个多 agent demo"和"设计了容错机制并完成五个故障演练（附实测记录）"是两种分量；
- 真实团队里架构文档与 chaos drill 报告就是生产系统的标配产物——这是工程习惯训练，不是额外负担。

---

## 二、代码精讲

### 2.1 server 包：零业务逻辑的协议翻译层

`stage-03-multi-agent/internal/server/server.go:1` 的包注释把定位说得很死：本包只做"HTTP 请求 ↔ 编排引擎方法调用"的协议翻译；状态机、审批闸、熔断全部在内层包里，本包零业务逻辑。

这条纪律的价值在于：**将来换任何前端形态（CLI、gRPC、定时任务），内层包一行不动**。项目里已有旁证——第 11 章的 `cmd/hitl-demo` CLI 和本章的 HTTP 服务消费的是同一套 `hitl.Service` + `orchestrator`，两个"前端"互不影响。分层值不值钱，就看第二个前端接入时要不要改内层。

### 2.2 DTO：对外契约与内部模型的隔离

API 不直接序列化 `task.Task`，而是定义了一层投影类型（`server.go:48`）：

```go
type TaskView struct {
	ID          string      `json:"id"`
	Goal        string      `json:"goal"`
	Status      task.Status `json:"status"` // Status 是 string 类型，直接序列化成 "running" 等
	TotalTokens int         `json:"total_tokens"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}
```

DTO（Data Transfer Object）这层隔离解决两个问题：内部模型 `task.Task` 以后加字段（比如练习 4 的预算字段）不会意外泄漏成对外契约；反过来对外契约要调整（改字段名、加派生字段）也不用动内部模型。

两个字段级决策值得停下来想：

- `SubtaskView.Prompt`（`server.go:62`）必须带出——审批人要看"它到底要干什么"才能做决定，这是第 11 章 HITL 在 API 层的落点；
- `TaskDetailView`（`server.go:72`）被 `GET /api/tasks/{id}` 和 SSE 快照**共用**：详情页首屏渲染与 SSE 实时刷新吃同一份结构，前端只维护一套解析（`web/lib/api.ts:44` 的注释同样标了这一点）。

审批请求体 `decideRequest`（`server.go:84`）里 `By`（审批人）是必填字段——审计不留名等于没有审计，第 11 章的纪律延伸到了 HTTP 层。

### 2.3 Server 装配：依赖、路由与统一出口

`Server` 结构体（`server.go:115`）持有四个依赖：`store`（checkpoint 真相源）、`svc`（审批落盘）、`orch`（长任务入口）、`db`（本包自建的读连接），外加两个时间参数（`server.go:141`）：`pollInterval`（SSE 轮询间隔，默认 1s）与 `heartbeatInterval`（心跳间隔，默认 15s）——1.5 节的两个数字就是从这里来的。

`New`（`server.go:132`）里有一个值得想的设计：为什么 server 要自己再开一条 SQLite 连接？因为任务列表需要"全部任务含终态"，而 `task.Store` 的契约（练习 2）只暴露 `ListResumable`（非终态）。不回改练习 2 的契约，照 `hitl.NewService` 的先例对同一文件开自己的**只读**连接。配套纪律：**状态修改一律走 Store/Service 的状态机守卫，绝不直接 SQL 改状态**——否则练习 2 的守卫、练习 5 的审批语义全被绕过。`SetMaxOpenConns(1)`（`server.go:138`）与 task.Open 同一理由：SQLite 单写者，钉成串行。

路由（`server.go:156`）用 Go 1.22+ 标准库 ServeMux 的方法+通配符模式，五条路由一目了然：

```go
mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
mux.HandleFunc("GET /api/tasks", s.handleListTasks)
mux.HandleFunc("GET /api/tasks/{id}", s.handleGetTask)
mux.HandleFunc("POST /api/tasks/{id}/approve", s.handleApprove)
mux.HandleFunc("GET /api/tasks/{id}/events", s.handleTaskEvents)
```

不引第三方路由库——五条路由用不上，标准库的模式路由已经能表达方法分派与路径参数（`r.PathValue("id")`）。这与项目"不引重型框架"的约定一致。

`withCORS`（`server.go:170`）放开跨域：看板 dev server 跑在 `:3000`，API 在 `:8080`，浏览器跨源 fetch / EventSource 都需要这组头。注意注释里的生产警告：`Access-Control-Allow-Origin: *` 是最宽配置，真实部署应收紧为看板域名。

`writeJSON` / `writeErr`（`server.go:185`）统一了出口：所有响应都是 JSON，错误统一是 `{"error": "..."}`——前端 `web/lib/api.ts:48` 的 `request()` 只处理这一种形状，契约简单到不会错。

### 2.4 五个 handler：TODO(练习8) 逐个拆解

五个 handler 的实现是练习 8 的主战场（`server.go:199-341`），骨架的 TODO 注释已经把流程、提示、验收写全。这里讲清每个 handler 背后的**设计决策**，实现留给你。

**`handleCreateTask`（`server.go:223`）——提交任务。** 流程一句话：解析 → 校验 → 生成任务 ID → 后台 goroutine 跑 `orch.Run` → 立即 202。决策点有四个：① 返回 202 而不是 200（1.2 节）；② goroutine 的 ctx 用 `context.Background()` 而不是 `r.Context()`（1.3 节，本练习第一坑）；③ `Run` 返回 `ErrWaitingHuman` 是"让出等审批"不是失败（第 11 章的哨兵错误），不记错误日志，任务状态已在 checkpoint 里，HTTP 层无需再做什么；④ 骨架诚实地标注了一个已知小竞态——`Run` 在 goroutine 里才 `CreateTask`，202 返回后立刻 GET 详情可能短暂 404。不为它加同步，因为前端轮询 / SSE 天然容忍（下一拍就有了）。**容忍无害竞态比消除它更便宜**——这是很典型的工程判断。

**`handleListTasks`（`server.go:244`）——任务列表。** 走本包自建读连接 SELECT，按 `created_at` 倒序。细节：空列表要返回 `[]` 而不是 `null`（`make([]TaskView, 0)` 初始化），前端 `map` 不用判空——API 设计里"形状稳定"是对调用方的基本善意。

**`handleGetTask`（`server.go:262`）——任务详情。** `r.PathValue("id")` → `store.LoadTask` → `toDetailView`。不存在与读取失败统一 404：详情接口的调用方只关心"有没有这个任务"。

**`handleApprove`（`server.go:290`）——人工审批。** 五个里最绕的一个，决策点三个：① 决定落盘必须走 `svc.Decide`（不直接 SQL 改状态）；子任务不在 `waiting_human` 时 Decide 会报错，透传为 409 Conflict——"当前状态不允许这个操作"，重复点击、过期页面都撞在这里；② **全部批完才触发续跑**——一个任务可能同时有多个待批项，批一个就 Resume 一次的话，第一次 Resume 会撞上剩余的审批闸立刻又让出，无害但浪费一轮 goroutine 与状态迁移；所以 Decide 后要重新 LoadTask，确认任务在 `waiting_human` 且没有其它子任务还在等批，才后台 Resume；③ 还有别的待批项时也返回 200——本次审批本身已成功，看板靠 SSE 看到"还在等下一项"。

**`handleTaskEvents`（`server.go:339`）——SSE 事件流。** 五步流程：断言 `http.Flusher`（不支持则 500）→ 先 LoadTask 确认任务存在（别让看板对着空任务空转）→ 写 SSE 响应头三件套 → poll-diff 循环（1s 轮询、有变化才推、15s 注释行心跳）→ `r.Context().Done()` 退出。帧格式手写，不需要库：

```text
data: {"id":"...","status":"running",...}\n\n
```

即 `data: ` 前缀 + 单行 JSON + 两个换行结尾。写完必须 `Flush`，否则数据堆在缓冲区里，"实时"名存实亡。还有一个反直觉点：**任务到终态后不要主动关连接**——浏览器 EventSource 会自动重连，关了等于让它按重连节奏反复刷同样的终态快照；保持连接靠心跳挂着即可，页面关闭时浏览器断开，`r.Context()` 随之取消，循环自然退出。

### 2.5 cmd/server：双模式与崩溃恢复的接线

`stage-03-multi-agent/cmd/server/main.go` 是装配层，骨架已完整提供，看懂三块接线即可。

**demo 假实现（`main.go:41`、`main.go:55`）。** `demoPlanner` 预置固定三步计划，其中"删除过期数据"标了 `RequiresApproval: true`——演示的确定性来自这里：审批点必现。`demoWorker` 的两个细节都不是装饰：① 1.2s 延时模拟真实 worker 的 LLM 耗时——没有它任务瞬间跑完，看板上看不到 `pending → running → done` 的流转，SSE 演示名存实亡；② 返回 42 个假 token——看板成本栏有数字可看，且能验证 token 记账链路（第 12 章成本归因的末端）。

**模式切换（`main.go:84`）。** 按 `DEEPSEEK_API_KEY` 是否设置选择 demo 或真实模式（`LLMPlanner` + `AgentWorker`，练习 3 的实现）。真实模式下 registry 传 nil = worker 无工具（纯生成型子任务）；要挂计算器 / HTTP 抓取 / 知识库工具时，在这里 `api.NewRegistry()` + Register——mini-agent 的全部工具经门面包导出（阶段三结构决策），即插即用。

**启动即恢复（`main.go:104`）。** 进程启动时 `store.ListResumable` 找出上次没跑完的任务，逐个后台 `Resume`——`waiting_human` 的会再次让出等审批，已批未执行的接着跑。这是第 9 章"状态外置、进程无状态"在服务入口的兑现：**重启 = 自动续跑**，也是练习 9 故障演练报告里演练 1 的 HTTP 路径。

### 2.6 web/ 看板：页面分工与 SSE 消费位置

看板是刻意"裸写"的 Next.js（无 UI 库、无状态管理库），四个文件分工：

- **`web/lib/api.ts`——类型与请求封装。** `API_BASE`（`api.ts:11`）默认指向本地 `:8080`，可用 `NEXT_PUBLIC_API_BASE` 覆盖；`TaskStatus`（`api.ts:15`）与 Go 侧六个状态一一对应；接口字段照吃 DTO 的 snake_case（Go JSON tag 约定）。`request()`（`api.ts:48`）统一解析 `{"error": ...}` 错误形状。
- **`web/app/page.tsx`——列表页（已完整提供）。** 提交表单 + 全任务总览，2s 轮询刷新（`page.tsx:32`）。列表是低频总览场景，轮询够用且简单——1.4 节选型对照的"轮询侧"。
- **`web/app/tasks/[id]/page.tsx`——详情页（练习 8 前端主战场）。** 骨架提供首屏加载 + 2s 轮询 + 子任务渲染，两个 TODO：①（`page.tsx:25`）把轮询换成 `EventSource` 订阅 SSE——首屏仍先 `getTask` 一次（SSE 连接建立前页面不白屏），effect cleanup 里 `es.close()`（不关的话每进一次详情页泄漏一条长连接；React 18+ StrictMode 开发模式下 effect 会双跑，cleanup 写对就无害）；②（`page.tsx:64`）给 `waiting_human` 子任务渲染审批区——高亮、展示 prompt（审批人要看"它要干什么"）、批准 / 驳回按钮（点击后 disable 防重复提交）、审批人标识 `"dashboard-user"`（审计留名的前端落点；真实产品里应是登录态用户名）。
- **`web/components/status-badge.tsx`——状态徽章。** `waiting_human` 用醒目橙色（`status-badge.tsx:16`）——它是唯一"需要人做事"的状态，看板的第一职责就是让审批项一眼跳出来。

一条体验闭环值得做完练习后回味：点"批准"后**不需要手动刷新**——服务端 Decide 落盘 → 后台 Resume 续跑 → 状态迁移落盘 → SSE 下一拍推到看板。第 11 章的"事件驱动恢复"，在看板上变成了肉眼可见的流转。

---

## 三、进阶拓展（带代码）

### 3.1 最小 SSE 服务端 + EventSource 前端：协议的第三次相遇

SSE 协议在本教程里出现三次：第 3 章你在 mini-agent 里**手写客户端解析**（找 `data: ` 前缀、按 `\n\n` 切帧），第 7 章 Vercel AI SDK 替你消费，本章轮到你在**服务端生成**它。三个角色都演过一遍，这个协议应该能默写了。一个完整可运行的最小服务端：

```go
// 最小 SSE 服务端：每秒向浏览器推一帧计数。
// 教学要点全在注释里：响应头三件套、帧格式、Flush、断开检测。
package main

import (
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		// 1. 拿到 http.Flusher：SSE 的本质是"写完立刻冲刷"，
		//    不 Flush 数据就堆在响应缓冲区里，客户端迟迟收不到。
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		// 2. SSE 响应头三件套：协议类型、禁缓存、长连接。
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for n := 1; ; n++ {
			select {
			case <-r.Context().Done(): // 客户端断开（关页面/导航走）→ 退出，goroutine 不泄漏
				return
			case <-tick.C:
				// 3. 帧格式："data: " 前缀 + 单行数据 + 两个换行结尾 = 一帧。
				if _, err := fmt.Fprintf(w, "data: {\"tick\": %d}\n\n", n); err != nil {
					return // 写失败 = 客户端已经走了
				}
				flusher.Flush() // 每帧必 Flush，否则"实时"名存实亡
			}
		}
	})
	log.Fatal(http.ListenAndServe(":8090", nil))
}
```

前端更短——EventSource 把第 3 章手写的那些全包了：

```html
<script>
  // EventSource：浏览器原生 SSE 客户端，自动按 "data: ...\n\n" 分帧、断线自动重连。
  const es = new EventSource("http://localhost:8090/events");
  es.onmessage = (e) => console.log("收到帧:", JSON.parse(e.data));
  es.onerror = () => console.log("断线，浏览器会自动重连");
  // 页面关闭时浏览器自动断开；手动关闭：es.close()
</script>
```

没有浏览器也能验证：`curl -N http://localhost:8090/events` 会看到逐帧输出（`-N` 关掉 curl 自己的输出缓冲）。SSE 就是一条"永远不结束的 HTTP 响应"，curl 是它最好的调试工具——练习 8 验收 SSE handler 时用的就是这一招。

### 3.2 背压、断线与 Last-Event-ID 续传

**断开检测**在 3.1 的代码里已经有了：`r.Context().Done()` 与"写失败即返回"双保险。真正容易漏的是**背压**——客户端消费得慢（弱网、标签页挂后台）时，服务端的写会阻塞。本项目的 poll-diff 快照天然免疫：每拍最多一帧全量状态，这帧丢了下帧覆盖，慢客户端最多看到的状态旧一点。但如果你做的是**事件流**（每一帧都不可丢，如操作日志、通知），就必须给每个客户端配缓冲队列，并定好"跟不上怎么办"的策略：

```go
select {
case ch <- frame: // 客户端消费正常：入队
default: // 缓冲已满：丢帧保系统（或断开这个慢客户端）——策略必须显式选择
}
```

不处理背压的 fan-out 推送，一个慢客户端能把整个分发 goroutine 拖死。

**Last-Event-ID 续传**。事件流场景下"断线期间错过的帧怎么补"？SSE 协议内置了答案：帧里可以带一个 `id: ` 字段，浏览器 EventSource 重连时会自动带上 `Last-Event-ID` 请求头（值是它收到的最后一帧的 id），服务端据此补发。完整可运行的最小模式：

```go
// Last-Event-ID 断线续传：事件日志 + 重连补发（完整可运行）。
package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

// 事件日志：真实系统里是 ring buffer 或 DB 表，这里用固定 slice 演示。
var events = []string{
	`{"step": 1, "msg": "计划落盘"}`,
	`{"step": 2, "msg": "s1 完成"}`,
	`{"step": 3, "msg": "等待审批"}`,
	`{"step": 4, "msg": "审批通过，续跑"}`,
	`{"step": 5, "msg": "任务 done"}`,
}

func main() {
	http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// 重连时浏览器 EventSource 自动带 Last-Event-ID 头
		// （值 = 它收到的最后一帧的 id），从下一帧补发。
		from := 0
		if v := r.Header.Get("Last-Event-ID"); v != "" {
			from, _ = strconv.Atoi(v)
		}
		for i := from; i < len(events); i++ {
			select {
			case <-r.Context().Done(): // 客户端断开
				return
			case <-time.After(500 * time.Millisecond): // 模拟事件间隔
			}
			// 一帧两行：id: 是续传锚点，data: 是载荷，\n\n 结尾。
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", i+1, events[i]); err != nil {
				return
			}
			flusher.Flush()
		}
		// 流结束后保持连接（真实系统在这里继续推新事件，演示略）。
		<-r.Context().Done()
	})
	log.Fatal(http.ListenAndServe(":8091", nil))
}
```

验证方法：先 `curl -N --max-time 2 http://localhost:8091/events` 收到 id 1-2 的帧，再 `curl -N -H "Last-Event-ID: 2" ...` 重连，会看到服务端从 id 3 补发——这就是续传。本项目的 poll-diff **不需要**这个机制（快照是全量的，重连第一帧即最新状态，1.5 节的"断线自愈"），但面试讲 SSE 时 Last-Event-ID 是标准追问点，且事件流场景必须会。

### 3.3 生产化讨论：这个骨架离生产还差什么

骨架是学习项目，有几处刻意的简化。能讲清"差在哪、怎么补"，比假装它是生产级值钱得多。

**认证鉴权**。骨架 CORS 全开、审批人 `by` 是请求体里自报的字符串。最小修补是一个 middleware：

```go
// withAuth：最简 Bearer token 校验（演示形态，不是生产方案）。
func withAuth(next http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

一个 SSE 特有的坑：`EventSource` **不能自定义请求头**，token 只能走 Cookie 或 URL query 参数（query 会进访问日志，要脱敏）。生产方案是短寿命 ticket：先 POST 换一个一次性 ticket，再拿 ticket 开 EventSource。审批接口有了真鉴权，"审计留名"才有意义——否则 `by` 可以随便填。

**多实例部署的亲和性**。单机时 SSE 连接和任务状态都在一个进程里。水平扩到多实例后：① 任务状态在 SQLite/Postgres 里，任何实例都能服务任何查询——poll-diff 又一次显出好处（无状态读）；② 如果用的是事件 hook / 内存队列方案，"任务跑在实例 A、SSE 连接挂在实例 B"就推不到，需要 sticky session 或一层广播总线（Redis Pub/Sub、NATS）。**推送方案决定扩容成本**，这是选型时容易漏掉的维度。

**轮询 vs 推送的再权衡**。连接数上万后，"每连接每秒一次 DB 读"的 poll-diff 会成为 DB 压力：可以改成单实例内一个 poller + 内存广播（读一次推全体），或者干脆退回客户端轮询（配合 ETag/304 省带宽）。看板这类内部工具，几千连接以下 poll-diff 都是甜点区；C 端大规模推送才需要专门的推送层。没有一劳永逸的方案，只有和规模匹配的方案。

**优雅退出**。三个 `cmd/` 入口目前都是 `log.Fatal(http.ListenAndServe(...))` 裸奔——`log.Fatal` 内部调 `os.Exit`，**defer 一律不执行**；收到 SIGINT 更是直接终止，`tracer.Flush`、DB `Close` 全部落空。第 12 章坑 5 说"退出前必须 Flush"，落点就在这里：

```go
// serveWithGracefulShutdown：SIGINT/SIGTERM 到来时停止接新连接，
// 等在途请求（含 SSE 长连接）排空或超时，最后执行 flush 收尾。
func serveWithGracefulShutdown(addr string, handler http.Handler, flush func()) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// 不设 WriteTimeout——它会把 SSE 长连接整个掐死；
		// 只设 ReadHeaderTimeout 防慢速攻击（Slowloris）。
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		return err // 启动即失败（端口被占等）
	case <-ctx.Done():
	}
	log.Println("收到退出信号，开始优雅关闭……")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Shutdown 超时，强制关闭: %v", err)
		_ = srv.Close()
	}
	flush() // tracer Flush、DB Close——退出前的固定一步
	return nil
}
```

三个要点：`signal.NotifyContext` 把信号翻译成 ctx 取消，比手动 `signal.Notify` + channel 少管一个 goroutine；`Shutdown` 是"优雅"的核心——停止接新连接、等在途请求完成，SSE 长连接会一直等到超时上限，所以看板场景这个超时别设太长（`EventSource` 会自动重连，断开代价低）；`flush()` 放最后，所有资源的收尾统一挂进这个闭包。

**服务端超时与 SSE 的相互作用**。Q5 会讲代理侧超时，服务端自身还有配套的另一半：`http.Server` 的 `WriteTimeout` 是"整个响应的写 deadline"——设了它，SSE 长连接到点就被掐死；`ReadTimeout` 同理覆盖整个请求的读取。SSE 服务的正确配置是**只设 `ReadHeaderTimeout`**，长连接的存活交给应用层心跳（§1.5 的注释行心跳）+ 客户端自动重连，而不是 server 级写超时。

---

## 四、面试视角

> 以下每题给"标准回答 → 常见追问 → 加分点"。自测方法：不看回答口述，再对照差距。

**Q1：系统设计题——"设计一个多 agent 任务执行平台"。请先给个 5 分钟架构版。**

这是第 8-13 章的总装验收，也是第 14 章完整答题骨架的预演。按时间盒组织：

- **0:00-0:30 需求边界**：用户提交自然语言目标；系统分解为子任务并发执行；高风险操作需人工审批；任务跑几分钟到几小时，进程重启不丢进度；用户实时看到进度与成本。
- **0:30-1:10 总装**：四个角色——HTTP 接单层（无状态）、编排器（planner 分解 + worker 池并发 + critic 评审）、数据库（checkpoint 唯一真相源）、看板前端。HTTP 层只接单：校验、返 202，执行放后台。
- **1:10-2:10 状态机与 checkpoint**：任务状态机 `pending → planning → running → (waiting_human) → done/failed`，每次迁移落盘；子任务幂等键；崩溃恢复 = 重启读 checkpoint、跳过已完成子任务。
- **2:10-3:00 编排与并发**：选 planner-worker（子任务可并行）；worker 池 errgroup + semaphore 限流（限在 LLM 配额内）；context 超时预算分层；critic 叠加做质量，轮次 + token 双重熔断防烧钱。
- **3:00-3:40 HITL**：高风险子任务迁 `waiting_human` 落盘让出（不占 goroutine）；审批 API 把决定落盘；全部批完触发 Resume 续跑。
- **3:40-4:20 实时性与观测**：看板用 SSE（单向推送的天然场景）；实现选 poll-diff 快照 + 心跳（零侵入、跨进程正确、断线自愈）；trace 嵌套 span 对应 agent 层级，token 核算到子任务。
- **4:20-5:00 容错与收尾**：失败四件套（重试以幂等为前提、降级、死信）；LLM 输出过 schema 校验才进状态机；主动交代已知局限与演进方向（同步编排 → 事件驱动、SQLite → Postgres、poll-diff → 事件 hook）。

追问链：
- "量上来怎么扩？" → HTTP 层无状态可水平扩；编排执行拆独立 worker 进程，DB 换 Postgres，任务分发走队列；推送层加广播总线；
- "为什么不用消息队列做事件驱动？" → 当前规模同步编排简单可调试、链路显式；事件驱动是上量后的演进方向，而"状态全在 DB"的设计已经为切换留好了路；
- "多实例同时恢复同一个任务怎么办？" → 需要任务锁 / lease（DB 行锁或分布式锁），单机版没做——主动承认这是已知边界，比被问倒强。

加分点：开口先讲"能单 agent 就别多 agent"作为拆 agent 的前提（第 10 章）；给量化证据（离线测试数与 `-race` 结果、故障演练实测记录）；每个取舍都说得出"什么时候该换方案"。

**Q2：SSE 和 WebSocket 怎么选？**

标准回答：看方向与频率。服务端 → 客户端单向推送选 SSE——HTTP 之上的文本协议、浏览器 EventSource 原生支持（自动分帧、自动重连）、对代理 / 网关友好；双向高频交互（协作编辑、聊天室）选 WebSocket，代价是协议升级、心跳重连背压自己管、基础设施要显式放行；低频或环境受限就轮询兜底。本项目列表页轮询、详情页 SSE，就是按频率分层的实例。

追问链：
- "EventSource 有什么限制？" → 只能 GET、不能自定义 header（鉴权要走 Cookie / ticket）、HTTP/1.1 下每域名约 6 条连接上限（HTTP/2 多路复用可缓解）；
- "断线怎么办？" → EventSource 自动重连；要不要补发错过的事件取决于推送的是快照还是事件流——快照天然自愈，事件流用 `id: ` 字段 + `Last-Event-ID` 补发（进阶 3.2）。

加分点：主动提 SSE 的字节成本（文本协议，每帧只有 `data: ` 前缀开销，比 WebSocket 帧头略大但可忽略）；能讲"为什么不用 WebSocket 做看板"——杀鸡用牛刀，还给服务端引入连接态管理复杂度。

**Q3：为什么 HTTP 层要"接单即返"？同步等任务跑完会怎样？**

标准回答：两个生命周期错配——HTTP 请求的合理寿命是秒级（浏览器与网关超时），agent 任务跑几分钟到几小时。同步等待 = 浏览器超时、网关 504、连接数被占满，而且进程重启时进行中的请求全丢。所以写路径只"接单"（校验 + 生成 ID + 后台 goroutine + 202），进度靠读路径（GET / SSE）拿——前提是状态外置，任务状态每次迁移都落在 DB 里。

追问链：
- "任务失败怎么呈现给用户？" → 失败也是状态：状态机迁 failed 落盘，读路径自然呈现，错误信息进 checkpoint；HTTP 提交接口只可能返回"接单失败"（入参不合法），不存在"执行失败"的响应；
- "202 和 200 的区别重要吗？" → 语义上 202 明确"已受理、处理中"，客户端拿到 task_id 后该去查进度而不是等结果；长轮询 / webhook 回调是另两种通知模式，看板场景 SSE 体验最好。

**Q4：后台 goroutine 里 panic 了怎么办？**

标准回答：Go 里任何 goroutine 的未 recover panic 会终止整个进程——一个任务的 bug 会杀掉所有任务，必须分层防御：

1. **worker 级**：worker pool 对每个子任务的执行 `defer recover`，panic 转成该子任务 failed（和其他失败走同一条路径）。提醒：练习 1 的四条契约里没有 recover——这是值得你主动补上的第五条契约，生产级 pool 不能没有它；
2. **HTTP 触发级**：接单 handler 派生的后台 goroutine 加 recover 兜底，兜住就把任务置 failed 落盘 + 记日志：

```go
go func() {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("任务 %s 后台 goroutine panic: %v", id, p)
			// 把任务显式置 failed 落盘：否则它永远停在 running，成为僵尸任务
		}
	}()
	// ... 调 orchestrator.Run / Resume ...
}()
```

3. **进程级**：真的崩了，checkpoint 还在——重启后从断点续跑（第 9 章崩溃恢复是最后一张网）。

追问链：
- "recover 之后任务状态是什么？" → 必须显式置 failed，否则永远停在 running；
- "怎么发现已经在跑的僵尸任务？" → 两条：启动恢复流程把停在 running 的子任务迁回 pending 重排队（本项目已做）；巡检 `updated_at` 长时间不动的 running 任务（生产监控项）。

**Q5：SSE 在负载均衡 / 反向代理后面有什么坑？**

标准回答：四类。① **缓冲**：nginx 默认 `proxy_buffering on` 会把帧攒成批，要对 SSE 的 location 关掉（`proxy_buffering off`，或后端发 `X-Accel-Buffering: no` 响应头），否则"实时"变"每几十秒一批"；② **空闲超时**：代理默认 60s 左右掐无字节连接，心跳间隔必须小于链路最短空闲超时（本项目 15s）；③ **连接数**：SSE 是长连接，每个在线用户占一条——LB 与服务端的连接上限要按在线人数规划，浏览器侧 HTTP/1.1 还有每域名约 6 条的上限；④ **多实例亲和**：状态外置 + poll-diff 时任意实例都能服务任意连接（无需亲和）；事件 hook / 内存队列方案则需要 sticky session 或广播层。

加分点：给得出 nginx 具体配置点；主动补一句"这也是我选 poll-diff 的隐性理由之一"——把方案选型和运维成本连起来讲。

---

## 五、常见坑

1. **用请求 ctx 跑后台任务**（1.3 节）：响应一返回 `r.Context()` 就被取消，任务刚起步就被掐死。现象是任务永远停在半路、日志 `context canceled`，极易误判成编排器的 bug。铁律：触发型写路径 `context.Background()`，读路径 `r.Context()`。
2. **SSE 写完不 Flush**：帧堆在响应缓冲区，前端"偶尔实时、偶尔不实时"，因为缓冲攒满了才冲一次。每一帧写完必须 `Flush`——`http.Flusher` 断言失败（某些 middleware 包装了 ResponseWriter）要显式报错而不是静默退化。
3. **不写心跳被中间代理掐线**：本地直连一切正常，上了 nginx / 网关就"过一分钟准断"。心跳间隔要小于链路最短空闲超时；心跳用注释行（`: hb`），不要用 `data:` 帧——前者不触发前端 `onmessage`。
4. **推送无 diff 无节流**：每拍无脑推全量 JSON，带宽和前端重渲染双重浪费。poll-diff 的比较只需一次 `bytes.Equal`；反过来说 diff 本身也有成本（每秒序列化一次），本项目规模下远低于推送成本——量级变了要重新算账（3.3 节）。
5. **任务终态后主动关 SSE 连接**：EventSource 会自动重连，服务端关连接等于让浏览器按重连节奏反复刷同样的终态快照。终态保持连接、靠心跳挂着，页面关闭时 `r.Context()` 自然取消。
6. **demo 模式与真实模式行为分叉未标注**：demo 的固定计划、假 token 如果不标注，演示时自己会混淆、面试官会误解你的数字。骨架的做法值得抄：启动日志明示当前模式（`cmd/server/main.go:85`），demo 产出文本带 `[demo 产出]` 前缀（`main.go:63`）。

---

## 六、动手练习

### 练习 8：HTTP/SSE 引擎 + 实时看板

- **Go 侧实现区**：`stage-03-multi-agent/internal/server/server.go` 的五个 `TODO(练习8)`——`handleCreateTask`（`server.go:223`）、`handleListTasks`（`server.go:244`）、`handleGetTask`（`server.go:262`）、`handleApprove`（`server.go:290`）、`handleTaskEvents`（`server.go:339`）。每个 TODO 块里都有流程、提示与验收标准，动手前重读 2.4 节的设计决策。
- **前端实现区**：`stage-03-multi-agent/web/app/tasks/[id]/page.tsx` 的 `TODO(练习8)`①（EventSource 替换轮询，`page.tsx:25`）与 ②（审批交互，`page.tsx:64`）。
- **前置依赖（重要）**：server 的测试要跑真编排器、真 checkpoint、真审批——**先完成练习 1 / 2 / 3 / 5**，否则内层包还是桩，测试跑不起来。
- **验收**：

```bash
cd stage-03-multi-agent
go vet ./internal/server/ ./cmd/server/
go test ./internal/server/ -count=1

# demo 模式全链路（不设 API key，零成本）
env -u DEEPSEEK_API_KEY go run ./cmd/server &
curl -X POST localhost:8080/api/tasks -d '{"goal":"写一份数据治理周报"}'
curl localhost:8080/api/tasks/<task_id>        # 看状态流转到 waiting_human
curl -N localhost:8080/api/tasks/<task_id>/events   # 看 data: 帧逐条推出
curl -X POST localhost:8080/api/tasks/<task_id>/approve \
  -d '{"subtask_id":"s2","approved":true,"by":"me"}'

# 看板（本机 node 与 pnpm 的兼容问题，先加 PATH）
cd web && PATH=/opt/homebrew/opt/node/bin:$PATH npm install
PATH=/opt/homebrew/opt/node/bin:$PATH npm run build   # 类型与构建全绿
PATH=/opt/homebrew/opt/node/bin:$PATH npm run dev      # 打开 http://localhost:3000
```

- 参考答案：`docs/solutions/stage-03/exercise-8-server-dashboard.md`（**完成后再看**；含五个 handler、详情页实现、进阶 TokenChart 与验证记录）。

### 练习 9：架构文档 + 故障演练报告（文档型，无代码）

- **出题说明**：`stage-03-multi-agent/docs/README.md` 的 `TODO(练习9)`（大纲条目不可缺）。
- **产出**：在 `stage-03-multi-agent/docs/` 下新建 `architecture.md`（按"问题 → 架构 → 量化结果"口径，每个机制都要能指到你亲手实现的代码）与 `failure-drills.md`（五个演练，每个按"目的 / 步骤 / 预期 / 实际结果 / 结论"五段写）。
- **纪律**：步骤里的命令必须真实跑过；"实际结果"贴关键输出摘要，没跑过的路径如实标"未实测"并说明间接背书——诚实标注比漂亮数字值钱。量化不许编造：现阶段最硬的量化是"离线测试全绿 + `-race` 无竞争 + 演练实测记录"。
- 参考答案：`docs/solutions/stage-03/exercise-9-architecture-report.md`（完成后再看；含两份示范文档全文与评分对照清单）。

做完练习 8-9，阶段三的验收标准（`docs/stages/stage-03-multi-agent-production.md` 第七节）就全部可勾了——特别是"项目 3 可演示完整闭环"与"崩溃恢复可演示"这两条，正好用 demo 模式在面试官面前现场跑。

---

## 本章小结

- HTTP 层只"接单"：请求生命周期（秒）与任务生命周期（分钟-小时）错配，写路径接单即返 202，进度靠读路径；前提是状态外置（checkpoint 在 SQLite）。
- context 铁律：触发型写路径用 `context.Background()`，读路径用 `r.Context()`——用反了就是"任务刚起步就被掐死"。
- 推送选型按方向与频率：单向推送选 SSE（协议简单、EventSource 全包），双向高频选 WebSocket，低频兜底用轮询；本项目列表页轮询 + 详情页 SSE 是真实对照。
- poll-diff（1s 轮询 + 有变化才推 + 15s 注释行心跳）胜在零侵入、跨进程正确、断线自愈；事件 hook 是性能瓶颈出现后的优化方向。
- server 包零业务逻辑：协议翻译 + DTO 隔离 + 统一 `{"error": ...}` 出口；换任何前端形态内层不动。
- 练习 9 的两份文档不是作文，是把阶段三全部知识固化成可复述的面试资产——写得清才讲得清，跑得过的演练才叫容错。

下一章：[第 14 章：面试与求职作战手册](14-interview-and-career.md)——把三个项目和全部章节兑换成 offer：题库索引、系统设计答题骨架、简历写法、STAR 行为题。
