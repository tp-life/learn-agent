# 练习 8 参考答案：Go 编排引擎 HTTP/SSE API + Next.js 实时看板

> 对应 TODO：`stage-03-multi-agent/internal/server/server.go` 的五个 `TODO(练习8)`
> （handleCreateTask / handleListTasks / handleGetTask / handleApprove /
> handleTaskEvents）、`web/app/tasks/[id]/page.tsx` 的 `TODO(练习8)`①②。
> **完成练习并自评后再看本文档。**
>
> 本文档全部实现代码（五个 handler + server 测试 + 详情页 + 进阶 TokenChart）
> 已于 2026-08-14 实际粘贴进项目验证：
> `go vet ./internal/server/ ./cmd/server/` 通过；
> `go test ./internal/server/ -count=1`（3 个测试）与 `-race` 全部通过；
> demo 模式 `go run ./cmd/server` + curl 全链路实测通过；
> web 骨架态 `npm install && npm run build` 通过，答案详情页 + 进阶 TokenChart
> 临时替换后 `npm run build` 同样通过，并对 demo 模式 Go 服务做了一次
> `npm run dev` 联调 smoke。验证后即恢复骨架、删除临时测试与进阶组件——
> **实现只属于答案，不进项目代码树**。详细记录见文末"验证记录"一节。
>
> **验证前提（如实说明）**：server 的测试要跑真编排器、真 checkpoint、真审批，
> 而骨架里 `pool.Run` 是 panic 桩、`task.Store`/`hitl.Service` 的方法是错误桩、
> `Orchestrator.Run/Resume` 是错误桩。验证时临时落地了四份既有参考答案：
> 练习1 的 `Pool.Run`、练习2 的 Store 实现（含练习5 前置补丁的两条迁移边）、
> 练习3 的编排器 Run/Resume（合并练习5 审批闸）、练习5 的 hitl Pending/Decide，
> 验证后已全部恢复骨架。你自己跑本练习的测试前，需要先完成练习1/2/3/5。

---

## 一、参考实现

### 1. `internal/server/server.go`（五个 handler + 任务 ID 生成；骨架其余部分不变）

import 从骨架扩为：`bytes` / `context` / `crypto/rand` / `database/sql` /
`encoding/hex` / `encoding/json` / `errors` / `fmt` / `log` / `net/http` /
`strings` / `time` + 三个 internal 包 + sqlite 驱动。

```go
// newTaskID 生成任务 ID：毫秒时间戳 + 4 字节随机 hex。
// 可读（能按时间排序个大概）且碰撞可忽略；不引 uuid 库。
func newTaskID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t-%d", time.Now().UnixNano()) // rand 都能失败时的兜底
	}
	return fmt.Sprintf("t-%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

// handleCreateTask 提交任务：HTTP 只"接单"，执行丢给后台 goroutine。
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法 JSON: %w", err))
		return
	}
	goal := strings.TrimSpace(req.Goal)
	if goal == "" {
		writeErr(w, http.StatusBadRequest, errors.New("goal 不能为空"))
		return
	}
	id := newTaskID()
	go func() {
		// 本练习第一坑：ctx 必须用 context.Background()，不能透传 r.Context()——
		// 响应一返回 r.Context() 就被取消，分钟级长任务刚起步就被掐死。
		if _, err := s.orch.Run(context.Background(), id, goal); err != nil &&
			!errors.Is(err, orchestrator.ErrWaitingHuman) {
			// ErrWaitingHuman 是"让出等审批"不是失败（哨兵错误，练习5）；
			// 真实失败的状态已落在 checkpoint 里（任务 failed），这里只补日志。
			log.Printf("server: 任务 %s 执行失败: %v", id, err)
		}
	}()
	// 202 Accepted：请求已受理，处理进行中——语义比 200 准确。
	// 已知小竞态：Run 在 goroutine 里才 CreateTask，立刻 GET 详情可能短暂 404，
	// 前端的轮询/SSE 天然容忍（下一拍就有了），不为它加同步。
	writeJSON(w, http.StatusAccepted, map[string]string{"task_id": id})
}

// handleListTasks 任务列表：本包自建读连接查询（不动 task.Store 契约）。
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, goal, status, total_tokens, created_at, updated_at
		 FROM tasks ORDER BY created_at DESC`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("查询任务列表: %w", err))
		return
	}
	defer rows.Close()
	tasks := make([]TaskView, 0) // 空列表序列化成 [] 而不是 null，前端 map 不用判空
	for rows.Next() {
		var t TaskView
		if err := rows.Scan(&t.ID, &t.Goal, &t.Status,
			&t.TotalTokens, &t.CreatedAt, &t.UpdatedAt); err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("读取任务行: %w", err))
			return
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("遍历任务列表: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

// handleGetTask 任务详情：LoadTask 一把读全（任务 + 子任务）。
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, subs, err := s.store.LoadTask(r.Context(), id)
	if err != nil {
		// 详情接口的调用方只关心"有没有这个任务"：不存在与读取失败统一 404。
		writeErr(w, http.StatusNotFound, fmt.Errorf("任务 %s: %w", id, err))
		return
	}
	writeJSON(w, http.StatusOK, toDetailView(t, subs))
}

