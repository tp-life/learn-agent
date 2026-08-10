# 练习 2 参考答案：内存向量库 vectorstore

> 对应 TODO：`mini-agent/internal/vectorstore/store.go` 的三处 `TODO(练习2)`（CosineSimilarity / Search / Save+Load）。
> **完成练习并自评后再看本文档。**
> 本文档代码已于 2026-08-06 实际粘贴进项目验证：`go vet ./...` 通过，`go test ./internal/vectorstore/ -v` 15 个测试全部通过（验证后已恢复为骨架版）。
> 2026-08-06 回补：新增"三、进阶实现"一节（堆版 top-k），代码同样在项目副本中实测通过——
> 基础 15 个测试 + 堆版 3 个测试全绿，Benchmark 实测数字见该节（验证后项目保持骨架版）。

---

## 一、参考实现

### `internal/vectorstore/store.go`（只给出需要实现的部分；骨架的包注释、类型、NewStore/Add/Len 不变）

import 从骨架的 `errors`、`fmt` 改为（`errors` 不再需要）：

```go
import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)
```

```go
// CosineSimilarity 计算两个向量的余弦相似度：cos(θ) = a·b / (|a|·|b|)。
//
// 为什么用余弦而不是欧氏距离：embedding 的"语义"编码在方向上而非长度上，
// 余弦相似度只比方向、忽略模长，所以两段长短不同但语义相近的文本得分依然高。
// 结果范围 [-1, 1]：1 完全同向，0 正交（无关），-1 完全反向。
//
// 中间累加用 float64 而非 float32：1024 维点积要累加 1024 个乘积，
// float32 只有约 7 位十进制有效数字，累加误差已经可感知（面试加分点）。
func CosineSimilarity(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("vectorstore: dim mismatch: %d vs %d", len(a), len(b))
	}
	if len(a) == 0 {
		return 0, fmt.Errorf("vectorstore: empty vectors")
	}

	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	// 除零防护：零向量模长为 0，公式分母为 0。
	// 零向量没有"方向"，相似度无定义——返回 error 而不是静默给出 NaN，
	// NaN 一旦混进 Search 的排序，比较结果全是 false，顺序不可预期，极难排查。
	if normA == 0 || normB == 0 {
		return 0, fmt.Errorf("vectorstore: zero vector has no direction, cosine similarity undefined")
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB)), nil
}

// Search 暴力 top-k 检索：对全库每条记录算余弦相似度，返回得分最高的 topK 条，
// 按 Score 降序。空库返回空切片（不是错误）。
//
// topK <= 0 返回 error——调用方传 0 几乎一定是 bug（比如忘了设默认值），
// 静默返回空结果会让上游误以为"没检索到相关内容"而继续走错误分支。
// topK 超过库存量时不报错，返回全部即可（调用方想要的是"尽量多"）。
func (s *Store) Search(query []float32, topK int) ([]Hit, error) {
	if topK <= 0 {
		return nil, fmt.Errorf("vectorstore: topK must be positive, got %d", topK)
	}
	if len(s.docs) == 0 {
		return []Hit{}, nil
	}
	if len(query) != s.dim {
		return nil, fmt.Errorf("vectorstore: query dim = %d, want %d", len(query), s.dim)
	}

	hits := make([]Hit, 0, len(s.docs))
	for _, d := range s.docs {
		score, err := CosineSimilarity(query, d.Vector)
		if err != nil {
			// 理论上不会触发（Add 已保证全库维度一致），防御性返回。
			return nil, fmt.Errorf("vectorstore: score doc %s: %w", d.ID, err)
		}
		hits = append(hits, Hit{Doc: d, Score: score})
	}

	// 用 SliceStable 而非 Slice：得分相同（比如两条完全相同的文档）时
	// 保持入库顺序，检索结果可复现——测试和调试都依赖确定性输出。
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })

	if topK > len(hits) {
		topK = len(hits)
	}
	return hits[:topK], nil
}

// storeFile 是持久化的磁盘格式。dim 冗余存一份：Load 时可以先拿它做校验，
// 即使文件被手工改坏也能给出更明确的报错。
type storeFile struct {
	Dim       int        `json:"dim"`
	Documents []Document `json:"documents"`
}

// Save 把整个库序列化为 JSON 写入 path。
//
// 持久化的动机：内存库进程退出即丢，而重建索引要重新调 embedding API——
// 既花钱又慢，所以入库一次、落盘复用。
//
// 关于 float32 的 JSON 精度：Go 的 encoding/json 对 float32 输出"最短可往返"
// 的十进制表示，Go-to-Go 的 Save→Load 是无损的；但其他语言解析或手工编辑
// 文件时可能引入误差。对学习项目这完全可接受；生产大规模场景会用二进制
// 格式（gob/protobuf）或专用向量库——面试可以主动聊这个取舍。
func (s *Store) Save(path string) error {
	data, err := json.MarshalIndent(storeFile{Dim: s.dim, Documents: s.docs}, "", "  ")
	if err != nil {
		return fmt.Errorf("vectorstore: marshal: %w", err)
	}

	// 原子写入：先写同目录临时文件，成功后 rename 覆盖目标。
	// 直接写目标文件的话，写到一半进程崩溃会留下半个损坏的 JSON，
	// 下次启动 Load 直接挂掉且原数据已丢。rename 在同文件系统内是原子操作，
	// 任意时刻目标路径要么是完整的旧版本、要么是完整的新版本。
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vectorstore-*.tmp")
	if err != nil {
		return fmt.Errorf("vectorstore: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("vectorstore: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("vectorstore: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("vectorstore: rename temp file: %w", err)
	}
	return nil
}

// Load 从 JSON 文件恢复向量库，重建 dim 并逐条校验。
//
// 校验失败时返回 error 且不改动现有数据（先 Load 到局部变量再整体替换），
// 这样"重载一个坏文件"不会把正在运行的库冲掉。
// JSON 文件是外部输入（可能被手改、被旧版本程序写出），不能信任，必须校验：
// 校验规则与 Add 完全一致——ID 非空、Vector 非空、全库维度一致。
func (s *Store) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("vectorstore: read file: %w", err)
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("vectorstore: unmarshal: %w", err)
	}

	// 重建 dim：优先用文件里存的，没有（旧格式/手删）则用第一条记录的维度。
	dim := f.Dim
	if dim == 0 && len(f.Documents) > 0 {
		dim = len(f.Documents[0].Vector)
	}
	for i, d := range f.Documents {
		if d.ID == "" {
			return fmt.Errorf("vectorstore: file documents[%d] has empty ID", i)
		}
		if len(d.Vector) == 0 {
			return fmt.Errorf("vectorstore: file documents[%d] (%s) has empty vector", i, d.ID)
		}
		if len(d.Vector) != dim {
			return fmt.Errorf("vectorstore: file documents[%d] (%s) dim = %d, want %d", i, d.ID, len(d.Vector), dim)
		}
	}

	s.docs = f.Documents
	s.dim = dim
	return nil
}
```

