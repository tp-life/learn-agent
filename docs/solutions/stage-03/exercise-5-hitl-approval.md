# 练习 5 参考答案：Human-in-the-loop 审批点（中断/恢复 + CLI 演示）

> 对应 TODO：`stage-03-multi-agent/internal/hitl/hitl.go` 的 `TODO(练习5)`、
> `internal/orchestrator/orchestrator.go` 的 `TODO(练习5)`（审批闸）、
> `cmd/hitl-demo/main.go` 的 `TODO(练习5)`（演示主流程）。
> **完成练习并自评后再看本文档。**
>
> 本文档全部实现代码（基础 + 进阶）已于 2026-08-14 实际粘贴进项目验证：
> `go vet ./internal/hitl/ ./cmd/hitl-demo/ ./internal/orchestrator/` 通过；
> `go test ./internal/hitl/ ./internal/orchestrator/ -race -count=1` 全部通过
> （hitl 6 个测试含进阶 2 个 + orchestrator 练习3 回归 9 个，`-race` 无数据竞争）；
> `cmd/hitl-demo` 三条路径管道实测通过（详见文末附的实测记录）。
> 验证后即恢复骨架、删除测试与进阶实现文件——**实现只属于答案，不进项目代码树**。
>
> **验证前提（如实说明）**：本练习站在练习1/2/3 的肩膀上——编排器要跑真的
> 并发池与真的 SQLite checkpoint，而骨架里 `pool.Run` 是 panic 桩、
> `task.Store` 的方法是错误桩、`Orchestrator.Run/Resume` 是错误桩。
> 验证时临时落地了三份既有参考答案：练习1 的 `Pool.Run`、练习2 的 Store
> 实现（含下文第 0 节的迁移表补丁）、练习3 的编排器基础版，
> 验证后已全部恢复骨架。你自己跑本练习测试前，需要先完成练习1/2/3。

---

## 一、参考实现

### 0. 前置补丁：练习2 的子任务迁移表补两条边

练习2 参考答案（`docs/solutions/stage-03/exercise-2-task-checkpoint.md`）的
`subtaskTransitions` 里没有审批闸需要的两条边。**完成练习5 需先补（一行一个枚举值）**：

```go
var subtaskTransitions = map[Status][]Status{
	StatusPending: {StatusRunning, StatusFailed, StatusWaitingHuman}, // +waiting_human：审批闸
	StatusRunning:      {StatusDone, StatusFailed, StatusWaitingHuman, StatusPending},
	StatusWaitingHuman: {StatusRunning, StatusFailed, StatusPending}, // +pending：approve 重排队
	StatusFailed:       {StatusPending},
}
```

- `pending → waiting_human`：审批闸在执行前拦截（闸在迁入 running 之前，attempts 不自增）；
- `waiting_human → pending`：approve 后子任务回 pending 待 Resume 重跑。
- reject 用的 `waiting_human → failed`、任务级的 `running ↔ waiting_human`
  练习2 原表已有，不用动。

### 1. `internal/hitl/hitl.go`（Pending + Decide；骨架其余部分不变）

import 在骨架基础上加一个 `strings`：

```go
import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"stage-03-multi-agent/internal/task"

	_ "modernc.org/sqlite"
)
```

