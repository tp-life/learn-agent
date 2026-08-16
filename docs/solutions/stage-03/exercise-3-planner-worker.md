# 练习 3 参考答案：Planner/Worker 编排器

> 对应 TODO：`stage-03-multi-agent/internal/orchestrator/` 的 `TODO(练习3)`
> （`planner.go` 的 ValidatePlan/LLMPlanner.Plan、`worker.go` 的 AgentWorker.Execute、
> `orchestrator.go` 的 Run/Resume）。
> **完成练习并自评后再看本文档。**
>
> 本文档基础实现代码已于 2026-08-14 实际粘贴进项目验证：
> `go vet ./internal/orchestrator/` 与 `go test ./internal/orchestrator/ -race -count=1`
> 全部通过（9 个测试，`-race` 无数据竞争）。
> 进阶实现（DAG 依赖分发，见第三节）同日验证：临时粘贴为 `dag.go` + `dag_test.go`，
> 连同基础测试与练习4 实现共 21 个测试全绿，验证后即删除，项目保持骨架版。
>
> **复验记录（2026-08-14）**：mini-agent 内核补齐 `Agent.Usage()`（累计本次任务
> 所有 ReAct 轮次的 token 用量）后，`AgentWorker.Execute` 由"tokens 恒为 0 的占位"
> 改为真实记账（见 worker.go 一节）。本次复验重新临时落地全部实现与测试，
> `go vet` 与 `go test -race -count=1` 21 个测试再次全绿，验证后恢复骨架。
>
> **验证前提（如实说明）**：编排器测试要跑真的并发池与真的 SQLite checkpoint，
> 而项目骨架里 `pool.Run` 是 panic 桩、`task.Store` 的方法是错误桩。
> 验证时临时落地了两份既有参考答案：
> 练习1 的 `Pool.Run`（docs/solutions/stage-03/exercise-1-worker-pool.md）与
> 练习2 的 Store 实现（docs/solutions/stage-03/exercise-2-task-checkpoint.md），
> 验证后已恢复骨架。你自己跑本练习测试前，需要先完成练习1/2。

---

## 一、参考实现

### `internal/orchestrator/planner.go`（ValidatePlan + LLMPlanner；骨架其余部分不变）

import 扩为：`context` / `encoding/json` / `errors` / `fmt` / `strings` / `mini-agent/api`。

```go
// LLMPlanner 用 LLM 做任务分解：
// system prompt 约束输出 JSON → 解析 → ValidatePlan 校验 → 失败带错误信息重试（限次）。
type LLMPlanner struct {
	client     *api.Client
	maxRetries int // 校验失败后的重试次数上限（总尝试 = 1 + maxRetries）
	// chat 发一次 LLM 问答。抽成可注入字段的原因：mini-agent 的 llm.Client
	// 目前无法指向 httptest 假服务器（baseURL 是私有字段、无 WithBaseURL），
	// 没有这层注入，"校验失败重试"这条核心路径就没法离线测试。
	// NewLLMPlanner 把它装配为真实 client 调用；测试直接替换这个字段。
	chat func(messages []api.Message) (string, error)
}

// NewLLMPlanner 构造一个 LLM 任务分解器（默认重试 2 次）。
func NewLLMPlanner(client *api.Client) *LLMPlanner {
	p := &LLMPlanner{client: client, maxRetries: 2}
	p.chat = func(messages []api.Message) (string, error) {
		// 复用阶段一的 429/5xx 指数退避重试——transport 层的重试在内层做掉，
		// 外层 Plan 的重试只处理"内容不合法"这一类（职责分层）。
		resp, err := client.ChatWithRetry(messages, nil, 2)
		if err != nil {
			return "", err
		}
		return resp.Choices[0].Message.Content, nil
	}
	return p
}

// plannerSystemPrompt 约束 LLM 只输出 JSON 计划（教程 Q12 第①条：结构化输出约束）。
const plannerSystemPrompt = `你是任务分解器（planner）。把用户目标分解为若干可并行执行的子任务。
只输出 JSON，不要输出任何其他文字，不要用 markdown 代码块包裹。输出格式：
{"subtasks":[{"id":"s1","title":"一句话标题","prompt":"喂给执行 agent 的完整指令"}]}
要求：
- 子任务 2 到 6 个，相互独立、可并行执行；
- id 用 s1、s2…… 这样的短标识，全局唯一；
- prompt 必须自包含：执行 agent 看不到用户原始目标，也看不到其他子任务，
  完成该子任务所需的全部上下文都要写进 prompt。`

// planJSON 是 planner 输出的线缆结构（蛇形 JSON ↔ 驼峰 Go 的翻译层）。
type planJSON struct {
	Subtasks []struct {
		ID               string `json:"id"`
		Title            string `json:"title"`
		Prompt           string `json:"prompt"`
		RequiresApproval bool   `json:"requires_approval"`
	} `json:"subtasks"`
}

// Plan 分解 goal，返回校验通过的 Plan。校验失败带错误信息重试（限次）。
func (p *LLMPlanner) Plan(ctx context.Context, goal string) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	messages := []api.Message{
		{Role: "system", Content: plannerSystemPrompt},
		{Role: "user", Content: goal},
	}
	var lastErr error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		raw, err := p.chat(messages)
		if err != nil {
			// LLM 调用本身失败（网络/限流）不在此重试——chat 内部的
			// ChatWithRetry 已做过退避，再重试是双重退避。
			return Plan{}, fmt.Errorf("planner: LLM 调用失败: %w", err)
		}
		plan, err := parsePlan(raw)
		if err == nil {
			err = ValidatePlan(plan)
		}
		if err == nil {
			return plan, nil
		}
		lastErr = err
		// 校验失败把错误喂回 planner（教程 Q12 第③条）：
		// 保留模型的原始输出 + 指出具体问题，模型知道上次错在哪，
		// 比从零重发成功率高得多。
		messages = append(messages,
			api.Message{Role: "assistant", Content: raw},
			api.Message{Role: "user", Content: fmt.Sprintf(
				"你输出的计划未通过校验：%v。请修正后重新只输出 JSON。", err)},
		)
	}
	return Plan{}, fmt.Errorf("planner: 重试 %d 次后计划仍不合法: %w", p.maxRetries, lastErr)
}

// parsePlan 从 LLM 输出中提取并解析 JSON 计划。
func parsePlan(raw string) (Plan, error) {
	s := strings.TrimSpace(raw)
	// 容错：模型经常不顾指令裹一层 ```json 围栏或加前缀废话。
	// 不硬 TrimPrefix（模型可能写 "好的：```json..."），
	// 找第一个 '{' 到最后一个 '}' 截取最稳。
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return Plan{}, fmt.Errorf("plan: 输出中找不到 JSON 对象: %q", truncate(s, 80))
	}
	var pj planJSON
	if err := json.Unmarshal([]byte(s[start:end+1]), &pj); err != nil {
		return Plan{}, fmt.Errorf("plan: JSON 解析失败: %w", err)
	}
	plan := Plan{Subtasks: make([]SubtaskSpec, len(pj.Subtasks))}
	for i, js := range pj.Subtasks {
		plan.Subtasks[i] = SubtaskSpec{
			ID: js.ID, Title: js.Title, Prompt: js.Prompt,
			RequiresApproval: js.RequiresApproval,
		}
	}
	return plan, nil
}

