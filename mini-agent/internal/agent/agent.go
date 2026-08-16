// Package agent 实现 agent 的核心：ReAct 循环。
//
// 所谓 agent，本质就是一个循环：
//
//	for {
//	    resp := LLM(messages, tools)
//	    if resp 没有 tool_calls { return resp.Content }   // 模型认为任务完成
//	    for each tool_call { result := 执行工具; messages = append(messages, result) }
//	}
//
// 理解了这个循环，就理解了市面上所有 agent 框架 80% 的内核。
package agent

import (
	"fmt"
	"mini-agent/internal/llm"
	"mini-agent/internal/tools"
	"strings"
)

// Agent 持有一次任务的全部状态。
type Agent struct {
	client   *llm.Client
	registry *tools.Registry
	messages []llm.Message // 完整的对话历史，每轮循环都会增长

	MaxSteps int // 防止死循环的保险丝：模型可能反复调工具停不下来
	Verbose  bool

	// usage 累计本次任务所有 LLM 调用的 token 用量。
	// 阶段三的编排器需要按子任务核算成本（预算熔断、模型分级的收益量化），
	// 所以内核必须把用量暴露出来——否则上层只能瞎估。
	usage llm.Usage

	// OnDelta 若设置，模型生成的文本增量会实时回调（用于流式打印）。
	// 注意：只有模型在写最终回答时才有 content 增量；当它决定调工具时，
	// 流式分片里是 tool_calls 而非 content，不会触发这个回调。
	OnDelta func(text string)
}

func New(client *llm.Client, registry *tools.Registry, systemPrompt string) *Agent {
	return &Agent{
		client:   client,
		registry: registry,
		messages: []llm.Message{{Role: "system", Content: systemPrompt}},
		MaxSteps: 10,
	}
}

// Run 执行一次用户请求，返回最终文本回答。
func (a *Agent) Run(userInput string) (string, error) {
	a.messages = append(a.messages, llm.Message{Role: "user", Content: userInput})

	for step := 0; step < a.MaxSteps; step++ {

		if err := a.compressIfNeeded(); err != nil && a.Verbose {
			fmt.Printf("[compress] 失败（忽略，继续对话）： %v\n", err)
		}

		// 用流式请求替代非流式：返回结构相同（*llm.ChatResponse），
		// 但 content 增量会实时通过 OnDelta 打出。tool_calls 由
		// ChatStream 内部按 index 聚合，这里拿到的就是完整结果。
		resp, err := a.client.ChatStream(a.messages, a.registry.Schemas(), a.OnDelta)
		if err != nil {
			return "", err
		}
		choice := resp.Choices[0]
		msg := choice.Message

		// 累计 token 用量（含流式聚合后的响应），供上层做成本核算。
		a.usage.PromptTokens += resp.Usage.PromptTokens
		a.usage.CompletionTokens += resp.Usage.CompletionTokens
		a.usage.TotalTokens += resp.Usage.TotalTokens

		// 关键：assistant 的消息（含 tool_calls）必须原样放回历史，
		// 否则后续 role=tool 的消息失去对应关系，API 会报错。
		a.messages = append(a.messages, msg)

		if a.Verbose {
			fmt.Printf("[step %d] finish_reason=%s tokens=%d\n",
				step+1, choice.FinishReason, resp.Usage.TotalTokens)
		}

		// 没有工具调用 = 模型给出了最终答案，循环结束
		if len(msg.ToolCalls) == 0 {
			if a.OnDelta != nil {
				fmt.Println() // 流式打印后补一个换行
			}
			return msg.Content, nil
		}

		// 依次执行模型请求的每个工具，结果以 role=tool 追加回历史
		for _, tc := range msg.ToolCalls {
			if a.Verbose {
				fmt.Printf("[step %d] tool_call: %s(%s)\n", step+1, tc.Function.Name, tc.Function.Arguments)
			}

			result, err := a.registry.Call(tc.Function.Name, tc.Function.Arguments)
			if err != nil {
				// 工具失败不要把错误抛给用户 —— 把错误信息喂回给模型，
				// 它通常能换参数或换工具自我恢复。这是 agent 鲁棒性的关键一招。
				result = fmt.Sprintf("tool error: %v", err)
			}
			fmt.Println("这是工具的执行结果：", result)
			a.messages = append(a.messages, llm.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
	}

	fmt.Println("agent end")

	return "", fmt.Errorf("达到最大步数 %d，任务未完成", a.MaxSteps)
}

func (a *Agent) Messages() []llm.Message {
	return a.messages
}

// Usage 返回本次任务累计的 token 用量（所有 ReAct 轮次之和）。
// 阶段三编排器用它把成本核算到每个子任务：worker 执行完一个子任务后
// 读取 Usage()，随 checkpoint 落盘，供预算熔断与"哪个子任务最贵"分析。
func (a *Agent) Usage() llm.Usage {
	return a.usage
}

// ============================ 练习区（由学习者完成） ============================
//
// TODO(练习3): 上下文压缩 —— 长会话必备能力
//
// 背景：messages 每轮都在增长，token 成本是 O(轮次²)，且迟早撞上下文窗口上限。
//
// 任务：给 Agent 增加一个 compress 方法：当 messages 超过阈值（如 20 条）时，
// 把最早的一段历史（保留 system 和最近几轮）发给 LLM 做摘要，
// 用一条摘要消息替换掉它们。e
//
// 提示：
//   - 摘要请求的 prompt 要点：要求保留"事实、结论、用户偏好、未完成任务"
//   - 切分点要避开 tool_calls 与 role=tool 的配对中间——截断处若留下
//     一条孤儿 tool 消息（前面没有对应的 assistant tool_calls），API 会报错
//   - 压缩时机：在 Run 的循环开头检查，而不是等 API 报 length 错误
//
// 验收：构造一段 30+ 轮的长对话（可以脚本批量发），观察压缩触发后
// 对话仍能正常继续，且模型"记得"早期提到的关键信息（如用户名字）。
// 参考答案：docs/solutions/stage-01/exercise-3-context-compression.md（完成后再看）

const summaryPrompt = `请把以下对话历史压缩成一段摘要，要求保留：
	1、用户透漏的事实（姓名、偏好、背景信息）
	2、已得出的结论和答案
	3、未完成的任务和待办
	用第三人称陈述，控制在300字以内。对话如下：`

func (a *Agent) compressIfNeeded() error {
	const threshold = 20
	const keepRecent = 6

	if len(a.messages) <= threshold {
		return nil
	}

	split := len(a.messages) - keepRecent

	// 切分点不能在 tool
	for split < len(a.messages) && a.messages[split].Role == "tool" {
		split++
	}

	var sb strings.Builder

	for _, m := range a.messages[1:split] {
		fmt.Fprintf(&sb, "[%s]:%s\n", m.Role, m.Content)

		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&sb, "[%s 调用工具]: %s(%s)\n", m.Role, tc.Function.Name, tc.Function.Arguments)
		}
	}

	resp, err := a.client.Chat([]llm.Message{
		{Role: "system", Content: summaryPrompt},
		{Role: "user", Content: sb.String()},
	}, nil)
	if err != nil {
		return err
	}

	summary := llm.Message{
		Role:    "system",
		Content: "【早期对话摘要】" + resp.Choices[0].Message.Content,
	}
	fmt.Println("压缩对话结束，后面的结果：", resp.Choices[0].Message.Content)

	compressed := make([]llm.Message, 0, keepRecent+2)
	compressed = append(compressed, a.messages[0], summary)
	compressed = append(compressed, a.messages[split:]...)
	a.messages = compressed
	return nil
}
