# 练习 4 参考答案：Critic 评审循环 + 双重熔断

> 对应 TODO：`stage-03-multi-agent/internal/orchestrator/` 的 `TODO(练习4)`
> （`critic.go` 的 LLMCritic.Review、`orchestrator.go` 的评审循环与熔断）。
> **完成练习并自评后再看本文档。**
>
> 本文档基础实现代码已于 2026-08-14 实际粘贴进项目验证：
> `go vet ./internal/orchestrator/` 与 `go test ./internal/orchestrator/ -race -count=1`
> 全部通过（14 个测试：练习3 的 9 个 + 练习4 的 5 个，`-race` 无数据竞争）。
> 进阶实现（critic 结构化评审，见第三节）同日验证：临时粘贴为
> `critic_structured.go` + `critic_structured_test.go`，连同练习3 进阶（DAG）
> 共 21 个测试全绿，验证后即删除，项目保持骨架版。
>
> **复验记录（2026-08-14）**：mini-agent 内核补齐 `Agent.Usage()` 后，
> 练习3 的 `AgentWorker.Execute` 改为真实 token 记账（预算熔断从此有真实数据），
> 本次复验重新临时落地全部实现与测试，`go vet` 与 `go test -race -count=1`
> 21 个测试再次全绿，验证后恢复骨架。
>
> **验证前提**：同练习3 答案——验证时临时落地了练习1（Pool.Run）与
> 练习2（Store）的参考实现，验证后已恢复骨架。
> 本练习的代码是在练习3 参考实现之上的**增量修改**：先完成练习3 再叠加本练习。

---

## 一、参考实现

### `internal/orchestrator/critic.go`（LLMCritic.Review）

import 扩为：`context` / `fmt` / `strings` / `mini-agent/api`。

```go
// LLMCritic 用 LLM 做评审。
type LLMCritic struct {
	client *api.Client
	// chat 与 LLMPlanner 同样的可注入设计（llm.Client 无法指向假服务器）；
	// 多返回一个 tokens——评审消耗要计入任务预算熔断。
	chat func(messages []api.Message) (content string, tokens int, err error)
}

// NewLLMCritic 构造一个 LLM 评审者。
func NewLLMCritic(client *api.Client) *LLMCritic {
	c := &LLMCritic{client: client}
	c.chat = func(messages []api.Message) (string, int, error) {
		resp, err := client.Chat(messages, nil)
		if err != nil {
			return "", 0, err
		}
		return resp.Choices[0].Message.Content, resp.Usage.TotalTokens, nil
	}
	return c
}

// criticSystemPrompt 用裸文本约定而非 JSON：评审结论只有两种，字符串解析够用
// 且更省 token；结构化评审（score + issues JSON）见"进阶实现"一节。
const criticSystemPrompt = `你是质量评审（critic）。输入是一个子任务的要求和执行 agent 的产出。
评审要点：
- 产出是否回答了子任务的要求；
- 是否有实质内容（不是空话套话）；
- 事实是否明显有误。
只评不改——重写是执行 agent 的事。
输出格式（严格遵守）：
- 合格：第一行只写 PASS
- 不合格：第一行写 REJECT，第二行起写具体、可执行的修改意见`

// Review 评审 spec 的产出 output。
func (c *LLMCritic) Review(ctx context.Context, spec SubtaskSpec, output string) (Verdict, string, int, error) {
	if err := ctx.Err(); err != nil {
		return VerdictPass, "", 0, err
	}
	user := fmt.Sprintf("【子任务要求】\n标题：%s\n指令：%s\n\n【执行产出】\n%s",
		spec.Title, spec.Prompt, output)
	raw, tokens, err := c.chat([]api.Message{
		{Role: "system", Content: criticSystemPrompt},
		{Role: "user", Content: user},
	})
	if err != nil {
		return VerdictPass, "", tokens, fmt.Errorf("critic: LLM 调用失败: %w", err)
	}
	line, rest, _ := strings.Cut(strings.TrimSpace(raw), "\n")
	switch strings.ToUpper(strings.TrimSpace(line)) {
	case "PASS":
		return VerdictPass, "", tokens, nil
	case "REJECT":
		feedback := strings.TrimSpace(rest)
		if feedback == "" {
			// 空反馈等于让 worker 盲改，补一句兜底。
			feedback = "评审未通过但未给出具体意见，请对照子任务要求全面检查并改进。"
		}
		return VerdictReject, feedback, tokens, nil
	default:
		// 既非 PASS 也非 REJECT = critic 本次不可用，返回 error 走降级路径——
		// 不误判成 reject 让 worker 白重做，也不误判成 pass 静默放水。
		return VerdictPass, "", tokens, fmt.Errorf("critic: 无法解析评审结论（首行 %q）", truncate(line, 40))
	}
}
```

