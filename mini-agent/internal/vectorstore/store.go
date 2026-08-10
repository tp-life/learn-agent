// Package vectorstore 是一个内存向量库：给向量建索引、按相似度检索。
//
// 在 RAG 链路中的位置：
//
//	文档 --(chunking)--> 文本块 --(internal/embed)--> 向量 --(本包 Add)--> 向量库
//	用户问题 --(internal/embed)--> 查询向量 --(本包 Search)--> 相关文档 --(练习 4 的 RAG 工具)--> 拼进 prompt
//
// 上游是 internal/embed（练习 1，负责把文本变成向量），
// 下游是练习 4 的 RAG 工具（把 Search 结果塞进 prompt 给模型看）。
//
// 为什么手写暴力检索而不是直接用 Milvus/Qdrant（面试反直觉考点）：
// 暴力检索是 O(N) 全表扫描——每条记录算一次余弦相似度，取 top-k。
// 听起来"慢"，但 1024 维向量一次点积就是 1024 次乘加，
// 10 万条记录总共约 1 亿次浮点运算，现代 CPU 毫秒级跑完。
// 所以"必须上 ANN 索引（HNSW/IVF）"只在百万级以上才成立；
// 学习项目和个人知识库场景，暴力检索反而更简单、结果还精确（ANN 是近似，会丢召回）。
// 先手写暴力版理解检索本质，将来换真向量库时才知道它替你做了什么。
package vectorstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// Document 是向量库里的一条记录：一段文本 + 它的向量 + 溯源元数据。
type Document struct {
	// ID 是文档唯一标识，由调用方生成（如 "doc3-chunk7"）。
	// 入库时校验非空——没有 ID 的记录无法更新、无法在去重时定位。
	ID string

	// Text 是原始文本。向量库存它的原因：Search 返回的 Hit 要直接能拼进 prompt，
	// 如果只存向量，拿到检索结果还得回查一次原文，多一跳依赖。
	Text string

	// Vector 是 Text 的 embedding（由 internal/embed 生成，bge-m3 为 1024 维）。
	Vector []float32

	// Metadata 存溯源信息：来源文档名、chunk 序号、页码等。
	// 引用溯源（"这个答案来自《XX 文档》第 3 段"）全靠它——
	// 没有 Metadata，RAG 的答案就无法给出出处，用户无法验证，可信度大打折扣。
	Metadata map[string]string
}

// Hit 是一次检索命中：文档 + 相似度得分。
type Hit struct {
	Doc   Document
	Score float64 // 余弦相似度，范围 [-1, 1]，越大越相似
}

// Store 是内存向量库。用 slice 平铺存储，检索时全表扫描（暴力检索）。
//
// dim 记录全库统一的向量维度：第一条 Add 的记录定下维度，
// 之后所有记录的维度必须与它一致（见 Add 的校验注释）。
type Store struct {
	docs []Document
	dim  int
}

// NewStore 创建一个空的内存向量库。
func NewStore() *Store {
	return &Store{}
}

// Add 批量入库文档。任一文档校验失败则整批拒绝（all-or-nothing），
// 避免"一半入库一半没入"的中间状态——调用方重试时不用先清理。
//
// 校验规则：
//  1. ID 非空、Vector 非空（见字段注释）；
//  2. 全库维度一致：第一条记录定维度，后续维度不符直接拒绝。
//     为什么这条是硬约束：余弦相似度要求两个向量等长，维度不同的向量
//     根本无法计算相似度；如果让不同维度混进库里，Search 时要么 panic
//     要么算出无意义的结果——而且错误会潜伏到检索时才暴露，极难排查。
//     维度混杂的真实来源通常是"换了 embedding 模型忘了重建索引"。
func (s *Store) Add(docs ...Document) error {
	// 先整批校验，再统一追加，保证 all-or-nothing。
	for i, d := range docs {
		if d.ID == "" {
			return fmt.Errorf("vectorstore: docs[%d] has empty ID", i)
		}
		if len(d.Vector) == 0 {
			return fmt.Errorf("vectorstore: docs[%d] (%s) has empty vector", i, d.ID)
		}
		// 期望维度：库里已有记录用 s.dim；空库时以本批第一条的维度为准
		// （i == 0 时 want 就是自身长度，必然通过——第一条记录定维度）。
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

// Len 返回库中文档数量。
func (s *Store) Len() int {
	return len(s.docs)
}

// TODO(练习2): 余弦相似度 CosineSimilarity
//
// 【任务】实现：
//
//	func CosineSimilarity(a, b []float32) (float64, error)
//
// 计算两个向量的余弦相似度：cos(θ) = a·b / (|a|·|b|)，
// 即点积除以两个模长的乘积，结果范围 [-1, 1]（1 完全相同方向，0 正交，-1 完全相反）。
//
// 【提示】
//   - 一次循环里同时累加三个量：dot += a[i]*b[i]、normA += a[i]²、normB += b[i]²，
//     最后 dot / (sqrt(normA) * sqrt(normB))。累加用 float64——float32 在
//     1024 维累加下精度损失已经可感知（面试加分点：为什么中间计算用 float64）。
//   - 维度不等必须返回 error，不能默默按短的算（见 Add 的维度一致性注释）。
//   - 除零防护：任一向量是零向量（模长为 0）时公式分母为 0，必须返回 error
//     而不是 NaN——零向量没有"方向"，相似度无定义。
//   - 涉及库：math（Sqrt）。
//
// 【验收】go test ./internal/vectorstore/ 通过：相同向量相似度=1、
// 正交向量=0、维度不等报错、零向量报错。
//
// 参考答案：docs/solutions/stage-02/exercise-2-vector-store.md（完成后再看）
func CosineSimilarity(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, errors.New("vectorstore: dim mismatch")
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

	if normA == 0 || normB == 0 {
		return 0, fmt.Errorf("vectorstore: zero vector has no direction, cosine similarity undefined")
	}

	return dot / (math.Sqrt(normA) * math.Sqrt(normB)), nil
}

// TODO(练习2): 暴力 top-k 检索 Search
//
// 【任务】实现：
//
//	func (s *Store) Search(query []float32, topK int) ([]Hit, error)
//
// 用 query 向量对全库每条记录算余弦相似度，返回得分最高的 topK 条，按 Score 降序。
//
// 【提示】
//   - 直接复用上面的 CosineSimilarity，不要内联重算一遍。
//   - query 维度必须与 s.dim 一致，否则报错（理由同 Add 的维度校验）。
//   - topK 边界：topK <= 0 返回空结果还是报错？（建议：返回 error，调用方传 0
//     几乎一定是 bug）；topK 超过库存量时不要报错，返回全部即可。
//   - 空库返回空切片 + nil error，不算错误。
//   - 排序：sort.Slice 或 sort.SliceStable。思考排序稳定性在这里是否有影响
//     （得分相同时返回顺序是否确定），并在注释里写下你的选择和理由。
//   - 进阶思考（不必实现）：全排序是 O(N log N)，其实只需 top-k 可以用堆做到
//     O(N log k)。10 万条内差距不大，但面试可能问。
//
// 【验收】go test ./internal/vectorstore/ 通过：排序正确、topK 截断正确、
// topK 超过库存返回全部、空库不报错、query 维度不符报错。
//
// 参考答案：docs/solutions/stage-02/exercise-2-vector-store.md（完成后再看）
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
			return nil, fmt.Errorf("vectorstore: score doc %s: %w", d.ID, err)
		}
		hits = append(hits, Hit{Doc: d, Score: score})
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })

	if topK > len(hits) {
		topK = len(hits)
	}

	return hits[:topK], nil
}