```go
// Pending 返回所有等待人工审批的子任务（跨全部未完成任务聚合）。
//
// 真相源是 task.Store 的 subtask.status——刻意不查 approvals 表：
// 审计表只记录"已发生的决定"，拿它推断"谁在等批"就是双写不一致的开端。
func (s *Service) Pending(ctx context.Context) ([]PendingApproval, error) {
	ids, err := s.store.ListResumable(ctx)
	if err != nil {
		return nil, fmt.Errorf("hitl: list resumable: %w", err)
	}
	var out []PendingApproval
	for _, taskID := range ids {
		_, subs, err := s.store.LoadTask(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("hitl: load task %s: %w", taskID, err)
		}
		for _, sub := range subs {
			if sub.Status == task.StatusWaitingHuman {
				out = append(out, PendingApproval{
					TaskID:       taskID,
					SubtaskID:    sub.ID,
					SubtaskTitle: sub.Title,
					Prompt:       sub.Prompt,
				})
			}
		}
	}
	return out, nil
}

// Decide 把人工决定落盘：迁移子任务状态（真相源）+ 写审计行。
//
// 顺序刻意为"先状态后审计"：状态机是真相源，审计可以补记；
// 反过来"先审计后迁移"一旦第二步失败，会留下"有审计但状态没动"的假象，
// 排查时比"状态动了但少一行审计"更误导。
func (s *Service) Decide(ctx context.Context, taskID, subtaskID string, approve bool, by string) error {
	if strings.TrimSpace(by) == "" {
		return errors.New("hitl: 审批人（by）不能为空——审计不留名等于没有审计")
	}

	// 先确认这个子任务真的在等批：对不在 waiting_human 的子任务做决定
	// 是调用方 bug（重复提交、过期页面），必须显式报错而不是静默写审计。
	_, subs, err := s.store.LoadTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("hitl: load task %s: %w", taskID, err)
	}
	var target *task.Subtask
	for i := range subs {
		if subs[i].ID == subtaskID {
			target = &subs[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("hitl: 子任务 %s/%s 不存在", taskID, subtaskID)
	}
	if target.Status != task.StatusWaitingHuman {
		return fmt.Errorf("hitl: 子任务 %s/%s 当前状态 %s，不在等待审批",
			taskID, subtaskID, target.Status)
	}

	if approve {
		// 回 pending 待 Resume 重跑。
		if err := s.store.TransitionSubtask(ctx, taskID, subtaskID, task.StatusPending); err != nil {
			return fmt.Errorf("hitl: 批准回迁 pending 失败: %w", err)
		}
		// 消费审批旗标："批准"是一次性放行令牌。requires_approval 不清零的话，
		// Resume 重跑时审批闸会再次拦截——approve 多少次都执行不了（死循环）。
		// 注意这行 UPDATE 直接写了 task 包的表：Store 的契约（练习2）没有暴露
		// 旗标修改方法，而"清旗标"是审批决定的一部分，只能由本包完成；
		// 状态迁移本身仍然走 Store 的状态机守卫，没有绕过真相源。
		if _, err := s.db.ExecContext(ctx,
			`UPDATE subtasks SET requires_approval = 0 WHERE task_id = ? AND id = ?`,
			taskID, subtaskID); err != nil {
			return fmt.Errorf("hitl: 清审批旗标失败: %w", err)
		}
	} else {
		// 驳回：failed + Output 记驳回原因。刻意【不清】RequiresApproval 旗标——
		// 编排器 dispatch 凭 "failed 且 RequiresApproval 仍为 true" 识别
		// "这是人工驳回"，不进重试重排队（见下文 orchestrator 增量改动 2）。
		if err := s.store.FailSubtask(ctx, taskID, subtaskID,
			fmt.Sprintf("已被人工驳回（审批人：%s）", by)); err != nil {
			return fmt.Errorf("hitl: 驳回落盘失败: %w", err)
		}
	}

	// 审计落盘：谁、什么时候、对哪个子任务、批了什么。
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO approvals (task_id, subtask_id, subtask_title, decided_by, approved, decided_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		taskID, subtaskID, target.Title, by, approve, time.Now())
	if err != nil {
		// 状态已迁移成功，这里失败只影响审计——如实报错让调用方知道要补记。
		return fmt.Errorf("hitl: 审计落盘失败（状态迁移已生效）: %w", err)
	}
	return nil
}
```

### 2. orchestrator 审批闸增量（加在练习3 参考实现的哪个位置，逐处标注）

以下 4 处改动都基于练习3 参考答案（`exercise-3-planner-worker.md` 第一节的
`orchestrator.go` 实现）。`ErrWaitingHuman` 哨兵错误骨架已提供，不用自己定义。

**改动 1：`runSubtask` 开头加审批闸**（StartSpan 之后、`TransitionSubtask→running` 之前）：

```go
func (o *Orchestrator) runSubtask(ctx context.Context, taskID, root string, spec SubtaskSpec) (string, error) {
	span := o.tracer.StartSpan(ctx, root, "worker: "+spec.ID, map[string]any{"title": spec.Title})

	// ↓↓↓ 练习5 新增：审批闸 ↓↓↓
	// 高风险子任务先 parked 到 waiting_human 并让出，不执行 worker。
	// 闸必须在迁入 running 之前：等审批不算一次执行，attempts 不该自增。
	// approve 后 hitl.Service.Decide 会把子任务迁回 pending 并清掉
	// RequiresApproval 旗标——那时本闸不再拦截，正常执行。
	if spec.RequiresApproval {
		if err := o.store.TransitionSubtask(ctx, taskID, spec.ID, task.StatusWaitingHuman); err != nil {
			o.tracer.EndSpan(ctx, span, 0, 0, err)
			return "", fmt.Errorf("orchestrator: 子任务 %s 置等待审批失败: %w", spec.ID, err)
		}
		o.tracer.EndSpan(ctx, span, 0, 0, ErrWaitingHuman)
		return "", ErrWaitingHuman
	}
	// ↑↑↑ 练习5 新增结束 ↑↑↑

	if err := o.store.TransitionSubtask(ctx, taskID, spec.ID, task.StatusRunning); err != nil {
		// ……练习3 原有代码不变……
```

（练习4 的评审循环版 `runSubtask` 多一个 `consumed *atomic.Int64` 参数，
审批闸的位置不变——同样加在 StartSpan 之后、迁入 running 之前，与评审循环正交。）

**改动 2：`dispatch` 的子任务筛选循环**——练习3 的两段 `if` 替换为 `switch`：

```go
	var jobs []pool.Job
	for _, sub := range subs {
		switch {
		case sub.Status == task.StatusDone:
			continue // 幂等键语义：这份活干过了，跳过
		case sub.Status == task.StatusWaitingHuman:
			continue // ↩ 练习5 新增：已 parked 等审批，Decide 之后才会回 pending
		case sub.Status == task.StatusFailed && sub.RequiresApproval:
			// ↩ 练习5 新增：人工驳回的子任务（驳回时 Decide 刻意不清旗标）。
			// 驳回是终局决定，不进 "failed→pending" 的重试重排队。
			// 执行失败的子任务旗标已是 false（approve 时清掉），照常重试，不受影响。
			continue
		case sub.Status == task.StatusRunning || sub.Status == task.StatusFailed:
			// running = 崩溃现场（先重置再重跑）；failed = 重试重排队。（练习3 原有）
			if err := o.store.TransitionSubtask(ctx, taskID, sub.ID, task.StatusPending); err != nil {
				return "", err
			}
		}
		spec := SubtaskSpec{ /* ……练习3 原有代码不变…… */ }
		// ……jobs append 不变……
	}
```

