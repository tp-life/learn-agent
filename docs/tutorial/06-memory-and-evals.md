# 第 6 章：长期 Memory 与 Evals——让 agent 有记忆、让质量可度量

> 对应阶段：阶段二（进阶）· 项目 1 扩展 `mini-agent/internal/memory/` + 项目 2 `stage-02-kb-agent/scripts/`
> 代码位置：`mini-agent/internal/memory/memory.go`、`mini-agent/cmd/agent/main.go`、`stage-02-kb-agent/scripts/eval.ts`、`stage-02-kb-agent/lib/eval-metrics.ts`
> 前置：第 1 章（messages 与无状态 API）、第 3 章（ReAct 循环与上下文压缩）、第 4 章（embedding 与向量检索）、第 5 章（RAG 全链路）
> 学完后你能讲清：长期记忆为什么"本质也是一次检索"、写入/召回/遗忘三个设计决策怎么取舍、recall@k 与 MRR 怎么联读诊断、LLM-as-judge 的偏差与缓解——以及为什么没有 eval 集的调优都是玄学。

---

## 本章地图

本章讲两件事，它们看似无关，实则是同一个主题的两面：**让 agent 的能力可积累（Memory），让 agent 的质量可度量（Evals）**。

- Memory 的两层结构：短期 = messages 数组，长期 = 跨会话持久化事实
- "回忆即检索"：长期记忆与 RAG 共享 embedding + 向量检索底座
- 写入策略三问：记什么、谁来决定记、怎么忘
- Evals 的动机：为什么"感觉还行"不算数；测试集长什么样
- 检索侧指标：recall@k 与 MRR，以及两者联读的诊断法
- 生成侧指标：正确性与忠实度，LLM-as-judge 的方法与三类偏差
- 代码精讲：`memory.go`（Store / 两个工具 / 遗忘）、`eval.ts`、`eval-metrics.ts`
- 进阶（带代码）：后台记忆抽取、judge 最小实现、重要性衰减遗忘、调优闭环

---

## 一、概念详解

### 1.1 Memory 的两层：会话内历史 vs 跨会话事实

第 1 章的核心结论："对话历史就是状态"——`[]Message` 切片就是 agent 的短期记忆。它有两个天花板：

1. **进程退出即丢**。重启后 agent 对你一无所知，"我上次说过我不吃辣"这种期待完全落空。
2. **上下文窗口有限**。第 3 章用压缩（LLM 摘要早期历史）缓解，但压缩是有损的，且只解决"这一段对话太长"，不解决"下一段对话还记得你"。

**长期记忆** = 把值得跨会话保留的事实（用户偏好、身份背景、历史结论）持久化到外部存储，需要时再检索回来。两层对比：

| | 短期记忆 | 长期记忆 |
| --- | --- | --- |
| 载体 | messages 数组（内存） | 外部存储（本项目是 `memory.json`，生产可用 sqlite/pg） |
| 生命周期 | 会话内 | 跨会话 |
| 内容 | 对话原文（完整、未提炼） | 提炼后的事实（一句一条） |
| 使用方式 | 每轮全量重发 | 按需检索 top-k |
| 容量控制 | 截断 / 压缩（第 3 章） | 去重 / 遗忘（本章） |

**压缩与长期记忆的分工**（易混淆点，先掰清）：压缩保的是**当前对话的连贯性**——"把上文 50 轮压成 3 段摘要，好让这轮对话能继续"；长期记忆保的是**跨会话的用户画像与结论**——"这个用户不吃辣、是后端工程师"。压缩的产物仍进 messages 数组，长期记忆的产物进向量库；压缩是被动的（超阈值才触发），记忆写入是主动的（判断"这值得记"）。两者解决不同问题，互不替代，生产系统通常同时存在。

### 1.2 回忆本质也是一次检索

看长期记忆的"回忆"链路：embed(查询) → 向量库检索 top-k → 返回事实文本——和第 5 章 `kb_search` 的链路**一字不差**。所以 `memory` 包只是 `internal/embed` + `internal/vectorstore` 之上的一层薄封装，这一点包注释说得很直白（`mini-agent/internal/memory/memory.go:21-25`）：技术栈完全相同，真正的设计工作量不在检索代码，而在两个问题上：

- **写入路径不同**：RAG 的知识是离线预先灌入的静态文档（`/learn` 命令、上传 ingest）；memory 的内容是**会话中动态产生的事实**——没人提前告诉你该记什么，所以必须设计"什么值得记、谁来决定记"。
- **召回时机不同**：两条路线——每轮自动检索（把记忆无脑塞进每轮 prompt，实现简单但烧 token，还可能带入无关记忆干扰回答）vs 模型主动调 `memory_recall` 工具（判断"这个问题可能依赖历史信息"时才查）。本项目选后者：回忆不过是模型多了一件可调用的工具，正好是阶段一 ReAct 循环的复习。

推论很值钱：第 4 章学的全部检索知识（余弦相似度、top-k、阈值）原样适用于记忆；记忆特有的新问题只有三个——**写什么、何时写、怎么忘**。这正是 `memory.go` 包注释里的"设计三问"（`memory.go:9-25`），也是面试"长期 memory 怎么设计"的标准答题骨架。

### 1.3 写入策略三问

**第一问：记什么？** 值得记的：稳定偏好（"不吃辣"）、身份背景（"后端工程师"）、用户明确说"记住"的事、对话得出的重要结论。不值得记的：一次性请求、闲聊寒暄、只与当前对话相关的临时信息。记错了的代价不是"多占点存储"，而是**记忆库被垃圾稀释、检索质量下降**——和第 5 章 chunk 噪声稀释检索是同构的问题。

**第二问：谁来决定记？** 两条路线：

