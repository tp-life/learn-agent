# 第 3 章：ReAct 循环与 Agent 内核——一个循环吃透所有框架

> 对应阶段：阶段一（地基）· 项目 1 `mini-agent/`
> 代码位置：`mini-agent/internal/agent/agent.go`（本章核心教材）、`mini-agent/internal/llm/client.go`（ChatStream / ChatWithRetry）
> 前置：第 1 章（LLM API 与 messages 协议）、第 2 章（Function Calling 与工具层设计）
> 学完后你能讲清：ReAct 循环每一行代码在做什么、五种终止条件各自防什么、流式为什么能进循环、重试怎么分类、上下文压缩怎么不压坏协议配对——这些是所有 Agent 框架的共同内核，也是面试手写题的高频出处。

---

## 本章地图

- ReAct 思想：思考-行动-观察交替，直到任务完成
- "Agent = LLM + 工具 + 循环"：循环是本体，框架是包装
- 终止条件五种：正常终止、保险丝、不可恢复错误、超时、预算
- 历史即状态再深入：Agent 结构体的全部状态就是一个 messages 切片
- 代码精讲：`Run` 全流程、工具错误回喂、`compressIfNeeded` 逐段、Usage 计量
- 进阶拓展：SSE 流式聚合、指数退避与 jitter、流式重试的额外语义
- 面试视角：终止条件 / 流式进循环 / 历史三板斧 / 手画时序图 / 压缩切分点

---

## 一、概念详解

### 1.1 ReAct：思考-行动-观察的交替循环

ReAct 出自 Google Brain 2022 年的论文《ReAct: Synergizing Reasoning and Acting in Language Models》（Yao 等，发表于 ICLR 2023）。论文的核心观察是：让模型把**推理（Reasoning）**和**行动（Acting）**交替进行，比"一口气想完再做"或"无脑连续调工具"都强——行动的结果（Observation）回到上下文后，会修正后续的推理方向。

用 Function Calling 的协议语言翻译过来，三者的对应关系是：

| ReAct 论文概念 | 本项目的协议实体 | 谁产生 |
| --- | --- | --- |
| Reasoning（思考） | 模型决定调哪个工具、传什么参数（assistant 消息的 `tool_calls`） | 模型 |
| Acting（行动） | `registry.Call` 真正执行工具 | 你的代码 |
| Observation（观察） | `role=tool` 消息回到历史 | 你的代码 |

模型每看到一次 Observation，就重新"思考"一次：是继续调工具，还是已经足够给出最终答案。这个交替一直持续到模型认为任务完成。用伪代码写出来，就是 Agent 的全部：

```
messages = [system, user输入]
for {
    resp = LLM(messages, tools)          // 思考：直接回答 or 请求工具
    if resp 没有 tool_calls {
        return resp.Content              // 模型认为任务完成，循环结束
    }
    messages = append(messages, resp.Message)        // assistant 原样放回
    for each tc in resp.ToolCalls {
        result = 执行(tc)                           // 行动
        messages = append(messages, tool消息(result)) // 观察回喂
    }
}
```

这个循环不是某个框架的专利，它是 Agent 的**定义本身**。本项目的 `Run` 方法（`mini-agent/internal/agent/agent.go:51`）就是这个伪代码逐行对应的 Go 实现，第二章代码精讲逐段对照。

### 1.2 Agent = LLM + 工具 + 循环：循环是本体，框架是包装

把这句话拆开：

- **LLM**：决策者，负责"下一步做什么"（第 1 章）；
- **工具**：能力边界，负责"能做什么"（第 2 章）；
- **循环**：把决策和能力焊接起来的控制流——这是本章的主角。

市面上的框架（LangChain 的 AgentExecutor、AutoGen 的 ConversableAgent，以及各家新框架）剥掉外围抽象后，内核都是这个循环。框架提供的是包装：工具生态、memory 管理、回调与可观测性、与自家模型生态的绑定。这不是说框架没用——生产项目往往该用框架——而是说：

- **学的时候**：先手写循环，框架的每个抽象你都能说出"它替代了我手写的哪部分"；
- **面试的时候**："LangChain 的 Agent 是怎么工作的"这种题，正确答案就是这个循环 + 终止条件，背框架 API 是答不到点上的；
- **排障的时候**：框架出问题，能穿透到循环层看 messages 流向，才定位得了 bug。

一句话记忆卡片：**模型是"动嘴的"，代码是"动手的"，两者之间的协议是 tool_calls，循环是让这个对话持续下去的传送带。**

### 1.3 终止条件五种：循环必须有出口

没有出口的循环就是烧钱循环。Agent 循环的终止条件要能从"防什么"的角度讲出五种：

1. **无 tool_calls，正常终止**。模型返回纯文本 = 它认为任务完成。这是唯一"好"的出口，对应 `agent.go:85` 的判断。
2. **MaxSteps 保险丝**。模型可能陷入"调工具→结果不满意→再调"的死循环（例如反复 http_fetch 一个 403 的页面）。MaxSteps 是硬性上限，耗尽后报错退出（`agent.go:54`、`agent.go:116`）。本项目默认 10（`agent.go:46`）。注意它防的是**逻辑死循环**，不是慢。
3. **不可恢复错误**。LLM API 返回 401（鉴权失败）这类错误时，继续循环毫无意义——下一轮还是 401。本项目在 `agent.go:64` 直接 `return "", err` 抛出。区分"值不值得继续"靠错误分类，见进阶 3.2 的 `retryable`。
4. **超时熔断**。防的是"单次调用卡死"：网络对端挂死、模型生成超长。本项目的地基是 HTTP 客户端 120s 超时（`client.go:30`）；任务级、子任务级的超时预算分级在第 10 章展开。
5. **成本预算**。累计 token 花费超过预算就熔断——防的不是故障，是账单。前提是循环里持续计量，这正是本章 2.4 节 `Usage` 累计的设计动机，预算熔断本身也在第 10 章落地。

面试里能按"防什么"分类讲全五种，比背诵"无 tool_calls 就停"高一个档次——前者说明你知道循环会怎么失控。

### 1.4 历史即状态：解剖 Agent 结构体

