// Package tools 定义工具抽象与注册表。
//
// 核心思想：一个工具 = JSON Schema 描述（给模型看） + 执行函数（给自己跑）。
// 模型只能"请求"调用工具，真正执行的是我们的代码 —— 这是 agent 安全模型的基础。
package tools

import (
	"encoding/json"
	"fmt"
	"mini-agent/internal/llm"
)

// Tool 是一个可被模型调用的能力。
//
// 设计要点：前三个方法定义的是"给模型看的说明书"，Execute 才是"给我们跑的实现"。
// 模型在每一轮对话中只能看到 Name/Description/ParametersSchema 这三样，
// 它基于这些信息决定调不调、传什么参数 —— 所以"说明书"的质量直接决定 agent 的智商。
type Tool interface {
	// Name 是工具名，模型通过它选择工具。
	// 约定用 snake_case 动词短语（如 http_fetch），因为模型对这类命名选择准确率最高。
	Name() string

	// Description 告诉模型"什么时候该用这个工具"，是 prompt 工程的一部分。
	// 好的写法 = 用途 + 使用时机 + 反面提示（什么时候不要用）。
	// 面试点：多个工具职责重叠时模型会乱选，通常靠改 Description 划清边界解决。
	Description() string

	// ParametersSchema 是参数的 JSON Schema。
	// 每个参数都要写 description —— 模型传错参数的主要原因就是参数含义没说清。
	ParametersSchema() map[string]any

	// Execute 真正执行工具。args 是模型生成的 JSON 字符串（对应 Schema）。
	// 注意：args 是"不可信输入"，模型可能传畸形 JSON、漏字段、塞超长字符串，
	// 实现里必须做校验，绝不能直接拼进 shell 命令或 SQL。
	Execute(args string) (string, error)
}

// Registry 维护 name -> Tool 的映射。
type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Register(t Tool) {
	r.tools[t.Name()] = t
}

// Schemas 把所有注册工具转成 API 需要的格式。
func (r *Registry) Schemas() []llm.Tool {
	out := make([]llm.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.ParametersSchema(),
			},
		})
	}
	return out
}

// Call 按名字分发一次工具调用。
//
// 这是模型世界和代码世界的交界处：模型输出文本（工具名+参数），
// 这里把文本路由到真正的 Go 实现。框架里这层常叫 tool dispatch / tool executor。
func (r *Registry) Call(name, args string) (string, error) {
	t, ok := r.tools[name]
	fmt.Println("这里在执行真正的本地代码", name, args)
	if !ok {
		// 模型偶尔会编造不存在的工具名，不能 panic，要把错误喂回去让它自我纠正
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return t.Execute(args)
}

// decodeArgs 是一个小工具：把模型给的 JSON 字符串解析到结构体。
func decodeArgs(args string, v any) error {
	if err := json.Unmarshal([]byte(args), v); err != nil {
		return fmt.Errorf("invalid tool arguments %q: %w", args, err)
	}
	return nil
}

// ============================ 练习区（由学习者完成） ============================
//
// TODO(练习4): 文件读写工具 —— 重点是安全边界设计
//
// 任务：新建 file.go，实现两个工具：read_file（读文件内容）、
// write_file（写入文件），注册到 main.go 的 Registry。
//
// 提示（本练习的核心考点是安全，不是 IO）：
//   - 所有路径必须限制在一个工作目录内：拼接后用 filepath.Clean，
//     再检查是否仍落在根目录内（防 ../ 逃逸和绝对路径逃逸）
//   - read_file 和 http_fetch 一样要截断返回
//   - write_file 考虑：是否允许覆盖已存在文件？写前是否创建父目录？
//     这些取舍就是工具 Description 里要向模型说明的行为
//   - 写工具的 Description 时想清楚：模型什么时候该用 read、什么时候该用 write
//
// 验收：让 agent"把 1+1 的结果写入 result.txt 并读回来确认"；
// 再手动验证 read_file 传 "../../etc/passwd" 会被拒绝。
// 参考答案：docs/solutions/stage-01/exercise-4-file-tools.md（完成后再看）
