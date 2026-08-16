# 第 4 章：Embedding 与向量检索——给文本装上"语义坐标"

> 对应阶段：阶段二（进阶）· 项目 1 扩展（`mini-agent/`）
> 代码位置：`mini-agent/internal/embed/`（embedding 客户端）、`mini-agent/internal/vectorstore/`（内存向量库）
> 前置：第 1 章（LLM API 与 messages 协议）；第 2、3 章的工具层与 ReAct 循环不是本章必需，但检索最终会以"工具"形态接回 Agent（第 5 章）
> 学完后你能讲清：embedding 如何把"语义相近"变成可计算的距离、余弦相似度为什么是文本检索的默认度量、批量 embedding 的三个工程坑、10 万条以内为什么不需要 HNSW、向量库怎么选型——这是第 5 章 RAG 全链路的两大底座。

---

## 本章地图

- 从一个对不上的例子开始："怎么退款" vs "退货流程"——关键词匹配的字面局限
- embedding：把文本映射为固定维向量，语义近 = 向量距离近
- 余弦相似度：公式、为什么只看方向不看长度、值域直觉
- 一个事实：DeepSeek 没有 embedding API——硅基流动 bge-m3 / Ollama 本地 / OpenAI
- 批量调用与按 index 归位：响应顺序不可假设
- 检索 = 暴力最近邻 top-k：O(N) 什么时候够用；HNSW 的分层图直觉
- 向量库选型：内存手写 / pgvector / Qdrant / Milvus，按什么标准选
- 进阶：pgvector 实战、归一化后"内积 = 余弦"、embedding 结果缓存、维度一致性纪律

---

## 一、概念详解

### 1.1 从一个对不上的例子说起：关键词匹配的字面局限

第 1 章说过，LLM 的上下文窗口有限，不可能把全部资料塞进 prompt。RAG 的思路是：**先从资料库里找出和问题最相关的几段，再让模型基于这几段回答**（第 5 章完整展开）。

问题卡在第一步：怎么判断"相关"？

最直觉的做法是关键词匹配（搜索引擎的经典算法 BM25 就是这一路）：把文档和问题都拆成词，看词的重合度。它在字面一致的查询上工作得很好，但用户问"**怎么退款**"，文档里写的是"**退货流程说明**"——字面一个词都对不上，匹配失败。而这两段话的**语义**明明高度相关。

embedding 的做法：用一个专门的模型把任意文本映射成一个**固定长度的浮点数向量**（本项目用的 bge-m3 输出 1024 维），训练目标就是让"语义相近的文本，向量在空间里的距离也近"。于是"找相关内容"从语言问题变成了数学问题——**算距离**：

```
"怎么退款"        → [0.12, -0.38, 0.91, ...共1024维]  ─┐
                                                        ├─ 余弦相似度 ≈ 0.87（很相关）
"退货流程说明"    → [0.10, -0.35, 0.88, ...]          ─┘

"今天天气不错"    → [-0.55, 0.21, 0.03, ...]          ── 相似度 ≈ 0.12（不相关）
```

两个工程直觉值得记住：

- **embedding 模型是"语义压缩器"**。变长的文本被压进定长的向量：字面形式（措辞、语种、句式）被丢弃，语义内容被保留。这就是本章标题说的"给文本装上语义坐标"——从此每段文本在空间里有位置，"找相似"就是"找邻居"。
- **维度固定是刻意设计**。无论输入 10 个字还是 500 个字，输出都是 1024 维。定长才能两两算距离，才能建索引。它同时也意味着容量有限：一篇几万字的长文压进 1024 维，主题会被"稀释"——这是第 5 章必须先做 chunking（切块）的根本原因。

### 1.2 余弦相似度：为什么文本检索只看方向

有了向量，下一步是定义"距离"。文本检索的默认度量是**余弦相似度**：

```
cos(a, b) = a·b / (|a| · |b|)

其中  a·b = Σ aᵢ·bᵢ        （点积：对应维度相乘再求和）
      |a| = √(Σ aᵢ²)        （模长：各维度平方和开根号）
```

几何意义：两个向量夹角的余弦。**它只关心方向，不关心长度**——这正是文本场景要的性质：

- 同一个主题，写 100 字和写 500 字，两个向量的**指向**大致相同（语义方向一致），但**模长**可能差不少。如果用欧氏距离（|a−b|，对长度敏感），"同一主题的长短两段文字"可能被判成不相似——文本长度不应影响语义判断。
- 余弦把模长从公式里除掉（分母 |a|·|b|），等效于先把两个向量都缩放到单位长度再比方向。

值域直觉（背下来，面试和调阈值都用得到）：

| 值 | 几何含义 | 语义含义 |
| --- | --- | --- |
| 1 | 完全同向 | 语义相同 |
| 0 | 正交（90°） | 互不相关 |
| -1 | 完全反向 | 语义相反（实际检索中极少出现） |

工程经验：bge-m3 这类模型上，真实相关的文本对相似度通常在 0.5 以上，0.3 以下基本无关。本项目第 5 章的检索工具就把得分低于 0.3 的结果直接过滤（`mini-agent/internal/rag/tool.go:117` 的 `minScore`）。阈值没有普适值，要按你的数据实测，以 eval 数字为准（第 6 章）。

### 1.3 一个事实：DeepSeek 没有 embedding API

新手常在这里卡壳：配好了 `DEEPSEEK_API_KEY`，兴冲冲去找 embedding 接口——**DeepSeek 官方不提供 embedding API**（以官方文档为准）。生成模型和 embedding 模型是两类模型，厂商可以只做其一。

应对方案三选一（价格、额度均有时效，以各官方页面为准）：

| 方案 | 说明 | 代价 |
| --- | --- | --- |
| 硅基流动 SiliconFlow + `BAAI/bge-m3` | OpenAI 兼容接口，bge-m3 有免费档，中英双语效果好，RAG 主流选择；**本项目默认**（`embed.go:43-45`） | 注册送额度，学习基本零成本 |
| Ollama 本地跑 bge-m3 | `ollama pull bge-m3`，同样提供 OpenAI 兼容接口（`http://localhost:11434/v1`），零成本零网络、数据不出本机 | 首次加载慢，吃内存（bge-m3 约 2GB+） |
| OpenAI `text-embedding-3-small` | 效果好 | 收费且需海外网络/支付方式，本项目不推荐 |

