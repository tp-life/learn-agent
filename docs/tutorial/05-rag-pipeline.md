# 第 5 章：RAG 全链路——从文档到带引用的答案

> 对应阶段：阶段二（进阶）· 项目 1 扩展（`mini-agent/internal/rag`）
> 代码位置：`mini-agent/internal/rag/`（chunk.go / kb.go / tool.go，本章精讲）、`mini-agent/cmd/agent/main.go`（/learn 接线）
> 前置：第 4 章（embedding 与向量检索——本章的两大底座）；第 2 章（工具协议）、第 3 章（ReAct 循环）——RAG 最终以"工具"形态接回 Agent
> 学完后你能讲清：RAG 写入/查询双链路的每一步在做什么、chunking 为什么是检索质量的上限、`kb_search` 工具的防幻觉设计、bad case 三板斧怎么按链路逐段排查——这是 RAG 面试的主战场。

---

## 本章地图

- RAG 的本质：检索 + 增强 + 生成 = 开卷考试；与微调怎么分工
- 写入/查询双链路全图：每一步对应本项目的哪个函数
- chunking：三种切分策略对比，为什么它是"第一调参位"
- 混合检索与 rerank：分别解决"找不全"和"排不准"；引用生成：可验证性从哪来
- 代码精讲：chunk.go（结构优先贪心打包）、kb.go（Ingest 三步编排 + 幂等）、tool.go（kb_search 的防幻觉设计）
- 进阶：RRF 混合检索融合（完整实现）、query 改写、rerank 接入位、检索侧生产清单
- 面试核心题：bad case 三板斧的"按链路逐段定位"回答骨架

---

## 一、概念详解

### 1.1 RAG 的本质：检索 + 增强 + 生成 = 开卷考试

第 1 章说过，LLM 是无状态的概率生成器，知识有截止日期，上下文窗口有上限——你的文档它既没见过，也塞不下。RAG（Retrieval-Augmented Generation，检索增强生成）的思路一句话：**先从知识库里找出与问题最相关的几段，塞进 prompt，让模型基于这几段回答**。

类比开卷考试：LLM 是考生，知识库是允许带入考场的资料，**检索**是"翻书找相关页"，**增强**是"把翻到的几页摊开在桌上"（拼进 prompt），**生成**是"照着资料作答"。考生本身没变（模型权重不动），变的是他手边的资料——这是理解 RAG 一切设计的第一性原理。

三大价值，每条对应 LLM 裸用的一个痛点：

1. **知识可更新**：改库即更新，新文档入库下次检索就生效；微调要让模型"学会"新知识必须重新训练，又贵又慢还可能灾难性遗忘。
2. **答案可溯源**：检索到的每段资料带来源元数据，答案可标注"出自《XX》第 N 段"，用户可核查（1.6 节）。
3. **减少幻觉**：手里有资料就不必硬编；配合"资料不足就如实说"的纪律，幻觉空间被大幅压缩（2.3 节）。

**与微调的分工**（面试必考）：

| 维度 | RAG | 微调（fine-tuning） |
| --- | --- | --- |
| 改的是什么 | 开卷资料（模型外的知识库） | 考生本身（模型权重） |
| 擅长教 | **事实知识**：文档、FAQ、私有资料 | **风格/格式/领域语感**：话术、输出结构、术语习惯 |
| 知识时效 | 改库即更新 | 训完即冻结 |
| 可追溯性 | 有引用来源 | 黑盒记忆，说不出出处 |
| 成本与风险 | 低（embedding + 存储费用） | 高（训练 + 评估），可能灾难性遗忘 |

一句话：**RAG 教事实、微调教风格，两者互补**——成熟助手往往同时用：微调教会"说行话、按格式输出"，RAG 供应事实。"长上下文模型能否取代 RAG"的追问留到面试视角 Q1。

### 1.2 写入/查询双链路全图

RAG 由两条链路组成：**写入路径（离线）**把文档灌进库，**查询路径（在线）**在问答时把内容找回来：

```
写入路径（离线）                        查询路径（在线）
文档 → 解析 → chunking ─┐         用户问题 → embed(query)
                        ▼                          ▼
                 embed(每个 chunk)          向量检索 top-k（+混合/rerank）
                        │                          │
                        ▼                          ▼
                向量库（文本+向量+元数据） ──────> 相关 chunks + 出处
                                                         ▼
                                      拼 prompt：仅基于资料回答，用 [编号] 标注来源
                                                         ▼
                                      LLM 生成 → 带引用的答案
```

两条链路在本项目的落位（值得对着代码走一遍）：

| 链路步骤 | 代码位置 | 本章 |
| --- | --- | --- |
| 解析 + chunking | `main.go:129`（ReadFile）→ `chunk.go:78`（Chunk） | 2.1、2.4 |
| 批量 embed → 入库 | `embed.go:93` → `store.go:79`（Add，均第 4 章） | — |
| 入库编排（三步组装） | `kb.go:94`（Ingest） | 2.2 |
| query embed → top-k | `tool.go:134` → `store.go:180`（Search，第 4 章） | 2.3 |
| 拼 prompt（编号+来源） | `tool.go:156`（Execute 输出经 ReAct 循环成为 tool 消息） | 2.3 |
| 带引用生成 | LLM 按 prompt 中的编号标注 | 1.6 |

**全链路最重要的纪律：写入和查询必须用同一个 embedding 模型。** 向量是"语义坐标"，坐标系由模型定义——入库用 bge-m3、查询换别的模型，等于拿北京地图查上海坐标，且维度相同时**不报错、静默出错**（常见坑第 3 条）。这条纪律写进了 `KBSearch` 持有 `Embedder` 的设计理由（`tool.go:23-26`）。

### 1.3 chunking 决定检索质量的上限

文档不能整篇入库，两个硬约束：**主题稀释**（几万字压进 1024 维，各主题互相平均，向量"什么都不像"）与**输入上限**（bge-m3 最大输入 8192 token，以官方文档为准）。所以必须切块。三种主流策略：

