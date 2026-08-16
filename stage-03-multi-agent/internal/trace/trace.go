// Package trace 提供多 agent 系统的可观测性抽象：嵌套 trace（调用树）上报。
//
// 在整个 agent 链路中的位置：编排器（planner / worker / critic）每推进一步，
// 就通过本包的 Tracer 接口记录一个 span；span 的父子层级与 agent 层级一一对应——
// 一次任务一个 trace，planner 一个根 span，每个 worker 一个子 span，
// worker 内部每次 LLM 调用、每次工具执行是更深层 span。
// 每个 span 记 token 用量，事后才能回答"这个任务花了多少钱、哪个子任务最贵"——
// 成本优化（模型分级、砍 prompt、预算熔断）全部依赖这份数据驱动。
//
// 【面试考点】trace 和 log 的区别：
//   - log 是离散事件点（"发生了什么"），彼此之间没有结构关系；
//   - trace 是有父子层级的调用树（span 嵌套，每个 span 有开始/结束时间），
//     回答"哪个环节慢 / 哪个环节贵"。
//     排查"worker-3 为什么花了 80% 的 token"靠 trace；
//     排查"worker-3 当时收到了什么输入"靠 log。两者互补，不是替代关系。
//
// 为什么设计成接口：编排器只依赖 Tracer 接口，不依赖任何具体后端。
// 本地开发/单测用 Noop（零开销、零依赖）；接 Langfuse 只是换一个实现，
// 编排器代码一行不改。这也是"面向接口编程"在基础设施层的标准用法——
// 依赖注入的可观测性，和 mini-agent 里 Tool 接口是同一个思想。
package trace

import "context"

// Tracer 是可观测性后端的抽象。实现方：Noop（本地）、Langfuse（练习6）。
//
// 使用约定（编排器侧）：
//
//	root := tr.StartSpan(ctx, "", "task: 写周报", nil)      // 任务级根 span
//	sub  := tr.StartSpan(ctx, root, "worker-1", nil)        // 子任务 span
//	llm  := tr.StartSpan(ctx, sub, "llm-call", map[string]any{"model": "deepseek-chat"})
//	resp, err := llmClient.Chat(...)
//	tr.EndSpan(ctx, llm, resp.Usage.Input, resp.Usage.Output, err) // LLM 调用记 token
//	tr.EndSpan(ctx, sub, 0, 0, nil)
//	tr.EndSpan(ctx, root, 0, 0, nil)
//	tr.Flush(ctx) // 进程退出前
type Tracer interface {
	// StartSpan 开启一个 span，parentSpanID 为空串表示根 span（任务级）。
	// 返回 spanID 供 EndSpan 与作为子 span 的 parent 使用。
	StartSpan(ctx context.Context, parentSpanID, name string, metadata map[string]any) (spanID string)
	// EndSpan 结束 span，记录 token 用量与错误。
	EndSpan(ctx context.Context, spanID string, inputTokens, outputTokens int, err error)
	// Flush 进程退出前调用，确保缓冲的事件都上报了。
	Flush(ctx context.Context) error
}

// Noop 是什么都不做的 Tracer：本地开发与单测的默认值。
// 它的存在让"不接观测后端"成为一个显式选择，而不是 nil 判断散落各处。
type Noop struct{}

// NewNoop 返回空实现的 Tracer。
func NewNoop() *Noop { return &Noop{} }

// StartSpan 返回空 spanID（调用方传回 EndSpan 也不会有任何效果）。
func (n *Noop) StartSpan(_ context.Context, _, _ string, _ map[string]any) string { return "" }

// EndSpan 空实现。
func (n *Noop) EndSpan(_ context.Context, _ string, _, _ int, _ error) {}

// Flush 空实现。
func (n *Noop) Flush(_ context.Context) error { return nil }

// 编译期断言：Noop 必须始终满足 Tracer 接口——接口加方法时这里立刻报错，
// 比运行时才发现"某个实现漏了方法"早得多。
var _ Tracer = (*Noop)(nil)

// ============================ 练习区（由学习者完成） ============================
//
// TODO(练习6): Langfuse Tracer —— 嵌套 trace 上报 + token 成本核算
//
// 【任务】新建本包内文件 langfuse.go，实现：
//
//	func NewLangfuse(host, publicKey, secretKey string) *Langfuse
//
// 让 *Langfuse 满足 Tracer 接口（建议同样写 var _ Tracer = (*Langfuse)(nil) 编译期断言），
// 通过 Langfuse 公开 ingestion API 上报事件：
//   - POST {host}/api/public/ingestion，Basic Auth = publicKey:secretKey；
//   - body 是 {"batch": [事件...]}，批量上报；
//   - 事件类型：trace-create / span-create / span-update /
//     generation-create / generation-update；
//   - 一次任务 = 一个 trace；span 嵌套关系用 body 里的 parentObservationId 表达；
//   - 每次 LLM 调用对应一个 generation（StartSpan 的 metadata 里带 "model" 即视为
//     LLM 调用），EndSpan 时把 token 用量写进 generation-update 的 usage 字段；
//   - 按模型单价表在客户端算出 cost 一并上报；
//   - 进程退出前 Flush，把缓冲的事件发出去。
//
// 【提示】
//   - Langfuse ingestion API 的事件结构：每个事件是
//     {id, type, timestamp, body}；body 里关键字段：
//     traceId（属于哪个 trace）、id（observation 自己的 ID）、
//     parentObservationId（父 span ID，根 span 不带）、
//     startTime / endTime（RFC3339）、usage（{input, output, total}）。
//   - 事件 id / spanID / traceID 用 crypto/rand 生成 UUID v4（手写 20 行以内，
//     不需要引入第三方 uuid 库），或用递增计数也可以——但要保证全局唯一。
//   - 层级关系的维护：StartSpan 时 parentSpanID 为空 → 新建 traceID 并发
//     trace-create；非空 → 查父 span 的 traceID 继承下来。所以 Langfuse 结构体里
//     需要一个 map[spanID] -> (traceID, kind)，EndSpan 后删除。
//   - 成本 = 按模型单价表计算 input/output token 费用。DeepSeek 官方价区分
//     缓存命中/未命中（命中价约为未命中的 1/4），练习里可用简化的未命中单价，
//     但要在注释里说明这个简化。成本放 body 的自定义字段（如 calculatedCost）。
//   - 同步上报还是缓冲批量上报，是个有意的取舍点（想想各自代价），
//     基础版建议：StartSpan/EndSpan 只攒事件到内存 buffer，Flush 一次性 POST。
//   - 上报失败不应影响 agent 主流程：Flush 返回 error 让调用方决定，
//     StartSpan/EndSpan 绝不 panic、绝不阻塞。
//
// 【验收】写 trace_test.go，用 httptest.NewServer 起假 Langfuse：
//   - 模拟"任务 → 子任务 → 一次 LLM 调用"的三层调用后 Flush；
//   - 断言假服务器收到的 batch：trace-create 恰好一次；span 嵌套关系
//     parentObservationId 正确；generation 带 usage（input/output 数值对）；
//     请求头 Basic Auth 正确；
//   - go vet ./internal/trace/ 与 go test ./internal/trace/ 全绿。
//
// 参考答案：docs/solutions/stage-03/exercise-6-langfuse-trace.md（完成后再看）
