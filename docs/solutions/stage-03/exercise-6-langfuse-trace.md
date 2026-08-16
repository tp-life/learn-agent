# 练习 6 参考答案：Langfuse 嵌套 trace + token 成本核算

> 对应 TODO：`stage-03-multi-agent/internal/trace/trace.go` 的 `TODO(练习6)`。
> **完成练习并自评后再看本文档。**
> 本文档基础实现代码（`langfuse.go` + `trace_test.go`）已于 2026-08-14 实际粘贴进项目验证：
> `go vet ./internal/trace/` 与 `go test ./internal/trace/ -count=1`（5 个测试）全部通过。
> 进阶实现（异步批量上报，见第三节）同日以临时文件（`langfuse_async.go` + `langfuse_async_test.go`）
> 验证：`go test ./internal/trace/ -count=1` 共 8 个测试全绿，且异步测试 `-count=5` 连跑无 flake；
> 验证后即删除，项目保持骨架版。全程 httptest 假服务器，未连接真实 Langfuse。

---

## 一、参考实现

### `internal/trace/langfuse.go`（新建；骨架 `trace.go` 不变）

```go
package trace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// ingestionEvent 是 Langfuse ingestion API 的事件信封：
// POST {host}/api/public/ingestion 接收 {"batch": [event, ...]}。
// 每个事件自带 id（事件本身的去重 ID，服务端幂等用）、type（决定 body 怎么解释）、
// timestamp（事件发生时间）与 body（真正的载荷）。
type ingestionEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"` // trace-create / span-create / span-update / generation-create / generation-update
	Timestamp string         `json:"timestamp"`
	Body      map[string]any `json:"body"`
}

// spanState 是 StartSpan 时登记、EndSpan 时消费的本地状态：
// 上报事件的 body 需要 traceId 与"这个 observation 是 span 还是 generation"，
// 但 Tracer 接口只传 spanID，所以必须本地记一份。
type spanState struct {
	traceID string
	kind    string // "span" | "generation"
	model   string // kind == "generation" 时的模型名，算成本用
}

// pricePerMTok 简化的模型单价表（美元 / 1M tokens，[0]=input [1]=output）。
//
// 【简化说明】DeepSeek 官方定价区分缓存命中/未命中：以 deepseek-chat 为例，
// input 缓存未命中 $0.27/1M、命中 $0.07/1M（约为 1/4），output $1.10/1M。
// 生产实现应把 usage 里的 cached tokens 拆出来分别计价；
// 学习项目统一按未命中价算，偏高不偏低保守，单价以官网为准。
var pricePerMTok = map[string][2]float64{
	"deepseek-chat":     {0.27, 1.10},
	"deepseek-reasoner": {0.55, 2.19},
}

// costUSD 按单价表算一次 LLM 调用的美元成本；模型不在表里返回 0
// （不报错——成本算不出不该影响观测上报）。
func costUSD(model string, inputTokens, outputTokens int) float64 {
	p, ok := pricePerMTok[model]
	if !ok {
		return 0
	}
	return float64(inputTokens)/1e6*p[0] + float64(outputTokens)/1e6*p[1]
}

// Langfuse 是通过公开 ingestion API 上报事件的 Tracer 实现。
//
// 基础版的取舍：StartSpan/EndSpan 只往内存 buffer 追加事件，不发 HTTP；
// Flush 把攒下的事件一次性 POST。代价是进程崩溃丢 buffer 里未上报的事件，
// 收益是 agent 主流程完全不被观测网络调用拖慢/影响（进阶实现见答案文档第三节）。
type Langfuse struct {
	host      string
	publicKey string
	secretKey string
	http      *http.Client

	mu     sync.Mutex
	events []ingestionEvent
	spans  map[string]*spanState
	// emit 是事件出口，默认 append 到 events buffer。
	// 抽成字段是为了进阶异步版能换成 channel 发送而不重写 StartSpan/EndSpan。
	// 约定：调用 emit 时必须已持有 l.mu。
	emit func(ingestionEvent)
}

// NewLangfuse 创建一个同步缓冲版 Langfuse Tracer。
// host 形如 "http://localhost:3000"（自托管）或 "https://cloud.langfuse.com"。
func NewLangfuse(host, publicKey, secretKey string) *Langfuse {
	l := &Langfuse{
		host:      host,
		publicKey: publicKey,
		secretKey: secretKey,
		http:      &http.Client{Timeout: 10 * time.Second},
		spans:     map[string]*spanState{},
	}
	l.emit = func(ev ingestionEvent) { l.events = append(l.events, ev) }
	return l
}

