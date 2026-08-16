# 附录 A：术语速查表——面试前 20 分钟扫这一页

> 覆盖本教程 14 章全部核心概念。每条一句话定义 + 回链章节；用法：盖住右列自测，能脱口而出定义并能展开一个项目实例，就算过关。

## LLM API 与对话协议

- **messages 协议**：LLM API 的唯一输入，四角色数组；对话历史即状态，服务端无会话。→ [第 1 章](01-llm-api-and-messages.md)
- **system / user / assistant / tool**：四种消息角色；system 是最高优先级指令，tool 是工具执行结果的回挂角色。→ 第 1 章 §1.3
- **tool_calls**：assistant 消息里"我要调工具"的结构化意图，模型只产出意图，执行权在客户端。→ [第 2 章](02-function-calling-and-tools.md) §1.1
- **tool_call_id**：tool 消息与 assistant 工具调用配对的唯一纽带；漏挂、错挂、孤儿 tool 消息都会让 API 直接报错。→ 第 1 章 §1.3
- **temperature / top_p**：采样随机性的两个旋钮，改动一个就够；工具调用场景要调低。→ 第 1 章 §1.5
- **token 与上下文窗口**：计费与容量单位（中文约 1 字 ≈ 1-2 token）；窗口是硬上限，超出即报错或截断。→ 第 1 章 §1.6
- **上下文缓存（context caching）**：前缀相同的重复请求命中服务商缓存、显著降价——system prompt 等前缀要保持稳定。→ 第 1 章 §3.2
- **usage**：响应里的 token 计量；流式默认不返回，需 `stream_options.include_usage`。→ 第 1 章 §3.4、第 3 章 §2.4
- **SSE（Server-Sent Events）**：HTTP 长连接上的单向文本流协议，`data: ` 行帧，LLM 流式输出的载体。→ [第 3 章](03-react-loop-and-agent-kernel.md) §3.1
- **delta vs message**：流式按增量（delta）下发，非流式给完整 message；聚合 delta 才能得到完整消息。→ 第 3 章 §3.1
- **流式 tool_calls 聚合**：工具调用按 `index` 分片下传，必须按 index 拼完再解析 JSON。→ 第 3 章 §3.1
- **prompt 注入**：恶意内容（用户输入/检索文档）篡改模型指令；防御分层：指令分层 + 输入隔离 + 输出校验。→ 第 1 章 §1.7

## 工具与 Function Calling

- **function calling**：模型按 schema 产出结构化工具调用意图的能力；是 API 特性不是框架特性。→ 第 2 章 §1.1
- **工具三要素**：Name / Description / Schema；Description 写给模型看，要含"何时用、何时别用"。→ 第 2 章 §1.4、§3.3
- **Registry / dispatch**：工具注册表 + 按名分发；unknown tool 要把错误回喂而不是崩溃。→ 第 2 章 §2.2
- **错误回喂**：工具失败把 error 作为 tool 消息喂回模型，让模型自我恢复——agent 鲁棒性的关键一招。→ 第 2 章 §3.1、第 3 章 §2.2
- **args 不可信**：工具参数是模型生成的不可信输入，必须校验、限制范围、防路径逃逸。→ 第 2 章 §1.5、§2.5
- **并行 tool_calls**：一轮 assistant 可发多个工具调用；全部执行完按 id 回挂后继续循环。→ 第 2 章 §1.3

## Agent 内核

- **ReAct 循环**：Reason（思考）+ Act（行动）交替——"LLM → tool_calls → 执行 → 结果回喂 → LLM"直到无工具调用。→ 第 3 章 §1.1
- **终止条件**：无 tool_calls 终局 / 最大轮数 / 上下文超限压缩 / 错误 / 用户中断；循环必须有出口。→ 第 3 章 §1.3
- **历史即状态**：agent 的全部状态就是 messages 数组；无隐藏内存（显式记忆另算）。→ 第 3 章 §1.4
- **上下文压缩**：历史逼近窗口时摘要旧消息保近期；切分点避开 tool 消息组、摘要输入要带 tool_calls 渲染。→ 第 3 章 §2.3
- **错误分类重试**：4xx 不重试、5xx/429/网络错误指数退避 + jitter；总尝试 = 1 + maxRetries。→ 第 3 章 §3.2
- **脚本化假 LLM 测试**：把"调模型"抽象成接口，用按脚本应答的 fake 离线确定性测试循环。→ 第 3 章 §3.4

## RAG 与向量检索

