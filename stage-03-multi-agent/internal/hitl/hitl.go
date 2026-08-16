// Package hitl 实现 Human-in-the-loop（HITL）审批：
// 审批审计日志 + waiting_human 状态迁移的封装。
//
// 在整个 agent 链路中的位置：编排器（internal/orchestrator）执行到
// RequiresApproval 的高风险子任务时，把子任务置为 waiting_human 并让出
// （返回 ErrWaitingHuman）；人工通过 CLI（cmd/hitl-demo）或未来的 HTTP API
// 提交决定，本包的 Service.Decide 把决定落盘并迁移子任务状态；
// 之后调用方再调 orchestrator.Resume 从断点续跑。
//
// 设计核心（教程 3.1 Q6，面试高频）：**状态外置才能"暂停-恢复"**。
// 审批的"流程状态"（这个子任务是否在等批）以 subtask.status（task.Store）
// 为唯一真相源；本包的 approvals 表只是审计日志（谁、什么时候、批了什么），
// 不参与流程判断——避免"两个地方各记一份状态"的双写不一致。
// 因为真相全在 SQLite 里，进程重启后"已批未执行"的任务照样能续跑；
// 如果审批靠内存 channel 等，进程一死审批点就没了。
//
// 练习：本包的类型定义、NewService（建审计表）与 Close 无需学习者完成；
// Pending 与 Decide 为 TODO(练习5)，见下文标注。
package hitl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"stage-03-multi-agent/internal/task"

	// 与 internal/task 同一个纯 Go SQLite 驱动；本包自建一条连接（见 NewService）。
	_ "modernc.org/sqlite"
)

// Approval 是一条审批记录（审计日志）。
// 它回答的是"谁在什么时候对哪个子任务批了什么"——合规与复盘用；
// 不回答"这个子任务现在能不能跑"（那是 subtask.status 的职责）。
type Approval struct {
	TaskID       string
	SubtaskID    string
	SubtaskTitle string
	DecidedBy    string    // 审批人标识（CLI 用户名 / HTTP 登录态）；审计必须留名
	Approved     bool      // true=批准执行，false=驳回
	DecidedAt    time.Time // 决定时间（审计与审批超时判断都靠它）
}

// PendingApproval 是一个等待人工审批的子任务（给 CLI/看板展示用）。
type PendingApproval struct {
	TaskID       string
	SubtaskID    string
	SubtaskTitle string
	Prompt       string // 审批人要看"它到底要干什么"才能做决定
}

// Service 是 HITL 审批的入口。零值不可用，必须用 NewService 构造。
type Service struct {
	store *task.Store // 流程状态的唯一真相源：所有状态迁移都走 Store 的状态机守卫
	// db 是本包自建的第二条连接（与 store 指向同一个 SQLite 文件）：
	// approvals 审计表的读写、以及 Decide(approve) 时清 requires_approval 旗标
	// 都走这条连接。为什么不开在 task.Store 里：Store 的表结构与方法是练习2 的契约，
	// 审计是练习5 的新关注点，不该回改练习2 的代码——关注点分离。
	db *sql.DB
}

