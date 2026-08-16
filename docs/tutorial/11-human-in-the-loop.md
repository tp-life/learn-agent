# 第 11 章：Human-in-the-loop——高风险操作的刹车系统

> 对应阶段：阶段三（深入·多 Agent 编排与生产化）· 项目 3 `stage-03-multi-agent/`
> 代码位置：`stage-03-multi-agent/internal/hitl/`（本章精讲）、`internal/orchestrator/orchestrator.go:171`（审批闸契约）、`cmd/hitl-demo/`（演示 CLI）
> 前置：第 9 章（任务状态机与 checkpoint）、第 10 章（planner/worker 编排器）；并请回忆第 1 章进阶 3.1 的 prompt 注入三层防御——其中最硬的"人工审批"层就是本章
> 学完后你能讲清：为什么高风险操作前必须暂停、暂停-恢复为什么是状态机思想的直接应用、为什么审批不能靠内存 channel 等、审批与审计为什么要分两张表——这四问是 HITL 面试的全部主干。

---

## 本章地图

- 为什么需要 HITL：LLM 不确定性 × 工具副作用 = 必须有刹车
- 什么节点该让人介入：不可逆、高成本、低置信三类；审批点要像异常一样稀缺
- 暂停-恢复的实现原理：waiting_human 落盘 → 让出 → 事件驱动恢复
- 为什么不能只靠内存 channel 等审批（进程重启、资源占用、多实例路由）
- 真相源与审计分离：task.Store 管"能不能跑"，approvals 表管"谁批过什么"
- 哨兵错误 `ErrWaitingHuman`：用错误值做控制流（第 1 章 APIError 的同款思想）
- 进阶：审批超时与升级、审批策略配置化、与工具层的结合

---

## 一、概念详解

### 1.1 为什么需要 HITL：不确定性 × 副作用

两个事实叠加出 HITL 的必要性：

1. **LLM 输出是不确定的**（第 1 章 1.1 节）。模型的工具选择与参数是采样出来的，再强的 prompt 也只能降低出错概率，不能归零。
2. **工具可能有真实副作用**。删数据、发邮件、调付费 API——一旦执行，没有"撤销上一轮对话"这回事。

只读工具选错了，大不了重试；有副作用的工具选错了，损失已经发生。所以在**执行路径上**必须有一个人工关卡：高风险操作前先暂停，把"它到底要干什么"摆给人看，等 approve/reject 再决定动不动手。

回到第 1 章进阶 3.1 的 prompt 注入三层防御：system prompt 声明边界（模型可能不听）、工具结果包装（模型可能认不出）、**人工审批**。前两层都在"求模型别犯错"，只有第三层不依赖模型的配合——注入指令骗过了模型，也还要过人这一关。所以 HITL 是三层防御中**最硬的一层**，也是唯一一层"对手绕不过模型就彻底无效"的防线。

一句话心智模型：**LLM 负责提议，人负责批准，代码负责把关**。全自动系统处理 95% 的常规操作，剩下 5% 的高风险操作交给人——Human-in-the-loop 不是"不信任 AI"，而是把信任精确地只授予可逆的操作。

### 1.2 什么节点该让人介入：三类触发，一个代价

该设审批点的操作分三类（进阶 3.2 会把它们落成可查表的代码）：

| 类别 | 判据 | 例子 |
| --- | --- | --- |
| 不可逆 | 副作用无法撤销或撤销成本极高 | 删数据、发邮件、线上发布、转账 |
| 高成本 | 单次操作花费超过预算线 | 批量调用付费 API、起一大波云资源 |
| 低置信 | 执行侧自己都没把握 | critic 评分过低、计划校验勉强通过、参数由不可信内容推导而来 |

介入的代价同样真实：**延迟**（人从几分钟到几小时后才批）与**人力**（每个审批点都要占一个人的注意力）。所以审批点必须**像异常一样稀缺**——一个任务每步都要批的系统，等于把自动化又改回了人肉流程，还多了调度开销。反模式叫"审批疲劳"：审批人一天看 200 条审批，第 150 条开始就是无脑点同意（自动化偏见），刹车系统形同虚设。设计目标是把审批率压到个位数百分比，让每次审批都值得人认真看。

### 1.3 暂停-恢复的实现原理：状态机思想的直接应用

HITL 没有任何新发明——它就是第 9 章任务状态机的一个新状态、两条新迁移边：

```
                 审批闸拦截                approve（回迁）
  pending ──────────────────▶ waiting_human ──────────▶ pending ──▶ running ──▶ done
                                   │
                                   └──────── reject ──────────▶ failed
```

完整时序（对照阶段文档 3.3 的全生命周期图）：

1. 编排器分发子任务，执行前发现 `RequiresApproval=true` → 子任务迁到 `waiting_human` 并**落盘**（一次普通 checkpoint）；
2. 执行侧**让出**：`Run/Resume` 带着哨兵错误 `ErrWaitingHuman` 直接返回，任务级状态也落 `waiting_human`——没有 goroutine 在原地等；
3. 人工通过 CLI / HTTP API 看到待审批列表，提交 approve/reject；
4. 决定**写进 checkpoint**：approve → 子任务迁回 `pending` 并消费掉审批旗标；reject → 迁 `failed` 并记驳回原因；
5. 调用方再调 `Resume`，从断点续跑——被批准的子任务这次畅通无阻地执行。

