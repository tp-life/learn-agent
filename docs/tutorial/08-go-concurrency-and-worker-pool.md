# 第 8 章：Go 并发编排与 worker pool——多 Agent 系统的发动机

> 对应阶段：阶段三（深入）· 项目 3 `stage-03-multi-agent/`
> 代码位置：`stage-03-multi-agent/internal/pool/`（本章精讲；这是**练习骨架**，完整实现由你在练习 1 完成）
> 前置：第 1-3 章（mini-agent 内核：LLM 客户端、ReAct 循环、重试退避）；预习材料 `docs/multi-agent-orchestration-guide.md` 第三节
> 学完后你能讲清：为什么多 Agent 编排首先是并发工程、errgroup 与手动 channel 编排怎么选、超时预算怎么逐层分配、429 的三道防线——并亲手写出一个并发受限、单任务带超时、部分失败不拖垮整批的 worker pool。

---

## 本章地图

- 多 Agent 编排 = 一组并发 LLM 调用：为什么"后端用 Go 而不是全 TS"
- goroutine / channel / WaitGroup 最小速查 + 手动编排的三个易漏点
- errgroup 三件套：Go / Wait / WithContext / SetLimit
- 两种错误语义：一错全停 vs 部分失败（`Result.Err` 字段模式）
- context 取消传播与超时预算分配（10min → 2min → 3min → 60s）
- semaphore 限流与 429 三道防线
- 代码精讲：pool 骨架的 API 设计逐类型过
- 进阶（都可运行）：部分失败 pool 教学版、预算分配演示、带 jitter 的退避、goroutine 泄漏排查
- 面试六连问 + 常见坑 + 练习 1

---

## 一、概念详解

### 1.1 为什么多 Agent 编排首先是并发工程

第 10 章你会看到 planner-worker 编排的完整样貌：planner 把"调研三个竞品"拆成三个子任务，多个 worker 并行执行，结果汇总给 critic/汇总节点。拆开表象看本质：**一个 worker 子任务 ≈ 一串 LLM API 调用 + 工具调用**，并行跑 N 个 worker 就是同时维护 N 条这样的调用链。

所以多 Agent 系统的发动机不是某个神秘的"编排框架"，而是四个并发问题：

- **并行分发**：N 个子任务同时起跑（goroutine）；
- **等待与汇总**：全部跑完再汇总（WaitGroup / `errgroup.Wait`）；
- **限流**：同时开工数 ≤ API 配额（semaphore / `SetLimit`）；
- **生命周期**：哪一层慢了、失败了、被用户取消了，要有明确的停止机制（context）。

这也正是阶段三"后端用 Go 而不是全 TypeScript"这道面试题的答案（阶段文档〇节）：TS 的 `Promise.all` 能表达并行，但"并发度受限 + 逐级超时预算 + 一错全停/部分失败语义"这些编排语义，在 Go 里是 errgroup + context + semaphore 三个一等公民原语的直接组合，几十行就能写出一个生产可用的 worker pool；在 TS 里你得自己拼任务队列、AbortController 级联和计数器。本章把这套发动机讲透，第 10 章的编排器就只是它的上层组装。

### 1.2 goroutine / channel / WaitGroup 最小速查

速查级回顾，已熟的可以跳到 1.3。

**goroutine**：`go f()` 启动一个并发执行的函数。代价极小（KB 级栈起步），开几万个不心疼——要心疼的是它们打的下游 API。

**WaitGroup**：等"一组" goroutine 结束。

```go
var wg sync.WaitGroup
for _, task := range tasks {
	wg.Add(1)
	go func() {
		defer wg.Done()
		do(task)
	}()
}
wg.Wait() // 等所有 worker 结束
```

**channel**：goroutine 之间传结果的管道。

```go
results := make(chan Result, len(tasks)) // 带缓冲：worker 发送不阻塞
for _, task := range tasks {
	go func() { results <- do(task) }()
}
for range tasks {
	r := <-results // 收够 N 个，主 goroutine 自然继续
	merge(r)
}
```

手动编排（channel + WaitGroup 全自己写）有三个经典易漏点，每一个都对应一类生产事故：

1. **忘 close channel → 死锁**。`for r := range ch` 的接收方靠 close 才知道"没有了"；发送方忘了关，接收方 range 永不退出。全员死锁时 runtime 会报 `fatal error: all goroutines are asleep - deadlock!`，但只吊死一部分时连这个报错都没有，只是悄悄卡住。
2. **错误没人收 → 静默失败**。goroutine 里的 `err` 是一闪而过的局部变量，不塞进 channel 或共享结构它就永远消失——任务看起来"成功"了，只是结果不对。
3. **取消不传播 → goroutine 泄漏**。任务已超时，worker 还在跑；ctx 没传下去，worker 里的 HTTP 调用不知道该停，goroutine 越攒越多（进阶 3.4 专门讲排查）。

记住这三点，errgroup 的价值就不用背了：**它就是把这三个易漏点一次性包掉的 WaitGroup**。

### 1.3 errgroup 三件套

`golang.org/x/sync/errgroup`（已在项目依赖里，`stage-03-multi-agent/go.mod:18`）= WaitGroup + 错误收集 + context 联动。核心 API 就四个：

```go
g, ctx := errgroup.WithContext(parent) // 任一 goroutine 出错 → ctx 取消，其余止损
g.SetLimit(5)                          // 同时最多 5 个在跑，等价于内建 semaphore
for _, sub := range subs {
	g.Go(func() error {
		_, err := workerRun(ctx, sub) // 第一个非 nil error 由 Wait 返回
		return err
	})
}
if err := g.Wait(); err != nil { /* 第一个错误 */ }
```

