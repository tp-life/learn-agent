# 第 9 章：任务状态机与崩溃恢复——进程会死，状态不死

> 对应阶段：阶段三（深入·多 Agent 编排与生产化）· 项目 3 `stage-03-multi-agent/`
> 代码位置：`stage-03-multi-agent/internal/task/`（本章精讲；练习骨架，对应练习 2）
> 前置：第 1 章（"对话历史即状态"）、第 8 章（Go 并发编排）
> 学完后你能讲清：为什么多 Agent 系统必须把状态机每次迁移落盘、checkpoint 到底存什么、幂等键如何让"重试"与"崩溃恢复"共用一套机制、SQLite 在这个位置为什么是对的选择——以及亲手做一次 kill 进程→重启→续跑的崩溃演练。

---

## 本章地图

- 为什么需要持久化：长任务 + 进程必死 = 不持久化就是烧钱重来
- 状态机设计：六状态枚举 + 显式迁移边，为什么迁移必须是"事件"
- checkpoint 思想：每完成一步就存一步；恢复 = 读盘续跑
- checkpoint 存什么：逐项对应"恢复时要回答的问题"
- 幂等键：重试与崩溃恢复共用的判重机制
- 失败处理四件套：重试、幂等键、降级、死信
- SQLite 选型：嵌入式、事务、`SetMaxOpenConns(1)` 的原因
- 思想串联：状态外置、进程无状态——第 1 章的系统层放大
- 代码精讲：`internal/task` 骨架逐类型拆解 + TODO(练习2) 导读
- 进阶：迁移表驱动状态机、崩溃恢复教学实现、崩溃演练方法论、存储选型讨论

---

## 一、概念详解

### 1.1 为什么需要持久化：进程会死，状态不能死

第 1 章的单 Agent 对话是**秒级**的：一次问答几秒到十几秒，进程活着的概率接近 100%，状态放内存里没问题。多 Agent 任务完全是另一个量级：

```
planner 分解（几十秒）→ N 个 worker 并行执行（每个几十秒到几分钟）
  → critic 评审打回重做（可能循环几轮）→ 等人工审批（可能几小时）
```

一个任务跑几分钟到几小时，在这段时间里进程死亡不是小概率事件，而是**日常**：代码崩溃、发版重启、OOM 被操作系统杀掉、云厂商回收抢占式实例。任务跑得越久，撞上死亡点的概率越大。

不持久化的代价是双份的：

1. **前功尽弃**：已完成的 5 个子任务全部重跑——它们烧掉的 token 是真金白银，重跑等于把账再付一遍；
2. **副作用可能无法重来**：已发出的邮件、已创建的工单，"再执行一遍"不是回到原点，而是翻倍制造事实。

所以长任务系统里，"状态放在哪"是第一个架构决策，先于"用几个 agent、什么编排模式"。本章的答案：**状态外置到 SQLite，进程本身无状态**。

### 1.2 状态机设计：状态枚举 + 显式迁移边

任务的一生用六个状态表达（`task.go:36-43`，任务与子任务共用）：

| 状态 | 含义 | 谁在用 |
| --- | --- | --- |
| `pending` | 已创建，未开始 | 任务、子任务 |
| `planning` | planner 正在分解子任务 | 仅任务级 |
| `running` | 正在执行 | 任务、子任务 |
| `waiting_human` | 暂停等人工审批 | 任务、子任务（第 11 章 HITL） |
| `done` | 终态：成功 | 任务、子任务 |
| `failed` | 终态：失败（重试耗尽或不可重试错误） | 任务、子任务 |

状态只是静态的一半；另一半是**迁移边**——哪些状态允许变成哪些状态：

```
pending ──▶ planning ──▶ running ──▶ done
   │            │           │  ▲
   ▼            ▼           ▼  │
 failed ◀───────┴──── waiting_human（批准则迁回 running，拒绝则 failed）
```

为什么强调"每次迁移都是一次显式事件"，而不是随手改一个字段？三个回报：

- **可审计**：每次迁移带时间戳落盘（`updated_at`），"任务什么时候卡住的、卡在哪个状态"一查便知，不用猜；
- **当前态可审计**：注意本实现是**原地 UPDATE 的当前态快照**（没有事件表），迁移历史本身不落盘——能查出"什么时候卡在哪个状态"，但完整的"人生履历"要靠日志或事件表补齐；把迁移流落成事件表就是事件溯源的方向（Q3 加分点展开）；
- **非法迁移变成显式错误**：`pending` 直接跳 `done`、终态再迁出——这类"不可能的跳转"一旦出现就是 bug，状态机守卫让它在发生的那一刻报错，而不是留下一笔谁也看不懂的脏状态。

工程落点：迁移合法性用一张**数据表**（`map[Status][]Status`）集中表达，而不是把 `switch` 散在各处。数据驱动的好处：合法边一目了然、加边只改一处、可以直接打印成文档或图。进阶 3.1 会给出一个可运行的最小实现。

### 1.3 checkpoint 思想：每完成一步就存一步

checkpoint 的核心纪律一句话：**不是任务结束才存，而是每完成一步就存一步**。

为什么？进程死亡点不可预测。"任务结束才落盘"等价于赌"任务跑完之前进程不会死"——任务越长赌输概率越大，而且输一把就是全丢。每步落盘则把**最大丢失窗口**压缩到"当前正在执行的那一个子任务"：死了大不了重跑这一个，前面的产出都在盘上。

成本账也算得过来：SQLite 一次单条 UPDATE 是微秒级，而一步子任务里的一次 LLM 调用是几十秒、几千 token——**用廉价的确定性（磁盘写）换昂贵的确定性（不重复烧 token）**。恢复粒度 = checkpoint 粒度：存得越频繁丢得越少，代价是写放大；对"几十秒一步"的 Agent 任务，每步一次落盘的写放大完全可以忽略，这个权衡没有悬念。