- **embedding**：把文本映射为高维向量，语义相近则向量相近；bge-m3 = 1024 维。→ [第 4 章](04-embedding-and-vector-search.md) §1.1
- **余弦相似度**：方向夹角度量，与向量长度无关；归一化后内积 = 余弦。→ 第 4 章 §1.2
- **暴力检索 vs HNSW**：全量扫描 O(N·D) 精确但慢；HNSW 建图索引换近似——万级以下暴力就够。→ 第 4 章 §1.5
- **pgvector**：Postgres 的向量扩展，向量与业务数据同库；归一化向量用内积索引等价余弦。→ 第 4 章 §3.1
- **同模型纪律**：入库与查询必须用同一个 embedding 模型——换模型 = 换向量空间，旧向量全废。→ 第 4 章 §3.4、[第 5 章](05-rag-pipeline.md) §1.2
- **chunking**：长文切块；结构优先（标题/段落）+ 窗口兜底 + overlap 保上下文连续；是第一调参位。→ 第 5 章 §1.3、§2.1
- **all-or-nothing 入库**：chunk→embed→add 任一环节失败整批回滚，不留半个文档的脏库。→ 第 4 章 §2.3
- **混合检索**：向量（语义）+ BM25（关键词）互补，RRF 融合排名。→ 第 5 章 §1.4、§3.1
- **rerank**：粗排后精排模型重打分；接入位在检索后 prompt 前，要支持降级（rerank 挂了退回向量序）。→ 第 5 章 §1.5、§3.3
- **minScore 与空结果兜底**：相似度低于阈值宁可告诉模型"没资料"，不硬塞噪音——防幻觉的第一道闸门。→ 第 5 章 §2.3
- **防幻觉 prompt 三指令**：仅根据资料答 / 不足就明说 / 引用标注 [N]；且必须由服务端组装。→ 第 5 章 §2.3、[第 7 章](07-fullstack-kb-agent.md) §2.5
- **bad case 三板斧**：检索不到（chunk/embedding）→ 检索到没答好（prompt/模型）→ 答好了没引用（协议）；按链路定位。→ 第 5 章 Q3

## 记忆与评估

- **双层记忆**：短期 = 对话历史（窗口/压缩管理）；长期 = 显式事实，向量化存库、检索召回。→ [第 6 章](06-memory-and-evals.md) §1.1
- **回忆即检索**：记忆不加特殊机制，就是知识库里 `kind=memory` 的条目，走同一条检索链路。→ 第 6 章 §2.1
- **写入三问**：值不值得记（事实 vs 闲聊）/ 记成什么样（原子事实）/ 会不会重复（语义去重）。→ 第 6 章 §1.2
- **重要性衰减遗忘**：低重要性 + 久未被召回的记忆降权或清除；Forget 要高阈值防误删。→ 第 6 章 §3.3
- **recall@k / MRR**：检索质量双指标——命中率与排名位置联读，单独看一个会误判。→ 第 6 章 §1.5
- **LLM-as-judge**：用强模型给答案打分；三大偏差（位置/冗长/自偏好）要有缓解。→ 第 6 章 §3.2
- **eval 驱动调优**：先建评估集和基线指标，再调参——没有 eval 的调优是玄学。→ 第 6 章 §3.4

## 并发与工程（Go）

- **errgroup**：带 Wait + 错误传播 + ctx 取消的 goroutine 组；`SetLimit` 控并发。→ [第 8 章](08-go-concurrency-and-worker-pool.md) §1.3
- **Result.Err 模式**：部分失败语义——每个子任务的结果自带错误字段，收集方逐个判。→ 第 8 章 §1.4
- **预分配槽位**：结果写入各自下标而非 channel 汇聚，免锁免乱序。→ 第 8 章 §2.4
- **context 取消传播**：父 ctx 取消逐层传到所有子任务；超时预算逐层分配，善后换 Background。→ 第 8 章 §1.5
- **worker pool**：jobs channel 进、固定 N 个 worker 消费、Result 出；契约含关闭与背压语义。→ 第 8 章 §2.1
- **429 三道防线**：限并发（semaphore）+ 退避 jitter + 服从 Retry-After。→ 第 8 章 §3.3
- **goroutine 泄漏**：goroutine 阻塞在永不就绪的 channel 上；用 NumGoroutine/pprof/goleak 观测。→ 第 8 章 §3.4

## 持久化与崩溃恢复

- **状态机 + 显式迁移边**：六状态、迁移走数据驱动的合法边表，非法迁移当场报错。→ [第 9 章](09-task-persistence-and-recovery.md) §1.2
- **checkpoint 每步落盘**：恢复粒度 = checkpoint 粒度；每完成一步存一步。→ 第 9 章 §1.3
- **幂等键**：`taskID:subID` 判重——重试与崩溃恢复共用；有副作用的操作没有幂等 = 恢复即重复执行。→ 第 9 章 §1.5
- **at-least-once + 幂等去重**：宁可重放不可丢失，用幂等键消重——消息/任务系统的标准语义。→ 第 9 章 Q2
- **SQLite 单写者**：`SetMaxOpenConns(1)` 串行化写；WAL 提升读写并发；嵌入式零运维。→ 第 9 章 §1.7
- **状态外置、进程无状态**：状态放到比进程长寿的存储里，进程随便死——"历史即状态"的系统层放大。→ 第 9 章 §1.8
- **事件溯源（event sourcing）**：状态迁移即事件流全部落盘，可完整回放；本教程实现是当前态快照，事件表是演进方向。→ 第 9 章 Q3

## 多 Agent 编排