第 1 章讲过"对话历史就是状态"，现在看这个命题在代码里的完全体（`agent.go:22-39`）：

```go
type Agent struct {
	client   *llm.Client
	registry *tools.Registry
	messages []llm.Message // 完整的对话历史，每轮循环都会增长

	MaxSteps int // 防止死循环的保险丝：模型可能反复调工具停不下来
	Verbose  bool

	usage llm.Usage // 累计本次任务所有 LLM 调用的 token 用量

	OnDelta func(text string) // 流式文本增量回调
}
```

逐个字段分类：**`client` / `registry` 是依赖**（启动时注入，运行中不变），**`MaxSteps` / `Verbose` / `OnDelta` 是配置**，真正随运行变化的**可变状态只有两个：`messages` 和 `usage`**。而其中承载"任务进展"的，只有 `messages` 一个切片。

这个极简结构有两个重要推论：

- **Agent 的持久化 = 序列化一个切片**。想把暂停的 Agent 存盘、换台机器恢复，把 `messages` 落盘就够了——第 9 章的 checkpoint 机制就是这个思想在多 Agent 系统里的放大。
- **Agent 的调试 = dump 这个切片**。Agent 行为诡异时，把 messages 按 role 打印出来逐条看，90% 的问题（system 丢了、tool 消息孤儿了、历史里混进了脏内容）都能直接看出来。**messages 数组是 Agent 系统唯一的真相源（single source of truth）**。

---

## 二、代码精讲

### 2.1 `Run` 全流程：伪代码的逐行落地

`Run`（`agent.go:51-117`）约 60 行，是本项目最重要的一段代码。按执行顺序拆成七段看。

**第 1 段：用户输入入队（`agent.go:52`）**

```go
a.messages = append(a.messages, llm.Message{Role: "user", Content: userInput})
```

"历史即状态"的第一个动作：用户说的每句话先变成状态的一部分，之后每轮 LLM 请求都会带着它。

**第 2 段：循环骨架与保险丝（`agent.go:54`）**

```go
for step := 0; step < a.MaxSteps; step++ {
```

用有界的 `for` 而不是 `for {}`，把保险丝写进循环结构本身——这是比"循环里 if 判断退出"更难写错的形态。

**第 3 段：压缩前置检查（`agent.go:56-58`）**

```go
if err := a.compressIfNeeded(); err != nil && a.Verbose {
	fmt.Printf("[compress] 失败（忽略，继续对话）： %v\n", err)
}
```

两个设计点：其一，检查放在**每轮循环开头、请求之前**，而不是等 API 报 `length` 错误再补救——真等报错，这一轮请求已经浪费了，而且流式场景下增量可能已经打出一半，更难收拾。其二，压缩失败只打印、不中断对话：**压缩是优化手段，不是关键路径**，为它牺牲可用性不值得。

**第 4 段：流式请求与用量累计（`agent.go:63-73`）**

```go
resp, err := a.client.ChatStream(a.messages, a.registry.Schemas(), a.OnDelta)
if err != nil {
	return "", err
}
choice := resp.Choices[0]
msg := choice.Message

a.usage.PromptTokens += resp.Usage.PromptTokens
a.usage.CompletionTokens += resp.Usage.CompletionTokens
a.usage.TotalTokens += resp.Usage.TotalTokens
```

主循环走的是流式 `ChatStream` 而不是 `Chat`——这是练习 1 的成果，它能进循环的前提是"聚合成完整响应再决策"，原理在进阶 3.1 完整展开。API 错误直接抛出，对应终止条件 3。usage 累计见 2.4 节。

**第 5 段：assistant 消息原样放回（`agent.go:75-77`）**

```go
// 关键：assistant 的消息（含 tool_calls）必须原样放回历史，
// 否则后续 role=tool 的消息失去对应关系，API 会报错。
a.messages = append(a.messages, msg)
```

这是阶段一注意事项第 1 条，值得再强调一次机制：API 校验历史时，每条 `role=tool` 消息都要能用 `tool_call_id` 找到发起它的那个 tool_call。如果你把 `msg` 加工过（比如只存 Content、丢掉 ToolCalls），后面的 tool 消息就成了"孤儿"，API 直接报错。**放回历史的必须是模型原样返回的那条消息，一个字节都不能改。**

**第 6 段：终止判断（`agent.go:85-90`）**

```go
if len(msg.ToolCalls) == 0 {
	if a.OnDelta != nil {
		fmt.Println() // 流式打印后补一个换行
	}
	return msg.Content, nil
}
```

终止条件 1 的落地：没有 tool_calls = 最终答案。因为最终答案的 content 已经在流式回调里实时打完了，这里只需补个换行就返回。

**第 7 段：逐个执行工具（`agent.go:93-111`）**

```go
for _, tc := range msg.ToolCalls {
	result, err := a.registry.Call(tc.Function.Name, tc.Function.Arguments)
	if err != nil {
		// 工具失败不要把错误抛给用户 —— 把错误信息喂回给模型，
		// 它通常能换参数或换工具自我恢复。这是 agent 鲁棒性的关键一招。
		result = fmt.Sprintf("tool error: %v", err)
	}
	a.messages = append(a.messages, llm.Message{
		Role:       "tool",
		Content:    result,
		ToolCallID: tc.ID,
		Name:       tc.Function.Name,
	})
}
```

一次响应可以有多个 tool_calls（并行调用），逐个执行、每个结果单独一条 tool 消息、用各自的 `ToolCallID` 回指。全部执行完后循环进入下一轮，模型看到全部观察结果再决策。

把这七段画成时序图（对照阶段文档 3.3 节），每个箭头都能标注到上面的行号：

```
用户        CLI/Agent             LLM API           工具
 │ 输入       │                     │                │
 │──────────>│ append user (52)    │                │
 │           │──循环开始 (54)──────┐                │
 │           │ 压缩检查 (56)        │                │
 │           │ messages+tools      │                │
 │           │────────────────────>│                │
 │           │  SSE 分片:          │                │
 │           │  tool_calls 聚合     │                │
 │           │<────────────────────│                │
 │           │ append assistant (77)                │
 │           │─────────────────────────────────────>│
 │           │                执行 (98)，失败也返回文本
 │           │<─────────────────────────────────────│
 │           │ append tool 消息 (105)，进入下一轮 ───┤
 │           │ messages+tools（含观察）              │
 │           │────────────────────>│                │
 │  流式打出  │  SSE 分片: content  │                │
 │<──────────│<────────────────────│                │
 │           │ 无 tool_calls，return (85-89)         │
```