崩溃恢复的完整流程：

```
进程重启 → ListResumable（找所有非终态任务）
        → LoadTask（读出任务 + 全部子任务的 checkpoint）
        → 子任务按状态分类：done 跳过 / running 被打断的重置重跑 / pending 正常跑
        → 任务从断点继续，直到终态
```

阶段文档 `docs/stages/stage-03-multi-agent-production.md` 3.3 节的时序图里，每条 `═══` 线就是一次 checkpoint 落盘——**状态机每迁移一次就持久化一次，这是崩溃恢复的全部秘密**。

### 1.4 checkpoint 存什么

设计 checkpoint 内容的方法论：列出"重启后要接着跑，必须回答哪些问题"，每个问题对应一个要存的字段。

| 存什么 | 恢复时要回答的问题 |
| --- | --- |
| 任务 ID、目标（goal） | 这是哪个任务、要干什么 |
| 任务状态机当前状态 | 任务走到哪一步了，该从哪继续 |
| 每个子任务的状态 | 哪些干完了（跳过）、哪些没干（重跑）、哪些干到一半（重置重跑） |
| 每个子任务的输入（prompt） | 重跑时拿什么喂给 worker |
| 每个子任务的输出（output） | 已完成子任务的产出——不存就得重跑一遍才能拿结果 |
| 每个子任务的幂等键 | "这活的副作用到底发生没发生"的判重依据（1.5 节） |
| 已耗 token（子任务级 + 任务总账） | 成本观测、预算熔断（超支直接 failed） |
| 执行次数 attempts | 重试上限判断：attempts 到顶进死信 |
| 待审批项（requires_approval + waiting_human） | 恢复后"还在等人批准"的任务要重新挂起等审批（第 11 章） |

和第 1 章对照着看：`messages` 数组是**会话级**状态（让单个 agent"想起"对话）；checkpoint 是**系统级**状态（让整个编排系统"想起"任务）。本项目里子任务的 `prompt` + `output` 合起来就承担了"重建 worker 上下文"的角色——worker 是复用的 mini-agent 内核，恢复时把 prompt 重新喂给它即可，不需要保存它内部的逐轮 messages。

### 1.5 幂等键：重试与崩溃恢复共用的机制

**幂等**（idempotent）：同一个操作执行多次的效果 = 执行一次。GET 请求天然幂等；"创建工单"天然不幂等——调两次建两张单。

崩溃恢复和最普通的重试，本质是**同一个问题**："同一份工作可能被执行第二次"。网络超时重发一次 LLM 调用、进程重启重放一个子任务，判重依据是同一个——**幂等键**：

```
执行前先查："这个 key 成功过吗？"
  ├─ 成功过 → 直接返回旧结果，不再真执行
  └─ 没成功过 → 真执行，把结果登记在这个 key 下
```

key 的构造惯例：`taskID + subtaskID (+ opType)`，全系统唯一。本项目里它随子任务一起落盘（`Subtask.IdempotencyKey`，`task.go:63`），恢复逻辑靠它判断跳过，写入路径上"已完成就直接返回"做最后防线——**读路径判重 + 写路径幂等，双保险**。

没有幂等键的后果要往最坏想：子任务"发邮件给客户"，崩溃发生在"邮件已发出、checkpoint 还没来得及标 done"之间——恢复后重放，客户收到第二封。**有副作用的子任务没有幂等 = 恢复时副作用翻倍**，这是面试和生产的双重高压线。

### 1.6 失败处理四件套

子任务会失败，系统要有成体系的处置，而不是"报错然后没了"：

1. **重试**：429/5xx/网络抖动值得重试（第 3 章的指数退避在调用层已学过），但**重试以幂等为前提**——不幂等的操作重试就是制造重复副作用；
2. **幂等键**：重试与恢复共用的判重机制（1.5 节），是四件套的地基；
3. **降级**：主模型持续超时/限流就换便宜模型，critic 连续不可用就跳过评审直接放行（记日志）——有计划的牺牲质量保可用；
4. **死信**：重试 N 次仍失败的子任务标记 `failed` 进死信，不拖垮整个任务，最后在汇总报告里呈现给人看。

`attempts` 字段（`task.go:65`）就是"N 次"的数据源：每次迁入 `running` 自增 1，重试策略读它决定"再试一次"还是"进死信"。

### 1.7 存储选型：为什么是 SQLite

checkpoint 存哪？本项目选 SQLite（`go.mod:9` 的 `modernc.org/sqlite v1.56.0`），理由是四条的交集：

- **嵌入式零运维**：一个文件就是数据库，没有"先装个 Postgres、配用户、开端口"的门槛——学习项目与单机部署场景的最优解；
- **事务**：状态迁移往往涉及多行写入（子任务标 done + 任务总账累加 token），事务保证要么全成要么全不成；JSON 文件写到一半崩溃就是损坏状态；
- **查询方便**：看板要"按状态过滤任务"，恢复要"找所有非终态任务"，SQL 一行搞定；JSON 文件得全量读进内存自己过滤；
- **纯 Go 免 CGO**：`modernc.org/sqlite` 替代的是 `mattn/go-sqlite3` + cgo 工具链——换来交叉编译零配置，代价是极端写入吞吐略低，对中小规模完全够用（`task.go:25-28` 的注释写明了这笔交换）。

一个必须知道的坑位设计：Open 里有一行 `db.SetMaxOpenConns(1)`（`task.go:88`）。SQLite 是**单写者模型**——同一时刻只允许一个写事务；而 `database/sql` 默认是连接池，多个 goroutine 并发写时会各自拿连接去抢写锁，撞 `SQLITE_BUSY` 报错。把最大连接数钉成 1，让所有写操作在同一个连接上天然串行化，简单且够用。读多写少、写都在主流程上的 checkpoint 场景，这个串行化不是瓶颈（每次写微秒级）。