- **模型主动调 `memory_save` 工具**（本项目方案）：判断时机写在工具 Description 里。优点：零额外链路成本，模型最懂当前上下文；缺点：决策质量完全依赖 Description 写得好不好（见代码精讲 2.3 与常见坑 1），模型可能该记不记或逢话必记。
- **后台抽取**（进阶 3.1 给完整实现）：每轮对话结束后，用一个小模型从对话历史里提炼事实入库。优点：写入行为稳定可预期、可以加审核队列；缺点：每轮多一次 LLM 调用（成本），且抽取模型不懂上下文意图时会抽错。

**第三问：怎么忘？** 遗忘不是锦上添花，是长期记忆的必要组成——只进不出的记忆库迟早变成噪声库。三种策略由浅入深：

1. **写入时精确去重**：完全相同的事实不重复入库（`memory.go:104-108`）；
2. **主动遗忘接口**：用户说"忘掉我之前说的"时，按语义找到最相似的一条删掉——但阈值必须很高才敢删（`memory.go:184`，代码精讲 2.2 展开）；
3. **被动遗忘**：容量上限 + LRU / 重要性衰减，淘汰"久未被想起"的记忆（进阶 3.3 给教学实现）。

### 1.4 Evals：为什么"感觉还行"不行

场景：你给 RAG 调了 chunk 大小，跑了两三个问题，"好像变聪明了"。这个判断有三个致命伤：

1. **样本太少**：3 个问题碰巧变好不代表 30 个问题整体变好；
2. **确认偏误**：你倾向于看到自己期待的改进；
3. **无法回归**：下周改了 top-k，你怎么知道没把上次修好的问题改回去？

Eval（评估）= 把"感觉"换成"数字"：建一个**测试集**，每次改动后跑一遍 pipeline，用指标说话。测试集的最小形状 = **问题 + 期望命中的来源文档**（生成侧评估再加期望答案）。本项目测试集是 jsonl，一行一条（`stage-02-kb-agent/eval/dataset.jsonl`）：

```json
{"question": "chunk 切得太大或太小各有什么坏处？", "expect_sources": ["chunking.md"]}
```

建集的两条基本原则（面试视角 Q5 还会深挖"不自欺"）：问题分布要与真实使用一致（别全出"库里有标准答案"的送分题）；每个期望来源必须在库里真实存在且确实能回答该问题——脏测试集会让指标"看起来能跑"但结论全错，所以 `eval.ts` 对测试集逐行严格校验（代码精讲 2.4）。

### 1.5 检索侧指标：recall@k 与 MRR

**recall@k（召回率）**：对每个问题，期望来源是否出现在检索结果的前 k 名；取所有问题的命中比例。k=3、10 个问题、7 个命中 → recall@3 = 0.7。它回答的是"**找没找到**"。

**MRR（Mean Reciprocal Rank，平均倒数排名）**：对每个问题，找到第一个命中期望来源的名次 rank（从 1 数起），贡献 1/rank；都没命中贡献 0；取平均。期望源排第 1 → 贡献 1，排第 2 → 1/2，排第 3 → 1/3。它回答的是"**排得多靠前**"。

单看一个数字都会误判，**联读才有诊断意义**（面试高频，务必能脱口而出）：

| recall@k | MRR | 诊断 | 该做什么 |
| --- | --- | --- | --- |
| 低 | 低 | 检索根本没找到 | 召回侧问题：查 chunk 切分 / embedding 模型 / 混合检索（第 5 章三板斧） |
| 高 | 低 | 找到了但排名靠后 | 排序侧问题：上 rerank（精排）或调相似度阈值，**别动召回侧** |
| 高 | 高 | 检索侧合格 | 问题在生成侧：去查忠实度（1.6 节） |

还有一个隐藏设计点：指标按"**来源文档**"计而不是按"块"计——同一文档可能多个块进 top-k，`eval.ts` 会先按名次去重再算指标（`eval.ts:194-199`）。否则一篇文档占满 top-3 会把指标刷得很漂亮。

### 1.6 生成侧指标与 LLM-as-judge

检索对了，答案还可能编。生成侧看两个指标：

- **答案正确性**：答的是不是问题的答案；
- **忠实度（faithfulness）**：答案里的断言是否被检索到的内容支持——这是 RAG 特有的幻觉度量，"检索到了 A 却照着 B 答"就栽在这里。

麻烦在于：自然语言答案千变万化，没法用字符串匹配自动判分。工程上的出路是 **LLM-as-judge**：用另一次 LLM 调用当裁判，给答案打分。它能规模化（几百条测试集几分钟跑完），但**裁判本身有系统性偏差**，三类必须知道：

1. **偏爱长答案**（verbosity bias）：长的看起来"更专业"，分就高；
2. **偏爱自己模型的输出**（self-preference bias）：用 deepseek 评 deepseek 的答案会偏松——judge 最好换一个厂商的模型；
3. **位置偏置**（position bias）：两个答案对比打分时，交换顺序结果可能变。

对应缓解（进阶 3.2 落地为代码）：judge prompt **要求先引用证据再给分**（把判断锚定在原文上，抑制"看着顺就给高分"）；**抽样人工校验 judge 与人工的一致率**——judge 本身也要被 eval，"谁来监督监督者"的答案就是人。具体实践参数（用哪个模型当 judge、抽多少样本校验）以各厂商官方文档与论文为准。

---

## 二、代码精讲

### 2.1 Store：向量库之上的一层"事实"语义（`memory.go`）

`Store` 结构（`memory.go:54-58`）只有三个字段，每个都能对应回你之前的练习成果：

```go
type Store struct {
	vs   *vectorstore.Store // 底层存储与检索（练习 2 的成果）
	emb  Embedder           // 文本 → 向量（练习 1 的成果）
	path string             // 持久化文件路径：每次写入后立即落盘，防进程崩溃丢记忆
}
```

