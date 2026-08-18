// Package task 实现多 agent 系统的任务状态机与 SQLite checkpoint 持久化。
//
// 这个包解决什么问题：多 agent 编排里的一个"任务"是跑几分钟到几小时的长流程
// （planner 分解 → worker 逐个执行子任务 → 汇总），进程随时可能死。
// 如果状态只在内存里，进程一死一切重来——已烧掉的 token、已产生的副作用全部白费。
//
// 设计核心（面试高频考点）：**状态外置，进程无状态**。
// 阶段一学过"对话历史即状态"——agent 的记忆不在进程里，而在每次请求都全量重放的
// messages 里；本包是同一个思想在系统层的放大：任务/子任务的全部状态都在 SQLite 里，
// 进程本身不持有任何不可丢失的东西。**状态机每迁移一次就落盘一次（checkpoint）**，
// 崩溃恢复 = 重启后 ListResumable 找回未完成任务 → LoadTask 读出 checkpoint →
// 跳过已 done 的子任务续跑。
//
// 练习：本模块的类型定义、建表迁移与 Open/Close 无需用户完成；
// 状态机守卫与各 checkpoint 方法为 TODO(练习2)，见下文标注。
package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	// 纯 Go 实现的 SQLite 驱动（免 cgo），driver 名为 "sqlite"。
	// 这个库替代的是"mattn/go-sqlite3 + cgo 工具链"——换来的是交叉编译零配置，
	// 代价是极端写入吞吐略低，对学习/中小规模生产完全够用。

	_ "modernc.org/sqlite"
)

// Status 是任务与子任务共用的一组状态。
// 为什么不拆两套枚举：状态机语义完全一致（等待→执行→终态/等人），
// 共用一组常量能复用同一张迁移表思路，也让看板查询 SQL 不用写两遍。
type Status string

const (
	StatusPending      Status = "pending"       // 已创建，未开始
	StatusPlanning     Status = "planning"      // planner 正在分解子任务（仅任务级使用）
	StatusRunning      Status = "running"       // 正在执行
	StatusWaitingHuman Status = "waiting_human" // 暂停等人工审批（HITL，练习5 用）
	StatusDone         Status = "done"          // 终态：成功
	StatusFailed       Status = "failed"        // 终态：失败（重试耗尽或不可重试错误）
)

// Task 是一个长任务的 checkpoint 快照。
type Task struct {
	ID          string
	Goal        string
	Status      Status
	TotalTokens int // 全任务累计 token 消耗，用于成本观测与预算熔断
	CreatedAt   time.Time
	UpdatedAt   time.Time // 每次状态迁移刷新，"任务是不是卡死了"看它就够
}

// Subtask 是 planner 分解出来的一个执行单元，也是崩溃恢复的最小粒度。
type Subtask struct {
	ID               string
	TaskID           string
	Title            string
	Prompt           string
	Output           string
	Status           Status // 复用同一组状态：pending/running/done/failed/waiting_human
	IdempotencyKey   string // 幂等键：崩溃恢复与重试共用的判重依据
	TokensUsed       int
	Attempts         int  // 执行次数，每次进入 running 自增——重试上限判断靠它
	RequiresApproval bool // 练习5 HITL 审批点用：true 时执行前必须人工 approve
}

// Store 是 checkpoint 的唯一读写入口。
// 进程可以随便重启，Store 关掉再打开，状态分毫不丢——这就是"进程无状态"。
type Store struct {
	db *sql.DB
}

