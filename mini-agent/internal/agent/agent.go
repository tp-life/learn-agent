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
)

// Agent 持有一次任务的全部状态。
type Agent struct {
	client   *llm.Client
	registry *tools.Registry
	messages []llm.Message // 完整的对话历史，每轮循环都会增长

	MaxSteps int // 防止死循环的保险丝：模型可能反复调工具停不下来
	Verbose  bool
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
		resp, err := a.client.Chat(a.messages, a.registry.Schemas())
		if err != nil {
			return "", err
		}
		choice := resp.Choices[0]
		msg := choice.Message

		// 关键：assistant 的消息（含 tool_calls）必须原样放回历史，
		// 否则后续 role=tool 的消息失去对应关系，API 会报错。
		a.messages = append(a.messages, msg)

		if a.Verbose {
			fmt.Printf("[step %d] finish_reason=%s tokens=%d\n",
				step+1, choice.FinishReason, resp.Usage.TotalTokens)
		}

		// 没有工具调用 = 模型给出了最终答案，循环结束
		if len(msg.ToolCalls) == 0 {
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