- `Go(fn)`：内建 Add/Done，不可能忘调 `Done`；
- `Wait()`：等全部结束，返回**第一个**非 nil 错误（后续错误被丢弃——要全量收集得自己用 slice + mutex，或 `errors.Join`）；
- `WithContext(parent)`：派生的 ctx 在第一个错误出现时被取消——这是"一错全停"的开关；
- `SetLimit(n)`：超过 n 个在跑时，`Go` 调用本身阻塞排队，反压回分发循环。

**与手动 channel + WaitGroup 的取舍**（面试高频，第四节展开）：

- 任务列表一开始就确定、等全部完成再汇总 → errgroup，模板就这么几行；
- 任务边产生边消费（planner 边想边派）、结果要边完成边处理（SSE 实时进度）、拓扑动态 → channel 版 fan-in/fan-out（预习指南第三节第 5 条），灵活，但上面三个易漏点全要自己扛。

### 1.4 两种错误语义：一错全停 vs 部分失败

errgroup 默认给的语义是**一错全停**：任一 goroutine 返回 error → ctx 取消 → 其余 worker 被中止 → `Wait` 返回第一个错误。这对"缺一不可"的强关联任务是对的：MapReduce 分片缺一片结果就不完整，凑单接口缺一个库存就别下单。

但编排场景经常要的是**部分失败**：三个调研子任务挂了一个，另外两个的结论仍然有价值，要进汇总报告（阶段文档注意事项第 1 条）。这时不能让 error 逃出 goroutine，要让 worker 自己吃掉错误、把失败作为**结果值**返回：

```go
results := make([]Result, len(subs))
var g errgroup.Group
for i, sub := range subs {
	g.Go(func() error {
		v, err := workerRun(ctx, sub)
		results[i] = Result{Value: v, Err: err} // 失败作为结果值收进切片
		return nil                              // 关键：error 不交给 errgroup
	})
}
_ = g.Wait()
```

这就是 **`Result.Err` 字段模式**：失败从"控制流信号"降级为"数据"，由上层（编排器）统计成败比例、决定降级还是重试。两个细节：

- 结果写**预分配切片的对应下标**而不是 append——各 goroutine 写不同下标是 disjoint 写入，无数据竞争；append 共享 slice header 是经典 race（`-race` 必抓），还顺便丢了与输入的顺序对应；
- goroutine 对 errgroup **永远 `return nil`**——error 一旦返回给它，`WithContext` 的取消就会触发，语义悄悄退化成一错全停。

两种语义没有对错，**选错才出问题**：用一错全停跑调研任务，一个 403 废掉整批；用部分失败跑缺一不可的任务，汇总层忘了检查 `Err` 就会拿残缺结果当完整结果交给用户。

### 1.5 context 取消传播与超时预算分配

context 是 Go 跨 API 边界传递"死线与取消信号"的标准机制。两条铁律：

- **父取消 → 所有子孙级联取消**（errgroup 的一错全停就是靠它止损）；
- **goroutine 必须 `select ctx.Done()` 才能真正停下**——ctx 取消只是"发了信号"，阻塞在一个不监听 `Done` 的调用上的 goroutine 纹丝不动。所以"把 ctx 一路传到最底层的 HTTP 调用"是硬要求。

多级 Agent 调用里，超时不是"设一个值"而是**预算分配**：

```go
ctx, cancel := context.WithTimeout(parent, 10*time.Minute) // 任务总预算
defer cancel()

planCtx, planCancel := context.WithTimeout(ctx, 2*time.Minute)      // planner：开销不是产出
defer planCancel()
workerCtx, workerCancel := context.WithTimeout(ctx, 3*time.Minute)  // worker：从任务层派生
defer workerCancel()
callCtx, callCancel := context.WithTimeout(workerCtx, 60*time.Second) // 单次 LLM 调用
defer callCancel()
```

原则是**越下层预算越小，每层给上层留余量**（阶段文档 3.1 Q8）：单次 LLM 调用 60s 先超时，worker 层还剩两分钟做善后（记 checkpoint、走降级、换模型重试）；worker 三分钟超时，任务层还有余量记录"哪个子任务死了、为什么"。

反面教材是**全局一个死线透传到底**：10 分钟到点，所有层同一瞬间超时，planner、worker、HTTP 调用一起死，善后代码一行都跑不到——故障现场什么状态都没留下，第 9 章的崩溃恢复也无从谈起。这也是 `mini-agent` 的 HTTP 客户端显式设 120s 超时（`mini-agent/internal/llm/client.go:30`）的原因：那是预算分层最底一层的兜底。

另外两个执行细节：

- `defer cancel()` 一个都不能漏：漏了不泄漏 goroutine，但定时器资源要挂到超时点才释放，批量任务下是实打实的泄漏（`go vet` 的 lostcancel 检查能抓一部分）；
- 善后操作（写 checkpoint）不能再用已取消的 ctx，要用 `context.WithoutCancel` 或新起一个带短超时的 background ctx——否则存档本身也被取消了（预习指南第五节第 5 条，非常常见的坑）。

### 1.6 semaphore 限流与 429 三道防线

DeepSeek 这类 API 有 QPS/并发配额，100 个子任务同时打过去就是一片 429。semaphore（计数信号量）把"同时开工数"钉在上限内。Go 里两种常见实现：

```go
sem := make(chan struct{}, 5) // 容量 = 并发上限，对齐 API 配额
sem <- struct{}{}             // Acquire：满员则在此排队
defer func() { <-sem }()      // Release
```

另一种是 `golang.org/x/sync/semaphore` 的 `Weighted`（`Acquire(ctx, 1)` 等待时可响应取消）；errgroup 的 `SetLimit` 本质也是同款语义（与 semaphore 的细微差别见面试 Q6）。三者等价，选型看场景。