这个事实推出两个设计决策，在代码精讲里都会看到：

1. **embedding 客户端必须与 LLM 客户端分包**。baseURL、apiKey、模型名、端点（`/embeddings` vs `/chat/completions`）全都不同，硬塞进 `llm.Client` 只会让它背两套配置（`embed.go:1-12` 包注释写的就是这个理由）。
2. **"OpenAI 兼容协议"这个外壳可以整个复用**：同样的 `Authorization: Bearer` 头、同样的请求/响应 JSON 风格，甚至连错误类型都直接复用 `llm.APIError`（`embed.go:130`）。换一个兼容厂商只改 baseURL——第 1 章"baseURL 可替换"的价值在这里第二次兑现。

先看懂协议（硅基流动为例，与 OpenAI 格式完全一致）：

```bash
curl https://api.siliconflow.cn/v1/embeddings \
  -H "Authorization: Bearer $SILICONFLOW_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "BAAI/bge-m3",
    "input": ["怎么退款", "退货流程说明"]
  }'
```

响应（精简）：

```json
{
  "data": [
    { "index": 0, "embedding": [0.012, -0.038, "...共1024个浮点数..."] },
    { "index": 1, "embedding": [0.010, -0.035, "..."] }
  ],
  "usage": { "total_tokens": 12 }
}
```

### 1.4 批量调用与按 index 归位：本包最核心的坑

注意上面请求里 `input` 是**数组**：embedding 是批量接口，一次请求翻译多段文本。入库几百个 chunk 时，批量比逐段调用省掉大量 HTTP 往返，差距肉眼可见。这也是为什么客户端的超时给得比看起来长（60 秒，`embed.go:47`）——单批文本多时需要余量。

响应里每个元素带一个 `index` 字段，标明它对应 `input` 的第几段。**这就是坑：`data` 数组的顺序不能假设与输入顺序一致**。部分服务商会按 token 数等内部策略重排，协议文档不保证顺序。归位时必须按 `index` 放：

```go
vecs := make([][]float32, len(texts))
for _, d := range resp.Data {
	vecs[d.Index] = d.Embedding // 按 index 归位，不是 vecs[i]
}
```

如果按数组下标直接对应，文本和向量就错位了——入库时不会报任何错，但检索结果全是错的，而且极难排查（阶段文档注意事项第 1 条记录的就是它）。这个坑如此重要，项目专门写了一个"乱序响应"的测试来钉死它（`embed_test.go:48`，见代码精讲 2.2）。

### 1.5 检索 = 暴力最近邻 top-k；HNSW 什么时候才需要

假设库里有 N 条向量，查询来了：把查询也向量化，然后——**和库里每条向量各算一次余弦相似度，按得分排序，取前 k 个**。这就是暴力检索（flat / brute-force），O(N) 全表扫描，没有任何索引。

听起来"很慢"，算一笔账（量化能力是这一节的重点）：

- 1024 维 float32 向量 = 4KB。10 万条记录 = 约 400MB 内存、每次查询约 1 亿次浮点乘加。
- 现代 CPU 单核每秒百亿次浮点运算量级，1 亿次 = **毫秒级**。而且两条向量的模长可以预计算缓存，实际更省。

结论：**10 万条以内，纯内存暴力检索完全够用，而且结果是精确的**。学习项目和个人知识库场景，暴力版反而更简单、召回还是 100%。

那 HNSW 解决什么问题？数据量到百万级以上，O(N) 全表扫描就真的慢了（百万条 ≈ 10 亿次运算、4GB 内存），这时需要 **ANN（近似最近邻）索引**。HNSW（分层可导航小世界图，Hierarchical Navigable Small World）是主流算法，直觉用一张图讲清：

```
第 2 层（最稀疏）  ① ──────────────── ⑨              ← 入口层：大步跳跃，粗定位
                    \                /
第 1 层             ① ─── ④ ─── ⑥ ── ⑨ ─── ⑫        ← 每层逐步缩小范围
                     \    \     \     \
第 0 层（最密）  ① ② ③ ④ ⑤ ⑥ ⑦ ⑧ ⑨ ⑩ ⑪ ⑫ …          ← 包含全部节点，精细收敛
```

查询过程：从最上层入口点出发，在当前层**贪心**走向"离查询向量最近"的邻居；在当前层无法更近时，下到更密的一层继续走；到第 0 层时收敛在目标附近的小邻域。每一层把搜索范围指数级缩小，整体复杂度近似 **O(log N)**。

代价必须说全（只说优点就是背名词）：

- **召回率略降**：ANN 是"近似"，贪心路径可能错过真正的最近邻。用 `hnsw.ef_search` 等参数在速度与准确率间权衡。
- **建索引有成本**：内存额外开销、写入变慢、参数要调。
- **小数据量建索引反而更差**：pgvector 的实践经验是 1 万条以下顺序扫描更准，索引白建（见进阶 3.1）。

"什么时候不需要 HNSW"是面试里区分"真做过"和"背名词"的经典问题——能脱口而出上面那笔账的，才是做过的。面试视角 Q3 再完整演练。

### 1.6 向量库选型：按驱动因素选，不按热度选

向量库 = 给向量建索引的专用存储。它在"算距离"之外还顺带解决：持久化、按元数据过滤（"只搜 2024 年的文档"）、增删改、并发。四种典型方案：

| 方案 | 数据量甜点 | 优点 | 代价 | 适用场景 |
| --- | --- | --- | --- | --- |
| 内存手写（本项目 `vectorstore`） | <10 万 | 零依赖、精确召回、代码即教材 | 进程退出即丢（需自做持久化）、教学版无并发保护 | 学习、个人知识库、CLI 工具 |
| pgvector（Postgres 扩展） | 万级~百万级 | 向量与业务数据同库，SQL 过滤一把梭，运维 = 一个 Postgres | 多一个数据库要维护（很多项目本来就有 PG） | 项目 2 首选、中小生产系统 |
| Qdrant | 百万级+ | 专用向量库，过滤能力强，Rust 实现性能好 | 独立组件，运维成本上升 | 中大型系统、复杂过滤需求 |
| Milvus | 千万~亿级 | 分布式、GPU 索引、为超大规模而生 | 运维最重 | 大规模生产 |