### 1.8 思想串联：状态外置，进程无状态

回看整条学习线，本章是第 1 章思想在系统层的放大：

- **第 1 章**：LLM API 无状态 → "对话历史即状态"——记忆不在模型里，在每轮全量重发的 messages 里；
- **本章**：进程会死 → "checkpoint 即状态"——任务的命不在进程里，在每次迁移落盘的 SQLite 里。

同一个模式两个尺度：**把易失的东西（模型记忆、进程内存）变成可重建的东西，把状态放到比它更长寿的存储里**。这也是云原生十二要素里 "processes are stateless" 原则在 Agent 系统的具体化。后面第 11 章 HITL 的"暂停-恢复"之所以可能，全靠状态外置：如果审批等待是内存里一个阻塞的 channel，进程一重启审批点就永远丢了。

---

## 二、代码精讲

`stage-03-multi-agent/internal/task/` 只有一个文件 `task.go`（约 210 行），是练习 2 的骨架：类型定义、建表迁移、Open/Close 已写好；**状态机守卫与 8 个 checkpoint 方法是 TODO(练习2)**。逐块看设计——每个设计对应第一节的哪个概念，看完你就有了实现练习的完整图纸。

### 2.1 包注释即设计文档（`task.go:1-16`）

包注释把全章核心说完了，值得精读三遍：

```go
// 设计核心（面试高频考点）：**状态外置，进程无状态**。
// 阶段一学过"对话历史即状态"——agent 的记忆不在进程里，而在每次请求都全量重放的
// messages 里；本包是同一个思想在系统层的放大：任务/子任务的全部状态都在 SQLite 里，
// 进程本身不持有任何不可丢失的东西。**状态机每迁移一次就落盘一次（checkpoint）**，
// 崩溃恢复 = 重启后 ListResumable 找回未完成任务 → LoadTask 读出 checkpoint →
// 跳过已 done 的子任务续跑。
```

注意注释末尾的分工说明："类型定义、建表迁移与 Open/Close 无需用户完成；状态机守卫与各 checkpoint 方法为 TODO(练习2)"——骨架给的是"存储的壳"，你要写的是"状态机的魂"。

### 2.2 状态枚举（`task.go:34-43`）

```go
type Status string

const (
	StatusPending      Status = "pending"       // 已创建，未开始
	StatusPlanning     Status = "planning"      // planner 正在分解子任务（仅任务级使用）
	StatusRunning      Status = "running"       // 正在执行
	StatusWaitingHuman Status = "waiting_human" // 暂停等人工审批（HITL，练习5 用）
	StatusDone         Status = "done"          // 终态：成功
	StatusFailed       Status = "failed"        // 终态：失败（重试耗尽或不可重试错误）
)
```

三个设计细节：

- **任务与子任务共用一组状态**（`task.go:32-33` 注释）：两层的状态机语义完全一致（等待→执行→终态/等人），共用一组常量能复用同一张迁移表思路，看板查询 SQL 也不用写两遍；
- **`string` 而不是 `int` 枚举**：落库、日志、看板 API 里直接可读——排查故障时 `"running"` 比 `2` 友好得多，这是 Agent 系统"可观测优先"的缩影；
- **`planning` 仅任务级使用**：子任务没有"被规划"这个阶段，它是规划的产物——同一组枚举不代表每个状态对两层都有意义，迁移表会表达这个差异（练习里体会）。

### 2.3 Task 与 Subtask：checkpoint 的数据形状（`task.go:46-67`）

```go
type Task struct {
	ID          string
	Goal        string
	Status      Status
	TotalTokens int // 全任务累计 token 消耗，用于成本观测与预算熔断
	CreatedAt   time.Time
	UpdatedAt   time.Time // 每次状态迁移刷新，"任务是不是卡死了"看它就够
}

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
```

把字段和 1.4 节那张表逐行对一遍：**每个字段都回答一个恢复/运维问题**，没有一个是"先存着也许有用"。特别看两个：

- `UpdatedAt`：每次迁移刷新。"任务卡没卡死"不需要心跳机制，看这个字段够不够旧就够——简单问题用简单数据回答；
- `Attempts`：重试上限判断（四件套的"死信"）就靠它，所以"迁入 running 自增"这条规则写在 struct 注释里，实现时别漏。

`Subtask` 注释（`task.go:55`）说它是"崩溃恢复的最小粒度"——checkpoint 落盘、幂等判重、恢复跳过，全部以子任务为单位进行。

### 2.4 Store 与 Open：存储的壳（`task.go:71-122`）

```go
type Store struct {
	db *sql.DB
}
```

`Store` 只有一个 `*sql.DB` 字段——没有任何内存缓存、没有"当前任务"指针。这就是"进程无状态"的物证：进程随便重启，Store 关掉再打开，状态分毫不丢。

`Open`（`task.go:83`）做了三件事：打开数据库、`SetMaxOpenConns(1)`（`task.go:88`，原因见 1.7 节）、执行建表迁移（`task.go:93-116`）。迁移 SQL 里两个细节：

```sql
PRIMARY KEY (task_id, id) -- 子任务 ID 只在任务内唯一，联合主键
```

子任务主键是 `(task_id, id)` 联合键：子任务 ID 是 planner 输出的局部编号（s1/s2/s3），只在任务内唯一，全局唯一性由联合键保证——这也给幂等键 `taskID:subID` 提供了唯一性背书。