| 策略 | 做法 | 优点 | 缺点 | 适用 |
| --- | --- | --- | --- | --- |
| 固定窗口 + overlap | 每 N 字符/token 一刀，相邻块重叠 10~20% | 简单稳定 | 可能在句中切断 | 起步基线、无结构文本兜底 |
| 按结构切 | 按段落/标题/代码块边界切 | 块语义完整，质量最好 | 单段可能超长需硬切兜底 | 文档类首选（本项目） |
| 语义切 | 对文本做 embedding，在语义突变处下刀 | 切点最"懂"内容 | 入库前多跑 embedding，贵且慢 | 高价值语料，了解即可 |

块大小的两端症状（面试常考"怎么知道切坏了"）：**块太大**→ 多主题混入、向量被平均稀释，top-k 被同一篇长文的相邻块占满；**块太小**→ 上下文不完整、答案断章取义（"第 3 条"被切走，块里只剩"……除外"）。

**chunk 大小是 RAG 的第一调参位**——它在链路最上游，上游决定下游质量上限：检索回来的块断章取义或充满噪声，后面的 prompt 工程做得再好也救不回来。调优顺序永远是先调切分，再上混合检索/rerank。

本项目的取舍（`chunk.go:44`）：**结构优先**——按空行切段落、贪心打包、段落不拆；单段超限时退化为固定窗口硬切 + overlap。默认 400 字符（rune）+ 60 字符重叠，约合中文 200~270 token，落在常见起点（200~500 token）偏保守一侧。按字符而不是 token 计数是教学简化（不引入 tokenizer 库），换算经验值：中文 1 token ≈ 1.5~2 字符。

### 1.4 混合检索：向量与 BM25 互补

向量检索擅长**语义改写**（"怎么退款"命中"退货流程说明"），但有公认短板——**字面敏感的查询**：错误码（`E-4021`）、型号、人名、新造词在训练分布里出现少、编码不充分，而用户要的恰恰是字面精确匹配。关键词检索（经典算法 BM25）正好相反：字面重合就命中，语义改写束手无策。两者**互补而非替代**，生产做法是**混合检索**：两路各跑一次，结果合并成一路。

合并的难题：两路分数**量纲不同**（余弦 [-1,1] vs BM25 [0,+∞)），直接相加等于让数值大的一方主导。**RRF（倒数排名融合）**的解法优雅在"抛弃分数、只取名次"：每路第 rank 名贡献 `1/(k+rank)` 分，同文档跨路累加，按总分降序；k 惯例取 60。名次无量纲，天然可比。完整实现见进阶 3.1。

### 1.5 rerank：粗排求快、精排求准

向量检索是**粗排**：一次向量 + O(N) 扫表，毫秒级，但打分粗糙（查询和文档各自压成一个向量，交互信息丢了）。**rerank 是精排**：用专门模型（交叉编码器，如 bge-reranker 系列，以官方文档为准）把"查询 + 候选文档"**逐对**打分——看得细所以准，逐对算所以慢，只能对粗排 top-k 候选用。链路位置固定：**检索之后、生成之前**——粗排多捞（top-k 的 2~4 倍）→ 精排重排 → 只留 top-k 进 prompt，**用一次额外调用的成本换排序精度**。什么时候值得上看 eval：期望文档没进候选池（召回低）rerank 救不了；进了但排得靠后（召回高、MRR 低）才是它的适应症（第 6 章）。

### 1.6 引用生成：可验证性 = 用户信任

**引用 = 可验证性 = 用户信任 + 幻觉兜底**。没有引用，用户无法区分"照资料答的"和"自己编的"，RAG 相对裸 LLM 的核心卖点就不成立；有了引用，人可以跳回原文核查，幻觉更容易被兜住。

实现不依赖模型魔法，是三件套的接力：

1. **入库时写元数据**：chunk 的 Metadata 记下来源与块序号（`kb.go:126-129`）——溯源信息在写入侧埋好；
2. **检索结果带编号进 prompt**：`[1]（来源：notes.md，相似度 0.87）+ 正文`（`tool.go:156-158`）——模型读到的资料自带编号；
3. **prompt 要求标注**：工具输出开头写明"请在回答中用 [编号] 标注引用来源"。

局限要清醒：**引用不保证忠实**——模型可能曲解原文甚至编造编号。引用只是"让核查成本最低"的审计接口，配套还需忠实度 eval（第 6 章）与 2.3 节的防幻觉闸门；项目 2（第 7 章）会把引用做成可点击卡片完成体验闭环。

---

## 二、代码精讲

RAG 链路落在三个文件：`chunk.go`（切块）、`kb.go`（入库编排）、`tool.go`（检索工具），外加 `main.go` 的接线。

### 2.1 切块：`mini-agent/internal/rag/chunk.go`

**调参入口** `ChunkOptions{MaxChars, OverlapChars}`（`chunk.go:29-32`），注释点明两件事：MaxChars 是"第一调参位"；OverlapChars 防止语义在块边界被拦腰切断（一句话横跨两块时，重叠保证它在至少一块里完整），经验值是块大小的 10%~20%。

**主流程：结构优先的贪心打包**（`chunk.go:78-123`）。两步：`splitParagraphs`（`chunk.go:140-149`）按空行切段、TrimSpace、丢空段；再遍历段落**贪心打包**——放得下就追加，放不下就封存（flush）开新块；单段超限才硬切兜底（`chunk.go:95-110`）：

```go
	for _, para := range splitParagraphs(text) {
		paraLen := len([]rune(para))
		if paraLen > opts.MaxChars {
			flush()
			fmt.Println(paraLen, len(para), opts.MaxChars, "sssss")
			chunks = append(chunks, hardCut(para, opts)...)
			continue
		}
		addLen := paraLen
		if len(cur) > 0 {
			addLen += 2
		}

		if curLen+addLen > opts.MaxChars {
			flush()
		}
```

