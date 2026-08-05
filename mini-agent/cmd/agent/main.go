// CLI 入口：一个多轮对话的 agent。
// 用法：DEEPSEEK_API_KEY=sk-xxx go run ./cmd/agent
//
// 练习：本文件无需学习者完成的部分（仅组装各模块）。
// 练习 1 在 internal/llm/client.go，练习 3 在 internal/agent/agent.go，
// 练习 4 在 internal/tools/tools.go。
package main

import (
	"bufio"
	"fmt"
	"mini-agent/internal/agent"
	"mini-agent/internal/llm"
	"mini-agent/internal/tools"
	"os"
	"strings"
)

const systemPrompt = `你是一个乐于助人的助手。
规则：
1. 涉及算术时必须使用 calculator 工具，不要心算。
2. 需要访问网页时使用 http_fetch 工具。
3. 回答用中文，简洁直接。`

func main() {
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "请先设置环境变量 DEEPSEEK_API_KEY")
		os.Exit(1)
	}

	registry := tools.NewRegistry()
	registry.Register(tools.Calculator{})
	registry.Register(tools.HTTPFetch{})

	client := llm.NewClient(apiKey)

	ag := agent.New(client, registry, systemPrompt)
	ag.Verbose = true // 打印每一步的工具调用，方便观察 ReAct 循环

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

		// answer, err := ag.Run(input)
		answer, err := client.ChatStream(ag.Messages(), func(text string) {
			fmt.Println("返回数据:", text)
		})
		if err != nil {
			fmt.Println("出错:", err)
			continue
		}
		fmt.Println(answer)
	}
}
