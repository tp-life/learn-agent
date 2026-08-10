# 练习 1 参考答案：embedding client

> 对应 TODO：`mini-agent/internal/embed/embed.go` 的 `TODO(练习1)`。
> **完成练习并自评后再看本文档。**
> 本文档基础实现代码已于 2026-08-06 实际粘贴进项目验证：`go vet ./...` 与 `go test ./internal/embed/`（4 个测试）全部通过。
> 进阶实现（分批 + 重试，见第三节）同日回补并验证：临时粘贴进项目后 9 个测试全绿，验证后即删除，项目保持骨架版。

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

5. **批量上限与重试（加分项）**：硅基流动对单批 input 条数有限制（以官方文档为准），入库几百个 chunk 时需要分批循环调用 Embed 再拼接；429/5xx 需要指数退避重试。完整实现见下面"进阶实现"一节。

## 三、进阶实现（加分项：分批 + 重试）

> 回补记录：本节代码于 2026-08-06 以临时文件（`internal/embed/advanced.go` + `advanced_test.go`）实际粘贴进项目验证，
> `go vet ./...` 与 `go test ./internal/embed/ -v` 全部通过（9 个测试：基础 4 个 + 进阶 5 个），验证后已从项目删除——
> **进阶实现只属于答案，不进项目代码树**，项目保持骨架版。

### 设计取舍（先说清楚再看代码）

- **分批的拼回策略**：每批响应里的 index 是**批内**下标（从 0 开始）。本实现不手工处理 index 偏移——`Embed` 已保证返回值与输入切片一一对齐，所以按批次顺序 `append` 就天然保持全局顺序。易错写法是绕过 `Embed` 自己解析 data，然后忘记加批次偏移 `start`，导致第 2 批的向量覆盖第 1 批。
- **重试的错误分类**：复用 `llm.APIError`（`Embed` 返回的就是它），`errors.As` 取回状态码后按 429/5xx 分类。`llm.retryable` 是 llm 包的**私有函数**跨包用不了；生产正确做法是把"退避循环 + retryable 分类"抽成公共 helper（如 `internal/httpx`）让 llm/embed 共用。本实现只复制了 4 行分类逻辑——在学习项目里，复制 4 行的成本低于为此新建公共包，但这是债，第三个调用方出现时就该抽了。
- **与基础版的取舍**：基础版 `Embed` 保持单次请求的纯粹语义，分批/重试做成独立的包装方法而不是塞进 `Embed`——调用方（如入库管线）按需组合 `EmbedWithRetry` 内层 + `EmbedBatched` 外层，或反过来，职责清晰。代价是调用方要知道这两个方法的存在。

### `internal/embed/advanced.go`（进阶实现完整代码）

