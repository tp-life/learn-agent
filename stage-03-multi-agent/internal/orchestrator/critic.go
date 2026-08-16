// critic.go —— 评审者：对 worker 产出做质量把关，不通过则打回重做。
//
// 在编排链路中的位置：critic 叠加在 planner-worker 之上（教程核心概念第 2 条：
// critic 可以叠加在任何模式上）。worker 每产出一版，critic 评审一次，
// 通过则落盘 done，不通过则带反馈重做——直到通过或触发熔断（练习4）。
//
// 练习：本文件的类型定义与 NewLLMCritic 构造函数无需学习者完成；
// LLMCritic.Review 为 TODO(练习4)。
package orchestrator

import (
	"context"
	"errors"

	"mini-agent/api"
)

// Verdict 是 critic 的评审结论。
type Verdict int

const (
	// VerdictPass 评审通过：产出可以落盘进汇总。
	VerdictPass Verdict = iota
	// VerdictReject 评审不通过：打回重做，feedback 会拼进 worker 下一轮的 prompt。
	VerdictReject
)

// Critic 评审 worker 产出。
//
// 为什么是接口：① 可测——编排器测试注入假 Critic（先 reject 后 pass、
// 永远 reject、出错降级等场景全靠它构造）；② 模型分级——critic 是质量闸门，
// 值得用比 worker 更强的模型，换实现不换编排器（教程 Q5）。
type Critic interface {
	// Review 评审 spec 的产出 output。
	// 返回评审结论、不通过时的反馈意见、本次评审消耗的 token 数。
	Review(ctx context.Context, spec SubtaskSpec, output string) (verdict Verdict, feedback string, tokens int, err error)
}

// LLMCritic 用 LLM 做评审：把子任务要求 + worker 产出发给模型，
// 解析其 PASS / REJECT 结论。
type LLMCritic struct {
	// TODO(练习4) 要填的字段建议：
	//   client *api.Client
	//   chat   func(messages []api.Message) (content string, tokens int, err error)
	//          —— 与 LLMPlanner 同样的可注入设计：llm.Client 无法指向假服务器，
	//          不抽这层，结论解析与降级路径就没法离线测试。
	//          注意这个 chat 多返回一个 tokens（评审 token 要计入预算熔断）。
}

// NewLLMCritic 构造一个 LLM 评审者。
func NewLLMCritic(client *api.Client) *LLMCritic {
	return &LLMCritic{
		// TODO(练习4): 保存 client、装配真实 chat 闭包（client.Chat，取 Usage.TotalTokens）
	}
}

// 编译期断言：LLMCritic 必须始终满足 Critic 接口。
var _ Critic = (*LLMCritic)(nil)

// TODO(练习4): LLMCritic.Review —— 评审 prompt + 结论解析
//
// 任务：实现 Critic 接口：
//
//	func (c *LLMCritic) Review(ctx context.Context, spec SubtaskSpec, output string) (Verdict, string, int, error)
//
// 流程：构造 messages（critic system prompt + 子任务要求 + worker 产出）
// → chat → 解析结论 → 返回 verdict / feedback / tokens。
//
// 提示：
//   - 输出格式约定（写进 system prompt）：合格第一行只写 PASS；
//     不合格第一行写 REJECT，第二行起写具体修改意见。
//     用裸文本约定而非 JSON 的理由：评审结论只有两种，字符串解析够用且
//     更省 token；结构化评审（score + issues JSON）是练习4 的进阶方向；
//   - 解析：取第一行 strings.ToUpper 后比对 PASS / REJECT；
//     REJECT 但没给意见时补一句兜底反馈（空反馈等于让 worker 盲改）；
//   - 既非 PASS 也非 REJECT → 返回 error，交给编排器的降级路径处理
//     （critic 输出无法解析 = critic 本次不可用，不应误判成 reject 让 worker 白重做）；
//   - 评审要点写进 system prompt：是否回答了子任务要求、有无实质内容、
//     事实是否明显有误——critic 只评不改，重写是 worker 的事。
//
// 验收：测试注入假 chat：PASS / REJECT（带意见）/ 垃圾输出三类各断言一次。
//
// 参考答案：docs/solutions/stage-03/exercise-4-critic-loop.md（完成后再看）
func (c *LLMCritic) Review(ctx context.Context, spec SubtaskSpec, output string) (Verdict, string, int, error) {
	return VerdictPass, "", 0, errors.New("TODO(练习4): LLMCritic.Review 未实现")
}