另一个是 `IF NOT EXISTS`：重复 Open（崩溃恢复场景）时迁移幂等。注释（`task.go:90-92`）同时点破了它的局限——无法表达"改列"，真实项目用 golang-migrate 这类版本化迁移工具。**学习项目够用，但要知道边界在哪**——面试官追问"schema 演进怎么办"时这就是答案。

`Close` 的注释（`task.go:124-125`）也值得一读："即使不调用（崩溃），已提交的 checkpoint 也不会丢——这正是设计目标。"

> **database/sql 三分钟速查**（没写过 Go SQL 的话，做练习 2 前先看这里）。SQLite 的占位符是 `?`（Postgres 是 `$1`），三个动作覆盖练习 2 的全部需求：
>
> ```go
> // ① 写：Exec 无结果集；返回的 error 别丢。
> _, err := db.Exec(`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`,
> 	"done", time.Now(), taskID)
>
> // ② 查单行：QueryRow + Scan；sql.ErrNoRows 是"没找到"，属业务语义，不是系统错误。
> var status string
> err := db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status)
> if errors.Is(err, sql.ErrNoRows) { /* 任务不存在 */ }
>
> // ③ 查多行：rows 必须 Close，迭代完必须查 rows.Err()。
> rows, err := db.Query(`SELECT id FROM tasks WHERE status = ?`, "pending")
> if err != nil {
> 	return nil, err
> }
> defer rows.Close()
> for rows.Next() {
> 	var id string
> 	if err := rows.Scan(&id); err != nil {
> 		return nil, err
> 	}
> 	ids = append(ids, id)
> }
> return ids, rows.Err() // 迭代中途的断连错误在这里暴露
> ```
>
> 三条纪律：error 不丢；`ErrNoRows` 单独处理；`rows` 一定 `Close` + 迭代完查 `rows.Err()`——第三条漏了，连接会悄悄泄漏。

### 2.5 TODO(练习2) 导读：8 个方法即 8 种 checkpoint（`task.go:134-211`）

TODO 块（`task.go:134-160`）给出了完整提示，这里把 8 个方法按"在任务一生中的位置"串起来理解——**每个方法都是一次 checkpoint，没有一个是普通 CRUD**：

| 方法（位置） | 何时调用 | 它落盘了什么 |
| --- | --- | --- |
| `CreateTask`（`task.go:163`） | 用户提交任务 | 任务诞生，初始 `pending` |
| `Transition`（`task.go:169`） | 任务级每次状态迁移 | 任务状态 + `updated_at`（状态机守卫在这） |
| `SaveSubtasks`（`task.go:181`） | planner 产出计划后 | 整批子任务（初始 `pending`）+ 各自的幂等键 |
| `TransitionSubtask`（`task.go:175`） | 子任务每次状态迁移 | 子任务状态；迁入 `running` 时 `attempts` 自增 |
| `CompleteSubtask`（`task.go:189`） | 子任务执行成功 | output、token（子任务级 + 任务总账）；**必须幂等** |
| `FailSubtask`（`task.go:195`） | 子任务失败 | 错误信息（落在 output 字段）+ `failed` 状态 |
| `LoadTask`（`task.go:201`） | 恢复/看板读取 | 读出任务 + 全部子任务的完整 checkpoint |
| `ListResumable`（`task.go:208`） | 进程重启后第一件事 | 所有非终态任务 ID——崩溃恢复的入口 |

实现时最需要动脑的三处（TODO 提示的展开，不给出实现）：

1. **迁移守卫要先读后写**：守卫依赖当前状态，所以先 `SELECT` 当前状态、查表校验、非法则报错，再做 `UPDATE`——不要在 SQL 里盲改。这对应 1.2 节"非法迁移变成显式错误"。
2. **想一条容易漏的迁移边**：重启后发现停在 `running` 的子任务（执行体已随进程死亡）怎么迁回可重跑状态？TODO 提示（`task.go:146-147`）让你**先写崩溃恢复测试**——测试会替你发现这条边。这是"TDD 逼出设计"的绝佳实例。
3. **幂等落在写路径**：`CompleteSubtask` 发现子任务已是 `done`（幂等键随 `SaveSubtasks` 落盘，说明同一份工作已完成过）就直接返回，且不得重复累加 token（`task.go:186-188`）。崩溃恢复重放到这一步时，靠它保证副作用不翻倍。

验收方式见第六节。

---

## 三、进阶拓展（带代码）

### 3.1 一个最小迁移表驱动的状态机

**为什么值得单独写一遍**：状态机的正确性不该靠"调用方自觉按对顺序"。把合法性收敛成一张数据表 + 一个校验函数，非法迁移在任何调用点都会被同一个守卫拦下。下面的教学示例独立于仓库代码，可直接运行：

