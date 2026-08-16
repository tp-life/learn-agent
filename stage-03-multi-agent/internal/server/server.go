// Package server 把多 agent 编排引擎暴露成 HTTP + SSE API，
// 是阶段三"项目 3 集成"（练习8）的 Go 侧：前端看板（web/）通过它
// 提交任务、查询进度、审批高风险子任务、订阅实时事件。
//
// 在整个 agent 链路中的位置：
//
//	浏览器看板 ──HTTP/SSE──▶ 本包 ──▶ orchestrator（编排，练习3/4/5）
//	                                ├─▶ task.Store（checkpoint 真相源，练习2）
//	                                └─▶ hitl.Service（审批决定落盘，练习5）
//
// 设计核心（与阶段教程对齐）：
//   - 本包只做"协议翻译"：HTTP 请求 ↔ 编排引擎方法调用。
//     业务语义（状态机、审批闸、熔断）全部在内层包里，本包零业务逻辑——
//     将来换 CLI/gRPC 前端时内层一行不动；
//   - 长任务异步化：orchestrator.Run/Resume 是分钟级长任务，HTTP 请求
//     绝不能同步等它们——提交/审批接口把长任务丢进后台 goroutine 立即返回，
//     进度靠 GET 查询与 SSE 订阅获取（教程 3.3 时序图的工程化）；
//   - 读路径的真相源是 SQLite：任务状态、子任务产出全部从 task.Store 读，
//     进程无状态——看板随便刷新、服务随便重启（教程核心概念第 3 条）。
//
// 练习：本包的路由、DTO 类型、CORS、JSON 辅助函数与 New/Close 无需学习者完成；
// 5 个 handler 的实现为 TODO(练习8)，见下文标注。
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"stage-03-multi-agent/internal/hitl"
	"stage-03-multi-agent/internal/orchestrator"
	"stage-03-multi-agent/internal/task"

	// 本包对同一个 SQLite 文件开自己的读连接（见 New），因此自己也引驱动。
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// DTO：API 与内部模型之间的隔离层
// ---------------------------------------------------------------------------