### 2.2 为什么"工具失败把错误喂回模型"是鲁棒性关键一招

`agent.go:98-103` 这三行值得单独讲，因为它是 Agent 与传统程序错误处理哲学差异最大的地方。

传统程序里，错误处理的目标是**终止或上抛**；Agent 里，工具错误的第一选择是**变成观察，喂回模型**。原因是：错误文本对模型来说是有行动指导价值的信息——

- `http_fetch` 返回 403：模型可能换个 URL 重试，或退而用已有信息回答；
- `calculator` 报表达式非法：模型几乎总能修正好参数再调一次；
- 工具名拼错（模型确实会编造工具名）：`Registry.Call` 返回"未知工具"错误，模型看到后会改用真实存在的工具名。

对比循环里另一种错误的处理方式——LLM API 调用失败时（`agent.go:64`）直接 `return "", err` 抛给用户。两种处理的区分标准很清楚：

- **工具错误**：模型有能力根据错误信息调整行为 → 喂回，给自我恢复的机会；
- **API 错误**：模型看不到也管不了传输层的事 → 上抛，由外层（重试逻辑或用户）处理。

一个生产注意：喂回的错误文本也会进入历史、成为后续所有请求的上下文，它同样是**不可信内容**（第 1 章进阶 3.1 的注入面）。错误信息要截断、不要把堆栈或内部路径无过滤地塞进去。

### 2.3 `compressIfNeeded`：上下文压缩的五个设计点

练习 3 的成品（`agent.go:156-199`），解决第 1 章推论二提出的问题：成本随轮次平方增长、上下文窗口迟早撞墙。策略是"超阈值时，用 LLM 把早期历史摘要成一条消息"。逐段看五个设计点。

**设计点 1：阈值与保留窗口（`agent.go:157-162`）**

```go
const threshold = 20
const keepRecent = 6

if len(a.messages) <= threshold {
	return nil
}
```

按消息条数触发：超过 20 条才压缩，且始终保留最近 6 条原样不动。保留最近几轮的意义：摘要必然有损，最近上下文是模型"当前正在做什么"的直接依据，压掉它会导致任务断片。20/6 是经验值——更精确的做法是按 token 数触发（用 `usage.PromptTokens` 逼近窗口水位），这是阶段三的改进方向。

**设计点 2：切分点后移，避开孤儿 tool 消息（`agent.go:164-169`）**

```go
split := len(a.messages) - keepRecent

// 切分点不能在 tool
for split < len(a.messages) && a.messages[split].Role == "tool" {
	split++
}
```

切分点 `split` 左侧的被摘要、右侧的保留。如果 `split` 恰好落在一条 `role=tool` 消息上，意味着这条 tool 消息被保留了、而发起它的 assistant tool_calls 消息被压进了摘要——保留区出现一条找不到"发起者"的孤儿 tool 消息，API 直接报错。所以切分点要向后滑过整组连续的 tool 消息。

`split < len(a.messages)` 这个上界条件防的是数组越界 panic。但阶段一注意事项第 15 条记录了一个更深的边界：如果保留区**整组都是 tool 消息**（上一轮有大量并行工具调用时会发生），split 会后移到末尾，等于把最近上下文全部压掉——这违背了 keepRecent 的初衷。更稳妥的做法是上界处直接放弃本轮压缩（`if split >= len(a.messages) { return nil }`），等下一轮保留区出现非 tool 消息再压。当前实现选择了简单写法，知道这个边界并能说出改进方案，面试时就是加分点。

**设计点 3：摘要输入必须带上 tool_calls（`agent.go:171-179`）**

```go
for _, m := range a.messages[1:split] {
	fmt.Fprintf(&sb, "[%s]:%s\n", m.Role, m.Content)

	for _, tc := range m.ToolCalls {
		fmt.Fprintf(&sb, "[%s 调用工具]: %s(%s)\n", m.Role, tc.Function.Name, tc.Function.Arguments)
	}
}
```

容易漏的点：assistant 决定调工具时，**Content 往往为空**，调用信息全在 `ToolCalls` 里。如果只渲染 `m.Content`，摘要模型看到的早期历史里，所有 assistant 消息都是空的，"做过什么、查到过什么"全部丢失，摘要质量崩塌。所以渲染摘要输入时要显式把每次工具调用的名字和参数也写进去。

**设计点 4：摘要调用走非流式 `Chat`（`agent.go:181-184`）**

```go
resp, err := a.client.Chat([]llm.Message{
	{Role: "system", Content: summaryPrompt},
	{Role: "user", Content: sb.String()},
}, nil)
```

注意这是一次**独立的、不带工具的** LLM 调用：独立的 messages（不复用对话历史，`a.messages[0]` 的主 system prompt 也不参与），`tools=nil`（摘要不需要也不应该触发工具）。摘要是内部辅助调用，结果要等全文拿到再替换历史，用非流式最简单——流式重试的麻烦语义（进阶 3.3）在这里完全不必引入。

**设计点 5：摘要以 system 角色回插（`agent.go:189-198`）**

```go
summary := llm.Message{
	Role:    "system",
	Content: "【早期对话摘要】" + resp.Choices[0].Message.Content,
}

compressed := make([]llm.Message, 0, keepRecent+2)
compressed = append(compressed, a.messages[0], summary)
compressed = append(compressed, a.messages[split:]...)
a.messages = compressed
```

新历史 = 原 system prompt + 摘要消息 + 保留区，三段拼接。摘要用 `system` 角色而不是 `user`：这段文字不是用户说的，是系统对早期历史的陈述；用 user 角色会让模型误以为"用户又说了一段话"，可能直接去回复摘要本身。带一个显式标记前缀（`【早期对话摘要】`）也是工程习惯：让模型知道这段内容的性质，别当成新指令。