- **planner / worker / critic**：规划拆任务 → 并行执行 → 评审反馈回炉；critic 是可选叠加层。→ [第 10 章](10-orchestrator-planner-worker-critic.md) §1.3
- **生成-校验-带错重试**：planner 输出 JSON，四道防线（格式指令/围栏解析/结构校验/带错重试）。→ 第 10 章 §1.4
- **双重熔断**：轮次上限管深度、token 预算管广度；检查点设在 LLM 调用前。→ 第 10 章 §1.5
- **模型分级**：规划用强模型、执行用便宜模型——成本与质量的显式权衡。→ 第 10 章 §1.6
- **哨兵错误**：`ErrBudgetExceeded` / `ErrWaitingHuman`——"非正常但可预期"的出口统一用哨兵表达，`errors.Is` 判别。→ 第 10 章 §2.6
- **黑板 vs 消息传递**：共享状态读写 vs 显式消息流；教学项目选消息传递（显式、可追踪）。→ 第 10 章 §1.7

## Human-in-the-Loop

- **让出（yield）≠ 阻塞**：agent 挂起任务、释放执行权，等人工决策后从 checkpoint 恢复——复用崩溃恢复路径。→ [第 11 章](11-human-in-the-loop.md) §1.3
- **真相源与审计分离**：subtask.status 是真相源，approvals 表只记决策史；先迁状态后写审计。→ 第 11 章 §1.5
- **fail-closed**：审批超时默认拒绝（`system:timeout`），宁可误拒不可误放。→ 第 11 章 §3.1
- **审批策略表**：按工具/动作风险分级——只读免批、写需批、高危永远人工；与 planner 标记取并集。→ 第 11 章 §3.2
- **审批疲劳 / 自动化偏见**：审批太多人会变橡皮图章；HITL 设计上要控制审批频率与信息质量。→ 第 11 章 §1.2

## 观测与 MCP

- **trace / span / generation**：一次任务的调用树；span 是节点，LLM 调用记为 generation 带 token 计量。→ [第 12 章](12-observability-and-mcp.md) §1.1
- **Tracer 接口 + Noop**：观测抽象成接口，默认空实现——观测绝不影响主流程。→ 第 12 章 §2.1
- **结果评估 vs 轨迹评估**：eval 两层次；trace 是轨迹评估的数据底座。→ 第 12 章 §1.4
- **MCP（Model Context Protocol）**：工具/资源/prompt 的标准化接入协议，解决 N×M 集成问题；是标准化不是新能力。→ 第 12 章 §1.6
- **host / client / server 三角**：LLM 应用（host）内经 client 连接外部工具进程（server）；stdio 或 HTTP+SSE 传输。→ 第 12 章 §1.7
- **JSON-RPC 2.0**：MCP 的线缆格式——id 原样回显、通知不回复、NDJSON 行帧、标准错误码。→ 第 12 章 §1.10
- **stdout 即协议信道**：stdio 传输下 stdout 只准跑协议帧，日志必须走 stderr。→ 第 12 章坑 4

## 服务化与前端

- **接单即返 202**：请求生命周期 ≠ 任务生命周期；HTTP 层只接单派活，结果走读路径。→ [第 13 章](13-server-sse-dashboard.md) §1.2
- **ctx 铁律**：后台 goroutine 用 `context.Background()` 派生，读路径用 `r.Context()`。→ 第 13 章 §1.3
- **poll-diff**：轮询比对快照差异再推送——零侵入、跨进程正确、断线自愈。→ 第 13 章 §1.5
- **背压与 Last-Event-ID**：消费者慢就丢帧保新鲜；断线续传靠事件 id。→ 第 13 章 §3.2
- **优雅退出**：`signal.NotifyContext` + `http.Server.Shutdown` + 退出前 Flush；SSE 服务不设 WriteTimeout。→ 第 13 章 §3.3
- **UI Message Stream（AI SDK）**：一条 SSE 流混跑 text part（给模型/用户看）与 data part（给前端渲染）。→ 第 7 章 §1.3、§3.1
- **UIMessage vs ModelMessage**：前端消息含 parts/元数据，喂模型前要 `convertToModelMessages` 转换。→ 第 7 章 §1.3
- **globalThis 单例**：Next.js dev 热重载下保进程级状态的手段；serverless/多实例不成立。→ 第 7 章 §2.2
- **NEXT_PUBLIC_ 泄漏**：带此前缀的 env 会内联进客户端 bundle——密钥永远用无前缀变量。→ 第 7 章坑 2

## 面试元技能

- **30 秒电梯版**：每个项目准备一段"架构一句话 + 两条路径 + 三个关键决策"的电梯陈述。→ 第 7 章 §4、[第 14 章](14-interview-and-career.md)
- **追问链**：每个标准回答后面跟 2-3 层"为什么/如果……怎么办"；准备深度比准备广度值钱。→ 各章 §4
- **诚实标注边界**：主动说清"这是教学简化，生产还要 X/Y/Z"——比假装生产级加分得多。→ 第 14 章

---

返回 [教程首页](README.md) | 下一篇：[附录 B：结课综合项目](appendix-b-capstone.md)