// TaskView 是任务在 API/看板上的投影。
// 为什么不直接 JSON 序列化 task.Task：DTO 把"对外契约"和"内部模型"隔开——
// 内部模型以后加字段（如练习4 的预算字段）不会意外泄漏成对外契约。
type TaskView struct {
	ID          string      `json:"id"`
	Goal        string      `json:"goal"`
	Status      task.Status `json:"status"` // Status 是 string 类型，直接序列化成 "running" 等
	TotalTokens int         `json:"total_tokens"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// SubtaskView 是子任务的投影。看板审批按钮的显示条件就是
// Status == waiting_human，所以 RequiresApproval 也要带出去。
type SubtaskView struct {
	ID               string      `json:"id"`
	Title            string      `json:"title"`
	Prompt           string      `json:"prompt"` // 审批人要看"它到底要干什么"才能做决定
	Output           string      `json:"output,omitempty"`
	Status           task.Status `json:"status"`
	TokensUsed       int         `json:"tokens_used"`
	Attempts         int         `json:"attempts"`
	RequiresApproval bool        `json:"requires_approval"`
}

// TaskDetailView 是 GET /api/tasks/{id} 与 SSE 快照共用的载荷：
// 详情页首屏渲染与 SSE 增量刷新吃同一份结构，前端不用维护两套解析。
type TaskDetailView struct {
	TaskView
	Subtasks []SubtaskView `json:"subtasks"`
}

// createTaskRequest 是 POST /api/tasks 的请求体。
type createTaskRequest struct {
	Goal string `json:"goal"`
}

// decideRequest 是 POST /api/tasks/{id}/approve 的请求体。
// By（审批人）必填——审计不留名等于没有审计（练习5 的同一条纪律）。
type decideRequest struct {
	SubtaskID string `json:"subtask_id"`
	Approved  bool   `json:"approved"`
	By        string `json:"by"`
}

// toTaskView / toDetailView 是内部模型 → DTO 的转换（骨架提供，handler 直接用）。
func toTaskView(t *task.Task) TaskView {
	return TaskView{
		ID: t.ID, Goal: t.Goal, Status: t.Status,
		TotalTokens: t.TotalTokens, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func toDetailView(t *task.Task, subs []task.Subtask) TaskDetailView {
	views := make([]SubtaskView, len(subs))
	for i, s := range subs {
		views[i] = SubtaskView{
			ID: s.ID, Title: s.Title, Prompt: s.Prompt, Output: s.Output,
			Status: s.Status, TokensUsed: s.TokensUsed, Attempts: s.Attempts,
			RequiresApproval: s.RequiresApproval,
		}
	}
	return TaskDetailView{TaskView: toTaskView(t), Subtasks: views}
}

// ---------------------------------------------------------------------------
// Server：依赖装配与路由
// ---------------------------------------------------------------------------

// Server 是编排引擎的 HTTP 门面。零值不可用，必须用 New 构造。
type Server struct {
	store *task.Store                // checkpoint 真相源：详情、SSE 快照都从这里读
	svc   *hitl.Service              // 审批决定落盘（含审计）
	orch  *orchestrator.Orchestrator // 长任务执行入口（Run/Resume，后台 goroutine 里跑）
	db    *sql.DB                    // 本包自建的读连接：任务列表查询用（见 New）

	pollInterval      time.Duration // SSE 轮询 checkpoint 的间隔
	heartbeatInterval time.Duration // SSE 心跳间隔（防代理掐空闲连接）
}

// New 构造 HTTP 服务。dbPath 必须与 store/svc 打开的是同一个 SQLite 文件。
//
// 为什么本包还要自己开一条连接：任务列表（GET /api/tasks）需要"全部任务
// 含终态"，而 task.Store 的契约（练习2）只暴露 ListResumable（非终态）。
// 照 hitl.NewService 的先例，对同一个 SQLite 文件开自己的【读】连接做列表
// 查询——不回改练习2 的契约。纪律：这条连接只读，状态修改一律走
// Store/Service 的状态机守卫，绝不直接 SQL 改状态。
func New(store *task.Store, svc *hitl.Service, orch *orchestrator.Orchestrator, dbPath string) (*Server, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("server: open %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1) // 与 task.Open 同一理由：SQLite 单写者，钉成串行
	return &Server{
		store: store, svc: svc, orch: orch, db: db,
		pollInterval:      time.Second,
		heartbeatInterval: 15 * time.Second,
	}, nil
}

// Close 关闭本包自建的读连接（store/svc 的连接由它们自己关）。
func (s *Server) Close() error {
	return s.db.Close()
}

// Handler 返回装配好路由与 CORS 的 http.Handler。
//
// 用 Go 1.22+ ServeMux 的方法+通配符模式（"POST /api/tasks/{id}"），
// 不引第三方路由库——5 条路由用不上，标准库的模式路由已经能表达
// 方法分派与路径参数（r.PathValue("id")）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("POST /api/tasks/{id}/approve", s.handleApprove)
	mux.HandleFunc("GET /api/tasks/{id}/events", s.handleTaskEvents)
	return withCORS(mux)
}

// withCORS 放开跨域：看板 dev server 跑在 localhost:3000，API 在 :8080，
// 浏览器跨源 fetch/EventSource 都需要这组头。
// 生产注意：Access-Control-Allow-Origin: * 是最宽配置，真实部署应收紧为
// 看板的域名；本地学习项目图省事用 *。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions { // 预检请求直接应答，不进路由
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSON / writeErr 是统一出口：所有响应都是 JSON，错误也是
// {"error": "..."}——前端只用处理一种形状。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// ---------------------------------------------------------------------------
// 以下是 TODO(练习8) 留给学习者的实现。
// ---------------------------------------------------------------------------

// TODO(练习8): 提交任务 —— POST /api/tasks
//
// 任务：实现
//
//	func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request)
//
// 流程：解析 createTaskRequest → goal 去空白非空校验（400）→ 生成任务 ID
// → 【后台 goroutine】跑 orchestrator.Run → 立即返回 202 {"task_id": id}。
//
// 提示：
//   - 任务 ID 建议 "t-<毫秒时间戳>-<4字节随机hex>"（crypto/rand），
//     可读且碰撞可忽略；
//   - 后台 goroutine 的 ctx 必须用 context.Background()，不能用 r.Context()——
//     请求一返回 r.Context() 就被取消，长任务会刚启动就被掐死（本练习第一坑）；
//   - goroutine 里 Run 返回的错误：errors.Is(orchestrator.ErrWaitingHuman)
//     是正常让出（等审批），不记错误日志；其他错误记 log（任务状态已在
//     checkpoint 里是 failed，HTTP 层无需再做）；
//   - 已知小竞态：Run 在 goroutine 里才 CreateTask，202 返回后立刻 GET
//     详情可能短暂 404——前端轮询/SSE 天然容忍，如实注释即可，不必加同步。
//
// 验收：curl -X POST localhost:8080/api/tasks -d '{"goal":"..."}' 拿到
// task_id；随后 GET /api/tasks/{id} 能看到状态从 planning 向后流转。
//
// 参考答案：docs/solutions/stage-03/exercise-8-server-dashboard.md（完成后再看）
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, errors.New("TODO(练习8): handleCreateTask 未实现"))
}

// TODO(练习8): 任务列表 —— GET /api/tasks
//
// 任务：实现
//
//	func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request)
//
// 从 s.db（本包自建读连接）SELECT 全部任务，按 created_at 倒序，
// 返回 {"tasks": [...TaskView]}（空列表也要是 [] 不是 null——
// make([]TaskView, 0) 初始化，前端 map 不用判空）。
//
// 提示：
//   - 列与 toTaskView 一一对应：id, goal, status, total_tokens, created_at, updated_at；
//   - 只读这条连接，别动 Store 契约。
//
// 验收：curl localhost:8080/api/tasks 看到任务数组，新任务排在最前。
//
// 参考答案：docs/solutions/stage-03/exercise-8-server-dashboard.md（完成后再看）
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, errors.New("TODO(练习8): handleListTasks 未实现"))
}

// TODO(练习8): 任务详情 —— GET /api/tasks/{id}
//
// 任务：实现
//
//	func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request)
//
// 流程：r.PathValue("id") → store.LoadTask → toDetailView → 200。
// 任务不存在返回 404（LoadTask 的错误信息含 "not found"，用 strings.Contains
// 判断或直接统一 404——详情接口的调用方只关心"有没有这个任务"）。
//
// 验收：curl localhost:8080/api/tasks/<id> 看到任务 + 子任务数组
// （状态、tokens_used、attempts、output 齐全）。
//
// 参考答案：docs/solutions/stage-03/exercise-8-server-dashboard.md（完成后再看）
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, errors.New("TODO(练习8): handleGetTask 未实现"))
}

// TODO(练习8): 人工审批 —— POST /api/tasks/{id}/approve
//
// 任务：实现
//
//	func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request)
//
// 流程：解析 decideRequest → subtask_id / by 非空校验（400）→
// svc.Decide 落盘（子任务不在 waiting_human 时 Decide 会报错，透传为 409）
// → 判断是否需要触发续跑：LoadTask，任务在 waiting_human 且【没有其它
// 子任务还在 waiting_human】（一次可能有多个待批项，批完最后一个才续跑）
// → 【后台 goroutine】调 orchestrator.Resume → 立即返回 200 {"ok": true}。
//
// 提示：
//   - Resume 放后台 goroutine 的理由与 create 相同：HTTP 请求不等长任务。
//     Resume 返回 ErrWaitingHuman（还有别的审批点）属正常，不记错误日志；
//   - Resume 的 ctx 同样用 context.Background()；
//   - 不触发续跑的情况（还有子任务在等批）也返回 200——审批本身已成功，
//     看板靠 SSE 看到"还在等下一项"。
//
// 验收：demo 模式跑一个任务到 waiting_human 后，
// curl -X POST .../approve -d '{"subtask_id":"s2","approved":true,"by":"me"}'
// 返回 ok；随后 GET 详情能看到任务继续推进直至 done。
//
// 参考答案：docs/solutions/stage-03/exercise-8-server-dashboard.md（完成后再看）
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, errors.New("TODO(练习8): handleApprove 未实现"))
}

// TODO(练习8): 实时事件流 —— GET /api/tasks/{id}/events（SSE）
//
// 任务：实现
//
//	func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request)
//
// 用 SSE（Server-Sent Events）向看板推送任务/子任务状态变化。
//
// 流程：
//  1. 断言 w.(http.Flusher)（不支持则 500）；
//  2. 先 LoadTask 确认任务存在（不存在 404，别让看板对着空任务空转）；
//  3. 写 SSE 响应头：Content-Type: text/event-stream、Cache-Control: no-cache、
//     Connection: keep-alive，WriteHeader(200)；
//  4. 循环：每 pollInterval（默认 1s）LoadTask 一次，把 toDetailView 的 JSON
//     与上次推送的字节比较，【有变化才推】（poll-diff）；
//     每 heartbeatInterval（默认 15s）推一行 ": hb\n\n"（SSE 注释行）——
//     反向代理（nginx 等）会掐掉长时间无字节的连接，心跳是保活手段；
//  5. r.Context().Done()（客户端断开）时退出循环。
//
// SSE 帧格式（手写，不需要库）：
//
//	data: {"id":"...","status":"running",...}\n\n
//
// 即以 "data: " 开头、JSON 单行、两个换行结尾。写完必须 Flush，
// 否则数据堆在缓冲区里，"实时"就名存实亡。
//
// 为什么用 poll-diff 而不是给 orchestrator 加事件 hook（面试可讲的取舍）：
//   - poll-diff 实现零侵入：orchestrator/task 的契约（练习2/3/5）一行不改，
//     SQLite 本来就是真相源，1s 延迟对看板场景无感；
//   - 事件 hook 更实时（毫秒级）、更省查询，但要改编排器契约、处理hook
//     回调的并发与 panic 隔离，复杂度上一个台阶；
//   - 结论：看板是"给人看"的场景，秒级足够；hook 是性能出现瓶颈后的
//     优化方向（见参考答案进阶节的开放讨论）。
//
// 提示：
//   - 比较用 bytes.Equal(本次JSON, 上次JSON)，首次无条件推一帧（首屏快照）；
//   - ticker 记得 defer Stop；
//   - 任务到终态后【不要】主动关连接：浏览器 EventSource 会自动重连，
//     关了等于让它每秒重连一次刷同样的快照；保持连接靠心跳挂着即可，
//     看板页面关闭时浏览器会断开，r.Context() 随之取消。
//
// 验收：curl -N localhost:8080/api/tasks/<id>/events 能看到 data: 帧
// 随子任务状态变化逐条推出，空闲时约 15s 一行 ": hb"。
//
// 参考答案：docs/solutions/stage-03/exercise-8-server-dashboard.md（完成后再看）
func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, errors.New("TODO(练习8): handleTaskEvents 未实现"))
}
