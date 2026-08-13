// 本文件：KnowledgeBase —— RAG 链路的"入库侧"编排层。
//
// 在 agent 链路中的位置：它把前三个练习的零件组装成一句话的入库动作——
//
//	kb.Ingest(source, text) = rag.Chunk 切块（练习 3）
//	                        + Embedder.Embed 翻译成向量（练习 1）
//	                        + vectorstore.Store.Add 入库（练习 2）
//
// 检索侧（把库里的内容找回来给模型看）在 tool.go 的 KBSearch，两者共享
// 同一个 *vectorstore.Store。
package rag

import (
	"fmt"
	"mini-agent/internal/vectorstore"
	"strconv"
	"strings"
)

// Embedder 是"把一批文本翻译成向量"的能力抽象。
//
// 为什么这里自己定义一个小接口，而不是直接依赖 *embed.Client：
// 这是面向接口编程的经典教学点——KnowledgeBase 只关心"能 Embed"这个行为，
// 不关心背后是硅基流动的 HTTP 客户端还是测试里的假实现。
// *embed.Client 的方法签名恰好就是 Embed([]string) ([][]float32, error)，
// Go 的接口是隐式满足的，所以 *embed.Client 天然实现了本接口，
// 一行适配代码都不用写（接口定义在"使用方"而不是"实现方"，这是 Go 与
// Java 式接口思维最大的不同，面试高频）。
//
// 收益在测试里体现：测试可以用一个按文本内容返回预定义向量的假 Embedder，
// 让检索结果完全确定、无需网络、不烧 API 额度。
type Embedder interface {
	Embed(texts []string) ([][]float32, error)
}

// KnowledgeBase 是个人知识库：负责文档入库（切块 → embedding → 向量库）。
//
// 为什么把"入库编排"独立成一个类型而不是散在 main.go 里：
// chunk → embed → add 这三步有固定的顺序约束和错误处理约定（任一步失败
// 都不能污染向量库），收敛在一个方法里才能保证"调用方永远用不错"。
type KnowledgeBase struct {
	embedder Embedder
	store    *vectorstore.Store
	opts     ChunkOptions
}

// NewKnowledgeBase 组装知识库。opts 传零值时用 DefaultChunkOptions 兜底。
// embedder 直接传 *embed.Client 即可（隐式满足 Embedder 接口）。
func NewKnowledgeBase(embedder Embedder, store *vectorstore.Store, opts ChunkOptions) *KnowledgeBase {
	if opts.MaxChars <= 0 {
		opts = DefaultChunkOptions()
	}
	return &KnowledgeBase{embedder: embedder, store: store, opts: opts}
}

// Store 暴露底层向量库，供装配层（main.go）做持久化（Save/Load）。
// 注意：暴露指针意味着调用方可以绕过 Ingest 直接改库——学习项目为简单起见不设防，
// 生产代码会改成只暴露 Save/Load 两个方法。
func (kb *KnowledgeBase) Store() *vectorstore.Store {
	return kb.store
}

// TODO(练习4): 文档入库 Ingest —— RAG 的"写入路径"
//
// 【任务】
// 实现：
//
//	func (kb *KnowledgeBase) Ingest(source, text string) (int, error)
//
// 把一篇文档（source 是来源标识，如文件路径；text 是全文）入库，
// 返回写入的块数。流程固定为三步：
//  1. 用 Chunk(text, kb.opts) 切块；
//  2. 用 kb.embedder.Embed(chunks) 一次性批量 embedding（不要逐块调用，
//     批量接口一次 HTTP 请求搞定，见 internal/embed 的批量设计注释）；
//  3. 组装 []vectorstore.Document 后 store.Add 整批入库。
//
// 【提示】
//   - Document.ID 建议 fmt.Sprintf("%s#%d", source, i)（source + 块序号），
//     一眼可读、天然唯一。
//   - Document.Metadata 必须写 {"source": source, "chunk": strconv.Itoa(i)}——
//     引用溯源（"答案来自《XX》第 N 段"）全靠它，漏了 source 检索结果就没法
//     标注出处（见 vectorstore.Document 的 Metadata 注释）。
//   - 防御性校验：source 空白直接报错；Chunk 产出 0 块（空文档）直接报错；
//     Embed 返回的向量数与块数不一致直接报错（假实现/异常响应的自我保护）。
//   - 入库是最后一步：任何前置步骤失败都直接返回错误，此时向量库未被触碰——
//     配合 Add 的 all-or-nothing 语义，Ingest 整体也是 all-or-nothing。
//
// 【验收】
// 完成练习 2、3 后 go test ./internal/rag/ 通过（测试由参考答案提供，
// 用假 Embedder 验证：块数正确、Metadata 含 source 与 chunk 序号、
// 空文档报错）。
//
// 参考答案：docs/solutions/stage-02/exercise-4-rag-tool.md（完成后再看）
func (kb *KnowledgeBase) Ingest(source, text string) (int, error) {
	if strings.TrimSpace(source) == "" {
		return 0, fmt.Errorf("rag: source is empty")
	}

	chunks := Chunk(text, kb.opts)
	if len(chunks) == 0 {
		return 0, fmt.Errorf("rag: %s produced no chunks (empty or blank document)", source)
	}

	existing := kb.store.FindByMetadata("source", source)
	if sameChunks(existing, chunks) {
		return 0, nil
	}

	// 批量embedding
	vectors, err := kb.embedder.Embed(chunks)
	if err != nil {
		return 0, fmt.Errorf("rag: embed chunks of %s: %w", source, err)
	}

	if len(vectors) != len(chunks) {
		return 0, fmt.Errorf("rag: embedder returned %d vectors for %d chunks", len(vectors), len(chunks))
	}

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

	if err := kb.store.Add(docs...); err != nil {
		return 0, fmt.Errorf("rag: add chunks of %s:%w", source, err)
	}
	return len(docs), nil
}

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
