// Package memory 实现 agent 的长期记忆（long-term memory）。
//
// 在 agent 链路中的位置：阶段一的"对话历史即状态"是短期记忆——
// 只在会话内有效，进程退出即丢。本包把值得跨会话保留的事实
// （用户偏好、历史结论等）持久化到磁盘，并以工具形态
// （memory_save / memory_recall）挂进 ReAct 循环，
// 让模型自己决定"什么时候记、什么时候回忆"。
//
// 设计三问（面试高频：长期 memory 怎么设计？）：
//
//  1. 写什么？——用户偏好、重要背景、历史结论等"值得跨会话记住的事实"。
//     不是什么对话都记：一次性请求和闲聊进了记忆库只会稀释检索质量。
//     本项目把"该不该记"的判断交给模型（靠 memory_save 的 Description 引导）；
//     生产系统常用单独的"记忆抽取"步骤（用小模型从对话里提炼事实再入库）。
//
//  2. 何时回忆？——两条路线：每轮自动检索（把记忆无脑塞进每轮 prompt，
//     实现简单但烧 token，还可能带入无关记忆干扰回答）vs 模型主动调工具
//     （判断"这个问题可能依赖历史信息"时才查）。本项目选后者：
//     "回忆"不过是模型多了一件可调用的工具，这正是阶段一 ReAct 循环的复习。
//
//  3. 与 RAG 的异同？——技术栈完全相同（embedding + 向量库 + top-k 检索），
//     区别在写入路径与召回时机：RAG 的知识是离线预先灌入的静态文档；
//     memory 的内容是会话中动态产生的事实。所以本包只是
//     internal/embed + internal/vectorstore 之上的一层薄封装，
//     真正的设计工作量在上面两个问题的取舍上，不在检索代码。
//
// 练习：本包的核心实现由学习者完成，见下方 TODO(练习5)。
package memory

import (
	"errors"

	"mini-agent/internal/tools"
	"mini-agent/internal/vectorstore"
)

// Embedder 是 memory 包对"文本 → 向量"能力的最小依赖。
//
// 为什么各包独立定义接口而不抽一个公共包：这是 Go 的惯例——
// 在使用方定义接口（accept interfaces），各包只声明自己需要的方法集，
// 依赖方向干净，测试时可以轻松换成假实现。教学项目保持包自治：
// rag 包（练习 4）也会定义自己的 Embedder，签名相同但互不 import。
// 生产项目里若出现第三个使用者，再抽公共定义不迟（"三次重复再抽象"）。
type Embedder interface {
	// Embed 与 internal/embed.Client.Embed 签名一致：
	// 输入一批文本，返回一一对应的向量（result[i] 是 texts[i] 的向量）。
	Embed(texts []string) ([][]float32, error)
}

// Store 是长期记忆库：在向量库之上加"事实"语义与自动持久化。
// 它不做检索算法本身——那是 vectorstore 的职责（分层复用，不重造轮子）。
type Store struct {
	vs   *vectorstore.Store // 底层存储与检索（练习 2 的成果）
	emb  Embedder           // 文本 → 向量（练习 1 的成果）
	path string             // 持久化文件路径：每次写入后立即落盘，防进程崩溃丢记忆
}

// NewStore 组装长期记忆库。path 是 JSON 持久化文件路径
// （复用 vectorstore 的 Save/Load 格式）；启动时是否 Load 旧记忆由调用方决定。
func NewStore(vs *vectorstore.Store, emb Embedder, path string) *Store {
	return &Store{vs: vs, emb: emb, path: path}
}

// 编译期断言：两个工具必须实现 tools.Tool 接口，接口变更时编译即报错。
var (
	_ tools.Tool = MemorySave{}
	_ tools.Tool = MemoryRecall{}
)

// TODO(练习5): 记住事实 Remember
//
// 【任务】实现：
//
//	func (s *Store) Remember(fact string) error
//
// 把一条事实写入长期记忆并立即持久化。链路：embed 事实 → 入库 → Save 落盘。
//
// 【提示】
//   - 入参校验：TrimSpace 后为空的事实直接报错——空字符串 embedding 没有语义
//     （练习 1 的 embed.Client 也会拒绝它，但应该在更早的层面拦下）。
//   - embed 是批量接口，单条也要包成 []string{fact} 调用，取返回的第 0 个向量。
//   - 入库：vectorstore.Document{ID, Text: fact, Vector}。ID 必须唯一——
//     简单做法是 fmt.Sprintf("mem-%d", time.Now().UnixNano())。
//     Metadata 可以存 {"kind": "memory"} 之类，为将来"记忆与知识混存一库"留口子。
//   - 入库成功后立刻 s.vs.Save(s.path) 落盘：长期记忆的价值就在"跨会话"，
//     写内存不落盘等于没记。保存失败要把错误返回给调用方，不能吞掉。
//
// 【验收】go test ./internal/memory/ 通过：Remember 后立即 Recall 能命中。
//
// 参考答案：docs/solutions/stage-02/exercise-5-memory-tools.md（完成后再看）
func (s *Store) Remember(fact string) error {
	return errors.New("memory: Remember TODO 未实现")
}

