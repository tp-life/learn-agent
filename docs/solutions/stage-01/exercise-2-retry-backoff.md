# 练习 2 参考答案：重试与限流（指数退避）

> 对应题目：`mini-agent/internal/llm/client.go`（练习 2 已完成，TODO 已移除）
> ⚠️ 先自己实现，再对照本文档。

## 参考实现

```go
// APIError 携带 HTTP 状态码的错误，重试逻辑靠它区分"值不值得重试"。
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error %d: %s", e.StatusCode, e.Body)
}

// retryable 判断错误是否值得重试：
//   - 429 限流：值得，稍等再试
//   - 5xx 服务端错误：值得，通常是临时故障
//   - 无法识别的错误（网络层错误居多）：默认重试，属于保守策略
//   - 其他 4xx（401 鉴权失败、400 参数错误）：不值得，重试也是同样的结果
//
// 注意：默认分支把 marshal/build request 这类本地逻辑错误也归进了
// "可重试"——它们重试必然再失败，这么写只是图省事的保守取舍，
// 严格做法是再加一类"本地错误不重试"。
func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}
	return true // 无法识别的错误默认重试（保守策略，多数是网络层错误）
}

// ChatWithRetry 包装 Chat，指数退避，最多重试 maxRetries 次
// （总尝试次数 = 1 次首发 + maxRetries 次重试）。
// 退避间隔：1s → 2s → 4s（每次左移一位翻倍）。
func (c *Client) ChatWithRetry(messages []Message, tools []Tool, maxRetries int) (*ChatResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ { // 注意是 <=，range maxRetries 会少一次
		if attempt > 0 {
			backoff := time.Second << (attempt - 1)
			fmt.Printf("[retry] 第 %d 次重试，等待 %v（上次错误: %v）\n", attempt, backoff, lastErr)
			time.Sleep(backoff)
		}
		resp, err := c.Chat(messages, tools)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable(err) {
			return nil, err // 不可重试错误（如 401）立即失败，不浪费额度
		}
	}
	return nil, fmt.Errorf("重试 %d 次后仍失败: %w", maxRetries, lastErr)
}
```

Chat 里的错误返回相应改为（**这一步是整个练习的"接线"，漏了重试分类会静默失效**）：

```go
if resp.StatusCode != http.StatusOK {
	return nil, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
}
```

如果 `Chat` 仍返回 `fmt.Errorf` 普通错误，`retryable` 里的 `errors.As` 永远失败，
所有错误都落入"默认重试"分支——401 也会被重试 3 次，而且没有任何报错提示你接错了。
`ChatStream` 的非 200 分支同理也要返回 `APIError`。

## 关键设计点

1. **先分类，再重试**：重试的前提是把错误分成"可重试"（429/5xx/网络抖动）和"不可重试"（4xx 业务错误）。对 401 重试 3 次除了浪费时间和额度没有任何意义——这是面试官最想听到的点。
2. **`errors.As` 解包**：用自定义错误类型携带状态码，外层用 `errors.As` 取回，比字符串匹配健壮。
3. **指数退避而非固定间隔**：服务端过载时，固定间隔重试会形成"惊群"（所有客户端同一时刻一起重试）。生产中还会加随机抖动（jitter），如 `backoff * (0.5 + rand.Float64())`。
4. **失败要透出原始错误**：`fmt.Errorf("...: %w", lastErr)` 用 `%w` 包装，调用方还能继续 `errors.As` 分析。
5. **循环边界是常见 off-by-one**：`for attempt := range maxRetries` 只尝试 maxRetries 次（1 首发 + 2 重试），4s 那一档永远走不到，最终报错文案还与实际重试次数不符。题意"最多重试 3 次"对应 `attempt <= maxRetries`。
6. **和流式路径的关系**：练习 1 之后 agent 主循环走的是 `ChatStream`，本方法只覆盖非流式 `Chat`（摘要等辅助调用在用）。生产做法是把退避逻辑抽成通用 helper（如 `withRetry(func() (*ChatResponse, error))`）让两者共用。注意流式重试有额外语义：`onDelta` 已打出的增量在重试时会重复输出，需要去重或先缓冲。
7. **（加分项）context 取消**：`time.Sleep` 不可中断，生产写法是 `select { case <-time.After(d): case <-ctx.Done(): return ctx.Err() }`。

## 对照清单

- [ ] 定义了携带状态码的错误类型（或用其他方式传递状态码）
- [ ] `Chat`（和 `ChatStream`）的非 200 分支返回的是 `APIError` 而非 `fmt.Errorf`（接线步骤，漏了分类静默失效）
- [ ] 区分了可重试（429/5xx/网络错误）与不可重试（其他 4xx）错误
- [ ] 不可重试错误立即返回，不进入重试循环
- [ ] 循环次数正确：1 次首发 + maxRetries 次重试（不是 range maxRetries）
- [ ] 退避间隔指数增长（1s/2s/4s 或类似）
- [ ] 有最大重试次数上限，最终失败时返回带上下文的错误
- [ ] 最终错误用 `%w` 包装保留原始错误链
- [ ] 验证方式可行：改错 baseURL 触发网络错误（走默认可重试分支）；验证 5xx 分类可指向本地 mock 服务；验证"401 不重试"需把 API key 改错（前提是上一条接线已完成）
- [ ] （加分）支持 context 取消 / 加入了 jitter 抖动