### `internal/vectorstore/store_test.go`（新建，纯内存 + t.TempDir()，无网络依赖）

```go
package vectorstore

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// --- CosineSimilarity ---

// TestCosineSimilarity_Identical 相同向量相似度应为 1。
// 断言用近似比较：float64 累加的舍入误差让结果可能是 0.9999999999999999。
func TestCosineSimilarity_Identical(t *testing.T) {
	v := []float32{0.1, -0.2, 0.3, 0.4}
	got, err := CosineSimilarity(v, v)
	if err != nil {
		t.Fatalf("CosineSimilarity: %v", err)
	}
	if math.Abs(got-1) > 1e-9 {
		t.Errorf("identical vectors: got %v, want 1", got)
	}
}

// TestCosineSimilarity_Orthogonal 正交向量相似度应为 0。
func TestCosineSimilarity_Orthogonal(t *testing.T) {
	got, err := CosineSimilarity([]float32{1, 0, 0}, []float32{0, 1, 0})
	if err != nil {
		t.Fatalf("CosineSimilarity: %v", err)
	}
	if math.Abs(got) > 1e-9 {
		t.Errorf("orthogonal vectors: got %v, want 0", got)
	}
}

// TestCosineSimilarity_Opposite 反向向量相似度应为 -1。
func TestCosineSimilarity_Opposite(t *testing.T) {
	got, err := CosineSimilarity([]float32{1, 2}, []float32{-1, -2})
	if err != nil {
		t.Fatalf("CosineSimilarity: %v", err)
	}
	if math.Abs(got+1) > 1e-9 {
		t.Errorf("opposite vectors: got %v, want -1", got)
	}
}

// TestCosineSimilarity_DimMismatch 维度不等必须报错，不能按短的默默算。
func TestCosineSimilarity_DimMismatch(t *testing.T) {
	if _, err := CosineSimilarity([]float32{1, 2}, []float32{1, 2, 3}); err == nil {
		t.Error("dim mismatch: want error, got nil")
	}
}

// TestCosineSimilarity_ZeroVector 零向量无方向，必须报错而不是返回 NaN。
func TestCosineSimilarity_ZeroVector(t *testing.T) {
	if _, err := CosineSimilarity([]float32{0, 0}, []float32{1, 2}); err == nil {
		t.Error("zero vector: want error, got nil")
	}
}

// --- Add ---

// TestAdd_Validation 校验：空 ID、空向量、维度不符都要拒绝。
func TestAdd_Validation(t *testing.T) {
	s := NewStore()
	if err := s.Add(Document{Text: "x", Vector: []float32{1}}); err == nil {
		t.Error("empty ID: want error, got nil")
	}
	if err := s.Add(Document{ID: "a"}); err == nil {
		t.Error("empty vector: want error, got nil")
	}

	// 第一条记录定维度为 2，之后 3 维的必须被拒。
	if err := s.Add(Document{ID: "a", Vector: []float32{1, 2}}); err != nil {
		t.Fatalf("Add first doc: %v", err)
	}
	if err := s.Add(Document{ID: "b", Vector: []float32{1, 2, 3}}); err == nil {
		t.Error("dim mismatch: want error, got nil")
	}
}

// TestAdd_AllOrNothing 整批校验：批内有一条不合法，合法的也不能入库。
func TestAdd_AllOrNothing(t *testing.T) {
	s := NewStore()
	err := s.Add(
		Document{ID: "good", Vector: []float32{1, 2}},
		Document{ID: "bad", Vector: []float32{1, 2, 3}},
	)
	if err == nil {
		t.Fatal("want error for mixed-dim batch, got nil")
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0（all-or-nothing 被破坏）", s.Len())
	}
}

// --- Search ---

// newTestStore 建一个 2 维小库：a 与查询向量 [1,0] 同向，b 成 45°，c 正交。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	err := s.Add(
		Document{ID: "a", Text: "same direction", Vector: []float32{1, 0}},
		Document{ID: "b", Text: "45 degrees", Vector: []float32{1, 1}},
		Document{ID: "c", Text: "orthogonal", Vector: []float32{0, 1}},
	)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return s
}

// TestSearch_Ranking 检索结果必须按 Score 降序。
func TestSearch_Ranking(t *testing.T) {
	s := newTestStore(t)
	hits, err := s.Search([]float32{1, 0}, 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(hits) != len(want) {
		t.Fatalf("got %d hits, want %d", len(hits), len(want))
	}
	for i, id := range want {
		if hits[i].Doc.ID != id {
			t.Errorf("hits[%d].Doc.ID = %s, want %s", i, hits[i].Doc.ID, id)
		}
	}
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score < hits[i].Score {
			t.Errorf("not descending: hits[%d]=%v < hits[%d]=%v", i-1, hits[i-1].Score, i, hits[i].Score)
		}
	}
}

// TestSearch_TopKTruncate topK 截断：只要前 1 条就只给 1 条。
func TestSearch_TopKTruncate(t *testing.T) {
	s := newTestStore(t)
	hits, err := s.Search([]float32{1, 0}, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Doc.ID != "a" {
		t.Errorf("got %+v, want single hit a", hits)
	}
}

// TestSearch_TopKExceedsSize topK 超过库存时返回全部，不报错。
func TestSearch_TopKExceedsSize(t *testing.T) {
	s := newTestStore(t)
	hits, err := s.Search([]float32{1, 0}, 100)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Errorf("got %d hits, want 3（全部）", len(hits))
	}
}

// TestSearch_EmptyStore 空库返回空结果，不算错误。
func TestSearch_EmptyStore(t *testing.T) {
	s := NewStore()
	hits, err := s.Search([]float32{1, 0}, 5)
	if err != nil {
		t.Fatalf("Search on empty store: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("got %d hits, want 0", len(hits))
	}
}

// TestSearch_InvalidArgs topK <= 0 和 query 维度不符都要报错。
func TestSearch_InvalidArgs(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Search([]float32{1, 0}, 0); err == nil {
		t.Error("topK=0: want error, got nil")
	}
	if _, err := s.Search([]float32{1, 0, 0}, 1); err == nil {
		t.Error("query dim mismatch: want error, got nil")
	}
}

// --- Save / Load ---

// TestSaveLoad_RoundTrip 往返后 ID/Text/Vector/Metadata/dim 全部保真。
// Go 的 encoding/json 对 float32 输出最短可往返表示，所以可以直接断言相等。
func TestSaveLoad_RoundTrip(t *testing.T) {
	s := NewStore()
	docs := []Document{
		{ID: "d1", Text: "向量检索", Vector: []float32{0.1, 0.2, 0.3},
			Metadata: map[string]string{"source": "guide.md", "chunk": "0"}},
		{ID: "d2", Text: "RAG", Vector: []float32{-0.5, 1.5, 0},
			Metadata: map[string]string{"source": "guide.md", "chunk": "1"}},
	}
	if err := s.Add(docs...); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// t.TempDir() 自动创建并清理临时目录，测试不留垃圾文件。
	path := filepath.Join(t.TempDir(), "store.json")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2 := NewStore()
	if err := s2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s2.Len() != len(docs) {
		t.Fatalf("Len = %d, want %d", s2.Len(), len(docs))
	}
	hits, err := s2.Search(docs[0].Vector, 2)
	if err != nil {
		t.Fatalf("Search after Load: %v", err)
	}
	if hits[0].Doc.ID != "d1" {
		t.Errorf("top hit = %s, want d1（dim 未正确恢复？）", hits[0].Doc.ID)
	}

	// 逐字段断言保真。
	for i, want := range docs {
		got := s2.docs[i]
		if got.ID != want.ID || got.Text != want.Text {
			t.Errorf("docs[%d]: got {%s %s}, want {%s %s}", i, got.ID, got.Text, want.ID, want.Text)
		}
		if len(got.Vector) != len(want.Vector) {
			t.Fatalf("docs[%d] vector len = %d, want %d", i, len(got.Vector), len(want.Vector))
		}
		for j := range want.Vector {
			if got.Vector[j] != want.Vector[j] {
				t.Errorf("docs[%d].Vector[%d] = %v, want %v", i, j, got.Vector[j], want.Vector[j])
			}
		}
		if len(got.Metadata) != len(want.Metadata) {
			t.Fatalf("docs[%d] metadata len = %d, want %d", i, len(got.Metadata), len(want.Metadata))
		}
		for k, v := range want.Metadata {
			if got.Metadata[k] != v {
				t.Errorf("docs[%d].Metadata[%q] = %q, want %q", i, k, got.Metadata[k], v)
			}
		}
	}
}

// TestLoad_RejectsBadFile 坏文件必须报错：非法 JSON、维度混杂。
func TestLoad_RejectsBadFile(t *testing.T) {
	dir := t.TempDir()

	badJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badJSON, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewStore().Load(badJSON); err == nil {
		t.Error("invalid JSON: want error, got nil")
	}

	mixedDim := filepath.Join(dir, "mixed.json")
	content := `{"dim":2,"documents":[{"ID":"a","Vector":[1,2]},{"ID":"b","Vector":[1,2,3]}]}`
	if err := os.WriteFile(mixedDim, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewStore().Load(mixedDim); err == nil {
		t.Error("mixed dims: want error, got nil")
	}
}

// TestSave_AtomicWrite Save 成功后不应残留临时文件。
func TestSave_AtomicWrite(t *testing.T) {
	s := NewStore()
	if err := s.Add(Document{ID: "a", Vector: []float32{1}}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := s.Save(filepath.Join(dir, "store.json")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	leftover, err := filepath.Glob(filepath.Join(dir, ".vectorstore-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Errorf("leftover temp files: %v", leftover)
	}
}
```