// Open 打开（不存在则创建）SQLite 数据库并执行建表迁移。
//
// 为什么用 SQLite 而不是 JSON 文件：① 事务——多行写入要么全成要么全不成；
// ② 并发——锁机制比手写文件锁可靠得多；③ 查询——看板要按状态过滤、
// 恢复要"找所有非终态任务"，SQL 一行搞定，JSON 文件得全量读进内存自己过滤。
//
// SetMaxOpenConns(1)：SQLite 是单写者模型，database/sql 默认的连接池在并发写下
// 会撞 SQLITE_BUSY，钉成 1 个连接让驱动自己串行化，简单且够用。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("task: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	// 建表迁移：IF NOT EXISTS 保证重复 Open（崩溃恢复场景）幂等。
	// 真实项目会用 golang-migrate 等版本化迁移工具，学习项目单表演进用
	// IF NOT EXISTS 足够，但要清楚它无法表达"改列"——那是迁移工具存在的意义。
	const schema = `
CREATE TABLE IF NOT EXISTS tasks (
    id           TEXT PRIMARY KEY,
    goal         TEXT NOT NULL,
    status       TEXT NOT NULL,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS subtasks (
    id                TEXT NOT NULL,
    task_id           TEXT NOT NULL REFERENCES tasks(id),
    title             TEXT NOT NULL,
    prompt            TEXT NOT NULL,
    status            TEXT NOT NULL,
    output            TEXT NOT NULL DEFAULT '',
    idempotency_key   TEXT NOT NULL DEFAULT '',
    tokens_used       INTEGER NOT NULL DEFAULT 0,
    attempts          INTEGER NOT NULL DEFAULT 0,
    requires_approval INTEGER NOT NULL DEFAULT 0,
    created_at        TIMESTAMP NOT NULL,
    updated_at        TIMESTAMP NOT NULL,
    PRIMARY KEY (task_id, id) -- 子任务 ID 只在任务内唯一，联合主键
);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("task: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭底层数据库连接。进程退出前调用它落盘收尾；
// 但即使不调用（崩溃），已提交的 checkpoint 也不会丢——这正是设计目标。
func (s *Store) Close() error {
	return s.db.Close()
}

var taskTransitions = map[Status][]Status{
	StatusPending:      {StatusPlanning, StatusFailed},
	StatusPlanning:     {StatusRunning, StatusFailed},
	StatusRunning:      {StatusWaitingHuman, StatusDone, StatusFailed},
	StatusWaitingHuman: {StatusRunning, StatusFailed},
}

var subtaskTransitions = map[Status][]Status{
	StatusPending:      {StatusRunning, StatusFailed},
	StatusRunning:      {StatusDone, StatusFailed, StatusWaitingHuman, StatusPending},
	StatusWaitingHuman: {StatusRunning, StatusFailed},
	StatusFailed:       {StatusPending},
}

func canTransition(table map[Status][]Status, from, to Status) bool {
	for _, allowed := range table[from] {
		if allowed == to {
			return true
		}
	}

	return false
}

// ---------------------------------------------------------------------------
// 以下是 TODO(练习2) 留给学习者的实现。
// ---------------------------------------------------------------------------

// TODO(练习2): 状态机守卫与 checkpoint 落盘
//
// 任务：实现本文件中的 8 个 Store 方法（CreateTask / Transition /
//
//	TransitionSubtask / SaveSubtasks / CompleteSubtask / FailSubtask /
//	LoadTask / ListResumable），让每个状态迁移都成为一次 checkpoint。
//
// 提示：
//   - 合法迁移表用 map[Status][]Status 表达（key 是当前状态，value 是允许
//     迁到的状态列表），任务与子任务各一张：planning 是任务专属；子任务可以
//     从 failed 迁回 pending（重试重排队）。先 SELECT 当前状态查表校验，
//     非法迁移返回错误，不要在 SQL 里盲 UPDATE。
//   - 想一条容易漏的边：崩溃恢复时，重启后发现停在 running 的子任务（执行体
//     已随进程死亡）要怎么迁回可重跑状态？先写崩溃恢复测试，它会替你发现。
//   - 幂等落地：CompleteSubtask 发现子任务已是 done（幂等键已随 SaveSubtasks
//     落盘，说明同一份工作已完成过）就直接返回 nil，且不得重复累加 token。
//   - TransitionSubtask 迁入 running 时 attempts 自增 1（attempts = attempts + 1）。
//   - 每次迁移都刷新 updated_at = time.Now()——"任务卡没卡死"全靠它判断。
//   - CompleteSubtask 顺手把 tokens 累加进 tasks.total_tokens，成本观测要用。
//   - ListResumable 返回所有非终态（NOT IN done/failed）的任务 ID，
//     它是崩溃恢复的入口：重启后先调它，再逐个 LoadTask 续跑。
//
// 验收：go test ./internal/task/ 全部通过，必须包含崩溃恢复演练测试
// （用临时文
// 件 DB：写入一半 → Close → 重新 Open → ListResumable 能找回任务、
// 已完成子任务的 output 还在、重放 CompleteSubtask 不重复累加 token）。
//
// 参考答案：docs/solutions/stage-03/exercise-2-task-checkpoint.md（完成后再看）

// CreateTask 创建任务，初始状态 pending。
func (s *Store) CreateTask(ctx context.Context, id, goal string) error {
	if id == "" || goal == "" {
		return fmt.Errorf("task: id and goal must not be empty")
	}

	now := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, goal, status, created_at, updated_at) VALUES (?,?,?,?,?)`, id, goal, StatusPending, now, now)
	if err != nil {
		return fmt.Errorf("task: create %s:%w", id, err)
	}

	return nil
}

// Transition 迁移任务状态（带状态机守卫），是任务级 checkpoint 的唯一入口。
func (s *Store) Transition(ctx context.Context, taskID string, to Status) error {
	var from Status
	err := s.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task: %s not found", taskID)
	}

	if err != nil {
		return fmt.Errorf("task: read %s:%w", taskID, err)
	}

	if !canTransition(taskTransitions, from, to) {
		return fmt.Errorf("task: illegal transition %s -> %s (task %s)", from, to, taskID)
	}

	_, err = s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`, to, time.Now(), taskID)
	return err
}

// TransitionSubtask 迁移子任务状态（带状态机守卫）；迁入 running 时 attempts 自增。
func (s *Store) TransitionSubtask(ctx context.Context, taskID, subID string, to Status) error {
	var from Status
	err := s.db.QueryRowContext(ctx, `SELECT status FROM subtasks WHERE task_id = ? AND id = ?`, taskID, subID).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task: subtask %s/%s not found", taskID, subID)
	}
	if err != nil {
		return fmt.Errorf("task: illegal subtask transition %s -> %s (%s/%s)", from, to, taskID, subID)
	}

	attemptsBump := 0
	if to == StatusRunning {
		attemptsBump = 1
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE subtasks SET status = ?, attempts = attempts + ?, updated_at = ? WHERE task_id = ? AND id = ?`, to, attemptsBump, time.Now(), taskID, subID)

	return err
}