选型驱动因素按优先级：**数据量**（决定要不要 ANN 索引）→ **QPS**（决定要不要专用引擎）→ **过滤需求**（元数据过滤复杂度高时专用库更顺手）→ **运维成本**（每多一个组件，多一份部署、监控、备份）。

本教程的路径是刻意安排的：先手写内存版理解检索本质（本章练习 2），第 5 章用它跑通 RAG 全链路，项目 2 再换 pgvector（进阶 3.1 带你走一遍）。这样面试时两种都能讲，还知道专用向量库到底替你做了什么。

---

## 二、代码精讲

本章精讲两个包：`internal/embed`（162 行，把文本翻译成向量）和 `internal/vectorstore`（327 行，给向量建索引、按相似度检索）。它们在 RAG 链路中的位置（`store.go:1-18` 的包注释里画了同一张图）：

```
文档 --(chunking，第5章)--> 文本块 --(internal/embed)--> 向量 --(vectorstore.Add)--> 向量库
用户问题 --(internal/embed)--> 查询向量 --(vectorstore.Search)--> 相关文档 --> 拼进 prompt
```

### 2.1 embed 包：又一个 OpenAI 兼容客户端

打开 `mini-agent/internal/embed/embed.go`，先看为什么它独立成包（`embed.go:1-12` 包注释）：DeepSeek 没有 embedding API，embedding 必须走另一家服务商，baseURL、apiKey、模型名、端点全都不同；与 `llm.Client` 相同的只是"OpenAI 兼容协议"的外壳——同样的 Authorization 头、同样的错误处理模式。

常量定义（`embed.go:28`）：

```go
// bge-m3 的输出维度。写死它的意义：入库前校验向量长度，
// 维度错了说明模型/服务商配错了，越早报错越好——
// 否则错误向量悄悄入库，检索结果全错还很难排查。
const BgeM3Dimensions = 1024
```

**把上游的不变量写成显式常量，是防御性编程的基本功**：维度错 = 配置错，在第一次 embedding 时就该炸出来，而不是等检索结果离谱时再回头查。

`Client` 结构（`embed.go:32`）与 `llm.Client` 同构，四个字段：`baseURL`、`apiKey`、`model`、`httpClient`。构造函数（`embed.go:41`）默认指向硅基流动：

```go
func NewClient(apiKey string) *Client {
	return &Client{
		baseURL: "https://api.siliconflow.cn/v1",
		apiKey:  apiKey,
		model:   "BAAI/bge-m3",
		httpClient: &http.Client{
			Timeout: 60 * time.Second, // embedding 是批处理接口，单批文本多时需要余量
		},
	}
}
```

两个链式配置方法承担不同的语义份量：

- `WithBaseURL`（`embed.go:53`）：切服务商，比如本地 Ollama 的 `http://localhost:11434/v1`。安全操作，随便切。
- `WithModel`（`embed.go:60`）：切模型，**注释里挂着一条重要警告**——

```go
// WithModel 切换 embedding 模型。
// 注意：换模型 = 换向量空间，已入库的旧向量全部作废，必须重建索引。
func (c *Client) WithModel(model string) *Client
```

"换模型 = 换向量空间"是本章最重要的纪律之一：不同模型的向量空间各自独立训练，同样的文本在两个空间里的坐标毫无对应关系，跨空间算余弦是无意义的数学操作。进阶 3.4 和面试 Q6 展开。

### 2.2 Embed：批量请求、按 index 归位与三层防护

请求与响应结构（`embed.go:68`、`embed.go:79`）：

```go
type embeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"` // 数组——批量接口
}

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`    // 对应 input 的第几段
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}
```

`Embed` 方法（`embed.go:93`）承诺：`result[i]` 就是 `texts[i]` 的向量。兑现这个承诺靠的是四层检查，一层都不能少：

**第一层：输入校验**（`embed.go:94-102`）——空切片直接拒绝；逐段检查空白文本（`strings.TrimSpace`），把"texts[2] 是全空格"这种输入在花钱调 API 之前拦下。

**第二层：HTTP 与状态码**（`embed.go:117-131`）——非 200 时返回 `*llm.APIError`：

```go
if resp.StatusCode != http.StatusOK {
	return nil, &llm.APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
}
```

这是第 1 章埋的伏笔在兑现：**错误类型是控制流的一部分**。未来给 embedding 加重试时（429/5xx 重试、401 不重试），`errors.As` 取状态码的逻辑与 `llm` 包完全同构。测试 `embed_test.go:111` 就验证了 401 能还原成 `APIError` 且状态码保真。

**第三层：归位与边界**（`embed.go:138-153`）——本包的核心坑（概念 1.4）对应的防御代码：

```go
if len(embResp.Data) != len(texts) {
	return nil, fmt.Errorf("embed: got %d embeddings for %d texts", len(embResp.Data), len(texts))
}

result := make([][]float32, len(texts))

for _, d := range embResp.Data {
	if d.Index < 0 || d.Index >= len(texts) {
		return nil, fmt.Errorf("embed: index %d out of range [0,%d]", d.Index, len(texts))
	}

	if len(d.Embedding) != BgeM3Dimensions {
		return nil, fmt.Errorf("embed: text[%d] dim = %d, want %d", d.Index, len(d.Embedding), BgeM3Dimensions)
	}
	result[d.Index] = d.Embedding
}

for i, v := range result {
	if v == nil {
		return nil, fmt.Errorf("embed: texts[%d] missing in response", i)
	}
}
```

逐行值得讲的点：

- **先验数量，再按 index 归位，最后查缺失**——三步缺一不可。只验数量不查缺失，重复 index 会静默覆盖（两个元素都写 `index: 0` 时数量依然对）。
- **index 越界防护是一个真实修过的 off-by-one**：初版上界写成 `d.Index > len(texts)`，`index` 恰好等于 `len(texts)` 时穿透校验，下一行 `result[d.Index]` 直接 panic。教训写进了条件里：越界判断必须包含等号边界（`>=`），数组下标的合法区间是 `[0, len)`。
- **维度校验在归位时做**（`embed.go:149`）：服务商配错模型时，第一次调用就报"dim = 512, want 1024"，而不是让错误向量悄悄入库（呼应 2.1 的常量注释）。