// ValidatePlan 校验 LLM 输出的计划——模型负责生成，代码负责把关
// （教程注意事项第 3 条：LLM 输出进状态机之前必须过确定性校验）。
func ValidatePlan(p Plan) error {
	if len(p.Subtasks) == 0 {
		return errors.New("plan: 子任务列表为空")
	}
	if len(p.Subtasks) > MaxSubtasks {
		return fmt.Errorf("plan: 子任务数 %d 超过上限 %d", len(p.Subtasks), MaxSubtasks)
	}
	seen := make(map[string]bool, len(p.Subtasks))
	for i, s := range p.Subtasks {
		if strings.TrimSpace(s.ID) == "" {
			return fmt.Errorf("plan: subtasks[%d] 的 id 为空", i)
		}
		if seen[s.ID] {
			// 重复 ID 会让 checkpoint 主键冲突、幂等键失效，必须拦下。
			return fmt.Errorf("plan: subtasks[%d] 的 id %q 重复", i, s.ID)
		}
		seen[s.ID] = true
		if strings.TrimSpace(s.Title) == "" {
			return fmt.Errorf("plan: subtasks[%d] (%s) 的 title 为空", i, s.ID)
		}
		if strings.TrimSpace(s.Prompt) == "" {
			return fmt.Errorf("plan: subtasks[%d] (%s) 的 prompt 为空", i, s.ID)
		}
	}
	return nil
}

// truncate 截断长文本用于错误信息（模型原话可能很长）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
```

### `internal/orchestrator/worker.go`（AgentWorker.Execute）

```go
// AgentWorker 用 mini-agent 内核当执行体：每个子任务 new 一个 api.Agent。
type AgentWorker struct {
	client   *api.Client
	registry *api.Registry
}

// NewAgentWorker 构造 worker。registry 为 nil 时挂空注册表（纯生成型子任务）。
func NewAgentWorker(client *api.Client, registry *api.Registry) *AgentWorker {
	if registry == nil {
		registry = api.NewRegistry()
	}
	return &AgentWorker{client: client, registry: registry}
}

// workerSystemPrompt 的要点（教程 Q5"砍 context"）：
// 明确"只负责这一个子任务"，要求输出自包含结论——
// worker 拿不到用户原始目标，也看不到其他子任务。
const workerSystemPrompt = `你是一个子任务执行 agent。你只负责当前交给你的这一个子任务：
- 聚焦子任务目标本身，不要揣测全局任务，不要扩大范围；
- 可以使用提供的工具获取信息或完成操作；
- 最终输出一段自包含的结论文本——它会作为该子任务的产出进入汇总报告，
  读者看不到你的执行过程，结论里要带上关键事实与数据。`

// Execute 执行一个子任务：new 一个 Agent，跑完即弃（context 隔离）。
func (w *AgentWorker) Execute(ctx context.Context, spec SubtaskSpec) (string, int, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	// 每个子任务一个独立 Agent：messages 从 system prompt 全新开始，
	// 不背其他子任务的对话历史——多 agent 解决 context 膨胀的落地方式。
	ag := api.NewAgent(w.client, w.registry, workerSystemPrompt)
	output, err := ag.Run(spec.Prompt)
	if err != nil {
		return "", 0, fmt.Errorf("worker %s: %w", spec.ID, err)
	}
	// 真实 token 记账：内核的 Agent.Usage() 累计了本次任务所有 ReAct 轮次的
	// prompt/completion/total 用量（注意是"整趟 Run"的总量，不是单次调用）。
	// 这笔账经 CompleteSubtask 落进 checkpoint，预算熔断（练习4）与
	// 成本观测（练习6）都消费它。
	return output, ag.Usage().TotalTokens, nil
}
```

### `internal/orchestrator/orchestrator.go`（Run / Resume / 分发 / 汇总）

骨架的 Option 与 New 不变，实现以下方法（私有辅助方法一并给出）：

```go
// Run 执行完整任务生命周期，返回最终汇总文本。taskID 由调用方传入。
func (o *Orchestrator) Run(ctx context.Context, taskID, goal string) (summary string, err error) {
	// 任务级根 span；defer + 命名返回值保证任何出错路径都落 EndSpan。
	root := o.tracer.StartSpan(ctx, "", "task: "+goal, map[string]any{"task_id": taskID})
	defer func() { o.tracer.EndSpan(ctx, root, 0, 0, err) }()

	if err = o.store.CreateTask(ctx, taskID, goal); err != nil {
		return "", fmt.Errorf("orchestrator: 创建任务 %s: %w", taskID, err)
	}
	if err = o.store.Transition(ctx, taskID, task.StatusPlanning); err != nil {
		return "", err
	}
	if err = o.planAndSave(ctx, taskID, goal, root); err != nil {
		o.failTask(ctx, taskID) // planning → failed 是合法迁移（练习2 迁移表）
		return "", err
	}
	if err = o.store.Transition(ctx, taskID, task.StatusRunning); err != nil {
		return "", err
	}
	return o.dispatch(ctx, taskID, root)
}