## 二、关键设计点

1. **余弦相似度的三个坑都得堵住**：① 维度不等必须报错——按短向量截断算出的分数在数学上无意义，而且这种错误会伪装成"检索质量差"，排查成本极高；② 零向量必须报错——返回 NaN 的话，`sort.SliceStable` 的比较函数对 NaN 恒返回 false，排序结果不确定且不会报任何错；③ 中间累加用 float64——1024 维 float32 累加的舍入误差已可感知。**易错处**：只写 `dot / (math.Sqrt(normA) * math.Sqrt(normB))` 一行公式，三个防护一个都没有，正常数据下测试全过，坏数据进来才爆。

2. **topK 的不对称处理是有意的**：`topK <= 0` 报错、`topK > 库存` 不报错。理由：传 0 几乎一定是调用方 bug（忘了设默认值），静默返回空会让上游误判"没有相关内容"；而"要 100 条但只有 3 条"是正常情况，给全部就是正确回答。**易错处**：写成 `if topK > len(hits) { return nil, error }` 或反过来对 topK<=0 返回空，都属于没想清楚"谁在什么场景下会传这个值"。

3. **排序稳定性用 SliceStable**：得分相同的命中保持入库顺序。这里不是正确性问题（相同分数谁先谁后都对），而是**可复现性**问题——测试断言、调试对比、缓存命中都依赖确定输出。用 `sort.Slice` 也能通过本练习的测试，但得分并列的用例下顺序不保证，是个潜伏的坑。