### `internal/orchestrator/orchestrator.go`（在练习3 实现之上的增量）

**1. Orchestrator 结构体加两个运行时字段**（critic 降级用，并发安全必须 atomic）：

```go
	// critic 降级的运行时状态。pool 并发执行多个子任务，这两个计数器
	// 被多个 goroutine 读写，必须用 atomic（go test -race 会抓）。
	criticErrors   atomic.Int32 // critic 连续出错计数（成功一次清零）
	criticDisabled atomic.Bool  // 连续出错达阈值后整个任务跳过评审
```

（import 增加 `sync/atomic`；`errors` 包此时真正用到——哨兵错误。）

**2. 新增哨兵错误与预算检查**：

```go
// ErrBudgetExceeded 是预算熔断的哨兵错误：
// 子任务 job 返回它，分发结束后编排器用 errors.Is 识别并把任务迁为 failed。
// 哨兵错误而非字符串匹配，是为了让调用方能程序化地区分"烧钱烧停的"
// 和"真失败"——看板上这两种失败的处置完全不同（加预算续跑 vs 排查 bug）。
var ErrBudgetExceeded = errors.New("orchestrator: token 预算耗尽")

// checkBudget 预算熔断：tokenBudget > 0 且累计消耗已达预算即熔断。
func (o *Orchestrator) checkBudget(consumed *atomic.Int64) error {
	if o.tokenBudget > 0 && consumed.Load() >= int64(o.tokenBudget) {
		return ErrBudgetExceeded
	}
	return nil
}
```

**3. `dispatch` 增加预算计数器与熔断判定**（替换练习3 版本的两处）：

```go
func (o *Orchestrator) dispatch(ctx context.Context, taskID, root string) (string, error) {
	t, subs, err := o.store.LoadTask(ctx, taskID)
	if err != nil {
		return "", err
	}

	// token 预算从 checkpoint 续算：崩溃恢复前已烧的 token 也占预算。
	consumed := atomic.Int64{}
	consumed.Store(int64(t.TotalTokens))
	// 每个任务周期的降级状态独立：新任务/新恢复周期重新给 critic 机会。
	o.criticErrors.Store(0)
	o.criticDisabled.Store(false)

	var jobs []pool.Job
	for _, sub := range subs {
		if sub.Status == task.StatusDone {
			continue // 幂等键语义：这份活干过了，跳过
		}
		if sub.Status == task.StatusRunning || sub.Status == task.StatusFailed {
			if err := o.store.TransitionSubtask(ctx, taskID, sub.ID, task.StatusPending); err != nil {
				return "", err
			}
		}
		spec := SubtaskSpec{ID: sub.ID, Title: sub.Title, Prompt: sub.Prompt,
			RequiresApproval: sub.RequiresApproval}
		jobs = append(jobs, pool.Job{
			ID: sub.ID,
			Run: func(jctx context.Context) (string, error) {
				return o.runSubtask(jctx, taskID, root, spec, &consumed) // 多传 consumed
			},
		})
	}
	results := o.pool.Run(ctx, jobs)

	// 预算熔断：任一子任务撞上预算 → 整个任务 failed（防 critic 循环烧钱）。
	for _, r := range results {
		if errors.Is(r.Err, ErrBudgetExceeded) {
			o.failTask(ctx, taskID)
			return "", fmt.Errorf("orchestrator: 任务 %s 超出 token 预算，已熔断: %w", taskID, ErrBudgetExceeded)
		}
	}

	// 汇总与终态迁移与练习3 相同（以 checkpoint 为准重新 LoadTask）。
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
```

