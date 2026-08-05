package tools

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPFetch 抓取一个 URL 的内容（截断返回，防止爆掉上下文）。
//
// 工程要点：工具返回给模型的内容一定要有长度上限。
// 一个无界的工具结果可能一次烧掉几万 token，还会挤占上下文窗口。
//
// 练习：本文件无需学习者完成的部分（练习 4 文件工具见 tools.go 末尾）。
type HTTPFetch struct {
	MaxBytes int64
}

func (h HTTPFetch) Name() string { return "http_fetch" }

func (h HTTPFetch) Description() string {
	return "抓取指定 URL 的网页内容（纯文本）。当用户给出链接或需要查资料时使用。"
}

func (h HTTPFetch) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "要抓取的完整 URL，需以 http:// 或 https:// 开头",
			},
		},
		"required": []string{"url"},
	}
}

func (h HTTPFetch) Execute(args string) (string, error) {
	var p struct {
		URL string `json:"url"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return "", err
	}

	max := h.MaxBytes
	if max <= 0 {
		max = 8000 // 默认最多返回 8KB
	}

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(p.URL)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d from %s", resp.StatusCode, p.URL)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, max))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(body), nil
}