这里最反直觉、也最要害的一步是第 2 步的**让出**（yield），不是**阻塞**（block）。朴素的写法是 worker goroutine 里 `decision := <-approvalCh` 原地等——看起来简单，实际是三个灾难的合体（下一节逐条拆）。正确的"暂停"是任务退出执行、把恢复的责任交给未来的审批事件：**事件驱动恢复**。审批发生前，这个任务不占用任何执行资源；审批发生后，`Resume` 像崩溃恢复一样从 checkpoint 重建现场——事实上它复用的就是崩溃恢复那条代码路径。

### 1.4 审批决定也是状态：为什么不能只靠内存 channel

**审批决定是任务状态的一部分，必须落盘。** 考虑这个时间线：子任务进入 `waiting_human` → 人批了 approve → 进程在重跑前崩了。如果"已批准"只存在于内存（比如一个已被消费的 channel 消息），重启后系统看到的是"一个还在等审批的子任务"——人会收到第二次审批请求，更糟的是没人记得第一次批准过。

把这句话说完整：**进程重启后，"已批未执行"的任务必须能继续**。这就是"为什么编排引擎要配数据库"的最直接答案——不是为了让看板有数据可显示，而是因为审批这件事跨越了进程生命周期，内存里的任何东西都活不过重启。`cmd/hitl-demo` 的第三条演示路径（kill 进程 → 重启 → 审批现场与决定分毫不丢）演的就是这一件事。

顺带推平两个相关追问：

- **审批期间任务占什么资源？** 不占执行资源。让出后没有 goroutine、没有内存里的等待者，只有 DB 里一行 `waiting_human` 记录。成本是"看板上多一个黄点"，不是"线上挂着一千个阻塞协程"。
- **多实例部署时审批请求路由到哪台机器？** 任意一台。审批请求不依赖"哪个进程发起了等待"，任何实例都能 LoadTask、校验状态、落决定、触发 Resume——因为状态在 DB 里。这是"状态外置，进程无状态"（第 9 章）红利在 HITL 上的又一次兑现。

如果规模大到需要消息队列推审批事件，也可以——但 MQ 只是**传输层**（让恢复更快被触发），真相源仍然是 DB 里的状态与决定。把 MQ 消息当真相源，就回到了"内存 channel"的同一个坑。

### 1.5 真相源与审计分离：两张表各回答一个问题

HITL 落地时最容易犯的结构性错误，是把"流程状态"和"审计记录"揉成一张表。本项目的划分是：

- **真相源：`task.Store` 的 `subtask.status`**。它回答的问题是"这个子任务现在能不能跑"。所有流程判断（分发、Pending 聚合、Resume 筛选）只看它，且所有状态迁移都走 Store 的状态机守卫（非法迁移直接报错）。
- **审计：`hitl` 包的 `approvals` 表**。它回答的问题是"谁在什么时候对哪个子任务批了什么"。它只 INSERT、不参与流程判断——合规复盘、看板展示、事故追责都查它。

为什么不能合一：**双写不一致**。如果两处都记"在等批"，就要保证每次变化双写原子完成（没有跨表事务时必有窗口期），而双写不一致的 bug 是静默的——流程状态和审计各说各话，你都不知道该信谁。只留一个真相源，不一致的可能性从根上消失；审计丢了可以补记，状态错了系统直接做错事。

还有一个语义设计藏在 approve 里：**approve 是"一次性放行令牌"，不是永久豁免**。批准的语义是"这一次执行被放行"，落盘时要**消费掉** `requires_approval` 旗标——子任务迁回 `pending` 时旗标清零，Resume 重跑不再被闸拦。如果旗标不清，Resume 时审批闸再次拦截，approve 多少次都执行不了（死循环）；反过来如果做成"approve 一次、该子任务永久免检"，一次批准就成了永久通行证，违背了"每次执行前都该能拦"的初衷。reject 则刻意**保留**旗标：`failed && RequiresApproval==true` 这个组合让编排器一眼认出"这是人工驳回，不是执行失败"，不进重试重排队。一个布尔旗标，把"待批 / 已放行 / 已驳回"三种售后状态编码得清清楚楚。

### 1.6 哨兵错误 ErrWaitingHuman：错误值即控制流

"暂停"这个信号要从 worker 执行路径一路穿透 pool、dispatch 传到 `Run/Resume` 的调用方，Go 里最顺手的载体就是**哨兵错误**（第 1 章 `APIError` 的同款思想——错误类型是控制流的一部分）：

```go
// stage-03-multi-agent/internal/orchestrator/orchestrator.go:184
var ErrWaitingHuman = errors.New("orchestrator: 任务暂停，等待人工审批")
```

为什么不是给返回值加 `waiting bool`：

1. **签名零改动**。`Run/Resume` 保持 `(string, error)`，pool、dispatch 各层签名都不用加一个层层透传的标志位；
2. **天然冒泡**。哨兵错误穿透多层调用栈，调用方一处 `errors.Is` 就能区分"让出"与"失败"——CLI 进审批循环；HTTP 层则在后台 goroutine 里识别并吞掉这个哨兵（它不是失败，不打错误日志），"等待人工"的状态经读路径（GET 详情 / SSE 事件流）呈现给前端（第 13 章 §2.4 兑现）；
3. **词汇表一致**。它与练习 4 的 `ErrBudgetExceeded` 同构——编排器"非正常但可预期"的出口统一用哨兵错误表达，调用方学一遍就会。

对应的最常见误用：在 dispatch 里把它当普通失败处理（吞掉、记日志后继续、甚至把子任务迁 failed）——让出语义瞬间消失，任务被误判成失败。看到 `ErrWaitingHuman`，唯一正确的动作是"原样向上传"。

---

## 二、代码精讲

