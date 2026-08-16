# 第 12 章：可观测性与 MCP——看见系统、连接生态

> 对应阶段：阶段三（深入）· 项目 3 `stage-03-multi-agent/`
> 代码位置：`stage-03-multi-agent/internal/trace/`（本章精讲，对应练习 6）、`stage-03-multi-agent/cmd/mcp-server/`（本章精讲，对应练习 7）
> 前置：第 1 章（token 与 usage）、第 2 章（工具三要素与 function calling 协议）、第 10 章（planner/worker 编排与成本熔断）
> 学完后你能讲清：trace 和 log 的本质区别、span 层级为什么必须对齐 agent 层级、token 成本怎么归因到每个子任务、agent 系统"结果评估 + 轨迹评估"两层体系、MCP 解决什么问题、MCP 与 function calling 的分层关系、手写一个 MCP stdio server 需要哪些零件。

---

## 本章地图

- 概念详解（上）：可观测性
  - trace vs log：离散事件点 vs 父子层级调用树
  - trace/span 层级 = agent 层级：下钻排查的顺序
  - token 成本归因：没有观测就没有成本控制
  - agent eval 两层：结果评估 vs 轨迹评估
  - Langfuse 简介：开源自托管的 LLM 观测平台
- 概念详解（下）：MCP
  - N 个框架 × M 个工具的碎片化问题；MCP = 工具生态的 USB-C
  - 架构三角：server / client / host；传输层 stdio vs HTTP+SSE
  - 三原语：tools / resources / prompts
  - 与 function calling 的分层关系（面试必考）
  - JSON-RPC 2.0：MCP 消息的格式骨架
- 代码精讲：`internal/trace` 的 Tracer 抽象、`cmd/mcp-server` 的协议循环
- 进阶拓展：最小自建 trace 收集器、MCP client 最小调用演示、trace 数据驱动的成本分析
- 面试视角 / 常见坑 / 动手练习（练习 6、7）

---

## 一、概念详解（上）：可观测性——看见系统

### 1.1 trace vs log：两种回答不同问题的数据

到这一阶段，你的系统已经不是"一个 agent 一条线性日志"能看懂的了：planner 一次调用、N 个 worker 并发跑、每个 worker 内部又是多轮 ReAct 循环。10 个 worker 的日志按时间交错打印在一屏上，一行都看不懂。

先建立两个概念的精确区分（阶段文档 3.2 对比表里的一行，这里展开讲）：

- **log 是离散事件点**。"worker-3 收到了输入 X"、"14:02:11 请求超时"——每条日志独立成立，彼此之间没有结构关系。它回答的问题是"**发生了什么**"。
- **trace 是有父子层级的调用树**。一次完整任务是一棵 span 树：每个 span 是一次操作，带开始/结束时间和父子关系。它回答的问题是"**哪个环节慢、哪个环节贵**"。

两个排查场景体会差异：

- "worker-3 为什么花了 80% 的 token？"——按调用层级聚合的查询，靠 trace。在线性 log 里挖这个结论要翻几百行。
- "worker-3 当时收到的输入到底是什么？"——某个时刻的现场还原，靠 log（或 span 上挂的 metadata）。

**两者互补，不是替代关系。** 生产系统的做法是 trace 为主干、log 挂进 span 的 attributes 里，下钻到出问题的 span 后再看当时的事件流。

### 1.2 trace/span 层级 = agent 层级

这套概念来自 OpenTelemetry（最小化版本）：

- **Trace**：一次完整任务的全程，对应我们的任务 ID，是一棵 span 树的容器；
- **Span**：树上一个节点 = 一次操作（planner 分解、某个 worker 执行、单次 LLM 请求、单次工具调用），带开始/结束时间、属性（token 数、模型名）、父子关系。

设计要点只有一条，但极其重要：**span 的父子层级必须与 agent 系统的结构层级一一对应**：

```
trace: 任务"调研两个竞品"（总耗时 45s，4.9k tokens）
├── span: planner.plan（5s · 2k tokens）
├── span: worker-1 调研竞品A（19s）
│   ├── generation: llm.chat deepseek-chat（13s · in=1200 out=300）
│   └── span: tool.http_fetch（6s）
├── span: worker-2 调研竞品B（21s）
│   └── generation: llm.chat deepseek-chat（21s · in=900）❌ 超时
└── span: planner.summarize（4s · 2.5k tokens）
```

为什么要对齐？因为 **trace 的价值全在"下钻"**：任务超时了 → 看哪个 worker span 最长 → 再下钻到它内部哪次 LLM 调用慢。排查路径与系统结构同构，你脑子里"任务 → 子任务 → 单次调用"的模型可以直接映射到树上。反过来，如果层级乱挂（比如所有 LLM 调用平铺在 trace 根下），"哪个子任务最贵"就永远回答不了——trace 退化成按时间排序的 log，白埋了。

上面这棵样例树一眼就能读出三个结论：失败根因是 worker-2 的 LLM 调用超时；瓶颈在 worker 阶段而非规划/汇总；总成本集中在两次 LLM 调用上。

### 1.3 token 成本归因：没有观测就没有成本控制

第 1 章说过"usage 是唯一真实的成本数据源"；第 10 章讲了模型分级、砍 prompt、预算熔断。这两章之间缺的那块拼图就是本章：**成本数据必须先被结构化地采集下来，优化才有依据**。

做法是：**每个 span（至少每个 LLM 调用 span）记录 input/output token**。之后所有成本问题都变成对 span 数据的聚合查询：