var _ Tracer = (*Langfuse)(nil)

// StartSpan 见 Tracer 接口。parentSpanID 为空串 → 开新 trace（任务级根 span）；
// metadata 里带非空 "model" 字符串 → 这个 observation 是 LLM 调用（generation），
// 否则是普通 span。
func (l *Langfuse) StartSpan(_ context.Context, parentSpanID, name string, metadata map[string]any) (spanID string) {
	spanID = newUUID()
	now := nowString()

	l.mu.Lock()
	defer l.mu.Unlock()

	var traceID, parentObsID string
	if parentSpanID == "" {
		// 根 span：新开一个 trace。trace-create 与 span-create 是两个独立事件——
		// trace 是"这次任务"的容器，根 span 是容器里的第一个 observation。
		traceID = newUUID()
		l.emit(ingestionEvent{
			ID: newUUID(), Type: "trace-create", Timestamp: now,
			Body: map[string]any{"id": traceID, "name": name, "timestamp": now},
		})
	} else if parent, ok := l.spans[parentSpanID]; ok {
		traceID, parentObsID = parent.traceID, parentSpanID
	} else {
		// 父 span 不存在（比如父进程重启后状态丢了）：宁可自开新 trace 也不丢事件。
		// 层级断了的 trace 还能按时间查，事件丢了就永远没了。
		traceID = newUUID()
		l.emit(ingestionEvent{
			ID: newUUID(), Type: "trace-create", Timestamp: now,
			Body: map[string]any{"id": traceID, "name": name + " (orphan)", "timestamp": now},
		})
	}

	model, _ := metadata["model"].(string)
	kind := "span"
	if model != "" {
		kind = "generation"
	}
	l.spans[spanID] = &spanState{traceID: traceID, kind: kind, model: model}

	body := map[string]any{
		"id":        spanID,
		"traceId":   traceID,
		"name":      name,
		"startTime": now,
	}
	if parentObsID != "" {
		body["parentObservationId"] = parentObsID // 嵌套层级就靠这个字段表达
	}
	if len(metadata) > 0 {
		body["metadata"] = metadata
	}
	typ := "span-create"
	if kind == "generation" {
		typ = "generation-create"
		body["model"] = model
	}
	l.emit(ingestionEvent{ID: newUUID(), Type: typ, Timestamp: now, Body: body})
	return spanID
}

// EndSpan 见 Tracer 接口。发 span-update / generation-update 事件补 endTime；
// generation 额外带 usage 与客户端算好的成本；err 非 nil 时标 level=ERROR。
// 对未 Start 过的 spanID 直接忽略——观测永远不能影响主流程。
func (l *Langfuse) EndSpan(_ context.Context, spanID string, inputTokens, outputTokens int, err error) {
	now := nowString()

	l.mu.Lock()
	defer l.mu.Unlock()

	st, ok := l.spans[spanID]
	if !ok {
		return
	}
	delete(l.spans, spanID)

	body := map[string]any{
		"id":      spanID,
		"traceId": st.traceID,
		"endTime": now,
	}
	typ := "span-update"
	if st.kind == "generation" {
		typ = "generation-update"
		body["usage"] = map[string]any{
			"input":  inputTokens,
			"output": outputTokens,
			"total":  inputTokens + outputTokens,
		}
		// 成本在客户端算好直接上报：服务端聚合"这个任务花多少钱"时只是求和。
		body["calculatedCost"] = costUSD(st.model, inputTokens, outputTokens)
	}
	if err != nil {
		body["level"] = "ERROR"
		body["statusMessage"] = err.Error()
	}
	l.emit(ingestionEvent{ID: newUUID(), Type: typ, Timestamp: now, Body: body})
}

// Flush 把 buffer 里的事件一次性 POST 到 ingestion API。
// 取舍：上报失败后事件即丢弃（不重试、不放回 buffer）——观测数据的价值
// 低于主流程可靠性，为一批 trace 事件重试阻塞退出流程不划算；
// 真要保可以在这里把 events 放回去下次再试，代价是内存只涨不跌的风险。
func (l *Langfuse) Flush(ctx context.Context) error {
	l.mu.Lock()
	batch := l.events
	l.events = nil
	l.mu.Unlock()
	return l.post(ctx, batch)
}

