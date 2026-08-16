# 练习 2 参考答案：任务状态机 + SQLite checkpoint + 崩溃恢复

> 对应 TODO：`stage-03-multi-agent/internal/task/task.go` 的 `TODO(练习2)`。
> **完成练习并自评后再看本文档。**
> 本文档基础实现代码已于 2026-08-14 实际粘贴进项目验证：`go vet ./internal/task/` 与
> `go test ./internal/task/ -count=1`（5 个测试）全部通过。
> 进阶实现（事务版多行写入，见第三节）同日验证：临时粘贴为 `store_tx.go` + `store_tx_test.go`，
> 连同基础测试共 7 个测试全绿，验证后即删除，项目保持骨架版。
>
> 验证环境备注：go.mod 中的 `modernc.org/sqlite v1.56.0` 依赖由本次验证时
> `go get` + `go mod tidy` 补入（骨架的 `Open` 需要它），此依赖保留在项目中。

---

## 一、参考实现

### `internal/task/task.go`（只给出需要实现的部分；骨架的类型定义、Open/Close 不变）

import 与骨架一致（`context` / `database/sql` / `errors` / `fmt` / `time` / `_ "modernc.org/sqlite"`），无需改动。

```go
// taskTransitions 是任务级合法迁移表：key 是当前状态，value 是允许迁到的状态。
// done / failed 是终态：不在表里 = 任何迁出都非法。
var taskTransitions = map[Status][]Status{
	StatusPending:      {StatusPlanning, StatusFailed},
	StatusPlanning:     {StatusRunning, StatusFailed},
	StatusRunning:      {StatusWaitingHuman, StatusDone, StatusFailed},
	StatusWaitingHuman: {StatusRunning, StatusFailed},
}

// subtaskTransitions 是子任务级合法迁移表。与任务级的差别：
// 没有 planning；failed 可以迁回 pending（重试重排队）；
// running 也可以迁回 pending（崩溃恢复重置，见下）。
var subtaskTransitions = map[Status][]Status{
	StatusPending: {StatusRunning, StatusFailed},
	// running -> pending：崩溃恢复重置——重启后发现停在 running 的子任务
	// 一定是被打断的（进程已死，没有正在跑的执行体），先迁回 pending 再重跑。
	StatusRunning:      {StatusDone, StatusFailed, StatusWaitingHuman, StatusPending},
	StatusWaitingHuman: {StatusRunning, StatusFailed},
	StatusFailed:       {StatusPending}, // 重试重排队：死信前的最后一道门
	// done 是终态。
}

// canTransition 查迁移表：from → to 是否合法。
func canTransition(table map[Status][]Status, from, to Status) bool {
	for _, allowed := range table[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// CreateTask 创建任务，初始状态 pending。
func (s *Store) CreateTask(ctx context.Context, id, goal string) error {
	if id == "" || goal == "" {
		return fmt.Errorf("task: id and goal must not be empty")
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, goal, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, goal, StatusPending, now, now)
	if err != nil {
		return fmt.Errorf("task: create %s: %w", id, err)
	}
	return nil
}

// Transition 迁移任务状态（带状态机守卫），是任务级 checkpoint 的唯一入口。
//
// 先 SELECT 再 UPDATE 而不是一条 UPDATE 盲改：守卫依赖"当前状态"，
// 而当前状态必须读出来才能查表。这也是"每次迁移都是一次显式事件"的落点——
// 非法迁移在这里变成显式错误，而不是静默的脏状态。
func (s *Store) Transition(ctx context.Context, taskID string, to Status) error {
	var from Status
	err := s.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task: %s not found", taskID)
	}
	if err != nil {
		return fmt.Errorf("task: read %s: %w", taskID, err)
	}
	if !canTransition(taskTransitions, from, to) {
		return fmt.Errorf("task: illegal transition %s -> %s (task %s)", from, to, taskID)
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`, to, time.Now(), taskID)
	return err
}