- "这个任务花了多少钱？" → 对该 trace 下所有 generation 的 token × 单价求和；
- "哪个 worker 最贵？" → 按 worker span 分组聚合；
- "模型分级省了多少？" → 对比分级前后同类 span 的平均成本。

没有这层归因，"成本控制"就是拍脑袋：你知道总账单涨了，但不知道是该砍 planner 的 prompt、还是该给 worker-3 换便宜模型。**观测先行，优化后置**——这是数据驱动工程在 agent 系统里的具体形态。

### 1.4 agent eval 两层：结果对不对 vs 过程对不对

第 6 章学过 RAG 的评估（recall@k、LLM-as-judge），那一套是"结果评估"。多 agent 系统必须把评估拆成两层（阶段文档 3.1 Q10）：

- **结果评估**：最终产出对不对。有标准答案的用精确匹配/覆盖率；开放任务用 LLM-as-judge（注意其偏差，第 6 章已讲）。
- **轨迹评估**：过程对不对。planner 分解合理吗？worker 工具选对没有？步数有没有异常（绕路）？critic 拦截了几次？

两层必须分开看，因为四种组合的含义完全不同：

| 结果 | 轨迹 | 诊断 |
| --- | --- | --- |
| 好 | 好 | 正常 |
| 好 | 差 | **蒙的**——下次同样任务未必对，轨迹里的问题要修 |
| 差 | 好 | **单点故障**——过程合理只是某一步运气差/超时，好修 |
| 差 | 差 | 系统性问题，从分解逻辑查起 |

常用指标示例：任务成功率、平均子任务数、平均 token 成本、审批介入率（HITL 那章的暂停次数）。

**轨迹评估的数据从哪来？就是 trace。** 分解了几个子任务、每个 worker 调了什么工具、各自几步——span 树上全有。所以业界的标准闭环是：trace 落数据 → 导出成 eval 语料 → 离线回放分析 → 改进后再看 trace 对比。可观测性不只是"排障工具"，它是评估体系的数据底座。

### 1.5 Langfuse：开源自托管的 LLM 观测平台

落地工具本项目选 **Langfuse**（阶段文档与预习指南的口径）：

- **开源、可自托管**：`docker compose` 一键起（Postgres + ClickHouse + Web），数据不出本机，学习场景推荐；也有云免费层。
- **核心功能**就是 1.2 那棵树的采集、可视化与成本统计，外加 prompt 管理和 eval 数据集（正好接上 1.4 的闭环）。
- **接入方式**：暴露一个公开 ingestion API（`POST {host}/api/public/ingestion`，Basic Auth 用 publicKey:secretKey），客户端把 trace/span/generation 事件批量 POST 上去；也可以通过 OpenTelemetry 导出。练习 6 走前一条路——手写 HTTP 上报，把协议彻底看清楚。

架构上有一个关键决策，也是本章代码精讲的主线：**编排器不直接依赖 Langfuse，而是依赖一个自定义的 `Tracer` 接口**。本地开发用空实现，接 Langfuse 只是换一个实现——埋点与后端解耦。

## 一、概念详解（下）：MCP——连接生态

### 1.6 MCP 解决的问题：N × M 的碎片化

第 2 章你亲手设计了工具层：Name/Description/Schema 三要素 + Execute 执行。这套方式有一个隐含前提——**工具是编译进 agent 的**。改一个工具要改代码、重新部署；换一个 agent 框架（LangChain、Claude、某个 IDE），所有工具要按它的规范重写一遍。

推广到整个行业：N 个 agent 框架 × M 个工具 = **N×M 种接法**。每个工具提供方（Postgres、GitHub、文件系统……）要么不接入 AI，要么为每个框架写一套适配。

**MCP（Model Context Protocol，Anthropic 2024 年底推出）就是这个问题的答案**：定义一套标准协议，工具方实现一次 MCP server，任何 MCP client（Claude Desktop、IDE、你自研的 agent）都能直接用——类比 USB-C 统一充电口。官方自称"AI 应用的 USB-C"。

要认清本质：**MCP 不是新能力，是标准化**。你的 agent 通过 MCP 拿到的工具，最终还是以 function calling schema 的形式进模型（第 2 章的知识完全适用）。它替代的是"每个工具手写适配代码"，不是"模型调用工具的方式"。

### 1.7 架构三角与传输层

MCP 世界里有三个角色：

- **server**：暴露能力的一方。轻量进程，对外提供工具/数据（本章练习就是写一个）。
- **client**：消费能力的一方，通常内嵌在 agent 里，与每个 server 保持一对一连接。
- **host**：承载 client 的应用本体——Claude Desktop、IDE，或者我们的编排器。host 决定用哪些 server，client 负责协议通信。

传输层两种常用形态：

| 传输 | 模型 | 优点 | 代价 |
| --- | --- | --- | --- |
| **stdio** | host 把 server 作为子进程拉起，标准输入输出传 JSON-RPC | 零网络配置、免鉴权（本机管道）、生命周期跟随 client 免部署 | 只能本地用，无法远程共享 |
| **HTTP+SSE / Streamable HTTP** | server 独立部署，远程多客户端共享 | 可集中管理、团队共享 | 要处理鉴权、网络故障、部署运维 |

本地工具首选 stdio（Claude Desktop 的 server 全是这种）；平台团队给全公司提供工具用 HTTP。本章练习做 stdio 版——协议看得最清楚。

### 1.8 三原语：tools / resources / prompts

MCP server 能暴露的能力分三类：

