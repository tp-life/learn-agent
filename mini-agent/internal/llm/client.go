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
		// 429（限流）和 5xx 在生产中应做指数退避重试 —— 这是留给你的练习
		return nil, fmt.Errorf("api error %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	fmt.Printf("模型返回数据：%s\n", respBody)
	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("empty choices in response")
	}
	return &chatResp, nil
}

// ============================ 练习区（由学习者完成） ============================
//
// TODO(练习1): SSE 流式输出 —— 面试高频手写题
//
// 任务：给 Client 增加方法
//
//	func (c *Client) ChatStream(messages []Message, onDelta func(text string)) (*ChatResponse, error)
//
// 要求：
//   - 请求体加 "stream": true，响应是 text/event-stream（SSE）格式
//   - 每收到一个增量片段就回调 onDelta；全部到齐后返回聚合好的完整结果
//
// 提示：
//   - SSE 响应体按行读取（bufio.Scanner），数据行以 "data: " 开头，
//     结束标志是一行 "data: [DONE]"
//   - 流式响应的 JSON 结构和非流式不同：增量在 choices[0].delta.content
//     （非流式是 choices[0].message.content），需要定义新的响应结构体
//   - 思考：流式时 tool_calls 是分片到达的，如果要支持，需要按 index 拼接
//     （第一版可以先不支持工具调用，只做纯文本流式）
//
// 验收：把 main.go 的输出发送改为流式打印，能逐字看到回答"打出来"。
// 参考答案：docs/solutions/stage-01/exercise-1-sse-streaming.md（完成后再看）
//
// TODO(练习2): 重试与限流 —— 生产化基本功
//
// 任务：包装 Chat 方法，遇到 429（限流）和 5xx（服务端错误）时指数退避重试，
// 最多 3 次；4xx 其他错误（如 401 鉴权失败）不重试直接返回。
//
// 提示：
//   - 退避间隔：1s、2s、4s（可用 time.Sleep，注意别在测试里真等）
//   - 区分"可重试错误"和"不可重试错误"：可以定义一个带状态码的错误类型
//   - 加分项：支持 context.Context 取消
//
// 验收：暂时把 baseURL 改错触发 5xx，观察日志中出现 3 次重试后报错。
// 参考答案：docs/solutions/stage-01/exercise-2-retry-backoff.md（完成后再看）

func (c *Client) ChatStream(messages []Message, onDelta func(text string)) (string, error) {
	reqBody := ChatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: 0.3,
		Stream:      true,
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
		return "", fmt.Errorf("api error %d, %s", resp.StatusCode, b)
	}
	fmt.Println("开始请求数据：", messages)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var full strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Println(line)
		if !strings.HasPrefix(line, "data: ") {
			continue // 跳过空行、注释行
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
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

type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error %d: %s", e.StatusCode, e.Body)
}

func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}

	return true
}

func (c *Client) ChatWithRetry(messages []Message, tools []Tool, maxRetries int) (*ChatResponse, error) {
	var lastErr error

	for attempt := range maxRetries {
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