// TransitionSubtask 迁移子任务状态（带状态机守卫）；迁入 running 时 attempts 自增。
func (s *Store) TransitionSubtask(ctx context.Context, taskID, subID string, to Status) error {
	var from Status
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM subtasks WHERE task_id = ? AND id = ?`, taskID, subID).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task: subtask %s/%s not found", taskID, subID)
	}
	if err != nil {
		return fmt.Errorf("task: read subtask %s/%s: %w", taskID, subID, err)
	}
	if !canTransition(subtaskTransitions, from, to) {
		return fmt.Errorf("task: illegal subtask transition %s -> %s (%s/%s)", from, to, taskID, subID)
	}
	// attempts 只在"开始一次新执行"时自增：迁入 running 才计数。
	// 重试上限判断（attempts >= max 进死信）读的就是这个字段。
	attemptsBump := 0
	if to == StatusRunning {
		attemptsBump = 1
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE subtasks SET status = ?, attempts = attempts + ?, updated_at = ?
		 WHERE task_id = ? AND id = ?`, to, attemptsBump, time.Now(), taskID, subID)
	return err
}

// SaveSubtasks 把 planner 分解出的子任务批量落盘（初始状态 pending）。
//
// 注意这是逐行 INSERT 的基础版：第 3 行失败时前 2 行已落盘，计划不完整。
// 多行写入的原子性见"进阶实现"一节的事务版——基础版先求跑通。
func (s *Store) SaveSubtasks(ctx context.Context, taskID string, subs []Subtask) error {
	for i := range subs {
		sub := subs[i]
		if sub.ID == "" {
			return fmt.Errorf("task: subtasks[%d] has empty id", i)
		}
		now := time.Now()
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO subtasks (id, task_id, title, prompt, status, idempotency_key, requires_approval, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sub.ID, taskID, sub.Title, sub.Prompt, StatusPending,
			sub.IdempotencyKey, sub.RequiresApproval, now, now)
		if err != nil {
			return fmt.Errorf("task: save subtask %s/%s: %w", taskID, sub.ID, err)
		}
	}
	return nil
}

// CompleteSubtask 是子任务成功时的 checkpoint：落盘 output 与 token 消耗。
func (s *Store) CompleteSubtask(ctx context.Context, taskID, subID, output string, tokens int) error {
	var from Status
	var key string
	err := s.db.QueryRowContext(ctx,
		`SELECT status, idempotency_key FROM subtasks WHERE task_id = ? AND id = ?`,
		taskID, subID).Scan(&from, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task: subtask %s/%s not found", taskID, subID)
	}
	if err != nil {
		return fmt.Errorf("task: read subtask %s/%s: %w", taskID, subID, err)
	}
	// 幂等落地：已经 done 说明同一份幂等键的工作早已完成并落盘
	// （崩溃恢复/重试重放到这里），直接返回 nil，且绝不能重复累加 token。
	// 注意：这行 if 是整个崩溃恢复设计在代码里的"最后一厘米"——删掉它，
	// 重放就会双倍烧 token、双倍产生副作用，而测试必须能抓住这一点。
	if from == StatusDone {
		return nil
	}
	if !canTransition(subtaskTransitions, from, StatusDone) {
		return fmt.Errorf("task: illegal subtask transition %s -> %s (%s/%s)", from, StatusDone, taskID, subID)
	}
	now := time.Now()
	_, err = s.db.ExecContext(ctx,
		`UPDATE subtasks SET status = ?, output = ?, tokens_used = tokens_used + ?, updated_at = ?
		 WHERE task_id = ? AND id = ?`, StatusDone, output, tokens, now, taskID, subID)
	if err != nil {
		return fmt.Errorf("task: complete subtask %s/%s: %w", taskID, subID, err)
	}
	// token 同时累加进任务总计——成本观测与预算熔断读的是 task.total_tokens。
	// 易错处：只记子任务不记总账，预算熔断就拿不到全任务口径的数字。
	_, err = s.db.ExecContext(ctx,
		`UPDATE tasks SET total_tokens = total_tokens + ?, updated_at = ? WHERE id = ?`,
		tokens, now, taskID)
	return err
}