| 原语 | 谁决定何时用 | 类比 |
| --- | --- | --- |
| **tools** | 模型决定何时调 | function calling 的工具，最常见 |
| **resources** | 应用/用户决定何时读 | 只读数据源（文件、DB 记录），类似 GET |
| **prompts** | 用户从菜单触发 | 预置 prompt 模板（"总结这份日志"） |

练习 7 只实现 tools——它与"把 mini-agent 工具暴露出去"的目标直接相关。server 在握手时用 **capability 声明**自己支持哪些原语，不声明的合规 client 就不会来问——这就是能力协商的意义。

### 1.9 与 function calling 的分层关系（面试必考）

这是本章最容易被问、也最容易答混的一题。一句话版本：

> **function calling 是"模型 ↔ 应用"之间的调用机制；MCP 是"应用 ↔ 工具提供方"之间的接入协议。两者是堆叠关系，不是替代关系。**

典型链路把两者串起来：

```
┌─ MCP 层（应用 ↔ 工具提供方） ──────────────┐
│  client 连上 server → tools/list 拿到工具清单  │
└──────────────┬───────────────────────────────┘
               ▼ 转成 function calling schema
┌─ function calling 层（模型 ↔ 应用） ────────┐
│  tools 数组随请求发给模型 → 模型输出 tool_calls │
└──────────────┬───────────────────────────────┘
               ▼ client 把调用转发给 server
┌─ MCP 层 ────────────────────────────────────┐
│  tools/call → server 执行 → 结果回到对话历史   │
└──────────────────────────────────────────────┘
```

关键证据：**MCP tools/list 返回的 `{name, description, inputSchema}` 与 function calling 的 tool schema 几乎同构**——这不是巧合，正是为了让"运行时发现工具 → 喂给模型"这一步转换零成本（包一层 `{type:"function", function:{...}}` 即可，进阶 3.2 有完整代码）。

这个设计带来的架构变化：第 2 章的工具是"编译进 agent 的"，MCP 把工具变成**运行时发现**的——agent 启动时连上 server 列出工具，模型调工具时 agent 转发给 server 执行，**ReAct 循环本身一行不用改**（第 3 章的循环只认 tool schema 和 tool_calls，不关心工具住在本进程还是隔壁进程）。

### 1.10 JSON-RPC 2.0：MCP 消息的格式骨架

MCP 的消息层直接复用 JSON-RPC 2.0 规范，三种消息：

```json
// Request（有 id，必须回复）
{"jsonrpc":"2.0","id":1,"method":"tools/list"}

// Notification（无 id，绝不回复）
{"jsonrpc":"2.0","method":"notifications/initialized"}

// Response（回显 id，result 与 error 二选一）
{"jsonrpc":"2.0","id":1,"result":{"tools":[...]}}
{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}
```

记住四个要点，代码精讲会逐一看到对应物：

1. `id` 可以是数字或字符串，回显时必须**原样奉还**；
2. 通知没有 `id`，收到通知**绝不回复**；
3. 规范错误码：`-32700` parse error、`-32601` method not found、`-32602` invalid params；
4. stdio 传输下是 **newline-delimited JSON**：一行一条消息，无 HTTP 框架。

---

## 二、代码精讲

### 2.1 trace 包：一个接口隔开编排器与观测后端

`stage-03-multi-agent/internal/trace/trace.go` 全文只有 113 行，其中一半是注释——这些注释本身就是本章上半部分的浓缩，建议对照源码读。

**包注释即设计文档**（`trace.go:1-21`）：开篇就写清三件事——span 层级与 agent 层级的对应关系、每个 span 记 token 是为了成本归因、为什么设计成接口（编排器只依赖 `Tracer` 接口，本地用 Noop，接 Langfuse 只是换实现，编排器一行不改）。这是"面向接口编程"在基础设施层的标准用法，和 mini-agent 里 Tool 接口是同一个思想（第 2 章）。

**Tracer 接口**（`trace.go:37`）三个方法就是 trace 生命周期的全部：

```go
type Tracer interface {
	StartSpan(ctx context.Context, parentSpanID, name string, metadata map[string]any) (spanID string)
	EndSpan(ctx context.Context, spanID string, inputTokens, outputTokens int, err error)
	Flush(ctx context.Context) error
}
```

对照 1.2 的设计看接口形状：

- `StartSpan` 的 `parentSpanID` 为空串表示根 span（任务级）——**层级关系全靠这一个参数表达**；
- `EndSpan` 携带 `inputTokens/outputTokens`——token 记录点就钉在 span 结束时（1.3 的归因数据源）；`err` 让失败的 span 能被标记出来（样例树里的 ❌）；
- `Flush` 留给"缓冲批量上报"的实现：进程退出前确保事件都发出去了；
- 接口上方的使用约定注释（`trace.go:27-36`）直接给出编排器侧的埋点顺序：root → sub → llm-call，逐层 EndSpan，最后 Flush——这就是练习 6 的调用方视角。

**Noop 实现**（`trace.go:49-61`）：三个空方法。它的存在让"不接观测后端"成为一个显式选择，而不是 `if tracer != nil` 散落编排器各处。这是空对象模式（Null Object Pattern），写库代码时的常备手法。

**编译期断言**（`trace.go:65`）：

```go
var _ Tracer = (*Noop)(nil)
```

`Tracer` 接口加方法时这里立刻编译报错，比运行时才发现"某个实现漏了方法"早得多。你的 Langfuse 实现也应照写一行。

**TODO(练习6)**（`trace.go:69-113`）要你实现 `NewLangfuse(host, publicKey, secretKey)` 并让 `*Langfuse` 满足 `Tracer`。骨架的 TODO 块把任务、提示、验收都写清了，这里只点三个设计要害（实现留给你）：