```go
package main

import (
	"errors"
	"fmt"
)

// Status 是任务状态。用 string 别名而不是 int 枚举：落库、日志、看板 API
// 里直接可读——排查故障时 "running" 比 "2" 友好得多。
type Status string

const (
	StatusPending      Status = "pending"
	StatusPlanning     Status = "planning"
	StatusRunning      Status = "running"
	StatusWaitingHuman Status = "waiting_human"
	StatusDone         Status = "done"
	StatusFailed       Status = "failed"
)

// transitions 是任务级合法迁移表：key 是当前状态，value 是允许迁到的状态列表。
// 没出现在 key 里的状态（done / failed 两个终态）= 任何迁出都非法。
// 用数据（map）而不是散落的 switch：合法边一目了然，加边只改一处。
var transitions = map[Status][]Status{
	StatusPending:      {StatusPlanning, StatusFailed},
	StatusPlanning:     {StatusRunning, StatusFailed},
	StatusRunning:      {StatusWaitingHuman, StatusDone, StatusFailed},
	StatusWaitingHuman: {StatusRunning, StatusFailed},
}

// IllegalTransitionError 让"非法迁移"成为可识别的错误类型，
// 调用方用 errors.As 就能区分"业务非法"和"存储故障"。
type IllegalTransitionError struct {
	From, To Status
}

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("illegal transition %s -> %s", e.From, e.To)
}

// Machine 是一个最小状态机：只持有当前状态，所有迁移必须过表校验。
type Machine struct {
	current Status
}

func NewMachine() *Machine { return &Machine{current: StatusPending} }

// Transition 尝试迁移：合法则改状态并返回 nil；非法则返回错误、状态不变。
// 真实项目里（task.Store.Transition）这里还要顺手落盘——
// "校验通过 + 落盘成功"合起来才是一次完成的迁移事件。
func (m *Machine) Transition(to Status) error {
	for _, allowed := range transitions[m.current] {
		if allowed == to {
			m.current = to
			return nil
		}
	}
	return &IllegalTransitionError{From: m.current, To: to}
}

func (m *Machine) Current() Status { return m.current }

func main() {
	m := NewMachine()

	// 合法路径：pending -> planning -> running -> done
	for _, to := range []Status{StatusPlanning, StatusRunning, StatusDone} {
		if err := m.Transition(to); err != nil {
			fmt.Println("unexpected:", err)
		}
	}
	fmt.Println("after legal path:", m.Current())

	// 非法迁移：终态不可迁出，状态保持 done 不变
	err := m.Transition(StatusRunning)
	var ite *IllegalTransitionError
	if errors.As(err, &ite) {
		fmt.Println("rejected:", ite)
		fmt.Println("state stays:", m.Current())
	}
}
```

运行输出：

```text
after legal path: done
rejected: illegal transition done -> running
state stays: done
```

**取舍与生产注意**：

- 迁移表是**数据**，可以遍历打印成文档、画成图、喂给测试做穷举校验（"所有未列出的 from→to 组合都必须报错"）；switch 散落在各方法里时这些都做不到；
- 错误类型化（`IllegalTransitionError`）不是洁癖：调用方要区分"迁移非法"（业务 bug，不该重试）和"DB 写失败"（基础设施抖动，值得重试）——**错误类型是控制流的一部分**，第 3 章重试已经用过这个思想；
- 练习 2 里你要写**两张**这样的表（任务级/子任务级各一），子任务表多两条"回头边"——哪两条、为什么，留给崩溃恢复测试替你回答。

### 3.2 崩溃恢复流程的教学实现

**为什么**：恢复逻辑是"平时不跑、一跑必须对"的代码，值得先在不依赖任何数据库的沙盘里推演一遍。下面的教学示例用内存切片模拟 checkpoint（真实项目里这些行在 SQLite），用一个"幂等登记表"模拟外部系统，完整演示 1.3 节的恢复流程：

```go
package main

import (
	"errors"
	"fmt"
	"strings"
)

// ---- checkpoint 里的数据（真实项目里是 SQLite 的行，这里用切片模拟）----

type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
)

// Subtask 是崩溃恢复的最小粒度。状态、产出、幂等键三样缺一不可：
// 没有 Status 不知道干没干完；没有 Output 干完了也白干（得重跑一遍拿结果）；
// 没有 Key 无法判断"副作用到底发生没发生"。
type Subtask struct {
	ID     string
	Title  string
	Status Status
	Output string
	Key    string // 幂等键：taskID + subID，全系统唯一
}

// errInterrupted 模拟"进程在子任务执行中途死亡"：
// checkpoint 里子任务状态停在 running，但外部世界的副作用可能已经发生。
var errInterrupted = errors.New("process died mid-execution")

// Executor 是"干活的人"。sideEffects 模拟外部系统（如工单服务）的幂等
// 登记表：key 成功过 = 副作用已发生。真实项目里这张表在外部系统或你的 DB 里。
type Executor struct {
	sideEffects map[string]string // key -> 该次执行的真实产出
	crashOn     string            // 执行到这个子任务 ID 时模拟崩溃
	crashed     bool
}

func (e *Executor) Run(sub Subtask) (string, error) {
	// 幂等判重：这个 key 成功过就直接返回旧结果，副作用绝不翻倍
	if out, ok := e.sideEffects[sub.Key]; ok {
		return out, nil
	}
	if !e.crashed && e.crashOn == sub.ID {
		e.crashed = true
		// 崩溃最阴险的情形：副作用已登记成功，你却来不及把它标成 done
		e.sideEffects[sub.Key] = "产出:" + sub.Title + "（崩溃前已真实完成）"
		return "", errInterrupted
	}
	out := "产出:" + sub.Title
	e.sideEffects[sub.Key] = out
	return out, nil
}

// run 是正常执行循环：每个子任务"置 running -> 执行 -> 置 done"，
// 每一步都是一次 checkpoint（这里改切片等价于落盘）。
func run(subs []Subtask, ex *Executor) error {
	for i := range subs {
		sub := &subs[i]
		sub.Status = StatusRunning // checkpoint：开始执行
		out, err := ex.Run(*sub)
		if err != nil {
			return err // 进程死了：running 状态留在 checkpoint 里
		}
		sub.Output = out
		sub.Status = StatusDone // checkpoint：产出落盘
	}
	return nil
}

// resume 是崩溃恢复：重启后读 checkpoint，按状态分类处理每个子任务。
func resume(subs []Subtask, ex *Executor) error {
	for i := range subs {
		sub := &subs[i]
		switch sub.Status {
		case StatusDone:
			// 已完成：产出就在 checkpoint 里，直接跳过——省一遍 token
			fmt.Printf("  %s: done，跳过（复用产出 %q）\n", sub.ID, sub.Output)
		case StatusRunning:
			// 被打断：进程已死没有执行体，先显式重置回 pending 再重放。
			// running -> pending 这条迁移边就是为这一幕存在的。
			sub.Status = StatusPending // checkpoint：重置
			out, err := ex.Run(*sub)   // 幂等键在这里挡掉重复副作用
			if err != nil {
				return err
			}
			sub.Output = out
			sub.Status = StatusDone
			fmt.Printf("  %s: running 被打断，重置后重放完成\n", sub.ID)
		case StatusPending:
			// 没跑过：正常执行
			sub.Status = StatusRunning
			out, err := ex.Run(*sub)
			if err != nil {
				return err
			}
			sub.Output = out
			sub.Status = StatusDone
			fmt.Printf("  %s: pending，正常执行完成\n", sub.ID)
		}
	}
	return nil
}

func main() {
	// checkpoint：planner 分解出的三个子任务（初始全 pending）
	subs := []Subtask{
		{ID: "s1", Title: "调研", Status: StatusPending, Key: "t1:s1"},
		{ID: "s2", Title: "写稿", Status: StatusPending, Key: "t1:s2"},
		{ID: "s3", Title: "评审", Status: StatusPending, Key: "t1:s3"},
	}
	ex := &Executor{sideEffects: map[string]string{}, crashOn: "s2"}

	fmt.Println("第一次运行（将在 s2 处模拟崩溃）:")
	if err := run(subs, ex); err != nil {
		fmt.Println("  进程死亡:", err)
	}
	fmt.Println("checkpoint 现场:", statuses(subs))

	fmt.Println("重启恢复:")
	if err := resume(subs, ex); err != nil {
		fmt.Println("  恢复失败:", err)
	}
	fmt.Println("最终状态:", statuses(subs))
	fmt.Println("s2 的产出:", subs[1].Output)
}

func statuses(subs []Subtask) string {
	var b strings.Builder
	for _, s := range subs {
		fmt.Fprintf(&b, "%s=%s ", s.ID, s.Status)
	}
	return b.String()
}
```

