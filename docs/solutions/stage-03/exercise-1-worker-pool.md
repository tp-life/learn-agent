# 练习 1 参考答案：errgroup + semaphore 的 worker pool

> 对应 TODO：`stage-03-multi-agent/internal/pool/pool.go` 的 `TODO(练习1)`。
> **完成练习并自评后再看本文档。**
> 本文档基础实现代码已于 2026-08-14 实际粘贴进项目验证：`go vet ./internal/pool/` 与
> `go test ./internal/pool/ -race -count=1` 全部通过（4 个测试，`-race` 无数据竞争）。
> 进阶实现（RunStream 流式结果收集，见第三节）同日验证：临时粘贴进项目后 6 个测试全绿（基础 4 个 + 进阶 2 个），
> 验证后即删除，项目保持骨架版。
> 注意：参考实现依赖 `golang.org/x/sync`（errgroup）。若 `go.mod` 中还没有，
> 先执行 `cd stage-03-multi-agent && go get golang.org/x/sync`。

---

## 一、参考实现

### `internal/pool/pool.go`（只给出需要实现的 Run 方法及 import；骨架其余部分不变）

import 从骨架的 2 个标准库包扩为：

```go
import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"
)
```

```go
// Run 并行执行 jobs，返回与 jobs 顺序一一对应的结果切片（results[i] 对应 jobs[i]）。
//
// 契约：
//  1. 任意时刻并发执行的 job 数不超过 maxConcurrent（errgroup.SetLimit 充当 semaphore）；
//  2. 每个 job 用 context.WithTimeout(ctx, jobTimeout) 派生自己的超时预算；
//  3. 部分失败：单个 job 失败/超时收进 Result.Err，不影响其他 job；
//  4. ctx 被取消时尽快退出（级联取消 + 排队 job 开工前检查）。
func (p *Pool) Run(ctx context.Context, jobs []Job) []Result {
	// 预分配结果切片：goroutine 只写自己下标的槽位，disjoint 写入无数据竞争，
	// 且顺序天然与 jobs 一致——不需要 channel 收集、不需要事后排序。
	results := make([]Result, len(jobs))

	// 注意：这里用的是裸 errgroup.Group 而不是 errgroup.WithContext。
	// WithContext 派生的 ctx 会在任一 goroutine 返回 error 时被取消——"一错全停"，
	// 与本包要的"部分失败"语义相反。所以每个 goroutine 把 error 收进 results[i].Err
	// 之后必须 return nil，绝不能让 error 逃出 goroutine（阶段文档注意事项第 1 条）。
	g := new(errgroup.Group)
	g.SetLimit(p.maxConcurrent) // SetLimit 即 semaphore：超过上限的 Go 调用在此阻塞排队

	for i := range jobs {
		job := jobs[i] // Go 1.22+ 循环变量每轮新实例，本行是防御性习惯（也是给读者看的显式拷贝）
		g.Go(func() error {
			// 契约 4：排队等到 slot 时父 ctx 可能已取消，开工前先检查，避免无效执行。
			// （SetLimit 的阻塞不感知 ctx，靠这一行兜底"尽快退出"。）
			if err := ctx.Err(); err != nil {
				results[i] = Result{ID: job.ID, Err: err}
				return nil
			}

			// 契约 2：预算分层——在父 ctx 之下派生单 job 超时。
			// defer cancel() 必须写：不 cancel 的 WithTimeout 会让定时器挂到超时才释放（context 泄漏）。
			jctx, cancel := context.WithTimeout(ctx, p.jobTimeout)
			defer cancel()

			v, err := job.Run(jctx)
			results[i] = Result{ID: job.ID, Value: v, Err: err}
			return nil // 契约 3：error 已收进结果，不能返回给 errgroup（否则触发一错全停）
		})
	}

	// Wait 等所有 goroutine 结束；因为 goroutine 永远 return nil，返回值必为 nil，忽略之。
	// Wait 返回与 results 的写入之间有 WaitGroup 的 happens-before 保证，主 goroutine 读 results 安全。
	_ = g.Wait()
	return results
}
```

### `internal/pool/pool_test.go`（新建，全部假 job，无需网络/LLM）

四个测试：并发上限（atomic 计数器）、单 job 超时、部分失败、父 ctx 取消。