4. **Search 复用 CosineSimilarity 而不是内联重算**：内联省一次函数调用，但维度校验、零向量防护就得复制一遍，两处逻辑将来必然漂移。10 万条规模下函数调用开销可忽略（真要优化也是上 SIMD 或堆选 top-k，不是省调用）。

5. **原子写入（CreateTemp + Rename）**：直接 `os.WriteFile(path, ...)` 的话，写到一半进程崩溃会留下半个损坏 JSON，下次 Load 挂掉且旧数据已丢。临时文件必须建在**目标同目录**（`filepath.Dir(path)`）——跨文件系统的 rename 不是原子操作，建在 os.TempDir() 里可能翻车。**易错处**：错误路径上忘记 `os.Remove(tmpName)`，崩溃/失败后目录里堆满 `.vectorstore-*.tmp` 垃圾文件（测试 `TestSave_AtomicWrite` 专门查这个）。

6. **Load 先校验再替换**：全部校验通过后最后两行才动 `s.docs`/`s.dim`。这样对一个正在使用的库 Load 坏文件，返回 error 但旧数据完好。校验规则与 Add 完全一致（ID 非空、向量非空、维度一致）——JSON 文件是外部输入，可能被手改或来自旧版本程序，不能信任。

7. **float32 的 JSON 精度要说准确**：Go 的 `encoding/json` 对 float32 输出的是"最短可往返"十进制表示，所以 Go-to-Go 的 Save→Load **无损**，测试可以直接用 `!=` 断言向量相等。真正的精度风险在跨语言解析或手工编辑文件时。生产大规模场景不落 JSON 文本，用二进制格式或专用向量库——这是面试可以主动展开的取舍点。