HITL 横跨三个包：`task`（状态机加新状态）、`orchestrator`（审批闸与让出）、`hitl`（待审批聚合 + 决定落盘）。外加 `cmd/hitl-demo` 演示 CLI。骨架里类型与契约已给全，TODO(练习5) 是实现本身——本节讲清"每个零件是什么、为什么长这样"，实现留给你。

### 2.1 状态机侧的准备：`waiting_human` 状态与迁移边

`stage-03-multi-agent/internal/task/task.go:36` 的状态枚举里，`StatusWaitingHuman`（`task.go:40`）是 HITL 唯一新增的状态：

```go
StatusWaitingHuman Status = "waiting_human" // 暂停等人工审批（HITL，练习5 用）
```

子任务侧的高风险标记在 `Subtask.RequiresApproval`（`task.go:66`），由 planner 在分解时标记（`orchestrator/planner.go:28` 的 `SubtaskSpec` 同名字段落盘而来）。注意它的生命周期：落盘时为 true → approve 时被 Decide 清零（一次性令牌）→ reject 时保持 true（驳回识别用）——1.5 节的语义全靠这个字段承载。

状态机守卫（`task.go:175` 的 `TransitionSubtask`，练习 2 实现）需要两条新迁移边，这是练习 5 的**前置补丁**：

- `pending → waiting_human`：审批闸在执行前拦截用。闸在迁入 `running` **之前**，所以"等审批"不算一次执行，`attempts` 不自增；
- `waiting_human → pending`：approve 后回迁重排队用。

（`waiting_human → failed` 给 reject 用、任务级 `running ↔ waiting_human` 给让出/恢复用，练习 2 的迁移表里已有。）为什么迁移必须走守卫而不是裸 SQL：守卫先 SELECT 当前状态查表校验，非法迁移报错——"从 done 迁回 waiting_human"这类调用方 bug 会在第一现场爆炸，而不是污染数据后慢慢发酵。

### 2.2 hitl 包：审批请求/决定的数据结构

`stage-03-multi-agent/internal/hitl/hitl.go` 只有约 160 行，骨架给全了类型与构造函数。

两个数据结构分别服务两个读者（`hitl.go:37`、`hitl.go:47`）：

```go
// Approval 是一条审批记录（审计日志）。
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
```

两个字段值得停下想：

- `Approval.DecidedBy`：审计不留名等于没有审计——出了事故第一问就是"谁批的"。所以 Decide 要求 `by` 非空，这不是洁癖，是审计的最低有效性条件；
- `PendingApproval.Prompt`：审批人的决策依据。只给标题"删除过期数据"是不够的，审批人要看到完整指令（"删除 90 天前的过期业务数据"）才能判断参数是否合理——prompt 注入防御的最后一关能不能守住，就看人能不能看到这里有没有猫腻。

`Service`（`hitl.go:55`）持两条到**同一个 SQLite 文件**的连接：

```go
type Service struct {
	store *task.Store // 流程状态的唯一真相源：所有状态迁移都走 Store 的状态机守卫
	db    *sql.DB     // 本包自建连接：approvals 审计表读写、approve 时清旗标
}
```

为什么不把 approvals 表开进 `task.Store`：Store 的表结构与方法是练习 2 的契约，审计是练习 5 的新关注点，不该回改练习 2 的代码——关注点分离。`NewService`（`hitl.go:71`）里有两个 SQLite 实战细节值得记住：

- DSN 带 `_pragma=busy_timeout(5000)`（`hitl.go:72`）：两条连接并发写同一文件会撞 `SQLITE_BUSY`（单写者模型），撞锁时等待最多 5 秒而不是立刻报错；
- `db.SetMaxOpenConns(1)`（`hitl.go:77`）：与 `task.Open` 同一理由，把写钉成串行。生产上量后换 Postgres，这两个纠结自然消解。

### 2.3 TODO(练习5)：两个方法各自要实现什么

`hitl.go:106` 的 TODO 块把要求写得很细，这里讲清设计意图，实现是你的练习：

- **`Pending(ctx)`**（`hitl.go:150`）：跨全部未完成任务聚合 `waiting_human` 子任务。数据源是 `ListResumable` → 逐个 `LoadTask` → 按状态过滤。**刻意不查 approvals 表**——审计表只记已发生的决定，拿它推断"谁在等批"就是双写不一致的开端（1.5 节）。
- **`Decide(ctx, taskID, subtaskID, approve, by)`**（`hitl.go:158`）：把人工决定落盘。三个纪律：① 先校验子任务**真的在** `waiting_human`——对不在等批的子任务做决定是调用方 bug（重复提交、过期页面），必须报错而不是静默写审计；② approve 迁回 `pending` + 清旗标（一次性令牌，1.5 节），reject 走 `FailSubtask` 且保留旗标；③ **先迁状态，后写审计**——反过来"先审计后迁移"中途失败，会留下"有审计但状态没动"的假象，比"状态动了但少一行审计"更难排查。

编排器侧的 TODO（`orchestrator.go:186`）是配套的四处增量：审批闸（迁 `waiting_human` + 返回哨兵错误）、dispatch 筛选循环区分四种状态、`pool.Run` 后**以 DB 为准**检测让出（不看内存结果——Resume 场景下"谁在等批"可能是上一个进程留下的）、Resume 的状态补齐 map 加一行。三段分离是这里的架构要点：**编排器只负责"停下"，hitl.Service 负责"落决定"，调用方（CLI/HTTP）负责"再 Resume"**——谁也不黏着谁，进程随便重启。

### 2.4 cmd/hitl-demo：三条演示路径各自验证什么