三个细节值得停下想（`chunk.go:89`、`99` 的两行 `fmt.Println` 是工作区遗留的调试打印——为什么是真问题见常见坑第 6 条，读算法时忽略）：

- **段落不拆是设计目标**：段落要么整个进当前块、要么整个进下一块——结构完整的块检索质量最好，这是"按结构切"的落地；
- **记账要算分隔符**：`addLen += 2` 是段落间 `"\n\n"` 的长度，漏算会导致实际块长超限；
- **硬切前先 flush**：保住已有块的完整性，硬切产物独立成块。

**硬切兜底**（`chunk.go:151-168`）：

```go
func hardCut(para string, opts ChunkOptions) []string {
	runes := []rune(para)
	step := opts.MaxChars - opts.OverlapChars
	var out []string

	for start := 0; start < len(runes); start += step {
		end := start + opts.MaxChars

		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
		if end == len(runes) {
			break
		}
	}
	return out
}
```

- **必须用 `[]rune`，绝不能用 byte 下标**——本章第一个大坑。Go 字符串是 UTF-8 字节序列，`len(s)` 是**字节数**，一个汉字占 3 字节；按 byte 切会把汉字从中间劈开产出乱码。`[]rune` 解码成字符数组后，下标才是"第几个字"。`chunk_test.go:83` 用纯中文 + `utf8.ValidString` 把这条钉成测试。
- **步长 = MaxChars - OverlapChars**，相邻块重叠 OverlapChars 个字符；`end == len(runes)` 时 break，避免末尾多切一个全重叠废块。

**防御性参数归一** `normalizeChunkOptions`（`chunk.go:125-138`）：MaxChars ≤ 0 用默认值兜底，overlap 钳制到 [0, MaxChars-1]。为什么必须钳？看 hardCut 步长 `MaxChars - OverlapChars`——overlap ≥ max 时步长 ≤ 0，`for start += step` **永不前进，死循环**。`chunk_test.go:129` 用 2 秒 watchdog 验证"钳制后照常出块"。铁律：**凡参与循环条件的算术，先想"什么输入会让循环不终止"**。

### 2.2 入库编排：`mini-agent/internal/rag/kb.go`

**Embedder：定义在使用方的小接口**（`kb.go:32-34`）：

```go
type Embedder interface {
	Embed(texts []string) ([][]float32, error)
}
```

为什么不直接依赖 `*embed.Client` 而要自定义接口？Go 接口思想最经典的教学点：**接口定义在"使用方"而非"实现方"**——KnowledgeBase 只声明"我需要一个能 Embed 的东西"，`*embed.Client` 签名恰好匹配，Go 接口隐式满足，一行适配都不用写（与 Java 式显式 implements 相反，面试高频）。收益在测试兑现：`kb_test.go:10` 的 `fakeEmbedder` 按文本返回预定义向量，检索结果完全确定、无需网络、不烧额度。

**Ingest：三步编排 + 幂等**（`kb.go:94-141`），分四段看。

第一段，防御校验（`kb.go:95-102`）：source 空白报错；Chunk 产出 0 块（空文档）报错。错误信息带 source，一眼定位是哪篇文档的问题。

第二段，**幂等短路**（`kb.go:104-107`）：

```go
	existing := kb.store.FindByMetadata("source", source)
	if sameChunks(existing, chunks) {
		return 0, nil
	}
```

`/learn` 同一篇文档两次，不该重复烧 embedding 额度、更不该堆出重复块。按元数据捞出该 source 的已有块（`store.go:305`），与本次切出的块逐条比对（`sameChunks`，`kb.go:143-154`），没变直接返回 0。`kb_test.go:209` 用 `countingEmbedder` 把"重复入库一次 embedding 都不多调"钉成测试——**幂等不是口头承诺，是可测的行为**。

第三段，批量 embed + 数量校验（`kb.go:110-117`）：一次性批量调用（第 4 章的批量设计），向量数与块数不符直接报错——错位入库比不入库更糟糕。

第四段，组装 + 先删旧块再入库（`kb.go:119-139`）：

```go
	docs := make([]vectorstore.Document, len(chunks))

	for i, c := range chunks {
		docs[i] = vectorstore.Document{
			ID:     fmt.Sprintf("%s#%d", source, i),
			Text:   c,
			Vector: vectors[i],
			Metadata: map[string]string{
				"source": source,
				"chunk":  strconv.Itoa(i),
			},
		}
	}

	for _, old := range existing {
		kb.Store().Delete(old.ID)
	}
```

- **ID 设计 `source#序号`**：一眼可读、天然唯一，去重更新按 ID 定位；
- **Metadata 必须写 `source` + `chunk`**：1.6 节的引用溯源全靠它——漏了 source，检索结果就没法标注出处（`store.go:43-46` 的 Document 注释把这条写成契约）；
- **先删旧块再 Add**：文档更新后旧块必须清走，否则新旧块混库、检索可能命中过时内容（`kb_test.go:245` 验证"替换后只剩新版"）；
- **all-or-nothing 链条**：前置步骤失败时库未被触碰；`Add` 自身整批校验整批拒绝（`store.go:79-104`）——两层合起来 Ingest 整体原子，重试不用先清理半成品。

### 2.3 检索工具：`mini-agent/internal/rag/tool.go`

**KBSearch 为什么持有 Embedder**（`tool.go:23-26`）：问题是文本、索引是向量，检索前必须先把问题翻译成**与入库时同一个向量空间**——1.2 节"同一模型"纪律在结构上的体现：查询侧与入库侧共享同一个 Embedder 实例（`main.go:65-66`）。

**Description：说明书工程的完整范例**（`tool.go:55-60`）：

```go
func (t *KBSearch) Description() string {
	return "在用户的个人知识库中做语义检索，返回与问题最相关的文档片段（带来源标注）。" +
		"使用时机：问题涉及用户通过学习命令（/learn）收录的文档，如学习笔记、收藏的文章、项目资料。" +
		"不要用于：算术计算（用 calculator）、获取网页内容（用 http_fetch）、与知识库无关的常识问答。" +
		"如果检索结果为空或提示没有相关内容，必须如实告知用户知识库未覆盖该问题，不要编造来源。"
}
```