// post 是实际上报，抽出来供 Flush 与进阶异步版的后台 goroutine 共用。
func (l *Langfuse) post(ctx context.Context, events []ingestionEvent) error {
	if len(events) == 0 {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"batch": events})
	if err != nil {
		return fmt.Errorf("trace: marshal batch: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		l.host+"/api/public/ingestion", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("trace: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(l.publicKey, l.secretKey)

	resp, err := l.http.Do(req)
	if err != nil {
		return fmt.Errorf("trace: post ingestion: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("trace: ingestion status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// newUUID 用 crypto/rand 生成 UUID v4。事件 id / spanID / traceID 都要求
// 全局唯一，手写十几行就够，不必为这个引第三方 uuid 库。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败是系统级故障（熵源枯竭），继续跑也没意义。
		panic(fmt.Sprintf("trace: crypto/rand: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// nowString 返回 UTC 的 RFC3339Nano 时间串。统一 UTC 是为了和服务器/其他
// 机器的 trace 对齐时区，排查跨机器问题时不用换算。
func nowString() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
```

### `internal/trace/trace_test.go`（新建，httptest 假服务器，不连真实 Langfuse）

```go
package trace

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// capturedEvent 是假服务器侧解码用的事件结构（body 留 map，按需断言字段）。
type capturedEvent struct {
	ID   string         `json:"id"`
	Type string         `json:"type"`
	Body map[string]any `json:"body"`
}

// fakeLangfuse 起一个假 ingestion 服务：校验 Basic Auth，攒下所有事件。
type fakeLangfuse struct {
	srv *httptest.Server
	mu  sync.Mutex
	// events 按收到顺序摊开（跨多次 POST 合并），断言时按 type 过滤。
	events []capturedEvent
}

func newFakeLangfuse(t *testing.T, wantPK, wantSK string) *fakeLangfuse {
	t.Helper()
	f := &fakeLangfuse{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/ingestion" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		// Basic Auth 断言：Langfuse 用 publicKey:secretKey 做认证。
		pk, sk, ok := r.BasicAuth()
		if !ok || pk != wantPK || sk != wantSK {
			t.Errorf("BasicAuth = %q:%q (ok=%v), want %q:%q", pk, sk, ok, wantPK, wantSK)
		}
		// 顺便确认 Content-Type 与手工构造的 Authorization 头格式无误。
		if got := r.Header.Get("Authorization"); got != "Basic "+base64.StdEncoding.EncodeToString([]byte(wantPK+":"+wantSK)) {
			t.Errorf("Authorization header = %q", got)
		}
		var batch struct {
			Batch []capturedEvent `json:"batch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("decode batch: %v", err)
		}
		f.mu.Lock()
		f.events = append(f.events, batch.Batch...)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"successes":[],"errors":[]}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// byType 过滤出指定类型的事件。
func (f *fakeLangfuse) byType(typ string) []capturedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []capturedEvent
	for _, e := range f.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// TestLangfuse_NestedTraceAndUsage 是主验收用例：
// 模拟"任务 → 子任务 → 一次 LLM 调用"三层调用，Flush 后断言上报结构。
func TestLangfuse_NestedTraceAndUsage(t *testing.T) {
	fk := newFakeLangfuse(t, "pk-test", "sk-test")
	tr := NewLangfuse(fk.srv.URL, "pk-test", "sk-test")
	ctx := context.Background()

	root := tr.StartSpan(ctx, "", "task: 写周报", nil)
	sub := tr.StartSpan(ctx, root, "worker-1", nil)
	llmCall := tr.StartSpan(ctx, sub, "llm-call", map[string]any{"model": "deepseek-chat"})
	tr.EndSpan(ctx, llmCall, 1_000_000, 500_000, nil) // 用整百万方便核对成本
	tr.EndSpan(ctx, sub, 0, 0, nil)
	tr.EndSpan(ctx, root, 0, 0, errors.New("critic rejected"))

	if err := tr.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// 1. trace-create 恰好一次（一次任务一个 trace）。
	traces := fk.byType("trace-create")
	if len(traces) != 1 {
		t.Fatalf("trace-create count = %d, want 1", len(traces))
	}
	traceID, _ := traces[0].Body["id"].(string)
	if traceID == "" {
		t.Fatal("trace-create body missing id")
	}

	// 2. span-create 两次（root + worker），嵌套关系正确。
	spans := fk.byType("span-create")
	if len(spans) != 2 {
		t.Fatalf("span-create count = %d, want 2", len(spans))
	}
	byID := map[string]capturedEvent{}
	for _, e := range spans {
		id, _ := e.Body["id"].(string)
		byID[id] = e
		if tid, _ := e.Body["traceId"].(string); tid != traceID {
			t.Errorf("span %v traceId = %v, want %v", id, tid, traceID)
		}
	}
	rootSpan, ok := byID[root]
	if !ok {
		t.Fatalf("root span-create not found (id=%s)", root)
	}
	if _, hasParent := rootSpan.Body["parentObservationId"]; hasParent {
		t.Error("root span 不应有 parentObservationId")
	}
	subSpan, ok := byID[sub]
	if !ok {
		t.Fatalf("sub span-create not found (id=%s)", sub)
	}
	if got, _ := subSpan.Body["parentObservationId"].(string); got != root {
		t.Errorf("sub span parentObservationId = %q, want root spanID %q", got, root)
	}

	// 3. LLM 调用是 generation，挂在 worker span 下，带 model。
	gens := fk.byType("generation-create")
	if len(gens) != 1 {
		t.Fatalf("generation-create count = %d, want 1", len(gens))
	}
	if got, _ := gens[0].Body["parentObservationId"].(string); got != sub {
		t.Errorf("generation parentObservationId = %q, want sub spanID %q", got, sub)
	}
	if got, _ := gens[0].Body["model"].(string); got != "deepseek-chat" {
		t.Errorf("generation model = %q", got)
	}

	// 4. generation-update 带 usage 与客户端算出的成本。
	genUps := fk.byType("generation-update")
	if len(genUps) != 1 {
		t.Fatalf("generation-update count = %d, want 1", len(genUps))
	}
	usage, _ := genUps[0].Body["usage"].(map[string]any)
	if usage["input"] != 1_000_000.0 || usage["output"] != 500_000.0 || usage["total"] != 1_500_000.0 {
		t.Errorf("usage = %v", usage)
	}
	// 成本：1M input × $0.27 + 0.5M output × $1.10 = 0.27 + 0.55 = $0.82。
	// 浮点运算用容差比较（0.27+0.55 在 float64 下是 0.8200000000000001）。
	if cost, _ := genUps[0].Body["calculatedCost"].(float64); cost < 0.8199 || cost > 0.8201 {
		t.Errorf("calculatedCost = %v, want ≈0.82", cost)
	}

	// 5. root span 以 error 结束 → span-update 标 ERROR。
	var rootUpdate *capturedEvent
	for _, e := range fk.byType("span-update") {
		if id, _ := e.Body["id"].(string); id == root {
			ev := e // 拷贝，避免指向循环变量
			rootUpdate = &ev
		}
	}
	if rootUpdate == nil {
		t.Fatal("root span-update not found")
	}
	if got, _ := rootUpdate.Body["level"].(string); got != "ERROR" {
		t.Errorf("root span-update level = %q, want ERROR", got)
	}
	if _, ok := rootUpdate.Body["endTime"]; !ok {
		t.Error("span-update 缺 endTime")
	}
}

// TestLangfuse_FlushEmptyNoRequest：没有事件时 Flush 不应发请求。
func TestLangfuse_FlushEmptyNoRequest(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	t.Cleanup(srv.Close)

	tr := NewLangfuse(srv.URL, "pk", "sk")
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if hits != 0 {
		t.Errorf("server hits = %d, want 0（空 Flush 不该发请求）", hits)
	}
}

// TestLangfuse_EndSpanUnknownIDIgnored：对未 Start 的 span 调 EndSpan 必须静默忽略。
func TestLangfuse_EndSpanUnknownIDIgnored(t *testing.T) {
	fk := newFakeLangfuse(t, "pk", "sk")
	tr := NewLangfuse(fk.srv.URL, "pk", "sk")
	tr.EndSpan(context.Background(), "never-started", 1, 1, nil)
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(fk.byType("span-update")) + len(fk.byType("generation-update")); got != 0 {
		t.Errorf("update events = %d, want 0", got)
	}
}

// TestLangfuse_IngestionErrorSurfaces：假服务器 401 时 Flush 必须返回 error。
func TestLangfuse_IngestionErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid credentials"}`))
	}))
	t.Cleanup(srv.Close)

	tr := NewLangfuse(srv.URL, "bad", "bad")
	root := tr.StartSpan(context.Background(), "", "t", nil)
	tr.EndSpan(context.Background(), root, 0, 0, nil)
	if err := tr.Flush(context.Background()); err == nil {
		t.Fatal("want error from Flush, got nil")
	}
}

// TestCostUSD：单价表的纯函数测试——成本算错的回归由它兜底。
func TestCostUSD(t *testing.T) {
	if got := costUSD("deepseek-chat", 1_000_000, 0); got != 0.27 {
		t.Errorf("input-only cost = %v, want 0.27", got)
	}
	if got := costUSD("deepseek-chat", 0, 1_000_000); got != 1.10 {
		t.Errorf("output-only cost = %v, want 1.10", got)
	}
	if got := costUSD("unknown-model", 1_000_000, 1_000_000); got != 0 {
		t.Errorf("unknown model cost = %v, want 0", got)
	}
}
```

## 二、关键设计点

1. **为什么 span 层级要对应 agent 层级**：trace 的价值全在"下钻"。一次任务 = 一个 trace，planner = 根 span，worker = 子 span，单次 LLM 调用 / 工具执行 = 更深层 span——排查路径与系统结构同构：任务超时了 → 看哪个 worker span 最长 → 再下钻到它内部哪次 LLM 调用慢；账单超了 → 按 span 聚合 token，直接定位"worker-3 花了 80% 的钱"。如果层级乱挂（比如所有 LLM 调用平铺在 trace 下），"哪个子任务最贵"就回答不了，trace 退化成按时间排序的 log。**易错处**：层级靠 `parentObservationId` 一个字段表达，StartSpan 时忘了从父 span 继承 `traceId`，事件就挂到了别的 trace（或孤儿 trace）上。

2. **为什么 cost 在客户端算**：① 实时性——"预算熔断"（阶段三成本控制四招之一）要在任务进行中就知道花了多少钱，等服务商账单是小时/天级延迟，熔断早就晚了；② 服务端（Langfuse）也能配单价算成本，但单价表维护在后端 UI 里，与代码里用的模型容易脱节，客户端算则"用哪个模型、按什么价"在一处；③ 客户端算完只是个数，服务端聚合（按 trace/span 求和）不依赖它怎么算出来的。**DeepSeek 单价的坑**：官方价区分缓存命中/未命中——deepseek-chat input 未命中 $0.27/1M、命中 $0.07/1M（约 1/4 价差）；多 agent 系统里 planner 的 system prompt 前缀大量命中缓存，不区分会把成本高估数倍。本答案用简化的未命中单价（注释里已标明），生产应读 usage 里的 cached tokens 字段分别计价。

3. **同步上报 vs 缓冲批量上报的取舍**：同步（每个 span 结束就 POST）实现最简、崩溃不丢数据，但 agent 主流程的每次 LLM 调用都多背一次网络往返的延迟与失败面——观测拖累业务，本末倒置。缓冲批量（本答案基础版：StartSpan/EndSpan 只 append，Flush 一次 POST）把上报成本摊到接近零，代价是崩溃丢 buffer、长任务运行期间看板上看不到数据。进阶版的答案（第三节）是业界标准做法：内存队列 + 后台 goroutine 定时批量 + 队列上限丢弃。**面试口径**：可观测系统的第一原则是"观测不能影响被观测对象"——延迟、可靠性、资源占用都要隔离。

4. **span 与 generation 的区分方式**：Langfuse 里 generation 是带 model/usage 的特殊 observation，成本核算只认它。本答案用 `metadata["model"]` 是否存在来判定 kind——不改动 Tracer 接口（接口是编排器与后端之间的契约，加一个 `StartGeneration` 方法会让 Noop 和将来的每个实现都多一个方法）。代价是判定约定隐式，靠接口注释说明。**易错处**：token 记在普通 span 上而不是 generation 上，成本聚合就漏了。

5. **观测不影响主流程**：`StartSpan`/`EndSpan` 不 panic、不阻塞、不返回 error；`EndSpan` 对未知 spanID 静默忽略（调用顺序错乱是调用方的 bug，但不该用崩溃惩罚）；`Flush` 才返回 error 由调用方决定（通常只 log 不 abort）。上报失败后事件直接丢弃而不是放回 buffer 重试——为观测数据冒"内存只涨不跌"的风险不值得，这个取舍和阶段一"工具输出截断"是同一思想：防御性边界。

6. **`emit` 字段的抽法**：事件出口抽成一个函数字段（默认 append 到 buffer），进阶异步版只换掉这个字段就复用全部事件构造逻辑，不重写 `StartSpan`/`EndSpan`。这是"开闭原则"的最小实践：扩展（异步）不改已有代码，只替换一个依赖。

## 三、进阶实现（加分项：异步批量上报）

> 回补记录：本节代码于 2026-08-14 以临时文件（`internal/trace/langfuse_async.go` +
> `langfuse_async_test.go`）实际粘贴进项目验证，`go vet ./internal/trace/` 与
> `go test ./internal/trace/ -count=1` 全部通过（8 个测试：基础 5 个 + 进阶 3 个），
> 异步 3 个测试 `-count=5` 连跑无 flake；验证后已从项目删除——
> **进阶实现只属于答案，不进项目代码树**，项目保持骨架版。

### 设计取舍（先说清楚再看代码）

- **解决什么问题**：基础版 buffer + 退出前 Flush 有两个硬伤——长进程不退出就永不上报（看板看不到进行中的任务）；buffer 无上限，长任务内存只涨。异步版：内存队列 + 后台 goroutine 定时批量 POST + 队列上限满则丢。
- **丢弃策略选"丢新事件"**：队列满时 `select default` 直接丢新事件并计数（`Dropped()` 暴露给监控）。不选"丢最旧"——实现零额外复杂度，且连续超限说明后端已故障，旧事件价值同样有限。绝不阻塞主流程等队列。
- **Flush 语义保持一致**："返回时，调用前已提交的事件要么已上报、要么已丢弃"。实现上 Flush 给后台 goroutine 发带 ack channel 的请求，goroutine 先 drain 队列再 POST 再回 ack——**易错处**：不 drain 直接 POST pending，队列里还有事件没进 pending，Flush 返回后它们才慢慢出去，语义就破了。
- **复用方式**：嵌入 `*Langfuse` 复用事件构造，只换掉 `emit` 字段为 channel 发送；`Flush` 遮蔽基类同名方法。进程退出路径：`Flush`（确保数据出去）→ `Close`（停 goroutine，退出前兜底再发一次）。

### `internal/trace/langfuse_async.go`（进阶实现完整代码）

```go
package trace

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncLangfuse 异步批量上报版 Tracer：StartSpan/EndSpan 只把事件塞进内存队列
// （非阻塞，满了就丢并计数），后台 goroutine 按 FlushInterval 定时批量 POST，
// Flush 会立即触发一次上报并等待完成。
//
// 相比基础版（buffer + 退出前 Flush）解决两个问题：
//   - 长进程不退出就永远不上报，Langfuse 看板上看不到进行中的任务；
//   - buffer 无上限，长任务可能攒出可观内存。
//
// 代价：多一个 goroutine 的生命周期要管理（Close），以及队列满时的丢弃策略。
type AsyncLangfuse struct {
	*Langfuse // 嵌入复用 StartSpan/EndSpan 的事件构造；覆盖 Flush

	dropped  atomic.Int64    // 因队列满被丢弃的事件数（暴露给监控/日志）
	flushReq chan chan error // Flush 请求：带一个回传结果的 ack channel
	done     chan struct{}
	wg       sync.WaitGroup
}

// NewAsyncLangfuse 创建异步版 Tracer。
// queueSize 是内存队列上限；flushInterval 是后台批量上报间隔。
func NewAsyncLangfuse(host, publicKey, secretKey string, queueSize int, flushInterval time.Duration) *AsyncLangfuse {
	queue := make(chan ingestionEvent, queueSize)
	a := &AsyncLangfuse{
		Langfuse: NewLangfuse(host, publicKey, secretKey),
		flushReq: make(chan chan error),
		done:     make(chan struct{}),
	}
	// 换掉事件出口：不再 append 到 buffer，而是非阻塞塞进队列。
	// 【丢弃策略】队列满 → 丢新事件 + 计数。观测数据宁可丢也不能阻塞 agent 主流程——
	// 一次 LLM 调用被 trace 上报卡住，是本末倒置。选"丢新"而不是"丢最旧"：
	// 实现零额外复杂度，且连续超限说明后端已故障，旧事件价值也有限。
	a.Langfuse.emit = func(ev ingestionEvent) {
		select {
		case queue <- ev:
		default:
			a.dropped.Add(1)
		}
	}
	a.wg.Add(1)
	go a.loop(queue, flushInterval)
	return a
}

// loop 是后台上报 goroutine：攒事件 → 定时/被 Flush 触发/收到 Close 时批量 POST。
func (a *AsyncLangfuse) loop(queue chan ingestionEvent, interval time.Duration) {
	defer a.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var pending []ingestionEvent
	// drain 把队列里当前能拿的全拿进 pending（Flush 前调用，保证"已提交的事件都出去了"）。
	drain := func() {
		for {
			select {
			case ev := <-queue:
				pending = append(pending, ev)
			default:
				return
			}
		}
	}
	send := func() error {
		if len(pending) == 0 {
			return nil
		}
		// 后台 goroutine 没有调用方的 ctx，用 Background 兜底；
		// http.Client 自带 Timeout，不会无限挂住。
		err := a.post(context.Background(), pending)
		pending = nil // 成败都清：与基础版一致，观测失败不重试
		return err
	}

	for {
		select {
		case ev := <-queue:
			pending = append(pending, ev)
		case <-ticker.C:
			_ = send() // 定时上报失败只丢事件，无处返回错误，可接日志
		case ack := <-a.flushReq:
			drain()
			ack <- send()
		case <-a.done:
			drain()
			_ = send() // 退出前兜底上报一次
			return
		}
	}
}

// Flush 覆盖基类：立即触发后台上报并等待结果（尊重 ctx 取消）。
// 语义与基础版一致——返回时，调用前已提交的事件要么已上报、要么已丢弃。
func (a *AsyncLangfuse) Flush(ctx context.Context) error {
	ack := make(chan error, 1)
	select {
	case a.flushReq <- ack:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close 停止后台 goroutine（会先兜底上报一次）。进程退出路径：
// Flush（确保数据出去）→ Close（回收 goroutine）。
func (a *AsyncLangfuse) Close() {
	close(a.done)
	a.wg.Wait()
}

// Dropped 返回因队列满被丢弃的事件总数，给监控/日志用。
func (a *AsyncLangfuse) Dropped() int64 {
	return a.dropped.Load()
}
```

### `internal/trace/langfuse_async_test.go`（进阶测试完整代码）

三个测试要点：① 不调 Flush，ticker 也自动上报（事件顺序 = trace-create/span-create/span-update）；
② 队列满丢新事件并计数——确定性构造见注释；③ Flush 返回前队列里的事件必须已上报。

```go
package trace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestAsyncLangfuse_TickerAutoFlush：不调 Flush，后台 goroutine 也按 interval 自动上报。
func TestAsyncLangfuse_TickerAutoFlush(t *testing.T) {
	var mu sync.Mutex
	var got []capturedEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			Batch []capturedEvent `json:"batch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		got = append(got, batch.Batch...)
		mu.Unlock()
		w.Write([]byte(`{"successes":[]}`))
	}))
	t.Cleanup(srv.Close)

	tr := NewAsyncLangfuse(srv.URL, "pk", "sk", 64, 20*time.Millisecond)
	t.Cleanup(tr.Close)

	ctx := context.Background()
	root := tr.StartSpan(ctx, "", "task", nil)
	tr.EndSpan(ctx, root, 0, 0, nil)

	// 不 Flush，轮询等后台 goroutine 上报（最多 2s）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 { // trace-create + span-create + span-update = 3 个事件
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("2s 内只收到 %d 个事件，后台自动上报未生效", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (trace-create/span-create/span-update)", len(got))
	}
	var types []string
	for _, e := range got {
		types = append(types, e.Type)
	}
	want := []string{"trace-create", "span-create", "span-update"}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("events[%d].Type = %q, want %q（全列表 %v）", i, types[i], want[i], types)
		}
	}
}

// TestAsyncLangfuse_DropWhenQueueFull：队列满时丢新事件并计数。
// 确定性构造：queueSize=0（无缓冲队列，loop 不在 select 等待时发送必失败）；
// 先让 loop 发出第一个 POST 并被假服务器卡住响应（用 connected channel 确认
// loop 确实已阻塞在 HTTP 里），此刻发送的事件必然全部被丢。
func TestAsyncLangfuse_DropWhenQueueFull(t *testing.T) {
	release := make(chan struct{})
	connected := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(connected) }) // 第一次请求到达 = loop 已卡在 post
		<-release
		w.Write([]byte(`{"successes":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-release: // 已关闭则跳过（channel 只能 close 一次）
		default:
			close(release)
		}
	})

	tr := NewAsyncLangfuse(srv.URL, "pk", "sk", 0, 10*time.Millisecond)
	t.Cleanup(tr.Close)

	// 发 span 直到 loop 把它上报出去并卡在假服务器的响应上。
	// 无缓冲队列下单次发送可能恰逢 loop 在处理 tick 而被丢，重试几次必然成功——
	// loop 绝大部分时间都停在 select 等待上。
	deadline := time.After(2 * time.Second)
waitBlocked:
	for {
		tr.StartSpan(context.Background(), "", "warmup", nil)
		select {
		case <-connected:
			break waitBlocked
		case <-deadline:
			t.Fatal("2s 内 loop 未发出第一次上报")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// 此刻 loop 确定阻塞在 post：无缓冲队列无人接收，4 个事件必然全丢。
	before := tr.Dropped()
	tr.StartSpan(context.Background(), "", "t1", nil) // 2 个事件
	tr.StartSpan(context.Background(), "", "t2", nil) // 2 个事件
	if got := tr.Dropped() - before; got != 4 {
		t.Errorf("Dropped delta = %d, want 4", got)
	}

	close(release) // 放行：卡住的 post 与 Close 的兜底上报都能完成
}

// TestAsyncLangfuse_FlushDrainsQueue：Flush 返回前，队列里已提交的事件必须已上报。
func TestAsyncLangfuse_FlushDrainsQueue(t *testing.T) {
	var mu sync.Mutex
	var got []capturedEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			Batch []capturedEvent `json:"batch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&batch)
		mu.Lock()
		got = append(got, batch.Batch...)
		mu.Unlock()
		w.Write([]byte(`{"successes":[]}`))
	}))
	t.Cleanup(srv.Close)

	// interval 很大，排除定时上报干扰——只有 Flush 能触发上报。
	tr := NewAsyncLangfuse(srv.URL, "pk", "sk", 64, time.Hour)
	t.Cleanup(tr.Close)

	ctx := context.Background()
	root := tr.StartSpan(ctx, "", "task", nil)
	tr.EndSpan(ctx, root, 0, 0, nil)
	if err := tr.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Errorf("Flush 返回后收到 %d 个事件，want 3（Flush 必须把队列抽干）", len(got))
	}
}
```

### 进阶实现的易错处

1. **Flush 忘记 drain 队列**：只 POST `pending` 会把还在 channel 里的事件留在 Flush 之后，破坏"Flush 返回 = 已提交事件已出去"的语义——测试 `FlushDrainsQueue`（interval 设为 1 小时排除定时上报干扰）就是抓这个的。
2. **丢弃策略写成阻塞发送**：`queue <- ev` 不带 default，后端一故障所有 agent goroutine 全卡在 trace 上——观测故障升级为业务故障，可观测系统最忌讳的反模式。
3. **后台 goroutine 用调用方 ctx**：Flush 的 ctx 一取消，post 就中断；后台 goroutine 应该用 `context.Background()` + http.Client 自带 Timeout 兜底。
4. **丢弃场景的测试靠 sleep 猜时序**：第一版实现用"sleep 100ms 假设 loop 已进入 post"，既慢又 flaky。确定性做法：用 `connected` channel 确认 loop 已阻塞在 HTTP 里，再用无缓冲队列让后续发送必败。并发测试的通用原则：**用同步原语（channel）表达时序前提，不用 sleep 赌**。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [x] `NewLangfuse(host, publicKey, secretKey)` 返回的实现满足 `Tracer` 接口，有 `var _ Tracer = (*Langfuse)(nil)` 编译期断言
- [x] 根 span 开新 trace（trace-create 一次任务恰好一次），子 span 继承父的 traceId，嵌套用 `parentObservationId` 表达
- [x] LLM 调用上报为 generation（带 model），EndSpan 时 usage（input/output/total）写进 generation-update
- [x] 成本在客户端按单价表算出并上报；注释里说明了 DeepSeek 缓存命中/未命中的价差与本实现的简化
- [x] 错误结束的 span 标 `level=ERROR`；未 Start 的 spanID 调 EndSpan 不 panic
- [x] httptest 假服务器测试覆盖：嵌套层级、usage、Basic Auth、空 Flush 不发请求、上报失败 Flush 返回 error
- [x] `go vet ./internal/trace/` 与 `go test ./internal/trace/ -count=1` 全绿
- [x] 能口头回答：trace 和 log 的区别？为什么 cost 要在客户端算？同步上报和缓冲批量各自的代价？

加分项（做了才需要勾，参考"进阶实现"一节）：

- [x] 异步批量上报：内存队列 + 定时 Flush goroutine + 队列上限丢新事件并计数（`Dropped()`）
- [x] Flush 语义不破：返回前已提交的事件都已上报（测试用超长 interval 隔离定时上报验证）
- [x] 能口头回答：为什么丢弃策略选"丢新"而不是"丢最旧"或阻塞？后台 goroutine 为什么不能用调用方的 ctx？