注意 `Embedder` 是 memory 包**自己定义的最小接口**（`memory.go:46-50`），不 import embed 包的具体类型——Go 惯例"在使用方定义接口"，测试时可以换假实现（`memory_test.go:10-25` 的 `fakeEmbedder` 就是靠这个接口注入的，不需要真的调 embedding API）。

`Remember`（`memory.go:93-131`）链路四步：TrimSpace 校验 → **精确文本去重** → embed → 入库 + 立即 `Save` 落盘。最值得停下去重这段（`memory.go:104-108`）：

```go
for _, d := range s.vs.FindByMetadata("kind", "memory") {
	if d.Text == fact {
		return nil
	}
}
```

为什么只做**精确匹配**、不做"语义相似就判重"？注释（`memory.go:99-103`）给了一个漂亮的反例："用户不吃辣"和"用户现在吃辣了"语义高度相似但含义相反——语义去重会把这类**更新**误判为重复而丢弃，甚至更糟地把旧事实删掉。宁可选保守的精确匹配（代价只是措辞不同的重复多占一个 top-k 名额），也不冒误删真实信息的风险。**删除类操作，保守是设计美德**——这个原则在 2.2 的遗忘阈值上还会再出现一次。

另外两个细节：ID 由时间戳生成（`memory.go:116`）——注意当前实现用的是 `time.Now().Nanosecond()`，它只取"秒内的纳秒偏移"（0–999999999），快速连续写入有撞 ID 的风险（撞 ID 后 `Delete(id)` 会误删），文件内 TODO 提示（`memory.go:85`）建议的 `time.Now().UnixNano()` 才是稳妥做法，读到这里值得动手改掉；`Metadata: {"kind": "memory"}`（`memory.go:119`）是给"记忆与知识混存一个库"留的口子——未来可以按 kind 过滤，面试里能说出这个字段的用意是加分项。

`Recall`（`memory.go:155-176`）链路：embed 查询 → `vs.Search` → 只提取 `Doc.Text` 返回。两个边界决策：

- `topK <= 0` 时兜底为 3 而非报错（`memory.go:156-158`），注释点破了**层次论**：Recall 的调用方是工具层（模型可能不传 `top_k`），面向模型的边界层宜宽容兜底；而 `vectorstore.Search` 面向代码，topK<=0 直接报错。**同样是非法输入，面向模型的层要"宽容"，面向代码的层要"严格"**。
- 只返回 `[]string`，不暴露 `Hit`/`Score`：工具结果最终要拼进 prompt 给模型看，模型不需要相似度分数，给了反而稀释注意力（`memory.go:148-150`）。副作用与防护见常见坑 2。

### 2.2 遗忘：Forget 与保守阈值（`memory.go` + `forget_test.go`）

`Forget(query)`（`memory.go:186-216`）：embed 查询 → 取 top-1 → **相似度低于 `forgetMinScore` 就不删**，返回 0。阈值定在 0.9（`memory.go:184`），理由是检索的数学事实：top-1 **永远存在**——哪怕库里的记忆全与 query 无关，也总有一条"最相似"的。不设阈值的话，`Forget("火星气候")` 会把最无辜的一条记忆删掉。删除不可逆，所以"拿不准就不删，返回 0 让调用方如实告知"。

`forget_test.go` 把这套策略钉成了五个测试，读它们等于把设计复习一遍：

- `TestRemember_ExactDuplicateSkipped`（`forget_test.go:43-62`）：重复写入被跳过，且用计数 embedder 断言**去重短路发生在 embed 之前**（`forget_test.go:50-61`）——重复的写入连 embedding API 都不调，省钱；
- `TestRemember_SimilarButDifferentKept`（`forget_test.go:64-78`）：2.1 节那个"吃辣/不吃辣"反例的直接固化——语义相近但文本不同的两条事实必须都保留；
- `TestForget_RemovesMostSimilar`（`forget_test.go:80-130`）：删除生效、不误删其他记忆，且**重启后被遗忘的事实不复活**（`forget_test.go:114-129`）——验证删除也落了盘；
- `TestForget_NoSimilarMemoryReturnsZero`（`forget_test.go:132-150`）：低相似度时返回 0 且一条不删——高阈值的防误删行为；
- `TestForget_EdgeCases`（`forget_test.go:152-164`）：空库时 Forget 短路返回，连 embed 都不调（`forget_test.go:161-163`）。

### 2.3 工具说明书：Description 是记忆系统的"策略层"（`memory.go` + `main.go`）

1.3 节"模型主动记"方案的全部行为质量，都压在两个工具的 Description 上。对照读：

`MemorySave.Description`（`memory.go:228-233`）同时说清**什么时候该记**（用户明确说"记住"、透露稳定偏好）和**什么时候不该记**（一次性请求、闲聊、临时信息）——只写用途的话模型会把闲聊也记进库。`MemoryRecall.Description`（`memory.go:258-262`）强调"问题可能依赖历史信息时才查"——否则模型对无关问题也查记忆，每轮白烧一次 embedding 调用（有成本），还可能把无关记忆塞进上下文干扰回答。

这就是第 2 章"说明书工程"在记忆场景的重演：**Description 不是文档，是行为策略**。记忆系统"逢话必记"或"从不回忆"，九成是 Description 没写好，不是模型不行（常见坑 1）。

两个 `Execute`（`memory.go:316-358`）是阶段一纪律的复习：模型生成的 args 是不可信输入，坏 JSON 返回 error 喂回模型让它自我纠正，绝不 panic；空结果返回明确的否定句"没有找到相关记忆"（`memory.go:348-350`）——空串会让模型分不清"没查到"和"工具坏了"。