一个与第 1 章进阶 3.2 联动的代价要知道：压缩改写了历史前缀，下一轮请求的缓存前缀失效——压缩省 token 和缓存折扣之间有取舍，量大时值得实测。

### 2.4 Usage 累计与暴露：先计量，再谈成本控制

`agent.go:71-73` 在循环里逐轮累计，`Usage()`（`agent.go:126-128`）把累计值暴露给上层。这两处的注释写明了动机：阶段三的编排器要按子任务核算成本（预算熔断、模型分级的收益量化），**内核不暴露用量，上层只能瞎估**。"没有计量就没有成本控制"——这是从第 1 章就埋下的纪律。

这里必须诚实指出一个当前实现的边界：流式路径下这个计量是**待补全**的。看 `ChatStream` 的返回构造（`client.go:196-199`），聚合结果只填了 `Choices`，没有填 `Usage`——OpenAI 兼容协议下，流式响应默认不回传 usage，需要在请求里加 `stream_options: {"include_usage": true}`，服务端才会在流末尾单独发一个带 usage 的 chunk（具体行为以官方文档为准）。本项目当前没有开这个选项，所以流式主循环里 `Usage()` 累计的实际是零值。

这个边界不妨碍设计的正确性：**接口语义先就位**（循环每轮累计、对外暴露），数据来源后补（开 `include_usage` 聚合末尾 chunk，或关键计量走非流式调用）——阶段三做成本归因前需要把这块接上，面试时主动指出这一点，说明你真的读过自己写的代码。

---

## 三、进阶拓展（带代码）

### 3.1 SSE 流式原理与 ChatStream 聚合

**为什么要有流式**：非流式请求要等模型生成完所有 token 才一次性返回，长回答的首字延迟可能十几秒；流式（SSE，Server-Sent Events）让服务端逐 token 推送，首字延迟降到一秒内。对 CLI/聊天产品，这是体验刚需。

**SSE 协议格式**只有三条规则，用一段模拟字节流就能看清：

```go
// sse-demo: 用一段模拟的 SSE 字节流演示协议格式与解析骨架。
package main

import (
	"bufio"
	"fmt"
	"strings"
)

// 模拟服务端发来的原始字节流。SSE 的协议规则只有三条：
//  1. 每个事件以 "data: " 前缀开头，空行表示一个事件结束；
//  2. 以 ":" 开头的是注释行（服务端常用来发 keep-alive 心跳）；
//  3. "data: [DONE]" 是 OpenAI 兼容协议的流结束标记。
const fakeStream = ": keep-alive\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"357\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\" × 482\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\" = 172074\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

func main() {
	scanner := bufio.NewScanner(strings.NewReader(fakeStream))
	var answer strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // 空行与注释行都不是数据，跳过
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		// 真实代码里这一步是 json.Unmarshal 进 streamChunk，
		// 再把 delta.content 追加进聚合缓冲区（见 client.go ChatStream）。
		fmt.Printf("收到一个事件: %s\n", data)
		answer.WriteString(data)
	}
	fmt.Println("流结束，聚合缓冲区已拼出完整响应骨架")
}
```

**delta 与 message 的区别**：非流式响应的结果在 `choices[0].message`（完整的一条消息）；流式每个 chunk 的结果在 `choices[0].delta`（增量，通常只带 content 的一小段或 tool_calls 的一个分片）。`streamChunk` 的定义（`types.go:81-90`）就是照着这个差异写的——外层 tag 是 `json:"delta"`，写成 `message` 或 `content` 会静默解析不到任何东西（见常见坑 1）。

**tool_calls 分片聚合**是流式最硬核的部分（`types.go:92-100` 的注释是面试高频素材）：

- 第一个分片带 `index` / `id` / `function.name`，后续分片往往**只有 `index` 和 `function.arguments` 的一小段**；
- `arguments` 本身是"一段 JSON 文本"，被拆成多片传输，必须按 `index` 把字符串拼完整后才能 `json.Unmarshal`；
- 一次响应有多个并行 tool_calls 时，分片是交错到达的，靠 `index` 归组。

聚合代码（`client.go:173-186`）的处理：`id`/`type` 只在首片出现所以用 `if != ""` 赋值，`name` 和 `arguments` 一律 `+=` 拼接（注释里特别说明：name 理论上也可能分片，`+=` 最稳）。

**由此推出本章最重要的一句话：流式只能提前展示，不能提前决策。** content 可以边收边打印（那是给用户看的），但"是否调工具、调什么"必须等流结束、tool_calls 拼完整之后才能判断——半段 arguments 既不是合法 JSON，也无法保证模型不会在后续分片里"改主意"。

**为什么流式与非流式返回同构的 `*ChatResponse` 是好设计**（`client.go:101-103` 注释）：agent 循环需要的不只是文本，还有 tool_calls 和 finish_reason。让 `ChatStream` 聚合后返回与 `Chat` 完全相同的类型，`Run` 里就**不需要为流式写第二套分支**——`agent.go:63` 换一个方法名就完成了切换，循环体一行不改。这也是练习 1 的基础版（只流式最终答案、循环内仍用非流式）不被推荐的原因：那种写法要在循环内外各维护一套请求逻辑，且接不进 ReAct 主循环。

生产注意两点：`bufio.Scanner` 默认 64KB 行上限对长分片不够，要放宽（`client.go:138-139` 放宽到 1MB）；单片 JSON 解析失败时当前策略是跳过（`client.go:156-158`），生产环境至少要记日志，静默吞错会掩盖协议变更。

### 3.2 重试与指数退避：错误分类决定控制流

LLM API 是网络服务，限流（429）和临时服务端故障（5xx）是常态，没有重试的 Agent 在生产环境活不过一天。但**乱重试比不重试更糟**：对 401 重试 3 次只是浪费 7 秒钟后得到同样的失败。所以重试的第一性问题不是"怎么等"，而是"哪些错误值得重试"。

**错误分类**（`client.go:219-226`）：

```go
func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}

	return true
}
```

