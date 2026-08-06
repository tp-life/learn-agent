# 练习 1 参考答案：embedding client

> 对应 TODO：`mini-agent/internal/embed/embed.go` 的 `TODO(练习1)`。
> **完成练习并自评后再看本文档。**
> 本文档代码已于 2026-08-06 实际粘贴进项目验证：`go vet ./...` 与 `go test ./internal/embed/`（4 个测试）全部通过。

---

## 一、参考实现

### `internal/embed/embed.go`（只给出需要实现的 Embed 方法及其 import；骨架其余部分不变）

import 从骨架的 3 个包扩为：

```go
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mini-agent/internal/llm"
)
```

```go
// Embed 输入一批文本，返回与之一一对应的向量切片（result[i] 是 texts[i] 的向量）。
func (c *Client) Embed(texts []string) ([][]float32, error) {
	// 入参校验：空批量、空字符串都直接拦下。
	// 空字符串 embedding 没有语义，出现它几乎一定是上游 chunking 的 bug，
	// 静默放行会把坏数据送进向量库，到检索环节才暴露就难查了。
	if len(texts) == 0 {
		return nil, fmt.Errorf("embed: empty input")
	}
	for i, t := range texts {
		if strings.TrimSpace(t) == "" {
			return nil, fmt.Errorf("embed: texts[%d] is empty", i)
		}
	}

	body, err := json.Marshal(embeddingRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
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
		// 复用 llm.APIError 而不是裸 fmt.Errorf：保留状态码，
		// 将来做重试（429/5xx 退避）时才能分类——练习 2 在 llm 包踩过的坑。
		return nil, &llm.APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var embResp embeddingResponse
	if err := json.Unmarshal(respBody, &embResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if len(embResp.Data) != len(texts) {
		return nil, fmt.Errorf("embed: got %d embeddings for %d texts", len(embResp.Data), len(texts))
	}

	// 按 index 归位，绝不假设 data 顺序与输入一致。
	result := make([][]float32, len(texts))
	for _, d := range embResp.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("embed: index %d out of range [0,%d)", d.Index, len(texts))
		}
		if len(d.Embedding) != BgeM3Dimensions {
			return nil, fmt.Errorf("embed: texts[%d] dim = %d, want %d", d.Index, len(d.Embedding), BgeM3Dimensions)
		}
		result[d.Index] = d.Embedding
	}
	// 防御：len 相等但 index 重复时会有槽位留 nil，逐个确认。
	for i, v := range result {
		if v == nil {
			return nil, fmt.Errorf("embed: texts[%d] missing in response", i)
		}
	}
	return result, nil
}
```

### `internal/embed/embed_test.go`（新建，httptest 假服务器，无需真实 API key）

```go
package embed

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"mini-agent/internal/llm"
)

// makeVec 造一个 1024 维的测试向量，首元素写入标记值便于断言归属。
func makeVec(tag float32) []float32 {
	v := make([]float32, BgeM3Dimensions)
	v[0] = tag
	return v
}

// newFakeServer 起一个假的 /embeddings 服务。
// handler 收到请求后返回 jsonBody 与状态码。
func newFakeServer(t *testing.T, status int, jsonBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("missing Authorization header")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, jsonBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEmbed_ReordersByIndex 验证核心坑：响应 data 乱序时必须按 index 归位。
func TestEmbed_ReordersByIndex(t *testing.T) {
	texts := []string{"alpha", "beta", "gamma"}
	// 故意乱序返回：data[0] 是 texts[2] 的向量……
	resp := embeddingResponse{}
	resp.Data = append(resp.Data,
		struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}{Index: 2, Embedding: makeVec(2)},
		struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}{Index: 0, Embedding: makeVec(0)},
		struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}{Index: 1, Embedding: makeVec(1)},
	)
	body, _ := json.Marshal(resp)
	srv := newFakeServer(t, http.StatusOK, string(body))

	c := NewClient("fake-key").WithBaseURL(srv.URL)
	vecs, err := c.Embed(texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i, v := range vecs {
		if v[0] != float32(i) {
			t.Errorf("vecs[%d][0] = %v, want %d（归位错误）", i, v[0], i)
		}
	}
}

// TestEmbed_RejectsEmptyInput 验证入参校验：空批量、空字符串都要报错。
func TestEmbed_RejectsEmptyInput(t *testing.T) {
	c := NewClient("fake-key").WithBaseURL("http://127.0.0.1:1") // 不应发出任何请求
	if _, err := c.Embed(nil); err == nil {
		t.Error("empty slice: want error, got nil")
	}
	if _, err := c.Embed([]string{"ok", "  "}); err == nil {
		t.Error("blank string: want error, got nil")
	}
}

// TestEmbed_Non200ReturnsAPIError 验证非 200 返回带状态码的 *llm.APIError，
// 这是将来重试分类（429/5xx 才重试）的前提。
func TestEmbed_Non200ReturnsAPIError(t *testing.T) {
	srv := newFakeServer(t, http.StatusUnauthorized, `{"error":"invalid key"}`)
	c := NewClient("bad-key").WithBaseURL(srv.URL)

	_, err := c.Embed([]string{"hello"})
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *llm.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
}

// TestEmbed_WrongDimension 验证维度校验：模型/服务商配错时尽早报错。
func TestEmbed_WrongDimension(t *testing.T) {
	body := `{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}]}`
	srv := newFakeServer(t, http.StatusOK, body)
	c := NewClient("fake-key").WithBaseURL(srv.URL)

	if _, err := c.Embed([]string{"hello"}); err == nil {
		t.Error("want dimension error, got nil")
	}
}
```