接线在 `main.go:69-73`：

```go
memVs := vectorstore.NewStore()
_ = memVs.Load(memPath)           // 启动时恢复上次会话的记忆
memStore := memory.NewStore(memVs, embedClient, memPath)
registry.Register(memory.MemoryRecall{Store: memStore})
registry.Register(memory.MemorySave{Store: memStore})
```

注意：memory 和知识库**共用同一个 `embedClient`**（向量空间必须对齐）但**各用一个独立的 `vectorstore.Store`**——记忆与知识物理隔离、互不污染，混存是未来的事（靠 `kind` 元数据区分）。

### 2.4 eval.ts：把"跑一遍"工程化（`stage-02-kb-agent/scripts/eval.ts`）

脚本的骨架主线五步：解析 CLI → 加载测试集 → 建库（`--sample` 现场建 / 默认加载 `data/kb.json`）→ 逐题检索 → 聚合指标 + bad case 清单。四段值得精读：

**测试集数据结构**（`eval.ts:36-39`）：`{question, expect_sources[]}`——期望是一个数组，允许一个问题有多个正当来源。

**`loadDataset`（`eval.ts:92-119`）逐行校验**：缺字段、空数组、非字符串，全部带行号报错。为什么对输入这么狠？头部注释（`eval.ts:15-17`）说透了："指标的可信度取决于输入数据的真实性"——测试集是 eval 的地基，脏数据会让指标"看起来能跑"但结论全错。

**检索循环（`eval.ts:192-216`）的两个细节**：

- 所有问题**一次批量 embed**（`eval.ts:186`）而非逐题调用——一次 HTTP 往返，和 `buildSampleStore` 里"先切完所有文档的块再批量 embed"（`eval.ts:135-141`）是同一个成本意识；
- 命中判定前先**按来源去重**（`eval.ts:194-199`）：同一文档多个块进 top-k，只保留最高名次——1.5 节讲过，指标按"来源文档"计，否则一篇文档占满 top-3 会刷高指标。

**聚合输出（`eval.ts:218-246`）的容错设计**：指标计算包在 try/catch 里，失败不中断报告、bad case 清单照样打印（`eval.ts:229-232`），最后才置非零退出码（`eval.ts:244-246`）。这个设计是练习骨架期留下的好习惯：**bad case 清单是调优闭环的真正输入，比聚合数字更重要**——就算指标算不出来，清单也得让你看到。

### 2.5 eval-metrics.ts：两个纯函数（`stage-02-kb-agent/lib/eval-metrics.ts`）

指标函数被刻意写成**纯函数**（输入数组 → 输出数字，不碰网络/文件/向量库，文件头注释 `eval-metrics.ts:4-8`），可以脱离整个 RAG 管线单独测试。

`checkInputs`（`eval-metrics.ts:12-22`）：长度不一致、空输入直接抛错。哲学与 `loadDataset` 一脉相承：指标函数被喂脏数据应该报错（调用方 bug），而不是返回一个"看起来合理的数字"——**eval 的全部意义就是"数字可信"**（`eval-metrics.ts:44-46`）。

`recallAtK`（`eval-metrics.ts:50-70`）主体四行：

```ts
for (let i = 0; i < ranked.length; i++) {
	const topK = new Set(ranked[i].slice(0, k));
	if (expected[i].some((s) => topK.has(s))) {
		hit++;
	}
}
return hit / ranked.length;
```

`ranked[i].slice(0, k)` 即"前 k 名"；期望源**任一**命中即算命中（简化版定义；多期望源的严格变体是 `|期望 ∩ 前k| / |期望|` 按题平均，知道有这回事即可）。

`mrr`（`eval-metrics.ts:97-108`）同样直白：`findIndex` 找第一个命中的名次，`sum += 1 / (rank + 1)`（findIndex 从 0 数、名次从 1 数，所以 +1），未命中贡献 0。两个函数加起来不到 30 行——**检索评估的核心数学就是这么简单，难的是建一个好测试集和养成"改完必跑"的纪律**。

---

## 三、进阶拓展（带代码）

### 3.1 记忆抽取 prompt：让后台替你决定"记什么"

**为什么**：1.3 节说了两条写入路线，项目用的是"模型主动调 `memory_save`"。它的失效模式是"该记不记"——用户随口一句"我对花生过敏"，模型正忙着回答菜谱问题，没调工具，这条关乎安全的事实就丢了。生产系统（如各类记忆框架）常用第二条路线：**每轮对话结束后，后台用一个 LLM 调用从对话历史里提炼事实入库**。核心资产是一个抽取 prompt，调用骨架如下（教学代码，已在临时 module 中 `go build` / `go vet` 验证通过）：

```go
// Message 是一条对话消息（教学简化版，对应 mini-agent 的 llm.Message）。
type Message struct {
	Role    string
	Content string
}

// ChatCompleter 抽象"一次同步对话"的能力。
// 真实接入时用一个适配器包住 mini-agent 的 llm.Client
// （其 Chat 多一个 tools 参数，返回 *ChatResponse，适配层取 Content）。
type ChatCompleter interface {
	Chat(messages []Message) (string, error)
}

// FactSink 抽象"写入一条记忆"的能力，对应 memory.Store.Remember。
type FactSink interface {
	Remember(fact string) error
}

// extractPrompt 是后台抽取的核心资产——"什么值得记"的判断规则全在这里。
// 三个设计要点：
//  1. 给出"记/不记"的正反例，比抽象定义有效得多（第 1 章 1.7：给格式样例）；
//  2. 强制 JSON 数组输出，没有值得记的事实就输出空数组——
//     让"无产出"有一个确定的、可解析的表示；
//  3. 要求"改写成带主语的完整陈述句"，否则抽出来的是碎片
//     （"不吃辣"缺主语，入库后既难检索也难被模型正确使用）。
const extractPrompt = `你是记忆抽取器。从对话中抽取值得跨会话长期记住的事实。