- `429`（限流）/ `5xx`（服务端临时故障）：值得重试；
- 其他 `4xx`（401 鉴权失败、400 参数错误）：重试也是同样结果，直接失败；
- 无法识别的错误（网络层错误居多）：默认重试，保守策略。注释里坦白了这个默认分支的取舍——本地逻辑错误（如 marshal 失败）其实重试也必然失败，归进"可重试"是图省事；要严格应再加一类"本地错误不重试"。面试被追问"哪些错误不该重试"时，能补出这一层是加分点。

**这个分类能工作的前提是 `APIError` 必须经 `errors.As` 取得出来**。`Chat` 的非 200 分支（`client.go:75-80`）返回的是 `&APIError{StatusCode, Body}` 而不是 `fmt.Errorf`——这是修过的真实 bug：如果这里返回普通 error，`errors.As` 取不到状态码，**所有错误都落入默认分支被重试，401 也不例外，而且没有任何报错提示**。"错误类型是控制流的一部分"，接线漏接则分类静默失效。

**退避节奏**（`client.go:241-246`）：`backoff := time.Second << (attempt - 1)`，每次左移一位翻倍，1s → 2s → 4s。循环条件是 `attempt <= maxRetries`（总尝试 = 1 次首发 + maxRetries 次重试）——写成 `attempt < maxRetries` 或 `range maxRetries` 就是 off-by-one：maxRetries=3 时实际只重试 2 次，4s 那一档永远走不到（常见坑 5）。

**教学代码：带 jitter 的退避**。指数退避还有一个经典缺陷——惊群效应（thundering herd）：服务恢复的瞬间，所有被限流的客户端按同样的 1s/2s/4s 节奏同时重试，再次把服务打垮。解法是加随机抖动，错开重试时刻：

```go
// backoffWithJitter 计算第 attempt 次重试（attempt 从 1 开始）的等待时长。
//
// 三个要点：
//   - 指数部分 1s、2s、4s……每次左移一位翻倍，与 ChatWithRetry 一致；
//   - 封顶 30s：指数增长不设上限，第 10 次重试就要等 8 分钟，不合理；
//   - 抖动：在 [0, base/2) 区间加一个随机量，让同一时刻被限流的
//     多个客户端错开重试时刻，避免"惊群"——阶段三并发场景会复用。
func backoffWithJitter(attempt int) time.Duration {
	base := time.Second << (attempt - 1)
	const maxBackoff = 30 * time.Second
	if base > maxBackoff {
		base = maxBackoff
	}
	jitter := time.Duration(rand.Int64N(int64(base) / 2))
	return base + jitter
}
```

实际输出（每次运行不同）：1.04s → 2.14s → 4.89s → 9.73s → 18.70s → 37.8s。阶段三多个 worker 并发调 API 时会复用这个函数——单客户端看不出 jitter 的价值，并发场景它就是必需品。

生产注意再补两条：重试的**总时限**要有预算（三次退避 7 秒 + 每次请求最长 120 秒，一次用户提问最坏等 6 分钟——是否真的可接受，要按产品形态定）；chat completions 是只读调用所以重试安全，但对**有副作用的 API**（扣费、下单类），重试必须配幂等键，否则可能重复执行。

### 3.3 流式重试的额外语义：已打出的增量收不回

把重试直接包到 `ChatStream` 上，会出现一个非流式没有的问题：**onDelta 已经把上一段失败的增量打印给用户了，重试时模型会从头重新生成，同一句话在终端打两遍**。HTTP 层可以重试，用户的屏幕不能"撤回"。

两种典型场景的策略：

```go
// 场景 A：CLI 本地打印（本项目形态）——增量还在本地，可以"清场重打"
onDelta := func(text string) {
	fmt.Print(text)
}
// 重试前：打印 "[连接中断，重试中...]"，并把已打出的内容视为作废；
// 若终端支持，可用 ANSI 转义序列清掉已打行，让重试输出从干净状态开始。

// 场景 B：服务端把增量转发给下游（第 7、13 章的 SSE 网关形态）——
// 已发给浏览器的增量收不回，只能整个流作废：
// 给下游发一个 error 事件，让前端丢弃本次响应、等重试后的新流。
```

策略取舍的本质：**增量一旦离开你的进程边界，就不可回收**。所以工程惯例是"展示归展示、缓冲归缓冲"——进程内始终维护完整聚合缓冲，展示通道失败可以随时作废重打。

理解了这层语义，就明白本项目的一个现实选择：`ChatWithRetry` 目前只包非流式 `Chat`（`client.go:235-237` 的注释明说了这一点），主循环的 `ChatStream` 暂未接重试——不是忘了，而是**先看清语义、设计好策略，再接线**。这也正是练习 2 参考答案里"基础版只重试非流式"的局限说明。面试被问"你的重试怎么做的"，主动说出"流式路径的重试需要处理增量重复，我把它留作显式设计决策"，比含糊带过强得多。

### 3.4 给 Agent 内核写单元测试：脚本化假 LLM

面试高频题："你的 agent 怎么做单元测试？"先如实交代现状：mini-agent 里 embed/rag/memory 都有离线测试（fakeEmbedder、httptest 假服务器），唯独 `Run` 本体没有——因为 `Agent` 依赖具体类型 `*llm.Client`，假模型插不进来。这是教学项目的简化，但被问到时你要有现成答案：**把"调模型"这一步抽象成最小接口，再喂一个按脚本回答的假模型**。

两步模式（依赖倒置，和第 10 章 Planner/Worker/Critic 接口注入是同一手法）：

1. 定义只含 `Chat` 的最小接口，`Agent` 依赖接口而非具体 client——生产环境用 DeepSeek client，测试用假模型，二者满足同一契约；
2. 假模型按"脚本"逐轮播放预定响应：第 1 轮返回 `tool_calls`，第 2 轮返回终稿。确定性、零成本、离线可跑。

下面是一份完整可运行的教学实现（`go test` 实测通过），循环与 `agent.go` 的 `Run` 同构：