`cmd/hitl-demo/main.go` 全程离线（假 Planner 预置计划、假 Worker 回显，不烧 token），骨架给了全部零件，主流程是 TODO。

`fakePlanner`（`main.go:42`）预置三个子任务，`s2 "删除过期数据"` 标了 `RequiresApproval: true`——删数据是教科书级的高风险操作。`askDecision`（`main.go:59`）有一个容易被忽略的安全细节：stdin 被管道喂到 EOF 还没读到有效输入时**默认驳回**——演示脚本没喂够输入时，宁可不执行高风险操作（fail-closed，进阶 3.1 还会遇到这个原则）。

三条演示路径（验收命令见 `main.go:117-119`）各自钉死一个论点：

| 路径 | 命令 | 验证什么 |
| --- | --- | --- |
| approve | `printf 'a\n' \| go run ./cmd/hitl-demo --db /tmp/hitl.db` | 完整闭环：撞闸让出 → Pending 可见 → Decide → Resume 续跑至 done，s2 真实执行一次 |
| reject | `printf 'r\n' \| go run ./cmd/hitl-demo --db /tmp/hitl.db` | 驳回语义：s2 failed 记驳回信息、不被重排队，任务按部分失败语义 done |
| kill 重启 | 审批提示时 Ctrl-C，再执行同一条命令 | **本章灵魂**：`waiting_human` 现场与已落盘的决定跨进程存活（1.4 节） |

实现主流程时的两个坑提示（TODO 块里也写了）：`ErrWaitingHuman` 是 for 循环的**继续条件**不是失败，别进 `log.Fatal` 分支；`bufio.Reader` 全程只建一次——它会预读缓冲，每轮新建会把管道里后续的输入吞掉。

---

## 三、进阶拓展（带代码）

以下三段教学代码自足可跑（`go run` 验证过），讲的是生产化 HITL 的三个模式；它们是概念演示，与练习 5 的实现不重叠，做完练习后可对照参考答案的进阶部分继续深挖。

### 3.1 审批超时与升级策略

**问题**：`waiting_human` 是"人在回路"——人休假了、通知没发到，任务就永远挂着：不 done 也不 failed，看板上一片黄，还占着 `ListResumable`。没有超时兜底的 HITL 等于把"系统可用性"押在"审批人一定在线"上。

**模式**：周期扫描 + 超时自动驳回 + 升级通知钩子。

```go
// 教学示例：审批超时扫描器——把"永远悬挂"的等待项变成显式终态。
// 模式要点：waiting_human 不能永远等（人休假、通知没发到）。
// 超时兜底 = 周期扫描 + 默认拒绝（fail-closed）+ 升级通知钩子。
package main

import (
	"context"
	"fmt"
	"time"
)

// WaitingItem 是一个正在等待人工审批的操作。
type WaitingItem struct {
	ID           string
	WaitingSince time.Time // 进入等待的时刻（项目里对应 subtasks.updated_at）
}

// Store 是扫描器需要的最小存储接口：找出超时等待项 + 执行驳回。
type Store interface {
	// ListStale 返回等待起点早于 cutoff 的审批项。
	ListStale(ctx context.Context, cutoff time.Time) ([]WaitingItem, error)
	// Reject 把审批项置为终态并记录原因与"审批人"。项目里这一步走状态机守卫
	// （waiting_human → failed 是合法迁移边），decidedBy 记 "system:timeout"，
	// 与人工决定在审计上可区分。
	Reject(ctx context.Context, id, reason, decidedBy string) error
}

// Escalator 是升级通知钩子：发邮件/IM webhook 不是扫描器的职责，
// 扫描器只把"这批项被超时驳回了"这个事实抛给应用层。
type Escalator interface {
	Notify(ctx context.Context, item WaitingItem) error
}

// Scanner 周期扫描超时等待项并自动驳回。
// 刻意不内置后台 goroutine：库不管调度，调度（cron / time.Ticker）是应用的
// 职责——内置 goroutine 会随进程退出裸死，还让测试没法离线驱动。
type Scanner struct {
	Store    Store
	Escalate Escalator     // 可为 nil：只驳回，不通知
	MaxAge   time.Duration // 等待超过这个时长即视为超时
}

// Tick 执行一轮扫描，返回本轮被超时驳回的条数。由调用方周期触发。
func (s *Scanner) Tick(ctx context.Context) (int, error) {
	stale, err := s.Store.ListStale(ctx, time.Now().Add(-s.MaxAge))
	if err != nil {
		return 0, fmt.Errorf("查询超时等待项: %w", err)
	}
	done := 0
	for _, item := range stale {
		reason := fmt.Sprintf("审批超过 %s 无人处理，超时自动驳回", s.MaxAge)
		if err := s.Store.Reject(ctx, item.ID, reason, "system:timeout"); err != nil {
			return done, fmt.Errorf("超时驳回 %s: %w", item.ID, err)
		}
		done++
		if s.Escalate != nil {
			// 通知失败只记录、不回滚驳回：驳回是安全动作，通知是尽力而为。
			_ = s.Escalate.Notify(ctx, item)
		}
	}
	return done, nil
}

// ---------- 以下是用内存实现驱动的演示（真实实现里 Store 背后是 DB） ----------

type memStore struct{ items map[string]WaitingItem }

func (m *memStore) ListStale(_ context.Context, cutoff time.Time) ([]WaitingItem, error) {
	var out []WaitingItem
	for _, it := range m.items {
		if it.WaitingSince.Before(cutoff) {
			out = append(out, it)
		}
	}
	return out, nil
}

func (m *memStore) Reject(_ context.Context, id, reason, by string) error {
	delete(m.items, id)
	fmt.Printf("驳回 %s（%s，决定方 %s）\n", id, reason, by)
	return nil
}

type printEscalator struct{}

func (printEscalator) Notify(_ context.Context, it WaitingItem) error {
	fmt.Printf("升级通知：%s 已等待 %s，请负责人介入\n", it.ID, time.Since(it.WaitingSince).Round(time.Minute))
	return nil
}

func main() {
	store := &memStore{items: map[string]WaitingItem{
		"删除过期数据": {ID: "删除过期数据", WaitingSince: time.Now().Add(-2 * time.Hour)}, // 已超时
		"发送营销邮件": {ID: "发送营销邮件", WaitingSince: time.Now().Add(-time.Minute)},   // 未超时
	}}
	sc := &Scanner{Store: store, Escalate: printEscalator{}, MaxAge: 30 * time.Minute}

	n, err := sc.Tick(context.Background())
	fmt.Printf("本轮超时驳回 %d 项，剩余等待 %d 项，err=%v\n", n, len(store.items), err)
}
```