但要明确：**semaphore 只是第一道防线**。并发场景下 429 是常态不是异常，完整答案是三道防线（阶段文档 3.1 Q11）：

1. **源头限并发**：worker 并发数 ≤ API 配额能承受的量。限流要限在源头，比事后靠重试退避便宜得多；
2. **单次调用指数退避**：复用第 3 章的 `ChatWithRetry`（`mini-agent/internal/llm/client.go:238`）：429/5xx 才重试、尊重 `Retry-After`、**加 jitter 防惊群**——多个 worker 同时被限流时退避时间必须随机错开，否则"一起重试、一起再被限"（进阶 3.3 给实现）；
3. **持续限流走降级/排队**：换备用模型，或把子任务重新入队延后执行；最后兜底是进死信标记 failed，不拖垮整个任务（第 10 章编排器落地）。

架构上要把"被限流"当**预期路径**设计，而不是异常路径——这是并发 LLM 系统与普通后端最大的心智差异之一。

---

## 二、代码精讲

项目 3 的 pool 包是**练习骨架**：类型与构造函数完整，`Pool.Run` 留给你实现（`TODO(练习1)`）。本节讲清每个类型为什么这样设计——看懂设计意图，练习就是水到渠成。

### 2.1 包注释：设计决策都写在这里（`pool.go:1`）

`stage-03-multi-agent/internal/pool/pool.go:1` 的包注释本身就是一份小型架构文档，讲了三件事：

- **它在链路中的位置**：planner 分解 →【pool 并行执行 worker 子任务】→ 汇总/评审。pool 不产出智能，它是编排器（练习 3）的并发执行引擎；
- **为什么并发度必须受限**：worker 子任务本质是 LLM API 调用，撞 rate limit 的代价远大于排队；
- **为什么选部分失败语义**：明确点名放弃了 `errgroup.WithContext` 的一错全停——1.4 节的落地。

读开源项目先读包注释。本项目按 AGENTS.md 约定，包注释都写到"为什么"这一层，值得逐字读。

### 2.2 Job 与 Result：任务与结局（`pool.go:34`、`pool.go:46`）

```go
type Job struct {
	ID string
	Run func(ctx context.Context) (string, error)
}
```

- `Run` 接收 ctx（`pool.go:39`）：worker 的执行体是 LLM/工具调用，必须能被取消。ctx 由 pool 派生好（带超时预算）传进来——1.5 节预算分层在 API 上的体现；Job 内部所有阻塞操作都要 `select ctx.Done()`，这是对 Job 实现者的契约。
- 返回值是 `(string, error)` 而非泛型：编排层的子任务产出统一是文本结论（LLM 生成内容），保持简单，不为想象中的需求提前抽象。
- `ID`（`pool.go:36`）：由编排器分配（如 `"subtask-3"`），用于把结果对应回子任务、写 checkpoint、trace 归因。现在它只是一个字符串，到第 9、12 章它就是恢复与观测的锚点。

```go
type Result struct {
	ID    string
	Value string
	Err   error
}
```

`Result.Err`（`pool.go:49`）是 1.4 节"失败作为结果值"的落地。注意 **`Pool.Run` 的签名里没有 error**——"整批任务"不存在失败一说，失败永远属于单个 job。这个签名本身就是对错误语义的声明。

### 2.3 Pool 与 New：两个旋钮（`pool.go:53`、`pool.go:65`）

```go
type Pool struct {
	maxConcurrent int           // 同时执行的 job 数上限（semaphore 容量）
	jobTimeout    time.Duration // 每个 job 的超时预算
}
```

两个字段正好对应本章两大主题：`maxConcurrent`（`pool.go:55`）是 1.6 节"限流限在源头"的旋钮，注释里写了配置原则——**按 LLM 服务商的 rate limit 配额定，宁小勿大，429 的代价高于排队**；`jobTimeout`（`pool.go:58`）是 1.5 节预算分层的一环：**任务总预算（上层 ctx）> 单 worker 预算（本字段）> 单次 LLM 调用预算（更下层）**。

零值不可用、必须 `New` 构造（`pool.go:65`）：`maxConcurrent` 为 0 的 pool 一个 job 都跑不了，零值可用在这里没有意义。

### 2.4 TODO（练习1）：你要实现的 Run（`pool.go:69`）

骨架把契约写得很明确（`pool.go:69` 的 TODO 块），四条：

1. 任意时刻并发执行的 job 数不超过 `maxConcurrent`（semaphore 限流）；
2. 每个 job 用 `context.WithTimeout(ctx, p.jobTimeout)` 派生自己的 ctx 再调用 `job.Run`，`defer cancel()` 防泄漏；
3. 部分失败：单个 job 失败/超时收进对应 `Result.Err`，不影响其他 job；`results[i]` 对应 `jobs[i]`，`len(results) == len(jobs)`；
4. ctx 被外部取消时尽快退出：已在跑的靠派生 ctx 级联取消，排队中的不应再实际执行。

提示给出的思路（`pool.go:83` 起）为什么是对的，对照概念节就能看懂：