```go
package agent

import (
	"errors"
	"testing"
)

// —— 被测对象：最小 ReAct 循环（与 mini-agent 的 Run 同构）——

type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

type ToolCall struct {
	ID   string
	Name string
	Args string
}

// ChatModel 把"调模型"抽象成接口：生产是 DeepSeek client，测试是脚本假模型。
type ChatModel interface {
	Chat(msgs []Message) (Message, error)
}

type Agent struct {
	model  ChatModel
	tools  map[string]func(args string) (string, error)
	maxRun int
}

func (a *Agent) Run(msgs []Message) ([]Message, error) {
	for range a.maxRun {
		resp, err := a.model.Chat(msgs)
		if err != nil {
			return msgs, err
		}
		msgs = append(msgs, resp)
		if len(resp.ToolCalls) == 0 {
			return msgs, nil // 无工具调用 = 终局
		}
		for _, tc := range resp.ToolCalls {
			out, err := a.tools[tc.Name](tc.Args)
			if err != nil {
				out = "error: " + err.Error() // 工具失败：错误回喂，不中断
			}
			msgs = append(msgs, Message{Role: "tool", Content: out, ToolCallID: tc.ID})
		}
	}
	return msgs, errors.New("达到最大轮数")
}

// —— 脚本化假模型：按调用次序播放预定响应 ——

type scriptedModel struct {
	steps []Message
	calls int
}

func (m *scriptedModel) Chat(_ []Message) (Message, error) {
	m.calls++
	if len(m.steps) == 0 {
		return Message{}, errors.New("script exhausted")
	}
	step := m.steps[0]
	m.steps = m.steps[1:]
	return step, nil
}
```

配套的表驱动测试（三个用例各钉住一条核心语义）：

```go
func TestAgentRunsToolThenAnswers(t *testing.T) {
	fake := &scriptedModel{steps: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "now", Args: "{}"}}},
		{Role: "assistant", Content: "现在是 20 点"},
	}}
	ag := &Agent{model: fake, maxRun: 8, tools: map[string]func(string) (string, error){
		"now": func(string) (string, error) { return "20:00", nil },
	}}
	msgs, err := ag.Run([]Message{{Role: "user", Content: "几点了"}})
	if err != nil {
		t.Fatal(err)
	}
	// 钉消息演化序列：user → assistant(tool_calls) → tool → assistant(终稿)
	wantRoles := []string{"user", "assistant", "tool", "assistant"}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("len(msgs)=%d want %d", len(msgs), len(wantRoles))
	}
	for i, r := range wantRoles {
		if msgs[i].Role != r {
			t.Errorf("msgs[%d].Role=%s want %s", i, msgs[i].Role, r)
		}
	}
	if msgs[2].ToolCallID != "c1" { // 钉 tool_call_id 回挂
		t.Errorf("tool 消息必须回挂 tool_call_id，got %q", msgs[2].ToolCallID)
	}
	if fake.calls != 2 {
		t.Errorf("模型应被调用 2 次，实际 %d 次", fake.calls)
	}
}

func TestAgentToolErrorIsFedBack(t *testing.T) {
	fake := &scriptedModel{steps: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "boom", Args: "{}"}}},
		{Role: "assistant", Content: "工具挂了，我换个思路"},
	}}
	ag := &Agent{model: fake, maxRun: 8, tools: map[string]func(string) (string, error){
		"boom": func(string) (string, error) { return "", errors.New("kaboom") },
	}}
	msgs, err := ag.Run([]Message{{Role: "user", Content: "试"}})
	if err != nil {
		t.Fatal(err)
	}
	if msgs[2].Role != "tool" || msgs[2].Content != "error: kaboom" {
		t.Errorf("工具错误应回喂为 tool 消息，got %+v", msgs[2])
	}
}

func TestAgentMaxRounds(t *testing.T) {
	fake := &scriptedModel{steps: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "now", Args: "{}"}}},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "now", Args: "{}"}}},
	}}
	ag := &Agent{model: fake, maxRun: 2, tools: map[string]func(string) (string, error){
		"now": func(string) (string, error) { return "20:00", nil },
	}}
	if _, err := ag.Run([]Message{{Role: "user", Content: "几点"}}); err == nil {
		t.Fatal("超过最大轮数应返回错误")
	}
}
```

进阶两个方向：一是**契约测试**——fake 和真实 client 实现同一接口，但"真实 API 返回的 message 长什么样"这个假设要用 httptest 钉住（第 4 章 §2.2 的手法），否则 fake 全绿、真实环境翻车；二是**录制回放**——把真实 API 响应录成脚本文件让 fake 播放（VCR 模式），是介于纯假模型与真实调用之间的信心阶梯。想在真实 mini-agent 上动手完成这次"接口化 + 补测试"，正是附录 B 结课项目的 P3 部分。

---

## 四、面试视角

> 以下每题给"标准回答 → 常见追问 → 加分点"。自测方法：不看回答口述，再对照差距。本章题目与阶段文档 3.1 节 Q2/Q3/Q4 对应，但这里的回答按"能扛住三轮追问"的标准展开。

**Q1：ReAct 循环的终止条件有哪些？**

标准回答：五种，按"防什么"分类——① 模型返回无 tool_calls，任务完成，正常终止；② MaxSteps 保险丝，防逻辑死循环（模型反复调工具停不下来，死循环就是烧钱循环）；③ 不可恢复错误（如 401），继续循环无意义，直接上抛；④ 超时熔断，防单次调用卡死；⑤ 成本预算，累计 token 超预算就熔断，防账单失控。

追问链：
- "MaxSteps 和超时有什么区别？" → MaxSteps 防的是**循环次数**失控（每轮都正常返回但永远不停），超时防的是**单次调用**卡死；两者正交，都要。
- "预算熔断的前提是什么？" → 循环里每轮累计 usage 并暴露接口；没有计量就没有熔断的依据（本项目 `agent.go:71-73` + `Usage()`，阶段三落地熔断）。

加分点：指出①是唯一"好"出口，②-⑤都是"异常退出"，生产系统要分别打点统计——异常出口占比高说明工具或 prompt 有问题，这是可观测性的第一层指标。

**Q2：agent 循环里能用流式吗？怎么用？**

标准回答：能，但要守住一个不变量——**做决策（是否调工具）之前必须拿到完整的 tool_calls**。两种合规做法：① 循环内非流式，只在最终答案环节流式（简单，但要在循环内外写两套逻辑）；② 循环内也流式，客户端把 `delta.tool_calls` 分片按 `index` 聚合成完整结果再判断——本项目做法（`ChatStream`，`client.go:104`），好处是最终答案的流式体验直接在循环内获得，且流式/非流式返回同构的 `*ChatResponse`，循环体零改动。绝对不行的是边收流边决策：半段 arguments 不是合法 JSON，分片没拼完就 Unmarshal 必崩。