**改动 3：`dispatch` 在 `pool.Run` 之后、汇总之前加让出检查**——
练习3 原有代码里 `pool.Run` 之后已经有一次 `LoadTask`（汇总以 checkpoint 为准），
把 waiting 检查插在那次 `LoadTask` 和 `summarize` 之间：

```go
	_ = o.pool.Run(ctx, jobs)

	// 汇总以 checkpoint 为准，不用内存里的 results 拼（练习3 原有）。
	_, finalSubs, err := o.store.LoadTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	// ↓↓↓ 练习5 新增：有子任务在等审批 → 任务级落 waiting_human → 让出 ↓↓↓
	// 判断同样以 DB 为准（不看 pool 的内存 results）——"状态在库里"要贯彻到底。
	for _, s := range finalSubs {
		if s.Status == task.StatusWaitingHuman {
			// 任务级也落 waiting_human：看板/CLI 靠任务状态一眼认出"在等人"。
			if err := o.store.Transition(ctx, taskID, task.StatusWaitingHuman); err != nil {
				return "", err
			}
			return "", ErrWaitingHuman
		}
	}
	// ↑↑↑ 练习5 新增结束 ↑↑↑
	summary, failed := summarize(t.Goal, finalSubs)
	// ……练习3 原有代码不变……
```

**改动 4：`Resume` 的"状态补齐到 running"map 加一行**——
审批恢复时任务停在 waiting_human，要能补回 running：

```go
		next := map[task.Status]task.Status{
			task.StatusPending:      task.StatusPlanning,
			task.StatusPlanning:     task.StatusRunning,
			task.StatusWaitingHuman: task.StatusRunning, // ↩ 练习5 新增：审批恢复
		}[t.Status]
```

### 3. `cmd/hitl-demo/main.go`（TODO 部分：main 主流程 + approveLoop）

骨架已提供的 flag 接线、fakePlanner/echoWorker/askDecision 不变；
import 加 `errors`；`log.Fatal("TODO...")` 与 `_ = ...` 占位行替换为：

```go
	summary, err := runOrResume(ctx, store, orch)
	// ErrWaitingHuman 是"让出"不是失败——它是 for 的继续条件，不进 log.Fatal。
	for errors.Is(err, orchestrator.ErrWaitingHuman) {
		summary, err = approveLoop(ctx, svc, orch, reader)
	}
	if err != nil {
		log.Fatalf("任务失败: %v", err)
	}
	fmt.Println("\n" + summary)
}

// runOrResume 是"续跑 or 新跑"入口：checkpoint 里有未完成的 demo 任务就续跑
// （这是"杀掉进程重启"演示路径），否则开新任务。
func runOrResume(ctx context.Context, store *task.Store, orch *orchestrator.Orchestrator) (string, error) {
	ids, err := store.ListResumable(ctx)
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		if id == demoTaskID {
			fmt.Println("发现未完成的 demo 任务，从 checkpoint 续跑…")
			return orch.Resume(ctx, demoTaskID)
		}
	}
	fmt.Println("启动新任务：数据治理周报（含 1 个高风险子任务）…")
	return orch.Run(ctx, demoTaskID, "数据治理周报")
}

// approveLoop 处理一轮审批：列出待审批项 → 逐个询问 → 决定落盘 → Resume 续跑。
// 若 Resume 后仍有子任务等审批，返回的 err 会再次是 ErrWaitingHuman（main 继续循环）。
func approveLoop(ctx context.Context, svc *hitl.Service, orch *orchestrator.Orchestrator,
	reader *bufio.Reader) (string, error) {

	pending, err := svc.Pending(ctx)
	if err != nil {
		return "", err
	}
	if len(pending) == 0 {
		// 让出了却没人等批 = 状态不一致，值得显式报错而不是静默死循环。
		return "", fmt.Errorf("hitl-demo: ErrWaitingHuman 但无待审批项（状态不一致）")
	}
	fmt.Printf("\n%d 个高风险子任务等待人工审批：\n", len(pending))
	for _, p := range pending {
		approve := askDecision(reader, p)
		if err := svc.Decide(ctx, p.TaskID, p.SubtaskID, approve, "demo-user"); err != nil {
			return "", fmt.Errorf("决定落盘失败: %w", err)
		}
		if approve {
			fmt.Printf("    ✔ 已批准 %s\n", p.SubtaskID)
		} else {
			fmt.Printf("    ✘ 已驳回 %s\n", p.SubtaskID)
		}
	}
	fmt.Println("决定已落盘，从断点恢复执行…")
	return orch.Resume(ctx, demoTaskID)
}
```

### 4. `internal/hitl/hitl_test.go`（新建，审批全流程，无需网络/LLM）

四个测试：approve 续跑、reject 不重排队、Decide 后模拟崩溃重建再 Resume、
进阶的审批超时（见第三节）。假 Planner/Worker 是接口注入红利的又一次兑现——
整个 HITL 流程不烧一分 token 就能测。