- **用 errgroup + `SetLimit` 最简**：`SetLimit` 就是 1.3 节的内建 semaphore；也可以用 `semaphore.Weighted` 自己 Acquire/Release + `sync.WaitGroup` 手写一遍，体会两种写法的差别（面试手写题）；
- **结果写预分配槽位 `results[i]` 而不是 channel 收集**：1.4 节讲过——disjoint 写入无竞争、顺序天然与 jobs 一致、不需要额外的 fan-in goroutine。调用方（编排器）本来就要"全部完成再汇总"，channel 的"边完成边消费"优势用不上（真要流式是进阶题，见参考答案第三节）；
- **绝不能让 error 逃出 goroutine**：1.4 节——收进 `results[i].Err` 后 `return nil`，否则 errgroup 退化成一错全停；
- **排队 job 开工前检查 `ctx.Err()`**：`SetLimit` 的阻塞**不感知 ctx**，父取消后排队的 job 仍会逐个拿到 slot——开工前先检查是契约 4 的兜底。这条不在概念节的明面上，做练习时最容易漏。

验收：`go test ./internal/pool/` 通过（并发上限、超时、部分失败三个测试）。完整要求见第六节。

---

## 三、进阶拓展（带代码）

以下四段教学代码都可直接 `go run` 跑通（3.1 需要先 `go get golang.org/x/sync`）。它们是**模式级教学实现**：讲清思路，让你做练习 1 时知道自己在写什么——不是参考答案，刻意简化掉了部分契约（`jobTimeout`、契约 4），照抄进 `internal/pool` 过不了验收。

### 3.1 最小"部分失败语义" worker pool

**为什么写它**：1.4 节讲了两种错误语义，这里把"部分失败"的三要素落成一段能跑的代码——`SetLimit` 限并发、失败作为结果值（`Outcome.Err`）、预分配槽位写入（无竞争、保顺序）。

```go
// 教学示例：最小"部分失败语义" worker pool（可直接 go run；需 go get golang.org/x/sync）。
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
)

type task struct {
	name string
	fn   func(ctx context.Context) (string, error)
}

type outcome struct {
	name  string
	value string
	err   error
}

// runAll 并行执行 tasks，并发上限 limit，单任务失败不影响其他任务。
func runAll(ctx context.Context, limit int, tasks []task) []outcome {
	out := make([]outcome, len(tasks)) // 预分配槽位：goroutine 只写自己的下标，无竞争、保序
	g := new(errgroup.Group)           // 不用 errgroup.WithContext——那会引入"一错全停"
	g.SetLimit(limit)                  // 满员时 Go 调用本身阻塞排队，反压回分发循环

	for i := range tasks {
		t := tasks[i]
		g.Go(func() error {
			v, err := t.fn(ctx)
			out[i] = outcome{name: t.name, value: v, err: err} // 失败收进结果值
			return nil                                         // 关键：error 绝不交给 errgroup
		})
	}
	_ = g.Wait() // goroutine 恒返回 nil，Wait 只负责"等全部结束"
	return out
}

func main() {
	tasks := make([]task, 5) // 模拟 5 个调研子任务：task-3 撞 429，其余正常
	for i := range tasks {
		tasks[i] = task{
			name: fmt.Sprintf("task-%d", i),
			fn: func(ctx context.Context) (string, error) {
				select {
				case <-time.After(time.Duration(50*(i+1)) * time.Millisecond):
				case <-ctx.Done():
					return "", ctx.Err()
				}
				if i == 3 {
					return "", errors.New("api error 429: rate limited")
				}
				return fmt.Sprintf("调研结论 %d", i), nil
			},
		}
	}

	start := time.Now()
	results := runAll(context.Background(), 2, tasks)
	ok, failed := 0, 0
	for _, r := range results {
		if r.err != nil {
			failed++
			fmt.Printf("%-7s 失败: %v\n", r.name, r.err)
			continue
		}
		ok++
		fmt.Printf("%-7s 成功: %s\n", r.name, r.value)
	}
	fmt.Printf("汇总: %d 成功 / %d 失败，总耗时约 %v（并发上限 2）\n",
		ok, failed, time.Since(start).Round(time.Millisecond))
}
```

实测输出（耗时随调度略有波动）：

```text
task-0  成功: 调研结论 0
task-1  成功: 调研结论 1
task-2  成功: 调研结论 2
task-3  失败: api error 429: rate limited
task-4  成功: 调研结论 4
汇总: 4 成功 / 1 失败，总耗时约 455ms（并发上限 2）
```

三个观察实验（改改代码再跑，比读十遍解释有效）：

- **task-3 失败后 task-4 照样成功**——`return nil` 保住了部分失败语义。把它改成 `return err` 再跑一次，task-4 的结果会消失（退化为一错全停）：这是理解 1.4 节最快的实验；
- **输出按 task 序而非完成序**——预分配槽位天然保序，汇总层不用再做归位；
- **总耗时 ~455ms**（5 个任务、单个 50~250ms、并发上限 2）≈ 任务排成两条线串行——限流在源头生效的直接证据（对比：不限并发约 250ms，全串行约 750ms）。

对照练习 1 的差距（正是 TODO 的四条契约）：这里没有单任务 `jobTimeout` 派生、没有排队前的 `ctx.Err()` 检查（契约 4）、结果类型是简化的 `outcome` 而非带 `ID` 的 `Result`。练习就是把这三点补全。

### 3.2 超时预算分配：让 1.5 节的原则可观察

**为什么写它**：预算分层光讲原则不直观。这段代码让每层开工时打印 ctx 剩余预算，并故意让"竞品B"的 LLM 调用挂起——亲眼看"下层先超时、上层留余量善后"实际发生。真实比例（任务 10min → planner 2min → worker 3min → 单次调用 60s）等比缩小到毫秒级，方便直接跑。