8. **进阶：堆版 top-k 已有完整实现，见"三、进阶实现"**：全排序是 O(N log N)，只要 top-k 可以用最小堆做到 O(N log k)。实测 10 万条下堆版快约 11 倍、内存省约 4500 倍，但单次查询的绝对耗时都在毫秒级——是否值得用堆要看查询频率和延迟要求，量化结论见进阶实现一节。这与包注释里"暴力检索够用"是同一个判断：**先量化，再优化**。

## 三、进阶实现：堆版 top-k（O(N log k)）

> 对应 store.go 中 Search 的 TODO 提示"进阶要求"。本节代码已于 2026-08-06 在项目副本中
> 实测验证（等价性测试全过 + Benchmark 实测），不进项目代码树——项目里请自己实现。

### 为什么堆能做到 O(N log k)

全排序版把 N 条打分全部排序，再取前 k——但我们只关心前 k 名，后 N-k 名的相对顺序
白排了。换用**大小为 k 的最小堆**：堆里始终装着"目前见过的 top-k 候选"，堆顶是候选中
最差的一条。每扫到一条新记录，只和堆顶比一次：不如堆顶就直接丢弃（O(1)），比堆顶好
就替换堆顶并下沉调整（O(log k)）。全库扫一遍总成本 O(N log k)，k 越小越划算。

### 完整实现（`internal/vectorstore/topk.go`，新建文件）

