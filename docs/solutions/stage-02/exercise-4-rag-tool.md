# 练习 4 参考答案：RAG 检索工具 kb_search

> 对应 TODO：`mini-agent/internal/rag/kb.go` 的 `TODO(练习4)`（Ingest）与 `mini-agent/internal/rag/tool.go` 的 `TODO(练习4)`（KBSearch.Execute）。
> **完成练习并自评后再看本文档。**
> 本文档代码已于 2026-08-06 实际粘贴进项目验证：临时应用练习 2/3 参考实现后，`cd mini-agent && go vet ./...` 通过，`go test ./internal/rag/ -v` 6 个测试全部通过；验证后 store.go / chunk.go 已用备份逐字节恢复为骨架版（diff 确认一致），rag 包测试文件已移除，最终 `go vet ./... && go build ./...` 通过。
> 2026-08-06 进阶回补：新增"三、进阶实现"一节（重复 /learn 去重，对应原"关键设计点"第 9 条①的已知局限），代码在 `/tmp` 项目副本中实测验证（rag 包 6 基础 + 3 进阶、vectorstore 包 3 个新增测试全绿，`go vet ./...` 通过），不进项目代码树——项目里请自己实现。

---

## 一、参考实现

### `internal/rag/kb.go`（Ingest 的实现；骨架的包注释、Embedder、KnowledgeBase、NewKnowledgeBase、Store 不变）

import 从骨架的 `errors` + `vectorstore` 改为（`errors` 不再需要）：

```go
import (
 "fmt"
 "strconv"
 "strings"

 "mini-agent/internal/vectorstore"
)
```

```go
// Ingest 把一篇文档入库：切块 → 批量 embedding → 整批写入向量库。
// source 是来源标识（如文件路径），写入每条记录的 Metadata 供引用溯源。
// 返回写入的块数。
//
// 错误处理约定：任一步失败都直接返回错误，且此时向量库未被触碰
// （embedding 失败发生在 Add 之前；Add 自身是 all-or-nothing），
// 所以 Ingest 整体也是 all-or-nothing——调用方重试不用先清理半成品。
func (kb *KnowledgeBase) Ingest(source, text string) (int, error) {
 if strings.TrimSpace(source) == "" {
  return 0, fmt.Errorf("rag: source is empty")
 }

 chunks := Chunk(text, kb.opts)
 if len(chunks) == 0 {
  // 空文档/全空白文档：Chunk 返回 nil。此时入库 0 块却返回成功，
  // 调用方会误以为"学到了东西"，必须报错。
  return 0, fmt.Errorf("rag: %s produced no chunks (empty or blank document)", source)
 }

 // 批量 embedding：一次请求处理全部块，比逐块调用省掉大量 HTTP 往返
 // （见 internal/embed 的批量接口设计注释）。
 vectors, err := kb.embedder.Embed(chunks)
 if err != nil {
  return 0, fmt.Errorf("rag: embed chunks of %s: %w", source, err)
 }
 // 防御：embedder 是接口，假实现/异常实现可能返回数量不符的向量，
 // 不校验就会把错位向量写进库（检索全错且难排查）。
 if len(vectors) != len(chunks) {
  return 0, fmt.Errorf("rag: embedder returned %d vectors for %d chunks", len(vectors), len(chunks))
 }

 docs := make([]vectorstore.Document, len(chunks))
 for i, c := range chunks {
  docs[i] = vectorstore.Document{
   // ID = 来源 + 块序号：一眼可读、天然唯一（同来源重复入库会产生重复块，
   // 去重/更新的完整实现见本文"三、进阶实现"一节）。
   ID:     fmt.Sprintf("%s#%d", source, i),
   Text:   c,
   Vector: vectors[i],
   // 引用溯源全靠 Metadata：检索工具格式化输出时读 source 标注出处。
   Metadata: map[string]string{
    "source": source,
    "chunk":  strconv.Itoa(i),
   },
  }
 }
 if err := kb.store.Add(docs...); err != nil {
  return 0, fmt.Errorf("rag: add chunks of %s: %w", source, err)
 }
 return len(docs), nil
}
```

### `internal/rag/tool.go`（Execute 的实现；骨架的 kbSearchArgs、KBSearch、NewKBSearch、Name/Description/ParametersSchema 不变）

import 从骨架的 `errors` + `vectorstore` 改为（`errors` 不再需要）：

```go
import (
 "encoding/json"
 "fmt"
 "strings"

 "mini-agent/internal/vectorstore"
)
```

新增一个包级常量，放在 kbSearchArgs 之前：

```go
// minScore 是命中被视为"相关"的最低余弦相似度（经验阈值）。
// 暴力 top-k 有个隐蔽问题：库里哪怕完全没有相关内容，也永远会返回
// "最接近的 k 条"——top-k 只保证"相对最近"，不保证"真的相关"。
// 低分过滤就是补这个洞：低于阈值的命中是噪声，直接丢弃。
// 0.3 是 bge-m3 中文语义检索的常见起点：太小放进噪声，太大误杀相关块；
// 生产上会按业务标注数据调这个值，或改用 rerank 模型替代硬阈值。
const minScore = 0.3
```