```go
package pool

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// 全部用假 job（sleep + ctx 监听），不依赖网络/LLM。

// TestRun_RespectsConcurrencyLimit 用 atomic 计数器验证并发上限：
// 每个 job 开工时 inFlight+1 并刷新历史峰值，结束时 -1。
// 若峰值超过 maxConcurrent，说明限流失效。
func TestRun_RespectsConcurrencyLimit(t *testing.T) {
	const limit = 3
	p := New(limit, time.Second)

	var inFlight, peak atomic.Int32
	jobs := make([]Job, 10)
	for i := range jobs {
		i := i
		jobs[i] = Job{
			ID: fmt.Sprintf("job-%d", i),
			Run: func(ctx context.Context) (string, error) {
				cur := inFlight.Add(1)
				defer inFlight.Add(-1)
				// CAS 循环刷新峰值（atomic 没有 Max 操作）
				for {
					m := peak.Load()
					if cur <= m || peak.CompareAndSwap(m, cur) {
						break
					}
				}
				select {
				case <-time.After(20 * time.Millisecond):
					return "ok", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			},
		}
	}

	results := p.Run(context.Background(), jobs)
	if len(results) != len(jobs) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(jobs))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d].Err = %v, want nil", i, r.Err)
		}
		if r.ID != jobs[i].ID {
			t.Errorf("results[%d].ID = %q, want %q（顺序应与 jobs 一致）", i, r.ID, jobs[i].ID)
		}
	}
	if got := peak.Load(); got > limit {
		t.Errorf("并发峰值 = %d, 超过上限 %d", got, limit)
	}
	if got := peak.Load(); got < 2 {
		t.Errorf("并发峰值 = %d，疑似没有真正并行（测试失效）", got)
	}
}

// TestRun_JobTimeout 验证单 job 超时：jobTimeout 内不完成的 job 收到 DeadlineExceeded，
// 且超时只影响自己，快 job 照常成功。
func TestRun_JobTimeout(t *testing.T) {
	p := New(2, 50*time.Millisecond)

	jobs := []Job{
		{ID: "slow", Run: func(ctx context.Context) (string, error) {
			select {
			case <-time.After(5 * time.Second): // 远超 jobTimeout
				return "done", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}},
		{ID: "fast", Run: func(ctx context.Context) (string, error) {
			return "quick", nil
		}},
	}

	start := time.Now()
	results := p.Run(context.Background(), jobs)
	elapsed := time.Since(start)

	if !errors.Is(results[0].Err, context.DeadlineExceeded) {
		t.Errorf("slow job Err = %v, want context.DeadlineExceeded", results[0].Err)
	}
	if results[1].Err != nil || results[1].Value != "quick" {
		t.Errorf("fast job = %+v, want {Value: quick}", results[1])
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run 耗时 %v，超时取消没有生效（应该 ~50ms 就返回）", elapsed)
	}
}

// TestRun_PartialFailure 验证部分失败语义：中间的 job 失败，
// 不影响其他 job 成功，且结果顺序与 jobs 一致。
func TestRun_PartialFailure(t *testing.T) {
	p := New(3, time.Second)
	boom := errors.New("boom")

	jobs := []Job{
		{ID: "a", Run: func(ctx context.Context) (string, error) { return "va", nil }},
		{ID: "b", Run: func(ctx context.Context) (string, error) { return "", boom }},
		{ID: "c", Run: func(ctx context.Context) (string, error) {
			// 故意慢一点：确保 b 先失败，c 仍能成功跑完（没有被"一错全停"误杀）
			time.Sleep(30 * time.Millisecond)
			return "vc", nil
		}},
	}

	results := p.Run(context.Background(), jobs)
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	if results[0].Value != "va" || results[0].Err != nil {
		t.Errorf("results[0] = %+v, want {ID:a Value:va}", results[0])
	}
	if !errors.Is(results[1].Err, boom) {
		t.Errorf("results[1].Err = %v, want boom", results[1].Err)
	}
	if results[2].Value != "vc" || results[2].Err != nil {
		t.Errorf("results[2] = %+v, want {ID:c Value:vc}（b 的失败不应影响 c）", results[2])
	}
}

// TestRun_ContextCancelled 验证父 ctx 取消时尽快退出：
// 慢 job 被取消，排队中的 job 不应再实际执行。
func TestRun_ContextCancelled(t *testing.T) {
	p := New(1, 5*time.Second) // 上限 1：job-0 在跑时 job-1..2 排队

	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32
	jobs := make([]Job, 3)
	for i := range jobs {
		i := i
		jobs[i] = Job{
			ID: fmt.Sprintf("job-%d", i),
			Run: func(ctx context.Context) (string, error) {
				started.Add(1)
				select {
				case <-time.After(5 * time.Second):
					return "done", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			},
		}
	}

	done := make(chan []Result, 1)
	go func() { done <- p.Run(ctx, jobs) }()
	time.Sleep(50 * time.Millisecond) // 等 job-0 跑起来
	cancel()

	select {
	case results := <-done:
		if len(results) != 3 {
			t.Fatalf("len(results) = %d, want 3（取消也要给每个 job 一个结局）", len(results))
		}
		if !errors.Is(results[0].Err, context.Canceled) {
			t.Errorf("results[0].Err = %v, want context.Canceled", results[0].Err)
		}
		if got := started.Load(); got != 1 {
			t.Errorf("实际开工的 job 数 = %d, want 1（排队 job 不应再执行）", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 2s 内返回，ctx 取消没有生效")
	}
}
```