```go
package hitl

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stage-03-multi-agent/internal/orchestrator"
	"stage-03-multi-agent/internal/pool"
	"stage-03-multi-agent/internal/task"
	"stage-03-multi-agent/internal/trace"
)

// newFixture 用临时文件 DB 建好 Store/Service，返回路径供"模拟崩溃重建"用。
func newFixture(t *testing.T) (*task.Store, *Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hitl.db")
	store, err := task.Open(path)
	if err != nil {
		t.Fatalf("task.Open: %v", err)
	}
	svc, err := NewService(store, path)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return store, svc, path
}

// fakePlanner 预置两个子任务：s1 普通，s2 高风险（RequiresApproval）。
type fakePlanner struct{}

func (fakePlanner) Plan(context.Context, string) (orchestrator.Plan, error) {
	return orchestrator.Plan{Subtasks: []orchestrator.SubtaskSpec{
		{ID: "s1", Title: "收集数据", Prompt: "p1"},
		{ID: "s2", Title: "删除过期数据", Prompt: "p2", RequiresApproval: true},
	}}, nil
}

// fakeWorker 回显产出并记录每个子任务的执行次数。
type fakeWorker struct {
	mu    sync.Mutex
	calls map[string]int
}

func newFakeWorker() *fakeWorker { return &fakeWorker{calls: map[string]int{}} }

func (w *fakeWorker) Execute(_ context.Context, spec orchestrator.SubtaskSpec) (string, int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls[spec.ID]++
	return "产出:" + spec.ID, 10, nil
}

func (w *fakeWorker) callCount(id string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[id]
}

func newOrch(store *task.Store, worker orchestrator.Worker) *orchestrator.Orchestrator {
	return orchestrator.New(store, pool.New(2, 10*time.Second), fakePlanner{}, worker,
		orchestrator.WithTracer(trace.NewNoop()))
}

// runToWaiting 跑 Run 并断言停在审批点：s2 置 waiting_human、任务置 waiting_human、
// s2 未被执行（闸在迁入 running 之前，attempts 也不该自增）。
func runToWaiting(t *testing.T, ctx context.Context, store *task.Store,
	orch *orchestrator.Orchestrator, worker *fakeWorker) {
	t.Helper()
	if _, err := orch.Run(ctx, "t1", "数据治理"); !errors.Is(err, orchestrator.ErrWaitingHuman) {
		t.Fatalf("Run err = %v, want ErrWaitingHuman", err)
	}
	tk, subs, err := store.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}
	if tk.Status != task.StatusWaitingHuman {
		t.Errorf("task status = %s, want waiting_human", tk.Status)
	}
	if subs[0].Status != task.StatusDone {
		t.Errorf("s1 status = %s, want done（普通子任务照常执行）", subs[0].Status)
	}
	if subs[1].Status != task.StatusWaitingHuman {
		t.Errorf("s2 status = %s, want waiting_human", subs[1].Status)
	}
	if subs[1].Attempts != 0 {
		t.Errorf("s2 Attempts = %d, want 0（等审批不算一次执行）", subs[1].Attempts)
	}
	if worker.callCount("s2") != 0 {
		t.Errorf("s2 被执行了 %d 次，want 0（审批闸必须拦在 worker 之前）", worker.callCount("s2"))
	}
}

// assertAudit 直接查审计表，断言审批记录的内容。
func assertAudit(t *testing.T, svc *Service, taskID, subID string, wantApproved bool, wantBy string) {
	t.Helper()
	var title, by string
	var approved bool
	var at time.Time
	err := svc.db.QueryRow(
		`SELECT subtask_title, decided_by, approved, decided_at
		 FROM approvals WHERE task_id = ? AND subtask_id = ?`, taskID, subID).
		Scan(&title, &by, &approved, &at)
	if err != nil {
		t.Fatalf("审计行缺失: %v", err)
	}
	if approved != wantApproved || by != wantBy {
		t.Errorf("审计 = (approved=%v, by=%q), want (%v, %q)", approved, by, wantApproved, wantBy)
	}
	if title == "" || at.IsZero() {
		t.Errorf("审计行 title/decided_at 不应为空: %q / %v", title, at)
	}
}

// TestApproveFlow_ResumeCompletes：approve → 子任务回 pending + 清旗标 →
// Resume 续跑至 done，s2 恰好执行一次。
func TestApproveFlow_ResumeCompletes(t *testing.T) {
	ctx := context.Background()
	store, svc, _ := newFixture(t)
	defer store.Close()
	defer svc.Close()
	worker := newFakeWorker()
	orch := newOrch(store, worker)

	runToWaiting(t, ctx, store, orch, worker)

	// Pending 必须能看到 s2（这是 CLI/看板的数据来源）
	pend, err := svc.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pend) != 1 || pend[0].SubtaskID != "s2" || pend[0].TaskID != "t1" {
		t.Fatalf("Pending = %+v, want 仅 t1/s2", pend)
	}

	if err := svc.Decide(ctx, "t1", "s2", true, "alice"); err != nil {
		t.Fatalf("Decide(approve): %v", err)
	}
	// 决定落盘：子任务回 pending，审批旗标被消费（一次性放行令牌）
	_, subs, err := store.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if subs[1].Status != task.StatusPending {
		t.Errorf("approve 后 s2 status = %s, want pending", subs[1].Status)
	}
	if subs[1].RequiresApproval {
		t.Error("approve 后 RequiresApproval 应被清掉，否则 Resume 会再次被闸拦截")
	}
	assertAudit(t, svc, "t1", "s2", true, "alice")
	if pend, _ := svc.Pending(ctx); len(pend) != 0 {
		t.Errorf("Decide 后 Pending = %+v, want 空", pend)
	}

	summary, err := orch.Resume(ctx, "t1")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	tk, subs, _ := store.LoadTask(ctx, "t1")
	if tk.Status != task.StatusDone {
		t.Errorf("task status = %s, want done", tk.Status)
	}
	if worker.callCount("s2") != 1 {
		t.Errorf("s2 被执行 %d 次, want 恰好 1 次", worker.callCount("s2"))
	}
	if subs[1].Attempts != 1 {
		t.Errorf("s2 Attempts = %d, want 1", subs[1].Attempts)
	}
	if !strings.Contains(summary, "产出:s2") {
		t.Errorf("汇总缺少 s2 的产出:\n%s", summary)
	}
}

// TestRejectFlow_NotRequeued：reject → 子任务 failed 记驳回信息 →
// Resume 后不被重排队（worker 对 s2 始终零调用），任务按部分失败语义 done。
func TestRejectFlow_NotRequeued(t *testing.T) {
	ctx := context.Background()
	store, svc, _ := newFixture(t)
	defer store.Close()
	defer svc.Close()
	worker := newFakeWorker()
	orch := newOrch(store, worker)

	runToWaiting(t, ctx, store, orch, worker)

	if err := svc.Decide(ctx, "t1", "s2", false, "bob"); err != nil {
		t.Fatalf("Decide(reject): %v", err)
	}
	_, subs, _ := store.LoadTask(ctx, "t1")
	if subs[1].Status != task.StatusFailed {
		t.Errorf("reject 后 s2 status = %s, want failed", subs[1].Status)
	}
	if !strings.Contains(subs[1].Output, "已被人工驳回") {
		t.Errorf("reject 后 s2 Output = %q, want 含驳回信息", subs[1].Output)
	}
	if !subs[1].RequiresApproval {
		t.Error("reject 应保留 RequiresApproval 旗标（dispatch 凭它识别驳回、不重排队）")
	}
	assertAudit(t, svc, "t1", "s2", false, "bob")

	summary, err := orch.Resume(ctx, "t1")
	if err != nil {
		t.Fatalf("Resume: %v（驳回一个子任务不应让任务失败——部分失败语义）", err)
	}
	tk, _, _ := store.LoadTask(ctx, "t1")
	if tk.Status != task.StatusDone {
		t.Errorf("task status = %s, want done", tk.Status)
	}
	if worker.callCount("s2") != 0 {
		t.Errorf("被驳回的 s2 被执行了 %d 次, want 0（驳回是终局决定）", worker.callCount("s2"))
	}
	if !strings.Contains(summary, "已被人工驳回") {
		t.Errorf("汇总应呈现驳回信息:\n%s", summary)
	}
}

// TestCrashAfterDecide_ApprovalSurvivesRestart（本练习的灵魂）：
// Decide(approve) 后 Close 全部连接（模拟进程崩溃），用同一路径重建
// Store/Service/Orchestrator 再 Resume——"已批未执行"必须能继续。
// 这就是教程 Q6"状态外置"的验收：审批决定全在 SQLite 里。
func TestCrashAfterDecide_ApprovalSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	store, svc, path := newFixture(t)
	worker1 := newFakeWorker()
	orch1 := newOrch(store, worker1)

	runToWaiting(t, ctx, store, orch1, worker1)
	if err := svc.Decide(ctx, "t1", "s2", true, "alice"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	// 进程"崩溃"：已批未执行
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 重启：全部对象重建（worker 计数器也换新——内存状态一概不带过来）
	store2, err := task.Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()
	svc2, err := NewService(store2, path)
	if err != nil {
		t.Fatalf("reopen service: %v", err)
	}
	defer svc2.Close()
	worker2 := newFakeWorker()
	orch2 := newOrch(store2, worker2)

	if pend, _ := svc2.Pending(ctx); len(pend) != 0 {
		t.Errorf("重启后 Pending = %+v, want 空（s2 已批准）", pend)
	}
	summary, err := orch2.Resume(ctx, "t1")
	if err != nil {
		t.Fatalf("重启后 Resume: %v", err)
	}
	tk, _, _ := store2.LoadTask(ctx, "t1")
	if tk.Status != task.StatusDone {
		t.Errorf("task status = %s, want done（已批未执行必须能继续）", tk.Status)
	}
	if worker2.callCount("s2") != 1 {
		t.Errorf("重启后 s2 执行 %d 次, want 1", worker2.callCount("s2"))
	}
	if !strings.Contains(summary, "产出:s2") {
		t.Errorf("汇总缺少 s2 产出:\n%s", summary)
	}
	// 审计跨重启还在
	assertAudit(t, svc2, "t1", "s2", true, "alice")
}

// TestDecide_RejectsInvalidCalls：对不在等批的子任务做决定、审批人为空，都要报错。
func TestDecide_RejectsInvalidCalls(t *testing.T) {
	ctx := context.Background()
	store, svc, _ := newFixture(t)
	defer store.Close()
	defer svc.Close()

	if err := svc.Decide(ctx, "ghost", "s1", true, "alice"); err == nil {
		t.Error("不存在的任务: want error, got nil")
	}
	w := newFakeWorker()
	runToWaiting(t, ctx, store, newOrch(store, w), w)
	if err := svc.Decide(ctx, "t1", "s2", true, "  "); err == nil {
		t.Error("审批人为空: want error, got nil（审计必须留名）")
	}
	if err := svc.Decide(ctx, "t1", "s1", true, "alice"); err == nil {
		t.Error("s1 不在 waiting_human: want error, got nil")
	}
}
```