追问链：
- "tool_calls 分片长什么样？" → 首片带 `index`/`id`/`function.name`，后续分片只有 `index` + arguments 的一小段；多个并行调用的分片交错到达，靠 `index` 归组；`name` 也可能分片，所以拼接一律用 `+=`。
- "流式拿不到 usage 怎么办？" → 协议默认流式末尾不回传 usage，需请求加 `stream_options: {"include_usage": true}` 后在末尾 chunk 聚合（以官方文档为准）；计量关键路径也可以走非流式。

加分点：主动说"流式只能提前展示、不能提前决策"，并补一句流式重试的增量重复问题（进阶 3.3）——这说明你考虑过流式进生产，而不只是跑通过 demo。

**Q3：对话历史无限增长怎么办？**

标准回答：三板斧——① 滑动窗口，只保留最近 N 轮，简单粗暴但会丢早期信息；② 摘要压缩，超阈值时让 LLM 把早期历史总结成一条消息替换原文（本项目 `compressIfNeeded`，保留 system + 最近 6 条原样）；③ 重要事实抽取到结构化存储（长期 memory，按检索召回，第 6 章展开）。

追问链：
- "怎么选？" → 短会话/无状态工具型任务用滑窗就够；长对话且早期信息有后续价值（用户偏好、项目背景）用摘要；需要跨会话记忆、可精确召回的事实（姓名、ID、配置）用结构化抽取。真实系统往往三层叠加：滑窗兜底窗口上限，摘要保中期上下文，memory 保长期事实。
- "压缩的时机？" → 每轮循环开头、请求之前主动检查；不要等 API 报 length 错误再补救——那一轮请求已浪费，流式场景更麻烦。
- "压缩有什么隐性成本？" → 摘要本身是一次额外 LLM 调用（成本）；摘要是有损的（信息丢失风险）；压缩改写历史前缀会使上下文缓存 miss（第 1 章进阶 3.2 的取舍）。

加分点：指出按消息条数触发是简化做法，更精确的是按 token 水位（用 usage 逼近窗口）触发；以及摘要输入必须渲染 tool_calls，否则 assistant 的空调用消息会让摘要丢失"做过什么"。

**Q4：手画 ReAct 循环的时序图。**

标准回答：能画出 2.1 节那张图的核心骨架——用户输入入队 → 带全量历史请求 LLM → 返回 assistant 消息（含 tool_calls）→ **assistant 消息原样放回历史** → 逐个执行工具 → 每个结果一条 `role=tool` 消息（`tool_call_id` 回指）→ 带更新后的全量历史再请求 → 无 tool_calls 时返回最终答案。

面试官在这题上看三件事，逐一对应加分点：

- **assistant 的 tool_calls 消息和 tool 消息是否成对出现**——只画 tool 消息不画 assistant 放回，是"背过流程没写过代码"的典型破绽；
- **每轮请求是否带全量历史**——能顺手标出"所以成本 O(n²)、所以要做压缩"，直接串联 Q3；
- **工具失败的路径怎么画**——画成"错误文本照样以 tool 消息回喂，模型自我恢复"，而不是画成中断（2.2 节）。

**Q5：上下文压缩的切分点为什么要避开 tool 消息？**

标准回答：API 校验历史时，每条 `role=tool` 消息必须能用 `tool_call_id` 找到发起它的 assistant tool_calls 消息。切分点若落在 tool 消息上，这条 tool 消息被保留而它的"发起者"被压进摘要——保留区出现孤儿 tool 消息，API 直接报错。所以切分点要向后滑过整组连续 tool 消息（`agent.go:167-169`）。

追问链：
- "后移有没有边界问题？" → 有。保留区若整组是 tool 消息（上一轮大量并行调用时），split 会后移到末尾，把最近上下文全压掉，违背 keepRecent 初衷；稳妥做法是到上界就放弃本轮压缩，等下一轮再压（阶段一注意事项 15 记录的边界）。
- "摘要本身用什么角色回插？" → system，并加显式标记（如"【早期对话摘要】"）；用 user 会让模型误以为用户新说了一段话，可能直接去回复摘要。

加分点：把这题和"assistant 消息原样放回"（2.1 第 5 段）串成同一条纪律——**tool_calls 与 tool 消息的配对是协议级不变量，写入、保留、压缩三个环节都不能破坏它**。

---

## 五、常见坑

以下七条对应阶段一注意事项 9-15，全部是练习 1-3 实踩过的坑。每条讲清根因——知其所以然，下次才会在写代码的那一刻就避开。