// handleApprove 人工审批：Decide 落盘 → 全部批完则后台触发 Resume 续跑。
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req decideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("请求体不是合法 JSON: %w", err))
		return
	}
	if strings.TrimSpace(req.SubtaskID) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("subtask_id 不能为空"))
		return
	}
	if strings.TrimSpace(req.By) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("by（审批人）不能为空——审计必须留名"))
		return
	}
	// Decide 内部已校验"子任务确实在 waiting_human"——重复点击、过期页面
	// 都在这里拿到显式错误。409 Conflict：当前状态不允许这个操作。
	if err := s.svc.Decide(r.Context(), id, req.SubtaskID, req.Approved, req.By); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}

	// 是否触发续跑：任务在 waiting_human 且【没有其它子任务还在等批】。
	// 一个任务可能有多个待批项（planner 标了多个高风险子任务），
	// 批一个就 Resume 一次会让第一个 Resume 撞上剩余的审批闸立刻又让出——
	// 无害但浪费；等最后一批完再续跑，一次到位。
	t, subs, err := s.store.LoadTask(r.Context(), id)
	if err == nil && t.Status == task.StatusWaitingHuman {
		stillWaiting := false
		for _, sub := range subs {
			if sub.Status == task.StatusWaitingHuman {
				stillWaiting = true
				break
			}
		}
		if !stillWaiting {
			// 与 create 同一条纪律：后台 goroutine + context.Background()。
			// HTTP 请求不等长任务——Resume 可能再跑几分钟。
			go func() {
				if _, err := s.orch.Resume(context.Background(), id); err != nil &&
					!errors.Is(err, orchestrator.ErrWaitingHuman) {
					log.Printf("server: 任务 %s 审批后续跑失败: %v", id, err)
				}
			}()
		}
	}
	// 还有别的待批项时也返回 200：本次审批本身已成功，
	// 看板靠 SSE 看到"还在等下一项"。
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleTaskEvents SSE 实时事件流：poll-diff 推送任务快照。
//
// SSE 帧格式（手写，不需要库）："data: <单行JSON>\n\n"——
// "data: " 前缀 + 两个换行结尾是一帧；": " 开头是注释行（心跳用）。
func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("ResponseWriter 不支持 Flush"))
		return
	}
	// 先确认任务存在：对不存在的任务空转推送，看板会永远停在"加载中"。
	if _, _, err := s.store.LoadTask(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("任务 %s: %w", id, err))
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	poll := time.NewTicker(s.pollInterval)
	defer poll.Stop()
	hb := time.NewTicker(s.heartbeatInterval)
	defer hb.Stop()

	var last []byte
	// send 读一次 checkpoint，有变化才推（poll-diff）。返回 false = 连接该关了。
	send := func() bool {
		t, subs, err := s.store.LoadTask(r.Context(), id)
		if err != nil {
			return false // 任务被删/库坏了：断线让前端走 onerror 兜底
		}
		data, err := json.Marshal(toDetailView(t, subs))
		if err != nil {
			return true // 序列化失败不值得断线，下一拍再试
		}
		if bytes.Equal(data, last) {
			return true // 没变化不推：状态没动时连接上一片安静
		}
		last = data
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false // 写失败 = 客户端已经走了
		}
		flusher.Flush() // 不 Flush 数据堆在缓冲区，"实时"名存实亡
		return true
	}

	if !send() { // 首帧快照无条件推：前端 EventSource 一接上就有全量状态
		return
	}
	for {
		select {
		case <-r.Context().Done(): // 客户端断开（关页面/导航走）
			return
		case <-poll.C:
			if !send() {
				return
			}
		case <-hb.C:
			// 注释行心跳：反向代理（nginx 默认 proxy_read_timeout 60s）
			// 会掐掉长时间无字节的长连接，心跳是保活手段。
			if _, err := fmt.Fprint(w, ": hb\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
	// 注意：任务到终态后【不】主动关连接——EventSource 会自动重连，
	// 关了等于让它按重连节奏反复刷同样的终态快照；保持连接靠心跳挂着即可。
}
```

### 2. `internal/server/server_test.go`（新建，httptest 全链路，无需网络/LLM）

三个测试：全链路（提交→waiting_human→approve→done）、SSE 收到状态推送、
审批入参与冲突校验。假 Planner/Worker 注入——接口设计的红利再次兑现。

```go
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stage-03-multi-agent/internal/hitl"
	"stage-03-multi-agent/internal/orchestrator"
	"stage-03-multi-agent/internal/pool"
	"stage-03-multi-agent/internal/task"
)

// fakePlanner 固定三步计划：s2 高风险需审批（与 cmd/server 的 demo 同款）。
type fakePlanner struct{}

func (fakePlanner) Plan(context.Context, string) (orchestrator.Plan, error) {
	return orchestrator.Plan{Subtasks: []orchestrator.SubtaskSpec{
		{ID: "s1", Title: "收集数据", Prompt: "p1"},
		{ID: "s2", Title: "删除过期数据", Prompt: "p2", RequiresApproval: true},
		{ID: "s3", Title: "生成报告", Prompt: "p3"},
	}}, nil
}

// fakeWorker 快速回显并记录每个子任务的执行次数（断言审批闸用）。
type fakeWorker struct {
	mu    sync.Mutex
	calls map[string]int
}

func newFakeWorker() *fakeWorker { return &fakeWorker{calls: map[string]int{}} }

func (w *fakeWorker) Execute(_ context.Context, spec orchestrator.SubtaskSpec) (string, int, error) {
	w.mu.Lock()
	w.calls[spec.ID]++
	w.mu.Unlock()
	return "产出:" + spec.ID, 10, nil
}

func (w *fakeWorker) callCount(id string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[id]
}

// newTestServer 起完整栈：真 SQLite + 真 hitl + 真编排器（假 Planner/Worker）+ httptest。
// 这是"集成测试不烧 token"的标准姿势：越往下越真（存储/并发/状态机全真），
// 只有 LLM 这一层是假的。
func newTestServer(t *testing.T) (string, *fakeWorker) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "server-test.db")
	store, err := task.Open(dbPath)
	if err != nil {
		t.Fatalf("task.Open: %v", err)
	}
	svc, err := hitl.NewService(store, dbPath)
	if err != nil {
		t.Fatalf("hitl.NewService: %v", err)
	}
	worker := newFakeWorker()
	orch := orchestrator.New(store, pool.New(4, 10*time.Second), fakePlanner{}, worker)
	srv, err := New(store, svc, orch, dbPath)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	srv.pollInterval = 50 * time.Millisecond // 测试里加快轮询，等不起 1s
	srv.heartbeatInterval = time.Second
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		httpSrv.Close()
		srv.Close()
		svc.Close()
		store.Close()
	})
	return httpSrv.URL, worker
}