运行输出（注意最后一行）：

```text
第一次运行（将在 s2 处模拟崩溃）:
  进程死亡: process died mid-execution
checkpoint 现场: s1=done s2=running s3=pending
重启恢复:
  s1: done，跳过（复用产出 "产出:调研"）
  s2: running 被打断，重置后重放完成
  s3: pending，正常执行完成
最终状态: s1=done s2=done s3=done
s2 的产出: 产出:写稿（崩溃前已真实完成）
```

三个值得停下来想的点：

1. **s2 的产出带着"崩溃前已真实完成"的尾巴**——恢复时幂等键命中了登记表，重放没有真的再执行一遍，副作用没有翻倍。这就是 1.5 节的机制在跑；
2. **`running → pending` 这条"回头边"是被恢复场景逼出来的**：重启后停在 `running` 的子任务没有执行体（进程已死），必须先显式重置才能重跑。练习 2 的 TODO 提示让你先写崩溃恢复测试，就是为了让你在测试里撞上这条边；
3. 恢复循环里 `done → 跳过` 省的不只是时间，是**每一遍重跑都要重新付的 token 费**。

**生产注意**：真实系统里恢复循环在编排器启动时跑一次（`ListResumable` → 逐个 `LoadTask` → 续跑）；`waiting_human` 的子任务恢复时不是"重跑"而是"重新挂起等审批"（第 11 章）；恢复一大批任务时同样要过第 8 章的 semaphore 限流——重启后 100 个任务同时续跑，对 LLM API 就是一次自造的惊群。

### 3.3 崩溃演练方法论

恢复逻辑不演练等于没有。演练分两个层次，都要做：

**层次一：测试里的"假崩溃"**。用临时文件 DB：写入一半 → `Close` → 重新 `Open` → 验证恢复行为。注意必须用**文件库**而不是 `:memory:`——内存库 Close 后数据就没了，演练不了"重启"。这是练习 2 验收里指定的演练方式， CI 里每次都能跑。

**层次二：真进程的"真崩溃"**。把程序跑起来，任务执行中途 `kill -9` 掉，重启看续跑。这里必须分清两种死亡：

| 信号 | 语义 | 进程有机会做什么 |
| --- | --- | --- |
| `SIGTERM` / `SIGINT`（Ctrl-C） | 优雅退出 | Go 程序可以捕获，取消 ctx、给 worker 机会存最后一个 checkpoint、调用 `Close` 收尾 |
| `kill -9`（SIGKILL） | 内核直接杀死 | 什么都做不了——defer 不跑、缓冲不刷，这才是"真崩溃" |

崩溃恢复设计要过的是 `kill -9` 这一关；优雅退出做到的是"死前把现场整理得更好"（比如把 `running` 主动标成可恢复状态），属于锦上添花，不是恢复正确性的前提。**只测 Ctrl-C 不测 kill -9 的演练是不及格的**。

每次演练留一张记录表——它就是练习 9（故障演练报告）的原材料：

| 字段 | 示例 |
| --- | --- |
| 时间/版本 | 2026-08-20，commit abc123 |
| 演练场景 | 任务跑到第 2/3 个子任务时 `kill -9` |
| 预期行为 | 重启后 s1 跳过、s2 幂等重放、s3 正常执行；token 不重复计费 |
| 实际行为 | （如实记录，含意外） |
| 结论与改进 | 通过 / 发现的问题与修复链接 |

### 3.4 讨论：为什么不用 Redis / 内存 + 定期快照

"SQLite 是不是太土了"是常见反问。把三个候选放在同一个坐标系里比——**RPO（恢复点目标：崩溃时最多丢多少）**、事务性、运维成本：