```go
// 教学示例：超时预算逐层分配，每层打印剩余预算（可直接 go run）。
package main

import (
	"context"
	"fmt"
	"time"
)

func remaining(ctx context.Context) string { // 打印 ctx 剩余预算
	if d, ok := ctx.Deadline(); ok {
		return fmt.Sprintf("剩 %dms", time.Until(d).Milliseconds())
	}
	return "无死线"
}

// llmCall 模拟一次 LLM 调用：要么 latency 后返回，要么随 ctx 超时被掐断。
func llmCall(ctx context.Context, prompt string, latency time.Duration) (string, error) {
	fmt.Printf("    [llm] 调用 %q 开工（%s）\n", prompt, remaining(ctx))
	select {
	case <-time.After(latency):
		return "结论：" + prompt, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// worker 是中间层：下层（单次调用）超时返回后，本层还有余量做善后。
func worker(ctx context.Context, sub string, llmLatency time.Duration) (string, error) {
	fmt.Printf("  [worker %s] 开工（%s）\n", sub, remaining(ctx))
	callCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond) // 单次 LLM 调用预算
	defer cancel()
	v, err := llmCall(callCtx, sub, llmLatency)
	if err != nil {
		fmt.Printf("  [worker %s] 调用失败: %v（善后窗口 %s：记 checkpoint、走降级）\n", sub, err, remaining(ctx))
		return "", err
	}
	return v, nil
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond) // 任务总预算
	defer cancel()
	planCtx, planCancel := context.WithTimeout(ctx, 300*time.Millisecond) // planner 预算
	defer planCancel()
	fmt.Printf("[planner] 开工（%s）\n", remaining(planCtx))
	if _, err := llmCall(planCtx, "制定调研计划", 50*time.Millisecond); err != nil {
		fmt.Println("[planner] 规划失败:", err)
		return
	}

	for i, sub := range []string{"竞品A", "竞品B"} {
		// worker 从任务层 ctx 派生预算（600ms）——不是从 planCtx，
		// 否则 worker 会继承 planner 花剩下的 ~250ms 死线。
		workerCtx, workerCancel := context.WithTimeout(ctx, 600*time.Millisecond)
		latency := 100 * time.Millisecond
		if i == 1 {
			latency = 5 * time.Second // 竞品B 的 LLM 调用故意挂起：演示下层先超时、上层留余量
		}
		v, err := worker(workerCtx, sub, latency)
		workerCancel()
		if err != nil {
			fmt.Printf("[任务层] %s 失败已记录（%s，仍可继续处理其他子任务）\n", sub, remaining(ctx))
			continue
		}
		fmt.Printf("[任务层] %s 完成: %s\n", sub, v)
	}
}
```

实测输出（毫秒数每次略有不同）：

```text
[planner] 开工（剩 299ms）
    [llm] 调用 "制定调研计划" 开工（剩 299ms）
  [worker 竞品A] 开工（剩 599ms）
    [llm] 调用 "竞品A" 开工（剩 399ms）
[任务层] 竞品A 完成: 结论：竞品A
  [worker 竞品B] 开工（剩 599ms）
    [llm] 调用 "竞品B" 开工（剩 399ms）
  [worker 竞品B] 调用失败: context deadline exceeded（善后窗口 剩 197ms：记 checkpoint、走降级）
[任务层] 竞品B 失败已记录（剩 645ms，仍可继续处理其他子任务）
```

两个细节值得停下来想：

- **worker 的预算从任务层 `ctx` 派生，不是从 `planCtx`**——planner 的 300ms 已被规划花掉约 50ms，若从 `planCtx` 派生，worker 一开工就只剩 ~250ms。预算的"父层"选错是不报错的隐形 bug：能跑，但 worker 死得莫名其妙；
- **竞品B 超时那一刻，worker 层还剩 ~200ms、任务层还剩 ~600ms**——善后的空间（记 checkpoint、走降级、记录"哪个子任务死了"）就是分层预算买来的。对照 1.5 节的反面教材：全局一个死线，这一瞬间所有层同时死，输出里一行善后都看不到。

### 3.3 带 jitter 的指数退避（呼应第 3 章）

**为什么要升级版**：第 3 章的 `ChatWithRetry`（`mini-agent/internal/llm/client.go:238`）退避是固定的 `time.Second << (attempt - 1)`（`client.go:243`）、用裸 `time.Sleep` 等待。单 agent 顺序调用够用，并发场景有两个缺口：**无 jitter**——N 个 worker 同时收到 429，固定退避意味着同一瞬间一起重试、一起再被限（惊群效应，thundering herd）；**`time.Sleep` 不响应 ctx**——任务已取消，worker 还在睡满 4 秒。教学版补上这两点：

```go
// 教学示例：带 jitter 的指数退避，且等待可响应 ctx 取消（可直接 go run）。
package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// backoff 计算第 attempt 次重试（从 1 开始）的等待时长：
// 指数档 base*2^(attempt-1)，封顶 maxDelay，再做 full jitter——[0, d] 均匀随机。
// jitter 防惊群：N 个 worker 同时收到 429，若退避时长相同就会同一瞬间一起重试、一起再被限。
func backoff(attempt int, base, maxDelay time.Duration) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= maxDelay || d <= 0 { // d<=0 防御溢出翻转
			d = maxDelay
			break
		}
	}
	return time.Duration(rand.Int64N(int64(d)))
}

// sleepCtx 是可取消的等待：裸 time.Sleep 不响应 ctx，任务取消了 worker 还在睡觉。
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func main() {
	fmt.Println("第 N 次重试的退避时长（base=1s, max=30s，每档 3 次采样）：")
	for attempt := 1; attempt <= 5; attempt++ {
		fmt.Printf("  attempt=%d: %9v  %9v  %9v\n",
			attempt,
			backoff(attempt, time.Second, 30*time.Second).Round(time.Millisecond),
			backoff(attempt, time.Second, 30*time.Second).Round(time.Millisecond),
			backoff(attempt, time.Second, 30*time.Second).Round(time.Millisecond))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消的 ctx：sleepCtx 应立即返回而不是睡满 10s
	if err := sleepCtx(ctx, 10*time.Second); err != nil {
		fmt.Println("已取消的 ctx 下 sleepCtx 立即返回:", err)
	}
}
```