**4. `runSubtask` 替换为评审循环版**（本练习的核心）：

```go
// runSubtask 是单个子任务的完整执行路径（评审循环所在），由 pool 的 job 调用。
func (o *Orchestrator) runSubtask(ctx context.Context, taskID, root string, spec SubtaskSpec, consumed *atomic.Int64) (string, error) {
	span := o.tracer.StartSpan(ctx, root, "worker: "+spec.ID, map[string]any{"title": spec.Title})

	if err := o.store.TransitionSubtask(ctx, taskID, spec.ID, task.StatusRunning); err != nil {
		o.tracer.EndSpan(ctx, span, 0, 0, err)
		return "", fmt.Errorf("orchestrator: 子任务 %s 启动失败: %w", spec.ID, err)
	}

	feedback := ""
	totalTokens := 0
	for round := 0; ; round++ {
		// 熔断二（检查点在每次 LLM 调用之前）：预算耗尽立即停止烧钱。
		if err := o.checkBudget(consumed); err != nil {
			o.tracer.EndSpan(ctx, span, 0, totalTokens, err)
			return "", err
		}
		// 打回重做：feedback 拼进 spec 的副本——spec 是值类型，
		// 但显式拷贝让"不改共享状态"的意图可见。
		attempt := spec
		if feedback != "" {
			attempt.Prompt = spec.Prompt + "\n\n【上次产出未通过评审，评审意见】：" + feedback +
				"\n请针对意见修正后重新完成本子任务。"
		}
		output, tokens, err := o.worker.Execute(ctx, attempt)
		consumed.Add(int64(tokens))
		totalTokens += tokens
		if err != nil {
			o.failSubtask(ctx, taskID, spec.ID, err)
			o.tracer.EndSpan(ctx, span, 0, totalTokens, err)
			return "", fmt.Errorf("orchestrator: 子任务 %s 执行失败: %w", spec.ID, err)
		}

		if o.critic == nil || o.criticDisabled.Load() {
			return o.completeSubtask(ctx, span, taskID, spec.ID, output, totalTokens)
		}
		if err := o.checkBudget(consumed); err != nil {
			o.tracer.EndSpan(ctx, span, 0, totalTokens, err)
			return "", err
		}
		verdict, fb, ctokens, cerr := o.critic.Review(ctx, spec, output)
		consumed.Add(int64(ctokens))
		totalTokens += ctokens
		if cerr != nil {
			// critic 降级：评审出错【放行】本次产出（记 log），不把 critic 的故障
			// 转嫁成子任务失败；连续出错达阈值后整个任务跳过评审——
			// critic 是质量增强，不是单点故障（教程第 5 条）。
			n := o.criticErrors.Add(1)
			log.Printf("orchestrator: critic 评审出错（连续第 %d 次），子任务 %s 降级放行: %v", n, spec.ID, cerr)
			if n >= 2 {
				o.criticDisabled.Store(true)
				log.Printf("orchestrator: critic 连续不可用，任务 %s 后续子任务跳过评审", taskID)
			}
			return o.completeSubtask(ctx, span, taskID, spec.ID, output, totalTokens)
		}
		o.criticErrors.Store(0) // 成功一次，连续计数清零
		if verdict == VerdictPass {
			return o.completeSubtask(ctx, span, taskID, spec.ID, output, totalTokens)
		}
		// 熔断一：评审轮次上限。maxRounds = 子任务最多被执行的次数（首轮 + 重做）。
		if round+1 >= o.maxCriticRounds {
			err := fmt.Errorf("orchestrator: 子任务 %s 评审 %d 轮仍未通过，熔断（最后意见：%s）",
				spec.ID, o.maxCriticRounds, fb)
			o.failSubtask(ctx, taskID, spec.ID, err)
			o.tracer.EndSpan(ctx, span, 0, totalTokens, err)
			return "", err
		}
		feedback = fb
	}
}

// completeSubtask 是子任务成功时的 checkpoint 落盘 + span 收尾。
func (o *Orchestrator) completeSubtask(ctx context.Context, span, taskID, subID, output string, tokens int) (string, error) {
	if err := o.store.CompleteSubtask(ctx, taskID, subID, output, tokens); err != nil {
		o.tracer.EndSpan(ctx, span, 0, tokens, err)
		return "", fmt.Errorf("orchestrator: 子任务 %s checkpoint 失败: %w", subID, err)
	}
	o.tracer.EndSpan(ctx, span, 0, tokens, nil)
	return output, nil
}
```