## 二、关键设计点

1. **为什么审批不能靠内存 channel 等（教程 Q6，本练习的第一考点）**：
   假设 worker goroutine 里 `decision := <-ch` 阻塞等审批——进程一重启，
   channel 和等它的 goroutine 一起蒸发，"这个子任务在等批"这件事本身丢了；
   就算进程不死，阻塞的 goroutine 占着内存与 pool 并发额度；多副本部署时
   审批请求根本到不了持有 channel 的那个进程。本实现的"暂停"是**让出**：
   状态落盘（waiting_human）后 Run 直接返回，没有任何东西在内存里等；
   恢复由"人工决定落盘 + 再调 Resume"驱动。进程可以随便重启——
   `TestCrashAfterDecide_ApprovalSurvivesRestart` 演的就是这件事。

2. **真相源在 task.Store，hitl 只记审计**。"这个子任务能不能跑"只有一个答案：
   `subtask.status`。approvals 表回答的是另一个问题——"谁在什么时候批了什么"
   （合规、复盘、看板展示）。如果两张表都记"流程状态"，就要保证双写一致
   （没有跨表事务时必然有窗口期），而双写不一致的 bug 是静默的。推论：
   `Pending` 刻意不查 approvals 表；`Decide` 刻意"先迁状态、后写审计"——
   审计可以补记，"有审计但状态没动"的假象更难排查。