## 二、关键设计点

1. **为什么不用 `errgroup.WithContext` 的取消语义**：`WithContext` 派生的 ctx 在任一 goroutine 返回 error 时被取消，其余 job 全部中止——这是"一错全停"，对强关联任务（如 MapReduce 的分片）是对的，但编排场景里"三个调研子任务挂了一个，另外两个的结论仍要进汇总报告"。所以这里用裸 `errgroup.Group`（不用它派生 ctx），每个 goroutine 把 error 收进 `results[i].Err` 后 **`return nil`**——errgroup 在整个实现里只干两件事：并发上限（SetLimit）和等待全部完成（Wait），错误传递通道被故意弃用。**易错处**：写成 `return err` 或直接用 `g, ctx := errgroup.WithContext(ctx)` 再把派生 ctx 传给 job，都会悄悄退化成一错全停，且单测里如果失败 job 排在最后一个还可能测不出来——本答案的 `TestRun_PartialFailure` 故意让失败 job 在中间、慢 job 在末尾来抓这个回归。

2. **为什么结果写预分配槽位而不是 channel 收集**：`results := make([]Result, len(jobs))`，第 i 个 goroutine 只写 `results[i]`。Go 内存模型下，slice 的不同元素是不同变量，disjoint 下标写入无数据竞争（`-race` 验证通过）；`g.Wait()` 与 goroutine 退出之间有 WaitGroup 的 happens-before 保证，主 goroutine 之后读整个切片是安全的。对比 channel 收集：channel 方案结果**无序**（需要再排序或带下标回传）、要处理 close 时机、要多一个 fan-in 的接收循环。这里调用方（编排器）本来就要"全部完成后再汇总"，顺序对齐 jobs 还省一次归位，预分配槽位是严格更优解。channel 收集的真正主场是"边完成边消费"，见第三节进阶实现。**易错处**：`results = append(results, ...)` 在 goroutine 里是经典数据竞争——append 共享 slice header，`-race` 必抓。

3. **context 预算分层的意义**：`jctx, cancel := context.WithTimeout(ctx, p.jobTimeout)` 在父 ctx（任务级预算，来自编排器）之下派生 job 级预算。分层让超时**自下而上级联**：单 job 超时先触发、错误以 `context.DeadlineExceeded` 收进该 job 的 Result，其余 job 和上层编排器照常运行、还有时间记 checkpoint；若全局只透传一个死线，到点所有层一起死，连"哪个 job 超时了"都留不下（阶段文档 3.1 Q8）。**易错处**：`defer cancel()` 漏写会让每个 job 的定时器挂到超时才释放，批量任务下是实打实的资源泄漏（`go vet` 的 lostcancel 检查能抓到一部分）。

4. **`SetLimit` 的阻塞不感知 ctx**：`g.Go` 在并发满员时阻塞排队，这个阻塞**不会**因 ctx 取消而返回。所以 goroutine 拿到 slot 后第一件事是 `if err := ctx.Err(); err != nil`——否则父 ctx 取消后，排队的 job 仍会逐个"开工"，虽然派生 ctx 已取消会让它们快速返回，但终究是多走一遍；更严重的是如果 job.Run 不检查 ctx（第三方代码），就会真执行。这一行是契约 4"尽快退出"的兜底。