### 测试（追加进 `orchestrator_test.go`，假 Critic 无需网络）

```go
// fakeCritic 脚本化评审：先 reject 后 pass、永远 reject、出错降级全靠它构造。
type fakeCritic struct {
	mu    sync.Mutex
	calls int
	fn    func(spec SubtaskSpec, output string) (Verdict, string, int, error)
}

func (c *fakeCritic) Review(_ context.Context, spec SubtaskSpec, output string) (Verdict, string, int, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.fn(spec, output)
}

// TestCriticLoop_RejectThenPass 验证打回重做：第一次 reject（带意见），
// 第二次 pass；worker 被调 2 次，且第 2 次的 prompt 含评审意见。
func TestCriticLoop_RejectThenPass(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	defer store.Close()

	plan := Plan{Subtasks: []SubtaskSpec{{ID: "s1", Title: "写稿", Prompt: "写一份纪要"}}}
	worker := newFakeWorker(func(spec SubtaskSpec) (string, int, error) {
		return "纪要 v" + fmt.Sprint(len(spec.Prompt)), 10, nil
	})
	var criticCalls atomic.Int32
	critic := &fakeCritic{fn: func(_ SubtaskSpec, _ string) (Verdict, string, int, error) {
		if criticCalls.Add(1) == 1 {
			return VerdictReject, "缺少数据支撑", 5, nil
		}
		return VerdictPass, "", 5, nil
	}}
	o := newOrch(store, &fakePlanner{plan: plan}, worker, WithCritic(critic, 3))

	if _, err := o.Run(ctx, "task-1", "g"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if worker.calls["s1"] != 2 {
		t.Errorf("worker 应执行 2 次（首轮 + 重做 1 次），实际 %d", worker.calls["s1"])
	}
	if !strings.Contains(worker.prompts["s1"][1], "缺少数据支撑") {
		t.Errorf("重做 prompt 应含评审意见，got: %q", worker.prompts["s1"][1])
	}
	tk, subs, _ := store.LoadTask(ctx, "task-1")
	if tk.Status != task.StatusDone || subs[0].Status != task.StatusDone {
		t.Errorf("task/subtask = %s/%s, want done/done", tk.Status, subs[0].Status)
	}
	if tk.TotalTokens != 30 {
		t.Errorf("TotalTokens = %d, want 30（2×worker 10 + 2×critic 5）", tk.TotalTokens)
	}
}

// TestCriticLoop_RoundBreaker 验证熔断一：永远 reject → 达到轮次上限 → 子任务 failed。
func TestCriticLoop_RoundBreaker(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	defer store.Close()

	plan := Plan{Subtasks: []SubtaskSpec{{ID: "s1", Title: "写稿", Prompt: "p"}}}
	worker := newFakeWorker(func(spec SubtaskSpec) (string, int, error) {
		return "产出", 10, nil
	})
	critic := &fakeCritic{fn: func(_ SubtaskSpec, _ string) (Verdict, string, int, error) {
		return VerdictReject, "还是不行", 5, nil
	}}
	o := newOrch(store, &fakePlanner{plan: plan}, worker, WithCritic(critic, 2))

	if _, err := o.Run(ctx, "task-1", "g"); err == nil {
		t.Fatal("want error（唯一子任务熔断 = 全部失败）, got nil")
	}
	if worker.calls["s1"] != 2 {
		t.Errorf("worker 应执行恰好 maxRounds=2 次，实际 %d", worker.calls["s1"])
	}
	_, subs, _ := store.LoadTask(ctx, "task-1")
	if subs[0].Status != task.StatusFailed {
		t.Errorf("s1 status = %s, want failed", subs[0].Status)
	}
	if !strings.Contains(subs[0].Output, "熔断") {
		t.Errorf("失败原因应注明评审熔断，got: %q", subs[0].Output)
	}
}

// TestTokenBudgetBreaker 验证熔断二：worker+critic 累计 token 超预算 → 任务 failed。
func TestTokenBudgetBreaker(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	defer store.Close()

	plan := Plan{Subtasks: []SubtaskSpec{{ID: "s1", Title: "写稿", Prompt: "p"}}}
	worker := newFakeWorker(func(spec SubtaskSpec) (string, int, error) {
		return "产出", 100, nil
	})
	critic := &fakeCritic{fn: func(_ SubtaskSpec, _ string) (Verdict, string, int, error) {
		return VerdictReject, "再改改", 60, nil
	}}
	// 预算 150：第 1 轮 worker(100) + critic(60) = 160 ≥ 150 → 第 2 轮开工前熔断。
	o := newOrch(store, &fakePlanner{plan: plan}, worker, WithCritic(critic, 5), WithTokenBudget(150))

	_, err := o.Run(ctx, "task-1", "g")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("want ErrBudgetExceeded, got %v", err)
	}
	if worker.calls["s1"] != 1 {
		t.Errorf("worker 应只执行 1 轮（第 2 轮开工前熔断），实际 %d", worker.calls["s1"])
	}
	tk, _, _ := store.LoadTask(ctx, "task-1")
	if tk.Status != task.StatusFailed {
		t.Errorf("task status = %s, want failed（预算熔断）", tk.Status)
	}
}

// TestCriticError_Degrades 验证降级：critic 连续出错 → 放行 + 记 log →
// 达到阈值后整个任务跳过评审（第 3 个子任务不再调 Review）。
func TestCriticError_Degrades(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	defer store.Close()

	worker := newFakeWorker(func(spec SubtaskSpec) (string, int, error) {
		return "产出:" + spec.ID, 10, nil
	})
	critic := &fakeCritic{fn: func(_ SubtaskSpec, _ string) (Verdict, string, int, error) {
		return VerdictPass, "", 0, errors.New("critic 服务挂了")
	}}
	// 并发度 1 保证顺序确定：s1 出错(1)、s2 出错(2)→禁用、s3 跳过评审。
	o := New(store, pool.New(1, 10*time.Second), &fakePlanner{plan: threeSubPlan()}, worker,
		WithTracer(trace.NewNoop()), WithCritic(critic, 3))

	if _, err := o.Run(ctx, "task-1", "g"); err != nil {
		t.Fatalf("Run: %v（critic 故障不应让任务失败）", err)
	}
	critic.mu.Lock()
	got := critic.calls
	critic.mu.Unlock()
	if got != 2 {
		t.Errorf("critic 应只被调 2 次（连续出错 2 次后禁用），实际 %d", got)
	}
	tk, subs, _ := store.LoadTask(ctx, "task-1")
	if tk.Status != task.StatusDone {
		t.Errorf("task status = %s, want done（降级放行）", tk.Status)
	}
	for _, s := range subs {
		if s.Status != task.StatusDone {
			t.Errorf("subtask %s status = %s, want done", s.ID, s.Status)
		}
	}
}

// TestLLMCritic_ParsesVerdict 验证结论解析三分支：PASS / REJECT 带意见 / 垃圾走 error。
func TestLLMCritic_ParsesVerdict(t *testing.T) {
	c := NewLLMCritic(nil)

	c.chat = func(_ []api.Message) (string, int, error) { return "PASS", 7, nil }
	v, _, tokens, err := c.Review(context.Background(), SubtaskSpec{ID: "s1"}, "产出")
	if err != nil || v != VerdictPass || tokens != 7 {
		t.Errorf("PASS case: v=%v tokens=%d err=%v", v, tokens, err)
	}

	c.chat = func(_ []api.Message) (string, int, error) {
		return "REJECT\n内容太空泛，缺少数据", 9, nil
	}
	v, fb, tokens, err := c.Review(context.Background(), SubtaskSpec{ID: "s1"}, "产出")
	if err != nil || v != VerdictReject || fb != "内容太空泛，缺少数据" || tokens != 9 {
		t.Errorf("REJECT case: v=%v fb=%q tokens=%d err=%v", v, fb, tokens, err)
	}

	c.chat = func(_ []api.Message) (string, int, error) { return "我觉得还行吧", 3, nil }
	if _, _, _, err := c.Review(context.Background(), SubtaskSpec{ID: "s1"}, "产出"); err == nil {
		t.Error("无法解析的结论应返回 error（走降级路径），got nil")
	}
}
```