// FailSubtask 记录子任务失败（errMsg 落在 output 字段），状态迁为 failed。
//
// 错误信息写进 output 的取舍：不另开 error 列——failed 的子任务 output 本来就没有
// "产出"语义，复用一列让 schema 更小；代价是读方要靠 status 区分 output 是成果还是错误。
func (s *Store) FailSubtask(ctx context.Context, taskID, subID, errMsg string) error {
	var from Status
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM subtasks WHERE task_id = ? AND id = ?`, taskID, subID).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task: subtask %s/%s not found", taskID, subID)
	}
	if err != nil {
		return fmt.Errorf("task: read subtask %s/%s: %w", taskID, subID, err)
	}
	if !canTransition(subtaskTransitions, from, StatusFailed) {
		return fmt.Errorf("task: illegal subtask transition %s -> %s (%s/%s)", from, StatusFailed, taskID, subID)
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE subtasks SET status = ?, output = ?, updated_at = ?
		 WHERE task_id = ? AND id = ?`, StatusFailed, errMsg, time.Now(), taskID, subID)
	return err
}

// LoadTask 读出一个任务及其全部子任务的 checkpoint（崩溃恢复时据此续跑）。
func (s *Store) LoadTask(ctx context.Context, taskID string) (*Task, []Subtask, error) {
	t := &Task{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, goal, status, total_tokens, created_at, updated_at FROM tasks WHERE id = ?`,
		taskID).Scan(&t.ID, &t.Goal, &t.Status, &t.TotalTokens, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("task: %s not found", taskID)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("task: load %s: %w", taskID, err)
	}
	// ORDER BY rowid：按插入顺序还原子任务列表——planner 输出的顺序就是执行顺序。
	// 不显式 ORDER BY 的话 SQLite 不保证返回顺序，恢复后子任务可能乱序执行。
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, title, prompt, output, status, idempotency_key,
		        tokens_used, attempts, requires_approval
		 FROM subtasks WHERE task_id = ? ORDER BY rowid`, taskID)
	if err != nil {
		return nil, nil, fmt.Errorf("task: load subtasks of %s: %w", taskID, err)
	}
	defer rows.Close()
	var subs []Subtask
	for rows.Next() {
		var sub Subtask
		if err := rows.Scan(&sub.ID, &sub.TaskID, &sub.Title, &sub.Prompt, &sub.Output,
			&sub.Status, &sub.IdempotencyKey, &sub.TokensUsed, &sub.Attempts,
			&sub.RequiresApproval); err != nil {
			return nil, nil, fmt.Errorf("task: scan subtask of %s: %w", taskID, err)
		}
		subs = append(subs, sub)
	}
	return t, subs, rows.Err()
}

