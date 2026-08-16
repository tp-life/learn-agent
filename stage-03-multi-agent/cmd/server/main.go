// server：多 agent 编排引擎的 HTTP/SSE 服务入口（练习8）。
//
// 与 web/ 看板配套：看板（localhost:3000）通过本服务（默认 :8080）
// 提交任务、看进度、做审批、订阅 SSE 实时事件。
//
// 两种运行模式：
//   - demo 模式（未设置 DEEPSEEK_API_KEY）：假 Planner（固定三步计划，
//     其中"删除过期数据"需人工审批）+ 假 Worker（延时回显，不调用 LLM）。
//     不烧一分 token 就能演示"提交 → 并发执行 → 审批 → 续跑 → 完成"全链路——
//     面试现场演示、离线开发都用它；
//   - 真实模式：LLMPlanner（练习3）+ AgentWorker（练习3，mini-agent 内核）。
//     注意真实模式依赖练习3 的实现完成；骨架态下 Plan/Execute 会返回 TODO 错误。
//
// 练习：本文件的接线（flag、Store/Service/Orchestrator/Server 构造、
// demo 假实现、崩溃恢复续跑）已完整提供，无需学习者完成；
// 练习8 的实现区在 internal/server 的 5 个 handler 与 web/ 详情页。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"mini-agent/api"
	"stage-03-multi-agent/internal/hitl"
	"stage-03-multi-agent/internal/orchestrator"
	"stage-03-multi-agent/internal/pool"
	"stage-03-multi-agent/internal/server"
	"stage-03-multi-agent/internal/task"
)

// demoPlanner 预置一个含高风险子任务的固定计划（与 cmd/hitl-demo 同款）：
// 任何 goal 都分解为"收集数据 → 删除过期数据（需审批）→ 生成报告"。
// 固定计划的意义：demo 演示的是编排与看板链路，不是分解能力——
// 把 LLM 从这个链路里拿掉，演示就零成本、零网络依赖、结果可预期。
type demoPlanner struct{}

func (demoPlanner) Plan(_ context.Context, _ string) (orchestrator.Plan, error) {
	return orchestrator.Plan{Subtasks: []orchestrator.SubtaskSpec{
		{ID: "s1", Title: "收集数据", Prompt: "收集本周的业务数据"},
		{ID: "s2", Title: "删除过期数据", Prompt: "删除 90 天前的过期业务数据", RequiresApproval: true},
		{ID: "s3", Title: "生成报告", Prompt: "把数据整理成周报"},
	}}, nil
}

// demoWorker 延时回显的假 Worker。为什么要 sleep：真实 worker 是几十秒的
// LLM 调用，没有延时的话 demo 任务瞬间跑完，看板上看不到
// pending → running → done 的流转，SSE 演示就名存实亡。
// 返回 42 个假 token：看板的成本栏有数字可看，且能验证 token 记账链路。
type demoWorker struct{ delay time.Duration }

func (w demoWorker) Execute(ctx context.Context, spec orchestrator.SubtaskSpec) (string, int, error) {
	select {
	case <-time.After(w.delay):
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
	return fmt.Sprintf("[demo 产出] 已完成「%s」（指令：%s）", spec.Title, spec.Prompt), 42, nil
}

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	dbPath := flag.String("db", "server.db", "SQLite 路径（固定路径才能演示重启续跑）")
	flag.Parse()

	store, err := task.Open(*dbPath)
	if err != nil {
		log.Fatalf("打开任务库: %v", err)
	}
	defer store.Close()
	svc, err := hitl.NewService(store, *dbPath)
	if err != nil {
		log.Fatalf("打开审批服务: %v", err)
	}
	defer svc.Close()

	var planner orchestrator.Planner
	var worker orchestrator.Worker
	if os.Getenv("DEEPSEEK_API_KEY") == "" {
		log.Println("DEEPSEEK_API_KEY 未设置 → demo 模式（假 Planner/Worker，不调用真实 LLM）")
		planner = demoPlanner{}
		worker = demoWorker{delay: 1200 * time.Millisecond}
	} else {
		// 真实模式：planner 分解交给 LLM，worker 是 mini-agent 内核（ReAct 循环）。
		// registry 传 nil = worker 无工具（纯生成型子任务）；要挂 Calculator/
		// HTTPFetch/知识库工具时在这里 api.NewRegistry() + Register。
		client := api.NewClient(os.Getenv("DEEPSEEK_API_KEY"))
		planner = orchestrator.NewLLMPlanner(client)
		worker = orchestrator.NewAgentWorker(client, nil)
	}

	// 并发上限 4、单 job 预算 3 分钟：demo 三个子任务全并行；
	// 真实模式下对齐 DeepSeek 的并发配额，宁小勿大（教程 Q11）。
	orch := orchestrator.New(store, pool.New(4, 3*time.Minute), planner, worker)

	// 崩溃恢复（教程 Q4）：进程重启后把上次没跑完的任务续上。
	// waiting_human 的任务 Resume 后会再次让出等审批，无副作用；
	// 已批未执行的子任务会接着跑——"状态外置"在服务入口的兑现。
	if ids, err := store.ListResumable(context.Background()); err == nil {
		for _, id := range ids {
			id := id
			log.Printf("发现未完成任务 %s，从 checkpoint 续跑", id)
			go func() {
				// ErrWaitingHuman 是"让出等审批"不是失败（哨兵错误，练习5）。
				if _, err := orch.Resume(context.Background(), id); err != nil &&
					!errors.Is(err, orchestrator.ErrWaitingHuman) {
					log.Printf("续跑任务 %s 失败: %v", id, err)
				}
			}()
		}
	}

	srv, err := server.New(store, svc, orch, *dbPath)
	if err != nil {
		log.Fatalf("构造 HTTP 服务: %v", err)
	}
	defer srv.Close()

	log.Printf("看板 API  listening on %s（db=%s）", *addr, *dbPath)
	log.Printf("启动看板：cd web && npm run dev，打开 http://localhost:3000")
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}