```go
package embed

import (
	"errors"
	"fmt"
	"time"

	"mini-agent/internal/llm"
)

// EmbedBatched 把大批量文本按 batchSize 切成多批请求，再把结果拼回。
//
// 为什么需要它：硅基流动等服务商对单批 input 条数有上限（以官方文档为准），
// 入库几百个 chunk 时一次请求会被拒绝，必须分批。
//
// 拼回的关键：每批响应里的 index 是【批内】下标（从 0 开始），
// 不是全局下标。这里的做法是不手工处理 index 偏移——
// Embed 已经保证返回值与输入切片一一对齐（result[i] 对应 batch[i]），
// 所以按批次顺序 append 就天然保持了全局顺序。
// 易错写法是绕过 Embed 自己解析 data，然后忘记加批次偏移 start，
// 导致第 2 批的向量覆盖第 1 批的结果。
func (c *Client) EmbedBatched(texts []string, batchSize int) ([][]float32, error) {
	if batchSize <= 0 {
		return nil, fmt.Errorf("embed: batchSize must be positive, got %d", batchSize)
	}
	// 校验全部交给 Embed 做（空输入、空字符串），这里只负责切分与拼接。
	result := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := c.Embed(texts[start:end])
		if err != nil {
			// 报错时带上批次范围，否则几百条里挂了一条根本没法定位。
			return nil, fmt.Errorf("embed batch [%d:%d]: %w", start, end, err)
		}
		result = append(result, vecs...)
	}
	return result, nil
}

// EmbedWithRetry 包装 Embed，对 429/5xx 做指数退避重试
// （总尝试次数 = 1 次首发 + maxRetries 次重试），模式与 llm.ChatWithRetry 同构。
//
// 设计说明（面试可讲的点）：
// 错误分类依赖 llm.APIError——Embed 返回的就是这个类型，errors.As 能取回状态码。
// llm.retryable 是 llm 包的私有函数，跨包用不了；生产环境的正确做法是
// 把"退避循环 + retryable 分类"抽成公共 helper（如 internal/httpx）让
// llm/embed 共用。本实现只复制了 4 行分类逻辑，没有整段复制退避循环之外的东西，
// 是在"学习项目不新建包"与"不重复粘贴"之间的取舍——复制 4 行的成本
// 低于为此新建一个公共包，但要清楚这是债，第三个调用方出现时就该抽了。
func (c *Client) EmbedWithRetry(texts []string, maxRetries int) ([][]float32, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Second << (attempt - 1) // 1s → 2s → 4s …
			time.Sleep(backoff)
		}
		vecs, err := c.Embed(texts)
		if err == nil {
			return vecs, nil
		}
		lastErr = err

		// 与 llm.retryable 同构的分类（就是那 4 行）：
		// 429 限流 / 5xx 服务端错误值得重试；401/400 等 4xx 重试也是同样结果，直接返回。
		// 无法识别的错误（多为网络层错误）保守起见也重试。
		var apiErr *llm.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode != 429 && apiErr.StatusCode < 500 {
			return nil, err
		}
	}
	return nil, fmt.Errorf("embed: 重试 %d 次后仍失败: %w", maxRetries, lastErr)
}
```

### `internal/embed/advanced_test.go`（进阶测试完整代码）

三个测试要点：跨批乱序也能归位、429 后重试成功且恰好 2 次请求、401 不重试只请求 1 次。

```go
package embed

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// batchAwareServer 按请求体里的 input 内容回包：对每条文本 "t<i>"
// 返回 [i,0,...] 的向量，且【故意乱序】返回批内 data，用来验证
// 跨批拼回 + 批内 index 归位同时正确。
func batchAwareServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		var req embeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp := embeddingResponse{}
		// 倒序遍历 = 批内乱序返回
		for i := len(req.Input) - 1; i >= 0; i-- {
			var tag int
			if _, err := fmt.Sscanf(req.Input[i], "t%d", &tag); err != nil {
				t.Errorf("unexpected text %q", req.Input[i])
			}
			v := make([]float32, BgeM3Dimensions)
			v[0] = float32(tag)
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: v})
		}
		body, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, string(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEmbedBatched_JoinOrderAcrossBatches：5 条文本 batchSize=2 → 3 批，
// 每批乱序返回，最终结果必须全局有序（result[i] 是 texts[i] 的向量）。
func TestEmbedBatched_JoinOrderAcrossBatches(t *testing.T) {
	var hits atomic.Int32
	srv := batchAwareServer(t, &hits)
	c := NewClient("fake-key").WithBaseURL(srv.URL)

	texts := []string{"t0", "t1", "t2", "t3", "t4"}
	vecs, err := c.EmbedBatched(texts, 2)
	if err != nil {
		t.Fatalf("EmbedBatched: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("got %d vecs, want %d", len(vecs), len(texts))
	}
	for i, v := range vecs {
		if v[0] != float32(i) {
			t.Errorf("vecs[%d][0] = %v, want %d（跨批拼回顺序错误）", i, v[0], i)
		}
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server hits = %d, want 3（5 条 / batchSize 2）", got)
	}
}

// TestEmbedBatched_BatchSizeCoversAll：batchSize >= len(texts) 时只发一次请求。
func TestEmbedBatched_BatchSizeCoversAll(t *testing.T) {
	var hits atomic.Int32
	srv := batchAwareServer(t, &hits)
	c := NewClient("fake-key").WithBaseURL(srv.URL)

	vecs, err := c.EmbedBatched([]string{"t0", "t1"}, 100)
	if err != nil {
		t.Fatalf("EmbedBatched: %v", err)
	}
	if len(vecs) != 2 || vecs[0][0] != 0 || vecs[1][0] != 1 {
		t.Errorf("unexpected vecs: %v", [][]float32{vecs[0][:1], vecs[1][:1]})
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1", got)
	}
}

// TestEmbedBatched_RejectsBadBatchSize：batchSize <= 0 必须报错且不发请求。
func TestEmbedBatched_RejectsBadBatchSize(t *testing.T) {
	var hits atomic.Int32
	srv := batchAwareServer(t, &hits)
	c := NewClient("fake-key").WithBaseURL(srv.URL)

	if _, err := c.EmbedBatched([]string{"t0"}, 0); err == nil {
		t.Error("want error for batchSize=0, got nil")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("server hits = %d, want 0（不该发请求）", got)
	}
}

// TestEmbedWithRetry_429ThenSuccess：第一次 429，第二次 200，
// 应重试成功且恰好请求 2 次。
func TestEmbedWithRetry_429ThenSuccess(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"rate limited"}`)
			return
		}
		resp := embeddingResponse{}
		v := make([]float32, BgeM3Dimensions)
		v[0] = 42
		resp.Data = append(resp.Data, struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}{Index: 0, Embedding: v})
		body, _ := json.Marshal(resp)
		fmt.Fprint(w, string(body))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("fake-key").WithBaseURL(srv.URL)
	vecs, err := c.EmbedWithRetry([]string{"hello"}, 2)
	if err != nil {
		t.Fatalf("EmbedWithRetry: %v", err)
	}
	if vecs[0][0] != 42 {
		t.Errorf("vecs[0][0] = %v, want 42", vecs[0][0])
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server hits = %d, want 2（1 次 429 + 1 次成功）", got)
	}
}