值得记：用户的稳定偏好、习惯、身份背景、明确要求"记住"的事、对话得出的重要结论。
不要记：一次性请求、闲聊寒暄、只与当前对话相关的临时信息。

规则：
- 每条事实改写成一句带主语的完整陈述句（如"用户不吃辣"，而非"不吃辣"）；
- 只输出 JSON 字符串数组，如 ["用户不吃辣","用户是后端工程师"]；
- 没有值得记的事实就输出 []；
- 不要输出任何 JSON 以外的文字。`

// ExtractAndRemember 从一段对话历史中抽取值得长期记忆的事实并入库，
// 返回成功写入的条数。
//
// 调用时机：一轮对话结束后（或每 N 轮）由后台触发，不经手当前回答——
// 与"模型主动调 memory_save"相比，用户无感知，但判断力交给了抽取 prompt。
func ExtractAndRemember(llm ChatCompleter, sink FactSink, history []Message) (int, error) {
	// 1. 拼对话文本：只取 user/assistant，system 与工具结果对抽取是噪声。
	var b strings.Builder
	for _, m := range history {
		if m.Role == "user" || m.Role == "assistant" {
			fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
		}
	}
	if b.Len() == 0 {
		return 0, nil
	}

	// 2. 调 LLM 抽取。生产建议：这一步换便宜的小模型——
	// 抽取是格式化任务，用不着旗舰模型的推理能力（成本意识）。
	resp, err := llm.Chat([]Message{
		{Role: "system", Content: extractPrompt},
		{Role: "user", Content: b.String()},
	})
	if err != nil {
		return 0, fmt.Errorf("extract: chat: %w", err)
	}

	// 3. 解析：模型输出是不可信输入（阶段一纪律），坏 JSON 返回 error 而非 panic。
	// 生产可更稳：先剥可能的 ```json 围栏，或用 JSON mode / 伪工具约束输出（第 1 章进阶 3.3）。
	var facts []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp)), &facts); err != nil {
		return 0, fmt.Errorf("extract: parse %q: %w", resp, err)
	}

	// 4. 逐条入库：去重交给 sink（memory.Store.Remember 的精确去重），这里不重复造。
	saved := 0
	for _, f := range facts {
		if strings.TrimSpace(f) == "" {
			continue
		}
		if err := sink.Remember(f); err != nil {
			return saved, fmt.Errorf("extract: remember %q: %w", f, err)
		}
		saved++
	}
	return saved, nil
}
```

**取舍：模型主动记 vs 后台抽**（面试常考"你怎么选"）：

| | 模型主动调 memory_save | 后台抽取 |
| --- | --- | --- |
| 成本 | 零额外调用 | 每轮多一次 LLM 调用（可用小模型压成本） |
| 遗漏风险 | 高（模型忙着回答会忘记记） | 低（每轮必跑） |
| 误判风险 | 低（模型最懂当前上下文） | 中（抽取器不懂对话意图） |
| 可控性 | 策略散落在 Description 里 | 策略集中在抽取 prompt，可加审核队列 |

不是二选一：生产可以两者并存——`memory_save` 处理"明确说记住"的即时指令，后台抽取兜底"随口提到"的事实，去重交给 `Remember` 的精确匹配。

### 3.2 LLM-as-judge 的最小实现

**为什么**：1.6 节讲了生成侧指标（正确性、忠实度）没法用字符串匹配自动判分。最小可用的 judge 实现不到 50 行（教学代码，已用项目内 `tsc --noEmit --strict` 验证通过），关键全在 prompt 的反偏差设计：

```ts
/** ChatFn 抽象"一次同步对话"（接入时包住任意 OpenAI 兼容客户端，judge 可用不同厂商模型）。 */
type ChatFn = (system: string, user: string) => Promise<string>;

interface JudgeInput {
	question: string;
	contexts: string[]; // 检索到的 chunks（judge 打分的证据池）
	answer: string;     // 待评估的回答
}

interface JudgeResult {
	faithfulness: number; // 1-5：答案被检索资料支持的程度（无支持的断言 = 幻觉）
	correctness: number;  // 1-5：答案对问题的正确性
	evidence: string[];   // 打分依据：从 contexts 引用的原文片段（先证据后分数）
	reason: string;       // 一句话理由
}

// judge prompt 的三个反偏差设计：
// 1. 强制"先列 evidence 再打分"——把判断建立在原文上，抑制"看着顺就给高分"；
// 2. 给出打分锚点（1 和 5 各长什么样），减少尺度漂移；
// 3. 严格 JSON 输出 + 代码侧形状校验，脏输出直接报错（可加重试）。
const JUDGE_PROMPT = `你是严格的评估员。给定【问题】【检索资料】【待评估回答】，评估回答质量。

步骤（必须按序）：
1. 从【检索资料】中引用支撑或反驳该回答的原文片段，放入 evidence；
2. 基于 evidence 打分：
   - faithfulness（忠实度）：回答中的断言有多少被资料支持。5=全部有依据；1=大量编造。
   - correctness（正确性）：回答是否正确解决了问题。5=完全正确；1=答非所问或错误。
3. 输出 JSON：{"evidence": [...], "faithfulness": 1-5, "correctness": 1-5, "reason": "一句话"}
只输出 JSON，不要其他文字。回答的长度、文风不影响分数。`;

