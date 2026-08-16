// hitl-demo：HITL 审批点（练习5）的离线演示 CLI。
//
// 演示目标（对应教程 3.1 Q6 的"暂停-恢复"）：
//  1. 跑一个含高风险子任务（RequiresApproval）的任务，编排器撞审批闸后
//     让出（ErrWaitingHuman），进程不阻塞、不占 goroutine；
//  2. 人工在 stdin 输入 a（approve）/ r（reject），决定落盘；
//  3. Resume 从断点续跑；
//  4. --db 指定 SQLite 路径：跑到一半 Ctrl-C 杀掉进程，重新执行同一条命令，
//     任务从 checkpoint 续跑、"已批未执行"的决定不丢——状态外置的现场演示。
//
// 全程离线：Planner 是预置计划的假实现，Worker 是回显的假实现，不烧 token。
//
// 练习：main 的接线（flag、Store/Service/Orchestrator 构造）、假 Planner/Worker、
// askDecision 均已完整提供；"续跑 or 新跑"的入口选择与审批循环 approveLoop
// 为 TODO(练习5)。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"stage-03-multi-agent/internal/hitl"
	"stage-03-multi-agent/internal/orchestrator"
	"stage-03-multi-agent/internal/pool"
	"stage-03-multi-agent/internal/task"
)

// demoTaskID 固定任务 ID：演示"杀掉进程重启后续跑"需要重启前后找得到同一个任务。
const demoTaskID = "demo-task"

// fakePlanner 预置一个含高风险子任务的计划。
// 真实场景里 RequiresApproval 由 planner 按子任务风险标记（或 HTTP 层提交任务时指定），
// 演示里写死一个"删除过期数据"——删数据是教科书级的高风险操作。
type fakePlanner struct{}

func (fakePlanner) Plan(_ context.Context, _ string) (orchestrator.Plan, error) {
	return orchestrator.Plan{Subtasks: []orchestrator.SubtaskSpec{
		{ID: "s1", Title: "收集数据", Prompt: "收集本周的业务数据"},
		{ID: "s2", Title: "删除过期数据", Prompt: "删除 90 天前的过期业务数据", RequiresApproval: true},
		{ID: "s3", Title: "生成报告", Prompt: "把数据整理成周报"},
	}}, nil
}

// echoWorker 回显假 Worker：不调 LLM，立刻返回一段产出文本。
type echoWorker struct{}

func (echoWorker) Execute(_ context.Context, spec orchestrator.SubtaskSpec) (string, int, error) {
	return fmt.Sprintf("[worker 产出] 已完成「%s」", spec.Title), 0, nil
}

// askDecision 交互式读取一个审批决定：a=批准，r=驳回，其他输入重问。
// stdin 被管道喂完（EOF）时默认驳回——演示脚本没喂够输入时宁可不执行高风险操作。
func askDecision(r *bufio.Reader, p hitl.PendingApproval) bool {
	fmt.Printf("  [%s] %s\n    子任务指令：%s\n", p.SubtaskID, p.SubtaskTitle, p.Prompt)
	for {
		fmt.Print("    批准执行？(a=approve / r=reject): ")
		line, err := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "a":
			return true
		case "r":
			return false
		}
		if err != nil { // EOF 且没读到有效输入：保守驳回
			fmt.Println("    （输入结束，默认驳回）")
			return false
		}
	}
}

func main() {
	dbPath := flag.String("db", "hitl-demo.db", "SQLite 路径（固定路径才能演示重启续跑）")
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
	orch := orchestrator.New(store, pool.New(2, time.Minute), fakePlanner{}, echoWorker{})

	ctx := context.Background()
	reader := bufio.NewReader(os.Stdin)

	// TODO(练习5): 演示主流程 —— "续跑 or 新跑"入口 + 审批循环
	//
	// 任务：补全下面两段逻辑（建议把第 2 段抽成 approveLoop 函数）：
	//
	//  1. 入口选择：store.ListResumable 里已有 demoTaskID → 打印提示并
	//     orch.Resume（进程重启续跑的演示路径）；否则 orch.Run 开新任务。
	//
	//  2. 审批循环：只要上一步返回的错误 errors.Is(orchestrator.ErrWaitingHuman)，
	//     就：svc.Pending() 列出待审批项 → 逐个 askDecision → svc.Decide 落盘
	//     （审批人传 "demo-user"）→ orch.Resume 续跑；
	//     循环直到错误不再是 ErrWaitingHuman。
	//     最后：err 非空则 log.Fatal，否则打印最终汇总。
	//
	// 提示：
	//   - ErrWaitingHuman 是"让出"不是失败——它是 for 循环的继续条件，
	//     别进 log.Fatal 分支；
	//   - 每轮 Pending 可能有多项（多个高风险子任务），逐个问逐个 Decide；
	//   - askDecision 已提供；reader 在 main 里只建一次传下去
	//     （bufio.Reader 会预读缓冲，每轮新建会把管道里后续的输入吞掉）。
	//
	// 验收（三条路径都要手动跑通）：
	//   printf 'a\n' | go run ./cmd/hitl-demo --db /tmp/hitl.db  → s2 执行，任务 done
	//   printf 'r\n' | go run ./cmd/hitl-demo --db /tmp/hitl.db  → s2 failed，任务 done（部分失败）
	//   跑到审批提示时 Ctrl-C，再执行同一条命令 → 从 waiting_human 现场续跑
	//
	// 参考答案：docs/solutions/stage-03/exercise-5-hitl-approval.md（完成后再看）
	_, _, _, _, _ = ctx, reader, svc, orch, store
	log.Fatal("TODO(练习5): 演示主流程未实现")
}