- **内存 + 定期快照**：RPO = 快照间隔。间隔 5 分钟就最多丢 5 分钟工作量——对"一步几十秒、一步几千 token"的任务，这个窗口贵得离谱；且恢复要重放快照之后的日志，复杂度不低于直接落库。它适合"状态重建便宜"的系统，Agent 任务恰好相反。
- **Redis**：快，但它是内存优先的存储——持久化靠 RDB 快照 / AOF 日志，配置不当一样有丢失窗口；多行写的事务语义、按状态过滤的查询便利度都不及关系库；还多一个要部署运维的组件。单进程学习/中小规模生产，属于杀鸡用牛刀。
- **SQLite 每步同步落盘**：RPO ≈ 当前正在执行的那一步，事务保证多行写原子，零运维。代价是每次迁移一次磁盘写——微秒级，相对一步几十秒的 LLM 调用可忽略。

结论：**checkpoint 的写路径就在主流程上，用"每步同步落盘"换"恢复逻辑极简"**。规模上来之后（多进程、多机），SQLite 换 Postgres 是自然演进——Store 的方法集不变，调用方无感（面试 Q6 展开）。

---

## 四、面试视角

> 以下每题给"标准回答 → 追问链 → 加分点"。自测方法：不看回答口述，再对照差距。

**Q1：多 Agent 系统的崩溃恢复怎么设计？checkpoint 存什么？**

标准回答：核心是"状态外置 + 每步落盘"。任务状态机每迁移一次就 checkpoint 一次；恢复 = 重启后 `ListResumable` 找非终态任务 → `LoadTask` 读 checkpoint → 已 done 的子任务跳过、running 被打断的按幂等键判断重放、waiting_human 的恢复审批等待。checkpoint 存：任务 ID 与状态、每个子任务的（状态/输入/输出/幂等键/已耗 token/执行次数）、待审批项。

追问链：
- "为什么每步落盘而不是任务结束才存？" → 死亡点不可预测，结束才存 = 崩溃即全丢；每步落盘把最大丢失窗口压到"当前那一步"。成本上 SQLite 写是微秒级，LLM 一步是几十秒几千 token——廉价的确定性换昂贵的确定性；
- "恢复粒度由什么决定？" → checkpoint 粒度。存得越频繁丢得越少，代价是写放大—— Agent 任务步长几十秒，每步一次的写放大可忽略，权衡没有悬念。

加分点：主动补一句"恢复也要限流——重启后一批任务同时续跑对 LLM API 是自造惊群，要过 semaphore"（第 8 章知识的串联）。

**Q2：重试和幂等是什么关系？**

标准回答：重试是"再做一次"的**动作**，幂等是"做多次 = 做一次"的**性质**。重试以幂等为前提：不幂等的操作重试 = 副作用翻倍（建两张工单、发两封邮件）。落地靠幂等键：执行前查"这个 key 成功过吗"，成功过直接返回旧结果。崩溃恢复和重试是同一个问题——"同一份工作可能被执行第二次"——所以共用幂等键这一套机制。

追问链：
- "幂等键谁来存？" → 两处：自己的 checkpoint（判重跳过）+ 有副作用的外部系统（如支付/工单系统的 dedup 表）；双保险是"读路径判重 + 写路径幂等"；
- "拿不到外部系统的幂等支持怎么办？" → 退而求其次：副作用操作前加人工审批（HITL）、或设计补偿动作（撤销/退款），并在文档里标明这是 at-least-once 语义。

加分点：说出"exactly-once 在分布式系统里基本不存在，工程上都是 at-least-once + 幂等去重"——这是消息队列/支付系统的通用认知，讲出来说明知识面不止 Agent。

**Q3：为什么状态机迁移要落盘，而不是只在内存里维护？**

标准回答：因为进程会死（崩溃、发版、OOM），内存状态的寿命 = 进程寿命，而多 Agent 任务的寿命是分钟到小时级。状态必须放到比进程长寿的存储里——这是"状态外置、进程无状态"，也是 HITL 暂停-恢复（审批可能等几小时）能成立的前提。

追问链：
- "每次都写盘，性能怎么办？" → 算数量级：一次 UPDATE 微秒级，一步子任务几十秒，落盘开销在噪声里；且 SQLite 单写者 + 单连接串行化，没有锁竞争开销；
- "只在内存在性能敏感的系统里怎么做？" → 那是另一个权衡域（如高频交易）；Agent 系统的瓶颈永远是 LLM 调用，不是本地磁盘。

加分点：把迁移落盘和**事件溯源**（event sourcing）挂上钩——状态迁移即事件流，天然可审计、可回放。注意边界：本项目的 Store 只落了**当前态快照**（原地 UPDATE），还不是事件溯源；把每次迁移追加进一张事件表，才是从本章走向事件溯源的那一步。面试里主动讲清这个区别，比含糊引用术语更加分。

**Q4：恢复时发现子任务停在 running，怎么处理？**

标准回答：停在 running = 被崩溃打断（进程已死，没有执行体在跑它）。处理三步：先显式重置回 pending（状态机要有这条迁移边），再按幂等键判断——副作用已发生的直接复用旧结果标 done，没发生的重新执行，成功后照常 complete。

追问链：
- "为什么要有 running→pending 这条边，直接 running→running 不行吗？" → 语义上"开始一次新执行"应该是显式事件，attempts 自增、updated_at 刷新都挂在"迁入 running"上；重置回 pending 再迁入，让"被打断一次 + 重跑一次"在数据里留痕；
- "这条边怎么想到的？" → 诚实答案最加分：写崩溃恢复测试时被 `illegal transition` 报错逼出来的——这就是"先写恢复测试"的价值，不写到真崩溃那天才会暴露。

**Q5：并发写 SQLite 怎么处理？**

