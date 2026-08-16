// planner.go —— 任务分解器：把用户目标变成结构化的子任务计划。
//
// 在编排链路中的位置：orchestrator 拿到用户目标后的第一步就是调 Planner，
// 产出的 Plan 经确定性校验后落盘（checkpoint），再驱动 worker 并发执行。
//
// 练习：本文件的类型定义与 NewLLMPlanner 构造函数无需学习者完成；
// ValidatePlan 与 LLMPlanner.Plan 为 TODO(练习3)。
package orchestrator

import (
	"context"
	"errors"

	"mini-agent/api"
)

// SubtaskSpec 是 planner 分解出的一个子任务描述。
//
// 为什么 Prompt 要"自包含"：worker 是独立的 agent，看不到用户原始目标，
// 也看不到其他子任务——它唯一的输入就是这份 Prompt（砍 context，教程 Q5 第④招）。
// 所以 planner 生成 Prompt 时必须把完成该子任务所需的上下文都写进去。
type SubtaskSpec struct {
	ID    string // 任务内唯一标识（如 "s1"），幂等键 = taskID + ":" + ID
	Title string // 一句话标题，给看板和汇总报告用
	Prompt string // 喂给 worker agent 的子任务指令（自包含）
	// RequiresApproval 标记高风险子任务（练习5 HITL 用）：true 时执行前需人工审批。
	// 本练习只定义字段，审批逻辑在练习5 实现。
	RequiresApproval bool
}

// Plan 是 planner 的输出：一组可并行执行的子任务。
//
// 当前假设子任务相互独立（可并行）；"带依赖关系的 DAG 分发"是练习3 的进阶方向。
type Plan struct {
	Subtasks []SubtaskSpec
}

// MaxSubtasks 是单任务子任务数上限。
// 为什么需要上限：LLM 可能分解出几十个子任务，每个子任务 = 至少一轮 LLM 调用，
// 不上限等于把成本控制权交给模型的发挥。8 是经验值，够用且拦得住失控。
const MaxSubtasks = 8

// Planner 把用户目标分解为结构化计划。
//
// 为什么是接口（教程 Q5 的模型分级 + 可测性）：
//   - 可测：编排器测试注入假 Planner，不依赖真实 LLM/网络；
//   - 可换实现：LLMPlanner（本练习）之外，也可以有"固定模板 planner"
//     （如周报场景固定三段：收集数据 → 写稿 → 校对），编排器代码一行不改。
type Planner interface {
	// Plan 分解 goal，返回校验通过的 Plan。
	// 实现方负责：LLM 输出解析 + ValidatePlan 校验 + 校验失败带错误信息重试（限次）。
	Plan(ctx context.Context, goal string) (Plan, error)
}

// LLMPlanner 用 LLM 做任务分解：
// system prompt 约束输出 JSON → 解析 → ValidatePlan 校验 → 失败带错误信息重试（限次）。
//
// 设计要点（教程 Q12）：LLM 输出是不确定的，"模型负责生成，代码负责把关"——
// 任何要进状态机的 LLM 输出都必须先过 ValidatePlan 这道确定性校验。
type LLMPlanner struct {
	// TODO(练习3) 要填的字段建议：
	//   client     *api.Client
	//   maxRetries int  —— 校验失败后的重试次数上限（总尝试 = 1 + maxRetries）
	//   chat       func(messages []api.Message) (string, error)
	//              —— 发一次 LLM 问答的可注入函数。为什么抽成字段：
	//              mini-agent 的 llm.Client 目前无法指向 httptest 假服务器，
	//              没有这层注入，"校验失败重试"这条核心路径就没法离线测试。
	//              NewLLMPlanner 里把它设为真实 client 调用的闭包。
}

// NewLLMPlanner 构造一个 LLM 任务分解器（默认重试 2 次）。
func NewLLMPlanner(client *api.Client) *LLMPlanner {
	return &LLMPlanner{
		// TODO(练习3): 保存 client、设置默认 maxRetries、装配真实 chat 闭包
	}
}