1. **层级与 trace 归属的维护**：StartSpan 时 parent 为空 → 新建 traceID 并发 trace-create 事件；非空 → 继承父 span 的 traceID，嵌套关系用事件 body 里的 `parentObservationId` 表达。所以实现内部需要一个 `map[spanID] → (traceID, kind)` 的登记簿，EndSpan 后删除。
2. **span 与 generation 的区分**：Langfuse 里 generation 是带 model/usage 的特殊 observation，成本核算只认它。骨架定的约定是 StartSpan 的 metadata 里带 `"model"` 即视为 LLM 调用——不为此改接口（接口是编排器与后端之间的契约）。
3. **观测不能影响主流程**：StartSpan/EndSpan 绝不 panic、绝不阻塞、不返回 error；失败只允许从 Flush 返回。这是可观测系统的第一原则，参考答案的设计点部分有更完整讨论。

验收方式是 httptest 假服务器（TODO 块给了具体断言清单），不需要真的起 Langfuse。

### 2.2 mcp-server 包：stdio 上的 JSON-RPC 循环

`stage-03-multi-agent/cmd/mcp-server/main.go` 是练习 7 的骨架，协议类型与读写循环已就绪，要补的是 method dispatch。

**文件头注释**（`main.go:1-39`）先把本章下半部分的概念全写进去了：MCP 解决什么问题、架构三角、三原语、与 function calling 的分层、为什么选 stdio——以及那个最著名的坑：**stdout 是协议信道，任何日志必须走 stderr**。

**JSON-RPC 线缆类型**（`main.go:57-89`）是 1.10 的 Go 化身，两个字段细节值得停下：

```go
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}
```

- `ID` 用 `json.RawMessage` 原样保存：JSON-RPC 的 id 可以是数字或字符串，回显时必须原样奉还；定义成 `int` 会把字符串 id 的请求解析失败。同时 `len(ID)==0` 天然就是"通知"的判定条件（`main.go:60` 注释）。
- `Params` 延迟解析：不同 method 的 params 结构不同，先 RawMessage 收下，dispatch 后各 handler 再 unmarshal——和 mini-agent 里 `ToolCall.Arguments` 是 string 同理（第 1 章 2.1 节）：**线缆格式和解析时机是两件事**。

**main 的组装与读取循环**（`main.go:96-144`）三段：

- 注册表组装（`main.go:107-111`）：直接复用 `mini-agent/api` 门面包，`Calculator`、`HTTPFetch`、`NewReadFile(workspace)`、`NewWriteFile(workspace)` 四个工具——server 的职责是把已有工具"翻译"成 MCP，不是重写工具。
- 行读取循环（`main.go:119-139`）：`bufio.Scanner` 逐行读 stdin，一行一条消息。`main.go:118` 放宽了 scanner 默认 64KB 的行上限（write_file 的 content 参数可能很大）——这是真实踩坑点，不改会在大参数时静默截断报错。
- 解析失败分支（`main.go:125-133`）：连 JSON 都解析不了时按规范回 `-32700` 且 `id:null`（id 都解析不出来，只能回 null）。

**输出侧两个 helper**（`main.go:147-169`）：`logf` 写 stderr（服务端日志的唯一合法出口）；`writeResponse` 把响应写进**启动时保存的真 stdout** `protocolOut`（`main.go:94`）并立即 flush——client 可能阻塞等回复，忘了 flush 就是双端死等。

**TODO(练习7)**（`main.go:183-231`）要你实现 `dispatch(reg, req)` 与四个 method handler。骨架里 `dispatch` 目前是 stub（`main.go:232`）：一切方法回 `-32601`，保证骨架可编译可跑通循环。协议形状上你需要实现的三个方法是：

| method | 性质 | result 形状 |
| --- | --- | --- |
| `initialize` | 握手 | 三件套：`protocolVersion` / `capabilities.tools` / `serverInfo{name,version}` |
| `notifications/initialized` | 纯通知 | 不回复（返回 nil） |
| `tools/list` | 工具发现 | `result.tools` = `[{name, description, inputSchema}]` |
| `tools/call` | 工具执行 | `result.content` = `[{type:"text", text:...}]` |

三个最容易做错的点（TODO 块提示的浓缩）：

1. **工具业务错误不走 RPC error**：算式非法、路径越界这类失败要包成正常 result 加 `"isError": true`。RPC error 是"协议层失败"（方法不存在、参数畸形），工具失败是业务结果——client 会把它喂回模型让模型自我纠正，与 mini-agent 里"工具错误喂回模型"同一思想。塞错了地方，模型的自我纠正回路就断了。
2. **`tools/list` 的排序**：`Registry.Schemas()` 内部遍历 map，顺序随机，`sort.Slice` 按名字排一下输出才稳定可比对。
3. **stdout 污染**：`Registry.Call` 和 `Calculator.Execute` 里留着阶段一的 `fmt.Println` 调试输出，tools/call 直接调会污染协议信道——TODO 块给了处理思路（调用期间把 `os.Stdout` 临时换走），这就是 `protocolOut` 变量（`main.go:94`）存在的原因。

最后回头看 **schema 映射**这一件事，它把第 2 章和本章缝在一起：第 2 章说工具说明书三要素是 Name/Description/Schema；`tools/list` 输出的 `{name, description, inputSchema}` 正好是这三要素的 MCP 形状，其中 `inputSchema` 就是工具的 `ParametersSchema()`——**同一份 JSON Schema，既服务 function calling 又服务 MCP**，一份说明书两处用，这就是标准化的红利。

