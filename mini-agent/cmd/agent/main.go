// CLI 入口：一个多轮对话的 agent。
// 用法：DEEPSEEK_API_KEY=sk-xxx [SILICONFLOW_API_KEY=sk-yyy] go run ./cmd/agent
//
// 练习：本文件无需学习者完成的部分（仅组装各模块）。
// 练习 1 在 internal/llm/client.go，练习 3 在 internal/agent/agent.go，
// 工具练习在 internal/tools/tools.go，
// RAG 练习 4 在 internal/rag/kb.go（Ingest）与 internal/rag/tool.go（Execute）。
package main

import (
	"bufio"
	"fmt"
	"mini-agent/internal/agent"
	"mini-agent/internal/embed"
	"mini-agent/internal/llm"
	"mini-agent/internal/memory"
	"mini-agent/internal/rag"
	"mini-agent/internal/tools"
	"mini-agent/internal/vectorstore"
	"os"
	"strings"
)

const systemPrompt = `你是一个乐于助人的助手。
规则：
1. 涉及算术时必须使用 calculator 工具，不要心算。
2. 需要访问网页时使用 http_fetch 工具。
3. 回答用中文，简洁直接。`

// kbPath 是知识库向量索引的持久化文件。
// 放在 workspace 下与读写工具的根目录一致，方便用 read_file 工具查看。
const (
	kbPath  = "./workspace/kb.json"
	memPath = "memory.json"
)

func main() {
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "请先设置环境变量 DEEPSEEK_API_KEY")
		os.Exit(1)
	}

	registry := tools.NewRegistry()
	registry.Register(tools.Calculator{})
	registry.Register(tools.HTTPFetch{})
	registry.Register(tools.NewReadFile("./workspace"))
	registry.Register(tools.NewWriteFile("./workspace"))

	// ===== RAG 接线（练习 4 的组装层，非练习内容）=====
	// 知识库依赖 embedding API（硅基流动），所以只有配了 SILICONFLOW_API_KEY
	// 才启用；没配时 agent 退化为不带知识库的普通工具 agent。
	// 注意：internal/rag 的 Ingest 与 KBSearch.Execute 是 TODO(练习4) 桩，
	// 完成练习 4 后 /learn 命令与 kb_search 工具才真正可用；
	// 完成前调用它们只会得到"TODO 未实现"错误，不影响其他功能。
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

		memVs := vectorstore.NewStore()
		_ = memVs.Load(memPath)
		memStore := memory.NewStore(memVs, embedClient, memPath)
		registry.Register(memory.MemoryRecall{Store: memStore})
		registry.Register(memory.MemorySave{Store: memStore})

	}

	client := llm.NewClient(apiKey)

	ag := agent.New(client, registry, systemPrompt)
	ag.Verbose = true // 打印每一步的工具调用，方便观察 ReAct 循环
	// 流式打印最终回答：content 增量会实时回调这里；
	// 中间的工具调用步骤没有 content，不会干扰输出。
	ag.OnDelta = func(text string) { fmt.Print(text) }

	fmt.Println("mini-agent 已启动，输入问题开始对话，输入 exit 退出。")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n> ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "exit" {
			break
		}

		// /learn 是斜杠命令（客户端指令），不走 agent——
		// 入库是确定性的本地动作，没必要让模型经手。
		if strings.HasPrefix(input, "/learn") {
			learnFile(kb, strings.TrimSpace(strings.TrimPrefix(input, "/learn")))
			continue
		}

		// 回答已在 OnDelta 中流式打出，这里只需处理错误
		if _, err := ag.Run(input); err != nil {
			fmt.Println("出错:", err)
			continue
		}
	}
}

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
