// Package pool 是多 Agent 编排系统的并发底座：一个并发度受限、单任务带超时预算的 worker pool。
//
// 它在 agent 链路中的位置：编排器（orchestrator，练习3）拿到 planner 分解出的子任务列表后，
// 调用本包并行执行这些子任务，再把结果汇总给 critic / 汇总节点。
// 即：planner 分解 → 【pool 并行执行 worker 子任务】→ 汇总/评审。
//
// 为什么并发度必须受限：worker 子任务本质是 LLM API 调用，DeepSeek 等服务商有 rate limit（QPS/并发数），
// 不限并发会立刻撞上 429；同时本地 goroutine、连接数也是资源。
// 限流要限在源头（semaphore 控制同时开工的 job 数），比事后靠重试退避便宜得多——
// 见阶段文档 3.1 Q11 的"三道防线"。
//
// 为什么选"部分失败"语义，而不是 errgroup.WithContext 默认的"一错全停"：
// errgroup.WithContext 派生的 ctx 会在任一 goroutine 返回错误时取消，其余 worker 被中止——
// 这对"一组强关联任务"是对的，但编排场景里三个调研子任务挂了一个，
// 另外两个的结论仍然有价值、要进汇总报告（阶段文档注意事项第 1 条）。
// 所以本包让"失败"成为结果值（Result.Err），由编排器统计成败、决定降级或重试，
// 而不是让一次失败拖垮整批任务。
//
// 练习：本包除 Pool.Run 外无需学习者完成的部分（Job/Result 类型、New 构造函数均已完整提供）。
package pool

import (
	"context"
	"time"
)

// Job 是一个待执行的子任务。
//
// Run 签名为什么要接收 ctx 并返回 (string, error)：
// worker 子任务的执行体是 LLM 调用/工具调用，必须能被取消（超时、父任务取消），
// 所以 ctx 由 pool 派生好（带超时预算）传进来，Job 内部所有阻塞操作都要 select ctx.Done()；
// 返回值设计成 (string, error) 而非泛型，是因为编排层的子任务产出统一是文本结论
// （LLM 生成内容），保持简单，不为想象中的需求提前抽象。
type Job struct {
	// ID 由编排器分配（如 "subtask-3"），用于把结果对应回子任务、写 checkpoint、trace 归因。
	ID string
	// Run 是子任务执行体。约定：必须响应 ctx 取消（ctx.Err() != nil 时尽快返回）；
	// 返回的 error 会被收进 Result.Err，不会中断其他 job。
	Run func(ctx context.Context) (string, error)
}

// Result 是一个子任务的最终结局：要么 Value 有值，要么 Err 非空。
//
// 失败用值表达（Err 字段）而不是用 error 返回值表达，这正是"部分失败"语义的落地：
// Pool.Run 的签名里没有 error，因为"整批任务"不存在失败一说——失败永远属于单个 job。
type Result struct {
	ID    string // 与 Job.ID 对应
	Value string // 成功时的产出（Err 为 nil）
	Err   error  // 失败原因：业务错误、context.DeadlineExceeded（超时）、context.Canceled（父取消）
}

// Pool 是并发受限的 worker pool。零值不可用，必须用 New 构造。
type Pool struct {
	// maxConcurrent 是同时执行的 job 数上限（semaphore 容量），对齐 LLM API 的并发配额。
	maxConcurrent int
	// jobTimeout 是每个 job 的超时预算：pool 为每个 job 派生 context.WithTimeout(ctx, jobTimeout)。
	// 这是 context 预算分层的一环：任务总预算（上层 ctx）> 单 worker 预算（本字段）> 单次 LLM 调用预算（更下层）。
	jobTimeout time.Duration
}

// New 构造一个 Pool。
//
// maxConcurrent：并发上限，一般按 LLM 服务商的 rate limit 配额定（宁小勿大，429 的代价高于排队）。
// jobTimeout：单 job 超时预算，见 Pool.jobTimeout 的预算分层说明。
func New(maxConcurrent int, jobTimeout time.Duration) *Pool {
	return &Pool{maxConcurrent: maxConcurrent, jobTimeout: jobTimeout}
}

// TODO(练习1): errgroup + semaphore 的 worker pool
//
// 任务：实现
//
//	func (p *Pool) Run(ctx context.Context, jobs []Job) []Result
//
// 并行执行 jobs，契约四条：
//  1. 任意时刻并发执行的 job 数不超过 p.maxConcurrent（semaphore 限流）；
//  2. 每个 job 用 context.WithTimeout(ctx, p.jobTimeout) 派生自己的 ctx 再调用 job.Run，
//     注意 defer cancel() 防 context 泄漏；
//  3. 部分失败语义：单个 job 失败/超时不影响其他 job，错误收进对应 Result.Err；
//     返回的 results 与 jobs 顺序一致（results[i] 对应 jobs[i]），len(results) == len(jobs)；
//  4. ctx 被外部取消时尽快退出：已在跑的 job 靠派生 ctx 级联取消，排队中的 job 不应再实际执行。
//
// 提示：
//   - 用 errgroup.Group + SetLimit 最简（golang.org/x/sync/errgroup，先 go get 加入依赖）；
//     也可以 semaphore.Weighted 自己 Acquire/Release + sync.WaitGroup，体会两种写法差别；
//   - 结果写回预分配切片 results[i] 而不是 channel 收集：每个 goroutine 只写自己下标的槽位，
//      disjoint 写入无数据竞争，顺序天然与 jobs 一致，也不需要额外的 fan-in goroutine；
//   - 关键坑：errgroup.WithContext 是"一错全停"语义（任一 goroutine 返回 error 会取消派生 ctx），
//     与这里要的"部分失败"语义相反——所以 goroutine 要把 error 收进 results[i].Err 后 return nil，
//     绝不能让 error 逃出 goroutine（阶段文档注意事项第 1 条）；
//   - goroutine 闭包捕获循环变量 i / job 的正确姿势（Go 1.22+ 每轮新变量，旧版本需手动拷贝）。
//
// 验收：go test ./internal/pool/ 通过（并发上限、超时、部分失败三个测试）
//
// 参考答案：docs/solutions/stage-03/exercise-1-worker-pool.md（完成后再看）
func (p *Pool) Run(ctx context.Context, jobs []Job) []Result {
	panic("TODO(练习1): 待实现")
}
