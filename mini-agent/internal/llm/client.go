package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client 是一个 OpenAI 兼容的 chat completions 客户端。
// DeepSeek 官方 API 兼容 OpenAI 协议，只需替换 baseURL 和 apiKey。
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{
		baseURL: "https://api.deepseek.com",
		apiKey:  apiKey,
		model:   "deepseek-chat",
		httpClient: &http.Client{
			Timeout: 120 * time.Second, // 长输出时给足余量
		},
	}
}

// WithModel 切换模型，例如 "deepseek-reasoner"。
func (c *Client) WithModel(model string) *Client {
	c.model = model
	return c
}

// Chat 发起一次非流式对话补全。
// 面试/笔试高频点：为什么 agent 循环里通常用非流式？
// 因为需要拿到完整的 tool_calls 才能决定下一步动作。
func (c *Client) Chat(messages []Message, tools []Tool) (*ChatResponse, error) {
	reqBody := ChatRequest{
		Model:       c.model,
		Messages:    messages,
		Tools:       tools,
		Temperature: 0.3, // agent 场景偏低温度，减少发散
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

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// 必须返回带状态码的 APIError 而非普通 error：
		// ChatWithRetry 靠 errors.As 取回状态码来区分"值不值得重试"，
		// 这里若返回 fmt.Errorf，重试分类会静默失效（401 也会被重试）。
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("empty choices in response")
	}
	return &chatResp, nil
}

// ChatStream 发起流式对话补全（SSE），每收到一段文本增量就回调 onDelta，
// 流结束后返回聚合好的完整 ChatResponse——形态与非流式 Chat 完全一致，
// 因此 agent 循环可以无差别地使用两者。
//
// 聚合逻辑的两个要点：
//   - content 是逐片到达的文本，直接顺序拼接即可；
//   - tool_calls 是分片到达的，必须按 index 归组后拼接 arguments 字符串
//     （原理见 types.go 中 streamToolCall 的注释）。
//
// 为什么返回值是 *ChatResponse 而不是 string：
// agent 循环需要的不只是文本，还有 tool_calls 和 finish_reason；
// 让流式和非流式返回同一种类型，上层就不用为流式写第二套分支。
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
	var toolCalls []ToolCall   // 按下标聚合分片；len 随最大 index 增长
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

type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error %d: %s", e.StatusCode, e.Body)
}

// retryable 判断错误是否值得重试：
//   - 429 限流 / 5xx 服务端错误：值得，通常是临时故障
//   - 无法识别的错误（网络层错误居多）：默认重试，属于保守策略
//   - 其他 4xx（401 鉴权失败、400 参数错误）：不值得，重试也是同样结果
//
// 注意默认分支的取舍：marshal/build request 这类本地逻辑错误其实
// 重试也必然失败，归进"可重试"只是图省事；要严格可以再加一类
// "本地错误不重试"。面试追问"哪些错误不该重试"时这一点能加分。
func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}

	return true
}

// ChatWithRetry 包装 Chat，指数退避，最多重试 maxRetries 次
// （总尝试次数 = 1 次首发 + maxRetries 次重试）。
// 退避间隔：1s → 2s → 4s（每次左移一位翻倍）。
//
// 易踩的坑：循环写成 `for attempt := range maxRetries` 会少一次尝试
// （off-by-one），maxRetries=3 时实际只重试 2 次，4s 那一档永远走不到。
//
// 注意：agent 主循环走的是 ChatStream（练习 1 后），本方法目前主要用于
// 摘要等非流式辅助调用；生产中应把退避逻辑抽成通用 helper 让两者共用
// （流式重试有额外语义：onDelta 已打出的增量在重试时会重复，需要去重或清空）。
func (c *Client) ChatWithRetry(messages []Message, tools []Tool, maxRetries int) (*ChatResponse, error) {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Second << (attempt - 1)
			fmt.Printf("[retry] 第%d 次重试， 等待 %v （上次错误：%v）\n", attempt, backoff, lastErr)
			time.Sleep(backoff)
		}

		resp, err := c.Chat(messages, tools)
		if err == nil {
			return resp, nil
		}

		lastErr = err
		if !retryable(err) {
			return nil, err
		}

	}

	return nil, fmt.Errorf("重试 %d 次后仍失败： %w", maxRetries, lastErr)
}