## 二、关键设计点

1. **双重熔断为什么缺一不可（教程 Q5，面试高频）**：轮次熔断管"单点失控"——某个子任务怎么改都不过，critic 和 worker 在它身上无限拉锯；但它管不了"总量失控"——10 个子任务每个都在限内拉锯，加起来照样烧穿预算。预算熔断管总量，但它发现不了"某个子任务第一轮就陷入死循环反复重做同一错误"的局部病态（预算没到时一直烧）。一个管深度，一个管广度。生产上还会有第三重：wall-clock 超时（pool 的 jobTimeout 已部分覆盖）。

2. **critic 出错为什么放行而不是打回**（教程第 5 条"降级"）：critic 是质量增强层，不是正确性依赖。critic 挂了 → 如果当成 reject，worker 会被迫无意义重做甚至熔断 failed，把 critic 的故障放大成任务失败；如果当成静默 pass 不记 log，质量问题就隐形了。所以是"放行 + 记 log + 连续出错降级跳过"三级：单次出错放行留证据，连续出错说明 critic 服务性故障，整个任务不再浪费评审 token。**易错处**：LLMCritic 解析不出结论时返回 error 走降级路径，而不是返回 reject——"模型说了句废话"和"模型说不合格"是两回事，前者是 critic 不可用。