**第四层：测试钉死行为**。`embed_test.go` 用 `httptest.NewServer` 起假服务器，三个用例分别对应上面的坑：`embed_test.go:48` 返回乱序 data（index 2,0,1），验证归位正确；`embed_test.go:100` 返回 3 维向量，验证维度报错；`embed_test.go:89` 验证空输入拒绝。**不测网络的客户端测试**——假服务器模式在第 1 章项目的测试里反复出现，值得变成肌肉记忆。

### 2.3 vectorstore 包：Document 设计与入库校验

`Document`（`store.go:31`）是向量库里的一条记录，四个字段各有设计理由：

```go
type Document struct {
	ID       string            // 调用方生成（如 "doc3-chunk7"）；没有 ID 无法更新、去重、删除
	Text     string            // 原始文本：Search 命中的结果要直接能拼进 prompt，只存向量就得多回查一次原文
	Vector   []float32         // Text 的 embedding（bge-m3 为 1024 维）
	Metadata map[string]string // 溯源信息：来源文档名、chunk 序号、页码
}
```

`Metadata` 值得单独强调：**引用溯源全靠它**。第 5 章的 RAG 回答要标注"这个答案来自《XX 文档》第 3 段"，没有 Metadata 答案就无法给出出处，用户无法验证，可信度大打折扣。它还是元数据过滤（"只搜某个文档的 chunk"）的载体——`FindByMetadata`（`store.go:305`）和第 6 章的 memory 工具都依赖它。

`Hit`（`store.go:50`）= 文档 + 余弦得分（`float64`，范围 [-1,1]）。`Store`（`store.go:59`）只有两个字段：`docs []Document` 平铺存储、`dim int` 记录全库统一维度——第一条 Add 的记录定下维度，之后所有记录必须与它一致。

`Add`（`store.go:79`）的两个设计决策：

```go
// Add 批量入库文档。任一文档校验失败则整批拒绝（all-or-nothing），
// 避免"一半入库一半没入"的中间状态——调用方重试时不用先清理。
func (s *Store) Add(docs ...Document) error {
	// 先整批校验，再统一追加，保证 all-or-nothing。
	for i, d := range docs {
		if d.ID == "" { ... }
		if len(d.Vector) == 0 { ... }
		want := s.dim
		if want == 0 {
			want = len(docs[0].Vector)
		}
		if len(d.Vector) != want {
			return fmt.Errorf("vectorstore: docs[%d] (%s) dim = %d, want %d", i, d.ID, len(d.Vector), want)
		}
	}

	if s.dim == 0 && len(docs) > 0 {
		s.dim = len(docs[0].Vector) // 第一条记录定维度
	}
	s.docs = append(s.docs, docs...)
	return nil
}
```

- **all-or-nothing**：先整批校验、通过后再统一追加。任一文档失败则整批拒绝，调用方重试时不用清理"入了一半"的中间状态。这是写接口设计里性价比最高的一条纪律。
- **维度一致性是硬约束**（`store.go:94`）：余弦相似度要求两个向量等长，维度不同的向量根本无法计算。放不同维度混进库里，Search 时要么 panic 要么算出无意义结果——而且错误会潜伏到检索时才暴露。维度混杂的真实来源通常是"换了 embedding 模型忘了重建索引"（进阶 3.4）。

### 2.4 余弦相似度与暴力 top-k 检索

`CosineSimilarity`（`store.go:133`）——公式 1:1 落地，但累加方式藏着一个面试加分点：

```go
func CosineSimilarity(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, errors.New("vectorstore: dim mismatch")
	}
	if len(a) == 0 {
		return 0, fmt.Errorf("vectorstore: empty vectors")
	}

	var dot, normA, normB float64 // 注意：三个累加器都是 float64

	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}

	if normA == 0 || normB == 0 {
		return 0, fmt.Errorf("vectorstore: zero vector has no direction, cosine similarity undefined")
	}

	return dot / (math.Sqrt(normA) * math.Sqrt(normB)), nil
}
```

三个细节：

1. **输入 float32、累加 float64**（`store.go:142-148`）。float32 只有约 7 位十进制有效数字，1024 项连加时会不断发生"大数吃小数"，精度损失已经可感知；float64 累加的成本几乎为零。**中间计算升精度、输入输出保持 float32** 是数值计算的常见折中。面试问"为什么中间用 float64"就是考这个。
2. **一次循环同时累加三个量**（dot、normA、normB），只遍历一遍。
3. **零向量除零防护**（`store.go:150`）：零向量模长为 0、公式分母为 0，而零向量没有"方向"，相似度无定义——返回 error 而不是 NaN。

`Search`（`store.go:180`）——暴力 top-k 全貌：

```go
func (s *Store) Search(query []float32, topK int) ([]Hit, error) {
	if topK <= 0 {
		return nil, fmt.Errorf("vectorstore: topK must be positive, got %d", topK)
	}
	if len(s.docs) == 0 {
		return []Hit{}, nil // 空库返回空切片 + nil error，不算错误
	}
	if len(query) != s.dim {
		return nil, fmt.Errorf("vectorstore: query dim = %d, want %d", len(query), s.dim)
	}

	hits := make([]Hit, 0, len(s.docs))
	for _, d := range s.docs {
		score, err := CosineSimilarity(query, d.Vector)
		if err != nil {
			return nil, fmt.Errorf("vectorstore: score doc %s: %w", d.ID, err)
		}
		hits = append(hits, Hit{Doc: d, Score: score})
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })

	if topK > len(hits) {
		topK = len(hits) // topK 超过库存量时返回全部，不报错
	}
	return hits[:topK], nil
}
```

边界决策一览（每个都是调用方契约，测试逐条钉住）：