四句话对应四个要素（第 2 章"工具说明书"的完整应用）：**用途**（语义检索、带来源片段）、**使用时机**（/learn 收录的文档）、**反面提示**（算术/网页/常识各有归属——没有边界的说明会让模型逢问必搜，浪费 embedding 调用还把噪声塞进 prompt）、**防幻觉指令**（检索不到如实说——工具返回空时模型倾向于"圆场"，凭自身知识编造还伪装成来自知识库，必须在说明书里提前封住）。`ParametersSchema`（`tool.go:65-76`）同样讲究：query 的 description 直接教模型"用完整自然语言句子，比堆关键词效果更好"。

**Execute：读取路径五步**（`tool.go:119-161`）。

第一步，参数解析（`119-128`）：模型输出是不可信输入——畸形 JSON、空 query 返回 error，作为 tool 消息喂回模型自我纠正（第 2 章纪律）。第二步，**空库短路**（`130-132`）：`t.store.Len() == 0` 直接返回固定文案——库里一条都没有时检索注定为空，**省下一次 embedding 调用**（一次调用 = 一次 HTTP 往返 + 计费）。注意返回的是文案不是 error：对模型来说"库为空"是正常情况，不是工具故障。

第三步，embed + 检索（`134-142`）：`Embed([]string{query})` 翻译问题，`store.Search(vec, t.topK)` 取 top-3（topK=3 的取舍见 `tool.go:33-35`：太少漏关键块，太多稀释 prompt 还混噪声）。

第四步，**minScore 低分过滤**（`tool.go:117`、`144-149`）：

```go
const minScore = 0.3

	var relevant []vectorstore.Hit
	for _, h := range hits {
		if h.Score >= minScore {
			relevant = append(relevant, h)
		}
	}
```

top-k 只保证"分数最高的 k 个"，不保证"足够相关"——库里只有 3 条风马牛不相及的文档时 Search 照样返回 3 条。0.3 是教学经验值（生产看自己语料的分数分布调，第 6 章 eval）。全部被过滤时返回明确文案"知识库中没有相关内容……不要凭自身知识编造来源"（`tool.go:151-153`）。**这是 RAG 防幻觉的第一道闸**：宁可告诉模型"没有"，也不把低分噪声塞进 prompt 让它抓只言片语强行作答。

第五步，输出格式化（`tool.go:156-158`）：

```go
	sb.WriteString("以下是从知识库检索到的相关内容（请在回答中用[编号] 标注引用来源）：\n")
	for i, h := range relevant {
		fmt.Fprintf(&sb, "\n[%d]（来源：%s, 相似度 %.2f）\n%s\n", i+1, h.Doc.Metadata["source"], h.Score, h.Doc.Text)
	}
```

格式同时服务两个读者：给**模型**——编号 `[1][2]` 让它能在回答里标注引用；给**人**——来源与相似度让 Verbose 输出可直接核查（`kb_test.go:98` 断言输出必含 `[1]` 与来源名）。`Metadata["source"]` 能取到值，靠的正是 Ingest 强制写入的元数据——**写入侧埋的溯源信息，在读取侧兑现成引用**。

### 2.4 接线：`mini-agent/cmd/agent/main.go`

知识库的启用是有条件的优雅降级（`main.go:56-67`）：配了 `SILICONFLOW_API_KEY` 才启用 RAG；启动时 `store.Load` 尝试加载已有索引，文件不存在（首次运行）不是错误，其他错误提示后从空库继续——不让知识库故障拖垮整个 agent。

`/learn` 命令（`main.go:100-105`）有个值得注意的设计决策：

```go
		// /learn 是斜杠命令（客户端指令），不走 agent——
		// 入库是确定性的本地动作，没必要让模型经手。
		if strings.HasPrefix(input, "/learn") {
			learnFile(kb, strings.TrimSpace(strings.TrimPrefix(input, "/learn")))
			continue
		}
```

入库是读文件 + 切块 + embed + 写库的固定流程，没有需要模型判断的地方——**确定性的动作走代码，不走模型**，省 token 还消除模型乱传参的可能。这是"什么该交给模型"的分寸感。`learnFile`（`main.go:120-143`）三步：读文件 → `kb.Ingest` → 成功后**立即 Save 落盘**（`main.go:139`）——embedding 花了钱，进程退出就丢等于白烧，这是第 4 章 Save/Load 的真实动机。

---

## 三、进阶拓展（带代码）

### 3.1 RRF 混合检索融合（完整实现）

**为什么需要它**：1.4 节讲过，向量检索对字面敏感查询是短板。混合检索 = 向量路 + 关键词路并行，再按 **RRF** 融合——只取名次不取分数：每路第 rank 名贡献 `1/(k+rank)`，同文档跨路累加。完整可运行的教学实现（已过 `go vet`/`go build` 验证）：

