# Embedding 与向量库使用指南

> 面向 RAG 项目（阶段二）的实战说明。技术栈：Go + TypeScript，预算敏感。

---

## 一、这两个东西是干什么的

### Embedding（向量化）解决什么问题

LLM 的上下文窗口有限，不可能把全部资料塞进 prompt。RAG 的思路是：**先从资料库里找出和问题最相关的几段，再让模型基于这几段回答**。

问题是：怎么判断"相关"？关键词匹配（如 BM25）只能匹配字面，用户问"怎么退款"，文档里写的是"退货流程说明"，字面就对不上。

Embedding 的做法：用一个模型把任意文本映射成一个**固定长度的浮点数向量**（如 1024 维），语义相近的文本在向量空间里距离也近。于是"找相关内容"变成了数学问题——**算距离**。

```
"怎么退款"        → [0.12, -0.38, 0.91, ...]  ─┐
                                                ├─ 余弦相似度 0.87（很相关）
"退货流程说明"     → [0.10, -0.35, 0.88, ...]  ─┘

"今天天气不错"     → [-0.55, 0.21, 0.03, ...]  ── 相似度 0.12（不相关）
```

### 向量库解决什么问题

算距离本身很简单（几行代码），但当文档有几万、几百万段时，每次查询都全量遍历算一遍就太慢了（O(N)）。

向量库 = **给向量建索引的专用存储**，用 ANN（近似最近邻，如 HNSW 算法）把查询降到毫秒级。它还顺带解决：向量的持久化、按元数据过滤（如"只搜 2024 年的文档"）、增删改。

一句话总结分工：

| 组件 | 职责 | 类比 |
|---|---|---|
| Embedding 模型 | 文本 → 向量（语义数字化） | 翻译器 |
| 向量库 | 存向量 + 快速找最近邻 | 带索引的数据库 |
| LLM（DeepSeek） | 基于检索到的内容生成回答 | 大脑 |

---

## 二、Embedding 模型的选择（重点：DeepSeek 没有 embedding API）

DeepSeek 官方**不提供 embedding 接口**，这是新手常踩的坑。可选方案：

### 方案 A：硅基流动 SiliconFlow（推荐起步）

- 有 OpenAI 兼容的 embedding API，且有**免费模型**（如 `BAAI/bge-m3`）
- bge-m3：1024 维，中英双语效果好，RAG 场景主流选择
- 注册送额度，个人学习基本零成本
- Base URL：`https://api.siliconflow.cn/v1`，接口格式和 OpenAI 完全一致

### 方案 B：本地跑（完全免费，数据不出本机）

