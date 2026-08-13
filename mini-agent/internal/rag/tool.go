// 本文件：kb_search 工具 —— RAG 链路的"检索侧"，把知识库交到模型手上。
//
// 在 agent 链路中的位置：模型通过 tools.Tool 接口"请求"检索，
// Execute 把 用户问题 → 向量 → top-k 命中 → 带编号的参考文本 组装好，
// 作为 tool 消息回到对话历史，模型据此生成带引用的回答。
package rag

import (
	"encoding/json"
	"fmt"
	"mini-agent/internal/vectorstore"
	"strings"
)

// kbSearchArgs 是 kb_search 的参数结构，与 ParametersSchema 一一对应。
// 模型的输出是不可信输入：可能漏字段、传畸形 JSON，Execute 里必须校验。
type kbSearchArgs struct {
	Query string `json:"query"`
}

// KBSearch 实现 tools.Tool 接口：在用户知识库里做语义检索。
//
// 为什么检索工具持有 Embedder 而不只是 Store：
// 用户的问题是文本，库里的索引是向量，检索前必须先把问题翻译成
// 与入库时同一个向量空间——所以查询侧和入库侧必须用同一个 embedding 模型，
// 换模型 = 换向量空间，旧索引全部作废（见 embed.Client.WithModel 的注释）。
type KBSearch struct {
	embedder Embedder
	store    *vectorstore.Store
	topK     int
}

// NewKBSearch 创建 kb_search 工具，固定 topK=3。
// topK 的取舍：给太少可能漏掉关键块，给太多会稀释 prompt、浪费 token
// 还可能把低相关度的噪声喂给模型。3 是个人知识库场景的常见起点。
func NewKBSearch(embedder Embedder, store *vectorstore.Store) *KBSearch {
	return &KBSearch{embedder: embedder, store: store, topK: 3}
}

// Name 用 snake_case 动词短语（模型对这类命名选择准确率最高，见 tools.Tool 注释）。
func (t *KBSearch) Name() string { return "kb_search" }

// Description 是给模型看的"使用说明书"，属于 prompt 工程。
//
// 写法 = 用途 + 使用时机 + 反面提示（什么时候不要用）：
//   - 用途：在用户本地知识库中做语义检索；
//   - 使用时机：问题涉及用户学过/收藏的文档（学习笔记、收藏文章等）；
//   - 反面提示：不要用于算术（calculator）、网页内容（http_fetch）、
//     与知识库无关的常识问题——没有边界的工具说明会让模型逢问必搜，
//     浪费 embedding 调用还把噪声塞进 prompt。
//
// 最后一句"检索不到就如实说"是防幻觉的关键指令：工具返回空时，
// 模型倾向于凭自身知识编造一个"看起来来自知识库"的答案，
// 必须在 Description 里提前堵住。
func (t *KBSearch) Description() string {
	return "在用户的个人知识库中做语义检索，返回与问题最相关的文档片段（带来源标注）。" +
		"使用时机：问题涉及用户通过学习命令（/learn）收录的文档，如学习笔记、收藏的文章、项目资料。" +
		"不要用于：算术计算（用 calculator）、获取网页内容（用 http_fetch）、与知识库无关的常识问答。" +
		"如果检索结果为空或提示没有相关内容，必须如实告知用户知识库未覆盖该问题，不要编造来源。"
}

// ParametersSchema 只有一个必填参数 query。
// 参数的 description 告诉模型"传什么形式的问题效果最好"——
// 模型传错参数的主要原因就是参数含义没说清（见 tools.Tool 注释）。
func (t *KBSearch) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "检索问题，用完整的自然语言句子描述（如「余弦相似度为什么用 float64 累加」），比堆砌关键词检索效果更好",
			},
		},
		"required": []string{"query"},
	}
}

// TODO(练习4): kb_search 的 Execute —— RAG 的"读取路径"
//
// 【任务】
// 实现：
//
//	func (t *KBSearch) Execute(args string) (string, error)
//
// 流程：解析参数 → 查询 embedding → store.Search 取 topK → 格式化成
// 带 [1][2] 编号和来源标注的参考文本返回给模型。
//
// 【提示】
//   - 参数解析：json.Unmarshal 到 kbSearchArgs（tools.decodeArgs 是私有的，
//     这里自己解）。畸形 JSON 返回 error；query 去空白后为空也返回 error——
//     这类"模型用错了"的错误会作为 tool 消息喂回给模型，让它自我纠正。
//   - 空库短路：t.store.Len() == 0 时直接返回固定文案，不要浪费一次
//     embedding 调用（一次调用 = 一次 HTTP + 计费）。
//   - 检索：Embed([]string{query}) 取向量，再 t.store.Search(vec, t.topK)。
//   - 低分过滤：余弦相似度低于阈值（教学取值 0.3，可注释说明这是经验值）
//     的命中是"噪声"而非"相关内容"，全部低于阈值时返回明确文案
//     「知识库中没有相关内容……」——没有这句话，模型拿不到结果就会凭自身
//     知识编造，还伪装成来自知识库（RAG 防幻觉的第一道闸）。
//   - 输出格式建议（模型引用友好）：
//
//     以下是从知识库检索到的相关内容（请在回答中用 [编号] 标注引用来源）：
//
//     [1]（来源：notes.md，相似度 0.87）
//     块文本……
//
//   - 生产环境注意（写注释即可，不必实现）：真实系统还会在这里加结果缓存、
//     重排序（rerank）、检索结果按来源去重（输出侧去重，与参考答案"进阶实现"
//     一节的入库侧去重是两回事）等；embedding 调用应有超时与重试。
//
// 【验收】
// 完成练习 2、3 后 go test ./internal/rag/ 通过（测试由参考答案提供，
// 用假 Embedder 让查询确定性命中特定文档，覆盖：输出含 [1] 编号与来源、
// 空库文案、低分过滤文案）。
//
// 参考答案：docs/solutions/stage-02/exercise-4-rag-tool.md（完成后再看）

const minScore = 0.3

func (t *KBSearch) Execute(args string) (string, error) {
	var in kbSearchArgs
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("invalid tool arguments %q:%w", args, err)
	}

	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" {
		return "", fmt.Errorf("kb_search: query is empty")
	}

	if t.store.Len() == 0 {
		return "知识库当中没有相关内容(知识库为空，请先使用learn收录文档) ", nil
	}

	vecs, err := t.embedder.Embed([]string{in.Query})
	if err != nil {
		return "", fmt.Errorf("kb_search: embed query: %w", err)
	}

	hits, err := t.store.Search(vecs[0], t.topK)
	if err != nil {
		return "", fmt.Errorf("kb_search: search: %w", err)
	}

	var relevant []vectorstore.Hit
	for _, h := range hits {
		if h.Score >= minScore {
			relevant = append(relevant, h)
		}
	}

	if len(relevant) == 0 {
		return "知识库中没有相关内容° 请如实告知用户知识库未覆盖该问题，不要凭自身知识编造来源° ", nil
	}

	var sb strings.Builder
	sb.WriteString("以下是从知识库检索到的相关内容（请在回答中用[编号] 标注引用来源）：\n")
	for i, h := range relevant {
		fmt.Fprintf(&sb, "\n[%d]（来源：%s, 相似度 %.2f）\n%s\n", i+1, h.Doc.Metadata["source"], h.Score, h.Doc.Text)
	}

	return strings.TrimRight(sb.String(), "\n"), nil
}