// TODO(练习2): JSON 持久化 Save / Load
//
// 【任务】实现：
//
//	func (s *Store) Save(path string) error
//	func (s *Store) Load(path string) error
//
// Save 把整个库序列化成 JSON 写入 path；Load 从 path 读回并重建 Store
// （含恢复 dim）。内存库进程退出即丢，持久化是为了不用每次启动都重新 embedding
// （embedding 调用要花钱花时间——这是持久化的真实动机）。
//
// 【提示】
//   - 格式设计：直接 json.Marshal 一个含 Documents 字段的结构体即可
//     （可以把 dim 也存进去做冗余校验）。本包只依赖标准库，encoding/json 够用。
//   - float32 经 JSON 文本序列化有精度损失（十进制表示不精确）——对学习项目
//     可接受，但要在注释里说明；生产上大规模场景会用二进制格式（gob/protobuf）
//     或专用向量库，这是面试可聊的取舍。
//   - Save 建议先写临时文件再 rename（os.CreateTemp + os.Rename），
//     避免写到一半进程崩溃留下半个损坏文件——这个模式叫"原子写入"，值得记住。
//   - Load 不能只 Unmarshal 完事：要重建 s.dim，并逐条校验维度一致
//     （JSON 文件可能被手改坏，不能信任外部输入）；维度不符要报错。
//   - 涉及库：encoding/json、os、path/filepath。
//
// 【验收】go test ./internal/vectorstore/ 通过：Save→Load 往返后
// ID/Text/Vector/Metadata 全部保真（用 t.TempDir() 造临时路径），
// Load 一个维度混杂的坏文件要报错。
//
// 参考答案：docs/solutions/stage-02/exercise-2-vector-store.md（完成后再看）

type storeFile struct {
	Dim      int        `json:"dim"`
	Document []Document `json:"document"`
}

func (s *Store) Save(path string) error {
	data, err := json.MarshalIndent(storeFile{Dim: s.dim, Document: s.docs}, "", "  ")
	if err != nil {
		return fmt.Errorf("vectorstore: marshal: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".vectorstore-*.tmp")
	if err != nil {
		return fmt.Errorf("vectorstore: create temp file :%w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("vectorstore: close temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("vectorstore: rename temp file: %w", err)
	}

	return nil
}

// Load 从 JSON 文件恢复向量库。详见上方 TODO(练习2) 块。
func (s *Store) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("vectorstore: read file: %w", err)
	}

	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("vectorstore: unmarshal: %w", err)
	}

	dim := f.Dim
	if dim == 0 && len(f.Document) > 0 {
		dim = len(f.Document[0].Vector)
	}

	for i, d := range f.Document {
		if d.ID == "" {
			return fmt.Errorf("vectorstore: file document[%d] has empty ID", i)
		}

		if len(d.Vector) == 0 {
			return fmt.Errorf("vectorstore: file document[%d](%s) has empty vector", i, d.ID)
		}

		if len(d.Vector) != dim {
			return fmt.Errorf("vectorstore: file document[%d](%s) dim = %d, want = %d", i, d.ID, len(d.Vector), dim)
		}
	}

	s.docs = f.Document
	s.dim = dim
	return nil
}