- 用 [Ollama](https://ollama.com) 一行命令起本地服务：`ollama pull bge-m3`
- 同样提供 OpenAI 兼容接口（`http://localhost:11434/v1`）
- 缺点：首次调用加载模型慢，吃内存（bge-m3 约 2GB+）

### 方案 C：OpenAI `text-embedding-3-small`

- 效果好但收费且需要海外网络/支付方式，本项目不推荐

> 工程建议：把 embedding 调用抽象成一个接口，底层切换 provider 时业务代码不动。

---

## 三、Embedding API 怎么用

接口就是 OpenAI 的 `POST /embeddings`，输入文本数组，输出向量数组。

### curl 示例（先看懂协议）

```bash
curl https://api.siliconflow.cn/v1/embeddings \
  -H "Authorization: Bearer $SILICONFLOW_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "BAAI/bge-m3",
    "input": ["怎么退款", "退货流程说明"]
  }'
```

响应（已精简）：

```json
{
  "data": [
    { "index": 0, "embedding": [0.012, -0.038, "...共1024个浮点数..."] },
    { "index": 1, "embedding": [0.010, -0.035, "..."] }
  ],
  "usage": { "total_tokens": 12 }
}
```

### Go 调用示例

```go
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func Embed(apiKey, baseURL, model string, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(embedRequest{Model: model, Input: texts})
	req, _ := http.NewRequest("POST", baseURL+"/embeddings", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embed api %d: %s", resp.StatusCode, b)
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	// 注意：返回顺序要按 index 排序，不能假设和输入顺序一致
	vecs := make([][]float32, len(out.Data))
	for _, d := range out.Data {
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}
```

### TypeScript 调用示例

```ts
async function embed(texts: string[]): Promise<number[][]> {
  const resp = await fetch("https://api.siliconflow.cn/v1/embeddings", {
    method: "POST",
    headers: {
      Authorization: `Bearer ${process.env.SILICONFLOW_API_KEY}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ model: "BAAI/bge-m3", input: texts }),
  });
  if (!resp.ok) throw new Error(`embed api ${resp.status}: ${await resp.text()}`);
  const json = await resp.json();
  // 按 index 归位
  return json.data
    .sort((a: any, b: any) => a.index - b.index)
    .map((d: any) => d.embedding);
}
```

### 实用注意点

- **批量调用**：文档入库时一次传几十段文本，比逐条调用快得多，也省 HTTP 开销
- **向量要缓存/持久化**：同一段文本重复 embedding 是白花钱，入库后只对新文档做
- **维度一致性**：一个库里只能混用同一模型的向量，换 embedding 模型 = 全部重建索引
- 余弦相似度计算（Go，几行就够）：

```go
func CosineSim(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i] * b[i])
		na += float64(a[i] * a[i])
		nb += float64(b[i] * b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
```

---

## 四、向量库怎么用

### 选型建议（按你的场景）

| 方案 | 适合 | 成本 | 备注 |
|---|---|---|---|
| **pgvector** | 项目 2 首选 | 免费（Docker 起 Postgres） | 向量 + 业务数据放一个库，架构最简单 |
| Qdrant | 想体验专用向量库 | 免费（Docker / 云免费层） | 功能全，过滤能力强 |
| 内存切片 + CosineSim | 文档 < 1000 段 | 零依赖 | 学习原理时先用这个，别急着上库 |
| Milvus / Pinecone | 百万级以上 | 重 | 学习阶段用不到 |

**建议路径**：项目 2 先用纯内存实现（几十行代码），跑通全链路后再换成 pgvector——这样你面试时两种都能讲，还理解了向量库到底帮你做了什么。

### pgvector 快速上手

```bash
docker run -d --name pgvector \
  -e POSTGRES_PASSWORD=postgres -p 5432:5432 \
  pgvector/pgvector:pg16
```

建表（`vector(1024)` 对应 bge-m3 的维度）：

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

写入（Go，`lib/pq` 或 `pgx` 均可；向量用 pgvector 提供的类型序列化）：

```go
import "github.com/pgvector/pgvector-go"

_, err := db.Exec(
	`INSERT INTO documents (content, embedding, metadata) VALUES ($1, $2, $3)`,
	content, pgvector.NewVector(vec), metadataJSON,
)
```

查询（`<=>` 是余弦距离运算符，越小越相似）：

```sql
SELECT content, embedding <=> $1 AS distance
FROM documents
WHERE metadata->>'source' = 'refund-docs'   -- 元数据过滤
ORDER BY embedding <=> $1
LIMIT 5;
```

### 向量库帮你做的三件事（面试常问）

1. **ANN 索引**：HNSW 建图索引，把 O(N) 全量扫描变成近似 O(log N)，代价是召回率略降（用 `hnsw.ef_search` 等参数在速度和准确率间权衡）
2. **混合检索**：向量距离 + 标量过滤（`WHERE metadata ...`）一条 SQL 完成
3. **工程化**：持久化、并发、备份——这些自己手搓很费劲

---

## 五、串起来：RAG 的完整数据流

项目 2 里你会实现的链路：

```
【离线：入库】
文档 → 切分(chunking, 每段300~500字) → embedding API → 向量库
                                                        │
【在线：问答】                                          ▼
用户问题 → embedding API → 向量库查 top-5 → 拼进 prompt → DeepSeek → 带引用的回答
```

prompt 拼装示例：

```
system: 基于以下资料回答用户问题。资料中没有的内容就说不知道，不要编造。
        回答末尾用 [1][2] 标注引用的资料编号。

资料[1]: {第一段检索结果}
资料[2]: {第二段检索结果}
...

user: {用户问题}
```

### 最常见的三个 bad case（面试必考"RAG 怎么调优"）

1. **检索不到相关内容** → chunk 切太大/太小、embedding 模型不适配中文、该用关键词+向量混合检索
2. **检索到了但答非所问** → 拼 prompt 时 top-k 太多噪声大，加 rerank 或减少 k
3. **模型编造资料里没有的内容** → prompt 里明确"只基于资料回答"，并要求标注引用来源（方便人工核查）

---

## 六、费用估算（个人学习）

| 项目 | 方案 | 费用 |
|---|---|---|
| Embedding | 硅基流动 bge-m3 | 免费档够用 |
| 向量库 | pgvector Docker 本地 | 免费 |
| 生成 | DeepSeek deepseek-chat | 几元/月 |

---

## 下一步

进入项目 2（全栈知识库 Agent）时，我们会按这个顺序落地：

1. 先写内存版向量检索（纯 Go/TS，不用任何库）验证 embedding 效果
2. 接入硅基流动 embedding API，抽象 provider 接口
3. 换 pgvector 持久化，加元数据过滤
4. 拼 RAG prompt，接 DeepSeek 生成，写 eval 脚本评估质量