// Resume 从 checkpoint 恢复一个未完成任务。
func (o *Orchestrator) Resume(ctx context.Context, taskID string) (summary string, err error) {
	t, subs, err := o.store.LoadTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	root := o.tracer.StartSpan(ctx, "", "task(resume): "+t.Goal, map[string]any{"task_id": taskID})
	defer func() { o.tracer.EndSpan(ctx, root, 0, 0, err) }()

	switch t.Status {
	case task.StatusDone:
		// 已完成的任务直接汇总返回（重复 Resume 幂等）。
		summary, _ := summarize(t.Goal, subs)
		return summary, nil
	case task.StatusFailed:
		return "", fmt.Errorf("orchestrator: 任务 %s 已是终态 failed，不可恢复", taskID)
	}

	if len(subs) == 0 {
		// 崩溃在 planning 阶段（计划还没落盘）：重新分解。
		// planner 无副作用，重跑安全。
		if t.Status == task.StatusPending {
			if err = o.store.Transition(ctx, taskID, task.StatusPlanning); err != nil {
				return "", err
			}
		}
		if err = o.planAndSave(ctx, taskID, t.Goal, root); err != nil {
			o.failTask(ctx, taskID)
			return "", err
		}
	}
	// 已知边界：若崩溃发生在 SaveSubtasks 写了一半（基础版非事务），
	// 恢复时会以"已落盘的部分计划"继续跑而不重新分解——
	// 这正是练习2 进阶（事务版 SaveSubtasksTx）存在的意义。

	// 把任务状态补齐到 running（可能停在 pending/planning）。
	for {
		t, _, err = o.store.LoadTask(ctx, taskID)
		if err != nil {
			return "", err
		}
		if t.Status == task.StatusRunning {
			break
		}
		next := map[task.Status]task.Status{
			task.StatusPending:  task.StatusPlanning,
			task.StatusPlanning: task.StatusRunning,
		}[t.Status]
		if next == "" {
			return "", fmt.Errorf("orchestrator: 任务 %s 状态 %s 无法续跑", taskID, t.Status)
		}
		if err = o.store.Transition(ctx, taskID, next); err != nil {
			return "", err
		}
	}
	return o.dispatch(ctx, taskID, root)
}

// planAndSave 是 Run/Resume 共用的"分解 + 计划落盘"段。
func (o *Orchestrator) planAndSave(ctx context.Context, taskID, goal, root string) error {
	span := o.tracer.StartSpan(ctx, root, "planner", nil)
	plan, err := o.planner.Plan(ctx, goal)
	o.tracer.EndSpan(ctx, span, 0, 0, err)
	if err != nil {
		return fmt.Errorf("orchestrator: 任务分解失败: %w", err)
	}
	subs := make([]task.Subtask, len(plan.Subtasks))
	for i, spec := range plan.Subtasks {
		subs[i] = task.Subtask{
			ID:     spec.ID,
			Title:  spec.Title,
			Prompt: spec.Prompt,
			// 幂等键 = taskID + ":" + 子任务 ID：崩溃恢复与重试共用的判重依据。
			IdempotencyKey:   taskID + ":" + spec.ID,
			RequiresApproval: spec.RequiresApproval,
		}
	}
	if err := o.store.SaveSubtasks(ctx, taskID, subs); err != nil {
		return fmt.Errorf("orchestrator: 计划落盘失败: %w", err)
	}
	return nil
}

// dispatch 是 Run/Resume 共用的"并发分发 + 汇总 + 终态迁移"段。
func (o *Orchestrator) dispatch(ctx context.Context, taskID, root string) (string, error) {
	t, subs, err := o.store.LoadTask(ctx, taskID)
	if err != nil {
		return "", err
	}

	var jobs []pool.Job
	for _, sub := range subs {
		if sub.Status == task.StatusDone {
			continue // 幂等键语义：这份活干过了，跳过
		}
		if sub.Status == task.StatusRunning || sub.Status == task.StatusFailed {
			// running = 崩溃现场（执行体已随进程死亡，先重置再重跑）；
			// failed = 重试重排队。两条边都是练习2 迁移表里的合法边。
			if err := o.store.TransitionSubtask(ctx, taskID, sub.ID, task.StatusPending); err != nil {
				return "", err
			}
		}
		spec := SubtaskSpec{
			ID:               sub.ID,
			Title:            sub.Title,
			Prompt:           sub.Prompt,
			RequiresApproval: sub.RequiresApproval,
		}
		jobs = append(jobs, pool.Job{
			ID: sub.ID,
			Run: func(jctx context.Context) (string, error) {
				return o.runSubtask(jctx, taskID, root, spec)
			},
		})
	}
	// 部分失败语义：单个 job 的失败收在 Result.Err / 子任务 checkpoint 里，
	// 不中断其他 job，最终成败以 checkpoint 为准。
	_ = o.pool.Run(ctx, jobs)

	// 汇总以 checkpoint 为准，不用内存里的 results 拼——
	// Resume 场景下部分产出是上一轮写进 SQLite 的，不在本轮 results 里。
	_, finalSubs, err := o.store.LoadTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	summary, failed := summarize(t.Goal, finalSubs)
	if len(finalSubs) > 0 && failed == len(finalSubs) {
		o.failTask(ctx, taskID)
		return summary, fmt.Errorf("orchestrator: 任务 %s 全部子任务失败", taskID)
	}
	if err := o.store.Transition(ctx, taskID, task.StatusDone); err != nil {
		return "", err
	}
	return summary, nil
}

// runSubtask 是单个子任务的执行路径（练习3形态：执行 → 落盘，无评审）。
// 练习4 会把这个方法升级为评审循环版。
func (o *Orchestrator) runSubtask(ctx context.Context, taskID, root string, spec SubtaskSpec) (string, error) {
	span := o.tracer.StartSpan(ctx, root, "worker: "+spec.ID, map[string]any{"title": spec.Title})

	if err := o.store.TransitionSubtask(ctx, taskID, spec.ID, task.StatusRunning); err != nil {
		o.tracer.EndSpan(ctx, span, 0, 0, err)
		return "", fmt.Errorf("orchestrator: 子任务 %s 启动失败: %w", spec.ID, err)
	}
	output, tokens, err := o.worker.Execute(ctx, spec)
	if err != nil {
		o.failSubtask(ctx, taskID, spec.ID, err)
		o.tracer.EndSpan(ctx, span, 0, tokens, err)
		return "", fmt.Errorf("orchestrator: 子任务 %s 执行失败: %w", spec.ID, err)
	}
	if err := o.store.CompleteSubtask(ctx, taskID, spec.ID, output, tokens); err != nil {
		o.tracer.EndSpan(ctx, span, 0, tokens, err)
		return "", fmt.Errorf("orchestrator: 子任务 %s checkpoint 失败: %w", spec.ID, err)
	}
	o.tracer.EndSpan(ctx, span, 0, tokens, nil)
	return output, nil
}