```go
// 教学示例：RRF（Reciprocal Rank Fusion，倒数排名融合）混合检索。
package main

import (
	"fmt"
	"sort"
	"strings"
)

// Hit 是一路检索结果中的一条命中（教学简化版）。
type Hit struct {
	ID   string
	Text string
}

// rrfK 是 RRF 的平滑常数，惯例取 60（Cormack 等 2009，ES/Qdrant 沿用为默认值）。
// k 压住头部名次差异：第 1 名贡献 1/61、第 10 名 1/70，"两路中游"可赢"一路登顶"。
const rrfK = 60

// RRFuse 把多路检索结果融合成一路，按 RRF 总分降序。
// 为什么不能直接加分数？余弦在 [-1,1]、BM25 在 [0,+∞)，量纲不同，相加
// 等于让数值大的一方主导。RRF 只取名次：每路第 rank 名（从 1 计）贡献
// 1/(k+rank)，同文档跨路累加——名次无量纲，天然可比。
func RRFuse(lists ...[]Hit) []Hit {
	scores := map[string]float64{}
	byID := map[string]Hit{}

	for _, list := range lists {
		for i, h := range list {
			rank := i + 1 // 名次从 1 开始
			scores[h.ID] += 1.0 / float64(rrfK+rank)
			byID[h.ID] = h
		}
	}

	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j] // 同分按 ID 排序，输出确定（测试友好）
	})

	fused := make([]Hit, 0, len(ids))
	for _, id := range ids {
		fused = append(fused, byID[id])
	}
	return fused
}

// keywordSearch 是"能跑就行"的字面检索教学桩：按查询词出现次数打分。
// 真实系统用 BM25（考虑词频饱和与长度归一），直接引成熟库，别手搓。
func keywordSearch(query string, docs []Hit) []Hit {
	terms := strings.Fields(query)
	type scored struct {
		h Hit
		n int
	}
	var ss []scored
	for _, d := range docs {
		n := 0
		for _, t := range terms {
			n += strings.Count(d.Text, t)
		}
		if n > 0 {
			ss = append(ss, scored{d, n})
		}
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].n > ss[j].n })
	out := make([]Hit, len(ss))
	for i, s := range ss {
		out[i] = s.h
	}
	return out
}

func main() {
	docs := []Hit{
		{ID: "faq#0", Text: "退款流程：在订单页提交申请，三个工作日内原路退回。"},
		{ID: "faq#1", Text: "错误码 E-4021：支付渠道限额，请更换支付方式后重试。"},
		{ID: "faq#2", Text: "会员积分：每消费一元积一分，积分可抵扣订单金额。"},
	}

	// 模拟向量一路（真实代码里是 store.Search 的输出）：字面敏感的 E-4021
	// 没被充分编码，真正的答案 faq#1 掉到第 3；关键词一路字面命中直接登顶。
	vectorHits := []Hit{docs[0], docs[2], docs[1]}
	keywordHits := keywordSearch("E-4021 怎么解决", docs)

	fmt.Println("向量路： ", ids(vectorHits))
	fmt.Println("关键词路：", ids(keywordHits))
	fmt.Println("RRF 融合：", ids(RRFuse(vectorHits, keywordHits)))
	// 输出：RRF 融合：[faq#1 faq#0 faq#2]——faq#1 凭关键词路高名次回榜首。
}

func ids(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}
```

**取舍与生产注意**：

- **什么时候值得上**：查询里专有名词/错误码/型号密度高（客服工单、运维文档、电商检索），且 eval 显示向量路对这类查询召回差。语义查询为主的个人知识库，纯向量已够用——为用而用只增加复杂度，让 eval 数据说话；
- 生产里的关键词路别手搓：Elasticsearch、Qdrant 自带 BM25 + RRF 端点，pgvector 可搭配 pg_search 扩展（以各自官方文档为准）；
- RRF 不需要分数归一化，是它相对"加权分数融合"的最大优势——后者要调权重、还怕某路分数分布漂移。

### 3.2 query 改写：把口语化问题展开成检索友好形式

**为什么**：用户的问题是给人听的，不是给检索器听的——"付款的时候跳 E-4021，咋整？"口语化、有省略；多轮对话里的"它为什么报错"离开上下文更无法检索。**query 改写**让 LLM 先把问题展开成 1~3 个检索友好的查询，再逐个检索取并集：

```go
// rewritePrompt 三条规则各防一个翻车点：展开指代（"它为什么报错"离开上下文
// 无法检索）；专有名词原样保留（错误码被"润色"掉就搜不到）；限定输出格式
//（输出要被代码切分消费，格式越死越好）。
const rewritePrompt = `你是检索查询改写器。把用户的口语化问题改写成 1~3 个适合检索的查询。
规则：
1. 补全省略的主语与上下文，展开"它/这个/上面"等指代；
2. 专有名词、错误码、型号、人名原样保留，不得改写或翻译；
3. 只输出改写后的查询，一行一个，不要编号，不要解释。`

// Chat 是"一条 system + 一条 user，拿回文本"的最小抽象——
// 生产里直接复用第 1 章的 llm.Client.Chat 包一层即可。
type Chat func(ctx context.Context, system, user string) (string, error)

// RewriteQuery 把一个问题展开成若干检索查询。可靠性设计（LLM 输出不可信）：
// 逐行切分去空白丢空行；兜底——改写为空时返回原问题，链路不因改写失败中断。
func RewriteQuery(ctx context.Context, chat Chat, question string) ([]string, error) {
	out, err := chat(ctx, rewritePrompt, "用户问题："+question)
	if err != nil {
		return nil, fmt.Errorf("rewrite query: %w", err)
	}
	var queries []string
	for _, line := range strings.Split(out, "\n") {
		if q := strings.TrimSpace(line); q != "" {
			queries = append(queries, q)
		}
	}
	if len(queries) == 0 {
		queries = []string{question}
	}
	return queries, nil
}
```

（带假 Chat 演示的完整可运行版本已通过 `go vet`/`go build` 验证，输出为两行改写查询。）

**适用场景与代价**：每次问答多一次 LLM 调用——**延迟 +0.5~1s、成本 +一次 prompt**。它是"召回失败"的针对性手段而非默认配置：口语化重、多轮指代多的场景（客服对话）值得开；FAQ 式简短问题直接检索即可。工程上可把多个改写查询并发检索，或用更小的模型做改写压成本。

### 3.3 rerank 接入位：接口设计与成本权衡

**为什么**：1.5 节讲过原理，这里落地为代码结构。三个关键决策：接口定义在使用方（与 Embedder 同款思想）、粗排多捞给精排留翻盘空间、精排失败静默回退粗排（**rerank 是增强不是依赖**，它挂了不该拖垮检索链路）：