export async function judgeAnswer(chat: ChatFn, input: JudgeInput): Promise<JudgeResult> {
	const numbered = input.contexts.map((c, i) => `[${i + 1}] ${c}`).join("\n");
	const user = `【问题】${input.question}\n\n【检索资料】\n${numbered}\n\n【待评估回答】\n${input.answer}`;
	const raw = await chat(JUDGE_PROMPT, user);

	// judge 也是 LLM——输出同样不可信：形状校验不过就报错，让调用方重试。
	const parsed = JSON.parse(raw) as Partial<JudgeResult>;
	if (
		!Array.isArray(parsed.evidence) ||
		typeof parsed.faithfulness !== "number" ||
		typeof parsed.correctness !== "number" ||
		typeof parsed.reason !== "string"
	) {
		throw new Error(`judge 输出形状非法: ${raw}`);
	}
	return parsed as JudgeResult;
}
```

对 1.6 节三类偏差的落点：**"先证据后分数"**压的是"看着顺就给高分"的幻觉式评分；末句"长度、文风不影响分数"压 verbosity bias；self-preference 靠**换厂商模型当 judge**缓解（接入 `ChatFn` 时选一个和被评系统不同的模型即可）；位置偏置主要出现在"两答案对比"场景，逐条独立打分天然规避。**最后一条不可省**：抽样 10-20% 人工复核，算 judge 与人的一致率——一致率不达标，judge 的分数就没有引用价值。

### 3.3 重要性衰减遗忘：score = 相似度 × 时间衰减

**为什么**：`Forget` 是"用户主动删"，但更多记忆是**慢慢变得不重要**——三个月前的"用户在学 Go"可能早已过时。被动遗忘给每条记忆算一个随时间衰减的重要性分数，容量超限时淘汰最低分。教学骨架（已在临时 module 中 `go build` / `go vet` / 运行验证）：

```go
// ScoredMemory 是一条带元信息的记忆：
// CreatedAt 用于时间衰减；LastScore 记录最近一次被检索命中时的相似度
// （"被想起过"的记忆更重要——这是心理学里"提取强度"的朴素对应）。
type ScoredMemory struct {
	ID        string
	Text      string
	CreatedAt time.Time
	LastScore float64 // 最近一次检索命中的相似度；从未被命中为 0
}

// timeDecay 是指数时间衰减因子：每过一个 halfLife，权重减半。
// 直觉：一个月前的事如果这期间从没被想起过，它大概率已经不重要了。
func timeDecay(age time.Duration, halfLife time.Duration) float64 {
	return math.Pow(0.5, age.Hours()/halfLife.Hours())
}

// importance 计算一条记忆的当前重要性：重要性 = 最大历史相似度 × 时间衰减。
func importance(m ScoredMemory, now time.Time, halfLife time.Duration) float64 {
	return m.LastScore * timeDecay(now.Sub(m.CreatedAt), halfLife)
}