实测输出（随机采样，每次运行都不同）：

```text
第 N 次重试的退避时长（base=1s, max=30s，每档 3 次采样）：
  attempt=1:     949ms      643ms      800ms
  attempt=2:    1.944s      530ms      412ms
  attempt=3:    2.005s       15ms      1.64s
  attempt=4:     702ms      2.22s      483ms
  attempt=5:   10.037s    11.307s     12.41s
已取消的 ctx 下 sleepCtx 立即返回: context canceled
```

生产注意三条：

- **服务端回了 `Retry-After` 就服从它**（可在其上加小 jitter）——指数退避是没有服务端指引时的自保行为，不是与服务端对抗；
- full jitter（`[0, d]` 均匀随机）期望等待减半、恢复最快；equal jitter（`d/2 + rand(d/2)`）保留下限、更平滑。这套写法出自 AWS Architecture Blog 的 "Exponential Backoff and Jitter"，面试报得出出处是加分点；
- 退避只服务于"值得重试"的错误——`retryable`（`client.go:219`）的分类纪律不变：429/5xx 才进退避，4xx 直接失败。

### 3.4 goroutine 泄漏：制造它、看见它、定位它

**为什么单列**：1.2 节易漏点三（取消不传播）的最终形态就是泄漏——goroutine 阻塞在一个永远不会就绪的通道操作上，进程不死它不死。先把泄漏制造出来用肉眼看见，再讲生产里怎么定位。

```go
// 教学示例：goroutine 泄漏长什么样，以及怎么一眼看到它（可直接 go run）。
package main

import (
	"fmt"
	"runtime"
	"time"
)

// fixedFanIn 修复版：缓冲给足 + 收够 N 个，每个 goroutine 都有确定的退出路径。
func fixedFanIn() {
	results := make(chan string, 5) // 缓冲 = 任务数：发送永不阻塞
	for i := 0; i < 5; i++ {
		go func(i int) {
			results <- fmt.Sprintf("结论 %d", i)
		}(i)
	}
	for i := 0; i < 5; i++ { // 收够为止，发送方全部退出
		<-results
	}
}

// leakyFanIn 泄漏版：results 无缓冲，main 只收 1 个就返回，
// 其余 worker 永远阻塞在发送上——进程不死，goroutine 不死。
func leakyFanIn() {
	results := make(chan string) // 无缓冲：发送必须配对接收
	for i := 0; i < 5; i++ {
		go func(i int) {
			time.Sleep(time.Duration(i*10) * time.Millisecond) // 假装在跑 LLM 调用
			results <- fmt.Sprintf("结论 %d", i)                 // 没人接收 → 永远卡在这里
		}(i)
	}
	fmt.Println("  只收一个:", <-results) // 拿到"最快的"就走，剩下 4 个泄漏
}

func main() {
	fmt.Println("启动时 goroutine 数:", runtime.NumGoroutine())
	fixedFanIn()
	time.Sleep(100 * time.Millisecond)
	fmt.Println("修复版跑完后 goroutine 数:", runtime.NumGoroutine()) // 回到 1：无残留
	fmt.Println("跑泄漏版：")
	leakyFanIn()
	time.Sleep(100 * time.Millisecond)
	fmt.Println("泄漏版之后 goroutine 数:", runtime.NumGoroutine()) // 多出 4 个，且永不消失
}
```

实测输出：

```text
启动时 goroutine 数: 1
修复版跑完后 goroutine 数: 1
跑泄漏版：
  只收一个: 结论 0
泄漏版之后 goroutine 数: 5
```

泄漏版多出的 4 个 goroutine 会活到进程退出。生产环境没人用肉眼盯 `NumGoroutine`，标准定位路径是：

1. **挂 pprof**：`import _ "net/http/pprof"` 后访问 `/debug/pprof/goroutine?debug=2`，拿到全量 goroutine 调用栈——成百上千个栈顶停在同一个 `chan send` 就是铁证（`debug=1` 是按栈分组的计数，先看它更直观）；
2. **测试侧断言**：工程化用 `go.uber.org/goleak` 在测试结束断言"无新增 goroutine"；零依赖简化版就是上面 demo 的原理（`runtime.NumGoroutine()` 前后对比）；
3. **`-race` 抓不了泄漏**：race detector 查数据竞争，泄漏的 goroutine 完全"合法"，只是永远不走。面试 Q5 专门展开"怎么证明 pool 没泄漏"。

---

## 四、面试视角

> 以下每题给"标准回答 → 追问链 → 加分点"。自测方法：不看回答口述，再对照差距。

**Q1：errgroup 和手动 channel + WaitGroup 编排有什么区别？怎么选型？**

标准回答：errgroup 在 WaitGroup 之上包了三件事——`Wait` 返回第一个 error（不用自己写 error channel）、`WithContext` 出错自动取消派生 ctx、`Go` 内建 Add/Done 不会忘调；另有 `SetLimit` 内建限并发。手动 channel 版换来的是自由度：任务边产生边消费、结果边完成边处理、拓扑动态。

选型经验法则：**任务列表一开始就确定、等全部完成再汇总 → errgroup；流水线 / 动态拓扑 / 流式消费 → channel**。

追问链：

- "errgroup 能拿到全部错误吗？" → 不能，`Wait` 只给第一个；要全量收集得自己 mutex + slice 或 `errors.Join`（Go 1.20+）。
- "手动版最容易踩什么？" → 1.2 节三个易漏点全要自己扛：忘 close 死锁、错误没人收、取消不传播。