**取舍与生产注意**：

- **超时该默认放行还是默认拒绝？默认拒绝（fail-closed）**。论证：超时放行的含义是"没人看就自动执行高风险操作"——只要制造审批积压（哪怕只是等审批人下班）就能绕过全部人工防御，刹车系统变成了摆设。默认拒绝的代价是任务被卡住需要人工善后，但"没执行"永远比"执行错了"好恢复。fail-open 只在一种情况合理：操作本身危害极低、卡住业务的代价更高——而那种操作一开始就不该进审批点（1.2 节）。
- **审计上要可区分**：超时驳回的 `decidedBy` 记 `system:timeout`，与人工决定分开统计——"超时率"是审批 SLA 是否健康的核心指标，飙高说明审批人力不足或通知链路断了。
- **通知是钩子不是职责**：扫描器返回/上报被驳回的项，发送邮件、IM webhook 由应用层做。库内置 goroutine 定时器是反模式（随进程裸死、没法离线测），调度归应用（cron / ticker）。
- 等待起点从哪来：项目里每次状态迁移都刷新 `updated_at`，它天然就是"进入 `waiting_human` 的时刻"，不用加列。

### 3.2 审批策略配置化：规则表代替 if 链

**问题**：`RequiresApproval` 由 planner 逐个标记，本质是把风险判断交给了 LLM。更稳的做法是**代码侧有一张确定性规则表**：什么工具、什么操作、什么成本线要批，写成数据而不是散落在 prompt 和 if 链里——改规则不动代码，安全评审时一张表看得完。

```go
// 教学示例：审批策略配置化——把"什么操作要批"写成规则表，而不是 if 链。
package main

import "fmt"

// Risk 是操作的风险等级。
type Risk int

const (
	RiskLow    Risk = iota // 只读、可重试：搜索、查询
	RiskMedium             // 有副作用但可逆、低成本：写草稿、建分支
	RiskHigh               // 不可逆或高成本：删数据、发邮件、付费 API
)

// Operation 描述一次待执行的操作——审批策略只看这份事实描述，
// 不需要也不应该看到工具的内部实现。
type Operation struct {
	Tool          string  // 工具名
	Reversible    bool    // 副作用是否可逆
	EstimatedCost float64 // 预估成本（美元）
	Confidence    float64 // 执行侧对"该做这件事"的置信度（0~1，critic/自评给出）
}

// ApprovalPolicy 决定一个操作是否需要人工审批。
// 为什么是接口：规则表只是其中一种实现——也可以接配置中心、按租户分级、
// 按时间段收紧（夜间一律要批），而调用方（工具闸门）完全不感知差异。
type ApprovalPolicy interface {
	NeedsApproval(op Operation) bool
}

// RuleTable 把审批规则表达为数据：改规则不动代码，安全评审时一张表看得完。
type RuleTable struct {
	ToolRisk      map[string]Risk // 工具级风险底表（工具的"出厂评级"）
	MinRisk       Risk            // 达到该等级的工具一律要批
	CostLimitUSD  float64         // 单次操作预估成本超过它一律要批
	MinConfidence float64         // 置信度低于它一律要批（拿不准就交给人）
}

// NeedsApproval 按"三道独立闸门，任一触发即要批"判定：
// 不可逆 / 高成本 / 低置信——对应概念详解 1.2 的三类介入点。
func (t RuleTable) NeedsApproval(op Operation) bool {
	if risk, ok := t.ToolRisk[op.Tool]; ok && risk >= t.MinRisk {
		return true // 工具出厂就是高风险（删数据、发邮件）
	}
	if !op.Reversible {
		return true // 不可逆操作，无论工具评级如何都要批
	}
	if op.EstimatedCost > t.CostLimitUSD {
		return true // 花钱的操作按预算线批
	}
	return op.Confidence < t.MinConfidence // 模型自己都没把握，交给人
}

func main() {
	policy := RuleTable{
		ToolRisk: map[string]Risk{
			"search_web":   RiskLow,
			"write_draft":  RiskMedium,
			"delete_rows":  RiskHigh,
			"send_email":   RiskHigh,
			"billing_call": RiskMedium, // 出厂中风险，但可能被成本线拦下
		},
		MinRisk:       RiskHigh,
		CostLimitUSD:  1.0,
		MinConfidence: 0.6,
	}

	ops := []Operation{
		{Tool: "search_web", Reversible: true, Confidence: 0.95},                     // 只读高置信：放行
		{Tool: "delete_rows", Reversible: false, Confidence: 0.99},                   // 高风险工具：批
		{Tool: "write_draft", Reversible: true, EstimatedCost: 5.0, Confidence: 0.9}, // 超成本线：批
		{Tool: "write_draft", Reversible: true, Confidence: 0.3},                     // 低置信：批
		{Tool: "write_draft", Reversible: true, Confidence: 0.9},                     // 可逆+便宜+有把握：放行
	}
	for _, op := range ops {
		fmt.Printf("%-12s 可逆=%v 成本=%.1f 置信=%.2f → 需审批=%v\n",
			op.Tool, op.Reversible, op.EstimatedCost, op.Confidence, policy.NeedsApproval(op))
	}
}
```