## 二、关键设计点

1. **复用 `llm.APIError` 而非新建错误类型**：embed 包因此 import 了 `mini-agent/internal/llm`。这个依赖是有意为之——阶段一练习 2 的教训就是"错误分类依赖具体错误类型"，如果 embed 再造一个 `embed.APIError`，将来写重试时就要 `errors.As` 两次。统一复用一个类型，重试分类逻辑可以直接搬。**易错处**：返回 `fmt.Errorf("api error %d", code)` 看起来一样能用，但状态码丢了，重试分类会静默失效。

2. **index 归位是本练习的灵魂**：`result[d.Index] = d.Embedding` 一行就是全部，但如果写成按 data 数组顺序 `result[i] = d.Embedding`，单测里乱序用例会立刻抓到。**易错处**：只校验了 `len(data) == len(texts)` 还不够——index 重复时（如两个 index=0）会有槽位留 nil，所以归位后再扫一遍 nil 防御。

3. **维度校验写死 1024 的取舍**：默认模型是 bge-m3，写死能在"配错模型/服务商"时最早报错。代价是 `WithModel` 换成其他维度模型时校验会误报——更灵活的做法是把期望维度存成 Client 字段或从首批响应推断。学习项目选择"早报错"，面试可以主动讲这个权衡。

4. **测试用 httptest 而非真实 API**：① 不烧额度、不依赖网络，CI 可跑；② 乱序、错维度这类异常响应真实 API 永远不会给你，只有假服务器能构造；③ 真机验证应该是可选的 smoke test（读环境变量决定跑不跑），不该进默认测试集。

5. **批量上限与重试（加分项，未在本实现中做）**：硅基流动对单批 input 条数有限制（以官方文档为准），入库几百个 chunk 时需要分批循环调用 Embed 再拼接——注意分批后每批内部 index 从 0 开始，拼回时要加偏移。重试逻辑与 `llm.ChatWithRetry` 同构，真要做得先把 `retryable` 和退避循环从 llm 包抽成公共 helper，复制粘贴是下策。

## 三、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [x] `Embed(texts)` 语义正确：`result[i]` 对应 `texts[i]`，靠 **index 字段**归位而非 data 数组顺序
- [x] 空批量、空字符串输入有显式报错，且在校验阶段不发出 HTTP 请求
- [x] 非 200 响应返回**带状态码**的错误类型（可用 `errors.As` 取回状态码），不是裸 `fmt.Errorf`
- [x] 有维度校验（1024），维度不符时报错
- [x] `len(data) != len(texts)` 报错；归位后能防御 index 越界/重复导致的空洞
- [x] httptest 假服务器测试至少覆盖：乱序归位、空输入、非 200 错误三类
- [x] `go vet ./...` 和 `go test ./internal/embed/` 全绿
- [x] 能口头回答：为什么 embedding 不放进 `llm.Client`？为什么 data 顺序不可信？
