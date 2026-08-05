# 练习 1 参考答案：SSE 流式输出（含 tool_calls 支持）

> 对应题目：`mini-agent/internal/llm/client.go`（练习 1 已完成并扩展，TODO 已移除）
> ⚠️ 先自己实现，再对照本文档。
>
> **版本说明**：本答案已升级为"完整版"——第一版只做纯文本流式是合理的起点，
> 但那样接入时只能绕开 agent 主循环（工具调用全部失效），不能算正确答案。
> 本版与项目当前代码一致：流式聚合 tool_calls 分片，直接接入 ReAct 循环。

## 参考实现

流式响应的解析结构（`internal/llm/types.go`）：

```go
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
// 第一个分片带 index/id/function.name，后续分片往往只有 index 和
// function.arguments 的一小段字符串，必须按 index 拼完整后才能 Unmarshal。
type streamToolCall struct {
	Index    int    `json:"index"` // 分片属于第几个 tool_call（一次响应可有多个并行调用）
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
```

`ChatRequest` 需要补一个字段：

```go
Stream bool `json:"stream"`
```

`ChatStream`（`internal/llm/client.go`）：

```go
// ChatStream 发起流式对话补全（SSE），每收到一段文本增量就回调 onDelta，
// 流结束后返回聚合好的完整 ChatResponse——形态与非流式 Chat 完全一致，
// 因此 agent 循环可以无差别地使用两者。
func (c *Client) ChatStream(messages []Message, tools []Tool, onDelta func(text string)) (*ChatResponse, error) {
	reqBody := ChatRequest{
		Model:       c.model,
		Messages:    messages,
		Tools:       tools,
		Temperature: 0.3,
		Stream:      true,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(b)}
	}

	scanner := bufio.NewScanner(resp.Body)
	// 默认 64KB 的行上限对长分片可能不够，放宽到 1MB
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var content strings.Builder
	var toolCalls []ToolCall // 按下标聚合分片；len 随最大 index 增长
	finishReason := ""

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // 跳过空行、注释行（SSE 用空行分隔事件）
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 单片解析失败不致命，跳过即可（生产中可记日志）
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]

		if choice.Delta.Content != "" {
			content.WriteString(choice.Delta.Content)
			if onDelta != nil {
				onDelta(choice.Delta.Content)
			}
		}

		// tool_calls 分片聚合：按 index 定位到对应 ToolCall，
		// id/type/name 只在首个分片出现，arguments 逐片拼接。
		for _, d := range choice.Delta.ToolCalls {
			for len(toolCalls) <= d.Index {
				toolCalls = append(toolCalls, ToolCall{})
			}
			tc := &toolCalls[d.Index]
			if d.ID != "" {
				tc.ID = d.ID
			}
			if d.Type != "" {
				tc.Type = d.Type
			}
			tc.Function.Name += d.Function.Name // name 理论上也可能分片，用 += 最稳
			tc.Function.Arguments += d.Function.Arguments
		}

		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	msg := Message{Role: "assistant", Content: content.String(), ToolCalls: toolCalls}
	return &ChatResponse{
		Choices: []Choice{{Message: msg, FinishReason: finishReason}},
	}, nil
}
```

接入 agent 循环（`internal/agent/agent.go`）——**这是完整答案和"玩具版"的分水岭**：

```go
type Agent struct {
	// ...
	// OnDelta 若设置，模型生成的文本增量会实时回调（用于流式打印）。
	// 注意：只有模型在写最终回答时才有 content 增量；当它决定调工具时，
	// 流式分片里是 tool_calls 而非 content，不会触发这个回调。
	OnDelta func(text string)
}

func (a *Agent) Run(userInput string) (string, error) {
	a.messages = append(a.messages, llm.Message{Role: "user", Content: userInput})

	for step := 0; step < a.MaxSteps; step++ {
		// 流式与非流式返回同构的 *ChatResponse，循环体一行都不用改
		resp, err := a.client.ChatStream(a.messages, a.registry.Schemas(), a.OnDelta)
		if err != nil {
			return "", err
		}
		msg := resp.Choices[0].Message
		a.messages = append(a.messages, msg) // assistant 消息原样放回历史

		if len(msg.ToolCalls) == 0 {
			if a.OnDelta != nil {
				fmt.Println() // 流式打印后补一个换行
			}
			return msg.Content, nil
		}
		// ... 执行工具、追加 role=tool 消息，与非流式完全相同
	}
	// ...
}
```