**取舍与生产注意**：

- 规则表与 planner 标记不是替代关系是**叠加关系**：planner 标记覆盖"这个具体子任务语境上危险"（代码想不到的），规则表兜住"这类操作无论如何要批"（模型漏标的）。两者任一触发即批——安全规则取并集，不取交集。
- **未登记的工具按高风险处理**（fail-closed 默认值）：上面示例里查不到评级的工具会落到后三道判据，真实系统应直接要批——新工具上线忘了登记评级，宁可多批几次。
- 规则本身是变更敏感配置：进配置中心、留变更审计（谁、什么时候、把哪条规则放松了）。审批规则被悄悄改松，和被注入攻击是同级的安全事件。
- 批量审批是产品决策密集区（看板全选？按任务批量？），设计要点（部分失败语义、幂等、审计粒度、并发冲突）见参考答案第三节的开放讨论——这里不展开，但面试能讲。

### 3.3 与工具层的结合：审批闸门埋进执行路径

**问题**：项目里审批闸在**子任务粒度**（编排器分发前）。更细一层的演进是**工具调用粒度**：worker 的 ReAct 循环里，模型每次选中有副作用的工具，执行前先过审批策略。放在哪拦截决定了防线有没有缺口——散落在各工具内部，工具作者忘加检查就是漏洞。

**模式**：工具调用的唯一入口（dispatch）处放闸门，查 policy，需要审批则登记审批点、返回哨兵错误，**不执行**。

```go
// 教学示例：把审批检查埋进工具执行路径——dispatch 前的审批闸门。
// 模式要点：审批检查放在工具调用的唯一入口，而不是散落在各工具内部——
// 工具作者想绕都绕不开（默认安全，fail-closed）。
package main

import (
	"context"
	"errors"
	"fmt"
)

// ErrWaitingHuman 与项目 orchestrator.ErrWaitingHuman 同构：
// 闸门用哨兵错误把"暂停"信号一路传回调用方，
// 调用方用 errors.Is 区分"让出等审批"与"真实失败"。
var ErrWaitingHuman = errors.New("toolgate: 操作已暂停，等待人工审批")

// Tool 是可执行工具的最小抽象。
type Tool interface {
	Name() string
	Run(ctx context.Context, args string) (string, error)
}

// Policy 决定一次调用是否需要审批（进阶 3.2 的规则表就是一种实现；
// 这里简化为函数，保持示例自足）。
type Policy func(toolName, args string) bool

// Recorder 登记"谁在等批"。真实实现必须落库（本章核心论点：内存等审批
// 进程一重启就丢）；这里用接口隔离，演示用内存实现。
type Recorder interface {
	Park(toolName, args string) error
}

// Gate 是审批闸门：所有工具调用从它进入。
type Gate struct {
	Policy   Policy
	Recorder Recorder
}

// Dispatch 是工具执行的唯一入口：先查 policy，需要审批则登记审批点并
// 返回 ErrWaitingHuman——不执行工具。恢复执行是"审批落库后重新发起调用"
// （事件驱动），而不是这里阻塞等待。
func (g *Gate) Dispatch(ctx context.Context, t Tool, args string) (string, error) {
	if g.Policy(t.Name(), args) {
		if err := g.Recorder.Park(t.Name(), args); err != nil {
			return "", fmt.Errorf("登记审批点失败: %w", err)
		}
		return "", ErrWaitingHuman
	}
	return t.Run(ctx, args)
}

// ---------- 演示 ----------

type tool struct {
	name string
	run  func(ctx context.Context, args string) (string, error)
}

func (t tool) Name() string { return t.name }
func (t tool) Run(ctx context.Context, args string) (string, error) {
	return t.run(ctx, args)
}

type memRecorder struct{ parked []string }

func (m *memRecorder) Park(name, args string) error {
	m.parked = append(m.parked, name)
	return nil
}

func main() {
	highRisk := map[string]bool{"delete_rows": true, "send_email": true}
	gate := &Gate{
		Policy:   func(name, _ string) bool { return highRisk[name] },
		Recorder: &memRecorder{},
	}
	ctx := context.Background()

	search := tool{"search_web", func(_ context.Context, q string) (string, error) {
		return "搜索结果：" + q, nil
	}}
	deleteRows := tool{"delete_rows", func(_ context.Context, _ string) (string, error) {
		return "已删除 90 天前的数据", nil // 这行在演示里不应被执行到
	}}

	out, err := gate.Dispatch(ctx, search, "本周业务数据")
	fmt.Printf("search_web → %q, err=%v\n", out, err)

	out, err = gate.Dispatch(ctx, deleteRows, "older_than=90d")
	switch {
	case errors.Is(err, ErrWaitingHuman):
		fmt.Println("delete_rows → 已暂停等审批（哨兵错误），worker 未执行")
	case err != nil:
		fmt.Println("delete_rows → 真实失败:", err)
	default:
		fmt.Println("delete_rows → 已执行:", out)
	}
}
```

**取舍与生产注意**：

