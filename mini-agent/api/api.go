// Package api 是 mini-agent 内核的对外门面（facade）。
//
// 为什么需要这个包：mini-agent 的所有实现都在 internal/ 下，
// 按 Go 的 internal 可见性规则，只有本 module 目录树内的代码能 import 它们。
// 阶段三的项目 stage-03-multi-agent 是独立 module（独立 HTTP API、持久化、前端），
// 要复用这里的 ReAct 内核与工具，就必须有一条"导出的通道"。
//
// 设计取舍：不搬动 internal/ 里的任何文件（阶段一/二的代码与练习不受影响），
// 只在这里用类型别名（type alias）+ 函数转发把内核"提升"为可导出包。
// 别名而非新类型，保证外部拿到的 *api.Agent 就是 *agent.Agent，方法全部保留。
//
// 反直觉点（面试可能问）：internal 的判定基于**目录树位置**而非 module 名，
// 所以即使 stage-03 的 module 路径叫 mini-agent/xxx 也没用——
// 代码必须物理位于 mini-agent/ 目录下才能 import 它的 internal 包。
//
// 练习：本模块无需学习者完成的部分（阶段三练习在 stage-03-multi-agent/ 内）。
package api

import (
	"mini-agent/internal/agent"
	"mini-agent/internal/embed"
	"mini-agent/internal/llm"
	"mini-agent/internal/memory"
	"mini-agent/internal/rag"
	"mini-agent/internal/tools"
	"mini-agent/internal/vectorstore"
)

// ============================ llm 客户端与协议类型 ============================

type (
	// Client 是 OpenAI 兼容的 chat 客户端（默认 DeepSeek）。
	Client = llm.Client
	// Message / ToolCall 是 messages 协议的基本元素。
	Message  = llm.Message
	ToolCall = llm.ToolCall
	// ToolSchema / ToolFunction 是工具的线缆格式（API 请求里的 tools 字段）。
	// 起名 ToolSchema 而非 Tool，是为了给下面的 tools.Tool 接口让出短名字——
	// 外部代码打交道更多的是"可执行的工具接口"，而不是线缆结构。
	ToolSchema   = llm.Tool
	ToolFunction = llm.ToolFunction
	// ChatResponse / Choice / Usage 是一次调用的响应与 token 用量。
	// Usage 在阶段三特别重要：成本核算到每个子任务靠它。
	ChatResponse = llm.ChatResponse
	Choice       = llm.Choice
	Usage        = llm.Usage
	// APIError 保留 HTTP 状态码，供重试/降级逻辑分类（429 vs 5xx）。
	APIError = llm.APIError
)

// NewClient 创建 LLM 客户端（DeepSeek，可用 WithModel 切换模型做"模型分级"）。
var NewClient = llm.NewClient

// ============================ agent 内核（ReAct 循环） ============================

// Agent 是阶段一的 ReAct 循环内核。阶段三把它作为编排系统中的
// "单 agent 执行体"（worker）：每个子任务 new 一个 Agent，跑完即弃，
// 天然隔离 context——这正是"多 agent 解决 context 膨胀"的落地方式。
type Agent = agent.Agent

var NewAgent = agent.New

// ============================ 工具抽象与内置工具 ============================

type (
	// Tool 是工具接口：说明书（Name/Description/Schema）+ 执行（Execute）。
	Tool = tools.Tool
	// Registry 维护 name -> Tool，供 agent 循环 dispatch。
	Registry = tools.Registry
	// Calculator / HTTPFetch 是阶段一的内置工具。
	Calculator = tools.Calculator
	HTTPFetch  = tools.HTTPFetch
)

var (
	NewRegistry = tools.NewRegistry
	// NewReadFile / NewWriteFile 的 root 参数把文件操作限制在工作目录内。
	NewReadFile  = tools.NewReadFile
	NewWriteFile = tools.NewWriteFile
)

// ============================ 阶段二产出：RAG / Memory / 向量库 ============================
//
// 阶段三的 worker 可按需挂载这些工具（如"检索知识库后回答"类子任务）。

type (
	// EmbedClient 是 bge-m3 embedding 客户端（硅基流动 / Ollama）。
	EmbedClient = embed.Client
	// VectorStore 是纯内存向量库（余弦相似度暴力检索，可持久化到 JSON）。
	VectorStore = vectorstore.Store
	Document    = vectorstore.Document
	Hit         = vectorstore.Hit
	// KnowledgeBase 是 RAG 知识库（Ingest 切块入库），KBSearch 是其工具形态。
	KnowledgeBase = rag.KnowledgeBase
	ChunkOptions  = rag.ChunkOptions
	KBSearch      = rag.KBSearch
	RAGEmbedder   = rag.Embedder
	// MemoryStore 是长期记忆库，MemorySave/MemoryRecall 是其工具形态。
	MemoryStore  = memory.Store
	MemorySave   = memory.MemorySave
	MemoryRecall = memory.MemoryRecall
)

var (
	NewEmbedClient      = embed.NewClient
	NewVectorStore      = vectorstore.NewStore
	NewKnowledgeBase    = rag.NewKnowledgeBase
	DefaultChunkOptions = rag.DefaultChunkOptions
	NewKBSearch         = rag.NewKBSearch
	NewMemoryStore      = memory.NewStore
)