3. **ErrWaitingHuman 为什么是哨兵错误而不是返回值标志**。
   ① 签名零改动：`Run/Resume` 保持 `(string, error)`，pool、dispatch 各层
   签名都不用加 `waiting bool`；② 哨兵错误天然穿透多层调用冒泡，
   调用方 `errors.Is` 区分"让出"与"失败"（CLI 进审批循环，未来 HTTP 层
   映射成 202 + Location）；③ 与练习4 的 `ErrBudgetExceeded` 同构——
   编排器"非正常但可预期"的出口统一用哨兵错误表达，是一套一致的错误词汇表。
   **易错处**：在 dispatch 里把它当普通失败处理（吞掉或迁 failed）——
   让出语义就没了，任务会被误判成失败。

4. **approve 必须清 `requires_approval` 旗标，reject 必须保留**。
   审批是"一次性放行令牌"：approve 后子任务回 pending，如果旗标还在，
   Resume 重跑时审批闸再次拦截——approve 多少次都执行不了（死循环）。
   反过来 reject 保留旗标，dispatch 凭 `failed && RequiresApproval` 识别
   "这是人工驳回"而不进重试重排队；而批准过的子任务若执行失败
   （旗标已被清成 false），照常走练习3 的失败重试语义，不受影响。
   一个布尔旗标把"待批 / 已放行 / 已驳回"三种售后状态编码清楚了。

5. **审批闸在 `TransitionSubtask→running` 之前**。两个理由：
   ① attempts 语义纯净——`attempts` 只计"真实执行次数"，等审批不算执行
   （测试里断言 `Attempts == 0` 就是在守这条）；② 崩溃恢复语义不混淆——
   running 状态的子任务在恢复时被当作"被打断的现场"重置重跑，
   如果等审批的子任务占着 running，恢复逻辑就得区分两种 running。

6. **让出检查以 DB 为准，不看 pool 的内存 results**。dispatch 在 pool.Run
   之后重新 LoadTask 扫 waiting_human——与练习3"汇总以 checkpoint 为准"
   是同一个思想：Resume 场景下"谁在等批"可能是上一轮（甚至上一个进程）
   留下的，本轮 results 里根本没有。这也让 dispatch 不需要从 pool 的
   Result.Err 里解析哨兵错误——pool 的部分失败语义会把 ErrWaitingHuman
   收进 Result.Err，无害地丢弃即可，状态判断全走 DB。

## 三、进阶实现（加分项：审批超时自动驳回 + 升级通知钩子）

> 本节代码与基础版一起实际粘贴进项目验证（验证记录见文档头部）；
> **进阶实现只属于答案，不进项目代码树**，项目保持骨架版。

### 设计取舍（先说清楚再看代码）

- **为什么需要审批超时**：waiting_human 是"人在回路"——人休假了、通知没发到，
  任务就永远挂着：不 done 也不 failed，看板上一片黄，还占着 ListResumable。
  超时兜底把它变成显式的失败终态（超时=不批），并把记录返回给调用方去发
  升级通知（邮件/IM webhook——发送本身不是本包职责，返回值就是通知钩子）。
- **复用状态机守卫**：超时驳回走 `store.FailSubtask`（waiting_human→failed
  是合法边），不直接 SQL 改状态；审计行 `decided_by` 记 `system:timeout`，
  与人工决定可区分。
- **updated_at 当等待起点**：子任务表没有"进入 waiting_human 时刻"单列，
  每次状态迁移都刷新的 `updated_at` 就是这个时刻。读它只能直接 SQL
  （`task.Subtask` 结构不暴露该列）——读不越界，写仍走 Store。

### `internal/hitl/expire.go`（进阶实现完整代码）