// NewService 构造审批服务。dbPath 必须与 store 打开的是同一个 SQLite 文件
// （审计日志与状态机同库，恢复时一起被找到）。
//
// 为什么自建一条连接而不是复用 store 的：task.Store 不暴露内部 *sql.DB，
// 而 approvals 表是 hitl 自己的关注点。两条连接并发写同一 SQLite 文件可能撞
// SQLITE_BUSY（单写者模型），所以 DSN 里带 _pragma=busy_timeout——
// 撞锁时等待最多 5 秒而不是立刻报错（学习项目够用；生产上量后换 Postgres 自然消解）。
func NewService(store *task.Store, dbPath string) (*Service, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("hitl: open %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1) // 与 task.Open 同一理由：SQLite 单写者，钉成串行

	// 审计表迁移：IF NOT EXISTS 保证重复构造幂等（与 task.Open 同一套路）。
	const schema = `
CREATE TABLE IF NOT EXISTS approvals (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id       TEXT NOT NULL,
    subtask_id    TEXT NOT NULL,
    subtask_title TEXT NOT NULL,
    decided_by    TEXT NOT NULL,
    approved      INTEGER NOT NULL,
    decided_at    TIMESTAMP NOT NULL
);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("hitl: migrate: %w", err)
	}
	return &Service{store: store, db: db}, nil
}

// Close 关闭本包自建的连接（store 的连接由 store 自己关）。
func (s *Service) Close() error {
	return s.db.Close()
}

// ---------------------------------------------------------------------------
// 以下是 TODO(练习5) 留给学习者的实现。
// ---------------------------------------------------------------------------

// TODO(练习5): HITL 审批点 —— 待审批聚合 + 决定落盘与状态迁移
//
// 任务：实现本文件中的两个方法：
//
//	func (s *Service) Pending(ctx context.Context) ([]PendingApproval, error)
//	func (s *Service) Decide(ctx context.Context, taskID, subtaskID string, approve bool, by string) error
//
// 同时在 orchestrator 侧加审批闸（见 internal/orchestrator/orchestrator.go 的
// TODO(练习5) 块——两边一起构成本练习）。
//
// Pending 提示：
//   - 数据来源是 task.Store（真相源）：ListResumable 拿全部未完成任务 →
//     逐个 LoadTask → 收集 status == waiting_human 的子任务；
//   - 不要查 approvals 表来判断"谁在等批"——审计表只记录已发生的决定，
//     拿它当流程状态就是双写不一致的开端。
//
// Decide 提示：
//   - 先 LoadTask 找到子任务，校验它真的在 waiting_human——对不在等批的
//     子任务做决定是调用方 bug，必须报错而不是静默写审计；
//   - by（审批人）为空要报错：审计不留名等于没有审计；
//   - approve：TransitionSubtask 迁回 pending（待 Resume 重跑）+ 用自己的
//     db 连接 UPDATE subtasks SET requires_approval = 0 —— 审批是"一次性
//     放行令牌"，不清旗标的话 Resume 重跑时审批闸会再次拦截，死循环；
//   - reject：store.FailSubtask，错误信息写 "已被人工驳回（审批人：<by>）"——
//     驳回的子任务保持 RequiresApproval=true，编排器据此不重排队（见
//     orchestrator TODO(练习5) 块的分发改动说明）；
//   - 顺序：先迁移状态（真相源），成功后再 INSERT approvals 审计行。
//     反过来"先审计后迁移"会留下"有审计但状态没动"的假象，更难排查；
//   - 前置依赖：练习2 的子任务迁移表需要 pending→waiting_human 与
//     waiting_human→pending 两条边，练习2 参考答案的表里没有，
//     做本练习前先补上（一行加一个枚举值，详见参考答案第一节）。
//
// 验收：go test ./internal/hitl/ 通过，必须覆盖三条路径：
//  1. approve：Run 撞 ErrWaitingHuman → Pending 能看到 → Decide(approve)
//     → Resume 续跑至 done，子任务真实执行了一次；
//  2. reject：Decide(reject) → 子任务 failed、Output 记驳回信息、
//     Resume 后不被重排队（worker 对该子任务零调用）；
//  3. 模拟崩溃：Decide(approve) 后 Close 全部连接、用同一路径重建
//     Store/Service/Orchestrator 再 Resume —— "已批未执行"必须能继续
//     （这就是"状态外置"的验收，教程 Q6）。
//
// 参考答案：docs/solutions/stage-03/exercise-5-hitl-approval.md（完成后再看）

// Pending 返回所有等待人工审批的子任务（跨全部未完成任务聚合）。
func (s *Service) Pending(ctx context.Context) ([]PendingApproval, error) {
	// TODO(练习5): 实现此方法（要求见上方 TODO 块）
	return nil, errors.New("TODO(练习5): Pending 未实现")
}

// Decide 把人工决定落盘：写审计表 + 迁移子任务状态。
// approve=true：子任务回 pending 待 Resume 重跑（并消费掉审批旗标）；
// approve=false：子任务置 failed，Output 记录驳回信息。
func (s *Service) Decide(ctx context.Context, taskID, subtaskID string, approve bool, by string) error {
	// TODO(练习5): 实现此方法（要求见上方 TODO 块）
	return errors.New("TODO(练习5): Decide 未实现")
}