```go
// Reranker 是"精排"能力抽象：定义在使用方、实现可换
//（bge-reranker 等专用模型 / 交叉编码器 / 让 LLM 打分）。
type Reranker interface {
	// Rerank 返回与 docs 等长的索引排列（ret[0] = 新第 1 名的原下标）。
	// 返回下标而非文本：不复制大段文本，且调用方 Hit（Score、Metadata）不丢失。
	Rerank(ctx context.Context, query string, docs []string) ([]int, error)
}

// searchWithRerank 展示 rerank 的标准插入位：粗排之后、拼 prompt 之前。
// 粗排多捞（topK 的 2~4 倍）精排才有翻盘空间；精排失败静默回退粗排（降级优于故障）。
func searchWithRerank(ctx context.Context, coarse []Hit, query string, rr Reranker, topK int) []Hit {
	if rr == nil || len(coarse) <= topK {
		return coarse // 无精排器，或候选本就不超 topK，直接用粗排
	}
	texts := make([]string, len(coarse))
	for i, h := range coarse {
		texts[i] = h.Doc.Text
	}
	order, err := rr.Rerank(ctx, query, texts)
	if err != nil || len(order) != len(coarse) {
		return coarse // 外部服务输出不可信：报错或长度不符都回退粗排
	}
	reranked := make([]Hit, 0, topK)
	for _, idx := range order {
		if len(reranked) == topK {
			break
		}
		if idx >= 0 && idx < len(coarse) {
			reranked = append(reranked, coarse[idx])
		}
	}
	return reranked
}
```

（带演示 reranker 的完整版本已通过 `go vet`/`go build` 验证：粗排第 3 的候选被精排提到第 1。）

**接入本项目的插入点**：`KBSearch.Execute` 里 `store.Search`（`tool.go:139`）之后、minScore 过滤（`tool.go:144`）之前——Search 时 topK 放大到 3 倍做候选池，精排后只留 3 个再走阈值过滤。

**成本权衡**：rerank 逐对打分，候选池 ×N 就是 ×N 次打分与延迟（专用 rerank API 按量计费，以官方文档为准）；候选池取 topK 的 2~4 倍是常见折中。决策依据还是 eval：**召回高、MRR 低**才上 rerank。

### 3.4 生产环境检索侧清单

教学版 `kb_search` 推向生产，检索侧还差这几块（面试"你的 RAG 还缺什么"的答题素材）：

**结果缓存**。同一问题重复检索时，query embedding 可以缓存——依据是 embedding 的确定性（同文本 = 同向量，第 4 章实验验证过）：

```go
// cachedEmbed 给查询侧 embedding 加内存缓存：同一问题重复检索不再调 API。
// 生产注意：换带 LRU/TTL 的缓存；并发加锁或 sync.Map；切换模型时清空（换模型 = 换向量空间）。
func cachedEmbed(emb Embedder, cache map[string][]float32, query string) ([]float32, error) {
	if v, ok := cache[query]; ok {
		return v, nil
	}
	vecs, err := emb.Embed([]string{query})
	if err != nil {
		return nil, err
	}
	cache[query] = vecs[0]
	return vecs[0], nil
}
```

**按来源去重（输出侧）**。一篇长文的相邻块相似度天然接近，top-k 容易被同一篇文档占满、prompt 视角单一。按来源限流让结果覆盖多篇文档：

```go
// dedupeBySource 让每个来源最多保留 maxPerSource 个块（输入已按分数降序）。
// 注意这是"输出侧去重"，与入库侧的同源幂等（kb.go:104-107）是两个不同问题。
func dedupeBySource(hits []vectorstore.Hit, maxPerSource int) []vectorstore.Hit {
	count := map[string]int{}
	out := make([]vectorstore.Hit, 0, len(hits))
	for _, h := range hits {
		src := h.Doc.Metadata["source"]
		if count[src] >= maxPerSource {
			continue
		}
		count[src]++
		out = append(out, h)
	}
	return out
}
```

（以上两个函数均已随验证代码编译通过。）

**超时与重试**。embedding 是出网 HTTP 调用，纪律与第 1、3 章一脉相承：本项目 embedding 客户端已内置 60s 超时（`embed.go:47`）；重试复用 `llm` 包 `ChatWithRetry`（`client.go:238`）的同款模式——区分可重试错误（5xx/429/网络抖动）与不可重试错误（4xx 鉴权/参数错）、指数退避、次数上限。**给 embedding 链路加重试的前提是 Embed 幂等**（同输入同输出、无副作用），embedding API 天然满足。

**可观测性**。Verbose 模式打印每次检索的 query、命中数、分数分布——bad case 排查（面试 Q3）的第一动作就是"把实际进 prompt 的 chunks 打出来看"。没有观测，一切调优都是玄学。

---

## 四、面试视角

> 以下每题给"标准回答 → 追问链 → 加分点"。自测方法：不看回答口述，再对照差距。

**Q1：为什么需要 RAG？把知识微调进模型不行吗？长上下文窗口都上百万 token 了，RAG 还有必要吗？**

标准回答：三个原因——① 时效：微调后知识冻结，RAG 改库即更新；② 可追溯：RAG 能给出引用来源，微调是黑盒记忆；③ 成本与风险：微调贵且可能灾难性遗忘。分工上微调教"风格/格式/领域语感"，RAG 教"事实知识"，互补。长上下文不能取代 RAG：全量塞库每轮都烧整库 token（成本不可承受）、塞得越多噪声越多（lost-in-the-middle，长上下文中段内容易被忽略）、且无法按用户做文档权限过滤。

追问链：
- "RAG 最大的短板？" → 答案质量受检索质量制约（检索不到就答不了），每次问答多一次检索延迟；微调的知识随取随用——所以才说互补；
- "什么场景选微调？" → 输出格式/话术强约束、领域术语密度高且稳定、事实更新少的场景。

加分点：一句话总结——"RAG 改开卷资料，微调改考生本身"；主动提 lost-in-the-middle 与权限过滤这两个长上下文解决不了的问题。