1. **SSE 的 delta tag 写错会静默失败**（注意事项 9）。`streamChunk` 外层字段必须是 `json:"delta"`（`types.go:87`）。写成 `content` 或 `message` 时，`json.Unmarshal` **不报错**——它只是找不到对应字段，于是每个 chunk 解析出来都是空结构体，流式表现为"能跑、不报错、啥也不输出"。根因：Go 的 json 包默认忽略未知字段，协议字段名错了不会有人提醒你。防御：新协议结构体写完，先拿一段真实响应（curl 抓的）做单测或打印核对。
2. **分片未聚合就当 JSON 解析**（注意事项 10）。流式 tool_calls 的 arguments 是"被拆成多片的 JSON 字符串"，任意单片都大概率不是合法 JSON，边收边 Unmarshal 必崩；即使某片恰好合法，内容也是不完整的。根因：把"传输层分片"误当"语义层消息"——分片边界由网络缓冲决定，与 JSON 结构毫无关系。防御：牢记"按 index 拼完再解析"，`client.go:173-186` 是唯一正确的消费姿势。
3. **绕过 `Run` 直接调 `ChatStream`**（注意事项 11，练习 1 实踩）。为了"先做流式 demo"，在 main 里直接调 `ChatStream` 拿增量打印——结果用户输入没进 `messages`、工具永远不会触发、多轮对话失忆。根因：流式是**传输方式**，不是独立于循环的另一条链路；历史维护、终止判断、工具执行全在 `Run` 里，绕过它等于绕过了 Agent 本身。正确姿势：`OnDelta` 回调注入循环（`agent.go:63`），展示层与决策层各归其位。
4. **`Stream bool` 的 json tag 丢失会静默失效**（注意事项 12）。少了 `` `json:"stream"` `` 会序列化成 `"Stream": true`，服务端忽略这个未知字段，按非流式处理——然后你的 SSE 解析循环对一整段普通 JSON 扫不出任何 `data: ` 行。与坑 1 同根因（Go json 对未知/多余字段的双向静默），表现却是"不流式也不报错"，排查时容易先怀疑网络和鉴权绕一大圈。
5. **重试循环的 off-by-one**（注意事项 13）。`for attempt := range maxRetries` 总共只有 maxRetries 次尝试（1 首发 + 2 重试），4s 那一档永远走不到；"最多重试 3 次"要写 `attempt <= maxRetries`（`client.go:241`）。根因：把"重试次数"和"总尝试次数"混为一谈。防御：写注释明确"总尝试 = 1 + maxRetries"，并把三档退避时间写进测试断言。
6. **错误分类的"接线"漏接会静默失效**（注意事项 14）。`retryable` 靠 `errors.As` 取 `*APIError` 的状态码（`client.go:220-221`）；如果 `Chat` 的非 200 分支返回的是 `fmt.Errorf` 普通错误，`errors.As` 永远取不到，所有错误落入默认重试分支——401 也被重试 3 次，**全程没有任何报错**。根因：错误分类是跨函数约定，编译器检查不了"你有没有按约定返回错误类型"。防御：给 `retryable` 写单测——构造 401 的 `APIError` 断言不重试、构造普通错误断言走默认分支，把约定钉死在测试里。
7. **压缩切分点后移要有上界防护**（注意事项 15）。`split < len(a.messages)` 防住了越界 panic，但防不住"保留区整组是 tool 消息时 split 一路滑到末尾、最近上下文全被压掉"的语义错误；更稳的做法是触上界就放弃本轮压缩。根因：边界条件既包括"不崩"（数组不越界），也包括"不做蠢事"（保留窗口不被掏空）——前者类型系统和下标检查能帮你，后者只能靠场景推演。另一个配套细节：摘要输入要带 tool_calls 渲染（`agent.go:176-178`），否则 assistant 的空调用消息让摘要丢失全部"做过什么"。
8. **调试 `fmt.Println` 残留会污染输出通道**（本条是仓库现状，不对应注意事项）。`agent.go:104`（"这是工具的执行结果"）、`agent.go:114`（"agent end"）、`agent.go:193`（"压缩对话结束"）是开发期随手加的调试打印，跟着主流程一起输出——CLI 里只是难看；一旦 stdout 兼任协议信道（第 12 章 MCP 的 stdio 传输），这类打印会直接损坏协议帧。根因：调试输出没有走独立通道。防御：日志一律走 `log` 包或 stderr，提交前 `grep -rn "fmt.Print"` 扫一遍。

---

## 六、动手练习

阶段一的练习 1-3（SSE 流式、重试退避、上下文压缩）已经完成并合入项目代码——本章精讲的 `ChatStream`、`ChatWithRetry`、`compressIfNeeded` 就是它们的成果。所以本章的练习形式改为**复盘与实验**，目标是把这些代码从"写过"变成"能脱稿讲、能随手改"：

1. **重画时序图**：不看阶段文档，对照 `agent.go:51-117` 画出一次两步工具调用的完整时序图，要求每个箭头标注对应代码行号、每条消息标注 role；画完与 2.1 节的图对照，差异处就是理解缺口。
2. **流式对照实验**：把 `main.go:83` 的 `ag.OnDelta = ...` 一行注释掉，重跑 CLI 问同一个长问题（如"介绍一下 ReAct 模式"），感受首字延迟差异；再恢复。然后追问自己一个问题：注释掉 OnDelta 后，tool_calls 的聚合还在正常工作吗？为什么？（提示：`ChatStream` 的聚合与回调是两条独立路径。）
3. **压缩触发实验**：写个小脚本（或手工）连续发 30+ 轮消息触发压缩，观察 `[compress]` 输出，然后问模型"我第 2 轮告诉你的名字是什么"——验证摘要是否保住了早期事实。进阶：故意在压缩前安排一轮工具调用，观察切分点后移逻辑是否生效（可临时加一行打印 `split` 的值）。

自评参考答案（完成实验后再看）：`docs/solutions/stage-01/exercise-1-sse-streaming.md`、`exercise-2-retry-backoff.md`、`exercise-3-context-compression.md`。重点看每份答案的"关键设计点"一节——流式的增量重复语义、退避的默认分支取舍、压缩的上界边界，正是本章进阶拓展对应的深挖方向。

---

## 本章小结

- ReAct = 思考-行动-观察交替，直到模型认为任务完成；**Agent = LLM + 工具 + 循环，循环是本体，框架是包装**。
- 终止条件五种：无 tool_calls（正常）、MaxSteps（防死循环）、不可恢复错误（防无意义重试）、超时（防卡死）、预算（防账单失控）——按"防什么"记忆。
- Agent 的全部可变状态就是 `messages` 切片：持久化 = 序列化它，调试 = dump 它。
- 工具错误喂回模型自我恢复，API 错误上抛——区分标准是"错误信息对模型有没有行动指导价值"。
- 流式只能提前展示、不能提前决策；tool_calls 分片按 index 聚合完整后才能 Unmarshal；流式/非流式同构 `*ChatResponse` 让循环零改动接入。
- 重试的第一性问题是错误分类（429/5xx 才重试），`APIError` 必须经 `errors.As` 可取，退避加 jitter 防惊群；流式重试要额外处理增量重复。
- 压缩三纪律：切分点避开孤儿 tool 消息（并有上界防护）、摘要输入带 tool_calls、摘要以 system 角色回插。

下一章：[第 4 章：Embedding 与向量检索](04-embedding-and-vector-search.md)——让 Agent 拥有"见过"私有知识的能力，从把文本变成向量开始。