---

## 三、进阶拓展（带代码）

### 3.1 最小自建 trace 收集器：理解 Langfuse 到底帮你做了什么

**为什么自己写一个**：`Tracer` 接口只有三个方法，"上报到 Langfuse"听起来抽象。其实观测后端的核心机制——span 登记簿、父子关系、结束时的数据落位——不到一百行就能手写一个可运行的内存版。写完你会理解：Langfuse 做的事情本质相同，只是把"存 map"换成"发 HTTP 事件"，再附上可视化与聚合查询。下面代码已在临时 module 实测通过（`go vet` + `go run`）：

```go
package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Span 是调用树上的一个节点：一次操作（planner 分解、某个 worker、单次 LLM 调用）。
type Span struct {
	ID        string
	ParentID  string // 空串 = 根 span
	Name      string
	Start     time.Time
	End       time.Time
	InTokens  int
	OutTokens int
	Err       string
}

// Collector 是最小可用的 trace 收集器：span 存内存 map，任务结束后打印层级树。
// 真实后端（Langfuse）做的事本质相同，只是"存 map"换成"发 HTTP 事件"。
type Collector struct {
	mu    sync.Mutex
	spans map[string]*Span
	seq   int
}

func NewCollector() *Collector { return &Collector{spans: map[string]*Span{}} }

// Start 开启一个 span；parentID 为空串表示根 span（任务级）。返回 spanID。
func (c *Collector) Start(parentID, name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	id := fmt.Sprintf("span-%d", c.seq)
	c.spans[id] = &Span{ID: id, ParentID: parentID, Name: name, Start: time.Now()}
	return id
}

// End 结束 span，记下结束时间、token 与错误。真实实现里这里产生一条上报事件。
func (c *Collector) End(id string, inTokens, outTokens int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.spans[id]
	if !ok {
		return // 对未知 id 静默忽略：观测永远不能搞挂主流程
	}
	s.End = time.Now()
	s.InTokens, s.OutTokens = inTokens, outTokens
	if err != nil {
		s.Err = err.Error()
	}
}

// Print 把整棵 span 树按层级打印：哪层慢、哪层贵、哪层挂了，一眼可见。
func (c *Collector) Print() {
	c.mu.Lock()
	defer c.mu.Unlock()
	kids := map[string][]*Span{} // parentID -> 子 span 列表
	for _, s := range c.spans {
		kids[s.ParentID] = append(kids[s.ParentID], s)
	}
	for _, list := range kids { // 同层按开始时间排序，输出稳定
		sort.Slice(list, func(i, j int) bool { return list[i].Start.Before(list[j].Start) })
	}
	var walk func(parentID string, depth int)
	walk = func(parentID string, depth int) {
		for _, s := range kids[parentID] {
			status := ""
			if s.Err != "" {
				status = "  ❌ " + s.Err
			}
			fmt.Printf("%s%-26s %6.3fs  in=%-5d out=%-5d%s\n",
				strings.Repeat("  ", depth), s.Name,
				s.End.Sub(s.Start).Seconds(), s.InTokens, s.OutTokens, status)
			walk(s.ID, depth+1)
		}
	}
	walk("", 0)
}

func main() {
	// 模拟一次 planner-worker 任务的埋点：任务 → worker → LLM 调用/工具执行
	c := NewCollector()
	root := c.Start("", "task: 调研两个竞品")
	w1 := c.Start(root, "worker-1 调研竞品A")
	llm1 := c.Start(w1, "llm.chat deepseek-chat")
	time.Sleep(12 * time.Millisecond)
	c.End(llm1, 1200, 300, nil)
	tool := c.Start(w1, "tool.http_fetch")
	time.Sleep(6 * time.Millisecond)
	c.End(tool, 0, 0, nil)
	c.End(w1, 0, 0, nil)
	w2 := c.Start(root, "worker-2 调研竞品B")
	llm2 := c.Start(w2, "llm.chat deepseek-chat")
	time.Sleep(20 * time.Millisecond)
	c.End(llm2, 900, 0, fmt.Errorf("context deadline exceeded"))
	c.End(w2, 0, 0, nil)
	sum := c.Start(root, "planner.summarize")
	time.Sleep(4 * time.Millisecond)
	c.End(sum, 2100, 400, nil)
	c.End(root, 0, 0, nil)
	c.Print()
}
```

实测输出（缩进即层级，worker-2 的超时一眼可见）：

```
task: 调研两个竞品                0.045s  in=0     out=0
  worker-1 调研竞品A              0.019s  in=0     out=0
    llm.chat deepseek-chat      0.013s  in=1200  out=300
    tool.http_fetch             0.006s  in=0     out=0
  worker-2 调研竞品B              0.021s  in=0     out=0
    llm.chat deepseek-chat      0.021s  in=900   out=0      ❌ context deadline exceeded
  planner.summarize           0.004s  in=2100  out=400
```

对照 `Tracer` 接口看：`Start`/`End` 就是 `StartSpan`/`EndSpan` 的简化版（少了 ctx 和 metadata），`Print` 对应 Langfuse 的 Web 可视化。**取舍与生产注意**：内存版没有上报开销但也无法跨进程共享、进程退出数据即消失；真实后端要解决的是缓冲批量 vs 同步上报的取舍（同步拖慢主流程、缓冲崩溃丢数据）、队列上限与丢弃策略——这些正是练习 6 参考答案"进阶实现"讨论的内容，做完练习去对照。

