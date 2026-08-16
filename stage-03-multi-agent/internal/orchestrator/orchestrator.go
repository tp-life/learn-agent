// Package orchestrator 是多 agent 系统的编排层：
// planner 分解 → pool 并发执行 worker → （可选）critic 评审打回 → 汇总。
//
// 在整个 agent 链路中的位置：它坐在两根支柱之上——
// internal/pool（并发底座，练习1）与 internal/task（状态机 + checkpoint，练习2），
// 并把 mini-agent 内核（单 agent ReAct 循环）当作 worker 执行体复用（api.Agent）。
//
// 设计核心（与阶段教程对齐）：
//   - 状态机驱动：任务 pending → planning → running → done/failed，
//     每次迁移都调 task.Store 落盘——崩溃恢复的全部秘密（教程 3.1 Q4）；
//   - 模型负责生成，代码负责把关：planner 的 LLM 输出必须过 ValidatePlan
//     确定性校验才能进状态机（教程注意事项第 3 条）；
//   - 失败处理四件套（教程第 5 条）：幂等键（taskID+子任务ID）、
//     部分失败汇总、critic 降级放行、双重熔断（轮次上限 + token 预算）。
//
// 练习：本文件的类型定义、Option 模式与 New 构造函数无需学习者完成；
// Run/Resume 的状态机驱动、并发分发与汇总为 TODO(练习3)；
// 评审循环与双重熔断为 TODO(练习4)。
package orchestrator

import (
	"context"
	"errors"

	"stage-03-multi-agent/internal/pool"
	"stage-03-multi-agent/internal/task"
	"stage-03-multi-agent/internal/trace"
)

// Orchestrator 驱动任务的完整生命周期。零值不可用，必须用 New 构造。
type Orchestrator struct {
	store   *task.Store  // checkpoint 唯一读写入口：每次状态迁移都落盘
	pool    *pool.Pool   // 并发底座：worker 子任务的并行执行与限流
	planner Planner      // 任务分解（接口注入：测试用假 Planner，不依赖真实 LLM）
	worker  Worker       // 子任务执行体（接口注入，同上）
	critic  Critic       // 产出评审；nil 表示不评审（练习3 的形态）
	tracer  trace.Tracer // 可观测后端；默认 Noop，接 Langfuse 只换实现（练习6）

	maxCriticRounds int // 熔断一：单个子任务的评审轮次上限（练习4）
	tokenBudget     int // 熔断二：整个任务的 token 预算，0 表示不限（练习4）

	// TODO(练习4) 要补的运行时状态建议（critic 降级用，注意并发安全）：
	//   criticErrors   atomic.Int32 —— critic 连续出错计数（成功一次清零）
	//   criticDisabled atomic.Bool  —— 连续出错达阈值后整个任务跳过评审
	// 提示：pool 并发执行多个子任务，这两个计数器被多个 goroutine 读写，
	// 必须用 atomic，不能用普通字段（go test -race 会抓）。
}

// Option 是 Orchestrator 的可选配置（函数式选项模式）：
// 练习4/6 给编排器加能力时不动 New 的签名，调用方按需叠加。
type Option func(*Orchestrator)

// WithCritic 叠加 critic 评审循环（练习4）。
// maxRounds 是单个子任务最多被执行的次数（首轮 + 打回重做），
// 达到上限仍未通过评审则该子任务熔断为 failed。
func WithCritic(c Critic, maxRounds int) Option {
	return func(o *Orchestrator) {
		o.critic = c
		o.maxCriticRounds = maxRounds
	}
}

// WithTokenBudget 设置任务级 token 预算（练习4）：累计消耗
// （含崩溃恢复前已烧的，从 checkpoint 的 total_tokens 续算）超过预算时
// 任务直接 failed——防止 critic 打回循环无限烧钱（教程 Q5）。
func WithTokenBudget(n int) Option {
	return func(o *Orchestrator) { o.tokenBudget = n }
}