3. **feedback 怎么回到 worker**：`Worker.Execute` 的签名只有 spec，所以重做时构造 spec 副本、把评审意见拼进 Prompt（`【上次产出未通过评审，评审意见】：...`）。新 Agent 下一轮看到的仍是一份自包含 prompt——不改动 Worker 接口、不需要 agent 间消息通道。代价是 worker 看不到自己上一版的产出原文（只看到意见）；如果重做质量差，可以把上一版 output 也拼进 prompt，这是可调的产品决策。

4. **token 账的三个细节**：① worker 和 critic 的 token 都计入（只算 worker 会低估拉锯成本）；② `consumed` 从 `LoadTask` 的 TotalTokens 续算——Resume 后预算不被清零（否则崩溃恢复成了预算重置漏洞）；③ 全部轮次的 token 通过 `CompleteSubtask` 落进 checkpoint，成本账才完整。易错处：只记最后一轮的 token，或只在 reject 时记、pass 时漏记。

5. **并发安全**：多个子任务的 job 并发跑，`consumed`、`criticErrors`、`criticDisabled` 都是共享状态，必须 atomic。`go test -race` 是本练习的必跑项——普通测试跑不出数据竞争。降级测试用 `pool.New(1, ...)` 把并发度压成 1 换取顺序确定性：并发场景下"第 3 个子任务不再调 Review"无法稳定断言。测试里用可控的串行换取确定性断言，是并发代码测试的常用手法。

6. **检查点在每次 LLM 调用之前**：`checkBudget` 放在 worker.Execute 和 critic.Review 之前，而不是调用之后——之后检查意味着已经多烧了一轮。这与教程 Q8"预算分层、下层先触发"的精神一致：熔断要拦在花钱的动作前面。

## 三、进阶实现（加分项：critic 结构化评审输出）