### 3.2 MCP client 侧最小调用演示：tools/list → function calling schema 的映射

练习 7 做的是链路的右半边（server），这一节补左半边（client）的心智：client 拿到 server 的工具清单后做什么。下面代码自洽可运行（已实测），其中 `mcpTool`/`toolSchema` 是协议形状的本地镜像——在项目里后者就是第 2 章的 `llm.Tool`：

```go
package main

import (
	"encoding/json"
	"fmt"
)

// ---- MCP 侧形状：server 的 tools/list 返回的工具描述 ----

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ---- function calling 侧形状（对应第 2 章 llm.Tool 的线缆格式） ----

type toolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type toolSchema struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

// mcpToFunctionSchema 是"MCP 工具 → function calling schema"的全部转换工作：
// 包一层 {type:"function", function:{...}}，三个字段几乎一一对应——
// 这不是巧合，正是为了让"运行时发现工具 → 喂给模型"这一步零成本。
func mcpToFunctionSchema(t mcpTool) toolSchema {
	return toolSchema{
		Type: "function",
		Function: toolFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		},
	}
}

func main() {
	// 第 1 步：client 向 server 发 tools/list（stdio 传输下 = 往子进程 stdin 写一行）。
	listReq := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}
	line, _ := json.Marshal(listReq)
	fmt.Println(">>> " + string(line))

	// 第 2 步：解析 server 的响应（此处用字面量模拟，形状与练习 7 的输出一致）。
	respLine := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"calculator","description":"计算数学表达式","inputSchema":{"type":"object","properties":{"expression":{"type":"string","description":"要计算的表达式"}},"required":["expression"]}}]}}`
	var resp struct {
		Result struct {
			Tools []mcpTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		panic(err)
	}

	// 第 3 步：转成 function calling schema 喂模型——转换之后，ReAct 循环原样可用。
	schemas := make([]toolSchema, 0, len(resp.Result.Tools))
	for _, t := range resp.Result.Tools {
		schemas = append(schemas, mcpToFunctionSchema(t))
	}
	out, _ := json.MarshalIndent(schemas, "", "  ")
	fmt.Println(string(out))

	// 第 4 步：模型选中工具后，client 把 tool_call 桥接回 MCP 调用。
	// function calling 的 arguments 是 JSON 字符串，MCP 的 arguments 是 object，
	// 桥接 = Unmarshal 成 object 再放进 tools/call 的 params。
	var args map[string]any
	if err := json.Unmarshal([]byte(`{"expression":"1+2*3"}`), &args); err != nil {
		panic(err)
	}
	callReq := map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "calculator", "arguments": args},
	}
	line2, _ := json.Marshal(callReq)
	fmt.Println(">>> " + string(line2))
}
```

注意第 4 步那个方向相反的桥接：**function calling 的 `arguments` 是 JSON 字符串，MCP 的 `arguments` 是 object**。client 侧 Unmarshal、server 侧（练习 7 里）re-marshal 回字符串喂给 `Registry.Call`——协议边界上的"序列化格式翻译"是集成代码里最常出 bug 的地方，见到先想"这一层是 string 还是 object"。

**生产注意**：真实 client 还要管子进程生命周期（拉起/崩溃重启）、并发请求与响应的 id 配对（本演示是同步一问一答）、server 版本与 capability 协商；工具清单应缓存，server 支持时监听 `notifications/tools/list_changed` 再刷新。

### 3.3 trace 数据驱动的成本分析：从 span 到"哪个子任务最贵"

有了 1.3 的埋点（每 span 记 token），成本分析就是把 span 表当事实表做聚合。Langfuse 的 Web UI 替你做了，但查询逻辑本身值得会写——面试里"成本怎么归因"的最佳回答就是当场写出这类查询。

设 span 落表为 `spans(trace_id, span_id, parent_id, name, kind, model, input_tokens, output_tokens, cost_usd, started_at, ended_at)`：

```sql
-- ① 一个任务的总成本（generation 是 LLM 调用，成本只记在这类 span 上）
SELECT trace_id, SUM(cost_usd) AS total_cost
FROM spans
WHERE kind = 'generation'
GROUP BY trace_id;

-- ② 哪个 worker 最贵：按"任务的第二层 span"（worker span）分组，
--    把它子树下所有 generation 的成本归到它头上
SELECT w.name AS worker,
       SUM(g.cost_usd)              AS worker_cost,
       SUM(g.input_tokens)          AS worker_input_tokens
FROM spans w
JOIN spans g ON g.trace_id = w.trace_id
            AND g.parent_chain LIKE w.span_id || '%'  -- 伪代码：实际用递归 CTE 或路径列
WHERE w.parent_id = (SELECT span_id FROM spans r WHERE r.parent_id IS NULL)
GROUP BY w.name
ORDER BY worker_cost DESC;