```go
// Execute 执行一次知识库检索：query embedding → top-k → 格式化参考文本。
//
// 返回的字符串会原样进入对话历史（tool 消息）给模型阅读，
// 所以它的读者是模型而不是人——格式设计的目标是"模型容易正确引用"：
// [编号] + 来源标注，模型据此在回答里写 [1] 引用。
func (t *KBSearch) Execute(args string) (string, error) {
 // args 是不可信输入（模型生成）：畸形 JSON、空 query 都要显式拒绝。
 // 这类错误会作为 tool 消息喂回给模型，模型看到后会自行修正参数重试。
 var in kbSearchArgs
 if err := json.Unmarshal([]byte(args), &in); err != nil {
  return "", fmt.Errorf("invalid tool arguments %q: %w", args, err)
 }
 in.Query = strings.TrimSpace(in.Query)
 if in.Query == "" {
  return "", fmt.Errorf("kb_search: query is empty")
 }

 // 空库短路：库里什么都没有时，检索必然无意义，
 // 不要浪费一次 embedding 调用（一次调用 = 一次 HTTP 往返 + 计费）。
 if t.store.Len() == 0 {
  return "知识库中没有相关内容（知识库为空，请先用 /learn 收录文档）。", nil
 }

 vecs, err := t.embedder.Embed([]string{in.Query})
 if err != nil {
  return "", fmt.Errorf("kb_search: embed query: %w", err)
 }
 hits, err := t.store.Search(vecs[0], t.topK)
 if err != nil {
  return "", fmt.Errorf("kb_search: search: %w", err)
 }

 // 低分过滤：top-k 永远会返回"最接近的 k 条"，哪怕全是无关内容（见 minScore 注释）。
 var relevant []vectorstore.Hit
 for _, h := range hits {
  if h.Score >= minScore {
   relevant = append(relevant, h)
  }
 }
 // 没有相关内容时必须返回明确文案——这是 RAG 防幻觉的第一道闸：
 // 如果这里只返回空字符串，模型会凭自身知识编造答案并伪装成来自知识库。
 if len(relevant) == 0 {
  return "知识库中没有相关内容。请如实告知用户知识库未覆盖该问题，不要凭自身知识编造来源。", nil
 }

 var sb strings.Builder
 sb.WriteString("以下是从知识库检索到的相关内容（请在回答中用 [编号] 标注引用来源）：\n")
 for i, h := range relevant {
  fmt.Fprintf(&sb, "\n[%d]（来源：%s，相似度 %.2f）\n%s\n",
   i+1, h.Doc.Metadata["source"], h.Score, h.Doc.Text)
 }
 return strings.TrimRight(sb.String(), "\n"), nil
}
```

### `cmd/agent/main.go` 接线（已在项目中由 AI 组装好，不属于练习，列出供理解）

关键片段——知识库的创建、加载与工具注册：

```go
var kb *rag.KnowledgeBase
if sfKey := os.Getenv("SILICONFLOW_API_KEY"); sfKey != "" {
 embedClient := embed.NewClient(sfKey)
 store := vectorstore.NewStore()
 // 尝试加载已有知识库索引：文件不存在（首次运行）不是错误，
 // 其他错误（文件损坏、维度混杂）提示后仍用空库继续，不让 agent 起不来。
 if err := store.Load(kbPath); err != nil && !os.IsNotExist(err) {
  fmt.Println("加载知识库失败（将从空库开始）:", err)
 }
 kb = rag.NewKnowledgeBase(embedClient, store, rag.DefaultChunkOptions())
 registry.Register(rag.NewKBSearch(embedClient, store))
 fmt.Println("知识库已启用：/learn <文件路径> 收录文档，模型可用 kb_search 检索。")
}
```

REPL 里的 `/learn` 分支与 `learnFile`：

```go
// /learn 是斜杠命令（客户端指令），不走 agent——
// 入库是确定性的本地动作，没必要让模型经手。
if strings.HasPrefix(input, "/learn") {
 learnFile(kb, strings.TrimSpace(strings.TrimPrefix(input, "/learn")))
 continue
}
```

```go
// learnFile 让知识库学习一个本地文档（md/txt 等纯文本）：
// 读文件 → kb.Ingest 切块入库 → 成功后立即 Save 落盘。
//
// 为什么入库后马上落盘：embedding 调用花了时间和 API 额度，
// 进程退出就丢等于白花钱——这是持久化的真实动机（见 vectorstore 的练习注释）。
func learnFile(kb *rag.KnowledgeBase, path string) {
 if kb == nil {
  fmt.Println("知识库未启用：请设置 SILICONFLOW_API_KEY 后重启。")
  return
 }
 if path == "" {
  fmt.Println("用法：/learn <文件路径>")
  return
 }
 data, err := os.ReadFile(path)
 if err != nil {
  fmt.Println("读取文件失败:", err)
  return
 }
 n, err := kb.Ingest(path, string(data))
 if err != nil {
  fmt.Println("学习失败:", err)
  return
 }
 if err := kb.Store().Save(kbPath); err != nil {
  fmt.Println("保存知识库失败（块已入库但未落盘）:", err)
  return
 }
 fmt.Printf("已学习 %s：%d 个块入库，知识库累计 %d 个块。\n", path, n, kb.Store().Len())
}
```

### `internal/rag/kb_test.go`（新建，假 Embedder 确定性命中，无网络依赖）

