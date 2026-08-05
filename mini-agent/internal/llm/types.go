// Package llm 实现一个极简的 OpenAI 兼容聊天客户端（默认指向 DeepSeek）。
// 只覆盖 agent 开发最常用的能力：多轮消息、tool 定义、tool_calls 返回。
//
// 练习：本文件无需学习者完成的部分；练习 2（重试）在 client.go。
package llm

// Message 是对话历史中的一条消息。
// 对应 OpenAI/DeepSeek API 的 messages 数组元素。
//
// 四种角色的分工（agent 协议的骨架）：
//   - system:    给模型立规矩，整个会话一条，放最前面
//   - user:      用户输入
//   - assistant: 模型的回复；当它想调工具时，Content 可为空而 ToolCalls 非空
//   - tool:      工具的执行结果，必须通过 ToolCallID 回指 assistant 发起的调用
type Message struct {
	Role       string     `json:"role"` // system / user / assistant / tool
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant 要求调用工具时填充
	ToolCallID string     `json:"tool_call_id,omitempty"` // role=tool 时回指对应的调用
	Name       string     `json:"name,omitempty"`         // role=tool 时的工具名（可选，便于阅读）
}

// ToolCall 是模型返回的一次工具调用请求。
//
// 理解 function calling 的关键：模型本身不执行任何代码。
// 它只是在响应里填了这样一个结构："请帮我调 calculator，参数是 {...}"，
// 然后等我们的代码执行完，把结果以 role=tool 的消息喂回去，它再继续生成。
// 一次响应里可以有多个 ToolCall（并行工具调用）。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // 目前恒为 "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // 注意是 JSON 字符串而非对象，需要自行 Unmarshal
	} `json:"function"`
}

// Tool 用 JSON Schema 描述一个可供模型调用的工具。
type Tool struct {
	Type     string       `json:"type"` // 恒为 "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema
}

// ChatRequest 是发给 /chat/completions 的请求体（精简版）。
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	Stream      bool      `json:"stream"`
}

// ChatResponse 是 /chat/completions 的响应体（精简版）。
// ChatStream 聚合完成后也返回这个结构，让流式/非流式对上层（agent 循环）同构。
type ChatResponse struct {
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"` // "stop" | "tool_calls" | "length" ...
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// streamChunk 是流式（SSE）响应里单个 data 行的 JSON 结构。
// 注意和非流式的两点差异：
//   - 增量在 choices[0].delta 里，而不是 choices[0].message；
//   - delta 里通常只带 Content 或 ToolCalls 的一小段，需要逐片聚合。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []streamToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// streamToolCall 是流式响应里 tool_calls 的一个分片。
//
// 流式 + 工具调用的关键坑（面试高频追问）：
// 模型决定调工具时，tool_calls 不是一次给全的，而是分片到达——
// 第一个分片带 index/id/function.name，后续分片往往只有 index 和
// function.arguments 的一小段字符串。arguments 本身是"JSON 的字符串"，
// 被拆成多片传输，必须按 index 把字符串拼完整后才能 Unmarshal。
// 所以流式只能"提前展示"，不能"提前决策"：agent 循环必须等流结束、
// 拿到完整 tool_calls 后才能执行工具。
type streamToolCall struct {
	Index    int    `json:"index"` // 分片属于第几个 tool_call（一次响应可有多个并行调用）
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