```go
package vectorstore

import (
	"container/heap"
	"fmt"
)

// heapItem 是堆里的候选：入库序号 idx 用于得分并列时的决胜（tie-break），
// 保证与 Search 的稳定排序结果逐条一致。
type heapItem struct {
	idx   int
	score float64
}

// minHeap 是最小堆：堆顶永远是当前候选集中"最差"的一条，新候选只要比堆顶好就顶替它。
// "差"的定义：分数低者差；分数相同则入库序号大者差（后入库的先被淘汰）——
// 这正好对齐 Search 用 SliceStable 降序后"同分保持入库顺序"的语义。
type minHeap []heapItem

func (h minHeap) Len() int { return len(h) }

func (h minHeap) Less(i, j int) bool {
	if h[i].score != h[j].score {
		return h[i].score < h[j].score
	}
	return h[i].idx > h[j].idx
}

func (h minHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *minHeap) Push(x any) { *h = append(*h, x.(heapItem)) }

func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// better 报告候选 (score, idx) 是否比堆顶 worst 更好、应该顶替它。
// 严格"更好"才替换：同分且序号更大时不换，保证先入库的留下（稳定语义）。
func better(score float64, idx int, worst heapItem) bool {
	if score != worst.score {
		return score > worst.score
	}
	return idx < worst.idx
}

// SearchTopK 是 Search 的堆优化版：O(N log k) 代替全排序的 O(N log N)。
//
// 思路：维护一个大小不超过 k 的最小堆，堆顶是当前 top-k 候选里最差的一条。
// 扫到一条新记录，只在它比堆顶更好时才替换堆顶并 heap.Fix 下沉（O(log k)），
// 否则直接丢弃（O(1) 比较）。全库扫一遍，堆里就是 top-k。
//
// 收尾：连续 heap.Pop 的输出天然有序（堆排序原理：每次弹出 Less 意义下的最小值），
// 即"最差先出"，倒序填入结果就是得分降序、同分按入库序——与 Search 逐条一致。
func (s *Store) SearchTopK(query []float32, topK int) ([]Hit, error) {
	// 参数校验与 Search 完全一致：两个入口的行为必须可互换。
	if topK <= 0 {
		return nil, fmt.Errorf("vectorstore: topK must be positive, got %d", topK)
	}
	if len(s.docs) == 0 {
		return []Hit{}, nil
	}
	if len(query) != s.dim {
		return nil, fmt.Errorf("vectorstore: query dim = %d, want %d", len(query), s.dim)
	}

	h := &minHeap{}
	heap.Init(h)
	for i, d := range s.docs {
		score, err := CosineSimilarity(query, d.Vector)
		if err != nil {
			return nil, fmt.Errorf("vectorstore: score doc %s: %w", d.ID, err)
		}
		if h.Len() < topK {
			heap.Push(h, heapItem{idx: i, score: score})
		} else if better(score, i, (*h)[0]) {
			// 直接改堆顶再 Fix，比 Pop+Push 省一次堆调整。
			(*h)[0] = heapItem{idx: i, score: score}
			heap.Fix(h, 0)
		}
	}

	out := make([]Hit, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		it := heap.Pop(h).(heapItem)
		out[i] = Hit{Doc: s.docs[it.idx], Score: it.score}
	}
	return out, nil
}
```

### 等价性测试与 Benchmark（`internal/vectorstore/topk_test.go`，新建文件）