main.go 中只需设置回调，不要再绕过 `ag.Run` 直接调 `ChatStream`：

```go
ag.OnDelta = func(text string) { fmt.Print(text) } // 逐字打印
// 循环里：ag.Run(input)，回答已通过 OnDelta 流式打出
```

## 关键设计点

1. **delta ≠ message**：流式响应的结构变了，增量在 `choices[0].delta.content`。外层字段的 json tag 必须是 `delta`，写成 `content` 会**静默收不到任何数据**（Unmarshal 不报错，字段全是零值）——这是本练习真实踩过的坑。
2. **`[DONE]` 哨兵**：OpenAI 兼容协议用一行 `data: [DONE]` 表示流结束，它不是 JSON，必须在 Unmarshal 之前拦截。
3. **Scanner 缓冲**：`bufio.Scanner` 默认单行上限 64KB，长事件行会直接报错；要么放宽 buffer，要么改用 `bufio.Reader.ReadString('\n')`。
4. **返回 `*ChatResponse` 而不是 `string`**：agent 循环需要的不只是文本，还有 tool_calls 和 finish_reason。让流式/非流式返回同构结构，上层（ReAct 循环）就不需要为流式写第二套分支——这是"流式能否进主循环"的关键设计。
5. **tool_calls 分片重组是核心难点**：流式下工具调用按 `delta.tool_calls` 分片到达——首片带 `index`/`id`/`function.name`，后续分片只有 `index` + `arguments` 字符串片段。必须按 index 归组、字符串拼接，拼完整后 arguments 才是合法 JSON。所以流式只能"提前展示"，不能"提前决策"：agent 循环必须等流结束、拿到完整 tool_calls 后才能执行工具。
6. **接入方式决定对错**：把 `ChatStream` 接在 `ag.Run` 外面（绕过 agent 循环）是错误接法——用户输入不会进历史、工具永远不触发、多轮失忆。正确做法是让 `Run` 内部调用 `ChatStream`，循环逻辑不变。
7. **Go struct tag 丢失会静默失效**：`Stream bool` 少了 `` `json:"stream"` `` 会序列化成 `"Stream"` 字段，服务端直接忽略，表现为"不流式也不报错"。

## 对照清单

完成后逐条自评：

- [ ] 请求体正确带了 `"stream": true`（且 `Stream` 字段有正确的 json tag）
- [ ] 用独立的 `streamChunk` 结构解析 delta，而不是复用非流式的 `ChatResponse`
- [ ] delta 外层 json tag 是 `delta`，不是 `content`
- [ ] 正确处理了 `[DONE]` 结束标志（在 JSON 解析之前判断）
- [ ] 跳过了非 `data: ` 开头的行（空行、keep-alive 注释）
- [ ] 考虑了 Scanner 单行长度上限问题
- [ ] tool_calls 分片按 index 聚合，arguments 拼接完整（问一道计算题验证工具仍被触发）
- [ ] 返回值包含完整 Message（content + tool_calls），assistant 消息能原样入历史
- [ ] 流式接在 agent 循环内部，而不是绕过 `Run`（验证：多轮对话不失忆、工具调用正常）
- [ ] 流式过程中连接中断有错误处理（`scanner.Err()`）
- [ ] 运行效果：最终回答逐字/逐段打印出来，而非一次性出现