**Q2：chunk 切多大？怎么验证切得好不好？**

标准回答：没有银弹，常见起点 200~500 token + 10~20% overlap，按结构优先（本项目取 400 字符偏保守一侧，`chunk.go:44`）。验证两条：① 建检索测试集（问题 + 期望命中的 chunk）跑 recall@k；② bad case 分析看"期望内容是否被切散在两块里"——切散了加大 overlap 或改按结构切。

追问链：
- "切大/切小各自什么症状？" → 大：向量被多主题平均稀释、top-k 被同文相邻块占满；小：断章取义、块数膨胀成本上升；
- "overlap 为什么不能太大？" → 相邻块高度重复：浪费 token，top-k 里同一内容占两个坑位，实际信息量变少；
- "按 token 还是字符计？" → 生产按 token（接 tokenizer 对齐 embedding 上限）；教学按 rune 近似，中文 1 token ≈ 1.5~2 字符的换算要有数。

加分点：说出"第一调参位"的理由——chunk 在链路最上游，切坏了后面救不回来；调优顺序永远是先切分、再混合检索/rerank。

**Q3（本章核心题）：RAG 效果不好，bad case 怎么排查？**

标准回答（**按链路逐段定位**的骨架，建议脱稿能画）：先把 bad case 归三类，再按"上游到下游"顺序定位——

```
症状                    定位层              排查动作与对策
─────────────────────────────────────────────────────────
① 检索不到相关内容       检索层（召回）      query 侧：问题太短/口语化 → query 改写
（期望文档没进 top-k）                     索引侧：chunk 切坏 → 调切分；
                                          embedding 不适配领域 → 换模型重建索引
                                          度量侧：top-k 太小、阈值太高、
                                          字面查询 → 混合检索
② 检索到了但答非所问     排序层 / prompt 层  把实际进 prompt 的 chunks 打出来看：
                                          噪声混进 top-k → minScore 阈值 / rerank / 减 k；
                                          chunks 相关但模型没用好 → prompt 强化
                                          "仅基于给定资料回答"
③ 编造资料里没有的内容   生成层（忠实度）    prompt 加"资料不足就说不知道"；
                                          引用强制标注 + 人工核查；
                                          生成侧 eval 加忠实度指标
```

三个关键工程动作：**先打印实际进 prompt 的内容**（可观测性是排查前提）；**按上游到下游顺序查**（召回没解决前，调 prompt 都是无用功）；**每改一处用 eval 集前后对比**（第 6 章），没有 eval 的调优都是玄学。

追问链：
- "三类按什么顺序查？" → 检索 → 排序 → 生成：答非所问先看是不是噪声混进 top-k，编造先看是不是检索本来就空；
- "怎么证明调优有效？" → eval 集跑 recall@k / MRR / 忠实度前后对比，至少 3 条 bad case 逐条归因（练习 8-9 就是这套打法）。

加分点：给出具体观测手段（Verbose 打印命中分数分布、minScore 拦截日志）；提到 top-k 过多时 lost-in-the-middle 也会让"检索到了却答错"。

**Q4：混合检索和 rerank 分别解决什么？先上哪个？**

标准回答：混合检索解决**召回层**的"找不全"——向量（语义）与 BM25（字面）互补，错误码/型号/人名是向量短板，两路用 RRF 按名次融合（k=60 惯例）。rerank 解决**排序层**的"排不准"——粗排求快、精排求准，位置在检索与生成之间，用成本换精度。一个"找得更全"，一个"排得更准"。

追问链：
- "先上哪个？" → 看 bad case 类型：该搜到的没搜到（召回低）→ 混合检索；搜到了但排后面（召回高 MRR 低）→ rerank。这就是"指标驱动调优"；
- "两路分数直接相加行不行？" → 不行，量纲不同（余弦 [-1,1] vs BM25 [0,+∞)），RRF 只取名次天然无量纲。

加分点：知道 k=60 的出处与作用（压平名次差）；知道 rerank 只对 top-k 的 2~4 倍候选池做（成本）；能说出"精排失败静默回退粗排"的降级设计。

**Q5：RAG 的回答为什么要带引用？怎么实现？**

标准回答：引用 = 可验证性 = 用户信任 + 幻觉兜底——人能跳回原文核查才敢信。实现三件套：① 入库时 chunk 元数据写来源与序号（`kb.go:126-129`）；② 检索结果带 `[编号]+来源` 进 prompt（`tool.go:156-158`）；③ prompt 要求回答用 [编号] 标注。

追问链：
- "引用能杜绝幻觉吗？" → 不能：模型可能曲解原文甚至编造编号。引用只是让幻觉**可被低成本核查**，配套还要忠实度 eval 和"检索为空如实说"的闸门；
- "前端怎么配合？" → 引用做成可点击卡片跳回原文段落（第 7 章）——核查成本越低，信任越实。

加分点：把引用称为"人与模型之间的审计接口"；说出 Metadata 是"写入侧埋点、读取侧兑现"的契约——漏埋就断链。

**Q6：检索结果为空时，模型会有什么表现？怎么防？**

标准回答：模型倾向于**凭参数知识编造一个"看起来来自知识库"的答案**——tool 消息的存在暗示"应该有资料"，模型会圆场。本项目三层防御：① minScore=0.3 把低分噪声拦在 prompt 外（`tool.go:117`）；② 空结果返回明确文案"知识库未覆盖，不要凭自身知识编造来源"（`tool.go:151-153`）；③ Description 预写防幻觉指令（`tool.go:59`）。

追问链：
- "为什么不把低分结果给模型自己判断？" → 低分噪声稀释 prompt，模型抓只言片语强行作答——**阈值宁可误杀**；0.3 是经验起点，看分数分布用 eval 调；
- "模型还是编了怎么办？" → 生成侧 eval 加忠实度指标（答案是否被检索内容支持，可 LLM-as-judge，注意评判偏差，第 6 章）。