// SaveSubtasks 把 planner 分解出的子任务批量落盘（初始状态 pending）。
func (s *Store) SaveSubtasks(ctx context.Context, taskID string, subs []Subtask) error {
	for i := range subs {
		sub := subs[i]
		if sub.ID == "" {
			return fmt.Errorf("task: subtasks[%d] has empty id", i)
		}
		now := time.Now()
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO subtasks (id, task_id, title, prompt, status, idempotency_key, requires_approval, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?)`, sub.ID, taskID, sub.Title, sub.Prompt, StatusPending, sub.IdempotencyKey, sub.RequiresApproval, now, now)
		if err != nil {
			return fmt.Errorf("task: save subtask %s/%s: %w", taskID, sub.ID, err)
		}
	}
	return nil
}

// CompleteSubtask 是子任务成功时的 checkpoint：落盘 output 与 token 消耗。
// 必须幂等：子任务已是 done（同一份幂等键的工作已完成过）时直接返回 nil，
// 不得重复累加 token——崩溃恢复重放到这一步时靠它保证副作用不翻倍。
func (s *Store) CompleteSubtask(ctx context.Context, taskID, subID, output string, tokens int) error {
	var from Status
	var key string
	err := s.db.QueryRowContext(ctx,
		`SELECT status, idempotency_key FROM subtasks WHERE task_id = ? AND id = ?`, taskID, subID).Scan(&from, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task: subtasks %s/%s not found", taskID, subID)
	}
	if err != nil {
		return fmt.Errorf("task: read subtask %s/%s: %w", taskID, subID, err)
	}

	if from == StatusDone {
		return nil
	}

	if !canTransition(subtaskTransitions, from, StatusDone) {
		return fmt.Errorf("task: illegal subtask transition %s -> %s (%s/%s)", from, StatusDone, taskID, subID)
	}

	now := time.Now()
	_, err = s.db.ExecContext(ctx, `UPDATE subtasks SET status =?, output = ?, tokens_used = tokens_used + ?, updated_at = ? WHERE task_id = ? AND id = ?`,
		StatusDone, output, tokens, now, taskID, subID)
	if err != nil {
		return fmt.Errorf("task: complete subtask %s/%s: %w", taskID, subID, err)
	}

	_, err = s.db.ExecContext(ctx, `UPDATE tasks SET total_tokens = total_tokens + ?, updated_at = ? WHERE id = ?`,
		tokens, now, taskID)
	return err
}

// FailSubtask 记录子任务失败（errMsg 落在 output 字段），状态迁为 failed。
func (s *Store) FailSubtask(ctx context.Context, taskID, subID, errMsg string) error {
	var from Status
	err := s.db.QueryRowContext(ctx, `SELECT status FROM subtasks WHERE task_id = ? AND id = ?`, taskID, subID).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task: subtask %s/%s not found", taskID, subID)
	}
	if err != nil {
		return fmt.Errorf("task: illegal subtask transition %s -> %s (%s/%s)", from, StatusFailed, taskID, subID)
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE subtasks SET status = ?, output = ?, updated_at =? 
		 WHERE task_id = ? AND id = ?`,
		StatusFailed, errMsg, time.Now(), taskID, subID)
	return err
}

// LoadTask 读出一个任务及其全部子任务的 checkpoint（崩溃恢复时据此续跑）。
func (s *Store) LoadTask(ctx context.Context, taskID string) (*Task, []Subtask, error) {
	t := &Task{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, goal, status, total_tokens,created_at, updated_at FROM tasks WHERE id = ?`, taskID).Scan(&t.ID, &t.Goal, &t.Status, &t.TotalTokens, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("task: %s not found", taskID)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("task: load %s: %w", taskID, err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, title, prompt, output, status, idempotency_key, tokens_used, attempts, requires_approval 
		 FROM subtasks WHERE task_id = ? ORDER BY rowid`, taskID)
	if err != nil {
		return nil, nil, fmt.Errorf("task: load subtasks of %s: %w", taskID, err)
	}
	defer rows.Close()
	var subs []Subtask
	for rows.Next() {
		var sub Subtask
		if err := rows.Scan(&sub.ID, &sub.TaskID, &sub.Title, &sub.Prompt, &sub.Output, &sub.Status, &sub.IdempotencyKey,
			&sub.TokensUsed, &sub.Attempts, &sub.RequiresApproval); err != nil {
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
		`SELECT id FROM tasks WHERE status NOT IN (?,?) ORDER BY created_at`, StatusDone, StatusFailed)
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