加分点：能说出"一个系统里两者各司其职"——本项目 worker 池用 errgroup + 预分配槽位，结果上报 / SSE 流式进度用 channel（阶段文档 3.1 Q7）；并补一句"channel 版不是更高级，是更灵活也更危险"。

**Q2：errgroup.WithContext 的取消语义，在"收集部分结果"场景为什么是坑？**

标准回答：`WithContext` 派生的 ctx 在任一 goroutine 返回非 nil error 时被取消，其余 worker 被级联中止——一错全停。调研类任务"挂一个、其余结论仍要进报告"时，它会把已经拿到结论的 worker 也掐死，汇总层收到的全是 `context.Canceled`。更隐蔽的退化方式：用了裸 `Group` 却在 goroutine 里 `return err`——效果一模一样，语义悄悄变回一错全停。

追问："那父任务取消怎么传下去？" → **取消传播和错误语义是两件事**：父 ctx 照常透传给每个 job（外部取消、超时级联都还在），只是 error 不交给 errgroup。部分失败 ≠ 不响应取消。

加分点：给出判别句——"**失败是数据还是信号？是数据就收进 `Result.Err`，是信号才交给 errgroup**"；并指出测试陷阱：失败 job 恰好排最后时一错全停测不出来，测试要故意让失败发生在中间（参考答案 `TestRun_PartialFailure` 的排布设计）。

**Q3：context 超时预算怎么在多级调用中分配？**

标准回答：越下层预算越小，每层给上层留善后余量：任务 10min → planner 2min → 单 worker 3min → 单次 LLM 调用 60s → HTTP 客户端 120s 兜底（`client.go:30`）。每层 `context.WithTimeout` 从父层派生，下层先超时、以普通错误返回，上层还有时间记 checkpoint、走降级。

追问链：

- "全局一个死线透传会怎样？" → 到点所有层同一瞬间死，善后代码一行都跑不到，故障现场零状态，崩溃恢复无从下手（阶段文档注意事项第 2 条）。
- "worker 的 ctx 从哪派生？" → 从任务层，不是 planner 的 ctx——planCtx 的预算已被规划消耗（3.2 节实测演示）。
- "ctx 取消了，goroutine 就停了吗？" → 取消只是发信号，goroutine 必须 `select ctx.Done()`（HTTP 层靠 `http.NewRequestWithContext` 传到底）；善后操作要换 `context.WithoutCancel` 或新起短超时 ctx，不能再用已取消的。

加分点：能讲清"预算自下而上先触发"的时序，并指出 `defer cancel()` 漏调泄漏的是定时器资源而非 goroutine（`go vet` 的 lostcancel 能抓一部分）。

**Q4：并发 worker 打 LLM API 撞 429，怎么处理？**

标准回答：三道防线。① 源头限并发：worker 并发数 ≤ API 配额（semaphore / `SetLimit`）——排队几乎零成本，429 则是一次失败的计费请求加延迟惩罚，限流比退避便宜；② 单次调用指数退避：429/5xx 才重试（`client.go:219` 的错误分类）、尊重 `Retry-After`、加 jitter 防惊群、等待可响应 ctx（3.3 节）；③ 持续限流走降级 / 排队：换备用模型或子任务重新入队，兜底死信标 failed 不拖垮整个任务（第 10 章编排器落地）。

追问链：

- "jitter 防的具体是什么？" → 惊群：N 个 worker 同一瞬间被限，固定退避 = 同一瞬间一起重试、一起再被限；jitter 把重试时刻在时间上摊开。
- "并发数和 QPS 是一回事吗？" → 不是：并发 5 个长请求也可能超 QPS——semaphore 管"同时在场数"，QPS 要令牌桶另算（预习指南第五节第 3 条）。

加分点：一句"并发场景 429 是**常态不是异常**，被限流要当预期路径设计"，并说出 jitter 出自 AWS 的 "Exponential Backoff and Jitter"。

**Q5：怎么证明你的 pool 没有 goroutine 泄漏？**

标准回答：三层证据。① 设计层：每个 goroutine 都有确定退出路径——结果写预分配槽位（没有 channel 阻塞面）或缓冲给足，ctx 取消有监听分支；② 测试层：用例覆盖父取消 / 超时路径并断言 `Run` 按时返回；工程化用 `go.uber.org/goleak` 断言无残留，零依赖版用 `runtime.NumGoroutine()` 前后对比（3.4 节）；③ 运行层：pprof goroutine profile，长跑下 goroutine 数应回落基线，不随任务数单调上涨。

追问："`go test -race` 能抓泄漏吗？" → 不能——race detector 抓数据竞争，泄漏的 goroutine 是合法阻塞。两者互补：`-race` 证"没抢"，leak 检测证"都走了"。

加分点：给出排查动线——"`/debug/pprof/goroutine?debug=2` 拿全量栈，按栈顶分组；几百个 goroutine 停在同一个 `chan send`，行号直指泄漏点"。

**Q6：SetLimit 和 semaphore 是什么关系？**

标准回答：`SetLimit(n)` 就是 errgroup 内建的 semaphore：满员时 `Go` 调用在分发循环处阻塞排队，语义等价于容量 n 的信号量。差异两点：① 阻塞位置不同——`SetLimit` 的阻塞在 `Go` 调用处（不感知 ctx），`semaphore.Weighted` 的 `Acquire(ctx, 1)` 在 goroutine 内部，等待时可以响应取消；② 粒度不同——`SetLimit` 只管这一个 group，semaphore 可以跨 group、跨资源复用一个全局配额（比如全系统共享的 LLM 并发额度）。