加分点：点出范式意义——**防幻觉不仅靠 prompt 请求模型自律，更靠工具层"不给模型犯错的材料"**（行为约束做在数据流里，比做在祈祷里可靠）。

---

## 五、常见坑

1. **按 byte 切中文，产出乱码块**。`len(s)` 和 `s[i:j]` 都是字节单位，UTF-8 一个汉字占 3 字节——按 byte 切把汉字从中间劈开，入库是乱码。正确姿势：`[]rune(s)` 按字符切（`chunk.go:152`），测试用 `utf8.ValidString` 钉死（`chunk_test.go:83`）。
2. **OverlapChars ≥ MaxChars 死循环**。硬切步长 = MaxChars - OverlapChars，步长 ≤ 0 时 `for start += step` 永不前进。防御：入口归一化钳制 overlap（`chunk.go:134-136`），测试用带超时的 watchdog 验证（`chunk_test.go:129`）。铁律：凡参与循环条件的算术，先想"什么输入会让循环不终止"。
3. **入库与查询用不同 embedding 模型**。换模型 = 换向量空间，旧索引全废。最阴险的是**同维度异模型**：库里 bge-m3（1024 维），查询换另一个 1024 维模型，维度校验不报错、检索全错——静默故障最难查。防御：模型名写进配置与元数据，换模型走"清空 → 全量重建"。
4. **不设 minScore，低分噪声进 prompt**。top-k 只保证"分数最高"不保证"足够相关"——库里没有相关内容时 Search 照样返回 k 条。噪声稀释 prompt，模型抓只言片语强行作答，幻觉率比不检索还高。0.3 是经验起点，用分数分布和 eval 校准（`tool.go:117`）。
5. **Metadata 漏写 source，引用链断裂**。检索结果拿不到来源，"出自《XX》第 N 段"无从标注，可溯源卖点失效。契约写在写入侧：Ingest 强制写 `source` + `chunk`（`kb.go:126-129`），测试断言 Metadata 完整（`kb_test.go:67-72`）。
6. **调试 `fmt.Println` 残留进库函数**。当前 `chunk.go:89`、`chunk.go:99` 就有两行调试打印——库函数每次被调都向 stdout 喷内容：CLI 里冲乱流式输出，批处理场景刷屏拖慢。库代码不该有副作用输出，观察行为用测试或注入的 logger。教训：调试输出随调随删，提交前 `grep -rn "fmt.Println" internal/` 扫一遍。

---

## 六、动手练习

本章对应阶段二练习 3、4（Go 侧，代码位置均带 `TODO(练习N)` 标注）。

**练习 3：chunking（`mini-agent/internal/rag/chunk.go`）**

实现 `Chunk(text string, opts ChunkOptions) []string`：结构优先贪心打包（段落不拆）+ 超长段硬切带 overlap + `[]rune` 计量 + 参数归一化防御。本地版本还是骨架的话按 TODO 块【任务】【提示】【验收】完成；已实现过的话，重读边界清单逐条对照：多段打包不超限不拆段、硬切重叠正确、纯中文无乱码、空输入返回 nil、OverlapChars ≥ MaxChars 不死循环。

**练习 4：Ingest + KBSearch.Execute（`kb.go`、`tool.go`）**

- `Ingest(source, text string) (int, error)`：chunk → 批量 embed → Add 三步编排，带幂等短路（sameChunks）、先删旧块再入库、Metadata 写 source+chunk；
- `Execute(args string) (string, error)`：解析参数 → 空库短路 → embed → top-k → minScore 过滤 → `[编号]+来源` 格式化输出。

**验收（端到端）**：

1. `cd mini-agent && go test ./internal/rag/` 全绿；
2. 配好 `DEEPSEEK_API_KEY` + `SILICONFLOW_API_KEY`，`go run ./cmd/agent`；
3. `/learn docs/embedding-vectordb-guide.md`（或任意本地 md），观察"已学习：N 个块入库"；
4. 问文档里的问题（如"bge-m3 是多少维的？"）——模型应调用 kb_search 并给出带 `[1]` 引用的回答（Verbose 模式能看到检索到的块与相似度）；
5. 再问文档外的问题（如"今天星期几"）——观察模型是否如实说"知识库未覆盖"，体会 2.3 节的防幻觉闸门。

参考答案（**完成后再看**）：

- 练习 3：`docs/solutions/stage-02/exercise-3-chunking.md`
- 练习 4：`docs/solutions/stage-02/exercise-4-rag-tool.md`

---

## 本章小结

- RAG = 检索 + 增强 + 生成，本质是开卷考试：知识可更新、答案可溯源、减少幻觉；与微调分工——RAG 教事实，微调教风格。
- 双链路全图：写入（解析 → chunking → embed → 入库）与查询（embed → top-k → 拼 prompt → 带引用生成）必须用**同一个 embedding 模型**，换模型 = 换向量空间。
- chunking 决定检索质量上限：结构优先 + 硬切兜底，块太大稀释主题、块太小断章取义；chunk 是第一调参位，先调切分再上花哨检索。
- 混合检索补字面短板（RRF 按名次融合，k=60），rerank 用成本换排序精度（粗排多捞、精排取 top-k、失败回退）；上哪个由 eval 指标说了算。
- `kb_search` 是防幻觉设计的完整范例：Description 四要素、空库短路、minScore 阈值、空结果如实告知；引用是写入侧与读取侧的契约（Ingest 埋 Metadata，Execute 带编号进 prompt，prompt 要求标注）——防幻觉做在数据流里比做在 prompt 祈祷里可靠。
- bad case 三板斧按链路逐段定位：检索不到（召回）→ 答非所问（排序/prompt）→ 编造（忠实度）；先打印进 prompt 的内容，再从上游往下游查。

下一章：[第 6 章：长期 Memory 与 Evals](06-memory-and-evals.md)——记忆即检索（与 RAG 共享 embedding + 向量库底座），以及用 recall@k / MRR / LLM-as-judge 把本章所有调优位变成"用数字说话"的闭环。