-- ③ 模型分级效果：同角色（planner/worker/critic）不同模型的平均成本对比
SELECT name, model, AVG(cost_usd) AS avg_cost, COUNT(*) AS calls
FROM spans
WHERE kind = 'generation'
GROUP BY name, model;
```

查询 ② 揭示了一个实现要点：**"按子树聚合"要求层级信息可查询**——要么落表时冗余一个路径列（`trace/span1/span2`），要么用递归 CTE 现场展开。这就是为什么 1.2 强调"层级必须对齐 agent 层级"：层级乱了，② 这种查询根本写不出来。

得到数据后的典型动作（呼应第 10 章）：worker-3 成本占 60% → 给它单独换便宜模型或砍它的 prompt；planner 成本占比过高 → 简化计划 prompt 或减少 replan 次数。**观测 → 归因 → 优化 → 再观测验证**，闭环转起来才算会了成本控制。

---

## 四、面试视角

> 以下每题给"标准回答 → 常见追问 → 加分点"。自测方法：不看回答口述，再对照差距。

**Q1：MCP 解决什么问题？和 function calling 什么关系？**

标准回答：MCP 解决工具接入的标准化——没有它时 N 个 agent 框架 × M 个工具 = N×M 种接法；有了它工具方写一个 server，所有 client 通用，类似 USB-C 统一充电口。三原语：tools（可执行函数）、resources（可读数据）、prompts（模板）。和 function calling 是**不同层**：function calling 是"模型 ↔ 应用"之间模型表达调用意图的机制；MCP 是"应用 ↔ 工具提供方"之间的接入协议。典型链路：client 经 tools/list 拿到工具 → 转成 function calling schema 喂模型 → 模型输出 tool_calls → client 经 tools/call 调 server 执行。

追问链：

- "MCP 是不是替代了 function calling？" → 不是，是堆叠：MCP 之上模型依然用 function calling 表达"我要调这个工具"。证据：tools/list 的 `{name, description, inputSchema}` 与 function calling schema 几乎同构，就是为零成本转换设计的。
- "接入 MCP 后 ReAct 循环要改吗？" → 不用。循环只认 tool schema 和 tool_calls，工具住在本进程还是 MCP server 后面它不关心——变化的只是"工具发现从编译期变成运行时"。

加分点：主动点破"MCP 不是新能力是标准化"，并说出三原语各自"谁决定何时用"（tools 模型决定、resources 应用决定、prompts 用户触发）。

**Q2：agent 系统怎么做评估？**

标准回答：分两层。**结果评估**：最终产出对不对——有标准答案的精确匹配，开放任务 LLM-as-judge（注意其偏差）。**轨迹评估**：过程对不对——planner 分解合理吗、工具选对没有、步数异常吗、critic 拦了几次。两层分开看：结果好轨迹差是蒙的（不可持续），结果差轨迹好是单点故障（好修）。指标：任务成功率、平均子任务数、平均 token 成本、审批介入率。

追问："轨迹评估的数据从哪来？" → trace 落数据，离线回放分析；trace 就是现成的 eval 语料。能说出"观测 → 数据集 → 回归评估"这个闭环是加分点。

**Q3：trace 和 log 的区别？**

标准回答：log 是离散事件点，回答"发生了什么"；trace 是有父子层级的调用树（span 嵌套，各有起止时间），回答"哪个环节慢/贵"。排查"worker-3 为什么花了 80% 的 token"靠 trace；排查"worker-3 当时收到什么输入"靠 log。互补不替代，生产上 log 挂进 span 的 attributes。

加分点：补一句"span 层级必须对齐系统结构层级，否则按子树聚合的查询写不出来，trace 退化成按时间排序的 log"。

**Q4：手写一个 MCP server 要实现哪些部分？**

标准回答（按 stdio 版）：① JSON-RPC 2.0 消息类型（Request/Response/Notification，id 用 RawMessage 原样回显）；② stdin 行读取循环（newline-delimited JSON，注意放宽行缓冲上限）；③ 三个核心 method——`initialize`（握手三件套：protocolVersion、capabilities、serverInfo）、`tools/list`（工具清单，name/description/inputSchema）、`tools/call`（执行并包成 content 数组）；④ 通知不回复；⑤ 错误分层——协议层失败用 RPC error（-32601/-32602），工具业务错误用正常 result + isError:true。

追问："为什么工具错误不用 RPC error？" → RPC error 表示协议层失败；工具失败是业务结果，client 要把它喂回模型做自我纠正——塞错层就断了模型的纠错回路。

**Q5：MCP 工具的安全边界和自己写的工具层有什么异同？**

标准回答：**相同的都要做**——不可信输入校验（参数 schema 校验、路径越界检查、输出截断）不会因为工具走 MCP 就消失，server 端要把第 2 章工具层的安全纪律重做一遍，因为 client 传来的 arguments 同样是不可信输入。**多出来的**是传输层问题：stdio 靠本机管道天然免鉴权，但 HTTP+SSE 部署时 server 必须做鉴权与限流（它现在是网络服务）；还有 capability 协商——server 只声明自己支持的原语，client 不应假设更多。

加分点：提到 prompt 注入的传递性——MCP 工具返回的内容进入模型上下文，间接注入的防线（数据/指令边界包装、高风险工具审批）在 host 侧仍要保留（第 1 章进阶 3.1、第 11 章 HITL）。

**Q6：token 成本怎么归因到任务/子任务维度？**

标准回答：每个 span（至少每个 LLM 调用 span）记录 input/output token 与模型名，客户端按单价表算出成本一并上报；之后"任务花多少钱"= trace 内 generation 求和，"哪个 worker 最贵" = 按 worker span 子树聚合。成本在客户端算的原因：预算熔断要在任务进行中实时拿到数字，等服务商账单是小时级延迟，熔断早晚了。

追问："DeepSeek 计价有什么坑？" → 官方区分缓存命中/未命中价（命中约为未命中的 1/4），多 agent 系统 planner 的 system prompt 前缀大量命中缓存，不区分会把成本高估——生产应读 usage 里的 cached tokens 分别计价。

**Q7：stdio 传输的优缺点？**

标准回答：优点——零网络配置（client 拉起子进程即用）、生命周期跟随 client 免部署、本机管道天然免鉴权、协议极简（一行一条 JSON-RPC）。缺点——只能本地用、无法远程共享多客户端、server 崩溃要 client 负责拉起。远程/团队共享场景用 HTTP+SSE，代价是引入鉴权、网络故障处理与部署运维。

追问："stdio server 最大的坑？" → stdout 是协议信道，混进任何非 JSON 字节（一条 log）client 解析直接炸——日志必须走 stderr；复用的工具代码里有 fmt.Println 调试输出时要临时接管 os.Stdout。

---

## 五、常见坑

1. **span 不闭环：StartSpan 之后忘了 EndSpan**。最常见于出错路径——LLM 调用返回 error 直接 return，EndSpan 没执行。后果：span 树残缺（那个 span 永远"进行中"），登记簿 map 只涨不删（内存泄漏），而且**恰恰是最需要被观测的失败路径丢了数据**。纪律：用 `defer` 或在所有返回路径上显式 EndSpan，err 原样传进去。
2. **token 只在任务级汇总，无法下钻**。只在任务结束时记一个总 token 数，账单涨了只能看到"这个任务贵"，看不到是哪个 worker、哪次调用贵——1.3 的所有归因查询全部失效。token 必须记在产生它的那个 span 上（LLM 调用级）。
3. **把 MCP 当成 function calling 的替代**。层级混淆的典型表现：以为"接了 MCP 模型就会直接用工具了"，漏掉中间的 schema 转换；或者以为"有了 function calling 就不需要 MCP"，忽视工具发现/接入的标准化价值。记住 1.9 那张分层图：MCP 管"应用 ↔ 工具提供方"，function calling 管"模型 ↔ 应用"。
4. **stdio server 往 stdout 打日志，污染协议流**。stdout 的每一个字节都会被 client 当 JSON-RPC 解析，一条 `fmt.Println` 调试输出就能让解析崩掉。日志一律 stderr（骨架的 `logf` 就是这么写的，`main.go:147`）；复用的旧工具代码里有 Println 时，调用期间临时把 `os.Stdout` 换走（TODO(练习7) 提示的方案）。
5. **进程退出前忘 Flush**。缓冲批量上报的实现里，事件都在内存 buffer 里，不 Flush 直接退出 = 整段任务的 trace 全丢。退出路径里 Flush 必须是固定一步——SIGINT/SIGTERM 的信号语义对照见第 9 章 §3.3，优雅关闭（`signal.NotifyContext` + `http.Server.Shutdown`）的落地代码见第 13 章 §3.3。

---

## 六、动手练习

本章对应阶段三练习 6、7，位置与验收：

**练习 6：Langfuse Tracer**（`stage-03-multi-agent/internal/trace/trace.go` 的 `TODO(练习6)`）

- 任务：新建 `langfuse.go`，实现 `NewLangfuse(host, publicKey, secretKey)` 并使 `*Langfuse` 满足 `Tracer` 接口，通过 Langfuse 公开 ingestion API 批量上报 trace/span/generation 事件，每 span 记 token、按单价表算成本。
- 验收：写 `trace_test.go` 用 `httptest.NewServer` 起假 Langfuse，模拟"任务 → 子任务 → 一次 LLM 调用"三层调用后 Flush，断言 trace-create 恰好一次、嵌套关系正确、generation 带 usage、Basic Auth 正确；`go vet` 与 `go test` 全绿。详细提示见骨架 TODO 块。
- 参考答案：`docs/solutions/stage-03/exercise-6-langfuse-trace.md`（**完成后再看**；含异步批量上报的进阶实现）。

**练习 7：MCP stdio server**（`stage-03-multi-agent/cmd/mcp-server/main.go` 的 `TODO(练习7)`）

- 任务：实现 `dispatch` 与 `initialize` / `notifications/initialized` / `tools/list` / `tools/call` 四个 handler，替换骨架里的 stub（`main.go:232`）。
- 验收（在 `stage-03-multi-agent/` 下）：

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"calculator","arguments":{"expression":"1+2*3"}}}' \
  | go run ./cmd/mcp-server
```