// ListResumable 返回所有非终态任务的 ID——崩溃恢复的入口。
// 重启后调它拿到"上次没跑完的任务列表"，逐个 LoadTask 续跑。
func (s *Store) ListResumable(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM tasks WHERE status NOT IN (?, ?) ORDER BY created_at`,
		StatusDone, StatusFailed)
	if err != nil {
		return nil, fmt.Errorf("task: list resumable: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("task: scan resumable id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
```

### `internal/task/task_test.go`（新建，临时文件 DB，5 个测试）

```go
package task

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// newTestStore 用临时文件 DB（不是 :memory:）——崩溃恢复测试必须真实 Close 再 Open，
// 内存库 Close 后数据就没了，演练不了"重启"。
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

// planAndSave 把任务推到 running 并落盘子任务，是多个测试的公共前奏。
func planAndSave(t *testing.T, ctx context.Context, s *Store, taskID string, subs []Subtask) {
	t.Helper()
	if err := s.CreateTask(ctx, taskID, "测试目标"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	for _, to := range []Status{StatusPlanning, StatusRunning} {
		if err := s.Transition(ctx, taskID, to); err != nil {
			t.Fatalf("Transition -> %s: %v", to, err)
		}
	}
	if err := s.SaveSubtasks(ctx, taskID, subs); err != nil {
		t.Fatalf("SaveSubtasks: %v", err)
	}
}

// TestTransition_RejectsIllegal 验证状态机守卫：非法迁移必须报错，合法迁移放行。
func TestTransition_RejectsIllegal(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	if err := s.CreateTask(ctx, "t1", "goal"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// pending 不能直接跳到 running/done（必须先 planning）
	for _, to := range []Status{StatusRunning, StatusDone, StatusWaitingHuman} {
		if err := s.Transition(ctx, "t1", to); err == nil {
			t.Errorf("pending -> %s: want error, got nil", to)
		}
	}
	// 合法路径：pending -> planning -> running -> waiting_human -> running -> done
	for _, to := range []Status{StatusPlanning, StatusRunning, StatusWaitingHuman, StatusRunning, StatusDone} {
		if err := s.Transition(ctx, "t1", to); err != nil {
			t.Fatalf("legal transition -> %s: %v", to, err)
		}
	}
	// 终态不可迁出
	if err := s.Transition(ctx, "t1", StatusRunning); err == nil {
		t.Error("done -> running: want error, got nil（终态必须不可迁出）")
	}
	// 不存在的任务
	if err := s.Transition(ctx, "ghost", StatusPlanning); err == nil {
		t.Error("transition on missing task: want error, got nil")
	}
}

// TestCompleteSubtask_Idempotent 验证 checkpoint 幂等：
// 重复 CompleteSubtask 直接返回 nil，且 token 不重复累加。
func TestCompleteSubtask_Idempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	planAndSave(t, ctx, s, "t1", []Subtask{
		{ID: "s1", Title: "调研", Prompt: "...", IdempotencyKey: "t1:s1"},
	})
	if err := s.TransitionSubtask(ctx, "t1", "s1", StatusRunning); err != nil {
		t.Fatalf("TransitionSubtask: %v", err)
	}
	if err := s.CompleteSubtask(ctx, "t1", "s1", "调研结论", 100); err != nil {
		t.Fatalf("CompleteSubtask: %v", err)
	}
	// 崩溃恢复重放：同一个子任务再次 Complete，必须幂等
	if err := s.CompleteSubtask(ctx, "t1", "s1", "调研结论", 100); err != nil {
		t.Fatalf("CompleteSubtask replay: want nil, got %v", err)
	}

	task, subs, err := s.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}
	if subs[0].Status != StatusDone || subs[0].Output != "调研结论" {
		t.Errorf("subtask = %+v, want done with output", subs[0])
	}
	if subs[0].TokensUsed != 100 {
		t.Errorf("TokensUsed = %d, want 100（重放不得重复累加）", subs[0].TokensUsed)
	}
	if task.TotalTokens != 100 {
		t.Errorf("TotalTokens = %d, want 100（任务总计同样不得重复累加）", task.TotalTokens)
	}
	if subs[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", subs[0].Attempts)
	}
	// 幂等键要原样读回来——恢复逻辑靠它判断"这活干没干过"
	if subs[0].IdempotencyKey != "t1:s1" {
		t.Errorf("IdempotencyKey = %q, want t1:s1", subs[0].IdempotencyKey)
	}
}

// TestCrashRecovery 崩溃恢复演练（本练习的灵魂）：
// 跑到一半 Close（模拟进程死亡）→ 重新 Open → ListResumable 找回任务 →
// 已完成子任务的产出还在（跳过），未完成的不重复已完成的副作用。
func TestCrashRecovery(t *testing.T) {
	ctx := context.Background()
	s, path := newTestStore(t)

	planAndSave(t, ctx, s, "t1", []Subtask{
		{ID: "s1", Title: "调研", Prompt: "...", IdempotencyKey: "t1:s1"},
		{ID: "s2", Title: "写稿", Prompt: "...", IdempotencyKey: "t1:s2"},
		{ID: "s3", Title: "评审", Prompt: "...", IdempotencyKey: "t1:s3"},
	})
	// s1 跑完，s2 刚开始——此刻"进程崩溃"
	if err := s.TransitionSubtask(ctx, "t1", "s1", StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteSubtask(ctx, "t1", "s1", "调研结论", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionSubtask(ctx, "t1", "s2", StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // 优雅 Close 都算客气，真崩溃连这步都没有
		t.Fatal(err)
	}

	// 重启：重新 Open 同一个文件
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	ids, err := s2.ListResumable(ctx)
	if err != nil {
		t.Fatalf("ListResumable: %v", err)
	}
	if len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("ListResumable = %v, want [t1]", ids)
	}

	task, subs, err := s2.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}
	if task.Status != StatusRunning {
		t.Errorf("task status = %s, want running", task.Status)
	}
	if len(subs) != 3 {
		t.Fatalf("got %d subtasks, want 3", len(subs))
	}
	// 已完成的 s1：产出和 token 都在，恢复时跳过不重复执行
	if subs[0].Status != StatusDone || subs[0].Output != "调研结论" || subs[0].TokensUsed != 100 {
		t.Errorf("s1 = %+v, want done with checkpoint intact", subs[0])
	}
	// 被打断的 s2：状态停在 running，恢复方据此判断"要重跑"（幂等键保证重跑安全）
	if subs[1].Status != StatusRunning {
		t.Errorf("s2 status = %s, want running（被打断的现场）", subs[1].Status)
	}
	if subs[2].Status != StatusPending {
		t.Errorf("s3 status = %s, want pending", subs[2].Status)
	}
	// 续跑：先把被打断的 s2 迁回 pending（崩溃恢复重置），再重新执行，
	// 重跑成功（attempts 变 2），任务得以继续。
	if err := s2.TransitionSubtask(ctx, "t1", "s2", StatusPending); err != nil {
		t.Fatalf("reset interrupted s2: %v", err)
	}
	if err := s2.TransitionSubtask(ctx, "t1", "s2", StatusRunning); err != nil {
		t.Fatalf("resume s2: %v", err)
	}
	if err := s2.CompleteSubtask(ctx, "t1", "s2", "初稿", 200); err != nil {
		t.Fatal(err)
	}
	_, subs, err = s2.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if subs[1].Attempts != 2 {
		t.Errorf("s2 Attempts = %d, want 2（被打断 1 次 + 重跑 1 次）", subs[1].Attempts)
	}
}

// TestListResumable_ExcludesTerminal 验证终态任务不出现在恢复列表里。
func TestListResumable_ExcludesTerminal(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	// t1 走完全程到 done；t2 直接 failed；t3 停在 pending
	if err := s.CreateTask(ctx, "t1", "g"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []Status{StatusPlanning, StatusRunning, StatusDone} {
		if err := s.Transition(ctx, "t1", to); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateTask(ctx, "t2", "g"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, "t2", StatusFailed); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, "t3", "g"); err != nil {
		t.Fatal(err)
	}

	ids, err := s.ListResumable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "t3" {
		t.Errorf("ListResumable = %v, want [t3]", ids)
	}
}

// TestFailSubtask_AndRequeue 验证失败记录与"failed -> pending 重排队"路径。
func TestFailSubtask_AndRequeue(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	planAndSave(t, ctx, s, "t1", []Subtask{{ID: "s1", Title: "x", Prompt: "p"}})
	if err := s.TransitionSubtask(ctx, "t1", "s1", StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.FailSubtask(ctx, "t1", "s1", "LLM 超时"); err != nil {
		t.Fatalf("FailSubtask: %v", err)
	}
	// done 是终态：failed 不能直接 complete
	if err := s.CompleteSubtask(ctx, "t1", "s1", "x", 1); err == nil ||
		!strings.Contains(err.Error(), "illegal") {
		t.Errorf("complete a failed subtask: want illegal-transition error, got %v", err)
	}
	// 重试：failed -> pending -> running，attempts 继续累加
	if err := s.TransitionSubtask(ctx, "t1", "s1", StatusPending); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if err := s.TransitionSubtask(ctx, "t1", "s1", StatusRunning); err != nil {
		t.Fatal(err)
	}
	_, subs, err := s.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if subs[0].Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", subs[0].Attempts)
	}
	if subs[0].Output != "LLM 超时" {
		t.Errorf("Output = %q, want 保留失败现场", subs[0].Output)
	}
}
```

## 二、关键设计点

1. **为什么每次状态迁移都落盘，而不是任务结束才存**：进程死亡点不可预测。任务结束才存 = 崩溃即全丢，几小时的 token 和副作用全部白费；每步落盘把"最大丢失窗口"压到"当前正在执行的那一个子任务"。代价是每次迁移一次磁盘写——SQLite 单条 UPDATE 是微秒级，相对一次几十秒的 LLM 调用完全可忽略。**面试话术**：这是用廉价的确定性（磁盘写）换昂贵的确定性（不重复烧 token）。

2. **幂等键为什么崩溃恢复和重试共用**：两者的本质相同——"同一份工作可能被执行第二次"。崩溃恢复重放被打断的子任务，和重试重发一次 LLM 调用，判重依据是同一个：`IdempotencyKey` 落盘后，"这活干没干过"一查便知。本实现里它体现在两处：恢复方读 `LoadTask` 的结果按 key 判断跳过；`CompleteSubtask` 里 `if from == StatusDone { return nil }` 是 DB 侧的最后一道防线（双保险）。**易错处**：只在恢复逻辑里判跳过、不在写路径做幂等——并发 worker 或重试与恢复同时触发时照样双写。

3. **为什么用 SQLite 而不是 JSON 文件**：① **事务**：多行写入（planner 落盘 N 个子任务、CompleteSubtask 同时更新子任务和任务总账）要么全成要么全不成，JSON 文件写到一半崩溃就是损坏状态；② **并发**：多个 worker 并发写 checkpoint，SQLite 的锁/WAL 机制比手写文件锁可靠得多；③ **查询**：看板要"按状态过滤任务"、恢复要"找所有非终态任务"，SQL 一行搞定，JSON 得全量读进内存自己过滤。JSON 唯一胜出的场景是"给人看"——而 checkpoint 是给程序读的。

4. **`running -> pending` 这条边是验证时被测试逼出来的**：初版迁移表没有它，崩溃恢复测试里续跑被打断的 s2 时直接报 `illegal transition running -> running`。这暴露了一个真实设计问题：重启后停在 `running` 的子任务没有执行体（进程已死），必须先显式重置回 `pending` 才能重跑。**这是"先写崩溃恢复测试"的价值**——不写这个测试，这条边到真崩溃那天才会暴露。

5. **`ORDER BY rowid` 不是可有可无**：planner 输出的子任务顺序就是执行顺序（"写稿"必须在"调研"之后）。不显式 ORDER BY，SQLite 不保证返回顺序，恢复后可能乱序执行。易错写法是 `SELECT ... WHERE task_id = ?` 裸查。

6. **先 SELECT 守卫再 UPDATE，而非一条 SQL 盲改**：守卫依赖当前状态，当前状态必须读出来查表。严格说这存在 TOCTOU 竞态（读到 UPDATE 之间状态被别的 writer 改掉）——本实现靠 `SetMaxOpenConns(1)` + 单进程单 Store 规避；多进程写同一 DB 时才需要 `UPDATE ... WHERE status = ?` 乐观锁或事务版守卫（进阶方向，本练习不要求）。

7. **`db.SetMaxOpenConns(1)` 的原因**：SQLite 是单写者模型，`database/sql` 默认连接池在并发写下会撞 `SQLITE_BUSY`。钉成 1 个连接让驱动自己串行化。生产上量后换 Postgres 时这个限制自然消失，接口不变。

## 三、进阶实现（加分项：多行写入的事务原子性）

> 回补记录：本节代码于 2026-08-14 以临时文件（`internal/task/store_tx.go` + `store_tx_test.go`）
> 实际粘贴进项目验证，`go vet ./internal/task/` 与 `go test ./internal/task/ -count=1 -v`
> 全部通过（7 个测试：基础 5 个 + 进阶 2 个），验证后已从项目删除——
> **进阶实现只属于答案，不进项目代码树**，项目保持骨架版。

### 设计取舍（先说清楚再看代码）

- **基础版 `SaveSubtasks` 的缺口**：逐行 INSERT，第 3 行失败时前 2 行已落盘——崩溃恢复会读到"缺胳膊少腿"的计划，且重跑 planner 再 `SaveSubtasks` 会撞主键。`sql.Tx` 保证整批要么全成要么全不成。
- **`CompleteSubtask` 同样有两行写**：子任务 done + 任务 total_tokens 累加。两条 UPDATE 之间崩溃会留下"子任务 done 但总账少了"的脏账，预算熔断据此会放行已超支的任务。事务版把两行写绑成一个原子单元。
- **为什么不直接把基础版换成事务版**：学习项目里基础版先求"状态机 + 幂等 + 恢复"主线跑通，事务是第二层的正确性加固；且事务版代码更长，会冲淡主线的教学重点。生产项目应直接用事务版。
- **`defer tx.Rollback()` 惯例**：成功路径 `Commit` 后 `Rollback` 是 no-op，所以 defer 里无条件调它是安全的——这是 Go database/sql 事务的标准写法，比到处手写 rollback 分支可靠。

### `internal/task/store_tx.go`（进阶实现完整代码）

```go
package task

import (
	"context"
	"fmt"
	"time"
)

// SaveSubtasksTx 用事务批量落盘子任务：要么全部成功，要么全部回滚。
//
// 为什么需要事务版：planner 一次输出 N 个子任务，如果逐行 INSERT，
// 第 3 行失败时前 2 行已经落盘——崩溃恢复会读到一个"缺胳膊少腿"的计划，
// 且重跑 planner 再 SaveSubtasks 会撞主键。事务保证计划的完整性。
func (s *Store) SaveSubtasksTx(ctx context.Context, taskID string, subs []Subtask) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("task: begin tx: %w", err)
	}
	// 成功路径会 Commit 使 Rollback 变成 no-op，所以 defer 无条件 Rollback 是安全惯例。
	defer tx.Rollback()

	for i := range subs {
		sub := subs[i]
		if sub.ID == "" {
			return fmt.Errorf("task: subtasks[%d] has empty id", i)
		}
		now := time.Now()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO subtasks (id, task_id, title, prompt, status, idempotency_key, requires_approval, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sub.ID, taskID, sub.Title, sub.Prompt, StatusPending,
			sub.IdempotencyKey, sub.RequiresApproval, now, now)
		if err != nil {
			return fmt.Errorf("task: save subtask %s/%s: %w", taskID, sub.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("task: commit subtasks of %s: %w", taskID, err)
	}
	return nil
}

// CompleteSubtaskTx 事务版 checkpoint：子任务 done 与任务 total_tokens 累加
// 在同一事务里提交——基础版两条 UPDATE 之间崩溃会留下"子任务 done 但总账少了"
// 的脏账，事务版要么都成要么都不成。
func (s *Store) CompleteSubtaskTx(ctx context.Context, taskID, subID, output string, tokens int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("task: begin tx: %w", err)
	}
	defer tx.Rollback()

	var from Status
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM subtasks WHERE task_id = ? AND id = ?`,
		taskID, subID).Scan(&from)
	if err != nil {
		return fmt.Errorf("task: read subtask %s/%s: %w", taskID, subID, err)
	}
	if from == StatusDone {
		return nil // 幂等：同一份工作已完成，直接放行（此时事务会 Rollback，无副作用）
	}
	if !canTransition(subtaskTransitions, from, StatusDone) {
		return fmt.Errorf("task: illegal subtask transition %s -> %s (%s/%s)", from, StatusDone, taskID, subID)
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE subtasks SET status = ?, output = ?, tokens_used = tokens_used + ?, updated_at = ?
		 WHERE task_id = ? AND id = ?`, StatusDone, output, tokens, now, taskID, subID); err != nil {
		return fmt.Errorf("task: complete subtask %s/%s: %w", taskID, subID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET total_tokens = total_tokens + ?, updated_at = ? WHERE id = ?`,
		tokens, now, taskID); err != nil {
		return fmt.Errorf("task: add tokens to %s: %w", taskID, err)
	}
	return tx.Commit()
}
```

### `internal/task/store_tx_test.go`（进阶测试完整代码）

```go
package task