// failSubtask 落盘子任务失败；checkpoint 本身失败只记 log（不覆盖原始错误）。
func (o *Orchestrator) failSubtask(ctx context.Context, taskID, subID string, cause error) {
	if err := o.store.FailSubtask(ctx, taskID, subID, cause.Error()); err != nil {
		log.Printf("orchestrator: 子任务 %s/%s 失败现场落盘失败: %v", taskID, subID, err)
	}
}

// failTask 把任务迁为 failed；迁移失败只记 log（返回给调用方的原始错误优先）。
func (o *Orchestrator) failTask(ctx context.Context, taskID string) {
	if err := o.store.Transition(ctx, taskID, task.StatusFailed); err != nil {
		log.Printf("orchestrator: 任务 %s 迁移 failed 失败: %v", taskID, err)
	}
}

// summarize 把子任务产出确定性拼成汇总文本（不再起一次 LLM 调用——
// 省一轮 token，且格式可控）。返回汇总文本与失败子任务数。
// 部分失败语义：failed 子任务的失败原因也进汇总（教程注意事项第 1 条）。
func summarize(goal string, subs []task.Subtask) (string, int) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# 任务汇总：%s\n", goal)
	failed := 0
	for _, s := range subs {
		if s.Status == task.StatusDone {
			fmt.Fprintf(&sb, "\n## [%s] %s\n%s\n", s.ID, s.Title, s.Output)
		} else {
			failed++
			fmt.Fprintf(&sb, "\n## [%s] %s（未完成：%s）\n", s.ID, s.Title, s.Output)
		}
	}
	return sb.String(), failed
}
```

orchestrator.go 的 import 相应扩为：
`context` / `errors`（骨架 Run/Resume 桩用，实现后可移除）/ `fmt` / `log` / `strings` /
`stage-03-multi-agent/internal/pool` / `.../task` / `.../trace`。

### `internal/orchestrator/orchestrator_test.go`（新建，假 Planner/Worker，无需网络/LLM）

九个测试：ValidatePlan 表驱动、planner 校验失败重试、```json 围栏容错、重试耗尽、
完整生命周期、部分失败仍汇总、全部失败任务 failed、分解失败任务 failed、崩溃恢复。

```go
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mini-agent/api"
	"stage-03-multi-agent/internal/pool"
	"stage-03-multi-agent/internal/task"
	"stage-03-multi-agent/internal/trace"
)

// newTestStore 用临时文件 DB——崩溃恢复测试必须真实 Close 再 Open。
func newTestStore(t *testing.T) (*task.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.db")
	s, err := task.Open(path)
	if err != nil {
		t.Fatalf("task.Open: %v", err)
	}
	return s, path
}

// fakePlanner / fakeWorker：接口注入的意义就在这里——
// 编排器的全部状态机逻辑不烧一分 token 就能测。
type fakePlanner struct {
	plan  Plan
	err   error
	calls atomic.Int32
}

func (f *fakePlanner) Plan(_ context.Context, _ string) (Plan, error) {
	f.calls.Add(1)
	return f.plan, f.err
}

type fakeWorker struct {
	mu      sync.Mutex
	calls   map[string]int
	prompts map[string][]string
	fn      func(spec SubtaskSpec) (string, int, error)
}

func newFakeWorker(fn func(spec SubtaskSpec) (string, int, error)) *fakeWorker {
	return &fakeWorker{calls: map[string]int{}, prompts: map[string][]string{}, fn: fn}
}

func (w *fakeWorker) Execute(_ context.Context, spec SubtaskSpec) (string, int, error) {
	w.mu.Lock()
	w.calls[spec.ID]++
	w.prompts[spec.ID] = append(w.prompts[spec.ID], spec.Prompt)
	w.mu.Unlock()
	return w.fn(spec)
}

func newOrch(store *task.Store, planner Planner, worker Worker, opts ...Option) *Orchestrator {
	base := []Option{WithTracer(trace.NewNoop())}
	return New(store, pool.New(4, 10*time.Second), planner, worker, append(base, opts...)...)
}

func threeSubPlan() Plan {
	return Plan{Subtasks: []SubtaskSpec{
		{ID: "s1", Title: "调研", Prompt: "调研 A 方案"},
		{ID: "s2", Title: "写稿", Prompt: "根据调研写稿"},
		{ID: "s3", Title: "校对", Prompt: "校对全文"},
	}}
}

func TestValidatePlan(t *testing.T) {
	valid := func() Plan { return threeSubPlan() }

	tooMany := Plan{}
	for i := 0; i < MaxSubtasks+1; i++ {
		tooMany.Subtasks = append(tooMany.Subtasks,
			SubtaskSpec{ID: fmt.Sprintf("s%d", i), Title: "t", Prompt: "p"})
	}

	dup := valid()
	dup.Subtasks[1].ID = "s1"

	noTitle := valid()
	noTitle.Subtasks[0].Title = "  "

	cases := []struct {
		name    string
		plan    Plan
		wantErr bool
	}{
		{"合法计划", valid(), false},
		{"空计划", Plan{}, true},
		{"超上限", tooMany, true},
		{"重复ID", dup, true},
		{"空Title", noTitle, true},
	}
	for _, c := range cases {
		if err := ValidatePlan(c.plan); (err != nil) != c.wantErr {
			t.Errorf("%s: ValidatePlan err = %v, wantErr = %v", c.name, err, c.wantErr)
		}
	}
}

// TestLLMPlanner_RetriesAfterInvalidOutput 验证教程 Q12 的核心路径：
// 第一次输出垃圾 → 带校验错误重试 → 第二次输出合法 JSON → 通过。
func TestLLMPlanner_RetriesAfterInvalidOutput(t *testing.T) {
	p := NewLLMPlanner(nil) // client 不会被用到：注入假 chat
	var calls atomic.Int32
	p.chat = func(messages []api.Message) (string, error) {
		n := calls.Add(1)
		if n == 1 {
			return "我觉得可以这样做：先做调研……", nil // 不是 JSON
		}
		// 第二次请求必须带着上次的校验错误反馈（messages 末尾两条）
		if len(messages) < 4 {
			t.Errorf("重试时 messages 应含原始输出+错误反馈，got %d 条", len(messages))
		} else if !strings.Contains(messages[len(messages)-1].Content, "未通过校验") {
			t.Errorf("重试反馈应包含校验错误，got: %q", messages[len(messages)-1].Content)
		}
		return `{"subtasks":[{"id":"s1","title":"调研","prompt":"p1"},{"id":"s2","title":"写稿","prompt":"p2"}]}`, nil
	}

	plan, err := p.Plan(context.Background(), "写一份竞品分析")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Subtasks) != 2 {
		t.Fatalf("got %d subtasks, want 2", len(plan.Subtasks))
	}
	if calls.Load() != 2 {
		t.Errorf("chat 调用 %d 次, want 2（1 次失败 + 1 次重试成功）", calls.Load())
	}
}

// TestLLMPlanner_ToleratesCodeFence 验证 ```json 围栏容错。
func TestLLMPlanner_ToleratesCodeFence(t *testing.T) {
	p := NewLLMPlanner(nil)
	p.chat = func(_ []api.Message) (string, error) {
		return "好的，这是计划：\n```json\n{\"subtasks\":[{\"id\":\"s1\",\"title\":\"a\",\"prompt\":\"p\"}]}\n```", nil
	}
	plan, err := p.Plan(context.Background(), "goal")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Subtasks) != 1 || plan.Subtasks[0].ID != "s1" {
		t.Errorf("unexpected plan: %+v", plan)
	}
}