// 编译期断言：LLMPlanner 必须始终满足 Planner 接口。
var _ Planner = (*LLMPlanner)(nil)

// TODO(练习3): ValidatePlan —— LLM 输出进状态机前的确定性校验
//
// 任务：实现
//
//	func ValidatePlan(p Plan) error
//
// 校验四条（任一不满足即返回带定位信息的错误）：
//  1. 子任务列表非空；
//  2. 子任务数不超过 MaxSubtasks；
//  3. 子任务 ID 全局唯一（重复 ID 会让 checkpoint 主键冲突、幂等键失效）；
//  4. 每个子任务的 ID / Title / Prompt 去空白后非空。
//
// 提示：
//   - 用 map[string]bool 查重 ID；错误信息里带上出问题的是第几个子任务
//     （如 "subtasks[2]"），否则重试时喂回 planner 的错误没有定位价值；
//   - 这是纯函数，不碰 LLM/IO——它存在的意义就是"不相信模型"，
//     校验规则写严比写松好（教程注意事项第 3 条）。
//
// 验收：表驱动测试覆盖：空计划、超上限、重复 ID、空字段、合法计划五类。
//
// 参考答案：docs/solutions/stage-03/exercise-3-planner-worker.md（完成后再看）
func ValidatePlan(p Plan) error {
	return errors.New("TODO(练习3): ValidatePlan 未实现")
}

// TODO(练习3): LLMPlanner.Plan —— LLM 分解 + 解析 + 校验 + 带反馈重试
//
// 任务：实现 Planner 接口：
//
//	func (p *LLMPlanner) Plan(ctx context.Context, goal string) (Plan, error)
//
// 流程：构造 messages（system prompt + goal）→ chat → 解析 JSON → ValidatePlan
// → 通过则返回；不通过则把【模型的原始输出 + 校验错误】追加进 messages 重试，
// 重试 maxRetries 次仍不合法则返回错误（上层据此把任务迁为 failed）。
//
// 提示：
//   - system prompt 怎么约束 JSON 输出（教程 Q12）：明确要求"只输出 JSON，
//     不要任何其他文字、不要 markdown 代码块"，并给出 schema 示例：
//     {"subtasks":[{"id":"s1","title":"...","prompt":"..."}]}；
//     同时要求子任务相互独立、id 用短标识、prompt 自包含；
//   - 解析要容错：模型经常不顾指令裹一层 ```json 围栏——不要硬 TrimPrefix，
//     找第一个 '{' 和最后一个 '}' 截取再 json.Unmarshal 最稳；
//   - 重试喂错误（教程 Q12 第③条）：messages = append(messages,
//     assistant 的原始输出, user 的 "你输出的计划未通过校验：<err>，请修正后重新只输出 JSON")
//     ——保留对话上下文让模型知道自己上次错在哪，比从零重发成功率高得多；
//   - 真实 chat 闭包用 client.ChatWithRetry（复用阶段一的 429/5xx 退避）；
//   - 已知缺口：mini-agent 的 llm.Client.Chat 目前不接收 ctx，planner 层的超时
//     靠 client 内置的 http.Client.Timeout（120s）兜底；ctx 参数是接口预留。
//
// 验收：测试注入假 chat 闭包：第一次返回垃圾文本、第二次返回合法 JSON，
// 断言重试生效且第二次请求的 messages 里带有校验错误反馈；
// 另测 ```json 围栏包裹的输出也能解析。
//
// 参考答案：docs/solutions/stage-03/exercise-3-planner-worker.md（完成后再看）
func (p *LLMPlanner) Plan(ctx context.Context, goal string) (Plan, error) {
	return Plan{}, errors.New("TODO(练习3): LLMPlanner.Plan 未实现")
}