```go
package rag

import (
 "fmt"
 "strings"
 "testing"

 "mini-agent/internal/vectorstore"
)

// fakeEmbedder 按文本内容返回预定义向量：测试能精确控制"哪段文本对应哪个向量"，
// 从而让检索结果完全确定——无需网络、不烧 API 额度。
// 这正是骨架里 Embedder 接口存在的意义（接口定义在使用方，测试随便替换实现）。
type fakeEmbedder struct {
 vecs map[string][]float32
}

func (f fakeEmbedder) Embed(texts []string) ([][]float32, error) {
 out := make([][]float32, len(texts))
 for i, text := range texts {
  v, ok := f.vecs[text]
  if !ok {
   return nil, fmt.Errorf("fakeEmbedder: no vector for %q", text)
  }
  out[i] = v
 }
 return out, nil
}

// TestIngest_ChunkCountAndMetadata 验证入库路径的核心语义：
// 块数正确、每条记录的 ID/Text/Metadata（source + chunk 序号）都正确。
func TestIngest_ChunkCountAndMetadata(t *testing.T) {
 // 两个 60 rune 的段落，MaxChars=100 → 装不进同一块（60+2+60=122），必为 2 块。
 p1 := strings.Repeat("甲", 60)
 p2 := strings.Repeat("乙", 60)
 text := p1 + "\n\n" + p2

 emb := fakeEmbedder{vecs: map[string][]float32{
  p1: {1, 0},
  p2: {0, 1},
 }}
 store := vectorstore.NewStore()
 kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})

 n, err := kb.Ingest("notes.md", text)
 if err != nil {
  t.Fatalf("Ingest: %v", err)
 }
 if n != 2 {
  t.Errorf("Ingest 返回 %d 块, want 2", n)
 }
 if store.Len() != 2 {
  t.Fatalf("store.Len() = %d, want 2", store.Len())
 }

 // 用 p1 的向量检索，top1 必须是 p1 对应的文档。
 hits, err := store.Search([]float32{1, 0}, 2)
 if err != nil {
  t.Fatalf("Search: %v", err)
 }
 if len(hits) != 2 {
  t.Fatalf("got %d hits, want 2", len(hits))
 }

 first := hits[0].Doc
 if first.ID != "notes.md#0" {
  t.Errorf("ID = %q, want %q", first.ID, "notes.md#0")
 }
 if first.Text != p1 {
  t.Errorf("Text 未保真（首 20 rune: %.20q）", first.Text)
 }
 if first.Metadata["source"] != "notes.md" {
  t.Errorf("Metadata[source] = %q, want %q", first.Metadata["source"], "notes.md")
 }
 if first.Metadata["chunk"] != "0" {
  t.Errorf("Metadata[chunk] = %q, want %q", first.Metadata["chunk"], "0")
 }
 if hits[1].Doc.Metadata["chunk"] != "1" {
  t.Errorf("hits[1].Metadata[chunk] = %q, want %q", hits[1].Doc.Metadata["chunk"], "1")
 }
}

// TestIngest_RejectsBadInput 验证防御：空 source、空文档、全空白文档都报错，
// 且失败时向量库不被污染（all-or-nothing）。
func TestIngest_RejectsBadInput(t *testing.T) {
 emb := fakeEmbedder{vecs: map[string][]float32{}}
 kb := NewKnowledgeBase(emb, vectorstore.NewStore(), ChunkOptions{})

 if _, err := kb.Ingest("  ", "内容"); err == nil {
  t.Error("empty source: want error, got nil")
 }
 if _, err := kb.Ingest("a.md", ""); err == nil {
  t.Error("empty text: want error, got nil")
 }
 if _, err := kb.Ingest("a.md", "  \n\n\t "); err == nil {
  t.Error("blank text: want error, got nil")
 }
 if kb.Store().Len() != 0 {
  t.Errorf("失败的 Ingest 污染了向量库：Len = %d, want 0", kb.Store().Len())
 }
}

// TestKBSearch_FormatsNumberedHitsWithSource 验证检索输出格式：
// 含 [1] 编号、来源标注、命中块文本；低分（正交，得分 0）的块被过滤。
func TestKBSearch_FormatsNumberedHitsWithSource(t *testing.T) {
 p1 := strings.Repeat("苹果", 30) // 60 rune
 p2 := strings.Repeat("香蕉", 30)
 text := p1 + "\n\n" + p2

 emb := fakeEmbedder{vecs: map[string][]float32{
  p1:       {1, 0},
  p2:       {0, 1}, // 与查询正交 → 得分 0，低于阈值应被过滤
  "苹果是什么？": {1, 0},
 }}
 store := vectorstore.NewStore()
 kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})
 if _, err := kb.Ingest("fruits.md", text); err != nil {
  t.Fatalf("Ingest: %v", err)
 }

 tool := NewKBSearch(emb, store)
 out, err := tool.Execute(`{"query":"苹果是什么？"}`)
 if err != nil {
  t.Fatalf("Execute: %v", err)
 }
 for _, want := range []string{"[1]", "fruits.md", p1} {
  if !strings.Contains(out, want) {
   t.Errorf("输出缺少 %q：\n%s", want, out)
  }
 }
 // p2 得分 0 < minScore，必须被过滤：输出里既没有 [2] 也没有 p2 的文本。
 if strings.Contains(out, "[2]") {
  t.Errorf("低分块未被过滤（出现 [2]）：\n%s", out)
 }
 if strings.Contains(out, p2) {
  t.Errorf("低分块文本泄漏进输出：\n%s", out)
 }
}

// TestKBSearch_EmptyStore 验证空库文案：不报错、明确告知"没有相关内容"，
// 且短路在 embedding 之前（fakeEmbedder 里没有任何向量，被调用必然报错——
// 没报错就证明 embedding 调用被正确跳过了）。
func TestKBSearch_EmptyStore(t *testing.T) {
 tool := NewKBSearch(fakeEmbedder{vecs: map[string][]float32{}}, vectorstore.NewStore())
 out, err := tool.Execute(`{"query":"随便问"}`)
 if err != nil {
  t.Fatalf("Execute on empty store: %v", err)
 }
 if !strings.Contains(out, "知识库中没有相关内容") {
  t.Errorf("空库文案不符合预期：%q", out)
 }
}

// TestKBSearch_LowScoreFiltered 验证低分过滤文案：库非空但唯一命中得分 0，
// 必须返回"没有相关内容"而不是把无关块塞给模型。
func TestKBSearch_LowScoreFiltered(t *testing.T) {
 doc := "完全无关的文档内容"
 emb := fakeEmbedder{vecs: map[string][]float32{
  doc:  {0, 1},
  "查询": {1, 0}, // 与 doc 正交 → 得分 0
 }}
 store := vectorstore.NewStore()
 kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})
 if _, err := kb.Ingest("x.md", doc); err != nil {
  t.Fatalf("Ingest: %v", err)
 }

 tool := NewKBSearch(emb, store)
 out, err := tool.Execute(`{"query":"查询"}`)
 if err != nil {
  t.Fatalf("Execute: %v", err)
 }
 if !strings.Contains(out, "知识库中没有相关内容") {
  t.Errorf("低分过滤文案不符合预期：%q", out)
 }
 if strings.Contains(out, doc) {
  t.Errorf("无关文档泄漏进输出：%q", out)
 }
}

// TestKBSearch_InvalidArgs 验证不可信输入的防御：畸形 JSON、空 query 都报错
// （错误会作为 tool 消息喂回模型，让它自我纠正）。
func TestKBSearch_InvalidArgs(t *testing.T) {
 tool := NewKBSearch(fakeEmbedder{vecs: map[string][]float32{}}, vectorstore.NewStore())
 if _, err := tool.Execute(`{not json`); err == nil {
  t.Error("malformed JSON: want error, got nil")
 }
 if _, err := tool.Execute(`{"query":"  "}`); err == nil {
  t.Error("blank query: want error, got nil")
 }
}
```

## 二、关键设计点

1. **Embedder 接口定义在使用方，这是本练习的架构灵魂**：`KnowledgeBase` 和 `KBSearch` 都不认识 `*embed.Client`，只认识 `Embed([]string)` 这个行为。Go 接口隐式满足，所以真客户端直接传、假实现随便换——测试因此能脱离网络精确控制向量。**易错处**：把 `*embed.Client` 直接当作字段类型，测试时就被迫起 httptest 假服务器或烧真实 API，确定性全无。面试点："Go 的接口为什么应该定义在消费方而不是实现方"——答案就是本练习。

2. **入库顺序 = 失败安全顺序**：Chunk（纯计算，无副作用）→ Embed（外部调用，可能失败）→ Add（改状态，放最后）。任何前置失败时向量库都未被触碰，配合 Add 的 all-or-nothing，Ingest 整体要么全进要么不进。**易错处**：边切块边入库（逐块 Embed + Add），中途失败就留下半个文档在库里，重试时产生重复块。

3. **批量 Embed 而不是逐块 Embed**：几百个块逐块调用就是几百次 HTTP 往返，批量接口一次搞定（`internal/embed` 的练习 1 专门为此把 input 设计成数组）。**易错处**：忘了校验 `len(vectors) == len(chunks)`——embedder 是接口，数量不符时向量与块错位入库，检索结果全错还没人报错。

4. **Metadata 的 source 不是装饰，是防幻觉链条的一环**：检索工具格式化输出时读 `Metadata["source"]` 标注来源，模型才能在回答里写"[1] 来自 notes.md"。漏写 source，引用溯源链就断了（见 `vectorstore.Document` 的 Metadata 注释）。

5. **top-k 的隐蔽缺陷必须靠低分过滤补**：暴力检索永远返回"最接近的 k 条"，哪怕库里的内容全与问题无关——top-k 保证的是"相对最近"，不是"真的相关"。所以必须设 `minScore` 阈值把噪声滤掉。**易错处**：把 topK 条命中不加过滤全塞进 prompt，无关内容会误导模型（还不如不检索）。

6. **空结果的明确文案是 RAG 防幻觉第一道闸**：空库 / 过滤后无命中时返回"知识库中没有相关内容，不要编造"，而不是返回空字符串。模型拿到空字符串会理解为"工具没说话"，然后凭自身知识编一个看似来自知识库的答案。这个文案与 `Description()` 里的"如实告知"指令互相呼应——工具说明和工具输出两层都在堵幻觉。

7. **空库短路在 embedding 之前**：`store.Len() == 0` 时直接返回文案，不调 embedder。一次 embedding = 一次 HTTP 往返 + 计费，能在本地确定的答案绝不花外部调用的钱。测试 `TestKBSearch_EmptyStore` 用"假 embedder 被调用必然报错"来反向证明短路生效。

8. **Execute 的返回字符串是"给模型读的 prompt"，不是给人看的 UI**：所以开头写清"请在回答中用 [编号] 标注引用来源"，每条命中带 [编号]（来源 + 相似度）。模型引用编号的准确率远高于引用文件名。**易错处**：只拼接块文本不加编号和来源，模型无法标注出处，用户无法验证答案。

9. **基础版的已知局限（面试可主动展开）**：① 基础版同一来源重复 `/learn` 会产生重复块——去重/更新的**完整实现**（同 source 全量替换 + 未变化短路）见本文"三、进阶实现"一节；② `minScore` 是硬编码经验值，生产上按标注数据调或用 rerank 模型；③ 并发场景下 REPL 单线程没问题，做成服务后 Store 需要加锁；④ 大批量入库超出 embedding API 单批上限时需要分批（见练习 1 答案的"批量上限"讨论）。

## 三、进阶实现：重复 /learn 去重（同 source 全量替换）

> 本节是"关键设计点"第 9 条①的完整落地，2026-08-06 回补。代码已在 `/tmp` 项目副本中
> 实测验证（基础版 6 个测试不受影响 + 新增 3 个去重测试 + vectorstore 3 个删除/查找测试全绿），
> 不进项目代码树——项目里请自己实现。

### 策略取舍：为什么选"同 source 全量替换"而不是"按块哈希跳过"

候选方案有两个：

- **按块内容哈希跳过**：入库前算每个块的哈希，与库中已有块比对，重复的块跳过、新块追加。
  问题是它只解决"重复"，不解决"更新"——文档删掉一段后重新 /learn，旧块永远留在库里
  （没有机制知道"这个块属于该 source 但新版已不存在"），检索会命中已删除的内容。
- **同 source 全量替换（本实现选用）**：入库前按 `Metadata["source"]` 找到该来源的全部旧块，
  先删再写。重复 /learn 同文档 = 删旧写新（内容相同净效果为零）；文档修改后 /learn =
  旧版本块（包括变少后多出来的尾部块）全部清掉。库里永远只有每个 source 的最新版本，
  语义简单、可预期，与个人知识库"一个文件就是一份知识"的心智模型对齐。

全量替换的代价是"文档改一个字也要整篇重新 embedding"。用一个**未变化短路**把这笔开销
省掉：切块后先与库中该 source 的现有块逐位比对文本，完全一致就直接返回——连 embedding
调用都不发生（一次调用 = 一次 HTTP 往返 + 计费）。这样"重复 /learn 没变过的文档"零成本，
"修改过的文档"付一次全量 embedding，是两者优点的组合。

另一个刻意的边界：**去重只认同 source**。两个不同文件内容相同，是两条独立知识
（来源不同、引用标注不同），互不去重。

### vectorstore 新增：FindByMetadata + Delete（`internal/vectorstore/delete.go`，新建文件）

去重需要"按 source 找到旧块"和"按 ID 删除"两个底层能力，基础版 vectorstore 只有增和查，
这里补上（真实向量库 Milvus/Qdrant 同样提供 delete-by-filter / delete-by-id）：

```go
package vectorstore

// 本文件是 vectorstore 的进阶补充：按元数据查找与按 ID 删除。
// 动机：基础版只有"增"和"查"，没有"删"。但两个真实的进阶需求都依赖删除——
//   - RAG 重复 /learn 同一文档要去重/更新（练习 4 进阶：同 source 全量替换）；
//   - 长期记忆需要"遗忘"（练习 5 进阶：memory.Forget 按相似度找到后删除）。
//
// 真实向量库（Milvus/Qdrant）同样提供 delete-by-filter / delete-by-id，
// 这里手写最小版本，理解它们替你做了什么。

// FindByMetadata 返回 Metadata[key] == value 的全部文档，按入库顺序。
//
// 用元数据过滤而不是另建索引：库里只有几十上百条记录，全表扫一遍是微秒级，
// 维护倒排索引纯属过度设计——与"暴力检索够用"是同一个判断。
//
// 注意对 Metadata 为 nil 的文档安全：用 ok 判断键是否存在，
// 避免"nil map 查不存在的键返回零值"把没有该键的文档误判为匹配空串。
func (s *Store) FindByMetadata(key, value string) []Document {
 var out []Document
 for _, d := range s.docs {
  if v, ok := d.Metadata[key]; ok && v == value {
   out = append(out, d)
  }
 }
 return out
}

// Delete 按 ID 删除一条文档，返回是否真的删到了（ID 不存在返回 false）。
//
// 两个设计点：
//  1. 用 append(s.docs[:i], s.docs[i+1:]...) 原地删除，保持剩余文档的入库顺序
//     （Search 的稳定排序依赖入库顺序做同分决胜，顺序不能乱）。
//  2. 删空时把 dim 归零：空库应当表现得像 NewStore()——否则"删光旧模型的
//     向量后换模型重新入库"会被残留的 dim 校验拒绝，这个坑很隐蔽。
func (s *Store) Delete(id string) bool {
 for i, d := range s.docs {
  if d.ID == id {
   s.docs = append(s.docs[:i], s.docs[i+1:]...)
   if len(s.docs) == 0 {
    s.dim = 0
   }
   return true
  }
 }
 return false
}
```

### Ingest 升级为去重版（`internal/rag/kb.go`，替换基础版 Ingest 并新增 sameChunks）

行为变化只有一处：同 source 重复/修改入库不再产生重复块。首次入库、参数校验、
all-or-nothing 语义与基础版完全一致（基础版 6 个测试原样通过）。

```go
// Ingest 把一篇文档入库：切块 → 去重判断 → 批量 embedding → 同 source 替换式写入。
// source 是来源标识（如文件路径），写入每条记录的 Metadata 供引用溯源。
// 返回新写入的块数；同来源内容没有变化时返回 0（见去重注释）。
//
// 错误处理约定：任一步失败都直接返回错误，且此时向量库未被触碰
// （embedding 失败发生在 Add 之前；Add 自身是 all-or-nothing），
// 所以 Ingest 整体也是 all-or-nothing——调用方重试不用先清理半成品。
func (kb *KnowledgeBase) Ingest(source, text string) (int, error) {
 if strings.TrimSpace(source) == "" {
  return 0, fmt.Errorf("rag: source is empty")
 }

 chunks := Chunk(text, kb.opts)
 if len(chunks) == 0 {
  // 空文档/全空白文档：Chunk 返回 nil。此时入库 0 块却返回成功，
  // 调用方会误以为"学到了东西"，必须报错。
  return 0, fmt.Errorf("rag: %s produced no chunks (empty or blank document)", source)
 }

 // 去重短路（进阶）：同 source 且块文本序列完全一致 → 文档没变化，
 // 直接跳过，连 embedding 调用都省掉（一次调用 = 一次 HTTP 往返 + 计费）。
 // 去重只认"同 source + 内容全同"：不同 source 的相同文本是两条独立知识，
 // 不能互相去重（见参考答案进阶一节的取舍讨论）。
 existing := kb.store.FindByMetadata("source", source)
 if sameChunks(existing, chunks) {
  return 0, nil
 }

 // 批量 embedding：一次请求处理全部块，比逐块调用省掉大量 HTTP 往返
 // （见 internal/embed 的批量接口设计注释）。
 vectors, err := kb.embedder.Embed(chunks)
 if err != nil {
  return 0, fmt.Errorf("rag: embed chunks of %s: %w", source, err)
 }
 // 防御：embedder 是接口，假实现/异常实现可能返回数量不符的向量，
 // 不校验就会把错位向量写进库（检索全错且难排查）。
 if len(vectors) != len(chunks) {
  return 0, fmt.Errorf("rag: embedder returned %d vectors for %d chunks", len(vectors), len(chunks))
 }

 docs := make([]vectorstore.Document, len(chunks))
 for i, c := range chunks {
  docs[i] = vectorstore.Document{
   // ID = 来源 + 块序号：一眼可读；配合"同 source 全量替换"，
   // 重复入库同一文档不会产生重复块（见下方 Delete 循环）。
   ID:     fmt.Sprintf("%s#%d", source, i),
   Text:   c,
   Vector: vectors[i],
   // 引用溯源全靠 Metadata：检索工具格式化输出时读 source 标注出处。
   Metadata: map[string]string{
    "source": source,
    "chunk":  strconv.Itoa(i),
   },
  }
 }

 // 同 source 全量替换（进阶）：先删掉该来源的旧块再入库新块。
 // 文档修改后重新 /learn 时，旧版本的块（包括变少后多出来的尾部块）
 // 全部被清掉，库里永远只有该文档的最新版本。
 // 顺序上删除放在 embedding 成功之后：embedding 失败时旧数据完好。
 for _, old := range existing {
  kb.store.Delete(old.ID)
 }
 if err := kb.store.Add(docs...); err != nil {
  return 0, fmt.Errorf("rag: add chunks of %s: %w", source, err)
 }
 return len(docs), nil
}

// sameChunks 报告库中已有的块序列与新切出的块序列是否完全一致。
// existing 按入库顺序返回（FindByMetadata 保证），与 chunks 逐位比较 Text。
func sameChunks(existing []vectorstore.Document, chunks []string) bool {
 if len(existing) != len(chunks) {
  return false
 }
 for i, d := range existing {
  if d.Text != chunks[i] {
   return false
  }
 }
 return true
}
```

import 与基础版相同（`fmt`、`strconv`、`strings`、`mini-agent/internal/vectorstore`），无新增依赖。

### 进阶测试（`internal/vectorstore/delete_test.go` 与 `internal/rag/kb_dedup_test.go`，各新建）

```go
package vectorstore

import "testing"

// newMetaStore 建一个带元数据的 2 维小库：a、b 属于 x.md，c 属于 y.md，d 无元数据。
func newMetaStore(t *testing.T) *Store {
 t.Helper()
 s := NewStore()
 err := s.Add(
  Document{ID: "a", Text: "x 第一块", Vector: []float32{1, 0},
   Metadata: map[string]string{"source": "x.md", "chunk": "0"}},
  Document{ID: "b", Text: "x 第二块", Vector: []float32{0, 1},
   Metadata: map[string]string{"source": "x.md", "chunk": "1"}},
  Document{ID: "c", Text: "y 第一块", Vector: []float32{1, 1},
   Metadata: map[string]string{"source": "y.md", "chunk": "0"}},
  Document{ID: "d", Text: "无元数据", Vector: []float32{2, 1}},
 )
 if err != nil {
  t.Fatalf("Add: %v", err)
 }
 return s
}

// TestFindByMetadata 验证按元数据过滤：命中按入库顺序返回；
// 不存在的值返回空；无该键的文档（nil Metadata）不会误判匹配。
func TestFindByMetadata(t *testing.T) {
 s := newMetaStore(t)

 got := s.FindByMetadata("source", "x.md")
 if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
  t.Errorf(`FindByMetadata("source","x.md") = %v, want [a b]（按入库序）`, ids(got))
 }
 if got := s.FindByMetadata("source", "z.md"); len(got) != 0 {
  t.Errorf("不存在的 source 返回 %d 条, want 0", len(got))
 }
 // d 的 Metadata 是 nil：nil map 读不存在的键返回零值 ""，
 // 若实现不用 ok 判断键是否存在，这里会把 d 误判为匹配。
 if got := s.FindByMetadata("source", ""); len(got) != 0 {
  t.Errorf(`FindByMetadata("source","") 匹配到 %v（nil Metadata 被误判）`, ids(got))
 }
}

func ids(docs []Document) []string {
 out := make([]string, len(docs))
 for i, d := range docs {
  out[i] = d.ID
 }
 return out
}

// TestDelete 验证删除语义：存在的 ID 删除成功且从检索结果消失、
// 剩余文档保持入库顺序；不存在的 ID 返回 false 且库不变。
func TestDelete(t *testing.T) {
 s := newMetaStore(t)

 if !s.Delete("b") {
  t.Fatal(`Delete("b") = false, want true`)
 }
 if s.Len() != 3 {
  t.Fatalf("Len = %d, want 3", s.Len())
 }
 // 用与 b 完全同向的向量检索：b 已删除，top1 不能再是它。
 hits, err := s.Search([]float32{0, 1}, 4)
 if err != nil {
  t.Fatalf("Search: %v", err)
 }
 for _, h := range hits {
  if h.Doc.ID == "b" {
   t.Errorf("已删除的 b 仍出现在检索结果中")
  }
 }
 // 剩余文档保持入库顺序（得分并列时稳定排序依赖它）。
 if got := s.FindByMetadata("source", "x.md"); len(got) != 1 || got[0].ID != "a" {
  t.Errorf("x.md 剩余文档 = %v, want [a]", ids(got))
 }

 if s.Delete("not-exist") {
  t.Error(`Delete("not-exist") = true, want false`)
 }
 if s.Len() != 3 {
  t.Errorf("删除不存在的 ID 后 Len = %d, want 3", s.Len())
 }
}

// TestDelete_EmptyStoreResetsDim 验证删空后 dim 归零：
// 空库应表现得像 NewStore()，允许不同维度的新记录入库。
func TestDelete_EmptyStoreResetsDim(t *testing.T) {
 s := NewStore()
 if err := s.Add(Document{ID: "a", Vector: []float32{1, 0}}); err != nil {
  t.Fatal(err)
 }
 if !s.Delete("a") {
  t.Fatal(`Delete("a") = false`)
 }
 // 2 维的库删空后，3 维的新记录必须能入库（换 embedding 模型的场景）。
 if err := s.Add(Document{ID: "b", Vector: []float32{1, 2, 3}}); err != nil {
  t.Errorf("删空后 Add 3 维文档报错：%v（dim 未归零）", err)
 }
}
```

```go
package rag

import (
 "fmt"
 "strings"
 "testing"

 "mini-agent/internal/vectorstore"
)

// countingEmbedder 在假 Embedder 之上记录调用次数：
// 去重的核心收益是"内容没变就一次 embedding 都不调"，必须能断言这一点。
type countingEmbedder struct {
 vecs  map[string][]float32
 calls int // 累计 embed 的文本条数
}

func (c *countingEmbedder) Embed(texts []string) ([][]float32, error) {
 c.calls += len(texts)
 out := make([][]float32, len(texts))
 for i, text := range texts {
  v, ok := c.vecs[text]
  if !ok {
   return nil, fmt.Errorf("countingEmbedder: no vector for %q", text)
  }
  out[i] = v
 }
 return out, nil
}

// TestIngest_RepeatSameSourceIsNoop 验证重复 /learn 同一文档不产生重复块：
// 第二次 Ingest 返回 0、库大小不变，且完全没有再调 embedding（去重短路
// 发生在 embed 之前——这是"省 API 额度"的断言，不只是行为断言）。
func TestIngest_RepeatSameSourceIsNoop(t *testing.T) {
 p1 := strings.Repeat("甲", 60)
 p2 := strings.Repeat("乙", 60)
 text := p1 + "\n\n" + p2

 emb := &countingEmbedder{vecs: map[string][]float32{p1: {1, 0}, p2: {0, 1}}}
 store := vectorstore.NewStore()
 kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})

 n, err := kb.Ingest("notes.md", text)
 if err != nil || n != 2 {
  t.Fatalf("首次 Ingest: n=%d, err=%v; want n=2", n, err)
 }
 callsAfterFirst := emb.calls

 n, err = kb.Ingest("notes.md", text)
 if err != nil {
  t.Fatalf("重复 Ingest: %v", err)
 }
 if n != 0 {
  t.Errorf("重复 Ingest 返回 %d, want 0（内容未变应跳过）", n)
 }
 if store.Len() != 2 {
  t.Errorf("重复 Ingest 后 Len = %d, want 2（产生了重复块）", store.Len())
 }
 if emb.calls != callsAfterFirst {
  t.Errorf("重复 Ingest 多调了 %d 次 embedding，去重短路未生效（应一次 embedding 都不调）",
   emb.calls-callsAfterFirst)
 }
}

// TestIngest_ModifiedSourceReplacesOld 验证文档修改后重新入库是"全量替换"：
// 旧块全部消失（包括变少后多出来的尾部块），库里只有最新版本。
func TestIngest_ModifiedSourceReplacesOld(t *testing.T) {
 p1 := strings.Repeat("甲", 60)
 p2 := strings.Repeat("乙", 60)
 oldText := p1 + "\n\n" + p2 // v1：2 块
 newText := strings.Repeat("丙", 60) + "。"

 emb := &countingEmbedder{vecs: map[string][]float32{
  p1:      {1, 0},
  p2:      {0, 1},
  newText: {1, 1},
 }}
 store := vectorstore.NewStore()
 kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})

 if _, err := kb.Ingest("notes.md", oldText); err != nil {
  t.Fatalf("Ingest v1: %v", err)
 }
 n, err := kb.Ingest("notes.md", newText)
 if err != nil {
  t.Fatalf("Ingest v2: %v", err)
 }
 if n != 1 {
  t.Errorf("v2 Ingest 返回 %d, want 1", n)
 }
 // 旧版 2 块必须被清掉，库里只剩新版 1 块。
 if store.Len() != 1 {
  t.Fatalf("替换后 Len = %d, want 1（旧块未清干净）", store.Len())
 }
 hits, err := store.Search([]float32{1, 1}, 3)
 if err != nil {
  t.Fatalf("Search: %v", err)
 }
 if len(hits) != 1 || hits[0].Doc.Text != newText {
  t.Errorf("检索结果 = %v，库里应只剩新版内容", hits)
 }
 if old := store.FindByMetadata("source", "notes.md"); len(old) != 1 || old[0].Text != newText {
  t.Errorf("FindByMetadata 命中 %d 条，want 1 条新版", len(old))
 }
}

// TestIngest_DifferentSourcesIndependent 验证去重的作用域是"同 source"：
// 不同来源的相同文本是两条独立知识，互不去重、互不影响。
func TestIngest_DifferentSourcesIndependent(t *testing.T) {
 text := "同一段内容，两个来源各自收录。"
 emb := &countingEmbedder{vecs: map[string][]float32{text: {1, 0}}}
 store := vectorstore.NewStore()
 kb := NewKnowledgeBase(emb, store, ChunkOptions{MaxChars: 100, OverlapChars: 10})

 if _, err := kb.Ingest("a.md", text); err != nil {
  t.Fatal(err)
 }
 // 相同文本、不同 source：必须入库（去重不能跨 source 误伤）。
 if _, err := kb.Ingest("b.md", text); err != nil {
  t.Fatal(err)
 }
 if store.Len() != 2 {
  t.Fatalf("Len = %d, want 2（不同 source 的相同文本被错误去重）", store.Len())
 }
 // 重复 a.md 是 no-op，且不影响 b.md。
 if n, _ := kb.Ingest("a.md", text); n != 0 {
  t.Errorf("重复 a.md 返回 %d, want 0", n)
 }
 if got := store.FindByMetadata("source", "b.md"); len(got) != 1 || got[0].Text != text {
  t.Errorf("a.md 的重复入库影响了 b.md：%v", got)
 }
 if store.Len() != 2 {
  t.Errorf("最终 Len = %d, want 2", store.Len())
 }
}
```

### 易错处

1. **删除必须放在 embedding 成功之后**：先删旧块再 embed，一旦 embed 失败（网络抖动、
   额度耗尽），旧数据已经没了——"更新文档"变成"丢失文档"。本实现的顺序是
   切块 → 去重短路 → embed → 删旧 → 写新，任何外部失败发生时库都保持原样。
2. **去重短路比较的是"切块后的文本序列"，不是原文**：同一个文件只要切块参数不变，
   块序列就确定，逐位比 Text 足够；不需要引入哈希存储（Text 本身就存在库里，
   存哈希是冗余状态，两份真相将来必然漂移）。
3. **nil Metadata 的误判**：`d.Metadata[key]` 对 nil map 返回零值 `""`，
   不用 ok 判断键是否存在的话，`FindByMetadata("source", "")` 会把所有无元数据的
   文档误判为匹配（测试 `TestFindByMetadata` 专查这个）。
4. **删空要重置 dim**：`Delete` 把库删空后不重置 dim，库就"卡在"旧维度上——
   换 embedding 模型重新入库会被维度校验全部拒绝，且报错信息指向 Add 而不是 Delete，
   排查方向完全错误。
5. **全量替换削弱了 all-or-nothing 的一角**：Delete 循环与 Add 之间没有失败源
   （都是纯内存操作），但 Add 本身仍可能失败（如换了 embedding 模型导致维度不符）——
   此时旧块已删。学习项目接受这个取舍（此时本来就该清空旧索引重建）；
   生产做法是 staging 一个临时 Store 再整体切换，或给 Store 加事务式批量替换接口。

### 验证记录（2026-08-06 回补）

在 `/tmp/verify-ex45`（`cp -r mini-agent` 的副本，不影响在制代码）中应用
练习 2/3/4 基础答案 + 本节进阶代码 + 上述测试：

```
cd /tmp/verify-ex45
go vet ./...                          # 通过
go test ./internal/rag/ -v            # 9 个测试全 PASS（6 基础 + 3 去重进阶）
go test ./internal/vectorstore/ -v    # 3 个新增测试全 PASS（FindByMetadata/Delete/dim 重置）
```

验证后副本已删除，项目代码未做任何改动（mini-agent/ 下 git status 与回补前一致）。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [x] `Ingest` 流程为 切块 → 批量 Embed（一次调用）→ 整批 Add，Add 是唯一的改状态步骤且放最后
- [x] 每条记录的 ID 含 source 与块序号；Metadata 写入 `source` 和 `chunk` 两个键
- [x] 空 source、空/全空白文档、向量数与块数不符，都有显式报错；失败时向量库不被污染
- [x] `Execute` 校验畸形 JSON 与空 query；空库时短路返回文案、不调 embedding
- [x] 有低分过滤（阈值自选但注释说明理由），过滤后无命中时返回"知识库中没有相关内容"类明确文案
- [x] 检索输出含 [1][2] 编号 + 来源标注 + 引导模型标注引用的说明语
- [x] 测试用假 Embedder（按文本内容返回预定义向量）实现确定性命中，覆盖：块数与 Metadata、[1] 编号与来源、空库文案、低分过滤文案、畸形参数
- [x] 临时应用练习 2/3 后 `go vet ./...` 和 `go test ./internal/rag/` 全绿（测完记得把练习 2/3 的文件改回自己的实现）
- [x] 真机验证（可选）：设置 SILICONFLOW_API_KEY 后 `/learn` 一篇 md，再问相关问题，观察 kb_search 被调用且回答带来源
- [x] 能口头回答：为什么接口定义在使用方？为什么 top-k 必须配低分过滤？为什么空结果要给明确文案而不是空字符串？为什么入库要 all-or-nothing？
- [x] （进阶）vectorstore 补上 `FindByMetadata`（nil Metadata 用 ok 防误判）与 `Delete`（保持入库顺序、删空重置 dim），各有测试
- [x] （进阶）Ingest 去重：同 source 未变化时短路返回 0（一次 embedding 都不调）；修改后重新入库旧块全清（含变少的尾部块）；不同 source 的相同文本互不去重
- [x] （进阶）能讲清取舍：为什么选"同 source 全量替换"而不是"按块哈希跳过"？为什么删除必须放在 embedding 成功之后？全量替换对 all-or-nothing 的削弱在哪？