// TestLLMPlanner_RetryExhausted 验证重试耗尽返回错误（总尝试 = 1 + maxRetries）。
func TestLLMPlanner_RetryExhausted(t *testing.T) {
	p := NewLLMPlanner(nil)
	var calls atomic.Int32
	p.chat = func(_ []api.Message) (string, error) {
		calls.Add(1)
		return "仍然是废话", nil
	}
	if _, err := p.Plan(context.Background(), "goal"); err == nil {
		t.Fatal("want error after retries exhausted, got nil")
	}
	if calls.Load() != 3 { // 1 次首发 + 2 次重试
		t.Errorf("chat 调用 %d 次, want 3", calls.Load())
	}
}

// TestRun_FullLifecycle 验证完整生命周期：状态迁移、checkpoint、幂等键、token 总账、汇总。
func TestRun_FullLifecycle(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	defer store.Close()

	planner := &fakePlanner{plan: threeSubPlan()}
	worker := newFakeWorker(func(spec SubtaskSpec) (string, int, error) {
		return "产出:" + spec.ID, 10, nil
	})
	o := newOrch(store, planner, worker)

	summary, err := o.Run(ctx, "task-1", "写一份报告")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		if !strings.Contains(summary, "产出:"+id) {
			t.Errorf("汇总缺少 %s 的产出:\n%s", id, summary)
		}
	}

	tk, subs, err := store.LoadTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}
	if tk.Status != task.StatusDone {
		t.Errorf("task status = %s, want done", tk.Status)
	}
	if tk.TotalTokens != 30 {
		t.Errorf("TotalTokens = %d, want 30（3 子任务 × 10）", tk.TotalTokens)
	}
	for _, s := range subs {
		if s.Status != task.StatusDone {
			t.Errorf("subtask %s status = %s, want done", s.ID, s.Status)
		}
		if s.IdempotencyKey != "task-1:"+s.ID {
			t.Errorf("subtask %s IdempotencyKey = %q, want task-1:%s", s.ID, s.IdempotencyKey, s.ID)
		}
		if s.TokensUsed != 10 || s.Attempts != 1 {
			t.Errorf("subtask %s tokens/attempts = %d/%d, want 10/1", s.ID, s.TokensUsed, s.Attempts)
		}
	}
}

// TestRun_PartialFailureStillSummarizes 验证部分失败语义：
// 一个子任务失败，任务仍 done，汇总含成功产出与失败原因。
func TestRun_PartialFailureStillSummarizes(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	defer store.Close()

	worker := newFakeWorker(func(spec SubtaskSpec) (string, int, error) {
		if spec.ID == "s2" {
			return "", 5, errors.New("LLM 超时")
		}
		return "产出:" + spec.ID, 10, nil
	})
	o := newOrch(store, &fakePlanner{plan: threeSubPlan()}, worker)

	summary, err := o.Run(ctx, "task-1", "写一份报告")
	if err != nil {
		t.Fatalf("Run: %v（部分失败不应让任务失败）", err)
	}
	if !strings.Contains(summary, "产出:s1") || !strings.Contains(summary, "产出:s3") {
		t.Errorf("汇总缺少成功子任务的产出:\n%s", summary)
	}
	if !strings.Contains(summary, "LLM 超时") {
		t.Errorf("汇总应包含失败原因:\n%s", summary)
	}
	tk, subs, _ := store.LoadTask(ctx, "task-1")
	if tk.Status != task.StatusDone {
		t.Errorf("task status = %s, want done（部分失败语义）", tk.Status)
	}
	if subs[1].Status != task.StatusFailed {
		t.Errorf("s2 status = %s, want failed", subs[1].Status)
	}
}

// TestRun_AllSubtasksFail_TaskFails 验证全部失败 → 任务 failed。
func TestRun_AllSubtasksFail_TaskFails(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	defer store.Close()

	worker := newFakeWorker(func(spec SubtaskSpec) (string, int, error) {
		return "", 0, errors.New("全都挂了")
	})
	o := newOrch(store, &fakePlanner{plan: threeSubPlan()}, worker)

	if _, err := o.Run(ctx, "task-1", "g"); err == nil {
		t.Fatal("want error when all subtasks fail, got nil")
	}
	tk, _, _ := store.LoadTask(ctx, "task-1")
	if tk.Status != task.StatusFailed {
		t.Errorf("task status = %s, want failed", tk.Status)
	}
}

// TestRun_PlannerFailureFailsTask 验证分解失败 → 任务 failed（planning → failed 合法迁移）。
func TestRun_PlannerFailureFailsTask(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	defer store.Close()

	planner := &fakePlanner{err: errors.New("LLM 一直输出垃圾")}
	o := newOrch(store, planner, newFakeWorker(nil))

	if _, err := o.Run(ctx, "task-1", "g"); err == nil {
		t.Fatal("want error, got nil")
	}
	tk, _, _ := store.LoadTask(ctx, "task-1")
	if tk.Status != task.StatusFailed {
		t.Errorf("task status = %s, want failed", tk.Status)
	}
}