```go
package hitl

import (
	"context"
	"fmt"
	"time"

	"stage-03-multi-agent/internal/task"
)

// ExpireStale 把等待审批超过 maxAge 的子任务自动驳回（超时=不批），
// 审计行 decided_by 记 "system:timeout"。返回被处理的审批记录，
// 供调用方发升级通知（邮件/IM webhook——发送本身不在本包职责内）。
//
// 用法：由调用方用 cron/定时器周期触发（如每 5 分钟一次），
// 本包不内置后台 goroutine——库不管调度，调度是应用的职责。
func (s *Service) ExpireStale(ctx context.Context, maxAge time.Duration) ([]Approval, error) {
	cutoff := time.Now().Add(-maxAge)
	// updated_at 即"进入 waiting_human 的时刻"（每次状态迁移都刷新它）。
	// 直接 SQL 读 task 包的表：Subtask 结构不暴露 updated_at 列；
	// 读不越界——状态修改仍走 Store 的状态机守卫（FailSubtask）。
	rows, err := s.db.QueryContext(ctx,
		`SELECT task_id, id, title FROM subtasks WHERE status = ? AND updated_at < ?`,
		string(task.StatusWaitingHuman), cutoff)
	if err != nil {
		return nil, fmt.Errorf("hitl: query stale approvals: %w", err)
	}
	type staleRow struct{ taskID, subID, title string }
	var stale []staleRow
	for rows.Next() {
		var r staleRow
		if err := rows.Scan(&r.taskID, &r.subID, &r.title); err != nil {
			rows.Close()
			return nil, fmt.Errorf("hitl: scan stale row: %w", err)
		}
		stale = append(stale, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var expired []Approval
	for _, r := range stale {
		// waiting_human → failed 是合法边；与人工驳回一样保留 RequiresApproval
		// 旗标，dispatch 同样不会把它重排队。
		if err := s.store.FailSubtask(ctx, r.taskID, r.subID,
			fmt.Sprintf("审批超过 %s 无人处理，超时自动驳回", maxAge)); err != nil {
			return expired, fmt.Errorf("hitl: 超时驳回 %s/%s 失败: %w", r.taskID, r.subID, err)
		}
		a := Approval{
			TaskID: r.taskID, SubtaskID: r.subID, SubtaskTitle: r.title,
			DecidedBy: "system:timeout", Approved: false, DecidedAt: time.Now(),
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO approvals (task_id, subtask_id, subtask_title, decided_by, approved, decided_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			a.TaskID, a.SubtaskID, a.SubtaskTitle, a.DecidedBy, a.Approved, a.DecidedAt); err != nil {
			return expired, fmt.Errorf("hitl: 超时审计落盘失败: %w", err)
		}
		expired = append(expired, a)
	}
	return expired, nil
}
```

### `internal/hitl/expire_test.go`（进阶测试完整代码）

```go
package hitl

import (
	"context"
	"strings"
	"testing"
	"time"

	"stage-03-multi-agent/internal/task"
)

// TestExpireStale：超时的 waiting_human 自动驳回（+审计+不重排队），
// 未超时的不受影响。
func TestExpireStale(t *testing.T) {
	ctx := context.Background()
	store, svc, _ := newFixture(t)
	defer store.Close()
	defer svc.Close()
	worker := newFakeWorker()
	orch := newOrch(store, worker)

	runToWaiting(t, ctx, store, orch, worker)

	// 把 s2 的等待起点回拨到 1 小时前（updated_at = 进入 waiting_human 的时刻）
	if _, err := svc.db.Exec(
		`UPDATE subtasks SET updated_at = ? WHERE task_id = 't1' AND id = 's2'`,
		time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	expired, err := svc.ExpireStale(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if len(expired) != 1 || expired[0].SubtaskID != "s2" || expired[0].DecidedBy != "system:timeout" {
		t.Fatalf("expired = %+v, want 仅 s2 / system:timeout", expired)
	}
	_, subs, _ := store.LoadTask(ctx, "t1")
	if subs[1].Status != task.StatusFailed {
		t.Errorf("超时后 s2 status = %s, want failed", subs[1].Status)
	}
	if !strings.Contains(subs[1].Output, "超时自动驳回") {
		t.Errorf("s2 Output = %q, want 含超时原因", subs[1].Output)
	}
	if pend, _ := svc.Pending(ctx); len(pend) != 0 {
		t.Errorf("超时后 Pending = %+v, want 空", pend)
	}
	// 超时驳回 = 驳回语义：Resume 不重排队，任务部分失败语义 done
	if _, err := orch.Resume(ctx, "t1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if worker.callCount("s2") != 0 {
		t.Errorf("超时驳回的 s2 被执行 %d 次, want 0", worker.callCount("s2"))
	}
	tk, _, _ := store.LoadTask(ctx, "t1")
	if tk.Status != task.StatusDone {
		t.Errorf("task status = %s, want done", tk.Status)
	}
}

// TestExpireStale_LeavesFreshWaiting：未超时的等待项不被误伤。
func TestExpireStale_LeavesFreshWaiting(t *testing.T) {
	ctx := context.Background()
	store, svc, _ := newFixture(t)
	defer store.Close()
	defer svc.Close()

	runToWaiting(t, ctx, store, newOrch(store, newFakeWorker()), newFakeWorker())

	// 不回拨时间：s2 刚进入 waiting_human，远未超时
	expired, err := svc.ExpireStale(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if len(expired) != 0 {
		t.Errorf("expired = %+v, want 空（未超时不该被驳回）", expired)
	}
	pend, _ := svc.Pending(ctx)
	if len(pend) != 1 || pend[0].SubtaskID != "s2" {
		t.Errorf("Pending = %+v, want s2 仍在等待", pend)
	}
}
```