- `topK <= 0` 返回 error——调用方传 0 几乎一定是 bug，静默返回空会把 bug 藏起来；
- 空库返回空切片 + nil——"库里没东西"不是错误；
- query 维度不符报错——理由同 Add 的维度校验；
- **排序用 `sort.SliceStable`**（`store.go:201`）：得分相同时保持入库先后顺序，检索结果可复现。普通 `sort.Slice` 不保证相等元素的相对顺序，同一份数据两次查询可能返回不同顺序——调试和 eval 时这种不确定性很烦人；
- 全排序是 O(N log N)，只需 top-k 可用堆做到 O(N log k)——10 万条内差距不大，先不优化（面试可能问，见 Q3 追问）。

### 2.5 持久化与维护操作

内存库进程退出即丢，持久化的真实动机是**省钱**：embedding 调用要花钱花时间，不能每次启动都重新 embedding 全部文档（`store.go:210` 的 TODO 注释里写明了这个动机）。

`Save`（`store.go:244`）把 `storeFile{Dim, Document}` 序列化成 JSON 写盘，用的是一个值得记住的模式——**原子写入**：

```go
tmp, err := os.CreateTemp(filepath.Dir(path), ".vectorstore-*.tmp")
...
if _, err := tmp.Write(data); err != nil { ... }
if err := os.Rename(tmpName, path); err != nil { ... }
```

先写临时文件、再 `rename` 到目标路径：rename 在同一文件系统内是原子操作，进程在写入中途崩溃也不会留下半个损坏文件。**凡是要覆盖写重要文件，都用"临时文件 + rename"**。

`Load`（`store.go:270`）的纪律是**不信任外部输入**：JSON 文件可能被手改坏，所以不能只 Unmarshal 完事——要恢复 `dim`（兼容无 `dim` 字段的旧文件，退化为取第一条向量维度，`store.go:281-284`），并逐条校验 ID 非空、向量非空、维度与 `dim` 一致（`store.go:286-298`）。

两个已知的取舍（注释里都写了，面试可聊）：

- float32 经 JSON 文本序列化有精度损失（十进制表示不精确）。学习项目可接受；生产大规模场景用二进制格式（gob/protobuf）或专用向量库。
- `storeFile` 里存了 `dim` 做冗余校验——存储格式里带自描述元信息，是坏文件能被快速识别的关键。

`FindByMetadata`（`store.go:305`）线性扫描匹配 `Metadata[key] == value`，O(N) 但 10 万条内无感；`Delete`（`store.go:316`）按 ID 删除，注意一个细节：删光最后一条后把 `s.dim` 归零（`store.go:320-322`）——空库不该"记住"旧维度，否则换模型重建索引时第一条新向量会被旧 `dim` 拒绝。

---

## 三、进阶拓展（带代码）

### 3.1 pgvector 实战：从内存库到真数据库

**为什么**：内存库的天花板在第 5 章项目 2 会很快撞到——进程退出即丢、无并发保护、元数据过滤要自己写循环。pgvector 是 Postgres 的向量扩展：**向量与业务数据住同一个库**，元数据过滤就是普通 `WHERE`，持久化/备份/连接池全是 Postgres 现成能力。它是"中小生产系统向量库"的默认答案之一。

起库（Docker，一行）：

```bash
docker run -d --name pgvector \
  -e POSTGRES_PASSWORD=postgres -p 5432:5432 \
  pgvector/pgvector:pg16
```

建表（`vector(1024)` 对应 bge-m3 维度——维度在这里同样是硬约束）：

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE documents (
    id         BIGSERIAL PRIMARY KEY,
    content    TEXT NOT NULL,
    embedding  vector(1024) NOT NULL,
    metadata   JSONB DEFAULT '{}'
);

-- HNSW 索引：数据量大（>1万条）后再建，小数据量顺序扫描反而更准
CREATE INDEX ON documents USING hnsw (embedding vector_cosine_ops);
```

写入（Go，`pgvector-go` 提供向量类型的序列化）：

```go
import "github.com/pgvector/pgvector-go"

_, err := db.Exec(
	`INSERT INTO documents (content, embedding, metadata) VALUES ($1, $2, $3)`,
	content, pgvector.NewVector(vec), metadataJSON,
)
```

查询——`<=>` 是余弦**距离**运算符（= 1 − 余弦相似度，越小越相似），元数据过滤就是一条 `WHERE`：

```sql
SELECT content, embedding <=> $1 AS distance
FROM documents
WHERE metadata->>'source' = 'refund-docs'   -- 元数据过滤，等价于 FindByMetadata
ORDER BY embedding <=> $1
LIMIT 5;
```

**HNSW 索引何时建**（与概念 1.5 呼应）：1 万条以下顺序扫描足够快且结果精确，建了索引反而召回略降；数据量上万、查询 QPS 上来之后再建，并用 `hnsw.ef_search` 在速度与召回间调。生产注意：批量写入放事务里（逐条 INSERT 的网络往返会成为瓶颈）；备份跟着 Postgres 常规方案走，不用单独造轮子。

### 3.2 归一化：让"内积 = 余弦"的数学等价优化

**为什么**：`CosineSimilarity` 每次调用都要算两个模长（两遍乘加 + 两次开方）。但模长是向量的固有属性——**入库时把每个向量归一化为单位向量（模长 = 1），余弦公式就退化为点积**：

```
cos(a, b) = a·b / (|a|·|b|) = a·b / (1·1) = a·b
```

每次相似度计算省掉三分之二的运算量。10 万条库 × 每次查询全扫，这个节省是实打实的。不少 embedding 模型的官方实践也建议归一化后用内积（以官方文档为准）。

教学代码（已验证可编译运行）：

```go
// Normalize 把向量缩放为单位向量（模长 = 1）。
// 归一化只在入库时做一次，之后每次相似度计算都能受益。
func Normalize(v []float32) []float32 {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return append([]float32(nil), v...) // 零向量没有"方向"，无法归一化，原样返回
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}