- **粒度对比**：子任务粒度（项目现状）实现简单、审批次数少，但"一个子任务内模型自主调了三次高风险工具"拦不住；工具粒度拦得住，但审批次数增多、且要把 worker 的 ReAct 循环也做成可暂停-恢复的（对话历史 checkpoint，第 9 章同款思想）。实务上两层可以共存：子任务粒度管"这类活要不要干"，工具粒度管"这一步要不要动手"。
- **policy 求值本身出错时必须默认要批**（fail-closed），绝不能"查表失败就当不用批"——安全逻辑的默认方向永远是收紧。
- 恢复路径是"决定落库后重新发起调用"，所以工具执行必须幂等（第 9 章幂等键）——审批前后可能发生重放，副作用不能翻倍。

---

## 四、面试视角

> 以下每题给"标准回答 → 追问链 → 加分点"。自测方法：不看回答口述，再对照差距。阶段文档 3.1 Q6 是压缩版，这里是展开版。

**Q1：HITL 的暂停-恢复怎么实现？**

标准回答：状态机加一个 `waiting_human` 状态。编排器执行高风险子任务前把它迁到 `waiting_human` 并落盘（一次普通 checkpoint），然后**让出**——`Run/Resume` 带哨兵错误返回，不阻塞任何 goroutine。人工通过 API 提交 approve/reject，服务端把决定写进 checkpoint（approve：迁回 pending 并消费审批旗标；reject：迁 failed 记驳回原因），调用方再调 `Resume` 从断点续跑。恢复复用的就是崩溃恢复那条路径。

追问链：

- "审批期间这个任务占什么资源？" → 不占执行资源：没有阻塞的 goroutine、没有内存等待者，只有 DB 里一行状态记录。事件驱动恢复，审批发生前任务对系统零负担。
- "审批决定本身要不要落盘？" → 要，而且它就是审计依据。"已批未执行"跨进程重启必须有效——决定不落盘，重启后同一件事会让人批第二次，甚至第一次批准被静默吞掉。
- "多实例部署时，审批请求怎么路由到'正在等'的那个实例？" → 不需要路由：没有实例在等。状态在 DB，任意实例都能 LoadTask、校验、落决定、触发 Resume。这是"进程无状态"的直接红利。

加分点：主动区分**让出（yield）与阻塞（block）**，并指出让出后恢复路径与崩溃恢复是同一条代码——说明理解了"审批等待"和"进程崩溃"在系统眼里是同一件事：一个停在某个 checkpoint 上的状态。

**Q2：什么节点该让人介入？全部操作都审批行不行？**

标准回答：三类——不可逆（删数据、发邮件）、高成本（付费 API 超预算线）、低置信（模型自评/critic 评分过低）。全部审批不行：介入的代价是延迟加人力，审批点必须像异常一样稀缺。审批率一高就出现审批疲劳和自动化偏见——审批人第 150 条开始无脑点同意，防线形同虚设。设计目标是把审批率压到个位数百分比，让每次审批都值得认真看。

追问链：

- "怎么把审批率压下来？" → 把可逆操作的风险降下去（dry-run、软删除、沙箱），从源头减少"必须批"的量；规则表只拦真正高风险的类别；统计每个规则的触发率，触发率高而驳回率近零的规则说明太宽，该收紧。
- "低置信怎么量化？" → critic 评分、模型自评置信度、计划校验的勉强通过（比如重试到最后一次才过校验），任一低于阈值就升级为人工。本质是"系统自己知道什么时候自己不靠谱"。

加分点：提到**防御深度**——审批点和工具权限收敛（只读凭证、scoped token）、副作用可逆化（软删除）是叠加关系，不是有了审批就万事大吉。

**Q3：为什么不能用内存 channel 等审批？**

标准回答：三个独立理由，任一都致命。① **进程重启丢失**：worker goroutine 里 `<-ch` 阻塞等审批，进程一死，channel 和等它的 goroutine 一起蒸发，"这个子任务在等批"这件事本身丢了——不只是决定丢了，是"需要审批"这个事实丢了。② **资源占用**：每个等待中的任务占着一个 goroutine 和 worker pool 的并发额度，审批积压 = 执行容量被空耗。③ **多实例路由不到**：负载均衡后面有 N 个实例，审批请求到达的实例未必是持有 channel 的那个——状态在谁内存里，请求就必须路由到谁，这是有状态服务的全部麻烦。

追问链：

- "那我用 Redis/MQ 存审批事件行不行？" → 可以当**传输层**（让恢复更快触发、跨实例通知），但真相源必须是 DB 里的状态与决定：MQ 消息消费完就没了，Redis 有淘汰策略，都不能回答"重启后这个子任务到底能不能跑"。判断标准就一条：把消息队列整个停掉，系统还能不能从 DB 恢复全部审批现场？能，说明架构对了。
- "决定落盘和状态迁移先写哪个？" → 先迁状态（真相源），后写审计。反过来中途失败会留下"有审计但状态没动"的假象——审计可以补记，状态错了系统直接做错事。

加分点：把答案收束到一句话——"内存 channel 方案的本质错误是让**流程状态**有了第二个存放地，而且是最脆弱的那个"。

**Q4：approve 之后这个子任务就免检了吗？**

标准回答：不是。approve 的语义是**一次性放行令牌**：批准的是"这一次执行"，落盘时消费掉审批旗标（`requires_approval` 清零），子任务迁回 pending 等 Resume 重跑。如果做成永久豁免，一次批准就成了永久通行证，违背"每次执行前都该能拦"的初衷。配套设计：reject 刻意保留旗标，`failed && RequiresApproval` 这个组合让编排器区分"人工驳回"（终局决定，不重排队）和"执行失败"（正常重试）——一个布尔编码三种售后状态。

