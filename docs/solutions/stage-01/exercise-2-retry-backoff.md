# 练习 2 参考答案：重试与限流（指数退避）

> 对应题目：`mini-agent/internal/llm/client.go` 末尾 TODO(练习2)
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
//   - 网络层错误（连接超时等）：值得
//   - 其他 4xx（401 鉴权失败、400 参数错误）：不值得，重试也是同样的结果
func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}
	return true // 非 APIError = 网络层错误，可重试
}

// ChatWithRetry 包装 Chat，指数退避最多重试 maxRetries 次。
// 退避间隔：1s → 2s → 4s（每次左移一位翻倍）。
func (c *Client) ChatWithRetry(messages []Message, tools []Tool, maxRetries int) (*ChatResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
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

Chat 里的错误返回相应改为：

```go
if resp.StatusCode != http.StatusOK {
	return nil, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
}
```

## 关键设计点

1. **先分类，再重试**：重试的前提是把错误分成"可重试"（429/5xx/网络抖动）和"不可重试"（4xx 业务错误）。对 401 重试 3 次除了浪费时间和额度没有任何意义——这是面试官最想听到的点。
2. **`errors.As` 解包**：用自定义错误类型携带状态码，外层用 `errors.As` 取回，比字符串匹配健壮。
3. **指数退避而非固定间隔**：服务端过载时，固定间隔重试会形成"惊群"（所有客户端同一时刻一起重试）。生产中还会加随机抖动（jitter），如 `backoff * (0.5 + rand.Float64())`。
4. **失败要透出原始错误**：`fmt.Errorf("...: %w", lastErr)` 用 `%w` 包装，调用方还能继续 `errors.As` 分析。
5. **（加分项）context 取消**：`time.Sleep` 不可中断，生产写法是 `select { case <-time.After(d): case <-ctx.Done(): return ctx.Err() }`。

## 对照清单

- [ ] 定义了携带状态码的错误类型（或用其他方式传递状态码）
- [ ] 区分了可重试（429/5xx/网络错误）与不可重试（其他 4xx）错误
- [ ] 不可重试错误立即返回，不进入重试循环
- [ ] 退避间隔指数增长（1s/2s/4s 或类似）
- [ ] 有最大重试次数上限，最终失败时返回带上下文的错误
- [ ] 最终错误用 `%w` 包装保留原始错误链
- [ ] 验证方式可行：改错 baseURL 或触发 5xx，日志可见重试过程
- [ ] （加分）支持 context 取消 / 加入了 jitter 抖动