// Dot 计算两个向量的内积。
// 当两个向量都已归一化时，内积 = 余弦相似度——
// 省掉了每次计算里两个模长的乘加与开方。
func Dot(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("dim mismatch: %d vs %d", len(a), len(b))
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i]) // 仍用 float64 累加，理由同 store.go:142
	}
	return dot, nil
}
```

用法纪律：**入库时 `Normalize` 一次、查询向量也 `Normalize` 一次，之后全部用 `Dot`**。归一化是不可逆的尺度变换（原始模长丢失），但 embedding 的模长本身不携带语义，丢了不可惜。

对 pgvector 用户的意义——建索引时选的 ops 就是在选距离度量：

| ops | 运算符 | 适用 |
| --- | --- | --- |
| `vector_cosine_ops` | `<=>` 余弦距离 | 未归一化向量的默认选择 |
| `vector_ip_ops` | `<#>` 负内积 | 归一化向量：负内积升序 = 余弦降序，排序结果与余弦完全一致，计算更省 |
| `vector_l2_ops` | `<->` 欧氏距离 | 归一化后与余弦排序等价（L2² = 2 − 2·内积，单调相关） |

数学等价换来工程自由：归一化之后，三种度量的**排序结果一致**，你可以纯粹按"哪个算得快/哪个索引支持好"来选。

### 3.3 embedding 结果缓存：同一段文本不重复花钱

**为什么**：embedding 按 token 计费（免费档也有速率配额），而"同一段文本的向量"是确定性的——重复调用是纯浪费。入库重跑、eval 反复跑、测试调试，都是重复调用的重灾区。加一个装饰器缓存层，对调用方完全透明。

设计先借用一个已经见过的思想：`mini-agent/internal/rag/kb.go:32` 的 `Embedder` 小接口——**接口定义在使用方而不是实现方**，`*embed.Client` 的 `Embed([]string) ([][]float32, error)` 签名恰好满足它，Go 隐式接口一行适配代码都不用写（第 5 章会正式用它，这里先借来包一层）。

教学代码（已验证可编译运行）：

```go
// Embedder 是"把一批文本翻译成向量"的能力抽象。
// 与 mini-agent/internal/rag/kb.go:32 的 rag.Embedder 同款设计。
type Embedder interface {
	Embed(texts []string) ([][]float32, error)
}

// CachedEmbedder 用装饰器模式给任意 Embedder 加结果缓存：
// 同一段文本只付一次 embedding 的钱（免费档则省速率配额）。
type CachedEmbedder struct {
	inner Embedder
	mu    sync.RWMutex
	cache map[string][]float32
}

// NewCachedEmbedder 包装任意 Embedder，返回值本身仍是 Embedder——
// 对调用方完全透明：NewKnowledgeBase(NewCachedEmbedder(client), ...) 直接用。
func NewCachedEmbedder(inner Embedder) *CachedEmbedder {
	return &CachedEmbedder{inner: inner, cache: make(map[string][]float32)}
}

// Embed 先查缓存，只对未命中的文本调底层 API，最后按原下标拼回——
// "按 index 归位"的纪律在客户端缓存层同样适用。
func (c *CachedEmbedder) Embed(texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))

	// 第一遍：查缓存，收集未命中的文本和它们的下标
	var missTexts []string
	var missIdx []int
	c.mu.RLock()
	for i, t := range texts {
		if v, ok := c.cache[t]; ok {
			result[i] = v
		} else {
			missTexts = append(missTexts, t)
			missIdx = append(missIdx, i)
		}
	}
	c.mu.RUnlock()

	if len(missTexts) == 0 {
		return result, nil // 全部命中，零 API 调用
	}

	// 只对 miss 调 API——入库重跑、测试重跑时省的就是这一批
	vecs, err := c.inner.Embed(missTexts)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(missTexts) {
		return nil, fmt.Errorf("embed cache: got %d vectors for %d misses", len(vecs), len(missTexts))
	}

	c.mu.Lock()
	for j, v := range vecs {
		c.cache[missTexts[j]] = v
		result[missIdx[j]] = v // 按记录的下标归位，不假设任何顺序
	}
	c.mu.Unlock()

	return result, nil
}
```

注意两个与主章呼应的设计点：

- **部分命中合并**：一批 10 段文本命中 7 段，只对 3 段 miss 调 API，然后按记录的下标拼回原顺序——概念 1.4 的"归位"思想在客户端缓存层原样复用。
- **`sync.RWMutex`**：装饰器天生面向并发场景（Agent 可能并发处理多个请求），读多写少用读写锁。教学版 `Store` 没有加锁是刻意的简化（单机 CLI 够用），生产内存库要照这里的模式补。

取舍：内存 map 进程退出即丢——可与 `Store.Save/Load` 一样落盘，或换 SQLite；缓存 key 直接用原文，换模型时记得清缓存（更稳的 key 是 `model + "\x00" + text`）；缓存不设上限会无限增长，生产加 LRU 或 TTL。

### 3.4 维度一致性纪律：换模型 = 全量重建

本章把这条纪律拆成了三层防线，串起来看是一次完整的纵深防御设计：

1. **第一道（embed 包，源头）**：`embed.go:149` 归位时校验每条向量维度 = `BgeM3Dimensions`——模型配错在第一次 API 调用就暴露；
2. **第二道（vectorstore，入口）**：`store.go:94` `Add` 拒绝与全库维度不符的记录——混不进库；
3. **第三道（持久化，恢复路径）**：`store.go:295` `Load` 逐条校验文件内维度一致——坏文件/手改文件进不了内存。

为什么要这么兴师动众？因为**换 embedding 模型 = 换向量空间**（2.1 节 `WithModel` 的警告）：不同模型各自独立训练，同一个词在两个空间里的坐标毫无对应关系——即使维度碰巧相同，跨空间算余弦也是无意义的。维度不一致只是"换模型"最容易被机器捕获的症状，所以拿它当哨兵。

换模型的正确流程（生产清单）：

1. 新模型建**新库**（或清空旧库），绝不在旧库里混写；
2. 全量文档重新 embedding、全量入库（这正是缓存和批量调用省钱的地方）；
3. 灰度验证：新旧库并行跑检索，对比 top-k 质量（第 6 章 eval 数字说话）；
4. 切流量，旧库保留一段时间做回滚兜底。

---

## 四、面试视角

> 以下每题给"标准回答 → 常见追问 → 加分点"。自测方法：不看回答口述，再对照差距。

**Q1：embedding 模型和 LLM 有什么区别？**

