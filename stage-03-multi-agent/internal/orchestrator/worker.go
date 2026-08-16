// worker.go —— 子任务执行体：每个子任务一个独立的 mini-agent 实例。
//
// 在编排链路中的位置：orchestrator 把 planner 分解出的子任务交给 pool 并发执行，
// pool 里每个 job 的执行体就是 Worker.Execute。
//
// 练习：本文件的类型定义与 NewAgentWorker 构造函数无需学习者完成；
// AgentWorker.Execute 为 TODO(练习3)。
package orchestrator

import (
	"context"
	"errors"

	"mini-agent/api"
)

// Worker 执行单个子任务。
//
// 为什么是接口（与 Planner 同一思想）：
//   - 可测：编排器测试注入假 Worker（脚本化产出与 token 数），不烧真实 API 额度；
//   - 模型分级（教程 Q5）：可以用便宜模型当 worker、强模型当 planner/critic，
//     换实现不换编排器；
//   - 返回 tokens 是为了成本核算与预算熔断（练习4）：编排器把它累加进
//     子任务 checkpoint 与任务总账。
type Worker interface {
	// Execute 执行一个子任务，返回产出文本与本次消耗的 token 数。
	// 约定：必须响应 ctx 取消（pool 会为每个 job 派生超时预算）。
	Execute(ctx context.Context, spec SubtaskSpec) (output string, tokens int, err error)
}

// AgentWorker 用 mini-agent 内核当执行体：每个子任务 new 一个 api.Agent。
//
// 为什么每个子任务都 new 一个 Agent，而不是共享一个（教程核心概念第 1 条）：
// 单 agent 的局限是 context 膨胀——所有子任务的对话历史堆在一份 messages 里，
// 噪声互相稀释注意力。每子任务一个新 Agent = 天然的 context 隔离：
// 每个 worker 只背自己的 system prompt + 子任务 prompt，跑完即弃。
// 这正是"多 agent 解决 context 膨胀"的落地方式。
type AgentWorker struct {
	// TODO(练习3) 要填的字段建议：
	//   client   *api.Client   —— LLM 客户端（可用 WithModel 换便宜模型做模型分级）
	//   registry *api.Registry —— worker 可用工具（Calculator/HTTPFetch/读写文件等）
}

// NewAgentWorker 构造 worker。registry 为 nil 时 worker 无工具可用（纯生成型子任务）。
func NewAgentWorker(client *api.Client, registry *api.Registry) *AgentWorker {
	return &AgentWorker{
		// TODO(练习3): 保存 client 与 registry
	}
}

// 编译期断言：AgentWorker 必须始终满足 Worker 接口。
var _ Worker = (*AgentWorker)(nil)

// TODO(练习3): AgentWorker.Execute —— 每个子任务 new 一个 mini-agent 跑完即弃
//
// 任务：实现 Worker 接口：
//
//	func (w *AgentWorker) Execute(ctx context.Context, spec SubtaskSpec) (string, int, error)
//
// 流程：开工前检查 ctx.Err() → 用 worker 专用 system prompt new 一个
// api.Agent → agent.Run(spec.Prompt) → 返回产出。
//
// 提示：
//   - worker 的 system prompt 要点（教程 Q5 第④招"砍 context"）：
//     明确告诉它"你只负责当前这一个子任务"，要求最终输出自包含的结论文本
//     （这段文本会作为子任务产出进汇总报告）。它拿不到用户原始目标，
//     也看不到其他子任务——不要让它揣测全局；
//   - registry 为 nil 时传一个空的 api.NewRegistry()，避免内核里 nil 解引用；
//   - token 记账：agent.Run 返回后读 agent.Usage().TotalTokens（本次 Run 所有
//     ReAct 轮次的累计量）作为返回值——这笔账会经 CompleteSubtask 落盘，
//     供练习4 的预算熔断与练习6 的成本观测消费。注意必须在 Run 之后读，
//     且每子任务 new 一个 Agent 天然不串账。
//
// 验收：编排器整体测试（假 Worker）通过；真实 AgentWorker 的行为在
// 练习8 集成演示时用真实 API 冒烟验证。
//
// 参考答案：docs/solutions/stage-03/exercise-3-planner-worker.md（完成后再看）
func (w *AgentWorker) Execute(ctx context.Context, spec SubtaskSpec) (string, int, error) {
	return "", 0, errors.New("TODO(练习3): AgentWorker.Execute 未实现")
}