import (
	"context"
	"testing"
)

// TestSaveSubtasksTx_RollsBackOnError 验证事务原子性：
// 批次里有一行坏数据（重复主键）→ 整批回滚，一行都不落盘。
func TestSaveSubtasksTx_RollsBackOnError(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	if err := s.CreateTask(ctx, "t1", "g"); err != nil {
		t.Fatal(err)
	}
	// 第一批：s1 落盘成功
	if err := s.SaveSubtasksTx(ctx, "t1", []Subtask{{ID: "s1", Title: "a", Prompt: "p"}}); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	// 第二批：s2 是好的，但 s1 撞主键 → 整批必须回滚，s2 也不能留下
	err := s.SaveSubtasksTx(ctx, "t1", []Subtask{
		{ID: "s2", Title: "b", Prompt: "p"},
		{ID: "s1", Title: "dup", Prompt: "p"},
	})
	if err == nil {
		t.Fatal("want duplicate-key error, got nil")
	}
	_, subs, err := s.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].ID != "s1" {
		t.Errorf("subtasks = %v, want 只有第一批的 s1（第二批整体回滚）", subs)
	}
}

// TestCompleteSubtaskTx_Idempotent 验证事务版幂等语义与基础版一致。
func TestCompleteSubtaskTx_Idempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	planAndSave(t, ctx, s, "t1", []Subtask{{ID: "s1", Title: "a", Prompt: "p"}})
	if err := s.TransitionSubtask(ctx, "t1", "s1", StatusRunning); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.CompleteSubtaskTx(ctx, "t1", "s1", "out", 100); err != nil {
			t.Fatalf("CompleteSubtaskTx #%d: %v", i+1, err)
		}
	}
	task, subs, err := s.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if subs[0].TokensUsed != 100 || task.TotalTokens != 100 {
		t.Errorf("tokens = %d/%d, want 100/100（重放不得重复累加）",
			subs[0].TokensUsed, task.TotalTokens)
	}
}
```

### 进阶实现的易错处

1. **忘记 `defer tx.Rollback()`**：中途 return error 时不 rollback，连接上的事务会一直挂着，后续操作全部阻塞（SQLite 单写者）。
2. **在事务外查状态、事务内写**：`s.db.QueryRowContext` + `tx.ExecContext` 混用等于守卫脱离了事务保护，TOCTOU 竞态照样存在——要么都在 tx 里，要么接受基础版的单连接串行化。
3. **回滚测试只断言报错不断言现场**：撞主键报错只是表面，关键是 LoadTask 验证"s2 也没留下"——整批回滚才是事务的意义。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [ ] 合法迁移表用 `map[Status][]Status` 表达，任务/子任务两张表分开；非法迁移（如 pending→done、终态迁出）返回显式错误
- [ ] 子任务迁移表包含 `running -> pending`（崩溃恢复重置）与 `failed -> pending`（重试重排队）两条边，并能讲出为什么
- [ ] `CompleteSubtask` 幂等：已 done 直接返回 nil，token（子任务级与任务总账）都不重复累加；迁入 running 时 attempts 自增
- [ ] 每次状态迁移都刷新 `updated_at`；token 同时累加进 `tasks.total_tokens`
- [ ] `LoadTask` 子任务按 `ORDER BY rowid` 还原 planner 顺序；`ListResumable` 只返回非终态任务
- [ ] 测试覆盖：非法迁移被拒、幂等重放不重复累加、**崩溃恢复演练**（临时文件 DB → 写一半 → Close → 重开 → ListResumable 找回 → 已完成子任务产出还在且不重复执行）、终态任务不出现在恢复列表
- [ ] `go vet ./internal/task/` 和 `go test ./internal/task/ -count=1` 全绿
- [ ] 能口头回答：为什么每步落盘而不是任务结束才存？幂等键为什么恢复和重试共用？为什么用 SQLite 而不是 JSON 文件？

加分项（做了才需要勾，参考"进阶实现"一节）：

- [ ] `SaveSubtasks` / `CompleteSubtask` 有事务版，多行写入原子提交；撞主键时整批回滚有测试证明
- [ ] 能讲清 `defer tx.Rollback()` 为什么在 Commit 后是安全的，以及忘记它的后果（SQLite 单写者挂死）