> 回补记录：本节代码于 2026-08-14 以临时文件（`internal/orchestrator/critic_structured.go`
> + `critic_structured_test.go`）实际粘贴进项目验证，`go vet` 与 `go test -race` 全绿
> （连同基础与 DAG 进阶共 21 个测试），验证后已从项目删除，项目保持骨架版。

### 设计取舍

- **裸文本（基础版）vs JSON（进阶版）**：基础版 PASS/REJECT 两行约定，解析最简单、最省 token，缺点是没有量化信息。结构化版输出 `{"pass","score","issues"}`：score 支撑质量趋势观测（critic 拦截率、平均分变化——教程 Q10 的轨迹评估数据源），issues 清单让 worker 重做时逐条对着改。代价：JSON 解析多一层失败面（围栏、缺字段、score 越界），且模型输出 JSON 比两行文本更容易翻车。
- **解析失败仍走降级路径**：与基础版"无法解析 = critic 本次不可用"的约定完全一致——结构化不改变错误语义，只改变信息密度。
- **接入方式**：`ReviewDetailed` 与 `Review` 并存，编排器侧把 `o.critic.Review` 的调用换成一个适配器（把 Critique 翻成 Verdict + Feedback）即可，Critic 接口本身不用动。

### `internal/orchestrator/critic_structured.go`（进阶实现完整代码）

```go
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"mini-agent/api"
)

// Critique 是结构化评审结论。
type Critique struct {
	Pass   bool     `json:"pass"`
	Score  int      `json:"score"`  // 0-100，质量分
	Issues []string `json:"issues"` // 具体问题清单（pass 时可为空）
}

// Feedback 把 issues 拼成喂给 worker 的重做意见。
func (c Critique) Feedback() string {
	if len(c.Issues) == 0 {
		return "评审未通过但未给出具体意见，请对照子任务要求全面检查并改进。"
	}
	return strings.Join(c.Issues, "；")
}

const structuredCriticSystemPrompt = `你是质量评审（critic）。输入是一个子任务的要求和执行 agent 的产出。
只输出 JSON，不要任何其他文字，不要用 markdown 代码块包裹。格式：
{"pass":true,"score":85,"issues":[]}
- pass：产出是否合格；
- score：0-100 质量分（pass=true 一般 ≥ 60）；
- issues：不合格时的具体问题清单，每条是可独立执行的修改意见。只评不改。`

// ReviewDetailed 结构化评审：解析 JSON 结论；解析失败返回 error 走降级路径
// （与基础版"无法解析 = critic 本次不可用"的约定一致）。
func (c *LLMCritic) ReviewDetailed(ctx context.Context, spec SubtaskSpec, output string) (Critique, int, error) {
	if err := ctx.Err(); err != nil {
		return Critique{}, 0, err
	}
	user := fmt.Sprintf("【子任务要求】\n标题：%s\n指令：%s\n\n【执行产出】\n%s",
		spec.Title, spec.Prompt, output)
	raw, tokens, err := c.chat([]api.Message{
		{Role: "system", Content: structuredCriticSystemPrompt},
		{Role: "user", Content: user},
	})
	if err != nil {
		return Critique{}, tokens, fmt.Errorf("critic: LLM 调用失败: %w", err)
	}
	// 与 parsePlan 相同的围栏容错：截取第一个 '{' 到最后一个 '}'。
	s := strings.TrimSpace(raw)
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return Critique{}, tokens, fmt.Errorf("critic: 输出中找不到 JSON 对象: %q", truncate(s, 80))
	}
	var cq Critique
	if err := json.Unmarshal([]byte(s[start:end+1]), &cq); err != nil {
		return Critique{}, tokens, fmt.Errorf("critic: JSON 解析失败: %w", err)
	}
	if cq.Score < 0 || cq.Score > 100 {
		return Critique{}, tokens, fmt.Errorf("critic: score %d 越界 [0,100]", cq.Score)
	}
	return cq, tokens, nil
}
```

### `internal/orchestrator/critic_structured_test.go`（进阶测试完整代码）