标准回答：两类模型。embedding 模型把文本**压缩**成固定维向量（语义坐标），输出不可读、不可生成文本；LLM **生成**文本。用途上，embedding 服务检索/聚类/去重这类"比较"任务，LLM 服务生成任务。产品上的直接体现：DeepSeek 只有 LLM API 没有 embedding API，所以项目里 embedding 走硅基流动 bge-m3，两个客户端分包（`internal/embed` vs `internal/llm`）。

追问链：
- "embedding 模型能当 LLM 用吗？" → 不能，输出是浮点向量不是 token 序列；反过来 LLM 的隐藏层理论上可以抽出来当向量用，但效果不如专门训练的 embedding 模型（对比学习目标不同）。
- "为什么是 1024 维，能换别的维度吗？" → 维度是模型训练时定的架构参数，bge-m3 固定 1024 维；选模型就是选维度，换模型必须全库重建（进阶 3.4）。

加分点：主动指出"写入路径和查询路径必须用同一个 embedding 模型，否则向量空间不对齐"——这说明真接过 RAG。

**Q2：文本检索为什么用余弦相似度而不是欧氏距离？**

标准回答：余弦只关心方向不关心长度。同一主题写 100 字和 500 字，向量方向一致但模长不同；欧氏距离对模长敏感，会把"同主题的长短文本"判远。文本长度不应影响语义判断，所以用余弦。

追问链：
- "余弦的值域？0 和 -1 分别什么意思？" → [-1,1]；0 = 正交 = 无关，-1 = 反向，实际检索几乎只见 [0,1] 区间。
- "工程上有什么等价优化？" → 归一化后内积 = 余弦（进阶 3.2），省三分之二运算；pgvector 里对应换 `vector_ip_ops`。能讲出这个的基本是真做过性能优化的。

加分点：补一句"bge-m3 上实测相关文本对通常 >0.5，<0.3 基本无关，但阈值要按自己的数据用 eval 定"——把背诵落到量化经验。

**Q3：什么时候不需要 HNSW？**（区分"背名词"和"真做过"的招牌题）

标准回答（必须带数字）：1024 维 float32 向量 4KB/条，10 万条约 400MB 内存、一次全扫约 1 亿次浮点运算，现代 CPU 毫秒级完成，而且暴力检索召回是精确的 100%。所以 **10 万条以内不需要 ANN 索引**；pgvector 的实践经验也是 1 万条以下顺序扫描更准。HNSW 的价值在百万级以上：近似 O(log N)，代价是召回略降 + 建索引开销 + 参数调优。

追问链：
- "HNSW 原理一句话？" → 分层可导航小世界图：上层稀疏下层密集，查询从顶层入口贪心下钻，每层指数级缩小范围，第 0 层局部收敛。
- "召回略降怎么办？" → 调大 `ef_search`（用速度换召回）；或对 ANN 粗排结果用暴力精排/rerank 兜底。
- "暴力 top-k 还能再优化吗？" → 全排序 O(N log N) 可换堆 O(N log k)；模长可预计算；归一化后退化为点积（进阶 3.2）。三连都答得出，说明优化思路是体系化的。

**Q4：向量库怎么选型？**

标准回答：按驱动因素而非热度：数据量（要不要 ANN）→ QPS（要不要专用引擎）→ 过滤需求（元数据 WHERE 复杂度）→ 运维成本（多一个组件多一份运维）。学习/个人场景纯内存手写；已有 Postgres 的中小系统 pgvector（向量与业务数据同库是最大卖点）；百万级以上、过滤复杂的用 Qdrant/Milvus 等专用库。

追问链：
- "pgvector 相比专用向量库的缺点？" → 超大规模与高 QPS 下性能不如专用引擎；索引类型少；但它赢在"少一个组件"和数据一致性（向量与业务同事务）。
- "为什么不直接用 ES/OpenSearch？" → 可以（它也支持向量检索），如果团队已有 ES 运维经验是合理选择——选型的答案从来是"看上下文"，能给出权衡框架比背结论重要。

**Q5：调用 embedding API 有哪些工程坑？**

标准回答（本项目全踩过）：① **响应顺序不可假设**，必须按 `index` 字段归位，否则文本与向量错位、检索全错且不报错（`embed.go:144`，乱序测试 `embed_test.go:48`）；② **批量调用**，入库几百 chunk 时逐条调用的 HTTP 往返不可接受，但单批大了要留超时余量；③ **维度校验**，模型/服务商配错会在第一次调用暴露，放过去则错误向量悄悄入库（`embed.go:149` + `BgeM3Dimensions` 常量）；④ 输入校验：空白文本在花钱前拦下（`embed.go:98-102`）。

追问链：
- "index 校验有什么讲究？" → 越界判断含等号边界（`index >= len` 非法）——我们真实修过一个 `>` 写成 `>=` 的 off-by-one，穿透后下一行数组越界 panic（`embed.go:145`）。数量校验也不够：两个元素报同一个 index 会静默覆盖，所以最后还有一轮缺失检测（`embed.go:155-159`）。
- "结果要缓存吗？" → 同文本向量是确定性的，重复调用纯浪费；加装饰器缓存（进阶 3.3），key 注意带模型名。

**Q6：为什么换 embedding 模型必须重建索引？维度相同也不行吗？**

标准回答：不同模型的向量空间是各自独立训练出来的，维度只是"空间的坐标轴数量"，每个维度代表什么语义特征完全由训练决定。同样的文本，模型 A 的向量是 [0.1, -0.3, ...]，模型 B 的是 [0.8, 0.2, ...]——**坐标系不同，跨空间算余弦是把两个不同坐标系里的点硬放一起比方向，数学上无意义**。维度相同只是恰好坐标轴数量一样，空间本身依然不对齐。

追问链：
- "所以换模型的流程是？" → 新库 → 全量重新 embedding → 灰度对比 → 切流量（进阶 3.4 的四步清单）。
- "写入和查询路径呢？" → 必须用同一个模型同一个版本——查询向量在旧空间、库向量在新空间，等于跨空间检索，同样全错。本项目把这条写进了 `WithModel` 的注释（`embed.go:59`）和 `KBSearch` 持有 `Embedder` 的设计理由里（`mini-agent/internal/rag/tool.go:23-26`）。

**Q7：检索的 top-k 怎么选？**