### 进阶实现的易错处

1. **updated_at 比较格式**：task.Store 用驱动的 time.Time 绑定写 TIMESTAMP
   列，ExpireStale 的 cutoff 也必须以 time.Time 绑定传入（同一驱动同一格式，
   字符串比较才成立）；手写时间字符串去比较是格式地雷。
2. **超时驳回别清 RequiresApproval**：清了就会被 dispatch 当"执行失败"重排队
   ——超时驳回的子任务会被重新执行，与"超时=不批"的语义完全相反。
3. **ExpireStale 不管调度**：在库里 `go func(){ for { ... } }()` 内置定时器，
   进程退出时 goroutine 跟着死、测试还难写。返回列表让应用层调度，
   库保持纯粹。

### 批量审批 API 设计（开放讨论，无需实现）

**本项为开放讨论，无唯一正确实现，不要求写代码。** 理由：批量审批的合理形态
强依赖上游产品决策（看板全选？API 数组入参？按任务批量还是跨任务批量？），
学习项目没有真实产品约束，给"标准答案"反而是误导。讨论要点（面试可讲）：
① 批量 Decide 的部分失败语义——N 项里第 3 项状态已变（被并发审批），
是全部回滚还是逐项落盘逐项报告（参考 pool 的部分失败语义，倾向后者）；
② 幂等——同一批请求重发，已决定的项靠"必须在 waiting_human"校验天然幂等跳过；
③ 审计——每一项一行审计，绝不合并成一行（审计粒度=决定粒度）；
④ 并发——两个审批人同时批同一项，先到的生效、后到的收到"不在等待审批"错误，
这就是把真相源收敛在状态机里的红利。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [ ] 练习2 的子任务迁移表补了 `pending→waiting_human`、`waiting_human→pending`
      两条边，并能讲出各自服务谁（审批闸拦截 / approve 重排队）
- [ ] 审批闸在迁入 running 之前拦截：waiting_human 不自增 attempts、
      不执行 worker；闸返回 `ErrWaitingHuman`，dispatch 以 DB 为准检测并让出，
      任务级也落 waiting_human
- [ ] `Decide` 有输入校验：子任务必须在 waiting_human、审批人非空；
      approve → 回 pending + 清 `requires_approval`；reject → failed +
      Output 记驳回信息（含审批人）；先迁状态后写审计
- [ ] 被驳回的子任务不被 Resume 重排队（`failed && RequiresApproval` 跳过），
      且 approve 后执行失败的子任务正常重试——两种 failed 能区分开
- [ ] 三个核心测试通过：approve 续跑至 done、reject 不重排队、
      **Decide(approve) 后模拟崩溃重建再 Resume**（"已批未执行"能继续）
- [ ] hitl-demo 三条手动路径跑通：管道喂 `a` 任务 done、喂 `r` 部分失败 done、
      审批提示时 Ctrl-C 再执行同一命令能从 waiting_human 现场续跑
- [ ] `go vet ./internal/hitl/ ./cmd/hitl-demo/ ./internal/orchestrator/` 与
      `go test ./internal/hitl/ ./internal/orchestrator/ -count=1` 全绿
- [ ] 能口头回答：为什么审批不能靠内存 channel？为什么真相源在 task.Store
      而 approvals 表只记审计？ErrWaitingHuman 为什么是哨兵错误？

加分项（做了才需要勾，参考"进阶实现"一节）：

- [ ] `ExpireStale` 审批超时自动驳回：走状态机守卫迁 failed、审计记
      `system:timeout`、返回记录供升级通知；超时与未超时两个测试都过
- [ ] 能讲清批量审批 API 的四个设计要点（部分失败、幂等、审计粒度、并发冲突）

---

## 附：hitl-demo 实测记录（2026-08-14 验证时的真实输出摘要）

**路径 1：approve**（`printf 'a\n' | go run ./cmd/hitl-demo --db /tmp/hitl-a.db`）——
审批提示出现，输入 a 后 Resume 续跑，三个子任务全部 done，任务汇总含 s2 产出。

**路径 2：reject**（`printf 'r\n' | go run ./cmd/hitl-demo --db /tmp/hitl-r.db`）——
s2 驳回，任务按部分失败语义 done，汇总呈现
`[s2] 删除过期数据（未完成：已被人工驳回（审批人：demo-user））`，s1/s3 正常产出。

**路径 3：kill 重启续跑**——编译出二进制后在审批提示处 `pkill`（模拟崩溃）：

```
$ sqlite3 /tmp/hitl-k2.db "SELECT id,status FROM tasks; ..."   # 崩溃现场
demo-task|waiting_human
s1|done|0            ← requires_approval=0
s2|waiting_human|1   ← 审批点落盘，旗标还在
s3|done|0
（approvals 表 0 行——还没做决定）

$ printf 'a\n' | /tmp/hitl-demo --db /tmp/hitl-k2.db   # 重启同一条命令
发现未完成的 demo 任务，从 checkpoint 续跑…
1 个高风险子任务等待人工审批：…批准…决定已落盘，从断点恢复执行…
（任务 done；s2 执行一次；approvals 表留下 s2|demo-user|1）
```

进程重启后等待现场与（批后的）决定全部从 SQLite 恢复——"状态外置"的现场证据。