追问："SetLimit 不感知 ctx 的实际后果？" → 父取消后，排队的 job 仍会逐个拿到 slot 开工——所以拿到 slot 后第一件事是 `if err := ctx.Err(); err != nil` 检查（pool 契约 4 的兜底，2.4 节）。

加分点：三种实现选型张口就来——`SetLimit`（单 group 最简）、`semaphore.Weighted`（等待要可取消 / 跨 group 共享配额）、channel 手写（面试手写题，能顺手 `select` 加超时）。

---

## 五、常见坑

1. **errgroup 错误语义选错**：要部分失败却用了 `WithContext`，或在 goroutine 里 `return err`——语义悄悄退化成一错全停。最阴险的是测试假象：失败 job 恰好排最后时，其他 job 早已跑完，一错全停根本测不出来。测试要故意把失败放中间、慢 job 放末尾（参考答案 `TestRun_PartialFailure` 的排布）。
2. **全局一个死线透传到底**：总预算 10 分钟原样透到最底层，到点所有层同一瞬间超时，降级、checkpoint、善后全部没机会执行——故障现场什么状态都没留下。逐层 `WithTimeout` 递减预算，下层先死、上层留余量（1.5 节原则，3.2 节实测）。
3. **循环变量捕获**：Go 1.22 之前 `for _, v := range xs { go func() { use(v) } }` 的所有 goroutine 共享同一个 `v`，跑完全拿最后一个值；1.22+ 每轮迭代都是新变量，已免疫（本章示例代码依赖 1.22+ 语义，项目 `go.mod` 为 go 1.26）。但面试必问这段历史，且老代码库里到处是 `v := v` 防御写法——见到要认识，接手旧库别急着删。
4. **Wait 前忘 close channel → 死锁**：`for r := range ch` 的接收方靠 close 才知道"没有了"。全员死锁时 runtime 报 `fatal error: all goroutines are asleep - deadlock!`；只吊死一部分时连报错都没有，只是悄悄卡住。纪律：只有发送方能关、最好只有一个发送方；多发送方时用 WaitGroup 等齐后由专人关（参考答案进阶 RunStream 的"专门 goroutine `wg.Wait(); close(out)`"是标准解法）。
5. **SetLimit 的阻塞不感知 ctx**：满员时 `Go` 在分发循环上阻塞排队，父 ctx 取消也不会让它返回——排队的 job 仍会逐个拿到 slot。开工前的 `ctx.Err()` 检查一行兜底，漏了契约 4 就名存实亡（参考答案 `TestRun_ContextCancelled` 抓的就是这条）。

---

## 六、动手练习

**练习 1：errgroup + semaphore 的 worker pool**（阶段三练习 1，本章所有概念的落点）

- **位置**：`stage-03-multi-agent/internal/pool/pool.go:69` 的 `TODO(练习1)`；`golang.org/x/sync` 已在依赖里（`stage-03-multi-agent/go.mod:18`），不用 go get。
- **任务**：实现 `func (p *Pool) Run(ctx context.Context, jobs []Job) []Result`，四条契约照 TODO（`pool.go:75` 起）——并发上限、单 job 预算派生、部分失败、父取消尽快退出。下手前把 2.4 节再过一遍：那里已经讲了每条提示"为什么对"。
- **测试也是练习的一部分**：骨架里没有 `pool_test.go`，TODO 的验收要求"并发上限、超时、部分失败"三个测试——自己写。两个提示：并发上限用 atomic 计数器断言峰值，别用总耗时推算（调度抖动下很脆）；慢 job 用 `select ctx.Done()` 响应取消，别裸 `time.Sleep`，否则超时用例会被拖满。
- **验收**：
  1. `cd stage-03-multi-agent && go vet ./internal/pool/` 无警告；
  2. `go test ./internal/pool/ -race -count=1` 全绿；
  3. 能脱稿回答：为什么不用 `errgroup.WithContext`？为什么结果写槽位而不是 channel 收集？预算为什么要分层？
- **`-race` 的意义**：预分配槽位的 disjoint 写入本无竞争，但如果你顺手写成 `append` 共享 slice，不加 `-race` 的 `go test` 大概率照样全绿——竞争是概率事件，"碰巧没撞上"不等于"没有"。race detector 在运行期动态检测冲突访问，把它变成"一定被抓"。并发代码的测试不加 `-race`，等于只测了一半。
- **参考答案**：`docs/solutions/stage-03/exercise-1-worker-pool.md`（**完成并自评后再看**：基础实现 + 四个测试 + 进阶 RunStream 流式版）。

---

## 本章小结

- 多 Agent 编排的发动机是四个并发问题：并行分发（goroutine）、等待汇总（WaitGroup/errgroup）、限流（semaphore/SetLimit）、生命周期（context）。
- errgroup 包掉了手动编排的三个易漏点；固定任务集用 errgroup，流水线 / 动态拓扑才回 channel。
- 两种错误语义：一错全停（error 是信号）vs 部分失败（error 是数据，收进 `Result.Err`）——没有对错，选错才出问题。pool 选后者，`Run` 签名里没有 error 就是这个决定。
- 超时是预算分配不是设一个值：越下层越小，上层留善后余量；全局一个死线 = 连记录失败的机会都没有。
- 429 三道防线：源头限并发 → 指数退避 + jitter → 降级/排队；并发场景把被限流当预期路径设计。
- 每个 goroutine 启动前回答三问：谁等它结束？错误给谁？怎么让它停下来？

下一章：[第 9 章：任务状态机与崩溃恢复](09-task-persistence-and-recovery.md)——进程会死，状态不死：把本章 pool 跑出的每个子任务结局落盘成 checkpoint，崩溃之后接着跑。
