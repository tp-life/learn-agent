# 练习 1 参考答案：SSE 流式输出

> 对应题目：`mini-agent/internal/llm/client.go` 末尾 TODO(练习1)
> ⚠️ 先自己实现，再对照本文档。

## 参考实现

```go
// streamChunk 是流式响应的单条事件数据。
// 注意与非流式的区别：增量内容在 delta 而非 message 里。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Role    string `json:"role"`    // 仅首个 chunk 携带
			Content string `json:"content"` // 文本增量片段
		} `json:"delta"`
		FinishReason string `json:"finish_reason"` // 最后一个 chunk 为 "stop"
	} `json:"choices"`
}

// ChatStream 发起流式请求，每收到一段文本增量就回调 onDelta，
// 全部结束后返回拼接好的完整文本。
func (c *Client) ChatStream(messages []Message, onDelta func(text string)) (string, error) {
	reqBody := ChatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: 0.3,
		Stream:      true, // 关键开关
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("api error %d: %s", resp.StatusCode, b)
	}

	scanner := bufio.NewScanner(resp.Body)
	// 默认行缓冲上限 64KB，个别超长事件行会被截断报错，放宽到 1MB
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var full strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // 跳过空行、注释行（": keep-alive"）等
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break // 流式协议的结束哨兵
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 容错：单条坏事件不中断整个流（也可选择报错，取舍均可）
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		if delta != "" {
			full.WriteString(delta)
			onDelta(delta)
		}
	}
	if err := scanner.Err(); err != nil {
		return full.String(), fmt.Errorf("read stream: %w", err)
	}
	return full.String(), nil
}
```

`ChatRequest` 需要补一个字段：

```go
Stream bool `json:"stream,omitempty"`
```

main.go 中的使用方式：

```go
answer, err := client.ChatStream(ag.Messages(), func(text string) {
	fmt.Print(text) // 逐字打印
})
fmt.Println()
```

## 关键设计点

1. **delta ≠ message**：流式响应的结构变了，增量在 `choices[0].delta.content`，这是新手最常写错的地方。
2. **`[DONE]` 哨兵**：OpenAI 兼容协议用一行 `data: [DONE]` 表示流结束，它不是 JSON，必须在 Unmarshal 之前拦截。
3. **Scanner 缓冲**：`bufio.Scanner` 默认单行上限 64KB，长事件行会直接报错；要么放宽 buffer，要么改用 `bufio.Reader.ReadString('\n')`。
4. **聚合返回**：`onDelta` 负责"展示"（实时性），返回值负责"入账"（把完整回答 append 进对话历史）——两个职责都要，只回调不聚合的话历史会丢内容。
5. **（进阶思考）流式 + tool_calls**：流式下工具调用也是分片到达的（`delta.tool_calls`，按 index 归位拼接 arguments 字符串）。第一版不做是对的；做的时候记住核心难点是**分片重组**。

## 对照清单

完成后逐条自评：

- [ ] 请求体正确带了 `"stream": true`
- [ ] 用独立的 `streamChunk` 结构解析 delta，而不是复用非流式的 `ChatResponse`
- [ ] 正确处理了 `[DONE]` 结束标志（在 JSON 解析之前判断）
- [ ] 跳过了非 `data: ` 开头的行（空行、keep-alive 注释）
- [ ] 考虑了 Scanner 单行长度上限问题
- [ ] 完整文本被聚合并返回（历史记录不丢内容）
- [ ] 流式过程中连接中断有错误处理（`scanner.Err()`）
- [ ] 运行效果：回答逐字/逐段打印出来，而非一次性出现