标准回答：SQLite 是单写者模型，`database/sql` 默认连接池并发写会撞 `SQLITE_BUSY`。本项目做法：`SetMaxOpenConns(1)` 钉成单连接，让驱动串行化——checkpoint 场景写在主流程上、每次微秒级，串行化不是瓶颈。更多手段：开 WAL 模式提升读写并发、写操作走单队列、重试 `SQLITE_BUSY`。

追问链：
- "什么时候 SQLite 不够用？" → 多进程/多机同时写、写入吞吐真成瓶颈、需要跨实例复制——那时演进到 Postgres；
- "WAL 是什么？" → 预写日志模式：读写不互斥（读不阻塞写），崩溃恢复更快；代价是多两个辅助文件。知道名字和一句话原理即可。

**Q6：以后怎么从 SQLite 平滑演进到 Postgres？**

标准回答：关键是 Store 的**方法集就是接口**——`CreateTask/Transition/CompleteSubtask/LoadTask/ListResumable...` 这一组方法语义与存储无关，调用方（编排器、看板）只依赖方法签名。换 Postgres 时：同样的建表语义换成 Postgres 方言（`TEXT`→`TEXT`、`TIMESTAMP`→`TIMESTAMPTZ`、布尔列不再用 INTEGER）、去掉 `SetMaxOpenConns(1)`（Postgres 天然多写者）、迁移交给 golang-migrate 版本化管理。调用方一行不改。

追问："为什么不一开始就上 Postgres？" → 运维成本与复杂度匹配原则：单进程学习/中小规模，嵌入式零运维是最优解；过早引入外部数据库是过度设计。能讲清"什么时候该换"比"我直接上了 Postgres"更显工程判断力。

---

## 五、常见坑

1. **只在任务结束才落盘**：崩溃 = 全部重来，已烧的 token 和已产生的副作用全白费。checkpoint 的纪律是"每完成一步就存一步"，落盘调用必须挂在每次状态迁移上，而不是 `defer` 在任务结尾。
2. **重试无幂等导致副作用翻倍**：崩溃恰好发生在"副作用已发生、checkpoint 未标 done"之间，恢复重放就把邮件发两遍。凡有副作用的子任务，幂等键先行；写路径上"已 done 直接返回"是最后一厘米的防线。
3. **SQLite 多连接并发写报 `SQLITE_BUSY`**：根因是单写者模型撞上 `database/sql` 的默认连接池。`SetMaxOpenConns(1)` 一行解决；症状是"并发一高就偶发 busy 错误"，别当成随机 bug 重试糊弄过去。
4. **状态迁移无校验**：不做守卫、`UPDATE` 盲改，就会出现 `pending` 直接 `done`、终态再迁出的脏状态——恢复逻辑面对不可能的状态组合无从下手。守卫要落在写路径上（先 SELECT 查表校验再 UPDATE），非法迁移当场报错。
5. **用内存库/纯内存 map 演练恢复**：`:memory:` 库 Close 即蒸发，演练不了"重启后状态还在"。崩溃恢复测试必须用临时文件 DB，真实 Close → 重新 Open。

---

## 六、动手练习

本章对应 **练习 2**：实现 `stage-03-multi-agent/internal/task/task.go` 的 `TODO(练习2)`（`task.go:134-160`）——8 个 Store 方法，让每个状态迁移都成为一次 checkpoint。

要点回顾（详见 TODO 块与 2.5 节）：

1. 合法迁移表用 `map[Status][]Status` 表达，任务/子任务各一张；先 SELECT 查表校验，非法迁移返回显式错误；
2. 想清那条"容易漏的边"——先写崩溃恢复测试，让它替你发现；
3. `CompleteSubtask` 必须幂等：已 done 直接返回，token 不重复累加（子任务级与任务总账都是）；
4. 迁入 `running` 时 `attempts` 自增；每次迁移刷新 `updated_at`；
5. `ListResumable` 只返回非终态任务——它是崩溃恢复的入口。

**验收**（照 TODO 块）：

```bash
cd stage-03-multi-agent
go vet ./internal/task/ && go test ./internal/task/ -count=1
```

测试必须包含崩溃恢复演练：临时文件 DB → 写入一半 → Close → 重新 Open → `ListResumable` 找回任务、已完成子任务的 output 还在、重放 `CompleteSubtask` 不重复累加 token。

**真机演练**（强烈建议，为练习 9 攒素材）：把演示程序跑起来，任务中途 `kill -9`，重启观察续跑；按 3.3 节的记录表写一份演练记录。

完成后再看参考答案：`docs/solutions/stage-03/exercise-2-task-checkpoint.md`（含事务版进阶实现：多行写入的原子性加固）。自测口述三题：为什么每步落盘而不是结束才存？幂等键为什么恢复和重试共用？为什么用 SQLite 而不是 JSON 文件？

---

## 本章小结

- 多 Agent 任务是分钟到小时级的长任务，进程死亡是日常——不持久化 = 前功尽弃 + 重复花钱。
- 状态机六状态 + 数据驱动的迁移表；每次迁移都是显式事件：当前态可审计、非法当场报错（完整回放需事件表，见 Q3 加分点）。
- checkpoint 的纪律是"每完成一步就存一步"；恢复 = `ListResumable` → `LoadTask` → done 跳过 / running 重置重放 / pending 续跑。
- 幂等键是重试与崩溃恢复共用的判重机制；有副作用的操作没有幂等，恢复就是副作用翻倍。
- SQLite 胜在嵌入式零运维 + 事务 + 查询；单写者模型要 `SetMaxOpenConns(1)`。
- 状态外置、进程无状态——第 1 章"对话历史即状态"在系统层的放大。

下一章：[第 10 章：多 Agent 编排——planner/worker/critic](10-orchestrator-planner-worker-critic.md)——状态会存了，该让 planner 把任务拆开、worker 并行跑起来、critic 把关质量了。