// createTask 提交一个任务，返回 task_id。
func createTask(t *testing.T, base, goal string) string {
	t.Helper()
	resp, err := http.Post(base+"/api/tasks", "application/json",
		strings.NewReader(fmt.Sprintf(`{"goal":%q}`, goal)))
	if err != nil {
		t.Fatalf("POST /api/tasks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/tasks status = %d, want 202", resp.StatusCode)
	}
	var body struct {
		TaskID string `json:"task_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.TaskID == "" {
		t.Fatal("task_id 为空")
	}
	return body.TaskID
}

// waitTaskStatus 轮询详情直到任务进入目标状态（或超时失败）。
// 同时天然覆盖了"202 返回后立刻 GET 可能短暂 404"的竞态——404 就再试。
func waitTaskStatus(t *testing.T, base, id string, want task.Status) TaskDetailView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/tasks/" + id)
		if err == nil && resp.StatusCode == http.StatusOK {
			var detail TaskDetailView
			_ = json.NewDecoder(resp.Body).Decode(&detail)
			resp.Body.Close()
			if detail.Status == want {
				return detail
			}
		} else if err == nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("任务 %s 未在 10s 内进入 %s", id, want)
	return TaskDetailView{}
}

// TestHTTPLifecycle 全链路：提交 → 撞审批闸 waiting_human → approve → 续跑至 done。
func TestHTTPLifecycle(t *testing.T) {
	base, worker := newTestServer(t)

	id := createTask(t, base, "数据治理周报")

	// 1. 后台 goroutine 执行到 s2 审批闸 → 任务 waiting_human
	detail := waitTaskStatus(t, base, id, task.StatusWaitingHuman)
	byID := map[string]SubtaskView{}
	for _, s := range detail.Subtasks {
		byID[s.ID] = s
	}
	if byID["s1"].Status != task.StatusDone || byID["s3"].Status != task.StatusDone {
		t.Errorf("普通子任务应已 done: s1=%s s3=%s", byID["s1"].Status, byID["s3"].Status)
	}
	if byID["s2"].Status != task.StatusWaitingHuman {
		t.Errorf("s2 status = %s, want waiting_human", byID["s2"].Status)
	}
	if worker.callCount("s2") != 0 {
		t.Error("s2 在审批前被执行了（审批闸失效）")
	}

	// 2. 任务列表里能看到它，且状态是 waiting_human
	resp, err := http.Get(base + "/api/tasks")
	if err != nil {
		t.Fatalf("GET /api/tasks: %v", err)
	}
	var list struct {
		Tasks []TaskView `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	found := false
	for _, tv := range list.Tasks {
		if tv.ID == id {
			found = true
			if tv.Status != task.StatusWaitingHuman {
				t.Errorf("列表中任务状态 = %s, want waiting_human", tv.Status)
			}
		}
	}
	if !found {
		t.Error("任务列表里没有刚提交的任务")
	}

	// 3. approve → 服务端后台 Resume → 任务推进到 done
	resp, err = http.Post(base+"/api/tasks/"+id+"/approve", "application/json",
		strings.NewReader(`{"subtask_id":"s2","approved":true,"by":"tester"}`))
	if err != nil {
		t.Fatalf("POST approve: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve status = %d, want 200", resp.StatusCode)
	}

	final := waitTaskStatus(t, base, id, task.StatusDone)
	if worker.callCount("s2") != 1 {
		t.Errorf("approve 后 s2 执行 %d 次, want 恰好 1 次", worker.callCount("s2"))
	}
	if final.TotalTokens != 30 {
		t.Errorf("TotalTokens = %d, want 30（3 子任务 × 10）", final.TotalTokens)
	}
}

// TestSSE_PushesSnapshot：订阅 events，必须收到含 waiting_human 的 data 帧，
// 且 Content-Type 是 text/event-stream。
func TestSSE_PushesSnapshot(t *testing.T) {
	base, _ := newTestServer(t)
	id := createTask(t, base, "g")
	waitTaskStatus(t, base, id, task.StatusWaitingHuman) // 先到位再订阅，避免 404 竞态

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/api/tasks/"+id+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// 首帧快照立即到达，包含 waiting_human 状态。
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 快照是单行 JSON，放宽行长
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"status":"waiting_human"`) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("10s 内未收到 waiting_human 快照（scanner err: %v）", scanner.Err())
	}
}

// TestApprove_Validation：空 by / 空 subtask_id → 400；
// 对不在等批的子任务做决定 → 409；CORS 预检 → 204。
func TestApprove_Validation(t *testing.T) {
	base, _ := newTestServer(t)
	id := createTask(t, base, "g")
	waitTaskStatus(t, base, id, task.StatusWaitingHuman)

	post := func(body string) int {
		resp, err := http.Post(base+"/api/tasks/"+id+"/approve", "application/json",
			strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(`{"subtask_id":"s2","approved":true,"by":""}`); code != http.StatusBadRequest {
		t.Errorf("空 by status = %d, want 400（审计必须留名）", code)
	}
	if code := post(`{"subtask_id":"","approved":true,"by":"x"}`); code != http.StatusBadRequest {
		t.Errorf("空 subtask_id status = %d, want 400", code)
	}
	// s1 已 done，不在等批 → 409（Decide 的"必须在 waiting_human"校验透传）
	if code := post(`{"subtask_id":"s1","approved":true,"by":"x"}`); code != http.StatusConflict {
		t.Errorf("审批已 done 的子任务 status = %d, want 409", code)
	}

	// CORS 预检：浏览器跨源 POST 前会先发 OPTIONS
	req, _ := http.NewRequest(http.MethodOptions, base+"/api/tasks", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want 204", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") == "" {
		t.Error("缺少 Access-Control-Allow-Origin 头（看板跨源会失败）")
	}
}
```

### 3. `web/app/tasks/[id]/page.tsx`（详情页完整实现，替换骨架）

```tsx
/**
 * app/tasks/[id]/page.tsx —— 任务详情页：子任务进度 + token 成本 + 人工审批。
 *
 * 练习8 前端答案：① EventSource 订阅 SSE 实时刷新；② waiting_human 审批交互。
 */

"use client";

import { useEffect, useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { API_BASE, decide, getTask, type TaskDetail } from "@/lib/api";
import { StatusBadge } from "@/components/status-badge";

export default function TaskDetailPage() {
  const { id } = useParams<{ id: string }>();
  const [task, setTask] = useState<TaskDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  // 审批提交中的子任务 ID：点击后 disable 所有审批按钮防重复提交。
  const [deciding, setDeciding] = useState<string | null>(null);
  const [decideError, setDecideError] = useState<string | null>(null);

  useEffect(() => {
    // 首屏先拉一次：SSE 连接建立前页面不白屏。
    getTask(id)
      .then(setTask)
      .catch((e) => setError(String(e)));

    // EventSource 是浏览器原生 SSE 客户端：自动按 "data: ...\n\n" 分帧、
    // 断线自动重连——阶段二手写解析的那套这里全由浏览器代劳。
    // 服务端的 poll-diff 保证"状态没变时一帧都不推"，所以每次 onmessage
    // 都是真实变化，直接 setTask 全量替换即可（快照是全量不是增量）。
    const es = new EventSource(`${API_BASE}/api/tasks/${id}/events`);
    es.onmessage = (e) => {
      setTask(JSON.parse(e.data) as TaskDetail);
    };
    es.onerror = () => {
      // 断线期间 EventSource 自己重连；补一次 getTask 兜底，
      // 避免长时间停留在旧状态。注释行心跳（": hb"）不会触发 onmessage，
      // 这正是心跳用注释行的原因——保活但不惊动应用层。
      getTask(id)
        .then(setTask)
        .catch(() => {});
    };
    // cleanup 必须 close：组件卸载不关连接会泄漏长连接
    // （React StrictMode 开发模式 effect 会双跑，写对 cleanup 就无害）。
    return () => es.close();
  }, [id]);

  const onDecide = async (subtaskId: string, approved: boolean) => {
    setDeciding(subtaskId);
    setDecideError(null);
    try {
      // "dashboard-user" 是演示用审批人标识——审计必须留名
      // （练习5 的纪律在前端的落点）；真实产品里这里应是登录态用户名。
      await decide(id, subtaskId, approved, "dashboard-user");
      // 决定落盘后不需要手动刷新：服务端在"该任务全部批完"时自动
      // Resume 续跑，新状态经 SSE 推过来——事件驱动恢复的体感就在这里。
    } catch (e) {
      setDecideError(String(e));
    } finally {
      setDeciding(null);
    }
  };

  if (error) {
    return (
      <main style={{ maxWidth: 860, margin: "40px auto", fontFamily: "system-ui" }}>
        <div style={{ color: "#c00" }}>加载失败：{error}</div>
        <Link href="/">← 返回列表</Link>
      </main>
    );
  }
  if (!task) {
    return (
      <main style={{ maxWidth: 860, margin: "40px auto", fontFamily: "system-ui" }}>
        <div style={{ color: "#999" }}>加载中…</div>
      </main>
    );
  }

  return (
    <main
      style={{
        maxWidth: 860,
        margin: "40px auto",
        padding: "0 16px",
        fontFamily: "system-ui",
        display: "flex",
        flexDirection: "column",
        gap: 16,
      }}
    >
      <div>
        <Link href="/" style={{ color: "#1565c0" }}>
          ← 返回列表
        </Link>
      </div>
      <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
        <h1 style={{ margin: 0, fontSize: 20 }}>{task.goal}</h1>
        <StatusBadge status={task.status} />
      </div>
      <div style={{ color: "#666", fontSize: 13 }}>
        任务 {task.id} · 累计 {task.total_tokens} tokens · 更新于{" "}
        {new Date(task.updated_at).toLocaleTimeString()}
      </div>

      <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
        {task.subtasks.map((s) => (
          <div
            key={s.id}
            style={{
              // waiting_human 高亮：审批项是"需要人做事"的状态，必须一眼跳出来
              border:
                s.status === "waiting_human"
                  ? "1px solid #ffb74d"
                  : "1px solid #e0e0e0",
              background: s.status === "waiting_human" ? "#fffde7" : "#fff",
              borderRadius: 8,
              padding: "10px 14px",
              display: "flex",
              flexDirection: "column",
              gap: 6,
            }}
          >
            <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
              <StatusBadge status={s.status} />
              <b>
                [{s.id}] {s.title}
              </b>
              <span style={{ color: "#999", fontSize: 12 }}>
                {s.tokens_used} tokens · 执行 {s.attempts} 次
                {s.requires_approval ? " · 高风险" : ""}
              </span>
            </div>
            {s.output && (
              <div style={{ color: "#444", fontSize: 13, whiteSpace: "pre-wrap" }}>
                {s.output}
              </div>
            )}
            {s.status === "waiting_human" && (
              <div
                style={{
                  border: "1px solid #ffe082",
                  background: "#fff8e1",
                  borderRadius: 6,
                  padding: "8px 10px",
                  display: "flex",
                  flexDirection: "column",
                  gap: 6,
                }}
              >
                <div style={{ fontSize: 13 }}>
                  <b>等待人工审批</b> · 该子任务要求执行的操作：{s.prompt}
                </div>
                <div style={{ display: "flex", gap: 8 }}>
                  <button
                    disabled={deciding !== null}
                    onClick={() => onDecide(s.id, true)}
                    style={{
                      background: "#2e7d32",
                      color: "#fff",
                      border: "none",
                      borderRadius: 4,
                      padding: "6px 16px",
                      cursor: "pointer",
                    }}
                  >
                    批准执行
                  </button>
                  <button
                    disabled={deciding !== null}
                    onClick={() => onDecide(s.id, false)}
                    style={{
                      background: "#c62828",
                      color: "#fff",
                      border: "none",
                      borderRadius: 4,
                      padding: "6px 16px",
                      cursor: "pointer",
                    }}
                  >
                    驳回
                  </button>
                </div>
              </div>
            )}
          </div>
        ))}
      </div>
      {decideError && <div style={{ color: "#c00" }}>审批提交失败:{decideError}</div>}
    </main>
  );
}
```

## 二、关键设计点

1. **HTTP 只接单，长任务放后台 goroutine（本练习第一考点）**。
   `orchestrator.Run`/`Resume` 是分钟级长任务，同步等待意味着 HTTP 请求挂几分钟：
   浏览器超时、网关超时、连接数占满，全是事故。所以 create/approve 都是
   "落盘/触发 → 立即返回（202/200）→ 进度靠 GET 与 SSE 查"。这与 hitl-demo
   CLI 的审批循环是同一个"让出 + 事件驱动恢复"模型，只是触发源从 stdin 换成
   HTTP。**易错处**：goroutine 里用 `r.Context()`——响应一返回它就被取消，
   任务刚起步就被掐死；必须用 `context.Background()`。反过来说，读路径
   （list/get/SSE 的单次查询）用 `r.Context()` 是对的：客户端断开时查询
   没必要继续。

2. **SSE 用 poll-diff 而不是 orchestrator 事件 hook（简单 vs 实时的取舍）**。
   poll-diff：每秒读一次 checkpoint，JSON 变了才推。代价是 ≤1s 延迟和每秒一次
   SQLite 读；收益是**零侵入**——orchestrator/task/hitl 的契约（练习2/3/5）
   一行不改。事件 hook（状态迁移时回调）能做到毫秒级，但要改编排器契约、
   处理回调的并发与 panic 隔离。还有一个更深的理由：**checkpoint 在 SQLite 里，
   任何进程都能读；事件 hook 只在跑编排器的那个进程里有效**——approve 触发的
   状态变化发生在 HTTP 进程，如果编排器在另一个进程跑，hook 根本收不到，
   而 poll 读库天然跨进程正确（教程"状态外置"红利的又一次兑现）。
   看板是"给人看"的场景，秒级足够；hook 是性能瓶颈真的出现后的优化方向
   （见进阶节的开放讨论）。

3. **approve 后"全部批完才触发 Resume"**。planner 可能标多个高风险子任务，
   批一个就 Resume 一次，第一次 Resume 会撞上剩余的审批闸立刻又让出——无害
   但浪费一轮 goroutine 与状态迁移。所以 handler 里 Decide 之后重新 LoadTask，
   确认任务在 waiting_human 且没有任何子任务还在等批，才后台 Resume。
   **易错处**：不检查直接 Resume——能跑通单项审批的 demo，多项审批时行为
   就变成"批一项抖一下"。

4. **demo 模式的意义（面试演示不烧 API）**。cmd/server 在未设
   `DEEPSEEK_API_KEY` 时用固定计划的假 Planner + 延时回显的假 Worker：
   ① 演示零成本零网络依赖——面试现场热点断网也能跑；② 结果可预期——
   固定三步计划，审批点必现，不会演示到一半 LLM 给你分解出八个子任务；
   ③ 前端/HTTP 层的开发与测试完全不需要等练习3 的 LLM 实现。
   假 Worker 的 1.2s 延时不是装饰：没有它任务瞬间跑完，看板上看不到
   状态流转，SSE 演示名存实亡。**面试话术**：这就是 Planner/Worker 接口注入
   （练习3）的系统级红利——"换实现"从测试技巧变成了产品功能（demo 模式）。

5. **server 自建只读连接，不回改 task.Store 契约**。任务列表需要"全部任务
   含终态"，Store 只暴露 ListResumable。照 hitl.NewService 的先例：本包对同一
   SQLite 文件开自己的连接做列表查询，写操作仍全部走 Store/Service 的
   状态机守卫。这条纪律保证"状态迁移只有一个入口"——server 如果图省事直接
   SQL 改状态，练习2 的守卫、练习5 的审批语义就全被绕过了。

6. **SSE 的三个工程细节**。① 每帧写完必须 `Flush()`，否则数据堆在 HTTP
   缓冲区里，"实时"名存实亡；② 心跳用**注释行**（`: hb\n\n`）而不是
   `data:` 帧——注释行不会触发前端 `onmessage`，保活但不惊动应用层；
   反向代理（nginx 默认 60s）会掐空闲长连接，15s 心跳是保活手段；
   ③ 任务终态后**不主动关连接**——EventSource 会自动重连，服务端关连接
   等于让浏览器按重连节奏反复刷同样的终态快照；保持连接挂着，看板页面
   关闭时 `r.Context()` 取消，服务端循环自然退出。

7. **前端：EventSource 与 cleanup 纪律**。EventSource 是浏览器原生 API，
   自动分帧、自动重连——阶段二手写 SSE 解析（找 `data: ` 前缀、按 `\n\n`
   切帧）在这里全被代劳。两个纪律：effect cleanup 必须 `es.close()`
   （否则每进一次详情页泄漏一条长连接；React StrictMode 开发模式 effect
   双跑，cleanup 写对就无害）；首屏先 `getTask` 一次再接 SSE（连接建立前
   页面不白屏）。审批按钮点击后 `disabled` 防重复提交——服务端 Decide 的
   "必须在 waiting_human"校验是最后防线，但 409 错误不该是正常交互的一部分。

## 三、进阶实现（加分项：token 成本图表）

> 回补记录：本节代码于验证时以临时文件（`web/components/token-chart.tsx`）
> 实际粘贴进项目并接入详情页，`npm run build` 通过，验证后已从项目删除——
> **进阶实现只属于答案，不进项目代码树**，项目保持骨架版。

### 设计取舍

- **零依赖纯 CSS 条形图**：token 成本是"一眼看清谁最贵"的概览需求，
  横向条形图足够；引 recharts/chart.js 为一个条形图增加几百 KB 依赖不值。
  这也与项目"不引 UI 库"的约定一致。
- **数据已在快照里**：SSE 快照的 `tokens_used` 每帧都有，图表随 SSE 自动
  实时更新——这是"快照是全量不是增量"的附带红利，图表组件零事件处理。
- **为什么挂详情页而不是列表页**：成本分析是单任务下钻场景；列表页只显示
  总计（已有）。

### `web/components/token-chart.tsx`（进阶实现完整代码）

```tsx
/**
 * components/token-chart.tsx —— 子任务 token 成本条形图（练习8 进阶）。
 *
 * 零依赖：纯 inline style 的 div 条形，不引图表库。
 * 数据来自 SSE 快照的 tokens_used——快照每帧全量到达，
 * 本组件无需任何事件处理，随父组件重渲染自动实时更新。
 */

import type { SubtaskView } from "@/lib/api";

export function TokenChart({ subtasks }: { subtasks: SubtaskView[] }) {
  const total = subtasks.reduce((sum, s) => sum + s.tokens_used, 0);
  const max = Math.max(1, ...subtasks.map((s) => s.tokens_used)); // 防除零
  return (
    <div
      style={{
        border: "1px solid #e0e0e0",
        borderRadius: 8,
        padding: "10px 14px",
        display: "flex",
        flexDirection: "column",
        gap: 6,
      }}
    >
      <b style={{ fontSize: 14 }}>token 成本（共 {total}）</b>
      {subtasks.map((s) => (
        <div
          key={s.id}
          style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 12 }}
        >
          <span style={{ width: 40, color: "#666" }}>{s.id}</span>
          <div style={{ flex: 1, background: "#f5f5f5", borderRadius: 4, height: 14 }}>
            <div
              style={{
                width: `${(s.tokens_used / max) * 100}%`,
                background: "#1565c0",
                borderRadius: 4,
                height: "100%",
                transition: "width 0.3s", // SSE 推新值时条形平滑变长
              }}
            />
          </div>
          <span style={{ width: 48, textAlign: "right" }}>{s.tokens_used}</span>
        </div>
      ))}
    </div>
  );
}
```

接入详情页（答案第一节第 3 小节的 page.tsx 基础上两处改动）：

```tsx
import { TokenChart } from "@/components/token-chart";
// ……子任务列表 </div> 之后、decideError 之前插入：
{task.subtasks.length > 0 && <TokenChart subtasks={task.subtasks} />}
```

### 进阶实现的易错处

1. **除零**：全部子任务 tokens_used 为 0 时（如真实模式 token 记账未接）
   `Math.max(1, ...)` 兜底，宽度全 0 而不是 NaN%。
2. **不要为图表引入状态**：tokens 数据已在父组件的 `task` 状态里，
   图表做纯展示组件；给图表自己加 useState/useEffect 是双重数据源的开端。

### orchestrator 事件 hook 替换 poll（开放讨论，无需实现）

**本项为开放讨论，无唯一正确实现，不要求写代码。** 理由：给 orchestrator 加
事件 hook 需要修改练习3/5 已验收的契约（Run/dispatch 的迁移点都要插回调），
已超出练习8 的边界；且合理形态强依赖部署决策（编排器与 HTTP 服务同进程还是
分进程），学习项目没有真实约束。讨论要点（面试可讲）：
① hook 签名与触发点——在 task.Store 的每个迁移方法里回调（覆盖最全，但
Store 是练习2 的契约）还是在 orchestrator 层回调（只覆盖编排器驱动的迁移，
漏掉 hitl.Decide 的）；② 回调的并发与 panic 隔离——hook 在 worker goroutine
里同步执行，慢订阅者会拖慢编排主循环，需要缓冲 channel + 专用分发 goroutine；
③ **跨进程盲区**——hook 只在跑编排器的进程里有效，approve 这类由 HTTP 进程
发起的迁移收不到（除非 hook 挂在 Store 上且两进程共享……不，SQLite 不跨进程
发通知）；poll 读库天然跨进程正确。结论：同进程部署且延迟敏感时 hook 值得做，
否则 poll-diff 是更稳的默认。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [ ] `handleCreateTask` / `handleApprove` 把 Run/Resume 放后台 goroutine 并
      立即返回，goroutine 的 ctx 是 `context.Background()` 而不是 `r.Context()`；
      能讲清为什么
- [ ] `handleApprove` 走 `svc.Decide`（不直接 SQL 改状态），且在"任务
      waiting_human 且无其它待批子任务"时才触发 Resume；审批人 by 非空校验
- [ ] SSE：首帧立即推快照；`data: <json>\n\n` 帧格式手写正确；每帧后 Flush；
      有变化才推（diff）；注释行心跳；客户端断开（r.Context().Done()）循环退出
- [ ] 任务列表走 server 自建读连接，不回改 task.Store 契约；空列表返回 `[]`
- [ ] 详情页：EventSource 订阅 + cleanup `es.close()`；首屏先 getTask 兜底；
      waiting_human 子任务高亮 + 批准/驳回按钮（点击后 disable 防重复提交）
- [ ] 测试覆盖：httptest 全链路（提交→waiting_human→approve→done）、
      SSE 收到快照、审批 400/409 校验；`go vet` 与 `go test -count=1` 全绿
- [ ] demo 模式实测：`go run ./cmd/server`（不设 API key）+ curl 走完
      提交/查询/SSE/审批全链路；`web` 下 `npm run build` 通过
- [ ] 能口头回答：为什么 poll-diff 而不是事件 hook（含跨进程论据）？
      为什么 demo 模式本身值得做？SSE 心跳为什么是注释行？

加分项（做了才需要勾，参考"进阶实现"一节）：

- [ ] TokenChart 纯 CSS 条形图接入详情页，随 SSE 快照实时更新，
      `npm run build` 通过；能讲清为什么不引图表库
- [ ] 能讲清事件 hook 的三个讨论要点（触发点、并发隔离、跨进程盲区）

---

## 验证记录

**日期**：2026-08-14（`date +%F`）。

**验证前提**：骨架态的 pool/task/orchestrator/hitl 都是桩，验证时临时落地了
练习1/2/3/5 的既有参考实现（含练习5 的子任务迁移表补丁：pending→waiting_human、
waiting_human→pending 两条边），再粘贴本文档的 server handlers + server_test.go；
web 侧临时替换详情页答案并新增 token-chart.tsx。验证后全部恢复骨架、
删除 server_test.go 与 token-chart.tsx（恢复后与骨架逐字节 diff 一致）。

**Go 侧**（`cd stage-03-multi-agent`）：

- `go vet ./internal/server/ ./cmd/server/` 通过；`go vet ./...` 通过。
- `go test ./internal/server/ -count=1`：3 个测试全过
  （TestHTTPLifecycle / TestSSE_PushesSnapshot / TestApprove_Validation，0.7s）；
  `-race` 复跑同样全过（1.8s）。
- 恢复骨架后 `go build ./...` 通过。

**demo 模式 curl 实测**（`env -u DEEPSEEK_API_KEY go run ./cmd/server --addr :18080 --db /tmp/ex8-demo.db`）：

- `POST /api/tasks {"goal":"写一份数据治理周报"}` → 202 `{"task_id":"t-...-b33b3876"}`；
  约 4s 后 `GET /api/tasks/{id}`：任务 `waiting_human`，s1/s3 `done`（各 42 tokens），
  s2 `waiting_human` 且 `attempts: 0`（审批闸拦在执行前）；
- `GET /api/tasks` 列表含该任务，状态 `waiting_human`；
- `curl -N .../events`：立即收到首帧快照；新任务边跑边收帧时观察到
  `frame1: task=running subtasks=[running, waiting_human, running]` →
  `frame2: task=waiting_human subtasks=[done, waiting_human, done]`——
  有变化才推（poll-diff），无变化时连接安静；
- `POST .../approve {"subtask_id":"s2","approved":true,"by":"curl-tester"}` →
  `{"ok":true}`；约 3s 后任务 `done`，s2 `attempts: 1`，`total_tokens: 126`（3×42）。

**Web 侧**（`cd stage-03-multi-agent/web`，node v22.12.0 / npm 11.5.2 / Next.js 16.3.1）：

- 骨架态：`npm install`（60 包）+ `npm run build` 通过
  （路由：`/` 静态、`/tasks/[id]` 动态）；
- 答案详情页 + 进阶 TokenChart 临时替换后 `npm run build` 同样通过；
  恢复骨架后 `npx tsc --noEmit` 通过；
- 联调 smoke（demo Go 服务 :18080 + `npm run dev` :13000）：列表页与详情页
  HTTP 200，页面渲染正常，通过看板同源 API 创建任务成功。
  **未覆盖项（如实说明）**：浏览器侧的 EventSource 实时刷新与审批按钮点击
  未做自动化验证（无浏览器环境）——这两段逻辑的类型与构建已验证，
  运行时行为靠 SSE/审批的 curl 实测间接背书。