5. **测试为什么用 atomic 计数器而不是时间断言**：并发上限最直接的证据是"同时在跑的 job 数"，用 `inFlight.Add(1)` + CAS 刷新峰值即可精确断言；靠"总耗时 ≈ ceil(n/limit) × 单 job 耗时"的时间推算是脆弱的（调度抖动）。但反向 sanity check 仍要留一个（峰值 < 2 报"疑似没有并行"），否则一个串行实现的 pool 也能通过上限测试——**测试要同时证真和证伪**。假 job 的"慢"用 `select { time.After / ctx.Done() }` 而不是裸 `time.Sleep`，否则超时用例里慢 job 不会响应取消，Run 会被拖满 5 秒。

## 三、进阶实现（加分项：RunStream 流式结果收集）

> 回补记录：本节代码于 2026-08-14 以临时文件（`internal/pool/stream.go` + `stream_test.go`）实际粘贴进项目验证，
> `go vet ./internal/pool/` 与 `go test ./internal/pool/ -race -count=1` 全部通过（6 个测试：基础 4 个 + 进阶 2 个），
> 验证后已从项目删除——**进阶实现只属于答案，不进项目代码树**，项目保持骨架版。

### 设计取舍（先说清楚再看代码）

- **什么时候需要流式版**：`Run` 是"全部完成才返回"，编排器汇总场景够用；但 SSE 看板要实时推进度（"子任务 3/8 完成"），等最慢的 job 出来再一次性刷新体验太差，这时要边完成边拿结果。
- **代价 1：结果无序**。channel 送出的是完成序，不是 jobs 序，调用方必须按 `Result.ID` 归位（所以 Job.ID 从"便于调试"升级为"正确性依赖"）。
- **代价 2：errgroup 用不上了**。errgroup 的模型是"Go + Wait 一把梭"，没有"逐个产出"的口子；流式版退回 semaphore channel + WaitGroup 手工编排——errgroup 帮你做的三件事（错误收集、ctx 联动、等待）里有两件要自己写。这正好印证阶段文档 3.1 Q7：固定批量等全部 → errgroup；流水线/流式 → channel。
- **关键正确性点**：`out` 用 `len(jobs)` 容量带缓冲——worker 发送永不阻塞，调用方中途放弃接收（比如自己 ctx 取消提前 return）也不会泄漏 goroutine；`close(out)` 由专门 goroutine 在 `wg.Wait()` 后执行，这是"谁负责关 channel"的标准答案：由等待所有发送者退出的那一方关，任何单个 worker 都没资格关。

### `internal/pool/stream.go`（进阶实现完整代码）

```go
package pool

import (
	"context"
	"sync"
)

// RunStream 是 Run 的流式版本：结果边完成边从返回的 channel 送出，
// 全部结束后 channel 关闭。
//
// 与 Run 的取舍：
//   - 优点：先完成的先拿到，调用方（如 SSE 看板）可以实时推进度，不用等最慢的 job；
//   - 代价 1：结果无序，需要按 Job.ID 自行归位（调用方要有 ID→下标的映射）；
//   - 代价 2：并发原语从 errgroup 换回 semaphore channel + WaitGroup，取消传播、
//     close 时机都要自己管（errgroup 帮做的三件事这里全裸写一遍）。
//
// 实现要点：
//   - out 用 len(jobs) 容量带缓冲：worker 发送永不阻塞，即使调用方中途放弃接收
//     也不会泄漏 goroutine；
//   - close(out) 由专门的 goroutine 在 wg.Wait() 后做——这是"谁负责关 channel"
//     的标准答案：由最后一个发送者（计数等待）关，而不是某个 worker 关；
//   - 获取 slot 的 select 同时监听 ctx.Done()：父取消时排队的 job 直接以
//     context.Canceled 作为结局送出，不再实际执行（与 Run 的契约 4 对齐）。
func (p *Pool) RunStream(ctx context.Context, jobs []Job) <-chan Result {
	out := make(chan Result, len(jobs))
	sem := make(chan struct{}, p.maxConcurrent)
	var wg sync.WaitGroup

	for i := range jobs {
		job := jobs[i]
		wg.Add(1)
		go func() {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				out <- Result{ID: job.ID, Err: ctx.Err()}
				return
			}

			jctx, cancel := context.WithTimeout(ctx, p.jobTimeout)
			defer cancel()

			v, err := job.Run(jctx)
			out <- Result{ID: job.ID, Value: v, Err: err}
		}()
	}

	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
```

### `internal/pool/stream_test.go`（进阶测试完整代码）

两个测试要点：快结果先到达且 channel 最终关闭、限流在流式版同样生效。