追问链：

- "approve 后执行又失败了怎么办？" → 旗标已被消费，它就是一个普通子任务，走正常的失败重试语义（练习 3/4 的机制），不再触发审批。要不要"重试前再批一次"是产品决策，工程上两种都实现得出来——关键是这个选择是显式的，不是旗标生命周期的副作用。
- "两个审批人同时批同一个子任务？" → 先到的生效，后到的在"必须在 waiting_human"校验处报错。真相源收敛在状态机守卫里，并发冲突自然消解，不需要额外分布式锁。

**Q5：审批状态表和任务状态表为什么要分开？**

标准回答：两个表回答两个不同问题。`subtask.status` 是流程真相源，回答"现在能不能跑"，所有流程判断只看它，迁移走状态机守卫；`approvals` 是审计日志，回答"谁在什么时候批了什么"，只 INSERT 不参与流程判断。合一的代价是双写不一致：两处都记流程状态，没有跨表事务就必有窗口期，而这种 bug 是静默的——流程和审计各说各话，你不知道该信谁。只留一个真相源，不一致从根上消失。

加分点：能举出审计独立后的具体用途——合规复盘（这个删除操作谁批的）、SLA 监控（`system:timeout` 占比 = 审批人力健康度）、规则评审（哪条规则触发率高但驳回率零，说明太宽）。

---

## 五、常见坑

1. **审批状态只放内存**。`map[string]chan bool` 一记，进程重启审批点全丢，"已批未执行"的任务永远卡在 `waiting_human`。这是本章第一大坑，1.4 节整节在拆它——判断方法：kill 进程重启，审批现场还在不在。
2. **放行语义做成永久豁免**。approve 一次后旗标不清，Resume 被闸再次拦截死循环；或者反过来"approve 永久免检"，一次批准变永久通行证。正确语义是**一次性放行令牌**：approve 消费旗标，reject 保留旗标（供区分驳回与执行失败）。
3. **waiting_human 无超时机制**。审批人休假，任务永远悬挂——不是终态、不占执行资源，但占看板、占 `ListResumable`，还让用户以为"在跑"。必须有超时兜底（进阶 3.1），且超时默认**拒绝**不放行。
4. **审批缺审计记录**。只迁状态不留 `approvals` 行，或留了行但没记 `decided_by`——出了事故第一问"谁批的"答不上来，整个审批体系的价值归零。审计粒度 = 决定粒度：一批批 N 个就写 N 行，绝不合并。
5. **把 `ErrWaitingHuman` 当普通失败**。在 dispatch/pool 层吞掉它、记 error 日志、或把子任务迁 failed——让出语义消失，任务被误判失败。哨兵错误的纪律：要么 `errors.Is` 识别后走审批路径，要么原样向上传。

---

## 六、动手练习

**练习 5：HITL 审批点**（对应三处 TODO，一起做成本练习）：

1. `stage-03-multi-agent/internal/hitl/hitl.go:106` 的 `TODO(练习5)`：实现 `Pending`（待审批聚合，真相源是 task.Store）与 `Decide`（决定落盘：先迁状态后写审计）；
2. `stage-03-multi-agent/internal/orchestrator/orchestrator.go:186` 的 `TODO(练习5)`：审批闸 + dispatch 四种状态筛选 + 让出检查 + Resume 补齐，共四处增量；
3. `stage-03-multi-agent/cmd/hitl-demo/main.go:96` 的 `TODO(练习5)`："续跑 or 新跑"入口 + 审批循环。

前置补丁：练习 2 的子任务迁移表加 `pending→waiting_human`、`waiting_human→pending` 两条边（一行一个枚举值）。

验收（三条手动路径都要真跑）：

```bash
go test ./internal/hitl/                                            # 全流程测试
printf 'a\n' | go run ./cmd/hitl-demo --db /tmp/hitl.db             # approve：任务 done
printf 'r\n' | go run ./cmd/hitl-demo --db /tmp/hitl-r.db           # reject：部分失败 done
# 路径三：跑到审批提示时 Ctrl-C，再执行同一条命令 —— 从 waiting_human 现场续跑
```

加分项：审批超时自动驳回（`ExpireStale` 模式，进阶 3.1 给了模式级教学代码——项目内的完整落地版在参考答案里）。

参考答案：`docs/solutions/stage-03/exercise-5-hitl-approval.md`（**完成并自评后再看**）。

---

## 本章小结

- HITL 是 prompt 注入三层防御中最硬的一层：前两层求模型别犯错，这层不依赖模型配合。
- 介入点只设三类：不可逆、高成本、低置信；审批点要像异常一样稀缺，警惕审批疲劳。
- 暂停-恢复 = 状态机的直接应用：`waiting_human` 落盘 → 让出（不是阻塞）→ 审批事件驱动 Resume。
- 审批决定是状态，必须落盘：进程重启后"已批未执行"必须能继续——这是编排引擎配数据库的最直接理由。
- 真相源（task.Store）与审计（approvals 表）分离；approve 是一次性放行令牌，reject 保留旗标。
- `ErrWaitingHuman` 哨兵错误把"让出"穿透调用栈：签名零改动、`errors.Is` 区分、与错误词汇表一致。

下一章：[第 12 章：可观测性与 MCP](12-observability-and-mcp.md)——任务跑起来了，接下来要看得见它：嵌套 trace 对应 agent 层级、token 成本归因到每个子任务，以及把工具以 MCP 协议暴露给生态。
