# 练习 3 参考答案：上下文压缩

> 对应题目：`mini-agent/internal/agent/agent.go` 末尾 TODO(练习3)
> ⚠️ 先自己实现，再对照本文档。

## 参考实现

> 注意：`agent.go` 需要新增 `strings` import（当前只 import 了 `fmt`、`llm`、`tools`），
> 否则 `strings.Builder` 编译报错。

```go
const summaryPrompt = `请把以下对话历史压缩成一段摘要，要求保留：
1. 用户透露的事实（姓名、偏好、背景信息）
2. 已得出的结论和答案
3. 未完成的任务和待办
用第三人称陈述，控制在 300 字以内。对话如下：`

// compressIfNeeded 在历史过长时用 LLM 摘要替换早期消息。
// 在 Run 的循环开头调用。
func (a *Agent) compressIfNeeded() error {
	const threshold = 20 // 超过 20 条消息触发压缩
	const keepRecent = 6 // 保留最近 6 条原样不动

	if len(a.messages) <= threshold {
		return nil
	}

	// 候选切分点：压缩 [1:split]，保留 messages[split:]
	split := len(a.messages) - keepRecent

	// 关键：切分点不能落在 tool_calls 配对的中间。
	// 若 split 处是 role=tool 消息，说明它配对的 assistant tool_calls 在压缩区里，
	// 会留下"孤儿 tool 消息"导致 API 报错 —— 把切分点后移到配对结束之后。
	for split < len(a.messages) && a.messages[split].Role == "tool" {
		split++
	}
	if split >= len(a.messages) {
		// 保留区整组都是 tool 消息（如一次 ≥keepRecent 个并行调用），
		// 后移把最近上下文全吃掉了——本轮放弃压缩，比丢光任务状态强。
		return nil
	}

	// 把待压缩的历史渲染成纯文本，交给 LLM 摘要
	var sb strings.Builder
	for _, m := range a.messages[1:split] { // 跳过 [0] system
		fmt.Fprintf(&sb, "[%s]: %s\n", m.Role, m.Content)
		// assistant 调工具时 Content 通常为空，不补上工具信息的话，
		// 摘要会丢失"已经查过/算过什么"，压缩后模型可能重复调同一工具
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&sb, "[%s 调用工具]: %s(%s)\n", m.Role, tc.Function.Name, tc.Function.Arguments)
		}
	}
	resp, err := a.client.Chat([]llm.Message{
		{Role: "system", Content: summaryPrompt},
		{Role: "user", Content: sb.String()},
	}, nil) // 摘要请求不带工具，避免模型节外生枝
	if err != nil {
		return err // 压缩失败不阻塞对话，下次再试
	}

	summary := llm.Message{
		Role:    "system",
		Content: "【早期对话摘要】" + resp.Choices[0].Message.Content,
	}

	// 重组历史：system 原文 + 摘要 + 最近 N 条
	compressed := make([]llm.Message, 0, keepRecent+2)
	compressed = append(compressed, a.messages[0], summary)
	compressed = append(compressed, a.messages[split:]...)
	a.messages = compressed
	return nil
}
```

Run 循环开头接入：

```go
for step := 0; step < a.MaxSteps; step++ {
	if err := a.compressIfNeeded(); err != nil && a.Verbose {
		fmt.Printf("[compress] 失败（忽略，继续对话）: %v\n", err)
	}
	// ... 原有逻辑
}
```

## 关键设计点

1. **孤儿 tool 消息是本练习最大的坑**：assistant 的 tool_calls 和后续的 role=tool 消息是一个**不可分割的配对组**。切分点若落在组中间，保留下来的部分以 tool 消息开头，API 直接报 400。参考实现用"切分点后移跳过连续 tool 消息"解决；另一种等价做法是"前移切分点，把整组都压缩掉"。注意后移要加上界防护：如果保留区整组都是 tool 消息（一次发起很多并行调用时会发生），split 会后移到末尾，把最近上下文全压掉——此时应放弃本轮压缩。
2. **摘要请求不带 tools**：压缩调用本质是一次独立的总结任务，给工具只会让模型可能去调工具而不是写摘要。
3. **摘要也用 system 角色注入**：保证它在后续每轮都被模型当作"规矩/背景"看待；用 user 角色也可以，是合理的替代选择。
4. **压缩失败不阻塞对话**：摘要是一次优化，不是正确性依赖。失败时保留原历史继续跑，下一轮再试。想进一步降低失败率，可以直接复用练习 2 的 `ChatWithRetry`。
5. **保留最近几条原文**：摘要必然有损，最近几轮是任务进行中的上下文，原样保留能明显降低"压缩后变傻"的感觉。
6. **摘要输入要带上工具调用信息**：assistant 发起 tool_calls 时 Content 通常为空，只渲染 Content 会丢失"调了什么工具、参数是什么"，压缩后模型可能重复调用同一工具。参考实现里对 ToolCalls 做了单独渲染。
7. **（进阶）更省钱的判断**：按消息条数触发是简化版；生产做法是估算 token 数（如 tiktoken / 粗略按 字长÷2）接近窗口 80% 时触发。

## 对照清单

- [ ] 有明确的触发条件（条数或 token 估算）
- [ ] 切分点处理了 tool_calls / tool 配对，不会产生孤儿 tool 消息
- [ ] 切分点后移有上界防护，极端情况下不会把最近上下文全部压掉
- [ ] 摘要输入包含工具调用信息（不只渲染 Content）
- [ ] 摘要请求是独立调用，且不带 tools
- [ ] 摘要 prompt 说明了要保留什么（事实/结论/待办）
- [ ] system 原文始终保留在 messages[0]
- [ ] 最近若干条消息原样保留
- [ ] 压缩失败时对话能继续（降级而非中断）
- [ ] import 补了 `strings`（原样粘贴能编译）
- [ ] 验证通过：30+ 轮长对话后模型仍记得早期信息（如用户名字）