// TestEmbedWithRetry_401NoRetry：401 属于不可重试错误，
// 必须立即返回且只请求 1 次——重试 401 只会再失败一次还白烧退避时间。
func TestEmbedWithRetry_401NoRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid key"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient("bad-key").WithBaseURL(srv.URL)
	if _, err := c.EmbedWithRetry([]string{"hello"}, 3); err == nil {
		t.Fatal("want error, got nil")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1（401 不应重试）", got)
	}
}
```

### 进阶实现的易错处

1. **批内 index 误以为全局**：自己解析响应拼回时，必须加批次偏移 `start + d.Index`；复用 `Embed` 的返回值再 append 则天然规避。
2. **循环 off-by-one**：`for attempt := 0; attempt <= maxRetries; attempt++` 是"1 次首发 + maxRetries 次重试"；写成 `attempt < maxRetries` 会少一次尝试（llm.ChatWithRetry 注释里点过的同一个坑）。
3. **401 也重试**：丢失状态码（返回裸 `fmt.Errorf`）或分类写错，都会让 401 白重试——测试里用请求计数器（`atomic.Int32`）断言"恰好 1 次"才能抓住这类回归。
4. **重试测试的时间成本**：指数退避真实 `time.Sleep`，429 用例要约 1s；测试里把 `maxRetries` 控制在 2，别把退避档位打满。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [x] `Embed(texts)` 语义正确：`result[i]` 对应 `texts[i]`，靠 **index 字段**归位而非 data 数组顺序
- [x] 空批量、空字符串输入有显式报错，且在校验阶段不发出 HTTP 请求
- [x] 非 200 响应返回**带状态码**的错误类型（可用 `errors.As` 取回状态码），不是裸 `fmt.Errorf`
- [x] 有维度校验（1024），维度不符时报错
- [x] `len(data) != len(texts)` 报错；归位后能防御 index 越界/重复导致的空洞
- [x] httptest 假服务器测试至少覆盖：乱序归位、空输入、非 200 错误三类
- [x] `go vet ./...` 和 `go test ./internal/embed/` 全绿
- [x] 能口头回答：为什么 embedding 不放进 `llm.Client`？为什么 data 顺序不可信？

加分项（做了才需要勾，参考"进阶实现"一节）：

- [x] 分批：超过 batchSize 时切多批请求，跨批拼回后 `result[i]` 仍对应 `texts[i]`（批内乱序也能归位）
- [x] 重试：429/5xx 指数退避重试；401 等 4xx 不重试（用请求计数器验证恰好 1 次）
- [x] 能口头回答：为什么重试分类只复制 4 行而不是抽公共包？什么信号出现时该抽？