```go
package vectorstore

import (
	"fmt"
	"math/rand"
	"testing"
)

// newRandomStore 造一个 N 条、dim 维的随机向量库（固定种子，可复现）。
// 向量分量带正偏移，避免随机出近零向量触发 CosineSimilarity 的除零防护。
func newRandomStore(n, dim int, seed int64) *Store {
	r := rand.New(rand.NewSource(seed))
	s := NewStore()
	docs := make([]Document, n)
	for i := range docs {
		v := make([]float32, dim)
		for j := range v {
			v[j] = r.Float32() + 0.01
		}
		docs[i] = Document{ID: fmt.Sprintf("doc-%d", i), Vector: v}
	}
	if err := s.Add(docs...); err != nil {
		panic(err)
	}
	return s
}

// assertSameHits 逐条比较两个检索结果：ID 顺序与得分都要一致。
func assertSameHits(t *testing.T, want, got []Hit) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Doc.ID != want[i].Doc.ID || got[i].Score != want[i].Score {
			t.Errorf("hits[%d]: got {%s %v}, want {%s %v}",
				i, got[i].Doc.ID, got[i].Score, want[i].Doc.ID, want[i].Score)
		}
	}
}

// TestSearchTopK_Equivalent 随机向量下，堆版必须与全排序版逐条一致，
// 覆盖 k 的各种边界：1、小 k、k == N、k > N。
func TestSearchTopK_Equivalent(t *testing.T) {
	s := newRandomStore(1000, 16, 42)
	query := make([]float32, 16)
	for j := range query {
		query[j] = 0.5
	}
	for _, k := range []int{1, 5, 10, 100, 1000, 2000} {
		want, err := s.Search(query, k)
		if err != nil {
			t.Fatalf("Search(k=%d): %v", k, err)
		}
		got, err := s.SearchTopK(query, k)
		if err != nil {
			t.Fatalf("SearchTopK(k=%d): %v", k, err)
		}
		assertSameHits(t, want, got)
	}
}

// TestSearchTopK_Ties 得分并列（重复向量）时，堆版同样保持入库顺序。
func TestSearchTopK_Ties(t *testing.T) {
	s := NewStore()
	// 三条完全相同的向量 + 一条正交向量：前三名同分，必须按入库序 a、b、c。
	if err := s.Add(
		Document{ID: "a", Vector: []float32{1, 0}},
		Document{ID: "b", Vector: []float32{1, 0}},
		Document{ID: "c", Vector: []float32{1, 0}},
		Document{ID: "d", Vector: []float32{0, 1}},
	); err != nil {
		t.Fatal(err)
	}
	want, err := s.Search([]float32{1, 0}, 3)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SearchTopK([]float32{1, 0}, 3)
	if err != nil {
		t.Fatal(err)
	}
	assertSameHits(t, want, got)
	if got[0].Doc.ID != "a" || got[1].Doc.ID != "b" || got[2].Doc.ID != "c" {
		t.Errorf("tie order: got %s,%s,%s want a,b,c", got[0].Doc.ID, got[1].Doc.ID, got[2].Doc.ID)
	}
}

// TestSearchTopK_InvalidArgs 参数校验与 Search 对齐。
func TestSearchTopK_InvalidArgs(t *testing.T) {
	s := newRandomStore(10, 4, 1)
	if _, err := s.SearchTopK(make([]float32, 4), 0); err == nil {
		t.Error("topK=0: want error, got nil")
	}
	if _, err := s.SearchTopK(make([]float32, 8), 1); err == nil {
		t.Error("query dim mismatch: want error, got nil")
	}
	empty := NewStore()
	hits, err := empty.SearchTopK([]float32{1}, 5)
	if err != nil || len(hits) != 0 {
		t.Errorf("empty store: got %v hits, err %v; want 0 hits, nil err", len(hits), err)
	}
}

// --- Benchmark：N = 10 万条，128 维，k = 10 ---

const (
	benchN   = 100000
	benchDim = 128
	benchK   = 10
)

func BenchmarkSearchSort(b *testing.B) {
	s := newRandomStore(benchN, benchDim, 7)
	query := make([]float32, benchDim)
	for j := range query {
		query[j] = 0.5
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Search(query, benchK); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchHeap(b *testing.B) {
	s := newRandomStore(benchN, benchDim, 7)
	query := make([]float32, benchDim)
	for j := range query {
		query[j] = 0.5
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.SearchTopK(query, benchK); err != nil {
			b.Fatal(err)
		}
	}
}
```

验证命令与实测结果（2026-08-06，Apple M4 / darwin arm64）：

```
go vet ./...                                # 通过
go test ./internal/vectorstore/ -v          # 基础 15 个 + 堆版 3 个，全部 PASS
go test ./internal/vectorstore/ -run '^$' -bench=. -benchtime=10x -benchmem
BenchmarkSearchSort-10   10   82846588 ns/op   7200920 B/op    4 allocs/op
BenchmarkSearchHeap-10   10    7564158 ns/op      1608 B/op   27 allocs/op
```