标准回答：在**召回**与**噪声**间权衡：k 太小可能漏掉关键 chunk（召回失败）；k 太大把低相关噪声塞进 prompt，稀释上下文、浪费 token，还可能误导模型答非所问。个人知识库常见起点是 3~5（本项目 `kb_search` 固定 topK=3，`mini-agent/internal/rag/tool.go:36`），然后用 eval 数据调：看期望文档进没进 top-k（召回率，第 6 章）。

追问链：
- "除了调 k 还有什么手段？" → 相似度阈值过滤（本项目 `minScore=0.3`）、rerank 精排、混合检索兜字面查询——这些全是第 5、6 章的调优位。
- "k 和 chunk 大小有关系吗？" → 有：chunk 切得大，单块信息全但噪声多，k 可以小；chunk 小则单块信息碎，k 要适当大。chunk 是 RAG 第一调参位，第 5 章展开。

加分点：能把 top-k、阈值、chunk、rerank 串成"召回率 vs 噪声率"的统一权衡框架，而不是孤立背参数——这正是第 6 章 eval 驱动调优的思维方式。

---

## 五、常见坑

1. **按数组下标对应响应，而不是按 index 归位**。embedding API 不保证 `data` 顺序与输入一致，错位后文本与向量张冠李戴——入库不报错、检索全错、极难排查。防御三连：按 index 归位 + 数量校验 + 缺失检测（`embed.go:138-159`），再加一个乱序假服务器测试钉死（`embed_test.go:48`）。
2. **中文长度按 byte 算的坑（预告）**。Go 的 `len(s)` 是字节数，UTF-8 下一个汉字占 3 字节；凡是按长度切文本的地方（第 5 章 chunking 首当其冲），按 byte 切会把一个汉字切成两半、产出乱码 chunk。正确姿势是 `[]rune(s)` 后按字符数切。本章先用 `strings.TrimSpace` 判空白（它正确处理 UTF-8），切块的坑留到第 5 章正面撞上。
3. **维度混杂：换模型忘了重建索引**。余弦要求等长向量，混维入库会让 Search panic 或算出无意义结果，且错误潜伏到检索时才暴露。三道防线：`embed.go:149` 源头校验、`store.go:94` 入库拒绝、`store.go:295` 加载校验。换模型请走进阶 3.4 的重建流程，别心存侥幸。
4. **向量不持久化，每次启动重新 embedding 白花钱**。embedding 按 token 计费，内存库进程退出即丢——这就是 `Save/Load` 的真实动机（`store.go:210`）。写入用"临时文件 + rename"原子写，加载时逐条校验不信任外部输入。
5. **余弦累加用 float32，1024 维下精度损失可感知**。float32 约 7 位有效数字，千项连加会"大数吃小数"。中间累加器一律 float64，输入输出保持 float32（`store.go:142-148`）。同理，面试里"为什么中间用 float64"不是背术语，是这个具体现象。
6. **空输入/空白文本直接送 API**。浪费一次调用还可能被服务端拒绝（不同服务商行为不一），在客户端校验拦下（`embed.go:94-102`），报错信息带下标（`texts[2] is empty`）让调用方一眼定位。

---

## 六、动手练习

本章对应阶段二练习 1、2（Go 侧）。

**练习 1：embedding client（`mini-agent/internal/embed/`，已完成）**

代码已就绪并通过测试（`go test ./internal/embed/`）。建议做两个真实 API 实验建立直觉（需 `SILICONFLOW_API_KEY`，免费档够用，以官方页面为准）：

1. **确定性实验**：写一个小 main，对同一段文本连续调两次 `Embed`，比较两次向量——观察"同文本 = 同向量"的确定性（这就是进阶 3.3 缓存可行的原因）；
2. **语义排序实验**：对 `["怎么退款", "退货流程说明", "今天天气不错"]` 批量 embedding，用 `vectorstore.CosineSimilarity` 两两算分——验证"怎么退款 × 退货流程"的得分显著高于"怎么退款 × 今天天气"。这一步亲眼看到 0.8+ vs 0.1 量级的差距，比任何讲解都管用。

**练习 2：内存向量库（`mini-agent/internal/vectorstore/`）**

目标函数都带 `TODO(练习2)` 标注：`CosineSimilarity`、`Search`、`Save/Load`。如果你的本地版本还是骨架，按 TODO 块里的【任务】【提示】【验收】完成；已经实现过的话，重读 TODO 提示里的边界清单（零向量、topK 边界、原子写入、Load 校验），逐条对照自己的实现。

验收：`cd mini-agent && go test ./internal/vectorstore/` 全绿——相同向量得分 1、正交 0、零向量报错、维度不符报错、topK 截断正确、Save→Load 往返保真。

参考答案（**完成后再看**）：

- 练习 1：`docs/solutions/stage-02/exercise-1-embedding-client.md`
- 练习 2：`docs/solutions/stage-02/exercise-2-vector-store.md`

---

## 本章小结

- 关键词匹配死于字面，embedding 把语义映射为固定维向量（bge-m3 1024 维）：语义近 = 向量距离近，"找相关"变成"算距离"。
- 余弦相似度只看方向不看长度，是文本检索的默认度量；归一化后内积 = 余弦，是白捡的性能优化。
- DeepSeek 没有 embedding API：硅基流动 bge-m3（免费档）/ Ollama 本地是本项目路径，OpenAI 兼容协议让客户端外壳整个复用。
- 批量调用的核心纪律：**按 index 归位**，响应顺序不可假设；维度、数量、缺失三层校验一层不能少。
- 暴力检索 10 万条内毫秒级且精确，HNSW 是百万级以上的近似方案——先量化再谈索引。
- 换 embedding 模型 = 换向量空间：一个库只混同一模型的向量，换模型走全量重建流程。
- 向量库选型看数据量、QPS、过滤需求、运维成本：内存手写 → pgvector → 专用库，是学习与生产的双重进阶路径。

下一章：[第 5 章：RAG 全链路](05-rag-pipeline.md)——把本章的两大底座（embedding + 向量检索）接上 chunking 与 ReAct 工具层，拼出完整的"开卷考试"系统：切块、入库、`kb_search` 工具、带引用的生成，以及 bad case 三板斧。