// WithTracer 设置可观测后端；传 nil 保持默认 Noop。
func WithTracer(t trace.Tracer) Option {
	return func(o *Orchestrator) {
		if t != nil {
			o.tracer = t
		}
	}
}

// New 构造编排器。tracer 默认 Noop——"不接观测后端"是显式选择，
// 而不是 nil 判断散落各处（与 internal/trace 包注释的约定一致）。
func New(store *task.Store, p *pool.Pool, planner Planner, worker Worker, opts ...Option) *Orchestrator {
	o := &Orchestrator{
		store:   store,
		pool:    p,
		planner: planner,
		worker:  worker,
		tracer:  trace.NewNoop(),
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// TODO(练习3): Orchestrator.Run —— 状态机驱动 + checkpoint + 并发分发 + 汇总
//
// 任务：实现
//
//	func (o *Orchestrator) Run(ctx context.Context, taskID, goal string) (string, error)
//
// 执行完整任务生命周期，返回最终汇总文本。taskID 由调用方传入
// （方便 HTTP 层预生成并立刻返回给前端轮询）。
//
// 生命周期（每一步都是一次 checkpoint 落盘）：
//  1. store.CreateTask（初始 pending）；
//  2. Transition → planning，调 planner.Plan 分解；
//     失败 → Transition → failed，返回错误；
//  3. 把 Plan 转成 []task.Subtask 落盘（SaveSubtasks）：
//     幂等键 = taskID + ":" + 子任务 ID（崩溃恢复与重试共用的判重依据）；
//  4. Transition → running，把子任务转成 []pool.Job 并发分发：
//     每个 job 内：TransitionSubtask → running → worker.Execute →
//     成功 CompleteSubtask（落产出与 token）/ 失败 FailSubtask（落错误信息）；
//  5. 汇总：重新 LoadTask 读最终状态（不要用内存里的结果拼——Resume 场景下
//     部分产出在 checkpoint 里），把 done 子任务的产出按顺序拼成汇总文本，
//     failed 的列出失败原因（部分失败语义，教程注意事项第 1 条）；
//  6. 全部子任务失败 → Transition → failed 并返回错误；
//     否则 Transition → done，返回汇总。
//
// 提示：
//   - trace：任务一个根 span（StartSpan parent=""），planner 一个子 span，
//     每个子任务一个孙 span——span 层级 = agent 层级（教程第 7 条）；
//   - 汇总用确定性拼接（标题 + 产出），不要再起一次 LLM 调用——
//     省一轮 token，且汇总格式可控；LLM 汇总是锦上添花，不是必须；
//   - job 闭包里对 store 的并发调用是安全的：Store 内部 SetMaxOpenConns(1)
//     已串行化（练习2 的设计红利在这里兑现）。
//
// 验收：go test ./internal/orchestrator/ 通过——用假 Planner/Worker 覆盖：
// 完整生命周期状态迁移正确、部分子任务失败任务仍 done 且汇总含失败项。
//
// 参考答案：docs/solutions/stage-03/exercise-3-planner-worker.md（完成后再看）
func (o *Orchestrator) Run(ctx context.Context, taskID, goal string) (string, error) {
	return "", errors.New("TODO(练习3): Run 未实现")
}

// TODO(练习3): Orchestrator.Resume —— 崩溃恢复续跑
//
// 任务：实现
//
//	func (o *Orchestrator) Resume(ctx context.Context, taskID string) (string, error)
//
// 从 checkpoint 恢复一个未完成任务（练习5 的审批恢复也会复用它）。
//
// 流程：
//  1. LoadTask 读出任务与子任务；任务已是终态（done/failed）直接返回；
//  2. 没有子任务落盘（崩溃在 planning 阶段）→ 重新分解并落盘
//     （planner 无副作用，重跑安全）；
//  3. 有子任务：把任务状态补齐到 running（可能停在 pending/planning），
//     然后分发——已 done 的子任务【跳过】（幂等键语义：这份活干过了）；
//     停在 running 的先 TransitionSubtask 迁回 pending 再重跑
//     （进程已死，没有正在跑的执行体——练习2 崩溃恢复测试逼出来的那条边）；
//     failed 的也迁回 pending 重排队（重试语义）；
//  4. 分发、汇总、终态迁移与 Run 完全相同——建议把"分发 + 汇总"抽成
//     私有方法（如 dispatch）让 Run/Resume 共用，别让两条路径漂移。
//
// 提示：
//   - "跳过 done"不是看内存，是看 LoadTask 读出的子任务状态——
//     状态在 SQLite 里，进程无状态（教程核心概念第 3 条）；
//   - 已知边界：若崩溃发生在 SaveSubtasks 写了一半（基础版非事务），
//     恢复时会以"已落盘的部分计划"继续跑而不重新分解——这正是练习2
//     进阶（事务版 SaveSubtasksTx）存在的意义，这里如实注释即可。
//
// 验收：崩溃恢复测试——临时文件 DB 铺现场（s1 done、s2 停在 running、
// s3 pending）→ Close 重开 → Resume：worker 对 s1 零调用，s2/s3 各执行一次，
// 汇总里含 s1 从 checkpoint 读出的产出，任务最终 done。
//
// 参考答案：docs/solutions/stage-03/exercise-3-planner-worker.md（完成后再看）
func (o *Orchestrator) Resume(ctx context.Context, taskID string) (string, error) {
	return "", errors.New("TODO(练习3): Resume 未实现")
}

// ErrWaitingHuman 是编排器"让出"的哨兵错误（练习5 HITL）：
// 任务里有子任务进入 waiting_human 等人工审批时，Run/Resume 返回它。
//
// 为什么是哨兵错误而不是返回值标志（教程 Q6 的工程落地）：
//  1. "等待审批"不是失败——调用方必须用 errors.Is 把它和真实错误分开处理
//     （HTTP 层映射成 202/特定状态码，CLI 进入审批循环）；
//  2. 哨兵错误能穿透 pool → dispatch → Run/Resume 多层调用自然冒泡，
//     不用给每一层签名加返回值；
//  3. 与练习4 的 ErrBudgetExceeded 同构：编排器的"非正常但可预期"出口
//     统一用哨兵错误表达。
//
// 它由骨架提供（不是 TODO），因为它是 hitl-demo 与未来 HTTP 层（练习8）
// 共同依赖的契约——契约先定死，实现可以后补。
var ErrWaitingHuman = errors.New("orchestrator: 任务暂停，等待人工审批")

// TODO(练习5): HITL 审批闸 —— RequiresApproval 子任务的中断/恢复
//
// 任务：在练习3 参考实现的基础上加审批闸（与 internal/hitl 的 TODO(练习5)
// 一起构成本练习）。共四处处增量改动：
//
//  1. runSubtask 开头（StartSpan 之后、TransitionSubtask→running 之前）
//     加审批闸：spec.RequiresApproval 为 true 时，把子任务迁到
//     waiting_human 并直接返回 ErrWaitingHuman——不执行 worker。
//     闸必须在迁入 running 之前：waiting_human 不算一次执行，attempts 不该自增。
//     （练习4 的评审循环版 runSubtask 多一个 consumed 参数，闸的位置不变。）
//
//  2. dispatch 的子任务筛选循环改为区分四种状态：
//     done 跳过（原有）；waiting_human 跳过（已 parked 等审批，Decide 后才会
//     回 pending）；failed 且 RequiresApproval 仍为 true 的跳过——那是被人工
//     驳回的子任务，驳回是终局决定，不进"failed→pending"的重试重排队
//     （approve 会清掉 RequiresApproval 旗标，所以执行失败的正常重试不受影响）；
//     running / 其余 failed 照旧迁回 pending 重跑。
//
//  3. dispatch 在 pool.Run 之后、汇总之前：重新 LoadTask（以 DB 为准，不看
//     内存结果），若仍有子任务停在 waiting_human → Transition 任务到
//     waiting_human → 返回 ErrWaitingHuman 让出。
//
//  4. Resume 的"状态补齐到 running"map 加一行：waiting_human → running
//     （审批恢复时任务停在 waiting_human，要能补回 running）。
//
// 前置依赖：练习2 的子任务迁移表需要补两条边（一行一个枚举值）：
// pending→waiting_human（审批闸）、waiting_human→pending（approve 重排队）。
// 练习2 参考答案的表里没有，做本练习前先补上。
//
// 提示：
//   - 审批决定不在编排器里做：编排器只负责"停下"，hitl.Service.Decide 负责
//     "落决定"，调用方（CLI/HTTP）负责"再 Resume"——三段分离，进程随便重启；
//   - approve 后子任务回 pending 且 RequiresApproval 被 Decide 清掉，
//     所以 Resume 重跑时审批闸不再拦截——"批准"是一次性放行令牌；
//   - 让出时任务级也要落 waiting_human：看板/CLI 靠任务状态一眼认出
//     "这个任务在等人"，不用扫子任务。
//
// 验收：go test ./internal/hitl/ 的审批全流程测试通过（approve 续跑 /
// reject 不重排队 / Decide 后模拟崩溃重建再 Resume）。
//
// 参考答案：docs/solutions/stage-03/exercise-5-hitl-approval.md（完成后再看）

// TODO(练习4): 评审循环 + 双重熔断 + critic 降级
//
// 任务：在子任务执行路径上叠加 critic 评审（建议抽私有方法，如 runSubtask，
// 由分发的 job 调用）：
//
//	worker 产出 → critic 评审 → 通过：CompleteSubtask 落盘；
//	不通过：feedback 拼进 spec.Prompt 重做（如
//	"【上次产出未通过评审，评审意见】：<feedback>\n请针对意见修正后重新完成子任务。"）；
//	直到通过或触发熔断。
//
// 双重熔断（缺一不可，教程 Q5）：
//  1. 轮次熔断：单个子任务执行次数达到 maxCriticRounds 仍未通过 →
//     FailSubtask（错误信息注明评审熔断与最后的 feedback）；
//  2. 预算熔断：整个任务累计 token（worker + critic，含 Resume 前已烧的——
//     从 LoadTask 的 TotalTokens 续算）超过 tokenBudget → 任务直接 failed。
//     检查点在每次 LLM 调用之前；并发子任务共享一个 atomic 计数器。
//
// critic 降级（教程第 5 条）：critic.Review 返回 error 时【放行】本次产出
// （记 log，不把 critic 的故障转嫁成子任务失败）；连续出错达到阈值（如 2 次）
// 后整个任务跳过评审——critic 是质量增强不是单点故障。
//
// 提示：
//   - 轮次计数与 token 累加都在 job 闭包内做；注意 spec 是值类型，
//     重做时构造"带反馈的副本"，不要改共享的原始 spec；
//   - 预算超限时返回一个哨兵错误（如 ErrBudgetExceeded），
//     分发结束后用 errors.Is 判定 → 任务迁 failed；
//   - 每轮 worker/critic 的 token 都要累加进 CompleteSubtask 的 tokens 参数，
//     否则 checkpoint 里的成本账对不上。
//
// 验收：测试覆盖四条：先 reject 后 pass（worker 被调用 2 次且第 2 次 prompt
// 含 feedback）；永远 reject 触发轮次熔断；token 预算熔断（任务 failed）；
// critic 连续出错降级放行（任务仍 done，第 3 个子任务起不再调 Review）。
//
// 参考答案：docs/solutions/stage-03/exercise-4-critic-loop.md（完成后再看）