```go
package pool

import (
	"context"
	"testing"
	"time"
)

// TestRunStream_EarlyResultFirst 验证流式语义：快 job 的结果先于慢 job 到达，
// 且所有结果最终收齐、channel 被关闭。
func TestRunStream_EarlyResultFirst(t *testing.T) {
	p := New(2, time.Second)
	jobs := []Job{
		{ID: "slow", Run: func(ctx context.Context) (string, error) {
			time.Sleep(100 * time.Millisecond)
			return "vslow", nil
		}},
		{ID: "fast", Run: func(ctx context.Context) (string, error) {
			return "vfast", nil
		}},
	}

	ch := p.RunStream(context.Background(), jobs)

	first := <-ch
	if first.ID != "fast" {
		t.Errorf("第一个到达的结果 = %q, want fast（流式应先出快结果）", first.ID)
	}

	got := map[string]Result{first.ID: first}
	for r := range ch { // range 到 close 为止，顺带验证 close 发生（不死等）
		got[r.ID] = r
	}
	if len(got) != 2 {
		t.Fatalf("收到 %d 个结果, want 2", len(got))
	}
	if got["slow"].Value != "vslow" || got["slow"].Err != nil {
		t.Errorf("slow = %+v, want {Value:vslow}", got["slow"])
	}
}

// TestRunStream_ConcurrencyLimit 验证流式版同样受限流约束。
func TestRunStream_ConcurrencyLimit(t *testing.T) {
	p := New(1, time.Second) // 上限 1：必须串行
	jobs := []Job{
		{ID: "a", Run: func(ctx context.Context) (string, error) {
			time.Sleep(30 * time.Millisecond)
			return "va", nil
		}},
		{ID: "b", Run: func(ctx context.Context) (string, error) { return "vb", nil }},
	}

	start := time.Now()
	n := 0
	for range p.RunStream(context.Background(), jobs) {
		n++
	}
	if n != 2 {
		t.Fatalf("收到 %d 个结果, want 2", n)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("总耗时 %v < 30ms，疑似并发执行（上限 1 应串行）", elapsed)
	}
}
```

### 进阶实现的易错处

1. **close 时机**：让"最后一个完成的 worker"关 channel 需要原子计数判断，容易写成 `if wg 计数==0 { close }` 的竞态；标准解法就是答案里的"专门 goroutine `wg.Wait(); close(out)`"。
2. **无缓冲 out 的泄漏**：`out` 若不带缓冲，调用方提前 return 后所有在跑 worker 阻塞在发送上，goroutine 泄漏。缓冲容量给足 `len(jobs)` 是最省心的解法（代价是 O(n) 内存，结果本来就要这么多）。
3. **select 抢 slot 不监听 ctx**：`sem <- struct{}{}` 裸写在父取消时会一直排队等 slot；select 同时监听 `ctx.Done()` 才能"尽快退出"。
4. **误以为流式能替代批量**：编排器汇总场景用 RunStream 反而要自己做归位和"收齐判断"——两个 API 各有主场，别因为一个"更高级"就全换成它。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [x] 并发上限生效：任意时刻在跑的 job 数 ≤ maxConcurrent（用 atomic 峰值计数器验证，不是时间推算）
- [x] 每个 job 用 `context.WithTimeout(ctx, jobTimeout)` 派生 ctx，且 `defer cancel()` 没有漏写
- [x] 部分失败语义：单个 job 的 error 收进 `Result.Err`，**goroutine 对 errgroup 永远 `return nil`**——没有把 error 交给 errgroup（否则会退化成一错全停）
- [x] 结果写预分配槽位 `results[i]`（无 append 数据竞争），`results[i]` 对应 `jobs[i]`，`len(results) == len(jobs)`
- [x] 父 ctx 取消时：在跑 job 级联取消、排队 job 不再实际执行（有开工前的 `ctx.Err()` 检查）
- [x] 测试覆盖并发上限 / 超时 / 部分失败三类，假 job 用 `select ctx.Done()` 响应取消而非裸 sleep
- [x] `go vet ./internal/pool/` 和 `go test ./internal/pool/ -race -count=1` 全绿
- [x] 能口头回答：为什么不用 `errgroup.WithContext`？为什么结果写槽位而不是 channel 收集？context 预算为什么要分层？

加分项（做了才需要勾，参考"进阶实现"一节）：

- [x] RunStream 边完成边出结果：`out` 带 `len(jobs)` 缓冲防泄漏，`wg.Wait()` 后由专门 goroutine close
- [x] 能口头回答：什么场景该用 RunStream 而不是 Run？close channel 的权责为什么归"等待方"？