// TestResume_SkipsDoneSubtasks 崩溃恢复演练（本练习的灵魂）：
// s1 已完成、s2 停在 running（被打断）、s3 pending → Close 重开 → Resume
// → s1 零调用，s2/s3 各执行一次，汇总含 s1 的 checkpoint 产出。
func TestResume_SkipsDoneSubtasks(t *testing.T) {
	ctx := context.Background()
	store, path := newTestStore(t)

	// 铺崩溃现场
	if err := store.CreateTask(ctx, "task-1", "写一份报告"); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(ctx, "task-1", task.StatusPlanning); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSubtasks(ctx, "task-1", []task.Subtask{
		{ID: "s1", Title: "调研", Prompt: "p1", IdempotencyKey: "task-1:s1"},
		{ID: "s2", Title: "写稿", Prompt: "p2", IdempotencyKey: "task-1:s2"},
		{ID: "s3", Title: "校对", Prompt: "p3", IdempotencyKey: "task-1:s3"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(ctx, "task-1", task.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionSubtask(ctx, "task-1", "s1", task.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSubtask(ctx, "task-1", "s1", "调研结论", 100); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionSubtask(ctx, "task-1", "s2", task.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil { // 模拟进程死亡
		t.Fatal(err)
	}

	// 重启 + 恢复
	store2, err := task.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()

	planner := &fakePlanner{err: errors.New("planner 不该被调用")}
	worker := newFakeWorker(func(spec SubtaskSpec) (string, int, error) {
		return "产出:" + spec.ID, 10, nil
	})
	o := newOrch(store2, planner, worker)

	summary, err := o.Resume(ctx, "task-1")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if planner.calls.Load() != 0 {
		t.Errorf("恢复已有计划的任务不应重新分解，planner 被调 %d 次", planner.calls.Load())
	}
	if worker.calls["s1"] != 0 {
		t.Errorf("s1 已 done，不应重复执行，实际 %d 次", worker.calls["s1"])
	}
	if worker.calls["s2"] != 1 || worker.calls["s3"] != 1 {
		t.Errorf("s2/s3 应各执行 1 次，实际 %d/%d", worker.calls["s2"], worker.calls["s3"])
	}
	if !strings.Contains(summary, "调研结论") {
		t.Errorf("汇总应含 s1 从 checkpoint 读出的产出:\n%s", summary)
	}
	tk, subs, _ := store2.LoadTask(ctx, "task-1")
	if tk.Status != task.StatusDone {
		t.Errorf("task status = %s, want done", tk.Status)
	}
	if subs[1].Attempts != 2 {
		t.Errorf("s2 Attempts = %d, want 2（被打断 1 次 + 重跑 1 次）", subs[1].Attempts)
	}
}
```

## 二、关键设计点

1. **Planner/Worker 都是接口，这是本练习最重要的设计**。两个理由：① 可测性——编排器的核心是状态机驱动与 checkpoint 时序，这些逻辑必须能不烧 token、不依赖网络地测；接口注入假 Planner/Worker 后，9 个测试全是离线确定性测试。② 模型分级（教程 Q5）——planner 值得用强模型（分解错了全盘皆输），worker 可以用便宜模型；接口让"换模型"只是换一个实现，编排器一行不改。**面试话术**：接口不是为抽象而抽象，每换一个实现（假实现/分级模型/固定模板 planner）都是真实需求。

2. **`chat` 函数字段是对"不可测试的具体类型"的补救**。`llm.Client` 的 baseURL 是私有字段且没有 `WithBaseURL`，httptest 假服务器接不进去；如果 `Plan` 直接调 `p.client.ChatWithRetry`，"校验失败重试"这条核心路径就无法离线测。把"发一次问答"抽成可注入字段（构造函数装配真实实现），是同包测试替换的标准手法。生产上更彻底的做法是给 mini-agent 的 llm.Client 补 `WithBaseURL`——这属于内核侧的改动，本练习不动它。

3. **LLM 调用失败与校验失败走两条不同的重试路径**。chat 返回 error（网络/限流）时直接返回，不再重试——内层 `ChatWithRetry` 已做过指数退避，外层再循环是双重退避；只有"调用成功但内容不合法"才带反馈重试。易错写法是把两种错误混在一起统一重试：限流时被双重退避拖慢，还把 401 这种永远失败的错误也重试。

4. **每个子任务 new 一个 Agent 是 context 隔离的落地**（教程核心概念第 1 条）。共享一个 Agent 意味着所有子任务的工具返回、中间推理堆进同一份 messages——context 膨胀 + 噪声稀释。每子任务新 Agent：system prompt + 自包含的子任务 prompt，跑完即弃。代价是每个子任务重新付一份 system prompt 的 token——这就是教程说的"多 agent 用成本换可控 context"。

5. **汇总以 checkpoint 为准，不用内存里的 results 拼**。`dispatch` 末尾重新 `LoadTask` 再汇总——Resume 场景下，一部分产出是上一轮（甚至上一个进程）写进 SQLite 的，本轮 `pool.Run` 的 results 里根本没有。"状态在库里，进程无状态"要贯彻到最后一个环节，否则 Resume 出来的汇总会丢已完成部分。

6. **幂等键在编排层的两个兑现点**：① `dispatch` 跳过 `done` 子任务（恢复时不重跑）；② `running` 状态的子任务先迁回 `pending` 再重跑（练习2 被测试逼出来的那条边，在这里真正被消费）。再加上练习2 里 `CompleteSubtask` 的 DB 侧幂等，形成"恢复逻辑判跳过 + 写路径防双写"的双保险。

7. **worker 的 token 记账走内核的 `Agent.Usage()`**：`Agent.Run` 的签名是 `(string, error)` 不含 Usage，但内核把每轮 ReAct 的 token 用量累计在 Agent 实例上，`Run` 结束后读 `ag.Usage().TotalTokens` 即得整趟子任务的总消耗。这是"接口设计要前瞻"的一段活教材：阶段一设计 Run 签名时没留 Usage 返回值，阶段三做成本核算时就要补——补救方式有两种，改签名（破坏既有调用方）或加访问器（`Usage()`，向后兼容），内核选了后者。面试被问"加能力时怎么不动既有接口"，这就是答案。**易错处**：`Usage()` 是整趟 Run 的累计量，必须在 `Run` 返回后读；且一次 Execute 对应一个新 Agent，不存在跨子任务串账。

8. **汇总用确定性拼接而非 LLM 汇总**。再起一次 LLM 调用做"智能汇总"是常见诱惑，但：多烧一轮 token、引入新的失败点、格式不可控。确定性拼接（标题+产出，失败项带原因）够用且可靠；LLM 汇总是产品化阶段（练习8）可以按需叠加的增强，不是编排器的职责底线。

## 三、进阶实现（加分项：子任务依赖 DAG 分发）

> 回补记录：本节代码于 2026-08-14 以临时文件（`internal/orchestrator/dag.go` + `dag_test.go`）
> 实际粘贴进项目验证，`go vet ./internal/orchestrator/` 与
> `go test ./internal/orchestrator/ -race -count=1` 全部通过（连同基础与练习4 共 21 个测试），
> 验证后已从项目删除——**进阶实现只属于答案，不进项目代码树**，项目保持骨架版。

### 设计取舍（先说清楚再看代码）

- **为什么不直接给 `SubtaskSpec` 加 `DependsOn` 字段**：基础版契约是"子任务相互独立、全并行"，加一个大多数人留空的字段，会让"我以为我写了依赖其实没写"这类错误静默通过。独立类型 `DAGSubtask` 让"表达依赖"成为显式选择。
- **分波（wave）而非逐依赖调度**：Kahn 算法按层出队——同一波内无依赖可并行（丢给 pool），波与波之间串行等待。这是"并行度"与"依赖正确性"的最简单兼得；基础版 dispatch 相当于只有一波的特例。
- **前置失败快速失败**：某波有子任务失败时不再执行后续波（下游反正缺输入）。生产可放宽为"只跳过失败节点的下游"，学习项目先讲清主路径。
- **成环/缺失依赖必须在进状态机前拦下**：与 ValidatePlan 同一思想——模型（或人）给出的依赖图也是不可信输入，确定性校验兜底。

### `internal/orchestrator/dag.go`（进阶实现完整代码）

```go
package orchestrator

import (
	"context"
	"fmt"

	"stage-03-multi-agent/internal/pool"
)

// DAGSubtask 在 SubtaskSpec 之上声明前置依赖。
type DAGSubtask struct {
	SubtaskSpec
	DependsOn []string // 前置子任务 ID 列表（必须在同一计划内）
}

// TopoWaves 用 Kahn 算法把 DAG 子任务分层：
// 返回的每个 wave 内部相互无依赖（可并行），wave 之间按依赖序排列。
// 依赖缺失、自依赖、成环都返回错误——进状态机前的又一道确定性校验。
func TopoWaves(subs []DAGSubtask) ([][]DAGSubtask, error) {
	byID := make(map[string]DAGSubtask, len(subs))
	for _, s := range subs {
		if _, dup := byID[s.ID]; dup {
			return nil, fmt.Errorf("dag: 重复 ID %q", s.ID)
		}
		byID[s.ID] = s
	}
	indeg := make(map[string]int, len(subs))
	dependents := make(map[string][]string) // dep -> 依赖它的子任务
	for _, s := range subs {
		for _, dep := range s.DependsOn {
			if dep == s.ID {
				return nil, fmt.Errorf("dag: %q 依赖自身", s.ID)
			}
			if _, ok := byID[dep]; !ok {
				return nil, fmt.Errorf("dag: %q 依赖不存在的子任务 %q", s.ID, dep)
			}
			indeg[s.ID]++
			dependents[dep] = append(dependents[dep], s.ID)
		}
	}

	var waves [][]DAGSubtask
	var frontier []string
	for _, s := range subs { // 按声明顺序入队，保证波内顺序稳定（测试可断言）
		if indeg[s.ID] == 0 {
			frontier = append(frontier, s.ID)
		}
	}
	resolved := 0
	for len(frontier) > 0 {
		wave := make([]DAGSubtask, 0, len(frontier))
		var next []string
		for _, id := range frontier {
			wave = append(wave, byID[id])
			resolved++
			for _, d := range dependents[id] {
				indeg[d]--
				if indeg[d] == 0 {
					next = append(next, d)
				}
			}
		}
		waves = append(waves, wave)
		frontier = next
	}
	if resolved != len(subs) {
		return nil, fmt.Errorf("dag: 依赖成环，%d/%d 个子任务可调度", resolved, len(subs))
	}
	return waves, nil
}

// DispatchDAG 按拓扑序分波执行：每波丢给 pool 并行，波间串行等待。
// checkpoint/评审循环不变：每波内 job 的执行体仍是 runSubtask。
func (o *Orchestrator) DispatchDAG(ctx context.Context, p *pool.Pool, subs []DAGSubtask,
	run func(ctx context.Context, spec SubtaskSpec) (string, error)) error {

	waves, err := TopoWaves(subs)
	if err != nil {
		return err
	}
	for i, wave := range waves {
		jobs := make([]pool.Job, len(wave))
		for j, s := range wave {
			spec := s.SubtaskSpec
			jobs[j] = pool.Job{
				ID: s.ID,
				Run: func(jctx context.Context) (string, error) { return run(jctx, spec) },
			}
		}
		for _, r := range p.Run(ctx, jobs) {
			if r.Err != nil {
				// 前置失败，后续波没有执行意义：快速失败。
				//（生产可放宽为"只跳过依赖失败节点的下游"，学习项目先讲清主路径。）
				return fmt.Errorf("dag: 第 %d 波子任务 %s 失败: %w", i, r.ID, r.Err)
			}
		}
	}
	return nil
}
```

### `internal/orchestrator/dag_test.go`（进阶测试完整代码）

```go
package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"stage-03-multi-agent/internal/pool"
)

func TestTopoWaves_LinearChain(t *testing.T) {
	subs := []DAGSubtask{
		{SubtaskSpec: SubtaskSpec{ID: "a"}},
		{SubtaskSpec: SubtaskSpec{ID: "b"}, DependsOn: []string{"a"}},
		{SubtaskSpec: SubtaskSpec{ID: "c"}, DependsOn: []string{"b"}},
	}
	waves, err := TopoWaves(subs)
	if err != nil {
		t.Fatalf("TopoWaves: %v", err)
	}
	if len(waves) != 3 || waves[0][0].ID != "a" || waves[1][0].ID != "b" || waves[2][0].ID != "c" {
		t.Errorf("链式依赖应分 3 波 a→b→c，got %v", waveIDs(waves))
	}
}

func TestTopoWaves_Diamond(t *testing.T) {
	subs := []DAGSubtask{
		{SubtaskSpec: SubtaskSpec{ID: "a"}},
		{SubtaskSpec: SubtaskSpec{ID: "b"}, DependsOn: []string{"a"}},
		{SubtaskSpec: SubtaskSpec{ID: "c"}, DependsOn: []string{"a"}},
		{SubtaskSpec: SubtaskSpec{ID: "d"}, DependsOn: []string{"b", "c"}},
	}
	waves, err := TopoWaves(subs)
	if err != nil {
		t.Fatalf("TopoWaves: %v", err)
	}
	if len(waves) != 3 || len(waves[1]) != 2 {
		t.Fatalf("菱形依赖应分 3 波（中间波 2 个并行），got %v", waveIDs(waves))
	}
	if waves[2][0].ID != "d" {
		t.Errorf("d 应在最后一波，got %v", waveIDs(waves))
	}
}

func TestTopoWaves_RejectsCycleAndMissingDep(t *testing.T) {
	cycle := []DAGSubtask{
		{SubtaskSpec: SubtaskSpec{ID: "a"}, DependsOn: []string{"b"}},
		{SubtaskSpec: SubtaskSpec{ID: "b"}, DependsOn: []string{"a"}},
	}
	if _, err := TopoWaves(cycle); err == nil {
		t.Error("成环应报错，got nil")
	}
	missing := []DAGSubtask{{SubtaskSpec: SubtaskSpec{ID: "a"}, DependsOn: []string{"ghost"}}}
	if _, err := TopoWaves(missing); err == nil {
		t.Error("依赖不存在的子任务应报错，got nil")
	}
	self := []DAGSubtask{{SubtaskSpec: SubtaskSpec{ID: "a"}, DependsOn: []string{"a"}}}
	if _, err := TopoWaves(self); err == nil {
		t.Error("自依赖应报错，got nil")
	}
}

// TestDispatchDAG_RespectsOrder 用 pool 真实分波执行，验证依赖序：
// b/c 必须晚于 a 开始，d 必须最晚。
func TestDispatchDAG_RespectsOrder(t *testing.T) {
	subs := []DAGSubtask{
		{SubtaskSpec: SubtaskSpec{ID: "a"}},
		{SubtaskSpec: SubtaskSpec{ID: "b"}, DependsOn: []string{"a"}},
		{SubtaskSpec: SubtaskSpec{ID: "c"}, DependsOn: []string{"a"}},
		{SubtaskSpec: SubtaskSpec{ID: "d"}, DependsOn: []string{"b", "c"}},
	}
	var mu sync.Mutex
	var order []string
	o := &Orchestrator{}
	err := o.DispatchDAG(context.Background(), pool.New(2, 5*time.Second), subs,
		func(_ context.Context, spec SubtaskSpec) (string, error) {
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			order = append(order, spec.ID)
			mu.Unlock()
			return "ok", nil
		})
	if err != nil {
		t.Fatalf("DispatchDAG: %v", err)
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if pos["a"] > pos["b"] || pos["a"] > pos["c"] || pos["d"] <= pos["b"] || pos["d"] <= pos["c"] {
		t.Errorf("执行顺序违反依赖: %v", order)
	}
}

// TestDispatchDAG_FailsFast 前置失败 → 后续波不执行。
func TestDispatchDAG_FailsFast(t *testing.T) {
	subs := []DAGSubtask{
		{SubtaskSpec: SubtaskSpec{ID: "a"}},
		{SubtaskSpec: SubtaskSpec{ID: "b"}, DependsOn: []string{"a"}},
	}
	var ran []string
	o := &Orchestrator{}
	err := o.DispatchDAG(context.Background(), pool.New(2, 5*time.Second), subs,
		func(_ context.Context, spec SubtaskSpec) (string, error) {
			ran = append(ran, spec.ID)
			if spec.ID == "a" {
				return "", errors.New("a 挂了")
			}
			return "ok", nil
		})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if len(ran) != 1 || ran[0] != "a" {
		t.Errorf("b 不应执行（前置失败快速失败），ran = %v", ran)
	}
}

func waveIDs(waves [][]DAGSubtask) [][]string {
	out := make([][]string, len(waves))
	for i, w := range waves {
		for _, s := range w {
			out[i] = append(out[i], s.ID)
		}
	}
	return out
}
```

### 进阶实现的易错处

1. **Kahn 出队条件写错**：`indeg[d] == 0` 时才入下一波——写成 `<= 0` 会重复入队；忘记递减则永远只剩第一波。
2. **成环检测靠计数**：`resolved != len(subs)` 才是环的判据，不存在"frontier 为空即完成"的捷径——环上的节点入度永远不为 0，frontier 会提前空。
3. **闭包捕获**：`spec := s.SubtaskSpec` 的显式拷贝让 job 闭包捕获波内各自的 spec（Go 1.22+ 循环变量已每轮新实例，拷贝是给读者看的防御）。
4. **下游怎么拿到上游产出**：本实现的 wave 间传参没展开——生产做法是上游产出写 checkpoint（已经有了），下游子任务的 prompt 在分发时动态拼入上游 output（从 store 读出）。这是把 DispatchDAG 合入编排器时的主要工作量。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [ ] `ValidatePlan` 覆盖空计划 / 超上限 / 重复 ID / 空字段，错误信息带定位（哪个子任务什么问题）
- [ ] `LLMPlanner.Plan`：system prompt 约束只输出 JSON + schema 示例；解析容错 ```json 围栏；校验失败把【原始输出 + 错误】喂回重试（限次，耗尽返回错误）；LLM 调用失败不再外层重试
- [ ] `AgentWorker.Execute`：每子任务 new 一个 `api.Agent`（context 隔离），worker 专用 system prompt 要求自包含结论；tokens 用 `ag.Usage().TotalTokens` 真实记账（Run 结束后读，整趟累计量）
- [ ] `Run` 生命周期：CreateTask → planning → Plan → SaveSubtasks（幂等键 = taskID+":"+子任务ID）→ running → 分发 → 汇总 → done/failed，每次迁移都落盘
- [ ] `Resume`：LoadTask → 已 done 跳过、running/failed 重置 pending 重跑；汇总以 checkpoint 为准（不是内存结果）
- [ ] 部分失败语义：单个子任务失败任务仍 done 且汇总含失败原因；全部失败任务 failed
- [ ] 测试覆盖：生命周期、校验重试、崩溃恢复（s1 done 跳过 + s2 重跑 attempts=2 + 汇总含 checkpoint 产出）、部分失败，全部离线可跑
- [ ] `go vet ./internal/orchestrator/` 和 `go test ./internal/orchestrator/ -race -count=1` 全绿
- [ ] 能口头回答：为什么 Planner/Worker 是接口？为什么 worker 每子任务新 Agent？为什么汇总读 checkpoint 而不是内存结果？

加分项（做了才需要勾，参考"进阶实现"一节）：

- [ ] DAG 分发：Kahn 分波拓扑排序，波内并行波间串行；成环/缺失依赖/自依赖在进状态机前报错；前置失败快速失败；有用 pool 真实跑通的顺序断言测试
- [ ] 能口头回答：DAG 形态下"下游拿上游产出"怎么落地（checkpoint 读上游 output 拼下游 prompt）？
