# LangGraph 完全指南——给手写过 agent 内核的人

> 定位：阶段五配套教程，求职向。适用读者：已完成本仓库项目 1-3（手写 ReAct 内核、RAG、多智能体编排），需要系统补上框架经验的人。
> 学法一句话：**给每个框架概念找到"我手写过的对应物"**——你不是从零学框架，是在认亲。
> 代码说明：本文所有 TypeScript 代码均在 **langchain@1.5.9 / @langchain/langgraph@1.4.10** 上实际运行通过（用脚本化假模型替代真实 LLM，零 API 成本、结果确定性）。框架 API 仍可能演进，动手时以官方文档为准。

## 一、为什么学 LangGraph，以及它在 2026 年的位置

1. **JD 命中率**：agentic 岗位点名率最高的框架就是 LangChain/LangGraph 系；
2. **与你的手写系统同构度最高**：LangGraph 的核心抽象（显式状态 + 节点/边 + checkpoint + interrupt）与阶段三的编排引擎几乎一一对应；
3. **生态位在 1.0 后定型**（2025-10-22 LangChain 1.0 与 LangGraph 1.0 同发 GA）：**LangChain 的 `create_agent` 是高层入口，跑在 LangGraph 运行时上；要自定义控制流就下沉到 StateGraph**。老的 `AgentExecutor` 已废弃（2026-12 停止维护）。参考：[LangChain 1.0 架构转向](https://uvik.net/blog/langchain-vs-langgraph/)、[AgentExecutor 废弃公告](https://www.agenticwire.news/article/agentexecutor-deprecated-migrate-create-agent)。

**版本选型**：你的技术栈是 Go + TS，直接用 **JS/TS 版**（`@langchain/langgraph` + `langchain`）动手；Python 版文档和示例最多，读文档时两边对照即可，概念完全一致。

```bash
npm install langchain @langchain/langgraph @langchain/core zod
```

## 二、十分钟全景：先见森林

LangGraph 把 agent 系统建模为一张**状态图**：

```
状态（State）在图上流转，节点（Node）读状态、返回状态增量，
边（Edge）决定下一步去哪，条件边（Conditional Edge）做分支，
边可以指回前面构成环（Cycle）——ReAct 循环就是最小的一个环。
```

最小骨架（这就是你 mini-agent 的 `Run` 循环在框架里的样子）：

```ts
const graph = new StateGraph(MessagesAnnotation)
  .addNode("llm", callModel)            // 节点：一次 LLM 调用
  .addNode("tools", new ToolNode(tools)) // 节点：执行工具
  .addEdge(START, "llm")
  .addConditionalEdges("llm", toolsCondition) // 有 tool_calls → tools，否则 → END
  .addEdge("tools", "llm")              // 这条边就是 ReAct 的环
  .compile();                            // 编译成可运行的 Runnable
```

记住这张图，后面所有概念都是它的展开。

## 三、核心概念详解

### 3.1 State 与 Reducer：框架版的"历史即状态"

**是什么**：State 是在图上流转的共享数据，用 `Annotation.Root` 声明通道（channel）；每个通道可以挂一个 **reducer**——节点返回的是"增量"，reducer 决定增量如何合并进旧状态。

```ts
const State = Annotation.Root({
  action: Annotation<string>,          // 无 reducer：节点返回什么就覆盖什么
  log: Annotation<string[]>({
    reducer: (a, b) => a.concat(b),    // 有 reducer：增量追加
    default: () => [],
  }),
});
```

**手写对应物**：你在 mini-agent 里的 `messages = append(messages, msg)` 就是一个追加 reducer；框架把它泛化成"每个通道一种合并策略"。最常用的预置 state 是 `MessagesAnnotation`——只含一个 `messages` 通道，reducer 是 `add_messages`（追加 + 按 id 更新，且自动处理 ToolMessage 配对）。

**面试点**：被问"LangGraph 的 state 怎么更新"——答"节点返回增量，reducer 合并；默认覆盖，messages 通道用 add_messages 追加"。能补一句"这和我手写循环里的 append 是同一回事，框架只是把它显式化了"，立刻显出深度。

### 3.2 Node / Edge / Conditional Edge / Cycle

- **Node**：一个函数，输入是当前 state（或其视图），输出是 state 增量。LLM 调用、工具执行、纯计算都可以是节点。
- **Edge**：固定跳转（`addEdge("a", "b")`）。
- **Conditional Edge**：分支——一个函数读 state 返回下一个节点名。你手写的 `if len(msg.ToolCalls) == 0 { return }` 就是一个条件边。
- **Cycle**：边指回前面的节点即成环。**ReAct = 两点一环**：`llm → tools → llm → …` 直到条件边把流导向 `END`。

**面试点**："LangGraph 的循环怎么终止？"——条件边返回 `END`；对应你手写的终止条件五种。环本身是安全的（图执行是单步推进的），死循环防护（你的 MaxSteps）框架不内建，要自己记数或用 `recursionLimit`（invoke 的配置项，默认 25 步）——**`recursionLimit` 就是框架版的"最大步数保险丝"**。

### 3.3 ToolNode 与 toolsCondition：预制的工具执行器

`ToolNode` 是一个预制节点：读取最后一条 AIMessage 里的 `tool_calls`，逐个执行注册的工具，把结果包成 `ToolMessage`（自动回挂 `tool_call_id`）返回。`toolsCondition` 是配套的条件边函数：最后一条消息有 tool_calls 就去 `"tools"`，否则去 `END`。

**手写对应物**：你的 `Registry.Call` 分发 + "assistant 消息原样放回 + tool 消息回挂"那段纪律。框架把协议完整性做成了默认值——但你要知道它在做什么，因为出错时（工具抛异常变 ToolMessage 还是中断图，ToolNode 有配置项）排查靠的就是这层理解。

### 3.4 完整 ReAct 示例（已验证）

用脚本化假模型（与结课项目 P3 的 `fakeModel` 同构）替代真实 LLM，整个循环离线可跑：

```ts
import { StateGraph, MessagesAnnotation, START } from "@langchain/langgraph";
import { ToolNode, toolsCondition } from "@langchain/langgraph/prebuilt";
import { BaseChatModel } from "@langchain/core/language_models/chat_models";
import { AIMessage, BaseMessage, HumanMessage } from "@langchain/core/messages";
import { ChatResult } from "@langchain/core/outputs";
import { tool } from "@langchain/core/tools";
import { z } from "zod";

// 脚本化假模型：按调用次序播放预定 AIMessage——确定性、零成本、离线可跑
class ScriptedChatModel extends BaseChatModel {
  calls = 0;
  constructor(private steps: AIMessage[]) {
    super({});
  }
  _llmType() {
    return "scripted";
  }
  async _generate(_messages: BaseMessage[]): Promise<ChatResult> {
    this.calls++;
    const msg = this.steps.shift();
    if (!msg) throw new Error("script exhausted");
    return { generations: [{ message: msg, text: String(msg.content) }] };
  }
}

const getTime = tool(async () => "20:00", {
  name: "get_time",
  description: "获取当前时间",
  schema: z.object({}),
});

const model = new ScriptedChatModel([
  new AIMessage({ content: "", tool_calls: [{ id: "c1", name: "get_time", args: {} }] }),
  new AIMessage({ content: "现在是 20 点" }),
]);

const graph = new StateGraph(MessagesAnnotation)
  .addNode("llm", async (state) => ({ messages: [await model.invoke(state.messages)] }))
  .addNode("tools", new ToolNode([getTime]))
  .addEdge(START, "llm")
  .addConditionalEdges("llm", toolsCondition)
  .addEdge("tools", "llm")
  .compile();

const result = await graph.invoke({ messages: [new HumanMessage("现在几点？")] });
```

实测输出（消息演化序列与你的 mini-agent 完全一致）：

```
- human  content="现在几点？"
- ai     tool_calls=get_time
- tool   content="20:00"          ← ToolNode 自动回挂 tool_call_id
- ai     content="现在是 20 点"    ← 第二轮无 tool_calls，条件边导向 END
模型调用次数: 2
```

### 3.5 Checkpointer 与 thread_id：框架版的"每步落盘"

**是什么**：编译时挂 `checkpointer`，图每执行完一个 super-step（一组节点）就把**整个 state** 持久化一次；`thread_id` 标识一条会话/任务，同一 thread 的多次 invoke 自动接续状态。

```ts
const graph = builder.compile({ checkpointer: new MemorySaver() });
const config = { configurable: { thread_id: "approval-1" } };
await graph.invoke(input, config);            // 第一轮
await graph.invoke(nextInput, config);        // 同一 thread：状态还在
await graph.getState(config);                 // 读快照（对应你的 LoadTask）
```

实现有三档：`MemorySaver`（内存，测试用）、`SqliteSaver`、`PostgresSaver`（`@langchain/langgraph-checkpoint-sqlite` / `-postgres`）。

**手写对应物**：阶段三的 `task.Store`——"进程会死，状态不死"。差异要讲清：框架 checkpoint 存的是**泛化的 state 快照**，你存的是**任务领域模型**（状态机 + 幂等键）；框架给你恢复的机制，但"恢复后哪些步骤要幂等判重"仍是你的领域逻辑。

### 3.6 interrupt 与 Command(resume)：让出式 HITL 的一等公民（已验证）

**是什么**：节点里调 `interrupt(payload)` = 挂起图执行，把 payload 交给调用方；调用方处理完后用 `Command({ resume: value })` 恢复，`interrupt()` 这次返回 resume 的值，节点**从头重跑**（注意这个语义，见常见坑）。

```ts
import { interrupt, Command, MemorySaver } from "@langchain/langgraph";

const approval = async (s: typeof State.State) => {
  const answer = interrupt({ question: `是否批准「${s.action}」？` }); // 挂起点
  return { approved: answer === "y", log: [`审批结果: ${answer}`] };
};

const graph = new StateGraph(State)
  // ……addNode/addEdge 略……
  .compile({ checkpointer: new MemorySaver() }); // interrupt 必须有 checkpointer

const config = { configurable: { thread_id: "approval-1" } };
const first = await graph.invoke({ action: "删除生产库", log: [] }, config);
// first 含 __interrupt__ 键，负载即 { question: "是否批准「删除生产库」？" }

const second = await graph.invoke(new Command({ resume: "y" }), config);
```

实测输出：

```
中断时返回的键: [ 'action', 'log', '__interrupt__' ]
中断负载: {"question":"是否批准「删除生产库」？"}
恢复后的 log: ["提议执行: 删除生产库","审批结果: y","已执行"]
```

**手写对应物**：这就是你的 `ErrWaitingHuman` + `Resume`——你把"让出"用哨兵错误实现，框架把它做成了控制流原语。两处关键差异，面试可主动讲：① 框架恢复时从 checkpoint 重建整个状态，你从 task.Store 读任务模型重建；② `interrupt` 恢复时**节点从头重跑**（interrupt 前的代码会再执行一遍），你的恢复是从"等待的那个子任务"粒度续跑——所以 interrupt 节点里 interrupt() 之前不能有副作用代码。

### 3.7 Send：动态扇出，框架版的 planner/worker 分发（已验证）

**是什么**：条件边不返回节点名，而是返回一组 `Send(nodeName, payload)`——框架为每个 Send 并行调度一次目标节点，各实例的私有输入就是 payload；结果经 reducer 通道聚合。这就是 map-reduce 模式，对应你 planner 拆子任务 + worker pool dispatch。

```ts
import { Send } from "@langchain/langgraph";

const State = Annotation.Root({
  goal: Annotation<string>,
  tasks: Annotation<string[]>,
  results: Annotation<string[]>({ reducer: (a, b) => a.concat(b), default: () => [] }),
  task: Annotation<string>, // worker 的私有输入通道，由 Send 填充
});

const planner = async () => ({ tasks: ["调研", "写稿", "校对"] });
const fanout = (s: typeof State.State) => s.tasks.map((t) => new Send("worker", { task: t }));
const worker = async (s: { task: string }) => ({ results: [`${s.task}✓`] });
const aggregate = async (s: typeof State.State) => ({ results: [`汇总：共 ${s.results.length} 项`] });

const graph = new StateGraph(State)
  .addNode("planner", planner)
  .addNode("worker", worker)
  .addNode("aggregate", aggregate)
  .addEdge(START, "planner")
  .addConditionalEdges("planner", fanout, ["worker"])
  .addEdge("worker", "aggregate")
  .addEdge("aggregate", END)
  .compile();
```

实测输出：`聚合结果: ["调研✓","写稿✓","校对✓","汇总：共 3 项"]`（顺序为并行完成的实际序）。

**框架不管的两件事**（手写者的加分答案）：① **并发上限**——Send 扇出 100 个就同时跑 100 个，你的 pool 有 worker 数上限；② **429 退避**——限流重试是你的 client 层职责。用框架时这两块要自己补（middleware 或包在节点里）。

### 3.8 Streaming：三种粒度，对应你的三种推送（已验证）

```ts
const stream = await graph.stream(input, { streamMode: "updates" });
for await (const chunk of stream) { /* … */ }
```

- **`updates`**：每个节点执行完，推它返回的**增量**（节点名 → state 增量）——对应你的"步骤级进度"；
- **`values`**：每个 super-step 后推**全量 state**——对应你 poll-diff 的全量快照语义；
- **`messages`**：LLM token 级流（message chunk + metadata）——对应你的 `OnDelta`；
- 另有 `debug`（全事件）与 `custom`（节点内自定义事件）。多模式可传数组同时订阅。

实测（updates 模式依次收到 `llm` / `tools` / `llm` 三个增量包；values 模式每步推完整 messages 数组）。

**手写对应物**：阶段三的 SSE 选型在框架里是"选 streamMode"——概念没变，框架把分发做了。

### 3.9 createAgent 与 Middleware：高层入口与标准扩展点（已验证）

`createAgent`（LangChain 1.0，TS 里 `import { createAgent } from "langchain"`）一行装配一个 ReAct agent：模型 + 工具 + 内置消息循环。**它内部就是一个编译好的 StateGraph**——所以学会 StateGraph 后它不是黑盒。

真正值钱的是 **middleware**——框架把"包住模型/工具调用"做成了标准扩展点：

```ts
import { createAgent, createMiddleware } from "langchain";

// wrapToolCall 就是结课项目 ApprovalGate 的框架版
const approval = createMiddleware({
  name: "approval",
  wrapToolCall: async (request, handler) => {
    console.log("[人工审批]", request.toolCall.name, JSON.stringify(request.toolCall.args));
    return handler(request); // 放行；不调 handler 并返回 ToolMessage 即拒绝
  },
});

const agent = createAgent({ model, tools: [writeFile], middleware: [approval] });
```

实测输出：`[人工审批] 工具 write_file 参数 {"path":"a.txt"}` → 工具执行 → `最终回答: 写好了`。

middleware 钩子全家桶：`before_agent` / `before_model` / `wrap_model_call` / `wrap_tool_call` / `after_model` / `after_agent`。对照你的手写系统：`before_model` ≈ 压缩钩子 `compressIfNeeded`，`wrap_tool_call` ≈ `ApprovalGate`，`wrap_model_call` ≈ 重试包装 `ChatWithRetry`——**你在手写系统里做的横切关注点，框架里都是 middleware**。

### 3.10 Subgraph 与多 agent 模式

- **Subgraph**：一张图可以作为一个节点嵌进另一张图——对应你把"一个 agent"封装成编排器里的角色。
- **Supervisor 模式**：一个 supervisor 节点（通常带推理模型）根据 state 决定下一个 worker 是谁——对应你的 planner；worker 结果回 supervisor 再决策，构成环。
- **Swarm/handoff**：worker 之间直接把控制权交给彼此（`Command(goto=...)`）——对应 orchestrator 指南里的 handoff 模式。

不需要背 API：被问"多 agent 在 LangGraph 里怎么搭"，答"每个角色一个节点或子图，路由用条件边或 supervisor，任务级隔离用 Send 的私有输入"即可。

### 3.11 LangSmith：观测层

LangSmith 是同家公司的观测产品：设两个环境变量（`LANGSMITH_TRACING=true` + API key）即得嵌套 trace、token 计量、回放。**与你阶段三接 Langfuse 是同一层**——面试时一起讲："trace 的 span 层级对应 agent 层级，这套心智模型我在手写系统里用 Langfuse 落地过，LangSmith 是它的商业同类。"

## 四、能力分界：框架替你做了什么、没做什么

| 维度 | LangGraph 管不管 | 你手写系统的对应物 |
| --- | --- | --- |
| 消息状态合并 | ✅ reducer | `messages = append(...)` |
| 工具协议完整性（tool_call_id 回挂） | ✅ ToolNode | 手写纪律 |
| 状态持久化/恢复 | ✅ checkpointer | `task.Store`（SQLite） |
| HITL 让出/恢复 | ✅ interrupt/Command | `ErrWaitingHuman` + Resume |
| 动态并行分发 | ✅ Send | planner dispatch |
| 流式推送 | ✅ streamMode | OnDelta / SSE / poll-diff |
| 死循环保险丝 | ✅ `recursionLimit` | `MaxSteps` |
| **并发度上限** | ❌ 要自己包 | pool 的 worker 数 |
| **429 退避/重试** | ❌ 要自己包 | `ChatWithRetry` + 指数退避 |
| **token 预算熔断** | ❌ 要自己包 | 预算检查点（双重熔断） |
| **领域状态机/幂等键** | ❌ 是你的领域逻辑 | 六状态机 + `taskID:subID` |

**选型判断（面试直接可用）**：一次性调用、线性 pipeline 不要上 StateGraph（构建/编译/合并有真实开销）；需要**环、持久化、HITL、并行扇出**四类能力之一时它很值。框架接管的是"通用机制"，留给你的恰是"领域语义"——这与阶段三"server 零业务逻辑"是同一条设计哲学。

## 五、面试视角

**Q：你用过 LangGraph 吗？**
> "用过，而且是在手写过一个同构系统之后用的——我的理解方式不是 API 而是'它替我做了什么'：reducer 替代我手写的状态合并，checkpointer 替代我用 SQLite 做的每步落盘，interrupt/Command 对应我用哨兵错误实现的 HITL 让出-恢复。正因为手写过，我清楚它的边界：并发上限、429 退避、成本熔断框架都不管，这些我在手写系统里做过，用框架时知道要在哪补。"

**Q：LangChain 和 LangGraph 什么关系？**（1.0 后标准答案）
> "一层栈：LangChain 的 create_agent 是高层入口，跑在 LangGraph 运行时上；要自定义环、分支、中断恢复就下沉到 StateGraph。老的 AgentExecutor 已废弃。"

**Q：LangGraph 的状态持久化怎么做的？**
> "checkpointer 挂在编译后的图上，每个 super-step 把整个 state 落一次，thread_id 标识会话；MemorySaver/SqliteSaver/PostgresSaver 三档。恢复时按 thread_id 读快照重建——我在手写系统里实现过同语义的东西，区别是我存的是领域任务模型（状态机+幂等键），框架存的是泛化 state。"

**Q：interrupt 恢复的执行语义是什么？**（区分背文档和真用过）
> "节点从头重跑，interrupt() 这一次返回 resume 的值——所以 interrupt 之前不能有副作用代码，否则恢复时副作用执行两次。这和我手写恢复时'副作用要幂等'是同一条纪律的两种形态。"

**Q：框架缺什么？**
> "并发度上限（Send 扇出多少就同时跑多少）、429 退避、token 预算熔断——三块都要自己补 middleware 或包在节点里。我在手写系统里分别用 semaphore、指数退避+Retry-After、预算检查点做过。"

## 六、常见坑

1. **interrupt 节点里把副作用写在 interrupt() 之前**：恢复时节点重跑，副作用执行两次（重复发消息/扣费）。规则：interrupt 之前只读 state，副作用放 interrupt 之后。
2. **interrupt 没有 checkpointer**：编译时不挂 checkpointer，interrupt 无处可存，直接报错或行为异常。
3. **把 state 当可变对象原地改**：节点应该返回增量对象，不要 mutate 入参 state——reducer 语义依赖"返回增量"，原地改会绕过它导致诡异行为。
4. **Send 扇出无上限**：任务列表来自 LLM 时尤其危险（模型给你拆 50 个子任务，50 个并发直接打爆限流）——扇出前 clamp 列表长度（对应你的 `MaxSubtasks=8`）。
5. **thread_id 复用混乱**：一个会话一个 thread_id，复用别人的 id 会把两拨状态混在一起——和你的"幂等键命名空间"同理。
6. **TS/Python 示例混抄**：两边 API 名有细微差异（如 TS 的 `MessagesAnnotation` vs Python 的 `MessagesState`），跨语言抄代码先查对应版文档。
7. **版本漂移**：1.0（2025-10）后 API 基本稳定，但 minor 版本仍可能改名/移动导出路径——跑不起来先核对安装的版本号再看文档版本。

## 七、资源清单

- 官方文档：`https://docs.langchain.com`（1.0 文档，LangGraph 内嵌其中）；TS API 参考 `reference.langchain.com/javascript/`
- 官方课程：LangGraph Academy（免费，搜 "LangChain Academy"）
- TS 版仓库示例：`github.com/langchain-ai/langgraphjs` 的 examples
- 对照阅读：本仓库 [阶段三沉淀](stages/stage-03-multi-agent-production.md)、[多 agent 编排指南](multi-agent-orchestration-guide.md)、[教程第 10 章](tutorial/10-orchestrator-planner-worker-critic.md)、[第 11 章](tutorial/11-human-in-the-loop.md)

## 八、小结

LangGraph = 把你手写系统里的通用机制（状态合并、工具协议、落盘恢复、让出审批、并行分发、流式推送）做成了标准件，留下给你的恰好是领域语义（状态机、幂等、预算、限流）。学完本文你应该能：

- 默画 StateGraph 的最小 ReAct 图并指出环在哪；
- 讲清 reducer / checkpointer / interrupt / Send 各自对应你手写的什么；
- 说出框架不管的四件事及你手写过哪几件；
- 在"用框架"与"过度设计"之间给出有分寸的判断。

---

返回：[阶段五：模型原理与微调决策](stages/stage-05-model-finetuning.md) | [教程首页](tutorial/README.md) | [ROADMAP](ROADMAP.md)