即：N=10 万、128 维、k=10 时，堆版 **82.8ms → 7.6ms，快约 11 倍**；
每次查询的堆分配从 **7.2MB 降到 1.6KB**（全排序版要给 N 条 Hit 分配大切片，堆版只分配 k 条）。
（Benchmark 用 128 维而非 bge-m3 的 1024 维，是为了控制造库内存；维度只影响两版相同的
打分开销，差异全在"选 top-k"环节，结论可以外推。）

### 易错处

1. **堆方向搞反**：要选"分数最高"的 top-k，用的却是**最小堆**——堆顶放当前候选里最差的，
   新纪录比它好才进门。写成最大堆就变成"留住最差的 k 条"，结果恰好相反。
2. **并列得分的语义漂移**：Search 用 SliceStable，同分保持入库顺序。堆版如果 Less 只比分数、
   替换条件写成 `score >= 堆顶`，同分场景下返回的集合和顺序都会和 Search 不一致。
   本实现用 (score, idx) 二元比较对齐稳定语义，`TestSearchTopK_Ties` 专查这个。
3. **container/heap 的接口签名**：`Push/Pop` 是**指针接收者**且参数/返回值是 `any`，
   `Len/Less/Swap` 是值接收者——写反了编译报错信息不太直观。
4. **收尾顺序**：连续 Pop 是"最差先出"（堆排序原理），要**倒序填入**结果切片才是降序；
   直接按弹出顺序 append 得到的是升序。
5. **参数校验必须与 Search 对齐**：两个入口对外行为要可互换（topK<=0 报错、空库返回空、
   维度不符报错），否则调用方换个实现就踩雷。

### 什么时候才值得用堆（量化结论）

- **本学习项目 / 个人知识库：不值得换**。RAG 一次问答只查一两回，82ms 与 7.6ms 的差异
  用户无感，全排序版 10 行代码 vs 堆版 60 行 + 一堆边界语义，简单即正义。
- **值得换的信号**：QPS 高（检索在请求热路径上，11 倍的 CPU 差距直接换算成机器成本）；
  N 到百万级但暂时不想上 ANN 索引；对单次查询 p99 延迟有硬性要求；或内存/GC 敏感
  （每次查询省 7MB 分配，高并发下 GC 压力差距很大）。
- k 接近 N 时堆没有优势（O(N log k) 退化为 O(N log N)，还多一堆常数开销），
  堆的收益前提是 k ≪ N——top-k 检索天然满足。
- 面试话术：先给复杂度（O(N log N) → O(N log k)），再给实测数字（10 万条 11 倍、
  内存 4500 倍），最后给决策（低频毫秒级不优化，高频/大规模才上）——**先量化，再优化**。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [x] `CosineSimilarity` 公式正确（dot / (|a||b|)），中间累加用 float64
- [x] 维度不等、空向量、零向量（除零）都返回 error，不产生 NaN
- [x] `Search` 按 Score 降序，topK 截断正确；topK <= 0 报错、topK 超库存返回全部、空库返回空不报错、query 维度不符报错
- [x] 排序选择了稳定排序（或能讲清为什么自己的场景不需要稳定）
- [x] `Search` 复用 `CosineSimilarity`，没有复制粘贴公式
- [x] `Save`/`Load` 往返后 ID/Text/Vector/Metadata/dim 全部保真（测试用 `t.TempDir()`）
- [x] `Load` 重建 dim 并逐条校验（坏 JSON、维度混杂的文件都报错），校验失败不污染现有数据
- [x] `Save` 用临时文件 + rename 原子写入（或能讲清直接写的风险），错误路径清理临时文件
- [x] `go vet ./...` 和 `go test ./internal/vectorstore/` 全绿
- [x] 能口头回答：为什么 10 万条内不需要 HNSW？为什么用余弦不用欧氏距离？为什么维度混杂必须入库时就拒？
- [x] （进阶）用 `container/heap` 实现 O(N log k) 的堆版 top-k：最小堆维护大小为 k 的候选集，最后倒序输出
- [x] （进阶）堆版与全排序版的等价性测试通过（随机向量 + 并列得分的 tie-break），参数校验行为与 `Search` 对齐
- [x] （进阶）能讲清"什么时候才值得用堆"：复杂度（O(N log N) → O(N log k)）、实测差距（10 万条约 11 倍、每次查询省约 7MB 分配）、决策依据（低频毫秒级不优化）