// Evict 在容量超限时淘汰重要性最低的记忆，返回保留的切片。
// capacity 是硬上限：记忆库不能无限膨胀——库越大，top-k 检索的噪声越多。
// 这与 Forget 的高阈值是同一条原则的两面：一个防"误删"，一个防"稀释"。
func Evict(mems []ScoredMemory, capacity int, now time.Time, halfLife time.Duration) []ScoredMemory {
	if len(mems) <= capacity {
		return mems
	}
	sorted := make([]ScoredMemory, len(mems))
	copy(sorted, mems) // 不改乱调用方的切片，排序在副本上做
	sort.Slice(sorted, func(i, j int) bool {
		return importance(sorted[i], now, halfLife) > importance(sorted[j], now, halfLife)
	})
	for _, m := range sorted[capacity:] {
		fmt.Printf("遗忘: %s (重要性 %.3f)\n", m.Text, importance(m, now, halfLife))
	}
	return sorted[:capacity]
}
```

实跑这个模型能看到一个有教育意义的现象：一条 60 天前写入、相似度 0.9 的"用户不吃辣"（衰减后 0.225），会输给一条 10 天前、相似度仅 0.3 的"用户上周问过股票"（衰减后 0.238）——**纯时间衰减会误伤"重要但久未提起"的事实**。这正是取舍讨论的素材：

- **LRU（按最近使用时间淘汰）**：实现最简单，但"最近没用到 ≠ 不重要"（过敏史可能半年用不上一次）；
- **时间衰减**：平滑、符合直觉，但要调半衰期，且有上面的误伤问题；
- **生产做法**：叠加多个信号——命中次数、用户 pin 的"永不遗忘"标记、重要性人工修正；淘汰前写审计日志（谁、什么时候、因为多少分被忘），出错可恢复。这也呼应面试 Q6"记忆写错了怎么办"。

### 3.4 eval 驱动调优闭环

**为什么**：指标不是为了写周报，是为了驱动改动。闭环长这样：

```
跑 eval → 看 recall@k / MRR → 逐条分析 bad case → 归类 → 改一个变量 → 复测对比
```

bad case 归类直接复用第 5 章的三板斧，每一类对应不同的改法：

| bad case 特征 | 归类 | 改法 |
| --- | --- | --- |
| 问题口语化/太短，检索词不达意 | query 侧 | query 改写（LLM 先把问题展开成检索友好形式） |
| 期望内容被切散在两个块里 | 索引侧 | 调 chunk 大小 / 加大 overlap / 改按结构切 |
| 错误码、型号、专有名词查不到 | 度量侧 | 加 BM25 混合检索 |
| 找到了但排在 4-5 名 | 排序侧 | 加大 top-k、上 rerank（此时 recall 高 MRR 低） |

**两条纪律**：一次只改一个变量（否则指标变了不知道是谁的功劳）；每次改动记录前后指标与 bad case 变化——练习 9 的调优报告模板（`docs/solutions/stage-02/exercise-9-tuning-report.md`）就是这个纪律的纸面化。RAG 系统的迭代节奏就是"eval → 改 → eval"的循环，eval 集是这个循环的地基。

---

## 四、面试视角

> 以下每题给"标准回答 → 常见追问 → 加分点"。比阶段文档（`docs/stages/stage-02-rag-memory-evals.md` 3.1 节 Q8/Q9）更深一层，自测方法：不看回答口述，再对照差距。

**Q1：短期 memory 和长期 memory 的区别？各自怎么实现？**

标准回答：短期 = 会话内 messages 数组，每轮全量重发给无状态的 API，容量受上下文窗口限制，超阈值用 LLM 摘要压缩；长期 = 跨会话持久化的事实库（用户偏好、历史结论），用 embedding + 向量检索实现"回忆"，本项目是 `memory_save` / `memory_recall` 两个工具挂进 ReAct 循环。

追问链：
- "压缩能替代长期记忆吗？" → 不能。压缩保当前对话的连贯性，产物仍进 messages；长期记忆保跨会话事实，产物进外部存储。压缩是被动的有损压缩，记忆写入是主动的判断（1.1 节）。
- "长期记忆的数据结构长什么样？" → 每条 = 文本事实 + 向量 + 元数据（`kind`、时间戳），存向量库；写入即落盘（本项目 JSON 文件，生产 sqlite/pg）。

加分点：主动说出"长期记忆的设计核心不是检索（那是复用的），而是写入策略三问：记什么、谁来决定记、怎么忘"。

**Q2：长期 memory 和 RAG 有什么区别？**

标准回答：技术栈相同——embedding + 向量库 + top-k 检索，所以 memory 包是 embed + vectorstore 上的薄封装。区别在两点：**写入路径**（RAG 是离线预先灌入的静态文档；memory 是会话中动态产生的事实）与**召回时机**（RAG 在"需要知识"时检索；memory 可以每轮自动检索塞 prompt，也可以模型主动调工具，各有成本与噪声的取舍）。

追问链：
- "每轮自动检索记忆和模型主动调工具，你怎么选？" → 自动检索实现简单但每轮必烧一次 embedding 且可能带入无关记忆；主动调用省成本但依赖 Description 写得好。本项目选后者，生产可以混合：少量核心画像自动注入 + 长尾事实走工具。
- "记忆和知识能存一个库吗？" → 可以，用元数据区分（本项目的 `{"kind": "memory"}` 就是留的口子），检索时按 kind 过滤；但要注意两者的更新频率与遗忘策略不同，物理隔离（本项目两个 Store）运维上更省心。

加分点：指出"记忆是动态写入的 RAG"，所以 RAG 的全部调优手段（chunk 策略对应"事实怎么措辞"、混合检索、rerank）理论上都适用；记忆特有的问题只是写入与遗忘。

**Q3：recall@k 和 MRR 的区别？为什么两个都要看？**

标准回答：recall@k 看"期望文档进没进前 k 名"，MRR 看"第一个正确结果排第几"（贡献 1/rank 取平均）。联读诊断：都低 → 检索召回侧问题（chunk/embedding/混合检索）；recall 高 MRR 低 → 找到了但排后面，该上 rerank 而不是动召回侧；都高 → 问题在生成侧。

追问链：
- "k 怎么选？" → 按产品形态：生成环节实际消费几条就选几（本项目 top-3 进 prompt，就评 recall@3）；也可以报多个 k（@1/@3/@10）看曲线。
- "为什么不用 precision？" → 期望集通常只有 1-2 个正当来源，top-k 里其余位置"不正确但无害"，precision 的分母（k）是人为设定的，对问答场景没有解释力；MRR 对"第一个正确结果"敏感正好匹配"用户只看最前面几条"的行为。
- "要评估整个列表的排序质量呢？" → nDCG（了解名词即可，能说出"带位置折损的累计增益"就及格）。

**Q4：LLM-as-judge 靠谱吗？**

标准回答：可用于规模化评估（忠实度、正确性打分），是生成侧指标的现实选择；但裁判本身有三类系统性偏差：偏爱长答案、偏爱自己模型的输出、位置偏置。缓解：judge prompt 要求先引用证据再打分、judge 换不同厂商模型、逐条独立打分替代两两对比、抽样人工校验 judge 与人工的一致率。

追问链：
- "怎么知道你的 judge 本身准不准？" → 抽样人工复核算一致率（如 Cohen's kappa 或简单一致率），一致率不达标则 judge 分数不可引用——"谁来监督监督者"的答案是人。
- "judge 的输出格式怎么保证？" → 与一切结构化输出相同：prompt 给 JSON 样例 + 代码侧形状校验 + 失败重试，进阶 3.2 的实现就是这么做的。

**Q5：为什么说没有 eval 集的调优是玄学？评估集怎么建才不自欺？**

标准回答：没有测试集，"改好了"只是几个样本上的感觉，有确认偏误且无法回归；eval 把每次改动变成可对比的数字。建集不自欺的要点：问题分布与生产真实查询一致（从真实日志采样最好）；期望来源经过人工核实；规模足以支撑结论（8 条只能看趋势，差 1 题 recall 就跳 12.5 个百分点）。

追问链（这才是本题的分水岭）：
- "指标一直涨，是不是系统真的在变好？" → 警惕**过拟合到测试集**：反复对着同一批 bad case 调参，会把系统调成"只会答这 8 道题"。对策：留出集（dev/test 分离，调参只看 dev）、定期用新真实问题换血测试集、指标上涨时人工抽查确认。
- "检索指标很好，用户还是说答得烂？" → 检索侧指标不代表生成质量，要补生成侧指标（忠实度/正确性，LLM-as-judge），两层指标联读。

**Q6：记忆写错了怎么办？（纠错与遗忘接口怎么设计）**

标准回答：分三层。预防层：写入时精确去重（防重复）、Description 约束写入时机（防垃圾写入）；纠错层：`Forget(query)` 语义删除，但阈值定得很高（0.9）——top-1 永远存在，不设阈值会把最无辜的记忆删掉，删除不可逆所以"拿不准就不删"；更新层：矛盾事实作为新记忆并存而非覆盖（"用户不吃辣"与"用户现在吃辣了"语义相近但含义相反，语义去重会误删更新）。

追问链：
- "两条矛盾事实并存，检索回来模型听谁的？" → 好问题。教学项目如实返回交给模型判断；生产可在元数据里带时间戳，召回时新事实排前或要求模型"以最新为准"。
- "用户行使'被遗忘权'（合规场景）呢？" → 需要按用户维度物理删除 + 审计日志；这也是记忆要带元数据（用户 id、时间戳）的原因之一。

加分点：主动提到"淘汰类操作要保守+可审计"这条贯穿设计（去重用精确匹配、Forget 高阈值、Evict 写审计日志）——这是比单个接口更高一层的设计哲学。

---

## 五、常见坑

1. **`memory_save` 的 Description 不写清"何时记/何时不记"，模型逢话必记**：只写"把事实写入记忆库"，闲聊、临时请求全被入库，记忆库很快被垃圾稀释、检索质量下降。Description 是记忆系统的策略层（2.3 节），写完后用对抗用例实测（说"今天天气不错"看它调不调）。
2. **回忆结果不过滤低分，把噪声当事实**：向量检索 top-k 永远返回 k 条——哪怕全都不相关。本项目教学版 `Recall` 未设阈值（且刻意不向模型暴露分数，防注意力稀释），生产有两个改法：加相似度阈值过滤，或把 score 透传给模型让其自行判断相关度——各有取舍，但"不过滤也不告知"是最差解。
3. **评估集过小，指标没有意义**：8 条问题的测试集，1 题之差 recall 就跳 12.5 个百分点——这种粒度只能看趋势，不能下"改好了/改坏了"的结论。认真调优至少几十条，且与生产分布一致。
4. **judge 不打证据直接打分，得到幻觉式评分**：让 LLM 直接给分，它会"看着顺就给高分"，忠实度评分完全失真。必须强制"先引用资料原文、再打分"（进阶 3.2），并抽样人工复核一致率。
5. **指标函数对脏数据返回"看起来合理的数字"**：长度不匹配、空输入如果静默兜底，上游 bug 会被一个假指标掩盖。`eval-metrics.ts` 的 `checkInputs` 选择直接抛错（`eval-metrics.ts:12-22`）——eval 的全部意义就是数字可信，假安全感比报错可怕得多。

---

## 六、动手练习

本章对应阶段二的三个练习，按依赖顺序做：

1. **练习 5：长期 memory 工具**。位置：`mini-agent/internal/memory/memory.go` 的三处 `TODO(练习5)`——`Remember`（`memory.go:72`）、`Recall`（`memory.go:133`）、两个工具的 `Execute` + main.go 注册（`memory.go:281`）。验收：`go test ./internal/memory/` 通过；端到端启动 agent，说"记住我不吃辣"，再问"我喜欢吃什么"，观察 Verbose 日志中模型先调 `memory_recall` 再回答。参考答案：`docs/solutions/stage-02/exercise-5-memory-tools.md`（完成后再看，含遗忘与去重的进阶实现）。
2. **练习 8：eval 脚本指标函数**。骨架已就绪（CLI、数据加载、检索循环、输出格式全由 AI 写好），你只实现 `stage-02-kb-agent/lib/eval-metrics.ts` 的 `recallAtK` 与 `mrr` 两个纯函数。验收：`EMBEDDING_MOCK=1 pnpm eval --sample` 跑通（mock 向量无语义，数字只证明管线通了）；配好真实 embedding key 后再跑一遍对比数字——两次数字的差异本身就是"指标可信度取决于输入真实性"的一课。参考答案：`docs/solutions/stage-02/exercise-8-eval-script.md`（完成后再看）。
3. **练习 9：调优报告（无代码）**。基于练习 8 的 eval 做一轮真实调优：改 chunk 大小 / top-k / 混合检索中的一项，记录前后指标与 bad case 变化。模板：`docs/solutions/stage-02/exercise-9-tuning-report.md`。这份报告是简历上"数据驱动优化"的实证素材，认真对待。

---

## 本章小结

- Memory 两层：短期 = messages（每轮全量重发、压缩控容量），长期 = 跨会话事实库（持久化 + 检索式回忆）；压缩与长期记忆解决不同问题，互不替代。
- 回忆本质也是一次检索：与 RAG 共享 embedding + 向量库底座，差异只在写入路径（动态 vs 预灌）与召回时机（每轮自动 vs 模型主动）。
- 长期记忆的设计三问：记什么（事实/偏好/结论，闲聊不记）、谁来决定（模型主动调工具 vs 后台抽取）、怎么忘（精确去重 → 高阈值主动遗忘 → 重要性衰减）；删除类操作一律保守。
- Evals 把调优从玄学变工程：测试集 = 问题 + 期望来源；recall@k 看"找没找到"，MRR 看"排多靠前"，联读诊断——都低查召回，召回高 MRR 低上 rerank。
- 生成侧用 LLM-as-judge 评正确性与忠实度，但裁判有三类偏差；先证据后打分 + 换厂商 judge + 抽样人工一致率是底线。
- 闭环：eval → bad case 分类（query 侧/索引侧/度量侧）→ 一次改一个变量 → 复测记录。警惕过拟合到测试集。

下一章：[第 7 章：全栈知识库 Agent 实战](07-fullstack-kb-agent.md)——把 RAG 链路产品化：Next.js + Vercel AI SDK、流式 UI 与可点击的引用卡片。