应得到恰好 3 行响应（通知无回复）：id:1 握手三件套、id:2 四个工具、id:3 算出 `"7"`。再试 `1/0` 应得 `isError:true`，试未知方法应得 `-32601`。
- 参考答案：`docs/solutions/stage-03/exercise-7-mcp-server.md`（**完成后再看**）。

---

## 本章小结

- log 回答"发生了什么"，trace 回答"哪个环节慢/贵"；多 agent 系统必须上树状 trace，且 span 层级要对齐 agent 层级——下钻能力全在这份对齐上。
- token 记在 span（LLM 调用）级，成本归因才能下钻到子任务；观测先行、优化后置，没有观测就没有成本控制。
- agent eval 分两层：结果评估看产出，轨迹评估看过程；trace 是轨迹评估的数据底座。
- `Tracer` 接口把编排器与观测后端解耦：本地 Noop，生产 Langfuse，编排器一行不改。
- MCP 是工具生态的 USB-C：解决 N×M 碎片化，与 function calling 是堆叠关系（应用↔工具方 / 模型↔应用），tools/list 的 schema 与 function calling 同构，转换零成本。
- MCP stdio server = JSON-RPC 2.0 类型 + 行读取循环 + initialize/tools/list/tools/call 三个 method；stdout 是协议信道，日志永远走 stderr。

下一章：[第 13 章：产品化集成——HTTP/SSE 与实时看板](13-server-sse-dashboard.md)——把编排引擎、trace、审批全部接到 HTTP API 与 Next.js 实时看板上，项目 3 成型。