```go
package orchestrator

import (
	"context"
	"testing"

	"mini-agent/api"
)

func TestReviewDetailed_ParsesStructuredJSON(t *testing.T) {
	c := NewLLMCritic(nil)

	c.chat = func(_ []api.Message) (string, int, error) {
		return "```json\n{\"pass\":false,\"score\":40,\"issues\":[\"缺少数据支撑\",\"结论与要求不符\"]}\n```", 12, nil
	}
	cq, tokens, err := c.ReviewDetailed(context.Background(),
		SubtaskSpec{ID: "s1", Title: "写稿", Prompt: "p"}, "产出")
	if err != nil {
		t.Fatalf("ReviewDetailed: %v", err)
	}
	if cq.Pass || cq.Score != 40 || len(cq.Issues) != 2 {
		t.Errorf("unexpected critique: %+v", cq)
	}
	if tokens != 12 {
		t.Errorf("tokens = %d, want 12", tokens)
	}
	if got := cq.Feedback(); got != "缺少数据支撑；结论与要求不符" {
		t.Errorf("Feedback = %q", got)
	}
}

func TestReviewDetailed_GarbageGoesToDegradation(t *testing.T) {
	c := NewLLMCritic(nil)
	c.chat = func(_ []api.Message) (string, int, error) { return "我觉得还行", 3, nil }
	if _, _, err := c.ReviewDetailed(context.Background(), SubtaskSpec{ID: "s1"}, "产出"); err == nil {
		t.Error("无法解析应返回 error（走降级路径），got nil")
	}

	c.chat = func(_ []api.Message) (string, int, error) {
		return `{"pass":true,"score":130,"issues":[]}`, 3, nil
	}
	if _, _, err := c.ReviewDetailed(context.Background(), SubtaskSpec{ID: "s1"}, "产出"); err == nil {
		t.Error("score 越界应报错，got nil")
	}
}
```

### 进阶实现的易错处

1. **score 不校验范围**：模型可能输出 130 或 -5，不拦下就进了观测数据污染趋势统计。
2. **以为 JSON 就不会翻车**：模型照样可能裹围栏、加前缀、缺字段——围栏容错与解析失败的降级路径一个都不能少。
3. **用 score 替代 pass 判断**：`pass=false, score=90` 这种自相矛盾的输出存在；以 pass 字段为准、score 只做观测，不要 `score >= 60 算 pass` 二次推断。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [ ] `LLMCritic.Review`：PASS / REJECT 解析正确；REJECT 无意见时有兜底反馈；无法解析返回 error（不是误判 reject）；返回真实评审 token 数
- [ ] 评审循环：reject 后 feedback 拼进 spec 副本的 Prompt 重做（不改共享 spec）；pass 后 CompleteSubtask 落盘（含全部轮次的 worker+critic token）
- [ ] 熔断一：达到 maxRounds 仍未通过 → 子任务 failed，失败原因注明评审熔断与最后意见
- [ ] 熔断二：worker+critic 累计 token（含 Resume 前已烧的，从 checkpoint 续算）超预算 → 检查点在每次 LLM 调用之前 → 任务 failed，哨兵错误可用 errors.Is 识别
- [ ] critic 降级：单次出错放行 + 记 log；连续出错 2 次后整个任务跳过评审；共享计数器用 atomic
- [ ] 测试覆盖：reject→pass 重做（worker 2 次且第 2 次 prompt 含 feedback）、轮次熔断、预算熔断、降级放行（串行 pool 断言第 3 个子任务不调 Review）
- [ ] `go vet ./internal/orchestrator/` 和 `go test ./internal/orchestrator/ -race -count=1` 全绿
- [ ] 能口头回答：双重熔断为什么缺一不可？critic 出错为什么放行而不是打回？预算为什么要从 checkpoint 续算？

加分项（做了才需要勾，参考"进阶实现"一节）：

- [ ] 结构化评审：pass/score/issues JSON 解析，score 范围校验，解析失败走降级路径，有测试
- [ ] 能口头回答：score 和 pass 判断为什么不能混用（`score>=60 算 pass` 的坑）？