// TODO(练习5): 检索式回忆 Recall
//
// 【任务】实现：
//
//	func (s *Store) Recall(query string, topK int) ([]string, error)
//
// 用自然语言查询检索相关记忆，返回事实文本列表（按相关度降序）。
// 链路：embed 查询 → 向量库 Search → 提取命中文档的 Text。
//
// 【提示】
//   - topK <= 0 时给一个合理默认值（如 3）而不是报错：Recall 的调用方是
//     工具层（模型可能不传 top_k），对不可信输入"宽容兜底"比报错更实用。
//     注意这与 vectorstore.Search 对 topK<=0 报错的设计不同——层次不同，
//     面向模型的边界层宜宽，面向代码的底层库宜严。
//   - 空库时 Search 返回空结果（不是错误），Recall 也应返回空切片 + nil。
//   - 只返回 []string（事实文本），不暴露 Hit/Score：工具结果最终要拼进
//     prompt 给模型看，模型不需要相似度分数，给了反而稀释注意力。
//
// 【验收】go test ./internal/memory/ 通过：语义命中、空库不报错、
// topK 缺省有兜底。
//
// 参考答案：docs/solutions/stage-02/exercise-5-memory-tools.md（完成后再看）
func (s *Store) Recall(query string, topK int) ([]string, error) {
	return nil, errors.New("memory: Recall TODO 未实现")
}

// MemorySave 是给模型用的"记住"工具（说明书由 AI 写全，Execute 是练习）。
//
// 说明书设计：Description 必须同时说清"什么时候该记"和"什么时候不该记"——
// 只写用途的话，模型会把闲聊也记进库，记忆库很快被垃圾稀释（检索质量下降）。
type MemorySave struct {
	Store *Store
}

func (t MemorySave) Name() string { return "memory_save" }

func (t MemorySave) Description() string {
	return "把一条值得长期记住的事实写入记忆库（跨会话持久保存）。" +
		"使用时机：用户明确说“记住/记一下”，或透露稳定的个人偏好、习惯、重要背景" +
		"（如“我不吃辣”“我是后端工程师”）。" +
		"不要记录：一次性的请求、闲聊、只与当前对话相关的临时信息。"
}

func (t MemorySave) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"fact": map[string]any{
				"type":        "string",
				"description": "要记住的事实，用一句完整的话表述（如“用户不吃辣”）",
			},
		},
		"required": []string{"fact"},
	}
}

// MemoryRecall 是给模型用的"回忆"工具（说明书由 AI 写全，Execute 是练习）。
//
// 说明书设计：强调"问题可能依赖历史信息时才查"——如果模型对无关问题也查记忆，
// 每轮白烧一次 embedding 调用（有成本），还可能把无关记忆塞进上下文。
type MemoryRecall struct {
	Store *Store
}

func (t MemoryRecall) Name() string { return "memory_recall" }

func (t MemoryRecall) Description() string {
	return "从长期记忆中检索与查询相关的事实。" +
		"使用时机：用户的问题可能依赖历史信息时（如“我喜欢吃什么”“我上次说的事”），" +
		"先调用本工具再回答。如果问题与用户的过往明显无关，不要调用。"
}

func (t MemoryRecall) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "检索查询，用描述信息需求的自然语言短语（如“饮食偏好”），不必是用户的原话",
			},
			"top_k": map[string]any{
				"type":        "integer",
				"description": "最多返回几条记忆，缺省 3 条",
			},
		},
		"required": []string{"query"},
	}
}

// TODO(练习5): 两个工具的 Execute + 注册到 agent
//
// 【任务】
//  1. 实现 MemorySave.Execute：json.Unmarshal 解析 {"fact": "..."} →
//     调用 Store.Remember → 成功时返回确认文本（如 "已记住：xxx"）。
//  2. 实现 MemoryRecall.Execute：解析 {"query": "...", "top_k": N} →
//     调用 Store.Recall → 把事实列表编号拼成多行文本返回；
//     结果为空时返回"没有找到相关记忆。"（一句明确的否定结果比空字符串
//     更利于模型继续推理——空串会让模型分不清"没查到"和"工具坏了"）。
//  3. 在 cmd/agent/main.go 注册两个工具（本练习不改 main.go，由你完成）：
//
//     memStore := memory.NewStore(vectorstore.NewStore(), embClient, "memory.json")
//     _ = memStore // 启动时可选：vs.Load("memory.json") 恢复上次会话的记忆
//     registry.Register(memory.MemorySave{Store: memStore})
//     registry.Register(memory.MemoryRecall{Store: memStore})
//
//     其中 embClient 是练习 1 的 embed.Client（需 SILICONFLOW_API_KEY）。
//
// 【提示】
//   - args 是模型生成的不可信输入：畸形 JSON 必须返回 error 喂回模型
//     让它自我纠正，不能 panic。注意 tools.decodeArgs 是小写未导出，
//     跨包用不了，这里自己 json.Unmarshal。
//   - Remember/Recall 的 error 直接向上返回即可——ReAct 循环会把错误
//     文本喂回模型，这正是 agent 的自我纠错机制（阶段一已学）。
//
// 【验收】
//  1. go test ./internal/memory/ 通过（含工具层测试：坏 JSON 报错、
//     空库 Recall 返回"没有找到相关记忆。"）。
//  2. 端到端：启动 agent，说"记住我不吃辣"，再问"我喜欢吃什么"，
//     观察 Verbose 日志中模型先调 memory_recall 再回答。
//
// 参考答案：docs/solutions/stage-02/exercise-5-memory-tools.md（完成后再看）
func (t MemorySave) Execute(args string) (string, error) {
	return "", errors.New("memory: MemorySave.Execute TODO 未实现")
}

// Execute 见上方 TODO(练习5) 块。
func (t MemoryRecall) Execute(args string) (string, error) {
	return "", errors.New("memory: MemoryRecall.Execute TODO 未实现")
}
